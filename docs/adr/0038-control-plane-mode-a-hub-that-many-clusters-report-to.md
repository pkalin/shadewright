# ADR-0038: Control plane mode: a hub that many clusters report to

- Status: Proposed
- Date: 2026-11-14

## Context

Shadewright runs in one cluster today. Epic [#559](https://github.com/pkalin/shadewright/issues/559) asks for a second way to run it, for people who operate several clusters: one place that shows what the agents in all of them see and do. Task [#669](https://github.com/pkalin/shadewright/issues/669) asks for the ADR that settles how a cluster gets an identity a hub can check, what it reports, and how the hub reaches it. This is that ADR. Nothing in it is built.

How to read it. Every statement is one of three kinds:

- **Decided** means the maintainer decided it on [#559](https://github.com/pkalin/shadewright/issues/559), on the date given. This ADR only writes it down.
- **Proposed** means this ADR proposes it. It becomes a decision when the ADR is accepted.
- **Open** means nobody has decided it. It is listed under "Open questions" and the ADR does not guess.

"Fleet" in this ADR means the set of clusters. It has nothing to do with the name of the `shadewright-fleet` GitOps repository, which holds the lab's demo workloads.

Two properties of today's design shape the rest:

- The agent works with the model in the same cluster. The analyzer sends redacted context to the model deployment of its own settings ([ADR-0023](0023-how-the-analyzer-calls-the-model.md)), a cluster has its own daily token budget for it (`maxTokensPerDay`, [ADR-0023](0023-how-the-analyzer-calls-the-model.md)), and its own credential for the Anthropic API ([ADR-0037](0037-the-anthropic-api-as-a-second-model-provider.md)).
- Every change goes through Git, and every identity has a narrow set of writes (invariants 3 and 6 in `AGENTS.md`). Any new component has to fit into that without a new way to change a cluster.

## Decision

### 1. Two modes, one set of binaries (Proposed)

There are two modes with the same binaries and the same CRDs:

- **Standalone**, as today.
- **Control plane**: a hub, and clusters that report to it, called spokes in this ADR.

A spoke is a standalone install plus a reporter. It keeps working when the hub is down.

### 2. Where things run (the analyzer, the hub form and "the hub never calls a spoke" are Decided, 2026-11-14; the rest is Proposed)

- **Decided.** The analyzer runs in each spoke. Evidence and prompts never cross to the hub. Analysis works when the hub is down, and no channel into a spoke is needed. Each cluster has its own model credential ([ADR-0037](0037-the-anthropic-api-as-a-second-model-provider.md) for the key) and its own daily budget (`maxTokensPerDay`, [ADR-0023](0023-how-the-analyzer-calls-the-model.md)).
- **Decided.** The hub is a Kubernetes cluster with one namespace per spoke. Hub state goes through a small `StateStore` interface: read and write with compare-and-swap, list by key, a lock with a time limit, and an append-only log. That keeps a hub as a plain service on Azure Table and Blob Storage possible later ([ADR-0013](0013-adapters-for-every-external-system.md)).
- **Decided:** the hub never calls a spoke. **Proposed:** connections are outbound from the spoke, and the hub holds no kubeconfig or spoke credential.
- **Proposed.** Redaction runs in the spoke before anything leaves it. The hub treats everything from a spoke as untrusted input (invariant 4).

[![Control plane mode: two spokes, each with its own GitOps repository and model provider, report outbound to the intake service of the hub; the hub reads its Cluster objects from its own GitOps repository. In the prototype, spoke A is the hub's own cluster, so there the hub and spoke are not separate](../assets/diagrams/control-plane-overview.svg)](../assets/diagrams/control-plane-overview.svg "Open full size")

### 3. Onboarding (Decided, 2026-11-14)

- A new spoke is a pull request that adds its `Cluster` object to the hub's GitOps repository. Whoever may merge there approves it. There is no self-registration.
- The object records the spoke's OIDC issuer URL and the UID of its `kube-system` namespace. The hub accepts reports for a name only from that issuer and UID. A rebuilt cluster needs a reviewed change of the object.
- A suspended or revoked spoke is refused with a clear status, and the refusals are counted in a metric. What it sent before stays readable and is marked stale. Nothing new is stored.
- The spoke learns of it from the refusal. It sets a condition, backs off to a slow retry and keeps working standalone. The hub never calls it.
- A spoke with no reachable issuer (Kind, on-premises): the `Cluster` object carries the public keys (JWKS) that sign the spoke's ServiceAccount tokens, and the hub verifies tokens against them. There is no shared secret and no hub CA. Two decisions of 2026-11-14 apply, the second revising the first. Earlier: a key rotation in the spoke needs a reviewed change. Later: the reviewed merge that registers a cluster happens once, and after it a spoke with inline keys updates its own signing keys at the hub without another pull request (design below, Proposed). The earlier rule now applies only where key continuity is lost.
- **Decided, 2026-11-14:** onboarding must not require copying identifiers by hand. The spoke publishes a ready-to-use registration in the operator namespace. The design that follows is **Proposed**.

#### The registration manifest (Proposed)

- **Proposed.** The reporter keeps a ConfigMap `shadewright-cluster-registration` in the operator namespace (`shadewright-system` by default) with one key, `cluster.yaml`: the complete `Cluster` manifest (section 7) for this spoke, ready to commit to the hub's GitOps repository. It holds:
  - the `clusterId`;
  - the issuer URL, read from the `iss` claim of the reporter's own projected token (the reporter decodes its own token and verifies nothing, it only reads where the cluster says it comes from);
  - `keys: Discovery` or `Inline`;
  - for `Inline`, the cluster's ServiceAccount signing keys, read from the API server's `/openid/v1/jwks`;
  - the reporter's subject and the UID of `kube-system`.
- **Proposed.** A second key, `status`, carries the last result from the hub: accepted, refused with the reason, or registration out of date (key continuity lost, see "Key continuity for Inline"). It is written only by the reporter, from what the hub answered.
- **Proposed.** Which mode it writes: `Discovery` when a chart value says the issuer is reachable from the hub, `Inline` otherwise. The value name is not final (for example `reporter.registration.issuerReachable`). `Inline` is the safe default, because it needs no reachable issuer, which is the case for Kind and on-premises clusters. Whether that default is right is open (question 14).
- **Proposed.** `kubectl shadewright` gets a command that prints this manifest or writes it to a file, for people who do not read ConfigMaps. The name `kubectl shadewright registration` is a proposal and is not final. The hub's status page may show the expected shape of the manifest. No hub component reads the ConfigMap.
- **Proposed.** Onboarding then is three steps: install the spoke with the hub's URL, copy `cluster.yaml` into a pull request on the hub's GitOps repository, merge. Nothing is typed by hand.
- **Proposed.** What a reviewer has to look at: the issuer host (does it belong to the cluster the name claims?) and the cluster name. The subject follows a fixed convention (`system:serviceaccount:<operator namespace>:<reporter service account>`), so a different one is a reason to ask. The `kube-system` UID and the keys are copied, not judged.
- **Proposed.** Rotation for `Inline`: with `keyUpdates: Automatic` (below) a rotation needs no new pull request: the reporter sends the current key set and the hub follows it. The reporter rewrites `cluster.yaml` to match, so a later re-registration starts from current keys. Only when continuity is lost does it set `registration out of date` in `status`, and the hub then refuses the new tokens until a reviewed change merges.

Example, with placeholder values only (illustrative, not final):

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: shadewright-cluster-registration
  namespace: shadewright-system
data:
  cluster.yaml: |
    apiVersion: fleet.shadewright.io/v1alpha1
    kind: Cluster
    metadata:
      name: lab-kind
    spec:
      clusterId: lab-kind
      identity:
        keys: Inline
        issuer: https://issuer.example.com
        subject: system:serviceaccount:shadewright-system:shadewright-reporter
        kubeSystemUID: <uid of the kube-system namespace>
        jwks: |
          { "keys": [ { "kty": "RSA", "kid": "<key id>", "n": "<modulus>", "e": "AQAB" } ] }
  status: |
    result: registration out of date   # or accepted, or refused: <reason>
    reason: continuity lost, the old signing key was removed before the hub saw the new one
```

**The ConfigMap is a convenience, not evidence (Proposed).** Everything in it is public, there is no secret. It is self-reported by the spoke, so whoever can edit it can change what gets pasted. What makes a cluster trusted is the reviewed merge in the hub's repository, as before. For `Discovery` the hub also fetches the keys from the issuer itself, so a wrong issuer in the pasted manifest never verifies a token. **Open, not decided:** a CI check in the hub's repository that fetches the issuer's discovery document for a new `Cluster` object and reports whether keys resolve (question 15).

```mermaid
sequenceDiagram
    participant P as Person with merge rights on the hub repo
    participant R as Hub GitOps repo
    participant F as Flux (hub)
    participant C as Fleet controller
    participant S as Reporter (spoke)
    participant I as Intake
    Note over S: Proposed: the reporter writes the ConfigMap shadewright-cluster-registration (cluster.yaml, status) when it starts
    P->>S: Read cluster.yaml (kubectl shadewright registration, or the ConfigMap)
    P->>R: Pull request with that manifest, copied unchanged
    P->>R: Review (issuer host, cluster name) and merge
    F->>C: Cluster object applied
    C->>C: On a conflict (same issuer and subject, or a shared inline key) refuse both objects
    C->>C: Create the namespace, a RoleBinding to intake's ClusterRole, quota
    C->>C: Set Ready on the Cluster object
    Note over S: The spoke admin installs the reporter with the hub URL and audience, before or after the merge
    S->>I: First report to the path of its cluster, with a projected ServiceAccount token
    I->>I: Load that one Cluster object, verify the token against it, compare the kube-system UID
    I-->>S: Accepted
    S->>S: Record the result in the status key of the ConfigMap
    C->>C: Set Reporting (last seen, versions) on the Cluster object
    Note over S,I: Removing or suspending the object makes the next report a refusal. For inline keys, a rotation is followed by the hub without a new merge (keyUpdates Automatic), and only a lost continuity makes tokens fail until a reviewed change merges, which the status key says
```

#### Key continuity for `Inline` (Proposed, 2026-11-14)

- `spec.identity.jwks` in the reviewed `Cluster` object is the anchor. The hub keeps the keys it currently trusts in `Cluster.status`. Flux does not write status, so Git and the hub do not fight over it.
- A report authenticated with a token signed by a currently trusted key may carry the spoke's current key set, which is what its API server serves at `/openid/v1/jwks`. The hub then trusts the keys in that set and, after a grace period, drops trusted keys that are no longer in it. Kubernetes serves the old and the new key together during a rotation, which makes the handover possible.
- **Never updated by itself:** the cluster name, `clusterId`, issuer, subject and the `kube-system` UID. A rebuilt cluster, or a rotation where the old key was removed before the hub saw the new one, has lost continuity and needs a reviewed change, as before. The reporter's `status` key and the hub's condition say so.
- **Limits and visibility:** a cap on trusted keys per cluster, a minimum interval between changes, a condition and an Event on the `Cluster` object, and a notification for every accepted key change. The uniqueness rule (no two clusters share a signing key, section 6) is checked on every update, and an update that conflicts is refused.
- **A switch per cluster:** `spec.identity.keyUpdates: Automatic` or `Reviewed` (name not final). The proposed default is `Automatic`. `Reviewed` keeps the earlier behaviour: every key change is a reviewed pull request. `Discovery` clusters are unaffected, because the hub already follows their issuer's keys.
- **Trade-off.** Whoever controls a spoke long enough to send one authenticated report can add a key of their own and keep reporting as that spoke after the cluster is cleaned up. The damage is bounded to that spoke's namespace at the hub: false or missing reports. In phase 1 nothing a spoke sends reaches a model, a Git write or another spoke. Every key change is visible, and the remedy is to suspend the `Cluster` object and register again with a reviewed change. `Discovery` has the same exposure through the issuer. This bound must be re-examined before any later phase lets the hub act on spoke data.

### 4. Components (Proposed)

In each spoke:

- **`reporter`**, a fourth identity (invariant 6). Its reads, exactly:
  - In the operator namespace: Incidents, RemediationRequests and VerificationReports.
  - Cluster-scoped: `get` on `remediationsettings`, and `get` on the Namespace `kube-system` by `resourceNames` (for its UID).

  Its writes, exactly (Proposed): `get` and `update` on the one ConfigMap `shadewright-cluster-registration`, by `resourceNames`, and `get` on the non-resource URL `/openid/v1/jwks`. `create` cannot be limited by name, so the chart creates the ConfigMap, empty, and the reporter never creates anything. No other new grant.

  It has no Secrets, no Git identity and no model credential. Its egress goes only to the hub's host. It reads the settings status only to report it, never to decide anything (`AGENTS.md`, invariant 6).

At the hub:

- **`intake`**: an HTTPS service, stateless, N replicas. It verifies the token, validates the report and stores it.
- **`fleet controller`**: reconciles `Cluster` objects into a namespace, a RoleBinding for intake in that namespace, an object-count quota and staleness conditions.
- **A read-only status page**, and later the notifier ([#97](https://github.com/pkalin/shadewright/issues/97)).

In phase 1 the hub has no model credential and no Git write identity.

**Hub RBAC (Proposed).**

- The chart ships one static ClusterRole for intake's writes (the reported kinds). The fleet controller never creates Roles.
- The fleet controller creates Namespaces, ResourceQuotas and RoleBindings only. Its `bind` verb is limited by `resourceNames` to that one ClusterRole, and it has no `escalate`.
- RBAC cannot limit `create` on Namespaces by name. So a compromised fleet controller could create any namespace. A ValidatingAdmissionPolicy that limits the controller to the spoke namespace prefix is proposed to close this.
- **RBAC does not enforce the boundary between spokes.** Intake is one ServiceAccount, bound in every spoke namespace. The guard is intake's code: the namespace a report is written to comes from the verified `Cluster` object, never from the report or the request path. The build needs tests that a report cannot reach another cluster's namespace. Per-namespace impersonation by intake would move the guard into RBAC; it is an open question.
- No hub identity is `cluster-admin`.

[![Identities of control plane mode and what each may write: the three spoke identities keep their writes, the reporter reads, sends reports and updates only its own registration ConfigMap, intake writes only the reported kinds in the namespace of the cluster it verified, the fleet controller creates namespaces, ResourceQuotas and RoleBindings only, intake's writes are checked in its code and not by RBAC, and no hub component has a model credential or a Git write](../assets/diagrams/control-plane-identities.svg)](../assets/diagrams/control-plane-identities.svg "Open full size")

### 5. An intake service instead of the hub's Kubernetes API (Proposed)

The spoke posts to intake, and intake writes with its own identity. Reasons:

- Onboarding a spoke as an API server JWT authenticator would be a change to the cloud control plane, not only a reviewed merge in the hub's GitOps repository. On AKS, structured authentication offers JWT authenticators in preview ([Microsoft Learn](https://learn.microsoft.com/azure/aks/external-identity-provider-authentication-overview)), configured per authenticator through the Azure API (`az aks jwtauthenticator add`), and it requires a publicly accessible discovery endpoint (vendor docs, checked 2026-11-14). It is a preview feature and specific to one vendor. Other vendors were not checked.
- Inline keys have no path through an API server authenticator.
- Validation and rate limits would have no single place; at intake they sit in one.
- A hub as a plain service later changes nothing for spokes.

"Spokes write to the hub's API server directly" is recorded as the rejected alternative (below). The transport stays behind an interface ([ADR-0013](0013-adapters-for-every-external-system.md)).

### 6. How intake checks a spoke (Proposed)

**Finding the `Cluster` object.** Kind and kubeadm clusters share the default issuer, and every install has the same reporter subject, so issuer plus subject cannot identify a spoke that uses inline keys. The reporter therefore posts to a path that names its cluster. The name is a hint and is not trusted: intake loads that one `Cluster` object and verifies the token against that object's issuer, keys and subject. The name only selects what to verify against.

The reporter sends a projected ServiceAccount token with an audience that names this hub and a lifetime of about 10 minutes. Intake checks the signature, `iss`, `sub`, the audience and the expiry:

- The keys come from the issuer's discovery document, or from the inline keys of the `Cluster` object.
- `iss` and `sub` must equal those in the object.

The `kube-system` UID travels in the report and is compared with the object. This is not authentication. It guards against a rebuilt cluster that reuses an issuer or keys. The cluster ID inside a report is compared with the object too, and a mismatch is refused. The namespace a report is written to comes from the verified `Cluster` object in intake's code, never from the report.

**Order of checks.**

- Before the token verifies: a rate limit per source address, then one generic refusal with no reason. An unauthenticated request triggers no discovery fetch. Keys for registered `Cluster` objects are fetched on a schedule and cached, and an unknown `kid` triggers at most one refresh per cluster per interval.
- After it verifies: the per-cluster rate limit and the named reasons (suspended, revoked, over quota).

Issuer discovery is a fetch of a URL taken from a reviewed object, with these limits: https only, no redirects, size and time limits, a cache, and an issuer host that must match an allow-list of suffixes in the hub's settings.

**Uniqueness.** For `Discovery`, issuer plus subject must be unique. For `Inline`, no two objects may share a signing key. On a conflict the fleet controller refuses both objects (condition `IdentityConflict`). The older one does not stay: neither is accepted until a reviewed change resolves it.

### 7. The `Cluster` object (Proposed)

API group `fleet.shadewright.io/v1alpha1`, cluster-scoped, applied by Flux from the hub's GitOps repository.

- `spec.clusterId`: equal to the spoke's `clusterId`.
- `spec.identity`: `issuer`, `keys` (`Discovery` or `Inline`), `jwks` (the anchor for `Inline`), `keyUpdates` (`Automatic` or `Reviewed`, `Inline` only), `subject`, `kubeSystemUID`.
- `spec.suspended`.
- Status conditions `Ready`, `Reporting` (last seen) and `Stale`, the reported versions, and for `Inline` the currently trusted keys.

This sketch is illustrative and not final. Field names, validation and the way a value is derived are for the build task.

```yaml
apiVersion: fleet.shadewright.io/v1alpha1
kind: Cluster
metadata:
  name: lab-kind
spec:
  clusterId: lab-kind
  suspended: false
  identity:
    keys: Inline            # or Discovery, which uses the issuer's discovery document
    issuer: https://issuer.example.com
    subject: system:serviceaccount:<operator namespace>:<reporter service account>
    kubeSystemUID: <uid of the kube-system namespace>
    jwks: |
      { "keys": [ { "kty": "RSA", "kid": "<key id>", "n": "<modulus>", "e": "AQAB" } ] }
```

### 8. What a spoke reports in phase 1 (Proposed)

A typed allow-list, not the objects themselves:

- A **heartbeat**: operator, chart and pack versions, module status, and the delivery source report of `doctor`.
- **Summaries of Incidents**: fingerprint, subject kind, namespace and name, reason, phase, times.
- **Summaries of requests**: action type, target, phase, pull request URL, condition reasons.
- **Outcomes.**

There is no evidence, no prompt, no transcript, and no free text from cluster content beyond bounded, redacted fields. A setting lets a spoke send names hashed.

The hub stores these as kinds of their own. Their working names are `ReportedIncident` and `ReportedRequest`. They are never stored as `Incident` or `RemediationRequest`: no hub controller may act on an object a spoke supplied (invariants 3 and 4).

### 9. Delivery (Proposed)

Delivery is level-based and at least once.

- The reporter sends the current state on change, and a full list periodically.
- Intake upserts by the existing deterministic names (invariant 5) and drops an out-of-order write. How it orders writes is open (see the open questions).
- The reporter keeps nothing it cannot rebuild from the cluster's objects.

**Candidate for ordering (Proposed, open question 6).** Each report carries the reporter's epoch (its start time) and a counter. Intake accepts only a pair higher than the last one stored for that cluster. The epoch is compared only with other epochs from the same reporter, never with the hub's clock.

**Full lists.** A full list is a snapshot, sent as numbered pages with a completion marker. Nothing is marked gone until the snapshot is complete. A snapshot cut short by the quota, or by a lost page, marks nothing gone.
 Staleness is decided from recorded state on one clock: the time the hub itself recorded when it accepted a heartbeat, compared with the hub's clock. A time a spoke reports is shown, never used to decide whether data is stale.

### 10. Limits against a noisy or compromised spoke (Proposed)

- A rate limit per source address before the token verifies, and one per cluster after. A body size limit at intake.
- An object-count quota per spoke namespace.
- Bounded field lengths.
- Schema validation, with unknown fields refused.
- The status page escapes everything. The notifier fences spoke text and neutralises mentions.
- Nothing a spoke sends reaches a model or a Git write in phase 1.

### 11. Kill switch (Proposed)

Per cluster: `spec.suspended` on its `Cluster` object. Fleet-wide: the hub's settings pause intake. In phase 1 both only stop reporting.

### 12. The rule for later phases: the hub can only make a spoke do less (Proposed)

A reply to a report may carry restrictive directives (pause remediation, slow down), never permissive ones. So a compromised hub can stop remediation in a spoke, but cannot cause it.

A directive from the hub is held apart from the spoke's own kill switch (`docs/emergency.md`) and only adds to it. Lifting the hub's pause never lifts a local pause, and only the spoke's operator lifts a local one.

Configuration for a spoke (pack references, model profiles, policies) travels as a pull request to that spoke's GitOps repository (invariant 3), never as a directive.

### 13. Phases (Proposed)

1. **Fleet view**: reporter, intake, `Cluster`, status page, staleness, notifications. This is what the rest of this ADR specifies.
2. **Guard rails**: restrictive directives, a fleet-wide pause, configuration by pull request.
3. Each of these has its own ADR and is not decided here:
   - Remediation through the hub: a central executor holding the Git App. It would have to re-evaluate spoke proposals against policy held at the hub, treating them as untrusted input.
   - Horizontal scaling by key ownership.
   - The `StateStore` on Table and Blob Storage.

### 14. Prototype (Proposed)

One AKS lab cluster is the hub and also a spoke of itself (issuer discovery). A local Kind cluster is a second spoke (inline keys, outbound to the hub). So no second AKS cluster is needed.

**The prototype does not test the hub and spoke boundary.** With hub and spoke in one AKS cluster, the separation between them does not hold for that cluster: the hub's components share it with an executor that holds the Git App. The prototype tests registration and reporting, not that boundary. The Kind spoke is the only one that is separate from the hub. The cost has to be priced with `make lab-cost` in `shadewright-azure` before anything is built; this ADR gives no number.

### One report, from token to stored record

```mermaid
sequenceDiagram
    participant R as Reporter (spoke)
    participant I as Intake (hub)
    participant K as Cached issuer or inline keys
    participant N as Spoke namespace (hub)
    R->>R: Build the typed summaries from the cluster's objects, redact, bound the fields
    R->>R: Project a ServiceAccount token (audience: this hub, about 10 minutes)
    R->>I: HTTPS POST to the path naming its cluster, with the token and the report
    I->>I: Refuse an oversize body or a source address over its rate limit
    I->>I: Load the one Cluster object the path names (a hint, not trusted)
    I->>K: Keys from the cache, no fetch triggered by this request
    alt No such object, or the token does not verify against that object
        I-->>R: One generic refusal, no reason
    else Token verifies
        I->>I: Per-cluster rate limit
        alt Suspended, revoked, over quota, rate limited, or a kube-system UID or cluster ID that differs from the object
            I-->>R: Refused with a named reason, counted in a metric
            R->>R: Set a condition, back off to a slow retry, keep working standalone
        else Accepted for storage
            I->>I: Validate the schema, refuse unknown fields and over-long fields
            I->>N: Write into the namespace the Cluster object gives, upsert by deterministic names, drop an out-of-order write
            I-->>R: Accepted
        end
    end
    Note over I,K: Keys of registered clusters are fetched on a schedule and cached. An unknown kid causes at most one refresh per cluster per interval.
    Note over I,N: The hub records its own receipt time for the staleness conditions
```

Whether a refusal that is not about the cluster's state (a rate limit, a malformed body) tells the reporter to back off the same way as a refusal for a suspended cluster is open. The status codes and reason names are for the build task.

## How invariants 1 to 6 hold in phase 1

1. **Model output is never authorization.** The hub has no model credential, and nothing a spoke sends reaches a model there. The analyzer and the policy gate in each spoke are unchanged.
2. **Actions are a closed catalog.** Phase 1 adds no action and no way to ask for one. A report is data about requests that already exist. The directives of phase 2 can only restrict.
3. **Changes go through Git only.** The hub writes nothing to a spoke and nothing to any Git repository in phase 1. Automatic key updates change the hub's own trust state in `Cluster.status`, not a cluster, so this invariant is not touched. A new spoke is a reviewed pull request to the hub's repository. Configuration for a spoke, when it comes, is a pull request to that spoke's repository.
4. **Everything from the cluster is untrusted input.** The spoke redacts before anything leaves it. The hub validates again, bounds every field, stores spoke objects as kinds of their own that no controller acts on, and escapes or fences what it shows or sends.
5. **Reconciliation is idempotent.** Delivery is at least once. Intake upserts by deterministic names, so a repeated or replayed report changes nothing. Dropping an out-of-order report depends on the ordering rule, which is an open question (6); the candidate is an epoch and counter pair per report.
6. **Separate identities.** The reporter is a fourth identity with read-only access on the cluster's objects and no Secret, Git identity or model credential. Its only writes are `get` and `update` on its one registration ConfigMap by `resourceNames`, plus `get` on the non-resource URL `/openid/v1/jwks`. At the hub, intake writes only the reported kinds, and the fleet controller creates Namespaces, ResourceQuotas and RoleBindings only, with `bind` limited to one ClusterRole and no `escalate`. RBAC does not keep intake to one namespace, because it is one ServiceAccount bound in every spoke namespace: that guard is intake's code, which takes the namespace from the verified `Cluster` object, and it needs tests. No hub component has a model credential or a Git write identity. The build PR has to add the reporter, intake and the fleet controller to invariant 6; this ADR does not edit `AGENTS.md`.

## Threats and what stops each

| Threat | What stops it | What is left |
|---|---|---|
| A cluster pretends to be another spoke | The path names a `Cluster` object, a hint that only selects what to verify against. The token is checked against the keys, `iss` and `sub` of that reviewed object, with an audience that names this hub. The namespace written to comes from the object in intake's code, and a cluster ID in the report that differs is refused. The `kube-system` UID is compared too, but it only guards against a rebuilt cluster. | Anyone who can mint a token for the reporter's ServiceAccount in the spoke can report as that spoke. RBAC does not stop a bug in intake's code from writing to another namespace; tests and the open question on impersonation cover it. |
| A name is claimed twice | A `Cluster` is a reviewed merge. For `Discovery`, issuer plus subject must be unique; for `Inline`, no two objects may share a signing key. On a conflict both objects are refused (`IdentityConflict`). | Two objects with the same `clusterId` but different issuers: open. |
| Token replay | The audience names this hub and the lifetime is about 10 minutes. A replayed report upserts the same state. Dropping an older one depends on the open ordering question. | A captured token can be used until it expires, from anywhere that reaches intake. No nonce is proposed. |
| A flooding or poisoning spoke, or an unauthenticated flood | Before the token verifies: a rate limit per source address, one generic refusal with no reason, and no discovery fetch from an unauthenticated request. After: a per-cluster rate limit with named reasons. Body size, an object-count quota per namespace, bounded fields, schema validation that refuses unknown fields. Nothing reaches a model or Git in phase 1. | A spoke can fill its own quota with false records and show wrong data about itself. |
| Stale data read as current | Staleness is decided from the hub's own recorded receipt time. A suspended or revoked spoke's data stays readable and is marked stale. The status page shows when the hub last accepted a report. | A viewer who ignores the mark. |
| A compromised hub | It holds no kubeconfig or spoke credential, never calls a spoke, and has no Git write identity in phase 1. Replies carry only accepted or refused, and later only restrictive directives. | It can show false data and send false notifications. It can pause remediation in later phases. It cannot cause an action. In the prototype, hub and spoke share one AKS cluster with an executor that holds the Git App, so this row does not hold for that cluster. |
| Injection through the status page or notifications | Fields are bounded and redacted in the spoke. The status page escapes everything. The notifier fences spoke text and neutralises mentions. | Not built; the build tasks need tests for each. |
| A tampered registration ConfigMap (Proposed) | The ConfigMap is a convenience, not evidence. A cluster is trusted only after the reviewed merge of its `Cluster` object, where the reviewer checks the issuer host and the name. For `Discovery` the hub fetches keys from the issuer itself, so a wrong issuer never verifies a token. Everything in it is public. A CI check on new `Cluster` objects is proposed as optional and is not decided. | Whoever can edit the ConfigMap can change what gets pasted. A reviewer who merges without looking approves it. With `Inline`, wrong keys that get merged make the hub trust whoever holds the matching private key. |
| A spoke adds a key of its own through automatic key updates (Proposed) | Updates are accepted only on a report whose token a currently trusted key signed. The identity fields never change by themselves. A cap on trusted keys, a minimum interval between changes, the uniqueness check on every update, a condition, an Event and a notification for each change. `keyUpdates: Reviewed` turns it off per cluster. | Whoever controls a spoke long enough to send one authenticated report can keep reporting as it after cleanup. The damage is bounded to that spoke's namespace in phase 1. The remedy is to suspend the object and register again by a reviewed change. `Discovery` has the same exposure through the issuer. Re-examine before any phase lets the hub act on spoke data. |
| SSRF through issuer discovery | The URL comes only from a reviewed object. https only, no redirects, size and time limits, a cache, and an allow-list of host suffixes in the hub's settings. | Whether to refuse private or link-local addresses after name resolution: open. |

## Consequences

**Easier.** One view of all clusters, with no evidence, prompts or credentials gathered in one place. Each spoke is as safe as it is alone: it analyses, proposes and remediates with its own credential and budget, whether the hub is up or not. Adding a cluster uses the review path of every other change.

**Harder.**
- There is a new API group, a new set of components to build and operate, and a fourth identity in the spokes, with an `AGENTS.md` change in the build PR.
- The hub is a service that spokes depend on for the fleet view only. Its availability is an open question.
- A spoke rebuild, or a rotation that loses key continuity, needs a reviewed change at the hub.
- Hashed names (an optional setting) make the fleet view harder to read.

**Accepted.**
- In phase 1 the fleet view is read-only. The hub shows and notifies and proposes nothing.
- Reports are as true as the spoke that sends them. A compromised spoke can lie about itself.
- With `keyUpdates: Automatic` (the proposed default), a spoke that was controlled once can keep its place after cleanup by having added a key. This is bounded and visible (see "Key continuity for Inline") and is accepted for phase 1 only.
- Cost: the prototype must be priced with `make lab-cost` before it is built.

## Alternatives considered

- **Spokes write to the hub's Kubernetes API directly.** Rejected. Onboarding would be an Azure control plane change on AKS (JWT authenticators are a preview feature there, configured per authenticator through the Azure API, and they need a public discovery endpoint; other vendors were not checked), inline keys have no path, validation and rate limits would have no single place, and a plain-service hub would later change every spoke.
- **The analyzer at the hub.** Evidence and redacted prompts would cross to the hub, analysis would stop when the hub is down, a channel into every spoke would be needed to read evidence on demand, and one model credential and budget would serve all clusters. Decided against on 2026-11-14.
- **The hub as a plain service from the start.** It needs a `StateStore` on Table and Blob Storage before there is a reason to have one, and gives up namespaces, RBAC, quotas and Flux for the hub's own configuration. The `StateStore` interface keeps this open for phase 3.
- **A bootstrap token exchanged once for a client certificate, as `kubeadm join` does.** It needs a shared secret to hand out and protect, a hub CA, and certificate rotation. The reviewed object with inline public keys needs none of them. It was a fallback candidate on [#559](https://github.com/pkalin/shadewright/issues/559) and was replaced by the decision of 2026-11-14.
- **A message broker (Service Bus, NATS) now.** It adds a component, a credential per spoke and a cost, to carry level-based reports that are a small HTTPS post. Not needed for phase 1. It can come back behind the same transport interface.
- **Reusing the `Incident` kind at the hub.** A hub controller that reconciles `Incident` or `RemediationRequest` could act on an object a spoke supplied, which breaks invariants 3 and 4, and the same name would mean two things. The hub stores `ReportedIncident` and `ReportedRequest`, which nothing acts on.

## Open questions

1. Where the executor and the Git App sit in the phase that remediates through the hub, and how the hub's policy evaluates a spoke's proposal (phase 3, its own ADR).
2. Hub high availability: how many replicas of intake, what the state store gives, and what the status page shows while the hub restarts.
3. Retention of reported records: how long they stay, and what happens to a removed spoke's namespace.
4. Whether hashed names should be the default instead of an option.
5. Multi-tenancy at the hub: who may read which cluster's namespace, and whether the status page shows every cluster to every viewer.
6. How intake orders reports. An object's `resourceVersion` is opaque and must not be ordered. The candidate is the reporter's epoch plus a counter (section 9), and it is not chosen. How a reporter keeps the counter across a restart is part of it.
7. Where the reporter records the condition it sets after a refusal. It has no write on Incidents or requests. **Proposed answer:** the `status` key of its registration ConfigMap (section 3), which it may update by `resourceNames` and which needs no other grant. Open until the ADR is accepted.
8. How the heartbeat gets the delivery source report of `doctor`. `doctor` is a kubectl plugin that runs with a person's permissions and reads Flux objects, and the reporter's reads above do not include them. Either the reporter gets that read, or the report comes from state another component already records.
9. Whether `spec.clusterId` must equal the object name, and the rule for the name of a spoke's namespace.
10. Whether a refusal for a rate limit or a malformed body makes the reporter back off like a refusal for a suspended cluster, the reason names, and the leeway for clock difference when intake checks a token's expiry.
11. Whether intake refuses private and link-local addresses after name resolution of an issuer host, besides the host allow-list.
12. Whether intake should impersonate a per-namespace identity when it writes, so that RBAC enforces the boundary between spokes, and the cost of that.
13. The price of the prototype ([#669](https://github.com/pkalin/shadewright/issues/669)): `make lab-cost` in `shadewright-azure`.
14. Whether the chart value that picks `Discovery` or `Inline` for the registration manifest has `Inline` as its default, and its name.
15. Whether the hub's repository gets a CI check that fetches the issuer's discovery document for a new `Cluster` object and reports whether keys resolve, and who owns it.
16. The name of the command that prints the registration (`kubectl shadewright registration` is a proposal), and whether the hub's status page shows the expected shape of the manifest.
17. Whether `keyUpdates: Automatic` is the right default once phase 2 or 3 exists, when the hub can act on what spokes send, and the name, the cap, the minimum interval and the grace period of the key updates.

## Amendments

None.
