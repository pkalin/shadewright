---
title: Write or tighten a policy
---

# Write or tighten a policy

A `RemediationPolicy` is what decides, with fixed rules, whether a request may run and within which bounds. Only the cluster admin should be able to create or edit it (it is cluster-scoped). Background: [design 5.3](../design.md#53-remediationpolicy) and [ADR-0011](../adr/0011-policy-as-cluster-scoped-crd.md). Every field is in the [API reference](../reference/api.md#remediationpolicy).

## A starting point

This is the policy of the lab, `config/samples/lab/remediation_v1alpha1_remediationpolicy.yaml`. It allows raising the memory limit of containers in the `demo` namespace to between 64Mi and 128Mi, in steps of at most 50 percent, never lowering it, in one file pattern of one repository, and always with a person's approval:

```yaml
apiVersion: remediation.shadewright.io/v1alpha1
kind: RemediationPolicy
metadata:
  name: demo-memory
spec:
  requestNamespaces: [shadewright-system]
  targets:
    namespaces: [demo]
  actions:
    - type: SetContainerMemoryLimit
      setContainerMemoryLimit:
        min: 64Mi
        max: 128Mi
        maxIncreasePercent: 50
        maxDecreasePercent: 0
  gitOps:
    repositoryRef: fleet
    allowedPaths:
      - apps/demo/*-deployment.yaml
  approval: Required
```

`repositoryRef` names an entry of `RemediationSettings.spec.repositories` (the model and the request never choose a URL).

## Tighten it

Prefer the narrowest value in each place:

| To restrict | Set |
| --- | --- |
| Where it may act | `targets.namespaces` (required: there is no "all namespaces"); add `targets.objectSelector` (a label selector) to match only labelled workloads |
| How far a value may go | `min` and `max` of the action, and `maxIncreasePercent` per request |
| Lowering values | Keep `maxDecreasePercent: 0`. Decreases are refused unless you allow them |
| Which files | `allowedPaths`, a list of globs with a literal first segment. `*` and `**` never match a segment that starts with a dot, so `.github/` cannot be covered |
| Who may use it | `requestNamespaces`, whose default is the literal `[shadewright-system]`: set it to your operator namespace if you installed elsewhere, or every request is `Denied`. A request from any other namespace is `Denied` |
| Review | `approval: Required` (the default). `Optional` only lets a request ask for no approval, and auto-merge is not built, so a person merges either way |

An edit counts as a change of the policy: every request that waits for review on the old revision becomes `Superseded` and its pull request is closed, so apply a new request after changing one (see [the life of a request](../request-lifecycle.md)).

## Check it

Apply the policy, then apply a request that is just outside a bound and confirm it is `Denied` with the matching reason (`AboveMaximum`, `IncreaseTooLarge`, `PathNotAllowed`, ...). The reasons are listed in [troubleshooting](../troubleshooting.md#policy-decision). Every action bound has such a refusal test in the repository.

```bash
kubectl apply -f policy.yaml
kubectl get remediationpolicy
```

!!! note "Not yet run"
    The two `kubectl` commands are not yet run for this guide. The YAML is the lab sample, which the repository's API validation tests apply on every run (`test/apivalidation`), and the lab runbook uses it.
