# ADR-0018: Three axes of pluggability

- Status: Accepted
- Date: 2026-11-02

## Context

Shadewright should run on any conformant cluster and fit whatever a team already uses. [ADR-0013](0013-adapters-for-every-external-system.md) covers systems outside the cluster (Git hosting, delivery, model, observability, notifications, storage, signing). Two kinds of difference are not covered: the cluster platform itself (AKS, EKS, GKE, Cluster API, plain Kubernetes) and the add-ons installed in it (KEDA, Kyverno, external secrets and many more). Both decide what can go wrong, how to diagnose it and what a safe fix looks like.

## Decision

Shadewright is pluggable along three axes, each with a registry of modules and the same rule: the core works on plain Kubernetes with none of them, and a module only adds knowledge or capabilities.

1. **Platform services** (ADR-0013): `GitProvider`, `DeliveryVerifier`, `ModelProvider`, `MetricsSource`, `LogSource`, `Notifier`, `AnalysisStore`, `Signer`. Interfaces are written in neutral terms (a change request, not a GitHub pull request) and adapters declare what they support, for example whether the host can auto-merge; the core adapts instead of assuming GitHub. Planned Git adapters: GitHub, then Azure DevOps and GitLab.
2. **Cluster platforms**: a `Platform` module per provider (generic, AKS, EKS, GKE, Cluster API), detected from node `providerID` and labels. It contributes diagnosis (cloud events and quota or IP exhaustion, node capacity and autoscaling, managed add-ons, workload identity) and, later, platform-specific actions. The generic module is always present.
3. **Cluster integrations**: an `Integration` module per add-on ("Shadewright understands KEDA"). Each declares how it is detected (API groups, CRDs and versions), what it watches and how conditions become symptoms, deterministic diagnosis rules, read-only tools for the analyzer, redaction rules, verification checks, and optionally actions. Actions follow the same rules as every other action (typed, bounded, policy rule, GitOps patcher, refusal test).

Integrations that guard security (admission policy engines, secret managers, vulnerability scanners) diagnose only. They may explain why a deployment is refused or a secret is missing; they never propose to relax a policy, change a secret or suppress a finding.

Modules are enabled per cluster in `RemediationSettings`; the operator reports what it detected, and which modules are active, in status.

First modules: generic and AKS platforms, then EKS and GKE as detection-only skeletons. Integrations: core Kubernetes workloads and KEDA (both already have actions), then Flux, Kyverno, Secrets Store CSI driver and cloudflared, matching a real cluster these were taken from. Later candidates: External Secrets Operator, Reloader, cert-manager, Trivy Operator, Karpenter.

How modules are packaged and shipped is [ADR-0019](0019-extension-packaging.md).

## Consequences

- Supporting a new add-on or cloud is a contained piece of work with a clear checklist, which suits parallel work and outside contributions.
- The analyzer gets better context on clusters where modules match, and still works on clusters where none do.
- Interfaces must be designed around Shadewright's needs rather than the first vendor; capability flags add some complexity to the core.

## Alternatives considered

- **Hard-code AKS and the lab's add-ons**: fastest start, but every later platform or add-on would mean changing the core.
- **Let the model figure out any add-on on its own**: no code per add-on, but no deterministic diagnosis, no typed actions and nothing to test.
