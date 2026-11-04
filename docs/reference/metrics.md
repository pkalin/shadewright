---
title: Metrics reference
---

# Metrics reference

Both components serve Prometheus metrics on their metrics endpoint (`:8443` over HTTPS with `config/default`; who may scrape it and how is in the [configuration reference](configuration.md#metrics-endpoint)). Next to controller-runtime's own metrics (below), Shadewright adds the metrics on this page, all named `shadewright_*` and registered on controller-runtime's registry.

Every label takes its values from a closed set listed here, never from a name, namespace, UID, fingerprint or message in the cluster, so the number of series stays fixed whatever the cluster holds. A value a component does not know (a phase written by a newer version, a reason outside the list) is reported as `Other`. Tests in `internal/controller/metrics_test.go` and `internal/incidents/metrics_test.go` check, with `internal/testutil/metricguard`, that every `shadewright_*` metric the package registers is in the test's table, has only the labels the table names and only label values from the table's sets (after free text has been passed through the phase and reason paths), and that every metric in the table is named on this page. `internal/controller/denialreasons_test.go` fails when a reason constant of the policy check or the source mapping is missing from the denial reasons reported by name.

The phase gauges are computed when the endpoint is scraped, from the informer the leader's controller already uses, and only once that informer has synced: a scrape makes no API call and never starts a watch. Only the leader reports them; a standby replica, or a leader whose informer has not synced yet, reports no series for them. Counters are reported by the replica that did the work (the leader) and start again at zero when a Pod restarts: use `rate` or `increase`. Counters with a closed set of labels report every value from the start, at zero. Aggregating with `max` across replicas is safe for every gauge here.

## Executor

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `shadewright_remediation_requests` | gauge | `phase`: a [RemediationRequest phase](api.md), `None` (no phase yet) or `Other` | RemediationRequests in the operator namespace, by phase. Reported by the leader only; missing (not zero) before its informer has synced or when the read fails |
| `shadewright_policy_decisions_total` | counter | `outcome`: `allowed`, `denied`, `needs_intervention` | Decisions of the policy check, each counted at most once, after the write of the status that records it succeeded. A write that lands but whose answer is lost is not counted, and the next reconcile finds the request decided, so the counter undercounts while the API server has errors |
| `shadewright_policy_denials_total` | counter | `reason`: the `PolicyAllowed` reason of a `Denied` request, or `Other` | Requests the policy check denied, by reason. The reasons are those of the policy check (`PolicyNotFound`, `SettingsNotFound`, `TargetMismatch`, `PreviousValueMismatch`), of the policy rules (for example `AboveMaximum`, `KindNotAllowed`, `ModuleNotEnabled`) and of the source mapping (for example `RepositoryNotInPolicy`, `BranchMismatch`). Every reason in the list and `Other` start at zero |
| `shadewright_remediation_reconcile_errors_total` | counter | `kind`: `timeout`, `conflict`, `cancelled`, `other` | Errors the RemediationRequest reconciler returned, each retried with backoff. `timeout` is a step that reached its own deadline, `conflict` a status write made from a stale read (harmless when it is rare), `cancelled` a reconcile stopped by shutdown or a lost leadership. controller-runtime's `controller_runtime_reconcile_errors_total{controller="remediationrequest"}` counts the same errors without a kind |
| `shadewright_remediations_in_flight` | gauge | none | RemediationRequests that hold a slot under `maxConcurrentRemediations`, as the last check of the limits counted them. 0 until the first check |
| `shadewright_remediation_concurrency_limit` | gauge | none | `maxConcurrentRemediations` in force at the last check of the limits. 0 until the first check |
| `shadewright_remediation_waits_total` | counter | `reason`: `target_locked`, `concurrency_limit`, `circuit_open`, `circuit_half_open`, `limits_unknown` | Times a request that passed every check was held back `Queued` before the decision, by reason; a request is counted again at each 30 s recheck while it waits. `limits_unknown` is a check of the limits that failed and is retried with backoff (fail closed) |
| `shadewright_circuit_breaker_open` | gauge | none | 1 while the circuit breaker of [design 10, item 7](../design.md#10-security) is open or half-open (it closes only on a success), 0 while it is closed, as the last check of the limits found it. The gauge does not tell open from half-open: `shadewright_remediation_waits_total{reason="circuit_half_open"}` rises only while requests wait behind a running probe, so a half-open breaker that no request has tried yet looks like an open one |
| `shadewright_circuit_breaker_trips_total` | counter | none | Times this process found the breaker open after it had found it closed or half-open (or at its first check), so a probe that did not succeed is a trip. Starts again at zero when the Pod restarts |

## Analyzer

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `shadewright_analyzer_watched_namespaces` | gauge | none | Namespaces the analyzer watches (`--watch-namespaces`), set once at start on every replica. `0` is valid (the chart's default) and means it reads no events or pods and records no Incident: alert on it if you expect Incidents |
| `shadewright_incidents` | gauge | `phase`: an [Incident phase](api.md), `None` or `Other` | Incidents in the operator namespace, by phase. Reported by the leader only; missing (not zero) before its informer has synced or when the read fails |
| `shadewright_incidents_analysis_due` | gauge | none | Incidents whose `AnalysisDue` condition is `True`. Reported with the gauge above |
| `shadewright_analyses_used_today` | gauge | none | Analyses counted against the current UTC day in the budget ConfigMap, as the last pass of the analysis trigger read it. Missing while the budget is unknown: before the first pass, and after a pass that could not read the settings or could not read or trust the ConfigMap (the trigger then marks nothing due). On a replica that lost leadership it keeps the value of its last pass |
| `shadewright_analyses_per_day_limit` | gauge | none | The daily limit of the RemediationSettings (`spec.analysis.maxAnalysesPerDay`, or the deprecated `spec.maxAnalysesPerDay` when set and lower), set by every pass that read the settings and found a limit, even when the ConfigMap then fails. Missing while the settings are unknown |
| `shadewright_analysis_reservations_total` | counter | `result`: `reserved`, `budget_spent`, `budget_not_found`, `budget_unreadable`, `error` | Attempts to take a unit of the daily analysis budget before a model call. Only `reserved` lets the model be called. Nothing reserves units yet: the model-backed analysis is [#16](https://github.com/pkalin/shadewright/issues/16) |
| `shadewright_analysis_trigger_errors_total` | counter | none | Passes of the analysis trigger that failed (an unreadable settings object or budget, a failed write of `AnalysisDue`); each is retried at the next interval |
| `shadewright_incident_write_errors_total` | counter | none | Writes of an Incident's status that failed and are retried |
| `shadewright_incident_batches_dropped_total` | counter | `reason`: `invalid`, `fingerprint_conflict`, `stopped` | Batches of observations not stored: refused by the API server as invalid, an Incident of the expected name holds another fingerprint, or still unwritten when the process stopped |
| `shadewright_incident_observations_refused_total` | counter | none | Observations of a new fingerprint not stored because the cap on Incidents (`--max-incidents`) was reached |
| `shadewright_incident_occurrences_overflow_total` | counter | none | Observations of a new occurrence not counted because the caps on occurrences held unwritten (per Incident, per namespace share, total) were reached |

## controller-runtime metrics

controller-runtime adds, among others, `controller_runtime_reconcile_total`, `controller_runtime_reconcile_errors_total`, `controller_runtime_reconcile_time_seconds`, `workqueue_*` (labelled by controller name), `rest_client_requests_total` and `leader_election_master_status`, plus the Go and process collectors. See the [controller-runtime metrics reference](https://book.kubebuilder.io/reference/metrics-reference).

## Useful queries

```promql
# Requests waiting for a person
sum(shadewright_remediation_requests{phase=~"NeedsIntervention|AwaitingApproval"})

# Denials by reason over the last day
sum by (reason) (increase(shadewright_policy_denials_total[1d]))

# An analyzer that watches no namespace, so it records no Incident
max(shadewright_analyzer_watched_namespaces) == 0

# Share of today's analysis budget used
max(shadewright_analyses_used_today) / max(shadewright_analyses_per_day_limit)
```
