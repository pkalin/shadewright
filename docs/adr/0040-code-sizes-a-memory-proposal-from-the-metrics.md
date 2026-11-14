# ADR-0040: Code sizes a memory proposal from the metrics, and the pull request shows the evidence

- Status: Proposed
- Date: 2026-11-14

This ADR amends [ADR-0026](0026-the-analyzer-shows-the-model-the-policy-bounds.md) (the model sees the policy bounds) and settles the deferred alternative named there. Issues [#508](https://github.com/pkalin/shadewright/issues/508) (sizing in code) and [#506](https://github.com/pkalin/shadewright/issues/506) (evidence in the pull request) are one decision, so they share this page.

Each statement is marked **Decided** (2026-11-14, in the comments of #508 and #506), **Proposed** (worked out from the code, for the maintainer to confirm or change) or **Open** (a question this ADR does not answer).

## Context

In the lab run of 2026-11-07 the model proposed 96Mi for a container limited to 48Mi, and the policy refused the step. ADR-0026 answered by showing the model the bounds, and says what that costs: injected text that steers the model now gets a proposal at the exact maximum step, which passes the policy; "within the bounds, the person approving is now the only check on the size of the step". The pull request body says only that the policy bounds the step and that the model's reason is in the analysis record. ADR-0026 deferred deterministic sizing to #508 because it needs a rule per action and metrics the analyzer may not have.

What the code has today (at `0220bc9b`):

- **The size is the model's.** `MemoryLimitChange.Desired` is an argument of the result schema (`internal/analysis/result.go`). The loop replaces `previous` with the pod template's limit ([#494](https://github.com/pkalin/shadewright/issues/494), `previousFromTemplate`), and checks the direction and `NoChange`, but never the size. `policy.CheckAction` is a catalog check only.
- **The metrics are text.** The context builder (`internal/incidentcontext/metrics.go`) asks the source for the approved templates, among them `container_memory_working_set_bytes` (a range query over the window, `max by (pod, container)` per step), `container_memory_limit_bytes` and `container_last_terminated_oomkilled`, and renders the series as lines of a block. The samples are not kept as numbers; `Context` holds blocks, the template containers and the ScaledObject snapshot.
- **The window** is `windowOf`: from ten minutes before `firstSeen` to now, at least 15 minutes and at most 6 hours, one hour without `firstSeen`. The step is about 60 points, at least one minute. At most 16 pods are asked (`MaxMetricPods`), and an answer can be partial or too large.
- **The analysis loop has no policy.** The bounds block is built by the context builder from the named policies with `policy.BoundsFor` and `policy.AllowedMemory`; the loop sees the text only.
- **The request** has `action.setContainerMemoryLimit` (`container`, `previous`, `desired`) and no field for how the value was found. `spec` is immutable by CEL. The executor's `pullRequestBody` prints the change, the policy, the plan digest and a size sentence (`stepSize`), computed from the typed fields.
- **The policy** (`MemoryLimitBounds`): `min`, `max`, `maxIncreasePercent` (default 50), `maxDecreasePercent` (default 0, which refuses every decrease).

## Decision

[![Sizing from metrics: the context builder reads the memory working set from the metrics source and keeps the numbers; after the reply, code takes only the action and the container from the model, computes the size from the peak plus headroom (a multiplicative step when the peak is at the limit), rounds it and clamps it to the policy; no measurement or no room under the policy ends without a proposal; the request carries the new limit and typed evidence; the policy engine decides as before; the executor prints the evidence in the pull request body; the model sits beside the path and chooses only the action and the container](../assets/diagrams/sizing-from-metrics.svg)](../assets/diagrams/sizing-from-metrics.svg "Open full size")

### Decided on 2026-11-14

1. **Code sets the size.** For `SetContainerMemoryLimit` the desired limit is computed from the metrics the context builder read: the peak working set plus headroom, rounded, clamped to the policy range. The model chooses the action and the container only. A size it suggests is not used.
2. **No metrics, no proposal.** Without a measurement for the container the Incident is handed off with the reason `MissingData`. There is no fixed step and no fallback to a model value.
3. **The request carries the evidence.** Typed, bounded fields hold the numbers code used: the metric, the observed peak, the window, the headroom and the resulting value. They are values the context builder read, never model prose. The executor prints them in the pull request body ([#506](https://github.com/pkalin/shadewright/issues/506)).

### Proposed: the sizing rule

4. **Source.** The `container_memory_working_set_bytes` template over the analysis window, as the builder already asks for it. The measurement is the maximum over all samples and all pods of the container the proposal names. The builder keeps it as a typed value beside the block (a new field of `Context`), from the same answer the model sees, so the evidence and the context cannot differ. Values the model fetched with a tool are not used: only what the builder read.
5. **Peak, not a percentile.** A container is killed at its peak, not at its 99th percentile, so a percentile would size below the point that fails. The window is the one the analysis already has, so the rule adds no query and no template. The cost is that one transient spike sets the size; the headroom and the policy range limit what that can do (threats below).
6. **An OOM kill makes the peak a lower bound.** The working set cannot exceed the limit, so the container that was killed shows a peak at or just under the old limit and the measured peak understates the need. Rule: when the window holds an OOM kill of the container (`container_last_terminated_oomkilled`, already a template) or the peak is at least 90% of the template's limit, the need is at least the limit times 1.5. This is a proposal: the factor and the 90% are constants to confirm, and they are not read from the model. The factor equals the default `maxIncreasePercent`, so that a default policy allows the step.
7. **Headroom.** `need = ceil(peak x (1 + headroomPercent / 100))`, default 25. It is configured on the policy, as an optional `headroomPercent` of `MemoryLimitBounds` (1 to 100): the policy is the object the admin owns and the analyzer already reads for the bounds (ADR-0026), and the executor ignores the field, so the executor's decision does not change. A flag of the analyzer would apply one number to every policy; see the open question.
8. **Rounding.** Up to a multiple of 8Mi. Quantities are written in the binary form the action's pattern accepts.
9. **Clamp.** The computed value is limited to the range that `policy.AllowedMemory` returns for an increase from the template's limit: at least `min`, at most `max`, and at most `maxIncreasePercent` above the current limit, by the arithmetic the executor uses. One function means that the analyzer and the policy cannot disagree on the arithmetic.
10. **No proposal, with a reason.**
    - The need is above the policy `max`: hand-off with the new reason `PolicyMaxBelowNeed`. The text says that the policy bound is too low for the measured need, with the numbers, so an admin can see what to change. A value at the policy `max` would be Allowed and would not fix the fault, which is worse than a hand-off.
    - The clamped value is not above the current limit (the measured need is below it, or no increase is allowed): hand-off with the new reason `SizeNotAnIncrease`. Decreases are not proposed (item 17).
    - The need is above the step limit but not above `max`: the proposal is the largest allowed step, the evidence says it was limited by the step, and the next Incident for the same container may step again. **Open:** hand off instead, as with `max`.
11. **Measurements that do not count.** A partial answer, a capped pod set (more than 16 pods), fewer than five samples for the container, or a failed read gives no measurement, so decision 2 applies. A failed read is a source failure the builder already reports for a retry; it is never turned into "no series". No decision compares a sample time with the analyzer's clock: freshness is the sample count and the answer's own `Partial` flag, which are recorded state.

### Proposed: where it runs, and the policy engine

12. **In the analysis loop, in `finish`, after the reply is decoded and before the request is created.** It sits next to `previousFromCluster`: that function already takes `previous` from the template, and the sizing function takes `desired` from the measurement. The loop needs the policy's typed bounds, which only the context builder reads today, so the builder returns the bounds it used as typed values with the block text (one read, the same one the model's block was made from).
13. **The policy engine still decides every request in the executor** (invariant 1). The computation proposes; it does not authorize. The executor reads the policy again, and a value outside the bounds is Denied exactly as now. The evidence fields are not part of the decision and not read by it (invariant 2: no new action, no new parameter that changes what is applied).

### Proposed: the API and the pull request

14. **Evidence on the request.** A new optional `spec.sizing` next to `action`, set only by the analyzer for a memory proposal. Illustrative, not final:

    ```yaml
    spec:
      action:
        type: SetContainerMemoryLimit
        setContainerMemoryLimit:
          container: web
          previous: 48Mi
          desired: 72Mi
      sizing:
        metric: container_memory_working_set_bytes   # an enum of approved templates
        observedPeak: 50331648                       # whole bytes
        window: {start: "2026-11-14T08:00:00Z", end: "2026-11-14T09:00:00Z"}
        samples: 58
        atLimit: true                                # peak at the limit or OOM kill: the peak is a lower bound
        headroomPercent: 25
        limitedBy: Step                              # none, Step
    ```

    The bounds: `metric` an enum, `observedPeak` a bounded integer, `samples` 1 to 100000, `headroomPercent` 1 to 100, times as `metav1.Time`, no free strings. The resulting value is `action.setContainerMemoryLimit.desired`, not repeated. The spec is already immutable, so the evidence cannot change after the policy check. CEL requires `sizing` for a memory proposal that the analyzer made and forbids it on a revert. **Open:** whether the plan digest covers `sizing` (the digest then changes when only the evidence changes) or stays on the action alone, which keeps the executor's baseline and freshness checks as they are.
15. **The pull request body** gets one section from these fields: the observed peak, the window and the number of samples, the headroom, whether the peak was a lower bound, whether the policy step limited the size, and the resulting value. It replaces the sentence of ADR-0026 that sends the reader to the model's unverified reason: there is no model reason for the size any more. The body prints the typed fields only, redacted as the rest of the body is.

### Proposed: what the model sees

16. **The model no longer needs the policy bounds to size**, and ADR-0026's block stays only if another use appears: with this ADR, the model does not pick a number, so the block that was added so the model could stay inside the policy has no job. Proposed: stop adding the block for `SetContainerMemoryLimit`, and keep the analyzer's `get` on the named policies, which the loop now needs for the bounds (decision 12). The amendment of ADR-0026 says so. **Open:** whether the model should keep seeing that a policy exists for the action at all (`NoPolicyForAction` stays decided in code). The tool schema loses `desired` and `previous` of `MemoryLimitChange`; it keeps `container`. The prompt rule about the size ("the smallest change the evidence supports") is removed, and the catalog text says that the system sets the size. The `metrics` block stays, as evidence for the diagnosis.

### Proposed: evidence tiers (raised 2026-11-14, not decided)

The question raised on 2026-11-14: the Kubernetes metrics server (`metrics.k8s.io`) is installed on most clusters (AKS ships it) and Prometheus is not. Should it be a metrics source? Nothing in this section is decided. Decisions 1 to 3 and the rule above stay as they are unless the maintainer accepts tier 2 (open question 7).

What the code does today without Prometheus: `--prometheus-url` empty means no metrics source, and the builder renders the metrics block as "not available: no metrics source is configured". Nothing in the loop turns that into a hand-off. The status block still shows the container's last termination (reason `OOMKilled`) and the pod's own memory limit, so the model can propose a size, and today it is the model's. A hand-off with `MissingData` happens only if the model lists missing data in its reply (`internal/analysis/loop.go`). Decision 2 is what adds the hand-off for a missing measurement, so it holds only once this ADR is built.

21. **What the metrics server can and cannot say.** The current working set of each container, sampled over a short window. It has no history: no peak, and nothing from before a restart. After an OOM kill it describes the new container instance only.
22. **A second adapter behind the same interface.** `MetricsSource` has one method, `Query`, with approved templates (ADR-0013, ADR-0017). A metrics server adapter answers the working set template only and returns `ErrNotFound` for every other template. It reads with `get` and `list` on `pods.metrics.k8s.io` (API group `metrics.k8s.io`, resource `pods`), granted to the analyzer by a Role and RoleBinding in each watched namespace and never cluster-wide. It is off unless the API is present and a chart value allows it (name not fixed here). This adds a grant to the analyzer, so invariant 6 in `AGENTS.md` would be amended with it.
23. **Three tiers of evidence for a memory size.**
    1. Prometheus: the peak over the window, the full rule of items 4 to 11.
    2. Metrics server only: the OOM kill itself, read from container status (last terminated reason `OOMKilled`, and the limit of that pod's own spec, which the status block already shows), proves that the need was above the limit. The "at the limit" step of item 6 then applies without a peak: the need is the limit times 1.5, clamped as in item 9. The current working set from the metrics server is recorded as corroboration (for example, already above some share of the limit again), never used as the peak. For a container that was not OOM-killed, the metrics server alone gives no size, and the Incident is handed off.
    3. Neither: hand-off with `MissingData`, as decided (decision 2).
24. **Tier 2 revises nothing decided.** There is still no model value and no guessed number, but it widens what counts as a measurement from a series of samples to the kill recorded in container status. That is for the maintainer to decide (open question 7).
25. **The evidence fields name the tier.** `spec.sizing` gets a `source` value (`Prometheus`, `MetricsServer`, `ContainerStatus` or similar, an enum), and the pull request body prints it, so the person merging sees how strong the evidence is. For tier 2, `observedPeak` is the working set that was read, labelled as current, and `atLimit` is true because of the kill.
26. **Model context (follow-up, not decided here).** The same adapter would give the model a current usage figure on installs without Prometheus. That changes the model's input, so it needs an evaluation (ADR-0025) and belongs to a separate decision.

### Out of scope, named

17. **Decreases.** Not proposed. They need their own rule (a low peak is not a reason to cut a limit while the workload may be seasonal) and `maxDecreasePercent` is 0 by default. A follow-up may use the same measurement.
18. **`AdjustKedaMaxReplicas` ([#497](https://github.com/pkalin/shadewright/issues/497)).** That issue fills `previousValue` from the ScaledObject; the size of `maxReplicaCount` has no measurement in this ADR (the template is `deployment_replicas_unavailable`, and the backlog is not a template). A follow-up ADR when a sizing rule for replicas exists. The model keeps choosing that value until then, with ADR-0026 unchanged for that action.

### Evaluation and verification

19. **Evaluation (ADR-0025).** This changes the model's input (no bounds block for memory) and its output schema (no size), so the manifest digest changes and a new recorded evaluation is needed before the loop is used with the change. The cases assert different things: the model's reply is scored for the action, the container and the diagnosis; the size is asserted on the code's output for the recorded context, as a unit test of the rule with exact numbers (for example `oom`: peak at the 48Mi limit gives 72Mi). The "oversized proposal" score planned in [#505](https://github.com/pkalin/shadewright/issues/505) has nothing to score for memory and moves to the rule's tests. A recorded case with no metrics must end `MissingData`.
20. **Outcome verification (ADR-0030).** The recorded peak, window and headroom give a later "cleared" check a baseline to read, for example that the new working set stays below the new limit. Pointer only: the verifier's evidence stays container status in v0.5.0 (ADR-0030 decision 2) and nothing here changes it.

### Invariants

1. Model output is never authorization: the model chooses less than before, and the policy engine decides as before.
2. The catalog is closed and `SetContainerMemoryLimit` keeps its bounds and its test; `sizing` adds no action and no parameter that is applied.
3. Changes still go through Git by pull request.
4. The measurement is read from the metrics source by an approved template with no free-form query text, and the evidence fields are bounded values, redacted in the body; metric text from the source is never copied as prose (series labels are not in the evidence).
5. The request name and the pull request marker derive from the UID and the plan digest as before. If the digest covers `sizing`, the same request still maps to the same pull request.
6. No new identity and no new grant for the decisions above: the analyzer already reads the metrics source and the named policies; the executor reads only the request. Tier 2 (item 22, not decided) would add `get` and `list` on `pods.metrics.k8s.io` in the watched namespaces, and invariant 6 in `AGENTS.md` would be amended with it.

## Consequences

- The model cannot steer the size. Injected text that makes the model propose an action on a container still gets a proposal, but its size is the measured one, so the weakness ADR-0026 accepted (a steered value at the exact maximum step) closes for memory.
- A reviewer sees how the number was found, in numbers they can compare with their own dashboard, instead of a model's reason.
- The analyzer needs a metrics source to propose a memory change. Without one every memory Incident is handed off, as decision 2 intends.
- One more rule to keep right: the constants (90%, 1.5, 25%, 8Mi) are now code that an evaluation does not cover, and need tests with exact values.
- The analyzer's context builder returns typed values, and the loop reads the policy bounds: a larger change to `internal/incidentcontext` and `internal/analysis` than a prompt change.
- A CRD change (`spec.sizing`, `headroomPercent`) needs the usual API review and a chart CRD update.

### Threats

- **A tenant inflates its own working set to get a bigger limit.** A workload that allocates memory raises its peak and so the proposal. This is limited by the policy (`max`, step), by the headroom, and by the person who merges; the pull request now shows the peak next to the size, which makes it visible. It is within what the tenant could already do by asking for a larger limit in its own manifest. Accepted.
- **Stale or missing metrics.** Decision 11: no measurement, no proposal. The count of samples, not a timestamp comparison, is the freshness test.
- **A Prometheus that lies.** It can make the peak small (a proposal that is too low, which does not fix the fault, and the Incident returns) or large (a proposal at the policy `max`, which is Allowed and which a person reads). It cannot make a value outside the policy pass. The source is configured by the operator and the adapter uses approved templates only (ADR-0017); a second source would be a different ADR.
- **The peak understates after an OOM kill.** Item 6.
- **Tier 2: current usage shaped at the moment of the read.** A tenant can shape its working set at the instant the metrics server is sampled more easily than a peak over hours. In tier 2 the size comes from the kill and the limit, not from that number, which is only corroboration; the policy range bounds the result in any case.
- **A sample taken between two scrapes.** The step is at least a minute, so a spike shorter than the step can be missed; the headroom is the margin, and the next kill gives a new Incident.

## Alternatives considered

- **The model's value, cross-checked by code.** Code computes the size and hands off when the model's differs by more than a tolerance. It keeps the model's say, but the model still has to be shown the bounds and can still be steered up to the tolerance; and a disagreement ends in a hand-off for a number the code already knows. Rejected.
- **A fixed step** (always the maximum step, or always +50%). No measurement is needed, but the size has no relation to the need, and the maintainer decided on 2026-11-14 against a fixed step as a fallback. Rejected.
- **Link to the analysis record instead of printing the evidence.** Keeps the request small, but the record is outside the pull request and the reviewer has to look for it. Rejected for the body; the record stays the place for the diagnosis.
- **Headroom in a flag of the analyzer.** One number for all policies, no CRD change. Kept as the fallback if the policy field is refused.
- **A percentile of the working set.** Item 5.
- **The metrics server as the only source for the full rule.** It has no history, so there is no peak over the window and nothing from before a restart. Rejected; it is used only for tier 2 if accepted.

## Open questions

1. Headroom: the policy field (item 7) or one flag of the analyzer, and the default of 25%.
2. The constants of items 6 and 8: 90% of the limit, 1.5, 8Mi.
3. When the step limit is below the need: propose the largest step, or hand off (item 10).
4. Whether the plan digest covers `sizing` (item 14).
5. Whether the model keeps seeing that a policy exists for the action (item 16).
6. Whether one analysis of a workload with several containers may size more than one (the action names one container today).
7. Is tier 2 accepted: an OOM kill read from container status, with the metrics server as corroboration, counts as a measurement for the "at the limit" step (items 21 to 25).

## Amendments

None.
