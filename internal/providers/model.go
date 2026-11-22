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

package providers

import (
	"context"
	"encoding/json"
)

// Role is the author of a message in a model conversation.
type Role string

// Message roles.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn in a model conversation.
type Message struct {
	Role    Role
	Content string
	// ToolCalls are the calls the model asked for (assistant messages only).
	ToolCalls []ToolCall
	// ToolCallID ties a tool result to the call it answers (tool messages only).
	ToolCallID string
	// ProviderBlocks is the Completion.ProviderBlocks of the reply whose ToolCalls this assistant message
	// carries, copied unchanged; empty otherwise. Adapters that do not use it ignore it.
	ProviderBlocks Opaque
}

// ToolDefinition describes a tool the model may call.
type ToolDefinition struct {
	Name        string
	Description string
	// Parameters is the JSON schema of the arguments.
	Parameters json.RawMessage
}

// ToolCall is a call the model asked for. Arguments are untrusted and must be validated against the schema.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
	// HeldControls is set by [WithoutControls] on a call whose ID, name, or a decoded string or key of its
	// arguments held a control character, which it removed (#852). The analysis loop never runs such a call.
	// Adapters leave it false. It is the loop's mark, not part of a reply, so it is never marshaled.
	HeldControls bool `json:"-"`
}

// CompletionRequest is one call to a model.
type CompletionRequest struct {
	Messages []Message
	Tools    []ToolDefinition
	// ResponseSchema, when set, asks for structured output matching this JSON schema.
	ResponseSchema json.RawMessage
	// MaxOutputTokens bounds the reply; zero means the adapter's configured limit.
	MaxOutputTokens int
	// MaxBilledAttempts, when above zero, bounds the attempts of the call that the service may bill: an
	// adapter that reports its billed attempts (Completion.AttemptsReported) makes no further attempt once
	// this many may have been billed. The caller sets it from the budget it has left, so that charging every
	// attempt at the call's ceiling cannot pass the budget. Zero means the adapter's own bound. Adapters that do
	// not report their attempts ignore it.
	MaxBilledAttempts int
}

// BilledAttempt is one attempt of a call that the service may have billed.
type BilledAttempt struct {
	// Usage is what the attempt's reply reported; zero when UsageUnknown.
	Usage Usage
	// UsageUnknown is set when the attempt reported no usable usage: a failure after the request was sent, a
	// reply that could not be read, or one without a complete usage block. The caller charges it the call's
	// ceiling.
	UsageUnknown bool
}

// Usage counts the tokens of one call, for cost accounting (design 12.1).
type Usage struct {
	InputTokens       int64
	CachedInputTokens int64
	OutputTokens      int64
}

// Completion is the model's reply to one call.
type Completion struct {
	// Content is the text or structured output; it is untrusted.
	Content   string
	ToolCalls []ToolCall
	Usage     Usage
	// Model identifies the deployed model and version that answered.
	Model string
	// UsageUnknown says that the model answered at least one attempt of the call (so it was billed) but the
	// usage of that answer could not be read, for example a reply too large to read or not a chat completion.
	// Usage then counts only what was read; a budget should count the call conservatively, for example as
	// the request's whole output limit.
	UsageUnknown bool
	// AttemptsReported is set by an adapter that lists in BilledAttempts every attempt of the call that the
	// service may have billed, retries included. The caller then charges each listed attempt on its own (the
	// call's ceiling for one with unknown usage) instead of charging the call once from Usage and
	// UsageUnknown. Adapters that leave it false are charged as before. Only the OpenAI-compatible adapter
	// sets it (ADR-0042). Neither field is part of the JSON form of a Completion, which records and the wire
	// golden tests hash.
	AttemptsReported bool `json:"-"`
	// BilledAttempts lists, in order, the attempts the service may have billed when AttemptsReported is set;
	// empty when no attempt was sent. The last one, when it got the reply, carries the same Usage as the
	// Completion.
	BilledAttempts []BilledAttempt `json:"-"`
	// ProviderBlocks is what the adapter needs back on the assistant message that carries ToolCalls in the
	// next request of the conversation (Message.ProviderBlocks), for example thinking blocks. The caller never
	// reads, logs or stores it. Empty without tool calls and for adapters that need nothing back.
	ProviderBlocks Opaque
	// ContentControls lists, as set by [WithoutControls], where Content held a control character it removed:
	// one entry for each decoded string or object key of the JSON that held one. The caller, which knows the
	// schema of the content, decides what a control in each place means (#852). Adapters leave it empty; it is
	// the loop's mark, never marshaled.
	ContentControls []ControlPath `json:"-"`
}

// ControlPath is where a decoded string of a JSON document held a control character ([WithoutControls]).
type ControlPath struct {
	// Path is the names of the members and the decimal indexes of the array elements from the root to the
	// string, each without its control characters. It is empty for a string at the root, and for a control
	// whose place could not be told because the document does not parse.
	Path []string
	// Key says the string is the name of a member, the last element of Path, not its value.
	Key bool
}

// OutputBounder is implemented by a ModelProvider that may bill more output tokens for a request than its
// MaxOutputTokens, for example because it adds room for thinking to the limit it sends. The caller then
// bounds a call's cost with OutputSpend instead of MaxOutputTokens: in the budget check before the call, and
// in the charge of a call that failed or whose usage is unknown.
type OutputBounder interface {
	// OutputSpend returns the most output tokens a request with this MaxOutputTokens can be billed.
	OutputSpend(maxOutputTokens int) int
}

// ModelProvider calls a language model.
type ModelProvider interface {
	// Complete sends one request. Throttling and transient failures are returned as retryable errors; an
	// unknown deployment is ErrNotFound. A reply that breaks the request's contract is an error matching
	// ErrMalformedOutput; the Completion returned
	// with it carries the Usage and Model of that reply (the call was billed) and nothing else. With any
	// other error the Completion carries at most UsageUnknown, set when an answer was billed but not
	// counted. An adapter marks an error with ErrNotSent or ErrNotProcessed only when it knows for certain
	// that nothing of the call was billed.
	Complete(ctx context.Context, req CompletionRequest) (Completion, error)
}

// EstimateInputTokens conservatively counts one token per two bytes of model input, including tool schemas and provider blocks.
func EstimateInputTokens(req CompletionRequest) int64 {
	n := len(req.ResponseSchema)
	for _, m := range req.Messages {
		n += len(m.Content) + len(m.ToolCallID) + m.ProviderBlocks.Len()
		for _, c := range m.ToolCalls {
			n += len(c.ID) + len(c.Name) + len(c.Arguments)
		}
	}
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.Parameters)
	}
	return int64(n+1) / 2
}

// CompatibleUsageCeiling bounds untrusted reported usage by input plus output, with a rounded-up ten percent margin.
func CompatibleUsageCeiling(input, output int64) int64 {
	bound := input + output
	return bound + (bound+9)/10
}
