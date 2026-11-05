# 1. Goals

Shadewright is a safe-remediation controller for Kubernetes clusters run with GitOps. The diagnosis, from rules or a model, is an input; the product is the control of what happens with it. It:

1. Watches events, object status and basic metrics in selected namespaces.
2. Groups repeated symptoms into incidents.
3. Prepares a diagnosis backed by evidence, and a proposed fix.
4. Records the proposed fix as a typed `RemediationRequest`.
5. Checks it against deterministic policy.
6. For declarative changes, opens a pull request in the GitOps repository.
7. Waits for human approval, or enables auto-merge where policy allows it.
8. Watches the GitOps controller roll the change out and checks the real effect.

The model helps analyse. The Go controllers own permissions, process state, validation and execution. **A model response is never, on its own, authorization to change anything.**

## Non-goals for the MVP

- Direct writes to the cluster (`apply`, `patch`, `exec`, `delete`), including as a fallback when a PR fails.
- Changes to cloud infrastructure outside the GitOps repository.
- Multiple clusters, multiple Git providers at the same time, or self-hosted models.
- Fine-tuning or a vector database.

## First scope

One cluster, a few namespaces, one GitOps repository on GitHub synced by Flux, and a small catalog of fixes. Azure DevOps and Argo CD come later behind the same interfaces ([ADR-0004](../adr/0004-github-and-flux-first.md)).
