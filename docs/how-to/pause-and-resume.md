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

- Nothing ends during a pause: no request on its deadline or another wait, and no cleanup after `Expired` on its 24 hour cutoff.
- Time paused counts against a request's deadline and the grace of a handoff recovery, as before, so the first look after the pause ends what passed meanwhile (a request whose deadline passed ends `Expired`).
- Time paused does not count against a wait for a read of the Git host (for the merge commit or for what a merge brought: `MergeCheck`, `MergeContent` in `status.gitWaits`) or against the cutoff of the cleanup after `Expired`. The paused time a look at the request saw (`status.pauseSeen`) stretches each of them, so none ends without its full bound of time not paused, however often the pause comes and goes. A pause that begins and ends between two looks at a request stretches nothing for it.
- A merge the executor recorded (phases `Merged`, `Succeeded`, `Inconclusive`, and `Failed` after a merge) frees its target in the write that records it, during a pause too: that release reads nothing from the Git host.
- Any other request whose pull request can no longer merge frees its target within a few looks after the pause, each scheduled, never left waiting for an event.

## What to know

- A pause does not close an open pull request or revert a merge, and does not stop analysis (set `spec.analysis.maxAnalysesPerDay` to 0 for that; see the emergency page).
- A `helm upgrade` does not undo a pause, because the chart creates the settings object only once. A GitOps repository that manages the object does, so change it there too.
- A pause set while a write is in flight cannot recall that one request; no further write or retry goes out after the executor sees it.

!!! note "Not yet run"
    The commands are copied from the [emergency stop](../emergency.md) page and were not run again against a live cluster for this guide. The behaviour is covered by the kill switch tests in `internal/controller/killswitch_test.go`.
