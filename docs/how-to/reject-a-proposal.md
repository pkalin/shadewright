---
title: Reject a proposal
---

# Reject a proposal

Rejecting is closing the pull request without merging it. Nothing in the cluster has changed at that point, because Shadewright writes only to Git. Background: [the life of a request](../request-lifecycle.md).

## Steps

1. Close the pull request on your Git host, without merging. Add a comment saying why if your team wants one.
2. Within about five minutes the request ends in phase `NeedsIntervention` with reason `ChangeRequestClosed`. It is final, and the executor never reopens a pull request.

    ```bash
    kubectl -n shadewright-system get remediationrequest <name> \
      -o jsonpath='{.status.phase}{"\n"}{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
    ```

3. If the change is wanted after all (with another value, or after a fix), apply a new request with a new name. The spec of a request is immutable, so you cannot edit it.

## Other ways to end it

| Way | Effect |
| --- | --- |
| Delete the `RemediationRequest` | The executor closes its open pull request first (finalizer `remediation.shadewright.io/close-pull-request`), and only if the pull request carries the request's marker. While writes are [paused](pause-and-resume.md) the deletion waits |
| Do nothing | An unmerged pull request stays open and the request waits. It does not expire once the pull request exists; the 30 minute limit is only for opening it. A plan that goes stale is closed by Shadewright (`Superseded`) |

The branch `shadewright/<request UID>` is left in place after a close; delete it by hand if you like. A merged pull request is not undone by any of this: revert it in Git through your normal review.

!!! note "Not yet run"
    The `kubectl` command is not yet run for this guide. The behaviour comes from the executor code and the [emergency stop](../emergency.md) page (close by hand, deletion and the finalizer); `ChangeRequestClosed` is listed in [troubleshooting](../troubleshooting.md#pull-request-step).
