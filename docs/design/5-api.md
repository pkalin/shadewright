# 5. API

Group `remediation.shadewright.io`, version `v1alpha1` ([ADR-0009](../adr/0009-api-group-and-docs-domain.md)). Schemas are generated from Go types by Kubebuilder.

## 5.1. RemediationRequest

A namespaced CR that holds one concrete plan, the policy decision and the execution state. Example (values are illustrative):

```yaml
apiVersion: remediation.shadewright.io/v1alpha1
kind: RemediationRequest
metadata:
  name: orders-keda-capacity-001
  namespace: shadewright-system
spec:
  incidentRef:
    name: orders-scaling-001
    uid: "11111111-1111-4111-8111-111111111111"
  targetRef:
    apiVersion: keda.sh/v1alpha1
    kind: ScaledObject
    namespace: orders
    name: orders-worker
    uid: "22222222-2222-4222-8222-222222222222"
  mode: GitOps
  approvalRequired: true
  policyRef: keda-capacity
  action:
    type: AdjustKedaMaxReplicas
    adjustKedaMaxReplicas:
      previousValue: 10
      desiredValue: 15
  verification:
    timeout: 10m
    stableFor: 3m
    checks:
      - DeliveryRevisionApplied
      - KedaReady
      - QueueBacklogDecreasing
status:
  observedGeneration: 1
  phase: AwaitingApproval
  effectiveApprovalRequired: true
  source:
    repositoryRef: fleet
    targetBranch: main
    path: apps/orders/scaledobject.yaml
    buildPath: apps/orders
    baseCommit: "0123456789abcdef0123456789abcdef01234567"
  planDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  policyRevision: "keda-capacity/3"
  policyUID: "44444444-4444-4444-8444-444444444444"
  pullRequest:
    number: 42
    url: https://github.com/example/fleet/pull/42
    headSHA: "89abcdef0123456789abcdef0123456789abcdef"
    renderedTargetHead: "fedcba9876543210fedcba9876543210fedcba98"
  conditions:
    - type: PolicyAllowed
      status: "True"
      reason: WithinApprovedBounds
      message: "The change is within the policy bounds"
      observedGeneration: 1
      lastTransitionTime: "2026-11-01T10:00:00Z"
```

Each action type has its own typed parameter struct (a discriminated union on `action.type`), so validation happens in the schema rather than in a free-form map. The MVP catalog has two actions: `AdjustKedaMaxReplicas` and `SetContainerMemoryLimit`. Schema rules are enforced by the API server itself (OpenAPI and CEL), before any controller sees the object, and are covered by envtest tests in `test/apivalidation`. Valid examples of every kind are in `config/samples`, and a test applies them.

Schema and admission rules:

- `approvalRequired` defaults to `true`; `GitOps` is the default and, in the MVP, the only mode.
- The action type is an enum; parameters are typed and bounded. Memory values are whole bytes between 1Mi and 1Ti.
- Each action type fixes the kinds it may target: `AdjustKedaMaxReplicas` only a `keda.sh` ScaledObject (the policy check further requires `keda.sh/v1alpha1`, the only version KEDA 2.x serves and the one the readiness rule reads; another version is `Denied`, `KindNotAllowed`), `SetContainerMemoryLimit` only an `apps/v1` Deployment or StatefulSet. The target must be namespaced and pinned by UID.
- The spec says what to change, never where. The repository, branch and file are resolved by the controller from the Flux objects that deliver the target ([ADR-0012](../adr/0012-object-to-source-mapping.md)) and written to `status.source`. A request cannot point a change at another file, another repository or an unprotected branch.
- `policyRef` points to admin-managed configuration, and the policy decides which namespaces may use it (`requestNamespaces`). The model never chooses a URL, host, branch or credential reference.
- The target namespace and object are authorized independently, including cross-namespace references.
- Any change to the plan, resolved source, repository settings or policy invalidates an earlier approval. The digest covers the semantic change, the target UID, the resolved source with the repository URL and installation from settings, and the policy UID and generation (a recreated policy can reach the same generation again, never the same UID). It is `sha256:` over a versioned canonical encoding: every field labelled, length-prefixed and in a fixed order, and memory quantities written by value, so `256Mi` and `268435456` give the same digest (`internal/policy/digest`).
- The digest is fixed when the branch is created: the policy check stores it in `status.planDigest` in the same status write that sets `PolicyChecked`, from the very policy, settings and source the decision was made on. The branch is created later, at the head of the target branch as it is then, after the freshness check has confirmed that the target file there still holds the previous value. Later merges to the target branch do not change it: before each write the controller checks that the target file at the branch head still holds the previous value. If an input of the digest changes, the request becomes `Superseded`, its PR is closed and a new request is needed; the branch is never rebuilt or force-pushed. The check (`internal/policy/freshness`) reads the policy and settings from the API server, never the cache, compares the policy UID and generation and recomputes the digest from the live settings, confirms the target still has the planned UID, and reads the target file at the head of the target branch (not of the PR branch). Any error reading an input is "not fresh": the write waits and is retried, and only a definite change supersedes. The one exception is repository settings that can never open a Git provider: they end the request in `NeedsIntervention` (`RepositoryUnusable`) without closing anything. While `RemediationSettings.spec.paused` is set the write waits too. The cleanup runs in three steps: a status write that sets `PlanFresh` `False` with the stale reason and leaves the phase as it is (the mark that the cleanup has started), the idempotent close of the change request, and the status write that sets `Superseded`. A crash, restart or deadline anywhere in between leaves the mark, and the next reconcile finishes the cleanup without checking the plan again: it closes again (a no-op if the first close went through) and supersedes. A recorded pull request found closed while the mark is set was closed by that cleanup, so it ends `Superseded`, not `NeedsIntervention` (`ChangeRequestClosed`), even if the plan looks fresh again by then.
- Verification can only be made stricter: the controller always runs the checks the action needs, `stableFor` is at least 1m, and `timeout` is at most 24h and longer than `stableFor`.
- `approvalRequired: false` is a request for automation, not a way to lower central policy.
- The whole spec is immutable: a different plan is a new request, so an approval can never carry over to a changed plan.
- `/status` subresource, standard conditions and `observedGeneration` are part of the implementation.

Only the user or analyzer writes `metadata` and `spec`. Only the controller writes `status`. RBAC on `/status` separates proposal from decision and execution.

Raising `maxReplicaCount` makes sense only after confirming that the replica cap is the cause, and that the cluster and the downstream dependency have capacity. A backlog alone is not enough evidence. One precondition is enforced by deterministic code, not left to the evidence the model was shown (invariant 1): the policy check refuses `AdjustKedaMaxReplicas` unless the live `ScaledObject` is ready by the `KedaReady` rule (`Ready=True`, not paused, partial pauses and the annotations `autoscaling.keda.sh/paused`, `paused-replicas`, `paused-scale-in` and `paused-scale-out` included whatever their value, not being deleted, not on fallback, `HPAActive` not `False`). A missing or malformed object is not ready. A definite not-ready state is `NeedsIntervention` with reason `ScaledObjectNotReady` (section 6). A `ScaledObject` KEDA has not reported on yet (no status, no `Ready` condition, `Ready=Unknown` other than `PartialTriggerError`) is waited for, for up to 10 minutes, and then ends the same way. The freshness check before every write applies the same rule, confirmed by a second read.

## 5.2. Incident

A namespaced CR in the operator namespace, next to the requests that point at it ([ADR-0007](../adr/0007-incident-state-in-crds.md)), that holds the fingerprint, counters, first and last seen, the correlated owner, and references to analyses and requests. Large data (log excerpts, full model transcripts) lives outside etcd, or is dropped after analysis in the MVP.

## 5.3. RemediationPolicy

A cluster-scoped CR, writable only by the cluster admin ([ADR-0011](../adr/0011-policy-as-cluster-scoped-crd.md)). Each policy names the action types it allows, the target selector (namespaces and labels; kinds are fixed per action by the catalog), parameter bounds, the Git paths it may patch, the namespaces whose requests may use it, and whether review is always required. `metadata.generation` together with the policy UID is the revision recorded in every request's digest. Safe defaults:

- `approval` is `Required`.
- A policy must name its target namespaces: there is no "all namespaces".
- `requestNamespaces` is `[shadewright-system]`. A request from any other namespace is `Denied`, so the right to create requests somewhere does not grant a policy.
- Bounds have a minimum and a maximum, and `maxDecreasePercent` is 0: decreases are refused unless the admin allows them.
- `allowedPaths` are globs with a literal first segment. `*` and `**` never match a segment that starts with a dot, and no segment may start with a dot or a dash, so a policy cannot cover the whole repository, `.github/` or option-like names.

The decision is a pure function (`internal/policy`): it reads the request, the policy, the settings, the resolved source and the live target, and returns `Allowed`, `Denied` or `NeedsIntervention` with a machine-readable reason (one per check) and the effective approval. It uses no client and no clock, so the same input always gives the same decision. Missing input (no resolved source, no live target) gives `NeedsIntervention`; every other failed check gives `Denied`, except the target state check: a `ScaledObject` that is not ready gives `NeedsIntervention`, and one KEDA has not reported on yet gives `Wait`, which the controller decides again later. It checks, in order: the request's `policyRef` and namespace (`requestNamespaces`) and the `GitOps` mode; that the action type is in the catalog and its parameters are well formed; that the integration module that contributes the action, if any, is enabled in `RemediationSettings` `spec.modules.integrations` (`AdjustKedaMaxReplicas` needs `keda`); that the live target is the object `targetRef` names (UID, apiVersion, kind, namespace, name), that its kind is one the action may target and not one excluded by section 10 item 10, and that its namespace and labels are covered by `targets`; that the policy lists the action and the change is within `min`, `max`, `maxIncreasePercent` and `maxDecreasePercent` (compared by value, in arbitrary precision); and that the resolved repository is the policy's `gitOps.repositoryRef`, is configured in RemediationSettings with the resolved branch as its `targetBranch`, and that the file matches `allowedPaths`; last, that the live target is in a state the action may be applied to (for `AdjustKedaMaxReplicas`, a `ScaledObject` ready by `KedaReady`, whose identity in the live object is the target's). The path match is purely lexical, so it is never the only guard: a pattern such as `apps/**` also matches a file declaring a Role or a Secret, and the kind check refuses it. The decision does not trust the API server to have defaulted the policy: an unset field reads in its strictest sense (no request namespaces, approval `Required`, no increases or decreases). The effective approval is `false` only when the outcome is `Allowed`, the policy is `Optional` and the request asked for `approvalRequired: false`. One exception to the strict reading: a policy without `objectSelector` covers every object of the allowed kinds in its target namespaces. Labels are not a security boundary anyway, because anyone who can patch the object can set them.

The checks run in this order, and the first one that fails gives the reason. All of this is built (`internal/policy.Decide`).

```mermaid
flowchart TD
    start([Request, policy, settings, resolved source, live target])
    c0{All present?}
    c1["Request: policyRef, requestNamespaces, GitOps mode"]
    c2["Action: in the catalog, parameters well formed"]
    c2m["Module: the action's integration enabled in spec.modules"]
    c3["Target: the object targetRef names, kind allowed and not excluded, namespace and labels in targets"]
    c4["Policy action: listed, change within min, max, maxIncreasePercent, maxDecreasePercent"]
    c5["Source: the policy's repositoryRef, configured with this targetBranch, file in allowedPaths"]
    c6["Target state: for AdjustKedaMaxReplicas, the ScaledObject ready (KedaReady)"]
    ok[Allowed: WithinApprovedBounds]
    ap{"Policy Optional and the request asked for approvalRequired: false?"}
    start --> c0
    c0 -- yes --> c1
    c1 -- passes --> c2
    c2 -- passes --> c2m
    c2m -- passes --> c3
    c3 -- passes --> c4
    c4 -- passes --> c5
    c5 -- passes --> c6
    c6 -- passes --> ok
    ok --> ap
    ap -- yes --> auto[Effective approval: not required]
    ap -- no --> rev[Effective approval: required]
    c0 -- no --> ni[NeedsIntervention: IncompleteInput]
    c1 -- fails --> d1[Denied: PolicyMismatch, RequestNamespaceNotAllowed, ModeNotSupported]
    c2 -- fails --> d2[Denied: ActionUnknown, InvalidParameters]
    c3 -- fails --> d3[Denied: TargetMismatch, KindExcluded, KindNotAllowed, TargetNamespaceNotAllowed, TargetLabelsNotMatched, PolicyInvalid]
    c4 -- fails --> d4[Denied: ActionNotAllowed, BelowMinimum, AboveMaximum, IncreaseTooLarge, DecreaseNotAllowed, DecreaseTooLarge, PolicyInvalid]
    c5 -- fails --> d5[Denied: RepositoryNotAllowed, RepositoryNotConfigured, BranchNotAllowed, PathNotAllowed]
    c2m -- fails --> d2m[Denied: ModuleNotEnabled]
    c6 -- not ready --> n6[NeedsIntervention: ScaledObjectNotReady]
    c6 -- not reported yet --> w6[Wait: ScaledObjectReadinessUnknown]
```

## 5.4. RemediationSettings

One cluster-scoped object named `default`. It holds the kill switch (`paused`, checked right before every write), global limits (`maxConcurrentRemediations`, checked by the executor right before an allowed decision is written (section 10.7)), the analysis bounds and model deployments (`analysis`: its `maxAnalysesPerDay` is read today by the analysis trigger, 3.2, the rest is planned) and the Git repositories Shadewright may write to, each with its provider settings: for GitHub, the App client ID, installation ID and the signer (a Key Vault key, ADR-0006). Each repository also names the only branch pull requests may target (`targetBranch`, default `main`); the branch Flux tracks must equal it. URLs carry no credentials, query or fragment and no `.` or `..` path segment, and a GitHub repository must be `https://github.com/<owner>/<repo>`, so installation tokens never go to another host. A GitHub repository name may start with `.` (as in `.github`) but may not be `.` or `..` or end in `.git`. The schema holds a GitHub entry to the adapter's own rules, so a bad entry is refused when it is applied, not when a request first uses it: an owner of 1 to 39 characters starting with a letter or digit and a repository name of 1 to 100 characters, an App client ID of letters, digits, `.`, `_` and `-`, a target branch that starts with a letter or digit, has at most 200 characters and does not end with `.`, and a key ID that follows the Key Vault naming rules (a vault name of 3 to 24 characters with no consecutive hyphens, a key name of 1 to 127). The adapter checks the same rules again before it is built. The schema is checked only when an object is written, so an entry stored before a rule existed stays in the object after an upgrade. With CRD validation ratcheting (on by default since Kubernetes 1.30) such an entry, while it is left unchanged, does not block later writes to the object; without ratcheting the API server refuses every write to the object, setting `paused` (the kill switch) included, until the entry is fixed. In both cases the remedy is to fix or remove the entry, and until then a request that uses it ends `NeedsIntervention` (`RepositoryUnusable`, section 6). Policies and requests refer to repositories by name only. `analysis.maxAnalysesPerDay` is counted against in the analyzer's own ConfigMap in the operator namespace, not in this object, whose spec the analyzer only reads (3.2).

The kill switch is one gate (`writeAllowed`, `internal/controller/killswitch.go`). Every Git provider the executor opens is wrapped so that each write (creating the branch, the commit, opening a pull request, closing one, enabling auto-merge) first reads `default` from the API server, never from the cache, and calls the host only when the object exists and `paused` is not true. The wrapper also hands the gate to the adapter as a write checkpoint carried by the context (`providers.WithWriteCheckpoint`, a plain function, so the adapter imports nothing of Kubernetes). Every adapter must call it (`providers.CheckWrite`) right before each write request it sends and each retry: each of the several writes of one method (the GitHub commit is, today, a blob, a tree, a commit and the GraphQL branch update), after any slow work such as a token refresh, so the check is the last thing before the request is handed to the HTTP client. The GitHub adapter and the fake used in tests do; reads are not checked. The adapter gets only the class of a refusal (paused, or settings that cannot be read), never the API server's error text. A pause set at any moment, also after the freshness check passed, between the reads of one step or between two requests of one method, therefore stops the next write request in the reconcile it is set in. What it does not stop: a write request already handed to the HTTP client may complete, and there is no lock with the API server, so a pause set after the check's read does not stop that one request. No further write request or retry goes out once the pause is seen. A method stopped half way leaves at most unreferenced Git objects or a branch without its pull request; the steps of 8.2 are idempotent (the branch is found at its commit, the file on the branch is read before a commit, the open pull request of the branch is returned), so the request resumes without a duplicate (invariant 5). The gate fails closed: settings that cannot be read, or that are gone, block the write, which is retried with backoff. A request held back keeps its phase and the rest of its status (the deadline, a recorded pull request, the mark of a stale plan's cleanup), gets the condition `WritesPaused` (`True`, reason `WritesPaused`) and a Normal Event `WritesPaused`, and checks again every minute. Time spent paused still counts against a started deadline: a request whose deadline passes while paused expires once writes are allowed again (`expire` closes what it opened first, which is a write too). The first recheck that finds writes allowed again removes the condition, with an Event `WritesResumed`, whatever the rest of that check finds (a failure or a refusal of the host included), and the request goes on from where it stopped (a request that ends instead loses the condition without one). The Events are core Events about the request in the operator namespace, written with the grant on Events the executor's leader election Role already holds. The freshness check, the look at a recorded pull request and the close also read the switch first, so a paused executor does not read Git for a write it may not make; the gate is what holds whatever the path. Issues and comments (ADR-0014) are not built; they are writes and must go through the same gate. What a pause does not do (revoke a published pull request, revert a merge, stop analysis) is in the [emergency runbook](../emergency.md).

`modules` enables the platform and integration modules of 2.4 and 2.5 in two separate lists, `platforms` and `integrations`, since platform and integration names are separate namespaces. Each list is a set (a name given twice is refused) of at most 8 and 32 names; `internal/modules` refuses to build a registry with more compiled-in modules than that (2.4), so every module a binary has can be enabled. A name follows the rule `internal/modules` checks when it builds its registry: a DNS label that starts with a letter, at most 63 characters; a test keeps the schema and the registry in agreement. Names are a pattern, not a list of known modules, so a module added in a later release or shipped as a pack ([ADR-0019](../adr/0019-extension-packaging.md)) needs no schema change; the price is that a misspelt name is accepted, matches no compiled-in module and contributes nothing. The generic platform is always on and may not be listed. Nothing is enabled by default: with `modules` left out the analyzer uses the generic platform and no integration, whatever it detects, so a cluster admin enables each module on purpose. A module contributes hooks only when it is enabled here and detected ([ADR-0018](../adr/0018-three-axes-of-pluggability.md)): detection only reports and narrows what is enabled, it never enables a module, and there is no override that makes a module active on a cluster where it is not detected. More than one platform may be enabled, so one settings file can serve clusters on different platforms; only the detected one is active. Which modules are active is always worked out again from `spec.modules` and live detection, never read from status: status is a report, never an input, so a stale or forged status cannot enable anything.

`analysis` bounds every model-backed analysis and names the models it may call (11.1, 12.3). Its limits have safe defaults and hard upper bounds in the schema, and an explicit `0` is refused where it is out of bounds. Read today: `maxAnalysesPerDay` (analyses per UTC day across all Incidents, default 100, 0 to 10000, where 0 disables the model). The analysis trigger reads it (3.2): 0 marks every Incident not due. The daily count is enforced once the analysis reserves units of the budget ([#16](https://github.com/pkalin/shadewright/issues/16), [#76](https://github.com/pkalin/shadewright/issues/76)); until then nothing adds to it. Its default applies also when `analysis` is left out, since `analysis` itself defaults to an empty object. v0.1.0 had this field at the top level as `spec.maxAnalysesPerDay`. That field stays in the schema, deprecated and without a default, under a CEL rule on the field that refuses any write that sets or changes it; validation ratcheting lets an object stored by v0.1.0 keep its value through updates of other fields, and removing it always passes. While it is set the trigger goes by the lower of the two, so a 0 set by the v0.1.0 runbook keeps analysis off after an upgrade instead of falling back to the new field's default of 100. Planned: `maxContextBytes` (the redacted context of one analysis, default 32768, 4096 to 262144), `maxToolSteps` (read tool calls of one escalation, default 4, 0 to 16), `maxOutputTokens` (per model call, sent as `MaxOutputTokens`, default 2048, 256 to 16384) and `maxConcurrentAnalyses` (default 1, 1 to 8). An analysis makes at most 3 + `maxToolSteps` model calls: the triage call, one retry after invalid output, and `maxToolSteps` + 1 escalated turns; an invalid escalated result gets no retry and is handed off. `models` has a `triage` deployment for the one structured call and an optional `escalation` deployment for the stronger model ([ADR-0023](../adr/0023-how-the-analyzer-calls-the-model.md)); an escalation model without a triage model is refused, and so are two that name the same endpoint (ignoring a trailing `/`) and deployment (ignoring case), so the escalation cap cannot become a second budget for the triage model. Each names an `endpoint` (HTTPS only, at most 256 characters, a host name of lower-case DNS labels, a port from 1 to 65535 if any, no IP address, user info, query, fragment or dot segment), a `deployment` name and an `apiVersion` (`v1` or a valid-looking date, optionally `-preview`), and must set a daily cap `maxTokensPerDay` (no default, so the admin chooses it per deployment; 0 to 1,000,000,000, and 0 stops calls to it). There is no field for a key or any other credential: the analyzer authenticates with its workload identity (11.1). The schema is vendor-neutral ([ADR-0013](../adr/0013-adapters-for-every-external-system.md)), but the endpoint decides where the analyzer's Entra ID token and the redacted context go, so write access to this object is security-relevant, the `patch` right the [emergency runbook](../emergency.md) hands out for the kill switch included. Planned ([#76](https://github.com/pkalin/shadewright/issues/76)): the analyzer refuses a host outside an allow-list of suffixes held by the operator (a flag, not these settings), refuses private, loopback and link-local addresses when it dials, and requests a token for the Cognitive Services audience only.

What the analysis does when a model is missing (planned, #76): with no `triage` model it makes no model call, uses no unit of `analysis.maxAnalysesPerDay`, and hands an incident no runbook matched off to a person; with no `escalation` model an escalation trigger (ADR-0023) hands off; a deployment whose `maxTokensPerDay` is 0, or whose cap for the day is spent, is not called and the incident is handed off. Planned: no component reads the rest of `analysis` yet; the analysis loop ([#76](https://github.com/pkalin/shadewright/issues/76)) will enforce the limits, and the token caps need the spend counter of [#208](https://github.com/pkalin/shadewright/issues/208). Until then the only analysis limit in force is `analysis.maxAnalysesPerDay` (3.2).

The status reports what was detected and what is in use. `status.platform` has `detected`, the platform that claims every node with certainty (left out when none does), and `active`, the platform in use: `detected` when it is enabled in `spec.modules.platforms`, `generic` otherwise. So a cluster on AKS without `aks` enabled shows `detected: aks, active: generic`, and the admin sees why AKS diagnosis is missing. `status.integrations` lists every integration that is enabled or detected, with `detected`, the group version that matched (`version`, such as `keda.sh/v1alpha1`) and `active` (enabled and detected). A detected add-on whose module is not enabled is listed too, so the cluster admin can see what to enable; cloudflared, which has no API group and declares core `v1` (2.5), is listed as detected with `version: v1` on every cluster, so for it `detected: true` means nothing about whether cloudflared runs (only `active`, which follows `spec.modules`, does); an enabled name no compiled-in module has is listed as not detected. That makes at most 64 entries: 32 enabled names and at most 32 more detected ones, since only a compiled-in module can be detected. The schema refuses an integration that is active but not detected, a version without detection or detection without a version, `generic` given as detected, and an active platform that is neither `generic` nor the detected one. It does not tie status to `spec`: a status written before a spec edit would then block the edit. The analyzer writes these fields (2.4), with `observedGeneration` and the condition `UnknownModules`: `True` (reason `UnknownModulesEnabled`) when `spec.modules` names a module the binary does not have, with the names in the message, `False` (`AllModulesKnown`) otherwise; and the condition `DetectionFailed`: `True` (reason `DetectionFailed`, with a fixed message) when the last run could not list the nodes or read the discovery API, so the report is from the last run that succeeded or, when none has, shows only `generic` active and no integrations; `False` (`Detected`) after a run that succeeded; and the condition `NoWatchedNamespaces`, set from the count of `--watch-namespaces`, not from detection: `True` (reason `NoNamespacesWatched`) when the analyzer watches no namespace, so it reads no events or pods and records no Incident, `False` (`NamespacesWatched`, with the count) otherwise. Like the others it appears only once a `RemediationSettings` object exists, which the chart does not create by default (`settings.create: false`). Conditions of other types are kept as they are. No component reads any of these fields, or any other field of the status, to decide anything, and decision state of another component (signer health, a circuit breaker) must not be kept here. It is the analyzer's only write outside the operator namespace, an `update` of the status subresource ([ADR-0024](../adr/0024-analyzer-writes-detection-to-settings-status.md), invariant 6); the executor neither writes nor reads it.
