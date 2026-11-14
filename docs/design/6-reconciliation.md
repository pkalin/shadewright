# 6. Reconciliation

[![Shadewright executor: a RemediationRequest passes the policy's field checks, the source mapping, the full policy decision, the limits, the freshness check, the patcher with the semantic diff and the kill switch gate; the Key Vault signer lets the GitHub App open the pull request, the request waits in AwaitingApproval and reaches Merged when a person merges, with the merge receipt; with --verify-outcomes (off by default) it then waits in WaitingForDelivery until Flux applies the merge commit and is checked in Verifying (Delivered, Configured, RolledOut), ending Succeeded, Failed or Inconclusive; refusals end in Denied, NeedsIntervention or Superseded; built up to Succeeded, with the executor judging cleared from the verifier's report; a revert's link check before the field checks and its end Inconclusive with RevertApplied are built and not drawn, as is the analyzer proposing a revert; auto-merge is planned.](../assets/diagrams/executor.svg)](../assets/diagrams/executor.svg "Open full size")

[![RemediationRequest phases: Pending, PolicyChecked, PRCreated, AwaitingApproval or AutoMergeQueued, Merged, WaitingForDelivery, Verifying, Succeeded; built up to Succeeded, the steps after Merged only with --verify-outcomes (off by default), with AutoMergeQueued planned; a step before the merge can end in Denied, Failed, Expired, Cancelled, Superseded or NeedsIntervention; after the merge a request ends Succeeded, Failed or Inconclusive, never Expired.](../assets/diagrams/phases.svg)](../assets/diagrams/phases.svg "Open full size")

This section covers how a `RemediationRequest` moves through its phases: the phases and transitions (6.1), the policy check that decides a request (6.2), retries and deadlines (6.3), the shape of every reconcile (6.4), the pull request step (6.5), the checks that run before every write (6.6) and what happens when a request is deleted (6.7). Numbers and edge cases that only matter when you build or debug are in the table "Details" at the end (6.8).

The steps after `Merged` are built (#678, #681, #683): the merge receipt, the delivery window, the executor's own checks after the delivery, and `Cleared` judged from the verifier's report (#682), ending `Succeeded`, `Failed` or `Inconclusive`. They run only with the executor flag `--verify-outcomes` (chart value `executor.verification.enabled`), off by default until a lab run with a release candidate decides the default. On with the verifier deployed, a request can end `Succeeded`; on without a verifier, no report is written and every merge ends `Inconclusive` (`StaleReport`) or `Failed`, so the circuit breaker would not close again once tripped. Off, a merged request keeps `Merged` with its rollout report (#532).

Other terminal or waiting phases: `Denied`, `Failed`, `Expired`, `Cancelled`, `Superseded`, `NeedsIntervention`, `Inconclusive` (merged, and neither success nor failure could be shown, [ADR-0030](../adr/0030-merge-receipt-and-verification-outcomes.md) section 5). Conditions explain why; `phase` is a summary for humans.

## 6.1. Phases and transitions

The API defines every phase below. The executor sets the first step: a `Pending` request (or one without a phase yet) becomes `PolicyChecked`, `Denied` or `NeedsIntervention` in one status write.

What happens next, step by step:

- **Held back by a limit.** A request the policy allows stays `Pending` with the condition `Queued` while a limit of section 10.7 holds it back. It is checked again every 30 seconds, or every 10 minutes (and when the holder changes) behind a lock held by a request that stopped.
- **A duplicate of a merged request.** An undecided request whose Incident and target already have a merged request ends `Superseded` (`AnotherRequestMerged`) before it takes the target lock (6.6.3).
- **After a pull request closed without a merge.** An undecided request that waited behind the target lock of another request for the same target and field, whose pull request was then closed without a merge, ends `Superseded` (`AnotherRequestClosed`) before it takes the lock (6.6.4).
- **Freshness before merge.** The executor re-checks the freshness of a decided request before merge every five minutes, and at once on any change to the request (once its pull request waits for review, only when no full recheck ran in the last five minutes). While a pull request waits for review, the executor only looks at the pull request in between, every `--pull-request-poll-interval` (one minute by default), so a merge or a close is seen within that interval (#434); a look writes nothing to Git, so every write still comes right after a freshness check. A stale plan becomes `Superseded`, or `NeedsIntervention` if its repository settings became unusable, without waiting for a write.
- **Opening the pull request.** A fresh `PolicyChecked` request opens its pull request and moves to `PRCreated` and `AwaitingApproval` (6.5).
- **Merge.** Observing a person's merge is built: the request then reaches `Merged`, with the merge receipt in the same status write; a merge without a full merge commit ID ends `Inconclusive` (`NoMergeReceipt`) on the next reconcile (step 5 of 6.5).
- **Delivery and verification.** Built (#681, [ADR-0030](../adr/0030-merge-receipt-and-verification-outcomes.md) sections 2 and 4), behind `--verify-outcomes` (off by default; off, a request found in `WaitingForDelivery` or `Verifying` ends `Inconclusive` with `VerificationDisabled` at its next reconcile, so none stays there). A `Merged` request with a merge receipt moves to `WaitingForDelivery` on the next reconcile, with `status.deadline` at the end of the delivery window: one hour after `mergeReceipt.mergedAt`, extended by the time writes were paused. Delivery is asked against the receipt (its merge commit on its target branch, and the ancestry of a later applied commit in the receipt's repository), never the live settings. When Flux reports the merge commit, or a later commit that holds it, applied, one status write records `appliedObservedAt` and the rollout fields, sets `Delivered` `True` and moves the request to `Verifying` with `status.deadline` at the timeout: the larger of `spec.verification.timeout` and the rollout deadline plus the effective `stableFor` plus `--node-grace`, 21 minutes for a memory fix with the defaults. At the end of the window without that, the last read decides: Flux failing on exactly the merge commit ends `Failed` (`DeliveryFailed`); a failure on a later commit that holds it `Inconclusive` (`DeliveryBlocked`); a suspended Kustomization `DeliverySuspended`; anything else `DeliveryNotObserved`; reads that fail to the end of the window plus 10 minutes `DeliveryNotChecked`, a Flux that keeps moving between applied commits while it is read included. A cause that does not clear by itself (the receipt's repository gone from the settings or unusable, a Git provider without an ancestry check, no delivery verifier) is retried too, and named in `Delivered` and the final message. A failure inside the window is not final, since Flux retries. Every reconcile in `Verifying` (every 30 seconds) reads the target again: a live field without the desired value ends `Inconclusive` (`Configured` `False`, `ValueChanged`), a pod template or revision other than the one recorded ends `RolloutSuperseded`, and `RolledOut` follows the rollout (`RolloutPartitioned` for a partitioned StatefulSet, naming the pods left on the old revision). A target that is gone ends `TargetGone` and one with another UID `TargetReplaced`, at once, in either phase. A field that cannot be read on the target ends `Inconclusive` (`TargetNotChecked`), not `ValueChanged`. Each check then judges `Cleared` (#683, ADR-0030 sections 4 and 7): it reads the verifier's `VerificationReport` of the request (`get`, operator namespace) as untrusted evidence. An OOM kill of the action's container recorded in a report bound to the request ends `Failed` (`OOMKilled`) at once. A check is good when `RolledOut` is `True`, every desired replica is available (else `NotAvailable`) and the report passes the trust contract: missing, never sampled or with a sample counter that has not risen for 90 seconds by the executor's clock is `StaleReport`, another request, merge commit, target UID, template revision or container is `ReportMismatch`, coverage short of `spec.replicas` is `PartialRollout`. A good check opens the executor's own window (`status.verification`: `windowStart`, `lastReset`, `resets`, forward only by CEL), a bad one resets it. The request ends `Succeeded`, with `Cleared` and `Verified` `True` (`NoRecurrence`; `Cleared` gets `ResolvedBeforeRollout` instead under the rule below) in the same status write, when a good check finds both spans at least the effective `stableFor` plus `--node-grace` (the report's on the verifier's clock, the executor's on its own) and the report's counter higher than at a check at most 30 seconds earlier; once both spans are long enough the executor checks every 15 seconds. The counter is kept in memory. A check whose status write fails keeps the reset it found in memory, and the next check applies it again to the window it reads, so a lost write cannot let a window span the bad check it missed and end a request, a revert or a ScaledObject included. A restart or a leader change loses that memory, so no window counts time from before the executor process began reconciling (taken once on its clock, when it wins leader election, which on a standby that becomes leader later is that moment, not its start): the span of every window, a revert's and a ScaledObject's included, runs from the later of the stored `windowStart` and that moment, and the stored `windowStart` is kept as it is. The cost: after a restart or a leader change every open window needs a full `stableFor` (plus `--node-grace` for a pod target) again, so a request whose timeout comes sooner ends at the timeout `Inconclusive` with the last reset of its windows (without one `EvidenceMissing`, `ClearingNotChecked` for a ScaledObject), never `Succeeded` and never `Failed` for it; an executor that keeps restarting ends no verification before its timeout, the fail-closed side. A ScaledObject cannot succeed: once ready and configured for `stableFor` it ends `Inconclusive` (`ClearingNotChecked`), and a request naming `QueueBacklogDecreasing` ends `Inconclusive` (`CheckNotAvailable`) where it would succeed. At the timeout a request ends `Failed` (`RolloutStuck`) when `RolledOut` is `False`, `Failed` (`KedaNotReady`) when a ScaledObject target is not ready by the KedaReady rule, `Failed` (`Restarted`) when a report bound to the request recorded a restart above its baseline after its window opened, and otherwise `Inconclusive` with the reason that holds the window closed (for example `StaleReport`, or `PodNotReady` from the report for a pod not Ready without a restart). A report that cannot be read is retried, never read as missing, until 10 minutes past the timeout. A target that cannot be read until 10 minutes past the timeout ends `RolloutStuck` when that was recorded, else `Inconclusive` (`TargetNotChecked`). Each wait is measured on the executor's clock from a time it recorded itself; a clock set back by more than a minute ends the wait `Inconclusive` at once instead of stretching it, and one up to a minute behind (a leader on another node) does not. A request whose applied time the rollout report recorded in `Merged`, past its timeout when `--verify-outcomes` is turned on, ends `Inconclusive` (`ClearingNotChecked`). The circuit breaker reads `WaitingForDelivery` and `Verifying` as neither a success nor a failure (10.7), and with the flag on also a `Merged` request with a merge receipt, which has not moved to `WaitingForDelivery` yet: read as a success in that gap, a half-open probe that has just merged would close the breaker before its outcome is known. With the flag off `Merged` is a success, as before #681; a `Merged` request without a receipt is read as before (like `Inconclusive` with the `NoMergeReceipt` mark, a success otherwise). The kill switch gates writes only: while writes are paused every read still runs, and only the delivery window is extended. Past the merge a deadline never ends a request `Expired`.
- **Rollout after the merge with `--verify-outcomes` off, or of a request without a receipt.** Built (#532), reporting only, for every merged request while `--verify-outcomes` is off, and for a request merged before receipts existed (`MergedBeforeVerification`): the phase stays `Merged`. The executor waits up to an hour after the merge for the Flux Kustomization that applies a Deployment or StatefulSet target to report the merge commit (or a later commit of the target branch that holds it) applied, records that time once in `status.pullRequest.appliedObservedAt`, then reads the target's rollout status and sets `RolledOut`: `True` when every pod runs the applied template, `False` with reason `RolloutStuck` and one core Event when it did not within 10 minutes of that time. A delivery not seen within the hour gives `False` with reason `DeliveryNotObserved` and one Event; a target of another kind gives `Unknown` with reason `KindNotChecked`. It never acts on a pod (invariant 2) and uses only the `get` it already holds (invariant 6). A read that fails or a cancelled reconcile is never a verdict, but the check always ends with one: a stuck rollout is read again for an hour after the deadline, a newer template revision ends it with `RolloutSuperseded`, a gone or unreadable target with `RolloutNotDetermined`. The delivery is checked before the hour is judged, and time while writes are paused does not count against the hour (`status.rollout.pausedSince` and `pausedSeconds`), so a resume leaves the unused part of it. The rollout judged is the one the applied commit produced: right after Flux reports it applied, the executor reads the target and records, in the same status write as the applied time, the commit Flux applied (`status.rollout.appliedRevision`), the target's generation and a hash of its pod template, after asking Flux once more that it still reports the same commit applied (otherwise it looks again); the template revision is recorded once the target's controller observed that generation and the target's current one (a Deployment's revision annotation can name a later generation before its status does), and a template that changed again before then ends the check with `RolloutSuperseded`. A later commit of the target branch that holds the merge commit counts as ours, so the template it produced is judged. A change made outside Git (`kubectl edit`, another controller) that lands before that read cannot be told apart and is not covered. `status.rollout` is set once: CEL rules keep it and each of its fields, and `ended` never goes back to false, so neither the delivery window nor a final verdict can be reopened by a status write.
- **Resolved before the rollout.** Built (#684, [ADR-0030](../adr/0030-merge-receipt-and-verification-outcomes.md) section 4). The executor reads the request's Incident with its existing `get` and records its `status.count` twice in `status.verification.incidentCount`, each reading `{uid, count, readAt}` with `readAt` on its own clock, never changed once set but removable (CEL; an executor rolled back to a release without the field drops it, and a removed reading is never taken again): `atMerge` only in the status write that sets `Merged` with the receipt (when that read fails it stays absent for good, since a later reading could follow the change), and `atApplied` in the status write that sets `appliedObservedAt`, never later, and only by the process that took `atMerge` (after a restart or a change of leader in between the reason stays `NoRecurrence`). The read is skipped when the step's deadline leaves less than its own timeout plus one minute, so it never shortens the status write. `atMerge` takes its time after the read and `atApplied` before it, so the recorded span never exceeds the real one. `Cleared` `True` carries `ResolvedBeforeRollout` instead of `NoRecurrence` only when both readings exist, both UIDs equal `spec.incidentRef.uid`, the counts are equal and below 1000000 (saturated), and `atApplied.readAt - atMerge.readAt` is at least the effective `stableFor` plus twice the analyzer's aggregation window (`aggregation.DefaultWindow`, 5 minutes). A failed read never delays the merge, the delivery or the phase: the reading stays absent and the reason stays `NoRecurrence`. `Verified`, the phase and the circuit breaker do not change, and nothing branches on the reason.
- **A revert.** Built (#96, [ADR-0030](../adr/0030-merge-receipt-and-verification-outcomes.md) section 8): a request with `spec.reverts` has its link checked before the policy check (the named request exists with that UID, ended `Failed` with `Verified` False, is not a revert itself, has the same Incident, target, action type and container, its previous and desired are swapped exactly, and the revert's name derives from the failed UID), else it ends `Denied` with `InvalidRevert`; a failed read of the named request is retried. A valid revert goes through the policy check and the pull request like any request, always with approval required. In `Verifying` it reads no report: once rolled out, configured and available (at least one desired replica) held for `stableFor` it ends `Inconclusive` with `RevertApplied`, never `Succeeded`, with `Cleared` `Unknown` (`RevertApplied`); a check before the rollout completed resets its window (`PartialRollout`). The circuit breaker never reads a revert as a success, in any phase. The analyzer's deterministic revert proposer (`internal/analyzer/revert`, no model call) creates the revert of a request that ended `Failed` with `Verified` False: the same Incident, target and policy, the inverse action (`InverseAction`, the helper the link check compares against), approval required, named after the failed UID; it records its decision in the Incident's `status.revertsProposed` before the create (`Pending`, then `Created` once the revert exists) and never takes a second one, also after the revert was deleted. For a `Pending` entry it reads the revert's name first: a revert there that names the failed request moves the entry to `Created` whatever the checks say now. Otherwise every create, the first or a retried one, runs the same checks first, and right before it the Incident is read again (no create when the entry is no longer `Pending` or the Incident is being deleted); when a check says no, the revert's name is taken by a request with another link, the create is refused as invalid, or the failed request is deleted or its name taken by another UID, the entry moves to `Abandoned`, final, with a `reason` and one Warning event `RevertAbandoned`. `Pending` entries are also settled from the Incident's side, when the analyzer starts leading and whenever an Incident with one changes, so an entry whose request went away while no analyzer led does not stay `Pending`. CEL accepts a new entry only as `Pending`, lets a state move only from `Pending` to `Created` or `Abandoned`, sets `reason` exactly with `Abandoned` and never changes it, and a rule on the root keeps the status from being removed while the list has entries. It runs whether a triage model is set or not. It proposes a revert only when the request's `Verified` condition moved to False at most 24 hours ago and at most five minutes ahead of the analyzer's clock, so at start-up older failures are logged and left to a person. It makes none for a revert, for any other end (`Inconclusive` included), when the Incident or the target is gone (a Warning event on the request for the Incident), when the Incident is being deleted, when the target's kind is no longer served, or when the action cannot be inverted; a target read refused as `Forbidden` is retried with backoff until the 24 hour bound ends it; a full list (20) sets the Incident condition `RevertsStopped` with one Warning event, and a request at the revert's name that reverts something else is left as it is. Any other failed read or write is retried with backoff. The policy refuses the decrease of a revert by default (`DecreaseNotAllowed`); whether a policy may allow it is open.
- **Planned.** Enabling auto-merge is designed but not built. [ADR-0030](../adr/0030-merge-receipt-and-verification-outcomes.md) defines what comes after `Merged`: a merge receipt written with the phase (built, #678), delivery checked against it and the outcomes delivered, rolled out and configured as separate conditions (built, #681), cleared and the stable window (built, #683, from the verifier's report, #682), and the terminal phase `Inconclusive` for a merged change whose effect cannot be shown either way. The window logic lives in `internal/verification/clearing` (#679).

```mermaid
stateDiagram-v2
    [*] --> Pending
    state Active {
        state BeforeMerge {
            Pending --> PolicyChecked: allowed, source resolved
            PolicyChecked --> PRCreated: branch, commit, pull request
            PRCreated --> AwaitingApproval: review required
            PRCreated --> AutoMergeQueued: policy allows automation (planned)
            AwaitingApproval --> Merged: a person merged it (observed, merge receipt written)
            PRCreated --> Merged: merged before the first review step (observed)
            PolicyChecked --> Merged: an existing merged pull request is adopted (observed)
            AutoMergeQueued --> Merged: checks pass (planned)
        }
        Merged --> WaitingForDelivery: next reconcile with --verify-outcomes, deadline is the delivery window
        Merged --> Inconclusive: no merge receipt, NoMergeReceipt (next reconcile)
        WaitingForDelivery --> Verifying: Flux applied the merge commit or a descendant, Delivered True
    }
    WaitingForDelivery --> Failed: Flux failed on the merge commit at the end of the window, DeliveryFailed
    WaitingForDelivery --> Inconclusive: not observed, blocked, suspended, not checked, target gone or replaced, or --verify-outcomes off (VerificationDisabled)
    Verifying --> Verifying: a check that is not good resets status.verification (a repeat of the last reason on a closed window writes nothing)
    Verifying --> Failed: OOM kill in the report (OOMKilled); at the timeout RolledOut is False (RolloutStuck), the ScaledObject is not ready (KedaNotReady) or the report recorded a restart (Restarted)
    Verifying --> Inconclusive: value changed, template superseded, target gone, replaced or not readable, a ScaledObject after stableFor (ClearingNotChecked), at the timeout no evidence (StaleReport, PodNotReady, ...), or --verify-outcomes off (VerificationDisabled)
    Pending --> Denied: policy refuses
    Pending --> NeedsIntervention: source cannot be mapped, or the ScaledObject is not ready
    Pending --> Pending: KEDA has not reported on the ScaledObject yet, or Flux is reconciling the Kustomization (waits up to 10 minutes)
    Pending --> Superseded: another request of the same Incident and target was merged (6.6.3)
    Pending --> Superseded: a pull request for the same target and field was closed without a merge (6.6.4)
    Pending --> Pending: allowed, but a limit holds it back (Queued, checked again every 30 s, or 10 min behind a stopped holder)
    Verifying --> Succeeded: a fresh report of a quiet window for stableFor plus nodeGrace, Cleared True
    BeforeMerge --> Cancelled: cancelled before merge
    BeforeMerge --> Superseded: a plan input changed, the ScaledObject is no longer ready or the module was disabled
    BeforeMerge --> Expired: a step reached its deadline
    BeforeMerge --> Failed: a step failed
    BeforeMerge --> NeedsIntervention: cannot continue alone
    Succeeded --> [*]
    Denied --> [*]
    Failed --> [*]
    Expired --> [*]
    Cancelled --> [*]
    Superseded --> [*]
    Inconclusive --> [*]
```

## 6.2. The policy check

### What it does

The policy check is the first step. It loads the policy the request names, the `default` RemediationSettings and the live target, and then maps the target to its file in Git (ADR-0012).

It refuses in this order, and the first refusal decides:

1. A missing policy or settings object (`Denied`).
2. A kind the action cannot change, before the target is read (`NeedsIntervention`).
3. A missing target (`NeedsIntervention`) or one with another UID (`Denied`).
4. A field the live target does not set (`NeedsIntervention`), or a previous value that is not the live one (`Denied`, `PreviousValueMismatch`).
5. The policy's field checks (`internal/policy.Preflight`, section 5.3): request, action, module, target and bounds.
6. The source mapping, the first step that reads Flux objects or Git and so may need a GitHub token.
7. The full decision (`internal/policy.Decide`) on everything, the resolved source included.

Steps 1 to 5 read only the Kubernetes API, so a request refused there costs no Git read, no signing and no token.

The order is intended (#242): a refusal in step 5 wins over anything step 6 would have said, a stop for a person or a Git error worth a retry included, because the mapping does not run. A request refused in step 5 therefore has no `status.source`: no Git read happened.

### Why the target is read twice

The target is read again after the mapping, and the check starts over if its UID or resourceVersion changed. This way the previous value and the file in Git always describe the same object.

The same second read comes before a step 5 refusal by the policy's target check (kind, namespace, labels), the one field check that rests on what was read from the live object. A `Denied` is final, so a target that Flux relabelled after the first read starts the check over instead of being refused on its old labels. The other step 5 refusals depend only on the request, the policy and the settings, and need no second read.

### What it stores

It stores these fields together with the phase:

- `status.source`;
- `policyUID`;
- `policyRevision` (`name/generation`);
- `effectiveApprovalRequired`;
- the pull request step's `deadline` (unless writes are paused);
- the `PolicyAllowed` condition.

The write goes through the status subresource with the resourceVersion it read. A stale read ends in a conflict, so a decided request is never decided again.

### Metrics

- The decision is counted in `shadewright_policy_decisions_total` only after the status write succeeds. A lost answer undercounts and never counts twice.
- A denial is also counted by its reason, taken from a closed list of the reason constants of the controller, the policy rules and the source mapping. Any other text is counted as `Other`.
- Every reconcile error is counted by kind (timeout, conflict, cancelled, other) by a wrapper around the reconciler.
- The request phase gauge is served at scrape time by a leader-only runnable from the informer the controller already uses, with no API call.

All of them are listed in the [metrics reference](../reference/metrics.md).

### Findings

| Finding | Phase | `PolicyAllowed` |
| --- | --- | --- |
| Policy or settings object missing, target UID differs from the live object | `Denied` (`PolicyNotFound`, `SettingsNotFound`, `TargetMismatch`) | `False` |
| The previous value in the request is not the live value (the policy measures increases and decreases from it) | `Denied` (`PreviousValueMismatch`) | `False` |
| The live object does not set the field the action changes | `NeedsIntervention` (`FieldNotFound`) | `Unknown` |
| Source mapping denies (repository, branch, path or inventory) or `policy.Decide` denies | `Denied`, reason of the check | `False` |
| `policy.Preflight` denies (step 5), before the mapping; no `status.source` is written, since no Git read happened | `Denied`, reason of the check | `False` |
| `AdjustKedaMaxReplicas` without the `keda` integration enabled in `spec.modules` | `Denied` (`ModuleNotEnabled`) | `False` |
| `AdjustKedaMaxReplicas` on a `ScaledObject` that is definitely not ready (see "A `ScaledObject` that is not ready" below), checked after every check that denies | `NeedsIntervention` (`ScaledObjectNotReady`) | `Unknown` |
| `AdjustKedaMaxReplicas` on a `ScaledObject` KEDA has not reported on yet (no status, no `Ready` condition, `Ready=Unknown` other than `PartialTriggerError`) | no phase: see "A `ScaledObject` KEDA has not reported on" below | `Unknown` (`ScaledObjectReadinessUnknown`) while it waits |
| Source mapping needs a human (Helm, patches, missing target, unknown Flux shape), or the target got another UID while the mapping ran (`TargetChanged`) | `NeedsIntervention`, reason of the mapping | `Unknown` |
| The Kustomization is not `Ready` for its current generation but reports no failure: Flux is still reconciling it, for example applying a pull request just merged (#556) | no phase: see "A Kustomization Flux is still reconciling" below | `Unknown` (`KustomizationReconciling`) while it waits |
| The matching repository's settings cannot open a Git provider (see "Settings that cannot open a Git provider" below) | `NeedsIntervention`, `RepositoryUnusable` | `Unknown` |
| The Git host refuses a read of the source mapping for good (the table in 6.5.1: a 403 without rate limit signals, a token the installation refuses, an unexplained 422) | `NeedsIntervention` (`GitForbidden`, `GitRejected`) | `Unknown` |
| All checks pass | `PolicyChecked`, `WithinApprovedBounds` | `True` |
| All checks pass, but a limit (10.7) holds the request back | no phase (or `Pending`), condition `Queued` with the reasons `TargetLocked`, `ConcurrencyLimitReached`, `CircuitOpen` or `CircuitHalfOpen`; rechecked every 30 s (10 min behind a lock held by a stopped request) | unchanged |

### A `ScaledObject` that is not ready

`KedaReady` says a `ScaledObject` is definitely not ready when it shows one of these:

- a trigger error or `PartialTriggerError`;
- a fallback;
- a pause (partial pauses and pause annotations included);
- a deletion;
- an HPA that cannot read its metrics.

The result is `NeedsIntervention`, not `Denied`: the plan may be within every bound, but no catalog action fixes a trigger, so a person must look (the fleet scenario `broken-keda-trigger` expects this).

### A `ScaledObject` KEDA has not reported on

The request keeps no phase and is checked again after a delay that doubles with the time waited (15 s to 2 min). After 10 minutes (`TargetStateTimeout`) it becomes `NeedsIntervention` (`ScaledObjectNotReady`). The 10 minutes are counted since the condition took a wait reason (this one or `KustomizationReconciling`, below). A request the limits queue (10.7) has passed a policy check that did not wait, so its wait is over: `Queued` removes the waiting `PolicyAllowed` condition, and a wait after the request is let through starts its own 10 minutes (#556).

### A Kustomization Flux is still reconciling

The source mapping tells a Kustomization that fails from one Flux is still working on (`fluxstatus.Reconciling`, #556). It fails when a current condition says so: `Stalled=True`, or `Ready=False` with a reason other than `HealthCheckCanceled` (which Flux sets only while a new reconcile replaces the running one). Anything else that is not `Ready` for the current generation is a reconcile in progress: `Ready=Unknown` while a revision is applied, a status about an older generation, no `Ready` condition yet.

A failure ends the request `NeedsIntervention` (`KustomizationNotReady`) at once, as before. A reconcile in progress gives the reason `KustomizationReconciling`: the request keeps no phase, takes no lock and is checked again with the delays of the KEDA wait above (15 s to 2 min). After 10 minutes (`TargetStateTimeout`) it ends `NeedsIntervention` (`KustomizationNotReady`), with a message that says Flux did not finish (fail closed). The 10 minutes bound the whole wait, not each reason: a wait whose reason alternates between `ScaledObjectReadinessUnknown` and `KustomizationReconciling` keeps the start of the first (the `PolicyAllowed` condition keeps its `lastTransitionTime` while it moves between wait reasons), so it cannot restart its clock for good. A wait ends when a policy check does not wait, and the limits queue a request only after such a check: `Queued` removes the waiting condition, so time spent `Queued` behind the lock (while the holder's merge is applied, say) never counts towards a later wait. The freshness check of a decided request (6.6.1) retries such a mapping instead of calling the plan stale, for at most the same 10 minutes in every phase, also once the pull request is open and no deadline bounds it: the retries are recorded as `PlanFresh` `Unknown` with reason `KustomizationReconciling` (its `lastTransitionTime` is the clock, removed once a check passes), and after 10 minutes the plan is stale (`SourceChanged`), as it was at once before #556. Only the part that needs the mapping waits: the Git-only check of the target file still runs, so a file on the target branch that no longer holds the previous value is stale (`TargetFileChanged`) at once, and its pull request is closed. The `GitRepository` is not told apart: one that is not `Ready` still ends the request `SourceNotReady`.

### Settings that cannot open a Git provider

The schema refuses what the adapter refuses (5.4), so this finding is the runtime fallback. It catches an entry the API server stored before a rule existed (5.4 says how such an entry affects later writes).

## 6.3. Retries and deadlines

### What is retried

Only a failure worth retrying is retried, with backoff, or after the delay the host asked for. Such failures are those of the API server, the Git host, or a signer that is throttled, cannot reach Key Vault or Entra ID, or cannot get a token yet.

- The delay the host asks for is `Retry-After`. For a request that has not opened its pull request yet it is cut to the time left before `status.deadline`, so the request expires on time.
- The exception is a request whose stale plan is being cleaned up. It never expires and waits as long as the host asked.

### What is final

Every permanent refusal is a final phase:

- settings that can never open a Git provider (the same refusal ends a later freshness check in `NeedsIntervention`, `PlanFresh` reason `RepositoryUnusable`, without closing anything);
- a read of the source mapping that the Git host refuses for good (`GitForbidden`, `GitRejected`; the classes are in the table in 6.5.1).

### While a retry is pending

The request keeps no phase (or `Pending`) and no `PolicyAllowed` condition. The exception is the wait for KEDA to report on its `ScaledObject` (6.2), when `PolicyAllowed` is `Unknown` with a fixed message and is written once.

The retry limit is planned.

### No stale reads

The policy and settings are read from the API server on every check, never from the cache, so a deleted policy or a tightened setting takes effect at once.

### No Git URLs or provider text

Messages and conditions never carry Git URLs or provider text.

### Step deadlines

Every external call of a step runs under the step's own deadline. Each of these has its own:

- the policy check (`CheckTimeout`): the uncached reads (all pages of every list of the limits included) and the source resolver together;
- the freshness check, with its own context;
- the cleanup of a stale plan: open the repository, find and close the change request, as one sequence under one shared deadline;
- each Git step of the pull request step (`WriteTimeout`).

A step that reaches its deadline returns a retryable error and keeps the request in its phase. It never ends in a final phase. The caller's own cancellation stays a different error, so shutdown is not reported as a timeout.

## 6.4. Each reconcile

Each reconcile does these steps:

1. Gets the current CR and checks generation, plan validity and cancellation.
2. Reads the target object, its UID, the incident state and the policy revision.
3. Checks that the change is still needed and that no other action runs on the same target.
4. Rebuilds external state: does the branch, commit, PR or finished rollout already exist?
5. Does one idempotent step, or records why it is waiting.
6. Updates status and requeues with a deadline.

Never assume exactly-once execution. A PR can be created while the status write that records its number fails. The branch name `shadewright/<request-uid>` and a marker in the PR body (request UID and plan digest) let the controller find the existing operation after a restart. <span class="sw-status sw-built">built</span> (#539, off by default, opt-in with `--reviewer-hints`; [ADR-0028](../adr/0028-reviewer-hints-through-the-pull-request.md)) For a replacement request the branch name derives from `spec.chainRoot`, the UID of the first request of the supersede chain, and never from status, so a lost status write is recovered by the same name; the marker still holds the request's own UID and plan digest.

## 6.5. The pull request step

### What it does

The pull request step (`internal/controller/pullrequest.go`) runs for a fresh request in `PolicyChecked`, and again while its pull request waits in `PRCreated` or `AwaitingApproval` (step 6).

It does at most one write per reconcile. The write comes right after the freshness check, in the repository that check opened and at the target branch head it checked.

### Step 1: the deadline and the finalizer

- Its deadline, `status.deadline` (30 minutes by default), is stored by the policy check in the same status write that sets `PolicyChecked`. It also ends a request whose freshness check never passes (a Git host that stays down).
- The exception is a request whose stale plan is being cleaned up (`PlanFresh` `False` with the stale reason, phase unchanged). It finishes the cleanup past its deadline and ends `Superseded`, or `NeedsIntervention` if the close is refused.
- A request decided while writes are paused gets no deadline then. It gets one, in its own write, on the first freshness check that finds writes allowed again.
- Once started, time spent paused counts against the deadline. A request whose deadline passed during a pause becomes `Expired` when writes resume.
- The step then adds the finalizer `remediation.shadewright.io/close-pull-request`, in its own write.

### Step 2: look for an existing change request

It looks for a change request on `shadewright/<request-uid>` first (`shadewright/<chainRoot>` for a replacement in a supersede chain).

- One that carries the request's exact marker and holds the desired value at its head is adopted.
- A merged one becomes `Merged`.
- A closed one, or one without the marker, is `NeedsIntervention` (`ChangeRequestClosed`, `ForeignChangeRequest`).

### Step 3: branch, commit and pull request

Without an existing change request, the step does these things in order:

1. Creates the branch at the head of the target branch at which the freshness check has just found the file holding the previous value. It does not use a head read again, which may hold a file no check saw.
2. Renders the change at the head of the target branch that the freshness check of that later reconcile has read. This may be newer than the branch (the render rule in 6.5.2).
3. Commits the typed change on top of the branch head it read (the patch checks the previous value).
4. Opens the pull request with the evidence, scope, the size of the step beside the policy (ADR-0026), expected effect, risk, rollback and the marker.

Every write names the head it expects, so a lost status write or a concurrent reconcile finds the work instead of repeating it.

The title and body are built from validated fields and redacted. Two values stay out of the redactor. The first is the request's own `namespace/name`. Its creator chooses the name, so its shape proves nothing. It is kept only when it equals the name the analyzer derives from the request's own spec as the API server stored it: `remediation-` and, in hex, the first 12 bytes of the SHA-256 of `spec.incidentRef.uid`, `/` and the JSON of the plan's part of the spec (every field but `verification.timeout` and `verification.stableFor`, which the API server defaults). The namespace must also be one the redactor leaves unchanged (`RequestName` in `api/v1alpha1`, `keptRequestName`, #433). Nobody can steer that hash to a chosen value. The second is the Incident name. It is kept only when both of these hold:

- it has the collector's form (exactly `incident-` and 64 lower-case hex digits, with `-<n>` for occurrence n >= 2: 2 to 999999, no leading zero);
- the executor has read that Incident from the API server in the request's namespace, with the UID of `spec.incidentRef` and a fingerprint and occurrence number (`spec.occurrenceNumber`) that give that name (`verifiedIncidentName`, `redact.StringKeeping`).

Otherwise, or when the read fails, the name is redacted like the rest, and so is any request name that is not that derivation. So a hex secret written as `incident-<secret>` in a request, or as a request's name `remediation-<secret>`, does not reach the Git host.

### Step 4: recording the pull request

Recording the pull request sets `PRCreated` with its number, URL and head commit and clears the deadline. The next reconcile moves it to `AwaitingApproval` (`Approved` `False`, `WaitingForReview`).

Auto-merge is not built yet, so a request whose policy allows automation also waits for a person (the `AutoMergeQueued` edge of the diagram is planned).

### Step 5: watching the recorded pull request

From then on each reconcile looks at the recorded pull request before the freshness check, because a merged change makes the plan look stale. <span class="sw-status sw-built">built</span> (#678) It finds it by its recorded number in the repository of `status.planRepository` (`GetChangeRequest`), not by its branch, and checks that it is from the request's branch to its target branch: a branch can carry a newer pull request after the first merged (a supersede chain shares its branch), and the merge of the recorded one must still be recorded. A look-up that fails is retried and decides nothing.

- Merged is `Merged` (`Approved` `True`), with the merge receipt in the same status write (below).
- A moved head or a pull request that cannot be found is `NeedsIntervention` (`HeadMoved`, `ChangeRequestGone`). A pull request with the recorded number that is not from the request's branch to its target branch counts as not found, and so does an answer of the host for another number (`ErrUntrustedAnswer`); the condition message names the cause.
- Open, and the Git host reports a merge conflict with the target branch (`Conflicted`, on GitHub `mergeable` `false` with `mergeable_state` `dirty`, which only the look-up by number returns), is `NeedsIntervention` (`ChangeRequestConflict`, #680), not a rejection. The freshness check runs first: a stale plan ends `Superseded` and its pull request is closed as usual, and a check that fails decides nothing. GitHub computes mergeability lazily, so after the target branch moves (and moves back) one answer can be stale while the plan is fresh: a conflict ends the request only when confirmed. The first conflicted look with a fresh plan records `status.pullRequest.conflictSeen` (the time on the executor's clock, the recorded head and the target branch head the freshness check read) and requeues; nothing else runs meanwhile. The request ends only on a later conflicted look with a fresh plan at least two minutes after that time, on the same recorded head. A look that finds no conflict (`mergeable` `null` included) removes the mark, a moved head ends the request as above, and a stale plan ends `Superseded`. A move of the target branch that leaves the plan fresh does not start the wait again (the recorded target branch head is kept as what the first report was about), so a busy branch cannot postpone the end. A recorded time more than a minute after the executor's clock means the clock went back: the mark is recorded anew, so the wait is never cut short and lasts at most three minutes on a clock that does not go back again. Known limit: a clock that keeps going back by more than a minute starts the wait again each time, and no deadline covers an open pull request, so such a request does not end; it fails closed, holding the lock and writing nothing to Git. The decision reads only the recorded status and the executor's own clock, never a time of the Git host. A fresh plan that ends stops before the render of step 6, and the pull request is left open with no comment: resolving the conflict means a push to the branch, which is a person's. `PullRequestOpened` keeps saying it is open, so the request holds the lock on its target until it is merged or closed (`checkHeld`). A host that has not computed the answer yet (`mergeable` `null`) reports no conflict, and the next look asks again.
- Closed without a merge is a rejection, `NeedsIntervention` (`ChangeRequestClosed`). There are two exceptions. The cleanup of a stale plan has started (`PlanFresh` `False` with a stale reason, written before the close), or the plan is stale now: a stale plan's own close looks the same and ends `Superseded`.
- A merge after the cleanup has started still ends `Merged`, and the mark is removed.

**The merge receipt** <span class="sw-status sw-built">built</span> (#678, [ADR-0030](../adr/0030-merge-receipt-and-verification-outcomes.md) section 1). The status write that sets `Merged` also writes `status.mergeReceipt`: the repository and installation of `status.planRepository`, `status.source.targetBranch`, the pull request number, the head that was merged (`status.pullRequest.headSHA`), the merge commit (`status.pullRequest.mergeCommitSHA`) and `mergedAt` (`status.pullRequest.mergedAt` of that write). It is written on the three paths that record a merge: the recorded pull request, a merged pull request adopted by its marker after a lost write (`endMerged`), and the replacement's merge after a handoff (with the replacement's head). A merge with a foreign change (`MergedWithForeignChange`) gets none. Two CEL rules on the object keep the receipt equal to its first value (also when it or the whole status is removed and written again) and its merge commit equal to `status.pullRequest.mergeCommitSHA`. A lost write leaves the request in its earlier phase; the next look finds the pull request again by its number, and the host's answer for a merged pull request does not change, so the receipt is written with the same values (only `mergedAt`, the time of the write, is new). When the host reports no merge commit, or one that is not a full commit ID (GitHub computes it after the merge, so the adapter returns such a merge without one), the executor waits for it five minutes, measured on its own clock from when it first saw that state, which it records once in `status.mergeCommitAwaitedSince` (a CEL rule keeps it), so a restart does not start the wait again; it never compares with the host's merge time. A recorded start more than a minute after the executor's clock means the clock went back: the wait is then over, so it never lasts longer than six minutes on that clock. Meanwhile the request keeps its phase and its lock, and nothing else runs (a merged change makes the plan look stale). A commit reported within the wait is recorded with its receipt as usual. After the wait the request is `Merged` without a receipt, with `Verified` `Unknown`, reason `NoMergeReceipt`, in the same write; the next reconcile ends it `Inconclusive` with that reason, and the circuit breaker reads it as `Inconclusive` in between. On the adopt path the same wait comes before `checkMerged`, which without the merge commit ends `MergedWithForeignChange`; a handoff replacement records a pull request merged without one like an open one, and the look above waits. Every write that sets `Merged` carries the receipt or the `NoMergeReceipt` mark: `recordMerge` writes the merge commit only with the receipt (`status.pullRequest.mergeCommitSHA` is set once), and `setPhase` adds the mark on any other path. So a request merged before the receipt existed is the only one with neither: it keeps `Merged` and gets `Verified` `Unknown`, reason `MergedBeforeVerification`, once; its phase does not change, so the circuit breaker reads the same history. A receipt-less `Merged` request whose `Verified` has any other reason, which the executor never writes, ends `Inconclusive` (`NoMergeReceipt`) rather than passing. CEL also ties the receipt's pull request, head and `mergedAt` to `status.pullRequest` (so its number and head cannot change once a receipt exists) and allows a receipt only in `Merged` or a later phase. The decision reads only the recorded status and the executor's own clock.

These outcomes are written to the `Approved` condition. `PullRequestOpened` stays `True`, the pull request having been opened.

Once the pull request is closed without a merge, or the request ends on a confirmed merge conflict, its reason and message say so:

- `ChangeRequestClosed` ("Pull request #26 was closed without a merge") for a rejection;
- `PullRequestClosed` with why for a close of Shadewright's own ("Pull request #26 is closed: the plan is stale (PolicyChanged)", or "the change no longer renders as planned" with the render's reason);
- `ChangeRequestConflict` ("The Git host reports that the pull request conflicts with the target branch; it is left open for a person to resolve or close, and automation stopped") for a merge conflict seen twice, at least two minutes apart, on the same recorded head (above). The pull request stays open, and `PullRequestOpened` keeps saying so.

### Step 6: rendering again while it waits

While the pull request waits, a fresh plan is rendered again (the render rule in 6.5.2) when the freshness check found the target branch at another head than `status.pullRequest.renderedTargetHead`. That field holds the head of the last render that passed.

- The field is empty when the pull request is recorded, so the first recheck renders once more (the commit was rendered at a head the status does not keep).
- After that a quiet target branch costs no Git read beyond the freshness check, which returns the head and the target file it checked.
- A waiting pull request that fails the render is closed and the request stops in `NeedsIntervention` (the render rule in 6.5.2).

### What stops automation

These stop automation with `NeedsIntervention`:

- a branch whose file holds neither the previous nor the desired value, or that holds more than Shadewright's one commit on top of the target branch: before the commit, a commit that is not on the target branch; before the pull request is opened or recorded, or while it waits, anything beside the one commit that changes the target file, and a file on the branch that is not byte for byte the planned change of the target file (`BranchDiverged`). A pull request whose head moved during the open, or that is found before it was recorded and fails these checks, is closed and recorded. The render sees the target branch with the file replaced, so a foreign commit on the branch would reach the pull request unchecked (#524);
- a head that moved away from the recorded commit: if the new head holds more than the planned change, `BranchDiverged` with one comment that hands the pull request to a person, who may be working on it, so it stays open (ADR-0028; since #542 the comment, on whether or not reviewer hints are on, needs the App's ID first, one `GET /app` with a Key Vault signature per provider, design 9.2; only a whole-line marker in a comment the App's own bot user posted counts as the comment being there already; on a pull request with more than 1,000 comments, or when the App's ID cannot be read, nothing is posted; a post that fails is not repeated; the request stops either way, and its message says the comment failed); merged by then, `MergedWithForeignChange`; otherwise `HeadMoved`. A merged head is judged against its own parent on the target branch, not the target branch after the merge;
- a patch the patcher refuses (`PatchRefused`).

Nothing is force-pushed or rebuilt.

### When the deadline passes

A request that has not opened its pull request by its deadline closes what it opened and becomes `Expired` (`PullRequestDeadline`). This also holds while the freshness check keeps failing.

- If the cleanup of a stale plan has started (the mark), the request finishes the cleanup past its deadline and ends `Superseded` (or `NeedsIntervention` if the close is refused).
- If the Git host refuses the look-up or the close for good, the request still becomes `Expired`, with the reason `PullRequestDeadlineCleanupRefused` (not `PullRequestDeadline`). It keeps the lock on its target (10.7). The message says that a pull request of its own may be left open and must be closed by hand.
- A replacement whose handoff is `Reserved` first looks for its own commit on the branch (the recovery of the handoff, below). If that look fails for a reason worth retrying (the repository cannot be opened, the Git host does not answer), it is retried, past the deadline too, after the delay the host asked for (`Retry-After`) or with the error backoff, for at most 15 minutes after the deadline; the deadline alone does not end it, since its commit may be on the pull request already. Then it ends `NeedsIntervention` with the reason `HandoffRecoveryFailed`, its handoff `Aborted`. A pause, a final refusal or settings that cannot reach the repository end or hold it as for any request.

### 6.5.1. What the Git host refuses

A refusal the Git host gives for good does not wait for the deadline. The GitHub adapter classes each error status from its code and headers:

| GitHub answers | Class | The request |
| --- | --- | --- |
| 429 (any), or 403 with `Retry-After`, with `x-ratelimit-remaining: 0`, with the secondary rate limit message (searched in at most 16 KiB of the body, never kept), or with a body that cannot be read | retryable, with the delay asked for (`Retry-After`, else until `x-ratelimit-reset`, else one minute) | waits in place up to 15 seconds, else requeues after the delay, cut to `status.deadline` |
| 403 without those signals (a permission the App lacks, a rule of the repository), on a call or when the installation refuses the token | final, `ErrForbidden` | `NeedsIntervention` (`GitForbidden`); with a pull request recorded, see below |
| 422 that the state explains: the branch exists (at the same commit; at another, a conflict the next reconcile reads again), or an open pull request of the branch | resolved by the adapter | goes on; a pull request without the request's marker is `NeedsIntervention` (`ForeignChangeRequest`) |
| 422 that the state does not explain (a branch name or rule GitHub refuses, a pull request it will not open). A create of the branch or the pull request is sent again first, up to four attempts with backoff, because a read may come from a replica that does not show the branch, the pull request of a lost response or the commit just pushed yet; when the call runs out of time before the last attempt, it is retryable | final, `ErrRejected` | `NeedsIntervention` (`GitRejected`); with a pull request recorded, see below |
| 408, 5xx, network errors and timeouts | retryable | retried with backoff until `status.deadline`, then `Expired` |

The stop is written with fixed text, never with the response. It applies wherever the refusal comes from: the policy check's source mapping, the freshness check's reads and the pull request step.

**While a recorded pull request waits** (`PRCreated`, `AwaitingApproval`), a refusal does not stop the watch.

- The phase stays. `PlanFresh` `Unknown` (`GitForbidden`, `GitRejected`) says why.
- Shadewright writes nothing to Git.
- Each recheck still looks at the pull request first, so a later merge (or a moved head, or a pull request that is gone) is recorded. A close without a merge is decided once the freshness check can run again.
- The condition is removed once the freshness check passes again.

**A stale plan whose close the Git host refuses for good** ends in `NeedsIntervention` with `PlanFresh` `False` (`GitForbidden`, `GitRejected`). The condition says that the plan is stale and its change request was not closed. That request no longer watches its pull request.

**Still retried** whatever the status:

- reads of the render check;
- a GraphQL `updateRefs` refusal while the branch head has not moved (the commit);
- a deletion still waits for its close.

**A 422 when the installation token is created** is not classed either. GitHub documents it for a failed validation and for a spammed endpoint alike, so it is retried until the deadline.

### 6.5.2. The render rule

A validation refusal is written with fixed text, never with the content it checked.

#### What it does

The render rule covers both windows in which the target branch can move under the change: between cutting the branch and the commit, and while the pull request waits (#181). The change is rendered at the head of the target branch at which the freshness check has just found the plan fresh, which is what merging it would build.

#### What must match first

The merge must give exactly the file the change commits or committed.

- **Before the commit**, the target file at that head must be byte for byte the file on the branch the change is made from. A merge elsewhere passes. Any edit of that file (another field, a comment, another spelling of the previous value) stops the request.
- **While the pull request waits**, the target file at that head with the planned change applied must be byte for byte the file of the pull request. The patcher replaces the bytes of one scalar only, so the one difference it allows is how the previous value is spelled (`256Mi` or `268435456`). That is on the line the pull request changes, so Git cannot merge it without a conflict, and resolving that pushes to the branch (`HeadMoved`).

If another edit to the file was merged, the merge would combine edits that no render saw. The request stops in `NeedsIntervention` (`TargetFileDiverged`). A target file the patcher refuses at that head stops it with `PatchRefused`.

#### Then the render

The Kustomization's directory (`status.source.buildPath`) at that head is rendered with and without the change (8.2 steps 4 and 5). Anything but the planned difference stops the request in `NeedsIntervention` with the check's reason (such as `OtherObjectChanged`).

Before the commit, nothing is committed (`PullRequestOpened` `False`).

#### A waiting pull request that fails the render

It is closed for any of the reasons above. The close is the same one, with the same permission, as a stale plan's or an expired request's: only by its recorded number, in the repository the plan was made for, with no comment and nothing from the file.

Then the request stops in `NeedsIntervention`. This is written to the `Approved` condition with the reason and fixed text (never file content), and to `PullRequestOpened` (`PullRequestClosed`, step 5). `renderedTargetHead` keeps the last head that passed.

#### What you see

- On GitHub the pull request shows as closed without a merge by the App, with no comment. Its branch `shadewright/<request-uid>` stays.
- The reason is on the request (`kubectl get remediationrequest`).

#### Why it is closed

Shadewright no longer stands behind the pull request. Left open, it could still be approved and merged while the request says it would build something other than the plan, and nothing on the pull request would say so.

Shadewright never reopens it and does not watch it any more. The way back to the change is a new request.

#### What can go wrong

- **The close fails.** It is handled as the close of a stale plan's change request. A Git host failure, paused writes or the deadline leave the request waiting and are retried (the next reconcile renders and closes again). Settings that no longer reach the repository stop the request with the pull request left open.
- **The close succeeds but the status write after it fails.** The next reconcile finds the pull request closed and stops the request with `ChangeRequestClosed` instead of the render's reason.
- **A failed Git read** is retried like the rest of the step, after the delay the host asked for (`Retry-After`). A render cut short by the step's deadline (`WriteTimeout`) or by cancellation is retried, never a refusal.
- **A merge to the target branch does not wake the request.** It is seen at the next full recheck, at most five minutes later (the looks at the pull request in between read only the pull request). A person who merges the pull request in that window merges a change that was not rendered at the new head.

#### Limits

- A request whose `status.source` has no `buildPath` (one decided before the field existed) stops in `NeedsIntervention` (`PlanIncomplete`) before any commit and has to be decided again. In practice the freshness check sees the missing value first (the mapping and the plan digest both include it) and ends it `Superseded` (`SourceChanged`).
- A pull request adopted after a lost status write is rendered on its first recheck from the target file at its head. Other files on its branch are not compared (see the next point).
- Known limit: after a lost status write the step checks the file at the branch head, not that the head is its own commit, because the provider interface has no commit-parent read yet. A later push that keeps the desired value is caught only once the pull request is recorded.

### 6.5.3. Closing a change request

A change request is closed only when it is the pull request of the request's branch and carries the request's exact marker (a replacement whose body was not rewritten: the marker of an earlier request of its chain, 6.5.4). Once a number is recorded, it is closed only when it is that number.

So someone else's pull request on the same branch name, or a recorded number that is not the branch's own, is never closed. The cleanup of a stale plan then ends `NeedsIntervention`, `ForeignChangeRequest`.

### 6.5.4. Reviewer hints and the handoff

<span class="sw-status sw-built">built</span>, off by default, opt-in with the executor flag `--reviewer-hints` (#631); the GitOps repository must require the check `shadewright/review` from the App first ([ADR-0028](../adr/0028-reviewer-hints-through-the-pull-request.md), #538, #539, #542). Today a plan that goes stale ends `Superseded` and its pull request is closed (6.6.1). A reviewer's hint does the opposite: it never closes the pull request and never opens another.

- **Seeing the hint.** <span class="sw-status sw-built">built</span> (#538; off unless the executor runs with `--reviewer-hints`) On the look at a request in `AwaitingApproval` whose pull request is open with the head Shadewright pushed, the executor reads the conversation comments (never inline review comments) through `FeedbackSource`, one bounded look per poll (at most 5 pages), from its durable cursor in `status.commentCursor`. A sweep that found nothing keeps its `since` and the `ETag` of its answer, so while nothing changes the next look is a `304`. Only a person's comment counts (bots and Apps, the executor's own comments among them, are ignored), and only a first line that starts with `/shadewright reanalyze:`. A comment already stored is not handled again, and the cursor keeps the ID and `updated_at` of the newest 50 comments judged since its `since` (`status.commentCursor.seen`), so one that comes back on the watermark unchanged is not judged again, whatever the record of refusals still holds; the ones pushed out of that bound leave a watermark (`evictedAt`, `evictedID`: a command up to that ID and update time counts as judged, so an unchanged one refused at the cap is not accepted later when a slot frees up, while an edit after its judgement is judged again); `since` is inclusive (a comment created at it counts as read). A CEL rule keeps the cursor once set and never moves `since` back, and keeps an `ETag` only on the first page of a sweep. The code host is asked for the author's base permission first, at most 10 authors per look: a comment that would need an eleventh question ends the look there, unjudged and not refused, the cursor moves to just after the last comment judged, and the next look asks. A refusal or an unknown author ends it: such commands are counted in a log line and never recorded, so a person without write access cannot push records out; such a command is recorded in `seen` like any judged command, so it is asked about again only when it is edited. For a writer, the text is redacted before anything is stored and refused when the result is not clean (`NotRedactable`) or over 2 KiB (`TooLong`); a request with 3 hints `Consumed` or `Reserved` (the cap of a chain; `Pending` and `Declined` ones do not count) refuses the next (`CapReached`). `status.hints` holds at most 10 entries, and a stored hint is never removed, so a `Declined` one stays as the record (a CEL rule also keeps at most 3 `Consumed` or `Reserved`); once it is full, a further command is neither stored nor refused, the cursor passes it, and a log line counts it (no record, so no reply). An accepted hint is stored in `status.hints`, `Pending`, pinned by comment ID, digest and `updated_at`. An edit after that changes nothing and sets the hint's `editedAfterAccept` flag once (CEL: never unset); a comment read before and edited later to carry the command is refused as `EditedAfterSeen`. So is a refused command (`Empty`, `TooLong`) that its writer fixes by editing the comment, once a sweep has passed the comment's creation, and the stored reason stays (a comment has one record): the writer posts a new comment instead. A hint's `reason` is one of a closed list (the analysis outcomes it copies, `NoAnswer`, `PullRequestMerged`, `PullRequestClosed`, `RequestEnded`, `ReplacementDenied`, `HandoffAborted`, `HintsTurnedOff`); an outcome outside it is stored as `NoAnswer`. With `--reviewer-hints` off, the look reads no comments and declines every `Pending` or `Reserved` hint (`HintsTurnedOff`), aborts a standing handoff intent, and the admission denies any replacement (`HintsTurnedOff`) while one admitted before ends `Cancelled` with that reason without the handoff, its recovery included (ADR-0028, amendment of 2026-11-10). Refusals go into `status.refusedHints` (at most 10, no text; one still owed its reply is never dropped). Hints, refusals and the cursor are written in one status update, after the kill switch allowed it, so a lost write stores nothing and the next look makes the same decisions; a failed permission question stores what came before it and moves the cursor to just after it, and records a Warning Event `HintPermissionUnknown` on the request, once per comment in the process, since every later look stops at the same comment until the host answers. A hint is consumed once.
- **The analysis.** <span class="sw-status sw-built">built</span> (#537) The analyzer reads `status.hints`, analyzes again with the hint fenced, and creates a replacement with `spec.replaces` and `spec.chainRoot` (CEL: one only with the other); a plan equal to the replaced one creates nothing (`PlanUnchanged`). It records the hint it handled in the Incident's `status.handledHint` with the outcome in the `Analyzed` condition; a hint within the cooldown or with the budget spent is answered without an analysis (`CoolingDown`, `BudgetSpent`), see [section 3](3-observation-and-deduplication.md#analysis-trigger). <span class="sw-status sw-built">built</span> (#538) On the same look the executor reads the Incident the request names (`get`, from the API server) while a hint is `Pending`: when its `status.handledHint` names the hint outside phase `Analyzing` and `Analyzed` is `True`, an outcome that created a replacement (`Proposed`, `RunbookMatched`) is stored on the hint (`outcome`, CEL: set once, never changed) and leaves it to the handoff, whatever the Incident handles later, and any other outcome (`BudgetSpent`, `CoolingDown`, `NoProposal`, `PlanUnchanged`, a model error) declines it with that reason. A hint with no such answer one hour after it was accepted (`HintAnswerTimeout`: an analysis takes at most 10 minutes, a lost one runs again after the 15 minute cooldown) is declined as `NoAnswer`; an outcome outside the list is declined as `NoAnswer` too and logged with its value. The answers and the timeout run after every look at the pull request that did not end the request, also while reviewer hints are off, in `AutoMergeQueued`, or when the look failed, within a deadline of their own. Whatever ends the request, the status write that records the end declines every `Pending` hint: `PullRequestMerged`, `PullRequestClosed`, or `RequestEnded` (a moved head, a stale plan). <span class="sw-status sw-built">built</span> (#539) A hint answered `Proposed` or `RunbookMatched` is moved on by the handoff (`Reserved`, then `Consumed`), and is declined once every replacement naming it ended without taking over: `ReplacementDenied` when one was `Denied`, `HandoffAborted` otherwise (a replacement `Denied` for `ChainLinkInvalid` is not counted). Known limit: a hint answered `Proposed` or `RunbookMatched` whose replacement never appears (its create was lost, or the analyzer ended before it) stays `Pending`, and a later hint on the same request waits behind it until the request ends. <span class="sw-status sw-planned">planned</span> Turning hints on, per repository (ADR-0028 section 5): it waits for the check run on every pull request (below) and the ruleset that requires it. The lab runbook (#541).
- **The chain link is checked at admission.** <span class="sw-status sw-built">built</span> (#539) Before a branch name is computed for a request with `chainRoot`, at the policy check, the executor checks that the request named in `spec.replaces` is for the same Incident and target, has the same chain root, is `AwaitingApproval` with an open pull request and holds the named hint `Pending`. Otherwise the replacement is `Denied` (`ChainLinkInvalid`). It is not repeated: the predecessor changes during the handoff, so later reconciles accept the replacement only through the `status.handoff` the executor wrote (`Reserved` or `Done`, `from` equal to `spec.replaces`). Admission then takes the predecessor's lock and concurrency slot under `admitMu`; nothing else is exempt and the circuit breaker applies.
- **The handoff** <span class="sw-status sw-built">built</span> (#539, off by default; the reply #542) is written in this order: the replacement reserves (`holdsTarget`, `status.handoff`); the predecessor records its intent and the hint becomes `Reserved` in one write (a conflict on that write is tried again, since the predecessor writes its status on every look); the update of the branch; the replacement records the pull request and moves on; the predecessor ends `Superseded` with reason `HandedOff` (its hint `Consumed`, its `holdsTarget` false); then the title and body, the check run and the reply. A restart at any point resumes from what each status and the branch head show, as ADR-0028 section 4 lists. While its intent is recorded, the predecessor's `watchPullRequest` accepts a head whose parent is the head it recorded and that the App made, instead of `HeadMoved`: the executor reads no author, so "the App made it" means the chain from the old head changes only the target file and the file equals the replacement's plan, computed again from the file at the old head (the predecessor's value changed to the replacement's), so a person's one-commit push to the target file is `HeadMoved`. While the intent stands the predecessor writes nothing to Git: no freshness check (so no close of its stale plan), no close by its finalizer, which waits until the replacement took over or ended. Every close reads the request again from the API server right before it, so a reconcile that read the predecessor before the handoff never closes the pull request its replacement took over. A replacement whose branch update happened before a restart or a lost status write finds its commit before its own freshness check and deadline, so a merge of that commit, or a target branch that moved since, is still its own. A merge with a merge commit, or a fast-forward, puts the commit on the target branch, where the chain can no longer be told apart from it; the commit is then judged against its own parent instead (`CheckCommit`: one parent, the head the predecessor recorded, and only the target file changed, #591). An intent that aborted does not block a later hint of the same predecessor: its next replacement replaces it. The predecessor's other `Pending` hints move to the replacement at the record of the pull request, since the comment cursor it carries is past them. Only an admitted replacement (its `status.handoff.from` naming `spec.replaces`) frees a concurrency slot. `prMayBeOpen` treats `Superseded` with `HandedOff` as possibly open, and `checkHeld` does not look it up.
- **The commit.** <span class="sw-status sw-built">built</span> (#539) One new commit on the existing branch: no force-push, no rebase, no new base. Its net change against the base is exactly the new plan (<span class="sw-status sw-planned">planned</span> if the path differs, the old path is restored to base content; until then a plan for another file ends the replacement `Cancelled` with `HandoffAborted` and the old pull request stays). The ref moves by a fast-forward with the expected head as the parent; that catches a push by anyone. It does not catch a squash merge or a close, which leave the head ref alone, so the pull request is read right before the update and again right after. A close ends the replacement without taking over (`Cancelled`, `HandoffAborted`). For a merge the merged head decides: the old head means the predecessor is `Merged` and the replacement is `Cancelled`; the new commit means the replacement owns the merge and ends `Merged`. The reply says which plan reached the base. Every write of this section checks the kill switch right before it; while paused the handoff waits. Before the update, the render check (6.5.2) and the branch rule (#524) must pass. A person can still push to the branch (a force-push included); the request then stops as #524 says.
- **After it.** <span class="sw-status sw-built">built</span> (#539, off by default) The executor rewrites the title and body, recorded in `status.handoff.from.pullRequestUpdate` (`Done`); a final refusal of the Git host is recorded `Refused`, never as done, and is not asked again: a body the host did not rewrite keeps the predecessor's marker, which the replacement keeps accepting. After two or more refused rewrites in a row the body keeps the marker of a request further up the chain (#591): the close and the lock's look-up then walk the chain from the replacement through `status.handoff.from`, each link read by name and accepted only with the UID it names, the same chain root and an outgoing handoff to the request below it, up to the first request whose rewrite was done or that opened the pull request, and accept the marker of any request on that walk. A request of another chain or Incident, or one the walk does not reach, is never accepted; a link that no longer exists ends the walk and fails closed, as for any marker that is not the request's (6.5.3). A final refusal is a permission or rule the App lacks (`GitForbidden`), or a title or body the host would refuse as invalid (`GitRejected`: an empty or too long title, a body over 65536 characters, text that is not UTF-8); the pull request is read first, so one merged or closed meanwhile is left to the watch instead. A refusal sets `PullRequestOpened` (still `True`) to the reason `PullRequestRewriteRefused` with a Warning Event of the same reason. When the status write after the rewrite fails, the executor keeps the host's answer in memory and retries the write with backoff without sending the rewrite again (after a restart it is sent again, which changes nothing), and the look at the pull request still runs, so a merge or a close is recorded meanwhile. The replacement carries every hint of the chain, a declined one included, so each of their authors stays out of the approvers that count.
- **The check run and the merge gate.** <span class="sw-status sw-built">built</span> (#539, off by default) On every head of a Shadewright pull request, the first one it opened or adopted as well as one a handoff pushed, each look that finds the pull request open with the recorded head creates the check run `shadewright/review` `in_progress` on a head that has none, with the App permission `checks: write` that the token request asks for. Its state is `status.pullRequest.reviewCheck`, which names the head it is for (CEL: always `headSHA`); a write that records a new head starts without it, so a state never outlives its head. The merge gate decides only on a look whose read of the comments reached the end and stored what it accepted, so an approver whose hint is already on the pull request never counts: it reads every review of the pull request and creates the check run again, `completed` with `success` (`Passed`), once the latest review of someone who wrote no hint in the chain (compared by user ID) approves the recorded head (8.4). An approval of an earlier head never counts; an approval of the head by a hint author does not count either and gets a comment saying why, once per review (asked once per review by the executor process, not on every look). "Wrote a hint" means a hint stored in `status.hints`: a command that was never stored because the list was full (10 entries) does not keep its author out of the gate, because an unstored command never steered the change. Reviews are read in at most 4 looks; on a pull request with more the gate fails closed: it completes the check run with `failure` and a summary saying why, records it `Failed` with `Approved` `False` (reason `ReviewListTooLong`) and a Warning Event, and does not read the reviews again on that head, so a person decides the merge. A comment that runs out of time does not keep a passing gate from completing the check (the comments get half of the time of the step; those left are asked on a later look), and a comment the host refused for good is not asked again. A final refusal to create or complete it is recorded `Refused` and sets `Approved` `False` with reason `ReviewCheckRefused` and a Warning Event, so a pull request the required check blocks says why; it is not asked again on that head. Either reason is about one head and is final there. A request's head does not change in place today (a push stops it with `HeadMoved`, a handoff's new head belongs to the replacement), so recording `InProgress` or `Passed` also sets a leftover reason back to `WaitingForReview` only as a guard (#606); only a merge sets `Approved` `True`.
- **The reply.** <span class="sw-status sw-built">built</span> (#542, off by default) On each look that finds the pull request open with the recorded head while the request waits for review and no handoff intent of it stands, the executor replies in the same pull request, mentioning the author, to every hint in `status.hints` that is `Declined` or `Consumed` and to every command in `status.refusedHints`, once each: "Applied" with the new plan's typed values (what, target, desired value) and its head for the hint the request answers (`spec.replaces`); "Applied earlier" for a consumed hint further up the chain; "Not applied" with the hint's reason in words and as its code; "Not stored" with the refusal's reason, saying no hint was stored. The reply is a fixed template filled with typed fields; the one free text is the stored hint's text (redacted when it was accepted), quoted in a code span (so it neither mentions nor links), cut to 200 bytes, with format characters (bidi controls, zero width) turned into spaces, and left out when it holds a control character or a comment marker. No model text goes into a reply. It fails closed: the whole reply must be text the redactor would not change (only the recorded head, a commit ID, is kept); otherwise nothing is posted and the entry's `reply` records `NotRedactable`. It is posted through `CommentChangeRequest` with a marker that names the comment, so the pull request gets one reply per comment across retries, restarts and the requests of a chain (a replacement carries the hints and the refused commands with their `reply`). The marker counts only as a whole line of a comment the App's own bot user posted (the GitHub adapter reads the App's ID once with its JWT, `GET /app`), so a person cannot forge it to suppress a reply; the adapter reads the comments newest first, at most 50 pages of 20, and past them posts nothing (`TooManyComments`); the first page is read again at the end, so a comment that a deletion moved onto it is not missed. It posts at most once per call: a post that fails for a reason worth retrying may have gone through while the list of comments lags behind it, so it is not repeated in that call. The post and the status write pass the kill switch: while paused nothing is posted or recorded. A post that fails for a reason worth retrying ends the look's replies, and no reply of the request is posted again (nor its marker looked for) before 30 seconds have passed, or the delay the Git host asked for (a rate limit, at most an hour); the look is requeued for the end of the wait. It is tried on later looks at most 5 times per process, then recorded `GaveUp`, and a rate limit is no try (the count is dropped only with the status write that records it); a final refusal of the Git host is recorded `Refused`; a pull request closed or merged meanwhile records nothing and ends the look's replies. `status.refusedHints` never drops a refusal whose reply is still owed: at its 10 entries the oldest one with a `reply` goes, and while all 10 wait a new refusal is not recorded (a log line says so). `reply` is set once (CEL: never changed or unset, and only on a `Declined` or `Consumed` hint). At most 5 replies per look. Known limits: a hint declined in the write that ends the request (merged, closed, `RequestEnded`) gets no reply, since the request no longer looks at the pull request; a command not stored because the list of hints was full, or one from an author without write access, gets none either (no record, and an outsider must not be able to make Shadewright comment); the reply quotes but does not link the hint's comment; on a pull request with more than 1,000 comments no reply is posted.

## 6.6. Required mechanisms

- Leader election, bounded concurrency, a lock per target object; a request of an Incident whose fix another request merged ends `Superseded` before it takes the lock (6.6.3), and so does one for a field whose pull request a reviewer closed without a merge while it waited (6.6.4).
- Optimistic concurrency on Kubernetes objects and a base SHA check on every Git write.
- Retries with exponential backoff, jitter and `Retry-After`, with a limit (the limit is planned).
- Deadlines for policy, PR, delivery and verification, stored in status.
- No model call during reconcile when the context did not change.
- Policy and plan freshness re-checked right before every write (6.6.1).
- Auto-merge disabled when the request is cancelled before merge. After merge, undoing needs a separate revert PR.

### 6.6.1. The freshness check

#### What it does

It runs right before every write. The step is `ensureFresh` in the controller (around `freshness.Checker.Check`). The pull request step and the later state machine run right after it and write only on what it read. The reconcile runs it every five minutes for a decided request before merge.

#### What must still hold

- Same policy UID and generation, same settings.
- The source mapping (ADR-0012), run again with the policy check's resolver, still gives the recorded repository, branch, file and build directory (`status.source.buildPath`).
- The target file still holds the previous value.

The base commit is not compared, because it moves with every merge. A merge that moves it leaves the plan fresh but makes the pull request step render the change again at the new head.

#### For `AdjustKedaMaxReplicas`

- The `keda` integration must still be enabled in `spec.modules`. Disabled, the plan is stale, `PlanFresh` reason `ModuleNotEnabled`.
- The live `ScaledObject` must still be ready by the rule the policy check applied (`KedaReady`). This is checked after the previous value, so a changed `maxReplicaCount` reports `TargetChanged`.

A `ScaledObject` read not ready (a state not reported yet counts as not ready here) is not acted on at once. The check runs on through the mapping and the Git reads, then reads and checks the target again and last the policy and settings, as for a stale mapping.

- Only when it is still not ready is the plan stale (`PlanFresh` reason `ScaledObjectNotReady`), so it is `Superseded` and its pull request is closed.
- Ready on the second read, or a pause or a changed policy meanwhile, is a retry. One blip of the status never closes an approved plan.
- A failed read of the object is retried, never fresh.

#### Known limits

- The policy check reads the target again after the mapping and starts over when its resourceVersion changed. A `ScaledObject` whose status KEDA writes often (`lastActiveTime`, `health`) can therefore make it start over. Comparing the generation and the fields the check read instead would allow a status change the readiness rule depends on to pass unseen, so it is not done.
- KEDA's conditions carry no `observedGeneration`, so `Ready=True` read right after a spec change may still describe the previous spec. No settle time on the `ScaledObject`'s generation is required, because it would only shorten this window, not close it (threat model G1.10).

#### The read order

The mapping reads the branch head, the target and the Flux objects on every run. The freshness check, after its last Git read, reads these again in this order before it says fresh:

1. the branch head;
2. the target and those Flux objects;
3. last, the policy and settings (a pause returns "paused", any other change a retry).

The mapping also reads the Flux Kustomization and GitRepository again after scanning Git and retries if either changed, for a stop (`Denied`, `NeedsIntervention`) as for a source. A stale mapping is checked in the same order before it is acted on.

#### Fresh, stale or retried

- A mapping error is "not fresh" and is retried, never stale.
- A mapping that now denies or needs a person, or gives another source, is stale (`SourceChanged`), never fresh. The exception is a Kustomization Flux is still reconciling (`KustomizationReconciling`, 6.2): not fresh, retried for at most 10 minutes (`PlanFresh` `Unknown` meanwhile), then stale (`SourceChanged`). The target file is still read at the head of the target branch meanwhile, and one that no longer holds the previous value is stale (`TargetFileChanged`) at once.
- It is stale only once all of these hold: the mapping read the plan's repository and target branch; the head of that branch is still the commit it read (or the branch is still missing); the target and those Flux objects (a missing one still missing) are read again unchanged; and last the policy and settings it was given are unchanged.
- Otherwise the result describes a delivery or a tree that has gone, and the check is retried.

#### A stale plan

<span class="sw-status sw-built">built</span> (#539, off by default) The one `Superseded` that does not close the change request is reason `HandedOff` (6.5.4): the pull request lives on under the replacement request.

A stale plan becomes `Superseded` and its change request is closed (once, idempotently), never a rebuilt branch.

1. The status is marked first (`PlanFresh` `False`, phase unchanged).
2. The change request is closed.
3. The phase is set to `Superseded`.

So a close whose answer was lost is known to be Shadewright's own.

The close happens only in the repository the plan was made for (same URL and installation) and against its recorded target branch. If that entry is gone or now names another target branch, nothing is closed and the request ends in `NeedsIntervention` (`RepositoryUnavailable` or `TargetBranchChanged`).

### 6.6.2. The shared read cache

The mapping keeps the files it reads at a commit, which never change, in a cache shared by the policy check and the rechecks (`sourcemap.ReadCache`). The rechecks of a branch that has not moved read none of the build's files again.

- **Size.** At most 4,096 reads and 16 MiB, least recently used evicted first, in the executor's memory (256 Mi in `config/manager/manager.yaml` since #615).
- **Key.** An entry is keyed by the whole settings entry (URL, installation, App and signing key), the full commit ID and the path, so no other entry or repository reads it.
- **What is kept.** Only a file, a file too large and a path the adapter found absent from a tree it read at that commit (`providers.ErrPathNotInTree`), and in the same way a directory listing (`ListDir`), a directory over 1000 entries and a path that is not a directory: a tree at a commit ID never changes, so a listing is as safe to keep as a file, and a listing and a file read of one path have separate entries. A failed read is never kept, and neither is a plain not found. The GitHub adapter also gives a plain not found for a commit it does not have yet, any 404 or 410 and a token it cannot get, so an outage cannot hide a `.sourceignore` or a second kustomization file.
- **Verdicts.** Every read and listing, cached or not, counts against the mapping's read bound and byte budget, so a hit or an eviction never changes a verdict.

### 6.6.3. Duplicates of a merged request

<span class="sw-status sw-built">built</span> (#556) The analyzer may propose more than one request for one Incident and target. Only one holds the target lock at a time; the others wait, `Queued` with `TargetLocked`. Once the pull request of one of them is merged, the Incident has its fix, so each of the others ends `Superseded` with reason `AnotherRequestMerged` on `PlanFresh`, whose message names the merged request and its pull request. Before #556 they went on to the policy check as soon as the lock was released, met Flux applying the merged revision and ended `NeedsIntervention` (`KustomizationNotReady`), which asked a person to look at requests nobody needed (lab run of 2026-11-08).

- **Which requests.** Only one that cannot have a pull request of its own: no phase or `Pending`, no recorded pull request, no lock (`status.holdsTarget`), and no supersede chain (`spec.replaces` or `status.handoff`, which ADR-0028's checks decide). A request with a pull request open or merged, or in a handoff, is never ended this way.
- **Which merge.** A request with the same Incident UID and target UID, the same action type and the same field (`sourcemap.FieldFor`: the memory limit of another container of the same Deployment is another fix), in `Merged` (or a later phase) with a recorded pull request, merged after the duplicate was created. The time of the merge is `status.pullRequest.mergedAt`, written with the phase `Merged` and never moved; `lastPhaseTransitionTime` moves with every later phase (`WaitingForDelivery`, `Verifying`, `Succeeded`), so it would take a request proposed after the merge for a duplicate. A request merged before `mergedAt` existed counts only while it is still `Merged`, where `lastPhaseTransitionTime` is the merge's. A request proposed after the merge (the fix did not help) is decided as usual. The executor's time of the merge stands in for the Git host's, so a request created in the seconds between the merge and the executor seeing it counts as a duplicate too: it was proposed without the fix in view.
- **Still in effect.** The merged change must still hold, read from the live target the executor already reads (`get` on the target kinds): the field holds the merged request's desired value, or, within 10 minutes (`TargetStateTimeout`) of the merge, still its previous value (Flux has not applied the merge yet). A merge that was reverted and applied, a target changed since by anything else, deleted or replaced, or a merge older than 10 minutes that never reached the target, supersedes nothing: the duplicate is decided as usual, and its own previous value decides whether it is stale (fail toward not superseding). The live target is the cheapest sound signal: the target branch head would need a Git read and the patcher for every check, which a duplicate waiting behind the lock must not make, and the merged request's phase cannot tell a revert, since it stays `Merged` until verification is built (#92). A revert in the 10 minutes before Flux applies the merge is not seen; the duplicate it supersedes opened nothing, and the Incident is analyzed again if the symptom returns.
- **When.** Each time an undecided request is looked at, before anything is written to Git: for a request that is not `Queued` before its policy check, so neither a wait for the target's state (a request in a wait is not `Queued`) nor a refusal comes first; for a `Queued` request each time the limits of 10.7 let it through, before its policy check (so a duplicate waiting behind the lock reads no Git); and for every request under the admission lock right before an allowed decision takes the lock. The holder releases the lock in the status write that sets `Merged`, and every candidate the cache does not show ended is read again from the API server, so a check of the limits that found the lock free is always followed by a read that finds the merge. Two duplicates that run at once each write only their own status.
- **What it writes.** The duplicate's own status: phase `Superseded`, `PlanFresh` `False`, `holdsTarget` false, `Queued` removed. Nothing is written to Git (invariant 3), and the executor uses only the reads and the status write it already has (invariant 6).
- **Other Incidents.** A merge of another Incident's request supersedes nothing, even for the same target and field: one root cause that opens several Incidents (#432) is not covered. Such a request is decided as usual once the lock is free: once the merge is applied, the policy check finds the live value no longer the previous value it claims (`Denied`, `PreviousValueMismatch`); while Flux is still applying it, the request waits (6.2) and ends `NeedsIntervention` (`KustomizationNotReady`) if that takes more than 10 minutes.
- **Merged first.** The merge is the phase `Merged`, not the delivery: `status.pullRequest.appliedObservedAt` is recorded only for a Deployment or StatefulSet target, and the lock is released at the merge, so a check that waited for the delivery would let the duplicates through first. A merged change that Flux then fails to apply shows on the merged request (`RolledOut`), and the Incident is analyzed again if the symptom returns.

### 6.6.4. Requests after a pull request closed without a merge

<span class="sw-status sw-built">built</span> (#643) Requests for the same target and field may wait behind the target lock while another one's pull request is reviewed. When a reviewer closes that pull request without a merge, the reviewer said no to the change, and the waiting requests were planned without knowing it. Each of them ends `Superseded` with reason `AnotherRequestClosed` on `PlanFresh`, whose message names the closed request and its pull request. It does not go back to the analyzer: if the symptom continues, the Incident's next analysis after its cooldown proposes again, with the closed request in its history. Before #643 a waiting request took the lock once it was released and opened a second pull request for the same field: in the lab run of 2026-11-10 one OOM opened three Incidents, the first pull request (72Mi) was closed unmerged, and a waiting request opened another (64Mi) a minute later, which a person closed by hand. A check of the waiting request against the target's current state cannot see this: the target had not changed.

- **Which requests.** The same as in 6.6.3: no phase or `Pending`, no recorded pull request, no lock, no supersede chain; of any Incident.
- **The record.** A request the limits hold back (`Queued`, with `TargetLocked` or a reason that comes before it, such as `CircuitOpen`) while its target is locked records the requests that hold the lock on that target (name and UID, at most four: those for the same field first, then by name) in `status.queuedBehind`, in the status write that queues it. When a later check finds other holders (a handoff moved the lock to the replacement, or another request took it first), the recorded ones are checked first, as below, and only then replaced. Once set the record is never removed or emptied (a validation rule refuses that write; its entries may be replaced); it is read only while the request is undecided.
- **Which close.** A recorded request, read from the API server (the cache may not show the close yet, and `checkHeld` records a close of a stopped holder without a phase change), with the same UID as recorded, the same target UID, the same action type and the same field (`sourcemap.FieldFor`), that ended `NeedsIntervention` with reason `ChangeRequestClosed` (what a close without a merge keeps writing, [ADR-0030](../adr/0030-merge-receipt-and-verification-outcomes.md) section 6) (on `Approved` once its pull request was recorded, or on `PullRequestOpened` before, which also covers a holder that had stopped for another reason before `checkHeld` saw the close). A pull request Shadewright closed itself because the plan went stale (`Superseded`, `PullRequestClosed`) is no reviewer's no and ends nothing; nor does any other end, a deleted holder or one recreated under the same name. A request for another field of the same target (another container), or one that never waited behind the closed request (created, or first checked, after the lock was released), is decided as usual, whatever the clocks say. A request that did wait behind it ends even when it was created after the close on the Git host: it was planned without knowing it.
- **No clock.** The decision reads only recorded state (invariant 5): the record, the holder's final phase and reason, its target and field. Comparing the holder's `lastPhaseTransitionTime` with this request's `creationTimestamp` would be wrong both ways: `creationTimestamp` keeps whole seconds, the phase time is when the executor saw the close (after an outage, minutes later), and a holder that stopped before the close keeps its earlier phase time.
- **Known limits.** A request behind more than four holders records four of them; a holder for its field is recorded before the others, but when more than four hold the lock for that field, one may be unrecorded, and its close then ends nothing. A holder deleted before the waiter reads it ends nothing either, since its phase and reason are gone; the waiter is then decided as before #643 and may open a pull request for the same field.
- **Merged first.** When both a merge (6.6.3) and a close apply, the merge's reason is written.
- **When and what.** The same checks and status write as 6.6.3, at the same points, and also when a `Queued` request's holders change. A read that fails is returned and the request is looked at again, neither ended nor let through on it.
- **The Incident.** A request ended this way is like any other `Superseded` one for its Incident's closure (section 3): the Incident follows a newer request that names it, or moves to `Escalated` when there is none, for a person to look at.

## 6.7. Finalizer and deletion

A finalizer is used only where deleting the CR needs bounded external cleanup (closing an open PR, disabling auto-merge). The pull request step adds it before its first Git write.

Deleting a request closes its pull request, and then removes the finalizer, which releases the lock on its target. This holds in these cases:

- before merge;
- after it stopped in `NeedsIntervention` or `Expired`;
- while it holds the lock on its target (`status.holdsTarget`, in any phase, 10.7).

A repository that left the settings or changed its target branch, or a pull request that is already gone or merged, does not block the deletion.

While writes are paused the deletion waits, because closing is a write. Deleting the CR never rolls back a deployment.

## 6.8. Details

| Topic | Detail |
| --- | --- |
| Queued recheck | Every 30 seconds; every 10 minutes (and when the holder changes) behind a lock held by a stopped request |
| Freshness recheck before merge | Every five minutes, and at once on any change to the request (once its pull request waits, only when none ran in the last five minutes) |
| Look at a waiting pull request (merge, close, moved head) | Every `--pull-request-poll-interval`, one minute by default (30 s to 5 min); one GitHub REST call a look. The time since the last full recheck is kept in memory, so a restart or a new leader starts with a full recheck |
| Merge to the target branch | Does not wake the request; seen at the next recheck, at most five minutes later |
| `status.deadline` | 30 minutes by default; set by the policy check in the write that sets `PolicyChecked`; not set while writes are paused |
| KEDA not reported yet | Recheck delay doubles from 15 s to 2 min; after 10 minutes `TargetStateTimeout` gives `NeedsIntervention` (`ScaledObjectNotReady`) |
| Flux still reconciling (`KustomizationReconciling`) | Same delays and the same 10 minutes, counted once for a wait that moves between the two reasons; then `NeedsIntervention` (`KustomizationNotReady`). `Queued` ends a wait, so a later one starts its own 10 minutes. The freshness check of a decided request retries for 10 minutes too (`PlanFresh` `Unknown`), then `SourceChanged`; a stale target file is not waited for |
| Retry limit | Planned |
| Wait kept in the call | Up to 15 seconds; longer requeues after the delay, cut to `status.deadline` |
| Secondary rate limit message | Searched in at most 16 KiB of the body, never kept |
| Delay for a rate-limited answer | `Retry-After`, else until `x-ratelimit-reset`, else one minute |
| Create of branch or pull request after an unexplained 422 | Up to four attempts with backoff |
| Read cache | `sourcemap.ReadCache`: at most 4,096 reads and 16 MiB; executor memory 256 Mi |
| Finalizer | `remediation.shadewright.io/close-pull-request` |
| Pull request number in messages | Example: "Pull request #26 was closed without a merge" |
