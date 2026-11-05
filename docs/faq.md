---
title: FAQ
---

# FAQ

Short answers, each pointing to the page that has the detail. Terms are in the [glossary](glossary.md).

## What will it never do?

These are the project's invariants ([design 10](design.md#10-security), [threat model](threat-model.md)); a change that weakened one would need a new ADR first.

- **Apply anything directly.** It has no direct `apply`, `patch`, `delete` or `exec` against your workloads, not even as a fallback when a pull request fails. The only path to a change is a branch and a pull request ([ADR-0003](adr/0003-gitops-only-typed-remediation.md)).
- **Run a free-form command.** Actions are a closed catalog (today `SetContainerMemoryLimit` and `AdjustKedaMaxReplicas`), each with typed, bounded parameters.
- **Read or write Secrets.** Neither identity can read Secrets. Secrets and tokens are redacted before anything is stored. The remediation path cannot change policies, pipelines, RBAC or secrets.
- **Treat a model's answer as permission.** A model can only submit a proposal. Deterministic policy decides whether it may run, and the executor acts only on the plan whose digest matches the one it decided on. Everything read from the cluster is untrusted input and never changes its instructions or tools.
- **Merge on its own authority.** Pull requests are merged by a person (auto-merge is <span class="sw-status sw-planned">planned</span>, not built), and your branch rules still apply.
- **Push to a protected branch or bypass checks.** It writes only to branches named `shadewright/<request UID>`.

## What does it cost to run?

The operator is two small Deployments. In the current release nothing calls a model, so there is no inference cost today. When the analysis is built, the cost is tokens times price per analysis, bounded by `spec.analysis.maxAnalysesPerDay` (default 100) and by planned token caps. [Design 12](design.md#12-cost) has the formula and an illustration with made-up prices. The reference lab in Azure is disposable and priced in the `shadewright-azure` repository ([ADR-0008](adr/0008-disposable-azure-lab.md)).

## What data leaves the cluster?

Today, only calls to GitHub (reads of files and branch heads in the one configured repository, and the branch, commit and pull request it writes), to Azure Key Vault (a digest to sign) and to Microsoft Entra ID (the workload identity token). Nothing is sent to a model yet, and there is no telemetry. Details, redaction and retention: [data handling](data-handling.md).

## Which platforms work?

- **Kubernetes:** tested on the versions in [compatibility](reference/compatibility.md); the API server must support CEL validation.
- **GitOps:** Flux only, with a plain-manifest layout and a `GitRepository` that tracks a branch. Argo CD is <span class="sw-status sw-planned">planned</span>. Helm releases are not mapped.
- **Git host:** GitHub.com only, through a GitHub App. GitLab and Azure DevOps are planned.
- **Cluster platforms and add-ons:** generic and AKS modules, EKS and GKE (detection only), and the cloudflared, Flux, KEDA, Kyverno and Secrets Store CSI driver integrations. The actions available today target Deployments, StatefulSets and KEDA ScaledObjects.

## Can I try it without risk?

Yes: install in [observe-only mode](install.md). It records Incidents and has no Git credential, so it cannot open a pull request.

## It proposed nothing. Why?

The analysis that diagnoses and proposes is not built yet, so today requests are written by hand ([your first proposal](first-proposal.md)). To see what the analyzer saw, [read an Incident](how-to/read-an-incident.md).

## A request stopped. Where do I look?

[Find why a request was refused](how-to/find-why-refused.md), then [troubleshooting](troubleshooting.md).

## How do I stop it right now?

[Pause and resume writes](how-to/pause-and-resume.md) or the [emergency stop](emergency.md).
