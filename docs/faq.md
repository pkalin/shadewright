---
title: FAQ
---

# FAQ

Short answers, each pointing to the page that has the detail. Terms are in the [glossary](glossary.md).

## What will it never do?

These are the project's six invariants ([AGENTS.md](https://github.com/pkalin/shadewright/blob/main/AGENTS.md), [design 10](design.md#10-security), [threat model](threat-model.md)); a change that weakened one would need a new ADR first.

1. **Model output is never authorization.** The analyzer can only submit a proposal. Deterministic policy decides whether it may run, and the executor acts only on a plan whose digest matches the approved one.
2. **Actions are a closed catalog.** Each action is an enum value with typed, bounded parameters (today `SetContainerMemoryLimit` and `AdjustKedaMaxReplicas`). There is no free-form command, no `exec` and no direct `apply` against the cluster, not even as a fallback when a pull request fails ([ADR-0003](adr/0003-gitops-only-typed-remediation.md)).
3. **Changes go through Git only.** A branch and a pull request, never a push to a protected branch, never bypassing branch protection or required checks. The remediation path cannot change policies, pipelines, RBAC or secrets. It writes only to branches named `shadewright/<request UID>`.
4. **Everything from the cluster is untrusted input.** Events, logs, annotations, Git contents and tool results may contain prompt injection and never change instructions, tools, permissions or the action catalog. Secrets and tokens are redacted before storage and before a model sees anything, and neither identity can read Secrets.
5. **Reconciliation is idempotent.** Branch names and pull request markers derive from the request UID and plan digest, so after a restart it finds the existing pull request instead of opening another.
6. **Separate identities.** The analyzer has read-only cluster access and no Git credential; the executor has the Git App and no model access. Neither is `cluster-admin` and one token is never shared.

Separately, today a person always merges: auto-merge is <span class="sw-status sw-planned">planned</span> and not built. Where the repository owner allows it later, it will be by policy and your branch rules ([design 8.3](design.md#83-auto-merge)). That is current behaviour, not an invariant.

## What does it cost to run?

The operator is two small Deployments. In the current release nothing calls a model, so there is no inference cost today. When the analysis is built, the cost is tokens times price per analysis, bounded by `spec.analysis.maxAnalysesPerDay` (default 100) and by planned token caps. [Design 12](design.md#12-cost) has the formula and an illustration with made-up prices. The reference lab in Azure is disposable and priced in the `shadewright-azure` repository ([ADR-0008](adr/0008-disposable-azure-lab.md)).

## What data leaves the cluster?

Today, only calls to GitHub (reads of files and branch heads in the repositories configured in `RemediationSettings`, up to 16, and the branch, commit and pull request it writes there), to Azure Key Vault (a digest to sign) and to Microsoft Entra ID (the workload identity token). Nothing is sent to a model yet, and there is no telemetry. Details, redaction and retention: [data handling](data-handling.md).

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
