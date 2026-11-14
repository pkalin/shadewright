# ADR-0041: A freshness check at the merge of a pull request

- Status: Proposed
- Date: 2026-11-14

## Context

Issue [#737](https://github.com/pkalin/shadewright/issues/737) found a hole in the cap for a merged stale plan. This ADR settles how to close it. Nothing in it is built.

How to read it. Every statement is one of three kinds:

- **Decided** means the maintainer decided it on 2026-11-14. This ADR only writes it down.
- **Proposed** means this ADR proposes it. It becomes a decision when the ADR is accepted.
- **Open** means nobody has decided it. It is listed under "Open questions" and the ADR does not guess.

### What happens today

Before the merge the executor runs the freshness check ([ADR-0003](0003-gitops-only-typed-remediation.md), design section 6, `internal/policy/freshness`). It answers one question: is the world still the one the plan digest was made for. It runs right before every Git write, and as a full recheck every five minutes while the pull request waits (`freshRecheckInterval`). Between two full rechecks the executor only looks at the pull request, once a minute by default (`PullRequestPollInterval`, 30 seconds to 5 minutes). A definite change makes the check write `PlanFresh` `False` with the reason (`PolicyChanged`, `SettingsChanged`, `TargetChanged` and others), close the pull request and end the request `Superseded`.

That close can fail. When the Git host refuses it for good (#199), the request keeps its phase and watches the pull request, with the mark on it. A person who merges that pull request ends it `Merged`, with the mark kept. `stalePlanMerged` (`internal/controller/phases.go`) reads the mark, and `capStaleSuccess` ends the request `Inconclusive` with `StalePlanMerged` where a fresh one would have ended in a state the circuit breaker counts as a success: `Succeeded`, or `Merged` with `--verify-outcomes` off. With verification on, a failure still ends `Failed`.

### The hole

The cap reads a mark, and the mark is written by the periodic check. A person who merges between a change of the policy (or the settings, or the target) and the next full recheck merges a plan the executor does not yet know is stale. `stop` records `Merged` with no `PlanFresh` condition, and the request goes on as a fresh one.

Lab run of 2026-11-14, chart `0.5.0-rc.2`, scenario `oom`, row 11 of `scenarios/MATRIX.md` in the fleet repository (UTC):

| Time | Event |
| --- | --- |
| 19:26:09 | Pull request opened (web 48Mi to 72Mi), `status.policyRevision: demo-memory/1` |
| 19:26:44 | `demo-memory` changed, generation 2 |
| 19:26:51 | Pull request merged by a person |
| 19:27:31 | `Approved=True/ChangeRequestMerged`, no `PlanFresh` condition |
| 19:39:16 | `Verified=True/NoRecurrence`, phase `Succeeded` |

The matrix expected `Inconclusive` with `StalePlanMerged`. The new value was still inside the bounds of the changed policy here. Had the policy excluded it, the result would be a `Succeeded` request for a change the current policy refuses.

The window is not small: it is up to the full recheck period (five minutes) plus the poll interval after every change of the policy, and a person who edits a policy and merges right after is the case this ADR is about.

## Decision

### 1. A freshness check when the executor sees the merge (Decided 2026-11-14)

When the executor sees that the pull request was merged, it checks freshness once more before it records the merge: the policy generation, the settings and the target against what the plan recorded, whatever the periodic check has seen. It does so in every path that records a merge: the look at the recorded pull request (`watchStep`), a merge found for a pull request never recorded (`endMerged`), and a merge right after a handoff (`recordHandoff`).

### 2. Which reads it makes, and what a failed read does (Proposed)

The merge-time check is a reduced form of the periodic one. It decides from recorded state and from three reads of the API server, and from nothing else:

- **Recorded state:** `status.policyUID` and `status.policyRevision` (name and generation), `status.planRepository`, `status.source.targetBranch`, `status.planDigest`, and `spec.targetRef` with its UID.
- **Reads:** the `RemediationPolicy` the request names, the `RemediationSettings` `default`, and the target object, each by a `get` straight from the API server (the same reader `loadPlan` uses).
- **The comparisons** are those of `loadPlan` and `checkTarget`: the policy has the recorded UID and revision; the action's module is still enabled; the repository entry of the plan (URL and installation) still exists with the recorded target branch; recomputing the plan digest from the recorded request, source, policy and settings gives `status.planDigest`; the target has the recorded UID and still matches the policy's `objectSelector`.

It makes **no** Git read, no source mapping, no Flux read and no read of the target's field. That is on purpose. After the merge, the target file in Git holds the new value and Flux may already have applied it, so `checkFile` and the "previous value" part of `checkTarget` would call every merged plan stale. The periodic check keeps them, because it runs before the merge.

It compares no timestamp, and certainly not one of the Git host with one of the cluster. Whether the plan is stale is a function of the recorded plan and of what the API server holds when the executor looks. The pause (`spec.paused`) does not block it, as it does not block recording a merge: `loadPlan` returns `ErrPaused` first, so the merge-time check needs a variant that reads the settings without that test. It makes no Git write, so the kill switch has nothing to gate.

**A failed read is an error for a retry.** A read that fails for any reason but "not found" (a timeout, a 5xx, a cancelled context) is neither fresh nor stale: the request keeps its phase (`PRCreated` or `AwaitingApproval`), writes nothing, and the next look at the pull request, which sees it merged again, retries. A merge is never recorded as fresh because a read failed. A "not found" is a definite answer: a deleted policy, a missing settings object or a missing target is a change (`PolicyChanged`, `SettingsChanged`, `TargetChanged`).

### 3. A stale plan that the current policy still allows ends Inconclusive (Decided 2026-11-14)

The merge-time check writes `PlanFresh` `False` with the stale reason in the same status write that sets `Merged` (with the receipt, as [ADR-0030](0030-merge-receipt-and-verification-outcomes.md) section 1 has it). From there the existing code takes over unchanged: `stalePlanMerged` is true, `capStaleSuccess` ends the request `Inconclusive` with `StalePlanMerged`, and `breakerOutcome` reads it as no outcome. The message of `capStaleSuccess` says the plan "could not [be] close[d]"; for a plan found stale at the merge it must say that it was found stale when the merge was seen (Proposed wording change).

### 4. A stale plan that the current policy would refuse ends NeedsIntervention (Decided 2026-11-14; the name is Proposed)

Whether the current policy would allow the change is asked of `policy.Preflight` (`internal/policy/decide.go`) with the recorded request, the policy and the settings just read, and the live target. These are the field checks of `Decide` in the same order with the same reasons: the request may use the policy, the action is in the catalog, the module is enabled, the target is in a covered namespace and matches the selector, and the values are inside the policy's bounds. No second rule set exists, and the model is not asked (invariant 1).

If it refuses, the request ends `NeedsIntervention` with the new reason `MergedAgainstPolicy`. The merge is a fact and stays on the request: the receipt and the merge commit are recorded in the same write (the validation rule on `status.mergeReceipt` allows any phase after the merge), `PlanFresh` is `False` with the stale reason, and a Warning Event names the pull request and the refusal. No verification runs, and **Shadewright opens no revert proposal by itself**: a person merged it, so a person decides. The message says what the current policy refuses and that the change may be applied by Flux anyway.

### 5. The circuit breaker (Decided 2026-11-14 that neither is a success; the count of the second is Open)

Today `breakerOutcome` (`internal/controller/limits.go`) counts `Merged` and `Succeeded` as successes and `Failed` and `NeedsIntervention` as failures. `Inconclusive` is neutral. A `Merged` request that carries the mark of a stale plan is already read as no outcome (`stalePlanMerged`, #199), so the case in section 3 is not a success, today and with this ADR, as soon as the mark is written at the merge. The case in section 4 would count as a **failure**, because `NeedsIntervention` is one. Proposed: it is read as no outcome too, with a case in `breakerOutcome` for the reason, because the change did not fail, the policy moved under it, and a person's merge must not push the breaker toward opening. This departs from the phase table (`TestEveryPhaseIsClassified` classifies by phase), and the build adjusts that test. Whether it counts as a failure instead is an open question.

### 6. With verification on, a failure still ends Failed (Decided 2026-11-14)

Unchanged. Only the good end is capped: where a fresh plan would end `Succeeded` a stale one ends `Inconclusive`, and a request that would end `Failed` ends `Failed`. Section 4 ends before verification, so it never reaches that point.

[![Merge-time freshness check: when the executor sees a pull request merged, it reads the policy, the settings and the target from the API server and compares them with the recorded plan, with no Git or Flux read; a failed read retries and records nothing; a fresh plan goes on as today; a stale plan that the current policy would allow ends Inconclusive with StalePlanMerged; one that it would refuse ends NeedsIntervention with MergedAgainstPolicy, with no revert proposal and no Git write](../assets/diagrams/merge-time-freshness.svg)](../assets/diagrams/merge-time-freshness.svg "Open full size")

```mermaid
sequenceDiagram
    participant P as Person
    participant G as Git host
    participant E as Executor
    participant A as API server
    P->>A: edit the RemediationPolicy (generation 2)
    P->>G: merge the pull request
    E->>G: look at the recorded pull request
    G-->>E: merged, merge commit
    E->>A: get policy, settings, target
    alt a read fails
        A-->>E: error
        Note over E: nothing recorded, phase kept, retry at the next look
    else all reads answer
        A-->>E: policy generation 2, settings, target
        Note over E: compare with status.policyRevision, planRepository, planDigest, targetRef.uid
        alt same as recorded
            E->>A: status: Merged with receipt (as today)
        else changed and policy.Preflight allows
            E->>A: status: Merged with receipt, PlanFresh False (PolicyChanged)
            Note over E: capStaleSuccess ends it Inconclusive, StalePlanMerged
        else changed and policy.Preflight refuses
            E->>A: status: NeedsIntervention, MergedAgainstPolicy, receipt, PlanFresh False
            Note over E: no revert, no Git write, a Warning Event
        end
    end
```

## How the invariants hold

- **Invariant 2 (closed catalog, a policy rule for each action).** No action, parameter or rule is added. The check asks the same `policy.Preflight` code that decided the plan, with the policy as it is now, so a bound the maintainer tightened is applied to a change already merged. The rule for an action stays one place. (The issue lists invariant 2 as "policy decides"; in `AGENTS.md` that is the second half of 1, and the digest is also 1 and 5. This ADR touches all three.)
- **Invariant 3 (changes through Git only).** The merge-time check writes no Git object, opens no pull request, pushes nothing, closes nothing and merges nothing. Its outcome is a status write on a request the executor already owns. Refusing to propose a revert (section 4) keeps it that way: a revert is a new change of the cluster, and it needs its own decision by a person. A pull request that a person merged is never reverted by Shadewright in its own right.
- **Invariant 5 (idempotent, never exactly once).** The verdict is a pure function of the recorded plan and the policy, settings and target the API server holds. Run twice on the same inputs it gives the same answer. It is written in the same status write that sets the phase, so a restart finds the old phase (and looks again) or the new one (and is done), never a merge recorded without its verdict. Branch names and markers are untouched. No decision compares two clocks.
- **Invariant 1 (model output is never authorization).** The model plays no part. The executor still acts on a plan whose digest matches the approved one: a mismatch (`PlanChanged`) is one of the reasons for the stale verdict.

## Threats

- **Someone changes the policy to get a merged change marked stale.** What they get is a lower reported outcome: `Inconclusive` instead of `Succeeded`, or `NeedsIntervention` instead of `Succeeded`. The change is already merged and nothing in the check can un-merge, apply, revert or block it. Changing a `RemediationPolicy` needs the right to write one, and anyone who holds it can do worse directly (widen the bounds, for example). The mark is also true: the plan did not match the policy at the merge. The effect on the breaker is none or small (section 5), so it cannot be used to open it.
- **Someone changes the policy to make a merged change look fresh.** Not possible: any edit bumps the generation, a delete and recreate changes the UID, and an edit that is reverted still leaves another generation. The check compares against the recorded values, not against a copy of the earlier policy.
- **A forged `PlanFresh` condition.** Only the executor writes request status (invariant 6); nothing here changes that.
- **A read that never answers.** It leaves the request in `PRCreated` or `AwaitingApproval` with its lock, and the existing retry reporting (conditions and Events of the look at the pull request) says so. A request that cannot read the API server cannot do anything else either.

## The window that remains

The executor sees the merge at its next look at the pull request: up to the poll interval after it, one minute by default and at most five. Between the merge in Git and that look, nothing is recorded. This ADR does not shorten that time, and it does not need to: what the check compares is the state when the executor looks, against what the plan recorded.

- A policy edit **before** the merge is always seen, whatever the delay, because the generation moved and does not move back.
- A policy edit **between the merge and the look** marks the plan stale although it was fresh at the merge. The executor cannot know the policy at the instant of the merge without comparing a Git time with a cluster time, which it does not do. The error is on the safe side: it only lowers the reported outcome.
- A policy edit **after** the look is not a stale plan at the merge, and is out of scope.

What the check does not see, because it needs a source mapping that reads Git and Flux: a `gitOps` rule of the policy that was narrowed (allowed paths) and the readiness of a ScaledObject (`checkTargetState`). The periodic check keeps both before the merge. Whether the merge-time check should also run the source checks is an open question.

## Matrix row 11

Row 11 of `scenarios/MATRIX.md` expects `Inconclusive` with `StalePlanMerged` and says nothing about timing. With this ADR built it is reproducible without luck: change `demo-memory` (any change that bumps its generation), then merge the pull request, with no wait for the periodic check and no need to refuse the close. The merge-time check finds generation 2 against `demo-memory/1`. On 2026-11-14 the merge was 7 seconds after the change and the request ended `Succeeded`.

A second row is needed for section 4: change `demo-memory` so that the proposed value is outside its bounds (for example a maximum of 64Mi for a plan of 72Mi), then merge. Expected: `NeedsIntervention` with `MergedAgainstPolicy`, no second pull request, no revert request. The fleet repository keeps the matrix, so the row is added there with the build task, not by this ADR.

## Acceptance tests

Each must fail without the change. Names are the intent, not the final ones.

1. **Policy edited, then merged, no periodic check in between** (envtest, the row of #737): a request in `AwaitingApproval`; bump the policy generation, keeping the value in bounds; the pull request reads merged. Expect `Inconclusive` with `StalePlanMerged` and `PlanFresh` `False` with `PolicyChanged`, with `--verify-outcomes` on and off. Today: `Succeeded` or `Merged`, with no `PlanFresh`.
2. **Policy now refuses the change:** the same with a bound that excludes the value. Expect `NeedsIntervention` with `MergedAgainstPolicy`, the receipt recorded, no `RemediationRequest` with `spec.reverts`, no Git write by any provider call. Today: `Succeeded`.
3. **Settings and target:** a repository entry with another URL or installation, a module disabled, a target with another UID and a target relabelled out of the selector each end as in 1 or 2, by whether `Preflight` allows. Each fails today.
4. **A failed read retries:** the reader fails for the policy, then for the settings, then for the target; the pull request is merged. Expect the phase kept, nothing written, no `Merged`; once the reads answer, the right end. A merge is never recorded as fresh after a failed read. (A version that treats an error as "fresh" fails this test; today's code has no read, so the test is written against the new function.)
5. **Not a false positive:** unchanged policy, settings and target, the target file in Git already holds the new value and the live target the applied one. Expect the request goes on as today. This test passes before the change; it guards against building the check on the full `Check`, and fails if `checkFile` or the previous-value test runs at the merge.
6. **Pause does not block:** `spec.paused` set, the merge seen, a changed policy. Expect the verdict is still recorded.
7. **Idempotent:** the status write is lost after the check (a conflict), then retried, and the controller restarted between; the end is the same, with one Event and one receipt.
8. **All three paths:** a merge found by `endMerged` (never recorded) and one right after a handoff get the same check. Each fails today.
9. **The breaker:** a request ended by 1 is no success, one ended by 2 is no success and (as proposed) no failure, with the circuit staying closed after three of them; `TestEveryPhaseIsClassified` still passes.
10. **Lab:** rows 11 and 11b of the matrix, once, on the lab with verification on.

## Consequences

- A change of the policy, the settings or the target before a merge can no longer end `Succeeded`, whatever the timing of the periodic check. A merge of a plan the current policy refuses stops with a person, with the facts on the request.
- Each recorded merge costs three more reads of the API server, always served from the same reads the periodic check makes. No Git or Flux call is added.
- `stop` (or the three callers) gains a step before `Merged`, and the status write that ends `NeedsIntervention` after a merge needs a small change, since `stop` today sets `Approved` False for any phase but `Merged`. The `Approved` condition of such a request should keep saying that the pull request was merged. The exact conditions are decided in the build.
- The documents that say a stale merge is "known" (`docs/request-lifecycle.md`, `docs/troubleshooting.md`, design section 6, threat model G5.3) are updated with the build. They are not changed by this ADR, since nothing is built.
- A person who edits a policy during a review and merges gets a lower outcome than before for a change that was fine. That is the intended price.

## Alternatives considered

- **A shorter period for the periodic check.** Rejected. It narrows the window and never closes it: any period leaves a moment in which a merge finds no mark. A full recheck reads the Git host and Flux (source mapping, file reads) and costs calls; the poll interval is bounded from below by 30 seconds for the same reason (`MinPullRequestPollInterval`). A merge-time check costs three API reads once, and closes the window for the policy, the settings and the target.
- **A required status check on the pull request that goes red when the plan is stale.** Not chosen, and not rejected forever. It would stop the merge instead of lowering the outcome after it. What the Git App may do: it holds `checks: write` ([ADR-0028](0028-reviewer-hints-through-the-pull-request.md) section 5, [ADR-0006](0006-github-app-key-in-key-vault.md)), so it can create and complete a check run for a head of its own pull request, as `shadewright/review` does. What it may not do: merge, approve, dismiss reviews, change a ruleset or bypass one (invariant 3); the repository's ruleset must name the check, with the App as the expected source, which is a setting that only the maintainer changes (#199 is about the same limit from the other side: when the App lacks a permission, a person has to act). Against it: the check goes red only when the executor has seen the change, so the window moves from "the merge" to "the check", and a person can merge in it all the same; a check that stays pending while the executor is down blocks every merge, or, if not required, protects nothing; it exists only with `--reviewer-hints`, today off by default; and it needs a gate whose failure mode is argued in its own ADR. It could complement the merge-time check later. It is an open question below.
- **Compare the merge time of the Git host with the time of the policy edit.** Rejected: two clocks (invariant 5, working agreement). The recorded generation is the only comparison.
- **Open a revert proposal when the current policy refuses the merged change.** Rejected for now (decided 2026-11-14): a person merged it, so a person decides. A revert is also a new change of a cluster that the policy must decide on again, and the policy that refuses the change may refuse its revert.
- **Run the whole `Check` at the merge.** Rejected: it compares the target file and the live value with the previous value, which a merge has changed by definition. See the false-positive test.

## Open questions

1. Is the end of section 4 a failure for the circuit breaker, or no outcome as proposed in section 5?
2. Should the merge-time check also run the source checks (the `gitOps` rules of the policy, the readiness of a ScaledObject)? They need source mapping and so Git and Flux reads, which can fail and be retried, and after the merge Git holds the change.
3. Is a target that is gone at the merge a stale plan that ends `Inconclusive` (proposed: the policy cannot be asked about an object that is not there) or `NeedsIntervention`?
4. Should a required status check be added later (see the alternative), and under which setting?
5. Which condition carries the reason `MergedAgainstPolicy`, given that `Approved` is `True` for a merge?
6. Should the matrix get row 11b in the fleet repository with the build task?

## Amendments

None.
