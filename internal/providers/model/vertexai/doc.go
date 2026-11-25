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

// Package vertexai is the Vertex AI adapter of [providers.ModelProvider] and [providers.OutputBounder]
// (ADR-0050, ADR-0013). It calls one Gemini 2.5 model through the regional generateContent method. It imports
// the standard library and this module only: no vendor SDK. The analyzer does not use it yet: the factory
// and the token exchange are built in other packages.
//
// Destination. [New] takes the endpoint of a ModelDeployment,
// https://{location}-aiplatform.googleapis.com/v1/projects/{project}/locations/{location}, and the model ID
// of its deployment field. The location must be one of the EU regions ADR-0050 lists, and the same in the
// host and the path; the global endpoint, the EU multi-region endpoint and every other host are refused, as
// are a port, user info, a query, a fragment, an escaped path and an IP literal. The model must be a Gemini
// 2.5 model (Flash-Lite is refused in europe-west3, which does not serve it); Gemini 3 is refused. The
// request URL is built from the validated location, project and model, never from the endpoint text. Every
// refusal matches [providers.ErrInvalidConfig] with fixed text that never repeats the input.
//
// Credentials. Config.Tokens returns a federated access token for the analyzer's own ServiceAccount; the
// adapter asks for one at every attempt and never stores it. A token that is empty, too large or holds a
// character a header cannot carry is refused as [providers.ErrForbidden] before anything is sent. An error of
// the token source is replaced by fixed text that keeps only its class. The token never appears in an error,
// a [CallReport] or a log line, and nothing is logged.
//
// Transport. The client uses no proxy, requires TLS 1.2 or later, follows no redirect (a 3xx answer is
// [providers.ErrRejected]) and dials only public unicast addresses outside Config.InClusterRanges
// (openaiwire.Guard), checked on every resolved address. Every call has a deadline (Config.Timeout unless the
// caller's context has a shorter one) and each attempt its own (Config.AttemptTimeout), the token request
// included. A response that net/http returns after the attempt's context ended is the timeout and nothing of
// it is read. An error body is drained up to 64 KiB and never read into an error; a reply body is read up to
// Config.MaxResponseBytes.
//
// Errors. Network failures, attempt timeouts, 408, 429 and 5xx are retried with exponential backoff and
// jitter, up to Config.MaxAttempts attempts; a Retry-After delay is waited for when it is at most
// Config.MaxDelay and returned in a [providers.RetryableError] otherwise. 401 and 403 are
// [providers.ErrForbidden], 404 is [providers.ErrNotFound], 413 is [providers.ErrTooLarge] and any other status
// is [providers.ErrRejected]. A call ended by 429 is never marked [providers.ErrNotProcessed]: that Google does
// not bill it is not documented. A call none of whose attempts handed a request to the HTTP client (a
// request that could not be encoded, no token, a token refused) is marked [providers.ErrNotSent].
//
// Request. The leading system messages become systemInstruction. Adjacent user-side messages (user and tool
// results) become one user turn, in order, and assistant messages model turns. The offered tools become
// functionDeclarations with their JSON schema as parametersJsonSchema. A request without tools that carries
// a response schema asks for application/json with responseJsonSchema, after moving the string and array
// bounds (minLength, maxLength, pattern, minItems, maxItems, uniqueItems) into the description of their
// schema; a request that offers tools sends neither, because Gemini 2.5 refuses function declarations with a
// JSON response type. The caller validates every answer against its full schema.
//
// Output bound. generationConfig.maxOutputTokens is [Provider.OutputSpend] of the request's limit: the limit
// plus Config.ThinkingAllowance (default 4096 tokens, zero allowed when set explicitly), and
// thinkingConfig.thinkingBudget is the allowance, so thinking cannot use up the room of the answer while one
// billed cap bounds candidate and thinking tokens together.
//
// Tool turns. A function call whose reply gives no ID gets a local ID by position (vertex_call_0,
// vertex_call_1, ...), which is never sent back: its functionResponse has no ID. An ID the reply gives is
// echoed. Each tool result must answer the call at its position in the preceding assistant message, by ID, so
// a missing, repeated or reordered result is refused even when two calls name the same tool. A functionResponse
// carries the call's name and {"result": <tool text>}.
//
// Replay. A reply with function calls returns its parts in Completion.ProviderBlocks, bound to the configured
// model. An assistant message that carries tool calls must carry those blocks; before they are sent back the
// adapter checks the part types and the number, order, IDs, names and arguments of the calls against
// Message.ToolCalls (arguments compared after removing control characters on both sides, as the analysis loop
// stores them), then sends the parts exactly as they came, thoughtSignature included. A mismatch is
// [providers.ErrRejected] and nothing is sent.
//
// Reply. A reply is untrusted (invariant 4). It must be one JSON object without repeated keys, nested at
// most 100 levels deep, with a usable
// usageMetadata, a modelVersion of a valid form, exactly one candidate whose role is model and whose
// finishReason is STOP, and parts that are text (optionally a thought, which never reaches the answer) or a
// function call. A function call must name an offered tool, have object arguments and an ID and name without
// control characters. With no call the text must be nonempty, and a JSON object when a response schema was
// sent. Any other reply is [providers.ErrMalformedOutput]; it was billed, so its Usage and Model come with the
// error, or UsageUnknown when its usage cannot be read. modelVersion is returned as Completion.Model for the
// caller's exact model check.
//
// Usage. InputTokens is promptTokenCount plus toolUsePromptTokenCount, CachedInputTokens is
// cachedContentTokenCount (part of the prompt) and OutputTokens is candidatesTokenCount plus
// thoughtsTokenCount. Absent counts are zero. A count out of range, a cached count above the prompt, or a
// totalTokenCount other than the sum makes the usage unusable.
package vertexai
