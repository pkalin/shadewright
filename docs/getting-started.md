---
title: Get started
---

# Get started

Two steps take you from nothing to a reviewed pull request, in this order. Each page lists what it needs and how to check it worked. Changing Shadewright itself is a different path: see [Contribute](contribute.md).

!!! warning "What exists today"
    Shadewright is at v0.1.0. Built: the collector, the Incident controller, platform and module detection (all in the analyzer), and the executor's whole path from a `RemediationRequest` to an open pull request waiting for a person (policy check, source mapping, freshness check, branch, commit, pull request). **Not built:** the analysis that calls a model (so nothing proposes a request by itself), auto-merge, delivery tracking, verification, and the `Cancelled` phase. Every request is written by hand today. The Helm chart ([#83](https://github.com/pkalin/shadewright/issues/83)) is on `main` and is published to GHCR from the pre-release 0.2.0-rc.1 on (v0.1.0 has no chart). The [README](https://github.com/pkalin/shadewright#readme) and the [threat model](threat-model.md) list what is built and planned.

1. [Install in observe-only mode](install.md): the analyzer watches the namespaces you name and records `Incident` objects; nothing is proposed and nothing can write to Git. Use the release manifests with kustomize, or the Helm chart.
2. [Your first proposal, to a reviewed pull request](first-proposal.md): a hand-written `RemediationRequest` goes through the policy check and source mapping to one pull request that you review and merge. The complete, tested walk-through is the [lab runbook](lab-oom-acceptance.md).

Before you install, [Overview](overview.md) says what Shadewright does and does not do, and [Compatibility](reference/compatibility.md) lists the Kubernetes, Flux and KEDA versions it was tested with. To stop everything quickly at any point, see the [emergency stop](emergency.md).
