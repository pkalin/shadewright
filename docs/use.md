---
title: Use
---

# Use

What you do with Shadewright once it is installed: understand what it proposes, review it, and find out why a request stopped.

- [Concepts](concepts.md): the objects and parts, and how they relate.
- [The life of a request](request-lifecycle.md): each phase and condition, and what you can do in it.
- [How-to guides](how-to/index.md): review and approve, reject, write a policy, pause, limit namespaces, read an Incident, find why a request was refused.
- [Your first proposal, to a reviewed pull request](first-proposal.md): the hand-written request, the pull request it opens, and what the phases mean.
- [Troubleshooting](troubleshooting.md): every condition reason of a `RemediationRequest` with its cause and fix.
- [Emergency stop](emergency.md): pause the executor and stop the analysis.
- [FAQ](faq.md) and [Glossary](glossary.md).

The concepts are explained in the design: the [request lifecycle](design/6-reconciliation.md#6-reconciliation), the [API objects](design/5-api.md#5-api) (`RemediationRequest`, `Incident`, `RemediationPolicy`, `RemediationSettings`), [what `approvalRequired` means](design/8-gitops-on-github.md#81-what-approvalrequired-means) and [how a change is made](design/8-gitops-on-github.md#82-making-a-change). The fields of each object are in the [API reference](reference/api.md).
