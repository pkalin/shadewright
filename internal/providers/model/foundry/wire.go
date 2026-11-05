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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/pkalin/shadewright/internal/providers"
)

// Bounds of a reply. A reply outside them is malformed.
const (
	// maxToolCalls is the most tool calls one reply may hold: maxToolSteps is at most 16.
	maxToolCalls = 16
	// maxModelName bounds the model name a reply reports.
	maxModelName = 128
	// maxTokenBytes bounds an access token: an Entra ID JWT is a few KiB.
	maxTokenBytes = 16 << 10
)

// toolTypeFunction is the only kind of tool and tool call: a function.
const toolTypeFunction = "function"

var (
	// toolNamePattern is OpenAI's rule for function names.
	toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	// toolCallIDPattern bounds the IDs a reply gives its tool calls; they are sent back in tool messages.
	toolCallIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	// modelNamePattern is what a reported model and version may look like, for example gpt-5.4-mini-2026-03-17.
	modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)
)

// The request body of the chat completions API.
type (
	chatRequest struct {
		Model               string          `json:"model,omitempty"`
		Messages            []chatMessage   `json:"messages"`
		Tools               []chatTool      `json:"tools,omitempty"`
		ResponseFormat      *responseFormat `json:"response_format,omitempty"`
		MaxCompletionTokens int             `json:"max_completion_tokens"`
	}
	chatMessage struct {
		Role       string         `json:"role"`
		Content    *string        `json:"content"`
		ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
		ToolCallID string         `json:"tool_call_id,omitempty"`
	}
	chatToolCall struct {
		ID       string       `json:"id"`
		Type     string       `json:"type"`
		Function chatFunction `json:"function"`
	}
	chatFunction struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	chatTool struct {
		Type     string       `json:"type"`
		Function toolFunction `json:"function"`
	}
	toolFunction struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	responseFormat struct {
		Type       string     `json:"type"`
		JSONSchema jsonSchema `json:"json_schema"`
	}
	jsonSchema struct {
		Name   string          `json:"name"`
		Strict bool            `json:"strict"`
		Schema json.RawMessage `json:"schema"`
	}
)

// The reply of the chat completions API, only the fields the adapter reads.
type (
	chatResponse struct {
		Model   string       `json:"model"`
		Choices []chatChoice `json:"choices"`
		Usage   *chatUsage   `json:"usage"`
	}
	chatChoice struct {
		FinishReason string       `json:"finish_reason"`
		Message      replyMessage `json:"message"`
	}
	replyMessage struct {
		Content   *string        `json:"content"`
		Refusal   *string        `json:"refusal"`
		ToolCalls []chatToolCall `json:"tool_calls"`
	}
	chatUsage struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
)

// encode checks a request and builds its body. It returns the names of the tools offered, against which the
// reply's tool calls are checked. Errors are fixed text matching providers.ErrRejected (or ErrTooLarge), and
// never quote the request.
func (p *Provider) encode(req providers.CompletionRequest) ([]byte, map[string]bool, error) {
	rejected := func(msg string) error { return fmt.Errorf("foundry: %s: %w", msg, providers.ErrRejected) }
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
	body := chatRequest{MaxCompletionTokens: maxOut}
	if p.sendModel {
		body.Model = p.deployment
	}
	offered := map[string]bool{}
	for _, t := range req.Tools {
		if !toolNamePattern.MatchString(t.Name) || offered[t.Name] {
			return nil, nil, rejected("a tool name is invalid or repeated")
		}
		if !isJSONObject(t.Parameters) {
			return nil, nil, rejected("the parameters of a tool are not a JSON schema object")
		}
		offered[t.Name] = true
		body.Tools = append(body.Tools, chatTool{Type: toolTypeFunction, Function: toolFunction{
			Name: t.Name, Description: t.Description, Parameters: t.Parameters,
		}})
	}
	if len(req.ResponseSchema) > 0 {
		if !isJSONObject(req.ResponseSchema) {
			return nil, nil, rejected("the response schema is not a JSON object")
		}
		body.ResponseFormat = &responseFormat{Type: "json_schema", JSONSchema: jsonSchema{
			Name: "response", Strict: true, Schema: req.ResponseSchema,
		}}
	}
	for _, m := range req.Messages {
		cm, err := encodeMessage(m)
		if err != nil {
			return nil, nil, rejected(err.Error())
		}
		body.Messages = append(body.Messages, cm)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, nil, rejected("the request could not be encoded")
	}
	if len(data) > maxRequestBytes {
		return nil, nil, fmt.Errorf("foundry: the request is larger than %d bytes: %w", maxRequestBytes, providers.ErrTooLarge)
	}
	return data, offered, nil
}

// encodeMessage maps one message. Error text is fixed.
func encodeMessage(m providers.Message) (chatMessage, error) {
	content := m.Content
	cm := chatMessage{Role: string(m.Role), Content: &content}
	switch m.Role {
	case providers.RoleSystem, providers.RoleUser:
		if len(m.ToolCalls) > 0 || m.ToolCallID != "" {
			return chatMessage{}, errors.New("only assistant and tool messages carry tool calls")
		}
	case providers.RoleAssistant:
		if m.ToolCallID != "" {
			return chatMessage{}, errors.New("an assistant message has a tool call ID")
		}
		if content == "" && len(m.ToolCalls) > 0 {
			cm.Content = nil
		}
		for _, tc := range m.ToolCalls {
			if !toolCallIDPattern.MatchString(tc.ID) || !toolNamePattern.MatchString(tc.Name) ||
				!isJSONObject(tc.Arguments) {
				return chatMessage{}, errors.New("a tool call of an assistant message is invalid")
			}
			cm.ToolCalls = append(cm.ToolCalls, chatToolCall{ID: tc.ID, Type: toolTypeFunction, Function: chatFunction{
				Name: tc.Name, Arguments: string(tc.Arguments),
			}})
		}
	case providers.RoleTool:
		if !toolCallIDPattern.MatchString(m.ToolCallID) || len(m.ToolCalls) > 0 {
			return chatMessage{}, errors.New("a tool message needs a valid tool call ID and no tool calls")
		}
		cm.ToolCallID = m.ToolCallID
	default:
		return chatMessage{}, errors.New("a message has an unknown role")
	}
	return cm, nil
}

// parseResponse decodes and checks the body of a 200 reply. offered names the tools the request offered;
// structured says a response schema was sent. On ErrMalformedOutput after the usage block was read, the
// Completion carries Usage and Model only. Error text is fixed and never quotes the reply.
func parseResponse(data []byte, offered map[string]bool, structured bool) (providers.Completion, error) {
	malformed := func(msg string) error { return fmt.Errorf("foundry: %s: %w", msg, providers.ErrMalformedOutput) }
	var r chatResponse
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&r); err != nil {
		return providers.Completion{}, malformed("the reply is not a chat completion")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return providers.Completion{}, malformed("the reply has data after the chat completion")
	}
	if r.Usage == nil {
		return providers.Completion{}, malformed("the reply has no usage")
	}
	u := providers.Usage{InputTokens: r.Usage.PromptTokens, OutputTokens: r.Usage.CompletionTokens}
	if r.Usage.PromptTokensDetails != nil {
		u.CachedInputTokens = r.Usage.PromptTokensDetails.CachedTokens
	}
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CachedInputTokens < 0 || u.CachedInputTokens > u.InputTokens {
		return providers.Completion{}, malformed("the usage of the reply is out of range")
	}
	if len(r.Model) > maxModelName || !modelNamePattern.MatchString(r.Model) {
		return providers.Completion{}, malformed("the reply names no valid model")
	}
	billed := providers.Completion{Usage: u, Model: r.Model}
	fail := func(msg string) (providers.Completion, error) { return billed, malformed(msg) }

	if len(r.Choices) != 1 {
		return fail("the reply does not have exactly one choice")
	}
	ch := r.Choices[0]
	switch ch.FinishReason {
	case "stop", "tool_calls":
	case "length":
		return fail("the reply was cut off at the output token limit")
	case "content_filter":
		return fail("the reply was filtered")
	default:
		return fail("the reply ended for an unknown reason")
	}
	msg := ch.Message
	if msg.Refusal != nil && *msg.Refusal != "" {
		return fail("the model refused")
	}
	content := ""
	if msg.Content != nil {
		content = *msg.Content
	}
	if len(msg.ToolCalls) > maxToolCalls {
		return fail("the reply has too many tool calls")
	}
	calls := make([]providers.ToolCall, 0, len(msg.ToolCalls))
	seen := map[string]bool{}
	for _, tc := range msg.ToolCalls {
		switch {
		case tc.Type != toolTypeFunction:
			return fail("a tool call is not a function call")
		case !toolCallIDPattern.MatchString(tc.ID) || seen[tc.ID]:
			return fail("a tool call ID is invalid or repeated")
		case !offered[tc.Function.Name]:
			return fail("a tool call names a tool the request did not offer")
		case !isJSONObject([]byte(tc.Function.Arguments)):
			return fail("the arguments of a tool call are not a JSON object")
		}
		seen[tc.ID] = true
		calls = append(calls, providers.ToolCall{
			ID: tc.ID, Name: tc.Function.Name, Arguments: json.RawMessage(tc.Function.Arguments),
		})
	}
	if len(calls) == 0 {
		switch {
		case content == "":
			return fail("the reply has no content and no tool call")
		case structured && !isJSONObject([]byte(content)):
			return fail("the reply is not the JSON object the response schema asks for")
		}
		calls = nil
	}
	return providers.Completion{Content: content, ToolCalls: calls, Usage: u, Model: r.Model}, nil
}

// isJSONObject reports whether b is one valid JSON object, surrounded by white space at most.
func isJSONObject(b []byte) bool {
	t := bytes.TrimSpace(b)
	return len(t) > 0 && t[0] == '{' && json.Valid(t)
}

// checkToken checks an access token before it is sent: a header-safe JWT whose audience is Cognitive
// Services. The payload is decoded, not verified: the endpoint verifies the signature; this check only keeps
// a token for another resource (or anything that is not a token) from being sent. Errors are fixed text
// matching providers.ErrInvalidConfig.
func checkToken(tok string) error {
	invalid := func(msg string) error { return fmt.Errorf("foundry: %s: %w", msg, providers.ErrInvalidConfig) }
	if tok == "" || len(tok) > maxTokenBytes {
		return invalid("the access token is empty or too large")
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return invalid("the access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return invalid("the access token is not a JWT")
	}
	for _, p := range []string{parts[0], parts[2]} {
		if _, err := base64.RawURLEncoding.DecodeString(p); err != nil || p == "" {
			return invalid("the access token is not a JWT")
		}
	}
	var claims struct {
		Aud json.RawMessage `json:"aud"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return invalid("the access token is not a JWT")
	}
	var auds []string
	var one string
	if err := json.Unmarshal(claims.Aud, &one); err == nil {
		auds = []string{one}
	} else if err := json.Unmarshal(claims.Aud, &auds); err != nil {
		return invalid("the access token has no audience")
	}
	for _, a := range auds {
		if strings.TrimSuffix(a, "/") == cognitiveServicesAudience {
			return nil
		}
	}
	return invalid("the access token is not for the Cognitive Services audience")
}
