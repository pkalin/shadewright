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

package anthropic

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
)

// The API this adapter calls. The URL is a constant: no setting can point the key at another host.
const (
	// Endpoint is the base URL of the Anthropic API, the only host the Provider connects to.
	Endpoint = "https://api.anthropic.com"
	// APIVersion is the anthropic-version header the Provider sends.
	APIVersion = "2026-10-21"
	// messagesURL is where every request goes.
	messagesURL = Endpoint + "/v1/messages"
)

// Defaults and bounds of [Config], the same as the Foundry adapter's.
const (
	// DefaultMaxAttempts is how many times one call is sent before a transient failure is returned.
	DefaultMaxAttempts = 3
	// MaxAttempts is the most attempts Config.MaxAttempts may ask for: each attempt that reached the model may
	// be billed.
	MaxAttempts = 5
	// DefaultBaseDelay is the first backoff delay; each later one doubles, with jitter.
	DefaultBaseDelay = time.Second
	// DefaultMaxDelay caps one backoff delay, and is the longest delay asked for by the service that is waited
	// for in place; a longer one is returned to the caller.
	DefaultMaxDelay = 20 * time.Second
	// DefaultTimeout bounds one call, retries included.
	DefaultTimeout = 3 * time.Minute
	// DefaultAttemptTimeout bounds one attempt, from sending the request to reading the whole reply.
	DefaultAttemptTimeout = 90 * time.Second
	// DefaultMaxOutputTokens is sent as max_tokens when neither the request nor Config sets a limit. On the
	// current models thinking tokens count against it.
	DefaultMaxOutputTokens = 2048
	// MaxOutputTokens is the highest output limit a request may ask for, the maximum of
	// spec.analysis.maxOutputTokens.
	MaxOutputTokens = 16384
	// DefaultMaxResponseBytes bounds the body of a reply that is read.
	DefaultMaxResponseBytes = 1 << 20
	// maxRequestBytes bounds the encoded request. The context the analyzer sends is at most 256 KiB.
	maxRequestBytes = 4 << 20
	// maxConfigDelay, maxConfigTimeout and maxConfigResponseBytes bound what Config may ask for.
	maxConfigDelay         = 5 * time.Minute
	maxConfigTimeout       = 15 * time.Minute
	maxConfigResponseBytes = 16 << 20
	// maxDrain is how much of an error body is read and discarded, so the connection can be reused.
	maxDrain = 64 << 10
	// maxKeyBytes bounds the API key; maxKeyFileBytes the key file, white space around the key included.
	maxKeyBytes     = 512
	maxKeyFileBytes = 4 << 10
	// retryAfterCeiling caps a delay the service asks for; anything above maxDelay is returned to the caller.
	retryAfterCeiling = 10 * time.Minute
)

// modelPattern is what a configured model ID may look like, for example claude-opus-5-5.
var modelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)

// CallReport describes one Complete call, for metrics and cost accounting. It holds no prompt, reply text,
// key or URL.
type CallReport struct {
	// ConfiguredModel is the model ID the Provider is bound to.
	ConfiguredModel string
	// Model is the model that answered, as the reply named it; empty without a usable reply.
	Model string
	// Usage is the token usage of the reply; zero without a usable reply.
	Usage providers.Usage
	// UsageUnknown is set when an attempt got a 200 answer, so was billed, whose usage could not be read.
	UsageUnknown bool
	// Attempts is the number of requests sent (or tried) in this call.
	Attempts int
	// Duration is the time the call took, backoff included.
	Duration time.Duration
	// Err is the error Complete returned, nil on success.
	Err error
}

// Config configures a [Provider].
type Config struct {
	// Model is the model ID, exactly as the replies name it, for example claude-opus-5-5.
	Model string
	// KeyFile is the path of a file that holds the API key, read at every attempt. Required.
	KeyFile string
	// MaxOutputTokens is sent when a request leaves its own limit at zero. Zero means DefaultMaxOutputTokens;
	// at most MaxOutputTokens.
	MaxOutputTokens int
	// MaxAttempts bounds the attempts of one call. Zero means DefaultMaxAttempts; at most MaxAttempts.
	MaxAttempts int
	// BaseDelay is the first backoff delay. Zero means DefaultBaseDelay.
	BaseDelay time.Duration
	// MaxDelay caps one backoff delay and the delay asked for by the service that is waited for in place. Zero
	// means DefaultMaxDelay.
	MaxDelay time.Duration
	// Timeout bounds one call, retries included. Zero means DefaultTimeout. A shorter deadline on the caller's
	// context wins.
	Timeout time.Duration
	// AttemptTimeout bounds one attempt. Zero means DefaultAttemptTimeout.
	AttemptTimeout time.Duration
	// MaxResponseBytes bounds the reply body that is read. Zero means DefaultMaxResponseBytes.
	MaxResponseBytes int64
	// OnCall, when set, is called once at the end of every Complete call, on the caller's goroutine.
	OnCall func(CallReport)
}

// testHooks are what tests replace.
type testHooks struct {
	transport http.RoundTripper
	sleep     func(context.Context, time.Duration) error
	jitter    func(time.Duration) time.Duration
	now       func() time.Time
}

// Provider is a [providers.ModelProvider] for one Claude model. It is safe for concurrent use.
type Provider struct {
	model          string
	keyFile        string
	client         *http.Client
	maxOutput      int
	attempts       int
	base           time.Duration
	maxDelay       time.Duration
	timeout        time.Duration
	attemptTimeout time.Duration
	maxBody        int64
	onCall         func(CallReport)
	sleep          func(context.Context, time.Duration) error
	jitter         func(time.Duration) time.Duration
	now            func() time.Time
}

var _ providers.ModelProvider = (*Provider)(nil)

// New validates cfg and returns a Provider. It makes no network call and does not read the key file. Errors
// match [providers.ErrInvalidConfig] and never repeat the input.
func New(cfg Config) (*Provider, error) {
	return newProvider(cfg, testHooks{})
}

func newProvider(cfg Config, h testHooks) (*Provider, error) {
	invalid := func(msg string) error { return fmt.Errorf("anthropic: %s: %w", msg, providers.ErrInvalidConfig) }
	if !modelPattern.MatchString(cfg.Model) {
		return nil, invalid("the model ID is empty or invalid")
	}
	if cfg.KeyFile == "" {
		return nil, invalid("a key file is required")
	}
	if cfg.MaxOutputTokens < 0 || cfg.MaxOutputTokens > MaxOutputTokens {
		return nil, invalid("the output token limit is out of range")
	}
	if cfg.MaxAttempts < 0 || cfg.MaxAttempts > MaxAttempts {
		return nil, invalid("the attempt count is out of range")
	}
	if cfg.BaseDelay < 0 || cfg.MaxDelay < 0 || cfg.Timeout < 0 || cfg.AttemptTimeout < 0 || cfg.MaxResponseBytes < 0 {
		return nil, invalid("a duration or size is negative")
	}
	if cfg.BaseDelay > maxConfigDelay || cfg.MaxDelay > maxConfigDelay || cfg.Timeout > maxConfigTimeout ||
		cfg.AttemptTimeout > maxConfigTimeout || cfg.MaxResponseBytes > maxConfigResponseBytes {
		return nil, invalid("a duration or size is above its bound")
	}
	p := &Provider{
		model:          cfg.Model,
		keyFile:        cfg.KeyFile,
		client:         newHTTPClient(h.transport, cmpOr(cfg.AttemptTimeout, DefaultAttemptTimeout)),
		maxOutput:      cmpOr(cfg.MaxOutputTokens, DefaultMaxOutputTokens),
		attempts:       cmpOr(cfg.MaxAttempts, DefaultMaxAttempts),
		base:           cmpOr(cfg.BaseDelay, DefaultBaseDelay),
		maxDelay:       cmpOr(cfg.MaxDelay, DefaultMaxDelay),
		timeout:        cmpOr(cfg.Timeout, DefaultTimeout),
		attemptTimeout: cmpOr(cfg.AttemptTimeout, DefaultAttemptTimeout),
		maxBody:        cmpOr(cfg.MaxResponseBytes, DefaultMaxResponseBytes),
		onCall:         cfg.OnCall,
		sleep:          h.sleep,
		jitter:         h.jitter,
		now:            h.now,
	}
	if p.sleep == nil {
		p.sleep = sleep
	}
	if p.jitter == nil {
		// Half fixed, half random: spreads retries without making the delay arbitrarily short.
		p.jitter = func(d time.Duration) time.Duration { return d/2 + rand.N(d/2+1) } // #nosec G404 -- retry jitter, not a secret
	}
	if p.now == nil {
		p.now = time.Now
	}
	return p, nil
}

func cmpOr[T int | int64 | time.Duration](v, def T) T {
	if v == 0 {
		return def
	}
	return v
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

// newHTTPClient never follows redirects (the key must reach the API's host only), uses no proxy, requires TLS
// 1.2 or later, and bounds connection setup and the wait for headers (headerTimeout, the attempt timeout).
// Each attempt has its own deadline on its context.
func newHTTPClient(rt http.RoundTripper, headerTimeout time.Duration) *http.Client {
	if rt == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = nil
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		t.TLSHandshakeTimeout = 10 * time.Second
		t.ResponseHeaderTimeout = headerTimeout
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

// Complete sends one Messages API request, retrying transient failures. See the package documentation for
// the classes of its errors.
func (p *Provider) Complete(ctx context.Context, req providers.CompletionRequest) (providers.Completion, error) {
	start := p.now()
	c, attempts, err := p.complete(ctx, req)
	if p.onCall != nil {
		p.onCall(CallReport{
			ConfiguredModel: p.model, Model: c.Model, Usage: c.Usage, UsageUnknown: c.UsageUnknown,
			Attempts: attempts, Duration: p.now().Sub(start), Err: err,
		})
	}
	return c, err
}

func (p *Provider) complete(ctx context.Context, req providers.CompletionRequest) (providers.Completion, int, error) {
	body, offered, err := p.encode(req)
	if err != nil {
		return providers.Completion{}, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	// unknown stays set once an attempt was billed without a usage that reaches the caller.
	unknown := false
	done := func(c providers.Completion, attempts int, err error) (providers.Completion, int, error) {
		c.UsageUnknown = c.UsageUnknown || unknown
		return c, attempts, err
	}
	for attempt := 0; ; attempt++ {
		data, billed, err := p.once(ctx, body)
		if err == nil {
			c, perr := parseResponse(data, p.model, offered, len(req.ResponseSchema) > 0)
			return done(c, attempt+1, perr)
		}
		var c providers.Completion
		if billed {
			c = billedUsage(data)
		}
		if !providers.IsRetryable(err) || attempt+1 >= p.attempts {
			return done(c, attempt+1, err)
		}
		// Retrying drops this attempt's answer: if it was billed, its usage is not returned.
		unknown = unknown || billed
		d, asked := providers.RetryAfter(err)
		if asked && d > p.maxDelay {
			return done(c, attempt+1, err) // too long to block: the caller requeues
		}
		if !asked {
			d = p.jitter(min(p.base<<attempt, p.maxDelay))
		}
		if dl, ok := ctx.Deadline(); ok && p.now().Add(d).After(dl) {
			return done(c, attempt+1, err)
		}
		if serr := p.sleep(ctx, d); serr != nil {
			return done(providers.Completion{}, attempt+1, ctxError(ctx))
		}
	}
}

// readKey reads the API key from the key file. A file that cannot be read, or holds only white space, is
// retryable (a mounted Secret may be between two versions); a key too long or with characters a header cannot
// carry is ErrForbidden. Errors never hold the path, the file's content or the os error.
func (p *Provider) readKey() (string, error) {
	f, err := os.Open(p.keyFile)
	if err != nil {
		return "", &providers.RetryableError{Err: errors.New("anthropic: the API key file could not be read")}
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil {
		return "", &providers.RetryableError{Err: errors.New("anthropic: the API key file could not be read")}
	}
	if len(data) > maxKeyFileBytes {
		return "", fmt.Errorf("anthropic: the API key file is too large: %w", providers.ErrForbidden)
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", &providers.RetryableError{Err: errors.New("anthropic: the API key file is empty")}
	}
	if len(key) > maxKeyBytes {
		return "", fmt.Errorf("anthropic: the API key is too long: %w", providers.ErrForbidden)
	}
	for i := 0; i < len(key); i++ {
		if c := key[i]; c <= ' ' || c > '~' {
			return "", fmt.Errorf("anthropic: the API key holds a character a header cannot carry: %w",
				providers.ErrForbidden)
		}
	}
	return key, nil
}

// once sends one attempt and returns the body of a 200 reply. Every other outcome is a classified error with
// fixed text. billed is true once a 200 arrived before the attempt's deadline, error or not. A response that
// net/http returns after the attempt's context ended is the timeout whatever its status, and not billed. With
// an error, data is the body when it was read whole within the bound, so that its usage can still be read,
// and nil otherwise.
func (p *Provider) once(ctx context.Context, body []byte) (data []byte, billed bool, err error) {
	if ctx.Err() != nil {
		return nil, false, ctxError(ctx)
	}
	key, err := p.readKey()
	if err != nil {
		return nil, false, err
	}
	actx, cancel := context.WithTimeout(ctx, p.attemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodPost, messagesURL, bytes.NewReader(body))
	if err != nil {
		return nil, false, errors.New("anthropic: could not build the request")
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", APIVersion)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "shadewright")
	resp, err := p.client.Do(req)
	if err != nil {
		// The error of http.Client.Do holds the URL and possibly response-controlled text: replace it.
		return nil, false, transportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if actx.Err() != nil {
		// A response that raced the end of the attempt's context: the deadline came first, so it is the
		// timeout, retryable, and nothing of it is read.
		return nil, false, transportError(ctx, context.DeadlineExceeded)
	}

	code := resp.StatusCode
	if code != http.StatusOK {
		// The body of an error may quote the request: it is drained, never read into an error.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
		return nil, false, statusError(resp)
	}
	// A 200 was billed. An oversized body is not parsed (a cut JSON body has no reliable usage).
	tooLarge := fmt.Errorf("anthropic: the reply is larger than %d bytes: %w", p.maxBody, providers.ErrTooLarge)
	if resp.ContentLength > p.maxBody {
		return nil, true, tooLarge
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, p.maxBody+1))
	if err != nil {
		return nil, true, transportError(ctx, err)
	}
	if int64(len(data)) > p.maxBody {
		return nil, true, tooLarge
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return data, true, fmt.Errorf("anthropic: the reply is not JSON: %w", providers.ErrMalformedOutput)
	}
	return data, true, nil
}

// statusError classifies a status other than 200.
func statusError(resp *http.Response) error {
	code := resp.StatusCode
	msg := fmt.Errorf("anthropic: the API returned status %d", code)
	switch {
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500 && code <= 599:
		return &providers.RetryableError{RetryAfter: retryAfter(resp.Header), Err: msg}
	case code == http.StatusUnauthorized || code == http.StatusPaymentRequired || code == http.StatusForbidden:
		return fmt.Errorf("%w: %w", msg, providers.ErrForbidden)
	case code == http.StatusNotFound:
		// An unknown model, or one the account cannot use: there is no fallback to another one.
		return fmt.Errorf("%w: the model is not available: %w", msg, providers.ErrNotFound)
	case code == http.StatusRequestEntityTooLarge:
		return fmt.Errorf("%w: %w", msg, providers.ErrTooLarge)
	}
	return fmt.Errorf("%w: %w", msg, providers.ErrRejected)
}

// retryAfter reads the delay the API asks for in retry-after, in whole seconds. An HTTP date is ignored: it
// would compare the server's clock with this one.
func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	secs, err := strconv.ParseInt(v, 10, 64)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(min(secs, int64(retryAfterCeiling/time.Second))) * time.Second
}

// transportError replaces an error of the HTTP client, which may hold the URL or response text, with fixed
// text and its class. A timeout of one attempt, while the call's context is still alive, is retryable.
func transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctxError(ctx)
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		return &providers.RetryableError{Err: errors.New("anthropic: the model request timed out")}
	}
	return &providers.RetryableError{Err: errors.New("anthropic: the model request failed (network)")}
}

// ctxError reports the end of the call's context. A deadline is retryable (a later attempt may finish); a
// cancellation is not.
func ctxError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &providers.RetryableError{Err: fmt.Errorf("anthropic: model call: %w", context.DeadlineExceeded)}
	}
	return fmt.Errorf("anthropic: model call: %w", context.Canceled)
}
