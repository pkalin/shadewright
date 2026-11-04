# ADR-0003: GitOps-only, typed remediation

- Status: Accepted
- Date: 2026-11-01

## Context

An LLM can produce a plausible fix for most incidents, and is also steerable by anything in its context: events, logs, annotations, file contents. Giving it a shell or `kubectl` makes every prompt injection a cluster write.

## Decision

- The model only *proposes*. Its output becomes a `RemediationRequest` with an action from a closed enum and typed, bounded parameters.
- Deterministic policy code decides whether the plan may run and whether it needs human review.
- The only way a plan changes the cluster is a pull request to the GitOps repository, merged under the repository's own rules, and delivered by the GitOps controller.
- The plan is bound to a digest; approval applies to that digest only.
- Tools available to the model are read-only. A proposal is made only through the structured output of the call ([ADR-0023](0023-how-the-analyzer-calls-the-model.md), decision 5). MCP may expose the same tools later; it does not replace RBAC.

## Consequences

- The blast radius of a bad model answer is a PR that a human or a ruleset rejects.
- Every fix is reviewable, auditable and revertible in Git.
- Imperative fixes (restart a pod, cordon a node) are out of scope until a separate, separately authorized catalog exists.
- Fixes are slower than direct writes: minutes, not seconds.

## Alternatives considered

- Direct `kubectl` actions with an allow list: the allow list becomes the only defence, and there is no review or history.
- Free-form patches proposed by the model: impossible to bound or validate deterministically.

## Amendments

- 2026-11-04: the model has no `submit_remediation_proposal` tool. The structured output of the analysis call is the single proposal path, as [ADR-0023](0023-how-the-analyzer-calls-the-model.md) decision 5 decided on 2026-11-03; the Decision now says so. The decision itself stands: the model only proposes, and policy decides ([#244](https://github.com/pkalin/shadewright/issues/244)).
