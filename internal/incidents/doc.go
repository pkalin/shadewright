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
// Writes are batched: the first observation of an Incident is written at once, and the ones that follow
// within the aggregation window (5 minutes) go into one status write after it. Writes run in their own
// goroutine, so a slow API server does not hold up the observations. Each write reads the Incident fresh
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
// from due to not due or back. The analyses used today are read from the budget ConfigMap [BudgetName] in
// the operator namespace ([UsedToday]), so neither a restart nor deleting Incidents frees the daily budget
// (RemediationSettings maxAnalysesPerDay), and no more Incidents are due at once than the budget has left.
// Without the RemediationSettings object or the ConfigMap, or with a budget it cannot trust, nothing is due;
// the controller never creates the ConfigMap. Each pass has a deadline shorter than the interval
// (PassTimeout, 8 s), and the budget read within it one of its own (ReadTimeout, 5 s); a read that runs into
// it counts as a read that failed. The mark runs nothing: the analysis reads it and takes a unit of the
// budget with [ReserveAnalysis] before it calls the model.
package incidents
