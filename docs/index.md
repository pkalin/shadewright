---
title: Kubernetes incidents to reviewed GitOps pull requests
hide:
  - navigation
  - toc
---

<div class="sw-hero" markdown>

<h1 class="sw-visually-hidden">Shadewright</h1>

![Shadewright](assets/wordmark.png)

<p class="sw-tagline">A safe-remediation controller for Kubernetes clusters run with GitOps: it turns untrusted proposals, from rules or an LLM, into typed, bounded changes tied to policy and to the object's real source in Git, delivers them only as reviewed pull requests, and verifies the effect.</p>

[Get started](getting-started.md){ .md-button .md-button--primary }
[Design and decisions](design-and-decisions.md){ .md-button }
[GitHub](https://github.com/pkalin/shadewright){ .md-button }

</div>

Shadewright watches your cluster, groups repeated symptoms into incidents, asks an LLM for a diagnosis, and proposes a fix as a pull request to your GitOps repository. It never changes your workloads directly (it writes only its own control state: Incidents, RemediationRequests and their status, the remediation settings status, an analysis budget ConfigMap, leader election Leases and Events): your review rules decide what merges, and your GitOps controller delivers it.

## Status {#status}

<span class="sw-status sw-planned">Early</span> Shadewright is under construction. This says what is <span class="sw-status sw-built">built</span> and what is still <span class="sw-status sw-planned">planned</span>.
Built so far: the API and its validation, the policy check with source mapping and the freshness check, the YAML patchers, the semantic diff check, the GitHub and Key Vault adapters, the Prometheus metrics source and pod logs adapters (not wired yet), the pull request step (branch, commit and pull request, up to a person's review), the kill switch (`spec.paused`, one gate right before every Git write), the concurrency limit, the lock per target and the circuit breaker (retry limits and cooldowns between attempts are not built), the collector (events and pod status to deduplicated observations) and the Incident controller (one bounded `Incident` per fingerprint, marked due for analysis within a daily budget), the Prometheus metrics of both components, and the module registries with the generic and AKS platforms, EKS and GKE (detected only) and the cloudflared, Flux, KEDA, Kyverno and Secrets Store CSI driver integration modules (cloudflared replicas that are not ready, failing Flux objects, KEDA trigger errors and replica ceilings, and failing Kyverno policy report results, policies not Ready and refused admissions, as symptoms; secrets volumes that cannot be mounted, diagnosed from the kubelet's mount events). All of them are in the analyzer, which detects them and reports them in the settings status; nothing feeds the diagnosis rules yet, and the symptom sources are not consumed yet. Not built: the analysis, auto-merge, delivery and the verification. The [design](design.md) says what holds at each step, the [decisions](adr/index.md) say why, and the [threat model](threat-model.md) lists each guarantee as built or planned.

## How it works

<div class="grid cards" markdown>

-   [**1. Observe**](concepts.md#incident-built)

    Events and object status in the namespaces you choose, deduplicated into incidents.

-   [**2. Diagnose**](design/3-observation-and-deduplication.md#33-analysis)

    Rules first, then a model with read-only tools and redacted context. No evidence, no proposal.

-   [**3. Propose**](concepts.md#remediationrequest-built)

    A typed `RemediationRequest` from a closed catalog of actions, checked by deterministic policy.

-   [**4. Open a PR**](request-lifecycle.md)

    On a branch, in the one repository it may write to, with the evidence and a rollback plan.

-   [**5. Verify**](design/6-reconciliation.md#6-reconciliation)

    After merge, it follows the GitOps controller (Flux today) to the applied revision and checks that the symptom is gone.

</div>

## Principles

- The model's answer is never authorization.
- Every change is a reviewed, revertible commit.
- Everything read from the cluster is untrusted input.
- Separate, short-lived identities for analysis and for Git.

## Repositories

- [shadewright](https://github.com/pkalin/shadewright): the operator and these docs.
- [shadewright-azure](https://github.com/pkalin/shadewright-azure): Terraform for a disposable reference lab in Azure.
- [shadewright-fleet](https://github.com/pkalin/shadewright-fleet): the lab's GitOps repository and incident scenarios.
