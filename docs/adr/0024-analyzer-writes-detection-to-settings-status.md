# ADR-0024: The analyzer writes platform and integration detection to the RemediationSettings status

- Status: Accepted
- Date: 2026-11-03

## Context

[ADR-0018](0018-three-axes-of-pluggability.md) says the operator reports in status which modules it detected and which are active, and `RemediationSettings` has the fields for it since [#60](https://github.com/pkalin/shadewright/issues/60): `status.platform` (`detected`, `active`) and `status.integrations` (design 5.4). Detection needs the nodes and the discovery API, and runs in the analyzer, which builds the module registry ([ADR-0019](0019-extension-packaging.md)); the executor does not need detection.

AGENTS.md invariant 6 kept the analyzer's writes to the operator namespace: Incidents, its analysis budget ConfigMap and the requests it proposes, besides its own Lease and Events. `RemediationSettings` is cluster-scoped, so no identity could write its status, and the report could not be written at all. The cluster admin needs it: a cluster on AKS without `aks` enabled has no AKS diagnosis, and only the report (`detected: aks, active: generic`) says why.

Options were to give the analyzer a status write, to give it to the executor (which then needs node and discovery reads it otherwise does not have), to write the report to a ConfigMap or an Incident-like object in the operator namespace, or to only log and expose metrics.

The maintainer decided on 2026-11-03 ([#61](https://github.com/pkalin/shadewright/issues/61)) that the analyzer writes the report to the settings status.

## Decision

1. The analyzer may `update` the `remediationsettings/status` subresource, through its cluster-wide ClusterRole. No other verb on the subresource (no `patch`, no `get`), and nothing on the object itself beyond the `get`, `list` and `watch` it already has. There is no `resourceNames`: the object is cluster-scoped and the schema pins its name to `default`, so there is no other object to protect.
2. AGENTS.md invariant 6 is amended to list this write: the analyzer's only write outside the operator namespace.
3. The status is a report, never an input. No component (the analyzer, the executor, a module, any later controller) reads any field of the `RemediationSettings` status to decide anything: not `status.platform`, `status.integrations`, `status.observedGeneration` or any condition. Which modules are active is worked out again on every run from `spec.modules` and live detection. A module contributes hooks only when it is enabled and detected, and detection never enables one (ADR-0018, amended 2026-11-03). Decision state of other components, such as the health of the signer or the state of a circuit breaker, must never live in the settings status: the analyzer can write the whole status, so anything there may be forged by it.
4. The analyzer writes `status.platform`, `status.integrations`, `status.observedGeneration` and its own conditions `UnknownModules` (True when `spec.modules` names a module the binary does not have), `DetectionFailed` (True when the last run could not list the nodes or read discovery; the report is then the last one that succeeded, or only `generic` active when none has) and `NoWatchedNamespaces` (True when the analyzer was started with no namespace to watch), and keeps conditions of other types as they are. Each write reads the object from the API server and is retried on a conflict.
5. For detection the analyzer may `list` Nodes cluster-wide: no `get` or `watch`, so it starts no informer on them, and it keeps only the provider ID and the label keys. The discovery API needs no grant.

## Consequences

- The cluster admin sees in `kubectl get remediationsettings default -o yaml` what was detected, what is active and which enabled names match nothing.
- The analyzer gains its first write on a cluster-scoped object. A compromised analyzer can write any status that the schema accepts: a false report, or conditions that look like another component's. Since no component reads any field of the status to decide (decision 3), that misleads a human reader but enables no module, tool or action. The threat model lists it.
- The status update replaces the whole status. A later writer of the same status must also read fresh and retry on a conflict, and each writer owns its own fields and condition types. What it writes there is a report for people too, never state another component acts on (decision 3).
- `test/rbac` checks the grant against a real API server: `update` on the subresource is allowed, `patch` on it and any write on the spec are refused, and nodes can be listed but not read one by one or watched.

## Alternatives considered

- **The executor writes the status.** It would need node and discovery reads only for this, and the executor is the identity with Git write access, which should stay as narrow as possible.
- **A ConfigMap or a report object in the operator namespace.** Keeps invariant 6 as it was, but splits settings and their report across two objects, and the admin would look for the report in the wrong place. A ConfigMap also has no schema to bound what is written.
- **Logs and metrics only.** No new grant, but nothing durable that `kubectl` shows next to the settings, and the reason a module is not active would be hard to find.
- **A separate report object, or a status-only CRD, written only by the analyzer.** It would keep the analyzer's write off the settings object and let each report have its own owner. Not chosen: it is one more CRD to install, version and document, the admin would read the report away from the settings it explains, and it would not make the report trusted: whoever writes it (the analyzer) is the identity whose compromise matters, so decision 3 would still be needed.
- **Server-side apply with field ownership.** Each writer would own its fields through `managedFields`, and a writer could not clear another's fields by accident. Not chosen: it needs the `patch` verb, which also allows a write with no resourceVersion, and field ownership is bookkeeping, not access control: a writer with `patch` can force-take any field. Today there is one writer; `update` with a fresh read and a conflict retry is enough, and the analyzer already sets a field manager so a later move to apply is possible.
- **A `patch` grant instead of `update`.** A patch needs no resourceVersion and can race silently with another writer; `update` with the version read makes a conflict visible and retried.

## Amendments

- 2026-11-04: decision 4 lists a third condition, `NoWatchedNamespaces` (reasons `NoNamespacesWatched` and `NamespacesWatched`), set from the count of `--watch-namespaces` the binary was started with, so an analyzer that watches nothing (the Helm chart's default) says so next to the settings ([#307](https://github.com/pkalin/shadewright/issues/307), design 5.4). It is a report only, read by nothing, written with the same `update` of the status subresource; no grant changes and invariant 6 holds. The decision is unchanged.
- 2026-11-07: decision 4 lists a fourth condition, `SymptomSourcesNotWatched` (reasons `SourcesNotWatched`, `AllSourcesWatched`, and `SourcesNotChecked` while Unknown), which names the symptom source kinds of active modules that the collector does not watch and why: not served, not granted (the module's chart value is off) or failed ([#409](https://github.com/pkalin/shadewright/issues/409), design 5.4). Detection now also runs when the watch state of such a kind changes, besides its other triggers, so the report follows the change without waiting 10 minutes. Like the others it is a report only, read by nothing; the extra runs write with the same `update` of the status subresource, and no grant changes, so invariant 6 holds. The decision is unchanged.
