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

package openaicompatible

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/model/openaiwire"
)

// Defaults and bounds of [Config].
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
	// DefaultMaxOutputTokens is sent when neither the request nor Config sets a limit; it is the default of
	// spec.analysis.maxOutputTokens.
	DefaultMaxOutputTokens = 2048
	// MaxOutputTokens is the highest output limit a request may ask for, the maximum of
	// spec.analysis.maxOutputTokens.
	MaxOutputTokens = 16384
	// DefaultMaxResponseBytes bounds the body of a reply that is read. 16384 output tokens are about 64 KiB
	// of text, so a larger reply is not a chat completion of this adapter's requests.
	DefaultMaxResponseBytes = 1 << 20
	// maxConfigDelay, maxConfigTimeout and maxConfigResponseBytes bound what Config may ask for.
	maxConfigDelay         = 5 * time.Minute
	maxConfigTimeout       = 15 * time.Minute
	maxConfigResponseBytes = 16 << 20
	// maxDrain is how much of an error body is read and discarded, so the connection can be reused.
	maxDrain = 64 << 10
)

// CallReport describes one Complete call, for metrics and cost accounting. It holds no prompt, reply text,
// token or URL.
type CallReport struct {
	// Deployment is the configured deployment name.
	Deployment string
	// Model is the model and version that answered, as the reply named it; empty without a usable reply.
	Model string
	// Usage is the token usage of the reply; zero without a usable reply. An attempt that failed before an
	// answer arrived (a network error, a timeout, a status other than 200) may have been billed too, which
	// the service does not report.
	Usage providers.Usage
	// UsageUnknown is set when an attempt got a 200 answer, so was billed, whose usage could not be read: a
	// body larger than Config.MaxResponseBytes (not parsed, since a cut JSON body is not safe to read), a
	// read that failed, or a body without a usable usage block. The budget counts such a call
	// conservatively (#208).
	UsageUnknown bool
	// Attempts is the number of requests sent (or tried) in this call.
	Attempts int
	// Duration is the time the call took, backoff included.
	Duration time.Duration
	// Err is the error Complete returned, nil on success.
	Err error
}

// API names an OpenAI wire format.
type API = openaiwire.API

// The supported inference APIs.
const (
	APIChatCompletions = openaiwire.APIChatCompletions
	APIResponses       = openaiwire.APIResponses
)

// Config configures one endpoint and model, with an optional endpoint-bound key (ADR-0042).
type Config struct {
	// Endpoint is an HTTPS API base, for example https://api.openai.com/v1. This is the declared installation base, never a settings URL.
	Endpoint string
	// Deployment is the name of the model deployment.
	Deployment string
	// KeyFile is an optional API key file, read at each attempt; never a credential in settings.
	KeyFile string
	// Service permits private unicast addresses for this declared Service endpoint.
	Service *Service
	// InClusterRanges are the declared in-cluster ranges outside the private ones (ParseInClusterRanges). A
	// public endpoint whose name resolves into one is refused at dial time, before any byte is sent. Not used
	// for a Service endpoint, which may dial private addresses.
	InClusterRanges []netip.Prefix
	// API is ChatCompletions (the default) or Responses.
	API API
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
	dialer    func(*net.Dialer)
	sleep     func(context.Context, time.Duration) error
	jitter    func(time.Duration) time.Duration
	now       func() time.Time
}

// Provider is a [providers.ModelProvider] for one OpenAI-compatible endpoint and model. It is safe for concurrent use.
type Provider struct {
	url        string
	deployment string
	// api is the inference API: APIChatCompletions or APIResponses.
	api            API
	keyFile        string
	guard          openaiwire.Guard
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

// Patterns of the RemediationSettings ModelDeployment fields, checked again here because a Provider may be
// built from other input.
var deploymentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// New validates cfg and returns a Provider. It makes no network call. Errors match
// [providers.ErrInvalidConfig] and never repeat the input.
func New(cfg Config) (*Provider, error) {
	return newProvider(cfg, testHooks{})
}

func newProvider(cfg Config, h testHooks) (*Provider, error) {
	invalid := func(msg string) error {
		return fmt.Errorf("openai-compatible: %s: %w", msg, providers.ErrInvalidConfig)
	}
	if !deploymentPattern.MatchString(cfg.Deployment) {
		return nil, invalid("the model name is empty or invalid")
	}
	api := cfg.API
	if api == "" {
		api = APIChatCompletions
	}
	u, err := endpointURL(cfg.Endpoint, api)
	if err != nil {
		return nil, err
	}
	if err := ValidateEndpoints([]Endpoint{{URL: cfg.Endpoint, API: api, Models: []string{cfg.Deployment}, KeyFile: cfg.KeyFile, Service: cfg.Service}}, nil); err != nil {
		return nil, invalid("the endpoint declaration is invalid")
	}
	if err := ValidateInClusterRanges(cfg.InClusterRanges); err != nil {
		return nil, invalid("an in-cluster range is invalid")
	}
	guard := openaiwire.Guard{AllowPrivateService: cfg.Service != nil, InCluster: slices.Clone(cfg.InClusterRanges)}
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
		url:            u.String(),
		deployment:     cfg.Deployment,
		api:            api,
		keyFile:        cfg.KeyFile,
		guard:          guard,
		client:         openaiwire.NewHTTPClient(cmpOr(cfg.AttemptTimeout, DefaultAttemptTimeout), guard),
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
	if h.transport != nil {
		p.client.Transport = h.transport
	}
	if h.dialer != nil {
		// Tests give the guarded dialer a resolver and watch its control; the guard is the provider's own.
		d := p.guard.Dialer()
		h.dialer(d)
		p.client.Transport.(*http.Transport).DialContext = d.DialContext
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

// Complete sends one inference request, retrying transient failures. See the package documentation
// for the classes of its errors.
func (p *Provider) Complete(ctx context.Context, req providers.CompletionRequest) (providers.Completion, error) {
	start := p.now()
	c, attempts, err := p.complete(ctx, req)
	if p.onCall != nil {
		p.onCall(CallReport{
			Deployment: p.deployment, Model: c.Model, Usage: c.Usage, UsageUnknown: c.UsageUnknown,
			Attempts: attempts, Duration: p.now().Sub(start), Err: err,
		})
	}
	return c, err
}

func (p *Provider) complete(ctx context.Context, req providers.CompletionRequest) (providers.Completion, int, error) {
	body, offered, err := (openaiwire.Codec{API: p.api, Deployment: p.deployment, SendModel: true, MaxOutput: p.maxOutput}).Encode(req)
	if err != nil {
		return providers.Completion{}, 0, providers.NotSent(compatibleWireError(err))
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	usageCeiling := providers.CompatibleUsageCeiling(providers.EstimateInputTokens(req), int64(cmpOr(req.MaxOutputTokens, p.maxOutput)))
	// unknown stays set once an attempt was billed without a usage that reaches the caller.
	unknown := false
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
			c, perr := p.parse(data, offered, len(req.ResponseSchema) > 0)
			if !completeUsage(data, p.api) || c.Usage.InputTokens > usageCeiling || c.Usage.OutputTokens > usageCeiling-c.Usage.InputTokens {
				c = providers.Completion{UsageUnknown: true}
				perr = fmt.Errorf("openai-compatible: the reply has no complete bounded usage: %w", providers.ErrMalformedOutput)
			}
			return done(c, attempt+1, perr)
		}
		var c providers.Completion
		if billed {
			c = p.billedUsage(data)
			if c.Usage.InputTokens > usageCeiling || c.Usage.OutputTokens > usageCeiling-c.Usage.InputTokens {
				c = providers.Completion{UsageUnknown: true}
			}
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

// parse is parseResponse or parseResponses, after the Provider's API.
func (p *Provider) parse(data []byte, offered map[string]bool, structured bool) (providers.Completion, error) {
	if p.api == APIResponses {
		c, err := openaiwire.ParseResponses(data, offered, structured)
		return c, compatibleWireError(err)
	}
	c, err := openaiwire.ParseResponse(data, offered, structured)
	return c, compatibleWireError(err)
}

// billedUsage is billedUsage or billedResponsesUsage, after the Provider's API.
func (p *Provider) billedUsage(data []byte) providers.Completion {
	if !completeUsage(data, p.api) {
		return providers.Completion{UsageUnknown: true}
	}
	if p.api == APIResponses {
		return openaiwire.BilledResponsesUsage(data)
	}
	return openaiwire.BilledUsage(data)
}

// once sends one attempt and returns the body of a 200 reply. Every other outcome is a classified error with
// fixed text. billed is true once a 200 arrived before the attempt's deadline, error or not: the model
// answered and the service bills it. A response that net/http returns after the attempt's context ended is
// the timeout whatever its status, and not billed (#438). With an error, data is the body when it was read
// whole within the bound (a Content-Type other than JSON), so that its usage can still be read, and nil
// otherwise.
func (p *Provider) once(ctx context.Context, body []byte) (data []byte, billed, sent bool, err error) {
	if ctx.Err() != nil {
		return nil, false, false, ctxError(ctx)
	}
	key, err := p.readKey()
	if err != nil {
		return nil, false, false, err
	}
	actx, cancel := context.WithTimeout(ctx, p.attemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return nil, false, false, errors.New("openai-compatible: could not build the request")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "shadewright")
	resp, err := p.client.Do(req)
	if err != nil {
		// The error of http.Client.Do holds the URL and possibly response-controlled text: replace it.
		return nil, false, true, transportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if actx.Err() != nil {
		// net/http returns a response that raced the end of the request's context, such as the empty 200 a
		// server sends when its handler gives up on the client going away (#438). The deadline came first:
		// it is the timeout, retryable, never malformed output or a refusal, and nothing of it is read.
		return nil, false, true, transportError(ctx, context.DeadlineExceeded)
	}

	code := resp.StatusCode
	if code != http.StatusOK {
		// The body of an error may quote the request: it is drained, never read into an error.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
		return nil, false, true, p.statusError(resp)
	}
	// A 200 was billed. An oversized body is not parsed (a cut JSON body has no reliable usage): its usage
	// is unknown.
	tooLarge := fmt.Errorf("openai-compatible: the reply is larger than %d bytes: %w", p.maxBody, providers.ErrTooLarge)
	if resp.ContentLength > p.maxBody {
		return nil, true, true, tooLarge
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, p.maxBody+1))
	if err != nil {
		return nil, true, true, transportError(ctx, err)
	}
	if actx.Err() != nil {
		return nil, true, true, transportError(ctx, actx.Err())
	}
	if int64(len(data)) > p.maxBody {
		return nil, true, true, tooLarge
	}
	if key != "" {
		// Both texts are fixed and both are malformed output, retried the same way. Only the first says a
		// key was seen; the second is a reply the caller's decode would refuse anyway.
		switch scanReply(data, p.api, key) {
		case scanKey:
			return nil, true, true, fmt.Errorf("openai-compatible: the reply contains a credential: %w", providers.ErrMalformedOutput)
		case scanWrongType:
			return nil, true, true, fmt.Errorf("openai-compatible: the reply has a field of the wrong type: %w", providers.ErrMalformedOutput)
		}
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return data, true, true, fmt.Errorf("openai-compatible: the reply is not JSON: %w", providers.ErrMalformedOutput)
	}
	return data, true, true, nil
}

// statusError classifies a status other than 200.
func (p *Provider) statusError(resp *http.Response) error {
	code := resp.StatusCode
	msg := fmt.Errorf("openai-compatible: the model endpoint returned status %d", code)
	switch {
	case code == http.StatusTooManyRequests:
		return &providers.RetryableError{RetryAfter: p.retryAfter(resp.Header), Err: msg}
	case code == http.StatusRequestTimeout || code >= 500 && code <= 599:
		return &providers.RetryableError{RetryAfter: p.retryAfter(resp.Header), Err: msg}
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf("%w: %w", msg, providers.ErrForbidden)
	case code == http.StatusNotFound:
		// An unknown or removed deployment: there is no fallback to another one (#80).
		return fmt.Errorf("%w: the deployment is not available: %w", msg, providers.ErrNotFound)
	case code == http.StatusRequestEntityTooLarge:
		return fmt.Errorf("%w: %w", msg, providers.ErrTooLarge)
	}
	return fmt.Errorf("%w: %w", msg, providers.ErrRejected)
}

// retryAfter reads relative delays only. HTTP dates are ignored to avoid comparing different clocks.
func (p *Provider) retryAfter(h http.Header) time.Duration {
	// Asked-for delays longer than this are capped here; anything above maxDelay is returned to the caller.
	const ceiling = 10 * time.Minute
	if v := strings.TrimSpace(h.Get("retry-after-ms")); v != "" {
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil && ms > 0 {
			return time.Duration(min(ms, ceiling.Milliseconds())) * time.Millisecond
		}
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(h.Get("Retry-After")), 10, 64)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(min(secs, int64(ceiling/time.Second))) * time.Second
}

// transportError replaces an error of the HTTP client, which may hold the URL or response text, with fixed
// text and its class. A timeout of one attempt, while the call's context is still alive, is retryable.
func transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctxError(ctx)
	}
	if errors.Is(err, openaiwire.ErrDialRefused) {
		return fmt.Errorf("openai-compatible: the endpoint resolves to an address that is not public or is in-cluster: %w", providers.ErrInvalidConfig)
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		return &providers.RetryableError{Err: errors.New("openai-compatible: the model request timed out")}
	}
	return &providers.RetryableError{Err: errors.New("openai-compatible: the model request failed (network)")}
}

// ctxError reports the end of the call's context. A deadline is retryable (a later attempt may finish); a
// cancellation is not.
func ctxError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &providers.RetryableError{Err: fmt.Errorf("openai-compatible: model call: %w", context.DeadlineExceeded)}
	}
	return fmt.Errorf("openai-compatible: model call: %w", context.Canceled)
}

// serviceHost recognizes the exact DNS forms permitted for a declared Service endpoint.
func serviceHost(host string) bool {
	labels := strings.Split(host, ".")
	service := len(labels) == 3 && labels[2] == "svc"
	fullyQualified := len(labels) == 5 && strings.Join(labels[2:], ".") == "svc.cluster.local"
	return (service || fullyQualified) && len(labels[0]) <= 63 && len(labels[1]) <= 63 && dnsLabel.MatchString(labels[0]) && dnsLabel.MatchString(labels[1])
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
var endpointHostPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
var endpointPathPattern = regexp.MustCompile(`^(/[A-Za-z0-9_~-][A-Za-z0-9._~-]*)*/?$`)

func endpointURL(endpoint string, api API) (*url.URL, error) {
	base, err := CanonicalBase(endpoint)
	if err != nil {
		return nil, fmt.Errorf("openai-compatible: invalid endpoint: %w", providers.ErrInvalidConfig)
	}
	u, _ := url.Parse(base)
	if ReservedHost(u.Hostname()) {
		return nil, fmt.Errorf("openai-compatible: reserved endpoint host: %w", providers.ErrInvalidConfig)
	}
	switch api {
	case APIChatCompletions:
		return u.JoinPath("chat/completions"), nil
	case APIResponses:
		return u.JoinPath("responses"), nil
	default:
		return nil, fmt.Errorf("openai-compatible: invalid inference API: %w", providers.ErrInvalidConfig)
	}
}

// ReservedHost keeps compatible credentials away from Anthropic and Microsoft's model hosts.
func ReservedHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "api.anthropic.com" {
		return true
	}
	for _, suffix := range []string{"openai.azure.com", "cognitiveservices.azure.com", "services.ai.azure.com"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func (p *Provider) readKey() (string, error) {
	if p.keyFile == "" {
		return "", nil
	}
	unavailable := func() error {
		return &providers.RetryableError{Err: fmt.Errorf("openai-compatible: the API key file is unavailable: %w", providers.ErrCredentialUnavailable)}
	}
	malformed := func() error {
		return fmt.Errorf("openai-compatible: the API key file is malformed: %w", providers.ErrCredentialUnavailable)
	}
	f, err := os.OpenFile(p.keyFile, os.O_RDONLY|syscall.O_NONBLOCK, 0) // #nosec G304 -- absolute install-time credential path, never cluster or model input
	if err != nil {
		return "", unavailable()
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return "", unavailable()
	}
	if !st.Mode().IsRegular() {
		return "", malformed()
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", unavailable()
	}
	if len(data) > 4096 {
		return "", malformed()
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", unavailable()
	}
	if len(key) < 16 {
		return "", fmt.Errorf("openai-compatible: the API key file is malformed (key must be at least 16 bytes): %w", providers.ErrCredentialUnavailable)
	}
	if len(key) > 512 {
		return "", malformed()
	}
	for i := range len(key) {
		if key[i] <= ' ' || key[i] > '~' {
			return "", malformed()
		}
	}
	return key, nil
}

// containsKey reports whether the key is in data, read as one JSON document (or as raw bytes when data is
// not valid JSON), by the allow-list view of [keyScan]. It does not check the shape of a reply.
func containsKey(data []byte, key string) bool {
	found, _ := keyIn(data, key)
	return found
}

// completeUsage requires both counts even on Chat Completions. Foundry's historical parser stays unchanged.
func completeUsage(data []byte, api API) bool {
	// Details are objects, so decode counts apart from them.
	var raw struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(data, &raw) != nil {
		return false
	}
	names := []string{"prompt_tokens", "completion_tokens"}
	if api == APIResponses {
		names = []string{"input_tokens", "output_tokens"}
	}
	for _, name := range names {
		var n *int64
		if json.Unmarshal(raw.Usage[name], &n) != nil || n == nil || *n < 0 || *n > 1<<40 {
			return false
		}
	}
	return true
}

// scanResult is what scanReply found.
type scanResult int

const (
	scanClean scanResult = iota
	// scanKey: the key is in the reply.
	scanKey
	// scanWrongType: a field of the caller's decode has the wrong JSON type, so the joined scans could
	// not run; the caller's decode refuses the same reply.
	scanWrongType
)

// replyContainsKey reports whether scanReply refuses the reply, for either reason.
func replyContainsKey(data []byte, api API, key string) bool {
	return scanReply(data, api, key) != scanClean
}

// scanReply checks a reply for the key with the allow-list view of [keyScan], then checks that the fields
// the caller's later decode reads (openaiwire's responsesReply or chatResponse) have that decode's JSON
// types, and scans those fields once more as the caller joins them. This check never changes the reply or
// repairs an invalid result.
//
// It fails closed. A reply that is not valid JSON is scanned as raw bytes (and the caller refuses it). A
// valid reply is scanned over every string value, object key and number, whatever field holds it. When the
// typed decode fails (a wrong type in one of the caller's fields), the caller's decode refuses the same reply
// too, so the reply is refused as scanWrongType: nothing a caller would accept is refused, and no decoy field
// can switch the scan of the joined fields off.
func scanReply(data []byte, api API, key string) scanResult {
	res, _ := scanReplyWork(data, api, key)
	return res
}

// scanReplyWork is scanReply that also returns its work count: the bytes the scan decoded, viewed and searched.
func scanReplyWork(data []byte, api API, key string) (scanResult, int) {
	found, work := keyIn(data, key)
	if found {
		return scanKey, work
	}
	if !json.Valid(data) {
		return scanClean, work
	}
	work += len(data)
	// The fields the caller reads, in wire order: the text parts and arguments of every output item, or the
	// content and arguments of every choice; and the message text the caller joins from output_text parts.
	var texts []string
	var messageText strings.Builder
	if api == APIResponses {
		var r struct {
			Output []struct {
				Type      string `json:"type"`
				Arguments string `json:"arguments"`
				Content   []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if json.Unmarshal(data, &r) != nil {
			return scanWrongType, work
		}
		for _, item := range r.Output {
			for _, part := range item.Content {
				if item.Type == "message" && part.Type == "output_text" {
					messageText.WriteString(part.Text)
				}
				texts = append(texts, part.Text)
			}
			texts = append(texts, item.Arguments)
		}
	} else {
		var r struct {
			Choices []struct {
				Message struct {
					Content   *string `json:"content"`
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(data, &r) != nil {
			return scanWrongType, work
		}
		for _, choice := range r.Choices {
			if choice.Message.Content != nil {
				texts = append(texts, *choice.Message.Content)
			}
			for _, call := range choice.Message.ToolCalls {
				texts = append(texts, call.Function.Arguments)
			}
		}
	}
	// One stream of the caller's fields, their JSON decoded in place (the reply's own field names and the
	// values of other fields are not between them), then the raw join and the message text as documents.
	joined := newKeyScan(key)
	var raw strings.Builder
	for _, text := range texts {
		raw.WriteString(text)
		if joined.nestedJSON(text) {
			joined.walk(text, 1)
		} else {
			joined.add(text, false, false)
		}
	}
	found = joined.result()
	work += joined.work
	for _, doc := range []string{raw.String(), messageText.String()} {
		f, w := keyIn([]byte(doc), key)
		found, work = found || f, work+w
	}
	return resultOf(found), work
}

func resultOf(b bool) scanResult {
	if b {
		return scanKey
	}
	return scanClean
}

// maxNestedJSON is how many levels of JSON text inside string values the scan decodes: a tool call's
// arguments and structured content are JSON inside a string of the reply, and the caller decodes them once.
// Deeper text is scanned as it is written, so the work stays linear in the size of the reply.
const maxNestedJSON = 3

// keyScan builds the allow-list view of a reply and searches it for the key. The view keeps only ASCII
// letters, digits and the punctuation characters the key itself contains, and drops every other character,
// of any category, assigned or not, so a character inserted between the key's characters does not hide it.
// The key is printable ASCII (readKey refuses anything else), so the key is its own view.
//
// Every decoded string value of the reply, in document order, is one stream, so a key split across fields,
// parts or tool-call arguments is found too. A second stream adds the object keys in their place. JSON text
// inside a string (up to maxNestedJSON levels) is decoded and its values take the string's place in both
// streams; its text as written is searched on its own as well.
//
// Terminal escape sequences are not treated apart. Model text is stored without control characters (every
// Unicode Cc code point except newline and tab, #852), so the visible remains of a sequence such as ESC [0m
// stay in stored text, and the view keeps the same letters and digits. The view drops a superset of what
// storage removes, so a key that is contiguous in stored text is contiguous in the view.
//
// A key that comes back re-encoded (base64, percent encoding, homoglyphs, reordered) is not found, and no
// check of this kind can find it: the endpoint is given the key at install and is trusted with it.
type keyScan struct {
	key   []byte
	allow [utf8.RuneSelf]bool
	// streams: the views of the string values, of the string values and object keys, and of each of the two
	// with the numbers of the document as well. A number must not interrupt a stream of strings, and a key
	// split into a number is found in the streams that hold them.
	streams [4][]byte
	scratch []byte
	found   bool
	work    int
}

// newKeyScan returns a scan for key with empty streams.
func newKeyScan(key string) *keyScan {
	k := &keyScan{key: []byte(key)}
	for c := range utf8.RuneSelf {
		k.allow[c] = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
	}
	for i := range len(key) {
		if key[i] < utf8.RuneSelf {
			k.allow[key[i]] = true
		}
	}
	return k
}

// keyIn reports whether the key is in data, read as one document, and the work the scan took.
func keyIn(data []byte, key string) (bool, int) {
	k := newKeyScan(key)
	// The document as written: the only view of one that is not JSON, and one more of one that is.
	k.searchAlone(string(data))
	if json.Valid(data) {
		k.work += len(data)
		k.walk(string(data), 0)
	}
	return k.result(), k.work
}

// result searches the streams and reports whether the key was found in them or on its own before.
func (k *keyScan) result() bool {
	for _, stream := range k.streams {
		k.work += len(stream)
		k.found = k.found || bytes.Contains(stream, k.key)
	}
	return k.found
}

// view appends the allow-list view of s to dst.
func (k *keyScan) view(dst []byte, s string) []byte {
	for i := range len(s) {
		if c := s[i]; c < utf8.RuneSelf && k.allow[c] {
			dst = append(dst, c)
		}
	}
	k.work += len(s)
	return dst
}

// searchAlone searches the view of s on its own, outside the streams.
func (k *keyScan) searchAlone(s string) {
	k.scratch = k.view(k.scratch[:0], s)
	k.work += len(k.scratch)
	k.found = k.found || bytes.Contains(k.scratch, k.key)
}

// add appends the view of s to the streams: an object key to the two streams with keys, a number to the two
// streams with numbers, a string value to all four.
func (k *keyScan) add(s string, objectKey, number bool) {
	for i := range k.streams {
		withKeys, withNumbers := i%2 == 1, i >= 2
		if objectKey && !withKeys || number && !withNumbers {
			continue
		}
		k.streams[i] = k.view(k.streams[i], s)
	}
}

// walk adds every string value, object key and number of the valid JSON text in document order.
func (k *keyScan) walk(text string, depth int) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	type frame struct{ object, wantKey bool }
	var stack []frame
	next := func() {
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].wantKey = !stack[n-1].wantKey
		}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return
		}
		switch v := tok.(type) {
		case json.Delim:
			if v == '}' || v == ']' {
				stack = stack[:len(stack)-1]
				continue
			}
			next()
			stack = append(stack, frame{object: v == '{', wantKey: true})
		case string:
			n := len(stack)
			objectKey := n > 0 && stack[n-1].object && stack[n-1].wantKey
			next()
			if !objectKey && depth < maxNestedJSON && k.nestedJSON(v) {
				k.searchAlone(v)
				k.work += len(v)
				k.walk(v, depth+1)
				continue
			}
			k.add(v, objectKey, false)
		case json.Number:
			next()
			k.add(v.String(), false, true)
		default:
			next()
		}
	}
}

// nestedJSON reports whether s is a JSON object, array or string.
func (k *keyScan) nestedJSON(s string) bool {
	t := strings.TrimLeft(s, " \t\r\n")
	if t == "" || t[0] != '{' && t[0] != '[' && t[0] != '"' {
		return false
	}
	k.work += len(s)
	return json.Valid([]byte(t))
}

// compatibleWireError attributes the shared codec's fixed error text without changing its classification.
func compatibleWireError(err error) error {
	if err == nil {
		return nil
	}
	return &wireError{err: err}
}

type wireError struct{ err error }

func (e *wireError) Error() string {
	return strings.ReplaceAll(e.err.Error(), "foundry:", "openai-compatible:")
}
func (e *wireError) Unwrap() error { return errors.Unwrap(e.err) }
