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
//   - summary: the Incident itself (subject, reason, symptom, counts and times) and, for a cause Incident, its
//     absorbed tally: the consequence reasons of the fixed table, their counts and last seen times (ADR-0039
//     point 18), never text from the cluster;
//   - object status and owners: the target workload's status and conditions, the memory limits of its pod
//     template, the owner chain and the status of each of its current pods with that pod's own memory limits,
//     each labelled as the template's or the pod's (during a rollout they differ); for a KEDA ScaledObject
//     subject, the allow-listed fields of that one object, read with one get (ADR-0036), and the object itself
//     as [Context.ScaledObject] for the analysis loop to decide from;
//   - log excerpt: excerpts from a [providers.LogSource] of at most two containers, those that look most
//     relevant (a terminated instance or a container waiting in CrashLoopBackOff first, then another waiting or
//     unready container), at most two excerpts (one per instance) within each container's equal share of the
//     bound; an exited container with a previous instance reserves room from its share for that previous
//     instance, and a total context cut tries to keep the final lines of a previous instance ahead of earlier
//     log noise; every container's first read comes before any second read, each with one call timeout, and only a
//     first read that failed makes the section a failed source; the second reads run in what is left of a time
//     budget of two call timeouts for the section, and any second read skipped when time is up is still noted
//     as "could not fetch". That budget is the worst case; an exited container whose current instance hangs,
//     and a fallback read that hangs, take longer than one read of the previous instance (design 3.3, "Known
//     limits of the log section");
//   - metrics: the approved metric templates for the target's pods from a [providers.MetricsSource];
//   - Git: always "not available" for now: the analyzer has no Git identity (invariant 6, #373);
//   - policy bounds (#493): for each catalog action that can change the subject, the bounds of the
//     RemediationPolicy configured for it (Config.Policies), read by name and checked against the subject by
//     the policy package's own checks ([policy.BoundsFor]), so what the model is shown is what the executor's
//     policy step reads; or why any proposal would be Denied. It is operator configuration, not cluster
//     evidence: written from typed fields only (names, quantities, numbers), with no quotable values. Bounds
//     are shown only for a subject the builder resolved (or of a kind it does not read, said in the block).
//     A failed read (not NotFound) is logged, labelled and a failed source (an escalation signal); a missing
//     policy or settings object is said as the Denied it leads to. For a memory limit it also gives, per
//     container of the pod template, the desired values the policy allows from its current limit
//     ([policy.AllowedMemory]), or that the container has no limit. The policy still decides (invariant 1).
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
// Redaction and cuts: every piece of text goes through the [Redactor] before anything is cut. The summary,
// status, metrics and policy sections are written as pieces, the text the builder writes and each value from the
// cluster as it was read, and decided once on those pieces (redact.Redactor.PiecesWithin, #845); a redactor
// that cannot decide on pieces redacts each value on its own and the section again once assembled. A log
// block's header and its excerpt are redacted whole, the excerpt again whatever the source did. So no cut can
// split a secret before the redactor sees it or drop the key line that marks the next lines as a secret. When
// redaction withholds a section, a log header or a log excerpt whole (its work budget ran out, #873), the
// block says "could not fetch: withheld by redaction" (a log block under its title), with [LabelWithheld] and
// no values, and its section counts as a failed source, which escalates. [FitLog], which bounds a rendered log
// text again, does not redact: the text was redacted when it was rendered. After that, the builder removes
// whole lines of redacted text, with two exceptions that still operate only on already-redacted text: the
// summary shortens long values with a byte-count note instead of dropping lines, and when a log excerpt's
// final non-blank line alone is longer than its remaining budget (even when followed by trailing blank lines)
// the builder keeps the longest valid UTF-8 suffix of that already-redacted line on a rune boundary and
// outside any redaction marker (marked cut to the section or context bound) instead of leaving an empty or
// whitespace-only body. Every cut of a log excerpt (RenderLog with and without refit, [FitLog], the room a
// second read takes from the first excerpt and the total cut) calls the one cut of internal/logcut, which the
// log source uses too, and the rules here live only there (decision of 2026-11-28 on #1055); the total cut
// measures each candidate's rendered size exactly ([renderedSize]). A non-blank last line never yields an empty excerpt: when that cut would leave nothing
// (a run of "*" longer than the budget reaching the end of the line, or a tail of only whitespace), the
// builder keeps the tail of the line within the budget, cutting inside a run of more than three "*" if it
// must (the redactor writes at most three, and every part of such a run is only "*"), never inside another
// marker, and only when the text after the run does not fit without it. The rule holds from a cap of 43 bytes
// (the longest marker shape) for the cut of an excerpt and from 46 bytes after the header for the cut of the
// rendered context (plus the newline rendering adds and two bytes for "contextblock" twice in a marker kind);
// below that a last line that ends in a marker may leave the header alone. Real configuration can reach below
// either floor: RemediationSettings maxContextBytes (4 KiB to 48 KiB) can leave fewer than 46 bytes after a
// log header during the total cut, and [Limits.Validate] accepts section bounds down to 1 KiB, where long
// headers or reclaiming room for a failed second read can leave fewer than 43 bytes for the excerpt. Neither
// can reveal anything that was not already there; the fixed labels and
// availability values are code, never cluster data, and the error text of a failed source never enters a
// block. A value longer than 1 KiB once redacted is left out whole, never cut. Redaction never makes text
// longer (package redact), so a bound checked before a cut holds after it.
//
// What a cut keeps: the summary, status and metrics sections are written most relevant first (the target,
// then the pods with the most trouble, then the metric templates that explain a crash) and keep their head;
// a log excerpt keeps its tail, the newest lines, which for a previous instance are the lines just before its
// termination.
//
// Escalation (design 3.3, ADR-0023 decision 6): [Context.Escalate] is true when the context or appended text
// was cut to the total bound ([Context.Truncated]) or a source that is configured failed
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
