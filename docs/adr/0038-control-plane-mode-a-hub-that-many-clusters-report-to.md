# ADR-0038: Control plane mode: a hub that many clusters report to

- Status: Proposed
- Date: 2026-11-14

## Context

Shadewright runs in one cluster today. Epic [#559](https://github.com/shadewright-labs/shadewright/issues/559) asks for a second way to run it, for people who operate several clusters: one place that shows what the agents in all of them see and do. Task [#669](https://github.com/shadewright-labs/shadewright/issues/669) asks for the ADR that settles how a cluster gets an identity a hub can check, what it reports, and how the hub reaches it. This is that ADR. Nothing in it is built. Shadewright 0.7.0 is to ship the read-only fleet view of phase 1 (section 13) on it.

How to read it. Every statement is one of three kinds:

- **Decided** means the maintainer decided it on [#559](https://github.com/shadewright-labs/shadewright/issues/559), on the date given. This ADR only writes it down.
- **Proposed** means this ADR proposes it. It becomes a decision when the ADR is accepted.
- **Open** means nobody has decided it. It is listed under "Open questions" and the ADR does not guess.

"Fleet" in this ADR means the set of clusters. It has nothing to do with the name of the `shadewright-fleet` GitOps repository, which holds the lab's demo workloads.

Two properties of today's design shape the rest:

- The agent works with the model in the same cluster. The analyzer sends redacted context to the model deployment of its own settings ([ADR-0023](0023-how-the-analyzer-calls-the-model.md)), a cluster has its own daily token budget for it (`maxTokensPerDay`, [ADR-0023](0023-how-the-analyzer-calls-the-model.md)), and its own credential for the Anthropic API ([ADR-0037](0037-the-anthropic-api-as-a-second-model-provider.md)).
- Every change goes through Git, and every identity has a narrow set of writes (invariants 3 and 6 in `AGENTS.md`). Any new component has to fit into that without a new way to change a cluster.

On 2026-11-28 the maintainer narrowed the design twice. After three review passes that each found new gaps in per-spoke hub namespaces, the hub got one namespace for every spoke. After a fourth pass found gaps in per-record reported kinds and multi-page snapshots (ordering, a sweep across stateless replicas, a shared quota one spoke could fill), each spoke now sends one bounded snapshot of its state, which replaces that cluster's previous snapshot in one write. The dropped designs are under "Alternatives considered".

## Decision

### 1. Two modes, one set of binaries (Proposed)

There are two modes with the same spoke binaries plus hub binaries, and the same CRDs:

- **Standalone**, as today.
- **Control plane**: a hub, and clusters that report to it, called spokes in this ADR.

A spoke is a standalone install plus a reporter. It keeps working when the hub is down.

### 2. Where things run

- **Decided, 2026-11-14.** The analyzer runs in each spoke. Evidence and prompts never cross to the hub. Analysis works when the hub is down, and no channel into a spoke is needed. Each cluster has its own model credential ([ADR-0037](0037-the-anthropic-api-as-a-second-model-provider.md) for the key) and its own daily budget (`maxTokensPerDay`, [ADR-0023](0023-how-the-analyzer-calls-the-model.md)).
- **Decided, 2026-11-14:** the hub is a Kubernetes cluster. **Decided, 2026-11-28, replacing the `StateStore` interface of 2026-11-14:** hub state is Kubernetes objects, with no storage interface of its own. A hub as a plain service would need its own ADR. One hub namespace (`shadewright-hub` by default) holds intake, the fleet controller, the hub's settings and one snapshot object per spoke. The status page runs in a namespace of its own (section 4).
- **Decided:** the hub never calls a spoke. **Proposed:** connections are outbound from the spoke, and the hub holds no kubeconfig or spoke credential.
- **Proposed.** Redaction runs in the spoke before anything leaves it, with the existing redactor (`internal/redact`, moving to a shared module under [ADR-0048](0048-redaction-in-a-shared-module.md); [design 10.3](../design/10-security.md#103-redaction)), and again at the hub's intake, which refuses a snapshot it would change (section 10). The hub treats everything from a spoke as untrusted input (invariant 4).

[![Control plane mode: two spokes, each with its own GitOps repository and model provider, send a snapshot outbound to the intake service of the hub; intake replaces that cluster's one snapshot object in the hub namespace, the fleet controller reads the Cluster objects from the hub's GitOps repository, and the status page runs in its own namespace. In the prototype, spoke A is the hub's own cluster, so there the hub and spoke are not separate](../assets/diagrams/control-plane-overview.svg)](../assets/diagrams/control-plane-overview.svg "Open full size")

### 3. Onboarding (Decided, 2026-11-14, except where marked)

- A new spoke is a pull request that adds its `Cluster` object to the hub's GitOps repository. Whoever may merge there approves it. There is no self-registration. **Decided, 2026-11-28:** that object is all onboarding adds; there is no Namespace, RoleBinding or ResourceQuota per spoke.
- The object records the spoke's OIDC issuer URL and the UID of its `kube-system` namespace. The hub accepts reports for a name only from that issuer and UID. A rebuilt cluster needs a reviewed change of the object.
- A spoke with no reachable issuer (Kind, on-premises): the object carries the public keys (JWKS) that sign the spoke's ServiceAccount tokens. There is no shared secret and no hub CA. A key rotation in the spoke needs a reviewed change. (A later decision of the same day let inline keys change at the hub without a pull request. This ADR does not take it up, see "Alternatives considered"; accepting the ADR replaces that decision.)
- **Decided, 2026-11-14:** onboarding must not require copying identifiers by hand. **Decided, 2026-11-28, replacing the registration published in the operator namespace:** the registration is a command (below), and the spoke keeps no registration object.
- **Suspending (Proposed).** `spec.suspended: true` is the reversible stop: a spoke whose token verifies is refused with the named reason `suspended`, counted in a metric. Its last snapshot stays readable, marked stale. Setting it back to `false` resumes reporting.
- **Revoking (Decided, 2026-11-28).** A pull request sets `spec.revoked: true` and the object stays. Intake refuses every report for it with the generic refusal an unknown name gets, so a caller cannot tell a revoked name from one never registered. The status page lists the cluster as revoked, with the time of its last accepted snapshot, which stays readable. **Proposed:** a CEL transition rule keeps `revoked` true once set; a cluster that should report again registers under a new name. Deleting the object and its snapshot is a later reviewed change, after the retention (proposed: 30 days), not designed here.
- **Adopting a name again (Proposed).** The snapshot carries the UID of the `Cluster` object intake verified against, and the status page shows it only when that UID equals the current object's. When a new object appears for a name whose snapshot has another UID, the fleet controller deletes that snapshot before it sets `Ready`.
- The spoke learns of a suspension from the named refusal, and of a revocation only from the generic one. The reporter records the answer in a metric and its log, backs off and keeps working standalone.

#### The registration command (Proposed)

`kubectl shadewright registration` (name proposed) runs with the onboarding person's own kubeconfig for the spoke and reads every identity field live: the issuer from the API server's `/.well-known/openid-configuration`, the subject from the ServiceAccount of the reporter's Deployment, the UID from the `kube-system` Namespace, the `clusterId` from the install's settings and, for `Inline`, the keys from `/openid/v1/jwks`. It writes the onboarding change, the complete `Cluster` manifest (section 7), as one file in a local checkout of the hub's repository (`--hub-repo <path>`) or to standard output, and prints the fields for the pull request description. It does not commit or push. `--keys` picks `Discovery` or `Inline`; `Inline` is the default because it needs no issuer the hub can reach (question 14). Onboarding is: install the spoke with the hub's URL, run the command, open the pull request, merge.

The reviewer checks the full issuer URL (for AKS, against `oidcIssuerProfile.issuerUrl`), the cluster name, and the subject, which follows `system:serviceaccount:<operator namespace>:<reporter service account>`. The command shows what its runner's kubeconfig showed; the reviewed merge is what makes a cluster trusted. **Open:** a CI check in the hub's repository that fetches the issuer's discovery document for a new object (question 15).

Rotation for `Inline` is two reviewed changes: one adds the new public key while the old key still signs, and one removes the old key after the spoke signs with the new one. Kubernetes verifies with several keys and signs with one, so there is no gap. Running the command again prints the keys.

```mermaid
sequenceDiagram
    participant P as Person with merge rights on the hub repo
    participant R as Hub GitOps repo
    participant C as Fleet controller
    participant S as Reporter (spoke)
    participant I as Intake
    P->>S: kubectl shadewright registration: reads every identity field live from the spoke
    P->>R: Pull request with the Cluster manifest the command wrote; review and merge
    R->>C: Flux applies the Cluster object
    C->>C: Check the keys and uniqueness across all Cluster objects; on a conflict refuse both
    C->>C: Fetch issuer keys (Discovery), delete a snapshot of an earlier Cluster UID, set Ready for this generation
    S->>I: Snapshot to the path of its cluster, with a projected ServiceAccount token
    I->>I: Load that one Cluster object, require Ready at its generation, verify the token
    I-->>S: Accepted
    C->>C: Set Reporting (last seen, versions) from the snapshot
    Note over S,I: Suspending gives the named refusal suspended, revoking the generic one. Any change of inline keys is a reviewed pull request.
```

### 4. Components (Proposed)

In each spoke, **`reporter`**, a new identity, `shadewright-reporter` (invariant 6). One replica with the `Recreate` strategy, so two reporters of one cluster never send at once. Its grants, exactly:

- In the operator namespace: `get`, `list` and `watch` on `incidents` and `remediationrequests` (API group `remediation.shadewright.io`).
- Cluster-scoped: `get` on `remediationsettings`, and `get` on the Namespace `kube-system` by `resourceNames` (for its UID).
- For its metrics endpoint: `create` on `tokenreviews` and `subjectaccessreviews` through the shared metrics-auth ClusterRoleBinding, as the verifier has.
- **Open (question 8):** the Flux reads the delivery report (section 8) needs. The candidate is `get` and `list` on `gitrepositories` and `kustomizations`, cluster-wide or in the Flux namespace. Until that is decided it has none.

It writes nothing in the cluster, and has no Secrets, Git identity or model credential. Ingress, by NetworkPolicy: its metrics port only, from the monitoring namespace the chart names. Egress: DNS, the API server (443 and 6443, as in design 10) and TCP 443 for the hub. A NetworkPolicy cannot match host names ([design 10.6](../design/10-security.md#106-network)), so the hub's host is pinned in the reporter's settings, and it takes no URL from cluster data or the hub's replies. Its client follows no redirects, ignores proxy settings from the environment and requires verified TLS 1.2 or later. It reads the settings status only to report it (invariant 6).

At the hub, three identities, none `cluster-admin`, none with RBAC on Secrets, a model credential or a Git write identity. The hub's settings are one ConfigMap, `shadewright-hub-settings`, in the hub namespace, applied by Flux. The snapshot kind is `ClusterSnapshot` (working name, section 8). Intake and the fleet controller get `create` on `tokenreviews` and `subjectaccessreviews` for their metrics endpoints, and an ingress NetworkPolicy that allows their metrics port only from the monitoring namespace.

- **`intake`**: an HTTPS service, stateless, N replicas. Cluster-wide `get`, `list` and `watch` on `clusters`, no write on them. In the hub namespace `get`, `create` and `update` on `clustersnapshots` (no `delete`), and `get`, `list` and `watch` on the settings ConfigMap by `resourceNames`. It terminates TLS 1.2 or later itself, with a certificate and key mounted read-only from a Secret the hub's operator provides (by hand or from cert-manager) and read again when the file changes; it has no RBAC on Secrets. A Service of type `LoadBalancer` with `externalTrafficPolicy: Local` exposes it, so the peer is the caller's address, and its ingress NetworkPolicy also allows its HTTPS port from any address. A proxy in front is trusted only when its addresses are listed in the hub's settings (section 6). It is not the scoring intake of ADR-0047, and the binary is not `cmd/intake`.
- **`fleet controller`**: the only writer of `Cluster.status` (section 7). Cluster-wide `get`, `list` and `watch` on `clusters` and `update` on `clusters/status`. In the hub namespace `get`, `list`, `watch` and `delete` on `clustersnapshots` (delete only for the clean-up of section 3), `get`, `list` and `watch` on the settings ConfigMap by `resourceNames`, its leader election Lease and core Events.
- **`status page`**: its own identity `shadewright-fleet-status` in its own namespace (`shadewright-hub-status` by default), with `get`, `list` and `watch` on `clusters`, and on `clustersnapshots` through a RoleBinding in the hub namespace. No write and no metrics endpoint. It has no Service, Ingress or LoadBalancer and listens on 127.0.0.1 only. Its namespace has a default-deny NetworkPolicy with no ingress rule; kubelet health probes go to a port that serves only health. A person reaches it with `kubectl port-forward`, so the hub's API server authenticates the person and RBAC (`create` on `pods/portforward` in that namespace, which reaches nothing else) decides who may. Whoever reaches it sees every cluster (question 5).

**Egress (Proposed).** No hub binary makes an external call except the fleet controller's key fetch (section 6). Following [design 10.6](../design/10-security.md#106-network), intake and the status page get TCP 443 and 6443 for the API server and no DNS; the fleet controller gets DNS and TCP 443 and 6443. A standard NetworkPolicy cannot select the API server, so these allow any address on those ports unless the operator adds an `ipBlock` or a CNI rule, which the chart does not ship.

**Hub settings fail closed.** When `shadewright-hub-settings` is missing or does not validate (an unknown key, a value out of range, a malformed host or address), intake reports not ready and answers every request with a retryable refusal before reading the body, the fleet controller makes no discovery fetch (stored keys age out as in section 6), and no proxy is trusted. Each component counts the state in a metric, and the fleet controller emits an Event. Code defaults fill only values the settings leave out, never settings that fail to parse.

**What RBAC does and does not do.** No hub identity creates, updates or deletes a Namespace, RoleBinding, ResourceQuota, Role or ClusterRole, or holds `bind` or `escalate`, so no admission policy is needed. RBAC does not separate spokes: every snapshot sits in one namespace and intake writes all of them. The guard is intake's code (section 10), with tests.

[![Identities of control plane mode and what each may write: the three spoke identities keep their writes, the reporter only reads and sends one snapshot, intake replaces the snapshot of the verified cluster in the one hub namespace with per-cluster limits in its code, the fleet controller alone writes Cluster status and deletes a snapshot of an earlier registration, the status page runs in its own namespace, only reads and is reached by port-forward, onboarding adds only a Cluster object, and no hub component has a model credential or a Git write](../assets/diagrams/control-plane-identities.svg)](../assets/diagrams/control-plane-identities.svg "Open full size")

### 5. An intake service instead of the hub's Kubernetes API (Proposed)

The spoke posts to intake, and intake writes with its own identity. Onboarding a spoke as an API server JWT authenticator would be a change to the cloud control plane, not a reviewed merge: on AKS, JWT authenticators are in preview, configured per authenticator through the Azure API (`az aks jwtauthenticator add`), and need a publicly accessible discovery endpoint ([Microsoft Learn](https://learn.microsoft.com/azure/aks/external-identity-provider-authentication-overview), checked 2026-11-14; other vendors were not checked). Inline keys have no path there, and validation, redaction checks and per-cluster limits would have no single place. The transport stays behind an interface ([ADR-0013](0013-adapters-for-every-external-system.md)).

### 6. How intake checks a spoke (Proposed)

**Finding the `Cluster` object.** Kind and kubeadm clusters share the default issuer and every install has the same reporter subject, so issuer plus subject cannot identify a spoke with inline keys. The reporter posts to a path that names its cluster. The name is only a hint: intake loads that one object and verifies the token against its issuer, keys and subject.

**Readiness.** Intake accepts a report only while the object's `Ready` condition is `True` with `observedGeneration` equal to `metadata.generation`, in both key modes. Any spec change, an inline key included, stops intake until the fleet controller has checked the new generation. A verified token for an object that is not ready gets the named reason `not ready`.

**The token.** A projected ServiceAccount token with the hub's audience and `expirationSeconds: 600` (the least Kubernetes allows). Intake requires:

- `exp`, `iat` and `nbf`, with `exp` minus `iat` at most 15 minutes, `iat` and `nbf` not in the future and `exp` not past (leeway: question 10). A longer-lived token minted through `TokenRequest` is refused.
- a header `alg` from an asymmetric allow-list (proposed: `RS256`, `ES256`) that matches the JWK its `kid` selects: `RS256` only with `kty` `RSA`, `ES256` only with `kty` `EC` and `crv` `P-256`, and equal to the JWK's `alg` when it has one.
- `iss` and `sub` equal to the object's, and the hub's audience.

The keys are `spec.identity.jwks` for `Inline`, and for `Discovery` the keys the fleet controller fetched into status for the current issuer and generation (below). The `kube-system` UID and the cluster ID in the snapshot must equal the object's. The UID is not authentication; it catches a rebuilt cluster that reuses an issuer or keys.

**Order of checks.** Before the token verifies: a rate limit per source address, the body cap (section 10), then one generic refusal with no reason, also for a key ID the object does not hold and for a revoked object. The source address is the connection's peer; `X-Forwarded-For` and `Forwarded` count only from a proxy address listed in the hub's settings. Nothing a request carries makes the hub fetch anything. After it verifies: readiness, the per-cluster limits and the named reasons (`not ready`, `suspended`, `busy`, `over limit`, `redaction`).

**Keys (both kinds).** Only public asymmetric keys: `kty` `RSA` or `EC`. A key with `kty` `oct`, `alg` `none` or any private member (`d`, `p`, `q`, `dp`, `dq`, `qi`, `oth`, `k`) is refused: for `Inline` the fleet controller sets `InvalidKeys` and not `Ready`, for `Discovery` the fetch fails.

**Issuer keys for `Discovery`.** The fleet controller fetches them on a schedule (proposed: every 10 minutes) and writes them into `Cluster.status` with the hub's fetch time, the issuer and the object's generation. On a spec change it clears `Ready` and fetches again; intake uses stored keys only for the current issuer and generation. If fetches fail, intake keeps the stored keys until they are older than a maximum age (proposed: 24 hours, on the hub's clock), then refuses every token for that cluster. Each failure is a `KeysUnavailable` condition and an Event. A hub restore loses nothing: the controller fetches again. The fetch takes its URL only from a reviewed object, with these limits:

- The issuer host matches the allow-list in the hub's settings. An entry is an exact host (`oidc.example.com`) or a suffix that starts with a dot (`.oic.prod-aks.azure.com`), which matches only at a label boundary. Hosts are compared in lower case without a trailing dot, and IP literals never match. The list limits where the hub fetches; it does not show that an issuer belongs to a cluster (on AKS, issuers share a host per region), so the reviewer checks the full issuer URL.
- The issuer is `https` on port 443, the discovery document's `issuer` equals `spec.identity.issuer` exactly, and its `jwks_uri` is `https` on the same host and port.
- Both fetches: verified TLS 1.2 or later, no redirects, no environment proxy, a size limit (proposed: 64 KiB) and a time limit.
- The dial guard of [design 10.6](../design/10-security.md#106-network) checks every resolved address at connection time and refuses loopback, private (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, IPv6 unique local), link-local (169.254.0.0/16, fe80::/10), 100.64.0.0/10 and the hub's declared in-cluster ranges.

**Uniqueness.** No two `Cluster` objects, active or revoked, `Inline` or `Discovery`, may share a signing key, compared by JWK thumbprint ([RFC 7638](https://www.rfc-editor.org/rfc/rfc7638)); for `Discovery` the fetched keys count. No object may reuse the issuer plus subject of a `Discovery` object, whatever its own mode. Two `Inline` objects may share an issuer and subject, as Kind clusters do; their keys tell them apart. On a conflict the fleet controller refuses both (`IdentityConflict`) until a reviewed change resolves it. So a revoked cluster's keys or issuer cannot come back under a new name, in either mode, without a review that sees the conflict.

### 7. The `Cluster` object (Proposed)

API group `fleet.shadewright.io/v1alpha1`, cluster-scoped, applied by Flux from the hub's GitOps repository.

- `spec.clusterId`: equal to the spoke's `clusterId` and to `metadata.name`, enforced by a CEL rule. It is a DNS label.
- `spec.identity`: `issuer`, `keys` (`Discovery` or `Inline`), `jwks` (`Inline` only), `subject`, `kubeSystemUID`. The spec holds only reviewed keys.
- `spec.suspended`: the reversible stop. `spec.revoked`: the final one, kept true by a CEL transition rule (section 3).
- Status, written by the fleet controller only: conditions `Ready` (with `observedGeneration`), `Reporting` (last seen), `Stale`, `IdentityConflict`, `InvalidKeys` and `KeysUnavailable`, the reported versions, and for `Discovery` the fetched keys with their fetch time, issuer and generation.

This sketch is illustrative and not final. Field names, validation and the way a value is derived are for the build task.

```yaml
apiVersion: fleet.shadewright.io/v1alpha1
kind: Cluster
metadata:
  name: lab-kind
spec:
  clusterId: lab-kind
  suspended: false
  revoked: false
  identity:
    keys: Inline            # or Discovery, which uses the issuer's discovery document
    issuer: https://issuer.example.com
    subject: system:serviceaccount:<operator namespace>:<reporter service account>
    kubeSystemUID: <uid of the kube-system namespace>
    jwks: |
      { "keys": [ { "kty": "RSA", "kid": "<key id>", "n": "<modulus>", "e": "AQAB" } ] }
```

### 8. The snapshot (Proposed)

Each report is one snapshot: the whole state the spoke shows the hub, built fresh from the cluster's objects. It is a typed allow-list, not the objects themselves. Every field has a type, a length bound and, where it names a state, an enum:

- A **heartbeat**: the cluster ID and the `kube-system` UID (section 6), operator, chart and pack versions, module status, and the delivery report of `doctor` as a typed list of at most 256 sources (the cap `doctor` uses). Each source has the Kustomization and GitRepository as namespace and name, the repository's host and path, the branch, `settings` (`named`, `not-named`, `ambiguous`, `unknown`), `state` (`ok`, `refused`, `not-ready`, `unknown`) and a reason from the source mapping's fixed set. The free-text `detail` of `doctor` is not sent.
- **Summaries of Incidents**: fingerprint, subject kind, namespace and name, reason, phase, times.
- **Summaries of requests**: action type, target, phase, pull request URL, condition reasons, and for a finished request its final phase (one of the request phases of `api/v1alpha1` that end a request: `Succeeded`, `Denied`, `Failed`, `Expired`, `Cancelled`, `Superseded`, `NeedsIntervention`, `Inconclusive`) and the time it reached it.

The snapshot has a fixed maximum record count (proposed, not final: 500 Incidents and 500 requests) and a fixed maximum size as serialised (proposed: 512 KiB, well under the API server's object limit and under the largest entry the redactor's heap bound was measured for). A spoke with more picks the newest records up to the count and marks the snapshot as cut, which the status page shows. There is no evidence, no prompt, no transcript, and no free text from cluster content beyond bounded fields redacted in the spoke by the redactor of [ADR-0048](0048-redaction-in-a-shared-module.md) ([design 10.3](../design/10-security.md#103-redaction)). A setting lets a spoke send names hashed, each as `sha256:` and 64 hex digits.

The hub stores it as one `ClusterSnapshot` object per cluster in the hub namespace, named after the cluster. Intake adds what the hub knows and the spoke does not say: the UID of the `Cluster` object it verified against and the hub's own time of acceptance. It is never stored as `Incident` or `RemediationRequest`: no hub controller may act on an object a spoke supplied (invariants 3 and 4). The fleet controller sets `Reporting`, `Stale` and the versions on the `Cluster` object from the snapshot.

### 9. Delivery (Proposed)

- The reporter sends a snapshot on change (at most one every 30 seconds, proposed) and at least every 5 minutes (proposed). It sends one at a time and waits for the answer before the next.
- Intake replaces the cluster's snapshot in one write: it reads the current object and writes the new one with that object's `resourceVersion` (or creates it when there is none). A write that loses the version check is refused with the named reason `busy`, which is retryable. There is no partial state: a snapshot is stored whole or not at all, and what it does not list is gone from the hub.
- Intake does not order snapshots; the last accepted one wins. A reporter sends one at a time and runs as one replica with `Recreate`, so an older snapshot can replace a newer one only when a request from a stopped reporter arrives after the first of a new one, and the next snapshot corrects it.
- The reporter keeps nothing it cannot rebuild from the cluster's objects. A retried or replayed snapshot writes the same state again (invariant 5).

Staleness is decided from recorded state on one clock: the time the hub itself recorded when it accepted a snapshot, compared with the hub's clock. A time a spoke reports is shown, never used to decide whether data is stale.

### 10. Limits against a noisy or compromised spoke (Proposed)

- **One cluster per write, in code.** Intake takes the snapshot's name and cluster UID from the verified `Cluster` object, never from the snapshot or the path, writes only the snapshot of that name, and cannot delete.
- **Per-cluster limits, in code**, keyed by the verified cluster, values in the hub's settings (proposed, not final): one request in flight per cluster in each replica, a second refused with `busy` before any work (across replicas the version check lets only one write win); a report rate of 6 a minute per cluster in each replica, so N times that fleet-wide, refused with `over limit`; and the caps of section 8, with the body read through a limit of the size cap plus a small envelope and a larger one refused before the token is checked.
- **One spoke cannot take another's share.** Each cluster has exactly one object, named after its verified `Cluster` object and capped in size, so hub storage is at most the number of clusters times the cap. The chart's ResourceQuota on the hub namespace caps `ClusterSnapshot` objects at the hub's maximum number of clusters (proposed: 200); a spoke cannot create a second object, so it cannot take another's place under that quota. A replica redacts at most K snapshots at once (proposed: 4), and with one request in flight per cluster, one spoke holds at most one of those slots per replica.
- **Redaction, bounded and fail closed.** Intake runs the shared redactor of [ADR-0048](0048-redaction-in-a-shared-module.md) (`Redactor.Clean`) over every decoded string and key of the whole snapshot, under the redactor's work budget and heap admission. A snapshot the budget withholds, or whose wait for heap admission runs out, is refused with `busy` (retryable) and nothing is stored; one the redactor would change is refused with `redaction`. Nothing is redacted and stored. As in [ADR-0047](0047-scoring-analysis-records-outside-the-cluster.md) section 3, values whose shape intake checks in code (Incident and request names, fingerprints, hashed names) are kept out of that check because they trip the redactor, and a value that fails its shape check refuses the snapshot. Intake's redactor is that of its own build, so a newer rule catches what an older spoke let through. A refusal is counted with the rule kinds that matched, never the text.
- **Schema.** Unknown fields are refused and every field is bounded.
- **Pull request URLs** must be absolute `https` URLs with no userinfo, or the snapshot is refused. The status page renders one as a link only when its host is on the hub's allow-list of code hosts (proposed default: `github.com`), as plain text otherwise. It escapes everything.
- Nothing a spoke sends reaches a model, a notification or a Git write in phase 1.
- **Tests the build needs**: a snapshot for one cluster cannot create, update or delete another's, whatever its path, body or names; one cluster at its limits or holding its redaction slot does not refuse another's; each limit refuses at its bound; a withheld redaction stores nothing; revoked and unknown clusters get the same refusal; missing or invalid settings pause intake and fetches.

### 11. Kill switch (Proposed)

Per cluster: `spec.suspended` on its `Cluster` object, which is reversible, or `spec.revoked` (section 3). Fleet-wide: the hub's settings pause intake. In phase 1 these only stop reporting.

### 12. The rule for later phases: the hub can only make a spoke do less (Proposed)

A reply to a report may carry restrictive directives (pause remediation, slow down), never permissive ones. So a compromised hub can stop remediation in a spoke, but cannot cause it.

A directive from the hub is held apart from the spoke's own kill switch (`docs/emergency.md`) and only adds to it. Lifting the hub's pause never lifts a local pause, and only the spoke's operator lifts a local one.

Configuration for a spoke (pack references, model profiles, policies) travels as a pull request to that spoke's GitOps repository (invariant 3), never as a directive.

### 13. Phases (Proposed)

1. **Fleet view**: reporter, intake, fleet controller, `Cluster`, `ClusterSnapshot`, the read-only status page with the identity and access of section 4, staleness. No notifications. This is what the rest of this ADR specifies, and what Shadewright 0.7.0 ships.
2. **Guard rails**: restrictive directives, a fleet-wide pause, configuration by pull request, and notifications: the notifier of [#97](https://github.com/shadewright-labs/shadewright/issues/97), with its own identity, its credential named by an ADR under the static credentials rule of invariant 6, spoke text fenced and mentions neutralised.
3. Each of these has its own ADR and is not decided here:
   - Remediation through the hub: a central executor holding the Git App. It would have to re-evaluate spoke proposals against policy held at the hub, treating them as untrusted input.
   - Horizontal scaling by key ownership.

### 14. Prototype (Proposed)

One AKS lab cluster is the hub and also a spoke of itself (issuer discovery). A local Kind cluster is a second spoke (inline keys, outbound to the hub). So no second AKS cluster is needed.

**The prototype does not test the hub and spoke boundary.** The hub's components share the AKS cluster with an executor that holds the Git App, and so does the public intake endpoint. It tests registration and reporting only, and the hub namespaces are never in `watchNamespaces` of any component. **The lab cluster does not last.** `make lab-down` in `shadewright-azure` destroys it after every session, so its issuer URL and `kube-system` UID change on every apply and the hub's state goes with it. Each session registers its spokes again by pull request; it is proposed that `make lab-up` ends by running the registration command.

**Price (2026-11-14, `make lab-cost`, EUR, Sweden Central, Azure Retail Prices API).** The lab cluster costs 0.1992 per hour (one Standard_D4as_v5 node 0.1652, OS disk 0.0071, outbound public IPv4 0.0045, load balancer 0.0224, AKS Free tier). The Kind spoke costs nothing. The only addition is one inbound public IPv4 for intake, 0.0045 per hour: about 0.204 per hour, or 1.63 for an 8 hour session. A second AKS cluster as the hub would double that. Usage-based items (data processed, egress, model tokens) are not included.

### One snapshot, from token to stored object

```mermaid
sequenceDiagram
    participant R as Reporter (spoke)
    participant I as Intake (hub)
    participant K as Keys in the Cluster object
    participant N as Hub namespace
    R->>R: Build the snapshot from the cluster's objects, redact, bound the fields and the size
    R->>R: Project a ServiceAccount token (audience: this hub, 10 minutes)
    R->>I: HTTPS POST to the path naming its cluster, with the token and the snapshot
    I->>I: Refuse a peer address over its rate limit or a body over the cap
    I->>I: Load the one Cluster object the path names (a hint, not trusted)
    I->>K: Inline keys from spec, or Discovery keys in status for the current issuer and generation
    alt No such object, revoked, or the token does not verify (signature, alg, exp, iat, nbf, iss, sub, audience)
        I-->>R: One generic refusal, no reason
    else Token verifies
        alt Not Ready at this generation, suspended, busy, over limit, or a kube-system UID or cluster ID that differs
            I-->>R: Refused with a named reason, counted in a metric
            R->>R: Record it in a metric, back off, keep working standalone
        else Passes
            I->>I: Validate the whole snapshot, refuse unknown or over-long fields and a non-https pull request URL
            I->>I: Run the redactor under its budget; refuse (redaction, or busy when withheld) if it would change anything
            I->>N: Replace the cluster's ClusterSnapshot in one write with a version check, stamped with the Cluster UID and the hub's time
            I-->>R: Accepted
        end
    end
    Note over I,K: Intake fetches nothing. The fleet controller fetches issuer keys on a schedule and reports failures on the Cluster object.
```

Whether a refusal that is not about the cluster's state (a rate limit, a malformed body) tells the reporter to back off the same way as a refusal for a suspended cluster is open. The status codes and reason names are for the build task.

## How invariants 1 to 6 hold in phase 1

1. **Model output is never authorization.** The hub has no model credential, and nothing a spoke sends reaches a model there. The analyzer and the policy gate in each spoke are unchanged.
2. **Actions are a closed catalog.** Phase 1 adds no action and no way to ask for one. The directives of phase 2 can only restrict.
3. **Changes go through Git only.** The hub writes nothing to a spoke or to any Git repository in phase 1. A new spoke, a key rotation, a suspension and a revocation are each a reviewed pull request to the hub's repository; the registration command writes a file into a local checkout and does not commit or push.
4. **Everything from the cluster is untrusted input.** The spoke redacts, and the hub validates the whole snapshot again, refuses one its redactor would change, stores it as a kind no controller acts on and escapes what it shows. Nothing a spoke sends changes who the hub trusts.
5. **Reconciliation is idempotent.** A snapshot replaces the cluster's one object whole, so a repeated or replayed one writes the same state.
6. **Separate identities.** The reporter only reads, and has no Secret, Git identity or model credential. Intake creates and updates only `ClusterSnapshot` objects and has no write on `Cluster`; the fleet controller writes only `clusters/status` and deletes a snapshot of an earlier registration; the status page only reads. Components with a metrics endpoint get `create` on `tokenreviews` and `subjectaccessreviews`, as the verifier does. No hub identity creates Namespaces, RoleBindings or quotas, or holds `bind` or `escalate`. RBAC does not separate spokes; intake's code does, with tests. The build PR adds the four identities to invariant 6; this ADR does not edit `AGENTS.md`.

## Threats and what stops each

| Threat | What stops it | What is left |
|---|---|---|
| A cluster pretends to be another spoke | The path only selects which reviewed `Cluster` object to verify against. The token must match its keys, `iss`, `sub`, the hub's audience and an allowed `alg`, with the object `Ready` at its generation. The snapshot's name and UID come from the object. | Whoever can mint a token for the reporter's ServiceAccount can report as that spoke. A bug in intake's code could write another cluster's snapshot; the tests of section 10 cover it. |
| A name or identity claimed twice | Reviewed merge; `spec.clusterId` equals the name; key thumbprints and `Discovery` issuer plus subject unique across both modes, revoked objects included. | None known. |
| A long-lived or replayed token | `exp`, `iat` and `nbf` required, at most 15 minutes from `iat` to `exp`. A replayed snapshot writes the same state. | A captured token works until it expires. No nonce. |
| A flood, from a spoke or unauthenticated | Section 10: per-address rate and body cap before the token, then one request in flight, a rate, size and count caps, one object per cluster and one redaction slot per cluster in each replica. No request triggers a fetch. | A spoke can send false data about itself. The rate is per replica. |
| Secrets stored at the hub | Intake's own redactor over every string and key; a snapshot it would change is refused. | A secret the redactor does not recognise. |
| Stale data read as current | The hub's own time of acceptance; suspended and revoked snapshots marked; only snapshots of the current object's UID shown. | A viewer who ignores the mark. |
| A compromised hub | No kubeconfig or spoke credential, no call to a spoke, no Git write identity in phase 1; replies only accept or refuse, later only restrict. | False data, and in later phases a pause. In the prototype it shares a cluster with the executor (section 14). |
| A compromised fleet controller | It writes only `Cluster.status` and deletes snapshots. | Wrong keys for a `Discovery` cluster, deleted snapshots. |
| A hub pod reaching the status page | No Service, 127.0.0.1 only, default-deny namespace, port-forward only. | Whoever may port-forward sees every cluster (question 5). |
| Injection or phishing through the status page | Typed, bounded fields, redacted twice; `https` URLs without userinfo, links only to allow-listed code hosts; everything escaped. | Not built; each needs a test. |
| A wrong registration | The command reads every field live; the spoke keeps no registration object to edit; the reviewer checks the full issuer URL. | A command run against the wrong context and merged unchecked. Wrong inline keys make the hub trust their holder. |
| The public intake endpoint | The only hub component reachable from outside: no Git identity, model credential or external call, token checked before the body is parsed, no write on `Cluster`. | Egress to 443 and 6443 of any address unless the operator narrows it (design 10.6). |
| SSRF through issuer discovery | URL only from a reviewed object, never on request; exact or dot-prefixed host list, https on 443 on one host, exact `issuer`, no redirects or proxy, size and time limits, the dial guard. | A public host on an allowed suffix that is not an issuer. |
| Keys that are not public signing keys | Only `RSA` and `EC` public keys; `oct`, `alg: none` and private members refused; Discovery keys only for their issuer and generation. | None known. |

## Consequences

**Easier.** One view of all clusters, with no evidence, prompts or credentials gathered in one place. Each spoke is as safe as it is alone, with its own credential and budget, whether the hub is up or not. Adding a cluster is one object in a reviewed pull request. A cluster's state at the hub is one object replaced whole, so there is no ordering rule, no sweep of old records and no per-record quota.

**Harder.**
- A new API group, four new components to build and operate, and a new spoke identity with an `AGENTS.md` change in the build PR.
- Spokes depend on the hub for the fleet view only; its availability is open.
- The boundary between spokes at the hub is intake's code and its tests, not RBAC. Whoever may read the hub namespace reads every spoke.
- A spoke with more open Incidents or requests than the caps shows only the newest, marked as cut.
- Every inline key rotation is two reviewed pull requests, and a spoke rebuild needs one.
- A revoked spoke's object and snapshot stay until a later reviewed change, not designed yet.
- Hashed names (optional) make the view harder to read.

**Accepted.**
- In phase 1 the fleet view is read-only and sends no notifications.
- Reports are as true as the spoke that sends them.
- Each snapshot sends the whole state again, bounded by the size cap and the rate.
- Cost: the prototype is about 0.204 per hour, 1.63 for an 8 hour session (section 14).

## Alternatives considered

- **Per-record reported kinds with multi-page snapshots.** The design until 2026-11-28, dropped then. Intake upserted `ReportedIncident`, `ReportedRequest` and `ReportedHeartbeat` objects by derived names, and a full list came as numbered pages whose completion deleted what it did not list. The fourth review pass found that N stateless replicas had nowhere durable to keep a snapshot's progress, that a sweep from a replica's cache could delete live records, that ordering needed an epoch and counter nobody had settled, and that parallel requests could beat the cache-based count limit, so one spoke could fill the shared namespace quota and starve the others. Fixing it needed durable snapshot state, live-list sweeps and per-request caps. One object per cluster removes the progress state, the sweep, the ordering rule and the shared count, for a fixed cap on how much a spoke can show.
- **A registration ConfigMap kept by the reporter.** Dropped with the design above. It was self-reported, anyone with `update` on ConfigMaps in the operator namespace could change what got pasted, and the command had to read every field live anyway to be trusted. It cost the reporter's only write and a `get` on `/openid/v1/jwks`.
- **The `StateStore` interface.** Decided on 2026-11-14 to keep a hub on Azure Table and Blob Storage possible, dropped on 2026-11-28: nothing in phases 1 and 2 uses its compare-and-swap, lock or log.
- **One hub namespace per spoke, with a quota and RoleBindings from the onboarding pull request.** The design of 2026-11-14 to 2026-11-27. Three review passes each found new gaps (a revoked spoke's namespace the status page could not list, Flux pruning intake's RoleBinding on revocation, namespaces in deletion counted as ready, quotas and bindings the fleet controller could not check exactly). The boundary between spokes was still intake's code, because intake was one ServiceAccount bound in every spoke namespace; per-namespace impersonation would have moved it into RBAC at the cost of an `impersonate` grant on the component reachable from outside.
- **Revocation by deleting the `Cluster` object.** The status page would have nothing to list the spoke by, and its data no owner.
- **The fleet controller creates spoke namespaces, quotas and RoleBindings.** RBAC cannot limit `create` on Namespaces by name, so it needed a ValidatingAdmissionPolicy, and a gap in it would let a compromised controller bind intake's writes in `kube-system`.
- **Automatic key updates for inline keys (`keyUpdates: Automatic`).** Decided on 2026-11-14, dropped on 2026-11-27. The hub would trust a key a spoke listed in several authenticated reports. It bent invariant 4 (data a spoke sends changing whom the hub authenticates), let brief access in a spoke mint long-lived tokens to replay across the window, let one spoke block another's rotation by listing its next key, and lost promoted keys on a hub restore. Reviewed rotation costs two pull requests and removes all of it. Promotion by tokens signed with the pending key shows only that the signer holds the private key, not that the API server signs with it.
- **Intake fetches issuer keys itself.** That gives the component reachable from outside an external call, lets an unknown key ID trigger a fetch, and intake cannot write `Cluster.status` to report a failure.
- **Spokes write to the hub's Kubernetes API directly.** See section 5.
- **The analyzer at the hub.** Evidence and prompts would cross to the hub, analysis would stop when it is down, and one credential and budget would serve all clusters. Decided against on 2026-11-14.
- **A bootstrap token exchanged for a client certificate, as `kubeadm join` does.** A shared secret, a hub CA and certificate rotation, none of which inline public keys need. Replaced by the decision of 2026-11-14 on [#559](https://github.com/shadewright-labs/shadewright/issues/559).
- **A message broker (Service Bus, NATS) now.** A component, a credential per spoke and a cost, for a small HTTPS post. It can come back behind the transport interface.
- **Reusing the `Incident` kind at the hub.** A hub controller could act on an object a spoke supplied (invariants 3 and 4).

## Open questions

1. Where the executor and the Git App sit in the phase that remediates through the hub, and how the hub's policy evaluates a spoke's proposal (phase 3, its own ADR).
2. Hub high availability: how many replicas of intake, and what the status page shows while the hub restarts.
3. The retention after revocation (30 days is proposed in section 3, not decided), and the reviewed change that deletes a revoked `Cluster` object and its snapshot, including whether it is exported first.
4. Whether hashed names should be the default instead of an option.
5. Multi-tenancy at the hub: whether some viewers should see only some clusters. In phase 1 the status page shows every cluster to whoever may port-forward to it, and with one hub namespace RBAC cannot split reads by cluster.
6. Closed, 2026-11-28: intake does not order snapshots; the last accepted one wins (section 9).
7. Closed, 2026-11-28: the reporter records the hub's answer in a metric and its log, and writes nothing in the cluster (section 3).
8. How the snapshot gets the delivery source report of `doctor`. `doctor` is a kubectl plugin that runs with a person's permissions and reads Flux objects, and the reporter's reads above do not include them. Either the reporter gets that read (the candidate is in section 4), or the report comes from state another component already records.
9. Closed, 2026-11-27: `spec.clusterId` equals the object name (section 7).
10. Whether a refusal for a rate limit, `busy` or a malformed body makes the reporter back off like a refusal for a suspended cluster, the reason names, and the leeway for clock difference when intake checks `exp`, `iat` and `nbf`.
11. Closed, 2026-11-27: the issuer fetch refuses private, link-local, 100.64.0.0/10 and in-cluster addresses after name resolution (section 6).
12. Closed, 2026-11-28: intake does not impersonate a per-spoke identity. With one hub namespace the guard between spokes is intake's code and its tests (section 10).
13. Closed, 2026-11-14: the price of the prototype ([#669](https://github.com/shadewright-labs/shadewright/issues/669)) is in section 14.
14. Whether the registration command's `--keys` flag defaults to `Inline`.
15. Whether the hub's repository gets a CI check that fetches the issuer's discovery document for a new `Cluster` object and reports whether keys resolve, and who owns it.
16. The name of the command that writes the registration (`kubectl shadewright registration` is a proposal).

## Amendments

None.
