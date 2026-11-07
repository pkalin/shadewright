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

// Package analysis runs the bounded analysis of an Incident (design 3.3, ADR-0023): deterministic runbooks
// first, then the context builder, one unit of the daily budget, one structured call to the triage model,
// and on a deterministic escalation signal only, the escalation model with the read tools.
//
// The model's structured output ([Result]) is the only way a proposal is made: there is no proposal tool.
// Its JSON schema is generated from the Go types and the API's enums ([ResponseSchema]), so an action
// outside the closed catalog cannot be expressed, and [DecodeResult] checks the same bounds in Go, then the
// policy's catalog check (invariant 2); an action that fails it is handed off (ActionOutOfBounds) with no
// retry and no feedback, so the model cannot search for what the policy allows. A proposal needs at least
// one quote found verbatim within one data value of the context block or tool result it cites: only a value
// from a source in an available block ([incidentcontext.Block.Data], [tools.Result.Data]), never text code
// wrote around the values, never a block that is not available or could not be fetched, a header, labels, fixed
// lines or boilerplate; and only a successful tool answer about the Incident's subject or one of its
// attributed pods, never one about another object or none. The target is always the Incident's subject and
// the RemediationPolicy the one configured for the action type, never chosen by the model. The previous value
// of a memory limit change is the subject's pod template's, as the context builder read it, never the
// model's ([incidentcontext.Context.Template], #494); without one the proposal is handed off
// (PreviousValueUnknown). The [Controller], not the model, creates the
// RemediationRequest, in the operator namespace with approvalRequired true; the executor's policy check
// decides it (invariant 1). The result has no confidence field, and a reply that adds one is invalid.
//
// The prompt is code: an embedded template in prompts/, with the generated catalog and schema
// ([PromptVersion] hashes them). Context blocks and tool results are delimited, tagged with their
// provenance, and their text cannot close the block (every angle bracket look-alike is escaped). Nothing
// from the cluster, a tool or the model changes the system part, the tools offered or the catalog
// (invariant 4).
//
// Limits: a unit of the daily budget is reserved only once the first model call can be made (its deployment
// configured and its provider built). The triage call has no tools and gets one retry after invalid output,
// with a fixed corrective message that names the broken rule by a code; invalid output twice, or a context
// cut to its total bound or with a source that could not be fetched, escalates. The escalation makes at most
// maxToolSteps tool calls in maxToolSteps + 1 turns, offering the same tools on every turn (the adapter
// refuses a call to a tool not offered as malformed output); the last turn says in a fixed message that no
// calls are left, and a tool call on it is a hand-off (StepLimit). An invalid escalated result is handed off.
// Each tool call is bound to the namespace of the Incident's subject (another namespace is refused before the
// tool reads), get_delivery_status is not offered, and an answer about a pod the context builder did not
// attribute to the subject is labelled so. Each call is charged its reported usage, or its upper bound (the
// input estimate plus maxOutputTokens) when the usage is unknown or the call failed; a call that could take
// the analysis past its token bound, or a deployment past its maxTokensPerDay within this analysis, is not
// made. The analysis has a deadline under the trigger's analysis timeout. A retryable model error that asks
// for a short delay is waited out once, within the deadline and within the bound of 3 + maxToolSteps model
// calls per analysis, which the retry counts toward; otherwise it, ErrNotFound, ErrForbidden and malformed
// escalated output hand the incident off.
//
// The [Controller] claims an Incident the trigger marked due for its current generation (Open, or Analyzing
// taken as lost; never Resolved) by writing the phase Analyzing, lastAnalysisTime and the AnalysisDue
// condition in one status update, and writes the outcome (Proposed or Escalated, the Analyzed condition, the
// request's name and, with a store, the analysis record's ID) in another. A stop leaves the Incident Analyzing,
// which the trigger takes as lost after its analysis timeout.
//
// With a [providers.AnalysisStore] configured (Controller.Store), the controller writes one record of each run
// before the outcome ([Record], ADR-0015): counts, reason, models, prompt version, the request's name and the
// result's summary and missing data, every string from the cluster or the model redacted, never the context, the
// prompt or the raw reply. Its ID derives from the Incident's UID and the run's lastAnalysisTime and goes into
// status.analysisRecords (the newest 20). A record that cannot be written is logged and changes nothing else.
//
// The run is identified by the lastAnalysisTime its claim wrote, and the claim expires the trigger's analysis
// timeout after it (ADR-0025). Just before the create and in the outcome update the controller reads the
// Incident live and drops the result (no request, no outcome write, a log line) unless the claim is still the
// run's and not expired; the analysis deadline, the create and the outcome write fit inside the claim, each
// write cut to the time left on it. Requests carry the Incident's UID as the label [LabelIncidentUID], and a
// claim records the request of the previous run that a lost outcome write left out of
// status.remediationRequests: one created within that run's claim window, with the form an analysis gives it.
//
// Only the models the operator configured answer: a reply must name the model of its role
// (Config.ExpectedModels, ADR-0023 decision 8), and a role without one is not configured. The analyzer binary
// starts the controller only with its --triage-model flag, with the Foundry adapter behind a host allow-list
// ([ErrEndpointNotAllowed]).
//
// With a tracer provider (Config.TracerProvider, #90) each analysis is one OpenTelemetry trace: a root span
// with the outcome, and spans for the context, the budget reservation, the triage and escalation phases, each
// model call and each tool call. Spans carry IDs, counts, fixed result labels and the configured model and
// deployment names only, never text from the cluster, a tool or the model, nor an error's text (invariant 4).
package analysis
