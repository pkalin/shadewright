---
title: Turn reviewer hints on
---

# Turn reviewer hints on

Reviewer hints let a person steer a proposal from its pull request with a `/shadewright reanalyze:` comment ([give a reviewer hint](../use.md#give-a-reviewer-hint)). They are off by default. Turn them on per install with the executor flag `--reviewer-hints`, and only after the GitOps repository requires the check `shadewright/review` from the Shadewright App. Background: [ADR-0028](../adr/0028-reviewer-hints-through-the-pull-request.md), section 5 for the merge gate.

## Why the repository rule comes first

With hints on, the executor creates a check run `shadewright/review` on every head of the pull requests it opened. It passes only when the latest head has an approving review from someone who wrote no hint in the chain, so the person who steered a change cannot be its only approver. That check is the merge gate, and it blocks only what the repository requires:

- **With the rule** (the target branch requires `shadewright/review`, with the Shadewright App as the expected source), a pull request cannot merge until the check passes.
- **Without the rule, or without the source**, the check only informs. A hint author can approve and merge their own steered change, and with no source anyone with push access could post a commit status of that name. Do not turn hints on for that repository.

Shadewright cannot verify the rule: the App has no administration permission. Checking it is your part.

## 1. Require the check on the target branch

In the GitOps repository, edit the ruleset that targets the branch Shadewright opens pull requests against (`Settings` > `Rules` > `Rulesets`):

1. Under **Require status checks to pass**, add the check `shadewright/review`.
2. Set its source to the Shadewright GitHub App, not "any source".
3. Save the ruleset.

Through the API, the rule in the ruleset reads as follows, with the App's numeric ID as `integration_id`:

```json
{
  "type": "required_status_checks",
  "parameters": {
    "strict_required_status_checks_policy": false,
    "required_status_checks": [
      { "context": "shadewright/review", "integration_id": <app id> }
    ]
  }
}
```

### Pull requests people open to that branch

The rule applies to every pull request to the target branch, but only Shadewright's pull requests get the check `shadewright/review`: the executor creates it only on the pull requests it opened. A pull request a person opens to that branch waits for a check that never comes, and cannot merge. Choose one of these before you save the rule:

- **Maintainers on the ruleset's bypass list.** Add the role or team that merges people's changes as a bypass actor of the ruleset. They can merge their own pull requests past the missing check; GitHub records each bypass. Everyone else's pull requests to that branch stay blocked until a bypass actor merges them. Never add the Shadewright App to the bypass list.
- **A separate target branch for Shadewright.** The repository entry's `targetBranch` must be the branch Flux tracks ([configuration](../reference/configuration.md)), so make that a branch only Shadewright opens pull requests against, and require the check only there. People open their pull requests to another branch, which never waits for the check, and a maintainer carries that branch over to the tracked one with a push under the ruleset's bypass, as the scenario clusters do with `cluster/<cluster>` ([ADR-0022](../adr/0022-scenario-switch-and-clusters.md)). It costs one more step before a person's change is applied.

The lab fleet uses a maintainer bypass: its ruleset for `cluster/*` lists the repository admin role, the maintainer's, as its bypass actor, so the maintainer's own pull requests and resets of `cluster/lab` still go through.

The App also needs the `checks: write` permission. Every token request of the executor names it since #539, so an App without it already fails ([ADR-0006](../adr/0006-github-app-key-in-key-vault.md)). Nothing else changes: no new App permission and no Kubernetes RBAC.

## 2. Turn the flag on

With the Helm chart, set the value and upgrade (keep your values in a file, as in [upgrade with Helm](../upgrade-uninstall.md#upgrade-with-helm)):

```yaml
executor:
  reviewerHints:
    enabled: true
```

```bash
helm upgrade shadewright oci://ghcr.io/pkalin/charts/shadewright --version <version> \
  --namespace shadewright-system -f my-values.yaml --wait --timeout 5m
```

The chart renders `--reviewer-hints` on the executor only, and refuses the flag in `executor.extraArgs`. With the kustomize manifests, add `--reviewer-hints` to the args of the executor's container.

## 3. Check it

The executor logs the setting once at start:

```bash
kubectl -n shadewright-system logs deploy/shadewright-executor | grep 'Configured reviewer hints'
```

The line shows `enabled` true and, as a reminder, the required check `shadewright/review`. Within about one `--pull-request-poll-interval` (a minute by default), every waiting Shadewright pull request shows the check `shadewright/review`, in progress until a reviewer who wrote no hint approves its latest head. In the pull request's merge box the check is listed as required; if it is not, the rule of step 1 is missing.

## Turn it off

Set `executor.reviewerHints.enabled` back to `false` and upgrade, then remove the rule of step 1. Once hints are off the executor no longer completes the check, so while the rule stays, Shadewright's pull requests cannot merge.

!!! note "Not yet run"
    These steps were not run against a live cluster for this documentation. The flag, the chart value and the log line are covered by unit and chart tests; the lab run is #541.
