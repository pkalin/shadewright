# ADR-0044: The Git origin of an OCI source is declared, and the artifact is only a cross-check

- Status: Proposed
- Date: 2026-11-19

## Context

Issue [#935](https://github.com/pkalin/shadewright/issues/935). The source mapping of [ADR-0012](0012-object-to-source-mapping.md) follows a Flux Kustomization to its `GitRepository` and refuses every other source: the request ends `NeedsIntervention` with `UnsupportedSource`. Many clusters deliver through OCI artifacts instead. CI builds an artifact from a Git repository (`flux push artifact`) and pushes it to a registry, and Flux pulls it through an `OCIRepository`. The desired state still lives in Git, but an `OCIRepository` says where the artifact is, not which repository, branch and path it was built from. Those clusters get diagnoses and issues, never a pull request.

Today's code, so the change is read against it:

- A Kustomization whose `spec.sourceRef.kind` is not `GitRepository` stops the mapping with `UnsupportedSource` (`internal/gitops/sourcemap/fluxrules/flux.go:93-95`; the reason is defined at `fluxrules/result.go:87-88`).
- Flux objects are read as unstructured objects, and `go.mod` has no Flux module (`flux.go:43-44`). Only the fields the rules name are used.
- A `GitRepository` passes only when it sets none of `suspend`, `ignore`, `recurseSubmodules` or `sparseCheckout` (`flux.go:63`), and tracks one branch (`flux.go:258`); the Kustomization must have applied the revision the source last fetched (`flux.go:307`, `internal/fluxstatus/fluxstatus.go:227`).
- The source URL must equal exactly one repository of the settings, that repository must be the policy's `gitOps.repositoryRef`, and the branch must equal its `targetBranch` (`sourcemap.go:331-355`). The URL is never put in a message (`sourcemap.go:330`).
- The base commit of the pull request is the head of that branch, read from the Git provider at mapping time (`sourcemap.go:233`, `sourcemap.go:308`). The freshness check runs the mapping again with the settings' repositories and compares repository, branch, path and build directory, not the base commit (`internal/policy/freshness/freshness.go:651-666`, `freshness.go:683`).
- The executor's cluster-wide role has `get` on `kustomizations` and `gitrepositories` and nothing on `ocirepositories` (`config/rbac/executor/role.yaml:23` and `:36`, from the markers at `internal/controller/cache.go:57-58`; the chart's `dist/chart/templates/rbac/executor-role.clusterrole.yaml:24` and `:37`).
- After the merge, the delivery check reads a commit out of the Kustomization's `lastAppliedRevision` only when its source is a `GitRepository` (`internal/providers/delivery/flux/flux.go:154`, `flux.go:236-237`). The unit is `Applied` when that commit equals the merge commit, or descends from it by the plan repository's ancestry answer (`internal/providers/delivery.go:146`, `delivery.go:159-185`). A unit that never shows such a commit ends the check `DeliveryNotObserved` after the delivery window, one hour (`internal/controller/rollout.go:70`, `rollout.go:510`).

The last point matters most. For an OCI source the Kustomization applies `<tag>@sha256:<digest>` of the artifact (upstream: the revision of an `OCIRepository` artifact is the tag and the digest of the OCI artifact, [source-controller spec, "Artifact"](https://github.com/fluxcd/source-controller/blob/main/docs/spec/v1/ocirepositories.md)). It names no Git commit. The cluster applies an artifact CI built after the merge, never the merge commit itself.

This ADR widens a read (the executor reads `OCIRepository` objects) and the places a pull request can come from (a workload delivered through a declared artifact). [ADR-0021](0021-amending-accepted-adrs.md) says an amendment never widens on its own authority, so it is decided here and ADR-0012 gets only a pointer after acceptance. [ADR-0043](0043-the-executor-follows-its-own-pull-request-to-any-base.md) is the model: say what is widened, say what never happens, state the limits.

### What the upstream Flux API gives

Read from the current upstream documentation and source on 2026-11-19, not from memory (the repository vendors no Flux types, so there is no pinned version to read; the code reads `source.toolkit.fluxcd.io/v1`, `internal/fluxstatus/fluxstatus.go`):

- `OCIRepository` status carries `status.artifact` (type `meta.Artifact` of `fluxcd/pkg/apis/meta`): `revision`, `digest`, `url`, `size`, `lastUpdateTime` and `metadata`, a map of strings "such as OCI annotations".
- When the artifact was created with `flux push artifact`, `metadata` holds `org.opencontainers.image.created`, `org.opencontainers.image.source` ("the URL of the Git repository containing the source files") and `org.opencontainers.image.revision` ("the Git branch and commit SHA1 of the source files"). The Flux CLI requires `--source` and `--revision` and documents `--revision="$(git branch --show-current)@sha1:$(git rev-parse HEAD)"` (`cmd/flux/push_artifact.go` in fluxcd/flux2). The CLI strips credentials from the source it records. An artifact pushed by another tool, or with another `--revision`, can carry any value or none: the status example in the spec even shows `6.1.8/b3b00fe3...` as the revision annotation.
- The status of an `OCIRepository` has no field that says which Git repository, branch or path the content came from, other than those annotations. Without them there is nothing to read in the cluster. Registry access would be needed for anything else, and the executor has none.
- `spec.ref` (`tag`, `semver`, `digest`), `spec.ignore`, `spec.layerSelector`, `spec.suspend`, `spec.verify` (cosign or notation) and `spec.secretRef` or `spec.serviceAccountName` exist in the spec. Flux honours `.sourceignore` and a default exclusion list when it archives the artifact.

## Decision

[![The executor reads three things: the Kustomization and its OCIRepository status in the cluster, the declaration in RemediationSettings, and the Git repository through the Git App. The declaration says which repository and path the artifact is built from; the artifact's source and revision annotations are only compared with it. No declaration, a mismatch and an artifact without annotations each end NeedsIntervention with their own reason and no Git write. The executor never reads the registry, the artifact's content or any secret.](../assets/diagrams/oci-source-origin.svg)](../assets/diagrams/oci-source-origin.svg "Open full size")

This is option 3 of the issue, with one change: the declaration has no `branch` field.

### 1. The declaration

`RemediationSettings` gets a new list, `spec.ociSources`, next to `spec.repositories`:

```yaml
spec:
  repositories:
  - name: fleet
    url: https://github.com/example/fleet
    targetBranch: main
    provider: GitHub
    github: { ... }
  ociSources:
  - name: apps                  # Name, the list key
    ociRepository:
      namespace: flux-system    # DNS label
      name: apps                # DNS subdomain
    repositoryRef: fleet        # Name of an entry in spec.repositories
    path: clusters/prod         # GitDirPath, default "."
```

- `ociRepository` is the Flux object, by namespace and name. A pair appears at most once (a CEL rule, as the URL rule of `repositories`).
- `repositoryRef` must name an entry of `spec.repositories` (a CEL rule on the spec). The mapping checks it again and never relies on admission alone.
- `path` is the directory of the repository that the artifact's root corresponds to, `.` when CI packs the repository root (`flux push artifact --path=./clusters/prod` is `path: clusters/prod`). The build directory of a request is this path joined with the Kustomization's `spec.path`.
- The branch is not declared. It is the `targetBranch` of the entry `repositoryRef` names, as for a `GitRepository` (ADR-0012 step 2: the branch Flux tracks must equal it). A second field could only repeat that value or disagree with it, and a disagreement would be a state to validate and to explain. The issue lists the branch among the fields; this is a deliberate departure, open for the maintainer in the last section.
- At most 16 entries, as `repositories`.
- Fields of the `RemediationSettings` status are never read for any decision, as before.

### 2. What the mapping does for a Kustomization whose `sourceRef.kind` is `OCIRepository`

In this order, each stop being `NeedsIntervention` unless said otherwise. A read that fails is returned for retry and is never one of these stops (the rule of `Resolver.get`, `sourcemap.go:315-327`); the one exception is a refused read (point 2).

1. No `ociSources` entry names that namespace and name: stop, reason `OCIOriginNotDeclared`. The message names the object and the field to declare.
2. Read the `OCIRepository` (`get`, by the Kustomization's `spec.sourceRef`, the namespace defaulting to the Kustomization's as for a `GitRepository`, `flux.go:97-99`). Not found: `SourceNotFound`. A 403 on the read (the grant of section 3 is off): `OCISourceNotReadable`, since a retry could never succeed and would run without end and without a signal.
3. The object must not set `spec.suspend`, `spec.ignore` or `spec.layerSelector`: reason `SourceOptions`, as for the `GitRepository` options at `flux.go:63`. Each can make the artifact differ from the files in Git. A `.sourceignore` file stops the mapping in the Git tree as today. `spec.verify` and `spec.ref` are not constrained: the revision comes from the status.
4. It must be Ready for its current generation (`SourceNotReady`), and the Kustomization's `status.lastAppliedRevision` must equal `status.artifact.revision` (`RevisionMismatch`; the equality is of two strings from the cluster, as `AppliedFetched` at `fluxstatus.go:227`, with no tag parsing, since the OCI revision is not a Git revision).
5. The artifact must carry `org.opencontainers.image.source` and `org.opencontainers.image.revision` in `status.artifact.metadata`, and the revision must parse as `<branch>@sha1:<40 hex>` or `<branch>@sha256:<64 hex>` (`fluxstatus.ParseRevision`, `fluxstatus.go:187`). Otherwise: `OCIOriginUnrecorded`.
6. The cross-check against the declaration:
   - The annotated source must equal the URL of the declared repository exactly, or equal it with `.git` appended (the form `git config --get remote.origin.url` gives). No other rewriting: not `ssh://`, not `git@host:`, not a changed case.
   - The annotated branch must equal the repository's `targetBranch`.
   - The annotated commit must be the head of that branch or one of its ancestors (`AncestryChecker.IsAncestor`, `providers/delivery.go:191`, implemented for GitHub at `internal/providers/github/ancestry.go:39`, a read through the Git App the mapping already opens, `sourcemap.go:222`). The host not knowing the commit is a mismatch; a failed call is a retry.
   - Any difference: stop, reason `OCIOriginMismatch`, no Git write. The message names which of the three differed and shows no URL.
7. Then the rules of ADR-0012 go on as for a Git source: exactly one settings repository (the declared one), the policy's `repositoryRef`, the `targetBranch`, the declaration found by the scan in `path` joined with `spec.path`, the live value equal to the value in Git, `allowedPaths`. The base commit is the head of the branch, as today.

Reason names are the proposal; the builder may change a spelling in the code review, not the cases.

### 3. What is read, written and granted, and what never is

| | Read | Written | Granted | Never |
| --- | --- | --- | --- | --- |
| Executor, cluster | `get` on `ocirepositories` of the group `source.toolkit.fluxcd.io`: spec fields above, `status.artifact.revision` and `status.artifact.metadata`, Ready | nothing new | one new `get` rule in its own ClusterRole, opt-in (below) | `list`, `watch`, any write on the object; Secrets, ServiceAccounts, or the object's `secretRef`, `serviceAccountName` and `provider` followed; the artifact's tarball or `status.artifact.url` (an HTTP call to source-controller); the registry, with or without credentials |
| Executor, Git | the head of the declared branch, the ancestry of the annotated commit, files in the declared path, through the GitHub App of the declared repository | the pull request, in a repository of the settings, as today | no new App permission | any repository not in `spec.repositories`; a path or branch taken from an annotation |
| Settings | the declaration is written by whoever may write `RemediationSettings`, a cluster administrator | | | read from the status |
| Analyzer, verifier | | | nothing | `ocirepositories` |

The grant is opt-in. The chart gets the value `executor.ociSources.enabled`, default `false`, which renders a separate ClusterRole and ClusterRoleBinding with the one rule; kustomize gets the same as a separate component, as the analyzer's module grants (`dist/chart/values.yaml:246-250`, `config/rbac/analyzer/modules/flux/kustomization.yaml:1`). The default install stays as narrow as today. With the value off and an entry declared, the mapping ends with `OCISourceNotReadable`, and the lookup page tells the operator to turn it on.

### 4. How the invariants hold

1. **Model output is never authorization.** The declaration is settings written by an administrator, and the annotations are compared, not obeyed. Nothing from the artifact reaches the policy or the model. The plan digest still covers repository, branch, path and build directory (`freshness.go:683` compares them at the merge), so a declaration edited between the plan and the merge makes the plan stale (`SourceChanged`) through the mapping run again with the new settings (`freshness.go:661-666`).
2. **Closed catalog.** No new action, parameter or patcher. The action still changes one declared field of one manifest in Git.
3. **Git only.** The only write is the pull request the executor opens today, in a repository of `spec.repositories`, on a branch derived from the request, to the entry's `targetBranch`, in a file `allowedPaths` allows. The pipeline that builds the artifact (the workflow, the registry, the tags) is never touched, since the file must be a manifest the action's patcher accepts and a path the policy allows.
4. **Cluster input is untrusted.** The two annotations are input from a registry writer, so they are only compared for equality with values the administrator declared, and a difference stops the request. They are never used as a repository, a ref, a path or a URL, never put in a message in full (the message says which comparison failed), and never shown to the model. Branch and commit are parsed with the strict parser that also parses Git revisions (`fluxstatus.go:187`).
5. **Idempotent.** The mapping adds no state. Branch names, markers and the pull request lookup do not depend on the source kind. A restart maps again and gets the same answer or, after a new artifact, a fresh one.
6. **Separate identities.** One new `get` for the executor only, cluster-wide like its `gitrepositories` rule (opt-in, off by default). The analyzer and the verifier get nothing. No registry credential, no Secret read, no new App permission. AGENTS.md invariant 6 is amended in the pull request that builds this, to list the grant and its chart value.

### 5. The artifact revision, the base commit, the merge receipt and the verification

Three commits are in play, and they are not the same thing:

- **The base commit** of the pull request is the head of the declared branch when the request is mapped. It is not the artifact's commit. The artifact may be older than the head (CI is slower than a push), so the mapping checks that the artifact's commit is the head or an ancestor of it, and relies on the existing value check (live value equals the value in Git at the head) for the field it will change. A change in a file between the artifact's commit and the head that touches that field makes the values differ and stops with `ValueMismatch`; one that does not touch it is harmless.
- **The merge commit** is recorded in the merge receipt exactly as today (`api/v1alpha1/remediationrequest_types.go:554-559`, `:596-604`, ADR-0030 section 1). Nothing about the receipt depends on the source kind.
- **The applied commit** for an OCI unit is the commit in the revision annotation of the artifact the Kustomization applied. The delivery adapter will compute it only when `lastAppliedRevision` equals the current `status.artifact.revision` of the `OCIRepository` the Kustomization names; it fills `AppliedRef` and `AppliedCommit` from the annotation, as it does from `lastAppliedRevision` for a `GitRepository` (`flux.go:236-237`). Then the existing comparison runs unchanged: the unit is `Applied` when that commit is the merge commit or descends from it (`delivery.go:146`, `:159-185`), and the table of ADR-0030 section 2 applies row by row. The same goes for `AttemptedCommit` on a failure.
- When `lastAppliedRevision` is not the current artifact, there is no commit to read, and the status is `Pending`: the executor never reads an older artifact's metadata (it is gone from the status) and never reads the registry.
- The artifact's source annotation must also name the plan's repository, compared by the executor where it knows the plan; an artifact of another repository is `Pending`, never `Applied`. The ancestry question is asked of the plan repository, so a commit it does not know cannot count either.
- The window is the same hour (`rollout.go:70`). CI must build and Flux must pull within it, or the request ends `DeliveryNotObserved` (limit (d)).

What clears a request is not the annotation. After delivery the verification reads the live field, and a value that is not the desired one ends `Inconclusive` with `ValueChanged` (ADR-0030 section 2, table row "A later commit overwrote the field"). An annotation that lies can make the delivery look applied, never make a wrong value clear.

### 6. Behaviour at the edges

| Case | End state | Reason | Why |
| --- | --- | --- | --- |
| No declaration for the `OCIRepository` | `NeedsIntervention`, no Git write | `OCIOriginNotDeclared` | the repository and path cannot be known from the cluster; the message says what to declare |
| Declared, annotation differs (source, branch or commit not in the branch) | `NeedsIntervention`, no Git write | `OCIOriginMismatch` | the declaration and the artifact disagree about where the files live; a person decides which is right |
| Declared, artifact has no source or revision annotation, or one that does not parse | `NeedsIntervention`, no Git write | `OCIOriginUnrecorded` | see below |
| Declared, the executor may not read `ocirepositories` | `NeedsIntervention`, no Git write | `OCISourceNotReadable` | permanent until the grant exists |
| Object missing, not Ready, suspended, with `ignore` or `layerSelector`, applied revision differs | as for a `GitRepository` | `SourceNotFound`, `SourceNotReady`, `SourceOptions`, `RevisionMismatch` | the same rules, same reasons, same waits for a reconciling Kustomization |
| Any read or ancestry call fails | retried, no stop | none | a failed read is never a verdict |
| Other source kinds (`Bucket`, `ExternalArtifact`, `HelmChart`) | `NeedsIntervention` | `UnsupportedSource` | unchanged |

**An artifact without annotations is refused, not accepted on the declaration alone.** With the declaration alone:

- the executor would have no evidence that the artifact has anything to do with the declared repository, so a wrong declaration (a mistyped path, a repository that CI no longer builds from) would open pull requests that deliver nothing;
- it could not tell a delivered merge from an undelivered one, since the artifact's revision names no commit: every request would wait the full hour and end `DeliveryNotObserved`, which reads as a failure of Shadewright;
- the declaration would be the only guard, and the issue asks for the artifact as a second one.

A pipeline that does not record the annotations records them by adding `--source` and `--revision` to `flux push artifact`, which the CLI requires anyway.

## Consequences

Clusters that deliver through OCI artifacts can get pull requests. The cost is an explicit declaration per `OCIRepository`, an artifact pipeline that records its source, and one more opt-in grant.

Known limits:

- (a) The executor does not verify signatures and does not require `spec.verify`. The annotations are metadata of the manifest; anyone who can write to the registry can set them to match the declaration. They are a cross-check against mistakes and drift, not authentication. What bounds a forged artifact is the value check against Git, the render check and the review of the pull request, not the annotations. Whether to require a verified artifact (a setting that refuses an `OCIRepository` without `spec.verify`) is left open, below.
- (b) CI may change the files between Git and the artifact (a substitution step, a generated overlay). The mapping compares only the one field it will change. Another difference is invisible to it.
- (c) For an OCI unit, "applied" rests on an annotation, which is cluster-side input, where for Git the commit comes from Flux. A forged annotation that names the merge commit and the plan's repository would make the delivery look applied. The live-value check still decides the clearing (section 5).
- (d) The artifact is built after the merge, so the one-hour window includes the CI run and Flux's poll interval. A slower pipeline ends `DeliveryNotObserved` with the change possibly delivered later.
- (e) The delivery is read only while the applied artifact is the current one. Two artifacts in quick succession can leave no moment where the merge commit's artifact is both applied and current. The later one descends from the merge commit and counts. A Kustomization that applied an artifact that is then replaced and never applies another one shows `Pending`.
- (f) The source annotation is compared as a string. A pipeline that records an `ssh://` or `git@` form never matches until it records the HTTPS form.
- (g) A wrong `path` that still finds one declaration with the right live value opens a pull request in the wrong directory. The render check of the pull request step runs on the build directory the path implies, so it fails when that directory does not build to the same objects; it does not prove the path is the right one.
- (h) `kubectl shadewright doctor` and the analyzer's `get_delivery_status` compare only with a `GitRepository` (`fluxrules/flux.go:190-194`). They keep reporting an OCI source as not compared until a follow-up teaches them, and the analyzer's Flux grant is not widened here.
- (i) The default exclusions Flux applies to a Git artifact and to an OCI artifact are listed separately upstream. The builder compares the list the mapping copies (`internal/gitops/repopath/generate.go:41`) with the OCI one before it reuses it; a difference is a bug in the build, not a decision.
- (j) A declared `OCIRepository` whose `spec.url` is later pointed at another artifact keeps its declaration. The annotations are still compared on every mapping, so another origin ends `OCIOriginMismatch`, but a pipeline that copies the annotations passes.

## Alternatives considered

- **Option 1, the declaration alone.** Explicit, and no trust in the artifact. But nothing ties the artifact to the declared repository, and the delivery check could not work: the revision of the artifact is no commit, so every request would end `DeliveryNotObserved` or need a second mechanism to tell a delivered merge. Refused for the reasons of section 6.
- **Option 2, the annotations alone.** The annotations do not say the path, so the path would have to be guessed. They are also set by whoever writes to the registry, so a registry writer would choose where a pull request goes, within the repositories of the settings. That is invariant 4 turned around: input from outside decides a destination.
- **A `GitRepository` kept next to the `OCIRepository`, used only for the mapping.** The mapper could then reuse every Git rule. But the Kustomization does not deliver from that `GitRepository`, so its `lastAppliedRevision` and Ready say nothing about it, and nothing would tie it to the artifact that is applied. The cluster would also fetch a repository only to give the executor a URL, with credentials for it, and the mapping would pass for a source that is not the one in use. It trades a declaration in the settings for an object in the cluster that proves less.
- **Declaring a branch.** See section 1: it can only repeat or contradict `targetBranch`.
- **Reading the artifact's content from source-controller.** It would let the mapping read the files that Flux applies. It needs a network path and a grant to source-controller's artifact server, widens the executor's reach much further, and gives content that is again from the cluster side. Not taken.

## What is to be built

1. API: `spec.ociSources` with its CEL rules, generated manifests and CRD docs; the settings reference page.
2. Mapping: `fluxrules` rules for an `OCIRepository` source, the new reasons, the declaration and the ancestry check in `sourcemap`, the freshness check with the new input; the reasons added to the metrics list (`internal/controller/metrics.go:155`) and to the explain table (`cmd/kubectl-shadewright/explain.go:255`).
3. Delivery: `AppliedRef` and `AppliedCommit` from the annotation of the current artifact, the plan repository check, `Pending` otherwise.
4. RBAC: the opt-in ClusterRole in the chart and as a kustomize component; AGENTS.md invariant 6, the threat model and the design document updated with the grant.
5. Docs: the reasons page (`docs/reasons.md`), the how-it-works source mapping page, a how-to for declaring an OCI source and for `flux push artifact --source --revision`.
6. Tests that fail without the change: one per reason, the 403 case, the retry cases, a forged annotation that names another repository, and a lab run with an artifact pipeline.

## Amendments

None.
