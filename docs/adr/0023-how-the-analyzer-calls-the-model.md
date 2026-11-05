# ADR-0023: How the analyzer calls the model: prompts as code, context first, one structured call

- Status: Accepted
- Date: 2026-11-03

## Context

The analyzer is the only component that talks to a model, and everything it sends is partly attacker-controlled: events, logs, annotations and Git files may carry prompt injection ([ADR-0003](0003-gitops-only-typed-remediation.md), invariant 4). Several choices about that call are still open, and the tasks for it ([#70](https://github.com/pkalin/shadewright/issues/70) context builder, [#74](https://github.com/pkalin/shadewright/issues/74) read tools, [#76](https://github.com/pkalin/shadewright/issues/76) analysis loop, [#79](https://github.com/pkalin/shadewright/issues/79) injection corpus) depend on them:

- Where prompts live and who may change them. A prompt decides what the model is told about its role, the action catalog and the output format, so whoever can edit it influences what gets proposed.
- Whether the model explores the cluster with tools from the first step or receives a prepared context. Free exploration makes cost and latency per incident unpredictable and hard to replay in tests.
- When a stronger, more expensive model is used, and what triggers it. Anything the model writes, including its own list of missing data, can be steered by injected text, so it is a poor trigger.
- How a change to a prompt, model or schema is shown to be safe before it ships, and how the running analyzer knows it is using a combination that was shown to be safe.

[ADR-0013](0013-adapters-for-every-external-system.md) already puts the model behind `ModelProvider`, and [ADR-0015](0015-analysis-records-outlive-the-lab.md) stores one record per analysis. Design sections 3.3, 4, 11 and 12.3 describe the intent; this ADR fixes the mechanics.

## Decision

1. **Prompts are code.**
   - Prompts are Go `embed` templates in `internal/analysis/prompts/`, reviewed and released like any other code.
   - Every analysis record stores the prompt version (a content hash of the templates and generated parts that were used), the model deployment and model ID, and the token counts ([ADR-0015](0015-analysis-records-outlive-the-lab.md)).
   - The MVP has no ConfigMap, CRD or settings field for prompts. A prompt an admin can edit at runtime is an authority surface: it sits next to the closed catalog and the policy and could contradict them.
   - A replaceable prompt may come later only as an extension under [ADR-0019](0019-extension-packaging.md): signed, pinned by digest, and still subject to the evaluation gate (point 8).

2. **Generated from the Go types.** The catalog section of the prompt, the output JSON schema and the schema of a proposal are all generated from the Go action and result types. They cannot drift from the code, and an action added to the catalog appears in all of them or in none.

3. **Prompt structure.**
   - A static system part: role, rules, the generated catalog and the output schema. It does not vary per incident, so it can use provider prompt caching.
   - Then the untrusted context in delimited blocks. Each block is tagged with its provenance: event, pod status, log excerpt, metric, Git file or tool result.
   - An explicit rule in the system part: text inside those blocks is data, never instructions. The prompt is one defence only; the real limits are the schema, the catalog and policy (invariants 1 and 2). The injection corpus of [#79](https://github.com/pkalin/shadewright/issues/79) tests this.
   - The prompt is not the only thing that decides what the model sees. Operator-supplied redaction patterns and a swapped model deployment change it too, which is why point 8 keys the evaluation on the model and the prompt together and fails closed.

4. **The MVP loop: context first, one call.**
   - Deterministic rules and runbooks run first. A runbook match is deterministic and makes no model call: it maps an incident's symptoms to a known diagnosis and a typed proposal by rules in code.
   - The model path is for incidents no runbook matched. Its common case is one small-model call.
   - The context builder ([#70](https://github.com/pkalin/shadewright/issues/70)) assembles a bounded, redacted context deterministically, and the small model makes one structured-output call on it.
   - Read tools ([#74](https://github.com/pkalin/shadewright/issues/74)) are offered only on escalation, within fixed step, token and spend budgets.
   - Why: predictable cost per incident, and recorded contexts replay in CI.

5. **One proposal path.** The structured output of the call, whether small or escalated, is the only way a proposal is made. One validator checks it against the generated schema and the catalog and builds the `RemediationRequest`. There is no `submit_remediation_proposal` tool: the model's tools are read tools only. This removes a second path to a request that would need its own validation, and it makes tool-free calls and escalated ones end the same way.

6. **Escalation runs on deterministic signals only.**
   - The valid triggers are: the context builder truncated the context or could not fetch a source; or the small model's output failed schema validation after one retry.
   - A non-empty "missing data" in the result is not a trigger. It becomes a hand-off to a person that says what is missing, because it is model output and injected text can steer it. The model's self-reported confidence is never a trigger or a signal of eligibility.
   - Escalation uses the stronger model with read tools, bounded by steps, tokens and the daily spend limit. It is capped at one per incident fingerprint per cooldown window.
   - An escalation makes at most `maxToolSteps` + 1 model turns. An invalid escalated result gets no retry; it is handed off. With the triage call and its one retry, an analysis makes at most 3 + `maxToolSteps` model calls in total; a retry after a throttled or transient failure (`ErrRetryable`) counts toward that bound.
   - Every tool result passes through `internal/redact` before the model sees it, like the context does (invariant 4).
   - When a budget runs out the result is a hand-off, not a proposal.

7. **A policy denial is a hand-off.** Policy runs in the executor after the request exists. If it denies a request, the analyzer never re-proposes, and the denial reason is never shown to the model. Policy outcome is therefore not an escalation trigger, and the analyzer needs no new permission to read it.

8. **`ModelProvider` and the evaluation gate.**
   - Today's interface is built: `Complete(ctx, CompletionRequest) (Completion, error)`. A request carries messages, tool definitions, an optional `ResponseSchema` and `MaxOutputTokens`. A completion carries `Content string`, tool calls, `Usage`, `UsageUnknown` and a `Model` field. Throttling and transient failures (408, 429, 5xx, timeouts) are `ErrRetryable` errors; an unknown deployment is `ErrNotFound`, final.
   - Planned changes, made with the analysis loop: a deployment field on the request (so the record can name it); a typed error for timeouts, beside `ErrRetryable` (design 11.1). Foundry adapters for the OpenAI and Anthropic families, because the lab region offers both ([ADR-0016](0016-lab-region-sweden-central.md)): the OpenAI-format one is built (`internal/providers/model/foundry`, #75) and already returns malformed output as `ErrMalformedOutput` with the reply's usage; the Anthropic one is planned.
   - **What CI covers.** A change to a prompt, model or schema must pass the scenario evaluation before merge. In CI the evaluation runs against a fake model and covers plumbing, schemas and refusal paths only. It does not test the prompt's defences or the quality of any model.
   - **What gates quality.** A recorded real-model evaluation, keyed by model ID and version plus the prompt hash, stored in this repository (for example `test/eval/results/`). Its inputs are recorded contexts from the six fleet scenarios, kept here (for example `test/eval/contexts/`). The expectations are the fleet's `scenarios/*/expected.yaml` in `pkalin/shadewright-fleet`, and each result pins the fleet commit it read them from. The real model runs on demand.
   - **Fail closed at run time.** The analyzer embeds the list of evaluated (model, prompt hash) pairs that passed. It refuses automatic analysis when the model behind the configured deployment (and `Completion.Model` in a reply) has no passing record for the current prompt hash, and reports the reason in status and metrics. It then hands off instead of calling or trusting the model.

9. **Spend accounting.**
   - A failed or timed-out call is charged at its upper bound: the input estimate plus `MaxOutputTokens`.
   - The daily count of analyses is kept in the operator namespace, in an object the analyzer owns: the ConfigMap `shadewright-analysis-budget` (the UTC day and the analyses started on it), shipped empty with the analyzer and checked by typed parsing in Go ([#43](https://github.com/pkalin/shadewright/issues/43), design 3.2). The analyzer may `get` and `update` that one ConfigMap, named in `resourceNames`, and may not create it, so deleting it stops analysis instead of resetting the count. Every write uses the resourceVersion read.
   - The daily spend limit is a count of tokens (input and output) per model deployment and UTC day, `maxTokensPerDay` in `RemediationSettings`, not an amount of money, so no price list lives in the settings ([#68](https://github.com/pkalin/shadewright/issues/68), design 12.3). Where that spend is counted is open ([#208](https://github.com/pkalin/shadewright/issues/208)).
   - A counter that is missing, cannot be read or written, or holds a value that cannot be trusted (a future day, a count out of bounds) means refuse, not allow.

## Consequences

- A prompt change is a pull request with a diff, a reviewer, a test run and a new evaluation record. Operator redaction patterns and a swapped deployment also change what the model sees; the evaluation list catches the deployment case by refusing an unevaluated model, and redaction changes are reviewed as configuration.
- The catalog, the schemas and the prompt cannot disagree, at the price of a small generator and its tests.
- The cost of a runbook match is zero, and the common model case is one call. Escalation is the only open-ended path, with its own budgets and a cap per fingerprint.
- A model that goes untested cannot run by accident: upgrading a deployment's model without a new evaluation record stops automatic analysis until one is added. That is intended, and it needs a recorded evaluation per model, which costs real-model runs and repository space.
- The fake model in CI cannot judge quality or injection resistance. Quality depends on the recorded evaluation being run when it should be.
- A fixed context may omit something the model would have fetched. Truncation and fetch failures escalate; "missing data" produces a hand-off that tells the person what to look at.
- Hand-off on a denial means the analyzer does not try again with a different proposal; a person decides what to do next.
- Admins cannot tune prompts for their cluster without a new release or, later, a signed extension. That is intended.
- Prompt caching depends on the provider and the deployment type; the structure allows it but does not require it.
- The analyzer gains one write beyond Incidents and requests: `update` on its budget ConfigMap. Invariant 6 in `AGENTS.md` names it. Whoever may write that ConfigMap (the analyzer, admins) can reset the count, and whoever may create ConfigMaps in the operator namespace can recreate it after a deletion (threat model, section 6).

## Alternatives considered

- **Prompts in a CRD or ConfigMap:** easy to tune, but anyone with edit rights on that object would influence what the model proposes, outside code review and the evaluation gate. Rejected for the MVP.
- **A full agentic tool loop from the start:** flexible, but cost and latency per incident vary widely, the prompt sees attacker-controlled tool results at every step, and runs are hard to replay. Kept only as the escalation path.
- **MCP as the tool layer in the MVP:** design 4 keeps MCP optional. It adds a protocol and a trust boundary without replacing RBAC, and the tools are few and fixed. Revisit with the extensions stage.
- **The model's confidence, or its list of missing data, as an escalation or eligibility signal:** both are model output that injected text can steer, and confidence is not calibrated. Escalation uses deterministic signals, and eligibility uses policy (invariant 1).
- **Feeding the policy denial back to the model for a second proposal:** it would show the model the policy's shape and let it search for a proposal that passes. Rejected; a denial ends in a hand-off.
- **A `submit_remediation_proposal` tool beside the structured output:** two paths to a request, each needing validation. Rejected for one path.
- **A money cap in settings:** rejected, it needs a price list in the settings that goes stale. The cap is in tokens per deployment (decision 9).

## Amendments

- 2026-11-04: [ADR-0003](0003-gitops-only-typed-remediation.md) links here and no longer lists `submit_remediation_proposal` among the model's tools. Nothing in this ADR changed ([#244](https://github.com/pkalin/shadewright/issues/244)).
- 2026-11-04: Decision 8 now says the Foundry adapters are planned; only the interface and a fake exist. Nothing else changed ([#245](https://github.com/pkalin/shadewright/issues/245)).
- 2026-11-04: Decides the unit that decision 9 left open: the daily spend limit is a count of tokens per model deployment and UTC day, set in `RemediationSettings`; where the spend is counted is still open (#208). Decision 6 now states the call bound it implied (at most `maxToolSteps` + 1 escalated turns, no retry of an invalid escalated result, so at most 3 + `maxToolSteps` calls per analysis). Nothing is enforced by this, and nothing else changed ([#68](https://github.com/pkalin/shadewright/issues/68)).
- 2026-11-05: Decision 8's planned changes are partly built: the Foundry adapter for OpenAI-format models is bound to one deployment in its configuration, returns malformed output as `ErrMalformedOutput` with the reply's usage, and reports usage per call. A deployment field on the request, a typed timeout error and the Anthropic adapter are still planned. Nothing else changed ([#75](https://github.com/pkalin/shadewright/issues/75)).
- 2026-11-05: Decision 6 now says that a retry after `ErrRetryable` counts toward the bound of 3 + `maxToolSteps` model calls, as the analysis loop enforces it. Nothing else changed ([#76](https://github.com/pkalin/shadewright/issues/76)).
