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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/pkalin/shadewright/internal/providers"
)

// Bounds of a reply. A reply outside them is malformed.
const (
	// maxToolCalls is the most tool calls one reply may hold: maxToolSteps is at most 16.
	maxToolCalls = 16
	// maxModelName bounds the model name a reply reports.
	maxModelName = 128
	// maxUsageTokens bounds each usage count, so their sum cannot overflow.
	maxUsageTokens = 1 << 40
)

// Content block types.
const (
	blockText       = "text"
	blockToolUse    = "tool_use"
	blockToolResult = "tool_result"
)

// Stop reasons of a reply the adapter accepts.
const (
	stopEndTurn = "end_turn"
	stopToolUse = "tool_use"
)

var (
	// toolNamePattern is the API's rule for client tool names.
	toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	// toolCallIDPattern bounds the IDs a reply gives its tool calls; they are sent back in tool_result blocks.
	toolCallIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	// modelNamePattern is what a reported model may look like.
	modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`)
)

// The request body of the Messages API.
type (
	messagesRequest struct {
		Model        string        `json:"model"`
		MaxTokens    int           `json:"max_tokens"`
		System       []block       `json:"system,omitempty"`
		Messages     []message     `json:"messages"`
		Tools        []tool        `json:"tools,omitempty"`
		OutputConfig *outputConfig `json:"output_config,omitempty"`
	}
	message struct {
		Role    string  `json:"role"`
		Content []block `json:"content"`
	}
	// block is a text, tool_use or tool_result block; only the fields of its type are set.
	block struct {
		Type         string          `json:"type"`
		Text         string          `json:"text,omitempty"`
		ID           string          `json:"id,omitempty"`
		Name         string          `json:"name,omitempty"`
		Input        json.RawMessage `json:"input,omitempty"`
		ToolUseID    string          `json:"tool_use_id,omitempty"`
		Content      string          `json:"content,omitempty"`
		CacheControl *cacheControl   `json:"cache_control,omitempty"`
	}
	cacheControl struct {
		Type string `json:"type"`
	}
	tool struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	outputConfig struct {
		Format outputFormat `json:"format"`
	}
	outputFormat struct {
		Type   string          `json:"type"`
		Schema json.RawMessage `json:"schema"`
	}
)

// The reply of the Messages API, only the fields the adapter reads.
type (
	messagesResponse struct {
		Type       string       `json:"type"`
		Role       string       `json:"role"`
		Model      string       `json:"model"`
		Content    []replyBlock `json:"content"`
		StopReason string       `json:"stop_reason"`
		Usage      *apiUsage    `json:"usage"`
	}
	replyBlock struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	apiUsage struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	}
)

// encode checks a request and builds its body. It returns the names of the tools offered, against which the
// reply's tool calls are checked. Errors are fixed text matching providers.ErrRejected (or ErrTooLarge), and
// never quote the request.
func (p *Provider) encode(req providers.CompletionRequest) ([]byte, map[string]bool, error) {
	rejected := func(msg string) error { return fmt.Errorf("anthropic: %s: %w", msg, providers.ErrRejected) }
	if len(req.Messages) == 0 {
		return nil, nil, rejected("the request has no messages")
	}
	maxOut := req.MaxOutputTokens
	if maxOut == 0 {
		maxOut = p.maxOutput
	}
	if maxOut < 1 || maxOut > MaxOutputTokens {
		return nil, nil, rejected("the output token limit is out of range")
	}
	body := messagesRequest{Model: p.model, MaxTokens: maxOut}
	offered := map[string]bool{}
	for _, t := range req.Tools {
		if !toolNamePattern.MatchString(t.Name) || offered[t.Name] {
			return nil, nil, rejected("a tool name is invalid or repeated")
		}
		if !isJSONObject(t.Parameters) {
			return nil, nil, rejected("the parameters of a tool are not a JSON schema object")
		}
		offered[t.Name] = true
		body.Tools = append(body.Tools, tool{Name: t.Name, Description: t.Description, InputSchema: t.Parameters})
	}
	if len(req.ResponseSchema) > 0 {
		schema, err := outputSchema(req.ResponseSchema)
		if err != nil {
			return nil, nil, rejected("the response schema is not a JSON object")
		}
		body.OutputConfig = &outputConfig{Format: outputFormat{Type: "json_schema", Schema: schema}}
	}
	system, msgs, err := encodeMessages(req.Messages)
	if err != nil {
		return nil, nil, rejected(err.Error())
	}
	body.System, body.Messages = system, msgs
	data, err := json.Marshal(body)
	if err != nil {
		return nil, nil, rejected("the request could not be encoded")
	}
	if len(data) > maxRequestBytes {
		return nil, nil, fmt.Errorf("anthropic: the request is larger than %d bytes: %w", maxRequestBytes,
			providers.ErrTooLarge)
	}
	return data, offered, nil
}

// encodeMessages maps the conversation. The leading system messages become the system prompt, its last block
// marked for caching: tools and system render first, so the breakpoint caches the stable prefix. User and tool
// messages become user turns and assistant messages assistant turns; consecutive messages of one turn are
// merged, so the tool results of one assistant turn go back in one user turn. Error text is fixed.
func encodeMessages(in []providers.Message) ([]block, []message, error) {
	var system []block
	i := 0
	for ; i < len(in) && in[i].Role == providers.RoleSystem; i++ {
		m := in[i]
		if m.Content == "" || len(m.ToolCalls) > 0 || m.ToolCallID != "" {
			return nil, nil, errors.New("a system message is empty or carries tool calls")
		}
		system = append(system, block{Type: blockText, Text: m.Content})
	}
	if len(system) > 0 {
		system[len(system)-1].CacheControl = &cacheControl{Type: "ephemeral"}
	}
	if i == len(in) {
		return nil, nil, errors.New("the request has only system messages")
	}
	var out []message
	for _, m := range in[i:] {
		role, blocks, err := encodeMessage(m)
		if err != nil {
			return nil, nil, err
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			continue
		}
		out = append(out, message{Role: role, Content: blocks})
	}
	return system, out, nil
}

// encodeMessage maps one message after the system prompt to its turn's role and blocks. Error text is fixed.
func encodeMessage(m providers.Message) (string, []block, error) {
	switch m.Role {
	case providers.RoleSystem:
		return "", nil, errors.New("a system message follows the start of the conversation")
	case providers.RoleUser:
		if len(m.ToolCalls) > 0 || m.ToolCallID != "" {
			return "", nil, errors.New("only assistant and tool messages carry tool calls")
		}
		if m.Content == "" {
			return "", nil, errors.New("a user message is empty")
		}
		return "user", []block{{Type: blockText, Text: m.Content}}, nil
	case providers.RoleAssistant:
		if m.ToolCallID != "" {
			return "", nil, errors.New("an assistant message has a tool call ID")
		}
		var blocks []block
		if m.Content != "" {
			blocks = append(blocks, block{Type: blockText, Text: m.Content})
		}
		for _, tc := range m.ToolCalls {
			if !toolCallIDPattern.MatchString(tc.ID) || !toolNamePattern.MatchString(tc.Name) ||
				!isJSONObject(tc.Arguments) {
				return "", nil, errors.New("a tool call of an assistant message is invalid")
			}
			blocks = append(blocks, block{Type: blockToolUse, ID: tc.ID, Name: tc.Name,
				Input: json.RawMessage(bytes.TrimSpace(tc.Arguments))})
		}
		if len(blocks) == 0 {
			return "", nil, errors.New("an assistant message has no content and no tool call")
		}
		return "assistant", blocks, nil
	case providers.RoleTool:
		if !toolCallIDPattern.MatchString(m.ToolCallID) || len(m.ToolCalls) > 0 {
			return "", nil, errors.New("a tool message needs a valid tool call ID and no tool calls")
		}
		return "user", []block{{Type: blockToolResult, ToolUseID: m.ToolCallID, Content: m.Content}}, nil
	}
	return "", nil, errors.New("a message has an unknown role")
}

// unsupportedBounds are the JSON Schema keywords the API refuses in a structured output schema (a 400). They
// are removed and stated in the description instead, as the vendor's SDKs do; the caller validates the reply
// against its full schema.
var unsupportedBounds = map[string]bool{
	"minimum": true, "maximum": true, "exclusiveMinimum": true, "exclusiveMaximum": true, "multipleOf": true,
	"minLength": true, "maxLength": true, "maxItems": true, "uniqueItems": true,
	"minProperties": true, "maxProperties": true,
}

// outputSchema rewrites a response schema into the subset the API accepts. It walks schema positions only
// (properties, items, anyOf, allOf, oneOf, $defs, definitions, not), so a property named like a keyword is
// kept. The output is deterministic (sorted keys), which keeps the cached prefix and the API's compiled
// grammar stable across calls.
func outputSchema(raw json.RawMessage) (json.RawMessage, error) {
	if !isJSONObject(raw) {
		return nil, errors.New("not a JSON object")
	}
	var s map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&s); err != nil {
		return nil, err
	}
	rewriteSchema(s)
	return json.Marshal(s)
}

func rewriteSchema(s map[string]any) {
	var moved []string
	for k, v := range s {
		drop := unsupportedBounds[k]
		if k == "minItems" {
			// minItems of 0 or 1 is accepted; anything else is not.
			n, ok := v.(json.Number)
			drop = !ok || (n.String() != "0" && n.String() != "1")
		}
		if drop {
			b, _ := json.Marshal(v)
			moved = append(moved, k+" "+string(b))
			delete(s, k)
		}
	}
	if len(moved) > 0 {
		slices.Sort(moved)
		note := "Bounds: " + strings.Join(moved, ", ") + "."
		if d, ok := s["description"].(string); ok && d != "" {
			note = d + " " + note
		}
		s["description"] = note
	}
	for _, k := range []string{"items", "not", "additionalProperties"} {
		if sub, ok := s[k].(map[string]any); ok {
			rewriteSchema(sub)
		}
	}
	for _, k := range []string{"anyOf", "allOf", "oneOf", "prefixItems"} {
		if list, ok := s[k].([]any); ok {
			for _, e := range list {
				if sub, ok := e.(map[string]any); ok {
					rewriteSchema(sub)
				}
			}
		}
	}
	for _, k := range []string{"properties", "$defs", "definitions"} {
		if m, ok := s[k].(map[string]any); ok {
			for _, e := range m {
				if sub, ok := e.(map[string]any); ok {
					rewriteSchema(sub)
				}
			}
		}
	}
}

// parseResponse decodes and checks the body of a 200 reply. model is the configured model the reply must
// name; offered names the tools the request offered; structured says a response schema was sent. The reply
// was billed, so on ErrMalformedOutput the Completion carries Usage and Model only, or UsageUnknown when no
// usable usage block could be read. Error text is fixed and never quotes the reply.
func parseResponse(data []byte, model string, offered map[string]bool, structured bool) (providers.Completion, error) {
	malformed := func(msg string) error { return fmt.Errorf("anthropic: %s: %w", msg, providers.ErrMalformedOutput) }
	unknown := providers.Completion{UsageUnknown: true}
	var r messagesResponse
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&r); err != nil {
		return unknown, malformed("the reply is not a message")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return unknown, malformed("the reply has data after the message")
	}
	u, ok := usageOf(r.Usage)
	if !ok {
		return unknown, malformed("the reply has no usage, or its usage is out of range")
	}
	if !validModel(r.Model) {
		return providers.Completion{Usage: u}, malformed("the reply names no valid model")
	}
	billed := providers.Completion{Usage: u, Model: r.Model}
	fail := func(msg string) (providers.Completion, error) { return billed, malformed(msg) }

	if r.Model != model {
		return fail("the reply names another model than the configured one")
	}
	if r.Type != "message" || r.Role != "assistant" {
		return fail("the reply is not an assistant message")
	}
	switch r.StopReason {
	case stopEndTurn, stopToolUse:
	case "max_tokens":
		return fail("the reply was cut off at the output token limit")
	case "refusal":
		return fail("the model refused")
	default:
		return fail("the reply ended for an unknown reason")
	}
	var text strings.Builder
	var calls []providers.ToolCall
	seen := map[string]bool{}
	for _, b := range r.Content {
		switch b.Type {
		case blockText:
			text.WriteString(b.Text)
		case "thinking", "redacted_thinking":
			// Reasoning, not part of the answer; never returned to the caller.
		case blockToolUse:
			switch {
			case len(calls) >= maxToolCalls:
				return fail("the reply has too many tool calls")
			case !toolCallIDPattern.MatchString(b.ID) || seen[b.ID]:
				return fail("a tool call ID is invalid or repeated")
			case !offered[b.Name]:
				return fail("a tool call names a tool the request did not offer")
			case !isJSONObject(b.Input):
				return fail("the input of a tool call is not a JSON object")
			}
			seen[b.ID] = true
			calls = append(calls, providers.ToolCall{ID: b.ID, Name: b.Name,
				Arguments: json.RawMessage(bytes.TrimSpace(b.Input))})
		default:
			return fail("the reply has a content block of an unknown type")
		}
	}
	content := text.String()
	if len(calls) == 0 {
		switch {
		case content == "":
			return fail("the reply has no text and no tool call")
		case structured && !isJSONObject([]byte(content)):
			return fail("the reply is not the JSON object the response schema asks for")
		}
	}
	return providers.Completion{Content: content, ToolCalls: calls, Usage: u, Model: r.Model}, nil
}

// usageOf converts a usage block, and reports false when there is none or it is out of range. InputTokens is
// the whole prompt: the API reports the uncached remainder, the cache writes and the cache reads apart.
func usageOf(au *apiUsage) (providers.Usage, bool) {
	if au == nil {
		return providers.Usage{}, false
	}
	for _, n := range []int64{au.InputTokens, au.OutputTokens, au.CacheCreationInputTokens, au.CacheReadInputTokens} {
		if n < 0 || n > maxUsageTokens {
			return providers.Usage{}, false
		}
	}
	return providers.Usage{
		InputTokens:       au.InputTokens + au.CacheCreationInputTokens + au.CacheReadInputTokens,
		CachedInputTokens: au.CacheReadInputTokens,
		OutputTokens:      au.OutputTokens,
	}, true
}

func validModel(m string) bool { return len(m) <= maxModelName && modelNamePattern.MatchString(m) }

// billedUsage reads only the usage and model of a 200 reply that was refused before it was parsed (a
// Content-Type other than JSON, a body too large to read, a read that failed). data is the body, or nil when
// it was not read whole. Without a usable usage block the Completion says UsageUnknown.
func billedUsage(data []byte) providers.Completion {
	var r struct {
		Model string    `json:"model"`
		Usage *apiUsage `json:"usage"`
	}
	if data == nil || json.Unmarshal(data, &r) != nil {
		return providers.Completion{UsageUnknown: true}
	}
	u, ok := usageOf(r.Usage)
	if !ok {
		return providers.Completion{UsageUnknown: true}
	}
	c := providers.Completion{Usage: u}
	if validModel(r.Model) {
		c.Model = r.Model
	}
	return c
}

// isJSONObject reports whether b is one valid JSON object, surrounded by white space at most.
func isJSONObject(b []byte) bool {
	t := bytes.TrimSpace(b)
	return len(t) > 0 && t[0] == '{' && json.Valid(t)
}
