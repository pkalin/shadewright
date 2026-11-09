---
title: Review and approve a proposal
---

# Review and approve a proposal

A proposal reaches you as a pull request in your GitOps repository, made by the Shadewright GitHub App. Approving it is your ordinary review and merge. Background: [the life of a request](../request-lifecycle.md) and [design 8.1](../design/8-gitops-on-github.md#81-what-approvalrequired-means).

## Steps

1. Find the request waiting for you. It is in phase `AwaitingApproval`, and the `PR` column shows the pull request number.

    ```bash
    kubectl -n shadewright-system get remediationrequest
    ```

2. Open the pull request. The branch is `shadewright/<request UID>`, the title is `fix: set <what> of <namespace>/<name> to <value>`, and the body states the change (from and to), the scope (one value in one file), the evidence (request, incident, policy, plan digest), the expected effect, the risk and the rollback (revert the pull request).
3. Check what a reviewer should check:
    - The diff is one scalar in one file, and that file is the one you expect for the workload.
    - The new value is what you want, and sensible for the workload, not only inside the policy's bounds. The policy is a ceiling, not a recommendation.
    - Your required checks are green.
4. Approve and merge it as your repository rules require (a required review, `CODEOWNERS`, checks). Shadewright does not approve or merge, and with auto-merge not built (<span class="sw-status sw-planned">planned</span>) a person always merges, whatever the policy's `approval` says.
5. Within about a minute (the executor's `--pull-request-poll-interval`) the request moves to `Merged`. Delivery is then your GitOps controller's job. The executor follows it: the request's `RolledOut` condition says whether the change reached the pods, or why not (see [Rollout after the merge](../troubleshooting.md#rollout-after-the-merge)). Verifying that the fix worked is <span class="sw-status sw-planned">planned</span>, so check the symptom yourself.

## Do not push to the branch

Any other push to the Shadewright branch stops the automation (`HeadMoved`, phase `NeedsIntervention`). If the change needs editing, [reject](reject-a-proposal.md) it and apply a new request.

!!! note "Not yet run"
    The `kubectl` command is not yet run for this guide. The pull request title, body fields and branch name come from `internal/controller/pullrequest.go`; the walk-through run in the lab is the [OOM acceptance runbook](../lab-oom-acceptance.md).
