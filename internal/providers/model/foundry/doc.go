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

// Package foundry is the Microsoft Foundry adapter of [providers.ModelProvider] (ADR-0013, design section
// 11.1). It calls one deployment of an OpenAI-format model through the chat completions API, with structured
// output (a JSON schema, strict) and function tools. It imports the standard library and this module only.
//
// A [Provider] is bound at construction to one endpoint, deployment and API version, the fields of a
// ModelDeployment in RemediationSettings. With API version v1 it posts to <endpoint>/openai/v1/chat/completions
// (or <endpoint>/chat/completions when the endpoint already ends in /openai/v1) and names the deployment in the
// body; with a dated version it posts to <endpoint>/openai/deployments/<deployment>/chat/completions. It
// follows no redirect. Which hosts an endpoint may name is not checked here: that guard (an allow-list of
// suffixes, no private addresses) belongs to the analyzer (#76).
//
// Authentication is an Entra ID access token only, never a key: [WorkloadIdentity] exchanges the pod's
// projected service account token for one (the AZURE_* variables of the workload identity webhook), with the
// standard library. Every token, from any [TokenSource], is checked before it is sent: it must be a JWT whose
// audience is Cognitive Services, so a token minted for another resource never reaches the endpoint.
//
// Every call has a deadline (Config.Timeout unless the caller's context has a shorter one) and each attempt
// its own (Config.AttemptTimeout). Network failures, timeouts of one attempt, 408, 429 and 5xx are retried with
// exponential backoff and jitter, up to Config.MaxAttempts attempts; a delay the service asks for
// (retry-after-ms or Retry-After) is waited for when it is at most Config.MaxDelay, and a longer one is
// returned as a [providers.RetryableError] carrying it. There is no fallback to another deployment (#80):
// after the last attempt the error is retryable and the caller hands the incident off. 401 and 403 are
// [providers.ErrForbidden], 404 (an unknown or removed deployment) is [providers.ErrNotFound], 413 is
// [providers.ErrTooLarge] and any other 4xx is [providers.ErrRejected]; none of them is retried.
//
// A reply is untrusted (invariant 4). Its body is read up to Config.MaxResponseBytes, must be one JSON value
// of the chat completion shape with exactly one choice and a usage block, and is then checked against the
// request: a reply cut off at the output limit, filtered, refused, with no content and no tool call, with
// content that is not one JSON object when a response schema was sent, or with a tool call that names a tool
// the request did not offer, has a malformed ID or arguments that are not a JSON object, is
// [providers.ErrMalformedOutput]. Such a reply was billed, so its Usage and Model are returned with the error.
// The adapter does not check the content against the schema itself: the caller decodes it into its own
// types and validates it there.
//
// Token usage (input, cached input and output tokens) is returned with each reply and reported per call to
// Config.OnCall, with the attempts and the outcome, for cost accounting (design 12.1).
//
// Errors carry fixed text, the status code and their class; never a URL, a header, the endpoint, a token, the
// prompt or any text of the reply. Nothing is logged.
package foundry
