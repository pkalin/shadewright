# ADR-0049: One-way handoff of application-code incidents through the coordinator

- Status: Proposed
- Date: 2026-11-24

## Context

Some incidents have their fix in application code. Shadewright's actions are a closed catalog of manifest changes (invariant 2), so such an analysis ends `Escalated` with no proposal. Sablecourt is a separate agent orchestrator from the same organisation; an operator who runs both may want Shadewright to hand it such an incident ([epic #1130](https://github.com/shadewright-labs/shadewright/issues/1130)). That sends cluster data to a receiver whose agents write code.

[ADR-0028](0028-reviewer-hints-through-the-pull-request.md) section 1 and invariant 6 reserve every integration other than the code host for a future `shadewright-coordinator` with its own ADR. This is that ADR, for one integration. The only other outbound path, the `Notifier` of [ADR-0014](0014-notifications-github-issues-first.md), exists as an interface and a test fake (`internal/providers/notify.go`).

## Decision

1. **Scope: one way, off by default.** Shadewright sends a bounded, redacted incident to one receiver and records the receipt. It sends no rollout report and reads nothing back, the receiver's learnings included. This is a limit: the verifier samples only a `RemediationRequest` in `Verifying` (`sampled` in `internal/verifier/reconciler.go`), and tying a rollout to an outside pull request needs an image digest handshake. That handshake, and any read back, get their own ADR once approved handoffs show the classification below is right often enough.

2. **Eligibility is decided by code.** All of these must hold:
   - the subject (Deployment or StatefulSet) is in an install-time allowlist of namespaces and workloads, and an operator-owned mapping gives it a repository alias;
   - the Incident is not folded (`status.foldedInto` empty), ended `Escalated` with `Analyzed` reason `NoProposal` (so no runbook matched), and no `RemediationRequest` names it;
   - none of the exclusions holds: `OOMKilled` or exit 137, `Unschedulable`, `ImagePullBackOff`, `ErrImagePull`, `InvalidImageName`, `CreateContainerConfigError`, `CreateContainerError`, a reference that `internal/analyzer/refcheck` finds `Missing`, an exit of a pod being deleted or exit 143, or an Incident from a policy source (a policy report or an admission refusal). A refusal by policy never becomes another route to a change;
   - positive evidence the analyzer already holds, in at least 3 occurrences of the Incident: a non-zero exit of the same container, a kubelet `BackOff` on it, or `Unhealthy` readiness with a non-empty log excerpt of that container.

   The model's summary may travel as an untrusted hint. It never opens the gate, picks the repository or overrides an exclusion.

   **Each send also needs a person's approval** until the rules have a measured precision. Shadewright already takes human decisions as a spec field set through the API server under Kubernetes RBAC (`spec.cancel` on a request, `spec.paused` on the settings). The ledger (point 7) gets `spec.approved` in the same way: CEL lets it be set and never unset, and never true at creation. The coordinator may create entries and write their status, with no `update` or `patch` on an entry, so it cannot approve its own sends. The chart ships a ClusterRole for approvers.

3. **The sender is `shadewright-coordinator`:** its own Deployment, ServiceAccount, NetworkPolicy and image, off by default. Analyzer, executor and verifier get no new grant. In the operator namespace the coordinator may `get`, `list` and `watch` Incidents, `get` the settings by name (it holds sends while `spec.paused` is true), and `create`, `get`, `list`, `watch` and `delete` its ledger entries, with `update` on their status only. Its egress is DNS, the API server, the storage endpoint and the receiver.

4. **The payload reaches the coordinator as a snapshot, by reference.** After recording the analysis outcome, the analyzer writes an immutable snapshot under the prefix `escalation/` of its record container, through its existing store (`internal/providers/store/azureblob`: create only, checked by the redactor before the write). It puts the blob name, its SHA-256 and a 24 hour expiry in the Incident status it already writes; a failed write leaves a Warning Event and nothing else. The coordinator reads that blob with a storage reader role whose assignment condition limits it to the container and prefix, and refuses a blob whose digest differs. A lifecycle rule deletes snapshots and their versions after a day. The handoff needs a configured record store.

5. **The payload** is JSON, `schemaVersion: 1`, at most 16 KiB serialised. Fields and bounds:
   - `idempotencyKey`: 64 hex characters (point 7);
   - `incident`: `uid` (UUID), `fingerprint` (`sha256:` and 64 hex), `reason` (one word, 64 bytes), `count`, `firstSeen`, `lastSeen` (RFC 3339);
   - `workload`: `kind` (enum), `namespace` (63 bytes), `name` (253 bytes), `container` (63 bytes), `imageDigest` (`sha256:` and 64 hex, or absent);
   - `repositoryAlias`: from the coordinator's own mapping, never from the snapshot, at most 63 bytes, `^[a-z0-9-]+$`;
   - `rules`: rule set version and matched rule IDs, at most 8, from a fixed list;
   - `evidence`: at most 16 lines written by code (exit codes, reasons, restart counts, readiness), 2 KiB in all;
   - `logExcerpts`: at most 2, 4 KiB each, with container, collection time and a cut flag;
   - `untrustedHint`: the model's summary, at most 1 KiB, control characters removed.

   Never sent: raw logs, pod specs, environment values, commands, probe headers, labels, annotations, Secrets, ConfigMaps, image names, URLs, prompts, transcripts and tool results. Every string goes through the redactor's checked path (`StringTwiceChecked`, `internal/redact/before.go`), which withholds a whole entry, never part of one. A withheld excerpt is left out and marked; a withheld required field (identity, workload, evidence) means no snapshot. Before each send the coordinator checks the serialised bytes again: a match ends the entry `Withheld`, and a check that cannot finish sends nothing.

6. **Transport.** One exact HTTPS URL fixed at install, with the controls of [ADR-0042](0042-openai-compatible-model-endpoints.md) (`openaiwire.NewHTTPClient`): no redirects, no proxy from the environment, TLS 1.2 or later, and the dial guard, with private addresses only for a declared in-cluster Service. A CA bundle, when needed, is a read-only mount used as the only root set (no code reads one today). A projected ServiceAccount token with the receiver's audience is preferred. Where the receiver takes only a static token, this ADR names it under the static credential rules of invariant 6: a file mounted only in the coordinator, reread at each attempt, never logged.

7. **Delivery.** The ledger is the custom resource `CodeEscalation` in the operator namespace, named from the Incident UID. Its immutable spec, stored before the first send, holds the Incident reference, the snapshot name and digest, the alias and `idempotencyKey`, the SHA-256 of the cluster ID (`--cluster-id`) and the Incident UID. States move forward only (CEL): `AwaitingApproval`, `Sending`, then `Accepted`, `Refused`, `GaveUp`, `Withheld` or `Expired` (not approved in time). Attempts take 10 s each, at most 6, with jittered backoff from 1 minute to 1 hour; only a network error, 429 or 5xx is retried. After 5 failures in a row sends pause for 15 minutes, and at most 10 go per UTC day (a chart value). The reply is hostile: at most 4 KiB of JSON, a `status` from a fixed enum and a receipt ID matching `^[A-Za-z0-9_-]{1,64}$`; the rest is discarded. Nothing received becomes a prompt, a policy, a command or a trigger. Final entries are deleted after 30 days.

8. **In-cluster remediation is untouched.** The Incident's status stays the analyzer's, closure rules do not change, and the receiver's acceptance never resolves an Incident.

9. **What Shadewright requires of any receiver**, stated in the install docs because Shadewright cannot check it: a credential valid only on its intake route, with a role of its own and never the receiver's operator role; the same receipt for a repeated key and a refusal for another payload under a used key; every field treated as data, never as an instruction to its agents; redaction on receipt; and every code change through a pull request a person reviews.

10. **The model gate is a separate question.** `OpenAICompatible` can already call any HTTPS endpoint or declared in-cluster Service that serves Chat Completions or Responses, with an optional static bearer key (ADR-0042). Gaps confirmed in the code, to become tasks under the epic: system CA roots only; no authentication but a static bearer key, and no correlation header; a declared Service in `100.64.0.0/10` is refused (`PrivateServiceControl`); a reply without complete, bounded usage fails as malformed; spend across analyses is not reserved ([#208](https://github.com/shadewright-labs/shadewright/issues/208)).

## Consequences

- With the handoff off, nothing changes. With it on, an installation runs a fourth Deployment, a new CRD and a cloud read role, while the notifier of ADR-0014 still exists only as an interface.
- Log text an attacker can influence reaches a coding agent on the other side. Redaction, bounds and approval limit what goes but cannot make it harmless; point 9 asks the receiver to treat it as data.
- The rules may not tell a code fault from bad configuration: exit code 1 at start-up looks the same for both. Approval carries that risk until approved and refused handoffs give a precision figure.
- Invariant 6 gains a fourth identity and a static credential; `AGENTS.md` changes with the first build task, after acceptance. Installations without a record store cannot use the handoff.

## Alternatives considered

- The coordinator rebuilds the context with its own `pods/log` read in opted-in namespaces. Rejected: `pods/log` and `get` on pods reach every raw log line and environment value there, unredacted, and a second context builder would drift from the analyzer's. The snapshot read reaches only bounded, redacted objects the analyzer chose to write.
- The analyzer or the executor sends: ADR-0028 keeps other integrations out of both.
- Approval through the code host or chat: the coordinator holds no code host credential, and a new channel needs its own identity checks.
- The model's classification as the gate: refused by invariant 1 and ADR-0023.
- Snapshots in a custom resource: those hold no logs or large payloads.

## Questions for the red team

1. Can the coordinator, the analyzer or another custom resource writer approve a send, or create an approved entry?
2. Can cluster data or model output choose the repository alias or the URL, or lift an exclusion?
3. Does the positive evidence accept a configuration fault that no exclusion catches?
4. Can a policy denial or an admission refusal still lead to a send?
5. Can the snapshot read reach analysis records, or a snapshot swapped after the Incident names it be sent?
6. Can a withheld value reach the receiver in whole or in part?
7. Can a redirect, a proxy, DNS rebinding or a declared Service carry the credential to another host?
8. Can a restart or a lost answer send twice under different keys?
9. Can a reply change anything but its own entry's status, or reach a log, an Event or the model?
10. Can a failing receiver delay or change an analysis, a request or an Incident?
11. Does any grant of point 3 reach more than the handoff needs?

## Amendments

None.
