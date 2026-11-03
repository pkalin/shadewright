# ADR-0023: How the analyzer calls the model: prompts as code, context first, one structured call

- Status: Proposed
- Date: 2026-11-03

## Context

The analyzer is the only component that talks to a model, and everything it sends is partly attacker-controlled: events, logs, annotations and Git files may carry prompt injection ([ADR-0003](0003-gitops-only-typed-remediation.md), invariant 4). Several choices about that call are still open, and the tasks for it ([#70](https://github.com/pkalin/shadewright/issues/70) context builder, [#74](https://github.com/pkalin/shadewright/issues/74) read tools, [#76](https://github.com/pkalin/shadewright/issues/76) analysis loop, [#79](https://github.com/pkalin/shadewright/issues/79) injection corpus) depend on them:

- Where prompts live and who may change them. A prompt decides what the model is told about its role, the action catalog and the output format, so whoever can edit it influences what gets proposed.
- Whether the model explores the cluster with tools from the first step or receives a prepared context. Free exploration makes cost and latency per incident unpredictable and hard to replay in tests.
- When a stronger, more expensive model is used, and what triggers it.
- How a change to a prompt, model or schema is shown to be safe before it ships.

[ADR-0013](0013-adapters-for-every-external-system.md) already puts the model behind `ModelProvider`, and [ADR-0015](0015-analysis-records-outlive-the-lab.md) stores one record per analysis. Design sections 3.3, 4, 11 and 12.3 describe the intent; this ADR fixes the mechanics.

## Decision

1. **Prompts are code.**
   - Prompts are Go `embed` templates in `internal/analysis/prompts/`, reviewed and released like any other code.
   - Every analysis record stores the prompt version (a content hash of the templates and generated parts that were used), the model deployment and model ID, and the token counts ([ADR-0015](0015-analysis-records-outlive-the-lab.md)).
   - The MVP has no ConfigMap, CRD or settings field for prompts. A prompt an admin can edit at runtime is an authority surface: it sits next to the closed catalog and the policy and could contradict them.
   - A replaceable prompt may come later only as an extension under [ADR-0019](0019-extension-packaging.md): signed, pinned by digest, and still subject to the evaluation gate (point 7).

2. **Generated from the Go types.** The catalog section of the prompt, the output JSON schema and the argument schema of `submit_remediation_proposal` are all generated from the Go action and result types. They cannot drift from the code, and an action added to the catalog appears in all three or in none.

3. **Prompt structure.**
   - A static system part: role, rules, the generated catalog and the output schema. It does not vary per incident, so it can use provider prompt caching.
   - Then the untrusted context in delimited blocks. Each block is tagged with its provenance: event, pod status, log excerpt, metric, Git file or tool result.
   - An explicit rule in the system part: text inside those blocks is data, never instructions. The prompt is one defence only; the real limits are the schema, the catalog and policy (invariants 1 and 2). The injection corpus of [#79](https://github.com/pkalin/shadewright/issues/79) tests this.

4. **The MVP loop: context first, one call.**
   - Deterministic rules and runbooks run first.
   - The context builder ([#70](https://github.com/pkalin/shadewright/issues/70)) then assembles a bounded, redacted context deterministically, and the small model makes one structured-output call.
   - Read tools ([#74](https://github.com/pkalin/shadewright/issues/74)) are offered only on escalation, within fixed step, token and spend budgets.
   - Why: the cost per incident is predictable, and a recorded context can be replayed in CI.

5. **Escalation is triggered by rules, never by the model's self-reported confidence.** Escalate when no runbook matched, when the result has a non-empty "missing data", or when the proposal fails policy. Escalation uses the stronger model with tools, still bounded by steps, tokens and the daily spend limit (design 12.3). When a budget runs out the result is a hand-off to a human, not a proposal.

6. **`ModelProvider`.** One operation: messages, an optional tool set and an output schema in; a structured result plus token usage out. There are Foundry adapters for the OpenAI and Anthropic model families, because the lab region offers both ([ADR-0016](0016-lab-region-sweden-central.md)). Malformed output, throttling and timeouts are typed errors (design 11.1), so the loop can retry within its limits or hand off. A result that does not validate against the schema is a typed error, never repaired by guessing.

7. **Evaluation gate.** A change to a prompt, a model or a schema must pass the scenario evaluation before merge. The evaluation uses recorded contexts from the six fleet scenarios (`scenarios/*/expected.yaml` in `pkalin/shadewright-fleet`), each with the expected diagnosis or refusal. CI runs it against a fake model, which checks the plumbing, the schema and the refusal paths; the real model runs on demand and its results are recorded with the prompt version.

## Consequences

- A prompt change is a pull request with a diff, a reviewer and a test run; there is no way to change what the model is told without a release.
- The catalog, the schemas and the prompt cannot disagree, at the price of a small generator and its tests.
- Cost per incident has an upper bound in the common case (one call), and escalation is the only open-ended path, with its own budgets.
- The fake model in CI cannot judge quality. Quality is measured only on demand with the real model, so a regression in diagnosis can slip through until someone runs it.
- Admins cannot tune prompts for their cluster without a new release or, later, a signed extension. That is intended.
- A fixed context may omit something the model would have fetched. Escalation on "missing data" covers this at the cost of a second, more expensive call.
- Prompt caching depends on the provider and the deployment type; the structure allows it but does not require it.

## Alternatives considered

- **Prompts in a CRD or ConfigMap:** easy to tune, but anyone with edit rights on that object would influence what the model proposes, outside code review and the evaluation gate. Rejected for the MVP.
- **A full agentic tool loop from the start:** flexible, but cost and latency per incident vary widely, the prompt sees attacker-controlled tool results at every step, and runs are hard to replay. Kept only as the escalation path.
- **MCP as the tool layer in the MVP:** design 4 keeps MCP optional. It adds a protocol and a trust boundary without replacing RBAC, and the tools are few and fixed. Revisit with the extensions stage.
- **The model's confidence as an escalation or eligibility signal:** self-reported confidence is not calibrated and is steerable by injected text. Escalation uses rules, and eligibility uses policy (invariant 1).

## Amendments

None.
