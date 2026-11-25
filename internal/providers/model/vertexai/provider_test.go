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
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/providers/model/openaiwire"
)

// seeded is a generator with a fixed seed, for the generated corpora.
func seeded(a, b uint64) *rand.Rand {
	return rand.New(rand.NewPCG(a, b)) // #nosec G404 -- a seeded test corpus, not a secret
}

// Values that must never appear in an error or a report.
const (
	promptMark = "PROMPT-MARK-7f3a"
	bodyMark   = "ERRBODY-MARK-55d0"
	tokenMark  = "ya29.TOKEN-SECRET-0e1d" // #nosec G101 -- a synthetic token, to check that it never leaks
	sourceMark = "assertion-SECRET-77aa /var/run/secrets/tokens/google"
)

// Fixtures used in several tests.
const (
	testModel    = "gemini-2.5-flash"
	testProject  = "shadewright-lab-01"
	testLocation = "europe-west4"
	testEndpoint = "https://europe-west4-aiplatform.googleapis.com/v1/projects/shadewright-lab-01/locations/europe-west4"
	testHost     = "europe-west4-aiplatform.googleapis.com"
	testPath     = "/v1/projects/shadewright-lab-01/locations/europe-west4/publishers/google/models/gemini-2.5-flash:generateContent"
	listEvents   = "list_events"
	shopArgs     = `{"namespace":"shop"}`
	getLogs      = "get_pod_logs"
)

// fakeTokens is a TokenSource with a fixed answer.
type fakeTokens struct {
	tok   string
	err   error
	calls atomic.Int32
}

func (f *fakeTokens) Token(context.Context) (string, error) {
	f.calls.Add(1)
	return f.tok, f.err
}

// pipeListener is a net.Listener whose connections are in-memory pipes.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

// dial returns the client end of a new pipe whose server end Accept returns.
func (l *pipeListener) dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
	case <-ctx.Done():
	}
	_ = client.Close()
	_ = server.Close()
	return nil, net.ErrClosed
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// harness is a provider wired to an httptest TLS server that stands in for the regional host.
type harness struct {
	p        *Provider
	tokens   *fakeTokens
	srv      *httptest.Server
	requests atomic.Int32
	mu       sync.Mutex
	sleeps   []time.Duration
	reports  []CallReport
}

func newHarness(t *testing.T, handler http.HandlerFunc, mod func(*Config)) *harness {
	t.Helper()
	h := &harness{tokens: &fakeTokens{tok: tokenMark}}
	h.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		handler(w, r)
	}))
	// The server listens on an in-memory listener: the real HTTP and TLS stacks run, and no socket is opened,
	// so the tests run where loopback connections are not allowed.
	_ = h.srv.Listener.Close()
	pl := newPipeListener()
	h.srv.Listener = pl
	h.srv.StartTLS()
	t.Cleanup(h.srv.Close)
	// Every connection goes to the test server; its certificate names example.com, so that is the name the
	// TLS check expects, while the request still names the regional host.
	tr := h.srv.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.ServerName = "example.com"
	tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return pl.dial(ctx) }
	cfg := Config{
		Endpoint: testEndpoint,
		Model:    testModel,
		Tokens:   h.tokens,
		OnCall: func(r CallReport) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.reports = append(h.reports, r)
		},
	}
	if mod != nil {
		mod(&cfg)
	}
	p, err := newProvider(cfg, testHooks{
		transport: tr,
		sleep: func(ctx context.Context, d time.Duration) error {
			h.mu.Lock()
			h.sleeps = append(h.sleeps, d)
			h.mu.Unlock()
			return ctx.Err()
		},
		jitter: func(d time.Duration) time.Duration { return d },
	})
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}
	h.p = p
	return h
}

func (h *harness) lastReport(t *testing.T) CallReport {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.reports) == 0 {
		t.Fatal("no call report")
	}
	return h.reports[len(h.reports)-1]
}

func reply(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = io.WriteString(w, body)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// usageJSON is the usage metadata of replyJSON.
const usageJSON = `{"promptTokenCount":100,"candidatesTokenCount":30,"thoughtsTokenCount":20,` +
	`"cachedContentTokenCount":40,"totalTokenCount":150}`

// wantUsage is the usage of replyJSON: 100 prompt tokens, 40 of them cached, and 30 + 20 output tokens.
var wantUsage = providers.Usage{InputTokens: 100, CachedInputTokens: 40, OutputTokens: 50}

// replyJSON is a generateContent reply naming testModel, with one candidate that ends with STOP and the given parts.
func replyJSON(parts string) string {
	return `{"candidates":[{"content":{"role":"model","parts":` + parts + `},"finishReason":"` + finishStop +
		`","avgLogprobs":-0.1}],"usageMetadata":` + usageJSON + `,"modelVersion":"` + testModel +
		`","createTime":"2026-11-25T12:00:00Z","responseId":"r-1"}`
}

func textPart(s string) string { return `{"text":` + jsonString(s) + `}` }

// callPart is a function call part; id empty means none on the wire. Each has a thought signature.
func callPart(id, name, args string) string {
	fc := `"name":"` + name + `","args":` + args
	if id != "" {
		fc = `"id":"` + id + `",` + fc
	}
	return `{"functionCall":{` + fc + `},"thoughtSignature":"c2lnLQ==` + name + id + `"}`
}

func structuredRequest() providers.CompletionRequest {
	return providers.CompletionRequest{
		Messages: []providers.Message{
			{Role: providers.RoleSystem, Content: "You triage incidents."},
			{Role: providers.RoleUser, Content: "context " + promptMark},
		},
		ResponseSchema: json.RawMessage(`{"type":"object","properties":{"cause":{"type":"string","maxLength":64}},` +
			`"required":["cause"],"additionalProperties":false}`),
	}
}

// endpointDim is one part of a generated endpoint: its text for a location, and whether it is the valid one.
type endpointDim struct {
	text  func(loc string) string
	valid bool
}

func fixed(s string, valid bool) endpointDim {
	return endpointDim{text: func(string) string { return s }, valid: valid}
}

// otherEU is an allowed EU location other than loc.
func otherEU(loc string) string {
	if loc == "europe-west1" {
		return "europe-west9"
	}
	return "europe-west1"
}

// endpointDims are the parts of an endpoint in order: scheme, user info, host, version, project, path
// location, tail, query and fragment. Each has the valid value first.
var endpointDims = [][]endpointDim{
	{fixed("https://", true), fixed("http://", false), fixed("ftp://", false)},
	{fixed("", true), fixed("u:p@", false), fixed("u@", false)},
	{
		{text: func(l string) string { return l + "-aiplatform.googleapis.com" }, valid: true},
		fixed("aiplatform.googleapis.com", false),
		fixed("aiplatform.eu.rep.googleapis.com", false),
		{text: func(l string) string { return l + "-aiplatform.eu.rep.googleapis.com" }, valid: false},
		fixed("us-central1-aiplatform.googleapis.com", false),
		fixed("europe-west2-aiplatform.googleapis.com", false),
		fixed("europe-west6-aiplatform.googleapis.com", false),
		fixed("203.0.113.7", false),
		fixed("[2001:db8::1]", false),
		fixed("[::ffff:203.0.113.7]", false),
		{text: func(l string) string { return l + "-aiplatform.googleapis.com.evil.example" }, valid: false},
		{text: func(l string) string { return l + "-aiplatform.googleapis.com:443" }, valid: false},
		{text: func(l string) string { return l + "-aiplatform.googleapis.com:" }, valid: false},
		{text: func(l string) string { return strings.ToUpper(l) + "-aiplatform.googleapis.com" }, valid: false},
		{text: func(l string) string { return "x" + l + "-aiplatform.googleapis.com" }, valid: false},
	},
	{fixed("/v1", true), fixed("/v1beta1", false), fixed("/V1", false), fixed("", false)},
	{fixed("/projects/"+testProject, true), fixed("/projects/123456789012", true), fixed("/projects/Shadewright", false),
		fixed("/projects/abc", false), fixed("/projects/p%2Fx-lab", false), fixed("/projects/..", false),
		fixed("/projects/proj_lab01", false), fixed("/projects/", false), fixed("/project/"+testProject, false)},
	{
		{text: func(l string) string { return "/locations/" + l }, valid: true},
		{text: func(l string) string { return "/locations/" + otherEU(l) }, valid: false},
		fixed("/locations/global", false),
		fixed("/locations/europe-west2", false),
		{text: func(l string) string { return "/location/" + l }, valid: false},
	},
	{fixed("", true), fixed("/", true), fixed("/publishers", false), fixed("//", false)},
	{fixed("", true), fixed("?alt=json", false), fixed("?", false)},
	{fixed("", true), fixed("#f", false), fixed("#", false)},
}

// checkEndpoint builds an endpoint from one choice per dimension and checks New against the answer the
// choices give: valid exactly when every choice is the valid one.
func checkEndpoint(t *testing.T, loc string, choice []int) {
	t.Helper()
	var b strings.Builder
	valid := true
	texts := make([]string, len(choice))
	for d, c := range choice {
		dim := endpointDims[d][c]
		texts[d] = dim.text(loc)
		b.WriteString(texts[d])
		valid = valid && dim.valid
	}
	endpoint := b.String()
	p, err := New(Config{Endpoint: endpoint, Model: testModel, Tokens: &fakeTokens{tok: tokenMark}})
	if valid {
		if err != nil {
			t.Fatalf("%s: err = %v, want accepted", endpoint, err)
		}
		project := strings.TrimPrefix(texts[4], "/projects/")
		want := "https://" + loc + "-aiplatform.googleapis.com/v1/projects/" + project + "/locations/" + loc +
			"/publishers/google/models/" + testModel + ":generateContent"
		if p.url != want {
			t.Fatalf("%s: url %s, want %s", endpoint, p.url, want)
		}
		return
	}
	if !providers.IsInvalidConfig(err) || p != nil {
		t.Fatalf("%s: err = %v, want invalid config", endpoint, err)
	}
	// The fixed text names no host, project or query of the input.
	leaks := []string{endpoint, texts[7]}
	if strings.Contains(texts[2], ".") {
		leaks = append(leaks, texts[2])
	}
	if project := strings.TrimPrefix(texts[4], "/projects/"); len(project) > 3 {
		leaks = append(leaks, project)
	}
	for _, s := range leaks {
		if len(s) > 1 && strings.Contains(err.Error(), s) {
			t.Fatalf("%s: the error %q repeats the input %q", endpoint, err, s)
		}
	}
}

// Endpoints generated from valid and invalid parts: every part varied alone from the valid endpoint, in every
// allowed location, then a seeded sample of combinations. New accepts exactly the endpoints made of valid
// parts, builds the URL from them, and refuses the others with fixed text.
func TestNewEndpointCorpus(t *testing.T) {
	locs := make([]string, 0, len(euLocations))
	for l := range euLocations {
		locs = append(locs, l)
	}
	slices.Sort(locs)
	for _, loc := range locs {
		for d := range endpointDims {
			for c := range endpointDims[d] {
				choice := make([]int, len(endpointDims))
				choice[d] = c
				checkEndpoint(t, loc, choice)
			}
		}
	}
	r := seeded(1167, 50)
	for range 3000 {
		choice := make([]int, len(endpointDims))
		for d := range choice {
			// The valid value half of the time, so that combinations of one or two faults are common.
			if r.IntN(2) == 0 {
				choice[d] = r.IntN(len(endpointDims[d]))
			}
		}
		checkEndpoint(t, locs[r.IntN(len(locs))], choice)
	}
	long := testEndpoint + "/" + strings.Repeat("a", maxEndpointBytes)
	if _, err := New(Config{Endpoint: long, Model: testModel, Tokens: &fakeTokens{}}); !providers.IsInvalidConfig(err) {
		t.Errorf("a long endpoint: err = %v", err)
	}
}

// Models across every allowed location: only Gemini 2.5 IDs are accepted, Gemini 3 and others are refused,
// and Flash-Lite is refused where the region does not serve it.
func TestNewModelCorpus(t *testing.T) {
	valid := []string{"gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.5-flash-lite", "gemini-2.5-flash-001",
		"gemini-2.5-pro-preview-06-2025", "gemini-2.5-flash-lite-002"}
	invalid := []string{"", "gemini-3-pro-preview", "gemini-3.0-flash", "gemini-3", "gemini-2.0-flash",
		"gemini-1.5-pro", "gemini-2.5-flash-image", "Gemini-2.5-flash", "gemini-2.5-flash/../x", "gemini-2.5-flash ",
		"gemini-2.5-ultra", "gpt-5.4-mini", "gemini-2.5-flash-01", "gemini-2.5-flash@001", "publishers/google/models/gemini-2.5-flash"}
	for loc := range euLocations {
		endpoint := "https://" + loc + "-aiplatform.googleapis.com/v1/projects/" + testProject + "/locations/" + loc
		for _, m := range append(slices.Clone(valid), invalid...) {
			want := slices.Contains(valid, m) && (loc != "europe-west3" || !strings.HasPrefix(m, "gemini-2.5-flash-lite"))
			p, err := New(Config{Endpoint: endpoint, Model: m, Tokens: &fakeTokens{}})
			if want != (err == nil) {
				t.Errorf("%s in %s: err = %v, want accepted %v", m, loc, err, want)
				continue
			}
			if err != nil && (!providers.IsInvalidConfig(err) || len(m) > 3 && strings.Contains(err.Error(), m)) {
				t.Errorf("%s in %s: err = %v, want fixed invalid config", m, loc, err)
			}
			if err == nil && !strings.HasSuffix(p.url, "/publishers/google/models/"+m+":generateContent") {
				t.Errorf("%s in %s: url %s", m, loc, p.url)
			}
		}
	}
}

// The other fields of Config are checked against their bounds.
func TestNewRefusesOutOfRange(t *testing.T) {
	zero, over, negative := 0, MaxThinkingAllowance+1, -1
	for name, mod := range map[string]func(*Config){
		"no token source":            func(c *Config) { c.Tokens = nil },
		"an invalid in-cluster CIDR": func(c *Config) { c.InClusterRanges = []netip.Prefix{{}} },
		"an unmasked in-cluster CIDR": func(c *Config) {
			c.InClusterRanges = []netip.Prefix{netip.PrefixFrom(netip.MustParseAddr("100.64.1.1"), 10)}
		},
		"output limit above":    func(c *Config) { c.MaxOutputTokens = MaxOutputTokens + 1 },
		"output limit negative": func(c *Config) { c.MaxOutputTokens = -1 },
		"thinking above":        func(c *Config) { c.ThinkingAllowance = &over },
		"thinking negative":     func(c *Config) { c.ThinkingAllowance = &negative },
		"attempts above":        func(c *Config) { c.MaxAttempts = MaxAttempts + 1 },
		"negative delay":        func(c *Config) { c.BaseDelay = -time.Second },
		"timeout above":         func(c *Config) { c.Timeout = maxConfigTimeout + time.Second },
		"response bytes above":  func(c *Config) { c.MaxResponseBytes = maxConfigResponseBytes + 1 },
	} {
		cfg := Config{Endpoint: testEndpoint, Model: testModel, Tokens: &fakeTokens{}, ThinkingAllowance: &zero}
		mod(&cfg)
		if _, err := New(cfg); !providers.IsInvalidConfig(err) {
			t.Errorf("%s: err = %v, want invalid config", name, err)
		}
	}
	cfg := Config{Endpoint: testEndpoint, Model: testModel, Tokens: &fakeTokens{},
		InClusterRanges: []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10")}, ThinkingAllowance: &zero}
	if _, err := New(cfg); err != nil {
		t.Errorf("valid config: %v", err)
	}
}

// The client New builds uses no proxy, requires TLS 1.2, follows no redirect, waits for headers no longer
// than an attempt, and dials neither loopback nor a declared in-cluster range.
func TestDefaultClient(t *testing.T) {
	p, err := New(Config{Endpoint: testEndpoint, Model: testModel, Tokens: &fakeTokens{tok: tokenMark},
		AttemptTimeout: 7 * time.Second, InClusterRanges: []netip.Prefix{netip.MustParsePrefix("8.8.8.0/24")}})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := p.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport %T", p.client.Transport)
	}
	if tr.Proxy != nil || tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 ||
		tr.ResponseHeaderTimeout != 7*time.Second {
		t.Errorf("proxy set %v, TLS %+v, header timeout %s", tr.Proxy != nil, tr.TLSClientConfig, tr.ResponseHeaderTimeout)
	}
	if p.client.CheckRedirect == nil || !errors.Is(p.client.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Error("the client may follow redirects")
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	for _, addr := range []string{srv.Listener.Addr().String(), "8.8.8.8:443", "169.254.169.254:80"} {
		conn, err := tr.DialContext(t.Context(), "tcp", addr)
		if conn != nil {
			_ = conn.Close()
		}
		if !errors.Is(err, openaiwire.ErrDialRefused) {
			t.Errorf("dial %s: err = %v, want refused", addr, err)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("the loopback server saw %d requests", hits.Load())
	}
}

func TestCompleteStructuredSuccess(t *testing.T) {
	var got map[string]any
	var hdr http.Header
	var host, path, query string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hdr, host, path, query = r.Header.Clone(), r.Host, r.URL.Path, r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&got)
		reply(w, replyJSON(`[{"text":"thinking","thought":true},{"text":"{\"cause\":"},{"text":"\"oom\"}"}]`))
	}, nil)
	c, err := h.p.Complete(t.Context(), structuredRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if c.Content != `{"cause":"oom"}` || c.Usage != wantUsage || c.Model != testModel || c.ToolCalls != nil ||
		!c.ProviderBlocks.IsZero() || c.UsageUnknown {
		t.Errorf("completion = %+v", c)
	}
	if host != testHost || path != testPath || query != "" {
		t.Errorf("host %q, path %q, query %q", host, path, query)
	}
	if hdr.Get("Authorization") != "Bearer "+tokenMark || hdr.Get("Content-Type") != mimeJSON {
		t.Errorf("authorization as expected: %v, content type %q", hdr.Get("Authorization") == "Bearer "+tokenMark,
			hdr.Get("Content-Type"))
	}
	si, _ := got["systemInstruction"].(map[string]any)
	if fmt.Sprint(si) != "map[parts:[map[text:You triage incidents.]]]" {
		t.Errorf("systemInstruction = %v", got["systemInstruction"])
	}
	if fmt.Sprint(got["contents"]) != "[map[parts:[map[text:context "+promptMark+"]] role:user]]" {
		t.Errorf("contents = %v", got["contents"])
	}
	gc, _ := got["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(DefaultMaxOutputTokens+DefaultThinkingAllowance) ||
		fmt.Sprint(gc["thinkingConfig"]) != fmt.Sprintf("map[thinkingBudget:%d]", DefaultThinkingAllowance) ||
		gc["responseMimeType"] != mimeJSON {
		t.Errorf("generationConfig = %v", gc)
	}
	schema, _ := json.Marshal(gc["responseJsonSchema"])
	if string(schema) != `{"additionalProperties":false,"properties":{"cause":{"description":"Bounds: maxLength 64.",`+
		`"type":"string"}},"required":["cause"],"type":"object"}` {
		t.Errorf("responseJsonSchema = %s", schema)
	}
	if _, ok := got["tools"]; ok {
		t.Error("a request without tools sends tools")
	}
	if r := h.lastReport(t); r.ConfiguredModel != testModel || r.Model != testModel || r.Attempts != 1 || r.Err != nil ||
		r.Usage != wantUsage {
		t.Errorf("report = %+v", r)
	}
}

// maxOutputTokens is the output spend: the request's limit plus the thinking allowance, which is also the
// thinking budget, zero included when set.
func TestOutputSpendIsSentAndBounded(t *testing.T) {
	for _, tc := range []struct {
		allowance *int
		limit     int
		want      int
		budget    int
	}{
		{nil, 0, DefaultMaxOutputTokens + DefaultThinkingAllowance, DefaultThinkingAllowance},
		{nil, 1024, 1024 + DefaultThinkingAllowance, DefaultThinkingAllowance},
		{new(0), 1024, 1024, 0},
		{new(512), 300, 812, 512},
		{new(MaxThinkingAllowance), MaxOutputTokens, MaxOutputTokens + MaxThinkingAllowance, MaxThinkingAllowance},
	} {
		var gc map[string]any
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				GenerationConfig map[string]any `json:"generationConfig"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gc = body.GenerationConfig
			reply(w, replyJSON(`[{"text":"{}"}]`))
		}, func(c *Config) { c.ThinkingAllowance = tc.allowance })
		if got := h.p.OutputSpend(tc.limit); got != tc.want {
			t.Errorf("allowance %v, limit %d: OutputSpend = %d, want %d", tc.allowance, tc.limit, got, tc.want)
		}
		req := structuredRequest()
		req.MaxOutputTokens = tc.limit
		if _, err := h.p.Complete(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		tcfg, _ := gc["thinkingConfig"].(map[string]any)
		if gc["maxOutputTokens"] != float64(tc.want) || tcfg["thinkingBudget"] != float64(tc.budget) {
			t.Errorf("allowance %v, limit %d: generationConfig %v", tc.allowance, tc.limit, gc)
		}
	}
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {}, nil)
	req := structuredRequest()
	req.MaxOutputTokens = MaxOutputTokens + 1
	if _, err := h.p.Complete(t.Context(), req); !providers.IsRejected(err) || !providers.IsNotSent(err) || h.requests.Load() != 0 {
		t.Errorf("limit above the maximum: err = %v, %d requests", err, h.requests.Load())
	}
}

// Each status other than 200 has its class; the error body is never quoted; a redirect is not followed; only
// retryable statuses are retried; and a call ended by 429 is never marked as not processed.
func TestStatusClasses(t *testing.T) {
	for _, tc := range []struct {
		code     int
		class    func(error) bool
		attempts int32
	}{
		{http.StatusBadRequest, providers.IsRejected, 1},
		{http.StatusUnauthorized, providers.IsForbidden, 1},
		{http.StatusForbidden, providers.IsForbidden, 1},
		{http.StatusNotFound, providers.IsNotFound, 1},
		{http.StatusRequestEntityTooLarge, providers.IsTooLarge, 1},
		{http.StatusMovedPermanently, providers.IsRejected, 1},
		{http.StatusFound, providers.IsRejected, 1},
		{http.StatusTemporaryRedirect, providers.IsRejected, 1},
		{http.StatusPermanentRedirect, providers.IsRejected, 1},
		{http.StatusRequestTimeout, providers.IsRetryable, DefaultMaxAttempts},
		{http.StatusTooManyRequests, providers.IsRetryable, DefaultMaxAttempts},
		{http.StatusInternalServerError, providers.IsRetryable, DefaultMaxAttempts},
		{http.StatusServiceUnavailable, providers.IsRetryable, DefaultMaxAttempts},
	} {
		var redirected atomic.Int32
		target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Location", target.URL+"/steal")
			w.Header().Set("Content-Type", mimeJSON)
			w.WriteHeader(tc.code)
			_, _ = io.WriteString(w, `{"error":{"message":"`+bodyMark+`"}}`)
		}, nil)
		c, err := h.p.Complete(t.Context(), structuredRequest())
		target.Close()
		if err == nil || !tc.class(err) || h.requests.Load() != tc.attempts || redirected.Load() != 0 {
			t.Errorf("%d: err = %v, %d requests (want %d), %d redirected", tc.code, err, h.requests.Load(), tc.attempts,
				redirected.Load())
			continue
		}
		if strings.Contains(err.Error(), bodyMark) || strings.Contains(err.Error(), tokenMark) ||
			strings.Contains(err.Error(), "steal") || strings.Contains(err.Error(), testHost) {
			t.Errorf("%d: the error quotes the body, the token or a URL: %v", tc.code, err)
		}
		if providers.IsNotProcessed(err) || providers.IsNotSent(err) || c.UsageUnknown || c.Usage != (providers.Usage{}) {
			t.Errorf("%d: not processed %v, not sent %v, completion %+v", tc.code, providers.IsNotProcessed(err),
				providers.IsNotSent(err), c)
		}
	}
}

// A transient failure is retried with backoff and the retry's answer returned; a delay the service asks for
// is waited for when it is short and returned to the caller when it is long.
func TestRetries(t *testing.T) {
	t.Run("then success", func(t *testing.T) {
		var n atomic.Int32
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			if n.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			reply(w, replyJSON(`[{"text":"{}"}]`))
		}, nil)
		c, err := h.p.Complete(t.Context(), structuredRequest())
		if err != nil || c.Content != "{}" || h.requests.Load() != 2 || len(h.sleeps) != 1 || h.sleeps[0] != DefaultBaseDelay {
			t.Errorf("err = %v, content %q, %d requests, sleeps %v", err, c.Content, h.requests.Load(), h.sleeps)
		}
		if r := h.lastReport(t); r.Attempts != 2 {
			t.Errorf("report attempts %d", r.Attempts)
		}
	})
	t.Run("a short asked delay", func(t *testing.T) {
		var n atomic.Int32
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			if n.Add(1) == 1 {
				w.Header().Set("Retry-After", "3")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			reply(w, replyJSON(`[{"text":"{}"}]`))
		}, nil)
		if _, err := h.p.Complete(t.Context(), structuredRequest()); err != nil || len(h.sleeps) != 1 || h.sleeps[0] != 3*time.Second {
			t.Errorf("err = %v, sleeps %v", err, h.sleeps)
		}
	})
	t.Run("a long asked delay", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
		}, nil)
		_, err := h.p.Complete(t.Context(), structuredRequest())
		if d, ok := providers.RetryAfter(err); !ok || d != 120*time.Second || h.requests.Load() != 1 || providers.IsNotProcessed(err) {
			t.Errorf("err = %v, %d requests", err, h.requests.Load())
		}
	})
	t.Run("a billed answer dropped by a retry", func(t *testing.T) {
		var n atomic.Int32
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			if n.Add(1) == 1 {
				w.Header().Set("Content-Type", mimeJSON)
				w.Header().Set("Content-Length", "10")
				_, _ = io.WriteString(w, "{") // cut short: the read fails, retryable, billed
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}, func(c *Config) { c.MaxAttempts = 2 })
		c, err := h.p.Complete(t.Context(), structuredRequest())
		if !providers.IsRetryable(err) || !c.UsageUnknown {
			t.Errorf("err = %v, completion %+v, want retryable with unknown usage", err, c)
		}
	})
}

// A token source that fails, or a token that cannot be sent, ends the attempt before any request: the error
// keeps the class only, holds nothing of the source's error or the token, and is marked not sent.
func TestTokenFailures(t *testing.T) {
	secret := errors.New(sourceMark + " " + tokenMark)
	for name, tc := range map[string]struct {
		tok   string
		err   error
		class func(error) bool
		tries int32
	}{
		"forbidden":        {"", fmt.Errorf("%w: %w", secret, providers.ErrForbidden), providers.IsForbidden, 1},
		"invalid config":   {"", fmt.Errorf("%w: %w", secret, providers.ErrInvalidConfig), providers.IsForbidden, 1},
		"retryable":        {"", &providers.RetryableError{Err: secret}, providers.IsRetryable, DefaultMaxAttempts},
		"deadline":         {"", fmt.Errorf("%w: %w", secret, context.DeadlineExceeded), providers.IsRetryable, DefaultMaxAttempts},
		"other":            {"", secret, func(err error) bool { return err != nil && !providers.IsRetryable(err) }, 1},
		"empty token":      {"", nil, providers.IsForbidden, 1},
		"token with space": {"ya29.a b", nil, providers.IsForbidden, 1},
		"token with CR LF": {tokenMark + "\r\nX-Injected: 1", nil, providers.IsForbidden, 1},
		"token not ASCII":  {tokenMark + "é", nil, providers.IsForbidden, 1},
		"token too large":  {strings.Repeat("a", maxTokenBytes+1), nil, providers.IsForbidden, 1},
		"token with a NUL": {tokenMark + "\x00", nil, providers.IsForbidden, 1},
	} {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			reply(w, replyJSON(`[{"text":"{}"}]`))
		}, nil)
		h.tokens.tok, h.tokens.err = tc.tok, tc.err
		_, err := h.p.Complete(t.Context(), structuredRequest())
		if !tc.class(err) || h.tokens.calls.Load() != tc.tries {
			t.Errorf("%s: err = %v, %d token calls, want %d", name, err, h.tokens.calls.Load(), tc.tries)
		}
		if err == nil {
			continue
		}
		if h.requests.Load() != 0 || !providers.IsNotSent(err) {
			t.Errorf("%s: %d requests, not sent %v", name, h.requests.Load(), providers.IsNotSent(err))
		}
		if strings.Contains(err.Error(), "SECRET") || errors.Is(err, secret) {
			t.Errorf("%s: the error keeps the source's error or the token: %v", name, err)
		}
	}
}

// roundTripFunc is an http.RoundTripper made of a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newProviderWith(t *testing.T, tr http.RoundTripper, mod func(*Config)) *Provider {
	t.Helper()
	cfg := Config{Endpoint: testEndpoint, Model: testModel, Tokens: &fakeTokens{tok: tokenMark}, MaxAttempts: 1}
	if mod != nil {
		mod(&cfg)
	}
	p, err := newProvider(cfg, testHooks{transport: tr})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A response that arrives after the attempt's deadline, or after the caller canceled, is never read: the
// call is the timeout (retryable) or the cancellation (not retryable), carries no usage, and was sent.
func TestLateResponseIsNotRead(t *testing.T) {
	late := func(r *http.Request) (*http.Response, error) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		return &http.Response{StatusCode: http.StatusOK, Header: jsonHeader(),
			Body: io.NopCloser(strings.NewReader(replyJSON(`[{"text":"{}"}]`))), Request: r}, nil
	}
	t.Run("after the attempt deadline", func(t *testing.T) {
		p := newProviderWith(t, roundTripFunc(late), func(c *Config) { c.AttemptTimeout = 20 * time.Millisecond })
		c, err := p.Complete(t.Context(), structuredRequest())
		if !providers.IsRetryable(err) || !strings.Contains(err.Error(), "timed out") || providers.IsNotSent(err) ||
			!reflect.DeepEqual(c, providers.Completion{}) {
			t.Errorf("err = %v, completion %+v", err, c)
		}
	})
	t.Run("after the caller canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		p := newProviderWith(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
			cancel()
			return late(r)
		}), nil)
		c, err := p.Complete(ctx, structuredRequest())
		if !errors.Is(err, context.Canceled) || providers.IsRetryable(err) || providers.IsNotSent(err) ||
			!reflect.DeepEqual(c, providers.Completion{}) {
			t.Errorf("err = %v, completion %+v", err, c)
		}
	})
	t.Run("a transport error after sending", func(t *testing.T) {
		p := newProviderWith(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
			_, _ = io.Copy(io.Discard, r.Body)
			return nil, errors.New("reset by " + testHost + " " + bodyMark)
		}), nil)
		_, err := p.Complete(t.Context(), structuredRequest())
		if !providers.IsRetryable(err) || providers.IsNotSent(err) || strings.Contains(err.Error(), bodyMark) {
			t.Errorf("err = %v", err)
		}
	})
}

// A cancellation during the backoff ends the call with the cancellation, not the last attempt's error.
func TestCancelDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }, nil)
	h.p.sleep = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	_, err := h.p.Complete(ctx, structuredRequest())
	if !errors.Is(err, context.Canceled) || providers.IsRetryable(err) || h.requests.Load() != 1 {
		t.Errorf("err = %v, %d requests", err, h.requests.Load())
	}
}

// Concurrent callers share one Provider: each gets the answer to its own request.
func TestConcurrentCallers(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Contents []struct {
				Parts []struct{ Text string } `json:"parts"`
			} `json:"contents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		reply(w, replyJSON(`[{"text":`+jsonString(`{"echo":`+jsonString(body.Contents[0].Parts[0].Text)+`}`)+`}]`))
	}, nil)
	var wg sync.WaitGroup
	errs := make(chan string, 32)
	for i := range 32 {
		wg.Go(func() {
			req := structuredRequest()
			req.Messages[1].Content = fmt.Sprintf("caller %d", i)
			c, err := h.p.Complete(t.Context(), req)
			if want := fmt.Sprintf(`{"echo":"caller %d"}`, i); err != nil || c.Content != want {
				errs <- fmt.Sprintf("caller %d: err = %v, content %q", i, err, c.Content)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// A 200 reply that is too large, or not JSON, was billed: it is not parsed, and its usage is read when it
// can be or marked unknown.
func TestBilledRepliesNotParsed(t *testing.T) {
	t.Run("too large, declared", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			reply(w, replyJSON(`[{"text":"`+strings.Repeat("a", 2048)+`"}]`))
		}, func(c *Config) { c.MaxResponseBytes = 1024; c.MaxAttempts = 1 })
		c, err := h.p.Complete(t.Context(), structuredRequest())
		if !providers.IsTooLarge(err) || !c.UsageUnknown || c.Content != "" {
			t.Errorf("err = %v, completion %+v", err, c)
		}
	})
	t.Run("too large, streamed", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", mimeJSON)
			for range 64 {
				_, _ = io.WriteString(w, strings.Repeat(" ", 1024))
				w.(http.Flusher).Flush()
			}
		}, func(c *Config) { c.MaxResponseBytes = 1024; c.MaxAttempts = 1 })
		c, err := h.p.Complete(t.Context(), structuredRequest())
		if !providers.IsTooLarge(err) || !c.UsageUnknown {
			t.Errorf("err = %v, completion %+v", err, c)
		}
	})
	t.Run("not JSON", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, replyJSON(`[{"text":"{}"}]`))
		}, nil)
		c, err := h.p.Complete(t.Context(), structuredRequest())
		if !providers.IsMalformedOutput(err) || c.Usage != wantUsage || c.Model != testModel || c.Content != "" || c.UsageUnknown {
			t.Errorf("err = %v, completion %+v", err, c)
		}
	})
}

// A request that cannot be encoded sends nothing and asks for no token.
func TestEncodeFailureIsNotSent(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {}, nil)
	req := structuredRequest()
	req.Messages = append(req.Messages, providers.Message{Role: providers.RoleTool, ToolCallID: "c1", Content: "x"})
	_, err := h.p.Complete(t.Context(), req)
	if !providers.IsRejected(err) || !providers.IsNotSent(err) || h.requests.Load() != 0 || h.tokens.calls.Load() != 0 {
		t.Errorf("err = %v, %d requests, %d token calls", err, h.requests.Load(), h.tokens.calls.Load())
	}
	req.Messages[1].Content = strings.Repeat("x", maxRequestBytes)
	req.Messages = req.Messages[:2]
	if _, err := h.p.Complete(t.Context(), req); !providers.IsTooLarge(err) || !providers.IsNotSent(err) {
		t.Errorf("large request: err = %v", err)
	}
}

// tokenFunc is a TokenSource made of a function.
type tokenFunc func(context.Context) (string, error)

func (f tokenFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

// A call that ends while the token is being fetched hands nothing to the HTTP client: it is marked not sent,
// and the class is the cancellation or the timeout.
func TestEndDuringTokenFetchIsNotSent(t *testing.T) {
	var sent atomic.Int32
	count := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent.Add(1)
		return nil, errors.New("not reached")
	})
	t.Run("caller canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		p := newProviderWith(t, count, func(c *Config) {
			c.Tokens = tokenFunc(func(context.Context) (string, error) { cancel(); return tokenMark, nil })
		})
		_, err := p.Complete(ctx, structuredRequest())
		if !errors.Is(err, context.Canceled) || providers.IsRetryable(err) || !providers.IsNotSent(err) || sent.Load() != 0 {
			t.Errorf("err = %v, %d requests", err, sent.Load())
		}
	})
	t.Run("attempt deadline", func(t *testing.T) {
		p := newProviderWith(t, count, func(c *Config) {
			c.AttemptTimeout = 10 * time.Millisecond
			c.Tokens = tokenFunc(func(ctx context.Context) (string, error) { <-ctx.Done(); return tokenMark, nil })
		})
		_, err := p.Complete(t.Context(), structuredRequest())
		if !providers.IsRetryable(err) || !providers.IsNotSent(err) || sent.Load() != 0 {
			t.Errorf("err = %v, %d requests", err, sent.Load())
		}
	})
}

// blockUntilDone is a transport that never answers: the request ends with its context.
func blockUntilDone(count *atomic.Int32) roundTripFunc {
	return func(r *http.Request) (*http.Response, error) {
		count.Add(1)
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
}

// Each attempt has its own deadline, the call has one for all attempts, and a shorter deadline of the caller
// wins over both.
func TestTimeouts(t *testing.T) {
	t.Run("per attempt", func(t *testing.T) {
		var n atomic.Int32
		p := newProviderWith(t, blockUntilDone(&n), func(c *Config) {
			c.AttemptTimeout, c.BaseDelay, c.MaxAttempts = 20*time.Millisecond, time.Millisecond, 3
		})
		_, err := p.Complete(t.Context(), structuredRequest())
		if !providers.IsRetryable(err) || n.Load() != 3 || providers.IsNotSent(err) {
			t.Errorf("err = %v, %d attempts, want 3 timed out attempts", err, n.Load())
		}
	})
	t.Run("whole call", func(t *testing.T) {
		var n atomic.Int32
		p := newProviderWith(t, blockUntilDone(&n), func(c *Config) {
			c.Timeout, c.AttemptTimeout, c.BaseDelay, c.MaxAttempts = 50*time.Millisecond, time.Minute, time.Millisecond, 3
		})
		start := time.Now()
		_, err := p.Complete(t.Context(), structuredRequest())
		if !providers.IsRetryable(err) || n.Load() != 1 || time.Since(start) > 10*time.Second {
			t.Errorf("err = %v, %d attempts after %s, want one attempt ended by the call's deadline", err, n.Load(), time.Since(start))
		}
	})
	t.Run("caller's deadline", func(t *testing.T) {
		var n atomic.Int32
		p := newProviderWith(t, blockUntilDone(&n), nil)
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := p.Complete(ctx, structuredRequest())
		if !providers.IsRetryable(err) || n.Load() != 1 || time.Since(start) > 10*time.Second {
			t.Errorf("err = %v, %d attempts after %s", err, n.Load(), time.Since(start))
		}
	})
}

// endless is a body that never ends and counts what is read from it.
type endless struct{ n atomic.Int64 }

func (e *endless) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = ' '
	}
	e.n.Add(int64(len(b)))
	return len(b), nil
}

func (*endless) Close() error { return nil }

// Nothing is read without a bound: an error body is drained up to maxDrain, a reply up to MaxResponseBytes (and
// one byte more, to see that it is too large), whatever the headers say.
func TestReadsAreBounded(t *testing.T) {
	for _, tc := range []struct {
		status int
		limit  int64
		class  func(error) bool
	}{
		{http.StatusBadRequest, maxDrain, providers.IsRejected},
		{http.StatusInternalServerError, maxDrain, providers.IsRetryable},
		{http.StatusFound, maxDrain, providers.IsRejected},
		{http.StatusOK, 2048 + 1, providers.IsTooLarge},
	} {
		body := &endless{}
		p := newProviderWith(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: tc.status, ContentLength: -1, Body: body, Request: r,
				Header: jsonHeader()}, nil
		}), func(c *Config) { c.MaxResponseBytes = 2048 })
		_, err := p.Complete(t.Context(), structuredRequest())
		// The reader hands out whole buffers, so the count may pass the limit by at most one buffer.
		if !tc.class(err) || body.n.Load() > tc.limit+32<<10 {
			t.Errorf("status %d: err = %v, %d bytes read, want at most about %d", tc.status, err, body.n.Load(), tc.limit)
		}
	}
}
