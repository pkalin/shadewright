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

// Package incidentcontext is the context builder of the analyzer (design section 3.3, ADR-0023 decision 4):
// for one Incident it assembles, deterministically, the bounded and redacted context the model gets on its
// first call. It is named incidentcontext, not context, so that it never shadows the standard library's
// context package, which it and every caller import.
//
// The context has six sections, in this order, each with its own byte bound ([Limits]):
//
//   - summary: the Incident itself (subject, reason, symptom, counts and times);
//   - object status and owners: the target workload's status and conditions, the memory limits of its pod
//     template, the owner chain and the status of each of its current pods with that pod's own memory limits,
//     each labelled as the template's or the pod's (during a rollout they differ);
//   - log excerpt: at most two excerpts from a [providers.LogSource], of the containers that look most
//     relevant (a terminated previous instance first, then a waiting or unready container);
//   - metrics: the approved metric templates for the target's pods from a [providers.MetricsSource];
//   - Git: always "not available" for now: the analyzer has no Git identity (invariant 6, #373);
//   - policy bounds (#493): for each catalog action that can change the subject, the bounds of the
//     RemediationPolicy configured for it (Config.Policies), read by name and checked against the subject by
//     the policy package's own checks ([policy.BoundsFor]), so what the model is shown is what the executor's
//     policy step reads; or why any proposal would be Denied. It is operator configuration, not cluster
//     evidence: written from typed fields only (names, quantities, numbers), with no quotable values, and a
//     failed read is labelled but is no escalation signal. For a memory limit it also gives, per container
//     of the pod template, the desired values the policy allows from its current limit
//     ([policy.AllowedMemory]). The policy still decides (invariant 1).
//
// Each section is one or more [Block]s, tagged with a [Provenance] for the delimited blocks of ADR-0023
// decision 3. The text of a block is cluster data, untrusted (invariant 4): the caller puts it inside a
// block that the prompt says is data, never instructions.
//
// Pods: the target's pods are resolved from the Incident subject's kind and UID through controller owner
// references in the subject's namespace (Deployment to ReplicaSet to Pod, StatefulSet to Pod, ReplicaSet to
// Pod, or the Pod itself), with the analyzer's existing reads: a get of each workload from the API server and
// a list of the namespace's pods from the cache. Every hop must exist with the referenced UID and select the
// object below it by its labels. Names never come from a model or from text. Live reads give only the
// current pods: the pod set is labelled [LabelPodsUncertain] when one of them was created inside the window
// of the analysis or the workload rolled within it (pods of more than one ReplicaSet, a ReplicaSet created in
// the window, or a StatefulSet whose current and update revisions differ). Historical membership (for example
// kube_pod_owner) is not read.
//
// Redaction and cuts: every piece of text goes through the [Redactor] before anything is cut. Each value from
// the cluster is redacted on its own and the summary, status and metrics sections are redacted again as a
// whole once assembled; a log block's header and its excerpt are redacted whole, the excerpt again whatever the
// source did. So no cut can split a secret before the redactor sees it or drop the key line that marks the next
// lines as a secret. After that, the builder only removes whole lines of redacted text, which cannot reveal
// anything that was not already there; the fixed labels and availability values are code, never cluster data,
// and the error text of a failed source never enters a block. A value longer than 1 KiB once redacted is left
// out whole, never cut. Redaction never makes text longer (package redact), so a bound checked before a cut
// holds after it.
//
// What a cut keeps: the summary, status and metrics sections are written most relevant first (the target,
// then the pods with the most trouble, then the metric templates that explain a crash) and keep their head;
// a log excerpt keeps its tail, the newest lines, which for a previous instance are the lines just before its
// termination.
//
// Escalation (design 3.3, ADR-0023 decision 6): [Context.Escalate] is true only when the builder cut the whole
// context to [Limits.Total] ([Context.Truncated]) or a source that is configured failed
// ([Context.FailedSources]). A section cut to its own bound, a log excerpt the source marked truncated, with
// its end missing or uncertain about its instance, a partial metrics answer and an uncertain or capped pod set
// are labels on the evidence only, because whoever runs workloads in a watched namespace controls them. A
// source that is not configured is "not available", which is not a failure. When the context passed to
// [Builder.Build] is cancelled or past its deadline, Build returns an error instead of a context: an
// abandoned build is not a failed source. A source call that runs out of its own timeout while the build's
// context is still alive is a failed source.
//
// Namespaces: the builder is given the watched namespaces ([Config.Namespaces]), the same set as the cache and
// both sources. An Incident whose subject is in any other namespace gets status, logs and metrics "not
// available", refused before any read or source call; an empty set refuses every subject.
//
// The analyzer creates the builder with the Prometheus and pod logs adapters (internal/analyzer NewSources);
// the analysis loop (internal/analysis) calls it.
package incidentcontext
