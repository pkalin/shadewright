# ADR-0043: The executor follows its own pull request to any base

- Status: Accepted
- Date: 2026-11-19

## Context

Issue [#892](https://github.com/pkalin/shadewright/issues/892). A person can change the base branch of a pull request on the code host. The executor read its pull request on the target branch only, so a pull request moved to another base looked closed or gone. The request then released the target lock, or opened a second pull request, while the first could still be merged. [ADR-0030](0030-merge-receipt-and-verification-outcomes.md) section 1 says that a restart finds the existing pull request, and [ADR-0021](0021-amending-accepted-adrs.md) says an amendment never widens what is allowed on its own authority. The fix does widen two things, so they are decided here, and the amendments of ADR-0030 and [ADR-0032](0032-azure-devops-as-a-second-git-provider.md) only record them.

The two things:

- A read. `GetChangeRequest` (by number) and `ListChangeRequestsAnyBase` answer a pull request from the request's own `shadewright/` branch on every base branch of the configured repository, where `GetChangeRequest` and `ListChangeRequests` answered only on the target branch.
- One write. `CloseChangeRequestAnyBase` closes the executor's own marked pull request on a base other than the target branch. Its callers are both in the finalizer of a deleted request: `closeOwnOnOtherBases` (every open pull request with the request's marker on another base, with or without a recorded number) and `closeAllOwn` (the recorded number, where the read finds it open on another base). The kill switch gates it like `CloseChangeRequest`.

## Decision

[![A pull request from the request's own branch is read on every base. Open on another base: NeedsIntervention with ChangeRequestOnOtherBase, the lock is kept and nothing is written to it. Merged on another base: NeedsIntervention with ChangeRequestOnOtherBase, no merge recorded. Closed on another base: handled as closed. From a fork, another repository or another branch: ErrUntrustedAnswer for the whole call of a listing, not the request's by a recorded number. Never on another base: push, comment, check run, merge.](../assets/diagrams/other-base-pull-request.svg)](../assets/diagrams/other-base-pull-request.svg "Open full size")

**What is read.** Only pull requests whose head is a `shadewright/` branch of the configured repository and whose base is a branch of that repository. An entry from a fork, from another repository or from another branch is `ErrUntrustedAnswer` for the whole call, never skipped, because the answer is then not to the question asked. A pull request with a number that is not from such a branch is not found. More than `MaxBranchChangeRequests` (100) pull requests from the branch is `ErrTooLarge`: the read fails, the lock stays, and the look is retried. A base name is text from the code host (invariant 4). The provider returns it only when it passes the rule for target branches and leaves it out otherwise; the pull request is still returned with its number, which is what the lock is kept on. A condition message shows the name redacted and cut to 255 bytes.

**What is closed.** Only the executor's own pull request, and only on the deletion of the request: one that carries the request's marker, found by its recorded number or by the listing of every base. The close is retried like the close on the target branch. A recorded number is closed only when that pull request also carries the marker (an unmarked one is read as not the request's and left open), and the finalizer goes after the ones it found, with the exceptions in limit (c). A pull request without the marker is left open. A stop, a cancel, a deadline or a stale plan closes only on the target branch. The one that expires or is cancelled keeps the lock on a pull request open elsewhere and does not close it.

**What is never done on another base.** No push, comment, check run, merge, update of title or body, and no auto-merge, once the executor has seen the pull request there. The executor then writes nothing to that pull request except the close above. "Its own" pull request means one from the request's branch that carries the request's marker, not one the executor's App authored: a person with write access to the repository who opens a pull request from that branch and copies the marker gets the same treatment, as on the target branch, and every effect of that is a kept lock, a stop or a close.

**Verdicts.** The read by number has two verdicts of its own besides open, merged, closed and gone, and every caller handles them (cancel, deadline, supersede, kill switch, deletion, hint look, settlement; a test walks each stop path with the pull request moved):

- Open on another base: the watch and the adoption stop the request in phase `NeedsIntervention` with reason `ChangeRequestOnOtherBase`, naming the pull request, and the request keeps the lock (`status.holdsTarget`), saying in `TargetHeld` that pull request #N is open on another base and that closing it releases the lock. Moving it back to the target branch does not: the pull request is then open on the target and keeps the lock, and the stopped request only looks at the lock, so it makes no new freshness check and writes no review check there (limit (e)). Nothing is written to the pull request. A cancel that finds it says so and ends `Cancelled`, with the lock kept while the pull request is open.
- Merged on another base: a merge there delivers nothing the request can verify. The watch and the adoption stop the request in phase `NeedsIntervention` with reason `ChangeRequestOnOtherBase` naming the pull request; it records no merge and keeps no lock. A cancel ends the same way, not `Cancelled`, since the change is merged and may be delivered.
- Closed on another base is handled as closed. A request that lost the write recording its pull request finds it by its marker on any base and in any state, so a restart never opens a second pull request after a person closed, moved or merged the first (invariant 5).

**How the invariants hold.** Invariant 3: the executor opens, pushes and merges nothing on another base, and the one close is of its own marked pull request, the same kind of close it makes on the target branch, on the deletion of the request. It neither bypasses a check nor changes a protected branch. Invariant 5: the marker and the recorded number are what find the pull request again, now on every base, so a restart finds the existing one instead of opening another. Invariant 4: the base name is only compared for equality with the configured target branch (that comparison picks the verdict, the close path and which listed entries are skipped) and shown redacted and bounded; it is never used as a ref, a path or an instruction. A pull request is kept or closed on its number, its head branch, its repository and its marker.

## Consequences

The lock is no longer released because a person moved a pull request. The cost is a second read: with no number recorded, a request lists the pull requests of its branch on every base before it releases, and the finalizer lists them before it goes.

Known limits:

- (a) Only a marked pull request on another base counts for the lock today. A pull request from the same branch without the marker, open on another base, can be moved to the target branch and merged after the lock is released. On the target branch any open pull request from the branch keeps the lock. Decided on [#925](https://github.com/pkalin/shadewright/issues/925) on 2026-11-19: any open pull request from the request's branch keeps the lock on every base, marked or not, while only a marked one is adopted or closed. That change is built there, not in the pull request of #892.
- (b) A merge on another base releases the lock on one read. A base branch that is later merged into the target branch then delivers a change the request did not verify. No request records it. A person is told by the stop reason, `ChangeRequestOnOtherBase`.
- (c) The finalizer lets go without closing in some cases: when a close or a listing still fails 24 hours after the first failure (`cleanupFailingBound`, and the shorter grace for a refusal), and at once when the plan's repository is gone from the settings (`errRepositoryGone`) or its entry names another target branch (`errTargetBranchChanged`), where it lists no other base. A marked pull request can then stay open on another base with no lock, and a person closes it. The Warning Event `PullRequestLeftOpen` names it in the first two cases.
- (d) A cancel, an expiry, a stop or a stale plan closes nothing on another base, so a stopped request can hold the target until a person closes or moves the pull request. `TargetHeld` is the signal.
- (e) A window between a read and a write. The executor reads the pull request and then writes (a handoff's commit to the branch, the review check on the head commit). A person who moves the pull request to another base between the two gets that one write on a pull request that is now elsewhere, and a check run belongs to the commit, so a passing check written before a move also shows on the new base. Branch protection of that base still applies. In the other direction, a pull request moved back to the target branch after the request stopped keeps the review check it had and gets no new one, so a person can merge a plan that is no longer fresh; the request is final and records nothing. Making a stop fail its review check is part of [#925](https://github.com/pkalin/shadewright/issues/925).
- (f) The cap has no time bound. More than 100 pull requests from the branch keep the lock for as long as they exist: the look fails closed and is retried.

## Alternatives considered

- Ignore other bases. This is the defect of #892: a moved pull request looks gone, the lock is released and a second pull request can be opened.
- Refuse to act and leave the moved pull request open for a person. The lock would be kept, but a deleted request would leave an open pull request carrying its marker and branch, which can still merge.
- Close the marked pull request on another base at a cancel or at expiry. A person moved it there on purpose, and a close on a stop is a second write beyond the target branch that the decision of #892 does not take; the cost is limit (d), and `TargetHeld` tells the person what holds the target.
- Move it back to the target branch. That is a write on a pull request a person changed on purpose, and it can fail on the host's rules.

## Amendments

None.
