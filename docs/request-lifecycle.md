---
title: The life of a request
---

# The life of a request

What a `RemediationRequest` goes through, what you see at each step, what moves it on and what you can do. This summarises [design 6](design/6-reconciliation.md#6-reconciliation); every condition reason is explained in [troubleshooting](troubleshooting.md).

The steps after `Merged` are built: the merge receipt, the wait for delivery, the executor's own checks after it, and `Cleared`, which the executor judges from the verifier's `VerificationReport`, so a request can end `Succeeded`. They run only when the executor runs with `--verify-outcomes` (chart value `executor.verification.enabled`), which is off by default until a lab run with a release candidate decides it. With it on and no verifier deployed, no report is written and every merge ends `Inconclusive` (`StaleReport`) or `Failed`. Off, a merged request stays `Merged` and `RolledOut` reports its rollout, except a plan merged after it was known stale, which ends `Inconclusive` (`StalePlanMerged`) once its rollout check is over.

```mermaid
stateDiagram-v2
    [*] --> Pending
    Pending --> Pending: queued by a limit, KEDA has not reported yet, or Flux is reconciling
    Pending --> PolicyChecked: policy allows, source found
    Pending --> Denied: policy refuses
    Pending --> NeedsIntervention: source unclear or ScaledObject not ready
    Pending --> Superseded: another request of the Incident was merged
    Pending --> Superseded: the pull request it waited behind was closed without a merge
    PolicyChecked --> PRCreated: branch, commit, pull request
    PRCreated --> AwaitingApproval: review required
    AwaitingApproval --> Merged: a person merges it
    PolicyChecked --> Merged: an existing merged pull request is adopted
    PRCreated --> Merged: merged before the first review step
    PolicyChecked --> Expired: no pull request in 30 minutes
    PolicyChecked --> Superseded: a plan input changed
    PRCreated --> Superseded: a plan input changed
    AwaitingApproval --> Superseded: a plan input changed
    PolicyChecked --> NeedsIntervention: cannot continue alone
    PRCreated --> NeedsIntervention: cannot continue alone
    AwaitingApproval --> NeedsIntervention: pull request closed, head moved or not found
    Merged --> Inconclusive: no merge receipt (NoMergeReceipt)
    Merged --> Inconclusive: a stale plan merged, once its rollout check is over, --verify-outcomes off (StalePlanMerged)
    Merged --> WaitingForDelivery: next reconcile, with --verify-outcomes
    WaitingForDelivery --> Verifying: Flux applied the merge commit
    WaitingForDelivery --> Failed: Flux failed on the merge commit (DeliveryFailed)
    WaitingForDelivery --> Inconclusive: not delivered within the hour, the target is gone, or --verify-outcomes off
    Verifying --> Failed: an OOM kill in the verifier's report (OOMKilled), or at the timeout rollout stuck, ScaledObject not ready or restarts (RolloutStuck, KedaNotReady, Restarted)
    Verifying --> Inconclusive: value changed, template replaced, target gone or unreadable, a ScaledObject after stableFor, no evidence at the timeout, or --verify-outcomes off
    Verifying --> Inconclusive: a quiet window for a plan merged after it was known stale (StalePlanMerged)
    Verifying --> Succeeded: a quiet window, Cleared True
    Denied --> [*]
    Superseded --> [*]
    Expired --> [*]
    NeedsIntervention --> [*]
    Merged --> [*]
    Failed --> [*]
    Inconclusive --> [*]
```

## What each phase means and what you do

| Phase | What happened | What moves it on | You can |
| --- | --- | --- | --- |
| `Pending` | New, or held back (condition `Queued`: the concurrency limit, the circuit breaker, or another request that holds the lock on the same target until its pull request is known merged or closed, also after it stopped) | The policy check, rechecked every 30 seconds while queued. While Flux is still reconciling the Kustomization (`PolicyAllowed=Unknown`, reason `KustomizationReconciling`) it waits up to 10 minutes | Wait, or read the reason in [troubleshooting](troubleshooting.md#limits) |
| `PolicyChecked` | Policy allowed it (`PolicyAllowed=True`, reason `WithinApprovedBounds`); the plan digest is fixed | The pull request step | Nothing. It expires after 30 minutes without a pull request |
| `PRCreated` | The branch, commit and pull request exist | The executor records the review state | Open the pull request |
| `AwaitingApproval` | `Approved=False`, reason `WaitingForReview` | A person merges the pull request, seen within about a minute (`--pull-request-poll-interval`) | [Approve](how-to/review-and-approve.md) or [reject](how-to/reject-a-proposal.md) |
| `Merged` | The pull request was merged; `status.pullRequest.mergeCommitSHA` records the merge commit that delivery will be checked against (that commit or a later one of the target branch that holds it), and `status.mergeReceipt`, written in the same status write and never changed after, binds it to the repository, target branch, pull request number and head ([ADR-0030](adr/0030-merge-receipt-and-verification-outcomes.md)). With `--verify-outcomes` the next reconcile moves it to `WaitingForDelivery`; off (the default) it stays here and `RolledOut` reports whether its pods rolled out. A request merged before receipts existed stays here, gets `Verified=Unknown`, reason `MergedBeforeVerification`, once, and `RolledOut` reports whether its pods rolled out | The next reconcile, with `--verify-outcomes` | Nothing |
| `WaitingForDelivery` | Merged; the executor waits for the Flux Kustomization that applies the target to report the merge commit, or a later commit of the target branch that holds it, applied. `status.deadline` is one hour after the merge, extended by the time writes were paused. `Delivered=Unknown`, reason `DeliveryPending` | Flux applying it (to `Verifying`), or the end of the hour: the last read decides (`Failed` with `DeliveryFailed` when Flux failed on the merge commit itself, else `Inconclusive`); with `--verify-outcomes` turned off, `Inconclusive` (`VerificationDisabled`) at the next reconcile | Check your GitOps controller ([after the merge](#after-the-merge-did-it-reach-the-pods)) |
| `Verifying` | Delivered (`Delivered=True`); every 30 seconds the executor reads the target again: `Configured` (the live field holds the desired value) and `RolledOut` (the pods run the applied template), and the verifier's report, from which it judges `Cleared` (`Unknown`, `Observing`, while the window runs; its own window is `status.verification`). `status.deadline` is the timeout, 21 minutes after the delivery for a memory fix with the defaults | A quiet window for the effective `stableFor` plus `--node-grace` (11 minutes for a memory fix) on the executor's checks and in a fresh report ends it `Succeeded`; an OOM kill the report records ends it `Failed` (`OOMKilled`) at once; a changed value, template or target ends it `Inconclusive` at once; a ScaledObject ends `Inconclusive` (`ClearingNotChecked`) once ready and configured for `stableFor`; a revert (`spec.reverts`) reads no report and ends `Inconclusive` (`RevertApplied`) once rolled out, configured and available for `stableFor`, never `Succeeded`; at the timeout it ends `Failed` (`RolloutStuck`, `KedaNotReady`, or `Restarted` when the report recorded restarts) and otherwise `Inconclusive` with the reason that held the window closed (`StaleReport`, `PodNotReady`, ...); with `--verify-outcomes` turned off, `Inconclusive` (`VerificationDisabled`) at the next reconcile | `kubectl shadewright explain` shows rolled out, configured and cleared |
| `Failed` | After the merge: Flux failed on the merge commit itself at the end of the delivery window (`DeliveryFailed`), the rollout was still stuck at the timeout (`RolloutStuck`), a ScaledObject target was not ready at the timeout (`KedaNotReady`), the verifier's report recorded an OOM kill of the action's container (`OOMKilled`, at once) or restarts after its window opened (`Restarted`, at the timeout); `Verified=False` says which, and `Cleared=False` for the last two | Nothing, final. The circuit breaker counts it as a failure | Look at the merged change; revert it with a new request or by hand |
| `Denied` | Policy or settings refuse the plan, or a revert's link to the failed request does not hold (`InvalidRevert`) | Nothing, final | [Find the reason](how-to/find-why-refused.md), fix, apply a new request |
| `NeedsIntervention` | The executor cannot continue safely alone: a pull request closed without a merge (`ChangeRequestClosed`), a merge conflict the Git host still reports two minutes after it first did (`ChangeRequestConflict`), a moved head (`HeadMoved`), a render refusal, a ScaledObject that is not ready | Nothing, final | Read the reason, then apply a new request if the change is still wanted |
| `Superseded` | A plan input changed before merge (policy, settings, target, file in Git). If a pull request was open, Shadewright closed it; when the Git host had refused that close and the pull request was then found closed, `PullRequestOpened` says it is not known whether Shadewright's retried close or a person closed it. Or, reason `AnotherRequestMerged`, another request of the same Incident, target and field was merged while this one waited, before it took the target lock or opened a pull request. Or, reason `AnotherRequestClosed`, the pull request of another request for the same target and field was closed without a merge while this one waited, so it opens none for that field | Nothing, final | Apply a new request; for `AnotherRequestMerged`, nothing: the merged request is the fix; for `AnotherRequestClosed`, nothing unless the change is still wanted: a new analysis proposes again if the symptom continues |
| `Expired` | No pull request opened within 30 minutes of the decision | Nothing, final | Find why the Git calls failed, apply a new request |
| `Inconclusive` | Merged, and neither success nor failure could be shown; `Verified=Unknown` says why: `NoMergeReceipt` (the Git host reported no full merge commit within five minutes of the executor first seeing the merge), `DeliveryNotObserved`, `DeliveryBlocked`, `DeliverySuspended`, `DeliveryNotChecked`, `TargetGone`, `TargetReplaced`, `ValueChanged`, `RolloutSuperseded`, `TargetNotChecked`, `ClearingNotChecked`, `CheckNotAvailable`, `StalePlanMerged` (a person merged a plan Shadewright knew was stale; never `Succeeded`), the no-evidence reasons of the stable window (`StaleReport`, `ReportMismatch`, `PartialRollout`, `NotAvailable`, `PodNotReady`, `EvidenceMissing`, `ScaledToZero`, `ObservationGap`), `RevertApplied` (a revert ran its course; the change it undid had failed, so the Incident stays with a person) or `VerificationDisabled` (see [troubleshooting](troubleshooting.md#after-the-merge)) | Nothing, final. The circuit breaker reads it like `Expired`; the Incident is escalated | Check by hand what the merge put on the target branch and whether it was applied ([troubleshooting](troubleshooting.md#merge-receipt)) |

The API also defines `AutoMergeQueued` and `Cancelled`. Nothing sets them yet (planned). `Succeeded` is set after `Verifying` (`Cleared=True` and `Verified=True`, reason `NoRecurrence`; `Cleared` has `ResolvedBeforeRollout` instead when the Incident's count did not move from the merge to the rollout); it is final, and the circuit breaker clears older outcomes by it. Auto-merge is not built: even with a policy that has `approval: Optional` and a request with `approvalRequired: false`, a person decides.

Conditions give the reasons: `PolicyAllowed` (the policy check), `PullRequestOpened`, `Approved`, `PlanFresh` (the plan is still current, checked every five minutes before merge), `Delivered`, `Configured` and `RolledOut` (after the merge) and `Verified` (after the merge: `False` for `Failed`, `Unknown` for `Inconclusive` and for `MergedBeforeVerification`). A request that stopped and keeps the lock on its target because another pull request is open on its branch has `TargetHeld`, which names that pull request. A pause adds `WritesPaused`, see [pause and resume](how-to/pause-and-resume.md).

## After the merge: did it reach the pods?

A merge, and even Flux applying it, does not mean the pods run the fix. <span class="sw-status sw-built">built</span> With `--verify-outcomes` off (the default), and for a request merged before receipts existed: for a Deployment or StatefulSet target, once the Flux Kustomization that applies it reports the merge commit (or a later commit of the target branch that holds it) applied, the executor records that time in `status.pullRequest.appliedObservedAt`, reads the target's rollout status and sets `RolledOut`: `True` when every pod runs the new template, `False` with reason `RolloutStuck` and one Event when the rollout has not completed 10 minutes after the revision was applied, naming the StatefulSet pods still on the old revision and the manual step (it names a pod to delete only when it is certain the pod is not Ready). It reports only: deleting or restarting a pod stays your decision. A delivery not seen within an hour of the merge gives `False` with reason `DeliveryNotObserved` and one Event, naming the Kustomization and the merge commit; another target kind gives `Unknown` with reason `KindNotChecked`. A stuck rollout is read until an hour after its deadline, so one you unblock turns `True`; the check always ends with a verdict, `Unknown` with reason `RolloutSuperseded` when a newer template revision replaced the one applied with the merge, or `RolloutNotDetermined` when the target is gone or cannot be read. See [troubleshooting](troubleshooting.md#rollout-after-the-merge).

## What approving means

Shadewright never approves its own pull request and does not check reviews itself. Approval is your normal review on GitHub, enforced by your branch rules (required reviews and checks, `CODEOWNERS`). When a person merges, the executor sees it and ends the request `Merged`. See [design 8.1](design/8-gitops-on-github.md#81-what-approvalrequired-means).

## When the plan changes after the pull request

What happens depends on the cause. Only these cases close the pull request, and Shadewright never reopens one:

- A stale plan (a changed policy, settings, target or source file) ends `Superseded` and the executor closes its pull request.
- A render refusal while the pull request waits (`TargetFileDiverged`, `PatchRefused`, `BranchDiverged`, an unplanned difference in the rendered output) closes it and ends `NeedsIntervention`.
- A branch that does not hold exactly the planned change when the pull request is opened or found before it was recorded (`BranchDiverged`, for example a push to the branch during the open) closes it and ends `NeedsIntervention`.
- A push to a recorded pull request that leaves more than the planned change on the branch (`BranchDiverged`) ends `NeedsIntervention` with one comment on the pull request that hands it to a person; it stays open. Merged by then, the request ends `MergedWithForeignChange`.

These stop the automation but leave the pull request open, so a person closes it (or merges it knowingly): `HeadMoved` (the head moved to the same planned change), `ChangeRequestConflict` (the Git host reports a merge conflict and the plan is still fresh), `ChangeRequestGone`, `ForeignChangeRequest`. The way back to the change is a new request. See [troubleshooting](troubleshooting.md#pull-request-step).

## Read it yourself

```bash
kubectl -n shadewright-system get remediationrequest
kubectl -n shadewright-system get remediationrequest <name> \
  -o jsonpath='{.status.phase}{"\n"}{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
```

!!! note "Not yet run"
    The commands on this page and in the how-to guides were not run against a live cluster for these pages (no kind cluster was available). The `get` and `jsonpath` forms are the ones already used in [troubleshooting](troubleshooting.md) and the [lab runbook](lab-oom-acceptance.md); the short name `rr` and the printed columns (Phase, Policy, Action, Target, PR, Age) come from the API types.
