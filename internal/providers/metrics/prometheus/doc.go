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
// design sections 2.3 and 4). It works with any server that speaks the Prometheus query API (Prometheus,
// Thanos, Mimir, Grafana Cloud, Azure Monitor managed Prometheus).
//
// The adapter runs only the approved query templates of this package, listed by [Templates]. There is no way
// to send PromQL text: neither the model, the cluster nor the configuration can add or change a query. A
// template takes named parameters only, each a Kubernetes name: the namespace must be a DNS-1123 label and
// the workload a DNS-1123 subdomain, and anything else, an unknown or a missing parameter included, is
// refused with [ErrInvalidQuery] before a request is made. Values are inserted as quoted PromQL strings, and
// into regular expressions only after their metacharacters are escaped.
//
// Limits, checked before the request: the time range of a range query is at most [MaxRange], its step at
// least [MinStep], and the points it asks for per series at most [MaxPointsPerSeries]; no time may lie more
// than [MaxLookback] in the past or more than [MaxClockSkew] in the future.
//
// The response is untrusted input (invariant 4). It is decoded as a stream and refused with
// [providers.ErrTooLarge] as soon as it holds more than [MaxSeries] series, [MaxPointsPerSeries] points in a
// series, [MaxPoints] points in all or [MaxBodyBytes] bytes, so an oversized answer is never held whole. It
// is refused with [ErrInvalidResponse] when it is not the result type the template returns, when a series
// carries a label the template does not produce or a label value that is not a plain name (letters, digits,
// '.', '_', ':' and '-', at most 253 bytes), when a sample lies outside the time asked for or out of order,
// or when it is malformed in any other way. Warnings, error text and unknown fields of the response are
// never returned or logged. Label values still need redaction (internal/redact) before a model sees them;
// the adapter does not redact.
//
// Transport: TLS and authentication come from the *http.Client given in [Config]; redirects are never
// followed. Each query has a deadline ([Config.Timeout], also sent to the server as its evaluation timeout).
// The adapter does not retry: a network error, 429 or 5xx is a [providers.RetryableError] (with the delay of
// a Retry-After header, if any) and the caller decides. 401 and 403 are [providers.ErrForbidden], other 4xx
// [providers.ErrRejected]; error bodies are not read. Errors carry fixed text and the status code only, never
// the URL, a parameter, a header or response text; a canceled or expired context is returned so that
// errors.Is matches [context.Canceled] or [context.DeadlineExceeded].
package prometheus
