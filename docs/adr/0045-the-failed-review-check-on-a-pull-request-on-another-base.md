# ADR-0045: The failed review check on a pull request on another base

- Status: Proposed
- Date: 2026-11-20

## Context

Issue [#925](https://github.com/pkalin/shadewright/issues/925), built in pull request [#939](https://github.com/pkalin/shadewright/pull/939). [ADR-0043](0043-the-executor-follows-its-own-pull-request-to-any-base.md) lets the executor follow its own pull request when a person changes its base branch, and lists "check run" among the things it never does on another base. [ADR-0028](0028-reviewer-hints-through-the-pull-request.md) section 5 makes the check run `shadewright/review` the merge check of a pull request the executor opened, when reviewer hints are on (`--reviewer-hints`).

A check run belongs to a commit, not to a base. A passing check written while the pull request was on the target branch stays the latest check of its head after a person moves the pull request to another base. If the request then stops (its plan goes stale, it is cancelled, or it stops for another reason), nobody updates the check. A person can merge, on that old pass, a change the executor no longer stands behind, and moving the pull request back to the target branch does the same. The only fix is to complete the check as failed, which is a Git write on a pull request that is on another base. ADR-0043 listed that write under "never", so this decision widens a write.

[ADR-0021](0021-amending-accepted-adrs.md) says an amendment never widens what is allowed on its own authority: another accepted ADR has to decide it. The owner confirmed the widening in an issue comment on 2026-11-20, and that is not an ADR. So the write gets this ADR, accepted before #939 merges. ADR-0043 then links it in an amendment, and this ADR links back. ADR-0021 is not changed.

## Decision

[![With reviewer hints on, a stale plan, a cancel or a stop that finds the request's own pull request open on another base completes the check run shadewright/review on the recorded head as failed and records it Stale. The summary holds fixed text and one constant, never text from the code host. Nothing is written with hints off, when the pull request is merged on the other base, or when its read fails. Pause, a target branch change, a gone repository or a failing Git host delay the write and are said in the condition ReviewCheckNotFailed. Never on another base: push, comment, merge, update of title or body. No new read, no new grant.](../assets/diagrams/review-check-other-base.svg)](../assets/diagrams/review-check-other-base.svg "Open full size")

**What is written.** One check run: name `shadewright/review`, conclusion `failure`, on the recorded head commit of the request's own pull request, when the executor reads that pull request by its number and finds it open on a base other than the target branch. Three paths write it:

- A stale plan. Its cleanup fails the check before each try of the close.
- A cancel. It fails the check before it tries the close.
- A stop. The step that stops the request for a pull request open on another base writes it, and so does every look of a stopped holder that reads its pull request open there.

The executor then records state `Stale` for that head in the request's status, and the request never writes the check again, with any state. A failed write is not final: the stop goes on, and the next look of the holder writes it again. A look of a stopped holder whose failed check is recorded first reads the latest `shadewright/review` check of the head, and writes the failed check again only when another state, or none, shows last.

**Conditions.** All of them:

- Reviewer hints are on. With hints off the executor does not own the check and writes nothing.
- The pull request is the request's own recorded pull request, which the request still owns (no handoff, no handoff intent).
- It is read open on another base. A pull request merged on another base gets no check, and one not read at all (a failed read) gets none either; the read is retried.
- The cause is a stale plan, a cancel or a stop. Nothing else writes it on another base.

It is the only write to a pull request on another base besides the close that the finalizer of a deleted request makes (ADR-0043).

**What it carries.** A fixed title and a fixed summary, with one constant in the summary of a stale plan and of a stop: the stale reason of the plan, the phase the request stopped in, or `ChangeRequestOnOtherBase`. The summary of a cancel has no constant. The text never holds the base name, a branch name, a hint, a comment or any other text from the code host.

**What is read.** Nothing new. The read of the pull request by its number is ADR-0043's. The latest check of the head (`ReviewCheckRun`) and the read of the request live from the API server before the merge gate writes are ADR-0028's, as amended. The check is written to the recorded head, which the executor itself pushed or adopted.

**What is granted.** Nothing. The write is the check run of ADR-0028 and uses the App permission `checks: write`, which every token request already names.

**What is never done on another base.** No push, comment, merge, update of title or body, and no auto-merge, as ADR-0043 says. No check is written there when the pull request is merged, when hints are off, or while the kill switch pauses writes. A repository entry that names another target branch than the recorded one permits reads only, so it writes nothing either, and a repository that left the settings cannot be opened.

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
- (d) A pull request merged on another base gets no check. The merge has happened, so a check cannot prevent it, and ADR-0043 limit (b) stands.
- (e) A window between the read and the write. A person who moves the pull request again between the two gets the check on a pull request that is now elsewhere. A check run belongs to the commit, so it shows on every pull request with that head, including one on the target branch. That can only block.
- (f) The failed check is final for the request. Moving the pull request back does not clear it, and the request writes nothing else. A person who wants the change makes a new request.
- (g) It does not make the lock follow a reopened pull request: that is [#940](https://github.com/pkalin/shadewright/issues/940).

## Alternatives considered

- Write nothing. A passing check from before the move stays the last one, and a person can merge a change the executor stopped. This is the defect.
- Close the pull request on a stop. A person moved it on purpose, and closing it destroys their work. ADR-0043 already decided that a stop closes nothing on another base.
- Amend ADR-0043 alone. It would widen a write on the authority of an amendment, which ADR-0021 refuses.
- Write the check on a pull request merged on another base too. It prevents nothing there and adds a write.

## Amendments

None.
