# ADR-0045: The failed review check on a pull request on another base

- Status: Accepted
- Date: 2026-11-20

## Context

Issue [#925](https://github.com/pkalin/shadewright/issues/925), built in pull request [#939](https://github.com/pkalin/shadewright/pull/939). [ADR-0043](0043-the-executor-follows-its-own-pull-request-to-any-base.md) lets the executor follow its own pull request when a person changes its base branch, and lists "check run" among the things it never does on another base. [ADR-0028](0028-reviewer-hints-through-the-pull-request.md) section 5 makes the check run `shadewright/review` the merge check of a pull request the executor opened, when reviewer hints are on (`--reviewer-hints`).

A check run belongs to a commit, not to a base. A passing check written while the pull request was on the target branch stays the latest check of its head after a person moves the pull request to another base. If the request then stops (its plan goes stale, it is cancelled, or it stops for another reason), nobody updates the check. A person can merge, on that old pass, a change the executor no longer stands behind, and moving the pull request back to the target branch does the same. The only fix is to complete the check as failed, which is a Git write on a pull request that is on another base. ADR-0043 listed that write under "never", so this decision widens a write.

[ADR-0021](0021-amending-accepted-adrs.md) says an amendment never widens what is allowed on its own authority: another accepted ADR has to decide it. The owner confirmed the widening in an issue comment on 2026-11-20 and decided the exact list of writes in a later comment of the same date, and neither is an ADR. So the write gets this ADR, accepted before #939 merges. ADR-0043 then links it in an amendment, and this ADR links back. ADR-0021 is not changed.

## Decision

[![With reviewer hints on, the failed check run shadewright/review is written on the recorded head and recorded Stale in five cases: a stale plan, a cancel or a stop reads the request's own pull request open on another base, a stale plan or a cancel reads it closed on another base, a stopped holder whose own pull request is not open looks while another open pull request from its branch holds the lock and the check is still owed, and a stale plan, a cancel or a stop reads the pull request gone. Whenever the pull request is not read open on the target branch the summary is a fixed variant that promises no close. A pull request merged on another base, or read gone after a new installation of the repository, gets no check, and nothing is written with hints off or when the read fails. Pause, a target branch change, a gone repository or a failing Git host delay the write and are said in the condition ReviewCheckNotFailed. Never on another base: push, comment, merge, update of title or body. No new read and no new grant.](../assets/diagrams/review-check-other-base.svg)](../assets/diagrams/review-check-other-base.svg "Open full size")

**What is written.** One check run: name `shadewright/review`, conclusion `failure`, on the recorded head commit of the request's own pull request, with reviewer hints on. This is the exact list of cases that write it on another base, and there is no other:

1. A stale plan or a cancel reads the pull request by its number and finds it open on a base other than the target branch. The cleanup of the stale plan fails the check before each try of the close, and a cancel fails it before it tries the close.
2. A stop (the step that stops the request for a pull request open on another base, and every look of a stopped holder that reads its own pull request open there) reads it open on another base.
3. A stale plan or a cancel reads the pull request closed on another base (a person moved it, then closed it). A closed pull request can be reopened and merged, so the failed check fails closed: an old pass must not stay the last check of a head that can still merge.
4. A stopped holder looks, its own pull request is not open (read closed or merged, on any base), another open pull request from the request's branch keeps the lock, and the failed check is still owed (condition `ReviewCheckNotFailed` is set). The check belongs to the request's own commit on its own branch, not to one pull request: every pull request from that branch shows it, and the open one can merge on a pass written before the stop. This is the case that keeps the failed check from being covered by an older write.

5. A stale plan, a cancel or a stop reads the pull request gone: the Git host says the number is not the request's own, and a new installation of the repository does not explain it. The check is written on the recorded head all the same. After a new installation of the repository nothing is written, as before.

The executor then records state `Stale` for that head in the request's status, and the request never writes the check again, with any state. A failed write is not final: the stop goes on, and the next look of the holder writes it again. A look of a stopped holder whose failed check is recorded first reads the latest `shadewright/review` check of the head, and writes the failed check again only when another state, or none, shows last.

**A pull request read gone gets the check.** The check run is written on the request's own recorded commit, never on another pull request's head, so it can only keep that commit from merging. Without it an earlier pass stays on that commit while another pull request from the request's branch is open, and that pull request could merge a stopped plan. A commit the host does not know fails the write, and the condition `ReviewCheckNotFailed` shows it. After a new installation of the repository nothing is written, as before. A pull request merged on another base gets none: after a merge a check prevents nothing.

**Conditions.** All of them:

- Reviewer hints are on. With hints off the executor does not own the check and writes nothing.
- The pull request is the request's own recorded pull request, which the request still owns (no handoff, no handoff intent).
- The read found it open or closed on another base (cases 1 to 3), or gone without a new installation of the repository (case 5), or the stopped holder's look is in case 4. A pull request merged on another base, or read gone after a new installation of the repository, gets no check, and one not read at all (a failed read) gets none either; the read is retried.
- The cause is a stale plan, a cancel or a stop.

Beyond the close that the finalizer of a deleted request makes (ADR-0043), these are the only writes to a pull request on another base.

**What it carries.** A fixed title and a fixed summary. When the pull request is not read open on the target branch (on another base, read gone, or a stopped holder's look while its own pull request is not open) the summary is a fixed variant that says nothing is closed there and that a person decides what to do with the pull request, so it promises no close. It holds one constant in the summary of a stale plan and of a stop: the stale reason of the plan, the phase the request stopped in, or `ChangeRequestOnOtherBase`. The summary of a cancel has no constant. The text never holds the base name, a branch name, a hint, a comment or any other text from the code host.

**What is read.** Nothing new. The read of the pull request by its number is ADR-0043's. The latest check of the head (`ReviewCheckRun`) and the read of the request live from the API server before the merge gate writes are ADR-0028's, as amended. The check is written to the recorded head, which the executor itself pushed or adopted.

**What is granted.** Nothing. The write is the check run of ADR-0028 and uses the App permission `checks: write`, which every token request already names.

**What is never done on another base.** No push, comment, merge, update of title or body, and no auto-merge, as ADR-0043 says. No check is written there when the pull request is merged, when the repository was installed anew, when hints are off, or while the kill switch pauses writes. A repository entry that names another target branch than the recorded one permits reads only, so it writes nothing either, and a repository that left the settings cannot be opened.

**How the failed check stays the last one.** The merge gate reads the request live before it writes a check, and writes nothing for a request that is stopped, released, being deleted or gone. When that read fails, the only check it writes is the downgrade of a pass (pending or failed over a success). A stop's failed check is therefore never covered by a check written from an older copy of the request, and a pass never goes on the head without a live read that worked.

**When a write fails.** A write that cannot be made is said in status, not dropped: the condition `ReviewCheckNotFailed` carries the cause (paused writes, settings that permit no write, a refusal or a failure of the Git host) and a Warning Event is recorded when it is set or its cause changes. The earlier check stays on the head until a write goes through, which is the state before this decision, not a new one.

**How the invariants hold.**

- Invariant 3: the write is a failed required check, which can only keep a change from merging. It pushes nothing, merges nothing, and does not bypass a check or change a protected branch.
- Invariant 4: the base name and every other host text stays out of the check run. The summary holds constants of the executor. The head commit is a full object ID taken from the request's status, and the provider refuses any other value.
- Invariant 5: the recorded `Stale` state and the head are what find the work again. A lost status write makes the next look write the same failed run once more, which is harmless, and a restart does not open or close anything.
- Invariant 6: the executor is the only identity that writes it, with the permission it already has. The analyzer and the verifier gain nothing.
- Invariants 1 and 2 are not touched: no model output, no new action.

## Consequences

A pass written before a person moved the pull request no longer decides a merge once the request has stopped. The cost is one more write on a pull request on another base, a failed check run that blocks and cannot approve.

Known limits:

- (a) The check blocks only where the ruleset of that base requires `shadewright/review` with the Shadewright App as its source. The executor cannot verify a ruleset (ADR-0028 section 5). On a base without the rule, the failed check shows but stops nothing.
- (b) The check is on the recorded head. A person who pushes a new commit to the pull request gets a head with no run. A required check without a run blocks the merge (ADR-0028), but an unrequired one does not.
- (c) The write can be late or missing. While writes are paused, the settings entry names another target branch, the repository is gone from the settings, or the Git host fails or refuses the write, the earlier check stays on the head. The request says so in `ReviewCheckNotFailed` and tries again at its next look; a person must not merge on the earlier check. The looks run at `heldRecheckInterval` (10 minutes) while the request holds the lock, so the write has no bound of its own beyond that pace and the life of the lock.
- (d) A pull request merged on another base gets no check. After a merge a check cannot prevent it, and ADR-0043 limit (b) stands. A commit the host does not know fails the write of a gone pull request's check, and `ReviewCheckNotFailed` shows it.
- (e) A window between the read and the write. A person who moves the pull request again between the two gets the check on a pull request that is now elsewhere. A check run belongs to the commit, so it shows on every pull request with that head, including one on the target branch. That can only block.
- (f) The failed check is final for the request. Moving the pull request back does not clear it, and the request writes nothing else. A person who wants the change makes a new request.
- (g) It does not make the lock follow a reopened pull request: that is [#940](https://github.com/pkalin/shadewright/issues/940).

## Alternatives considered

- Write nothing. A passing check from before the move stays the last one, and a person can merge a change the executor stopped. This is the defect.
- Close the pull request on a stop. A person moved it on purpose, and closing it destroys their work. ADR-0043 already decided that a stop closes nothing on another base.
- Amend ADR-0043 alone. It would widen a write on the authority of an amendment, which ADR-0021 refuses.
- Write the check on a pull request merged on another base too. It prevents nothing there and adds a write.
- Write only when the pull request is open on another base. A closed one can be reopened and merged on the old pass, and a stopped holder's own pull request can be closed while another from its branch is open on the same head.
- Write nothing on a pull request read gone. The check is on the request's own recorded commit, so it can only keep that commit from merging, and without it an earlier pass stays while another pull request from the request's branch could merge a stopped plan. A commit the host does not know fails the write, which the condition shows.

## Amendments

None.
