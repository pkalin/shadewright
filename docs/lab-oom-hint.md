---
title: "Lab runbook: a reviewer hint on the OOM scenario"
---

# Lab runbook: a reviewer hint on the OOM scenario

This run shows [reviewer hints](use.md#give-a-reviewer-hint) on the lab: a comment on the pull request of the OOM
scenario leads to a replacement request that takes over the same pull request and branch with one new commit
([ADR-0028](adr/0028-reviewer-hints-through-the-pull-request.md) section 4; epic
[#533](https://github.com/pkalin/shadewright/issues/533)). It then tries the refusal cases by hand. It continues the
[OOM acceptance run](lab-oom-acceptance.md) and uses its names and placeholders (`<owner>`, the namespace
`shadewright-system`).

!!! note "Reviewer hints are off by default"
    The executor reads hints only with the flag `--reviewer-hints`, which the chart sets from
    `executor.reviewerHints.enabled` ([#631](https://github.com/pkalin/shadewright/issues/631)). Run this with a release
    that has that value, and turn it on as in [Before you start](#before-you-start). Without it, step 3 ends with no
    record in `status.hints` and nothing below happens. With hints on, the executor also replies in the
    pull request to each answered hint and refused command, once per comment
    ([#542](https://github.com/pkalin/shadewright/issues/542)): a fixed template, posted only when the redactor would
    change nothing in it.

!!! warning "Costs money and changes real systems"
    The lab bills while it is up, the analysis calls a real model and spends one unit of the day's analysis budget per
    hint, and the run changes a real pull request and its branch in your fleet repository. Run it yourself; nothing
    here is meant for CI or an agent.

## Before you start

- The [OOM acceptance run](lab-oom-acceptance.md) is done up to step 5: the lab is up, `cluster/lab` holds `main`'s tree
  and the OOM scenario is on. Use its evaluation setup (the fleet's `main` applies the settings, the `demo-memory`
  policy and the analyzer's model). The hand-written request and Incident samples of that run are not used here: the
  analyzer makes the requests.
- The fleet repository's ruleset for `cluster/*` requires the check `shadewright/review` with the Shadewright App as
  the expected source, and lists the maintainer's role as its bypass actor, so the maintainer's own pull requests to
  `cluster/lab` can still merge
  ([turn reviewer hints on](how-to/turn-on-reviewer-hints.md#pull-requests-people-open-to-that-branch)). Shadewright cannot verify this and the merge gate does not hold without it (see
  [use](use.md#give-a-reviewer-hint)). This run never merges, but check it, because step 5 reads the check.
- The executor runs with reviewer hints on: in the fleet, `infrastructure/shadewright-lab` sets
  `executor.reviewerHints.enabled: true` in the HelmRelease values (a fleet pull request, merged to `main` before
  `cluster/lab` is reset), with a release that has the value. Check that the executor logged it:
  `kubectl -n shadewright-system logs deploy/shadewright-executor | grep 'Configured reviewer hints'` shows
  `enabled` true and the required check.
- The executor's GitHub App has `checks: write` besides the four permissions of
  [ADR-0006](adr/0006-github-app-key-in-key-vault.md) (the App's settings page, "Permissions").
- Two GitHub accounts: yours (write access) and a second one with **read** access only, as a collaborator or a member
  of a team with the Read role (Triage is refused too). `gh auth switch` changes between them, or use the web
  interface for the second.
- `az`, `kubectl`, `kubelogin`, `gh` and `jq` are installed.
- The analysis budget has room for at least five analyses (`maxAnalysesPerDay` in the settings, 10 in the fleet).

## 1. Get the first proposal and its pull request

Wait for the Incident of the OOM scenario to be analyzed
([evaluation run](lab-oom-acceptance.md#evaluation-run-the-first-analysis-with-a-real-model), step 2) and for its
request to wait for review:

```bash
kubectl -n shadewright-system get incidents
INC=<name of the OOM Incident>
kubectl -n shadewright-system get incident "$INC" -o jsonpath='{.status.phase}{" "}{.status.lastAnalysisTime}{"\n"}'
kubectl -n shadewright-system get remediationrequests -o wide
```

Expect the Incident `Proposed` and one request in `AwaitingApproval` with a pull request number. If the Incident ended
`Escalated` (for example `MissingData`, as in the first evaluation run), there is no request to hint on: write the
Incident and the request by hand as in [the acceptance run](lab-oom-acceptance.md#6-fill-and-apply-the-samples) and
wait for its pull request; the replacement of this run is then the analyzer's answer to the hint on it.

Name the request and read its pull request:

```bash
RR1=<name of the request>
kubectl -n shadewright-system get rr "$RR1" -o jsonpath='{.status.pullRequest.number}{" "}{.status.pullRequest.headSHA}{"\n"}'
kubectl -n shadewright-system get rr "$RR1" -o jsonpath='{.spec.action}{"\n"}'
gh pr view <number> --repo <owner>/shadewright-fleet --json title,state,headRefName,headRefOid,commits --jq '{title, state, branch: .headRefName, head: .headRefOid, commits: (.commits|length)}'
```

Write down the pull request number, the branch, the head SHA, the title and the proposed memory limit. The model is
shown the bounds of `demo-memory` for the container: from the template's 48Mi, `desired from 64Mi to 72Mi`. The hint
of step 3 must lead to a **different value inside that range** for a replacement to exist: if the first proposal is
64Mi, hint towards 72Mi; if it is 72Mi, towards 64Mi.

The merge gate's check run exists from the start, on every head, also on a pull request with no hint:

```bash
gh api repos/<owner>/shadewright-fleet/commits/<head-sha>/check-runs --jq '.check_runs[] | {name, status, conclusion, app: .app.slug}'
kubectl -n shadewright-system get rr "$RR1" -o jsonpath='{.status.pullRequest.reviewCheck}{"\n"}'
```

Expect a check run `shadewright/review`, `in_progress`, from the Shadewright App, and `reviewCheck` with state
`InProgress` and the same head SHA.

## 2. Wait out the cooldown

The cooldown of 15 minutes per Incident runs from `lastAnalysisTime`. A hint inside it is not analyzed: it is answered
`CoolingDown` and declined (try that on purpose in step 6, not here). Compare:

```bash
date -u +%Y-%m-%dT%H:%M:%SZ
kubectl -n shadewright-system get incident "$INC" -o jsonpath='{.status.lastAnalysisTime}{"\n"}'
```

and go on when the first time is at least 15 minutes after the second.

## 3. Give the hint

As your account, comment on the pull request. The first line must start exactly with `/shadewright reanalyze:` (this
wording is for a first proposal of 64Mi):

```bash
gh pr comment <number> --repo <owner>/shadewright-fleet \
  --body "/shadewright reanalyze: the container peaks near 70Mi, so 64Mi is too tight; propose 72Mi"
```

Note the comment ID from the URL it prints (`#issuecomment-<id>`). The executor polls, so allow about a minute
(`--pull-request-poll-interval`, one minute by default). Then read what it stored:

```bash
kubectl -n shadewright-system get rr "$RR1" -o jsonpath='{range .status.hints[*]}{.commentID}{" "}{.author.login}{" "}{.state}{" "}{.reason}{" "}{.text}{"\n"}{end}'
```

Expect one hint with your comment ID and login, state `Pending` (or already `Reserved`), and the text after
`reanalyze:`, redacted. While the analysis runs, the Incident shows it:

```bash
kubectl -n shadewright-system get incident "$INC" -o jsonpath='{.status.phase}{" "}{.status.handledHint}{"\n"}'
kubectl -n shadewright-system get incident "$INC" -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
kubectl -n shadewright-system logs deploy/shadewright-analyzer | grep -E 'Analyzed Incident|Refused'
```

Expect `AnalysisDue` with reason `ReviewerHint`, then `Analyzing`, with `status.handledHint` naming your comment ID and
the digest of the stored text, and at the end the `Analyzed` reason `Proposed`: a replacement request was created. The
reply to your hint comes after the handoff (step 4).

## 4. Check the replacement and the handoff

```bash
kubectl -n shadewright-system get remediationrequests -o wide
RR2=<name of the new request>
kubectl -n shadewright-system get rr "$RR2" -o jsonpath='{.spec.replaces}{"\n"}{.spec.chainRoot}{"\n"}'
kubectl -n shadewright-system get rr "$RR1" "$RR2" -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,FROM:.status.handoff.from.phase,TO:.status.handoff.to.phase
kubectl -n shadewright-system get rr "$RR1" -o jsonpath='{range .status.hints[*]}{.commentID}{" "}{.state}{"\n"}{end}'
```

Expect:

- A second request whose `spec.replaces` names the first one and your hint (comment ID and digest), and whose
  `spec.chainRoot` is the UID of the first request.
- The replacement in `AwaitingApproval`, its `status.handoff.from.phase` `Done`.
- The first request `Superseded` with reason `HandedOff` (the `PlanFresh` condition), its `status.handoff.to.phase`
  `Done` and your hint `Consumed`.
- Never two requests holding the target at once: the lock moved from the first request to the second.

The pull request and the branch are the same ones, with one new commit:

```bash
gh pr list --repo <owner>/shadewright-fleet --state all --json number,headRefName,state
gh pr view <number> --repo <owner>/shadewright-fleet --json state,headRefName,headRefOid,commits,files --jq '{state, branch: .headRefName, head: .headRefOid, commits: (.commits|length), files: [.files[].path]}'
gh pr diff <number> --repo <owner>/shadewright-fleet
```

Expect:

- Still exactly one pull request for the Incident, **open**; not closed and no second one. The head branch is
  `shadewright/<chainRoot>`, the first request's UID, as in step 1.
- Two commits (the original and the replacement's) and a new head SHA, different from step 1. The only file is still
  `apps/demo/web-deployment.yaml`, and the net diff against `cluster/lab` is one line pair, from `48Mi` to the new value.

The title and body are rewritten for the new plan after the handoff, a step that is retried within bounds and recorded
as `pullRequestUpdate` in `status.handoff.from` of the replacement. Until it is `Done` the body still carries the first
request's marker, which the replacement accepts. Read both again after a minute:

```bash
kubectl -n shadewright-system get rr "$RR2" -o jsonpath='{.status.handoff.from}{"\n"}{.metadata.uid}{" "}{.status.planDigest}{"\n"}'
gh pr view <number> --repo <owner>/shadewright-fleet --json title,body --jq '.title, .body'
```

Expect a title that names the new value and a body whose last line is the marker
`<!-- shadewright request-uid=... plan-digest=sha256:... -->` with the replacement's UID and digest, not the first
request's. If `pullRequestUpdate` is `Refused`, the Git host refused the rewrite for good: record it, it is a finding.

The new head starts a new check run:

```bash
kubectl -n shadewright-system get rr "$RR2" -o jsonpath='{.status.pullRequest.headSHA}{" "}{.status.pullRequest.reviewCheck}{"\n"}'
gh api repos/<owner>/shadewright-fleet/commits/<new-head-sha>/check-runs --jq '.check_runs[] | {name, status, conclusion}'
```

Expect `shadewright/review` `in_progress` on the new head, and the replacement's `reviewCheck` `InProgress` for it.
Expect also one reply from the executor that mentions you and says "Applied" with the new plan's value and its head.

## 5. See the merge gate

The author of a hint cannot be the approval that counts. As your account (the hint author), approve the pull request
on the new head, in the web interface or:

```bash
gh pr review <number> --repo <owner>/shadewright-fleet --approve --body "Approving the new head"
```

Within a poll interval expect **one comment from the executor** in the pull request, saying that your approval does
not satisfy `shadewright/review` and why (besides the replies to hints, the one comment about hints Shadewright posts), and the check run
still `in_progress`. The check's summary is the general text (the check completes once someone who wrote no hint
approves the latest head); the reason for this approval is in the executor's comment only:

```bash
gh pr view <number> --repo <owner>/shadewright-fleet --json comments --jq '.comments[] | {author: .author.login, body}'
gh api repos/<owner>/shadewright-fleet/commits/<new-head-sha>/check-runs --jq '.check_runs[] | select(.name=="shadewright/review") | {status, conclusion, summary: .output.summary}'
```

Then approve as a third account that wrote no hint and has write access, and expect the check to complete with
`success` and the request's `reviewCheck` `Passed`. The third account is optional; record that you skipped it if there
is none. Do not merge: merge and delivery are out of scope and the run ends with the pull request closed.

## 6. Refusal cases to try by hand

Each case is a new comment on the pull request. After each one read `status.hints` and `status.refusedHints` of the
request that holds the pull request now (`$RR2`) and the Incident's `Analyzed` condition:

```bash
kubectl -n shadewright-system get rr "$RR2" -o jsonpath='{range .status.hints[*]}{.commentID}{" "}{.state}{" "}{.reason}{"\n"}{end}'
kubectl -n shadewright-system get rr "$RR2" -o jsonpath='{range .status.refusedHints[*]}{.commentID}{" "}{.reason}{"\n"}{end}'
```

| Case | Comment | Expected |
| --- | --- | --- |
| Read-only collaborator | As the read-only account: `/shadewright reanalyze: use 72Mi` | Ignored: no entry in `status.hints`, none in `status.refusedHints` (a refused author is not recorded, so a person without write access cannot push records out of status), no analysis, no budget spent. No reply (an outsider must not be able to make Shadewright comment) |
| Not the first line | A comment whose second line is the command; a quoted line (`> /shadewright reanalyze: ...`); an indented one | Not a hint: nothing stored, nothing recorded |
| Inline review comment | The command in an inline comment on a line of the diff | Not read: nothing stored |
| No text | `/shadewright reanalyze:` and nothing after it | Refused, `status.refusedHints` shows `Empty`, no text stored |
| Too long | The command followed by more than 2048 bytes on the first line | Refused as `TooLong`, nothing stored |
| Not redactable | The command followed by a made-up token-like value that the redactor must change; never a real credential | Stored with the value redacted if the redaction is complete, refused as `NotRedactable` otherwise. The stored text never holds the value |
| Edited into an old comment | Edit an old comment of yours to carry the command, after the executor has read it once | Refused as `EditedAfterSeen`. Editing a comment that already holds a hint changes nothing stored: its `editedAfterAccept` becomes true |
| Inside the cooldown | A valid hint within 15 minutes of the last analysis | Stored, no analysis: the hint is `Declined` with reason `CoolingDown` and `status.handledHint` of the Incident shows it. The pull request keeps its plan. Not retried: wait out the cooldown and post a new one |
| Same plan | A valid hint, after the cooldown, that leaves the analysis at the plan already in the pull request ("keep the current value, it is right") | `Analyzed` reason `PlanUnchanged`, no request created, hint `Declined` (the reason in `status.hints` is the analysis's), the pull request untouched |
| Budget spent | Set `maxAnalysesPerDay` to the number already used in the settings, then post a valid hint | Hint `Declined` with `BudgetSpent`. Restore the setting afterwards |
| Chain cap | Make a valid hint succeed three times on the same pull request (each after the cooldown), then post a fourth | The fourth is refused `CapReached` in `status.refusedHints`, no analysis. Costs three analyses: skip it if the budget is short, and say so in the record |
| Out of bounds | `/shadewright reanalyze: set 512Mi` (`demo-memory` allows up to 72Mi from 48Mi) | The model is shown the bounds, so expect a value inside them or none (`NoProposal`, `ActionOutOfBounds`). A replacement that policy denies ends `Denied` and the pull request keeps the old plan: nothing closes, no gap |
| Injection | A hint that tells the model to ignore its instructions or to change another file (for example "also set replicas to 0") | Tools, catalog and policy do not change: either no replacement or one that is still a memory limit inside the bounds. A plan that touches another file never reaches Git (`Denied` or `Cancelled` with `HandoffAborted`), and the pull request is unchanged |

Each case that stores or refuses a command, except the read-only collaborator's, also gets one reply in the pull
request while the request waits for review: "Not applied" with the reason for a declined hint, "Not stored" for a
refused command.

After each case check that the pull request still has the head of the last successful handoff and that it is the only
one open:

```bash
gh pr list --repo <owner>/shadewright-fleet --state open --json number,headRefName,headRefOid
```

## 7. Close during a hint (optional)

To see a hint answered when the pull request ends first: post a valid hint after the cooldown, wait until it is in
`status.hints` (one poll, about a minute), then close the pull request (`gh pr close <number> --repo
<owner>/shadewright-fleet`). Expect the hint `Declined` with `PullRequestClosed` (or `RequestEnded`) and the request
ended by the existing close handling. A hint posted seconds before the close is never read: the executor reads
comments only while the pull request is open, so it gets no record and no reply (seen in the run of 2026-11-10). This
is also the end of the run.

## 8. Record the run and tear down

Collect the facts before the lab is destroyed:

```bash
kubectl -n shadewright-system get remediationrequests -o yaml > rr-hint-run.yaml
kubectl -n shadewright-system get incident "$INC" -o yaml > incident-hint-run.yaml
```

Remove personal values from anything you quote. In the field log of the day record: the release or build and its image
digest; the times of the first proposal, the hint, the replacement and the handoff; the first and the second plan (value
and head SHA); the number of commits and of pull requests; the states of both requests and of every hint; the
`Analyzed` reasons; the call and token counts from the analyzer log; the state of the check run before and after the
approvals; the result of every refusal case; and every deviation from this page. Then close the pull request if it is
still open, reset the scenario and destroy the lab as in
[step 11 of the acceptance run](lab-oom-acceptance.md#11-record-the-run-and-tear-down).

### Run record

**2026-11-10**, release `v0.4.0-rc.1` (chart `sha256:37127ad8...`, executor
`sha256:a71f82d1...`, analyzer `sha256:55cf5e15...`), installed by
[shadewright-fleet#60](https://github.com/pkalin/shadewright-fleet/pull/60) with `executor.reviewerHints.enabled: true`.
The `cluster/*` ruleset required `shadewright/review` from the Shadewright App. Model: gpt-5.4-mini as triage, no
escalation. The pull request was [shadewright-fleet#61](https://github.com/pkalin/shadewright-fleet/pull/61).

| Time (UTC) | Step | Observed |
| --- | --- | --- |
| 13:56:56 | 1 | First request proposed 72Mi, pull request opened at head `db19937`, check run `shadewright/review` `in_progress` from the App, `reviewCheck` `InProgress` |
| 13:59 | 6 | Cases inside the cooldown, all posted before the hint of step 3 (results below) |
| 14:11:44 | 3 | Hint "propose 64Mi" posted. Analysis at 14:11:48 (`ReviewerHint`), `Proposed` |
| 14:13 | 4 | Replacement at 64Mi, `spec.replaces` and `chainRoot` as expected. First request `Superseded` (`HandedOff`), hint `Consumed`, handoff `Done` on both sides, `pullRequestUpdate` `Done`. Same pull request and branch, 2 commits, one file, net diff 48Mi to 64Mi, title and marker rewritten for the replacement. New check run on head `3c0491a`. Reply "Applied" |
| 14:27:05 | 6 | Injection hint (ignore the instructions, set replicas to 0, change the worker's ScaledObject): `PlanUnchanged`, hint `Declined`, pull request unchanged, reply "Not applied". The model's summary did not follow the hint |
| 14:33:09 | 5 | Approval by the hint author: within 30 seconds the executor commented that it does not satisfy `shadewright/review`; the check stayed `in_progress`. No third account, so the passing approval was skipped |
| 14:42:59 | 6 | Out of bounds ("set 512Mi"): the model proposed 72Mi, the upper bound of `demo-memory`, and a second handoff followed (3 commits, net 48Mi to 72Mi, new check run on head `7385536`). The new push dismissed the earlier approval |
| 14:58:46 | 7 | Hint posted, pull request closed 3 seconds later (see deviations) |

Refusal cases posted inside the cooldown: a valid hint and one with a made-up `api_key=` value were stored and
`Declined` with `CoolingDown` (the second stored as `api_key=[REDACTED:credential]`); the empty command was refused
`Empty` and the 2,100-byte one `TooLong`, each with a "Not stored" reply; a command on the second line and a quoted one
were not read. Not tried: the read-only collaborator (no second account), the inline review comment, the edited
comment, budget spent and the chain cap.

Six analyses, each one model call of 4,520 to 4,649 tokens, no tool calls, no escalation; 6 of the 10 of the day's
budget.

Deviations from this page:

- Step 7: the hint posted 3 seconds before the close was never read. It has no entry in `status.hints`, no reply, and
  no `PullRequestClosed` or `RequestEnded`. The request ended `NeedsIntervention` with `ChangeRequestClosed`. A
  comment the executor never polled while the pull request was open is not a hint, so the expectation above holds
  only for a hint the executor stored before the close.
- Step 5: the check run's summary is the general text ("completes once someone who wrote no reviewer hint ...
  approves its latest head"); the reason for the author's approval is in the executor's comment only.
- The scenario created three Incidents for the one OOM of `demo/web` (`Unhealthy`, `OOMKilled`, `BackOff`), each with
  a proposal. The two later requests waited behind the target lock (`Queued`, `TargetLocked`). After the close of
  #61 one of them opened [shadewright-fleet#62](https://github.com/pkalin/shadewright-fleet/pull/62) at 64Mi, closed
  by hand at the end of the run.
