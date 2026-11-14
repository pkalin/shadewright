# ADR-0039: Consequence symptoms attach to their cause's Incident

- Status: Proposed
- Date: 2026-11-14

## Context

Issue [#432](https://github.com/pkalin/shadewright/issues/432) (with [#642](https://github.com/pkalin/shadewright/issues/642) folded in) asks that one fault open one Incident. It has happened three times on the lab. On 2026-11-10 (`v0.4.0-rc.1`) and again on 2026-11-14 (`v0.5.0-rc.2`) the `oom` scenario opened three Incidents for `demo/web` within two minutes: `OOMKilled`, `BackOff` and `Unhealthy`. Each one used an analysis, each proposed a memory limit for the same container, and on 2026-11-10 a queued request opened a second pull request for the same fix ([#643](https://github.com/pkalin/shadewright/issues/643), fixed separately). On 2026-11-14 the lab also reached its daily cap of 40 analyses in one day of matrix runs. [#726](https://github.com/pkalin/shadewright/issues/726) is the other side: a single readiness failure at pod start opened an Incident and got a memory increase proposed.

The cause is in design section 3.2: the fingerprint hashes the reason, so `BackOff` and `Unhealthy` on a container that was OOM-killed are different fingerprints, hence different Incidents. The fingerprint is not the problem to fix. It is stable across restarts and keeps unrelated problems of one workload apart, which the acceptance criteria of #432 require.

Decisions taken by the maintainer on #432 and not reopened here:

- **Decided (2026-11-10):** the grouping is a deterministic rule in code. No model decides it. A matching observation is evidence on the cause's Incident and opens none of its own. The fingerprint does not change.
- **Decided (2026-11-10):** a consequence Incident already open when its cause's Incident opens is closed with a reason that names the cause's Incident. Its counts are kept. A request of it that has no pull request yet is cancelled.
- **Decided (2026-11-10):** rejected are skipping the analysis while another Incident of the workload is in progress (it delays unrelated problems) and grouping a workload's Incidents into one analysis (it changes model input).
- **Decided (2026-11-14):** the first version is one general container rule. `BackOff` and probe `Unhealthy` are consequences of any container-level cause Incident open on the same container: `OOMKilled`, an image that cannot be pulled, a missing Secret or another configuration error. There is no row per pair.
- **Decided (2026-11-14):** a consequence with no cause is held for a grace period (2 minutes proposed) and opens an Incident only if the symptom is still there after the wait.
- **Decided (2026-11-14):** a cause absorbs matching symptoms on its container for as long as its Incident is open, with no fixed timer. For the late-cause rule, a consequence Incident opened up to 10 minutes before the cause is closed in its favour.

What the code does today, which the proposals below start from (the numbers are of `main` at `0220bc9b`):

- `collector.Observation` (`internal/analyzer/collector`) carries the fingerprint, the subject (the verified owner workload), `Reason`, `Type`, `ReportingController`, the symptom, the occurrence, `Total`, `Count`, `Began`, `MayRepeat` and `Immediate`. It has no container name and no class of symptom.
- An event is read with `regarding` kind and name, reason, note, times and series. The `regarding.fieldPath`, which names the container in a kubelet event (`spec.containers{web}`), is not read.
- Pod status gives one observation per abnormal container termination (`OOMKilled`, or another reason with a non-zero exit code), with the container name in the symptom. Nothing observes a container's waiting state, so an image pull failure or a configuration error reaches the analyzer only as a kubelet Warning event.
- `Incident.Spec` has no container. The spec is immutable. `Incident.Status` has `phase`, `firstSeen`, `lastSeen`, `count`, at most 32 `occurrences`, `occurrencesDroppedThrough`, `resolvedTime`, `lastAnalysisTime`, `conditions` and more.
- `consume` in `internal/incidents/controller.go` decides, per observation: count it into an existing Incident, open one at once (`Immediate`, `recurs`, or a refused fingerprint), or hold a first sighting in memory (`internal/incidents/sightings.go`, seen again 2 to 15 minutes later). `BackOff` and `Unhealthy` are never `Immediate`, because the reason of an event is written by whoever can create events (design 3.1).
- The Incident controller is the only writer of the Incident's phase and `Lifecycle` condition (`internal/incidents/closure.go`). The executor is the only writer of a request's status, and it already holds `get` on the Incident a request names (AGENTS.md, invariant 6).

## Decision

[![One container's symptoms flow to one Incident: the OOMKilled termination opens the cause Incident; BackOff and Unhealthy of the same container are counted as evidence on it and open nothing; a consequence with no cause is held for the grace period and opens an Incident only if it persists; a consequence Incident that opened up to 10 minutes before the cause is closed in its favour and its request without a pull request is cancelled; a cause of another container keeps its own Incident.](../assets/diagrams/cause-rule.svg)](../assets/diagrams/cause-rule.svg "Open full size")

Each point is marked **Decided** (with the date of the comment on #432 that states it), **Proposed** (this ADR's proposal, to be settled by review) or **Open**.

### 1. The rule and its classes

1. **Decided (2026-11-10, 2026-11-14).** The rule is deterministic code with no model input or output. A consequence observation of a container that has an open cause Incident is evidence on that Incident and opens none. The fingerprint is unchanged.
2. **Proposed.** The collector sets two new fields on an observation, from what it read itself: `Container` (the container name, or empty) and `Class`, one of `cause`, `consequence` or none.
    - `consequence`: reason `BackOff` or probe `Unhealthy` from the kubelet, on a named container.
    - `cause`: a container termination read from pod status, with any reason (`OOMKilled` or an exit code), and a kubelet Warning event with reason `Failed` on a named container (an image that cannot be pulled, a missing Secret or ConfigMap key). Decided on 2026-11-14 is "any container-level cause"; this list is the proposal for what that means in code. A termination of any reason is included because a crash loop produces `BackOff` too.
    - Everything else has no class and goes through the gate as today.
3. **Proposed.** Reason and class are matched together with the reporting controller `kubelet`. Both are written by whoever creates the event, like every field of an event (invariant 4); section 8 says what that allows.

### 2. Where it runs and what state it needs

4. **Proposed.** The rule runs in the Incident controller's `consume`, after the occurrence is checked against what was counted and before the first-sighting gate and the fingerprint store. A `cause` observation goes the same way as today (`OOMKilled` stays `Immediate`; a `Failed` event goes through the gate). A `consequence` observation goes to the rule.
5. **Proposed.** The rule needs one piece of state: which open Incidents have a cause for which container. It is recorded on the Incident, not held in memory, as a new status field `status.containers` (at most 8 container names, each a DNS label, written by the controller from `Observation.Container` in the same status write that counts the observation). A cause Incident is open when its phase is not `Resolved` (`Open`, `Analyzing`, `Proposed` or `Escalated`) and its subject UID equals the observation's subject UID. The controller already loads every Incident at start and keeps them current from its own writes and its trigger passes, so a restart finds the causes again. A spec field was not chosen: the spec is immutable and the fingerprint does not hash the container for events.
6. **Proposed.** The hold for a consequence with no cause is kept by the collector, as it keeps module symptom states (`collector.SettleAfter`): it does not emit the observation, it reconciles the object again after the grace period (`collector.ConsequenceGrace`, 2 minutes proposed). On a restart the informer lists the events and pods again, the collector's question `Handled` finds the occurrence not counted, and the hold starts again from the new process's first sight, so it is rebuilt from cluster state and can only be longer, never lost. No decision compares the event's time with the collector's clock: the wait is measured on the collector's clock alone. The in-memory first sightings of the controller are not used for it.
7. **Proposed.** At the end of the wait the collector decides from state it reads in its own cache: the pod exists with the same UID and the named container is still not ready (for `Unhealthy`), or is still waiting in back-off or has restarted since the event (for `BackOff`). If so it emits the observation, and `consume` asks the cause question again at that moment, because a cause may have opened during the wait. If not, nothing opens. A read that fails is retried, never taken as "gone". Today a single event that is not seen again opens nothing, and one that is seen again 2 to 15 minutes later opens its Incident whatever the pod looks like now; this check replaces that rule for the two consequence reasons. It also covers #726, whose single readiness failure of a pod that is Ready seconds later would be dropped.

### 3. Which container

8. **Proposed.** For an event, the container is the one in `regarding.fieldPath`, read only in the form `spec.containers{<name>}` or `spec.initContainers{<name>}` with a DNS label name, and only when the regarding object is the Pod whose UID the collector verified. The name must also exist in that pod's spec in the collector's cache. Anything else (another form, a name not in the pod, a regarding object that is not a Pod) leaves `Container` empty.
9. **Proposed.** For a termination read from pod status, the container is the status entry's name, as the symptom already says.
10. **Proposed.** An observation with no named container is never absorbed and never held by this rule: it goes through the gate as today. A cause with no named container does not absorb. The rule does not guess from a pod's single container or from the text of a note.

### 4. What the cause's Incident shows

11. **Proposed.** A new status field `status.absorbed` holds one entry per consequence reason (at most 2 now, bounded to 8 by the schema): `reason`, `count` (saturating at 1,000,000 like `status.count`) and `lastSeen`. The occurrence of an absorbed observation is recorded in the cause's `status.occurrences` like any other, so the existing rules for restarts, the cap of 32 and the drop mark apply unchanged: a restart does not count an absorbed event twice. The price is that absorbed events share the 32 records with the cause's own occurrences; an old one dropped from the list is at worst counted again as `MayRepeat` or not at all (an undercount, section 3.2).
12. **Proposed.** An absorbed observation does not change `status.count` or `status.lastSeen`, so it does not make the cause due for analysis again. The closure rules' quiet hour does count `absorbed[].lastSeen`, so a cause whose consequences are still arriving is not resolved as quiet.
13. **Proposed, changes model input.** The model's context for the cause gets a small block built from `status.absorbed`: the reasons and counts and the last seen time, nothing from a note. The reasons come from the fixed table, not from the cluster, so the block carries no text an attacker writes. This changes what the model sees, so the evaluation gate of ADR-0025 applies: a recorded scenario for the `oom` case with the block, and no regression in the existing cases, before the change is built. **Open:** whether the first build leaves the block out, shows the cause alone, and adds the block after the evaluation. The rule itself does not depend on it.

### 5. A consequence Incident that opens before its cause

14. **Decided (2026-11-10, 2026-11-14).** When a cause Incident opens, a consequence Incident of the same subject and container that is open and was opened up to 10 minutes earlier is closed in its favour, its counts kept; a request of it with no pull request yet is cancelled.
15. **Proposed.** The Incident controller does it in the same pass that creates the cause Incident, from its own list of Incidents. "Up to 10 minutes earlier" compares the `firstSeen` of the two Incidents, both written by the Incident controller, so no two clocks are compared. A leader change moves the clock to another process, and the 10 minutes are wide enough for the skew between nodes. A consequence Incident older than that stays as it is, with its own analysis.
16. **Proposed.** The closed Incident ends in phase `Resolved` (the only phase that does not ask for work and that the trigger never marks due), with `resolvedTime`, the `Lifecycle` condition reason `FoldedIntoCause` and a fixed message, and a new status field `status.foldedInto` with the cause Incident's name and UID (the name derives from the fingerprint, so it holds nothing from the cluster). The `Lifecycle` reasons gain `FoldedIntoCause`; no new condition type is needed. One status write sets all of it, with the field manager `shadewright-incident-lifecycle`, and it is decided again from what the API server holds after a restart (invariant 5).
17. **Proposed, with an Open point.** The request is cancelled by the executor, not the analyzer, so each stays the only writer of its own objects. The executor already reads the Incident a request names. For a request that has not reached `PRCreated` it ends the request `Cancelled` with reason `IncidentFolded` when that Incident has `status.foldedInto`, in the same write that releases its target lock and its slot under `maxConcurrentRemediations`, as `endHandoff` does. A request that already has a pull request runs its course; the ADR does not close pull requests. **Open:** [#289](https://github.com/pkalin/shadewright/issues/289) says the `Cancelled` phase has no way in, and asks for a spec field or removal. The code has two ways in already, both of the reviewer hint handoff (`HandoffRefused`, `HandoffAborted`), so the phase is in use. Whether `IncidentFolded` is a third way in or waits for #289's decision is for the maintainer.
18. **Proposed.** An analysis already running on the closed Incident publishes nothing: the claim check of ADR-0025 reads the phase before it creates the request. **Open:** to be confirmed against `internal/analysis` when this is built; an analysis in flight has already used its unit, which is never given back.

### 6. When the cause resolves and the consequence stays

19. **Proposed.** A cause absorbs only while its Incident is open. When it is `Resolved` (for example the memory limit was fixed and the request `Succeeded`) and the probe still fails, the next `Unhealthy` finds no open cause: it is an orphan, held for the grace period and opened only if the container still fails. If the fingerprint has a folded Incident, that one is reopened as `Open` with `Lifecycle` `Reopened`, and analysed as usual. This needs a change to the recurrence rule, which would otherwise escalate a `Resolved` Incident seen again within 24 hours without analysis (design 3.2): an Incident closed as `FoldedIntoCause` is not a failed fix, so it is exempt from that escalation. **Open:** the same exemption when the folded Incident had already been analysed before the cause appeared.

### 7. Interaction with the other limits

20. **Proposed.**
    - **Debounce and cooldown:** unchanged for the cause. Absorbed observations add no data for the trigger (point 12), so they restart neither.
    - **`--max-incidents`:** a held or absorbed observation takes no Incident place. A folded Incident still counts until someone deletes it.
    - **Daily analysis budget:** a fold before the analysis starts saves the unit, since a `Resolved` Incident is not marked due. One folded while `Analyzing` has spent it.
    - **Target lock and `maxConcurrentRemediations`:** a cancelled request releases both in the same write (point 17). #643 stays a separate fix for two Incidents that propose the same field for another reason.

### 8. Out of scope

21. **Decided scope (this ADR).** KEDA, HPA, Flux and other non-container symptoms, and the symptom states of module sources (`condition:` occurrences), are not touched. Only the two consequence reasons on a named container and the causes of point 2 are in the table. A row per pair, or a table the operator can configure, is a later decision.

## Consequences

One OOM of `demo/web` should give one Incident, one analysis and at most one request. The acceptance checks are:

- the lab `oom` scenario gives one analysis and at most one request per workload;
- the evaluation cases `startup-probe` and `replaced-pod` open nothing;
- a missing Secret on one container next to an OOM on another keeps two Incidents.

Each needs a test that fails without the change.

It adds a status field to the Incident for the container (`containers`), one for the tally (`absorbed`) and one for the link (`foldedInto`), a `Lifecycle` reason, a request reason, a read of `fieldPath` in the collector and two new fields on `Observation`. The CRD changes are additive. A request that waited behind the target lock for a folded consequence is now cancelled instead of opening a second pull request.

What becomes harder: a symptom of the same container no longer has an Incident of its own while a cause is open, so a person reads the cause's `absorbed` tally to see it, and the Incident count under the cap no longer shows every symptom. A consequence takes the grace period longer to open, 2 minutes proposed, on top of the existing rules.

### How the invariants hold

1. Model output is never authorization: the rule is code and the model neither chooses the cause nor the fold. The model's context may get the tally (point 13), which is data.
2. The action catalog is unchanged.
3. Nothing is written to Git by the rule. The only effect on requests is a cancel before a pull request exists.
4. The reason, the controller and the `fieldPath` of an event are untrusted. The rule uses them only to match fixed values, checks the named container against the pod the collector verified, and stores only reasons from the fixed table and counts.
5. The cause state is in the Incident, the fold is one status write decided from recorded state, and the hold is rebuilt from the informer. Deciding the late-cause window uses two times written by the same component, and the grace period one clock.
6. The analyzer writes only the Incident, the executor only request status. The executor's `get` on the Incident is the one it has. No new grant is needed.

### Threats

- **A tenant provokes a cheap cause to hide a real `Unhealthy`.** Whoever owns a pod can make a container crash on purpose and so keep a cause Incident open, and the probe failures of a real fault would be absorbed. They stay visible in `status.absorbed` and, if point 13 is built, in the model's context; the cause Incident is analysed and, if it ends `Escalated`, a person sees it. The rule has no timer, as decided, so an Incident kept open this way absorbs for as long as it lasts. **Open:** whether the tally needs a bound, for example an escalation when `Unhealthy` counts keep rising after the cause's request has succeeded.
- **Event floods.** A flood of forged `BackOff` events for a container with an open cause costs one counted occurrence each, bounded by the existing caps (1,000 per observation, 256 held occurrences per Incident, the per-namespace share), and opens nothing. A flood for a container with no cause is held in the collector's bounded state and dropped if the container is fine.
- **A forged `Failed` event as a fake cause.** It needs a named container that exists in a pod of the same namespace. It makes `BackOff` and `Unhealthy` of that container fold into a `Failed` Incident, which the tenant can already cause by running a pod that fails. The cross-namespace and no-UID checks of design 3.1 stay.

## Alternatives considered

- **Skip the analysis while another Incident of the workload is in progress.** Rejected on 2026-11-10: it delays an unrelated problem on the same workload.
- **Group a workload's Incidents into one analysis.** Rejected on 2026-11-10: it changes the model's input for every workload.
- **A table with one row per cause and consequence pair.** Replaced on 2026-11-14 by the one general container rule.
- **Put the container in the fingerprint.** It would change every fingerprint and split Incidents that today merge, and it would still not join `BackOff` to `OOMKilled`, whose reasons differ.
- **Hold the consequence in the Incident controller's first-sighting memory.** It is bounded and forgotten on a restart, and it cannot read the pod's state. The collector holds the pod cache.

## Open questions

- Whether the first build shows the tally to the model (point 13).
- Whether `IncidentFolded` waits for the decision of #289 (point 17).
- A bound on absorbing, or an escalation, after the cause's request succeeded and the consequence keeps rising (threats).
- The exemption from the 24-hour recurrence escalation for a folded Incident that was analysed before (point 19).
- The grace period of 2 minutes and the 10 minute window: values to tune on lab runs.

## Amendments

None.
