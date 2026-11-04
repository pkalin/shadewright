---
title: Metrics reference
---

# Metrics reference

Both components serve Prometheus metrics on their metrics endpoint (`:8443` over HTTPS with `config/default`; who may scrape it and how is in the [configuration reference](configuration.md#metrics-endpoint)). Next to controller-runtime's own metrics (below), Shadewright adds the metrics on this page, all named `shadewright_*` and registered on controller-runtime's registry.

Every label takes its values from a closed set listed here, never from a name, namespace, UID, fingerprint or message in the cluster, so the number of series stays fixed whatever the cluster holds. A value a component does not know (a phase written by a newer version, a reason outside the list) is reported as `Other`. Tests in `internal/controller/metrics_test.go` and `internal/incidents/metrics_test.go` fail when a metric has a label value outside its set or is missing from this page.

The phase gauges are computed when the endpoint is scraped, from the component's informer cache, so a scrape makes no API call. Every replica reports them, the standby ones included, so aggregate them with `max`, not `sum`. Counters are reported by the replica that did the work (the leader) and start again at zero when a Pod restarts: use `rate` or `increase`. Counters with a closed set of labels report every value from the start, at zero.

## Executor

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `shadewright_remediation_requests` | gauge | `phase`: a [RemediationRequest phase](api.md), `None` (no phase yet) or `Other` | RemediationRequests in the operator namespace, by phase. Missing (not zero) when the cache cannot be read within 5 s |
| `shadewright_policy_decisions_total` | counter | `outcome`: `allowed`, `denied`, `needs_intervention` | Decisions of the policy check, each counted once, after the status that records it was written |
| `shadewright_policy_denials_total` | counter | `reason`: the `PolicyAllowed` reason of a `Denied` request, or `Other` | Requests the policy check denied, by reason. The reasons are those of the policy check (`PolicyNotFound`, `SettingsNotFound`, `TargetMismatch`, `PreviousValueMismatch`), of the policy rules (for example `AboveMaximum`, `KindNotAllowed`, `ModuleNotEnabled`) and of the source mapping (for example `RepositoryNotInPolicy`, `BranchMismatch`). Created on the first denial with that reason |
| `shadewright_remediation_reconcile_errors_total` | counter | `kind`: `timeout`, `conflict`, `cancelled`, `other` | Errors the RemediationRequest reconciler returned, each retried with backoff. `timeout` is a step that reached its own deadline, `conflict` a status write made from a stale read (harmless when it is rare), `cancelled` a reconcile stopped by shutdown or a lost leadership. controller-runtime's `controller_runtime_reconcile_errors_total{controller="remediationrequest"}` counts the same errors without a kind |

## Analyzer

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `shadewright_incidents` | gauge | `phase`: an [Incident phase](api.md), `None` or `Other` | Incidents in the operator namespace, by phase. Missing (not zero) when the cache cannot be read within 5 s |
| `shadewright_incidents_analysis_due` | gauge | none | Incidents whose `AnalysisDue` condition is `True` |
| `shadewright_analyses_used_today` | gauge | none | Analyses counted against the current UTC day in the budget ConfigMap, as the last pass of the analysis trigger read it. Set only when the settings and the budget could be read and trusted; on a replica that lost leadership it keeps its last value |
| `shadewright_analyses_per_day_limit` | gauge | none | `maxAnalysesPerDay` of the RemediationSettings, read with the gauge above |
| `shadewright_analysis_reservations_total` | counter | `result`: `reserved`, `budget_spent`, `budget_not_found`, `budget_unreadable`, `error` | Attempts to take a unit of the daily analysis budget before a model call. Only `reserved` lets the model be called. Nothing reserves units yet: the model-backed analysis is [#16](https://github.com/pkalin/shadewright/issues/16) |
| `shadewright_analysis_trigger_errors_total` | counter | none | Passes of the analysis trigger that failed (an unreadable settings object or budget, a failed write of `AnalysisDue`); each is retried at the next interval |
| `shadewright_incident_write_errors_total` | counter | none | Writes of an Incident's status that failed and are retried |
| `shadewright_incident_batches_dropped_total` | counter | `reason`: `invalid`, `fingerprint_conflict`, `stopped` | Batches of observations not stored: refused by the API server as invalid, an Incident of the expected name holds another fingerprint, or still unwritten when the process stopped |
| `shadewright_incident_observations_refused_total` | counter | none | Observations of a new fingerprint not stored because the cap on Incidents (`--max-incidents`) was reached |
| `shadewright_incident_occurrences_overflow_total` | counter | none | Observations of a new occurrence not counted because the caps on occurrences held unwritten were reached |

## controller-runtime metrics

controller-runtime adds, among others, `controller_runtime_reconcile_total`, `controller_runtime_reconcile_errors_total`, `controller_runtime_reconcile_time_seconds`, `workqueue_*` (labelled by controller name), `rest_client_requests_total` and `leader_election_master_status`, plus the Go and process collectors. See the [controller-runtime metrics reference](https://book.kubebuilder.io/reference/metrics-reference).

## Useful queries

```promql
# Requests waiting for a person
max(shadewright_remediation_requests{phase=~"NeedsIntervention|AwaitingApproval"})

# Denials by reason over the last day
sum by (reason) (increase(shadewright_policy_denials_total[1d]))

# Share of today's analysis budget used
max(shadewright_analyses_used_today) / max(shadewright_analyses_per_day_limit)
```
