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

## Amendments

- 2026-11-03: the detection rule for platforms and the rule for when a module's hooks apply were fixed as `internal/modules` was built ([#214](https://github.com/pkalin/shadewright/pull/214), design 2.4 and 2.5). A provider ID scheme names the cloud, not the managed platform, so a platform is recognised by the node labels its managed service sets, with the scheme only as an extra condition; a scheme alone claims nothing, and ambiguous or mixed nodes give the generic platform. A module contributes hooks (symptom sources, rules, tools, actions) only when the cluster admin enabled it in `RemediationSettings` and it is detected: detection alone adds no tool or action, and an enabled but undetected module contributes nothing. Module tools belong to the tool set that the evaluation gate of [ADR-0023](0023-how-the-analyzer-calls-the-model.md) covers. This narrows what a module can do and widens nothing. The decision is unchanged.
- 2026-11-03: who writes the report in status is decided by [ADR-0024](0024-analyzer-writes-detection-to-settings-status.md): the analyzer, with an update of the `remediationsettings/status` subresource; the status is never read back to decide which modules are active. The decision is unchanged.
- 2026-11-04: an integration for an add-on with no API group of its own (cloudflared) declares the core group version of the kinds it reads (`v1`). Every cluster serves it, so for such a module detection does not narrow: it is reported as detected on every cluster and is active wherever the cluster admin enables it, which stays the only gate. Only diagnosis-only modules may do this: `internal/modules` refuses to build a registry with a module whose detection is core group versions only and that offers tools, actions or verification checks ([#67](https://github.com/pkalin/shadewright/issues/67), design 2.5). Enabling a module stays deliberate (nothing is enabled by default), and no tool or action is reachable through this. The decision is unchanged.
