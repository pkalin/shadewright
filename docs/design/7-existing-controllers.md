# 7. Existing controllers

Rule: change the declaration in its real source, and leave execution to the controller that owns the object. Shadewright never competes for the same fields with KEDA, Flux or an HPA.

| Controller | What we analyse | Fix path |
| --- | --- | --- |
| KEDA | `ScaledObject` and `ScaledJob` conditions, trigger errors and fallback, the HPA's metrics, backlog and replica limits (`internal/modules/keda`, built; symptom sources not consumed yet); cluster and downstream capacity (planned) | PR changing a justified `ScaledObject` field or the Helm values behind it |
| Flux | Kustomization and HelmRelease readiness, revision, build and apply errors, GitRepository fetch and authentication errors, drift from the source | PR fixing the manifest, values or Kustomization |
| Crossplane (later) | `Ready` and `Synced`, provider messages, XR dependencies | PR changing the XR or its parameters |

**KEDA:** never set Deployment replicas by hand when an autoscaler owns them. `cooldownPeriod` applies to scaling to zero; scaling from one to many is the HPA's job. A broken trigger authentication is not fixed by raising `maxReplicaCount` ([ScaledObject spec](https://keda.sh/docs/2.21/reference/scaledobject-spec/)), and neither is any other trigger error: the policy check refuses `AdjustKedaMaxReplicas` unless the `ScaledObject` is ready (`KedaReady`), at the decision and before every write (sections 5.1 and 6). Match the adapter to the installed version.

**Flux:** after merge, check that the `Kustomization` (or `HelmRelease`) reports the merge commit as its last applied revision and is `Ready`, then check workload health. `Ready` alone does not prove the incident is gone. Shadewright never suspends a Kustomization, disables pruning or changes Flux config to push a fix through.
