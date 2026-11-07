# 15. Decisions made and open questions

Decided on 2026-11-01:

| Topic | Decision |
| --- | --- |
| Policy format | Cluster-scoped `RemediationPolicy` CRD ([ADR-0011](../adr/0011-policy-as-cluster-scoped-crd.md)) |
| Object to source mapping | Flux labels plus plain manifests only, otherwise `NeedsIntervention` ([ADR-0012](../adr/0012-object-to-source-mapping.md)) |
| External systems | An interface and adapters for every one of them ([ADR-0013](../adr/0013-adapters-for-every-external-system.md)) |
| Notifications | GitHub issue in the fleet repo first; Teams through a `Notifier` adapter later ([ADR-0014](../adr/0014-notifications-github-issues-first.md)) |
| Analysis records | Kept, redacted, in storage that outlives the lab ([ADR-0015](../adr/0015-analysis-records-outlive-the-lab.md)) |
| Lab region and models | Sweden Central; the model is picked by benchmark from what Foundry offers there ([ADR-0016](../adr/0016-lab-region-sweden-central.md)) |
| Lab metrics | Prometheus in the lab cluster, Grafana later if needed ([ADR-0017](../adr/0017-prometheus-in-the-lab.md)) |
| Kill switch | A field in a cluster-scoped config object, checked before every write |
| Evaluation | Recorded contexts per scenario with the expected diagnosis or refusal; CI runs them against a fake model, the real model on demand |
| Agent observability | controller-runtime Prometheus metrics and Shadewright's own `shadewright_*` metrics on the same endpoint ([reference](../reference/metrics.md)), plus OpenTelemetry traces per analysis (phases, model calls with tokens, tool calls, latency; off by default, OTLP gRPC, IDs, counts and fixed labels only: [reference](../reference/traces.md)) |
| Packaging | Scaffolded once with Kubebuilder's Helm plugin, then maintained by hand and guarded by `test/chart`. Details in the list "Packaging, in detail" below the table |
| Pluggability | Three axes: platform services, cluster platforms, cluster integrations ([ADR-0018](../adr/0018-three-axes-of-pluggability.md)); security integrations diagnose only; a module contributes hooks only when enabled and detected (2026-11-03) |
| Extension packaging | Compiled in now, registered at start-up from an explicit list; signed declarative OCI packs later ([ADR-0019](../adr/0019-extension-packaging.md)) |
| Controller identities | Separate analyzer and executor ServiceAccounts and roles; incidents and requests reachable only in the operator namespace, the analyzer's namespaced reads only in the watched namespaces; cluster-wide it reads only the settings and a Node list (2.2, [ADR-0024](../adr/0024-analyzer-writes-detection-to-settings-status.md)) |
| Detection report (2026-11-03) | The analyzer writes detection to the `RemediationSettings` status; no component reads any field of it to decide ([ADR-0024](../adr/0024-analyzer-writes-detection-to-settings-status.md)) |
| Scenario switch (2026-11-03) | Flux on each cluster tracks its own `cluster/<cluster>` branch; the scenario harness moves it forward with one scenario or reset commit at a time and never writes `main`; the agent's pull requests target that branch, under rulesets that mirror `main`'s protections for them; scenarios run on Kind and the lab ([ADR-0022](../adr/0022-scenario-switch-and-clusters.md)) |
| YAML patching (2026-11-04) | One scalar replaced in place with `go.yaml.in/yaml/v3`, then a semantic check that only that value changed ([ADR-0020](../adr/0020-yaml-patching.md)) |
| Calling the model (2026-11-03) | Prompts as code, context first with one structured call, escalation on deterministic signals with bounded tools, an evaluation gate keyed on model and prompt that the analyzer enforces at run time; a durable daily count of analyses that fails closed ([ADR-0023](../adr/0023-how-the-analyzer-calls-the-model.md)) |


Packaging, in detail:

- The chart and the images are signed keyless by digest, with SBOMs and provenance for the images ([verify a release](../upgrade-uninstall.md#verify-a-release)).
- The Helm chart is in `dist/chart`. It is published to `oci://ghcr.io/pkalin/charts/shadewright` on release tags, pre-release tags included (#83, #82).
- The GitHub release lists the image and chart digests with the verify commands (#84).
- The published chart runs the release's signed images by digest. A chart from a source checkout needs the digests or the explicit `image.allowTag` opt-in.
- It installs with no values as an idle Shadewright: no watched namespace, no Azure workload identity.
- It passes `ct lint`, `ct install` and `helm test` in CI (#307).

Still open:

1. **Helm-rendered and patched objects.** How to map and patch objects that come from Helm charts, Kustomize patches or generators. Revisit when a real case needs it.
2. **YAML patching.** Decided on 2026-11-04: [ADR-0020](../adr/0020-yaml-patching.md) is accepted.
   - Replace the bytes of one scalar found with `go.yaml.in/yaml/v3`, then check that only that value changed.
   - A spike showed that `kyaml` reformats the file and changes aliased values.
   - Kept as an item so that the numbers do not change (row above).
3. **Model choice.** Benchmark on the scenario set at stage 1: a small model for triage and a stronger one for escalation.
4. **Teams.** Whether the maintainer's Microsoft account can receive Teams Workflows webhooks. If not, GitHub issues stay the only notifier for now.
5. **Required checks in policy.** Whether a policy should name verification checks a request cannot drop, beyond the ones each action always runs.
6. **Scenario switch.** Decided on 2026-11-03: [ADR-0022](../adr/0022-scenario-switch-and-clusters.md) is accepted (row above). Kept as an item so that the numbers below do not change.
7. **Calling the model.** Decided on 2026-11-03: [ADR-0023](../adr/0023-how-the-analyzer-calls-the-model.md) is accepted (row above). Where the daily spend is counted is [#208](https://github.com/pkalin/shadewright/issues/208).
8. **Incident retention.**
   - Incidents are bounded by a count (3.2), not deleted: the analyzer has no `delete` on them.
   - Decided on 2026-11-05 ([#251](https://github.com/pkalin/shadewright/issues/251)): the cap keeps counting every Incident, `Resolved` ones included, so the number of objects stays bounded. `Resolved` marks the ones safe to delete, and an admin deletes them by hand, then restarts the analyzer ([procedure](../how-to/clear-resolved-incidents.md)). Automatic retention with an archive, which needs a delete grant (invariant 6), is a separate ADR task ([#458](https://github.com/pkalin/shadewright/issues/458), planned). Who moves an Incident to `Resolved`, and when, is the lifecycle in 3.2. Built: a symptom seen again on a `Resolved` Incident escalates it within 24 hours of `resolvedTime` (or without one) and opens the next occurrence after, each a new object under the cap. **Planned, not built:** nothing sets `Resolved` or `resolvedTime` yet.
9. **Kustomizations that wait.**
   - A Kustomization with `wait: true` or `healthChecks` stays not Ready while its workload is unhealthy, so the mapper refuses it ([ADR-0012](../adr/0012-object-to-source-mapping.md), amendment of 2026-11-04).
   - Open: whether, and how safely, to accept applied-but-unhealthy objects ([#259](https://github.com/pkalin/shadewright/issues/259)).
