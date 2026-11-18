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
	"strings"

	"github.com/pkalin/shadewright/internal/providers"
)

// The Responses API (#501): the same request as a chat completion, in another shape. Messages become input
// items, the tool calls of an assistant message function_call items after it, and tool results
// function_call_output items; the response schema goes in text.format. The request is stateless: store is
// false, so the service keeps neither the redacted context nor the reply, and each call sends the whole
// conversation again, as with chat completions.

// Output item and content types the adapter reads. Any other output item is refused.
const (
	itemMessage            = "message"
	itemFunctionCall       = "function_call"
	itemFunctionCallOutput = "function_call_output"
	itemReasoning          = "reasoning"
	partOutputText         = "output_text"
	partRefusal            = "refusal"
)

// The request body of the Responses API.
type (
	responsesRequest struct {
		Model           string           `json:"model"`
		Input           []responsesInput `json:"input"`
		Tools           []responsesTool  `json:"tools,omitempty"`
		Text            *responsesText   `json:"text,omitempty"`
		MaxOutputTokens int              `json:"max_output_tokens"`
		// Store is always false: the service keeps no copy of the request or the reply.
		Store bool `json:"store"`
	}
	// responsesInput is one input item: a message (Role and Content), a function_call (CallID, Name,
	// Arguments) or a function_call_output (CallID, Output).
	responsesInput struct {
		Type      string  `json:"type"`
		Role      string  `json:"role,omitempty"`
		Content   *string `json:"content,omitempty"`
		CallID    string  `json:"call_id,omitempty"`
		Name      string  `json:"name,omitempty"`
		Arguments string  `json:"arguments,omitempty"`
		Output    *string `json:"output,omitempty"`
	}
	responsesTool struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
		// Strict is always false: the Responses API makes a function tool strict unless told otherwise, and
		// chat completions do not, so the same tool definitions keep the same meaning in both APIs.
		Strict bool `json:"strict"`
	}
	responsesText struct {
		Format responsesFormat `json:"format"`
	}
	responsesFormat struct {
		Type   string          `json:"type"`
		Name   string          `json:"name"`
		Strict bool            `json:"strict"`
		Schema json.RawMessage `json:"schema"`
	}
)

// The reply of the Responses API, only the fields the adapter reads.
type (
	responsesReply struct {
		Model             string `json:"model"`
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []responsesOutput `json:"output"`
		Usage  *responsesUsage   `json:"usage"`
	}
	responsesOutput struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   []responsesPart `json:"content"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
	}
	responsesPart struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	}
	// responsesUsage is the usage block. Both counts must be present: a block without them (a chat completion's,
	// for example) is no usable usage, and the call is counted as UsageUnknown rather than as zero tokens.
	responsesUsage struct {
		InputTokens        *int64 `json:"input_tokens"`
		OutputTokens       *int64 `json:"output_tokens"`
		InputTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	}
)

// maxOutputItems bounds the output items of one reply: at most maxToolCalls calls, a message and the
// reasoning items between them.
const maxOutputItems = 4 * maxToolCalls

// responsesBody is the Responses body of a checked request. Each message keeps its role and content; an
// assistant message is followed by one function_call item per tool call, and is left out when it has no
// content but tool calls, as chat completions send it with a null content.
func responsesBody(cr checkedRequest, deployment string) responsesRequest {
	body := responsesRequest{Model: deployment, MaxOutputTokens: cr.maxOut}
	for _, m := range cr.messages {
		switch {
		case m.Role == string(providers.RoleTool):
			body.Input = append(body.Input, responsesInput{Type: itemFunctionCallOutput, CallID: m.ToolCallID,
				Output: m.Content})
			continue
		case m.Content != nil:
			body.Input = append(body.Input, responsesInput{Type: itemMessage, Role: m.Role, Content: m.Content})
		}
		for _, tc := range m.ToolCalls {
			body.Input = append(body.Input, responsesInput{Type: itemFunctionCall, CallID: tc.ID,
				Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
	}
	for _, t := range cr.tools {
		body.Tools = append(body.Tools, responsesTool{Type: toolTypeFunction, Name: t.Name,
			Description: t.Description, Parameters: t.Parameters})
	}
	if cr.schema != nil {
		body.Text = &responsesText{Format: responsesFormat{Type: "json_schema", Name: responseSchemaName,
			Strict: true, Schema: cr.schema}}
	}
	return body
}

// parseResponses decodes and checks the body of a 200 reply of the Responses API, with the same rules and
// results as parseResponse for a chat completion: a reply that did not complete (cut off at the output limit,
// filtered, failed), a refusal, an output item of a type the request cannot produce, or content and tool calls
// that break the request's contract are ErrMalformedOutput, returned with the reply's Usage and Model. The
// content is the text of the reply's output_text parts, in order; reasoning items are skipped and never
// returned. Error text is fixed and never quotes the reply.
func parseResponses(data []byte, offered map[string]bool, structured bool) (providers.Completion, error) {
	malformed := func(msg string) error { return fmt.Errorf("foundry: %s: %w", msg, providers.ErrMalformedOutput) }
	unknown := providers.Completion{UsageUnknown: true}
	var r responsesReply
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&r); err != nil {
		return unknown, malformed("the reply is not a response")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return unknown, malformed("the reply has data after the response")
	}
	u, ok := usageOf(r.Usage.chat())
	if !ok {
		return unknown, malformed("the reply has no usage, or its usage is out of range")
	}
	if !validModel(r.Model) {
		return providers.Completion{Usage: u}, malformed("the reply names no valid model")
	}
	billed := providers.Completion{Usage: u, Model: r.Model}
	fail := func(msg string) (providers.Completion, error) { return billed, malformed(msg) }

	switch r.Status {
	case "completed":
	case "incomplete":
		reason := ""
		if r.IncompleteDetails != nil {
			reason = r.IncompleteDetails.Reason
		}
		switch reason {
		case "max_output_tokens":
			return fail("the reply was cut off at the output token limit")
		case "content_filter":
			return fail("the reply was filtered")
		default:
			return fail("the reply ended for an unknown reason")
		}
	default:
		return fail("the reply did not complete")
	}
	if len(r.Output) > maxOutputItems {
		return fail("the reply has too many output items")
	}
	var content strings.Builder
	var calls []chatToolCall
	for _, it := range r.Output {
		switch it.Type {
		case itemReasoning:
		case itemMessage:
			if it.Role != string(providers.RoleAssistant) {
				return fail("a message of the reply is not the assistant's")
			}
			for _, part := range it.Content {
				switch part.Type {
				case partOutputText:
					content.WriteString(part.Text)
				case partRefusal:
					return fail("the model refused")
				default:
					return fail("a message of the reply has content of an unknown type")
				}
			}
		case itemFunctionCall:
			if len(calls) == maxToolCalls {
				return fail("the reply has too many tool calls")
			}
			calls = append(calls, chatToolCall{ID: it.CallID, Type: toolTypeFunction,
				Function: chatFunction{Name: it.Name, Arguments: it.Arguments}})
		default:
			return fail("the reply has an output item of an unknown type")
		}
	}
	return checkOutput(billed, content.String(), calls, offered, structured)
}

// chat converts a Responses usage block to the chat completions one, which usageOf checks. A missing block, or
// one without both counts, is nil.
func (ru *responsesUsage) chat() *chatUsage {
	if ru == nil || ru.InputTokens == nil || ru.OutputTokens == nil {
		return nil
	}
	cu := &chatUsage{PromptTokens: *ru.InputTokens, CompletionTokens: *ru.OutputTokens}
	if ru.InputTokensDetails != nil {
		cu.PromptTokensDetails = &struct {
			CachedTokens int64 `json:"cached_tokens"`
		}{CachedTokens: ru.InputTokensDetails.CachedTokens}
	}
	return cu
}

// billedResponsesUsage is billedUsage for a reply of the Responses API.
func billedResponsesUsage(data []byte) providers.Completion {
	var r struct {
		Model string          `json:"model"`
		Usage *responsesUsage `json:"usage"`
	}
	if data == nil || json.Unmarshal(data, &r) != nil {
		return providers.Completion{UsageUnknown: true}
	}
	return billedOf(r.Model, r.Usage.chat())
}
