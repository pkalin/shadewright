# ADR-0026: The analyzer shows the model the policy bounds

- Status: Accepted
- Date: 2026-11-07

## Context

In the lab run of 2026-11-07 the model proposed raising the memory limit of `demo/web` from 48Mi to 96Mi. The policy `demo-memory` (64Mi to 128Mi, an increase of at most 50%) Denied it with `IncreaseTooLarge`, and the incident went to a person although a change within the bounds (64Mi to 72Mi) would have passed. Nothing in the context named the bounds, so the model guessed ([#493](https://github.com/pkalin/shadewright/issues/493)).

[ADR-0023](0023-how-the-analyzer-calls-the-model.md) rejected feeding a policy denial back to the model: a denial shows the shape of the policy one probe at a time and lets the model search for what passes. The question is whether showing the bounds up front is that loop by another route.

## Decision

The context builder adds a policy bounds block to every analysis with `--action-policies`: for each catalog action that can change the subject, the bounds of the RemediationPolicy that would decide it, computed by the executor's own policy code (`policy.BoundsFor`, `policy.AllowedMemory`), or why any proposal would be Denied. The block is labelled operator configuration, rendered from the policy's typed fields only, and never quotable as evidence ([design 3.3](../design/3-observation-and-deduplication.md#the-prompt-and-the-result)).

- It is shown once, before the model answers, and is the same whatever the model proposes. There is still no feedback after a denial: a denial still ends in a hand-off, and its reason is never shown to the model (ADR-0023 decision 7 stands).
- Nothing clamps the proposal to the bounds. The policy still decides every request in the executor, which reads the policy again (invariant 1).
- To read the policy the analyzer gets `get` on `remediationpolicies`, by name only: the policies `--action-policies` names (`resourceNames`), with no `list` or `watch` (invariant 6).

## Consequences

This is not the loop ADR-0023 rejected. The model cannot probe: it gets one fixed view before it answers and learns nothing from a proposal. The bounds are not secret either: they are in the GitOps repository and the cluster, and the security of a change comes from the policy check and the person who merges the pull request, not from the model not knowing the limits.

The analyzer can read the named policies (their bounds, targets, repository name and allowed paths: configuration, nothing secret). A compromised analyzer learns the bounds and still cannot pass the policy check.

Risk: the model aims at the maximum. In the evaluation a reply was written to ask for 72Mi as the most the block shows. Mitigations:

1. A prompt rule: the bounds are limits, not a target; propose the smallest change the evidence supports and say in the summary which evidence sets the size.
2. The pull request body shows the size of the step (an increase of N% of the previous value) beside "within policy", with a note that the policy bounds the step but does not say it is the right size, and that the diagnosis names the evidence for the size. The executor has only the request's typed fields, so the body shows the size, not the model's own words; carrying the model's size evidence into the body would need a field on the request and is not decided here.
3. The evaluation scores oversized proposals ([#505](https://github.com/pkalin/shadewright/issues/505), planned).

## Alternatives considered

- A preflight in the analyzer: run the policy check before creating the request and hand off when it would Deny, without showing the bounds. It needs the same read, the model still guesses, and it is a second decision point to keep in step with the executor.
- Both the bounds and the preflight: the preflight adds the second decision point for little, since the executor decides anyway.
- Nothing: every guess outside the bounds goes to a person, as in the lab run.

## Amendments

None.
