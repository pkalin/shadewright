# ADR-0042: A memory limit is proposed only for a container that was killed, and code sets the size

- Status: Proposed
- Date: 2026-11-15

This ADR supersedes [ADR-0040](0040-code-sizes-a-memory-proposal-from-the-metrics.md) and amends [ADR-0026](0026-the-analyzer-shows-the-model-the-policy-bounds.md) (the model sees the policy bounds). It settles issues [#508](https://github.com/pkalin/shadewright/issues/508) (sizing in code) and [#506](https://github.com/pkalin/shadewright/issues/506) (evidence in the pull request). The decision is the fourth one of 2026-11-15 on #508. Everything here is <span class="sw-status sw-planned">planned</span>: nothing is built, and the text says what the code will do.

## Context

ADR-0040 let the model name an action and a container, and code compute the size. Three review passes found three different holes in its gate, which was assembled from several paths (the Incident's `OOMKilled` symptom, a peak of at least 90% of the limit, two evidence tiers, a fallback). The last one: an Event can carry the reason `OOMKilled`. The collector accepts any Event reason that fits its schema (`validReason` in `internal/analyzer/collector/owners.go`, `events.go`), and the Incident's `reason` is that string, so a symptom read from `spec.reason` can be forged by whoever can create Events in a watched namespace. The collector never marks such an observation `Immediate` for the same reason (`internal/analyzer/collector/collector.go`). This ADR replaces the whole gate with one, built on a source that cannot be forged that way.

What the code has today:

- **The collector reads container status.** `terminations` (`internal/analyzer/collector/pods.go`) walks the init and regular container statuses of a pod and takes `lastState.terminated` and `state.terminated` with the reason `OOMKilled` or a non-zero exit code. Each becomes an observation with the occurrence ID `termination:` plus a hash of the pod UID, the container name and the termination's container ID (`occurrence`). Its `Began` is the termination's `finishedAt`, the node's clock.
- **The Incident cannot say which container.** `spec.symptom` is a fixed text (`Container <name> terminated with exit code <n>`), redacted and normalized, not a field. `status.occurrences` keeps only `id`, `total` and `began` (`api/v1alpha1/incident_types.go`). `firstSeen` and `lastSeen` are arrival times.
- **The context builder** (`internal/incidentcontext`) reads the pods of the subject and keeps the pod template's containers (`Context.Template`, `Context.Pods`), but no list of killed containers and no typed policy bounds. The loop (`finish` in `internal/analysis/loop.go`) takes `previous` from the template (`previousFromTemplate`) and never checks the size.
- **The policy** (`MemoryLimitBounds`, `api/v1alpha1/remediationpolicy_types.go`) has `min`, `max`, `maxIncreasePercent` (default 50) and `maxDecreasePercent`.

## Decision

[![One gate and one size rule: the collector records a termination read from pod status on the Incident with the time it arrived; the context builder matches the current container status to that record and names the killed containers; code accepts a SetContainerMemoryLimit only for one of them, sizes it as the limit times one plus the headroom, rounded up and limited by the policy range, and the pull request shows typed evidence; an Event never opens the gate; the model chooses the action and the container only](../assets/diagrams/memory-limit-one-gate.svg)](../assets/diagrams/memory-limit-one-gate.svg)

### 1. The gate

A `SetContainerMemoryLimit` for a container is accepted only when that container's own status, read from the pod, shows an `OOMKilled` termination inside the analysis window. Otherwise the Incident is handed off with the reason `NoMemoryPressure`. There is no peak path, no tier and no fallback, and nothing waits for [ADR-0039](0039-consequence-symptoms-attach-to-their-causes-incident.md).

- **Source.** Only container status. An Event with the reason `OOMKilled` never counts, and neither does `spec.reason` or the symptom text of the Incident, because their origin is not known (threat model G4.8, the `Immediate` rule). Only a `termination:` occurrence counts, and the collector writes those from `terminations` alone.
- **Which container.** The Incident cannot carry it, so the builder reads it again from the pod. For each pod of the subject it already attributes (`Context.Pods`), it runs the same `terminations` and `occurrence` code (moved to a package both can import) and keeps the container names whose occurrence ID is in the Incident's `status.occurrences` with a recorded arrival time inside the window. It passes them to the loop as a typed list (`Context.Killed`, container names from pod status, never from the Incident). In `finish`, after the catalog check, a proposal whose container is not in the list is `NoMemoryPressure`. A multi-container pod: only the killed container passes, so a killed sidecar cannot step the main container.
- **Inside the window.** The window is `windowOf` (`internal/incidentcontext/builder.go`): from ten minutes before `firstSeen`, at least 15 minutes and at most six hours. Both ends are arrival times of the analyzer's own recording. The node's `finishedAt` is never compared with the analyzer's clock, and no two sources' clocks meet. The Incident has no arrival time per occurrence, so one must be added: `status.occurrences[].observedAt`, set by the Incident controller once, when it first counts the occurrence, with the clock that sets `firstSeen`. It is set only for an observation the collector saw as new. An observation with `MayRepeat` (the termination is older than the collector's first reconcile, as after a restart) is recorded without it, because its arrival says nothing about the kill: a container killed days ago, whose status is still sticky, must not pass. The cost is that a kill that happened while the collector was down gives `NoMemoryPressure` until the next kill. A record dropped from the 32 newest (`MaxOccurrenceRecords`) counts as no kill, the same way.
- **A failed read** of the pods is a source failure that the builder already reports for a retry. It is never turned into "no kill".

### 2. The size

`need = ceil(limit x (1 + headroomPercent / 100))`, where `limit` is the pod template's limit (`previousFromTemplate`, #494). In whole bytes: `(limit x (100 + h) + 99) / 100`, which fits an `int64` (limit at most 1Ti, h at most 100). Then:

1. Round `need` up to a multiple of the unit, 8Mi (a starting value, tuned on lab runs).
2. Take the range `policy.AllowedMemory` returns for an increase from `limit` (`internal/policy/bounds.go`): `lo = max(min, limit + 1)` and `hi = min(max, limit + floor(limit x maxIncreasePercent / 100))`, the arithmetic of `maxDelta`. One function, so the analyzer and the executor's decision cannot disagree.
3. No range (`lo` above `hi`, which includes a limit at or above `max`): hand-off `SizeNotAnIncrease`.
4. A value above `hi` becomes `hi` itself, not rounded again: `limitedBy: Max` when `max` is the lower bound (or equal), `Step` when the step limit is. A value below `lo` becomes `lo`, `limitedBy: None`.

The measured peak decides nothing. Decreases and `AdjustKedaMaxReplicas` ([#497](https://github.com/pkalin/shadewright/issues/497)) are out of scope.

Worked, `headroomPercent` 30, step 50, 1Mi = 1048576 bytes:

1. Limit 48Mi, `max` 1Gi. `48Mi x 1.3` = 62.4Mi, rounded up to 64Mi. `hi = min(1Gi, 48Mi + 24Mi)` = 72Mi. Result 64Mi, `None`.
2. Limit 100Mi, `max` 1Gi. 130Mi, rounded up to 136Mi. `hi` = 150Mi. Result 136Mi, `None`.
3. Limit 10Mi, `max` 1Gi. 13Mi, rounded up to 16Mi. `hi = 10Mi + floor(10Mi x 50 / 100)` = 15Mi. 16Mi is above `hi`: 15Mi (not a multiple of 8Mi), `Step`.
4. Policy `demo-memory` (`min` 64Mi, `max` 128Mi, step 50), limit 48Mi. Round one: 62.4Mi up to 64Mi, `lo` 64Mi, `hi` 72Mi: 64Mi. Round two from 64Mi: 83.2Mi up to 88Mi, `hi = min(128, 96)`: 88Mi. Round three from 88Mi: 114.4Mi up to 120Mi, `hi = min(128, 132)` = 128Mi: 120Mi. Round four from 120Mi: 156Mi up to 160Mi, `hi = min(128, 180)` = 128Mi: 128Mi, `Max`. Round five from 128Mi: `lo = 128Mi + 1` is above `hi` = 128Mi: `SizeNotAnIncrease`.
5. `min` above the result: `min` 75Mi, `max` 128Mi, step 50, limit 52Mi. 67.6Mi, rounded up to 72Mi. `lo` 75Mi, `hi = min(128, 52 + 26)` = 78Mi. 72Mi is below `lo`: 75Mi, `None`.

### 3. The policy field

`headroomPercent` is an optional field of `MemoryLimitBounds`: default 30, bounds 10 to 100 in the schema. The analyzer also checks it in code, because the schema runs only at write: an object stored before the CRD gained the field has none (30 is used), and one that bypassed validation can hold any value. A value outside 10 to 100 is a hand-off with the new reason `HeadroomInvalid`. The executor ignores the field, so its decision does not change; the policy is where the analyzer already reads the bounds (the `analyzer-policy-reader` role, `get` only).

### 4. Typed evidence

An optional `spec.sizing` on the request, set by the analyzer for a memory proposal and only with `SetContainerMemoryLimit` (CEL), from values the builder read and code computed, never from model text. The spec is immutable, so it cannot change after the policy check. Illustrative:

- `headroomPercent`: 10 to 100, the value used. `limitedBy`: `None`, `Step` or `Max`.
- `observedPeak`: whole bytes, 1 to 1Ti, with `window` (start and end) and `samples` (1 to 100000). The three are set together or absent. When no peak was measured (no Prometheus, a partial or capped answer, a failed read) they are absent, never zero.

The executor prints these in the pull request body: the old and new limit, the headroom, `limitedBy` in words, that the container was killed (from the gate), and either the peak with its window or "no peak was measured". Without `sizing` the body says no sizing evidence was recorded. A peak is evidence for the reader and for the later outcome check of [ADR-0030](0030-merge-receipt-and-verification-outcomes.md); it opens no gate and sets no size.

### 5. Hand-off reasons

`NoMemoryPressure`, `SizeNotAnIncrease` and `HeadroomInvalid` join every closed list of reasons (each place verified to exist on `main`):

- the `Hint.reason` enum and `HintDeclineReasons` in `api/v1alpha1/hint_types.go` (they must stay equal), then the CRD and chart CRD, regenerated;
- `declineText` in `internal/controller/hintreply.go`, in the group "the new analysis made no proposal";
- `Reason` in `internal/analysis/loop.go` and `outcomeMessages` in `internal/analysis/controller.go`;
- `internal/analysis/record.go` where it lists reasons, and the reason tables of `docs/troubleshooting.md` and `docs/reference/api.md`;
- `cmd/kubectl-shadewright/explain.go` (the section of each reason), and `refusalReasons` and `resultReasons` in `test/eval/harness_test.go`.

## Consequences

- The size is a fixed step per kill, not a size fitted to the need. A container far below its need takes several rounds, each a pull request a person approves and each verified (ADR-0030). A default step of +30% replaces ADR-0040's +50%.
- A container under memory pressure that is never killed gets no proposal. A workload that runs near its limit by design is not stepped.
- The analyzer needs no metrics source to propose. With one, the peak is shown.
- A change to the Incident status (`observedAt`), the policy (`headroomPercent`), the request (`spec.sizing`) and three reasons: the usual API review, a chart CRD update, and an evaluation run (ADR-0025) because the model's input and output schema change (no size argument, no bounds block for memory). The size is asserted by unit tests of the rule with the exact numbers above, not by the evaluation cases.
- The prompt rule about the smallest change and the policy bounds block for memory limits go (ADR-0026 gets a dated amendment). The analyzer keeps its `get` on the named policies, now for the headroom and the range.

### Invariants

1. **Model output is never authorization.** The model picks the action and the container, and code accepts only a killed one. The executor's policy decision runs as before and does not read `spec.sizing`.
2. **Closed catalog.** No new action or applied parameter.
3. **Git only.** The change is a pull request.
4. **Cluster input is untrusted.** The gate reads the kubelet's container status, not Event or symptom text. The evidence has typed, bounded fields.
5. **Idempotent reconciliation.** Open: whether the plan digest covers `sizing`.
6. **Identities.** No new RBAC verb: the analyzer already has `get`, `list` and `watch` on pods (`config/rbac/analyzer/watch/role.yaml`), `get` on `remediationpolicies` (`config/rbac/analyzer/policies/role.yaml`) and writes Incident status. ADR-0040's grant on `pods.metrics.k8s.io` is not made.

### Threats

- **A forged Event.** It cannot open the gate: its occurrence is `event:` and the gate reads `termination:` records matched with pod status.
- **Pod status written by hand.** Whoever can update `pods/status` is already a node-level or cluster-admin identity; the collector trusts the same field (G4.8). The result is a step bounded by the policy and approved by a person.
- **A killed sidecar used to step another container.** Only the killed container's name passes.
- **Injected text.** It can choose the action and the container, only among killed containers, and not the size.
- **A lying Prometheus.** It changes the evidence, not the gate and not the size.
- **Repeated kills stepping to `max`.** A tenant that makes its own container run out of memory raises its limit one step per kill. The policy `max` and `maxIncreasePercent` bound it, a person approves each pull request, and each step is verified (ADR-0030), where a kill after the rollout ends the request `Failed`.
- **A kill replayed after a collector restart.** `MayRepeat` records carry no arrival time and do not count.

## Alternatives considered

- **Size from the peak with a fixed 1.5 step.** At the limit the peak cannot exceed the limit, so with a factor of 1.5 a headroom from 10 to 50 never set a size and the policy field decided nothing at its default. Rejected.
- **A peak of at least 90% of the limit as the gate.** A workload near its limit by design (a JVM sized to its limit, page cache in the working set) passes on every Incident, fills the new limit and passes again, round after round up to `max`. Rejected.
- **An `OOMKilled` reason on the Incident as the gate.** An Event can carry that reason, so it can be forged. Rejected.
- **`PolicyMaxBelowNeed`.** Nothing measures a need any more, so there is no need to compare with `max`. A step that does not fit is clamped to `max`. Rejected.
- **Evidence tiers and a metrics server adapter.** Two paths to one gate were the source of the holes. Rejected with the tiers.

## Open questions

1. A value below `min` is raised to `min` (example 5). The decision names the step limit and `max` only; confirm this.
2. Whether the plan digest covers `sizing` or stays on the action alone, which leaves the executor's baseline and freshness checks as they are.

## Amendments

None.
