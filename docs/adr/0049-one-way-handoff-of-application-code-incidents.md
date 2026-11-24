# ADR-0049: One-way handoff of application-code incidents through the coordinator

- Status: Proposed
- Date: 2026-11-24

## Context

Application-code fixes lie outside Shadewright's closed action catalog (invariant 2). Operators running Sablecourt may want to hand it those incidents ([epic #1130](https://github.com/shadewright-labs/shadewright/issues/1130)). This exports cluster data to a receiver whose agents write code.

[ADR-0028](0028-reviewer-hints-through-the-pull-request.md) section 1 and invariant 6 reserve integrations other than the code host for a separate coordinator. [ADR-0014](0014-notifications-github-issues-first.md) describes notifications; `internal/providers/notify.go` defines their interface. This proposes a separate handoff, off by default. Decisions below require implementation unless identified as current code.

## Decision

### 1. Analyzer-owned candidate selection

The analyzer selects candidates by code. The analysis must end `Escalated`, and the analyzer must prove no `RemediationRequest` was created for the Incident. It checks retained history and every live request naming the Incident, including a recovered uncertain create. Deletion cannot erase that history. Failed or uncertain checks and policy-refused requests exclude the Incident.

`NoProposal` in `internal/analysis/loop.go`, `finish`, means the model returned no proposal, not that no runbook matched. Model reasons may only narrow the gate. Without `--triage-model`, `cmd/analyzer/analysis.go` starts no analysis controller: there is no completed analysis, verdict or snapshot. Phase `Escalated` alone is insufficient.

All additional rules must pass:

- An install-time allowlist names the Deployment or StatefulSet, namespace, application image repository and destination repository alias. Parse images as image references; another image repository or an uncertain image identity refuses the workload.
- `status.folded` is false. Unlike `status.foldedInto`, which clears on reopen, this flag persists (`api/v1alpha1/incident_types.go`). A policy source on this Incident or any Incident folded into it excludes it. The analyzer retains policy-source fold provenance across reopen and deletion. Incomplete provenance excludes the Incident.
- Exclusions cover `OOMKilled`, exit 137, `Unschedulable`, `ImagePullBackOff`, `ErrImagePull`, `InvalidImageName`, `CreateContainerConfigError`, `CreateContainerError`, exits of deleting pods, exit 143, policy reports and admission refusals. For each Secret or ConfigMap reference, `Missing` and `Unknown` both exclude it: `internal/analyzer/refcheck` has no positive existence answer.
- Require three distinct observations for the same container. Retain the newest three qualifying records per Incident in analyzer-owned status. Each holds a stable observation ID, container identity, time and a typed reason (`NonzeroExit`, `ContainerBackoff` or `FailedReadiness`), plus the exit code for an exit. Derive IDs from pod UID, container and the failure episode so repeated reads or reports of the same episode count once. Uncertain episode identity supplies no evidence. `OccurrenceRecord` lacks these facts, and `status.count` includes Event series increments.
- The current analysis context must show a qualifying failure and include a nonempty, checked log excerpt for that container. Missing or withheld evidence excludes the Incident. Workloads can cause these failures and write the logs, so the result is a candidate for human review.

The analyzer records the verdict, rule version and analysis identity in Incident status, then adds the snapshot reference after storage succeeds. Its digest binds the verdict and payload. A new analysis, request, fold or changed selection verdict invalidates the reference.

### 2. Snapshot storage

The current `azureblob.Store` ID pattern permits one segment. `Put` writes only `<ID>.json` at the container root with `If-None-Match: *` and refuses an existing name. It cannot write the proposed prefix.

Add a typed snapshot operation with fixed prefix `escalation/`, validated leaf ID, 16 KiB bound, decoded-string redaction validation and conditional writes refusing existing names. Do not accept arbitrary paths or weaken record IDs. After success the analyzer publishes the name, SHA-256 of the final serialised payload and 24-hour expiry. Failed or uncertain writes publish no usable reference; an existing blob is not assumed to match.

Once the outcome is recorded, offer a bounded snapshot job to a separate bounded queue without waiting. Its own workers check and write snapshots under deadlines. They publish the reference only if the analysis identity and verdict are still current. Queue saturation, restart or write failure can lose a handoff without requeuing analysis or changing its outcome.

This decision extends invariant 6 to permit analyzer snapshot writes in its existing record container and coordinator and approver reads restricted to `escalation/` there. Pin those scopes. The existing `blobs/write` grant covers the writes and permits overwrites. Conditional PUT is adapter behavior. Digest verification protects integrity and versioning provides recovery evidence. A lifecycle rule removes snapshots and versions after a day. A record store is required. See [ADR-0015's amendment](0015-analysis-records-outlive-the-lab.md#amendments), [ADR-0029](0029-azure-module-environment-and-cluster-layers.md) section 6 and [#154](https://github.com/shadewright-labs/shadewright/issues/154).

### 3. Payload and redaction

The snapshot is the final JSON request body, at most 16 KiB. It carries `schemaVersion: 1`, the selection verdict, fixed rule version, install-mapped repository alias and an `untrusted` object. That object holds all cluster-user-influenced fields: Incident identity, fingerprint, reason, count, timestamps, workload, container alias, optional image digest, matched rule IDs, evidence summaries, excerpts and model hint. None chooses a destination or changes instructions.

Use a UUID, SHA-256 fingerprint and optional image digest, a workload kind enum and RFC 3339 timestamps. Namespace and workload name are bounded at 63 and 253 bytes. All reason fields use the fixed reason enum in decision 1. Use an operator-approved container alias in every container reference, including excerpt metadata, and withhold the snapshot if the mapping is missing. Aliases match `^[a-z0-9-]+$` and have at most 63 bytes. Allow 8 fixed rule IDs, 16 code-written evidence lines totalling 2 KiB, 2 excerpts of 4 KiB each with collection time and cut/withheld flags, and a 1 KiB model hint with controls removed. The schema omits pod specs, environment values, commands, probe headers, labels, annotations, Secret and ConfigMap contents, image names, prompts, transcripts and tool results.

Before storage, strings use `StringTwiceChecked` (`internal/redact/before.go`). Matches are masked in place; matching does not withhold an entire entry. Work exhaustion withholds the entry whole. Code also withholds on size or heap admission limits and expired admission waits (`internal/redact/budget.go`). A withheld optional excerpt is omitted and marked; a withheld required identity or evidence prevents the snapshot.

Before each send, the coordinator checks decoded strings, keys and scalar key/value relationships. An independent outbound check looks for URLs with userinfo and long high-entropy tokens, classes already covered by redactor rules. A match ends `Withheld`. An unfinished check sends nothing. The redactor preserves Kubernetes names. Identifiers and free text can still disclose short passwords or other unrecognised secrets, personal data, internal hosts or plain URLs. Some emails and IP addresses are masked.

### 4. Approval covers content

`CodeEscalation`, a namespaced ledger entry named from Incident UID, stores the Incident reference, analysis identity, snapshot name, expiry, final payload digest, alias and idempotency key in spec. The spec is immutable except for the one-time approval transition, which sets `approved` and its bound digest together. Changed content cannot reuse approval.

A new read-only kubectl plugin view reads the snapshot with the approver's own storage role, prints the exact bytes and computed digest and verifies the ledger digest. It warns that a network policy or admission policy may be the cause, because classification cannot see those causes. Logs stay out of CRs: `AGENTS.md` says, "Never put raw logs or large payloads in a CR."

Every send requires a person's approval of those bytes. Removing that requirement needs another ADR. Approval patches `spec.approved: true` and sets `spec.approvedDigest` to `spec.payloadDigest`. CEL makes approval set-never-unset and pins its digest, following `spec.cancel` for the one-way transition. Creation-time CEL uses `optionalOldSelf=true` to forbid true approval or any approval digest on create.

The shipped approver ClusterRole grants only `patch` on entries. Grant it to designated approvers through explicit RoleBindings in the operator namespace, with no aggregation into namespace `admin` or `edit`. Anything with `update` can also approve, including a GitOps controller applying a manifest. Install guidance forbids broader grants. Approval is enforced in coordinator code and does not survive its compromise.

### 5. Coordinator identity and forged entries

`shadewright-coordinator` has its own Deployment, ServiceAccount, image, storage reader and NetworkPolicy. In the operator namespace it may read Incidents (`get/list/watch`), get settings by name, create/get/list/watch/delete entries and update their status only. It has no request, pod, Event, Secret, model or code-host grant. Executor and verifier gain no privilege.

The coordinator checks the allowlist, repository alias and ledger against the analyzer's selection verdict. It recomputes every non-approval spec field from the Incident and its own mapping and validates both approval fields against the transition rules and recomputed digest. Any differing entry ends `Refused`, including one it did not create. Missing provenance, changed mappings, unknown fields and expired references refuse. It reads only the referenced snapshot under the fixed prefix and verifies its digest. It never repeats symptom classification or trusts an entry's asserted origin.

### 6. Transport and pause

One exact HTTPS intake URL is fixed at install. Adopt [ADR-0042](0042-openai-compatible-model-endpoints.md) controls: verified TLS 1.2 or later, no redirects, no environment proxy and guarded dialing. `openaiwire.NewHTTPClient` supplies these current controls; receiver address pinning and optional mounted CA roots are new work.

A NetworkPolicy cannot name a DNS name. Install fixes the receiver as an IP range, an in-cluster Service whose ClusterIP is pinned for the dial guard, or an operator-controlled egress proxy. Also permit only required DNS, API server and storage paths. DNS outside the pinned destination refuses the dial. Service routing and proxy administration remain operator trust boundaries.

Prefer a projected ServiceAccount token for the receiver's audience. This ADR also permits an intake-only static integration token under invariant 6: coordinator-only read-only file mount, no Secret API read, environment variable or CR field, reread each attempt, never emitted in errors or telemetry.

After reading the token and completing slow preparation, immediately before each attempt, read `spec.paused` live. Paused, missing or unreadable settings send nothing. Every retry repeats the check. This follows `internal/providers/github/client.go`, `once`, where the write check follows token acquisition.

### 7. Delivery and receiver contract

The idempotency key is SHA-256 of a fixed protocol domain and Incident UID, with no cluster ID. The coordinator has its own `--cluster-id` for local accounting; neither it nor a guessable hash is exported. The ledger fixes the key before approval and reuses it after restart, timeout and credential rotation. The key is a transport header recomputed by the coordinator, outside the body.

States advance from `AwaitingApproval` to `Sending`, then `Accepted`, `Refused`, `GaveUp`, `Withheld` or `Expired`. Attempts have 10-second deadlines and at most 6 tries with jittered backoff from 1 minute to 1 hour. Only network errors, 429 and 5xx retry. Five consecutive failures pause sends for 15 minutes. The durable daily limit defaults to 10. Unknown ledger or budget state sends nothing. Terminal entries remain 30 days. The analyzer retains a handoff marker so deletion cannot permit another export for that UID.

Accept at most 4 KiB of JSON with a fixed status enum and a receipt ID matching `^[A-Za-z0-9_-]{1,64}$`. Before persisting any receipt, check the raw reply and decoded JSON against the exact credential sent on that attempt. An echo or a check that cannot finish ends `Refused` with no receipt. Use fixed errors, discard all other reply content and omit response text and wrapped transport errors from logs. Apply the echo guard in [ADR-0042](0042-openai-compatible-model-endpoints.md).

The receiver uses an intake-only credential, returns identical receipts for repeated keys and refuses different bytes under a used key. It treats fields as data, redacts on receipt and changes code only through reviewed pull requests.

`Accepted` is terminal delivery only. An immediate `MoreInformationRequired` reply ends `Refused`. The approving operator owns follow-up outside the integration. There is no callback, learning import or rollout report. Acceptance never resolves an Incident or changes remediation. Verifying outside changes needs a separate image-digest handshake decision.

## Consequences

This adds a fourth identity, CRD, storage operations, scoped grants and approval workflow. Invariant 6 and installation guidance change with the first build after acceptance. Invariants 1 through 5 retain their authorization, catalog, Git, untrusted-input and idempotency boundaries.

`Unknown` reference results drop some real code faults. Configuration and code failures can look identical, and hidden network or admission policy causes remain possible. Log injection and disclosure remain risks despite redaction and approval. A compromised analyzer can forge a selection verdict. A compromised coordinator can bypass approval and use its credential.

Before enablement, test every exclusion and bound, repeated observations, forged entries, approval at creation, digest mismatch and content mutation. Test the RoleBindings, coordinator patch refusal and approval through a broader GitOps-style update grant. Cover storage conflicts, swapped blobs, a full queue and stalled writes, decoded escaped secrets, withholding, pause after token acquisition on every retry, destination changes, replay, credential echoes and terminal requests for more information.

## Alternatives considered

- Copy bounded payload into ledger status: rejected by the CR log/payload prohibition. Approvers use their own scoped storage access.
- Rebuild context in the coordinator: pod and log grants expose more data and duplicate classification.
- Send from analyzer or executor: forbidden by invariant 6 and ADR-0028.
- Model classification as authorization: forbidden by invariant 1 and [ADR-0023](0023-how-the-analyzer-calls-the-model.md).
- Chat or code-host approval: requires another identity-verification decision and credential.

## Red team

- Model-induced export: decision 1 requires code-based candidate selection and no request.
- Policy bypass: decision 1 excludes policy sources, folded policy sources and existing requests.
- Foreign image or repository routing: decisions 1 and 5 bind image repository and alias to install mappings.
- Forged entry or approval: decisions 4 and 5 require creation guards, spec recomputation and digest-bound approval.
- Snapshot replacement or unseen content: decisions 2 and 4 require conditional writes, digest checks and exact-byte review.
- Encoded secrets or log injection: decision 3 checks decoded strings and labels cluster data `untrusted`; receiver isolation remains required.
- Redirect, proxy or DNS theft: decision 6 fixes destination, TLS and dial restrictions.
- Pause during token acquisition: decision 6 checks after the read on every attempt.
- Replay or cluster-ID guessing: decision 7 persists delivery state and omits cluster ID from the key.
- Hostile reply or receiver outage: decision 7 rejects credential echoes and bounds replies and retries.
- Coordinator compromise: decisions 4 and 6 state the approval limit and bound credential scope and egress.

## Amendments

None.
