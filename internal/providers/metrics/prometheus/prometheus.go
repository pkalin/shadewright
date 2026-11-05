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

package prometheus

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/retryafter"
)

// Limits on queries and results. They are fixed in code so that no configuration can widen them.
const (
	// MaxRange bounds End minus Start of a range query.
	MaxRange = 6 * time.Hour
	// MinStep is the finest resolution of a range query: the default global scrape interval of the
	// prometheus-community chart the lab installs (1m), at which cAdvisor and kube-state-metrics are scraped
	// there; a finer step would only repeat samples. With MaxRange it bounds a range query to 361 points per
	// series, within MaxPointsPerSeries.
	MinStep = time.Minute
	// MaxLookback bounds how far in the past Start (or End of an instant query) may lie.
	MaxLookback = 7 * 24 * time.Hour
	// MaxClockSkew bounds how far in the future End may lie.
	MaxClockSkew = time.Minute
	// MaxSeries bounds the series of one result.
	MaxSeries = 50
	// MaxPointsPerSeries bounds the samples of one series received. The points a range query asks for are
	// bounded by MaxRange and MinStep, at most MaxRange/MinStep+1 (361), below it.
	MaxPointsPerSeries = 500
	// MaxPoints bounds the samples of one result, all series together.
	MaxPoints = 5000
	// MaxBodyBytes bounds the response body, after any decompression. MaxPoints samples with their labels
	// take about 200 KiB.
	MaxBodyBytes = 1 << 20
	// DefaultTimeout bounds one query when Config.Timeout is zero.
	DefaultTimeout = 30 * time.Second
	// MaxTimeout bounds Config.Timeout.
	MaxTimeout = 2 * time.Minute
	// MaxNamespaces bounds Config.Namespaces.
	MaxNamespaces = 256
)

// ErrInvalidQuery means the query was refused before any request: an unknown or missing parameter, a value
// that is not a valid name, a namespace outside Config.Namespaces, or a time range, step or resolution
// outside the limits. It is permanent.
var ErrInvalidQuery = errors.New("invalid metrics query")

// ErrInvalidResponse means the server's answer was refused: malformed, of another shape than the template
// returns, or with labels or samples outside what was asked for.
var ErrInvalidResponse = errors.New("invalid metrics response")

// Config configures a [Source].
type Config struct {
	// URL is the base URL of the Prometheus query API, such as http://prometheus-server.prometheus.svc. It
	// must be http or https, with no user information, query or fragment; a path is kept as a prefix of
	// /api/v1/... Credentials belong in Client, never in the URL.
	URL string
	// Client sends the requests and carries TLS settings and authentication (for example a transport that
	// adds a bearer token). Nil means a client that requires TLS 1.2 or later on https. Redirects are never
	// followed, whatever the client says.
	Client *http.Client
	// Timeout bounds one query, from the request to the end of the decoded body; it is also sent to the
	// server as its evaluation timeout. Zero means DefaultTimeout; at most MaxTimeout.
	Timeout time.Duration
	// Namespaces are the namespaces a query may name, each a DNS-1123 label, at most MaxNamespaces; the
	// analyzer passes its watched namespaces (--watch-namespaces). Prometheus applies no authorization of its
	// own, so this set is what keeps queries to the namespaces the analyzer may read. Empty refuses every
	// query.
	Namespaces []string
}

// Source is a [providers.MetricsSource] for one Prometheus-compatible query API. It is safe for concurrent
// use.
type Source struct {
	base       *url.URL
	client     *http.Client
	timeout    time.Duration
	namespaces map[string]struct{}
	now        func() time.Time
}

var _ providers.MetricsSource = (*Source)(nil)

// New validates cfg and returns a Source. It makes no network call. Errors never repeat the URL or a
// namespace.
func New(cfg Config) (*Source, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("prometheus URL cannot be parsed: %w", providers.ErrInvalidConfig)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("prometheus URL must be http or https: %w", providers.ErrInvalidConfig)
	case u.Host == "" || u.Opaque != "":
		return nil, fmt.Errorf("prometheus URL has no host: %w", providers.ErrInvalidConfig)
	case u.User != nil:
		return nil, fmt.Errorf("prometheus URL must not hold credentials: %w", providers.ErrInvalidConfig)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "":
		return nil, fmt.Errorf("prometheus URL must not have a query or fragment: %w", providers.ErrInvalidConfig)
	}
	if cfg.Timeout < 0 || cfg.Timeout > MaxTimeout {
		return nil, fmt.Errorf("timeout must be between 0 and %s: %w", MaxTimeout, providers.ErrInvalidConfig)
	}
	if len(cfg.Namespaces) > MaxNamespaces {
		return nil, fmt.Errorf("more than %d namespaces: %w", MaxNamespaces, providers.ErrInvalidConfig)
	}
	namespaces := make(map[string]struct{}, len(cfg.Namespaces))
	for i, ns := range cfg.Namespaces {
		if len(validation.IsDNS1123Label(ns)) > 0 {
			return nil, fmt.Errorf("namespace %d is not a DNS-1123 label: %w", i, providers.ErrInvalidConfig)
		}
		namespaces[ns] = struct{}{}
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	var client http.Client
	if cfg.Client != nil {
		client = *cfg.Client
	} else {
		client = http.Client{Transport: defaultTransport()}
	}
	// Go keeps some headers, Authorization among them, on a redirect to the same host; never follow one.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Source{base: u, client: &client, timeout: timeout, namespaces: namespaces, now: time.Now}, nil
}

func defaultTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	t.TLSHandshakeTimeout = 10 * time.Second
	// No ResponseHeaderTimeout: Prometheus sends the headers only once the query is evaluated, and the
	// deadline of the query (Config.Timeout, up to MaxTimeout) already bounds the whole request.
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return t
}

// window is the time a valid result's samples may carry, in milliseconds since the epoch.
type window struct {
	fromMs, toMs int64
	instant      bool
}

// Query runs an approved template. [providers.ErrNotFound] when the template is unknown, [ErrInvalidQuery]
// when the parameters or times are refused; see the package documentation for the other errors.
func (s *Source) Query(ctx context.Context, q providers.MetricsQuery) (providers.MetricsResult, error) {
	t, ok := registry[q.Template]
	if !ok {
		return providers.MetricsResult{}, fmt.Errorf("unknown metrics template: %w", providers.ErrNotFound)
	}
	expr, err := t.renderQuery(q.Params)
	if err != nil {
		return providers.MetricsResult{}, err
	}
	// After renderQuery, which requires the namespace parameter. The error never repeats the value.
	if _, ok := s.namespaces[q.Params[ParamNamespace]]; !ok {
		return providers.MetricsResult{}, fmt.Errorf("parameter %s is not an allowed namespace: %w", ParamNamespace,
			ErrInvalidQuery)
	}
	path, form, win, err := s.timeParams(q)
	if err != nil {
		return providers.MetricsResult{}, err
	}
	form.Set("query", expr)

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	server, err := serverTimeout(ctx)
	if err != nil {
		return providers.MetricsResult{}, err
	}
	form.Set("timeout", strconv.FormatInt(server.Milliseconds(), 10)+"ms")
	return s.do(ctx, path, form, win, t.info.Labels)
}

// serverTimeout is the evaluation timeout sent to the server: a tenth below the time left before the
// deadline of ctx (Config.Timeout or the caller's, whichever is sooner), so that a slow query ends with the
// server's timeout error, which the adapter can tell apart, and not with the client giving up.
func serverTimeout(ctx context.Context) (time.Duration, error) {
	left := DefaultTimeout
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	d := (left - left/10).Truncate(time.Millisecond)
	if d < time.Millisecond {
		return 0, fmt.Errorf("no time left for the prometheus query: %w", context.DeadlineExceeded)
	}
	return d, nil
}

// timeParams checks the times of q against the limits and returns the API path and time parameters.
func (s *Source) timeParams(q providers.MetricsQuery) (string, url.Values, window, error) {
	now := s.now()
	if q.End.IsZero() {
		return "", nil, window{}, fmt.Errorf("end time is required: %w", ErrInvalidQuery)
	}
	if q.End.After(now.Add(MaxClockSkew)) {
		return "", nil, window{}, fmt.Errorf("end time is in the future: %w", ErrInvalidQuery)
	}
	v := url.Values{}
	if q.Step == 0 {
		if !q.Start.IsZero() {
			return "", nil, window{}, fmt.Errorf("an instant query takes no start time: %w", ErrInvalidQuery)
		}
		if q.End.Before(now.Add(-MaxLookback)) {
			return "", nil, window{}, fmt.Errorf("end time is more than %s ago: %w", MaxLookback, ErrInvalidQuery)
		}
		end := q.End.UnixMilli()
		v.Set("time", formatMs(end))
		return "api/v1/query", v, window{fromMs: end, toMs: end, instant: true}, nil
	}
	// The range checks use the times in whole milliseconds, as they are sent: the server counts points
	// from those, not from the times given. MaxRange and MinStep bound the points asked for per series to
	// MaxRange/MinStep+1, which TestRangeLimitsStayWithinThePointCap keeps within MaxPointsPerSeries.
	start, end := q.Start.UnixMilli(), q.End.UnixMilli()
	switch {
	case q.Step < MinStep:
		return "", nil, window{}, fmt.Errorf("step is below %s: %w", MinStep, ErrInvalidQuery)
	case q.Step%time.Millisecond != 0:
		return "", nil, window{}, fmt.Errorf("step must be whole milliseconds: %w", ErrInvalidQuery)
	case q.Start.IsZero() || start >= end:
		return "", nil, window{}, fmt.Errorf("start time must be before end time: %w", ErrInvalidQuery)
	case end-start > MaxRange.Milliseconds():
		return "", nil, window{}, fmt.Errorf("time range is longer than %s: %w", MaxRange, ErrInvalidQuery)
	case q.Start.Before(now.Add(-MaxLookback)):
		return "", nil, window{}, fmt.Errorf("start time is more than %s ago: %w", MaxLookback, ErrInvalidQuery)
	}
	v.Set("start", formatMs(start))
	v.Set("end", formatMs(end))
	v.Set("step", strconv.FormatInt(q.Step.Milliseconds(), 10)+"ms")
	return "api/v1/query_range", v, window{fromMs: start, toMs: end}, nil
}

// formatMs writes a time in milliseconds as the decimal seconds the API takes.
func formatMs(ms int64) string {
	sign := ""
	if ms < 0 {
		sign, ms = "-", -ms
	}
	return fmt.Sprintf("%s%d.%03d", sign, ms/1000, ms%1000)
}

// do sends one request and decodes the response. Errors carry fixed text and the status code only.
func (s *Source) do(ctx context.Context, path string, form url.Values, win window, labels []string,
) (providers.MetricsResult, error) {
	u := s.base.JoinPath(path)
	u.RawQuery = form.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return providers.MetricsResult{}, fmt.Errorf("prometheus request cannot be built: %w", providers.ErrInvalidConfig)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return providers.MetricsResult{}, transportError(ctx, "prometheus request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if err := statusError(resp, s.now()); err != nil {
		return providers.MetricsResult{}, err
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return providers.MetricsResult{}, fmt.Errorf("prometheus response is not JSON: %w", ErrInvalidResponse)
	}
	if resp.ContentLength > MaxBodyBytes {
		return providers.MetricsResult{}, fmt.Errorf("prometheus response is larger than %d bytes: %w", MaxBodyBytes,
			providers.ErrTooLarge)
	}
	body := &capReader{r: resp.Body, left: MaxBodyBytes}
	res, err := decode(body, win, labels)
	if err != nil {
		// A body cut short by the connection (a reset, an unexpected EOF) is transient, not a bad answer.
		if ctx.Err() != nil || body.readErr != nil {
			return providers.MetricsResult{}, transportError(ctx, "prometheus response was not read")
		}
		return providers.MetricsResult{}, err
	}
	return res, nil
}

// transportError reports a failed request or body read. The error of the HTTP client is dropped: it holds
// the URL and, for some failures, addresses.
func transportError(ctx context.Context, msg string) error {
	if err := ctx.Err(); err != nil {
		// ctx.Err() is context.Canceled or context.DeadlineExceeded itself; a custom cause never reaches it.
		return fmt.Errorf("%s: %w", msg, err)
	}
	return &providers.RetryableError{Err: errors.New(msg)}
}

// statusError classifies a non-2xx status. Only the error envelope of a 503 is read, bounded, for its
// errorType; no body text ever reaches the error.
func statusError(resp *http.Response, now time.Time) error {
	code := resp.StatusCode
	switch {
	case code >= 200 && code <= 299:
		return nil
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf("prometheus refused the identity (status %d): %w", code, providers.ErrForbidden)
	case code == http.StatusServiceUnavailable && serverTimedOut(resp):
		// The server ran out of the evaluation timeout the adapter sent. Final, since an evaluation that hit
		// the timeout usually would again (the server counts time in its queue too).
		return fmt.Errorf("prometheus timed out evaluating the query (status %d): %w", code, providers.ErrRejected)
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500:
		return &providers.RetryableError{
			RetryAfter: retryafter.Parse(resp.Header.Get("Retry-After"), now, maxRetryAfter),
			Err:        fmt.Errorf("prometheus is unavailable (status %d)", code),
		}
	default:
		return fmt.Errorf("prometheus refused the query (status %d): %w", code, providers.ErrRejected)
	}
}

// maxRetryAfter caps a delay asked for by the server.
const maxRetryAfter = 10 * time.Minute

// maxErrorEnvelope bounds the part of an error body read for its errorType.
const maxErrorEnvelope = 4 << 10

// serverTimedOut reports whether a JSON error body is Prometheus's answer to a query that ran out of its
// evaluation timeout: errorType "timeout". Only the top-level keys up to errorType are read, through a
// bounded reader; every other value is skipped and dropped, and a body that is cut, malformed or too long
// counts as no timeout (the status stays retryable).
func serverTimedOut(resp *http.Response) bool {
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return false
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxErrorEnvelope))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return false
		}
		if key == "errorType" {
			v, err := dec.Token()
			return err == nil && v == "timeout"
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return false
		}
	}
	return false
}
