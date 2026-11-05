# ADR-0025: Analysis claims, units and the evaluation manifest

- Status: Proposed
- Date: 2026-11-05

## Context

The analysis loop ([#76](https://github.com/pkalin/shadewright/issues/76), PR #415) is about to call a model, and four contracts around it are still unwritten ([#249](https://github.com/pkalin/shadewright/issues/249)):

- **Ownership.** `AnalysisDue` is a scheduling hint, not ownership. The loop claims an Incident by writing the phase `Analyzing` and `lastAnalysisTime` in one status update with the resourceVersion it read, so two runs never claim the same round. But the Incident has no run ID, so a run that stalls past the trigger's 10 minute timeout cannot be told apart from the run that claimed the Incident after it, and neither can check that it still owns the Incident before it creates a `RemediationRequest`.
- **Counting.** `ReserveAnalysis` adds one to the day's count, and [ADR-0023](0023-how-the-analyzer-calls-the-model.md) does not say what one unit is: an incident, an attempt or a call. Spend is a separate count of tokens ([#68](https://github.com/pkalin/shadewright/issues/68), [#208](https://github.com/pkalin/shadewright/issues/208)).
- **Evaluation identity.** ADR-0023 decision 8 keys a passing evaluation on (model, prompt hash). Other inputs change what the model sees or how a result is judged: the tools offered, the context builder, the redaction patterns, the rubric.
- **Durability.** Between "the analyzer accepted an observation" and "a record is stored" there are several states, and nothing says whether a failed record write stops a proposal ([ADR-0015](0015-analysis-records-outlive-the-lab.md), [#77](https://github.com/pkalin/shadewright/issues/77)).

These contracts must exist before the analyzer binary starts the loop. Design 3.2, 3.3, 11 and 12.3 link here.

## Decision

1. **A durable claim with a run ID and an expiry.**
   - The claim update that writes the phase `Analyzing` and `lastAnalysisTime` also writes `status.analysisRun`: `id` (a random UUID, new for every attempt), `startedAt`, `expiresAt`, and the generation and subject the run read. It is the same single status update with the resourceVersion read, so there is still one writer per round.
   - `expiresAt` is `startedAt` plus the trigger's analysis timeout (10 minutes). The loop's own deadline (8 minutes) is shorter, so a live run ends before its claim lapses. An expired claim is the `AnalysisTimedOut` case of the trigger: the next claim replaces `analysisRun` with a new ID.
   - **Recheck before publishing.** Just before it creates the `RemediationRequest`, and again before it writes the outcome, the run reads the Incident with a live `get` (not the cache) and continues only if the run ID is its own, the claim has not expired, and the UID, generation and subject are unchanged. Otherwise it drops the result: no request, no outcome write, a log line with the run ID.
   - **What the recheck does not do.** A create cannot be conditioned on the Incident's resourceVersion, so a gap of milliseconds remains. It is closed by idempotence, not by the recheck: the request name derives from the Incident UID and the spec it was proposed for (PR #415), so a second run that gets through creates the same name, receives `AlreadyExists` and treats it as done (invariant 5). The outcome update is conditioned on the resourceVersion read in the recheck, so a stale run cannot overwrite a newer one.
   - The run ID is a correlation key for logs and the stored record. It carries no authority: nothing reads it to decide what may run (invariant 1).

2. **One analysis unit is one claimed attempt.**
   - A unit covers every call, retry and tool round of one run (at most 3 + `maxToolSteps` calls, ADR-0023 decision 6). It is reserved with `ReserveAnalysis` after the claim and the context build, and before the first model call, so an attempt that ends before any call (a runbook match, a failed context build) takes none.
   - The day is the UTC day at reservation. A run that crosses midnight counts once, on the day it reserved. A unit is never returned: a lost response, a timeout, a lost leadership or a crash all leave it counted, so the count can only be too high.
   - A claim that expired and is taken again is a new attempt and takes a new unit. The cooldown (15 minutes) already limits how often that happens for one Incident.
   - **Money is separate.** The limit on tokens per deployment and UTC day (`maxTokensPerDay`, #208) and the per-analysis token bound are not units and are not converted into them. All three bounds must hold: the unit count caps how many analyses start, the per-analysis bound caps one run, and the daily tokens cap what the provider can bill. A retry costs tokens, never a unit.

3. **An evaluation manifest.**
   - Every recorded real-model evaluation result names a manifest of the inputs that affect behaviour: the model deployment, model ID and version; the hash of the prompt templates and generated parts; the hash of the tool definitions offered on escalation; the context builder's version and limits (size bound, sources); the redaction configuration (built-in rules version and operator patterns); the hash of the generated output schema and catalog; the rubric (the fleet commit of `expected.yaml`); the pass thresholds; and the number of repeats.
   - The embedded list of passing evaluations is keyed by the digest of that manifest, not by (model, prompt hash). A change to any field gives a different digest, so it invalidates the pass until a new evaluation is recorded.
   - At run time the analyzer computes the digest of what it is configured to use and refuses automatic analysis, with the reason in status and metrics, when no passing record has it. Fields only the harness knows (rubric, thresholds, repeats) are part of the stored result and checked when the list is built in CI, not at run time. Operator redaction patterns change the run-time digest, so an admin who changes them stops automatic analysis until the combination is evaluated. That is intended.
   - This amends ADR-0023 decision 8 once this ADR is accepted ([ADR-0021](0021-amending-accepted-adrs.md)). The harness is [#78](https://github.com/pkalin/shadewright/issues/78).

4. **Durability steps, and whether a record gates the proposal.**
   - The steps, in order: (a) **accepted in memory**: the collector or controller holds the observation in a batch, and a crash loses it. (b) **Persisted Incident**: the status write has succeeded and a restart finds it. (c) **Claimed snapshot**: the claim update with the run ID has succeeded, and the context is built from the Incident as claimed (UID, generation, subject); only this step lets a run call the model. (d) **Stored record**: the redacted record is in the `AnalysisStore` ([ADR-0015](0015-analysis-records-outlive-the-lab.md)).
   - **v0.3.0: a stored record does not gate the proposal.** The `AnalysisStore` is not built (#77), so a gate would mean no proposals at all. The outcome (run ID, manifest digest, prompt version, tokens, result and reason) is logged and written as the condition `Analyzed` on the Incident. The proposal is already bounded by invariants 1 and 2 without a record.
   - **When this changes.** Once an `AnalysisStore` is configured and on (ADR-0015 makes it on by default), the record is written before the request is created and a failed write is a hand-off, not a proposal: a proposal with no audit trail should not exist. The write has its own timeout, and the request name already derives from the Incident and spec, so a retry finds an earlier request. This is a follow-up amendment tied to #77, not part of v0.3.0.

5. **Invariants.**
   - Invariant 1 holds: the claim, run ID and manifest authorize nothing. The model still returns structured output, the controller creates the request with `approvalRequired: true`, and policy decides.
   - Invariant 6 holds with no new permission: the claim and the outcome are writes to the Incident status (already allowed), the recheck is a `get` on an Incident (inside the analyzer's read access), the request is a create it already has, and the unit uses the budget ConfigMap it already updates.

6. **PR #415.** It claims with the phase and `lastAnalysisTime`, reserves one unit before the first model call and names requests from the Incident UID and spec, so decision 2 and the idempotence in decision 1 already fit. A follow-up adds `status.analysisRun` to the claim (an additive status field, with CRD, docs and tests), the live recheck before the request create and before the outcome write, and the manifest digest in the run-time gate.

## Consequences

- A stalled run cannot publish after its claim was taken over, and a stale outcome cannot overwrite a newer one. The remaining gap is closed by the deterministic request name, so it costs no duplicate.
- The daily count of analyses is predictable: one per attempt. It overcounts after crashes and lost responses, which fails closed.
- Three bounds exist side by side (units, per-analysis tokens, daily tokens), and an admin sets each. The settings do not gain a price list.
- An evaluation pass is valid only for the exact combination it covered. Any change to the context builder, tools or redaction needs a new real-model run, which costs money and review time.
- In v0.3.0 a proposal can exist without a stored record. The log and the Incident condition are the only trail until #77, and the amendment above is the point where that stops.
- The Incident status grows by one small field.

## Alternatives considered

- **No claim field; rely on the phase, `lastAnalysisTime` and the resourceVersion:** it prevents two simultaneous claims, but a stalled run cannot be told from its successor, so it could publish after being replaced. Rejected.
- **A Lease per Incident:** a precise lock, but a new object kind and a new permission (invariant 6) for what a status field gives. Rejected.
- **Recheck by conditioning the request create on the Incident:** not possible for a create in the Kubernetes API. The deterministic name does that job.
- **One unit per Incident:** simple, but a re-analysis after new occurrences would be free, and the daily count would not bound model starts. Rejected.
- **One unit per model call:** bounds calls directly but makes the count depend on retries and tool rounds, so a retry could stop a later Incident from being analyzed, and the limit is hard to explain. Calls are bounded by the per-analysis limits and tokens instead.
- **Converting tokens into units, or a money cap in units:** needs a price list and hides what is bounded. Rejected (ADR-0023 decision 9).
- **Keep the evaluation key as (model, prompt hash):** a change to tools, context or redaction would keep a pass that no longer describes the behaviour. Rejected.
- **Gate every proposal on a stored record now:** no store exists, so no proposals. Deferred until #77.
- **Never gate on a record:** loses the audit property of ADR-0015 once a store is on. Rejected as the end state.

## Amendments

None.
