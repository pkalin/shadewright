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

// Package incidents is the Incident controller of the analyzer (design 3.2, ADR-0007): it turns the
// collector's observations into one durable Incident per fingerprint, in the operator namespace.
//
// Each fingerprint names one Incident ([Name]), so a restart finds it. The status holds the count (which
// saturates at 1,000,000), first and last seen, and for the newest 32 occurrences the highest total counted
// and when the occurrence began, newest first; the newest begin time among the records dropped is kept as
// occurrencesDroppedThrough. Nothing from the cluster goes into the Incident beyond the subject, the reason
// and the normalized, redacted symptom the collector computed.
//
// A Resolved Incident is never analyzed again (#251). An observation of its fingerprint seen more than
// [RecurrenceWindow] (24 hours) after its resolvedTime, with nothing of it unwritten, opens the next
// occurrence instead, through the same gate as a new fingerprint: a new Incident named "incident-<hex>-<n>"
// (remediationv1alpha1.IncidentName) whose spec links it to the one before and whose status starts empty,
// under the cap like a new fingerprint. Any other observation written to it moves it to Escalated in the same
// status update (condition Recurred), unless it was seen no later than resolvedTime; an Incident the closure
// rules resolved by quiet time before any analysis, or one folded into its cause's Incident (FoldedIntoCause,
// analysed before or not), goes back to Open instead (#466, ADR-0039 point 24). Which Incidents are
// Resolved is known from load, every trigger pass and each write; the latest occurrence of each fingerprint
// from the names load lists, so a restart writes there. A later occurrence deleted while running is never
// created again from memory: its batches are dropped and the newest that exists becomes the latest.
//
// A symptom opens its Incident only when it recurs or is immediate (#267). An observation that proves
// recurrence on its own (its occurrence's latest repeat came at least [RecurAfter] after it
// began; a total of 2 or more alone is not, as a burst within seconds makes a series too) opens its Incident at once, as does one the collector marks Immediate (a
// container OOMKilled in pod status, or a symptom state of a module's source). Any other observation of a
// fingerprint without an Incident is held in memory as a first sighting and opens its Incident when it is
// seen again at least [RecurAfter] (2 minutes) and at most [RecurWithin] (15 minutes) after the first
// sighting; a sighting before 2 minutes keeps waiting, one after 15 minutes starts over, and a symptom seen
// once opens nothing. Whatever opens the Incident counts every sighting held for it first. A fingerprint
// refused at the cap on Incidents is refused again as each observation arrives. First sightings are bounded
// (Config.MaxFirstSightings, 4,096, oldest forgotten first, at most 4 occurrences each, copies of one
// merged), answered by Handled, forgotten after 15 minutes and on a restart, and counted in the gauge
// shadewright_incident_first_sightings_held.
//
// A consequence (a kubelet BackOff or Unhealthy on a named container) whose pod status shows a cause is
// counted on the cause's Incident as evidence when the controller's own list has that Incident with
// status.cause for the same subject UID (ADR-0039, #791): in status.absorbed and status.absorbedOccurrences,
// apart from the Incident's own count, lastSeen and occurrences, so it never makes the cause due, restarts no
// cooldown, does not keep it from going quiet and takes no place under the cap. The cause's phase is not
// read. A consequence whose cause has no Incident yet goes through the gate as any observation; one held as
// a first sighting is counted on the cause in full once a later observation of it links there, or once the
// cause's observation is consumed (#820), found through the first sightings' index by cause fingerprint. A pod-status
// cause sets status.cause, and every counted observation adds its container (a DNS label, at most 8) to
// status.containers. The drop mark of a cause's absorbed list answers Handled only for consequences linked to
// that cause's fingerprint; a replay whose cause fingerprint changed or is empty is not recognised and goes
// through the gate.
//
// A consequence Incident that opened before its cause is folded into the cause's Incident (ADR-0039 points
// 12 to 15 and 23, #793): the write that creates a cause Incident, or sets its status.cause, makes its fold
// owed, and the write loop's pass folds (the fast path); every trigger pass, the first at start, also starts
// from the listed consequence Incidents that may still be folded and looks for a cause Incident created at
// most RecurWithin after each (creationTimestamps only, never the controller's clock), so a cause written at
// stop, before a restart, during an outage or before the cache saw its consequence is folded too. It folds
// each kubelet BackOff or Unhealthy Incident of the same subject UID and container, neither Resolved nor
// Escalated and never folded before (status.folded), created at most [RecurWithin] before the cause (by
// creationTimestamp) none of whose requests reached a pull request: Resolved, Lifecycle FoldedIntoCause,
// status.foldedInto and status.folded, in one status write with the resource version it read, decided again
// on a conflict. The closure rules skip a folded Incident. A fold that fails is owed again RetryAfter later
// and tried by the next trigger pass. A fold happens at most once per Incident.
//
// Writes are batched: the observations that open an Incident, or the first one of an existing Incident in
// a process, are written at once, and the ones that follow within the aggregation window (5 minutes) go
// into one status write after it. Writes run in their own goroutine, so a slow API server does not hold
// up the observations. Each write reads the Incident fresh
// and retries on conflict. A write whose answer was lost is kept apart from later observations and
// recognised, occurrence by occurrence, from the totals the status records, so it is not added twice; a
// batch none of whose occurrences can show that is written again (an overcount rather than a loss). A stop,
// or a lost leadership, writes what is pending and what failed within StopTimeout (5 s).
//
// The collector asks how much of an occurrence was counted ([Controller.Handled]); the answer comes from
// memory, loaded from the Incidents at start and kept in step with every write, and waits until that load
// is done. The memory is bounded by the cap on Incidents times the records each keeps, plus the occurrences
// held unwritten, which are capped per Incident, per namespace and in total
// (Config.MaxPendingOccurrencesPerIncident, 256, Config.MaxPendingOccurrencesPerNamespace, 2,500, and
// Config.MaxPendingOccurrences, 10,000), so one noisy namespace cannot fill the total. Each occurrence is
// charged to the namespace it came from (collector.Observation.Source), the namespace RBAC grants event
// creation in, not to its subject's, which a cluster-scoped subject does not have. A new occurrence over a
// cap is not counted, an undercount counted in shadewright_incident_occurrences_overflow_total; its
// observation still moves the Incident's last seen, so the Incident is written all the same.
//
// At the cap on Incidents (500 by default, --max-incidents), an observation of a new fingerprint is not
// stored: it is counted in shadewright_incident_observations_refused_total, logged once per fingerprint,
// and reported in a Warning event about the analyzer's Pod at most once per EventInterval (10 minutes by
// default). Existing Incidents keep their records, so nothing is counted again. A batch the API server
// refuses as invalid, or meant for an Incident whose name holds another fingerprint, is dropped, not
// retried, and counted in shadewright_incident_batches_dropped_total by reason; so is a batch still
// unwritten when a stop's deadline passes.
//
// The controller also marks which Incidents are due for analysis (design 3.2): every TriggerInterval (10 s)
// it decides each one with [Due] and sets the condition AnalysisDue, with a reason, when the decision turns
// from due to not due or back, and over the reason Analyzing the analysis wrote once that run has ended. The
// analyses used today are read from the budget ConfigMap [BudgetName] in the operator namespace
// ([UsedToday]), so neither a restart nor deleting Incidents frees the daily budget (RemediationSettings
// spec.analysis.maxAnalysesPerDay, or the deprecated spec.maxAnalysesPerDay when lower), and no more
// Incidents are due at once than the budget has left. Without the RemediationSettings object, a limit in it,
// or the ConfigMap, or with a budget it cannot trust, nothing is due; the controller never creates the
// ConfigMap. Each pass has a deadline shorter than the interval
// (PassTimeout, 8 s), and the budget read within it one of its own (ReadTimeout, 5 s); a read that runs into
// it counts as a read that failed. The mark runs nothing: the analysis reads it and takes a unit of the
// budget with [ReserveAnalysis] before it calls the model.
package incidents
