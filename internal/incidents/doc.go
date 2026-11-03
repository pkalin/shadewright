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
// within the aggregation window (5 minutes) go into one status write after it. Each write reads the
// Incident fresh and retries on conflict, a write whose result was lost is recognised by its occurrence
// totals and not added twice, and a failed write keeps its batch for the next try, so counts are neither
// lost nor doubled. A clean stop writes what is pending.
//
// The collector asks how much of an occurrence was counted ([Controller.Handled]); the answer comes from
// memory, loaded from the Incidents at start and kept in step with every write, and waits until that load
// is done. The memory is bounded by the cap on Incidents times the records each keeps.
//
// At the cap on Incidents (500 by default, --max-incidents), an observation of a new fingerprint is not
// stored: it is counted in shadewright_incident_observations_refused_total, logged once per fingerprint,
// and reported in a Warning event about the analyzer's Pod at most once per EventInterval (10 minutes by
// default). Existing Incidents keep their records, so nothing is counted again. A batch the API server
// refuses as invalid is dropped, not retried.
package incidents
