# 7. Existing controllers

Rule: change the declaration in its real source, and leave execution to the controller that owns the object. Shadewright never competes for the same fields with KEDA, Flux or an HPA.

This section lists what we analyse in each controller and the fix path (7.1), then the rules for KEDA (7.2) and Flux (7.3).

## 7.1. What we analyse and how we fix

| Controller | What we analyse | Fix path |
| --- | --- | --- |
| KEDA | `ScaledObject` and `ScaledJob` conditions, trigger errors and fallback, the HPA's metrics, backlog and replica limits (`internal/modules/keda`, built; the HPA replica ceiling is consumed as an Incident about the `ScaledObject`, the `ScaledObject` and `ScaledJob` sources about the object itself, with `analyzer.modules.keda.enabled`, on by default); cluster and downstream capacity (planned) | PR changing a justified `ScaledObject` field or the Helm values behind it |
| Flux | Kustomization and HelmRelease readiness, revision, build and apply errors, GitRepository fetch and authentication errors, drift from the source | PR fixing the manifest, values or Kustomization |
| Crossplane (later) | `Ready` and `Synced`, provider messages, XR dependencies | PR changing the XR or its parameters |

## 7.2. KEDA

### Replicas and cooldown

- Never set Deployment replicas by hand when an autoscaler owns them.
- `cooldownPeriod` applies to scaling to zero. Scaling from one to many is the HPA's job.

### A broken trigger is not fixed by more replicas

A broken trigger authentication is not fixed by raising `maxReplicaCount` ([ScaledObject spec](https://keda.sh/docs/2.21/reference/scaledobject-spec/)), and neither is any other trigger error.

So the policy check refuses `AdjustKedaMaxReplicas` unless the `ScaledObject` is ready (`KedaReady`). It does so at the decision and before every write (sections 5.1 and 6). The analyzer applies the same rule earlier, to the ScaledObject it read before the model call: one that is not ready is not offered the action, and the analysis ends `ScalerNotReady` unless the result is invalid, the model call fails or the result proposes another action, which keep their own end ([ADR-0036](../adr/0036-the-model-sees-the-scaledobject-of-a-keda-incident.md), section 3.3).

### Versions

Match the adapter to the installed version.

## 7.3. Flux

### After the merge (partly built, [#19](https://github.com/pkalin/shadewright/issues/19))

Check that the `Kustomization` (or `HelmRelease`) reports the merge commit as its last applied revision and is `Ready`. Then check workload health. `Ready` alone does not prove the incident is gone.

Built: the executor records the merge commit in `status.pullRequest.mergeCommitSHA` when it sees the merge (also of its pull request found merged after a lost status write, #561; never of one merged with a foreign change, `MergedWithForeignChange`, whose landed change is not the planned one), and the Flux adapter of `DeliveryVerifier` (`internal/providers/delivery/flux`) reports whether a unit applied that commit, is `Ready` and has observed its current generation. A later commit of the target branch applied over it counts too, and a failing attempt of such a commit counts as a failure: the Git host answers whether the merge commit is in its history (`AncestryChecker`; on GitHub, a compare), and an unanswered check counts as not applied. A revision of another branch never counts; Flux's commit-only revisions (`sha1:<commit>`) are compared by commit. Whether a later commit still holds the change, or reverted it, is not the delivery check's concern: the verification checks after delivery ([#93](https://github.com/pkalin/shadewright/issues/93)) read the live value and catch it. Wired for the phases after the merge (#681, section 6.1): the executor reads the `Kustomization` named by the target's Flux labels in `WaitingForDelivery`, asks about the merge receipt's commit and target branch, and the ancestry of the receipt's repository, records the time it first sees the commit applied in `status.pullRequest.appliedObservedAt` (set once) and moves the request to `Verifying`, where the rollout deadline and the timeout are measured from that time. At the end of the delivery window it tells a failure of the merge commit itself (`DeliveryFailed`) from one of a later commit (`DeliveryBlocked`) by the attempted commit, and reads `Suspended`. A request merged before receipts existed keeps the rollout report of #532 in `Merged`. A `HelmRelease`'s revisions are chart versions, which name no commit, so the adapter never reports one as applied; the `Kustomization` that applies it does. The adapter, the source mapping's checks before a write and the Flux module's symptom rules read Flux status through one package, `internal/fluxstatus`.

### What Shadewright never does

Shadewright never suspends a Kustomization, disables pruning or changes Flux config to push a fix through.

## 7.4. Details

| Topic | Detail |
| --- | --- |
| KEDA module | `internal/modules/keda` is built; its HPA source is consumed (2.5.3), its `ScaledObject` and `ScaledJob` sources too, about the object itself (2.5.1) |
| Cluster and downstream capacity checks | Planned |
| Crossplane | Later |
| Delivery and health check after merge | The rollout report (`RolledOut`, #532) is built on the Flux delivery adapter, and so are the delivery phases and the executor's checks in `Verifying` (#681); and so is the check that the incident cleared, read from the verifier's report (#683) |
| Readiness rule for `AdjustKedaMaxReplicas` | `KedaReady`, checked at the decision and before every write |
