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

// Package anthropic is the Anthropic API adapter of [providers.ModelProvider] (ADR-0013, design section
// 11.1). It calls one Claude model through the Messages API, with structured output (output_config.format, a
// JSON schema) and client tools. It imports the standard library and this module only: no vendor SDK. So far
// only the evaluation harness uses it (EVAL_MODEL=anthropic, docs/evaluation.md); the analyzer does not
// (#729).
//
// A [Provider] is bound at construction to one model ID, which must be the ID the replies name: a reply that
// names any other model is [providers.ErrMalformedOutput], with its usage (ADR-0023 decision 8; the analysis
// loop refuses it again as an unexpected model). There is no fallback to another model or provider: the
// server-side fallbacks of the API are not requested.
//
// The Provider posts to https://api.anthropic.com/v1/messages only. The host is a constant, not a setting, so
// no configuration can send the key elsewhere; the Provider follows no redirect and uses no proxy.
//
// Authentication is an API key in the x-api-key header, read from Config.KeyFile at every attempt, so a
// rotated key is used without a restart. Every key file error matches [providers.ErrCredentialUnavailable]: a
// file that cannot be read, or is empty, is retryable; a file that is too large, or a key that is too long or
// holds characters a header cannot carry (an inner newline, a UTF-8 byte order mark), is malformed, not
// retried, and stays so until a person fixes the file. It is never [providers.ErrForbidden]: nothing was sent.
// The error of a call none of whose attempts handed a request to the HTTP client is marked
// [providers.ErrNotSent], so nothing of it can have been billed. The key never appears in an error, a
// [CallReport] or a log line, and nothing is logged.
//
// The request: the leading system messages become the top-level system prompt, whose last block carries a
// cache_control breakpoint, so the stable prefix (tool definitions and instructions) is cached where the
// model supports caching for a prefix that long. User and tool messages become user turns (tool results as
// tool_result blocks), consecutive ones merged into one turn; assistant messages become text and tool_use
// blocks. A response schema is sent as output_config.format after removing the keywords the API does not
// accept in a schema (numeric and length bounds, maxItems, minItems above 1); each removed bound is appended
// to the description of its schema. The caller validates the reply against its full schema, as with every
// adapter.
//
// Thinking: the request sends no thinking setting, so the model runs with its own (adaptive thinking on the
// current models, which on some cannot be turned off), and sends output_config.effort from Config.Effort
// (default low). Thinking tokens count as output, so max_tokens is the request's output limit plus
// Config.ThinkingAllowance (default 4096 tokens): thinking up to the allowance does not cut off an answer that
// fits the limit. Usage reports every output token, thinking included, so the caller's budgets count them. A
// reply that still stops at max_tokens is [providers.ErrMalformedOutput] with its usage. Thinking blocks never
// reach the caller's text: a reply with tool calls returns its whole content in Completion.ProviderBlocks,
// opaque to the caller, and an assistant message that carries them is sent back with exactly those blocks, in
// their order and unchanged, as the API expects of a tool round. Such blocks must hold exactly the message's
// tool calls, in order, or the request is [providers.ErrRejected].
//
// Every call has a deadline (Config.Timeout unless the caller's context has a shorter one) and each attempt
// its own (Config.AttemptTimeout). Network failures, timeouts of one attempt, 408, 429 and 5xx (529, overloaded,
// included) are retried with exponential backoff and jitter, up to Config.MaxAttempts attempts; a delay the
// service asks for in retry-after (seconds; an HTTP date is ignored, it would compare the server's clock with
// ours) is waited for when it is at most Config.MaxDelay, and a longer one is returned as a
// [providers.RetryableError] carrying it. 401, 402 and 403 are [providers.ErrForbidden], 404 (an unknown model,
// or one the account cannot use) is [providers.ErrNotFound], 413 is [providers.ErrTooLarge] and any other 4xx
// is [providers.ErrRejected]; none of them is retried.
//
// A reply is untrusted (invariant 4). Its body is read up to Config.MaxResponseBytes and must be one JSON
// message with a usage block. A reply cut off at max_tokens, refused (stop_reason refusal), ended for a
// reason other than end_turn or tool_use, with a content block of a type the adapter does not know, with no
// text and no tool call, with text that is not one JSON object when a response schema was sent, or with a
// tool call that names a tool the request did not offer, has a malformed ID or input that is not a JSON
// object, is [providers.ErrMalformedOutput]. Such a reply was billed, so its Usage and Model are returned with
// the error.
//
// Usage: InputTokens is the whole prompt, input_tokens plus cache_creation_input_tokens plus
// cache_read_input_tokens (the API reports the three apart); CachedInputTokens is cache_read_input_tokens.
// Tokens written to the cache count as plain input, although the API bills them above the input price (1.25
// times for the five-minute cache), so a cost estimate from these numbers is slightly low. As in the Foundry
// adapter, a 200 whose usage cannot be read sets Completion.UsageUnknown.
//
// Errors carry fixed text, the status code and their class; never a URL, a header, the key, its file name,
// the prompt or any text of the reply.
package anthropic
