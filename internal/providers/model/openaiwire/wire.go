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

package openaiwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/shadewright-labs/shadewright/internal/providers"
)

// Bounds of a reply. A reply outside them is malformed.
const (
	// maxToolCalls is the most tool calls one reply may hold: maxToolSteps is at most 16.
	maxToolCalls = 16
	// maxModelName bounds the model name a reply reports.
	maxModelName = 128
)

// toolTypeFunction is the only kind of tool and tool call: a function.
const toolTypeFunction = "function"

// responseSchemaName is the name the response schema is sent under, in both APIs.
const responseSchemaName = "response"

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

// checkedRequest is a request that passed every check, in the chat completions form both encoders start from.
type checkedRequest struct {
	maxOut   int
	tools    []toolFunction
	schema   json.RawMessage
	messages []chatMessage
	offered  map[string]bool
}

// encode checks a request and builds its body in the Provider's API. It returns the names of the tools
// offered, against which the reply's tool calls are checked. Errors are fixed text matching
// providers.ErrRejected (or ErrTooLarge), and never quote the request.
func (p *Codec) encode(req providers.CompletionRequest) ([]byte, map[string]bool, error) {
	cr, err := p.check(req)
	if err != nil {
		return nil, nil, err
	}
	var body any
	if p.API == APIResponses {
		body = responsesBody(cr, p.Deployment)
	} else {
		body = chatBody(cr, p.Deployment, p.SendModel)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, nil, fmt.Errorf("foundry: the request could not be encoded: %w", providers.ErrRejected)
	}
	if len(data) > maxRequestBytes {
		return nil, nil, fmt.Errorf("foundry: the request is larger than %d bytes: %w", maxRequestBytes, providers.ErrTooLarge)
	}
	return data, cr.offered, nil
}

// check checks a request against the rules both APIs share. Errors are fixed text matching
// providers.ErrRejected.
func (p *Codec) check(req providers.CompletionRequest) (checkedRequest, error) {
	rejected := func(msg string) error { return fmt.Errorf("foundry: %s: %w", msg, providers.ErrRejected) }
	if len(req.Messages) == 0 {
		return checkedRequest{}, rejected("the request has no messages")
	}
	maxOut := req.MaxOutputTokens
	if maxOut == 0 {
		maxOut = p.MaxOutput
	}
	if maxOut < 1 || maxOut > MaxOutputTokens {
		return checkedRequest{}, rejected("the output token limit is out of range")
	}
	cr := checkedRequest{maxOut: maxOut, offered: map[string]bool{}}
	for _, t := range req.Tools {
		if !toolNamePattern.MatchString(t.Name) || cr.offered[t.Name] {
			return checkedRequest{}, rejected("a tool name is invalid or repeated")
		}
		if !isJSONObject(t.Parameters) {
			return checkedRequest{}, rejected("the parameters of a tool are not a JSON schema object")
		}
		cr.offered[t.Name] = true
		cr.tools = append(cr.tools, toolFunction{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	if len(req.ResponseSchema) > 0 {
		if !isJSONObject(req.ResponseSchema) {
			return checkedRequest{}, rejected("the response schema is not a JSON object")
		}
		cr.schema = req.ResponseSchema
	}
	for _, m := range req.Messages {
		cm, err := encodeMessage(m)
		if err != nil {
			return checkedRequest{}, rejected(err.Error())
		}
		cr.messages = append(cr.messages, cm)
	}
	return cr, nil
}

// chatBody is the chat completions body of a checked request.
func chatBody(cr checkedRequest, deployment string, sendModel bool) chatRequest {
	body := chatRequest{MaxCompletionTokens: cr.maxOut, Messages: cr.messages}
	if sendModel {
		body.Model = deployment
	}
	for _, t := range cr.tools {
		body.Tools = append(body.Tools, chatTool{Type: toolTypeFunction, Function: t})
	}
	if cr.schema != nil {
		body.ResponseFormat = &responseFormat{Type: "json_schema", JSONSchema: jsonSchema{
			Name: responseSchemaName, Strict: true, Schema: cr.schema,
		}}
	}
	return body
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
// structured says a response schema was sent. The reply was billed, so on ErrMalformedOutput the Completion
// carries Usage and Model only, or UsageUnknown when no usable usage block could be read. Error text is fixed
// and never quotes the reply.
func parseResponse(data []byte, offered map[string]bool, structured bool) (providers.Completion, error) {
	malformed := func(msg string) error { return fmt.Errorf("foundry: %s: %w", msg, providers.ErrMalformedOutput) }
	unknown := providers.Completion{UsageUnknown: true}
	var r chatResponse
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&r); err != nil {
		return unknown, malformed("the reply is not a chat completion")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return unknown, malformed("the reply has data after the chat completion")
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
	return checkOutput(billed, content, msg.ToolCalls, offered, structured)
}

// checkOutput checks the content and tool calls of a reply that completed, in either API, and returns the
// Completion. billed carries the reply's Usage and Model, and is what an error returns. Error text is fixed and
// never quotes the reply.
func checkOutput(billed providers.Completion, content string, toolCalls []chatToolCall, offered map[string]bool,
	structured bool) (providers.Completion, error) {
	fail := func(msg string) (providers.Completion, error) {
		return billed, fmt.Errorf("foundry: %s: %w", msg, providers.ErrMalformedOutput)
	}
	if len(toolCalls) > maxToolCalls {
		return fail("the reply has too many tool calls")
	}
	calls := make([]providers.ToolCall, 0, len(toolCalls))
	seen := map[string]bool{}
	for _, tc := range toolCalls {
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
	return providers.Completion{Content: content, ToolCalls: calls, Usage: billed.Usage, Model: billed.Model}, nil
}

// isJSONObject reports whether b is one valid JSON object, surrounded by white space at most.
// usageOf converts a usage block, and reports false when there is none or it is out of range.
func usageOf(cu *chatUsage) (providers.Usage, bool) {
	if cu == nil {
		return providers.Usage{}, false
	}
	u := providers.Usage{InputTokens: cu.PromptTokens, OutputTokens: cu.CompletionTokens}
	if cu.PromptTokensDetails != nil {
		u.CachedInputTokens = cu.PromptTokensDetails.CachedTokens
	}
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CachedInputTokens < 0 || u.CachedInputTokens > u.InputTokens {
		return providers.Usage{}, false
	}
	return u, true
}

func validModel(m string) bool { return len(m) <= maxModelName && modelNamePattern.MatchString(m) }

// billedUsage reads only the usage and model of a 200 reply that was refused before it was parsed (a
// Content-Type other than JSON, a body too large to read, a read that failed). data is the body, read within
// the response bound, or nil when it was not read whole. Without a usable usage block the Completion says
// UsageUnknown, so that a budget counts the call conservatively.
func billedUsage(data []byte) providers.Completion {
	var r struct {
		Model string     `json:"model"`
		Usage *chatUsage `json:"usage"`
	}
	if data == nil || json.Unmarshal(data, &r) != nil {
		return providers.Completion{UsageUnknown: true}
	}
	return billedOf(r.Model, r.Usage)
}

// billedOf is the Completion of a billed reply that was refused before it was parsed: its usage, or
// UsageUnknown without a usable usage block, and its model when that is valid.
func billedOf(model string, cu *chatUsage) providers.Completion {
	u, ok := usageOf(cu)
	if !ok {
		return providers.Completion{UsageUnknown: true}
	}
	c := providers.Completion{Usage: u}
	if validModel(model) {
		c.Model = model
	}
	return c
}

func isJSONObject(b []byte) bool {
	t := bytes.TrimSpace(b)
	return len(t) > 0 && t[0] == '{' && json.Valid(t)
}
