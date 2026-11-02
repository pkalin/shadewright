# ADR-0008: A disposable Azure lab in its own subscription

- Status: Accepted
- Date: 2026-11-01

## Context

Developing the operator needs a real cluster with a GitOps controller and a managed model endpoint. The maintainer's laptop is not suited to running a local cluster all day, and the lab must stay inside a small monthly credit without touching other workloads.

## Decision

- Unit and controller tests run on envtest, no cluster needed. End-to-end tests run on Kind in GitHub Actions.
- The lab runs in Azure, in a subscription separate from any production workload, in three Terraform roots: `bootstrap` (state), `foundation` (Key Vault, identities, model deployment, budget; stays up) and `lab` (AKS, federation, Flux; applied for a session and destroyed after).
- AKS: Free tier, one small node pool, Entra ID with Azure RBAC, local accounts disabled, public API server limited by `authorized_ip_ranges`.

## Consequences

- Most of the cost is node hours, paid only while working.
- A public API server (IP-restricted) keeps `kubectl` and `make run` simple; this is a deliberate difference from private production clusters.
- Recreating the cluster often tests the bootstrap path continuously.

## Alternatives considered

- Private cluster: needs `az aks command invoke` or a jump host for every `kubectl`, which is painful for operator development.
- A permanent cluster: simpler, but pays for idle hours.
- Local Kind or Minikube only: no workload identity, no managed model, and heavy on a laptop.
