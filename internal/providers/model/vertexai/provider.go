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

package vertexai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/providers/model/openaiwire"
	"github.com/shadewright-labs/shadewright/internal/providers/retryafter"
)

// Defaults and bounds of [Config], the same as the other model adapters' where they have the same meaning.
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
	// DefaultAttemptTimeout bounds one attempt, from asking for the token to reading the whole reply.
	DefaultAttemptTimeout = 90 * time.Second
	// DefaultMaxOutputTokens is the output limit of a request that leaves its own at zero, when Config sets
	// none.
	DefaultMaxOutputTokens = 2048
	// MaxOutputTokens is the highest output limit a request may ask for, the maximum of
	// spec.analysis.maxOutputTokens.
	MaxOutputTokens = 16384
	// DefaultThinkingAllowance is added to the request's output limit in maxOutputTokens and sent as the
	// thinking budget when Config.ThinkingAllowance is nil.
	DefaultThinkingAllowance = 4096
	// MaxThinkingAllowance is the largest thinking allowance Config may ask for.
	MaxThinkingAllowance = 32768
	// DefaultMaxResponseBytes bounds the body of a reply that is read.
	DefaultMaxResponseBytes = 1 << 20
	// maxRequestBytes bounds the encoded request, as in the Anthropic adapter.
	maxRequestBytes = 4 << 20
	// maxConfigDelay, maxConfigTimeout and maxConfigResponseBytes bound what Config may ask for.
	maxConfigDelay         = 5 * time.Minute
	maxConfigTimeout       = 15 * time.Minute
	maxConfigResponseBytes = 16 << 20
	// maxDrain is how much of an error body is read and discarded, so the connection can be reused.
	maxDrain = 64 << 10
	// maxTokenBytes bounds an access token.
	maxTokenBytes = 16 << 10
	// maxEndpointBytes bounds the endpoint text.
	maxEndpointBytes = 256
	// retryAfterCeiling caps a delay the service asks for; anything above maxDelay is returned to the caller.
	retryAfterCeiling = 10 * time.Minute
)

// mimeJSON is the media type of a request body, and the only one a reply may have.
const mimeJSON = "application/json"

// hostSuffix ends the host of every regional endpoint.
const hostSuffix = "-aiplatform.googleapis.com"

// euLocations are the EU regions whose regional route serves the Gemini 2.5 models (ADR-0050, Google
// documentation checked on 2026-11-25). London and Zurich are outside the EU; the global and multi-region
// endpoints cannot promise one region.
var euLocations = map[string]bool{
	"europe-central2": true, "europe-north1": true, "europe-southwest1": true, "europe-west1": true,
	"europe-west3": true, "europe-west4": true, "europe-west8": true, "europe-west9": true,
}

// noFlashLite are the EU regions that do not serve Gemini 2.5 Flash-Lite.
var noFlashLite = map[string]bool{"europe-west3": true}

var (
	// modelPattern is a Gemini 2.5 model ID: Pro, Flash or Flash-Lite, optionally with a version or a dated
	// preview suffix.
	modelPattern = regexp.MustCompile(`^gemini-2\.5-(pro|flash|flash-lite)(-[0-9]{3}|-preview-[0-9]{2}-[0-9]{4})?$`)
	// projectPattern is a Google Cloud project ID, or a project number.
	projectPattern = regexp.MustCompile(`^([a-z][a-z0-9-]{4,28}[a-z0-9]|[0-9]{1,20})$`)
)

// TokenSource returns federated access tokens for the analyzer's own ServiceAccount, for the Vertex AI
// endpoint. It must be safe for concurrent use. Its errors are never passed on: only their class (canceled,
// deadline, retryable, forbidden) is kept.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// CallReport describes one Complete call, for metrics and cost accounting. It holds no prompt, reply text,
// token or URL.
type CallReport struct {
	// ConfiguredModel is the model ID the Provider is bound to.
	ConfiguredModel string
	// Model is the modelVersion the reply named; empty without a usable reply.
	Model string
	// Usage is the token usage of the reply; zero without a usable reply.
	Usage providers.Usage
	// UsageUnknown is set when an attempt got a 200 answer, so was billed, whose usage could not be read.
	UsageUnknown bool
	// Attempts is the number of attempts made (or tried) in this call.
	Attempts int
	// Duration is the time the call took, backoff included.
	Duration time.Duration
	// Err is the error Complete returned, nil on success.
	Err error
}

// Config configures a [Provider].
type Config struct {
	// Endpoint is the endpoint of the ModelDeployment:
	// https://{location}-aiplatform.googleapis.com/v1/projects/{project}/locations/{location}.
	Endpoint string
	// Model is the Gemini 2.5 model ID, the deployment field of the ModelDeployment.
	Model string
	// Tokens returns the access tokens. Required.
	Tokens TokenSource
	// InClusterRanges are the declared in-cluster ranges outside the private ones: an address in one is
	// refused at dial time, like a private address.
	InClusterRanges []netip.Prefix
	// MaxOutputTokens is the output limit of a request that leaves its own at zero. Zero means
	// DefaultMaxOutputTokens; at most MaxOutputTokens.
	MaxOutputTokens int
	// ThinkingAllowance is added to the request's output limit in maxOutputTokens and sent as the thinking
	// budget. Nil means DefaultThinkingAllowance; zero is allowed when set; at most MaxThinkingAllowance.
	ThinkingAllowance *int
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

// Provider is a [providers.ModelProvider] for one Gemini 2.5 model at one regional endpoint. Its fields do not
// change after New, so it is safe for concurrent use when its TokenSource is.
type Provider struct {
	url               string
	model             string
	tokens            TokenSource
	client            *http.Client
	maxOutput         int
	thinkingAllowance int
	attempts          int
	base              time.Duration
	maxDelay          time.Duration
	timeout           time.Duration
	attemptTimeout    time.Duration
	maxBody           int64
	onCall            func(CallReport)
	sleep             func(context.Context, time.Duration) error
	jitter            func(time.Duration) time.Duration
	now               func() time.Time
}

var (
	_ providers.ModelProvider = (*Provider)(nil)
	_ providers.OutputBounder = (*Provider)(nil)
)

// New validates cfg and returns a Provider. It makes no network call and asks for no token. Errors match
// [providers.ErrInvalidConfig] and never repeat the input.
func New(cfg Config) (*Provider, error) {
	return newProvider(cfg, testHooks{})
}

func newProvider(cfg Config, h testHooks) (*Provider, error) {
	invalid := func(msg string) error { return fmt.Errorf("vertexai: %s: %w", msg, providers.ErrInvalidConfig) }
	if cfg.Tokens == nil {
		return nil, invalid("a token source is required")
	}
	project, location, err := parseEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, invalid(err.Error())
	}
	if err := checkModel(cfg.Model, location); err != nil {
		return nil, invalid(err.Error())
	}
	for _, p := range cfg.InClusterRanges {
		if !p.IsValid() || p != p.Masked() {
			return nil, invalid("an in-cluster range is not a valid CIDR")
		}
	}
	if cfg.MaxOutputTokens < 0 || cfg.MaxOutputTokens > MaxOutputTokens {
		return nil, invalid("the output token limit is out of range")
	}
	thinking := DefaultThinkingAllowance
	if cfg.ThinkingAllowance != nil {
		thinking = *cfg.ThinkingAllowance
	}
	if thinking < 0 || thinking > MaxThinkingAllowance {
		return nil, invalid("the thinking allowance is out of range")
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
	attemptTimeout := cmpOr(cfg.AttemptTimeout, DefaultAttemptTimeout)
	guard := openaiwire.Guard{InCluster: slices.Clone(cfg.InClusterRanges)}
	p := &Provider{
		url:               generateURL(project, location, cfg.Model),
		model:             cfg.Model,
		tokens:            cfg.Tokens,
		client:            newHTTPClient(h.transport, attemptTimeout, guard),
		maxOutput:         cmpOr(cfg.MaxOutputTokens, DefaultMaxOutputTokens),
		thinkingAllowance: thinking,
		attempts:          cmpOr(cfg.MaxAttempts, DefaultMaxAttempts),
		base:              cmpOr(cfg.BaseDelay, DefaultBaseDelay),
		maxDelay:          cmpOr(cfg.MaxDelay, DefaultMaxDelay),
		timeout:           cmpOr(cfg.Timeout, DefaultTimeout),
		attemptTimeout:    attemptTimeout,
		maxBody:           cmpOr(cfg.MaxResponseBytes, DefaultMaxResponseBytes),
		onCall:            cfg.OnCall,
		sleep:             h.sleep,
		jitter:            h.jitter,
		now:               h.now,
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

// parseEndpoint checks an endpoint and returns its project and location. The returned error text is fixed.
func parseEndpoint(endpoint string) (project, location string, err error) {
	if len(endpoint) > maxEndpointBytes {
		return "", "", errors.New("the endpoint is too long")
	}
	// An empty query or fragment ("?" or "#" alone) leaves no trace in url.URL, so the text is checked too.
	u, err := url.Parse(endpoint)
	if err != nil || strings.ContainsAny(endpoint, "?#") || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || u.RawPath != "" {
		return "", "", errors.New("the endpoint is not an HTTPS URL without user info, query, fragment or escapes")
	}
	host := u.Host
	if _, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]")); err == nil {
		return "", "", errors.New("the endpoint host is an IP address, not the regional host name")
	}
	if host != u.Hostname() {
		return "", "", errors.New("the endpoint has a port")
	}
	switch {
	case host == "aiplatform.googleapis.com":
		return "", "", errors.New("the global endpoint is refused: it does not keep processing in one region")
	case strings.HasSuffix(host, ".rep.googleapis.com"):
		return "", "", errors.New("the multi-region endpoint is refused: it does not keep processing in one region")
	case !strings.HasSuffix(host, hostSuffix):
		return "", "", errors.New("the endpoint host is not a regional Vertex AI host")
	}
	location = strings.TrimSuffix(host, hostSuffix)
	if !euLocations[location] {
		return "", "", errors.New("the endpoint location is not an EU region this adapter allows")
	}
	parts := strings.Split(strings.TrimSuffix(u.Path, "/"), "/")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "v1" || parts[2] != "projects" || parts[4] != "locations" {
		return "", "", errors.New("the endpoint path is not /v1/projects/{project}/locations/{location}")
	}
	if parts[5] != location {
		return "", "", errors.New("the location of the endpoint path is not the location of its host")
	}
	if !projectPattern.MatchString(parts[3]) {
		return "", "", errors.New("the endpoint project is not a project ID or number")
	}
	return parts[3], location, nil
}

// checkModel checks the model ID for the location. The returned error text is fixed.
func checkModel(model, location string) error {
	switch {
	case strings.HasPrefix(model, "gemini-3"):
		return errors.New("the model is a Gemini 3 model, which no EU region serves")
	case !modelPattern.MatchString(model):
		return errors.New("the model is not a Gemini 2.5 model ID")
	case noFlashLite[location] && strings.HasPrefix(model, "gemini-2.5-flash-lite"):
		return errors.New("the location does not serve Flash-Lite")
	}
	return nil
}

// generateURL builds the generateContent URL from the validated project, location and model.
func generateURL(project, location, model string) string {
	u := url.URL{
		Scheme: "https",
		Host:   location + hostSuffix,
		Path:   "/v1/projects/" + project + "/locations/" + location + "/publishers/google/models/" + model + ":generateContent",
	}
	return u.String()
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

// newHTTPClient is openaiwire.NewHTTPClient: no proxy, TLS 1.2 or later, no redirect, the guard on every
// dialed address, and headerTimeout for the wait for headers. Tests replace the transport.
func newHTTPClient(rt http.RoundTripper, headerTimeout time.Duration, guard openaiwire.Guard) *http.Client {
	c := openaiwire.NewHTTPClient(headerTimeout, guard)
	if rt != nil {
		c.Transport = rt
	}
	return c
}

// OutputSpend implements [providers.OutputBounder]: maxOutputTokens is the request's limit (or the default
// when it sets none) plus the thinking allowance, and thinking counts as output.
func (p *Provider) OutputSpend(maxOutputTokens int) int {
	if maxOutputTokens == 0 {
		maxOutputTokens = p.maxOutput
	}
	return maxOutputTokens + p.thinkingAllowance
}

// Complete sends one generateContent request, retrying transient failures. See the package documentation
// for the classes of its errors.
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
		return providers.Completion{}, 0, providers.NotSent(err)
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	// unknown stays set once an attempt was billed without a usage that reaches the caller.
	unknown := false
	// sent is set once any attempt handed its request to the HTTP client; until then nothing can be billed.
	sent := false
	done := func(c providers.Completion, attempts int, err error) (providers.Completion, int, error) {
		c.UsageUnknown = c.UsageUnknown || unknown
		if !sent {
			err = providers.NotSent(err)
		}
		return c, attempts, err
	}
	for attempt := 0; ; attempt++ {
		data, billed, attemptSent, err := p.once(ctx, body)
		sent = sent || attemptSent
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

// once makes one attempt and returns the body of a 200 reply. Every other outcome is a classified error with
// fixed text. billed is true once a 200 arrived before the attempt's deadline, error or not. A response that
// net/http returns after the attempt's context ended is the timeout whatever its status, and not billed. With
// an error, data is the body when it was read whole within the bound, so that its usage can still be read,
// and nil otherwise. sent is false only when the attempt ended before the request was handed to the HTTP
// client.
func (p *Provider) once(ctx context.Context, body []byte) (data []byte, billed, sent bool, err error) {
	if ctx.Err() != nil {
		return nil, false, false, ctxError(ctx)
	}
	actx, cancel := context.WithTimeout(ctx, p.attemptTimeout)
	defer cancel()
	tok, err := p.tokens.Token(actx)
	if err != nil {
		return nil, false, false, tokenError(ctx, err)
	}
	if err := checkToken(tok); err != nil {
		return nil, false, false, err
	}
	if actx.Err() != nil {
		// The deadline or the cancellation came while the token was fetched: nothing was handed to the client.
		return nil, false, false, transportError(ctx, actx.Err())
	}
	req, err := http.NewRequestWithContext(actx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return nil, false, false, errors.New("vertexai: could not build the request")
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", mimeJSON)
	req.Header.Set("Accept", mimeJSON)
	req.Header.Set("User-Agent", "shadewright")
	// From here on the request may have left, even when Do returns an error.
	resp, err := p.client.Do(req)
	if err != nil {
		// The error of http.Client.Do holds the URL and possibly response-controlled text: replace it.
		return nil, false, true, transportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if actx.Err() != nil {
		// A response that raced the end of the attempt's context: the deadline (or the caller's cancellation)
		// came first, so nothing of it is read.
		return nil, false, true, transportError(ctx, context.DeadlineExceeded)
	}
	if resp.StatusCode != http.StatusOK {
		// The body of an error may quote the request: it is drained, never read into an error.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
		return nil, false, true, statusError(resp, p.now())
	}
	// A 200 was billed. An oversized body is not parsed (a cut JSON body has no reliable usage).
	tooLarge := fmt.Errorf("vertexai: the reply is larger than %d bytes: %w", p.maxBody, providers.ErrTooLarge)
	if resp.ContentLength > p.maxBody {
		return nil, true, true, tooLarge
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, p.maxBody+1))
	if err != nil {
		return nil, true, true, transportError(ctx, err)
	}
	if int64(len(data)) > p.maxBody {
		return nil, true, true, tooLarge
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != mimeJSON {
		return data, true, true, fmt.Errorf("vertexai: the reply is not JSON: %w", providers.ErrMalformedOutput)
	}
	return data, true, true, nil
}

// checkToken checks an access token before it is sent: nonempty, bounded, and only characters a header value
// carries without folding (visible ASCII). Google access tokens are opaque, so nothing else is read from it.
// Errors are fixed text matching providers.ErrForbidden: the token is the identity's credential, so a wrong
// one is fixed by a person on the identity, like a 401 or 403.
func checkToken(tok string) error {
	refuse := func(msg string) error { return fmt.Errorf("vertexai: %s: %w", msg, providers.ErrForbidden) }
	if tok == "" || len(tok) > maxTokenBytes {
		return refuse("the access token is empty or too large")
	}
	for i := range len(tok) {
		if c := tok[i]; c <= ' ' || c > '~' {
			return refuse("the access token holds a character a header cannot carry")
		}
	}
	return nil
}

// statusError classifies a status other than 200.
func statusError(resp *http.Response, now time.Time) error {
	code := resp.StatusCode
	msg := fmt.Errorf("vertexai: the endpoint returned status %d", code)
	switch {
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500 && code <= 599:
		after := retryafter.Parse(resp.Header.Get("Retry-After"), now, retryAfterCeiling)
		return &providers.RetryableError{RetryAfter: after, Err: msg}
	case code >= 300 && code <= 399:
		return fmt.Errorf("%w: redirects are not followed: %w", msg, providers.ErrRejected)
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf("%w: %w", msg, providers.ErrForbidden)
	case code == http.StatusNotFound:
		// An unknown model, or one the project cannot use here: there is no fallback to another one.
		return fmt.Errorf("%w: the model is not available: %w", msg, providers.ErrNotFound)
	case code == http.StatusRequestEntityTooLarge:
		return fmt.Errorf("%w: %w", msg, providers.ErrTooLarge)
	}
	return fmt.Errorf("%w: %w", msg, providers.ErrRejected)
}

// transportError replaces an error of the HTTP client, which may hold the URL or response text, with fixed
// text and its class. A timeout of one attempt, while the call's context is still alive, is retryable.
func transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctxError(ctx)
	}
	if errors.Is(err, openaiwire.ErrDialRefused) {
		return fmt.Errorf("vertexai: the endpoint resolves to an address that is not public: %w", providers.ErrInvalidConfig)
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		return &providers.RetryableError{Err: errors.New("vertexai: the model request timed out")}
	}
	return &providers.RetryableError{Err: errors.New("vertexai: the model request failed (network)")}
}

// tokenError replaces an error of the token source with fixed text and its class. Nothing of err stays
// reachable: it may carry a URL, an assertion or a credential.
func tokenError(ctx context.Context, err error) error {
	const msg = "vertexai: no access token"
	switch {
	case ctx.Err() != nil:
		return ctxError(ctx)
	case providers.IsForbidden(err), providers.IsInvalidConfig(err):
		return fmt.Errorf("%s: %w", msg, providers.ErrForbidden)
	case providers.IsRetryable(err), errors.Is(err, context.DeadlineExceeded):
		after, _ := providers.RetryAfter(err)
		return &providers.RetryableError{RetryAfter: after, Err: errors.New(msg)}
	}
	return errors.New(msg)
}

// ctxError reports the end of the call's context. A deadline is retryable (a later attempt may finish); a
// cancellation is not.
func ctxError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &providers.RetryableError{Err: fmt.Errorf("vertexai: model call: %w", context.DeadlineExceeded)}
	}
	return fmt.Errorf("vertexai: model call: %w", context.Canceled)
}
