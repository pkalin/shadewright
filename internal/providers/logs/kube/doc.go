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

// Package kube is the Kubernetes pod logs adapter of [providers.LogSource] (ADR-0013, design sections 2.3 and
// 4). It reads the log of one container of one pod through the pods/log subresource of the API server, with
// client-go's pods client ([PodsGetter], which CoreV1() of a clientset, real or fake, satisfies). It is built
// but not wired: the analyzer does not create it yet, and has no grant on pods/log.
//
// Scope: a query may name only a namespace in [Config.Namespaces] (the analyzer's watched namespaces); any
// other is refused with [ErrInvalidQuery] before a request, an empty set refuses every query, and the error
// never repeats the namespace. RBAC would refuse such a read too once the analyzer has a grant, but the
// adapter does not rely on the identity it is given. The pod must be a DNS-1123 subdomain and the container,
// required (one container per call), a DNS-1123 label.
//
// Bounds, fixed in code so that no configuration can widen them: a query reads back its Since ([DefaultSince]
// when it sets none, at most [MaxSince], rounded up to whole seconds) and at most the last [TailLines] lines,
// and asks the server for at most [MaxReadBytes] (LimitBytes). The adapter reads at most [MaxReadBytes] of
// the stream whatever the server sends, keeping only the last [providers.LogQuery.MaxBytes] in memory
// ([DefaultBytes] when zero, at most [MaxBytes]). The excerpt is the newest part of what was read: when bytes
// were dropped from its front, its first line, which may be partial, is dropped too, and when the read
// stopped at [MaxReadBytes], the partial last line, so a cut never leaves half of a secret for the redactor
// to miss; Truncated is then set. Previous selects the previous, terminated instance of the container. The
// stream is never followed, and timestamps are not asked for. Each query has a deadline ([Config.Timeout] or
// the caller's, whichever is sooner).
//
// The log is untrusted input (invariant 4). Invalid UTF-8 is replaced, and the text is redacted with
// internal/redact ([Config.Redactor], or the built-in rules) before it leaves the adapter; redaction never
// makes the text longer, so the excerpt stays within its byte bound. Redaction is best effort (see package
// redact); a multi-line secret, such as a PEM key, that a cut splits at a line boundary loses its header, and
// its remaining lines are redacted only if they look random enough. The adapter logs nothing.
//
// Errors carry fixed text and the status code only, never the namespace, pod or container name, log text or
// the API server's message. The adapter does not retry (client-go's own retries are turned off): 404 is
// [providers.ErrNotFound]; 400, which the API server answers for a container the pod does not have, a
// previous instance that does not exist or a container that has not started, and any other 4xx are
// [providers.ErrRejected]; 401 and 403 are [providers.ErrForbidden]; 408, 429, 5xx, a network error and a
// stream cut short are a [providers.RetryableError] (with the delay the server asked for, capped at 10
// minutes). A canceled or expired context is returned so that errors.Is matches [context.Canceled] or
// [context.DeadlineExceeded].
package kube
