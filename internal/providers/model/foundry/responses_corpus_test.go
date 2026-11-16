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
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

// The corpus tests of the Responses API wire format (#501). Each generated case is built with its right answer
// known by construction, and is written in both formats: the chat completions parser, unchanged from main, is
// the reference, so the Responses parser must accept, refuse and return exactly what the chat parser does for
// the same reply, and the Responses request must carry exactly what the chat request carries.

// corpusTools are the tools the generated requests offer.
var corpusTools = []string{"get_logs", "get_events", "describe_object"}

// genReply is one reply known by construction.
type genReply struct {
	// end is how the reply ended: endStop, endToolCalls, "length", endFiltered or "other".
	end      string
	content  *string
	refusal  string
	calls    []providers.ToolCall
	usage    providers.Usage
	model    string
	extraMsg bool // the Responses form splits the content over two output_text parts
}

// texts are contents that must survive the parse byte for byte: markers, escapes, Unicode, JSON in JSON.
var texts = []string{
	`{"cause":"oom","next":"raise the limit"}`,
	`{"cause":"line one\nline two \"quoted\" \\ back","marker":"` + replyMark + `"}`,
	`{"cause":"zażółć 漢字 🙂","x":[1,2,{"y":null}]}`,
	`  {"padded":true}  `,
	"plain text " + replyMark,
	"",
}

func pick[T any](r *rand.Rand, s []T) T { return s[r.IntN(len(s))] }

func genCall(r *rand.Rand, i int) providers.ToolCall {
	args := pick(r, []string{`{}`, webArgs, `{"namespace":"shop","name":"api","tail":200}`,
		`{"q":"a \"quoted\" ` + replyMark + `"}`})
	return providers.ToolCall{ID: fmt.Sprintf("call_%d_%d", r.IntN(1000), i), Name: pick(r, corpusTools),
		Arguments: json.RawMessage(args)}
}

func genReplyCase(r *rand.Rand) genReply {
	in := int64(r.IntN(100000))
	g := genReply{
		end:   pick(r, []string{endStop, endStop, endToolCalls, endToolCalls, "length", endFiltered, "other"}),
		usage: providers.Usage{InputTokens: in, CachedInputTokens: in / int64(1+r.IntN(4)), OutputTokens: int64(r.IntN(9000))},
		model: pick(r, []string{replyModel, "gpt-6.1-sol-2026-09-30", "gpt-5.4"}),
	}
	if r.IntN(4) > 0 {
		t := pick(r, texts)
		g.content = &t
		g.extraMsg = len(t) > 2 && r.IntN(2) == 0
	}
	if r.IntN(10) == 0 {
		g.refusal = "I cannot " + replyMark
	}
	if g.end == endToolCalls || r.IntN(5) == 0 {
		for i := range r.IntN(4) {
			g.calls = append(g.calls, genCall(r, i))
		}
	}
	return g
}

// chatJSON writes the reply as a chat completion.
func (g genReply) chatJSON() string {
	msg := map[string]any{"role": "assistant", "content": g.content}
	if g.refusal != "" {
		msg[partRefusal] = g.refusal
	}
	calls := make([]map[string]any, 0, len(g.calls))
	for _, c := range g.calls {
		calls = append(calls, map[string]any{"id": c.ID, keyType: "function",
			"function": map[string]any{"name": c.Name, "arguments": string(c.Arguments)}})
	}
	if len(calls) > 0 {
		msg[endToolCalls] = calls
	}
	b, _ := json.Marshal(map[string]any{"id": "chatcmpl-1", "object": "chat.completion", "model": g.model,
		"choices": []any{map[string]any{"index": 0, "finish_reason": g.end, itemMessage: msg}},
		"usage": map[string]any{"prompt_tokens": g.usage.InputTokens, "completion_tokens": g.usage.OutputTokens,
			"prompt_tokens_details": map[string]any{"cached_tokens": g.usage.CachedInputTokens}}})
	return string(b)
}

// responsesJSON writes the same reply as a Responses API response, with a reasoning item first.
func (g genReply) responsesJSON() string {
	status, details := statusCompleted, any(nil)
	switch g.end {
	case "length":
		status, details = statusIncomplete, map[string]any{keyReason: "max_output_tokens"}
	case endFiltered:
		status, details = statusIncomplete, map[string]any{keyReason: endFiltered}
	case "other":
		status, details = statusIncomplete, map[string]any{keyReason: "something_new"}
	}
	output := []any{map[string]any{keyType: "reasoning", "id": "rs_1", "summary": []any{}}}
	var parts []any
	if g.content != nil && *g.content != "" {
		if g.extraMsg {
			half := len(*g.content) / 2
			for !utf8Boundary(*g.content, half) {
				half--
			}
			parts = append(parts, map[string]any{keyType: partOutputText, keyText: (*g.content)[:half], keyAnnotations: []any{}},
				map[string]any{keyType: partOutputText, keyText: (*g.content)[half:], keyAnnotations: []any{}})
		} else {
			parts = append(parts, map[string]any{keyType: partOutputText, keyText: *g.content, keyAnnotations: []any{}})
		}
	}
	if g.refusal != "" {
		parts = append(parts, map[string]any{keyType: partRefusal, partRefusal: g.refusal})
	}
	if parts != nil {
		output = append(output, map[string]any{keyType: itemMessage, "id": "msg_1", "role": "assistant",
			keyStatus: statusCompleted, "content": parts})
	}
	for i, c := range g.calls {
		output = append(output, map[string]any{keyType: "function_call", "id": fmt.Sprintf("fc_%d", i),
			"call_id": c.ID, "name": c.Name, "arguments": string(c.Arguments), keyStatus: statusCompleted})
	}
	b, _ := json.Marshal(map[string]any{"id": "resp_1", "object": "response", keyStatus: status,
		"incomplete_details": details, "model": g.model, "output": output, "error": nil,
		"usage": map[string]any{"input_tokens": g.usage.InputTokens, "output_tokens": g.usage.OutputTokens,
			"input_tokens_details":  map[string]any{"cached_tokens": g.usage.CachedInputTokens},
			"output_tokens_details": map[string]any{"reasoning_tokens": 7}, "total_tokens": 1}})
	return string(b)
}

func utf8Boundary(s string, i int) bool { return i <= 0 || i >= len(s) || s[i]&0xC0 != 0x80 }

// want is the answer by construction: a success keeps the content and every call unchanged.
func (g genReply) want(structured bool) (providers.Completion, bool) {
	content := ""
	if g.content != nil {
		content = *g.content
	}
	ok := (g.end == endStop || g.end == endToolCalls) && g.refusal == "" &&
		(len(g.calls) > 0 || (content != "" && (!structured || isJSONObject([]byte(content)))))
	if !ok {
		return providers.Completion{Usage: g.usage, Model: g.model}, false
	}
	c := providers.Completion{Content: content, Usage: g.usage, Model: g.model}
	if len(g.calls) > 0 {
		c.ToolCalls = g.calls
	}
	return c, true
}

func offeredTools() map[string]bool {
	m := map[string]bool{}
	for _, t := range corpusTools {
		m[t] = true
	}
	return m
}

func TestResponsesCorpusParsesAsChat(t *testing.T) {
	r := rand.New(rand.NewPCG(501, 2026)) // #nosec G404 -- a seeded generator keeps the corpus reproducible
	offered := offeredTools()
	accepted := 0
	for i := range 3000 {
		g := genReplyCase(r)
		structured := r.IntN(2) == 0
		want, ok := g.want(structured)
		chat, chatErr := parseResponse([]byte(g.chatJSON()), offered, structured)
		resp, respErr := parseResponses([]byte(g.responsesJSON()), offered, structured)
		if (chatErr == nil) != ok || (respErr == nil) != ok {
			t.Fatalf("case %d (%+v, structured %v): chat err %v, responses err %v, want ok %v", i, g, structured,
				chatErr, respErr, ok)
		}
		if !ok && (!providers.IsMalformedOutput(chatErr) || !providers.IsMalformedOutput(respErr)) {
			t.Fatalf("case %d: errors of another class: %v, %v", i, chatErr, respErr)
		}
		if !reflect.DeepEqual(chat, want) || !reflect.DeepEqual(resp, want) {
			t.Fatalf("case %d (%+v, structured %v):\nchat      %+v\nresponses %+v\nwant      %+v", i, g, structured,
				chat, resp, want)
		}
		if ok {
			accepted++
		}
	}
	if accepted < 500 {
		t.Fatalf("only %d generated replies were valid: the corpus does not reach the success path", accepted)
	}
}

// genRequest builds a request of the analysis loop's shape: system, user, then tool rounds.
func genRequest(r *rand.Rand) providers.CompletionRequest {
	req := providers.CompletionRequest{
		Messages: []providers.Message{
			{Role: providers.RoleSystem, Content: "You triage incidents. " + pick(r, texts)},
			{Role: providers.RoleUser, Content: "context " + promptMark + " " + pick(r, texts)},
		},
		MaxOutputTokens: pick(r, []int{0, 1, 512, MaxOutputTokens}),
	}
	if r.IntN(2) == 0 {
		req.ResponseSchema = json.RawMessage(`{"type":"object","properties":{"cause":{"type":"string"}},"required":["cause"],"additionalProperties":false}`)
	}
	if r.IntN(3) > 0 {
		for _, name := range corpusTools[:1+r.IntN(len(corpusTools))] {
			req.Tools = append(req.Tools, providers.ToolDefinition{Name: name, Description: "Reads " + name,
				Parameters: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`)})
		}
		for round := range r.IntN(3) {
			n := 1 + r.IntN(3)
			calls := make([]providers.ToolCall, 0, n)
			for i := range n {
				calls = append(calls, genCall(r, round*10+i))
			}
			req.Messages = append(req.Messages, providers.Message{Role: providers.RoleAssistant,
				Content: pick(r, []string{"", "", "let me look " + replyMark}), ToolCalls: calls})
			for _, c := range calls {
				req.Messages = append(req.Messages, providers.Message{Role: providers.RoleTool, ToolCallID: c.ID,
					Content: pick(r, append([]string{"", "tool result " + promptMark}, texts...))})
			}
		}
	}
	return req
}

// requestView is what a request carries to the model, read back from either body.
type requestView struct {
	Messages  []providers.Message
	Tools     []providers.ToolDefinition
	Schema    string
	MaxTokens int
	Model     string
}

func chatView(t *testing.T, body []byte) requestView {
	t.Helper()
	var b chatRequest
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	v := requestView{MaxTokens: b.MaxCompletionTokens, Model: b.Model}
	for _, m := range b.Messages {
		pm := providers.Message{Role: providers.Role(m.Role), ToolCallID: m.ToolCallID}
		if m.Content != nil {
			pm.Content = *m.Content
		}
		for _, c := range m.ToolCalls {
			pm.ToolCalls = append(pm.ToolCalls, providers.ToolCall{ID: c.ID, Name: c.Function.Name,
				Arguments: json.RawMessage(c.Function.Arguments)})
		}
		v.Messages = append(v.Messages, pm)
	}
	for _, tl := range b.Tools {
		v.Tools = append(v.Tools, providers.ToolDefinition{Name: tl.Function.Name, Description: tl.Function.Description,
			Parameters: tl.Function.Parameters})
	}
	if b.ResponseFormat != nil {
		v.Schema = b.ResponseFormat.JSONSchema.Name + "|" + fmt.Sprint(b.ResponseFormat.JSONSchema.Strict) + "|" +
			string(b.ResponseFormat.JSONSchema.Schema)
	}
	return v
}

// responsesView reads a Responses body back. Consecutive function_call items after an assistant message (or
// at the start of a turn) belong to that assistant message, as the adapter writes them.
func responsesView(t *testing.T, body []byte) requestView {
	t.Helper()
	var b struct {
		Model           string `json:"model"`
		MaxOutputTokens int    `json:"max_output_tokens"`
		Store           *bool  `json:"store"`
		Input           []struct {
			Type      string  `json:"type"`
			Role      string  `json:"role"`
			Content   *string `json:"content"`
			CallID    string  `json:"call_id"`
			Name      string  `json:"name"`
			Arguments string  `json:"arguments"`
			Output    *string `json:"output"`
		} `json:"input"`
		Tools []struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
			Strict      *bool           `json:"strict"`
		} `json:"tools"`
		Text *struct {
			Format struct {
				Type   string          `json:"type"`
				Name   string          `json:"name"`
				Strict bool            `json:"strict"`
				Schema json.RawMessage `json:"schema"`
			} `json:"format"`
		} `json:"text"`
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		t.Fatalf("the Responses body has a field this test does not know: %v", err)
	}
	if b.Store == nil || *b.Store {
		t.Fatal("the Responses body does not say store: false")
	}
	v := requestView{MaxTokens: b.MaxOutputTokens, Model: b.Model}
	for _, it := range b.Input {
		switch it.Type {
		case itemMessage:
			if it.Content == nil {
				t.Fatal("a message item has no content")
			}
			v.Messages = append(v.Messages, providers.Message{Role: providers.Role(it.Role), Content: *it.Content})
		case "function_call":
			last := len(v.Messages) - 1
			if last < 0 || v.Messages[last].Role != providers.RoleAssistant {
				v.Messages = append(v.Messages, providers.Message{Role: providers.RoleAssistant})
				last++
			}
			v.Messages[last].ToolCalls = append(v.Messages[last].ToolCalls, providers.ToolCall{ID: it.CallID,
				Name: it.Name, Arguments: json.RawMessage(it.Arguments)})
		case "function_call_output":
			if it.Output == nil {
				t.Fatal("a function_call_output item has no output")
			}
			v.Messages = append(v.Messages, providers.Message{Role: providers.RoleTool, ToolCallID: it.CallID,
				Content: *it.Output})
		default:
			t.Fatalf("unknown input item type %q", it.Type)
		}
	}
	for _, tl := range b.Tools {
		if tl.Type != toolTypeFunction || tl.Strict == nil || *tl.Strict {
			t.Fatalf("tool %q: type %q, strict %v; want a function tool with strict false, as in chat", tl.Name,
				tl.Type, tl.Strict)
		}
		v.Tools = append(v.Tools, providers.ToolDefinition{Name: tl.Name, Description: tl.Description,
			Parameters: tl.Parameters})
	}
	if b.Text != nil {
		if b.Text.Format.Type != jsonSchemaType {
			t.Fatalf("text.format.type = %q", b.Text.Format.Type)
		}
		v.Schema = b.Text.Format.Name + "|" + fmt.Sprint(b.Text.Format.Strict) + "|" + string(b.Text.Format.Schema)
	}
	return v
}

func TestResponsesCorpusRequestCarriesWhatChatCarries(t *testing.T) {
	chatP, err := New(Config{Endpoint: azureEndpoint, Deployment: testDeployment, APIVersion: "v1", Tokens: &fakeTokens{}})
	if err != nil {
		t.Fatal(err)
	}
	respP, err := New(Config{Endpoint: azureEndpoint, Deployment: testDeployment, APIVersion: "v1", API: APIResponses,
		Tokens: &fakeTokens{}})
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(501, 11)) // #nosec G404 -- a seeded generator keeps the corpus reproducible
	for i := range 2000 {
		req := genRequest(r)
		chatBody, chatOffered, chatErr := chatP.encode(req)
		respBody, respOffered, respErr := respP.encode(req)
		if chatErr != nil || respErr != nil {
			t.Fatalf("case %d: chat %v, responses %v", i, chatErr, respErr)
		}
		if !reflect.DeepEqual(chatOffered, respOffered) {
			t.Fatalf("case %d: offered %v, %v", i, chatOffered, respOffered)
		}
		cv, rv := chatView(t, chatBody), responsesView(t, respBody)
		if !reflect.DeepEqual(cv, rv) {
			t.Fatalf("case %d: the Responses body carries another request:\nchat      %+v\nresponses %+v", i, cv, rv)
		}
		// Known by construction: every message and tool of the request, unchanged.
		want := requestView{Messages: req.Messages, Tools: req.Tools, Model: testDeployment,
			MaxTokens: req.MaxOutputTokens}
		if want.MaxTokens == 0 {
			want.MaxTokens = DefaultMaxOutputTokens
		}
		if len(req.ResponseSchema) > 0 {
			want.Schema = "response|true|" + string(req.ResponseSchema)
		}
		norm := func(v requestView) requestView {
			for j := range v.Messages {
				if len(v.Messages[j].ToolCalls) == 0 {
					v.Messages[j].ToolCalls = nil
				}
			}
			return v
		}
		if !reflect.DeepEqual(norm(rv), norm(want)) {
			t.Fatalf("case %d: the Responses body lost or changed part of the request:\ngot  %+v\nwant %+v", i, rv, want)
		}
	}
}

// Every request the chat encoder refuses, the Responses encoder refuses with the same class.
func TestResponsesCorpusRefusesWhatChatRefuses(t *testing.T) {
	chatP, _ := New(Config{Endpoint: azureEndpoint, Deployment: testDeployment, APIVersion: "v1", Tokens: &fakeTokens{}})
	respP, _ := New(Config{Endpoint: azureEndpoint, Deployment: testDeployment, APIVersion: "v1", API: APIResponses,
		Tokens: &fakeTokens{}})
	r := rand.New(rand.NewPCG(501, 12)) // #nosec G404 -- a seeded generator keeps the corpus reproducible
	breaks := []func(*providers.CompletionRequest){
		func(q *providers.CompletionRequest) { q.Messages = nil },
		func(q *providers.CompletionRequest) { q.MaxOutputTokens = MaxOutputTokens + 1 },
		func(q *providers.CompletionRequest) { q.MaxOutputTokens = -1 },
		func(q *providers.CompletionRequest) {
			q.Tools = append(q.Tools, providers.ToolDefinition{Name: "bad name", Parameters: json.RawMessage(`{}`)})
		},
		func(q *providers.CompletionRequest) {
			q.Tools = append(q.Tools, providers.ToolDefinition{Name: "x", Parameters: json.RawMessage(`[]`)})
		},
		func(q *providers.CompletionRequest) { q.ResponseSchema = json.RawMessage(`"no"`) },
		func(q *providers.CompletionRequest) { q.Messages[0].ToolCallID = callZero },
		func(q *providers.CompletionRequest) { q.Messages[1].Role = "developer" },
		func(q *providers.CompletionRequest) {
			q.Messages = append(q.Messages, providers.Message{Role: providers.RoleTool, ToolCallID: "bad id"})
		},
		func(q *providers.CompletionRequest) {
			q.Messages = append(q.Messages, providers.Message{Role: providers.RoleAssistant,
				ToolCalls: []providers.ToolCall{{ID: callZero, Name: logsTool, Arguments: json.RawMessage(`[1]`)}}})
		},
		func(q *providers.CompletionRequest) {
			q.Messages = append(q.Messages, providers.Message{Role: providers.RoleUser, Content: strings.Repeat("x", maxRequestBytes)})
		},
	}
	for i := range 500 {
		req := genRequest(r)
		b := r.IntN(len(breaks))
		breaks[b](&req)
		_, _, chatErr := chatP.encode(req)
		_, _, respErr := respP.encode(req)
		if chatErr == nil || respErr == nil {
			t.Fatalf("case %d (break %d): chat %v, responses %v; want both refused", i, b, chatErr, respErr)
		}
		if providers.IsRejected(chatErr) != providers.IsRejected(respErr) ||
			providers.IsTooLarge(chatErr) != providers.IsTooLarge(respErr) {
			t.Fatalf("case %d (break %d): classes differ: %v, %v", i, b, chatErr, respErr)
		}
		if strings.Contains(respErr.Error(), promptMark) {
			t.Fatalf("case %d: the error quotes the request", i)
		}
	}
}
