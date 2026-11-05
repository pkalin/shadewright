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
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"

	"github.com/pkalin/shadewright/internal/providers"
)

// Values that must never appear in an error, a report or a log line.
const (
	promptMark = "PROMPT-MARK-7f3a"
	replyMark  = "REPLY-MARK-91bc"
	bodyMark   = "ERRBODY-MARK-55d0"
)

// Fixtures used in several tests.
const (
	testDeployment = "triage"
	datedVersion   = "2024-10-21"
	callZero       = "call_0"
	logsTool       = "get_logs"
	testEndpoint   = "https://example.com/"
	azureEndpoint  = "https://r.openai.azure.com/"
)

// fakeJWT returns an unsigned JWT with the given audience claim (a string or a list).
func fakeJWT(aud any) string {
	enc := base64.RawURLEncoding
	payload, _ := json.Marshal(map[string]any{"aud": aud, "tid": "tenant", "secret": "TOKEN-SECRET-0e1d"})
	return enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString(payload) + "." +
		enc.EncodeToString([]byte("signature"))
}

var goodToken = fakeJWT("https://cognitiveservices.azure.com")

type fakeTokens struct {
	tok   string
	err   error
	calls atomic.Int32
}

func (f *fakeTokens) Token(context.Context) (string, error) {
	f.calls.Add(1)
	return f.tok, f.err
}

// harness is a provider wired to an httptest TLS server.
type harness struct {
	p        *Provider
	srv      *httptest.Server
	requests atomic.Int32
	sleeps   []time.Duration
	reports  []CallReport
	mu       sync.Mutex
}

func newHarness(t *testing.T, handler http.HandlerFunc, mod func(*Config)) *harness {
	t.Helper()
	h := &harness{}
	h.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		handler(w, r)
	}))
	t.Cleanup(h.srv.Close)
	// The endpoint must be a DNS name; every connection goes to the test server, whose certificate names
	// example.com.
	tr := h.srv.Client().Transport.(*http.Transport).Clone()
	addr := h.srv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	cfg := Config{
		Endpoint:   testEndpoint,
		Deployment: testDeployment,
		APIVersion: "v1",
		Tokens:     &fakeTokens{tok: goodToken},
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

// reply writes a 200 chat completion.
func reply(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = io.WriteString(w, body)
}

func completionJSON(finish, message string) string {
	return `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-5.4-mini-2026-03-17",` +
		`"choices":[{"index":0,"finish_reason":"` + finish + `","message":` + message + `}],` +
		`"usage":{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150,"prompt_tokens_details":{"cached_tokens":64}}}`
}

func structuredRequest() providers.CompletionRequest {
	return providers.CompletionRequest{
		Messages: []providers.Message{
			{Role: providers.RoleSystem, Content: "You triage incidents."},
			{Role: providers.RoleUser, Content: "context " + promptMark},
		},
		ResponseSchema: json.RawMessage(`{"type":"object","properties":{"cause":{"type":"string"}},"required":["cause"],"additionalProperties":false}`),
	}
}

var wantUsage = providers.Usage{InputTokens: 120, CachedInputTokens: 64, OutputTokens: 30}

func TestCompleteStructuredSuccess(t *testing.T) {
	var got map[string]any
	var auth, path string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		reply(w, completionJSON("stop", `{"role":"assistant","content":"{\"cause\":\"oom\"}","refusal":null}`))
	}, nil)

	c, err := h.p.Complete(context.Background(), structuredRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if c.Content != `{"cause":"oom"}` || c.Usage != wantUsage || c.Model != "gpt-5.4-mini-2026-03-17" || c.ToolCalls != nil {
		t.Errorf("completion = %+v", c)
	}
	if path != "/openai/v1/chat/completions" || auth != "Bearer "+goodToken {
		t.Errorf("path %q, authorization header as expected: %v", path, auth == "Bearer "+goodToken)
	}
	if got["model"] != testDeployment || got["max_completion_tokens"] != float64(DefaultMaxOutputTokens) {
		t.Errorf("model %v, max_completion_tokens %v", got["model"], got["max_completion_tokens"])
	}
	rf, _ := got["response_format"].(map[string]any)
	js, _ := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" || js["strict"] != true || js["schema"] == nil {
		t.Errorf("response_format = %v", rf)
	}
	if msgs, _ := got["messages"].([]any); len(msgs) != 2 {
		t.Errorf("messages = %v", got["messages"])
	}
	r := h.lastReport(t)
	if r.Deployment != testDeployment || r.Model != c.Model || r.Usage != wantUsage || r.Attempts != 1 || r.Err != nil {
		t.Errorf("report = %+v", r)
	}
}

func TestCompleteDatedAPIVersion(t *testing.T) {
	var got map[string]any
	var path, query string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&got)
		reply(w, completionJSON("stop", `{"role":"assistant","content":"hello"}`))
	}, func(c *Config) { c.APIVersion = datedVersion; c.MaxOutputTokens = 512 })
	req := providers.CompletionRequest{Messages: []providers.Message{{Role: providers.RoleUser, Content: "hi"}}}
	if _, err := h.p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if path != "/openai/deployments/triage/chat/completions" || query != "api-version=2024-10-21" {
		t.Errorf("path %q query %q", path, query)
	}
	if _, ok := got["model"]; ok || got["max_completion_tokens"] != float64(512) {
		t.Errorf("body = %v", got)
	}
}

func TestChatURL(t *testing.T) {
	for _, tc := range []struct{ endpoint, version, want string }{
		{"https://r.services.ai.azure.com/", "v1", "https://r.services.ai.azure.com/openai/v1/chat/completions"},
		{"https://r.services.ai.azure.com", "v1", "https://r.services.ai.azure.com/openai/v1/chat/completions"},
		{"https://r.openai.azure.com/openai/v1", "v1", "https://r.openai.azure.com/openai/v1/chat/completions"},
		{"https://r.openai.azure.com/openai/v1/", "v1", "https://r.openai.azure.com/openai/v1/chat/completions"},
		{azureEndpoint, datedVersion, "https://r.openai.azure.com/openai/deployments/triage/chat/completions?api-version=2024-10-21"},
		{"https://r.openai.azure.com/openai/v1", datedVersion, ""},
		{"http://r.openai.azure.com/", "v1", ""},
		{"https://user" + "@" + "r.openai.azure.com/" /* user info, split so it does not read as an address */, "v1", ""},
		{"https://r.openai.azure.com/?x=1", "v1", ""},
		{"https://r.openai.azure.com/#f", "v1", ""},
		{"https:///path", "v1", ""},
		{"", "v1", ""},
		// A DNS host name only (threat model G3.9).
		{"https://20.1.2.3/", "v1", ""},
		{"https://20.1.2.3:8443/", "v1", ""},
		{"https://[2603:1020:1001::1]/", "v1", ""},
		{"https://[::ffff:20.1.2.3]/", "v1", ""},
		{"https://0x14.1.2.3/", "v1", ""},
		{"https://335610371/", "v1", ""},
		{"https://r.openai.azure.com./", "v1", ""},
		{"https://:443/", "v1", ""},
	} {
		got, err := chatURL(tc.endpoint, testDeployment, tc.version)
		if (err != nil) != (tc.want == "") || got != tc.want {
			t.Errorf("chatURL(%q, %q) = %q, %v; want %q", tc.endpoint, tc.version, got, err, tc.want)
		}
	}
}

// The wait for response headers follows Config.AttemptTimeout, not the default.
func TestClientHeaderTimeoutFollowsAttemptTimeout(t *testing.T) {
	for _, d := range []time.Duration{0, 5 * time.Second, 10 * time.Minute} {
		p, err := New(Config{Endpoint: azureEndpoint, Deployment: testDeployment, APIVersion: "v1",
			Tokens: &fakeTokens{}, AttemptTimeout: d})
		if err != nil {
			t.Fatal(err)
		}
		want := cmpOr(d, DefaultAttemptTimeout)
		if got := p.client.Transport.(*http.Transport).ResponseHeaderTimeout; got != want {
			t.Errorf("AttemptTimeout %v: ResponseHeaderTimeout = %v, want %v", d, got, want)
		}
	}
}

func TestNewRefusesInvalidConfig(t *testing.T) {
	ok := Config{Endpoint: azureEndpoint, Deployment: testDeployment, APIVersion: "v1", Tokens: &fakeTokens{}}
	if _, err := New(ok); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	for name, mod := range map[string]func(*Config){
		"no tokens":         func(c *Config) { c.Tokens = nil },
		"no deployment":     func(c *Config) { c.Deployment = "" },
		"deployment path":   func(c *Config) { c.Deployment = "../x" },
		"bad version":       func(c *Config) { c.APIVersion = "v2" },
		"plain http":        func(c *Config) { c.Endpoint = "http://r.openai.azure.com/" },
		"query":             func(c *Config) { c.Endpoint = "https://r.openai.azure.com/?api-key=" + promptMark },
		"output too large":  func(c *Config) { c.MaxOutputTokens = MaxOutputTokens + 1 },
		"too many attempts": func(c *Config) { c.MaxAttempts = MaxAttempts + 1 },
		"negative timeout":  func(c *Config) { c.Timeout = -time.Second },
		"timeout too long":  func(c *Config) { c.Timeout = maxConfigTimeout + time.Second },
		"delay too long":    func(c *Config) { c.MaxDelay = maxConfigDelay + time.Second },
		"body bound":        func(c *Config) { c.MaxResponseBytes = maxConfigResponseBytes + 1 },
	} {
		c := ok
		mod(&c)
		_, err := New(c)
		if !providers.IsInvalidConfig(err) {
			t.Errorf("%s: err = %v, want invalid config", name, err)
		}
		if err != nil && strings.Contains(err.Error(), promptMark) {
			t.Errorf("%s: error quotes the input", name)
		}
	}
}

func TestCompleteToolCalls(t *testing.T) {
	var got map[string]any
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		reply(w, completionJSON("tool_calls", `{"role":"assistant","content":null,"tool_calls":[`+
			`{"id":"call_1","type":"function","function":{"name":"get_logs","arguments":"{\"pod\":\"web-0\"}"}}]}`))
	}, nil)
	req := providers.CompletionRequest{
		Messages: []providers.Message{
			{Role: providers.RoleUser, Content: "why"},
			{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{{ID: callZero, Name: logsTool, Arguments: json.RawMessage(`{"pod":"a"}`)}}},
			{Role: providers.RoleTool, ToolCallID: callZero, Content: "log lines"},
		},
		Tools: []providers.ToolDefinition{{Name: logsTool, Description: "Reads logs", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	c, err := h.p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(c.ToolCalls) != 1 || c.ToolCalls[0].ID != "call_1" || c.ToolCalls[0].Name != logsTool ||
		string(c.ToolCalls[0].Arguments) != `{"pod":"web-0"}` || c.Content != "" {
		t.Errorf("completion = %+v", c)
	}
	tools, _ := got["tools"].([]any)
	msgs, _ := got["messages"].([]any)
	if len(tools) != 1 || len(msgs) != 3 {
		t.Fatalf("tools %v messages %v", got["tools"], got["messages"])
	}
	asst, _ := msgs[1].(map[string]any)
	tool, _ := msgs[2].(map[string]any)
	if asst["content"] != nil || asst["tool_calls"] == nil || tool["tool_call_id"] != callZero {
		t.Errorf("assistant %v tool %v", asst, tool)
	}
}

// Every reply that breaks the contract is ErrMalformedOutput, is not retried, and returns the usage when
// the reply had one.
func TestCompleteMalformedOutput(t *testing.T) {
	toolsReq := structuredRequest()
	toolsReq.Tools = []providers.ToolDefinition{{Name: logsTool, Parameters: json.RawMessage(`{"type":"object"}`)}}
	call := func(id, name, args string) string {
		return `{"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":` + args + `}}`
	}
	many := make([]string, maxToolCalls+1)
	for i := range many {
		many[i] = call(fmt.Sprintf("c%d", i), logsTool, `"{}"`)
	}
	for _, tc := range []struct {
		name, body  string
		contentType string
		withUsage   bool
		// unknown: billed, but no usage could be read.
		unknown bool
	}{
		{name: "not JSON", body: "<html>" + replyMark, unknown: true},
		{name: "wrong content type", body: completionJSON("stop", `{"content":"{}"}`), contentType: "text/plain", withUsage: true},
		{name: "wrong content type, no usage", body: "<html>" + replyMark, contentType: "text/html", unknown: true},
		{name: "trailing data", body: completionJSON("stop", `{"content":"{}"}`) + `{}`, unknown: true},
		{name: "no usage", body: `{"model":"m","choices":[{"finish_reason":"stop","message":{"content":"{}"}}]}`, unknown: true},
		{name: "negative usage", body: `{"model":"m","choices":[],"usage":{"prompt_tokens":-1,"completion_tokens":1}}`, unknown: true},
		{name: "cached above input", body: `{"model":"m","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":2}}}`, unknown: true},
		{name: "no model", body: `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, withUsage: true},
		{name: "model with spaces", body: `{"model":"a b","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, withUsage: true},
		{name: "no choice", body: `{"model":"m","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, withUsage: true},
		{name: "two choices", body: `{"model":"m","choices":[{"finish_reason":"stop","message":{"content":"{}"}},{"finish_reason":"stop","message":{"content":"{}"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, withUsage: true},
		{name: "cut off", body: completionJSON("length", `{"content":"{\"cause\":\"`+replyMark+`"}`), withUsage: true},
		{name: "filtered", body: completionJSON("content_filter", `{"content":""}`), withUsage: true},
		{name: "unknown finish", body: completionJSON("other", `{"content":"{}"}`), withUsage: true},
		{name: "refusal", body: completionJSON("stop", `{"content":null,"refusal":"`+replyMark+`"}`), withUsage: true},
		{name: "empty content", body: completionJSON("stop", `{"content":""}`), withUsage: true},
		{name: "not an object", body: completionJSON("stop", `{"content":"`+replyMark+`"}`), withUsage: true},
		{name: "array", body: completionJSON("stop", `{"content":"[1]"}`), withUsage: true},
		{name: "tool not offered", body: completionJSON("tool_calls", `{"tool_calls":[`+call("c1", "delete_pod", `"{}"`)+`]}`), withUsage: true},
		{name: "tool id invalid", body: completionJSON("tool_calls", `{"tool_calls":[`+call("c 1", logsTool, `"{}"`)+`]}`), withUsage: true},
		{name: "tool id repeated", body: completionJSON("tool_calls", `{"tool_calls":[`+call("c1", logsTool, `"{}"`)+`,`+call("c1", logsTool, `"{}"`)+`]}`), withUsage: true},
		{name: "tool args not object", body: completionJSON("tool_calls", `{"tool_calls":[`+call("c1", logsTool, `"[]"`)+`]}`), withUsage: true},
		{name: "tool args not JSON", body: completionJSON("tool_calls", `{"tool_calls":[`+call("c1", logsTool, `"{`+replyMark+`"`)+`]}`), withUsage: true},
		{name: "tool not a function", body: completionJSON("tool_calls", `{"tool_calls":[{"id":"c1","type":"code","function":{"name":"get_logs","arguments":"{}"}}]}`), withUsage: true},
		{name: "too many tool calls", body: completionJSON("tool_calls", `{"tool_calls":[`+strings.Join(many, ",")+`]}`), withUsage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				ct := tc.contentType
				if ct == "" {
					ct = "application/json"
				}
				w.Header().Set("Content-Type", ct)
				_, _ = io.WriteString(w, tc.body)
			}, nil)
			c, err := h.p.Complete(context.Background(), toolsReq)
			if !providers.IsMalformedOutput(err) || providers.IsRetryable(err) {
				t.Fatalf("err = %v, want malformed output, not retryable", err)
			}
			if h.requests.Load() != 1 {
				t.Errorf("sent %d requests, want 1", h.requests.Load())
			}
			if c.Content != "" || c.ToolCalls != nil {
				t.Errorf("malformed reply returned content: %+v", c)
			}
			if tc.withUsage != (c.Usage != providers.Usage{}) {
				t.Errorf("usage = %+v, want usage %v", c.Usage, tc.withUsage)
			}
			if c.UsageUnknown != tc.unknown {
				t.Errorf("UsageUnknown = %v, want %v", c.UsageUnknown, tc.unknown)
			}
			if tc.name == "wrong content type" && (c.Usage != wantUsage || c.Model == "") {
				t.Errorf("billed reply lost its usage or model: %+v", c)
			}
			if r := h.lastReport(t); r.Usage != c.Usage || r.Model != c.Model || r.UsageUnknown != c.UsageUnknown || r.Err == nil {
				t.Errorf("report = %+v", r)
			}
			if strings.Contains(err.Error(), replyMark) {
				t.Error("error quotes the reply")
			}
		})
	}
}

// A 200 whose body could not be read was billed: retrying it leaves the call's usage unknown.
func TestCompleteBilledReadFailureIsUsageUnknown(t *testing.T) {
	var n atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "4096")
			_, _ = io.WriteString(w, `{"id":`) // the connection closes short of Content-Length
			return
		}
		reply(w, completionJSON("stop", `{"content":"{}"}`))
	}, nil)
	c, err := h.p.Complete(context.Background(), structuredRequest())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if h.requests.Load() != 2 || c.Usage != wantUsage || !c.UsageUnknown {
		t.Errorf("requests %d, completion %+v; want 2 requests, the usage of the second and UsageUnknown",
			h.requests.Load(), c)
	}
	if r := h.lastReport(t); !r.UsageUnknown || r.Usage != wantUsage {
		t.Errorf("report = %+v", r)
	}
}

func TestCompleteOversizedReply(t *testing.T) {
	big := completionJSON("stop", `{"content":"{\"x\":\"`+strings.Repeat("a", 4096)+`\"}"}`)
	for name, flush := range map[string]bool{"with content length": false, "chunked": true} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if flush {
					w.(http.Flusher).Flush() // no Content-Length: the limit is found while reading
				}
				_, _ = io.WriteString(w, big)
			}, func(c *Config) { c.MaxResponseBytes = 1024 })
			c, err := h.p.Complete(context.Background(), structuredRequest())
			if !providers.IsTooLarge(err) || providers.IsRetryable(err) {
				t.Fatalf("err = %v, want too large", err)
			}
			if h.requests.Load() != 1 {
				t.Errorf("sent %d requests, want 1", h.requests.Load())
			}
			// Billed, but a cut body is not parsed: its usage is unknown, and said so.
			if !c.UsageUnknown || c.Usage != (providers.Usage{}) || c.Content != "" {
				t.Errorf("completion = %+v, want only UsageUnknown", c)
			}
			if r := h.lastReport(t); !r.UsageUnknown {
				t.Errorf("report = %+v, want UsageUnknown", r)
			}
		})
	}
}

func TestCompleteRetriesThrottlingThenSucceeds(t *testing.T) {
	var n atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			w.Header().Set("retry-after-ms", "1500")
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			reply(w, completionJSON("stop", `{"content":"{}"}`))
		}
	}, nil)
	c, err := h.p.Complete(context.Background(), structuredRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if c.Usage != wantUsage {
		t.Errorf("usage = %+v", c.Usage)
	}
	// The delay asked for, then the exponential backoff of the second attempt.
	if want := []time.Duration{1500 * time.Millisecond, 2 * DefaultBaseDelay}; fmt.Sprint(h.sleeps) != fmt.Sprint(want) {
		t.Errorf("sleeps = %v, want %v", h.sleeps, want)
	}
	if r := h.lastReport(t); r.Attempts != 3 || r.Err != nil {
		t.Errorf("report = %+v", r)
	}
}

func TestCompleteRetryBound(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusRequestTimeout} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}, func(c *Config) { c.MaxAttempts = 4 })
			_, err := h.p.Complete(context.Background(), structuredRequest())
			if !providers.IsRetryable(err) {
				t.Fatalf("err = %v, want retryable", err)
			}
			if h.requests.Load() != 4 || len(h.sleeps) != 3 {
				t.Errorf("requests %d sleeps %d, want 4 and 3", h.requests.Load(), len(h.sleeps))
			}
			if r := h.lastReport(t); r.Attempts != 4 {
				t.Errorf("attempts = %d", r.Attempts)
			}
		})
	}
}

// A delay asked for that is longer than MaxDelay is not waited for: it goes back to the caller at once.
func TestCompleteReturnsLongRetryAfter(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}, nil)
	_, err := h.p.Complete(context.Background(), structuredRequest())
	if d, ok := providers.RetryAfter(err); !ok || d != 120*time.Second {
		t.Fatalf("RetryAfter = %v, %v (err %v)", d, ok, err)
	}
	if h.requests.Load() != 1 || len(h.sleeps) != 0 {
		t.Errorf("requests %d sleeps %v", h.requests.Load(), h.sleeps)
	}
}

func TestCompleteFinalStatuses(t *testing.T) {
	for code, is := range map[int]func(error) bool{
		http.StatusBadRequest:            providers.IsRejected,
		http.StatusUnauthorized:          providers.IsForbidden,
		http.StatusForbidden:             providers.IsForbidden,
		http.StatusNotFound:              providers.IsNotFound,
		http.StatusRequestEntityTooLarge: providers.IsTooLarge,
		http.StatusUnprocessableEntity:   providers.IsRejected,
		http.StatusFound:                 providers.IsRejected, // a redirect is never followed
	} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				if code == http.StatusFound {
					w.Header().Set("Location", "https://example.invalid/steal")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				_, _ = io.WriteString(w, `{"error":{"message":"`+bodyMark+` `+promptMark+`"}}`)
			}, nil)
			_, err := h.p.Complete(context.Background(), structuredRequest())
			if !is(err) || providers.IsRetryable(err) {
				t.Fatalf("err = %v", err)
			}
			if h.requests.Load() != 1 {
				t.Errorf("sent %d requests, want 1", h.requests.Load())
			}
			if s := err.Error(); strings.Contains(s, bodyMark) || strings.Contains(s, promptMark) ||
				strings.Contains(s, testEndpoint) {
				t.Errorf("error leaks the body or the URL: %v", err)
			}
		})
	}
}

func TestCompleteCancellation(t *testing.T) {
	t.Run("during a request", func(t *testing.T) {
		started := make(chan struct{})
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			close(started)
			_, _ = io.Copy(io.Discard, r.Body) // the server sees the client go away only after the body is read
			<-r.Context().Done()
		}, nil)
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-started; cancel() }()
		_, err := h.p.Complete(ctx, structuredRequest())
		if !errors.Is(err, context.Canceled) || providers.IsRetryable(err) {
			t.Fatalf("err = %v, want canceled, not retryable", err)
		}
		if h.requests.Load() != 1 {
			t.Errorf("sent %d requests, want 1", h.requests.Load())
		}
	})
	t.Run("during the backoff", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}, nil)
		ctx, cancel := context.WithCancel(context.Background())
		h.p.sleep = func(context.Context, time.Duration) error { cancel(); return context.Canceled }
		_, err := h.p.Complete(ctx, structuredRequest())
		if !errors.Is(err, context.Canceled) || providers.IsRetryable(err) {
			t.Fatalf("err = %v, want canceled", err)
		}
		if h.requests.Load() != 1 {
			t.Errorf("sent %d requests, want 1", h.requests.Load())
		}
	})
	t.Run("before the call", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {}, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := h.p.Complete(ctx, structuredRequest()); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if h.requests.Load() != 0 {
			t.Errorf("sent %d requests, want 0", h.requests.Load())
		}
	})
}

// One attempt that hangs times out and is retried; the call's own deadline ends it as retryable.
func TestCompleteTimeouts(t *testing.T) {
	t.Run("attempt timeout is retried", func(t *testing.T) {
		var n atomic.Int32
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			if n.Add(1) == 1 {
				_, _ = io.Copy(io.Discard, r.Body) // the server sees the client go away only after the body is read
				<-r.Context().Done()
				return
			}
			reply(w, completionJSON("stop", `{"content":"{}"}`))
		}, func(c *Config) { c.AttemptTimeout = 100 * time.Millisecond })
		if _, err := h.p.Complete(context.Background(), structuredRequest()); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if h.requests.Load() != 2 {
			t.Errorf("sent %d requests, want 2", h.requests.Load())
		}
	})
	t.Run("call deadline", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body) // the server sees the client go away only after the body is read
			<-r.Context().Done()
		}, func(c *Config) { c.Timeout = 100 * time.Millisecond })
		_, err := h.p.Complete(context.Background(), structuredRequest())
		if !errors.Is(err, context.DeadlineExceeded) || !providers.IsRetryable(err) {
			t.Fatalf("err = %v, want deadline exceeded, retryable", err)
		}
		if h.requests.Load() != 1 {
			t.Errorf("sent %d requests, want 1", h.requests.Load())
		}
	})
	t.Run("no backoff past the deadline", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}, func(c *Config) { c.Timeout = time.Second; c.BaseDelay = 5 * time.Second })
		_, err := h.p.Complete(context.Background(), structuredRequest())
		if !providers.IsRetryable(err) || h.requests.Load() != 1 || len(h.sleeps) != 0 {
			t.Fatalf("err %v requests %d sleeps %v", err, h.requests.Load(), h.sleeps)
		}
	})
}

func TestCompleteTokenAudience(t *testing.T) {
	for name, tc := range map[string]struct {
		tok string
		ok  bool
	}{
		"audience with slash": {fakeJWT("https://cognitiveservices.azure.com/"), true},
		"audience in a list":  {fakeJWT([]string{"x", "https://cognitiveservices.azure.com"}), true},
		"other audience":      {fakeJWT("https://management.azure.com"), false},
		"look-alike audience": {fakeJWT("https://cognitiveservices.azure.com.evil.example"), false},
		"no audience":         {fakeJWT(nil), false},
		"not a JWT":           {"opaque-api-key-" + promptMark, false},
		"empty":               {"", false},
		"bad signature part":  {strings.TrimSuffix(goodToken, "ZQ") + "!!", false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				reply(w, completionJSON("stop", `{"content":"{}"}`))
			}, func(c *Config) { c.Tokens = &fakeTokens{tok: tc.tok} })
			_, err := h.p.Complete(context.Background(), structuredRequest())
			if tc.ok != (err == nil) {
				t.Fatalf("err = %v, want ok %v", err, tc.ok)
			}
			if !tc.ok {
				if h.requests.Load() != 0 {
					t.Error("a token for another audience was sent")
				}
				if providers.IsRetryable(err) || strings.Contains(err.Error(), promptMark) {
					t.Errorf("err = %v", err)
				}
				// The same class as WorkloadIdentity's refusal of such a token.
				if !providers.IsForbidden(err) || providers.IsInvalidConfig(err) {
					t.Errorf("err = %v, want forbidden only", err)
				}
			}
		})
	}
}

func TestCompleteTokenErrors(t *testing.T) {
	leak := errors.New("token endpoint said " + bodyMark)
	for name, tc := range map[string]struct {
		err       error
		retryable bool
		forbidden bool
		attempts  int32
	}{
		"retryable": {&providers.RetryableError{Err: leak}, true, false, 3},
		"forbidden": {fmt.Errorf("%w: %w", leak, providers.ErrForbidden), false, true, 1},
		"unknown":   {leak, false, false, 1},
	} {
		t.Run(name, func(t *testing.T) {
			tokens := &fakeTokens{err: tc.err}
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {}, func(c *Config) { c.Tokens = tokens })
			_, err := h.p.Complete(context.Background(), structuredRequest())
			if err == nil || providers.IsRetryable(err) != tc.retryable || providers.IsForbidden(err) != tc.forbidden {
				t.Fatalf("err = %v", err)
			}
			if errors.Is(err, leak) || strings.Contains(fmt.Sprintf("%+v", err), bodyMark) {
				t.Errorf("the token source's error is reachable: %v", err)
			}
			if tokens.calls.Load() != tc.attempts || h.requests.Load() != 0 {
				t.Errorf("token calls %d requests %d", tokens.calls.Load(), h.requests.Load())
			}
		})
	}
}

func TestCompleteRefusesInvalidRequests(t *testing.T) {
	user := providers.Message{Role: providers.RoleUser, Content: promptMark}
	for name, req := range map[string]providers.CompletionRequest{
		"no messages":       {},
		"unknown role":      {Messages: []providers.Message{{Role: "developer", Content: promptMark}}},
		"output too large":  {Messages: []providers.Message{user}, MaxOutputTokens: MaxOutputTokens + 1},
		"negative output":   {Messages: []providers.Message{user}, MaxOutputTokens: -1},
		"schema not object": {Messages: []providers.Message{user}, ResponseSchema: json.RawMessage(`"x"`)},
		"tool name":         {Messages: []providers.Message{user}, Tools: []providers.ToolDefinition{{Name: "a b", Parameters: json.RawMessage(`{}`)}}},
		"tool repeated": {Messages: []providers.Message{user}, Tools: []providers.ToolDefinition{
			{Name: "a", Parameters: json.RawMessage(`{}`)}, {Name: "a", Parameters: json.RawMessage(`{}`)}}},
		"tool parameters":    {Messages: []providers.Message{user}, Tools: []providers.ToolDefinition{{Name: "a"}}},
		"tool message no id": {Messages: []providers.Message{{Role: providers.RoleTool, Content: "x"}}},
		"user with calls":    {Messages: []providers.Message{{Role: providers.RoleUser, ToolCalls: []providers.ToolCall{{ID: "c", Name: "a", Arguments: json.RawMessage(`{}`)}}}}},
		"assistant call args": {Messages: []providers.Message{{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{
			{ID: "c", Name: "a", Arguments: json.RawMessage(`[]`)}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {}, nil)
			_, err := h.p.Complete(context.Background(), req)
			if !providers.IsRejected(err) || strings.Contains(err.Error(), promptMark) {
				t.Fatalf("err = %v, want rejected without the prompt", err)
			}
			if h.requests.Load() != 0 {
				t.Errorf("sent %d requests", h.requests.Load())
			}
		})
	}
}

// Nothing the adapter returns, reports or logs holds the token, the prompt or the reply, on any path.
func TestNoSecretInErrorsReportsOrLogs(t *testing.T) {
	var logged strings.Builder
	ctx := logr.NewContext(context.Background(), funcr.New(func(prefix, args string) {
		logged.WriteString(prefix + args + "\n")
	}, funcr.Options{Verbosity: 10}))
	var n atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, bodyMark)
		case 2:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, bodyMark+promptMark)
		default:
			reply(w, completionJSON("length", `{"content":"`+replyMark+`"}`))
		}
	}, nil)
	_, err := h.p.Complete(ctx, structuredRequest())
	if !providers.IsMalformedOutput(err) {
		t.Fatalf("err = %v", err)
	}
	r := h.lastReport(t)
	dump := fmt.Sprintf("%v %+v %#v %+v", err, err, err, r) + logged.String()
	for _, s := range []string{promptMark, replyMark, bodyMark, goodToken, "TOKEN-SECRET", testEndpoint} {
		if strings.Contains(dump, s) {
			t.Errorf("%q leaked: %s", s, dump)
		}
	}
	if logged.Len() != 0 {
		t.Errorf("the adapter logged: %s", logged.String())
	}
}

func TestParseResponseAcceptsStructuredWithToolCalls(t *testing.T) {
	body := completionJSON("tool_calls", `{"content":"thinking","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{}"}}]}`)
	c, err := parseResponse([]byte(body), map[string]bool{"t": true}, true)
	if err != nil || len(c.ToolCalls) != 1 || c.Content != "thinking" {
		t.Fatalf("c = %+v, err = %v", c, err)
	}
}

// FuzzParseResponse feeds arbitrary bodies to the reply parser: it never panics, a success always has
// content or a tool call of an offered tool with object arguments, and an error never quotes the body.
func FuzzParseResponse(f *testing.F) {
	f.Add([]byte(completionJSON("stop", `{"content":"{\"a\":1}"}`)), true)
	f.Add([]byte(completionJSON("tool_calls", `{"tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{}"}}]}`)), false)
	f.Add([]byte(`{"model":"m","choices":[],"usage":{}}`), false)
	f.Add([]byte(`not json`), true)
	f.Fuzz(func(t *testing.T, data []byte, structured bool) {
		c, err := parseResponse(data, map[string]bool{"t": true}, structured)
		if err != nil {
			if !providers.IsMalformedOutput(err) {
				t.Fatalf("error of another class: %v", err)
			}
			if c.Content != "" || c.ToolCalls != nil {
				t.Fatal("content returned with an error")
			}
			if len(data) > 8 && strings.Contains(err.Error(), string(data)) {
				t.Fatal("error quotes the body")
			}
			return
		}
		if c.Content == "" && len(c.ToolCalls) == 0 {
			t.Fatal("success without content or tool call")
		}
		if structured && len(c.ToolCalls) == 0 && !isJSONObject([]byte(c.Content)) {
			t.Fatal("structured success that is not a JSON object")
		}
		for _, tc := range c.ToolCalls {
			if tc.Name != "t" || !isJSONObject(tc.Arguments) || !toolCallIDPattern.MatchString(tc.ID) {
				t.Fatalf("unchecked tool call %+v", tc)
			}
		}
		if c.Usage.InputTokens < 0 || c.Usage.OutputTokens < 0 || c.Usage.CachedInputTokens > c.Usage.InputTokens {
			t.Fatalf("usage out of range %+v", c.Usage)
		}
	})
}

func TestRefusePrivate(t *testing.T) {
	for addr, ok := range map[string]bool{
		"20.1.2.3:443":            true,
		"[2603:1020:1001::1]:443": true,
		"127.0.0.1:443":           false,
		"10.0.0.1:443":            false,
		"172.16.0.1:443":          false,
		"192.168.1.1:443":         false,
		"169.254.169.254:80":      false,
		"100.64.1.1:443":          false,
		"0.0.0.0:443":             false,
		"224.0.0.1:443":           false,
		"[::1]:443":               false,
		"[fe80::1]:443":           false,
		"[fd00::1]:443":           false,
		"[::ffff:10.0.0.1]:443":   false,
		"not-an-address":          false,
		"[fe80::1%en0]:443":       false,
		// Reserved IPv4.
		"192.0.0.8:443":     false,
		"192.0.2.1:443":     false,
		"198.18.0.1:443":    false,
		"198.19.255.1:443":  false,
		"198.51.100.1:443":  false,
		"203.0.113.1:443":   false,
		"240.0.0.1:443":     false,
		"255.255.255.255:1": false,
		"0.1.2.3:443":       false,
		"100.127.0.1:443":   false,
		// NAT64 well-known prefix: judged by the embedded IPv4.
		"[64:ff9b::1401:203]:443": true,  // 20.1.2.3
		"[64:ff9b::a00:1]:443":    false, // 10.0.0.1
		"[64:ff9b::7f00:1]:443":   false, // 127.0.0.1
		"[64:ff9b::a9fe:a9fe]:80": false, // 169.254.169.254
		"[64:ff9b::6440:101]:443": false, // 100.64.1.1
		// Local-use NAT64: the IPv4 position is not fixed, refused outright.
		"[64:ff9b:1::1401:203]:443": false,
		// 6to4: judged by the embedded IPv4.
		"[2002:1401:203::1]:443": true,  // 20.1.2.3
		"[2002:a00:1::1]:443":    false, // 10.0.0.1
		"[2002:7f00:1::1]:443":   false, // 127.0.0.1
		// IPv4-compatible (deprecated): judged by the embedded IPv4.
		"[::1401:203]:443": true,
		"[::a00:1]:443":    false,
		"[::]:443":         false,
		// Teredo, SIIT, other reserved IPv6: refused outright.
		"[2001:0:4136:e378:8000:63bf:3fff:fdd2]:443": false,
		"[2001::1401:203]:443":                       false,
		"[::ffff:0:a00:1]:443":                       false,
		"[100::1]:443":                               false,
		"[2001:db8::1]:443":                          false,
		"[3fff::1]:443":                              false,
		"[fec0::1]:443":                              false,
		"[ff02::1]:443":                              false,
	} {
		if err := refusePrivate("tcp", addr, nil); (err == nil) != ok {
			t.Errorf("refusePrivate(%s) = %v, want allowed %v", addr, err, ok)
		}
	}
}

// The default client never connects to a loopback endpoint, so no token reaches it. The endpoint is a host
// name that resolves to loopback: an IP literal is refused by New already.
func TestDefaultClientRefusesALoopbackEndpoint(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	if _, err := New(Config{Endpoint: srv.URL, Deployment: testDeployment, APIVersion: "v1", Tokens: &fakeTokens{tok: goodToken}}); !providers.IsInvalidConfig(err) {
		t.Fatalf("IP literal endpoint: err = %v, want invalid config", err)
	}
	p, err := New(Config{Endpoint: "https://localhost:" + port, Deployment: testDeployment, APIVersion: "v1", Tokens: &fakeTokens{tok: goodToken}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Complete(context.Background(), structuredRequest())
	if !providers.IsInvalidConfig(err) || providers.IsRetryable(err) || hits.Load() != 0 {
		t.Fatalf("err = %v, hits %d", err, hits.Load())
	}
}
