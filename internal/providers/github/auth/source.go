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

package auth

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
)

var _ interface {
	Token(context.Context) (Token, error)
} = (*Source)(nil)

// Defaults for [Config].
const (
	// DefaultTimeout bounds one Token call that refreshes, signing included.
	DefaultTimeout = 30 * time.Second
	// DefaultRefreshBefore is how long before expiry a cached token is replaced.
	DefaultRefreshBefore = 5 * time.Minute

	apiBase = "https://api.github.com"
	// maxResponseBody bounds every response body. A token response is well under 4 KiB.
	maxResponseBody = 64 << 10
	maxTokenLen     = 1024
	permWrite       = "write"
	minValidity     = time.Minute
)

// permissions are the installation token permissions of design section 8.4, nothing more.
var permissions = map[string]string{
	"contents":      permWrite,
	"pull_requests": permWrite,
	"issues":        permWrite,
	"metadata":      "read",
}

// Config configures a [Source].
type Config struct {
	// ClientID is the GitHub App's client ID (the JWT issuer).
	ClientID string
	// InstallationID is the installation that covers the repository.
	InstallationID int64
	// RepositoryURL is https://github.com/<owner>/<repo>; tokens are scoped to this one repository.
	RepositoryURL string
	// Signer signs the JWT digest; the private key stays behind it.
	Signer providers.Signer
	// Timeout bounds one refresh. Zero means DefaultTimeout.
	Timeout time.Duration
	// RefreshBefore is how long before expiry the token is replaced. Zero means DefaultRefreshBefore.
	RefreshBefore time.Duration
}

// testHooks are what tests replace: the HTTP transport (the host stays pinned in the URL) and the clock.
type testHooks struct {
	transport http.RoundTripper
	now       func() time.Time
}

// Source returns installation tokens for one repository, cached until shortly before they expire. It is
// safe for concurrent use.
type Source struct {
	clientID string
	instID   int64
	repo     Repository
	signer   providers.Signer
	timeout  time.Duration
	margin   time.Duration
	client   *http.Client
	now      func() time.Time

	mu       sync.Mutex
	tok      Token
	expires  time.Time
	inflight *refresh
}

type refresh struct {
	done chan struct{}
	err  error
}

// New validates cfg and returns a Source. It makes no network call.
func New(cfg Config) (*Source, error) { return newSource(cfg, testHooks{}) }

func newSource(cfg Config, h testHooks) (*Source, error) {
	if err := validateClientID(cfg.ClientID); err != nil {
		return nil, err
	}
	if err := validateInstallationID(cfg.InstallationID); err != nil {
		return nil, err
	}
	repo, err := ParseRepositoryURL(cfg.RepositoryURL)
	if err != nil {
		return nil, err
	}
	if cfg.Signer == nil {
		return nil, errors.New("signer is required")
	}
	if cfg.Signer.Algorithm() != providers.RS256 {
		return nil, errors.New("GitHub App JWTs need an RS256 signer")
	}
	s := &Source{
		clientID: cfg.ClientID,
		instID:   cfg.InstallationID,
		repo:     repo,
		signer:   cfg.Signer,
		timeout:  cfg.Timeout,
		margin:   cfg.RefreshBefore,
		now:      h.now,
	}
	if s.timeout <= 0 {
		s.timeout = DefaultTimeout
	}
	if s.margin <= 0 {
		s.margin = DefaultRefreshBefore
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.client = newHTTPClient(h.transport)
	return s, nil
}

// Repository returns the repository the tokens are scoped to.
func (s *Source) Repository() Repository { return s.repo }

// newHTTPClient never follows redirects (Go keeps the Authorization header on a same-host https to http
// redirect), requires TLS 1.2 or later, and bounds connection setup and the wait for response headers.
func newHTTPClient(rt http.RoundTripper) *http.Client {
	if rt == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		t.TLSHandshakeTimeout = 10 * time.Second
		t.ResponseHeaderTimeout = 15 * time.Second
		t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		rt = t
	}
	return &http.Client{
		Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Token returns a valid installation token, refreshing it when it is within RefreshBefore of expiry.
// Concurrent callers share one refresh. A failed refresh leaves the cached token untouched, so a later
// call tries again.
//
// Errors: ErrNotFound when the installation does not exist; a retryable error (with Retry-After when
// GitHub sent one) on throttling, server errors, timeouts and network failures; other errors otherwise.
// No error contains the token, the JWT or response text.
func (s *Source) Token(ctx context.Context) (Token, error) {
	for {
		s.mu.Lock()
		if s.valid() {
			t := s.tok
			s.mu.Unlock()
			return t, nil
		}
		if f := s.inflight; f != nil {
			s.mu.Unlock()
			select {
			case <-f.done:
			case <-ctx.Done():
				return Token{}, fmt.Errorf("wait for installation token: %w", ctx.Err())
			}
			// The leader's own cancellation says nothing about this caller: go around and lead or follow.
			if f.err != nil && !errors.Is(f.err, context.Canceled) {
				return Token{}, f.err
			}
			continue
		}
		f := &refresh{done: make(chan struct{})}
		s.inflight = f
		s.mu.Unlock()

		tok, exp, err := s.fetch(ctx)

		s.mu.Lock()
		if err == nil {
			s.tok, s.expires = tok, exp
		}
		f.err = err
		s.inflight = nil
		s.mu.Unlock()
		close(f.done)
		if err != nil {
			return Token{}, err
		}
		return tok, nil
	}
}

// valid reports whether the cached token may still be used; s.mu must be held.
func (s *Source) valid() bool {
	return s.tok.value != "" && s.now().Add(s.margin).Before(s.expires)
}

func (s *Source) fetch(ctx context.Context) (Token, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	jwt, err := buildJWT(ctx, s.signer, s.clientID, s.now())
	if err != nil {
		return Token{}, time.Time{}, s.classifyCtx(err)
	}
	body, err := json.Marshal(struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}{[]string{s.repo.Name}, permissions})
	if err != nil {
		return Token{}, time.Time{}, fmt.Errorf("encode token request: %w", err)
	}
	url := apiBase + "/app/installations/" + strconv.FormatInt(s.instID, 10) + "/access_tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Token{}, time.Time{}, errors.New("build installation token request")
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "shadewright")

	resp, err := s.client.Do(req)
	if err != nil {
		// The error of http.Client.Do holds the URL and possibly response-controlled text: replace it.
		return Token{}, time.Time{}, s.requestError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	op := "installation token for " + s.repo.String()
	if resp.StatusCode != http.StatusCreated {
		// Drain nothing: the body is never read on failure, so no response text can reach an error.
		return Token{}, time.Time{}, s.statusError(op, resp)
	}
	data, err := readBounded(resp)
	if err != nil {
		return Token{}, time.Time{}, fmt.Errorf("%s: %w", op, err)
	}
	return s.parse(op, data)
}

// classifyCtx marks a deadline hit while signing as retryable.
func (s *Source) classifyCtx(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &providers.RetryableError{Err: err}
	}
	return err
}

func (s *Source) requestError(ctx context.Context, err error) error {
	op := "installation token for " + s.repo.String()
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &providers.RetryableError{Err: fmt.Errorf("%s: %w", op, context.DeadlineExceeded)}
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s: %w", op, context.Canceled)
	}
	return &providers.RetryableError{Err: &opaqueError{msg: op + ": GitHub request failed (network)", err: err}}
}

// opaqueError hides the text of a transport error behind a fixed message.
type opaqueError struct {
	msg string
	err error
}

func (e *opaqueError) Error() string { return e.msg }
func (e *opaqueError) Unwrap() error { return e.err }

var errTooLarge = errors.New("GitHub response is larger than 64 KiB")

func readBounded(resp *http.Response) ([]byte, error) {
	if resp.ContentLength > maxResponseBody {
		return nil, errTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return nil, errors.New("read GitHub response failed")
	}
	if len(data) > maxResponseBody {
		return nil, errTooLarge
	}
	return data, nil
}

func (s *Source) statusError(op string, resp *http.Response) error {
	code := resp.StatusCode
	msg := fmt.Errorf("%s: GitHub returned status %d", op, code)
	switch code {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %w", msg, providers.ErrNotFound)
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: GitHub rejected the App JWT", msg)
	case http.StatusForbidden:
		// A rate limit also answers 403; it comes with Retry-After or an exhausted quota.
		if resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return &providers.RetryableError{RetryAfter: retryAfter(resp), Err: msg}
		}
		return fmt.Errorf("%w: the App may not create tokens for this installation", msg)
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return &providers.RetryableError{RetryAfter: retryAfter(resp), Err: msg}
	}
	return msg
}

type tokenResponse struct {
	Token        string `json:"token"`
	ExpiresAt    string `json:"expires_at"`
	Repositories []struct {
		FullName string `json:"full_name"`
	} `json:"repositories"`
}

func (s *Source) parse(op string, data []byte) (Token, time.Time, error) {
	var r tokenResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return Token{}, time.Time{}, fmt.Errorf("%s: response is not valid JSON", op)
	}
	if !validToken(r.Token) {
		return Token{}, time.Time{}, fmt.Errorf("%s: response holds no usable token", op)
	}
	exp, err := time.Parse(time.RFC3339, r.ExpiresAt)
	if err != nil {
		return Token{}, time.Time{}, fmt.Errorf("%s: response has no valid expiry", op)
	}
	if !exp.After(s.now().Add(minValidity)) {
		return Token{}, time.Time{}, fmt.Errorf("%s: token expires too soon", op)
	}
	if len(r.Repositories) != 1 || !strings.EqualFold(r.Repositories[0].FullName, s.repo.String()) {
		return Token{}, time.Time{}, fmt.Errorf("%s: token is not scoped to exactly this repository", op)
	}
	return Token{value: r.Token}, exp, nil
}

// validToken accepts printable ASCII without spaces, so the token cannot break out of a header.
func validToken(t string) bool {
	if t == "" || len(t) > maxTokenLen {
		return false
	}
	for i := 0; i < len(t); i++ {
		if t[i] <= ' ' || t[i] >= 0x7f {
			return false
		}
	}
	return true
}

// retryAfter reads a Retry-After header in seconds or as an HTTP date, capped at one hour.
func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
	}
	return min(max(d, 0), time.Hour)
}
