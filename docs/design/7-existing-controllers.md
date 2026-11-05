# 7. Existing controllers

Rule: change the declaration in its real source, and leave execution to the controller that owns the object. Shadewright never competes for the same fields with KEDA, Flux or an HPA.

This section lists what we analyse in each controller and the fix path (7.1), then the rules for KEDA (7.2) and Flux (7.3).

## 7.1. What we analyse and how we fix

| Controller | What we analyse | Fix path |
| --- | --- | --- |
| KEDA | `ScaledObject` and `ScaledJob` conditions, trigger errors and fallback, the HPA's metrics, backlog and replica limits (`internal/modules/keda`, built; symptom sources not consumed yet); cluster and downstream capacity (planned) | PR changing a justified `ScaledObject` field or the Helm values behind it |
| Flux | Kustomization and HelmRelease readiness, revision, build and apply errors, GitRepository fetch and authentication errors, drift from the source | PR fixing the manifest, values or Kustomization |
| Crossplane (later) | `Ready` and `Synced`, provider messages, XR dependencies | PR changing the XR or its parameters |

## 7.2. KEDA

### Replicas and cooldown

- Never set Deployment replicas by hand when an autoscaler owns them.
- `cooldownPeriod` applies to scaling to zero. Scaling from one to many is the HPA's job.

### A broken trigger is not fixed by more replicas

A broken trigger authentication is not fixed by raising `maxReplicaCount` ([ScaledObject spec](https://keda.sh/docs/2.21/reference/scaledobject-spec/)), and neither is any other trigger error.

So the policy check refuses `AdjustKedaMaxReplicas` unless the `ScaledObject` is ready (`KedaReady`). It does so at the decision and before every write (sections 5.1 and 6).

### Versions

Match the adapter to the installed version.

## 7.3. Flux

### After the merge (planned, [#19](https://github.com/pkalin/shadewright/issues/19))

Check that the `Kustomization` (or `HelmRelease`) reports the merge commit as its last applied revision and is `Ready`. Then check workload health. `Ready` alone does not prove the incident is gone.

### What Shadewright never does

Shadewright never suspends a Kustomization, disables pruning or changes Flux config to push a fix through.

## 7.4. Details

| Topic | Detail |
| --- | --- |
| KEDA module | `internal/modules/keda` is built; its symptom sources are not consumed yet |
| Cluster and downstream capacity checks | Planned |
| Crossplane | Later |
| Delivery and health check after merge | Planned |
| Readiness rule for `AdjustKedaMaxReplicas` | `KedaReady`, checked at the decision and before every write |
