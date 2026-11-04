# Emergency stop

This page says how to stop Shadewright quickly, and what each step does not stop. Every setting below lives in the one cluster-wide `RemediationSettings` object, named `default` ([API reference](reference/api.md)). Run the commands with a kubeconfig that may patch it. A pause is set by kubectl or GitOps: with the Helm chart, `settings.create=true` only creates the object when it does not exist, and Helm never changes it after the install, so a `helm upgrade` does not undo a pause (a GitOps repository that manages the object does, so change it there too).

## What pausing does not do

Pausing stops the executor's own Git writes. It does not revoke a proposal that is already published, and it does not revert a merge:

- A pull request that is already open stays open. Anyone with write access, or a repository rule that merges by itself, can still merge it.
- A merged change stays merged, and the GitOps controller keeps deploying it. To undo it, revert it in Git by hand, through your normal review.
- Pausing does not stop analysis. Stop that with step 2.

## 1. Stop new Git writes: `spec.paused`

```bash
kubectl patch remediationsettings default --type merge -p '{"spec":{"paused":true}}'
```

The executor reads the settings right before each write and, while `paused` is true, does none of these:

- push a branch or open a pull request (issues are planned, [ADR-0014](adr/0014-notifications-github-issues-first.md), and are also covered by the pause);
- close a pull request, including the one a deleted request owns or one whose plan went stale;
- finish a deletion: the finalizer `remediation.shadewright.io/close-pull-request` stays on a deleted `RemediationRequest` until the pause ends, because closing is a write.

The check is made right before each write, from the API server, so a pause also stops a request whose step has already started: the next write is not made. A write that has already passed the check when you set the pause may still complete; every later write is stopped. If the executor cannot read the settings at that moment, or the object is gone, it does not write either.

It also does not look at its open pull requests while paused. Requests keep their state and check again every minute; each one held back shows the condition `WritesPaused` and an Event of the same reason:

```bash
kubectl get events -n <operator-namespace> --field-selector reason=WritesPaused
```

To resume:

```bash
kubectl patch remediationsettings default --type merge -p '{"spec":{"paused":false}}'
```

Each request goes on from where it stopped; the condition is removed and an Event `WritesResumed` is recorded.

## 2. Stop analysis: `spec.maxAnalysesPerDay: 0`

```bash
kubectl patch remediationsettings default --type merge -p '{"spec":{"maxAnalysesPerDay":0}}'
```

With `0` the analyzer starts no new analysis (an incident is not due, reason `AnalysisDisabled`), so no new analysis is started and no new proposal appears. Incidents are still recorded. The analysis loop that calls the model is not built yet, so today no model calls happen either way, but this setting already marks incidents not due. To resume, set the value back (the default is 100).

To stop both at once, put both fields in one patch.

## 3. Close an open pull request by hand

Close it on GitHub, without merging, as you would any pull request. The executor does not fight this: it never reopens a pull request. When it next looks (within about five minutes, since it polls; after the pause ends if paused), it sees the closed pull request and ends the `RemediationRequest` in phase `NeedsIntervention` with reason `ChangeRequestClosed`. Nothing is pushed or opened again for it.

Deleting the `RemediationRequest` also closes its pull request, but only while not paused, so do not use that to stop things during a pause. A pull request that does not carry the request's marker is never closed by the executor.

## 4. Revoke access

Pausing is reversible with one patch. Revoking is what to do when you do not trust the executor's credentials. Both steps are in [shadewright-azure](https://github.com/pkalin/shadewright-azure#readme); use the sections named here and do not copy IDs into Git.

- **GitHub App (first, effective at once):** uninstall the App from the fleet repository (GitHub, Settings, Applications, Installed GitHub Apps, Configure, Uninstall). Its installation tokens stop working, so the executor can no longer push, open or close anything there. See "GitHub App" in the shadewright-azure README, which installs it on that one repository alone.
- **Key Vault `sign` grant (second):** remove the role assignment that lets the executor's identity sign with the App key, by setting `github_app_key_imported = false` in the foundation's `terraform.tfvars` and applying (step 5 of "GitHub App" adds it). This only stops new tokens: the executor caches an installation token until shortly before it expires, up to an hour, so a token it already holds keeps working until then. Show the plan and check that only that one role assignment goes before you apply.

Revoking does not close pull requests or revert merges either.
