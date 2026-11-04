---
title: Troubleshooting
---

# Troubleshooting

A `RemediationRequest` that stops says why in its conditions. Find the reason, then the row below. Every reason in the executor and the policy code is listed, taken from the code at v0.1.0.

```bash
kubectl -n shadewright-system get remediationrequest <name> \
  -o jsonpath='{.status.phase}{"\n"}{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
kubectl -n shadewright-system logs deploy/shadewright-executor
```

## How to read the result

| Phase | Meaning | Retried? |
| --- | --- | --- |
| `Denied` | Policy or settings do not allow the plan | No. Fix the cause and apply a request with a new name (the spec is immutable) |
| `NeedsIntervention` | The executor cannot decide or continue safely; a person must look | No, same as above. Automation for that request has stopped |
| `Superseded` | The plan went stale before merge and its pull request was closed | No. A new plan is a new request |
| `Expired` | No pull request was opened within 30 minutes of the policy decision | No |
| `Pending`, `PolicyChecked`, `PRCreated`, `AwaitingApproval` | Working or waiting for a person | Yes |
| `Merged` | The pull request was merged. Delivery and verification are **planned** | Ends here |

`Cancelled`, `AutoMergeQueued`, `WaitingForDelivery`, `Verifying`, `Succeeded` and `Failed` exist in the API but nothing sets them yet (**planned**).

The reason is on one of these conditions:

- `PolicyAllowed`: the policy check. `False` is `Denied`, `Unknown` is `NeedsIntervention` or a wait, `True` is allowed.
- `PullRequestOpened`: a stop before a pull request is recorded, and the history of one that was closed.
- `Approved`: a stop after a pull request is recorded; `WaitingForReview` while it waits.
- `PlanFresh`: `False` when the plan went stale (`Superseded`), `Unknown` while the Git host refuses a freshness check.
- `Queued`: `True` while a limit holds a request back before its policy check (see [Limits](#limits)).

Many reasons exist in more than one place with the same cause (the source mapping and the policy both check the repository); they are listed once.

## No reason at all

A request stuck in `Pending` with no condition is being retried: the signer cannot be built (no workload identity), or Key Vault or GitHub returned an error that may pass. Source mapping opens the repository and its signer before the policy decision, so the request stays `Pending`, not `PolicyChecked`, and nothing ends it (the 30 minute deadline starts only after an allowed decision). Read the executor log and fix what it names: the `azure.workload.identity/client-id` annotation, the federated credential, the `sign` grant on the key, the Key Vault `keyID`, or a GitHub outage. A host that asks for a delay (`Retry-After`) is waited for.

## Limits

Set on `Queued` (`True`) when the policy allows a request but a limit holds it back; a refusal at the first check is never held, but a request already `Queued` is decided again only once the limits let it through, so a policy change that now refuses it waits for a free slot. The request keeps no phase or `Pending` and checks only the limits every 30 seconds, so it reads no Git meanwhile; once they let it through, the condition goes and the policy check runs again. Limits count the requests in the operator namespace.

| Reason | Cause | Fix |
| --- | --- | --- |
| `TargetLocked` | Another active request names the same target UID: from `PolicyChecked` until `Merged` or a final phase, or deleted but still closing its pull request | Wait for it to end, or delete it if it should not go on |
| `ConcurrencyLimitReached` | `maxConcurrentRemediations` requests are active | Wait, review the open pull requests, or raise the limit in `RemediationSettings` |
| `CircuitOpen` | Three requests that went through the policy check failed (`Failed`, `NeedsIntervention`) since the latest `Merged` (or `Succeeded`) request, the latest less than 30 minutes ago, or the probe let through after the cool-down ended less than 30 minutes ago without a success (`Expired`, `Cancelled` and `Superseded` included). Outside a probe `Expired` does not count | Find out why they failed. 30 minutes after the latest failure (or failed probe) the breaker is half-open and lets one request through as the probe; it closes when a request is `Merged` (or `Succeeded`). Deleting the failed requests closes it at once. After an upgrade it starts closed: requests that ended before then are not counted |
| `CircuitHalfOpen` | The breaker's cool-down is over and another request runs as its probe: the first one let through after the trip (by the time of its `PolicyAllowed` condition), until it reaches a final phase or `Merged` | Wait for the probe: its success (`Merged`, `Succeeded`) closes the breaker and the waiting requests go through, anything else opens it again for 30 minutes. Find the probe with `kubectl get rr` (the request in `PolicyChecked` or later, before `Merged`, let through last); deleting it lets the next request through as a new probe |

A request that stays in `Pending` with no condition while the executor logs a failed list of `RemediationRequests` is held back too: the limits cannot be read, so nothing goes through. A request that was already `Queued` keeps its earlier reason, if any, while the list is unreadable.

## Request and target

Set by the policy check, on `PolicyAllowed`.

| Reason | Outcome | Cause | Fix |
| --- | --- | --- | --- |
| `PolicyNotFound` | Denied | `spec.policyRef` names no `RemediationPolicy` | Create the policy or correct the name |
| `SettingsNotFound` | Denied | No `RemediationSettings` named `default` | Create it |
| `InvalidInput` | NeedsIntervention | The action does not fit the target: not a namespaced object pinned by UID, or a kind the action may not change; or the action has no previous value | Use the catalog: `SetContainerMemoryLimit` on an `apps/v1` Deployment or StatefulSet, `AdjustKedaMaxReplicas` on a `keda.sh/v1alpha1` ScaledObject, with `uid` set |
| `TargetNotFound` | NeedsIntervention | The target does not exist | Check namespace and name; the request is final, so apply a new one |
| `TargetMismatch` | Denied | The live object has another UID than `targetRef.uid` | Read the current UID and apply a new request. Also happens if the object was recreated |
| `TargetChanged` | NeedsIntervention | The UID changed while the source was mapped (see also the freshness table) | Apply a new request |
| `PreviousValueMismatch` | Denied | `previous` (or `previousValue`) is not the live value, so the policy's percent limits would be measured from a wrong baseline | Read the live value and apply a new request |
| `FieldNotFound` | NeedsIntervention | The field the action changes is not set on the live object, or in the file in Git | The action changes an existing value. Set it in Git first |
| `ValueMismatch` | NeedsIntervention | The value in the file differs from the live one: someone or something changed the live object (a manual edit, an autoscaler) or Flux has not applied the file yet | Make the live value and the file agree (let Flux apply, or revert the live change), then apply a new request with `previous` equal to that value |
| `WithinApprovedBounds` | allowed | Every check passed | Nothing to do |

## Delivery from Flux

The executor maps the live object to its file through Flux labels, the `Kustomization` and its `GitRepository` (ADR-0012). All are `NeedsIntervention` unless marked, on `PolicyAllowed`.

| Reason | Cause | Fix |
| --- | --- | --- |
| `HelmManaged` | A `HelmRelease` manages the object; Helm values are not supported | Not fixable here. Change the chart values by hand |
| `NoFluxLabels` | The object has no `kustomize.toolkit.fluxcd.io` labels (created by hand or by another tool) | Let a Flux Kustomization apply it |
| `InvalidFluxLabels` | The labels are not valid object names | Remove the false labels and let Flux reapply |
| `KustomizationNotFound` | The labelled Kustomization does not exist (or Flux CRDs are not installed) | Restore it or reapply the object |
| `KustomizationSuspended` | The Kustomization is suspended, so a merge would not be applied | Resume it |
| `KustomizationNotReady` | The Kustomization is not `Ready` for its current generation (failing, or still reconciling) | Fix the Kustomization (`flux get kustomizations`), wait until it is ready, then apply a new request |
| `NotInInventory` | **Denied.** The Kustomization named by the labels does not list the object in `status.inventory`, so the labels cannot be trusted | Check which Kustomization really applies it. A copied label does not grant anything |
| `KustomizationTransforms` | The Kustomization sets one of `patches`, `patchesStrategicMerge`, `patchesJson6902`, `images`, `components`, `namePrefix`, `nameSuffix`, `targetNamespace`, `postBuild`, so Git is not what is applied | Move the transforms into the manifests |
| `RemoteCluster` | The Kustomization applies to another cluster (`spec.kubeConfig`) | Not supported |
| `Decryption` | The Kustomization decrypts SOPS files | Not supported: editing would break the MAC |
| `UnsupportedSource` | The source is not a `GitRepository`, or `sourceRef` is invalid | Use a Git source |
| `SourceNotFound` | The `GitRepository` does not exist | Restore it |
| `SourceNotBranch` | The `GitRepository` does not track only `spec.ref.branch` (no ref, or a tag, semver, name or commit is set) | Track a branch |
| `SourceIncludes` | The `GitRepository` has `spec.include` | Remove the include |
| `SourceOptions` | The `GitRepository` is suspended or sets `ignore`, `recurseSubmodules` or `sparseCheckout`, or a `.sourceignore` file is on the searched paths | Remove the option or file |
| `SourceNotReady` | The `GitRepository` is not `Ready` for its current generation | Fix the source (URL, credentials) and wait |
| `RevisionMismatch` | The Kustomization has not applied the revision its source last fetched, or that revision is not from the tracked branch | Wait for Flux to catch up, then apply a new request |

## Repository, branch and path

On `PolicyAllowed`; repeated by the policy check on the resolved source.

| Reason | Outcome | Cause | Fix |
| --- | --- | --- | --- |
| `RepositoryNotConfigured` | Denied | The `GitRepository` URL equals no `spec.repositories[].url` in the settings, or more than one. The match is exact: case and a `.git` suffix count | Make the settings URL equal the Flux URL |
| `RepositoryNotInPolicy` | Denied | The matching repository is not the policy's `gitOps.repositoryRef` (or that name is in no settings entry) | Correct `repositoryRef` |
| `RepositoryNotAllowed` | Denied | The same check, made again by the policy on the resolved source | As `RepositoryNotInPolicy` |
| `RepositoryUnusable` | NeedsIntervention | The settings entry cannot be used to open the repository (URL, client ID, installation ID, `keyID` or signer). Also the end of a waiting request whose settings became unusable | Fix the settings, apply a new request |
| `BranchMismatch` | Denied | The branch Flux tracks is not the repository's `targetBranch` | Align `targetBranch` with Flux (default is `main`) |
| `BranchNotAllowed` | Denied | The same check, made again by the policy | As `BranchMismatch` |
| `BranchNotFound` | NeedsIntervention | The target branch does not exist in the repository | Create it or correct the settings |
| `PathNotAllowed` | Denied | The file that declares the object matches none of the policy's `allowedPaths` | Widen `allowedPaths` deliberately, or the change is out of scope |

## Files in Git

All `NeedsIntervention`, on `PolicyAllowed`. The mapping reads plain kustomize manifests only.

| Reason | Cause | Fix |
| --- | --- | --- |
| `InvalidPath` | `spec.path`, or a path in a kustomization file, is not a plain relative path inside the repository | Use a relative path |
| `NoKustomizationFile` | A directory has no kustomization file (Flux would generate one) | Add a `kustomization.yaml` listing the resources |
| `UnsupportedKustomization` | A kustomization file uses more than plain `resources`: patches, generators, components, transformers, prefixes, a namespace or remote resources | Flatten the layout |
| `FileNotReadable` | A file the build needs is missing, over 1 MiB or not valid YAML | Fix the file |
| `TooManyFiles` | The build is deeper than 8 directories, lists more than 256 files or needs more than 512 reads | Simplify the layout |
| `InputTooLarge` | The build reads more than 16 MiB in all or holds more than 4096 YAML documents | Split the Kustomization or remove what it does not need |
| `DeclarationNotFound` | No document declares the object (for example a generator or patch produces it) | Declare it in a plain manifest |
| `AmbiguousDeclaration` | More than one document could declare it | Keep exactly one |

## Policy decision

On `PolicyAllowed`; all `Denied` unless marked. These are the checks of the policy against the plan, in the order they run.

| Reason | Cause | Fix |
| --- | --- | --- |
| `IncompleteInput` | NeedsIntervention. The request, policy, settings, source or target is missing | Should not happen; report it with the executor log |
| `PolicyMismatch` | The policy is not the one the request names | Report it |
| `RequestNamespaceNotAllowed` | The request's namespace is not in `requestNamespaces` | Add the operator namespace to the policy |
| `ModeNotSupported` | Mode is not GitOps | Use GitOps |
| `ActionUnknown` | The action type is not in the catalog | Use a catalog action |
| `InvalidParameters` | Parameters missing, from another action, outside the catalog's range, or `desired` equals `previous` (memory: whole bytes between 1Mi and 1Ti; replicas: 1 to 1000) | Correct the parameters |
| `KindExcluded` | The target's kind is never remediated: Shadewright's own, admission, CRD, RBAC and Flux API groups; Secrets, ServiceAccounts, Namespaces, PersistentVolumes and Claims, NetworkPolicies | By design. Change it by hand |
| `KindNotAllowed` | The action cannot target that apiVersion and kind | Use the kinds above |
| `TargetNamespaceNotAllowed` | The target's namespace is not in `targets.namespaces` | Add it, or the change is out of scope |
| `TargetLabelsNotMatched` | The labels do not match the policy's `objectSelector` | Label the object or adjust the selector |
| `ActionNotAllowed` | The policy does not list the action | Add it with bounds |
| `PolicyInvalid` | The policy is unusable: a malformed selector, an action listed twice, or bounds missing or outside the schema | Fix the policy |
| `BelowMinimum` | `desired` is below `min` | Choose a value in bounds |
| `AboveMaximum` | `desired` is above `max` | As above |
| `IncreaseTooLarge` | The increase is more than `maxIncreasePercent` of `previous` | Smaller step, or raise the limit deliberately |
| `DecreaseNotAllowed` | A decrease, and `maxDecreasePercent` is 0 or unset | Decreases are refused by default |
| `DecreaseTooLarge` | The decrease is more than `maxDecreasePercent` | Smaller step |
| `ModuleNotEnabled` | The action belongs to an integration module (`AdjustKedaMaxReplicas`: `keda`) that `spec.modules.integrations` does not list | Enable the module in the settings. Also a freshness reason |
| `ScaledObjectNotReady` | NeedsIntervention. The ScaledObject is not ready (`Ready` not true, paused, on fallback, `HPAActive` false, being deleted, or malformed), so raising `maxReplicaCount` is not the fix. Also reached when KEDA did not report within 10 minutes | Fix the autoscaler first |
| `ScaledObjectReadinessUnknown` | Not final. `PolicyAllowed` is `Unknown` while KEDA has not yet reported the ScaledObject; checked again at 15 s to 2 minute intervals, up to 10 minutes | Wait, and check KEDA runs |

## Pull request step

Stops before a pull request is recorded are on `PullRequestOpened`; after, on `Approved`. All end `NeedsIntervention` unless marked.

| Reason | Cause | Fix |
| --- | --- | --- |
| `PlanIncomplete` | `status.source` has no `buildPath`, so the change cannot be rendered | Apply a new request |
| `PatchRefused` | The typed patcher refused the file on the branch or on the target branch. The message names a patch reason (see below) | Fix the file, apply a new request |
| `TargetFileDiverged` | The target file on the target branch is not the file the change was made from (another edit landed), before the commit or while the pull request waited. Merging would combine edits no render saw. Nothing is committed in the first case; a waiting pull request is closed in the second | Apply a new request against the current file |
| `BranchDiverged` | The Shadewright branch holds neither the previous nor the planned value | Delete the branch if it is not needed, apply a new request |
| `ForeignChangeRequest` | A pull request for the branch exists without this request's marker; it is not touched | Close or rename it, apply a new request |
| `HeadMoved` | Someone else pushed to the branch, or the pull request's head is not the commit Shadewright made | Review the branch by hand. Automation stopped |
| `ChangeRequestClosed` | The pull request was closed without a merge by a person (or a close whose status write was lost). It is never reopened | Intended if you rejected it. Otherwise apply a new request |
| `ChangeRequestGone` | The recorded pull request cannot be found | Check the repository |
| `ChangeRequestMerged` | Phase `Merged`. The pull request was merged | Done. Delivery is Flux's job and verification is **planned** |
| `PullRequestDeadline` | Phase `Expired`: no pull request within 30 minutes of the decision | Find why Git calls failed (log), apply a new request. If the Git host also refused the cleanup, the message says to close any pull request by hand |
| `TargetBranchChanged` | The repository entry of the plan now names another target branch | Restore the setting or apply a new request |
| `RepositoryUnavailable` | The repository of the plan is no longer in the settings | Restore it. Any pull request is not closed |
| `GitForbidden` | The Git host refused the App: a permission it lacks, an installation, or a ruleset. Not retried | Compare with the four permissions of [ADR-0006](adr/0006-github-app-key-in-key-vault.md) and the rulesets; apply a new request. While a pull request waits it is `PlanFresh` `Unknown` and Shadewright writes nothing but keeps watching, and clears it when the host answers again |
| `GitRejected` | The Git host refused the request as invalid and Git state does not explain it. Not retried | Read the executor log, fix, apply a new request |
| `PullRequestOpened` | Progress, not a failure: the pull request is open on Shadewright's commit. Becomes the closed form below | None |
| `WaitingForReview` | `Approved` is `False`: the pull request waits for a person. With `approval` not required it still waits, because auto-merge is not built | Review and merge it |
| `PullRequestClosed` | Shadewright closed its pull request (stale plan or a change that no longer renders as planned); the message gives the number and why | See the freshness and render tables |

## Freshness

Before merge the executor rechecks every five minutes, and on any change, that the plan still matches the world. A stale plan ends `Superseded` with `PlanFresh=False`, and its pull request is closed (branch left in place). If the close cannot be done the phase is `NeedsIntervention` with `TargetBranchChanged`, `ForeignChangeRequest`, `RepositoryUnavailable`, `RepositoryUnusable`, `GitForbidden` or `GitRejected` as above.

| Reason | Cause | Fix |
| --- | --- | --- |
| `PolicyChanged` | The policy was deleted, recreated or edited (any change of its generation) | Apply a new request against the new policy |
| `SettingsChanged` | The settings or the repository entry were removed, or its URL, installation or branch changed | As above |
| `TargetChanged` | The target is gone, has another UID, or no longer holds the previous value live | As above |
| `TargetFileChanged` | The file on the target branch is gone, no longer declares the target alone, or no longer holds the previous value | As above |
| `PlanChanged` | The recorded inputs no longer give the recorded digest | Apply a new request |
| `SourceChanged` | Mapping the target again gives another repository, branch or file, or now denies or needs a person | Look at the Flux objects, apply a new request |
| `ScaledObjectNotReady` | The ScaledObject is not ready on two reads | Fix the autoscaler |
| `ModuleNotEnabled` | The module was removed from the settings | Enable it, apply a new request |

While `spec.paused` is true nothing is written and requests wait; none of these is reached. See the [emergency stop](emergency.md).

## Render check

Before committing, and again when the target branch moves while the pull request waits, the executor renders the kustomize build with and without the change and requires one difference: the planned field of the target. A refusal puts the reason below on `PullRequestOpened` or `Approved` with the message "The rendered change is not exactly the planned one", ends `NeedsIntervention`, and closes a waiting pull request.

| Reason | Cause |
| --- | --- |
| `InvalidInput` | The target, action, paths or file set are not usable |
| `UnsupportedKustomization` | A kustomization file uses generators, Helm charts, plugins, replacements or anything outside the allow-list |
| `InvalidReference` | A kustomization refers to a URL, remote base, absolute path or a path leaving the repository |
| `FileMissing` | The build needs a file that is missing, or a directory has no kustomization file or more than one |
| `TooLarge` | A limit was hit: file size, number of files or reads, depth, or directory visits |
| `InvalidYAML` | A kustomization file does not parse or has a duplicate key |
| `RenderFailed` | Kustomize cannot build the path, before or after the change |
| `FileNotInBuild` | The changed file is not a resource of the build |
| `TargetNotFound` | The rendered output does not hold the target |
| `AmbiguousTarget` | The target may appear more than once, or two objects share an identity |
| `ObjectsAddedOrRemoved` | The change adds or removes a rendered object |
| `OtherObjectChanged` | The change alters another rendered object, for example through a shared base |
| `OtherFieldChanged` | The change alters another field of the target |
| `PreviousValueDiffers` | The rendered target before the change does not hold the previous value: something else sets the field |
| `DesiredValueMissing` | The rendered target after the change lacks the desired value, for example an overlay overrides it |
| `FieldNotFound` | The rendered target does not set the field |
| `SourceIgnored` | A `.sourceignore` applies to a directory of the build |
| `SchemaInvalid` | The changed object fails the basic schema checks |

Fix for all: make the build plain, so the one value is declared once and nothing overrides it, then apply a new request.

The patcher reasons named inside a `PatchRefused` message are `PreviousValueDiffers` (the file no longer holds the planned baseline), `FieldMissing` (object, field or container absent), `AmbiguousContainer` (two containers with the name), `TargetNotFound`, `AmbiguousTarget`, `Unsupported` (YAML the in-place edit does not handle), `InvalidInput`, `InvalidYAML` (does not parse, duplicate key or too large) and `VerificationFailed` (the patched file differs from the original in more than the one value).

## Not request reasons

An Incident that never gets analyzed shows why in its `AnalysisDue` condition. It is due (status `True`) with `FirstAnalysis`, `NewOccurrence`, `CooldownExpired` or `AnalysisTimedOut`. Not due (status `False`): `AnalysisDisabled` (`maxAnalysesPerDay` is 0), `DailyBudgetReached`, `BudgetNotFound` (the ConfigMap `shadewright-analysis-budget` was deleted; recreate it empty), `BudgetUnreadable`, `BudgetReadFailed`, `SettingsNotFound`, `SettingsUnreadable`, `Debouncing`, `CoolingDown`, `NoNewData`, `Analyzing`, `Proposed`, `Escalated`, `NotObserved`, `AnalysisStartUnknown` and `ForeignFingerprint`. The settings status carries `UnknownModules` (a name in `spec.modules` no module has) and `DetectionFailed`. No analysis runs yet, so `AnalysisDue` is a mark only.
