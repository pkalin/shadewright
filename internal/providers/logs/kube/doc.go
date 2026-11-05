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
// client-go's pods client, and reads the pod itself through the REST client to tie the excerpt to an instance
// ([PodsGetter]: CoreV1() of a real clientset; [New] refuses the fake clientset's, which has no REST client).
// The pod read needs get on pods, which the analyzer's watch role already grants, bound only in the watched
// namespaces. The analyzer creates it unless --pod-logs=false (internal/analyzer NewSources) and gives it to
// the context builder; get on pods/log is in the analyzer's watch role, bound only in the watched namespaces.
//
// Instance: a log request names a pod and a container by name only. It cannot ask for a pod UID or a
// container ID, and Previous is relative to the instance running when the kubelet answers. So the adapter
// reads the pod before the first and after the last logs request, and records in the excerpt the pod UID, the
// container ID of the instance asked for (the current container, or for Previous the last terminated one)
// and the restart count from the check before, and the time the reading ended ([providers.LogExcerpt]). Pod
// UIDs and container IDs are never reused, so equal checks mean that no other instance answered either logs
// request, the second one included; the restart count is compared as an extra signal only, since it can go
// back to 0 after a node restart. The excerpt is marked InstanceUncertain when the checks differ in any of
// the three, when the second check fails (the pod is gone, the read is refused or fails, or the deadline
// passes: the logs already read are returned), or when the status names no instance for the container or an
// ID longer than [providers.MaxLogInstanceIDLength] bytes or with other characters than letters, digits and
// ._:/+- (such an ID is recorded empty). For the current container it is also marked when the status does not
// say which instance the kubelet serves: while the container waits, the kubelet serves the last terminated
// instance, and a terminated state without a container ID names none. What a log request still cannot guarantee: the
// instance is inferred from the pod's status, not proved by the answer, and the status is the kubelet's
// report, which can lag the runtime, so a restart the status does not show yet at the second check is not
// seen; the kubelet keeps the log of one previous instance only, and only while it keeps that container. An
// uncertain excerpt is a label on the evidence, not a reason to escalate (design section 3.3): restarts and
// recreation are controlled by whoever runs workloads in a watched namespace. The metadata stays with the
// excerpt, never in Incident status.
//
// Scope and checks, before any request: a query may name only a namespace in [Config.Namespaces] (the
// analyzer's watched namespaces); any other is refused with [ErrInvalidQuery], an empty set refuses every
// query, and the error never repeats the namespace. RBAC refuses such a read too, but the adapter does not
// rely on the identity it is given. The pod must be a DNS-1123 subdomain and
// the container, required (one container per call), a DNS-1123 label. [New] refuses a nil client or REST client, a namespace
// that is not a DNS-1123 label, more than [MaxNamespaces] namespaces and a timeout above [MaxTimeout].
//
// Bounds, fixed in code so that no configuration can widen them. A query of the current container reads back
// its Since ([DefaultSince] when it sets none); a query of the previous, terminated instance reads back its
// Since only when it sets one, and otherwise has no time limit, since that instance stopped at some time in
// the past. Since is at most [MaxSince] and rounded up to whole seconds. The first request asks for the last
// [TailLines] lines and for at most [MaxReadBytes] plus one byte (LimitBytes), and the adapter reads no more
// than that whatever the server sends; the extra byte tells a log of exactly [MaxReadBytes] from a longer
// one. The API server applies LimitBytes from the start of the lines it selected, so when they hold more than
// [MaxReadBytes], the newest lines are missing: the adapter then sends one more request, for fewer lines (the
// lines that would fit at the mean length of the first read, times 0.75). If that read is cut too, the
// excerpt is what it holds and EndMissing is set. Each read holds at most [MaxReadBytes] plus one byte in
// memory. Lines the server leaves out because of TailLines or Since are not reported.
//
// The excerpt is the newest [providers.LogQuery.MaxBytes] of the read ([DefaultBytes] when zero, at most
// [MaxBytes]), in whole lines: when older bytes are dropped, the first line is dropped too unless it starts
// right after a newline, and Truncated is set; when the read was cut, its last, partial line is dropped. So a
// cut never leaves half of a secret for the redactor to miss. The stream is never followed, and timestamps
// are not asked for. Each query has a deadline ([Config.Timeout] or the caller's, whichever is sooner); a
// stream that makes no progress for 100 reads in a row fails as cut short.
//
// The log is untrusted input (invariant 4). Invalid UTF-8 is replaced, and the text is redacted with
// internal/redact ([Config.Redactor], or the built-in rules) before it leaves the adapter; redaction never
// makes the text longer, so the excerpt stays within its byte bound. Redaction is best effort (see package
// redact); a multi-line secret, such as a PEM key, that a cut splits at a line boundary loses its header, and
// its remaining lines are redacted only if they look random enough. The volume of logs is controlled by
// whoever can write logs in a watched namespace, so Truncated and EndMissing say nothing about the incident.
// The adapter logs nothing.
//
// Errors carry fixed text and the status code only, never the namespace, pod or container name, log text or
// the API server's message. The adapter retries no request, and client-go's own retries are turned off for
// the logs requests and the pod reads alike, so neither a status with Retry-After nor a reset connection is
// sent again. A failed first pod read, and a failed logs request, are classified alike: 404 is
// [providers.ErrNotFound]; 401 and 403 are [providers.ErrForbidden]; 408, 429, 5xx, a network error, a stream
// cut short and the expiry of [Config.Timeout] are a [providers.RetryableError] (with the delay the server
// asked for, capped at 10 minutes); any other 4xx, such as 400, is [providers.ErrRejected]. The error names
// which request failed ("pod read" or "logs request"). A failed second pod read is not an error: it marks the
// excerpt uncertain. When the caller's context is done, its error is returned, so that errors.Is matches
// [context.Canceled] or [context.DeadlineExceeded].
package kube
