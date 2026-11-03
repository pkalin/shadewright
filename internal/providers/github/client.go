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

package github

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/github/auth"
	"github.com/pkalin/shadewright/internal/providers/github/ratelimit"
)

// Defaults for [Config].
const (
	// DefaultMaxAttempts is how many times one request is sent before a transient failure is returned.
	DefaultMaxAttempts = 4
	// DefaultBaseDelay is the first backoff delay; each later one doubles, with jitter.
	DefaultBaseDelay = 500 * time.Millisecond
	// DefaultMaxDelay caps one backoff delay, and is the longest delay requested by GitHub that is waited for
	// in place; a longer one is returned to the caller.
	DefaultMaxDelay = 15 * time.Second
	// DefaultTimeout bounds one method call, retries included.
	DefaultTimeout = 2 * time.Minute
)

const (
	apiBase = "https://api.github.com"
	// maxJSONBody bounds a JSON response. A tree of one directory with a thousand entries is about 150 KiB.
	maxJSONBody    = 2 << 20
	attemptTimeout = 60 * time.Second
)

// TokenSource returns installation tokens for one repository. *auth.Source implements it.
type TokenSource interface {
	Token(ctx context.Context) (auth.Token, error)
	Repository() auth.Repository
}

// Config configures a [Provider].
type Config struct {
	// RepositoryURL is https://github.com/<owner>/<repo>, the only repository the provider reaches.
	RepositoryURL string
	// TargetBranch is the branch change requests target. It is never written to.
	TargetBranch string
	// Tokens returns installation tokens; its repository must be RepositoryURL.
	Tokens TokenSource
	// MaxAttempts bounds the attempts of one request. Zero means DefaultMaxAttempts.
	MaxAttempts int
	// BaseDelay is the first backoff delay. Zero means DefaultBaseDelay.
	BaseDelay time.Duration
	// MaxDelay caps one backoff delay and the delay requested by GitHub that is waited for in place. Zero
	// means DefaultMaxDelay.
	MaxDelay time.Duration
	// Timeout bounds one method call, retries included. Zero means DefaultTimeout.
	Timeout time.Duration
}

// testHooks are what tests replace.
type testHooks struct {
	transport http.RoundTripper
	token     func(context.Context) (string, error)
	sleep     func(context.Context, time.Duration) error
	jitter    func(time.Duration) time.Duration
	now       func() time.Time
}

// Provider is a [providers.GitProvider] for one GitHub repository and target branch. It is safe for
// concurrent use.
type Provider struct {
	repo     auth.Repository
	target   string
	token    func(context.Context) (string, error)
	client   *http.Client
	attempts int
	base     time.Duration
	maxDelay time.Duration
	timeout  time.Duration
	sleep    func(context.Context, time.Duration) error
	jitter   func(time.Duration) time.Duration
	now      func() time.Time
}

var _ providers.GitProvider = (*Provider)(nil)

// New validates cfg and returns a Provider. It makes no network call. Errors never repeat the input.
func New(cfg Config) (*Provider, error) {
	if cfg.Tokens == nil {
		return nil, errors.New("token source is required")
	}
	src := cfg.Tokens
	h := testHooks{token: func(ctx context.Context) (string, error) {
		t, err := src.Token(ctx)
		if err != nil {
			return "", err
		}
		return t.Reveal(), nil
	}}
	p, err := newProvider(cfg, h)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(src.Repository().String(), p.repo.String()) {
		return nil, errors.New("the token source is scoped to another repository")
	}
	return p, nil
}

func newProvider(cfg Config, h testHooks) (*Provider, error) {
	repo, err := auth.ParseRepositoryURL(cfg.RepositoryURL)
	if err != nil {
		return nil, err
	}
	if !validTargetBranch(cfg.TargetBranch) {
		return nil, errors.New("target branch is empty, invalid, or a Shadewright branch")
	}
	if h.token == nil {
		return nil, errors.New("token source is required")
	}
	p := &Provider{
		repo:     repo,
		target:   cfg.TargetBranch,
		token:    h.token,
		client:   newHTTPClient(h.transport),
		attempts: cfg.MaxAttempts,
		base:     cfg.BaseDelay,
		maxDelay: cfg.MaxDelay,
		timeout:  cfg.Timeout,
		sleep:    h.sleep,
		jitter:   h.jitter,
		now:      h.now,
	}
	if p.attempts <= 0 {
		p.attempts = DefaultMaxAttempts
	}
	if p.base <= 0 {
		p.base = DefaultBaseDelay
	}
	if p.maxDelay <= 0 {
		p.maxDelay = DefaultMaxDelay
	}
	if p.timeout <= 0 {
		p.timeout = DefaultTimeout
	}
	if p.sleep == nil {
		p.sleep = sleep
	}
	if p.jitter == nil {
		// Half fixed, half random: spreads retries without making the delay arbitrarily short.
		p.jitter = func(d time.Duration) time.Duration { return d/2 + rand.N(d/2+1) }
	}
	if p.now == nil {
		p.now = time.Now
	}
	return p, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// newHTTPClient never follows redirects (Go keeps the Authorization header on some redirects), requires
// TLS 1.2 or later, and bounds connection setup, the wait for headers and each attempt.
func newHTTPClient(rt http.RoundTripper) *http.Client {
	if rt == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		t.TLSHandshakeTimeout = 10 * time.Second
		t.ResponseHeaderTimeout = 30 * time.Second
		t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		rt = t
	}
	return &http.Client{
		Transport: rt,
		Timeout:   attemptTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// call is one REST request. op is a fixed label for errors; path is built from escaped segments only.
type call struct {
	op     string
	method string
	path   string
	query  url.Values
	body   any
	accept string
	limit  int64
	// graphQL sends the call to the GraphQL endpoint instead of path.
	graphQL bool
}

// response is a response whose status is not a transient failure. body is read only for 2xx.
type response struct {
	status int
	// header is kept for 2xx only: GraphQL reports a rate limit with status 200 and the delay in headers.
	header http.Header
	body   []byte
}

func (r response) ok() bool { return r.status >= 200 && r.status <= 299 }

// errBodyTooLarge marks a 2xx response larger than the call's limit.
var errBodyTooLarge = errors.New("GitHub response is too large")

// do sends a call, retrying transient failures with backoff and jitter. Statuses other than 2xx and
// transient failures are returned as a response, for the method to classify.
func (p *Provider) do(ctx context.Context, c call) (response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := p.once(ctx, c)
		if err == nil || !providers.IsRetryable(err) || attempt+1 >= p.attempts {
			return resp, err
		}
		d, asked := providers.RetryAfter(err)
		if asked && d > p.maxDelay {
			return response{}, err // too long to block a reconcile: the caller requeues
		}
		if !asked {
			d = p.jitter(min(p.base<<attempt, p.maxDelay))
		}
		if dl, ok := ctx.Deadline(); ok && p.now().Add(d).After(dl) {
			return response{}, err
		}
		if serr := p.sleep(ctx, d); serr != nil {
			return response{}, ctxError(c.op, ctx)
		}
	}
}

func (p *Provider) once(ctx context.Context, c call) (response, error) {
	if ctx.Err() != nil {
		return response{}, ctxError(c.op, ctx)
	}
	tok, err := p.token(ctx)
	if err != nil {
		return response{}, tokenError(c.op, err)
	}
	if tok == "" {
		return response{}, fmt.Errorf("%s: installation token is empty", c.op)
	}
	var body io.Reader
	if c.body != nil {
		data, err := json.Marshal(c.body)
		if err != nil {
			return response{}, fmt.Errorf("%s: encode request", c.op)
		}
		body = bytes.NewReader(data)
	}
	u := apiBase + "/repos/" + url.PathEscape(p.repo.Owner) + "/" + url.PathEscape(p.repo.Name) + c.path
	if c.graphQL {
		u = apiBase + "/graphql"
	}
	if len(c.query) > 0 {
		u += "?" + c.query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, c.method, u, body)
	if err != nil {
		return response{}, fmt.Errorf("%s: build request", c.op)
	}
	accept := c.accept
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "shadewright")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.client.Do(req)
	if err != nil {
		// The error of http.Client.Do holds the URL and possibly response-controlled text: replace it.
		return response{}, p.transportError(ctx, c.op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	code := resp.StatusCode
	switch {
	case code >= 200 && code <= 299:
		limit := c.limit
		if limit <= 0 {
			limit = maxJSONBody
		}
		data, err := readBounded(resp, limit)
		if errors.Is(err, errBodyTooLarge) {
			return response{}, fmt.Errorf("%s: %w", c.op, errBodyTooLarge)
		}
		if err != nil {
			return response{}, p.transportError(ctx, c.op, err)
		}
		return response{status: code, header: resp.Header, body: data}, nil
	case code == http.StatusForbidden || code == http.StatusTooManyRequests:
		if d, limited := ratelimit.Limited(resp, p.now()); limited {
			return response{}, &providers.RetryableError{
				RetryAfter: d,
				Err:        fmt.Errorf("%s: GitHub rate limit (status %d)", c.op, code),
			}
		}
		return response{status: code}, nil
	case code == http.StatusRequestTimeout || (code >= 500 && code <= 599):
		return response{}, &providers.RetryableError{
			RetryAfter: ratelimit.RetryAfter(resp.Header, p.now()),
			Err:        fmt.Errorf("%s: GitHub returned status %d", c.op, code),
		}
	}
	return response{status: code}, nil
}

func readBounded(resp *http.Response, limit int64) ([]byte, error) {
	if resp.ContentLength > limit {
		return nil, errBodyTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errBodyTooLarge
	}
	return data, nil
}

// transportError replaces an error of the HTTP client, which may hold the URL or response text, with fixed
// text and its class.
func (p *Provider) transportError(ctx context.Context, op string, err error) error {
	switch {
	case ctx.Err() != nil:
		return ctxError(op, ctx)
	case errors.Is(err, context.DeadlineExceeded):
		return &providers.RetryableError{Err: fmt.Errorf("%s: GitHub request timed out", op)}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &providers.RetryableError{Err: fmt.Errorf("%s: GitHub request timed out", op)}
	}
	return &providers.RetryableError{Err: fmt.Errorf("%s: GitHub request failed (network)", op)}
}

// tokenError replaces an error of the token source with fixed text. Config.Tokens may be any TokenSource,
// whose errors can carry transport or SDK text, a URL or a credential, so only the class is kept: canceled,
// past its deadline, retryable (with the requested delay), not found or forbidden. Nothing of err stays reachable.
func tokenError(op string, err error) error {
	msg := op + ": installation token unavailable"
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s: %w", msg, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		after, _ := providers.RetryAfter(err)
		return &providers.RetryableError{RetryAfter: after, Err: fmt.Errorf("%s: %w", msg, context.DeadlineExceeded)}
	case providers.IsRetryable(err):
		after, _ := providers.RetryAfter(err)
		return &providers.RetryableError{RetryAfter: after, Err: errors.New(msg)}
	case providers.IsNotFound(err):
		return fmt.Errorf("%s: %w", msg, providers.ErrNotFound)
	case providers.IsForbidden(err):
		// The installation refuses the token (a permission the App lacks): final, like a 403 on a call.
		return fmt.Errorf("%s: %w", msg, providers.ErrForbidden)
	}
	return errors.New(msg)
}

// ctxError reports the end of the caller's context. A deadline is retryable (a later reconcile may finish);
// a cancellation is not.
func ctxError(op string, ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &providers.RetryableError{Err: fmt.Errorf("%s: %w", op, context.DeadlineExceeded)}
	}
	return fmt.Errorf("%s: %w", op, context.Canceled)
}

// statusError classifies a status that the method did not handle itself. A 403 or 429 that is a rate limit
// never gets here (once returns it as retryable), so a 403 here is a missing permission or a rule of the
// repository: final. So is a 422 the method could not resolve by reading the state.
func statusError(op string, code int) error {
	msg := fmt.Errorf("%s: GitHub returned status %d", op, code)
	switch code {
	case http.StatusNotFound, http.StatusGone:
		return fmt.Errorf("%w: %w", msg, providers.ErrNotFound)
	case http.StatusConflict:
		return fmt.Errorf("%w: %w", msg, providers.ErrConflict)
	case http.StatusForbidden:
		return fmt.Errorf("%w: %w", msg, providers.ErrForbidden)
	case http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %w", msg, providers.ErrRejected)
	}
	return msg
}

// decode parses a 2xx JSON response.
func decode(op string, r response, v any) error {
	if err := json.Unmarshal(r.body, v); err != nil {
		return fmt.Errorf("%s: GitHub response is not valid JSON", op)
	}
	return nil
}
