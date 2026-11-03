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

[Read the design](design.md){ .md-button .md-button--primary }
[Decisions](adr/index.md){ .md-button }
[GitHub](https://github.com/pkalin/shadewright){ .md-button }

</div>

Shadewright watches your cluster, groups repeated symptoms into incidents, asks an LLM for a diagnosis, and proposes a fix as a pull request to your GitOps repository. It never changes the cluster directly: your review rules decide what merges, and your GitOps controller delivers it.

!!! warning "Early"
    Built so far: the API and its validation, the policy check with source mapping and the freshness check, the YAML patchers, the fingerprint and redaction functions (not called by any binary yet), the GitHub and Key Vault adapters (used to read Git; no pull request is opened yet) and the RBAC of both identities. Not built: the collector, the analysis, the pull request itself and the verification. The [design](design.md) says what holds at each step; the [decisions](adr/index.md) say why.

## How it works

<div class="grid cards" markdown>

-   **1. Observe**

    Events and object status in the namespaces you choose, deduplicated into incidents.

-   **2. Diagnose**

    Rules first, then a model with read-only tools and redacted context. No evidence, no proposal.

-   **3. Propose**

    A typed `RemediationRequest` from a closed catalog of actions, checked by deterministic policy.

-   **4. Open a PR**

    On a branch, in the one repository it may write to, with the evidence and a rollback plan.

-   **5. Verify**

    After merge, it follows Flux to the applied revision and checks that the symptom is gone.

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
