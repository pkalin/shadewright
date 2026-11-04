---
title: Data handling
---

# Data handling

What Shadewright stores, what it sends where, what it publishes and how long each of it lives. State at v0.1.0. Where a part is designed but not built, the row says **planned** and links its issue or ADR. The project's claims about secrets are invariant 4 in [AGENTS.md](https://github.com/pkalin/shadewright/blob/main/AGENTS.md): everything from the cluster is untrusted, and secrets and tokens are redacted before storage and before a model sees anything. The adversaries and the tests behind each claim are in the [threat model](threat-model.md).

## What it reads

| Source | What | By |
| --- | --- | --- |
| Events (`events.k8s.io`) and Pods | Reason, type, reporting controller, note text, object names and owners, container termination state, in the **watched namespaces only** | Analyzer |
| Deployments, ReplicaSets, StatefulSets | Owner references and metadata (`get`) | Analyzer |
| Nodes | `list`, for platform detection | Analyzer |
| Flux `Kustomization` and `GitRepository`, the target workload or `ScaledObject`, policy and settings | Status and spec, for source mapping and the freshness check | Executor |
| The Git repository | The files of the build for the target's Kustomization (up to 256 files, 1 MiB each), through the GitHub API | Executor |

Neither identity reads Secrets, ConfigMap data (except its own budget ConfigMap) or **pod logs**. Reading logs is **planned** ([#72](https://github.com/pkalin/shadewright/issues/72)), bounded and redacted.

## Redaction

One function, `internal/redact`, removes secrets and personal data before text is stored. It replaces a match with a marker that names the kind (`[REDACTED:jwt]`, `[REDACTED:token]`, `[REDACTED:ip]`, and so on). It covers private keys, JWTs, tokens with known prefixes and long random strings, connection strings, credentials in URLs, Authorization values, values of secret-named keys in env, JSON and YAML, emails and IP addresses. It is best effort and errs towards removing too much; the package comment lists the known gaps (JSON escaped more than three times, a Kubernetes env entry whose `value` is far from its `name`, a secret in a format no rule knows). The package accepts extra patterns from the operator, but **no flag or setting passes any yet**, so only the built-in rules apply.

## What is stored in the cluster

| Object | Holds | Never holds | Lifetime |
| --- | --- | --- | --- |
| `Incident` (operator namespace) | Fingerprint, the subject (kind, namespace, name), event reason and type, reporting controller, the **normalized and redacted symptom** (at most 1024 characters; names, UIDs, digests, timestamps, IPs and durations replaced by placeholders), counters and times, up to 32 occurrence records (an ID and a count each), analysis state and conditions | Raw event text, logs, transcripts | **No expiry.** At most 500 (`--max-incidents`); nothing deletes them, so once the cap is reached someone has to delete some, and the analyzer counts again only when it restarts. Retention by age is **not built** (design question 8) |
| `ConfigMap shadewright-analysis-budget` | `day` (UTC) and `used` (a count) | | Reused; reset by the first analysis of a new UTC day |
| `RemediationRequest` and status | The action and its bounds, the target (kind, namespace, name, UID), the resolved source (repository, branch, file path), plan digest, the pull request number, URL and head commit, conditions with fixed text | Provider response text, tokens, file contents | **No expiry**; deleted by whoever deletes it (which closes its open pull request, see [upgrade and uninstall](upgrade-uninstall.md)) |
| `RemediationSettings` and `RemediationPolicy` | Repository URLs, the GitHub App client ID, the installation ID, the Key Vault key URL, the allowed scope | The private key, any token | Until deleted. Client and installation IDs are not secrets but belong in your overlay, not in a public repository |
| Events | Fixed-text reasons about requests and the cap | Provider text | The cluster's event TTL (the API server's `--event-ttl`, 1 hour by default) |
| Pod logs of the two components | Structured log lines with names, counts and error text | Tokens (a token type prints as a placeholder), the JWT, repository URLs in errors, key material | Whatever your log collection keeps |
| Memory | The collector's maps (10,000 events, 5,000 pods, least recently used dropped), read caches of Git files at a commit | | Until the pod restarts |

Anything in the cluster is readable by whoever has `get` on it, so treat the Incidents as operational data of the watched namespaces: they name workloads and the reasons they fail.

## What is sent to the model

**Nothing in v0.1.0.** The analysis that calls a model is not built, the analyzer has no model credential and no workload identity annotation, and no component has a model client. What is decided ([ADR-0023](adr/0023-how-the-analyzer-calls-the-model.md), **planned**, epic [#16](https://github.com/pkalin/shadewright/issues/16)):

- The model gets a bounded, redacted context built deterministically: the incident summary, object status and owners, a log excerpt, selected metrics and recent Git changes for the object's source. Untrusted text is delimited and tagged with its provenance and the prompt says it is data, not instructions.
- Read tools are offered only on escalation, within fixed step, token and spend budgets.
- The model's output is a proposal only; deterministic policy decides whether it runs (invariant 1).
- The endpoint is Microsoft Foundry first. In the lab the model deployments are in a subscription of the project's own, behind Entra ID (no API keys).
- How much it may spend per day is `spec.maxAnalysesPerDay` (default 100).

When it is built, this page gets the exact fields sent.

## What leaves the cluster today

| To | What | When |
| --- | --- | --- |
| `api.github.com` | Reads of the files and branch heads in the configured repository; a branch, one commit changing one value in one file, a pull request, and a close of that pull request. The App JWT (10 minutes) and an installation token scoped to that one repository, with four permissions | Only the executor, and only after a request is allowed and writes are not paused |
| Key Vault | The SHA-256 digest of the JWT to sign. The key stays in the vault | Each time a new installation token is needed |
| Microsoft Entra ID | The projected ServiceAccount token, for a Key Vault token | Same |
| The Kubernetes API | Reads and the writes listed in the [configuration reference](reference/configuration.md#namespaces-and-rolebindings) | Always |

No telemetry, usage reporting or update check exists in either binary: the only packages that open outbound HTTP connections are the GitHub adapter, its token source and the Key Vault signer. Opt-in sharing of outcomes is **planned** and off by default ([#284](https://github.com/pkalin/shadewright/issues/284)). OpenTelemetry traces per analysis are **planned** ([#90](https://github.com/pkalin/shadewright/issues/90)); the NetworkPolicies allow no destination for them yet.

The NetworkPolicies limit ports and directions (DNS, TCP 443 and 6443), not destinations: what keeps the analyzer away from GitHub is that its binary has no GitHub adapter and holds no GitHub credential. See the [design](design.md#10-security) for the two ways to narrow it.

## What is published

The executor writes to the fleet repository, which is public in the reference lab. A pull request carries:

- the branch name `shadewright/<request UID>`;
- the commit message `fix: set <field> of <namespace>/<name> to <value>`;
- the pull request body: the change (action, field, previous and desired value), the file path and target branch, the request's namespace, name and UID, the Incident name, the policy name and generation, the plan digest, the expected effect, the risk, the rollback and whether approval is required, plus a marker with the UID and the digest;
- the changed file, with that one value different.

Everything in the title and body comes from validated fields and is redacted once more before it is sent. The Incident name is the one exception, because it is a long hex string that looks like a token: it is shown unredacted only if the executor reads that Incident and its name is the one the collector derives from its fingerprint (`incident-` and 64 hex digits). Otherwise it is shown redacted. The symptom text, event notes and logs are **not** in the pull request.

A merged pull request, its history and any branch left behind are in the Git host's retention, not Shadewright's. Shadewright never deletes a branch; a pull request it closed keeps its branch.

## Retention

| Data | Default | Where it is set |
| --- | --- | --- |
| Incidents, requests | No expiry. Incidents capped at 500 | `--max-incidents`; deleting is manual |
| Analysis records (redacted transcripts of each analysis) | **Not written yet**: the `AnalysisStore` adapter is **planned** ([#77](https://github.com/pkalin/shadewright/issues/77)). In the reference lab the store is a blob container in the foundation, kept **180 days** (`analysis_retention_days`), with blob versioning, and 7 days of soft delete for deleted blobs | `shadewright-azure/foundation`; [ADR-0015](adr/0015-analysis-records-outlive-the-lab.md). In other clusters the store is to be on by default and configurable (planned). The writer role does not yet enforce create-only ([#154](https://github.com/pkalin/shadewright/issues/154)) |
| Key Vault audit log (`KeySign` for every use of the App key) | 90 days in a Log Analytics workspace that outlives the lab | `shadewright-azure/foundation` |
| Key Vault, deleted vault or key | Recoverable for 7 days; purge protection on | Foundation |
| Pull requests, branches, commits | Git host's, indefinitely | Your repository settings |
| Kubernetes Events | Cluster event TTL (1 hour by default) | API server |

The lab cluster is destroyed after every session, so everything in the cluster goes with it; the blob container, the vault and the audit workspace are in the foundation and do not.

## Deleting data

- Cluster: `kubectl delete` the Incidents and requests. Deleting a request first closes its open pull request.
- Git: close or delete the pull requests and `shadewright/*` branches in the repository.
- Azure (lab): the records in the `analyses` container (a delete leaves the versions until the retention rule removes them), the audit workspace and the vault follow Azure's own rules above.
