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
// to send PromQL text: neither the model, the cluster nor the configuration can add or change a query (the
// configuration can only scope every selector to one cluster, see Source profile below). A
// template takes named parameters only, each a Kubernetes name: the namespace must be a DNS-1123 label and
// the workload, which only deployment_replicas_unavailable takes, a DNS-1123 subdomain, and anything else, an
// unknown or a missing parameter included, is refused with [ErrInvalidQuery] before a request is made.
// Prometheus applies no authorization of its own, so the adapter does: a query may name only a namespace in
// [Config.Namespaces] (the analyzer's watched namespaces), any other is refused with [ErrInvalidQuery] before
// a request, and an empty set refuses every query; the error never repeats the namespace.
//
// The pod templates ([TemplateInfo.Pods]) select exactly the pods named by [providers.MetricsQuery.Pods],
// never pods whose names merely start like the target's: 1 to [MaxPods] distinct names (duplicates are
// dropped first), each a DNS-1123 subdomain of at most [MaxPodName] characters, or the query is refused with
// [ErrInvalidQuery] before a request, with an error that gives the position of a refused name, never the
// name; any other template refuses a pod set. A pod template returns a series per container, so a set within
// [MaxPods] can still give more than [MaxSeries] series: that answer is refused with [providers.ErrTooLarge],
// and the caller splits the set.
//
// The adapter has no Kubernetes client and does not decide which pods belong to the target, nor can it check
// that they are in the namespace the query names. Its caller resolves them in that same namespace from the
// target's kind and UID through controller owner references (Deployment to ReplicaSet to Pod, StatefulSet to
// Pod), never from a name prefix or model output. The selector matches the namespace and the pods together,
// so a pod of the same name in another namespace is never selected. The caller can read only the current
// pods of the target (deleted pods are gone), so it marks the attribution uncertain when any of them started
// inside the time asked for or the workload rolled within it. The analyzer cannot read DaemonSets, so their
// pods are not attributed unless a later change adds that grant.
//
// The names are sorted and joined as escaped alternatives of one anchored regular expression; a series of
// any other pod in the response, or one without a pod label, is refused with [ErrInvalidResponse]. The
// longest query, every name and the cluster setting at their longest, keeps the request URL within 32 KiB.
// Values, the cluster setting included, are inserted as quoted PromQL strings, and into regular expressions
// only after their metacharacters are escaped.
//
// Source profile. The supported source holds the series of one cluster: a Prometheus dedicated to the cluster,
// such as the one of the lab (ADR-0017). Every template keeps the cluster label apart, and the adapter drops it
// from the result; a response with two series of one container that differ only in cluster (two clusters with
// the same namespace, pod and container, or a source that labels only some of its series) is refused with
// [ErrInvalidResponse] rather than mixed, and so is a cluster label value that is not a plain name. A shared
// backend that holds several clusters therefore needs [Config.Cluster], which adds cluster="<value>", quoted,
// to every selector of every template. The label must be stored on the series (by the scrape or by remote
// write), not an external label the queried server adds only on its way out; a mistyped value or a label the
// series do not carry leaves every result empty, so [Source.CheckCluster] checks, with one fixed query that is not a
// template and returns no label, that kube-state-metrics and cAdvisor both hold series of it
// ([ErrClusterNotFound] when not). Only the administrator sets it; neither a query, the model
// nor a custom resource can, and [New] refuses a value that is not 1 to [MaxClusterValue] letters, digits,
// '.', '_', ':', '/' or '-' (starting and ending with a letter or digit), with [providers.ErrInvalidConfig]
// and an error that does not repeat it.
//
// The namespace check holds only if the namespace, pod, container and cluster labels of the cAdvisor and
// kube-state-metrics series are the scrape's: a job that scrapes tenant pods must not honour their labels
// (honor_labels: false), or must drop the container_* and kube_* metric names from what it keeps. Metric values
// stay evidence a tenant can influence.
//
// Within that profile, a measurement counts once even when several series hold it. Two scrape jobs that collect
// the same cAdvisor or kube-state-metrics series (or two kube-state-metrics replicas) differ only in job or
// instance, and every template reduces the series of one container (namespace, pod, container, cluster) before
// it aggregates: gauges and states take the max over the container, and the CPU templates take the max over
// each container instance (the cAdvisor id label) and then sum the instances, with the series without an id
// (the kubelet's /metrics/resource) reduced apart and the larger of the two estimates kept. A restarted
// container therefore gives the larger working set of its old and new instance while both have series, never
// their sum, and a CPU rate over the 5-minute window that counts each instance's part of it once. The comments
// above the templates and on cpuRate in templates.go give the reason per metric.
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
// a list of strings. Infos, and warnings that start with "PromQL info: " (where older Prometheus releases put them),
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
