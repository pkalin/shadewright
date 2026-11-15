# ADR-0042: A memory limit is proposed only for a container that was killed, and code sets the size

- Status: Proposed. Acceptance waits for a lab run of the [out-of-memory scenario](../lab-oom-hint.md) that confirms the gate.
- Date: 2026-11-15

This ADR supersedes [ADR-0040](0040-code-sizes-a-memory-proposal-from-the-metrics.md) and amends [ADR-0026](0026-the-analyzer-shows-the-model-the-policy-bounds.md). It settles [#508](https://github.com/pkalin/shadewright/issues/508) (sizing in code) and [#506](https://github.com/pkalin/shadewright/issues/506) (evidence in the pull request). Everything here is <span class="sw-status sw-planned">planned</span>: nothing is built.

## Context

ADR-0040 let the model name an action and a container and let code compute the size. Its gate was assembled from several paths (the Incident's `OOMKilled` symptom, a peak of at least 90% of the limit, two evidence tiers, a fallback), and three review passes each found a different hole in it. The last one: an Event can carry the reason `OOMKilled`, and the Incident's `reason` is that string, so whoever can create Events in a watched namespace can forge the symptom. This ADR replaces the gate with one built on a source that cannot be forged that way, and keeps one size rule.

Two links of ADR-0040 move here: [ADR-0034](0034-declarative-pack-format.md) open question 2 (pack-named values, to be revisited once sizing by code exists, which this is for memory) and [#741](https://github.com/pkalin/shadewright/issues/741) (metrics as evidence for the outcome check of [ADR-0030](0030-merge-receipt-and-verification-outcomes.md)).

## Decision

[![One gate and one size rule: a SetContainerMemoryLimit is accepted only for a container whose pod status shows an OOMKilled termination that the collector saw happen, in a pod whose limit equals the template limit; code sizes it as the limit times one plus the headroom, rounded up and limited by the policy range (Step, Max or Min); the request carries typed evidence including the killed pod; an Event never opens the gate and the measured peak is evidence only; the model chooses the action and the container, never the size](../assets/diagrams/memory-limit-one-gate.svg)](../assets/diagrams/memory-limit-one-gate.svg)

### 1. The gate

A `SetContainerMemoryLimit` for a container is accepted only when all five conditions hold:

1. There is a termination with the reason `OOMKilled`. Other non-zero exits do not count.
2. It is a termination of the named container, read by the collector from the pod's status. An Event never counts, and neither does the Incident's `spec.reason` or its symptom text, because their origin is not known (threat model G4.8).
3. The collector saw it happen. A termination already in the status when the collector started (after a restart, or one no Incident counted) never opens the gate. The collector sets this mark and nothing downstream rewrites it.
4. The killed pod's memory limit for that container equals, by value, the template limit the step starts from. The pod's limit is `spec.containers[].resources.limits.memory`. An absent limit on either side is never equal, and when the pod's status shows other resources in effect (an in-place resize) there is no proposal. A kill of an old pod at a limit the template has already left (a rollout in progress) does not count.
5. The Incident being analyzed counted it, inside its analysis window. The time of the count and the start of the window come from the analyzer's own clock; the node's `finishedAt` is never compared with it.

Otherwise the Incident is handed off with `NoMemoryPressure`. There is no peak path, no tier and no fallback, and nothing waits for [ADR-0039](0039-consequence-symptoms-attach-to-their-causes-incident.md).

### 2. The size

`need = ceil(limit x (1 + headroomPercent / 100))`, where `limit` is the template's limit. Round `need` up to a multiple of 8Mi (a starting value, tuned on lab runs). Then take the range the policy allows for an increase from `limit`: `lo = max(min, limit + 1)` and `hi = min(max, limit + floor(limit x maxIncreasePercent / 100))`. The analyzer and the executor use one function for it, so they cannot disagree.

- No range (`lo` above `hi`, which includes a limit at or above `max`): hand-off `SizeNotAnIncrease`.
- A value above `hi` becomes `hi` itself, not rounded again: `limitedBy` is `Max` when `max` is the lower bound (or equal), `Step` when the step limit is.
- A value below `lo` becomes `lo`, with `limitedBy: Min` when `lo` is `min`. Below `min` the policy would deny the request, so raising to `min` is decided.

Worked, `headroomPercent` 30, step 50, `max` 1Gi unless stated, each a single step of its own Incident:

1. Limit 48Mi: 62.4Mi rounds up to 64Mi. `hi` = 72Mi. Result 64Mi, `None`.
2. Limit 100Mi: 130Mi rounds up to 136Mi. `hi` = 150Mi. Result 136Mi, `None`.
3. Limit 10Mi: 13Mi rounds up to 16Mi. `hi` = 15Mi. Result 15Mi (not a multiple of 8Mi), `Step`.
4. `min` 75Mi, `max` 128Mi, limit 52Mi: 67.6Mi rounds up to 72Mi. `lo` = 75Mi, `hi` = 78Mi. Result 75Mi, `Min`.
5. `max` 128Mi, limit 120Mi: 156Mi rounds up to 160Mi. `hi = min(128Mi, 180Mi)` = 128Mi. Result 128Mi, `Max`. From 128Mi, `lo` is above `hi`: `SizeNotAnIncrease`.

### 3. The policy field

`headroomPercent` is an optional field of the memory limit bounds of the `RemediationPolicy`: default 30, bounds 10 to 100. The schema checks it at write, and the analyzer checks it again in code, because an object stored before the field existed has none (30 is used) and one that bypassed validation can hold any value. A value outside 10 to 100 is a hand-off with the new reason `HeadroomInvalid`. The executor ignores the field. A default is never used when the policy cannot be read: that is a hand-off, never defaults.

### 4. Evidence

The request gets an optional, immutable `spec.sizing`, set by the analyzer only for `SetContainerMemoryLimit` from values code read and computed, never from model text. It carries the headroom used, `limitedBy` (`None`, `Min`, `Step` or `Max`), the killed pod's name, UID and creation time, and optionally the peak working set over the window with its window and sample count. When no peak was measured (no Prometheus, a partial answer, a failed read) the peak is absent, never zero.

The executor prints in the pull request body the old and new limit, the headroom, `limitedBy` in words, the killed pod (name, UID and creation time), so a reviewer can check the evidence, and the peak when measured or the words "no peak was measured". The peak is evidence only: it opens nothing, closes nothing and sets no size. It is also what the later outcome check of ADR-0030 can read.

### 5. One step per Incident

The size is one step, not a size fitted to the need, and nothing here says that steps converge on the need. A kill after the step ends the request `Failed` with reason `OOMKilled` (ADR-0030 section 4) and the Incident `Escalated`, so a person decides. That holds only when outcome verification is on. `--verify-outcomes` defaults to false in the executor, and the chart value `executor.verification.enabled` leaves it off. With it off, a kill after the step moves nothing: the request stays `Merged`, the Incident stays `Proposed` and is not analyzed again.

No revert is proposed after a failed step, because a revert would put back the limit the container was already killed at. ADR-0030 gets a dated amendment for it, since this removes a proposal (<span class="sw-status sw-planned">planned</span>). Lab runs decide whether the default headroom of 30 should be larger.

### 6. Order

In the analyzer: the catalog type and the subject, the template's limit (`PreviousValueUnknown` when absent), the gate, the size by code, then the policy's action check on the action code built. The model's size is never used.

## Consequences

- A container under memory pressure that is never killed gets no proposal. A workload that runs near its limit by design is not stepped.
- The prompt rule about the smallest change and the policy bounds block for memory limits go (ADR-0026 gets a dated amendment). The model's input and output schema change (no size argument), so an evaluation run (ADR-0025) is needed.
- Three new hand-off reasons join the closed lists: `NoMemoryPressure`, `SizeNotAnIncrease` and `HeadroomInvalid`.

## Alternatives considered

- **Size from the peak with a fixed 1.5 step.** At the limit the peak cannot exceed the limit, so a headroom from 10 to 50 never set a size and the policy field decided nothing at its default.
- **A peak of at least 90% of the limit as the gate.** A workload near its limit by design passes on every Incident, fills the new limit and passes again, up to `max`.
- **A peak under 90% closing the gate.** A container that allocates fast is killed between two scrapes, so its measured peak can be far under the limit it hit. The lab's own out-of-memory scenario does this.
- **An `OOMKilled` reason on the Incident as the gate.** An Event can carry it, so it can be forged.
- **A seen-live mark from the informer's old and new objects.** The pod reconciler is level-based: it receives a name and reads the pod, so it never holds both objects.

## Threats and accepted residuals

- **A forged Event.** Closed: an Event is never read by the gate.
- **A kill replayed after a collector restart.** Closed by the seen-live mark. Residual: the mark compares the node's `finishedAt` with the collector's start time, so a node clock running ahead could make a replayed kill look live. The step stays bounded by the policy and the approval.
- **A look-alike pod (G4.9).** Someone who can create pods in a watched namespace can make a pod that passes as the workload's and have it killed. The policy and the approval bound the step, and the body names the pod with its UID and creation time.
- **Writers of `pods/status` and `pods/resize`.** The first can write a termination, which the collector trusts as well (G4.8). The second can change the limit the pod runs with, and then the status shows other resources in effect, so there is no proposal.
- **A node-wide out-of-memory kill.** The kubelet reports it as `OOMKilled` even far below the container's limit. A known false positive, bounded by the policy and the approval. The body shows the peak when there is one, so a reviewer can see a kill far under the limit.
- **Injected text.** It can pick only a container that passes the gate, and never the size.

## Invariants

1. **Model output is never authorization.** The model picks the action and the container, and code accepts only a killed one. The executor's policy decision runs as before and does not read `spec.sizing`.
2. **Closed catalog.** No new action or applied parameter.
3. **Git only.** The change is a pull request.
4. **Cluster input is untrusted.** The gate reads container status, not Event or symptom text. The evidence has typed, bounded fields.
5. **Idempotent reconciliation.** Whether the plan digest covers `sizing` is settled in the build.
6. **Identities.** No new RBAC verb: the analyzer already reads pods and `get`s the named policies. The wording of invariant 6 in `AGENTS.md` (why the analyzer may `get` named policies) is updated in the pull request that builds this.

## Settled in the build

The task issues decide, against the code, with the builder and the code review checking each:

- the context fields that carry the killed container and its pod;
- the window the gate uses;
- a hand-off reason of its own for a failed pod read or a failed policy read (not `MissingData`, which the evaluation harness scores as the model's refusal);
- the closed lists the new reasons join;
- how the revert proposer recognises a sized request: `SetContainerMemoryLimit` with `spec.sizing` set;
- whether the plan digest covers `sizing`.

## Amendments

None.
