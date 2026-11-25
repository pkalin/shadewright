/*
Copyright 2026 The Shadewright Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/providers/model/openaiwire"
)

const (
	// TokenURL is the only address the projected token is sent to.
	TokenURL = "https://sts.googleapis.com/v1/token" // #nosec G101 -- a public endpoint, not a credential
	// Scope is the OAuth scope asked for the federated access token.
	Scope = "https://www.googleapis.com/auth/cloud-platform"
	// AzureExchangeAudience is the audience of the token projected for Azure Entra ID. A token for it is
	// never sent to Google.
	AzureExchangeAudience = "api://AzureADTokenExchange"
	// MaxSubjectBytes is Google's limit on the mapped subject.
	MaxSubjectBytes = 127
)

const (
	grantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange" // #nosec G101 -- an RFC 8693 name, not a credential
	typeAccessToken    = "urn:ietf:params:oauth:token-type:access_token"   // #nosec G101 -- an RFC 8693 name, not a credential
	typeJWT            = "urn:ietf:params:oauth:token-type:jwt"
	subjectPrefix      = "system:serviceaccount:"

	// maxTokenFile bounds the projected token file; a projected token is a few KiB.
	maxTokenFile = 64 << 10
	// maxTokenResponse bounds the body of a 200 from STS.
	maxTokenResponse = 64 << 10
	// maxDrain is how much of a refusal body is read and discarded, so the connection can be reused.
	maxDrain = 64 << 10
	// maxAccessToken bounds the access token STS returns.
	maxAccessToken = 16 << 10
	// maxProjectionAudience bounds the configured projection audience.
	maxProjectionAudience = 1024
	// maxPath bounds the configured token file path.
	maxPath = 4096
	// maxInClusterRanges is openaicompatible.MaxInClusterRanges.
	maxInClusterRanges = 32
	// maxLifetime caps the lifetime STS states for a token.
	maxLifetime = 24 * time.Hour
	// refreshMarginCap is the most time before its expiry that a cached token is replaced.
	refreshMarginCap = 5 * time.Minute
	// exchangeTimeout bounds one exchange, and the wait for the response headers.
	exchangeTimeout = 30 * time.Second
)

var (
	audiencePattern = regexp.MustCompile(`^//iam\.googleapis\.com/projects/[1-9][0-9]{0,19}/locations/global/workloadIdentityPools/[a-z0-9-]{4,32}/providers/[a-z0-9-]{4,32}$`)
	// namespacePattern is a DNS-1123 label; serviceAccountPattern a DNS-1123 subdomain.
	namespacePattern      = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	serviceAccountPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	// embeddedIPv4Forms are the IPv6 ranges an in-cluster range may not overlap, as in
	// openaicompatible.CheckInClusterRange.
	embeddedIPv4Forms = []netip.Prefix{
		netip.MustParsePrefix("::ffff:0:0/96"), netip.MustParsePrefix("::/96"),
		netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("2002::/16"),
	}
)

// Config is the installation's declaration of the exchange. Every field is installation configuration, never
// cluster or model data.
type Config struct {
	// Audience is the STS audience, the workload identity pool provider:
	// //iam.googleapis.com/projects/{number}/locations/global/workloadIdentityPools/{pool}/providers/{provider}.
	Audience string
	// TokenFile is the clean absolute path of the projected token mounted for Google.
	TokenFile string
	// Subject is the analyzer's ServiceAccount, system:serviceaccount:<namespace>:<serviceaccount>.
	Subject string
	// ProjectionAudience is the audience the Google token is projected with.
	ProjectionAudience string
	// InClusterRanges are the declared in-cluster ranges outside the private ones, refused by the dialer.
	InClusterRanges []netip.Prefix
}

// Source returns federated access tokens; see the package documentation.
type Source struct {
	audience, tokenFile, subject, projectionAudience string
	tokenURL                                         string
	client                                           *http.Client
	now                                              func() time.Time

	// mu guards the fields below; it is never held across the network or a file read.
	mu        sync.Mutex
	token     string
	refreshAt time.Time
	// inflight is the exchange in progress, shared by every caller that needs a token meanwhile.
	inflight *exchangeCall
}

// exchangeCall is one exchange in progress. Its result is set before done is closed; waiters is guarded by
// Source.mu.
type exchangeCall struct {
	cancel  context.CancelFunc
	waiters int
	done    chan struct{}
	token   string
	err     error
}

// New checks cfg and returns a Source. It makes no network call and does not read the token file.
func New(cfg Config) (*Source, error) {
	return newSource(cfg, nil)
}

func newSource(cfg Config, rt http.RoundTripper) (*Source, error) {
	if err := validate(cfg); err != nil {
		return nil, err
	}
	client := openaiwire.NewHTTPClient(exchangeTimeout, openaiwire.Guard{InCluster: slices.Clone(cfg.InClusterRanges)})
	if rt != nil {
		client.Transport = rt
	}
	return &Source{
		audience: cfg.Audience, tokenFile: cfg.TokenFile, subject: cfg.Subject,
		projectionAudience: cfg.ProjectionAudience,
		tokenURL:           TokenURL, client: client, now: time.Now,
	}, nil
}

func validate(cfg Config) error {
	invalid := func(what string) error { return fmt.Errorf("sts: %s: %w", what, providers.ErrInvalidConfig) }
	switch {
	case !audiencePattern.MatchString(cfg.Audience):
		return invalid("the STS audience is not a workload identity pool provider")
	case !cleanAbsolute(cfg.TokenFile):
		return invalid("the token file is not a clean absolute path")
	case !validSubject(cfg.Subject):
		return invalid("the subject is not a Kubernetes ServiceAccount of at most 127 bytes")
	case !printable(cfg.ProjectionAudience, maxProjectionAudience):
		return invalid("the projection audience is not printable ASCII of at most 1024 bytes")
	case isAzureAudience(cfg.ProjectionAudience):
		return invalid("the projection audience is the Azure Entra ID audience")
	case !validInClusterRanges(cfg.InClusterRanges):
		return invalid("the in-cluster ranges are not valid")
	}
	return nil
}

func cleanAbsolute(p string) bool {
	return p != "" && len(p) <= maxPath && filepath.IsAbs(p) && filepath.Clean(p) == p && p != "/" &&
		!strings.ContainsRune(p, 0)
}

func validSubject(s string) bool {
	if len(s) > MaxSubjectBytes {
		return false
	}
	rest, ok := strings.CutPrefix(s, subjectPrefix)
	if !ok {
		return false
	}
	ns, sa, ok := strings.Cut(rest, ":")
	return ok && len(ns) <= 63 && namespacePattern.MatchString(ns) &&
		len(sa) <= 253 && serviceAccountPattern.MatchString(sa)
}

// printable reports whether s is 1 to maxLen bytes, each in 0x21 to 0x7e.
func printable(s string, maxLen int) bool {
	if s == "" || len(s) > maxLen {
		return false
	}
	for i := range len(s) {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func isAzureAudience(a string) bool {
	return strings.EqualFold(strings.TrimRight(a, "/"), AzureExchangeAudience)
}

// validInClusterRanges applies the rule of openaicompatible.ValidateInClusterRanges to parsed ranges: at most
// 32, none covering every address, no host bits, and no IPv6 range overlapping a form that embeds IPv4.
func validInClusterRanges(ranges []netip.Prefix) bool {
	if len(ranges) > maxInClusterRanges {
		return false
	}
	for _, p := range ranges {
		if !p.IsValid() || p.Bits() == 0 || p.Masked() != p {
			return false
		}
		if p.Addr().Is6() && slices.ContainsFunc(embeddedIPv4Forms, p.Overlaps) {
			return false
		}
	}
	return true
}

// Token returns the cached access token, or exchanges the projected token for a new one.
func (s *Source) Token(ctx context.Context) (string, error) {
	if ctx.Err() != nil {
		return "", ctxError(ctx)
	}
	s.mu.Lock()
	if s.token != "" && s.now().Before(s.refreshAt) {
		tok := s.token
		s.mu.Unlock()
		return tok, nil
	}
	call := s.inflight
	if call == nil {
		// Detached from this caller's cancellation: the exchange serves every waiter, and is canceled only
		// when all of them have left.
		xctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		call = &exchangeCall{done: make(chan struct{}), cancel: cancel}
		s.inflight = call
		go s.run(xctx, call)
	}
	call.waiters++
	s.mu.Unlock()
	select {
	case <-call.done:
		return call.token, call.err
	case <-ctx.Done():
		s.mu.Lock()
		call.waiters--
		if call.waiters == 0 {
			// Nobody waits any more: stop the exchange, and let the next caller start a fresh one rather than
			// join this canceled one.
			call.cancel()
			if s.inflight == call {
				s.inflight = nil
			}
		}
		s.mu.Unlock()
		return "", ctxError(ctx)
	}
}

// run performs one exchange for every caller waiting on call, and caches its token.
func (s *Source) run(ctx context.Context, call *exchangeCall) {
	defer call.cancel()
	tok, refreshAt, err := s.exchange(ctx)
	s.mu.Lock()
	if err == nil {
		s.token, s.refreshAt = tok, refreshAt
	}
	if s.inflight == call {
		s.inflight = nil
	}
	s.mu.Unlock()
	call.token, call.err = tok, err
	close(call.done)
}

func ctxError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("sts: the token request was canceled: %w", context.Canceled)
	}
	return &providers.RetryableError{Err: fmt.Errorf("sts: no access token: %w", context.DeadlineExceeded)}
}

// refreshMargin is how long before its expiry a token of the given lifetime is replaced.
func refreshMargin(lifetime time.Duration) time.Duration {
	return min(refreshMarginCap, lifetime/2)
}

// exchange returns a new access token and the time to replace it. ctx ends only when every caller waiting
// for it has left (see Token); the exchange is bounded by exchangeTimeout.
func (s *Source) exchange(ctx context.Context) (string, time.Time, error) {
	assertion, err := readTokenFile(s.tokenFile)
	if err != nil {
		return "", time.Time{}, &providers.RetryableError{Err: fmt.Errorf("sts: %w", err)}
	}
	if err := s.checkAssertion(assertion); err != nil {
		return "", time.Time{}, err
	}
	form := url.Values{
		"grant_type":           {grantTokenExchange},
		"audience":             {s.audience},
		"scope":                {Scope},
		"requested_token_type": {typeAccessToken},
		"subject_token_type":   {typeJWT},
		"subject_token":        {assertion},
	}
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, errors.New("sts: could not build the token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	start := s.now()
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", time.Time{}, ctxError(ctx)
		}
		return "", time.Time{}, &providers.RetryableError{Err: errors.New("sts: no answer from Google STS")}
	}
	defer func() { _ = resp.Body.Close() }()
	if ctx.Err() != nil {
		// net/http can return an answer that raced the end of the exchange's context: every waiter left, or
		// exchangeTimeout passed. The end came first: no status of it is classified and no token of it is
		// cached.
		return "", time.Time{}, ctxError(ctx)
	}
	switch code := resp.StatusCode; {
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
		return "", time.Time{}, &providers.RetryableError{Err: fmt.Errorf("sts: Google STS returned status %d", code)}
	case code != http.StatusOK:
		// The body may quote the request: it is drained, never read into an error.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
		return "", time.Time{}, fmt.Errorf("sts: Google STS refused the token request with status %d: %w",
			code, providers.ErrForbidden)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse+1))
	if err != nil || len(data) > maxTokenResponse {
		return "", time.Time{}, &providers.RetryableError{Err: errors.New("sts: the answer of Google STS could not be read")}
	}
	if ctx.Err() != nil {
		// A body read whole after the end is not taken either.
		return "", time.Time{}, ctxError(ctx)
	}
	var body struct {
		AccessToken     string `json:"access_token"`
		IssuedTokenType string `json:"issued_token_type"`
		TokenType       string `json:"token_type"`
		ExpiresIn       int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &body); err != nil || !strings.EqualFold(body.TokenType, "Bearer") ||
		(body.IssuedTokenType != "" && body.IssuedTokenType != typeAccessToken) || body.ExpiresIn <= 0 ||
		!printable(body.AccessToken, maxAccessToken) {
		return "", time.Time{}, &providers.RetryableError{Err: errors.New("sts: the answer of Google STS could not be used")}
	}
	lifetime := time.Duration(min(body.ExpiresIn, int64(maxLifetime/time.Second))) * time.Second
	return body.AccessToken, start.Add(lifetime - refreshMargin(lifetime)), nil
}

// checkAssertion decodes the projected token's payload, without verifying it, and refuses a token that is
// not a JWT, is not for exactly the projection audience or is not for the subject. Errors are fixed text
// matching providers.ErrForbidden.
func (s *Source) checkAssertion(tok string) error {
	refuse := func(msg string) error { return fmt.Errorf("sts: %s: %w", msg, providers.ErrForbidden) }
	sub, auds, ok := decodeClaims(tok)
	switch {
	case !ok:
		return refuse("the projected token is not a JWT")
	case slices.ContainsFunc(auds, isAzureAudience):
		return refuse("the projected token is for the Azure Entra ID audience")
	case len(auds) != 1 || auds[0] != s.projectionAudience:
		return refuse("the projected token is not for the Google projection audience alone")
	case sub != s.subject:
		return refuse("the projected token is for another ServiceAccount")
	}
	return nil
}

// decodeClaims returns the sub and aud claims of a compact JWT: three non-empty base64url parts without
// padding. The payload must be one JSON object with no repeated name; names are matched exactly, so no
// spelling another parser would read differently is taken. aud is a string or an array of strings.
func decodeClaims(tok string) (sub string, auds []string, ok bool) {
	payload, ok := jwtPayload(tok)
	if !ok {
		return "", nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return "", nil, false
	}
	seen := map[string]bool{}
	var haveSub bool
	for dec.More() {
		t, err := dec.Token()
		name, isName := t.(string)
		if err != nil || !isName || seen[name] {
			return "", nil, false
		}
		seen[name] = true
		var v any
		if err := dec.Decode(&v); err != nil {
			return "", nil, false
		}
		switch name {
		case "sub":
			if sub, haveSub = v.(string); !haveSub {
				return "", nil, false
			}
		case "aud":
			if auds, ok = audiences(v); !ok {
				return "", nil, false
			}
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return "", nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", nil, false
	}
	return sub, auds, haveSub
}

// jwtPayload returns the decoded payload of a compact JWT, whose bytes are all base64url or dots.
func jwtPayload(tok string) ([]byte, bool) {
	for i := range len(tok) {
		if !base64URLOrDot(tok[i]) {
			return nil, false
		}
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, false
	}
	var payload []byte
	for i, p := range parts {
		b, err := base64.RawURLEncoding.DecodeString(p)
		if err != nil || p == "" {
			return nil, false
		}
		if i == 1 {
			payload = b
		}
	}
	return payload, true
}

func base64URLOrDot(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
}

// audiences reads the aud claim: a string, or an array of strings.
func audiences(v any) ([]string, bool) {
	switch a := v.(type) {
	case string:
		return []string{a}, true
	case []any:
		auds := make([]string, 0, len(a))
		for _, e := range a {
			s, isString := e.(string)
			if !isString {
				return nil, false
			}
			auds = append(auds, s)
		}
		return auds, true
	}
	return nil, false
}

// readTokenFile reads a regular file of at most maxTokenFile bytes. It opens the path without blocking, so a
// pipe cannot hold the read, and checks the opened file. Errors are fixed and never quote the path or the
// content.
func readTokenFile(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) // #nosec G304 -- the installation's token file path, never cluster or model data
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", errors.New("the projected token file does not exist")
	case err != nil:
		return "", errors.New("the projected token file could not be read")
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", errors.New("the projected token file is not a readable regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxTokenFile+1))
	switch {
	case err != nil:
		return "", errors.New("the projected token file could not be read")
	case len(b) > maxTokenFile:
		return "", errors.New("the projected token file is too large")
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", errors.New("the projected token file is empty")
	}
	return tok, nil
}
