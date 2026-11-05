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

// Package prometheus is the Prometheus HTTP API adapter of [providers.MetricsSource] (ADR-0013, ADR-0017,
// design sections 2.3 and 4). It speaks the plain Prometheus query API; Thanos, Mimir, Grafana Cloud and
// Azure Monitor managed Prometheus serve the same API, but the adapter is untested against them.
//
// The adapter runs only the approved query templates of this package, listed by [Templates]. There is no way
// to send PromQL text: neither the model, the cluster nor the configuration can add or change a query. A
// template takes named parameters only, each a Kubernetes name: the namespace must be a DNS-1123 label and
// the workload a DNS-1123 subdomain, and anything else, an unknown or a missing parameter included, is
// refused with [ErrInvalidQuery] before a request is made. Prometheus applies no authorization of its own, so
// the adapter does: a query may name only a namespace in [Config.Namespaces] (the analyzer's watched
// namespaces), any other is refused with [ErrInvalidQuery] before a request, and an empty set refuses every
// query; the error never repeats the namespace. A template that selects pods by name also refuses
// a workload name longer than [MaxPodWorkloadName], which pod names cut. Values are inserted as quoted PromQL
// strings, and into regular expressions only after their metacharacters are escaped.
//
// Limits, checked before the request on the times as sent (whole milliseconds): the time range of a range
// query is at most [MaxRange] and its step at least [MinStep], so it asks for at most 361 points per series;
// an instant query has no start time; no time may lie more than [MaxLookback] in the past or more than
// [MaxClockSkew] in the future.
//
// The response is untrusted input (invariant 4). It is decoded as a stream and refused with
// [providers.ErrTooLarge] as soon as it holds more than [MaxSeries] series, [MaxPointsPerSeries] points in a
// series, [MaxPoints] points in all or [MaxBodyBytes] bytes, so an oversized answer is never held whole. It
// is refused with [ErrInvalidResponse] when it is not application/json, when it is not the result type the
// template returns, when a series carries a label the template does not produce or a label value that is not
// a plain name (letters, digits, '.', '_', ':' and '-', at most 253 bytes), when a sample lies more than a
// second outside the time asked for or its times are not strictly increasing, or when it is malformed in any
// other way. A successful response with a warning (a store behind the server that did not answer, for example)
// is returned with [providers.MetricsResult] Partial set; so is one whose warnings field has another shape than
// a list of strings. Infos, and warnings that start with "PromQL info: " (where releases before 2.50 put them),
// describe the query, not missing data, and do not make a result partial. The text of warnings, infos and
// errors and unknown fields of the response are never returned or logged. Label values still need redaction
// (internal/redact) before a model sees them; the adapter does not redact.
//
// Transport: TLS and authentication come from the *http.Client given in [Config]; redirects are never
// followed. Each query has a deadline ([Config.Timeout] or the caller's, whichever is sooner); the server is
// sent an evaluation timeout a tenth below the time left. The adapter does not retry: a network error, a body
// cut short by the connection, 408, 429 and 5xx are a [providers.RetryableError] (with the delay of a
// Retry-After header in seconds or as an HTTP date, if any, capped at 10 minutes) and the caller decides. A
// 503 whose JSON error envelope has errorType "timeout" is the exception: the server ran out of its
// evaluation timeout, and it is treated as final ([providers.ErrRejected]) since an evaluation that hit the
// timeout usually would again (time in the server's queue counts towards it too). 401 and 403 are
// [providers.ErrForbidden], any other status (3xx included) [providers.ErrRejected]. Error bodies are not
// read, except the first 4 KiB of a 503 for its errorType. Errors carry fixed text and the status code only, never a parameter
// value, an unknown parameter name, the URL, a header or response text; a canceled or expired context is
// returned so that errors.Is matches [context.Canceled] or [context.DeadlineExceeded].
package prometheus
