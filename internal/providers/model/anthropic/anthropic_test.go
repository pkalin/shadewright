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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	keyMark    = "sk-ant-KEY-SECRET-0e1d"
)

// Fixtures used in several tests.
const (
	testModel = "claude-opus-5-5"
	logsTool  = "get_logs"
	callZero  = "toolu_0"
	callOne   = "toolu_1"
)

// harness is a provider wired to an httptest TLS server that stands in for the API's host.
type harness struct {
	p        *Provider
	srv      *httptest.Server
	keyFile  string
	requests atomic.Int32
	sleeps   []time.Duration
	reports  []CallReport
	mu       sync.Mutex
}

func writeKey(t *testing.T, path, key string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newHarness(t *testing.T, handler http.HandlerFunc) *harness {
	t.Helper()
	h := &harness{keyFile: filepath.Join(t.TempDir(), "api-key")}
	writeKey(t, h.keyFile, keyMark+"\n")
	h.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		handler(w, r)
	}))
	t.Cleanup(h.srv.Close)
	// Every connection goes to the test server; its certificate names example.com, so that is the name the
	// TLS check expects, while the request still names the API's own host.
	tr := h.srv.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.ServerName = "example.com"
	addr := h.srv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	cfg := Config{
		Model:   testModel,
		KeyFile: h.keyFile,
		OnCall: func(r CallReport) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.reports = append(h.reports, r)
		},
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
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

// messageJSON is a reply of the Messages API naming model, with the given stop reason and content blocks.
func messageJSON(model, stop, content string) string {
	return `{"id":"msg_01","type":"message","role":"assistant","model":"` + model + `","content":` + content +
		`,"stop_reason":"` + stop + `","stop_sequence":null,` +
		`"usage":{"input_tokens":20,"cache_creation_input_tokens":36,"cache_read_input_tokens":64,"output_tokens":30}}`
}

// wantUsage is the usage of messageJSON: the whole prompt is 20 + 36 + 64 tokens, 64 of them read from the
// cache.
var wantUsage = providers.Usage{InputTokens: 120, CachedInputTokens: 64, OutputTokens: 30}

func structuredRequest() providers.CompletionRequest {
	return providers.CompletionRequest{
		Messages: []providers.Message{
			{Role: providers.RoleSystem, Content: "You triage incidents."},
			{Role: providers.RoleUser, Content: "context " + promptMark},
		},
		ResponseSchema: json.RawMessage(`{"type":"object","properties":{"cause":{"type":"string","maxLength":64}},"required":["cause"],"additionalProperties":false}`),
	}
}

func TestCompleteStructuredSuccess(t *testing.T) {
	var got map[string]any
	var hdr http.Header
	var host, path string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hdr, host, path = r.Header.Clone(), r.Host, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		reply(w, messageJSON(testModel, stopEndTurn,
			`[{"type":"thinking","thinking":"","signature":"sig"},{"type":"text","text":"{\"cause\":\"oom\"}"}]`))
	})

	c, err := h.p.Complete(context.Background(), structuredRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if c.Content != `{"cause":"oom"}` || c.Usage != wantUsage || c.Model != testModel || c.ToolCalls != nil {
		t.Errorf("completion = %+v", c)
	}
	if host != "api.anthropic.com" || path != "/v1/messages" {
		t.Errorf("host %q, path %q", host, path)
	}
	if hdr.Get("x-api-key") != keyMark || hdr.Get("anthropic-version") != APIVersion || hdr.Get("Authorization") != "" {
		t.Errorf("x-api-key as expected: %v, anthropic-version %q", hdr.Get("x-api-key") == keyMark,
			hdr.Get("anthropic-version"))
	}
	if got["model"] != testModel || got["max_tokens"] != float64(DefaultMaxOutputTokens) {
		t.Errorf("model %v, max_tokens %v", got["model"], got["max_tokens"])
	}
	for _, k := range []string{"temperature", "top_p", "top_k", "thinking", "fallbacks"} {
		if _, ok := got[k]; ok {
			t.Errorf("the request sends %s", k)
		}
	}
	system, _ := got["system"].([]any)
	last, _ := system[len(system)-1].(map[string]any)
	if len(system) != 1 || last["text"] != "You triage incidents." ||
		fmt.Sprint(last["cache_control"]) != "map[type:ephemeral]" {
		t.Errorf("system = %v", got["system"])
	}
	format, _ := got["output_config"].(map[string]any)["format"].(map[string]any)
	schema, _ := json.Marshal(format["schema"])
	if format["type"] != "json_schema" || strings.Contains(string(schema), `"maxLength":`) ||
		!strings.Contains(string(schema), `Bounds: maxLength 64.`) {
		t.Errorf("output_config.format = %v", format)
	}
	if msgs, _ := got["messages"].([]any); len(msgs) != 1 {
		t.Errorf("messages = %v", got["messages"])
	}
	r := h.lastReport(t)
	if r.ConfiguredModel != testModel || r.Model != c.Model || r.Usage != wantUsage || r.Attempts != 1 || r.Err != nil {
		t.Errorf("report = %+v", r)
	}
}

// A tool loop: the earlier tool calls and their results go back in turns the API accepts, and the reply's
// tool call reaches the caller.
func TestCompleteToolUseRoundTrip(t *testing.T) {
	var got struct {
		Messages []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
		Tools []map[string]any `json:"tools"`
	}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		reply(w, messageJSON(testModel, stopToolUse, `[{"type":"thinking","thinking":"","signature":"s"},`+
			`{"type":"text","text":"Reading the logs."},`+
			`{"type":"tool_use","id":"toolu_2","name":"`+logsTool+`","input":{"pod":"web-1"}}]`))
	})
	req := providers.CompletionRequest{
		Messages: []providers.Message{
			{Role: providers.RoleSystem, Content: "instructions"},
			{Role: providers.RoleUser, Content: "context"},
			{Role: providers.RoleAssistant, ToolCalls: []providers.ToolCall{
				{ID: callZero, Name: logsTool, Arguments: json.RawMessage(`{"pod":"web-0"}`)},
				{ID: callOne, Name: logsTool, Arguments: json.RawMessage(`{"pod":"web-2"}`)},
			}},
			{Role: providers.RoleTool, ToolCallID: callZero, Content: "log line"},
			{Role: providers.RoleTool, ToolCallID: callOne, Content: ""},
			{Role: providers.RoleUser, Content: "last turn"},
		},
		Tools: []providers.ToolDefinition{{Name: logsTool, Description: "Reads logs.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"pod":{"type":"string"}}}`)}},
		ResponseSchema: json.RawMessage(`{"type":"object"}`),
	}
	c, err := h.p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(c.ToolCalls) != 1 || c.ToolCalls[0].ID != "toolu_2" || c.ToolCalls[0].Name != logsTool ||
		string(c.ToolCalls[0].Arguments) != `{"pod":"web-1"}` || c.Content != "Reading the logs." {
		t.Errorf("completion = %+v", c)
	}
	if len(got.Tools) != 1 || got.Tools[0]["name"] != logsTool || got.Tools[0]["input_schema"] == nil {
		t.Errorf("tools = %v", got.Tools)
	}
	roles := make([]string, len(got.Messages))
	for i, m := range got.Messages {
		roles[i] = m.Role
	}
	if strings.Join(roles, ",") != "user,assistant,user" {
		t.Fatalf("roles = %v", roles)
	}
	asst, results := got.Messages[1].Content, got.Messages[2].Content
	if len(asst) != 2 || asst[0]["type"] != blockToolUse || asst[0]["id"] != callZero ||
		fmt.Sprint(asst[0]["input"]) != "map[pod:web-0]" {
		t.Errorf("assistant turn = %v", asst)
	}
	if len(results) != 3 || results[0]["type"] != blockToolResult || results[0]["tool_use_id"] != callZero ||
		results[0]["content"] != "log line" || results[1]["tool_use_id"] != callOne ||
		results[2]["type"] != blockText || results[2]["text"] != "last turn" {
		t.Errorf("results turn = %v", results)
	}
}

func TestCompleteRefusesAnotherModel(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, messageJSON("claude-opus-4-8", stopEndTurn, `[{"type":"text","text":"{\"cause\":\"oom\"}"}]`))
	})
	c, err := h.p.Complete(context.Background(), structuredRequest())
	if !providers.IsMalformedOutput(err) {
		t.Fatalf("err = %v, want malformed output", err)
	}
	if c.Content != "" || c.Model != "claude-opus-4-8" || c.Usage != wantUsage || c.UsageUnknown {
		t.Errorf("completion = %+v, want usage and the reply's model only", c)
	}
}

func TestCompleteMalformedOutputKeepsUsage(t *testing.T) {
	text := func(s string) string { return `[{"type":"text","text":` + s + `}]` }
	tests := []struct {
		name, stop, content string
	}{
		{"cut off", "max_tokens", text(`"{\"cause\":"`)},
		{"refused", "refusal", `[]`},
		{"unknown stop reason", "pause_turn", text(`"{}"`)},
		{"not a JSON object", stopEndTurn, text(`"` + replyMark + `"`)},
		{"no content", stopEndTurn, `[]`},
		{"unknown block", stopEndTurn, `[{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}]`},
		{"tool not offered", stopToolUse, `[{"type":"tool_use","id":"toolu_1","name":"delete_pod","input":{}}]`},
		{"bad tool ID", stopToolUse, `[{"type":"tool_use","id":"bad id","name":"` + logsTool + `","input":{}}]`},
		{"input not an object", stopToolUse, `[{"type":"tool_use","id":"toolu_1","name":"` + logsTool + `","input":[1]}]`},
		{"repeated tool ID", stopToolUse, `[{"type":"tool_use","id":"toolu_1","name":"` + logsTool + `","input":{}},` +
			`{"type":"tool_use","id":"toolu_1","name":"` + logsTool + `","input":{}}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				reply(w, messageJSON(testModel, tt.stop, tt.content))
			})
			req := structuredRequest()
			req.Tools = []providers.ToolDefinition{{Name: logsTool, Parameters: json.RawMessage(`{"type":"object"}`)}}
			c, err := h.p.Complete(context.Background(), req)
			if !providers.IsMalformedOutput(err) {
				t.Fatalf("err = %v, want malformed output", err)
			}
			if c.Content != "" || c.ToolCalls != nil || c.Usage != wantUsage || c.Model != testModel {
				t.Errorf("completion = %+v", c)
			}
			if h.requests.Load() != 1 {
				t.Errorf("sent %d requests, want 1", h.requests.Load())
			}
		})
	}
	t.Run("not JSON", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, messageJSON(testModel, stopEndTurn, text(`"{}"`)))
		})
		c, err := h.p.Complete(context.Background(), structuredRequest())
		if !providers.IsMalformedOutput(err) || c.Usage != wantUsage || c.UsageUnknown {
			t.Fatalf("err = %v, completion = %+v", err, c)
		}
	})
	t.Run("no usage", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			reply(w, `{"type":"message","role":"assistant","model":"`+testModel+`","content":[],"stop_reason":"end_turn"}`)
		})
		c, err := h.p.Complete(context.Background(), structuredRequest())
		if !providers.IsMalformedOutput(err) || !c.UsageUnknown || !h.lastReport(t).UsageUnknown {
			t.Fatalf("err = %v, completion = %+v", err, c)
		}
	})
}

func TestCompleteRetriesThrottlingWithRetryAfter(t *testing.T) {
	var n atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		reply(w, messageJSON(testModel, stopEndTurn, `[{"type":"text","text":"{}"}]`))
	})
	if _, err := h.p.Complete(context.Background(), structuredRequest()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(h.sleeps) != 1 || h.sleeps[0] != 7*time.Second {
		t.Errorf("sleeps = %v, want the 7s the API asked for", h.sleeps)
	}
	if r := h.lastReport(t); r.Attempts != 2 || r.Err != nil {
		t.Errorf("report = %+v", r)
	}
}

func TestCompleteReturnsLongRetryAfter(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := h.p.Complete(context.Background(), structuredRequest())
	if d, ok := providers.RetryAfter(err); !providers.IsRetryable(err) || !ok || d != 2*time.Minute {
		t.Fatalf("err = %v, retry after %v", err, d)
	}
	if h.requests.Load() != 1 || len(h.sleeps) != 0 {
		t.Errorf("sent %d requests and slept %v, want 1 and none", h.requests.Load(), h.sleeps)
	}
}

// An HTTP date in retry-after is not read: it would compare the server's clock with the local one.
func TestRetryAfterIgnoresDates(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
	if d := retryAfter(h); d != 0 {
		t.Errorf("retryAfter(date) = %v, want 0", d)
	}
}

func TestCompleteServerErrorsAreRetriedThenReturned(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, 529} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"`+bodyMark+`"}}`)
			})
			_, err := h.p.Complete(context.Background(), structuredRequest())
			if !providers.IsRetryable(err) {
				t.Fatalf("err = %v, want retryable", err)
			}
			if h.requests.Load() != DefaultMaxAttempts {
				t.Errorf("sent %d requests, want %d", h.requests.Load(), DefaultMaxAttempts)
			}
			if want := []time.Duration{time.Second, 2 * time.Second}; fmt.Sprint(h.sleeps) != fmt.Sprint(want) {
				t.Errorf("sleeps = %v, want %v", h.sleeps, want)
			}
		})
	}
}

func TestCompleteFinalStatuses(t *testing.T) {
	tests := []struct {
		code int
		is   error
	}{
		{http.StatusNotFound, providers.ErrNotFound},
		{http.StatusUnauthorized, providers.ErrForbidden},
		{http.StatusPaymentRequired, providers.ErrForbidden},
		{http.StatusForbidden, providers.ErrForbidden},
		{http.StatusRequestEntityTooLarge, providers.ErrTooLarge},
		{http.StatusBadRequest, providers.ErrRejected},
		// A redirect is not followed: the key goes to the API's host only.
		{http.StatusTemporaryRedirect, providers.ErrRejected},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.code), func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				if tt.code == http.StatusTemporaryRedirect {
					w.Header().Set("Location", "https://elsewhere.example/v1/messages")
				}
				w.WriteHeader(tt.code)
				_, _ = io.WriteString(w, bodyMark)
			})
			_, err := h.p.Complete(context.Background(), structuredRequest())
			if !errors.Is(err, tt.is) || providers.IsRetryable(err) {
				t.Fatalf("err = %v, want %v and not retryable", err, tt.is)
			}
			if h.requests.Load() != 1 {
				t.Errorf("sent %d requests, want 1", h.requests.Load())
			}
		})
	}
}

func TestCompleteCancellation(t *testing.T) {
	t.Run("during a request", func(t *testing.T) {
		started := make(chan struct{})
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
			close(started)
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
		})
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
		})
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
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
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

// The key file is read at every call, so a rotated key is sent without a new Provider.
func TestCompleteReadsTheKeyFileEveryCall(t *testing.T) {
	var keys []string
	var mu sync.Mutex
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("x-api-key"))
		mu.Unlock()
		reply(w, messageJSON(testModel, stopEndTurn, `[{"type":"text","text":"{}"}]`))
	})
	if _, err := h.p.Complete(context.Background(), structuredRequest()); err != nil {
		t.Fatal(err)
	}
	writeKey(t, h.keyFile, "sk-ant-rotated\n")
	if _, err := h.p.Complete(context.Background(), structuredRequest()); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != keyMark || keys[1] != "sk-ant-rotated" {
		t.Errorf("the second call did not send the new key (%d calls)", len(keys))
	}
}

func TestCompleteKeyFileErrors(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(path string)
		retryable bool
	}{
		{"missing", func(path string) { _ = os.Remove(path) }, true},
		{"empty", func(path string) { _ = os.WriteFile(path, []byte(" \n"), 0o600) }, true},
		{"not header safe", func(path string) { _ = os.WriteFile(path, []byte("sk ant\n"), 0o600) }, false},
		{"too long", func(path string) { _ = os.WriteFile(path, []byte(strings.Repeat("k", maxKeyBytes+1)), 0o600) }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				reply(w, messageJSON(testModel, stopEndTurn, `[{"type":"text","text":"{}"}]`))
			})
			tt.setup(h.keyFile)
			_, err := h.p.Complete(context.Background(), structuredRequest())
			if tt.retryable && !providers.IsRetryable(err) || !tt.retryable && !providers.IsForbidden(err) {
				t.Fatalf("err = %v, want retryable %v (else forbidden)", err, tt.retryable)
			}
			if h.requests.Load() != 0 {
				t.Errorf("sent %d requests without a key, want 0", h.requests.Load())
			}
			if strings.Contains(err.Error(), h.keyFile) || strings.Contains(err.Error(), filepath.Base(h.keyFile)) {
				t.Errorf("the error names the key file: %v", err)
			}
		})
	}
}

func TestNoSecretInErrorsReportsOrLogs(t *testing.T) {
	var logged strings.Builder
	ctx := logr.NewContext(context.Background(), funcr.New(func(prefix, args string) {
		logged.WriteString(prefix + args + "\n")
	}, funcr.Options{Verbosity: 10}))
	paths := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"throttled, failing, then cut off", func() http.HandlerFunc {
			var n atomic.Int32
			return func(w http.ResponseWriter, r *http.Request) {
				switch n.Add(1) {
				case 1:
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, bodyMark)
				case 2:
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, bodyMark+promptMark)
				default:
					reply(w, messageJSON(testModel, "max_tokens", `[{"type":"text","text":"`+replyMark+`"}]`))
				}
			}
		}()},
		{"unauthorized", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid x-api-key `+keyMark+`"}}`)
		}},
		{"another model", func(w http.ResponseWriter, r *http.Request) {
			reply(w, messageJSON("claude-opus-4-8", stopEndTurn, `[{"type":"text","text":"`+replyMark+`"}]`))
		}},
		{"not JSON", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, replyMark+keyMark)
		}},
	}
	for _, tt := range paths {
		t.Run(tt.name, func(t *testing.T) {
			logged.Reset()
			h := newHarness(t, tt.handler)
			_, err := h.p.Complete(ctx, structuredRequest())
			if err == nil {
				t.Fatal("no error")
			}
			r := h.lastReport(t)
			dump := fmt.Sprintf("%v %+v %#v %+v", err, err, err, r) + logged.String()
			for _, s := range []string{promptMark, replyMark, bodyMark, keyMark, h.keyFile} {
				if strings.Contains(dump, s) {
					t.Errorf("%q leaked: %s", s, dump)
				}
			}
			if logged.Len() != 0 {
				t.Errorf("the adapter logged: %s", logged.String())
			}
		})
	}
	t.Run("key file errors", func(t *testing.T) {
		h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
		writeKey(t, h.keyFile, keyMark+" "+keyMark)
		_, err := h.p.Complete(ctx, structuredRequest())
		if err == nil || strings.Contains(fmt.Sprintf("%v %#v", err, err), keyMark) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestCompleteRefusesInvalidRequests(t *testing.T) {
	user := providers.Message{Role: providers.RoleUser, Content: "context"}
	tests := map[string]providers.CompletionRequest{
		"no messages":       {},
		"only system":       {Messages: []providers.Message{{Role: providers.RoleSystem, Content: "x"}}},
		"late system":       {Messages: []providers.Message{user, {Role: providers.RoleSystem, Content: "x"}}},
		"empty user":        {Messages: []providers.Message{{Role: providers.RoleUser}}},
		"unknown role":      {Messages: []providers.Message{{Role: "developer", Content: "x"}}},
		"bad tool name":     {Messages: []providers.Message{user}, Tools: []providers.ToolDefinition{{Name: "a b", Parameters: json.RawMessage(`{}`)}}},
		"schema not object": {Messages: []providers.Message{user}, ResponseSchema: json.RawMessage(`[]`)},
		"output limit":      {Messages: []providers.Message{user}, MaxOutputTokens: MaxOutputTokens + 1},
		"tool message ID":   {Messages: []providers.Message{user, {Role: providers.RoleTool, ToolCallID: "a b"}}},
	}
	for name, req := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
			if _, err := h.p.Complete(context.Background(), req); !errors.Is(err, providers.ErrRejected) {
				t.Fatalf("err = %v, want rejected", err)
			}
			if h.requests.Load() != 0 {
				t.Errorf("sent %d requests", h.requests.Load())
			}
		})
	}
}

func TestNewRefusesInvalidConfig(t *testing.T) {
	tests := map[string]Config{
		"no model":      {KeyFile: "k"},
		"bad model":     {Model: "Claude Opus", KeyFile: "k"},
		"no key file":   {Model: testModel},
		"output limit":  {Model: testModel, KeyFile: "k", MaxOutputTokens: MaxOutputTokens + 1},
		"attempts":      {Model: testModel, KeyFile: "k", MaxAttempts: MaxAttempts + 1},
		"negative":      {Model: testModel, KeyFile: "k", Timeout: -1},
		"timeout bound": {Model: testModel, KeyFile: "k", Timeout: maxConfigTimeout + 1},
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := New(cfg); !providers.IsInvalidConfig(err) {
				t.Fatalf("err = %v, want invalid config", err)
			}
		})
	}
	if _, err := New(Config{Model: testModel, KeyFile: "k"}); err != nil {
		t.Fatalf("New: %v", err)
	}
}
