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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

func useResponses(c *Config) { c.API = APIResponses }

// Literals of the Responses tests, named so that they are not repeated.
const (
	oomCause             = `{"cause":"oom"}`
	webArgs              = `{"pod":"web-0"}`
	jsonSchemaType       = "json_schema"
	caseNotJSON          = "not JSON"
	caseWrongContentType = "wrong content type"
	contentTypeJSON      = "application/json"
	servicesEndpoint     = "https://r.services.ai.azure.com/"
	v1Endpoint           = "https://r.openai.azure.com/openai/v1"
	v1EndpointSlash      = v1Endpoint + "/"
	plainHTTPEndpoint    = "http://r.openai.azure.com/"
	ipEndpoint           = "https://20.1.2.3/"
	endFiltered          = "content_filter"
	statusCompleted      = "completed"
	statusIncomplete     = "incomplete"
	keyAnnotations       = "annotations"
	keyStatus            = "status"
	keyType              = "type"
	keyReason            = "reason"
	endStop              = "stop"
	endToolCalls         = "tool_calls"
	keyText              = "text"
)

// responseJSON is a completed Responses API reply with the given output items, after a reasoning item.
func responseJSON(output string) string {
	return responsesWith(statusCompleted, "null", output)
}

func responsesWith(status, details, output string) string {
	if output != "" {
		output = "," + output
	}
	return `{"id":"resp_1","object":"response","status":"` + status + `","incomplete_details":` + details +
		`,"model":"` + replyModel + `","output":[{"type":"reasoning","id":"rs_1","summary":[]}` + output + `],` +
		`"usage":{"input_tokens":120,"output_tokens":30,"total_tokens":150,"input_tokens_details":{"cached_tokens":64},` +
		`"output_tokens_details":{"reasoning_tokens":20}}}`
}

func textItem(text string) string {
	b, _ := json.Marshal(text)
	return `{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":` +
		string(b) + `,"annotations":[]}]}`
}

func callItem(callID, name, args string) string {
	b, _ := json.Marshal(args)
	return `{"type":"function_call","id":"fc_1","call_id":"` + callID + `","name":"` + name + `","arguments":` +
		string(b) + `,"status":"completed"}`
}

func TestResponsesStructuredSuccess(t *testing.T) {
	var got map[string]any
	var path, query string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&got)
		reply(w, responseJSON(textItem(oomCause)))
	}, useResponses)
	c, err := h.p.Complete(context.Background(), structuredRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if c.Content != oomCause || c.Usage != wantUsage || c.Model != replyModel || c.ToolCalls != nil {
		t.Errorf("completion = %+v", c)
	}
	if path != "/openai/v1/responses" || query != "" {
		t.Errorf("path %q query %q", path, query)
	}
	if got["model"] != testDeployment || got["max_output_tokens"] != float64(DefaultMaxOutputTokens) ||
		got["store"] != false {
		t.Errorf("model %v, max_output_tokens %v, store %v", got["model"], got["max_output_tokens"], got["store"])
	}
	for _, k := range []string{"messages", "response_format", "max_completion_tokens", "reasoning", "previous_response_id"} {
		if _, ok := got[k]; ok {
			t.Errorf("the body has %q", k)
		}
	}
	text, _ := got["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	if format[keyType] != jsonSchemaType || format["name"] != "response" || format["strict"] != true || format["schema"] == nil {
		t.Errorf("text = %v", got["text"])
	}
	if in, _ := got["input"].([]any); len(in) != 2 {
		t.Errorf("input = %v", got["input"])
	}
	if r := h.lastReport(t); r.Model != replyModel || r.Usage != wantUsage || r.Attempts != 1 || r.Err != nil {
		t.Errorf("report = %+v", r)
	}
}

// The escalation request of #501: read tools, a tool round, and the reply's function calls come back as tool
// calls.
func TestResponsesToolCalls(t *testing.T) {
	var got map[string]any
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		reply(w, responseJSON(callItem("call_1", logsTool, webArgs)))
	}, useResponses)
	req := providers.CompletionRequest{
		Messages: []providers.Message{
			{Role: providers.RoleSystem, Content: "You investigate."},
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
		string(c.ToolCalls[0].Arguments) != webArgs || c.Content != "" {
		t.Errorf("completion = %+v", c)
	}
	in, _ := got["input"].([]any)
	want := []string{
		`{"content":"You investigate.","role":"system","type":"message"}`,
		`{"content":"why","role":"user","type":"message"}`,
		`{"arguments":"{\"pod\":\"a\"}","call_id":"call_0","name":"get_logs","type":"function_call"}`,
		`{"call_id":"call_0","output":"log lines","type":"function_call_output"}`,
	}
	if len(in) != len(want) {
		t.Fatalf("input = %v", in)
	}
	for i, item := range in {
		b, _ := json.Marshal(item)
		if string(b) != want[i] {
			t.Errorf("input[%d] = %s, want %s", i, b, want[i])
		}
	}
	tools, _ := got["tools"].([]any)
	if b, _ := json.Marshal(tools); string(b) != `[{"description":"Reads logs","name":"get_logs","parameters":{"type":"object"},"strict":false,"type":"function"}]` {
		t.Errorf("tools = %s", b)
	}
	if _, ok := got["text"]; ok {
		t.Error("a request without a schema sent text.format")
	}
}

func TestResponsesMalformedOutput(t *testing.T) {
	toolsReq := structuredRequest()
	toolsReq.Tools = []providers.ToolDefinition{{Name: logsTool, Parameters: json.RawMessage(`{"type":"object"}`)}}
	many := make([]string, maxToolCalls+1)
	for i := range many {
		many[i] = callItem(fmt.Sprintf("c%d", i), logsTool, `{}`)
	}
	reasoning := make([]string, maxOutputItems)
	for i := range reasoning {
		reasoning[i] = `{"type":"reasoning","summary":[]}`
	}
	okText := textItem(`{"cause":"x"}`)
	for _, tc := range []struct {
		name, body  string
		contentType string
		withUsage   bool
		unknown     bool
	}{
		{name: caseNotJSON, body: "<html>" + replyMark, unknown: true},
		{name: "a chat completion", body: completionJSON("stop", `{"content":"{}"}`), unknown: true},
		{name: caseWrongContentType, body: responseJSON(okText), contentType: "text/plain", withUsage: true},
		{name: "trailing data", body: responseJSON(okText) + `{}`, unknown: true},
		{name: "no usage", body: `{"model":"m","status":"completed","output":[]}`, unknown: true},
		{name: "usage without counts", body: `{"model":"m","status":"completed","output":[],"usage":{"total_tokens":9}}`, unknown: true},
		{name: "null usage", body: `{"model":"m","status":"completed","output":[],"usage":null}`, unknown: true},
		{name: "cached above input", body: `{"model":"m","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"input_tokens_details":{"cached_tokens":2}}}`, unknown: true},
		{name: "no model", body: `{"status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`, withUsage: true},
		{name: "failed", body: responsesWith("failed", "null", okText), withUsage: true},
		{name: "in progress", body: responsesWith("in_progress", "null", okText), withUsage: true},
		{name: "no status", body: responsesWith("", "null", okText), withUsage: true},
		{name: "cut off", body: responsesWith(statusIncomplete, `{"reason":"max_output_tokens"}`, textItem(`{"cause":"`+replyMark)), withUsage: true},
		{name: "filtered", body: responsesWith(statusIncomplete, `{"reason":"content_filter"}`, ""), withUsage: true},
		{name: "incomplete, no reason", body: responsesWith(statusIncomplete, "null", okText), withUsage: true},
		{name: "no output", body: responseJSON(""), withUsage: true},
		{name: partRefusal, body: responseJSON(`{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"` + replyMark + `"}]}`), withUsage: true},
		{name: "unknown part", body: responseJSON(`{"type":"message","role":"assistant","content":[{"type":"output_audio"}]}`), withUsage: true},
		{name: "not the assistant", body: responseJSON(`{"type":"message","role":"user","content":[{"type":"output_text","text":"{}"}]}`), withUsage: true},
		{name: "unknown item", body: responseJSON(`{"type":"web_search_call","id":"ws_1","status":"completed"}`), withUsage: true},
		{name: "built-in tool call", body: responseJSON(`{"type":"code_interpreter_call","id":"ci_1"}`), withUsage: true},
		{name: "not an object", body: responseJSON(textItem(replyMark)), withUsage: true},
		{name: "tool not offered", body: responseJSON(callItem("c1", "delete_pod", `{}`)), withUsage: true},
		{name: "tool id invalid", body: responseJSON(callItem("c 1", logsTool, `{}`)), withUsage: true},
		{name: "tool id missing", body: responseJSON(callItem("", logsTool, `{}`)), withUsage: true},
		{name: "tool id repeated", body: responseJSON(callItem("c1", logsTool, `{}`) + "," + callItem("c1", logsTool, `{}`)), withUsage: true},
		{name: "tool args not object", body: responseJSON(callItem("c1", logsTool, `[]`)), withUsage: true},
		{name: "tool args not JSON", body: responseJSON(callItem("c1", logsTool, `{`+replyMark)), withUsage: true},
		{name: "too many tool calls", body: responseJSON(strings.Join(many, ",")), withUsage: true},
		{name: "too many items", body: responseJSON(strings.Join(reasoning, ",")), withUsage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				ct := tc.contentType
				if ct == "" {
					ct = contentTypeJSON
				}
				w.Header().Set("Content-Type", ct)
				_, _ = io.WriteString(w, tc.body)
			}, useResponses)
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
			if tc.withUsage != (c.Usage != providers.Usage{}) || c.UsageUnknown != tc.unknown {
				t.Errorf("usage = %+v, unknown %v; want usage %v, unknown %v", c.Usage, c.UsageUnknown, tc.withUsage, tc.unknown)
			}
			if tc.name == caseWrongContentType && (c.Usage != wantUsage || c.Model != replyModel) {
				t.Errorf("billed reply lost its usage or model: %+v", c)
			}
			if r := h.lastReport(t); r.Usage != c.Usage || r.UsageUnknown != c.UsageUnknown || r.Err == nil {
				t.Errorf("report = %+v", r)
			}
			if strings.Contains(err.Error(), replyMark) {
				t.Error("error quotes the reply")
			}
		})
	}
}

// Text split over several output_text parts and messages is joined in order; reasoning between them is
// skipped.
func TestParseResponsesJoinsTextInOrder(t *testing.T) {
	body := responseJSON(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"a\":"},` +
		`{"type":"output_text","text":"1,"}]},{"type":"reasoning","summary":[]},` + textItem(`"b":2}`))
	c, err := parseResponses([]byte(body), nil, true)
	if err != nil || c.Content != `{"a":1,"b":2}` {
		t.Fatalf("c = %+v, err = %v", c, err)
	}
}

// Retries, throttling and statuses are the chat path's: one test shows they apply to the Responses API too.
func TestResponsesRetriesThenSucceeds(t *testing.T) {
	n := 0
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if n++; n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		reply(w, responseJSON(textItem(oomCause)))
	}, func(c *Config) { useResponses(c); c.Endpoint = v1EndpointSlash })
	c, err := h.p.Complete(context.Background(), structuredRequest())
	if err != nil || c.Content != oomCause || h.requests.Load() != 2 {
		t.Fatalf("c = %+v, err = %v, requests %d", c, err, h.requests.Load())
	}
}

// A billed reply that is not parsed keeps the usage its Responses usage block reports.
func TestBilledResponsesUsage(t *testing.T) {
	if c := billedResponsesUsage([]byte(responseJSON(""))); c.Usage != wantUsage || c.Model != replyModel || c.UsageUnknown {
		t.Errorf("c = %+v", c)
	}
	for _, body := range []string{"", "{", `{"model":"m"}`, completionJSON("stop", `{"content":"{}"}`)} {
		if c := billedResponsesUsage([]byte(body)); !c.UsageUnknown {
			t.Errorf("%q: c = %+v, want UsageUnknown", body, c)
		}
	}
	if c := billedResponsesUsage(nil); !c.UsageUnknown {
		t.Errorf("nil: c = %+v", c)
	}
}

func TestResponsesURL(t *testing.T) {
	for _, tc := range []struct{ endpoint, version, want string }{
		{servicesEndpoint, "v1", "https://r.services.ai.azure.com/openai/v1/responses"},
		{"https://r.services.ai.azure.com", "v1", "https://r.services.ai.azure.com/openai/v1/responses"},
		{v1Endpoint, "v1", "https://r.openai.azure.com/openai/v1/responses"},
		{v1EndpointSlash, "v1", "https://r.openai.azure.com/openai/v1/responses"},
		{azureEndpoint, datedVersion, ""},
		{azureEndpoint, datedVersion + "-preview", ""},
		{plainHTTPEndpoint, "v1", ""},
		{"https://r.openai.azure.com/?x=1", "v1", ""},
		{ipEndpoint, "v1", ""},
		{"https://r.openai.azure.com./", "v1", ""},
	} {
		got, err := responsesURL(tc.endpoint, tc.version)
		if (err != nil) != (tc.want == "") || got != tc.want {
			t.Errorf("responsesURL(%q, %q) = %q, %v; want %q", tc.endpoint, tc.version, got, err, tc.want)
		}
	}
}

func TestNewRefusesInvalidAPI(t *testing.T) {
	base := Config{Endpoint: azureEndpoint, Deployment: testDeployment, APIVersion: "v1", Tokens: &fakeTokens{}}
	for _, api := range []API{"", APIChatCompletions, APIResponses} {
		c := base
		c.API = api
		if _, err := New(c); err != nil {
			t.Errorf("API %q refused: %v", api, err)
		}
	}
	for name, mod := range map[string]func(*Config){
		"unknown API":            func(c *Config) { c.API = "Completions" },
		"lower case":             func(c *Config) { c.API = "responses" },
		"responses, dated":       func(c *Config) { c.API = APIResponses; c.APIVersion = datedVersion },
		"responses, plain http":  func(c *Config) { c.API = APIResponses; c.Endpoint = plainHTTPEndpoint },
		"responses, IP endpoint": func(c *Config) { c.API = APIResponses; c.Endpoint = ipEndpoint },
	} {
		c := base
		mod(&c)
		if _, err := New(c); !providers.IsInvalidConfig(err) {
			t.Errorf("%s: err = %v, want invalid config", name, err)
		}
	}
}

// FuzzParseResponses feeds arbitrary bodies to the Responses reply parser, with the properties of
// FuzzParseResponse.
func FuzzParseResponses(f *testing.F) {
	f.Add([]byte(responseJSON(textItem(`{"a":1}`))), true)
	f.Add([]byte(responseJSON(callItem("c1", "t", `{}`))), false)
	f.Add([]byte(responsesWith(statusIncomplete, `{"reason":"max_output_tokens"}`, "")), false)
	f.Add([]byte(`{"model":"m","status":"completed","output":[],"usage":{}}`), false)
	f.Add([]byte(`not json`), true)
	f.Fuzz(func(t *testing.T, data []byte, structured bool) {
		c, err := parseResponses(data, map[string]bool{"t": true}, structured)
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
		if len(c.ToolCalls) > maxToolCalls {
			t.Fatal("too many tool calls")
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
