---
title: Glossary
---

# Glossary

The terms used across these docs. Acronyms also show a tooltip where they appear. Longer explanations are in [concepts](concepts.md).

Action
:   One entry of the closed catalog of changes Shadewright can make, with typed, bounded parameters. Today `SetContainerMemoryLimit` and `AdjustKedaMaxReplicas`.

Action catalog
:   The closed list of actions. There is no free-form command. See [concepts](concepts.md).

Analyzer
:   The component that reads the cluster, records Incidents and (planned) proposes requests. It has no Git credential.

Approval
:   A person's review and merge of the pull request. Shadewright does not approve its own pull requests. See [the life of a request](request-lifecycle.md#what-approving-means).

Circuit breaker
:   A limit that holds new requests back after repeated failures, with the reason `CircuitOpen` or `CircuitHalfOpen`. See [troubleshooting](troubleshooting.md#limits).

Collector
:   The part of the analyzer that turns events and pod status into deduplicated observations.

Condition
:   A typed status entry (`PolicyAllowed`, `PullRequestOpened`, `Approved`, `PlanFresh`, `Queued`) with a machine-readable reason. The phase is only a summary.

Digest (plan digest)
:   A `sha256:` hash of the plan, the target, the resolved source and the policy revision. The executor acts only on a plan whose digest matches the approved one.

Executor
:   The component that reconciles requests, runs the policy check and, with the Git App, opens the pull request.

Fingerprint
:   The hash that identifies an Incident: cluster, namespace, owner, reason and normalized symptom.

GitOps controller
:   The tool that deploys what is in Git. Flux today; Argo CD is planned.

Incident
:   The record of one repeated symptom on one workload. See [read an Incident](how-to/read-an-incident.md).

Inconclusive
:   The final phase of a merged request whose effect could be shown neither way; condition `Verified` gives the reason ([ADR-0030](adr/0030-merge-receipt-and-verification-outcomes.md)).

Kill switch
:   `spec.paused` on `RemediationSettings`: a gate before every Git write. See [pause and resume](how-to/pause-and-resume.md).

Merge receipt
:   `status.mergeReceipt`: the repository, target branch, pull request, head and merge commit of a merge, written once with the phase `Merged` ([ADR-0030](adr/0030-merge-receipt-and-verification-outcomes.md)).

Module
:   A platform (AKS, EKS, GKE) or an integration (KEDA, Flux, ...) that adds detection, symptoms or actions, enabled by name in `spec.modules`.

Observe-only mode
:   An install with no repository in the settings and `spec.paused` true, so the analyzer records Incidents and the executor could not write to Git.

Operator namespace
:   The namespace (`shadewright-system` by default) that holds the Deployments, Incidents and RemediationRequests.

Phase
:   The one-word summary of where a request is, such as `AwaitingApproval`. See [the life of a request](request-lifecycle.md).

Plan
:   The concrete change a request holds: action, target, previous and desired value.

Policy check
:   The deterministic decision of whether a request may run, a pure function of the request, policy, settings, source and live target.

RemediationPolicy
:   The cluster-scoped, admin-owned bounds a request must stay within. See [write or tighten a policy](how-to/write-a-policy.md).

RemediationRequest
:   One plan, its policy decision and its execution state. Its spec is immutable.

RemediationSettings
:   The single cluster-scoped object `default`: kill switch, limits, repositories, modules.

Source mapping
:   Finding the one file in Git that declares a live object, by following the Flux objects that deliver it ([ADR-0012](adr/0012-object-to-source-mapping.md)).

Superseded
:   The final phase of a request whose plan went stale before merge. Its pull request is closed.

Target
:   The live object a request changes, pinned by UID.

Target lock
:   A limit by which a request waits while another request holds the lock on the same target (`status.holdsTarget`), from its allowed decision until its pull request is known merged (with `--verify-outcomes` on, until the merged request ends), or closed with no other pull request open on its branch that may deliver its change (condition `TargetHeld` names one), also after it stopped (`TargetLocked`).

Watched namespaces
:   The namespaces the analyzer reads, set by `--watch-namespaces`. See [limit the namespaces it watches](how-to/limit-watched-namespaces.md).
