---
title: Reject a proposal
---

# Reject a proposal

Rejecting is closing the pull request without merging it. Nothing in the cluster has changed at that point, because Shadewright writes only to Git. Background: [the life of a request](../request-lifecycle.md).

## Steps

1. Close the pull request on your Git host, without merging. Add a comment saying why if your team wants one.
2. Within about a minute (the executor's `--pull-request-poll-interval`) the request ends in phase `NeedsIntervention` with reason `ChangeRequestClosed`. It is final, and the executor never reopens a pull request.

    ```bash
    kubectl -n shadewright-system get remediationrequest <name> \
      -o jsonpath='{.status.phase}{"\n"}{range .status.conditions[*]}{.type}={.status} {.reason}{"\n"}{end}'
    ```

3. Know that a rejection counts against the circuit breaker: the request ends `NeedsIntervention`, which counts as a failure. Three failures in 30 minutes hold back every new request (`CircuitOpen`, see [troubleshooting](../troubleshooting.md#limits)). Deleting the rejected requests closes the breaker at once.
4. If the change is wanted after all (with another value, or after a fix), apply a new request with a new name. The spec of a request is immutable, so you cannot edit it.

## Other ways to end it

| Way | Effect |
| --- | --- |
| Delete the `RemediationRequest` | The executor closes its open pull request first (finalizer `remediation.shadewright.io/close-pull-request`), and any other open pull request of its branch that carries the request's marker; one without the marker is left alone. This holds also for a stopped request that still holds the lock on its target. While writes are [paused](pause-and-resume.md) the deletion waits |
| Leave the pull request open | An unmerged pull request stays open and the request waits. A stopped request that holds the lock on its target keeps other requests for that target waiting until its pull request is known closed or merged (looked up every 10 minutes). A waiting request does not expire once the pull request exists; the 30 minute limit is only for opening it. A plan that goes stale is closed by Shadewright (`Superseded`) |

The branch `shadewright/<request UID>` is left in place after a close; delete it by hand if you like. A merged pull request is not undone by any of this: revert it in Git through your normal review.

!!! note "Not yet run"
    The `kubectl` command is not yet run for this guide. The behaviour comes from the executor code and the [emergency stop](../emergency.md) page (close by hand, deletion and the finalizer); `ChangeRequestClosed` is listed in [troubleshooting](../troubleshooting.md#pull-request-step).
