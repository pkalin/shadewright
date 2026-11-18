---
title: Pause and resume writes
---

# Pause and resume writes

The kill switch is `spec.paused` on the one `RemediationSettings` object, `default`. While it is true the executor sends no write to the Git host: no branch, no pull request, no close. Background: [design 5.4](../design/5-api.md#54-remediationsettings). The complete page, with what a pause does not undo and how to revoke access, is the [emergency stop](../emergency.md).

## Pause

```bash
kubectl patch remediationsettings default --type merge -p '{"spec":{"paused":true}}'
```

Requests keep their state and wait, each showing the condition `WritesPaused`:

```bash
kubectl get events -n shadewright-system --field-selector reason=WritesPaused
```

## Resume

```bash
kubectl patch remediationsettings default --type merge -p '{"spec":{"paused":false}}'
```

Each request goes on from where it stopped, the condition is removed and an Event `WritesResumed` is recorded.

What a pause ends and what it does not:

- Before the merge nothing ends during a pause: no request on its deadline or another wait, and no cleanup after `Expired` on its 24 hour cutoff. After a merge, the delivery check and the verification go on during a pause as before and can end a request (`Succeeded`, `Failed`, `Inconclusive`); only the delivery window is extended by the paused time.
- Time paused counts against a request's deadline and the grace of a handoff recovery, as before, so the first look after the pause ends what passed meanwhile (a request whose deadline passed ends `Expired`).
- A wait for a read of the Git host (for the merge commit or for what a merge brought: `MergeCheck`, `MergeContent` in `status.gitWaits`) whose end passed during the pause ends only after one more read, at the first look after the pause: an answer goes on as after any read (a merge commit reported records the merge with its receipt), a failed read (a rate limit counts as one), a read that cannot start because the repository left the settings, or answers about the pull request that disagree end the wait there. A look that meets the pause again keeps the wait and tries again within the poll interval, and so does a look that finds no `RemediationSettings` object at all. The cutoff of the cleanup after `Expired` ends the same way, on one more close after the pause. The same holds after an executor that was down or a leader change: no wait for a read ends without that read.
- A merge the executor recorded (phases `Merged`, `Succeeded`, `Inconclusive`, `Failed` after a merge) frees its target in the write that records it, during a pause too: that release reads nothing from the Git host. With `--verify-outcomes` on, a merge held for its verification frees it in the write that ends the verification.
- A merge that brought more than the plan (`MergedWithForeignChange`), whose content could not be read (`MergeContentUnreadable`) or could not be checked (`NeedsIntervention` with `MergeNotCheckable`) keeps its target during a pause: it is freed by the first look after the pause that finds no other open pull request of its branch, or, if its repository was removed from the settings, at the first look after the pause. A `RemediationSettings` object that is missing is not a removed repository: the target stays held until the settings exist again. No request can open a pull request while paused, so nothing waits for that target meanwhile.
- Any other request whose pull request can no longer merge frees its target within a few looks after the pause, each scheduled, never left waiting for an event.

## What to know

- A pause does not close an open pull request or revert a merge, and does not stop analysis (set `spec.analysis.maxAnalysesPerDay` to 0 for that; see the emergency page).
- A `helm upgrade` does not undo a pause, because the chart creates the settings object only once. A GitOps repository that manages the object does, so change it there too.
- A pause set while a write is in flight cannot recall that one request; no further write or retry goes out after the executor sees it.

!!! note "Not yet run"
    The commands are copied from the [emergency stop](../emergency.md) page and were not run again against a live cluster for this guide. The behaviour is covered by the kill switch tests in `internal/controller/killswitch_test.go`.
