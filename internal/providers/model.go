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
}

// CompletionRequest is one call to a model.
type CompletionRequest struct {
	Messages []Message
	Tools    []ToolDefinition
	// ResponseSchema, when set, asks for structured output matching this JSON schema.
	ResponseSchema json.RawMessage
	// MaxOutputTokens bounds the reply; zero means the adapter's configured limit.
	MaxOutputTokens int
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
}

// ModelProvider calls a language model.
type ModelProvider interface {
	// Complete sends one request. Throttling and transient failures are returned as retryable errors; an
	// unknown deployment is ErrNotFound. A reply that breaks the request's contract is an error matching
	// ErrMalformedOutput; the Completion returned
	// with it carries the Usage and Model of that reply (the call was billed) and nothing else. With any
	// other error the Completion carries at most UsageUnknown, set when an answer was billed but not
	// counted.
	Complete(ctx context.Context, req CompletionRequest) (Completion, error)
}
