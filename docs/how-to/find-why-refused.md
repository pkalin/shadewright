---
title: Find why a request was refused
---

# Find why a request was refused

A request that is refused or stops says why in its conditions. Background: [design 6](../design/6-reconciliation.md#6-reconciliation) and [the life of a request](../request-lifecycle.md).

## Steps

With the [kubectl plugin](check-status.md#explain-one-request), `kubectl shadewright explain <name> -n shadewright-system` does steps 1 to 3 in one read-only command: the phase, the condition timeline, the condition that stopped the request and a link to its section of troubleshooting. Without it:

1. Print the phase and every condition with its reason and message:

    ```bash
    kubectl -n shadewright-system get remediationrequest <name> \
      -o jsonpath='{.status.phase}{"\n"}{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
    ```

2. Read the phase, and check every condition: `PolicyAllowed`, `PullRequestOpened`, `Approved`, `PlanFresh` and `Queued`:
    - `Denied`: policy or settings do not allow the plan. The reason is on `PolicyAllowed=False`.
    - `NeedsIntervention`: the executor cannot decide or continue alone. The reason is on `PolicyAllowed`, `PullRequestOpened` or `Approved`.
    - `Superseded`: the plan went stale; the reason is on `PlanFresh=False`.
    - `Pending` with a condition `Queued`: a limit holds it back.
    - `Pending` with no condition at all: it is being retried, usually a Git or signer problem. Read the executor log.
3. Look the reason up in [troubleshooting](../troubleshooting.md). It lists every reason with its cause and the fix. The reasons of a policy refusal are grouped under [policy decision](../troubleshooting.md#policy-decision).
4. If the reason is not enough, read the executor log:

    ```bash
    kubectl -n shadewright-system logs deploy/shadewright-executor
    ```

5. A refused request is final. Fix the cause and apply a new request with a new name, because the spec is immutable.

!!! note "Not yet run"
    The commands are the ones printed in [troubleshooting](../troubleshooting.md) and were not run again against a live cluster for this guide.
