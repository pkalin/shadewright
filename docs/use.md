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
- [Give a reviewer hint](#give-a-reviewer-hint): steer a proposal from its pull request (not enabled yet).
- [FAQ](faq.md) and [Glossary](glossary.md).

The concepts are explained in the design: the [request lifecycle](design/6-reconciliation.md#6-reconciliation), the [API objects](design/5-api.md#5-api) (`RemediationRequest`, `Incident`, `RemediationPolicy`, `RemediationSettings`), [what `approvalRequired` means](design/8-gitops-on-github.md#81-what-approvalrequired-means) and [how a change is made](design/8-gitops-on-github.md#82-making-a-change). The fields of each object are in the [API reference](reference/api.md).

## Give a reviewer hint

**Not enabled in the shipped binary yet.** The code is on `main` ([ADR-0028](adr/0028-reviewer-hints-through-the-pull-request.md)), but `cmd/main.go` never turns the executor's `ReviewerHints` setting on, so a comment is not read until the tasks #590, #591 and #592 land. What follows is what happens once it is on.

**How.** Comment on the pull request Shadewright opened. The first line must start exactly with `/shadewright reanalyze:`, followed by your hint on the same line:

```text
/shadewright reanalyze: the leak is in the sidecar, not the app
```

- Only a conversation comment counts, not an inline review comment. A command that is quoted, indented or on a second line is not a hint. Do not edit a command into an old comment: post a new one.
- Only a person with write access to the repository can give a hint. Bots and Apps are ignored, the executor's own comments included. A command from someone without write access is ignored, with no reply and no record.
- The text is redacted and kept in the request's status. It is at most 2048 bytes after redaction. A chain takes at most 3 hints, and a request keeps at most 10 in all.
- The executor polls, so a hint is seen within about the pull request poll interval (`--pull-request-poll-interval`), not at once.

**What happens.** The analysis runs again for the Incident with your hint shown as a reviewer's opinion. It costs one unit of the daily analysis budget and the cooldown applies. If it proposes a different plan that policy allows, a new request replaces the old one and takes over the same pull request and branch with one new commit. No second pull request is opened and the old one is not closed. The old request ends `Superseded` with reason `HandedOff`. A hint never authorizes anything: policy checks the new plan like any other.

If the analysis proposes nothing or the same plan, or is refused (budget spent, cooldown), the pull request keeps the old plan. The hint is then `Declined` with that reason in `status.hints` of the request, and `status.handledHint` of the Incident shows the hint last answered. A hint with no answer after an hour is declined as `NoAnswer`. If the pull request is merged or closed first, the hint is declined too. The reasons are in [troubleshooting](troubleshooting.md#not-request-reasons).

**Replies.** The executor does not reply to a hint in the pull request yet ([#542](https://github.com/pkalin/shadewright/issues/542)). Read `status.hints` and `status.refusedHints` of the request (`kubectl get remediationrequest <name> -o yaml`) to see what became of a comment. The one comment it does post is on an approval by a hint author (below).

**The merge gate.** The executor creates a check run `shadewright/review` on every head of its pull requests. It passes only when the latest head has an approving review from someone who wrote no hint in the chain. If you gave a hint, your own approval does not count and you get a comment saying so, once per review: another reviewer has to approve. An approval of an earlier head does not count either. The gate holds only if the repository's ruleset requires `shadewright/review` with the Shadewright App as the expected source. Shadewright cannot check that rule. Without it the check informs and does not block, so do not turn hints on for that repository. The App also needs the `checks: write` permission.
