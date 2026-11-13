---
title: Check an install with kubectl
---

# Check an install with kubectl

After an install or an upgrade, `kubectl shadewright doctor` says in one command what is missing or wrong, before an Incident or a request shows it. It is part of the same kubectl plugin as [`status` and `explain`](check-status.md) (install it as described there), runs on your machine with your kubeconfig and your permissions, and only reads: no write, no call to GitHub, Azure or a model.

```bash
kubectl shadewright doctor -n shadewright-system
kubectl shadewright doctor -n shadewright-system -o json
```

The flags are those of `status` without `-A`: `-n` (the operator namespace, default the kubeconfig context's), `-o text|json`, `--kubeconfig`, `--context` and `--request-timeout`.

Each check is `pass`, `warn` or `fail`, with one line of why. A warn or a fail is followed by a link to its section below.

```text
Namespace shadewright-system
PASS  CRD incidents.remediation.shadewright.io: established, serves v1alpha1
...
PASS  RemediationSettings default: the spec parses, at most 20 analyses per day
      Reported by the analyzer (shown only): platform aks detected, aks active; integration keda active
WARN  Kill switch: ON: writes are paused (spec.paused)
      See https://shadewright.io/how-to/check-install/#kill-switch
PASS  Deployment analyzer shadewright-analyzer: available, 1 of 1 replicas available
PASS  Deployment executor shadewright-executor: available, 1 of 1 replicas available
PASS  Repository fleet: complete: provider GitHub, target branch main
PASS  Flux GitRepository flux-system/fleet (repository fleet): Ready
FAIL  Flux Kustomization flux-system/apps: failing, Ready BuildFailed: <the controller's message>
      See https://shadewright.io/how-to/check-install/#flux
WARN  Delivery sources: 1 watched namespaces, 4 workloads: 2 from Kustomizations, 1 from a HelmRelease, 1 without Flux labels; 2 sources: 0 ok, 1 refused, 1 not ready, 0 unknown
      See https://shadewright.io/how-to/check-install/#delivery-sources

Delivery sources of the watched namespaces
  Namespace demo: 4 workloads: 2 from Kustomizations, 1 from a HelmRelease (HelmManaged), 1 without Flux labels (NoFluxLabels), 0 with invalid Flux labels (InvalidFluxLabels)
  NOT-READY  Kustomization flux-system/apps (1 workloads), GitRepository flux-system/fleet github.com/acme/fleet branch main, settings: repository fleet; KustomizationNotReady: the Kustomization is not Ready for its current generation
  REFUSED    Kustomization flux-system/podinfo (1 workloads), GitRepository flux-system/podinfo github.com/acme/podinfo branch main, settings: not named; RepositoryNotConfigured: no single repository of the RemediationSettings has the URL of its GitRepository exactly

9 passed, 2 warnings, 1 failed
```

The JSON output holds `namespace`, `failed` and `checks`, each with `name`, `level`, `detail`, `link` and, on the settings check, `reported`; and `delivery`, the report of [delivery sources](#delivery-sources) with `watchedNamespaces`, `unchecked`, `namespaces` (per namespace `workloads`, `kustomization`, `helmRelease`, `noFluxLabels`, `invalidFluxLabels` and `unchecked`) and `sources` (per Kustomization `kustomization`, `workloads`, `gitRepository`, `url`, `branch`, `settings`, `repository`, `state`, `reason` and `detail`), and `sourcesNotListed`, the number of sources left out beyond the first 256 (absent when none were). `settings` is `named` (exactly one settings repository has the URL), `not-named` (none has it), `ambiguous` (more than one has it) or `unknown` (the settings or the GitRepository were not read).

**Exit status.** The command exits 1 when any check fails, and 0 when every check passes or warns. A warn alone never fails it, so a script or a pipeline step can run it after an upgrade. An error before any check runs (a kubeconfig that does not load, a bad flag) exits 1 too.

**Reads you may not make.** A read your permissions do not allow is a warn ("not readable with your permissions, so it was not checked"), never a fail: it says nothing about the install. Any other read error is a fail, since the check could not run, except in the [delivery sources](#delivery-sources) report, where a list that could not be read is a warn and is printed as `NOT CHECKED`. The reads are listed under [permissions](#permissions).

An install in observe-only mode, as [install](../install.md) leaves it, warns four times and fails nothing: the kill switch is on, the daily analysis limit is 0, no repository is configured, and the analyzer watches no namespace (the chart's default), so there are no [delivery sources](#delivery-sources) to report.

## CRDs

One check per Shadewright CRD: `incidents`, `remediationrequests`, `remediationsettings` and `remediationpolicies` in `remediation.shadewright.io`.

| Result | Meaning | What to do |
| --- | --- | --- |
| fail: does not exist | The CRD is not installed | Apply the CRDs of the release (`kubectl apply -k`, or `helm upgrade`, see [upgrade](../upgrade-uninstall.md#upgrade)) |
| fail: does not serve v1alpha1 | The CRD in the cluster serves another version than the one the plugin reads | Install the CRDs and the plugin of the same release |
| fail: is not Established | The API server does not serve the CRD yet | Wait with `kubectl wait --for=condition=Established crd/<name>`, then run doctor again |
| warn: is version X and this plugin is Y | The chart's label `app.kubernetes.io/version` on the CRD names another release than the plugin | Upgrade the plugin or the install so both are one release. Not compared for a `dev` build or CRDs installed with kustomize (no label) |

## Settings

The `RemediationSettings` object `default`. Its spec is decoded into the plugin's own type.

| Result | Meaning | What to do |
| --- | --- | --- |
| fail: does not exist | The analyzer refuses every analysis (`SettingsNotFound`) and the executor every write | Create it, as in [install](../install.md) |
| fail: the spec does not parse | A field has a type the plugin does not accept | Compare the object with the [API reference](../reference/api.md) of your release |
| fail: maxAnalysesPerDay is not set | The analyzer refuses every analysis (`SettingsIncomplete`) | Set `spec.analysis.maxAnalysesPerDay` |
| warn: fields this plugin does not know | The spec has fields the plugin's types do not have: the CRDs are likely newer than the plugin | Install the plugin of the CRDs' release |
| warn: the daily analysis limit is 0 | Analysis is disabled on purpose (observe-only) | Raise `spec.analysis.maxAnalysesPerDay` to enable analysis |

The line "Reported by the analyzer (shown only)" is the platform and integration detection the analyzer writes in the settings status ([ADR-0024](../adr/0024-analyzer-writes-detection-to-settings-status.md)). Doctor shows it and never uses it: no check reads any field of that status to decide its level, as no component of Shadewright does (invariant 6). Missing detection is in the status conditions: `kubectl get remediationsettings default -o jsonpath='{.status.conditions}'`.

## Kill switch

`spec.paused` of the settings. On is a warn, not a fail: writes are held back on purpose ([pause and resume](pause-and-resume.md)). Without a readable settings object the state is unknown, also a warn; without any settings object the executor blocks every write, which the settings check already reports as a fail.

## Deployments

The analyzer and executor Deployments of the namespace, found by the label `control-plane=analyzer` and `control-plane=executor` that both the chart and the kustomize install set.

| Result | Meaning | What to do |
| --- | --- | --- |
| fail: no Deployment labelled ... | None in that namespace | Pass the operator namespace with `-n` |
| fail: not available | Its `Available` condition is not `True`, or no replica is available | `kubectl -n <ns> describe deploy/<name>` and the pod events: image pull, workload identity, resources |
| fail: rollout stuck | Its `Progressing` condition is `False` with `ProgressDeadlineExceeded` for the current generation: the old replicas still serve, but the current spec will not roll out by itself | `kubectl -n <ns> describe deploy/<name>` and the events of the new pods; fix the cause, then roll out again |
| warn: rollout pending | Available, but the Deployment controller has not observed the current spec yet (`status.observedGeneration` is below `metadata.generation`), so its conditions, a passed progress deadline included, are about an earlier spec | Run doctor again in a moment, or wait for `kubectl -n <ns> rollout status deploy/<name>` |
| warn: a rollout is under way: N of M replicas updated, K running | Available, but fewer replicas than `spec.replicas` run the current spec, or old replicas still run next to the updated ones | Wait for `kubectl -n <ns> rollout status deploy/<name>` |
| warn: only N of M replicas ready | Available, but fewer replicas are ready than `spec.replicas` | `kubectl -n <ns> get pods -l control-plane=<component>` and the events of the pods that are not ready |
| warn: scaled to 0 replicas | Stopped on purpose | Scale it up when it should run |

## Repositories

Each entry of `spec.repositories` must be complete for the executor to open a pull request: `url`, and for GitHub `github.appClientID`, `github.installationID` and `github.signer.azureKeyVault.keyID`. The check reads only the settings object: it calls neither GitHub nor Key Vault, so a wrong installation ID or a key the executor may not use shows only in the conditions of the first request (see [troubleshooting](../troubleshooting.md#pull-request-step)).

| Result | Meaning | What to do |
| --- | --- | --- |
| fail: incomplete, missing ... | The named fields are empty | Set them, see [first proposal](../first-proposal.md) |
| fail: provider ... is not one this plugin knows | The plugin is older than the CRDs | Install the plugin of the CRDs' release |
| warn: targetBranch is not set | `main` is used | Set it when the branch Flux tracks is another one |
| warn: none configured | No pull request can be opened (observe-only) | Add a repository when you leave observe-only |
| warn: not checked: the RemediationSettings was not read | The settings object does not exist or is not readable with your permissions (see [settings](#settings)), so neither the repositories nor Flux were checked | Fix the settings check first |

## Flux

For each repository with a URL: the Flux `GitRepository` objects, in every namespace, whose `spec.url` equals the repository's URL exactly (the match the executor makes, [ADR-0012](../adr/0012-object-to-source-mapping.md)), and the `Kustomization` objects that apply each of them. Each is checked against the rules the executor's source mapping applies to its spec (the same code, so the two cannot differ), then its `Ready` condition, and for a Ready Kustomization whether it has applied the revision its GitRepository last fetched. The rules that need a target object, such as `status.inventory`, are not checked.

| Result | Meaning | What to do |
| --- | --- | --- |
| fail: the executor refuses it: `<reason>`: ... | The executor's source mapping stops on this object's spec for every target it delivers, even when Flux reports it `Ready`. Doctor runs the executor's own rules, so the reason is the one a request would get: on a GitRepository `SourceOptions` (`spec.suspend`, `spec.ignore`, `spec.recurseSubmodules` or `spec.sparseCheckout`), `SourceIncludes` (`spec.include`) or `SourceNotBranch` (no `spec.ref.branch`, or `spec.ref.tag`, `semver`, `name` or `commit`); on a Kustomization `KustomizationSuspended` (`spec.suspend`), `RemoteCluster` (`spec.kubeConfig`), `KustomizationTransforms` (`patches`, `images`, `components`, `namePrefix`, `nameSuffix`, `targetNamespace`, `postBuild` and the like) or `Decryption` (`spec.decryption`) | Remove the field, or expect the requests for its targets to stop with that reason; see [delivery from Flux](../troubleshooting.md#delivery-from-flux) |
| fail: tracks branch X, not the target branch | The executor denies every request for targets it delivers (`BranchMismatch`) | Make `spec.ref.branch` and `targetBranch` equal |
| fail: failing, ... | `Stalled` is `True` or `Ready` is `False` for the current generation; the reason and the controller's message follow | `flux get sources git -A`, `flux get kustomizations -A`, and [delivery from Flux](../troubleshooting.md#delivery-from-flux) |
| warn: reconciling | Not `Ready` yet and not failing | Run doctor again once Flux has reconciled |
| warn: Ready, but it has not applied the revision its GitRepository last fetched | On a Kustomization: its `status.lastAppliedRevision` is not the `status.artifact.revision` of its Ready GitRepository, by the executor's own comparison. Flux is still reconciling, and until it has applied that revision the executor refuses requests for its targets (`RevisionMismatch`) | Run doctor again after the Kustomization's next reconcile (`flux reconcile kustomization <name> -n <ns>` starts one); if it stays, see [delivery from Flux](../troubleshooting.md#delivery-from-flux) |
| warn: no Flux GitRepository has the URL | No target delivered from this repository can be changed (`RepositoryNotConfigured`). After a cut list (below) it says "among the ones read": one may exist | Make the URLs equal, character for character (no `.git`, lower case) |
| warn: no Kustomization applies this GitRepository | Nothing applies it, or a HelmRelease does, which doctor does not check. After a cut list (below) it says "among the ones read": one may exist | Check that the path you expect is applied |
| warn: does not serve Flux | Flux is not installed in the cluster | Install Flux, or expect every request to stop at the source mapping |
| warn: more than 10000 objects: only the first 10000 were read | The cluster holds more GitRepositories or Kustomizations than doctor lists (20 pages of 500); a "none among the ones read" warn below may be one it did not reach | Check the objects of that repository with `flux get sources git -A` and `flux get kustomizations -A` |

## Delivery sources

The report above starts from the other side: from the workloads of the namespaces the analyzer watches, it lists the Flux objects that deliver them and says for each whether the executor's source mapping would accept it ([ADR-0012](../adr/0012-object-to-source-mapping.md)). Use it before you onboard a namespace, to see which repositories and branches the settings must name, and after, to see why requests for a workload would stop.

- **Watched namespaces** come from the `--watch-namespaces` argument of the analyzer Deployment (the chart's `analyzer.watchNamespaces`), parsed as the analyzer parses it: by the rules of Go's flag package, so the last occurrence wins, and nothing after `--` or after the first argument that is not a flag counts. At most 256 are read.
- **Workloads** are the Deployments and StatefulSets there, the kinds the action catalog changes through Git. Only their metadata is read. Each is counted by its labels, in the mapping's order: `helm.toolkit.fluxcd.io/name` means a HelmRelease installed it (`HelmManaged`: the mapping does not edit Helm values); no `kustomize.toolkit.fluxcd.io/name` and `/namespace` labels is `NoFluxLabels`; labels that are not a valid name are `InvalidFluxLabels`. The others count for the Kustomization they name.
- **Sources** are one line per Kustomization those labels name, with its GitRepository, the GitRepository's URL as host and path only (no scheme, user, password, port, query or fragment, since `spec.url` may carry credentials), the branch it tracks, and the settings repository whose `url` equals `spec.url` exactly, as the executor matches it. A URL prints only when its scheme is `ssh`, `http` or `https`, or it is an scp-style `user@host:path`, and no `@` follows the user part; anything else prints as `(not a URL)`, so a password with an unescaped `/`, `?` or `#` is never printed in part. At most 256 sources are listed, then a line `and N more sources, not listed`; the check line counts them all.

Each source has a state and, unless it is `ok`, the reason a request for its workloads would get. Doctor calls the mapping's own rules (`KustomizationRefusal`, `CheckGitRepository` and `CompareSource`) in the executor's sequence, so the reasons cannot differ from the executor's: a Kustomization that is suspended is `KustomizationSuspended`, one that is not Ready is `not-ready` whatever its spec sets, and only a Ready one is checked for `spec.kubeConfig`, transforms and decryption. The rules that need a target object (`status.inventory`), the policy's `repositoryRef`, the branch in the code host and the files in Git are not checked: doctor calls no Git host.

| State | Meaning | What to do |
| --- | --- | --- |
| `ok` | The Kustomization and its GitRepository pass the mapping's rules, both are Ready, the Kustomization has applied what its source last fetched, and exactly one settings repository has the URL with the branch as its `targetBranch` | Nothing |
| `refused` | The mapping stops on it whatever Flux does next. On the Kustomization: `KustomizationNotFound`, `KustomizationSuspended`, `RemoteCluster`, `KustomizationTransforms`, `Decryption`, `UnsupportedSource` (its source is not a GitRepository); on the GitRepository: `SourceNotFound`, `SourceOptions` (a suspended source is one), `SourceIncludes`, `SourceNotBranch`; against the settings: `RepositoryNotConfigured` (no repository, or more than one, has the URL; `settings: not named` or `named by more than one repository`) or `BranchMismatch` | Remove the field, add or correct the repository in the settings, or accept that requests for these workloads stop with that reason; see [delivery from Flux](../troubleshooting.md#delivery-from-flux) |
| `not-ready` | Flux has not reconciled it: `KustomizationNotReady`, `KustomizationReconciling`, `SourceNotReady`, or `RevisionMismatch` (it has not applied the revision its GitRepository last fetched) | Run doctor again after the next reconcile; if it stays, see the Flux checks above |
| `unknown` | A read was not allowed or failed, the list was cut short, or the settings were not read | The detail names the read; see below |

The check line `Delivery sources` passes only when every workload comes from a source in state `ok` and every read was made; otherwise it warns. It never fails: a cluster may deliver some workloads by other means on purpose, and a missing permission says nothing about the install.

**Reads not made.** A read your permissions do not allow, or that fails, is printed as `NOT CHECKED` with its verb, resource and namespace (for example `list statefulsets.apps in namespace demo: not allowed with your permissions`), and the counts and sources it would have given are missing or `unknown`, never `ok`. An unreadable analyzer Deployment leaves the watched namespaces unknown, and the line names that read too (for example `list deployments.apps in namespace shadewright-system: not allowed with your permissions, so the watched namespaces are unknown`).

## Permissions

Doctor reads with `get` and `list` only. Beyond the reads of [`status`](check-status.md#permissions) it needs these; without one, its check warns. The [delivery sources](#delivery-sources) report adds one read: `list` on `deployments` and `statefulsets` in each watched namespace (the Role `shadewright-doctor-workloads` below, bound in each one). It reads the analyzer Deployment's arguments with the `list` on `deployments` in the operator namespace, and the Flux objects with the `list` of the Flux checks.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: shadewright-doctor
  namespace: shadewright-system
rules:
- apiGroups: ["apps"]
  resources: ["deployments"]
  verbs: ["list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: shadewright-doctor
rules:
- apiGroups: ["apiextensions.k8s.io"]
  resources: ["customresourcedefinitions"]
  resourceNames:
  - incidents.remediation.shadewright.io
  - remediationpolicies.remediation.shadewright.io
  - remediationrequests.remediation.shadewright.io
  - remediationsettings.remediation.shadewright.io
  verbs: ["get"]
- apiGroups: ["source.toolkit.fluxcd.io"]
  resources: ["gitrepositories"]
  verbs: ["list"]
- apiGroups: ["kustomize.toolkit.fluxcd.io"]
  resources: ["kustomizations"]
  verbs: ["list"]
---
# In each watched namespace, for the delivery sources report.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: shadewright-doctor-workloads
  namespace: demo
rules:
- apiGroups: ["apps"]
  resources: ["deployments", "statefulsets"]
  verbs: ["list"]
```

The chart creates neither role: bind them to the people who run doctor. The Flux lists span every namespace, since a source may live in any; they are read in pages of 500, at most 20 pages.

!!! note "Not yet run"
    Doctor is covered by unit tests in `cmd/kubectl-shadewright` with fake clients, one per check passing, failing and forbidden. It has not yet run against a Kind cluster or the lab.
