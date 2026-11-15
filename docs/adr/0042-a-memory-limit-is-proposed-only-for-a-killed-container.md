# ADR-0042: A memory limit is proposed only for a container that was killed, and code sets the size

- Status: Proposed
- Date: 2026-11-15

This ADR supersedes [ADR-0040](0040-code-sizes-a-memory-proposal-from-the-metrics.md) and amends [ADR-0026](0026-the-analyzer-shows-the-model-the-policy-bounds.md) (the model sees the policy bounds). It settles issues [#508](https://github.com/pkalin/shadewright/issues/508) (sizing in code) and [#506](https://github.com/pkalin/shadewright/issues/506) (evidence in the pull request). The decision is the fourth one of 2026-11-15 on #508, with the design calls and the fifth decision of that day. Everything here is <span class="sw-status sw-planned">planned</span>: nothing is built, and the text says what the code will do.

## Context

ADR-0040 let the model name an action and a container, and code compute the size. Three review passes found three different holes in its gate, which was assembled from several paths (the Incident's `OOMKilled` symptom, a peak of at least 90% of the limit, two evidence tiers, a fallback). The last one: an Event can carry the reason `OOMKilled`. The collector accepts any Event reason that fits its schema (`validReason` in `internal/analyzer/collector/owners.go`, `events.go`), and the Incident's `reason` is that string, so a symptom read from `spec.reason` can be forged by whoever can create Events in a watched namespace. The collector never marks such an observation `Immediate` for the same reason (`internal/analyzer/collector/collector.go`). This ADR replaces the whole gate with one, built on a source that cannot be forged that way. ADR-0040 also carried two links that move here: [ADR-0034](0034-declarative-pack-format.md) open question 2 (pack-named values, to be revisited when sizing by code exists, which this is for memory) and [#741](https://github.com/pkalin/shadewright/issues/741) (metrics as evidence for the outcome check of [ADR-0030](0030-merge-receipt-and-verification-outcomes.md)).

What the code has today:

- **The collector reads container status.** `terminations` (`internal/analyzer/collector/pods.go`) walks the init and regular container statuses of a pod and takes `lastState.terminated` and `state.terminated` with the reason `OOMKilled` or any non-zero exit code. Each becomes an observation with the occurrence ID `termination:` plus a hash of the pod UID, the container name and the termination's container ID (`occurrence`). Its `Began` is the termination's `finishedAt`, the node's clock, clamped to the collector's own now (`notFuture`). `pending` marks a termination `MayRepeat` when it finished before the collector's first reconcile (`start`) or no later than `repeatThrough`; `deliver` in `collector.go` clears that mark through `Handled` (wired in `cmd/analyzer/main.go`), and `consume` in `internal/incidents/controller.go` clears it again, so downstream it says nothing about whether the kill was seen live.
- **The Incident cannot say which container.** `spec.symptom` is a fixed text (`Container <name> terminated with exit code <n>`), redacted and normalized, not a field. `status.occurrences` keeps only `id`, `total` and `began` (`api/v1alpha1/incident_types.go`). `firstSeen` comes from `notFuture(ObservedAt)` in the Incident controller, and for a termination `ObservedAt` is the node's `finishedAt` (clamped): it is the time of the kill on the node's clock, not an arrival time.
- **The context builder** (`internal/incidentcontext`) reads the pods of the subject and keeps the pod template's containers (`Context.Template`, `Context.Pods`), but no list of killed containers and no typed policy bounds. The loop (`finish` in `internal/analysis/loop.go`) takes `previous` from the template (`previousFromTemplate`) and never checks the size.
- **The policy** (`MemoryLimitBounds`, `api/v1alpha1/remediationpolicy_types.go`) has `min`, `max`, `maxIncreasePercent` (default 50) and `maxDecreasePercent`.

## Decision

[![One gate and one size rule: the collector marks a termination read from pod status as seen live when it finished after the collector started; the Incident controller records it with its own clock only for such a kill; the context builder matches the current container status to those records, keeps only OOMKilled, and names the killed containers with their pod and its limit; code accepts a SetContainerMemoryLimit only for one of them whose pod limit equals the template limit, sizes it as the limit times one plus the headroom, rounded up and limited by the policy range (Step, Max or Min), and the pull request shows typed evidence including the killed pod; an Event never opens the gate, and a measured peak under 90% of the limit can close it but never open it; the model chooses the action and the container only](../assets/diagrams/memory-limit-one-gate.svg)](../assets/diagrams/memory-limit-one-gate.svg)

### 1. The gate

A `SetContainerMemoryLimit` for a container is accepted only when a pod of the subject shows, for that container, an `OOMKilled` termination that the collector saw happen live and an Incident counted inside the window, in a pod whose memory limit for that container is the template's current one. Otherwise the Incident is handed off with the reason `NoMemoryPressure`. There is no peak path, no tier and no fallback, and nothing waits for [ADR-0039](0039-consequence-symptoms-attach-to-their-causes-incident.md). A measurement can close the gate, never open it (below).

- **Source.** Only container status. An Event with the reason `OOMKilled` never counts, and neither does `spec.reason` or the symptom text of the Incident, because their origin is not known (threat model G4.8, the `Immediate` rule). Only a `termination:` occurrence counts.
- **Only `OOMKilled`.** `terminations` also returns other non-zero exits, and the fingerprint includes the reason (`internal/incidents/fingerprint/fingerprint.go`). The gate keeps only terminations whose reason is the `ReasonOOMKilled` constant. Test: a crash loop that exits with code 1 gets `NoMemoryPressure`.
- **Seen live.** A separate mark, `SeenLive`, on the collector's `Observation`. The collector sets it in `pending` (`internal/analyzer/collector/pods.go`) for a termination that finished after `start` and after `repeatThrough`, and `deliver` and `consume` never rewrite it (they clear `MayRepeat`, which cannot be the mark). Only a `SeenLive` observation gets an `observedAt`. A kill found in `lastState` after a restart, or one no Incident counted (the analyzer was down, the Incident is gone), arrives with `Handled` answering not found and counts as new for the Incident's total, but has no `observedAt` and never opens the gate. Test: a restart with `Handled` returning not found leaves the record without `observedAt`. The cost: a kill that happened while the collector was down gives `NoMemoryPressure` until the next kill.
- **`observedAt`.** The Incident's `status.occurrences[].observedAt` is the Incident controller's own clock when it first counts a `SeenLive` occurrence, set once and never changed. A record dropped from the 32 newest (`MaxOccurrenceRecords`) counts as no kill.
- **Which container.** The Incident cannot carry it, so the builder reads it again from the pod. For each pod of the subject it already attributes (`Context.Pods`, which for a Deployment holds the pods of every ReplicaSet, `resolveDeployment` in `internal/incidentcontext/resolve.go`), it runs the same `terminations` and `occurrence` code (moved to a package both can import) and keeps those whose occurrence ID is in `status.occurrences` with an `observedAt` inside the window. It passes them as a typed list (`Context.Killed`: container name, pod name and the pod's memory limit for that container, all read from the pod, never from the Incident). In `finish`, a proposal whose container has no entry is `NoMemoryPressure`. A killed sidecar cannot step the main container.
- **At the current limit.** The entry's pod limit is the field `spec.containers[].resources.limits.memory` of the killed pod. It must equal the template limit the step starts from (`previousFromTemplate` in `internal/analysis/loop.go`), compared by quantity value (`64Mi` and `67108864` are equal), and an absent limit on either side is never equal. When `status.containerStatuses[].resources` of the pod shows other resources in effect (an in-place resize, `pods/resize`), there is no proposal: the spec no longer says what the container ran with. A kill of an old pod during a rollout, at a limit the template has already left, is `NoMemoryPressure`. Worked: a merged change took the template from 48Mi to 64Mi; an old 48Mi pod is killed mid-rollout; the entry says 48Mi, the template 64Mi: no proposal. A kill of a 64Mi pod after the rollout does pass.
- **The window.** It is `windowOf` (`internal/incidentcontext/builder.go`) with one change for the gate: its start comes from the earliest `observedAt` among the Incident's records (in place of `firstSeen`), ten minutes before it, at least 15 minutes and at most six hours before the builder's `now`. The start, the end and every `observedAt` are readings of the analyzer's own clock (the Incident controller and the builder run in the one analyzer process), and the node's `finishedAt` is never compared with them.
- **A measurement can close the gate, never open it.** When the builder has a measurement for the killed pod's container and its peak working set over the window is under 90% of that pod's limit, the hand-off is `NoMemoryPressure`: the kernel also kills containers far below their limit when a node runs out of memory, and the kubelet reports them as `OOMKilled`. The measurement is the working set and the limit of the same pod and container (the limit from the pod, as above). A partial, capped or stale answer, a failed read or no Prometheus is no measurement, and then the kill alone counts. A node-wide out-of-memory kill is a known false positive in that case: the cgroup v2 `memory.events` counter `oom_kill` counts kills by any OOM killer, so a container the node killed looks like one that hit its own limit. It costs one step that a person approves, bounded by the policy.
- **Failed or capped reads.** The builder does not retry a failed pod read today: `internal/incidentcontext/builder.go` and `types.go` only mark the section in `FailedSources`, and `resolve.go` leaves `Pods` and `Template` empty. The gate hands off with `MissingData`, never `NoMemoryPressure`, when the status section failed or the pod set is capped (`MaxPods`, `Truncated`). This is new code.

### 2. The size

`need = ceil(limit x (1 + headroomPercent / 100))`, where `limit` is the pod template's limit (`previousFromTemplate`, #494). In whole bytes: `(limit x (100 + h) + 99) / 100`, which fits an `int64` (limit at most 1Ti, h at most 100). Then:

1. Round `need` up to a multiple of the unit, 8Mi (a starting value, tuned on lab runs).
2. Take the range `policy.AllowedMemory` returns for an increase from `limit` (`internal/policy/bounds.go`): `lo = max(min, limit + 1)` and `hi = min(max, limit + floor(limit x maxIncreasePercent / 100))`, the arithmetic of `maxDelta`. One function, so the analyzer and the executor's decision cannot disagree.
3. No range (`lo` above `hi`, which includes a limit at or above `max`): hand-off `SizeNotAnIncrease`.
4. A value above `hi` becomes `hi` itself, not rounded again: `limitedBy: Max` when `max` is the lower bound (or equal), `Step` when the step limit is. A value below `lo` becomes `lo`: `limitedBy: Min` when `lo` is `min`. Below `min` the policy would deny the request, so raising to `min` is decided.

The measured peak sets no size, and it can only close the gate. Decreases and `AdjustKedaMaxReplicas` ([#497](https://github.com/pkalin/shadewright/issues/497)) are out of scope.

Worked, `headroomPercent` 30, step 50, 1Mi = 1048576 bytes:

1. Limit 48Mi, `max` 1Gi. `48Mi x 1.3` = 62.4Mi, rounded up to 64Mi. `hi = min(1Gi, 48Mi + 24Mi)` = 72Mi. Result 64Mi, `None`.
2. Limit 100Mi, `max` 1Gi. 130Mi, rounded up to 136Mi. `hi` = 150Mi. Result 136Mi, `None`.
3. Limit 10Mi, `max` 1Gi. 13Mi, rounded up to 16Mi. `hi = 10Mi + floor(10Mi x 50 / 100)` = 15Mi. 16Mi is above `hi`: 15Mi (not a multiple of 8Mi), `Step`.
4. Policy `demo-memory` (`min` 64Mi, `max` 128Mi, step 50), five separate steps from different starting limits, each its own Incident. From 48Mi: 62.4Mi up to 64Mi, `lo` 64Mi, `hi = min(128, 72)`: 64Mi. From 64Mi: 83.2Mi up to 88Mi, `hi` 96Mi: 88Mi. From 88Mi: 114.4Mi up to 120Mi, `hi = min(128, 132)` = 128Mi: 120Mi. From 120Mi: 156Mi up to 160Mi, `hi = min(128, 180)` = 128Mi: 128Mi, `Max`. From 128Mi: `lo = 128Mi + 1` is above `hi` = 128Mi: `SizeNotAnIncrease`.
5. `min` above the result: `min` 75Mi, `max` 128Mi, step 50, limit 52Mi. 67.6Mi, rounded up to 72Mi. `lo` 75Mi, `hi = min(128, 52 + 26)` = 78Mi. 72Mi is below `lo`: 75Mi, `Min`.

### 3. The order in the analyzer

Today `finish` takes `previous` from the template and runs `policy.CheckAction` on an action that still holds the model's `desired` (`internal/analysis/loop.go`). For a memory limit the order becomes: the catalog type and the subject check, the template's limit for the container (`PreviousValueUnknown` when absent or out of the bounded form, as `previousFromTemplate` does today), the gate, the size by code, then `CheckAction` on the action code built. The model's size is never on that path. Two hand-offs of `previousFromTemplate` can no longer occur for memory, because they compared the model's value: `DirectionReversed` (it compared the model's claimed direction) and `NoChange` (a built value is above the limit or the hand-off is `SizeNotAnIncrease`). Both stay for `AdjustKedaMaxReplicas`. `PreviousValueUnknown` stays (no template limit, or one outside the bounded form), and `CheckAction` can still return `ActionOutOfBounds` for a built value the schema refuses.

### 4. The policy field

`headroomPercent` is an optional field of `MemoryLimitBounds`: default 30, bounds 10 to 100 in the schema. The analyzer also checks it in code, because the schema runs only at write: an object stored before the CRD gained the field has none (30 is used), and one that bypassed validation can hold any value. A value outside 10 to 100 is a hand-off with the new reason `HeadroomInvalid`. The executor ignores the field, so its decision does not change; the policy is where the analyzer already reads the bounds (the `analyzer-policy-reader` role, `get` only).

The policy read never defaults. The analyzer takes the bounds from `policy.BoundsFor` (`internal/policy/bounds.go`). No policy configured for the action (the `ReasonNoPolicyForAction` check in `internal/analysis/loop.go`) is `NoPolicyForAction`. A policy that is named but cannot be read (a failed read, a refusal by RBAC, an object that is absent) is `MissingData`, since no reason of its own exists for it. Invalid bounds keep the reason `BoundsFor` gives. Only a valid policy with no allowed increase is `SizeNotAnIncrease`.

### 5. Typed evidence

An optional `spec.sizing` on the request, set by the analyzer for a memory proposal and only with `SetContainerMemoryLimit` (CEL), from values the builder read and code computed, never from model text. The spec is immutable, so it cannot change after the policy check. Illustrative:

- `headroomPercent`: 10 to 100, the value used. `limitedBy`: `None`, `Step`, `Max` or `Min`.
- `killedPod`: the name, UID and creation time of the pod whose kill opened the gate, from the builder's list.
- `observedPeak`: whole bytes, 1 to 1Ti, with `window` (start and end) and `samples` (1 to 100000). The three are set together or absent. When no peak was measured (no Prometheus, a partial or capped answer, a failed read) they are absent, never zero.

The executor prints these in the pull request body: the old and new limit, the headroom, `limitedBy` in words, that the container was killed and in which pod (name, UID and creation time, from `killedPod`), so a reviewer can check it, and either the peak with its window or "no peak was measured". Without `sizing` the body says no sizing evidence was recorded. A peak is evidence for the reader and for the later outcome check of [ADR-0030](0030-merge-receipt-and-verification-outcomes.md); it can close the gate, never open it, and it sets no size.

### 6. Hand-off reasons

`NoMemoryPressure`, `SizeNotAnIncrease` and `HeadroomInvalid` join every closed list of reasons (each place verified to exist on `main`):

- the `Hint.reason` enum marker in `api/v1alpha1/hint_types.go`, then the CRD (`config/crd/bases`) and chart CRD (`dist/chart/templates/crd`), regenerated;
- `HintDeclineReasons` in the same file, the Go list that must equal the enum marker: `internal/controller/hints.go` declines a hint with an outcome only when this list holds it, and otherwise records `NoAnswer`, so a reason missing there would turn into the wrong answer;
- `declineText` in `internal/controller/hintreply.go`, in the group "the new analysis made no proposal";
- `Reason` in `internal/analysis/loop.go` and `outcomeMessages` in `internal/analysis/controller.go`;
- the reason tables of `docs/troubleshooting.md` and `docs/reference/api.md`;
- `cmd/kubectl-shadewright/explain.go` (the section of each reason), and `refusalReasons` and `resultReasons` in `test/eval/harness_test.go`.

## Consequences

- The size is one fixed step per Incident, not a size fitted to the need, and nothing here says that steps converge on the need. After the step a kill ends the request `Failed` with reason `OOMKilled` (ADR-0030 section 4), the Incident becomes `Escalated` and is never analyzed again ([design 3](../design/3-observation-and-deduplication.md)), and a person decides. The message says that the step was not enough. No revert is proposed for that case, because a revert would put back the limit the container was already killed at; ADR-0030 gets a dated amendment, since this removes a proposal (the proposer of #96 is built today, so this is a code change too). A default step of +30% replaces ADR-0040's +50%, and lab runs decide whether the default headroom should be larger.
- A container under memory pressure that is never killed gets no proposal. A workload that runs near its limit by design is not stepped.
- The analyzer needs no metrics source to propose. With one, the peak is shown, and a peak under 90% of the limit closes the gate.
- A change to the Incident status (`observedAt`), the collector's observation (`SeenLive`), the policy (`headroomPercent`), the request (`spec.sizing`) and three reasons: the usual API review, a chart CRD update, and an evaluation run (ADR-0025) because the model's input and output schema change (no size argument, no bounds block for memory). The size is asserted by unit tests of the rule with the exact numbers above, not by the evaluation cases.
- The prompt rule about the smallest change and the policy bounds block for memory limits go (ADR-0026 gets a dated amendment). The analyzer keeps its `get` on the named policies, now for the headroom and the range. The wording of invariant 6 in `AGENTS.md` (why the analyzer may `get` named policies) is updated in the pull request that builds this.

### Invariants

1. **Model output is never authorization.** The model picks the action and the container, and code accepts only a killed one. The executor's policy decision runs as before and does not read `spec.sizing`.
2. **Closed catalog.** No new action or applied parameter.
3. **Git only.** The change is a pull request.
4. **Cluster input is untrusted.** The gate reads the kubelet's container status, not Event or symptom text. The evidence has typed, bounded fields.
5. **Idempotent reconciliation.** Open: whether the plan digest covers `sizing`.
6. **Identities.** No new RBAC verb: the analyzer already has `get`, `list` and `watch` on pods (`config/rbac/analyzer/watch/role.yaml`), `get` on `remediationpolicies` (`config/rbac/analyzer/policies/role.yaml`) and writes Incident status. ADR-0040's grant on `pods.metrics.k8s.io` is not made.

### Threats

- **A forged Event.** It cannot open the gate: its occurrence is `event:` and the gate reads `termination:` records matched with pod status.
- **Pod status written by hand.** Any identity granted `update` or `patch` on `pods/status` in a watched namespace can write a termination; the collector trusts the same field (G4.8). Writers of `pods/resize` can change the limit the pod runs with: the gate then reads the container status and gives no proposal when it shows other limits in effect. The result is a step bounded by the policy and approved by a person.
- **A pod made to pass as the workload's (G4.9).** Someone who can create pods in a watched namespace copies the workload's controller reference and labels and has their own container killed. Accepted residual: the policy and the approval bound the step, and the pull request body names the killed pod with its UID and creation time, so a reviewer can check the evidence. A name alone can be imitated, since the attacker names their pod as they like.
- **A killed sidecar used to step another container.** Only the killed container's name passes.
- **Injected text.** It can choose the action and the container, only among killed containers, and not the size.
- **A lying Prometheus.** It changes the evidence and can close the gate by reporting a low peak (a hand-off, a person can still act); it cannot open the gate and does not set the size.
- **Kills stepping the limit up.** A tenant that makes its own container run out of memory gets one step per Incident; a kill after the step ends the request `Failed` and the Incident `Escalated`, so there is no second step in it. The policy `max` and `maxIncreasePercent` bound each step and a person approves each pull request.
- **A node-wide out-of-memory kill.** It is reported as `OOMKilled` and can open the gate when no measurement exists. Accepted, see the gate; the policy and the approval bound it.
- **A kill replayed after a collector restart.** The `SeenLive` mark is set by the collector alone and never rewritten, so a kill found in `lastState` at start, or one no Incident counted, has no `observedAt` and does not count, whatever `Handled` answers. The test for `finishedAt` against `start` compares the node's clock with the collector's, so a node clock far ahead could make a replayed kill look live; the step stays bounded by the policy and the approval.
- **An old pod killed during a rollout.** Its limit is not the template's, so it does not count.

## Alternatives considered

- **Size from the peak with a fixed 1.5 step.** At the limit the peak cannot exceed the limit, so with a factor of 1.5 a headroom from 10 to 50 never set a size and the policy field decided nothing at its default. Rejected.
- **A peak of at least 90% of the limit as the gate.** A workload near its limit by design (a JVM sized to its limit, page cache in the working set) passes on every Incident, fills the new limit and passes again, round after round up to `max`. Rejected.
- **An `OOMKilled` reason on the Incident as the gate.** An Event can carry that reason, so it can be forged. Rejected.
- **`PolicyMaxBelowNeed`.** Nothing measures a need any more, so there is no need to compare with `max`. A step that does not fit is clamped to `max`. Rejected.
- **"Seen live" from informer update events.** The mark could be "the termination's container ID is absent from the old object and present in the new one", with no clock. The collector's pod handling cannot do it: `podReconciler.Reconcile` in `internal/analyzer/collector/pods.go` is a level-based reconcile that receives only the pod's name and reads the pod, and the predicate in `sources.go` sees both objects but only decides whether to enqueue. The rule with `finishedAt` against `start` stays. Rejected for that reason.
- **Evidence tiers and a metrics server adapter.** Two paths to one gate were the source of the holes. Rejected with the tiers.

## Open questions

1. Whether the plan digest covers `sizing` or stays on the action alone, which leaves the executor's baseline and freshness checks as they are.

## Amendments

None.
