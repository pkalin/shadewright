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

import "github.com/pkalin/shadewright/internal/providers"

// API identifies an OpenAI wire format.
type API string

// Supported inference APIs and bounds, shared with Foundry.
const (
	// APIChatCompletions is the Chat Completions wire format.
	APIChatCompletions API = "ChatCompletions"
	// APIResponses is the Responses wire format.
	APIResponses API = "Responses"
	// DefaultMaxOutputTokens is the default per-call output limit.
	DefaultMaxOutputTokens = 2048
	// MaxOutputTokens is the largest per-call output limit.
	MaxOutputTokens = 16384
	maxRequestBytes = 4 << 20
)

// Codec is a stateless encoder for a deployment. Adapters validate configuration before using it.
type Codec struct {
	// API selects the wire format.
	API API
	// Deployment is the model or deployment name.
	Deployment string
	// SendModel includes the deployment in the Chat Completions body.
	SendModel bool
	// MaxOutput is the validated default output bound.
	MaxOutput int
}

// Encode validates and encodes a request without interpreting or changing its schemas.
func (p Codec) Encode(req providers.CompletionRequest) ([]byte, map[string]bool, error) {
	return p.encode(req)
}

// ParseResponse parses an untrusted Chat Completions reply, retaining usage on malformed output.
func ParseResponse(data []byte, offered map[string]bool, structured bool) (providers.Completion, error) {
	return parseResponse(data, offered, structured)
}

// ParseResponses parses an untrusted Responses reply, retaining usage on malformed output.
func ParseResponses(data []byte, offered map[string]bool, structured bool) (providers.Completion, error) {
	return parseResponses(data, offered, structured)
}

// BilledUsage reads only usage and model from a billed Chat Completions reply refused before parsing.
func BilledUsage(data []byte) providers.Completion { return billedUsage(data) }

// BilledResponsesUsage reads only usage and model from a billed Responses reply refused before parsing.
func BilledResponsesUsage(data []byte) providers.Completion { return billedResponsesUsage(data) }
