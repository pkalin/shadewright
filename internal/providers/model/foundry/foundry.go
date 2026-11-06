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

package foundry

import (
	"bytes"
	"context"
	"crypto/tls"
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
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/retryafter"
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
	// maxRequestBytes bounds the encoded request. The context the analyzer sends is at most 256 KiB
	// (spec.analysis.maxContextBytes), so anything near this is a caller's bug.
	maxRequestBytes = 4 << 20
	// maxConfigDelay, maxConfigTimeout and maxConfigResponseBytes bound what Config may ask for.
	maxConfigDelay         = 5 * time.Minute
	maxConfigTimeout       = 15 * time.Minute
	maxConfigResponseBytes = 16 << 20
	// maxDrain is how much of an error body is read and discarded, so the connection can be reused.
	maxDrain = 64 << 10
)

// cognitiveServicesScope is the only scope a token is requested for, and its resource the only audience a
// token sent to the endpoint may have.
const (
	cognitiveServicesScope    = "https://cognitiveservices.azure.com/.default"
	cognitiveServicesAudience = "https://cognitiveservices.azure.com"
)

// TokenSource returns Entra ID access tokens for the Cognitive Services audience. [WorkloadIdentity]
// implements it. Its errors are never passed on: only their class (canceled, deadline, retryable,
// forbidden) is kept.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

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

// Config configures a [Provider]. Endpoint, Deployment and APIVersion are the fields of one ModelDeployment
// in RemediationSettings.
type Config struct {
	// Endpoint is the HTTPS base URL of the inference endpoint, for example https://<resource>.services.ai.azure.com/
	// or https://<resource>.openai.azure.com/openai/v1. No user info, query or fragment.
	Endpoint string
	// Deployment is the name of the model deployment.
	Deployment string
	// APIVersion is v1 or a dated version such as 2024-10-21, optionally with -preview.
	APIVersion string
	// Tokens returns the access tokens. Required.
	Tokens TokenSource
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

// Provider is a [providers.ModelProvider] for one Foundry deployment. It is safe for concurrent use.
type Provider struct {
	url        string
	deployment string
	// sendModel names the deployment in the body (API version v1).
	sendModel      bool
	tokens         TokenSource
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
var (
	deploymentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	apiVersionPattern = regexp.MustCompile(`^(v1|[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])(-preview)?)$`)
)

// New validates cfg and returns a Provider. It makes no network call. Errors match
// [providers.ErrInvalidConfig] and never repeat the input.
func New(cfg Config) (*Provider, error) {
	return newProvider(cfg, testHooks{})
}

func newProvider(cfg Config, h testHooks) (*Provider, error) {
	invalid := func(msg string) error { return fmt.Errorf("foundry: %s: %w", msg, providers.ErrInvalidConfig) }
	if cfg.Tokens == nil {
		return nil, invalid("a token source is required")
	}
	if !deploymentPattern.MatchString(cfg.Deployment) {
		return nil, invalid("the deployment name is empty or invalid")
	}
	if !apiVersionPattern.MatchString(cfg.APIVersion) {
		return nil, invalid("the API version is not v1 or a date")
	}
	u, err := chatURL(cfg.Endpoint, cfg.Deployment, cfg.APIVersion)
	if err != nil {
		return nil, invalid(err.Error())
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
		url:            u,
		deployment:     cfg.Deployment,
		sendModel:      cfg.APIVersion == "v1",
		tokens:         cfg.Tokens,
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

// chatURL builds the chat completions URL from typed parts. The returned error text is fixed.
func chatURL(endpoint, deployment, apiVersion string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.Opaque != "" || len(endpoint) > 256 {
		return "", errors.New("the endpoint is not an HTTPS URL without user info, query or fragment")
	}
	// A DNS host name only (threat model G3.9): an IP literal, or a numeric form some resolvers read as one,
	// would skip the analyzer's host allow-list. The last label must start with a letter, as in the CRD pattern.
	host := u.Hostname()
	if _, err := netip.ParseAddr(host); err == nil {
		return "", errors.New("the endpoint host is an IP address, not a DNS name")
	}
	if last := host[strings.LastIndexByte(host, '.')+1:]; last == "" || !isASCIILetter(last[0]) {
		return "", errors.New("the endpoint host is not a DNS name")
	}
	base := strings.TrimSuffix(u.Path, "/")
	v1Base := strings.HasSuffix(base, "/openai/v1")
	switch {
	case apiVersion == "v1" && v1Base:
		u.Path = base + "/chat/completions"
	case apiVersion == "v1":
		u.Path = base + "/openai/v1/chat/completions"
	case v1Base:
		return "", errors.New("an endpoint ending in /openai/v1 needs API version v1")
	default:
		u.Path = base + "/openai/deployments/" + deployment + "/chat/completions"
		u.RawQuery = url.Values{"api-version": {apiVersion}}.Encode()
	}
	u.RawPath = ""
	return u.String(), nil
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

// errDialRefused marks a connection refused by refusePrivate.
var errDialRefused = errors.New("the address is not a public unicast address")

// refusePrivate is the dialer's control: it runs on the resolved address of every connection, so a host name
// that resolves (or is rebound) to an address that is not public unicast gets no connection and no token
// (design 11.1). See publicAddress.
func refusePrivate(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errDialRefused
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" || !publicAddress(ip) {
		return errDialRefused
	}
	return nil
}

// publicAddress reports whether ip is a public unicast address. IPv4 (also mapped into IPv6) must be global
// unicast, not private and outside reservedIPv4. An IPv6 address that embeds an IPv4 address the standard way
// (NAT64 64:ff9b::/96, 6to4 2002::/16, IPv4-compatible ::/96) is judged by that IPv4 address, so a DNS64
// answer for a public endpoint still connects while one for 10.0.0.1 does not. Any other IPv6 address must be
// global unicast, not unique-local and outside reservedIPv6, which refuses outright the forms whose IPv4
// cannot be read reliably (local-use NAT64 64:ff9b:1::/48, Teredo 2001::/32, SIIT ::ffff:0:0:0/96).
func publicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Is4() {
		return ip.IsGlobalUnicast() && !ip.IsPrivate() && !inAny(reservedIPv4, ip)
	}
	b := ip.As16()
	switch {
	case nat64WellKnown.Contains(ip), ipv4Compatible.Contains(ip):
		return publicAddress(netip.AddrFrom4([4]byte(b[12:16])))
	case sixToFour.Contains(ip):
		return publicAddress(netip.AddrFrom4([4]byte(b[2:6])))
	}
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !inAny(reservedIPv6, ip)
}

func inAny(prefixes []netip.Prefix, ip netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// Address ranges refused besides those the netip predicates cover (loopback, private, link-local, multicast,
// unspecified, unique-local).
var (
	nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")
	ipv4Compatible = netip.MustParsePrefix("::/96")
	sixToFour      = netip.MustParsePrefix("2002::/16")
	reservedIPv4   = mustPrefixes(
		"0.0.0.0/8",       // this network
		"100.64.0.0/10",   // shared address space (carrier-grade NAT, some cluster networks)
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"192.88.99.0/24",  // 6to4 relay anycast
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"240.0.0.0/4",     // reserved, and broadcast
	)
	reservedIPv6 = mustPrefixes(
		"::/8",          // reserved, after the embedded forms above: SIIT, local-use NAT64 64:ff9b:1::/48
		"100::/64",      // discard
		"2001::/23",     // IETF protocol assignments, Teredo 2001::/32 included
		"2001:db8::/32", // documentation
		"3fff::/20",     // documentation
		"fec0::/10",     // deprecated site-local
	)
)

func mustPrefixes(s ...string) []netip.Prefix {
	ps := make([]netip.Prefix, len(s))
	for i, p := range s {
		ps[i] = netip.MustParsePrefix(p)
	}
	return ps
}

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// newHTTPClient never follows redirects (the bearer token must reach the configured host only), requires
// TLS 1.2 or later, connects to public unicast addresses only (refusePrivate), and bounds connection setup
// and the wait for headers (headerTimeout, the attempt timeout of the caller). Each attempt has its own
// deadline on its context.
func newHTTPClient(rt http.RoundTripper, headerTimeout time.Duration) *http.Client {
	if rt == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = nil
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		t.TLSHandshakeTimeout = 10 * time.Second
		t.ResponseHeaderTimeout = headerTimeout
		t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: refusePrivate}).DialContext
		rt = t
	}
	return &http.Client{
		Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Complete sends one chat completion request, retrying transient failures. See the package documentation
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
			c, perr := parseResponse(data, offered, len(req.ResponseSchema) > 0)
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

// once sends one attempt and returns the body of a 200 reply. Every other outcome is a classified error with
// fixed text. billed is true once a 200 arrived, error or not: the model answered and the service bills it.
// With an error, data is the body when it was read whole within the bound (a Content-Type other than JSON),
// so that its usage can still be read, and nil otherwise.
func (p *Provider) once(ctx context.Context, body []byte) (data []byte, billed bool, err error) {
	if ctx.Err() != nil {
		return nil, false, ctxError(ctx)
	}
	tok, err := p.tokens.Token(ctx)
	if err != nil {
		return nil, false, tokenError(ctx, err)
	}
	if err := checkToken(tok); err != nil {
		return nil, false, err
	}
	actx, cancel := context.WithTimeout(ctx, p.attemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return nil, false, errors.New("foundry: could not build the request")
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "shadewright")
	resp, err := p.client.Do(req)
	if err != nil {
		// The error of http.Client.Do holds the URL and possibly response-controlled text: replace it.
		return nil, false, transportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	code := resp.StatusCode
	if code != http.StatusOK {
		// The body of an error may quote the request: it is drained, never read into an error.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
		return nil, false, p.statusError(resp)
	}
	// A 200 was billed. An oversized body is not parsed (a cut JSON body has no reliable usage): its usage
	// is unknown.
	tooLarge := fmt.Errorf("foundry: the reply is larger than %d bytes: %w", p.maxBody, providers.ErrTooLarge)
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
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	isJSON := err == nil && mt == "application/json"
	// net/http returns a reply that raced the end of the attempt's context (#438), such as the empty 200 a
	// server sends when its handler gives up on the client going away. Past the deadline, a reply that is
	// not whole JSON is the timeout, retryable, not malformed model output.
	if actx.Err() != nil && (!isJSON || !json.Valid(data)) {
		return data, true, transportError(ctx, context.DeadlineExceeded)
	}
	if !isJSON {
		return data, true, fmt.Errorf("foundry: the reply is not JSON: %w", providers.ErrMalformedOutput)
	}
	return data, true, nil
}

// statusError classifies a status other than 200.
func (p *Provider) statusError(resp *http.Response) error {
	code := resp.StatusCode
	msg := fmt.Errorf("foundry: the model endpoint returned status %d", code)
	switch {
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500 && code <= 599:
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

// retryAfter reads the delay Azure asks for: retry-after-ms first, then Retry-After.
func (p *Provider) retryAfter(h http.Header) time.Duration {
	// Asked-for delays longer than this are capped here; anything above maxDelay is returned to the caller.
	const ceiling = 10 * time.Minute
	if v := strings.TrimSpace(h.Get("retry-after-ms")); v != "" {
		if ms, err := strconv.ParseInt(v, 10, 64); err == nil && ms > 0 {
			return time.Duration(min(ms, ceiling.Milliseconds())) * time.Millisecond
		}
	}
	return retryafter.Parse(h.Get("Retry-After"), p.now(), ceiling)
}

// transportError replaces an error of the HTTP client, which may hold the URL or response text, with fixed
// text and its class. A timeout of one attempt, while the call's context is still alive, is retryable.
func transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctxError(ctx)
	}
	if errors.Is(err, errDialRefused) {
		return fmt.Errorf("foundry: the endpoint resolves to an address that is not public: %w", providers.ErrInvalidConfig)
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		return &providers.RetryableError{Err: errors.New("foundry: the model request timed out")}
	}
	return &providers.RetryableError{Err: errors.New("foundry: the model request failed (network)")}
}

// tokenError replaces an error of the token source with fixed text and its class. Nothing of err stays
// reachable: it may carry a URL, SDK text or a credential.
func tokenError(ctx context.Context, err error) error {
	const msg = "foundry: no access token"
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
		return &providers.RetryableError{Err: fmt.Errorf("foundry: model call: %w", context.DeadlineExceeded)}
	}
	return fmt.Errorf("foundry: model call: %w", context.Canceled)
}
