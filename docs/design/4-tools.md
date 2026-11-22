# 4. Tools

This section covers the read tools the analyzer offers to the model, and the adapters behind them. The Prometheus adapter for `query_metrics` and the pod logs adapter for `get_pod_logs` are built and given to the context builder ([#371](https://github.com/pkalin/shadewright/issues/371)) and the read tools. The read tools are <span class="sw-status sw-built">built</span> in `internal/tools` ([#74](https://github.com/pkalin/shadewright/issues/74)), on the same sources, and offered to the escalation model by the analysis loop, which the analyzer starts only with `--triage-model` ([#76](https://github.com/pkalin/shadewright/issues/76)); every call is bound to the namespace of the Incident's subject, and `get_delivery_status` is not offered.

The analyzer exposes tools to the model through its own adapters.

- In the MVP loop they are offered only when an analysis escalates (3.3, 11.2). The first call runs on the prepared context alone.
- Every tool result goes through `internal/redact` before the model sees it.
- MCP is optional: it can expose the same tools later, but it is not required by the controller and it does not replace Kubernetes RBAC ([ADR-0003](../adr/0003-gitops-only-typed-remediation.md)).

| Tool | Purpose | Mode | Status |
| --- | --- | --- | --- |
| `get_resource`, `list_events` | Object state and symptom history: a Pod, Deployment, StatefulSet or ReplicaSet | Read, redacted | <span class="sw-status sw-built">built</span>, offered on escalation ([#76](https://github.com/pkalin/shadewright/issues/76)) |
| `get_pod_logs` | Bounded time range and byte count | Read, redacted | tool <span class="sw-status sw-built">built</span>; adapter <span class="sw-status sw-built">built</span>, given to the context builder ([#371](https://github.com/pkalin/shadewright/issues/371)) |
| `query_metrics` | Approved query templates only, for one pod or one Deployment the tool reads first (the HPA templates are the context builder's only) | Read, redacted | tool <span class="sw-status sw-built">built</span>; adapter <span class="sw-status sw-built">built</span>, given to the context builder ([#371](https://github.com/pkalin/shadewright/issues/371)) |
| `get_delivery_status` | Flux Kustomization / HelmRelease delivery state (suspended, failed, applied or pending), revision and readiness, in a delivery namespace | Read, redacted | tool <span class="sw-status sw-built">built</span>, on the Flux adapter of [#92](https://github.com/pkalin/shadewright/issues/92) when the Flux module's grant is rendered or `--module-grants` is not set ([#562](https://github.com/pkalin/shadewright/issues/562)); not offered by the loop |
| `read_git_file`, `get_recent_changes` | Map an object to its source in Git | Read, redacted | arguments checked, then always "not available" and never offered: the analyzer has no Git identity ([#373](https://github.com/pkalin/shadewright/issues/373)) |
| `check_missing_reference` | Whether a Secret or ConfigMap a pod references is missing (4.3) | Read | <span class="sw-status sw-built">built</span>, offered on escalation ([#76](https://github.com/pkalin/shadewright/issues/76)) |

**Bounds of every tool** (`internal/tools`):

- The arguments are one JSON object of at most 2 KiB with exactly the schema's property names (case-sensitive, no duplicates, no others) and types; each value is then checked: names, enums, ranges, and a kind allow list where a tool names objects. An integer may take any JSON form with a zero fractional part (`30`, `30.0`, `3e1`); a fraction is refused as the wrong type, and an explicit `0` is a value, refused when outside the range, never read as an absent argument. They come from the model and are untrusted.
- A namespace outside `--watch-namespaces` is refused before any read; an empty list refuses every namespace.
- `get_delivery_status` is the exception: Flux keeps its Kustomizations and HelmReleases in its own namespace, not in the workloads'. Its namespace is checked against a separate list of delivery namespaces, empty unless set, so every call is refused until then (fail closed). That list never widens the collector or any other tool's reads. Since [#562](https://github.com/pkalin/shadewright/issues/562) the analyzer sets it to the watched namespaces, and gives the tool a delivery system, by the collector's rule for the Flux module's grant (`collector.Granted`): when `--module-grants` names `flux` (chart value `analyzer.modules.flux.enabled`), or is not given at all (an install without the chart), as the collector then checks and watches Flux's kinds too. The grant is `list` and `watch` on Kustomizations, HelmReleases and GitRepositories, bound in each watched namespace. Reading Flux's own namespace therefore needs it among the watched namespaces, as section 2 says. With `--module-grants` given without `flux`, or with no watched namespace, the tool is not available; where it is on but the analyzer may not list Flux's kinds after all, or Flux's CRDs are not installed, it answers `could not fetch`, never a state or `not found`.
- `get_delivery_status` reads through the Flux adapter of the executor (`internal/providers/delivery/flux`, its status read without the commit comparison), so both components read a unit's status the same way. The analyzer has no `get` on Flux objects: it reads the one unit, and the GitRepository of a Kustomization, with `list` and a field selector on the name (`analyzer.ListGetter` on the API reader), so it needs no new permission and starts no informer. The answer's first line, `delivery: suspended`, `delivery: failed`, `delivery: applied` or `delivery: pending`, is a fixed word written by code, by the rules of the executor's source mapping and `kubectl shadewright doctor` (`sourcemap.Suspended`, `sourcemap.CompareSource`, ADR-0012): suspended when `spec.suspend` is set, whatever the status says; failed when the unit reports a failure of its current generation; applied when it is Ready for its current generation and, for a Kustomization of a GitRepository, has applied the revision that GitRepository last fetched (compared only while the GitRepository passes the mapping's source rules and is Ready, as `doctor` does); pending otherwise (not reconciled yet, behind its source, or a GitRepository the mapping refuses or that is not Ready, so the comparison could not be made and the executor would stop before it). A GitRepository that cannot be read, and a list that fails (only a successful empty list means `not found`), give `could not fetch`, never a state without the comparison. It is not evidence a quote may cite, and since every field comes from the unit's own status it is a reading for the diagnosis, never a verification (the executor's delivery check compares the merge commit, 7.3). The applied revision and the message follow as redacted values; the adapter redacts the whole message before it bounds it to 1 KiB, so the cut never leaves the prefix of a secret that no rule recognises.
- The reads of one call share a timeout (30 seconds by default, at most 2 minutes). A read that runs out of it gives "could not fetch", labelled timed out; a call whose caller cancelled returns an error instead.
- An answer redaction withholds whole, because the work of its decision ran out of its budget, the text is longer than 4 MiB, or the answer holds more pieces than its bytes allow (the reasons `work`, `size` and `heap` of `shadewright_redaction_withheld_total`) ([#873](https://github.com/pkalin/shadewright/issues/873), [10.3](10-security.md#103-redaction)), gives "could not fetch: withheld by redaction", labelled `withheld by redaction`, with no values: nothing of the answer reaches the model. This holds for every read tool, `get_pod_logs` included (the log source returns the excerpt as withheld, or redaction withholds a value of its header, the pod UID or the container ID), and for a delivery message the adapter's redaction withheld. The budget is the bound on every shape (1,024 steps per byte plus a fixed amount): the known expensive shapes listed in [10.3](10-security.md#103-redaction), a flag and a comma repeated (`--token,}`, quadratic until [#962](https://github.com/pkalin/shadewright/issues/962)) or quoted pairs glued on one line, are withheld from 2,718 to 99,720 bytes by shape and entry point (`TestBudgetWithholdSizes`), so a tenant can make its own text unreadable to the analyzer; the analysis then escalates as for any failed source, within the daily analysis budget.
- Each result has a byte bound (2 to 17 KiB by tool) and is cut in whole lines of redacted text; a log excerpt keeps its newest lines. `get_resource` is not cut in lines: to stay valid JSON within its bound it leaves out whole values, then map keys, each replaced by a note of its size (`(N bytes left out)`), and is labelled "cut to the result bound".
- A tool result is decided once, on the text the tool returns, and is not redacted again ([#845](https://github.com/pkalin/shadewright/issues/845), sixth decision of 2026-11-18). Each read tool writes its answer as pieces: the text it writes (a header line, a field name, JSON punctuation) and each value from a source, written as the answer writes it: on a line (`list_events`, `check_missing_reference`, `get_delivery_status`, `query_metrics`, where a line break or a tab becomes a space) or as the content of a JSON string (`get_resource`, whose answer is the object's JSON). Every other control character (Unicode category Cc) is left out in both, and the decision reads the value and the answer as written that way, so the model is shown no control and a key or token that a removed control joins is hidden ([11](11-model.md#the-modelprovider-interface), [#852](https://github.com/pkalin/shadewright/issues/852)). `redact.Pieces` hides the union of three readings of those bytes (ninth decision of 2026-11-18): each piece alone, the answer as the tool writes it (where a line break inside a value ends a match as it ends a line for that value alone, tenth decision of 2026-11-18, so a secret on one line of a value does not take the lines after it), and the reading of the release before (thirteenth decision of 2026-11-18): the redactor of `origin/main` before #845, kept as `internal/redact/released` (rewritten by #873 so that its work is linear on the staircases and glued fields that grew faster before, `--token,}` and `-password,` repeated aside until #962, which the work budget withholds instead, and compared with the unchanged copy of the release's code in `released/testdata/v052` by tables and by a fuzz target), applied as that release's tools applied it (each value redacted, made valid UTF-8 and redacted again, written as the tool wrote it, then the whole answer twice), each of its spans traced back to the bytes it stands for. For the first two readings a line break inside a value ends a line, however the answer writes it; the third reads each text as main's passes read it, so everything main hid at a tool is hidden because main's own code says so. The tests compare the decision with main on the text the tools return, in the encoding they return it in (twelfth decision of 2026-11-18): nothing main hid at a tool shows ([10.3](10-security.md#103-redaction)). The readings only find spans and nothing is replaced until all three have run, so a rule that hides a map key never takes away the word in it that hides the value under it. The answer is the decided pieces joined. So a key in one field and its value in a sibling or the next field are seen together (an HTTP header's name and value, a flag at the end of a container's command and its value in the arguments), and so is a pattern that exists only in the returned line (`Warning Bearer Pod/<name>`). A value is one value, as the object has it (fifth decision of 2026-11-18): a value under a map key, a field name or a label name that is a secret key (`redact.SecretKey`: the whole name, read by main's rule for a quoted key, so `auth`, `pass` and `DB_PASS` count whatever the value looks like) is hidden whole, line breaks, blanks and markers included, and nothing beyond it; so are the `value` and `values` fields of an object whose `key` or `name` is a secret key (a selector's requirement, an HTTP header). A key found inside a value hides as far as main hid at that tool: on a line of `list_events` the rest of a note after the line of the key stays; in JSON a `\n` does not end a value, the next blank does. Two keys of a map that are decided alike stay two entries (the later one gets ` (2)`, ` (3)` and so on, one counter per decided key, so renaming is linear), so a map keeps as many entries as the object has. `get_resource` encodes the object once and walks the JSON tokens of that output in one pass (seventh decision of 2026-11-18): every string token that holds a value is a piece in its place, with its key from the tokens around it, and a type that writes its own JSON (an int-or-string, a quantity, a time) is tokens like any other, so a probe port written as `--password V` hides `V` as on main. A map key is decided like a value (eighth decision of 2026-11-18), and the reading of the release before reads it in the answer as main did: a key with a connection string, a line break and more text is hidden to its end. The answer of `get_resource` is cut at 16 KiB like main's, and its replacements are fitted on the text after them: when the decided answer, with the map keys renamed after the decision counted, is longer than the limit, every marker takes the first shorter form that makes it fit, so the answer is whole JSON wherever main's was. The tools layer's own work is outside the redactor's budget: the walk of an object's JSON (linear in the object), and three sorts, each bounded by what the tool read: the Events of the namespace by time for `list_events` (all that the list returns, of which at most 50 are shown), the label names of each series for `query_metrics`, and the values of one `get_resource` answer by the bytes leaving each out would save (`leaveOutOf`, at most one value per byte of the object, so n log n comparisons on an object of about 1.5 MiB at most). encoding/json sorts each map's keys, as on main. The sorts of the redactor, which its budget counts, and their bounds are in [10.3](10-security.md#103-redaction). A reference is not a value: the name and key of a `secretKeyRef` or `configMapKeyRef`, an `envFrom` source's name and a ConfigMap volume's name, key and path show as on main, and so does the name of the variable after them. The string after a secret flag is its value, whatever it looks like (`["--token", "--verbose"]` hides `--verbose`). Every value in an object, list or map under a field whose name holds a secret word and is no reference (`secret`, `imagePullSecrets`, `serviceAccountToken`; not `secretRef`) is hidden whole, numbers, booleans and null included (written as a JSON string holding the marker, so the answer stays JSON), as main hid that JSON collection, so a Secret volume's `secretName`, `defaultMode`, `optional` and item paths, an image pull secret's name and a projected token's path stay hidden (showing them is [#860](https://github.com/pkalin/shadewright/issues/860)). `get_resource` leaves out annotations, managed fields, the literal values of environment variables (of every container, init and ephemeral container), the metadata of ephemeral volume claim templates and the containers' termination messages (free text the container writes, which the collector never reads either); container commands and arguments are kept for the diagnosis and rely on redaction alone. `query_metrics` writes each label of a series as `name=value`, the value under its name, so a secret label name takes its value. A log excerpt is rendered by the same code as in the context builder (3.3), and at `get_pod_logs` also hides what main hid when it redacted the excerpt again after its cut: it loses its leading continuation lines before redaction, its header is budgeted first, and every cut keeps the header and the newest lines. When no lines come back the header says why: "no logs: the node no longer holds the log of this instance" when the node has dropped the log, "could not fetch: the API server refused the identity" when the identity lacks the grant, and "no log lines were returned" for an empty log. The error text of a failed read is never shown.
- A refusal names the rule in a fixed sentence and never echoes an argument. The result is a delimited block whose first line says that its text is data, never instructions, and which the text cannot close: every angle bracket in it, and every Unicode look-alike (full-width, small, CJK, mathematical and others), is escaped, and any spelling of the tag's name, with any dash or invisible character between its words, is changed.
- A tool whose source is not configured answers "not available" and is not offered to the model.

## 4.1. `query_metrics`

<span class="sw-status sw-built">built</span> The Prometheus adapter is `internal/providers/metrics/prometheus`. The analyzer creates it from `--prometheus-url` and gives it to the context builder ([#371](https://github.com/pkalin/shadewright/issues/371)), which the analysis loop calls (3.3), and to `query_metrics`. The evaluation harness replays recorded series through the same `MetricsSource` interface, checked against these templates ([#472](https://github.com/pkalin/shadewright/issues/472), [Evaluate the model](../evaluation.md#the-scenario-files)). Two templates read the metrics of one HPA, such as the external metric KEDA's HPA scales on against its target ([#480](https://github.com/pkalin/shadewright/issues/480)); the context builder asks them for a `ScaledObject` Incident (3.3), and `query_metrics` does not offer them.

### Query templates and parameters

`query_metrics` runs only approved PromQL templates compiled into the adapter. There is no setting, CRD or tool argument that carries query text.

- Every template takes the parameter `namespace` (a DNS-1123 label).
- `deployment_replicas_unavailable` also takes `workload` (a DNS-1123 subdomain). It matches the Deployment by its label and takes any valid name.
- `hpa_metric_current` and `hpa_metric_target` also take `hpa` (a DNS-1123 subdomain, the HPA's name). They match the HPA by kube-state-metrics' `horizontalpodautoscaler` label with one exact matcher, so an HPA whose name only starts like it is never selected. The context builder takes the name from the `ScaledObject`'s `status.hpaName` (3.3).
- The other templates, the pod templates, take the exact pods of the target instead.
- Any other value or parameter is refused before a request is made.

| Template | Result per series | Source |
| --- | --- | --- |
| `container_memory_working_set_bytes` | memory working set of each container, bytes | cAdvisor |
| `container_memory_limit_bytes` | memory limit of each container, bytes | kube-state-metrics |
| `container_cpu_usage_cores` | CPU used by each container, cores, 5-minute rate | cAdvisor |
| `container_cpu_throttled_ratio` | share of CFS periods throttled, 0 to 1, over 5 minutes | cAdvisor |
| `container_restarts_total` | restart count of each container | kube-state-metrics |
| `container_last_terminated_oomkilled` | 1 when the container's last termination was OOMKilled | kube-state-metrics |
| `container_waiting_reason` | 1 per waiting container, with the reason (CrashLoopBackOff, ImagePullBackOff, ...) | kube-state-metrics |
| `deployment_replicas_unavailable` | unavailable replicas of the Deployment named `workload` | kube-state-metrics |
| `hpa_metric_current` | current value of each metric of the HPA named `hpa`, from its status, per `metric_name` and `metric_target_type` (`value`, `average` per pod, or `utilization` in percent of the request) | kube-state-metrics (`kube_horizontalpodautoscaler_status_target_metric`) |
| `hpa_metric_target` | target of each metric of the HPA named `hpa`, from its spec, in the same units | kube-state-metrics (`kube_horizontalpodautoscaler_spec_target_metric`) |

### Namespace scope

Prometheus applies no authorization of its own: whoever can reach it can query every namespace it scrapes. So the adapter enforces the namespaces itself.

- It is given the analyzer's watched namespaces (`--watch-namespaces`, the same list its RBAC grants, section 10.1).
- A query naming any other namespace is refused before a request, with an error that does not repeat the name. An empty list refuses every query.
- The analyzer passes it when it creates the adapter from `--prometheus-url` ([#371](https://github.com/pkalin/shadewright/issues/371)), and refuses to start with more than 256 watched namespaces.
- When it is created, the adapter also refuses a namespace in that list that is not a DNS-1123 label, more than 256 namespaces and a timeout over 2 minutes.

### Which pods a query names

A pod template selects exactly the pods its caller names, never pods whose names only start like the target's. A name prefix would let a neighbour supply the target's evidence (the StatefulSet pod `web-v2-0` next to the Deployment `web`, or a standalone pod named like one of its own).

**Who decides which pods belong to the target.** The adapter has no Kubernetes client, so it does not decide this, and it cannot check that the pods are in the namespace the query names. The context builder (3.3, built, not called yet) resolves them in that same namespace. The context builder's pods never come from the model. `query_metrics` as a tool is different: the model names a pod, which the tool reads first and which must exist in a watched namespace, so its answer can be about any pod there, outside the builder's attribution (3.3); treat it as evidence about that pod only.

- It starts from the target's kind and UID and follows controller owner references (Deployment to ReplicaSet to Pod, StatefulSet to Pod, ReplicaSet to Pod).
- It uses the analyzer's existing reads: a get of each workload from the API server and the cached list of the namespace's pods.
- Every hop must exist with the referenced UID and select the object below it.
- A Deployment's ReplicaSets are read at most 16: those named like the Deployment's first, then those named by the most pods, so that a few pods naming made-up ReplicaSets cannot crowd them out. Enough such pods can, and the pod set is then labelled capped.
- The selector matches the namespace and the pods together, so a pod of the same name in another namespace is never selected.
- The analyzer has no grant on DaemonSets (3.1), so their pods are not attributed unless a later change adds that grant.

**Uncertain attribution.** Live reads give only the current pods of the target: deleted pods are gone, and an Incident keeps only a hash of a pod UID. So the attribution is marked uncertain when any of those pods started inside the time asked for or the workload rolled within it. The pods of an old ReplicaSet that is gone are not in the set.

- Decided with #70: the builder keeps the current pods with that mark and does not read historical membership (such as `kube_pod_owner` matched by owner UID).
- It counts the workload as rolled when its current pods belong to more than one ReplicaSet, when one of those ReplicaSets was created inside the window, or when a StatefulSet's current and update revisions differ or its pods carry two revisions.
- An uncertain attribution is a label on the evidence, not a reason to escalate (3.3).

### Pod names in a query

- The adapter takes 1 to 50 distinct names (duplicates are dropped first; the result cap of 50 series could never hold more).
- Each name is a DNS-1123 subdomain of at most 63 characters. The pods of a Deployment are named through `generateName`, which the API server cuts to 58 characters before it adds five random ones, and a StatefulSet name is in practice at most 52 characters.
- Anything else is refused before a request, with an error that does not repeat a name. A template that does not select pods refuses a pod set.
- A pod template returns a series per container, so a set within the cap can still give more than 50 series: that answer is refused as too large.
- A target with more pods than fit is queried in split sets, or its evidence is marked as capped. Neither is a reason to escalate (3.3).
- The names are escaped and joined into one anchored regular expression (`pod=~"^(a|b)$"`). A response with a series of any other pod, or without a pod label, is refused.
- The query is sent in the URL, which the largest pod set keeps within 32 KiB. Prometheus accepts that; a proxy in front of it may need a larger limit.

### Source profile

Source profile ([#333](https://github.com/pkalin/shadewright/issues/333), [configuration reference](../reference/configuration.md#metrics-source-profile-prometheus)): the supported source holds the series of one cluster, like the lab's Prometheus ([ADR-0017](../adr/0017-prometheus-in-the-lab.md)).

**The `cluster` label.**

- Every template keeps the `cluster` label apart and the adapter drops it from the result.
- Without the adapter's cluster setting, an answer with two series of one container that differ only in `cluster` is refused as an invalid response, never mixed. That covers two clusters with the same namespace, pod and container, and a source that labels only some of its series.
- A shared backend (Thanos, Mimir, Grafana Cloud, Azure Monitor managed Prometheus; all untested) needs the cluster setting, which adds `cluster="<value>"` to every selector of every template.
- The label must be stored on the series (by the scrape or by remote write), not an external label the queried server adds only on its way out. A mistyped value or a label the series do not carry leaves every result empty. So the analyzer checks at startup and every 10 minutes, with one fixed query that returns no label, that kube-state-metrics and cAdvisor both hold series with the value; when they do not, it keeps running and metrics are unavailable (every query fails, which escalates, 3.3) until a check finds them ([#371](https://github.com/pkalin/shadewright/issues/371), [configuration reference](../reference/configuration.md#metrics-source-profile-prometheus)).
- The value is 1 to 100 letters, digits, `.`, `_`, `:`, `/` or `-`, quoted like a parameter. The administrator sets it (`--prometheus-cluster`), never a query, the model or a CR, and the adapter refuses anything else when it is created.

**Where the namespace scope holds.** It holds only if the `namespace`, `pod`, `container` and `cluster` labels of cAdvisor and kube-state-metrics series are the scrape's. A job that scrapes tenant pods must not honour their labels (`honor_labels: false`), or must drop the `container_*` and `kube_*` metric names from what it keeps. Otherwise a pod can expose a series named like a template's metric with another namespace's labels. Metric values stay evidence a tenant can influence.

**A measurement counts once.** Within that profile, two scrape jobs that collect the same cAdvisor or kube-state-metrics series differ only in `job` or `instance`, and every template reduces the series of one container (namespace, pod, container, cluster) before it aggregates. The HPA templates reduce the series of one HPA metric (namespace, HPA, metric name, target type, cluster) to their max.

- Gauges and states take the max over the container, so after a restart a memory value is the larger of the old and the new instance while both have series, never their sum.
- The CPU templates take the max over each container instance (cAdvisor's `id`, its cgroup) and sum the instances, which ran one after the other within the 5-minute window.
- Series without `id` (the kubelet's `/metrics/resource`, or a scrape that drops it) are reduced apart, and the larger of the two estimates is kept. So a container measured both ways is never counted twice, and a ratio such as throttled periods over periods covers each instance once.
- A standalone cAdvisor must be relabeled to the kubelet's labels with the same `id`, or its CPU counts as another instance.

### Limits, fixed in code

**Time and size.**

- A range query covers at most 6 hours with a step of at least 1 minute, so it asks for at most 361 points per series. The step is the default global scrape interval of the prometheus-community chart the lab installs, at which cAdvisor and kube-state-metrics are scraped (only the lab's demo pods are scraped every 15 seconds).
- An instant query takes no start time.
- No time lies more than 7 days back or more than a minute ahead.
- A result holds at most 50 series, 500 points per series, 5,000 points and 1 MiB, and is refused as soon as it exceeds any of them while it is read.
- The response must be `application/json`, and its samples may lie at most 1 second outside the time asked for, with strictly increasing times.
- A series may carry only the labels its template aggregates by, with plain-name values, and a series of a pod template only a pod asked for. A pod template takes at most 50 pods of at most 63 characters each.

**Partial answers.**

- A successful answer that carries warnings (for example a store behind the server that did not answer) is returned marked partial. Infos, and warnings that start with `PromQL info:` (where older Prometheus releases put them), do not mark it.
- A series missing from a partial answer may exist and a total in it may be short, so a caller that concludes anything from absence or from a sum treats it as not knowing. The `query_metrics` result tells the model so with a fixed marker, never the warning text.
- A partial answer is not a reason to escalate (3.3): a pod in a watched namespace can make Prometheus warn, for example by exposing a metric of the same name with another type, so like a truncated log it is input a tenant controls.
- The text of warnings, infos and errors from Prometheus is dropped ([#334](https://github.com/pkalin/shadewright/issues/334)).

**Deadlines and retries.**

- Each query has a deadline: 30 seconds by default, at most 2 minutes.
- The server is sent an evaluation timeout a tenth below the time left. A query that runs out of it (a 503 with `errorType` `timeout`) is treated as final instead of retryable, since an evaluation that hit the timeout usually would again (time waiting in the server's queue counts towards it too).
- The adapter does not retry a query. Its caller, the analysis tool loop (<span class="sw-status sw-built">built</span>, started with `--triage-model`), owns the retry and its bound, in line with the rule that every external call has bounded retries.
- The result still goes through `internal/redact` before the model sees it, like every tool result.

## 4.2. `get_pod_logs`

<span class="sw-status sw-built">built</span> The pod logs adapter is `internal/providers/logs/kube`. The analyzer creates it unless `--pod-logs=false` and gives it to the context builder, which is not called yet ([#371](https://github.com/pkalin/shadewright/issues/371)).

### What a call is

A call names one namespace, pod and container, and optionally the previous, terminated instance of the container.

- Like the Prometheus adapter, it is given the analyzer's watched namespaces and refuses any other, and every call when the list is empty, before a request, with an error that does not repeat the name. RBAC refuses such a read too (the analyzer's `get` on `pods/log` is bound only in the watched namespaces), but the adapter does not rely on it.
- The pod must be a DNS-1123 subdomain and the container a DNS-1123 label, checked before a request.
- When it is created, the adapter refuses a missing client, a namespace that is not a DNS-1123 label, more than 256 namespaces and a timeout over 2 minutes. The analyzer refuses to start when `--watch-namespaces` lists more than 256 and logs are on (or `--prometheus-url` is set), with an error that names the limit ([#371](https://github.com/pkalin/shadewright/issues/371)).

### Limits, fixed in code

**Window and reads.**

- The time window is at most 24 hours (`sinceSeconds`, rounded up). It is 1 hour by default for the current container; for the previous instance there is no default window, since it stopped at some time in the past.
- The first request asks for the last 2,000 lines (`tailLines`) and at most 1 MiB and one byte (`limitBytes`; the extra byte tells a log of exactly 1 MiB from a longer one). The adapter reads no more than that whatever the server sends.
- The API server applies `limitBytes` from the start of the lines it selected, so when they hold more than 1 MiB the newest lines are missing. The adapter then asks once more for fewer lines (those that would fit at the mean length seen, times 0.75). If that read is cut too, the excerpt is marked with its end missing.
- Each read holds at most 1 MiB and one byte in memory.

**The excerpt.**

- It is the newest 64 KiB of the read by default, at most 256 KiB, in whole lines. When older bytes are dropped, the first line goes too unless it starts right after a newline, and the excerpt is marked truncated. When the read was cut, its partial last line is dropped. So a cut never hands half a secret to the redactor.
- Lines the server leaves out because of `tailLines` or the window are not reported.
- Invalid UTF-8 is replaced, and the excerpt is redacted with `internal/redact` (built-in rules; operator patterns are <span class="sw-status sw-planned">planned</span>, nothing configures them yet) inside the adapter, before it leaves it.
- A truncated excerpt, or one with its end missing, is not a reason to escalate (3.3).

**Deadlines.** Logs are never followed. Each call has a deadline (30 seconds by default, at most 2 minutes). A stream that ends with it, or makes no progress for 100 reads in a row, is an error, not an excerpt.

### Which instance answered

A log request names the pod and container by name only: it cannot ask for a pod UID or a container ID, and "previous" means the instance before the one running when the kubelet answers. A StatefulSet pod recreated under the same name, or a restart between the adapter's two requests, would hand over logs of another instance. So:

- The adapter reads the pod (`get` on pods, from the analyzer watch role bound in the watched namespaces only) before the first and after the last log request.
- The excerpt records the pod UID, the container ID of the instance asked for and the restart count from the first check, and the time the reading ended ([#339](https://github.com/pkalin/shadewright/issues/339)).
- Pod UIDs and container IDs are never reused, so equal checks mean no other instance answered either request. The restart count is compared too, as an extra signal only, since it can go back to 0 after a node restart.

**The excerpt is marked uncertain when:**

- the checks differ;
- the second check fails (the pod is gone, the read is refused or fails, or the deadline passes; the logs already read are returned);
- the status names no instance, or an ID over 128 bytes or with unexpected characters;
- for the current container: the status does not say which instance the kubelet serves. While the container waits, the kubelet serves the last terminated instance, and a terminated state without a container ID names none.

A failed first check is an error, not an excerpt.

**What a log request cannot guarantee even then.** The instance is inferred from the pod's status, which is the kubelet's report and can lag the runtime, and the kubelet keeps the log of one previous instance only.

The instance metadata stays with the excerpt, in the evidence, never in Incident status (which keeps only a hash of a pod UID). Like an uncertain metrics attribution, an uncertain excerpt is a label on the evidence, not a reason to escalate (3.3): restarts and recreation are controlled by whoever runs workloads in a watched namespace.

## 4.3. The missing reference check

<span class="sw-status sw-built">built</span> `internal/analyzer/refcheck` answers whether a Secret or ConfigMap a pod references is missing, with no permission on either (10.3). The read tool `check_missing_reference` offers it to the escalation model ([#74](https://github.com/pkalin/shadewright/issues/74), [#76](https://github.com/pkalin/shadewright/issues/76)).

It is a pure function of the pod and the Events in its namespace, both read by the analyzer already. The answer is `missing` with its evidence, or `unknown`; there is no `exists`.

**`missing` needs the kubelet's own report in its fixed shape (Kubernetes v1.37.2), in one of two forms.**

1. A container or init container waiting with reason `CreateContainerConfigError` whose message is exactly the kubelet's error for one of that container's own non-optional `env` or `envFrom` references: `secret "<name>" not found`, the plural `secrets` from the API server, or `couldn't find key <key> in Secret <namespace>/<name>`, likewise for ConfigMaps.
2. While the pod is `Pending` and none of its containers has got past volume setup, a Warning `FailedMount` Event from the `kubelet` about the pod (namespace, name and UID) whose note is exactly `MountVolume.SetUp failed for volume "<volume>" : <error>` for one of the pod's own non-optional `secret`, `configMap` or `projected` volume sources.
    - "Not got past volume setup": the kubelet mounts every volume before it pulls an image or starts any container, so a container that runs, ran or waits for any reason but `ContainerCreating` or `PodInitializing` means the volumes are mounted.
    - That Event must be the latest SetUp error of the volume, and the volume must still fail: the Event, or the kubelet's `Unable to attach or mount volumes: unmounted volumes=[...]` Event listing the volume, was last observed within the past 10 minutes.
    - The second Event is needed because client-go's recorder spends one budget on all Warning Events about a pod, whatever their reason (25 at once, then one per 5 minutes). So on a pod stuck for long the SetUp error may go unrefreshed while the kubelet's timeout Event (every 2 minutes and 3 seconds) is refreshed, or the other way round. 10 minutes leave one spare refill.
    - A mount that failed and then succeeded within the last 10 minutes, while every container still shows a default waiting reason, still counts.

**Names are never taken from a message.** The check builds the texts the kubelet would write for each reference in the pod spec and compares. It reads the list of unmounted volumes only to compare its entries with the spec's volume names. So the name, key, container and volume it returns come from the spec, and a crafted Event or status cannot make it name an object the pod does not reference. A crafted Event can still make a referenced object look missing; the result says whether the evidence was a container status or an Event.

**Output.** No message text is returned, and the result is bounded: one answer, a few names from the spec. A key missing from a volume's `items`, a missing image pull Secret and ephemeral containers give `unknown`.

## 4.4. What the model cannot do

- There is no proposal tool: a proposal is the structured output of the call (3.3), checked by one validator.
- PR creation and auto-merge are not model tools at all: only the remediation controller does them, after policy.
- The model never gets a shell, `kubectl exec`, `kubectl apply`, or a way to change policy.
- Every tool has an argument schema, a timeout, a response size limit, an allow list of object kinds and server-side authorization.
- Tool results, logs and repository content are untrusted data, including when they contain instructions addressed to the agent.
- RBAC on the ServiceAccount enforces the limits; hiding write tools is not enough.
