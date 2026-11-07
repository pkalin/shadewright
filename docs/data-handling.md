---
title: Data handling
---

# Data handling

What Shadewright stores, what it sends where, what it publishes and how long each of it lives. State on `main`. Where a part is designed but not built, the row says **planned** and links its issue or ADR. The project's claims about secrets are invariant 4 in [AGENTS.md](https://github.com/pkalin/shadewright/blob/main/AGENTS.md): everything from the cluster is untrusted, and secrets and tokens are redacted before storage and before a model sees anything. The adversaries and the tests behind each claim are in the [threat model](threat-model.md).

## What it reads

| Source | What | By |
| --- | --- | --- |
| Events (`events.k8s.io`) and Pods | Reason, type, reporting controller, note text, object names and owners, container termination state, in the **watched namespaces only** | Analyzer |
| Deployments, ReplicaSets, StatefulSets | Owner references and metadata (`get`) | Analyzer |
| Nodes | `list`, for platform detection | Analyzer |
| Prometheus (only with `--prometheus-url`; read by the context builder and the read tools when the analysis runs, #76) | The results of the approved query templates ([design, section 4](design/4-tools.md#4-tools)), only for the **watched namespaces**: Prometheus applies no authorization, so the adapter refuses any other namespace before a request | Analyzer |
| Flux `Kustomization` and `GitRepository`, the target workload or `ScaledObject`, policy and settings | Status and spec, for source mapping and the freshness check | Executor |
| The Git repository | The files of the build for the target's Kustomization (up to 256 files, 1 MiB each), through the GitHub API | Executor |

Neither identity reads Secrets or ConfigMap data (except its own budget ConfigMap). The executor reads no **pod logs**. The analyzer may (`get` on `pods/log` in the watched namespaces only; `--pod-logs=false` turns the source off, and in the Helm chart `analyzer.podLogs.enabled=false` also removes the grant), but reads none yet: the context builder that holds the pod logs source is not called until the analysis loop is built ([#76](https://github.com/pkalin/shadewright/issues/76)). Once it is, the source reads one container per call, only in the watched namespaces, at most 24 hours back and at most 1 MiB per request (at most two requests per call), and returns at most 256 KiB of whole lines, redacted before it leaves the adapter and again by the context builder ([design, section 4](design/4-tools.md#4-tools)). Excerpts are never stored in a custom resource.

## Redaction

One function, `internal/redact`, removes secrets and personal data before text is stored. It replaces a match with a marker that names the kind (`[REDACTED:jwt]`, `[REDACTED:token]`, `[REDACTED:ip]`, and so on). It covers private keys, JWTs, tokens with known prefixes and long random strings, connection strings, credentials in URLs, Authorization values, values of secret-named keys in env, JSON and YAML, emails and IP addresses. It is best effort and errs towards removing too much; the package comment lists the known gaps (JSON escaped more than three times, a Kubernetes env entry whose `value` is far from its `name`, a secret in a format no rule knows). The package accepts extra patterns from the operator, but **no flag or setting passes any yet**, so only the built-in rules apply.

## What is stored in the cluster

| Object | Holds | Never holds | Lifetime |
| --- | --- | --- | --- |
| `Incident` (operator namespace) | Fingerprint, the subject (kind, namespace, name), event reason and type, reporting controller, the **normalized and redacted symptom** (at most 1024 characters; names, UIDs, digests, timestamps, IPs and durations replaced by placeholders), counters and times, up to 32 occurrence records (an ID and a count each), analysis state and conditions | Raw event text, logs, transcripts | **No expiry.** At most 500 (`--max-incidents`); nothing deletes them, so once the cap is reached someone has to delete some, and the analyzer counts again only when it restarts. Retention by age is **not built** (design question 8) |
| `ConfigMap shadewright-analysis-budget` | `day` (UTC) and `used` (a count) | | Reused; reset by the first analysis of a new UTC day |
| `RemediationRequest` and status | The action and its bounds, the target (kind, namespace, name, UID), the resolved source (repository, branch, file path), plan digest, the pull request number, URL and head commit, conditions with fixed text | Provider response text, tokens, file contents | **No expiry**; deleted by whoever deletes it (which closes its open pull request, see [upgrade and uninstall](upgrade-uninstall.md)) |
| `RemediationSettings` and `RemediationPolicy` | Repository URLs, the GitHub App client ID, the installation ID, the Key Vault key URL, the allowed scope | The private key, any token | Until deleted. Client and installation IDs are not secrets but belong in your overlay, not in a public repository |
| Events | Fixed-text reasons about requests, the cap and Incidents skipped as not recent (`AnalysisSkipped`) | Provider text | The cluster's event TTL (the API server's `--event-ttl`, 1 hour by default) |
| Pod logs of the two components | Structured log lines with names, counts and error text; the analyzer's outcome line also has the result's summary and missing data (model text, redacted) | Tokens (a token type prints as a placeholder), the JWT, repository URLs in errors, key material | Whatever your log collection keeps |
| Memory | The collector's maps (10,000 events, 5,000 pods, least recently used dropped), read caches of Git files at a commit | | Until the pod restarts |

Anything in the cluster is readable by whoever has `get` on it, so treat the Incidents as operational data of the watched namespaces: they name workloads and the reasons they fail.

## What is sent to the model

**Nothing unless the analyzer runs with `--triage-model`** (off by default, also in the Helm chart: `analyzer.analysis.triageModel`). Without it the analysis loop does not start, no model is called and no Azure token is requested. With it ([#76](https://github.com/pkalin/shadewright/issues/76), [ADR-0023](adr/0023-how-the-analyzer-calls-the-model.md)), the analyzer calls the deployments named in `spec.analysis.models` through the Microsoft Foundry adapter ([#75](https://github.com/pkalin/shadewright/issues/75)), with its own Azure workload identity (an Entra ID token for the Cognitive Services audience only, no API key), and only after the endpoint passed the host allow-list of `--model-endpoint-suffixes`. Until the evaluation gate exists ([#78](https://github.com/pkalin/shadewright/issues/78)), turn it on only for an evaluation run in the lab.

What one analysis sends, per call:

- **The system part**, the same for every incident: the role and rules, the action catalog and the output JSON schema, all from code (`internal/analysis/prompts/`). No cluster data.
- **The context**, one user message of delimited blocks tagged with their provenance: the incident summary (subject kind, namespace and name, reason, counts and times), the object status and owners, a log excerpt of the attributed pods (only with `--pod-logs`), selected metrics (only with `--prometheus-url`), and a Git section that says it is not available. Every block is redacted by `internal/redact` (built-in rules and operator patterns) and bounded in size before the model sees it; the prompt says the text is data, not instructions.
- **On escalation only** (with `--escalation-model`): the read tool definitions, and each tool's redacted, bounded answer, at most `maxToolSteps` calls, each bound to the namespace of the incident's subject.
- The deployment name, `maxOutputTokens` and the response schema.

What is kept: nothing is stored beyond the Incident's status (phase, the condition `Analyzed` with a fixed reason, and the names of the requests created) and the request a valid proposal creates. The analyzer logs the outcome: reason, call and token counts, the model IDs the replies named, the prompt version and, from a valid result, its summary and the data it said were missing, redacted again and within the result's bounds ([#422](https://github.com/pkalin/shadewright/issues/422)); never the context, the prompt or the raw reply. With `--analysis-store-account` and `--analysis-store-container` (Helm: `analyzer.analysis.recordStore.azureBlob`), the analyzer also writes one analysis record per run to that Blob Storage container ([ADR-0015](adr/0015-analysis-records-outlive-the-lab.md), [#77](https://github.com/pkalin/shadewright/issues/77)) and puts its ID in the Incident's `status.analysisRecords` (the newest 20): the Incident's name, UID, fingerprint, reason and subject, when the run began and ended, the outcome's reason, the call, tool call and token counts, the deployments and the model IDs, the prompt version, the request's name, and the result's summary and missing data. Every string from the cluster, the settings or the model is redacted, and the record is refused before anything is sent if the redactor would still change it. It holds no context, prompt, tool result or raw reply. Without those flags no record is written. The model's output is a proposal only; deterministic policy decides whether it runs (invariant 1). How much it may spend: `spec.analysis.maxAnalysesPerDay` analyses per UTC day (default 100), and `spec.analysis.models.*.maxTokensPerDay` tokens per deployment, enforced within one analysis only until its counter exists ([#208](https://github.com/pkalin/shadewright/issues/208)). Where inference runs depends on the deployment type ([design, section 11](design/11-model.md)).

## What leaves the cluster today

| To | What | When |
| --- | --- | --- |
| `api.github.com` | Reads of the files and branch heads in the configured repository; a branch, one commit changing one value in one file, a pull request, and a close of that pull request. The App JWT (10 minutes) and an installation token scoped to that one repository, with four permissions | Only the executor, and only after a request is allowed and writes are not paused |
| Key Vault | The SHA-256 digest of the JWT to sign. The key stays in the vault | Each time a new installation token is needed |
| Microsoft Entra ID | The projected ServiceAccount token, for a Key Vault token | Same |
| The Kubernetes API | Reads and the writes listed in the [configuration reference](reference/configuration.md#namespaces-and-rolebindings) | Always |

No telemetry, usage reporting or update check exists in either binary: the only packages that open outbound HTTP connections are the GitHub adapter, its token source and the Key Vault signer, the Prometheus adapter, only when `--prometheus-url` is set, the Microsoft Foundry adapter with its Entra ID token source, in the analyzer only and only with `--triage-model`, and the Blob Storage analysis store, in the analyzer only and only with `--analysis-store-account` (egress to the model endpoint and Entra ID on 443, which the analyzer's NetworkPolicy already allows). The analyzer queries the configured Prometheus with the approved templates of the [design](design/4-tools.md#4-tools) when the analysis runs ([#76](https://github.com/pkalin/shadewright/issues/76)); its NetworkPolicy needs the Prometheus rule for that (`networkPolicy.analyzer.prometheus`, [design, section 10](design/10-security.md#10-security)). Opt-in sharing of outcomes is **planned** and off by default ([#284](https://github.com/pkalin/shadewright/issues/284)). OpenTelemetry traces per analysis ([#90](https://github.com/pkalin/shadewright/issues/90)) are off by default; turned on (`OTEL_TRACES_EXPORTER=otlp`, or the chart value `analyzer.tracing.endpoint`), the analyzer sends them over OTLP gRPC to the collector you name, and its NetworkPolicy needs the collector rule (`networkPolicy.analyzer.traceCollector`). A span carries IDs, counts, durations, fixed result labels and the configured model and deployment names only: no prompt, context, log, tool or model text, no error message and no token ([traces reference](reference/traces.md#what-a-span-carries)).

The NetworkPolicies limit ports and directions (DNS, TCP 443 and 6443, and for the analyzer, if you enable it in the chart, TCP to the one Prometheus you name), not destinations: what keeps the analyzer away from GitHub is that its binary has no GitHub adapter and holds no GitHub credential. See the [design](design/10-security.md#10-security) for the two ways to narrow it.

## What is published

The executor writes to the fleet repository, which is public in the reference lab. A pull request carries:

- the branch name `shadewright/<request UID>`;
- the commit message `fix: set <field> of <namespace>/<name> to <value>`;
- the pull request body: the change (action, field, previous and desired value), the file path and target branch, the request's namespace, name and UID, the Incident name, the policy name and generation, the plan digest, the expected effect, the risk, the rollback and whether approval is required, plus a marker with the UID and the digest;
- the changed file, with that one value different.

Everything in the title and body comes from validated fields and is redacted once more before it is sent. The Incident name and the request's own `namespace/name` are the two exceptions, because their names are long hex strings that look like tokens. The Incident name is shown unredacted only if the executor reads that Incident and its name is the one the collector derives from its fingerprint (`incident-` and 64 hex digits). The request name is shown unredacted only if it is the name the analyzer derives from the request's own spec (`remediation-` and 24 hex digits of a SHA-256 of the Incident's UID and the plan), not merely a name of that shape, and only with a namespace the redactor leaves unchanged. Otherwise each is shown redacted. The symptom text, event notes and logs are **not** in the pull request.

A merged pull request, its history and any branch left behind are in the Git host's retention, not Shadewright's. Shadewright never deletes a branch; a pull request it closed keeps its branch.

## Retention

| Data | Default | Where it is set |
| --- | --- | --- |
| Incidents, requests | No expiry. Incidents capped at 500 | `--max-incidents`; deleting is manual |
| Analysis records (redacted transcripts of each analysis) | Written only with `--analysis-store-account` (Azure Blob Storage, [#77](https://github.com/pkalin/shadewright/issues/77)): one per analysis, create-only, never read back by the analyzer. They hold the outcome, counts and redacted summary listed above, not yet the full transcript. In the reference lab the store is a blob container in the foundation, kept **180 days** (`analysis_retention_days`), with blob versioning, and 7 days of soft delete for deleted blobs | `shadewright-azure/foundation`; [ADR-0015](adr/0015-analysis-records-outlive-the-lab.md). In other clusters the store is to be on by default and configurable (planned). The writer role does not yet enforce create-only ([#154](https://github.com/pkalin/shadewright/issues/154)) |
| Key Vault audit log (`KeySign` for every use of the App key) | 90 days in a Log Analytics workspace that outlives the lab | `shadewright-azure/foundation` |
| Key Vault, deleted vault or key | Recoverable for 7 days; purge protection on | Foundation |
| Pull requests, branches, commits | Git host's, indefinitely | Your repository settings |
| Kubernetes Events | Cluster event TTL (1 hour by default) | API server |

The lab cluster is destroyed after every session, so everything in the cluster goes with it; the blob container, the vault and the audit workspace are in the foundation and do not.

## Deleting data

- Cluster: `kubectl delete` the Incidents and requests. Deleting a request first closes its open pull request.
- Git: close or delete the pull requests and `shadewright/*` branches in the repository.
- Azure (lab): the records in the `analyses` container (a delete leaves the versions until the retention rule removes them), the audit workspace and the vault follow Azure's own rules above.
