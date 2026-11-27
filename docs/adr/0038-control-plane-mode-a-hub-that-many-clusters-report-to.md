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

## Decision

### 1. Two modes, one set of binaries (Proposed)

There are two modes with the same spoke binaries plus hub binaries, and the same CRDs:

- **Standalone**, as today.
- **Control plane**: a hub, and clusters that report to it, called spokes in this ADR.

A spoke is a standalone install plus a reporter. It keeps working when the hub is down.

### 2. Where things run (the analyzer, the hub form and "the hub never calls a spoke" are Decided, 2026-11-14; the rest is Proposed)

- **Decided.** The analyzer runs in each spoke. Evidence and prompts never cross to the hub. Analysis works when the hub is down, and no channel into a spoke is needed. Each cluster has its own model credential ([ADR-0037](0037-the-anthropic-api-as-a-second-model-provider.md) for the key) and its own daily budget (`maxTokensPerDay`, [ADR-0023](0023-how-the-analyzer-calls-the-model.md)).
- **Decided.** The hub is a Kubernetes cluster with one namespace per spoke. Hub state goes through a small `StateStore` interface: read and write with compare-and-swap, list by key, a lock with a time limit, and an append-only log. That keeps a hub as a plain service on Azure Table and Blob Storage possible later ([ADR-0013](0013-adapters-for-every-external-system.md)).
- **Decided:** the hub never calls a spoke. **Proposed:** connections are outbound from the spoke, and the hub holds no kubeconfig or spoke credential.
- **Proposed.** Redaction runs in the spoke before anything leaves it, with the existing redactor (`internal/redact`, moving to a shared module under [ADR-0048](0048-redaction-in-a-shared-module.md); [design 10.3](../design/10-security.md#103-redaction)), and again at the hub's intake, which refuses what it would change (section 10). The hub treats everything from a spoke as untrusted input (invariant 4).

[![Control plane mode: two spokes, each with its own GitOps repository and model provider, report outbound to the intake service of the hub; the hub reads its Cluster objects and spoke namespaces from its own GitOps repository. In the prototype, spoke A is the hub's own cluster, so there the hub and spoke are not separate](../assets/diagrams/control-plane-overview.svg)](../assets/diagrams/control-plane-overview.svg "Open full size")

### 3. Onboarding (Decided, 2026-11-14, except where marked)

- A new spoke is a pull request that adds its `Cluster` object to the hub's GitOps repository. Whoever may merge there approves it. There is no self-registration.
- The object records the spoke's OIDC issuer URL and the UID of its `kube-system` namespace. The hub accepts reports for a name only from that issuer and UID. A rebuilt cluster needs a reviewed change of the object.
- A spoke with no reachable issuer (Kind, on-premises): the `Cluster` object carries the public keys (JWKS) that sign the spoke's ServiceAccount tokens, and the hub verifies tokens against them. There is no shared secret and no hub CA. A key rotation in the spoke needs a reviewed change. (A later decision of the same day let inline keys change at the hub without a pull request. This ADR does not take it up, see "Alternatives considered"; accepting the ADR replaces that decision.)
- **Decided, 2026-11-14:** onboarding must not require copying identifiers by hand. The spoke publishes a ready-to-use registration in the operator namespace. The design that follows is **Proposed**.
- **The objects of a spoke at the hub (Proposed).** The same pull request adds the spoke's objects at the hub, all from one Kustomize component in the hub's repository that takes only the cluster name: the Namespace `<prefix><name>`, an object-count ResourceQuota, and RoleBindings in that namespace to the chart's static ClusterRoles for intake, the fleet controller and the status page (section 4). Flux applies them. No hub component creates a Namespace, a RoleBinding or a quota.
- **Suspending and revoking (Proposed).** `spec.suspended: true` is the reversible stop: a spoke whose token verifies is refused with the named reason `suspended`, counted in a metric, what it sent before stays readable and is marked stale, and nothing new is stored. Setting it back to `false` resumes reporting. Revocation is a pull request that removes the `Cluster` object and intake's RoleBinding for that spoke and keeps the namespace with its records. The status page lists a spoke namespace with no `Cluster` object as removed, with the time the hub last accepted a report from it. After the retention (proposed: 30 days, not final) a second pull request removes the namespace. Nothing new is stored after revocation.
- **Adopting a name again (Proposed).** Intake stamps every record it stores with the UID of the `Cluster` object it verified against. When a `Cluster` object is added for a namespace that still holds records, the fleet controller deletes every record whose stamp differs from the new object's UID before it sets `Ready`, and intake refuses reports with the named reason `not ready` until then. A re-registered spoke never shows the records of the one before.
- The spoke learns of a suspension from the named refusal. After a revocation it sees only the generic refusal, the same one an unknown name gets: with no `Cluster` object there is nothing to verify the token against, and before a token verifies no reason is given (section 6). A different answer for a removed name would tell any caller which names were once registered. The reporter sets a condition, backs off to a slow retry and keeps working standalone. The hub never calls it.

#### The registration manifest (Proposed)

- The reporter keeps a ConfigMap `shadewright-cluster-registration` in the operator namespace (`shadewright-system` by default) with one key, `cluster.yaml`: the complete `Cluster` manifest (section 7) for this spoke, ready to commit to the hub's GitOps repository. It holds the `clusterId`, the issuer URL (read from the `iss` claim of the reporter's own projected token, which the reporter decodes without verifying), `keys: Discovery` or `Inline`, for `Inline` the signing keys read from the API server's `/openid/v1/jwks`, the reporter's subject and the UID of `kube-system`.
- A second key, `status`, carries the last result from the hub: accepted, or refused with the reason. When the keys it reads differ from those of the last accepted report and the hub refuses, the reporter says there that the `Cluster` object needs a reviewed change. The reporter writes it, from what the hub answered. Anyone with `update` on ConfigMaps in the operator namespace can change both keys; RBAC decides who that is, not this ADR.
- The chart creates the ConfigMap only when it is absent, and never overwrites it on an upgrade or a reconcile. The reporter rewrites both keys on every pass, so a cleared ConfigMap heals. A deleted one comes back with the next chart upgrade, because the reporter cannot create it.
- `Discovery` is written when a chart value says the issuer is reachable from the hub, `Inline` otherwise (value name not final, for example `reporter.registration.issuerReachable`). `Inline` is the safe default, because it needs no reachable issuer. Whether that default is right is open (question 14).
- `kubectl shadewright` gets a command, proposed as `kubectl shadewright registration`, that prints the manifest or writes it to a file. It does not trust the ConfigMap: with the person's kubeconfig it reads every identity field live and prints each mismatch with the ConfigMap. The issuer comes from the API server's `/.well-known/openid-configuration`, the subject from the ServiceAccount of the reporter's Deployment, the UID from the `kube-system` Namespace, the `clusterId` from the install's settings and, for `Inline`, the keys from `/openid/v1/jwks`. The person who opens the pull request runs it and pastes the output into the description. No hub component reads the ConfigMap.
- Onboarding then is three steps: install the spoke with the hub's URL, put `cluster.yaml` and the spoke's folder into a pull request on the hub's GitOps repository, merge. Nothing is typed by hand.
- What a reviewer checks: the full issuer URL (does it belong to the cluster the name claims? for AKS, against `oidcIssuerProfile.issuerUrl`), the cluster name, the subject (it follows `system:serviceaccount:<operator namespace>:<reporter service account>`, so a different one is a reason to ask), the command's output, and that the spoke's folder uses the shared component unchanged.
- Rotation for `Inline` is two reviewed changes: one that adds the new public key to `spec.identity.jwks` while the old key still signs, and, after the spoke signs with the new key, one that removes the old key. Kubernetes verifies with several keys and signs with one, so this leaves no gap.

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
    result: refused   # or accepted
    reason: signing keys changed since the last accepted report; the Cluster object needs a reviewed change
```

**The ConfigMap is a convenience, not evidence (Proposed).** Everything in it is public. It is self-reported by the spoke, so whoever can edit it can change what gets pasted, for example an issuer of another cluster on the same AKS host, which passes the host allow-list. What makes a cluster trusted is the reviewed merge in the hub's repository, with the live comparison of the command above. **Open, not decided:** a CI check in the hub's repository that fetches the issuer's discovery document for a new `Cluster` object and reports whether keys resolve (question 15).

```mermaid
sequenceDiagram
    participant P as Person with merge rights on the hub repo
    participant R as Hub GitOps repo
    participant F as Flux (hub)
    participant C as Fleet controller
    participant S as Reporter (spoke)
    participant I as Intake
    Note over S: Proposed: the reporter writes the ConfigMap shadewright-cluster-registration (cluster.yaml, status) on every pass
    P->>S: kubectl shadewright registration: manifest, live check of every identity field
    P->>R: Pull request: the Cluster manifest and the spoke folder (namespace, quota, RoleBindings from the shared component)
    P->>R: Review (full issuer URL, name, subject, command output) and merge
    F->>F: Apply the Cluster object, namespace, quota and RoleBindings
    C->>C: On a conflict (same issuer and subject, or a shared inline key) refuse both objects
    C->>C: Check the spoke's objects exist, fetch issuer keys (Discovery), delete records of an earlier Cluster UID
    C->>C: Set Ready on the Cluster object
    Note over S: The spoke admin installs the reporter with the hub URL and audience, before or after the merge
    S->>I: First report to the path of its cluster, with a projected ServiceAccount token
    I->>I: Load that one Cluster object, verify the token against it, compare the kube-system UID
    I-->>S: Accepted
    S->>S: Record the result in the status key of the ConfigMap
    C->>C: Set Reporting (last seen, versions) on the Cluster object
    Note over S,I: Suspending the object makes the next verified report a refusal with the reason suspended. Revocation removes the object and intake's RoleBinding, so the spoke sees the generic refusal; the namespace goes in a later pull request after the retention. Any change of inline keys is a reviewed pull request.
```

### 4. Components (Proposed)

In each spoke:

- **`reporter`**, a new spoke identity, `shadewright-reporter` (invariant 6). Its reads, exactly:
  - In the operator namespace: `get`, `list` and `watch` on `incidents`, `remediationrequests` and `verificationreports` (API group `remediation.shadewright.io`).
  - Cluster-scoped: `get` on `remediationsettings`, and `get` on the Namespace `kube-system` by `resourceNames` (for its UID).
  - **Open (question 8):** the Flux reads that the heartbeat's delivery report (section 8) needs. The candidate is `get` and `list` on `gitrepositories` and `kustomizations` (groups `source.toolkit.fluxcd.io` and `kustomize.toolkit.fluxcd.io`), cluster-wide or in the Flux namespace. Until that is decided the reporter has no Flux read.
  - `get` on the one ConfigMap `shadewright-cluster-registration` by `resourceNames`, and `get` on the non-resource URL `/openid/v1/jwks`.

  Its writes, exactly: `update` on that one ConfigMap, by `resourceNames`. `create` cannot be limited by name, so the chart creates the ConfigMap and the reporter never creates anything. No other new grant.

  It has no Secrets, no Git identity and no model credential. Egress, by NetworkPolicy: DNS (UDP and TCP 53 to the cluster's DNS), the API server (443 and 6443, as the existing rule of design 10), and TCP 443 for the hub. A NetworkPolicy cannot match host names ([design 10](../design/10-security.md)), so the network does not limit the reporter to the hub. The hub's host is pinned in the reporter's settings, and the reporter takes no URL from cluster data or from the hub's replies. Its client follows no redirects, ignores the environment's proxy settings and requires verified TLS 1.2 or later. It reads the settings status only to report it, never to decide anything (`AGENTS.md`, invariant 6).

At the hub:

- **`intake`**: an HTTPS service, stateless, N replicas. It verifies the token, validates and checks the report (sections 6, 8 and 10) and stores it. It reads `Cluster` objects with their status and has no write on them. It makes no outbound connection: its egress is the hub's API server only. It is not the scoring intake of ADR-0047, and the binary is not `cmd/intake`.
- **`fleet controller`**: alone writes `Cluster.status` (`update` on `clusters/status`): `Ready`, `Reporting`, `Stale`, `IdentityConflict`, and for `Discovery` the issuer keys it fetched (section 6). It sets `Ready` only when the spoke's Namespace, quota and RoleBindings exist, no identity conflict holds, records of an earlier registration are gone (section 3) and, for `Discovery`, keys were fetched. Its only other write is `delete` on the reported kinds in spoke namespaces, for that clean-up. Its egress is the API server and the issuer fetch of section 6.
- **`status page`**, read-only, its own identity `shadewright-fleet-status`: `get`, `list` and `watch` on `clusters`, and on the reported kinds in each spoke namespace through its RoleBinding there. No write, no Secrets, and no egress but the hub's API server. In phase 1 it is served only on a ClusterIP Service, with no Ingress or LoadBalancer. A person reaches it with `kubectl port-forward`, so the hub's API server authenticates the person and RBAC decides who may (`create` on `pods/portforward` in the hub's namespace). Whoever reaches it sees every cluster (question 5). It holds no credential.

In phase 1 the hub has no model credential, no Git write identity and no notifier (section 13).

**Hub RBAC (Proposed).**

- The chart ships static ClusterRoles: intake's writes on the reported kinds, the fleet controller's `list` and `delete` on them, and the status page's reads. Each is bound in a spoke namespace only by the RoleBinding of the onboarding pull request (section 3).
- Cluster-wide, by the chart: the fleet controller may `get`, `list` and `watch` `clusters`, Namespaces, ResourceQuotas and RoleBindings, and `update` `clusters/status`; the status page may `get`, `list` and `watch` `clusters`. No hub identity creates or patches a Namespace, RoleBinding, ResourceQuota, Role or ClusterRole, or holds `bind` or `escalate`. So no admission policy is needed to keep them out of `kube-system` or the operator namespace. If a later change gives a hub component one of those writes, it comes with a ValidatingAdmissionPolicy with `failurePolicy: Fail` that checks both CREATE and UPDATE.
- **RBAC does not enforce the boundary between spokes.** Intake is one ServiceAccount, bound in every spoke namespace. The guard is intake's code: the namespace a report is written to comes from the verified `Cluster` object, never from the report or the request path. The build needs tests that a report cannot reach another cluster's namespace. Per-namespace impersonation by intake would move the guard into RBAC; it is an open question (12).
- No hub identity is `cluster-admin`.

[![Identities of control plane mode and what each may write: the three spoke identities keep their writes, the reporter reads, sends reports and updates only its own registration ConfigMap, intake writes only the reported kinds in the namespace of the cluster it verified and makes no outbound connection, the fleet controller alone writes Cluster status and deletes records of an earlier registration, the status page only reads and is reached by port-forward, spoke namespaces, quotas and RoleBindings come from the reviewed onboarding pull request, and no hub component has a model credential or a Git write](../assets/diagrams/control-plane-identities.svg)](../assets/diagrams/control-plane-identities.svg "Open full size")

### 5. An intake service instead of the hub's Kubernetes API (Proposed)

The spoke posts to intake, and intake writes with its own identity. Reasons:

- Onboarding a spoke as an API server JWT authenticator would be a change to the cloud control plane, not only a reviewed merge in the hub's GitOps repository. On AKS, structured authentication offers JWT authenticators in preview ([Microsoft Learn](https://learn.microsoft.com/azure/aks/external-identity-provider-authentication-overview)), configured per authenticator through the Azure API (`az aks jwtauthenticator add`), and it requires a publicly accessible discovery endpoint (vendor docs, checked 2026-11-14). It is a preview feature and specific to one vendor. Other vendors were not checked.
- Inline keys have no path through an API server authenticator.
- Validation, redaction checks and rate limits would have no single place; at intake they sit in one.
- A hub as a plain service later changes nothing for spokes.

The transport stays behind an interface ([ADR-0013](0013-adapters-for-every-external-system.md)).

### 6. How intake checks a spoke (Proposed)

**Finding the `Cluster` object.** Kind and kubeadm clusters share the default issuer, and every install has the same reporter subject, so issuer plus subject cannot identify a spoke that uses inline keys. The reporter therefore posts to a path that names its cluster. The name is a hint and is not trusted: intake loads that one `Cluster` object and verifies the token against that object's issuer, keys and subject.

The reporter sends a projected ServiceAccount token with an audience that names this hub and a lifetime of about 10 minutes. Intake checks the signature, `iss`, `sub`, the audience and the expiry. `iss` and `sub` must equal those in the object. The keys are `spec.identity.jwks` for `Inline`, and for `Discovery` the keys the fleet controller fetched into the object's status.

The `kube-system` UID travels in the report and is compared with the object. This is not authentication. It guards against a rebuilt cluster that reuses an issuer or keys. The cluster ID inside a report is compared with the object too, and a mismatch is refused.

**Order of checks.**

- Before the token verifies: a rate limit per source address, then one generic refusal with no reason. A token with a key ID that the object does not hold is refused the same way. Nothing a request carries makes the hub fetch anything.
- After it verifies: the per-cluster rate limit and the named reasons (`not ready`, `suspended`, over quota, `redaction`). A revoked spoke, whose `Cluster` object is gone, never gets this far and sees the generic refusal.

**Issuer keys for `Discovery`.** The fleet controller fetches them on a schedule (proposed: every 10 minutes, not final) and writes them with the hub's fetch time into `Cluster.status`. A key the issuer added is usable after the next fetch, and one it removed is dropped then. If fetches fail, intake keeps using the stored keys until their fetch time is older than a maximum age (proposed: 24 hours, not final, compared with the hub's clock) and then refuses every token for that cluster. The fleet controller reports each failure as a condition (`KeysUnavailable`) and an Event on the `Cluster` object. A failed fetch is retried, never a reason to trust old keys longer. A hub restore loses nothing: the fleet controller fetches again.

The fetch takes its URL only from a reviewed object and has these limits:

- The issuer host must match an allow-list of suffixes in the hub's settings. This limits where the hub fetches. It does not show that an issuer belongs to a cluster (on AKS, issuers share a host per region), so the reviewer checks the full issuer URL.
- It fetches `<issuer>/.well-known/openid-configuration`, and the document's `issuer` must equal `spec.identity.issuer` exactly.
- Its `jwks_uri` must be `https`, on the same host and port as the issuer.
- Both fetches: https only, verified TLS 1.2 or later, no redirects, no environment proxy, a size limit (proposed: 64 KiB each) and a time limit.
- The dial guard of [design 10](../design/10-security.md) checks every resolved address at connection time and refuses loopback, private (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, IPv6 unique local), link-local (169.254.0.0/16, fe80::/10), 100.64.0.0/10 and the hub's declared in-cluster ranges.

**Uniqueness.** For `Discovery`, issuer plus subject must be unique. For `Inline`, no two objects may share a signing key, compared by JWK thumbprint ([RFC 7638](https://www.rfc-editor.org/rfc/rfc7638)), not by key ID. On a conflict the fleet controller refuses both objects (condition `IdentityConflict`). Neither is accepted until a reviewed change resolves it.

### 7. The `Cluster` object (Proposed)

API group `fleet.shadewright.io/v1alpha1`, cluster-scoped, applied by Flux from the hub's GitOps repository.

- `spec.clusterId`: equal to the spoke's `clusterId` and to `metadata.name`, enforced by a CEL rule. It is a DNS label short enough that the spoke namespace prefix plus the name fits 63 characters.
- `spec.identity`: `issuer`, `keys` (`Discovery` or `Inline`), `jwks` (`Inline` only), `subject`, `kubeSystemUID`. The spec holds only reviewed keys.
- `spec.suspended`: the reversible stop (section 3).
- Status, written by the fleet controller only: conditions `Ready`, `Reporting` (last seen), `Stale`, `IdentityConflict` and `KeysUnavailable`, the reported versions, and for `Discovery` the fetched keys with their fetch time.

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

A typed allow-list, not the objects themselves. Every field has a type, a length bound and, where it names a state, an enum:

- A **heartbeat**: operator, chart and pack versions, module status, and the delivery report of `doctor` as a typed list of at most 256 sources (the cap `doctor` uses). Each source has the Kustomization and GitRepository as namespace and name, the repository's host and path, the branch, `settings` (`named`, `not-named`, `ambiguous`, `unknown`), `state` (`ok`, `refused`, `not-ready`, `unknown`) and a reason from the source mapping's fixed set. The free-text `detail` of `doctor` is not sent.
- **Summaries of Incidents**: fingerprint, subject kind, namespace and name, reason, phase, times.
- **Summaries of requests**: action type, target, phase, pull request URL, condition reasons.
- **Outcomes**: the request's name, its final phase (one of the request phases of `api/v1alpha1` that end a request: `Succeeded`, `Denied`, `Failed`, `Expired`, `Cancelled`, `Superseded`, `NeedsIntervention`, `Inconclusive`) and the time it reached it.

There is no evidence, no prompt, no transcript, and no free text from cluster content beyond bounded fields redacted in the spoke by the redactor of [ADR-0048](0048-redaction-in-a-shared-module.md) ([design 10.3](../design/10-security.md#103-redaction)). A setting lets a spoke send names hashed.

The hub stores these as kinds of their own. Their working names are `ReportedIncident` and `ReportedRequest`. They are never stored as `Incident` or `RemediationRequest`: no hub controller may act on an object a spoke supplied (invariants 3 and 4).

### 9. Delivery (Proposed)

Delivery is level-based and at least once.

- The reporter sends the current state on change, and a full list periodically.
- Intake upserts by the existing deterministic names (invariant 5) and drops an out-of-order write. How it orders writes is open (question 6).
- The reporter keeps nothing it cannot rebuild from the cluster's objects.

**Candidate for ordering (open question 6).** Each report carries the reporter's epoch (its start time) and a counter. Intake accepts only a pair higher than the last one stored for that cluster. The epoch is compared only with other epochs from the same reporter, never with the hub's clock.

**Full lists.** A full list is a snapshot, sent as numbered pages with a completion marker. Nothing is marked gone until the snapshot is complete. A snapshot cut short by the quota, or by a lost page, marks nothing gone.

Staleness is decided from recorded state on one clock: the time the hub itself recorded when it accepted a heartbeat, compared with the hub's clock. A time a spoke reports is shown, never used to decide whether data is stale.

### 10. Limits against a noisy or compromised spoke (Proposed)

- A rate limit per source address before the token verifies, and one per cluster after. A body size limit at intake.
- An object-count quota per spoke namespace.
- Schema validation of the types of section 8, with unknown fields refused and every field bounded.
- **The redactor again.** Intake runs the shared redactor of [ADR-0048](0048-redaction-in-a-shared-module.md) (`Redactor.Clean`) over every decoded string and key of a report, and refuses the whole report with the named reason `redaction` when it would change anything. Nothing is redacted and stored. As in [ADR-0047](0047-scoring-analysis-records-outside-the-cluster.md) section 3, values whose shape intake checks in code (the deterministic Incident and request names, fingerprints) are kept, because they trip the redactor. Intake's redactor is that of its own build, so a rule added after a spoke was built catches what the older spoke let through. A refusal is counted with the rule kinds that matched, never the text.
- **Pull request URLs** must parse as absolute `https` URLs with no userinfo. Any other is refused at intake, so the status page never renders a `javascript:` or credential-bearing link.
- The status page escapes everything.
- Nothing a spoke sends reaches a model, a notification or a Git write in phase 1.

### 11. Kill switch (Proposed)

Per cluster: `spec.suspended` on its `Cluster` object, which is reversible, or revocation (section 3). Fleet-wide: the hub's settings pause intake. In phase 1 these only stop reporting.

### 12. The rule for later phases: the hub can only make a spoke do less (Proposed)

A reply to a report may carry restrictive directives (pause remediation, slow down), never permissive ones. So a compromised hub can stop remediation in a spoke, but cannot cause it.

A directive from the hub is held apart from the spoke's own kill switch (`docs/emergency.md`) and only adds to it. Lifting the hub's pause never lifts a local pause, and only the spoke's operator lifts a local one.

Configuration for a spoke (pack references, model profiles, policies) travels as a pull request to that spoke's GitOps repository (invariant 3), never as a directive.

### 13. Phases (Proposed)

1. **Fleet view**: reporter, intake, fleet controller, `Cluster`, the read-only status page with the identity and access of section 4, staleness. No notifications. This is what the rest of this ADR specifies, and what Shadewright 0.7.0 ships.
2. **Guard rails**: restrictive directives, a fleet-wide pause, configuration by pull request, and notifications: the notifier of [#97](https://github.com/shadewright-labs/shadewright/issues/97), with its own identity, its credential named by an ADR under the static credentials rule of invariant 6, spoke text fenced and mentions neutralised.
3. Each of these has its own ADR and is not decided here:
   - Remediation through the hub: a central executor holding the Git App. It would have to re-evaluate spoke proposals against policy held at the hub, treating them as untrusted input.
   - Horizontal scaling by key ownership.
   - The `StateStore` on Table and Blob Storage.

### 14. Prototype (Proposed)

One AKS lab cluster is the hub and also a spoke of itself (issuer discovery). A local Kind cluster is a second spoke (inline keys, outbound to the hub). So no second AKS cluster is needed.

**The prototype does not test the hub and spoke boundary.** With hub and spoke in one AKS cluster, the separation between them does not hold for that cluster: the hub's components share it with an executor that holds the Git App. The prototype tests registration and reporting, not that boundary. The public intake endpoint is also in the cluster that runs the executor and analyzer, so one rule holds there: the spoke namespace prefix is never in `watchNamespaces` of any component. **The lab cluster does not last.** `make lab-down` in `shadewright-azure` destroys the lab AKS cluster after every session, so its issuer URL and the UID of its `kube-system` namespace change on every apply, and the hub's state goes with it. So each lab session registers its spokes again, by a pull request to the hub's repository. It is proposed that the lab script prints the registration at the end of `make lab-up`, so that the pull request needs no hand-copied values.

**Price (2026-11-14, `make lab-cost`, EUR, Sweden Central, Azure Retail Prices API).** The lab cluster costs 0.1992 per hour (one Standard_D4as_v5 node 0.1652, OS disk 0.0071, outbound public IPv4 0.0045, load balancer 0.0224, AKS Free tier). Hub and one spoke run in it and the second spoke on a local Kind cluster, which costs nothing. The only addition is one inbound public IPv4 for the intake service, 0.0045 per hour: about 0.204 per hour, or 1.63 for an 8 hour session. A second AKS cluster as the hub would double that to about 0.40 per hour. Usage-based items (data processed, egress, model tokens) are not included.

### One report, from token to stored record

```mermaid
sequenceDiagram
    participant R as Reporter (spoke)
    participant I as Intake (hub)
    participant K as Keys in the Cluster object
    participant N as Spoke namespace (hub)
    R->>R: Build the typed summaries from the cluster's objects, redact, bound the fields
    R->>R: Project a ServiceAccount token (audience: this hub, about 10 minutes)
    R->>I: HTTPS POST to the path naming its cluster, with the token and the report
    I->>I: Refuse an oversize body or a source address over its rate limit
    I->>I: Load the one Cluster object the path names (a hint, not trusted)
    I->>K: Inline keys from spec, or Discovery keys the fleet controller stored in status
    alt No such object (never registered, or revoked), or the token does not verify against that object
        I-->>R: One generic refusal, no reason
    else Token verifies
        I->>I: Per-cluster rate limit
        alt Not ready, suspended, over quota, rate limited, or a kube-system UID or cluster ID that differs from the object
            I-->>R: Refused with a named reason, counted in a metric
            R->>R: Set a condition, back off to a slow retry, keep working standalone
        else Passes
            I->>I: Validate the schema, refuse unknown and over-long fields and a non-https pull request URL
            I->>I: Run the redactor, refuse the report (reason redaction) if it would change anything
            I->>N: Write into the namespace the Cluster object gives, stamped with its UID, upsert by deterministic names, drop an out-of-order write
            I-->>R: Accepted
        end
    end
    Note over I,K: Intake fetches nothing. The fleet controller fetches issuer keys on a schedule and reports failures on the Cluster object.
    Note over I,N: The hub records its own receipt time for the staleness conditions
```

Whether a refusal that is not about the cluster's state (a rate limit, a malformed body) tells the reporter to back off the same way as a refusal for a suspended cluster is open. The status codes and reason names are for the build task.

## How invariants 1 to 6 hold in phase 1

1. **Model output is never authorization.** The hub has no model credential, and nothing a spoke sends reaches a model there. The analyzer and the policy gate in each spoke are unchanged.
2. **Actions are a closed catalog.** Phase 1 adds no action and no way to ask for one. A report is data about requests that already exist. The directives of phase 2 can only restrict.
3. **Changes go through Git only.** The hub writes nothing to a spoke and nothing to any Git repository in phase 1. A new spoke, its namespace and its bindings at the hub, a key rotation and a revocation are each a reviewed pull request to the hub's repository. Configuration for a spoke, when it comes, is a pull request to that spoke's repository.
4. **Everything from the cluster is untrusted input.** The spoke redacts before anything leaves it. The hub validates again, bounds every field, refuses a report its redactor would change, stores spoke objects as kinds of their own that no controller acts on, and escapes what it shows. Nothing a spoke sends changes who the hub trusts: keys come from the reviewed object or from the issuer.
5. **Reconciliation is idempotent.** Delivery is at least once. Intake upserts by deterministic names, so a repeated or replayed report changes nothing. Dropping an out-of-order report depends on the ordering rule (question 6).
6. **Separate identities.** The reporter is a new spoke identity, `shadewright-reporter`, with read-only access on the cluster's objects and no Secret, Git identity or model credential. Its only write is `update` on its one registration ConfigMap by `resourceNames`. At the hub, intake writes only the reported kinds and has no write on `Cluster`. The fleet controller writes only `clusters/status` and deletes records of an earlier registration. The status page only reads. No hub identity creates Namespaces, RoleBindings or quotas, or holds `bind` or `escalate`. RBAC does not keep intake to one namespace: that guard is intake's code, and it needs tests. No hub component has a model credential or a Git write identity. The build PR has to add the reporter, intake, the fleet controller and the status page to invariant 6; this ADR does not edit `AGENTS.md`.

## Threats and what stops each

| Threat | What stops it | What is left |
|---|---|---|
| A cluster pretends to be another spoke | The path names a `Cluster` object, a hint that only selects what to verify against. The token is checked against the keys, `iss` and `sub` of that reviewed object, with an audience that names this hub. The namespace written to comes from the object in intake's code, and a cluster ID in the report that differs is refused. The `kube-system` UID is compared too, but it only guards against a rebuilt cluster. | Anyone who can mint a token for the reporter's ServiceAccount in the spoke can report as that spoke, with that spoke's standing only. RBAC does not stop a bug in intake's code from writing to another namespace; tests and question 12 cover it. |
| A name is claimed twice | A `Cluster` is a reviewed merge, and `spec.clusterId` equals the object name. For `Discovery`, issuer plus subject must be unique; for `Inline`, no two objects share a signing key by thumbprint. On a conflict both objects are refused (`IdentityConflict`). | None known. |
| Token replay | The audience names this hub and the lifetime is about 10 minutes. A replayed report upserts the same state. Dropping an older one depends on question 6. | A captured token can be used until it expires, from anywhere that reaches intake. No nonce is proposed. |
| A flooding or poisoning spoke, or an unauthenticated flood | Before the token verifies: a rate limit per source address and one generic refusal; no request makes the hub fetch anything. After: a per-cluster rate limit with named reasons. Body size, an object-count quota per namespace, typed and bounded fields, unknown fields refused. Nothing reaches a model or Git in phase 1. | A spoke can fill its own quota with false records and show wrong data about itself. |
| Secrets stored at the hub by an old or compromised spoke | Intake runs its own build's redactor over every string and key and refuses the report if it would change anything. | A secret the redactor does not recognise. |
| Stale data read as current | Staleness is decided from the hub's own recorded receipt time. A suspended spoke's data, and a revoked one's during the retention, stays readable and is marked stale or removed. Records of an earlier registration are deleted before a re-registered spoke is `Ready`. | A viewer who ignores the mark. |
| A compromised hub | It holds no kubeconfig or spoke credential, never calls a spoke, and has no Git write identity in phase 1. Replies carry only accepted or refused, and later only restrictive directives. | It can show false data. It can pause remediation in later phases. It cannot cause an action. In the prototype, hub and spoke share one AKS cluster with an executor that holds the Git App, so this row does not hold for that cluster. |
| A compromised fleet controller | It writes only `Cluster.status` and deletes reported records in spoke namespaces. It creates no namespace or binding. | It can make the hub trust wrong keys for a `Discovery` cluster, or delete reported records. |
| Injection through the status page | Fields are typed, bounded and redacted twice. Pull request URLs are `https` without userinfo. The status page escapes everything. It is reachable only by port-forward. | Not built; the build tasks need tests for each. |
| A tampered registration ConfigMap | The ConfigMap is a convenience, not evidence. A cluster is trusted only after the reviewed merge, where the reviewer checks the full issuer URL against the provider record, and `kubectl shadewright registration` compares every identity field with the live cluster. Everything in it is public. | A reviewer who merges without the comparison. With `Inline`, wrong keys that get merged make the hub trust whoever holds the matching private key. |
| A public intake endpoint in the cluster that also runs the executor and analyzer (prototype) | Intake is the only hub component reachable from outside. It holds no Git identity, model credential or egress, verifies the token before it parses a body, and has no write on `Cluster`. The spoke namespace prefix is never in any component's `watchNamespaces`. | In the prototype the endpoint shares a cluster with the executor that holds the Git App, so the prototype does not test this boundary (section 14). A later hub gets its own cluster. |
| SSRF through issuer discovery | The URL comes only from a reviewed object, and no request triggers a fetch. The issuer host is on an allow-list, the discovery `issuer` must match exactly, `jwks_uri` is https on the issuer's host, no redirects or proxy, size and time limits, and the dial guard refuses loopback, private, link-local, 100.64.0.0/10 and in-cluster addresses. | A public host on an allowed suffix that serves something other than an issuer. |

## Consequences

**Easier.** One view of all clusters, with no evidence, prompts or credentials gathered in one place. Each spoke is as safe as it is alone: it analyses, proposes and remediates with its own credential and budget, whether the hub is up or not. Adding a cluster uses the review path of every other change, and the hub's identities have no write that could place a binding outside a spoke namespace.

**Harder.**
- There is a new API group, a new set of components to build and operate, and a new spoke identity, `shadewright-reporter`, with an `AGENTS.md` change in the build PR.
- The hub is a service that spokes depend on for the fleet view only. Its availability is an open question.
- Every inline key rotation is two reviewed pull requests at the hub, and a spoke rebuild needs one.
- Deleting a removed spoke's namespace is a second pull request that someone has to open after the retention.
- Hashed names (an optional setting) make the fleet view harder to read.

**Accepted.**
- In phase 1 the fleet view is read-only and sends no notifications. The hub shows and proposes nothing.
- Reports are as true as the spoke that sends them. A compromised spoke can lie about itself.
- Cost: the prototype is about 0.204 per hour, 1.63 for an 8 hour session (section 14), and the lab is destroyed after each session.

## Alternatives considered

- **Automatic key updates for inline keys (`keyUpdates: Automatic`).** Decided on 2026-11-14 and dropped by this revision on 2026-11-27. A spoke would list its new keys in its reports, intake would record them, and the fleet controller would trust a key after it was listed in several authenticated reports over a window. Review found that it bent invariant 4 (data a spoke sends changing whom the hub authenticates as that spoke), that someone with brief access in a spoke could mint long-lived tokens and replay them across the window, that one spoke could list another's next key and block that spoke's rotation and onboarding, that stale observations could promote discarded keys after a suspension or re-adoption, and that a hub restore lost every promoted key. It needed a new kind, a notifier and three more open questions. Reviewed rotation (section 3) costs two pull requests per rotation and removes all of it.
- **Promotion by tokens signed with the pending key.** Part of the design above, not chosen even there: such a token shows that its signer holds the private key, not that the cluster's API server signs with it.
- **The fleet controller creates spoke namespaces, quotas and RoleBindings.** The first draft. RBAC cannot limit `create` on Namespaces by name, nor a cluster-wide RoleBinding or ResourceQuota grant to certain namespaces, so it needed a ValidatingAdmissionPolicy, and a gap in that policy (failure policy, UPDATE checks) would let a compromised controller bind intake's writes in `kube-system` or the operator namespace. Putting these objects in the reviewed onboarding pull request removes the grant and the policy. It also makes deleting a removed spoke's namespace a reviewed change rather than a controller timer.
- **Intake fetches issuer keys itself.** It would give the one component reachable from outside an egress path, and a refresh on an unknown key ID would let an unauthenticated request trigger a fetch. Intake also cannot write `Cluster.status` to report a failed fetch. The fleet controller fetches instead.
- **Spokes write to the hub's Kubernetes API directly.** Rejected. Onboarding would be an Azure control plane change on AKS (JWT authenticators are a preview feature there, configured per authenticator through the Azure API, and they need a public discovery endpoint; other vendors were not checked), inline keys have no path, validation and rate limits would have no single place, and a plain-service hub would later change every spoke.
- **The analyzer at the hub.** Evidence and redacted prompts would cross to the hub, analysis would stop when the hub is down, a channel into every spoke would be needed to read evidence on demand, and one model credential and budget would serve all clusters. Decided against on 2026-11-14.
- **The hub as a plain service from the start.** It needs a `StateStore` on Table and Blob Storage before there is a reason to have one, and gives up namespaces, RBAC, quotas and Flux for the hub's own configuration. The `StateStore` interface keeps this open for phase 3.
- **A bootstrap token exchanged once for a client certificate, as `kubeadm join` does.** It needs a shared secret to hand out and protect, a hub CA, and certificate rotation. The reviewed object with inline public keys needs none of them. It was a fallback candidate on [#559](https://github.com/shadewright-labs/shadewright/issues/559) and was replaced by the decision of 2026-11-14.
- **A message broker (Service Bus, NATS) now.** It adds a component, a credential per spoke and a cost, to carry level-based reports that are a small HTTPS post. Not needed for phase 1. It can come back behind the same transport interface.
- **Reusing the `Incident` kind at the hub.** A hub controller that reconciles `Incident` or `RemediationRequest` could act on an object a spoke supplied, which breaks invariants 3 and 4, and the same name would mean two things. The hub stores `ReportedIncident` and `ReportedRequest`, which nothing acts on.

## Open questions

1. Where the executor and the Git App sit in the phase that remediates through the hub, and how the hub's policy evaluates a spoke's proposal (phase 3, its own ADR).
2. Hub high availability: how many replicas of intake, what the state store gives, and what the status page shows while the hub restarts.
3. Retention of reported records while a spoke is registered, and the length of the retention after a spoke is removed (30 days is proposed in section 3, not decided). Whether a removed spoke's reported objects should be exported or archived before the namespace is deleted.
4. Whether hashed names should be the default instead of an option.
5. Multi-tenancy at the hub: who may read which cluster's namespace, and whether the status page shows every cluster to every viewer (in phase 1 it does, to whoever may port-forward to it).
6. How intake orders reports. An object's `resourceVersion` is opaque and must not be ordered. The candidate is the reporter's epoch plus a counter (section 9), and it is not chosen. How a reporter keeps the counter across a restart is part of it, and so is a clock that steps back across a restart, which would give a lower epoch that intake refuses until the stored pair is cleared.
7. Where the reporter records the condition it sets after a refusal. It has no write on Incidents or requests. **Proposed answer:** the `status` key of its registration ConfigMap (section 3), which needs no other grant. Open until the ADR is accepted.
8. How the heartbeat gets the delivery source report of `doctor`. `doctor` is a kubectl plugin that runs with a person's permissions and reads Flux objects, and the reporter's reads above do not include them. Either the reporter gets that read (the candidate is in section 4), or the report comes from state another component already records.
9. Closed, 2026-11-27: `spec.clusterId` equals the object name, and the spoke's namespace is the prefix plus that name (section 7).
10. Whether a refusal for a rate limit or a malformed body makes the reporter back off like a refusal for a suspended cluster, the reason names, and the leeway for clock difference when intake checks a token's expiry.
11. Closed, 2026-11-27: the issuer fetch refuses private, link-local, 100.64.0.0/10 and in-cluster addresses after name resolution (section 6).
12. Whether intake should impersonate a per-namespace identity when it writes, so that RBAC enforces the boundary between spokes, and the cost of that.
13. Closed, 2026-11-14: the price of the prototype ([#669](https://github.com/shadewright-labs/shadewright/issues/669)) is in section 14.
14. Whether the chart value that picks `Discovery` or `Inline` for the registration manifest has `Inline` as its default, and its name.
15. Whether the hub's repository gets a CI check that fetches the issuer's discovery document for a new `Cluster` object and reports whether keys resolve, and who owns it.
16. The name of the command that prints the registration (`kubectl shadewright registration` is a proposal), and whether the hub's status page shows the expected shape of the manifest.

## Amendments

None.
