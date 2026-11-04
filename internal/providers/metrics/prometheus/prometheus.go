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
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
)

// Limits on queries and results. They are fixed in code so that no configuration can widen them.
const (
	// MaxRange bounds End minus Start of a range query.
	MaxRange = 6 * time.Hour
	// MinStep is the finest resolution of a range query; the lab scrapes every 15 seconds.
	MinStep = 15 * time.Second
	// MaxLookback bounds how far in the past Start (or End of an instant query) may lie.
	MaxLookback = 7 * 24 * time.Hour
	// MaxClockSkew bounds how far in the future End may lie.
	MaxClockSkew = time.Minute
	// MaxSeries bounds the series of one result.
	MaxSeries = 50
	// MaxPointsPerSeries bounds the samples of one series, asked for and received.
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
)

// ErrInvalidQuery means the query was refused before any request: an unknown or missing parameter, a value
// that is not a valid name, or a time range, step or resolution outside the limits. It is permanent.
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
}

// Source is a [providers.MetricsSource] for one Prometheus-compatible query API. It is safe for concurrent
// use.
type Source struct {
	base    *url.URL
	client  *http.Client
	timeout time.Duration
	now     func() time.Time
}

var _ providers.MetricsSource = (*Source)(nil)

// New validates cfg and returns a Source. It makes no network call. Errors never repeat the URL.
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
	return &Source{base: u, client: &client, timeout: timeout, now: time.Now}, nil
}

func defaultTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = 30 * time.Second
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
func (s *Source) Query(ctx context.Context, q providers.MetricsQuery) ([]providers.Series, error) {
	t, ok := registry[q.Template]
	if !ok {
		return nil, fmt.Errorf("unknown metrics template: %w", providers.ErrNotFound)
	}
	expr, err := t.renderQuery(q.Params)
	if err != nil {
		return nil, err
	}
	path, form, win, err := s.timeParams(q)
	if err != nil {
		return nil, err
	}
	form.Set("query", expr)
	form.Set("timeout", strconv.FormatInt(s.timeout.Milliseconds(), 10)+"ms")

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.do(ctx, path, form, win, t.info.Labels)
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
	switch {
	case q.Step < MinStep:
		return "", nil, window{}, fmt.Errorf("step is below %s: %w", MinStep, ErrInvalidQuery)
	case q.Step%time.Millisecond != 0:
		return "", nil, window{}, fmt.Errorf("step must be whole milliseconds: %w", ErrInvalidQuery)
	case q.Start.IsZero() || !q.Start.Before(q.End):
		return "", nil, window{}, fmt.Errorf("start time must be before end time: %w", ErrInvalidQuery)
	case q.End.Sub(q.Start) > MaxRange:
		return "", nil, window{}, fmt.Errorf("time range is longer than %s: %w", MaxRange, ErrInvalidQuery)
	case q.Start.Before(now.Add(-MaxLookback)):
		return "", nil, window{}, fmt.Errorf("start time is more than %s ago: %w", MaxLookback, ErrInvalidQuery)
	case int64(q.End.Sub(q.Start)/q.Step)+1 > MaxPointsPerSeries:
		return "", nil, window{}, fmt.Errorf("range and step ask for more than %d points per series: %w",
			MaxPointsPerSeries, ErrInvalidQuery)
	}
	start, end := q.Start.UnixMilli(), q.End.UnixMilli()
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
) ([]providers.Series, error) {
	u := s.base.JoinPath(path)
	u.RawQuery = form.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("prometheus request cannot be built: %w", providers.ErrInvalidConfig)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, transportError(ctx, "prometheus request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if err := statusError(resp); err != nil {
		return nil, err
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return nil, fmt.Errorf("prometheus response is not JSON: %w", ErrInvalidResponse)
	}
	if resp.ContentLength > MaxBodyBytes {
		return nil, fmt.Errorf("prometheus response is larger than %d bytes: %w", MaxBodyBytes, providers.ErrTooLarge)
	}
	series, err := decode(&capReader{r: resp.Body, left: MaxBodyBytes}, win, labels)
	if err != nil {
		if ctx.Err() != nil {
			return nil, transportError(ctx, "prometheus response was not read")
		}
		return nil, err
	}
	return series, nil
}

// transportError reports a failed request or body read. The error of the HTTP client is dropped: it holds
// the URL and, for some failures, addresses.
func transportError(ctx context.Context, msg string) error {
	if err := ctx.Err(); err != nil {
		if errors.Is(context.Cause(ctx), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%s: %w", msg, context.DeadlineExceeded)
		}
		return fmt.Errorf("%s: %w", msg, context.Canceled)
	}
	return &providers.RetryableError{Err: errors.New(msg)}
}

// statusError classifies a non-2xx status without reading the body.
func statusError(resp *http.Response) error {
	code := resp.StatusCode
	switch {
	case code >= 200 && code <= 299:
		return nil
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf("prometheus refused the identity (status %d): %w", code, providers.ErrForbidden)
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500:
		return &providers.RetryableError{
			RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
			Err:        fmt.Errorf("prometheus is unavailable (status %d)", code),
		}
	default:
		return fmt.Errorf("prometheus refused the query (status %d): %w", code, providers.ErrRejected)
	}
}

// maxRetryAfter caps a delay asked for by the server.
const maxRetryAfter = 10 * time.Minute

// retryAfter reads a Retry-After header in seconds; dates and anything else are ignored.
func retryAfter(h string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || n <= 0 {
		return 0
	}
	if n >= int(maxRetryAfter/time.Second) {
		return maxRetryAfter
	}
	return time.Duration(n) * time.Second
}
