---
title: Pause and resume writes
---

# Pause and resume writes

The kill switch is `spec.paused` on the one `RemediationSettings` object, `default`. While it is true the executor sends no write to the Git host: no branch, no pull request, no close. Background: [design 5.4](../design.md#54-remediationsettings). The complete page, with what a pause does not undo and how to revoke access, is the [emergency stop](../emergency.md).

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

## What to know

- A pause does not close an open pull request or revert a merge, and does not stop analysis (set `spec.analysis.maxAnalysesPerDay` to 0 for that; see the emergency page).
- A `helm upgrade` does not undo a pause, because the chart creates the settings object only once. A GitOps repository that manages the object does, so change it there too.
- A pause set while a write is in flight cannot recall that one request; no further write or retry goes out after the executor sees it.

!!! note "Not yet run"
    The commands are copied from the [emergency stop](../emergency.md) page and were not run again against a live cluster for this guide. The behaviour is covered by the kill switch tests in `internal/controller/killswitch_test.go`.
