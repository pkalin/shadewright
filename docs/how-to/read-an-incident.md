---
title: Read an Incident
---

# Read an Incident

An `Incident` is the record of one repeated symptom on one workload. Reading it tells you what Shadewright saw and whether it is due for analysis. Background: [design 3.2](../design/3-observation-and-deduplication.md#32-two-levels-of-deduplication), [ADR-0007](../adr/0007-incident-state-in-crds.md) and the [Incident fields](../reference/api.md#incident).

## List them

```bash
kubectl -n shadewright-system get incidents
```

The columns are Phase, Kind and Subject (the workload the symptom is correlated to), Reason (such as `OOMKilled` or `FailedScheduling`), Count and Last seen. Names look like `incident-<64 hex digits>`, from the fingerprint, which hashes the cluster ID, namespace, owner, reason and normalized symptom. Repeated events raise `Count` on the same Incident instead of creating another.

## Read one

```bash
kubectl -n shadewright-system get incident <name> -o yaml
```

| Where | Meaning |
| --- | --- |
| `spec.subject`, `spec.reason`, `spec.type` | What the symptom is about. Immutable: a different symptom is a new Incident |
| `spec.symptom` | The normalized, redacted description (at most 1024 characters), with names, UIDs, IPs and times replaced by placeholders |
| `status.firstSeen`, `status.lastSeen`, `status.count` | How long and how often. The count saturates at 1,000,000 |
| `status.phase` | Today always `Open`. `Analyzing`, `Proposed`, `Escalated` and `Resolved` exist in the API but nothing sets them yet (<span class="sw-status sw-planned">planned</span>) |
| `status.conditions`, type `AnalysisDue` | Whether the Incident is due for analysis, and why or why not (`FirstAnalysis`, `Debouncing`, `CoolingDown`, `AnalysisDisabled`, `DailyBudgetReached`, ...). The reasons are listed in [troubleshooting](../troubleshooting.md#not-request-reasons) |
| `status.remediationRequests` | Names of requests made for this Incident. Always empty today: nothing writes it yet (<span class="sw-status sw-planned">planned</span>), not even for a request written by hand |

The analysis that calls a model is not built, so an Incident being "due" starts no model call yet. An Incident holds no logs and no raw event text.

## When there are none

See [no Incident appears](../troubleshooting.md#no-incident-appears) and [limit the namespaces it watches](limit-watched-namespaces.md).

!!! note "Not yet run"
    The two `kubectl` commands are not yet run for this guide. Columns and the phase behaviour come from the API types and `internal/incidents`.
