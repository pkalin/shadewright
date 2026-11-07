---
title: Troubleshooting
---

# Troubleshooting

A `RemediationRequest` that stops says why in its conditions. Find the reason, then the row below. Every reason in the executor and the policy code is listed, taken from the code on `main`.

For an overview first, run [`kubectl shadewright status`](how-to/check-status.md): per Incident its due reason, the outcome of the last analysis and its requests with phase, policy decision and pull request, then the day's analysis budget and the kill switch, read-only with your own permissions.

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

A request stuck in `Pending` with no condition is being retried: the signer cannot be built (no workload identity), or Key Vault or GitHub returned an error that may pass. Source mapping opens the repository and its signer before the full policy decision (after the field checks, design 6.2), so the request stays `Pending`, not `PolicyChecked`, and nothing ends it (the 30 minute deadline starts only after an allowed decision). Read the executor log and fix what it names: the `azure.workload.identity/client-id` annotation, the federated credential, the `sign` grant on the key, the Key Vault `keyID`, or a GitHub outage. A host that asks for a delay (`Retry-After`) is waited for.

## Limits

Set on `Queued` (`True`) when the policy allows a request but a limit holds it back; a refusal at the first check is never held, but a request already `Queued` is decided again only once the limits let it through, so a policy change that now refuses it waits for a free slot. The request keeps no phase or `Pending` and checks only the limits every 30 seconds (every 10 minutes, and at once when the holder changes, behind a lock held by a request that stopped), so it reads no Git meanwhile; once they let it through, the condition goes and the policy check runs again. Limits count the requests in the operator namespace.

| Reason | Cause | Fix |
| --- | --- | --- |
| `TargetLocked` | Another request names the same target UID and holds the lock (`status.holdsTarget`): from its allowed decision until its pull request is known merged or closed, also after it stopped (for example `NeedsIntervention` with `HeadMoved`, or `Expired` with `PullRequestDeadlineCleanupRefused`), and while it is deleted until its finalizer is gone. Behind a stopped holder the request checks every 10 minutes, and at once when the holder changes | Wait for it to end. For a stopped holder, close or merge its pull request: its next look-up (every 10 minutes) frees the target. Or delete the holder: its finalizer closes the pull request and the lock goes with the finalizer. If the Git host refuses that close, the holder stays `Terminating` and keeps the lock: fix the permission, or close the pull request and remove the finalizer by hand ([uninstall, step 3](upgrade-uninstall.md#uninstall)). A stopped holder whose look-up keeps failing (its repository left the settings, the recorded pull request cannot be found) keeps the lock until you delete it, after checking that its pull request is closed |
| `ConcurrencyLimitReached` | `maxConcurrentRemediations` requests are active | Wait, review the open pull requests, or raise the limit in `RemediationSettings` |
| `CircuitOpen` | Three requests that went through the policy check failed (`Failed`, `NeedsIntervention`) since the latest success (`Merged` or a later phase: `WaitingForDelivery`, `Verifying`, `Succeeded`), the latest less than 30 minutes ago, or the breaker's probe ended less than 30 minutes ago without a success (`Expired`, `Cancelled`, `Superseded` and `Denied` included). Outside a probe `Expired` does not count | Find out why they failed. 30 minutes after the latest failure (or failed probe) the breaker is half-open and lets a request through as the probe; it closes on a success. Deleting the failed requests closes it at once. After an upgrade it starts closed: requests that ended before then are not counted |
| `CircuitHalfOpen` | The breaker's cool-down is over and another request runs as its probe: a request let through while the breaker was tripped, marked with `status.breakerProbe: true`. It runs until it reaches a final phase or `Merged`, also while it is being deleted, so a probe that waits for a person's approval holds the others back until it merges, fails, or is closed or deleted (once its pull request is recorded it has no `status.deadline`, so it does not expire) | Wait for the probe: its success (`Merged` or a later phase) closes the breaker and the waiting requests go through, anything else opens it again for 30 minutes. Find the probe with `kubectl -n shadewright-system get rr -o jsonpath='{range .items[?(@.status.breakerProbe==true)]}{.metadata.name} {.status.phase}{"\n"}{end}'` (earlier probes, which have ended, are listed too); deleting the running one lets the next request through as a new probe once its finalizer is gone, without the extra cool-down an ended probe would add. A request let through before this field existed is never taken as a probe |

A request that stays in `Pending` with no condition while the executor logs a failed list of `RemediationRequests` is held back too: the limits cannot be read, so nothing goes through. A request that was already `Queued` keeps its earlier reason, if any, while the list is unreadable. The executor lists requests with field selectors on `status.phase`, `status.holdsTarget` and `status.breakerCounts` (#293, #390), so the list also fails when the RemediationRequest CRD does not have those selectable fields yet (an upgrade that has not applied the CRDs of the same release) or the API server is too old for CRD field selectors (Shadewright needs Kubernetes 1.32); apply the CRDs of the release or upgrade the cluster. An expired continue token between pages fails one check only; the next one reads again.

The breaker reads only the ended requests whose `status.breakerCounts` is not `false` ([design 10.7](design/10-security.md#the-breakers-watermark)): the latest request in `Succeeded`, every outcome after it, and requests not classified yet. A long row of failures and probes without a final success stays read, about 48 more requests a day while the breaker is tripped. Right after the upgrade that adds the field, the first checks still read the older successes and failures, as the release before did; after each decision up to 100 of those before the latest `Succeeded` are cleared, so the reads shrink over the following checks.

- **Log lines.** `Failed to mark the outcomes the circuit breaker no longer needs` means a clearing read or write failed. Nothing is let through by it: those requests are read again at the next check. At error level, and counted in `shadewright_circuit_breaker_clear_errors_total`, it is a failure that does not go away by itself (`Forbidden`, `Invalid`): check the executor's role and that the CRDs match the release.
- **What the breaker reads:** `kubectl -n shadewright-system get rr --field-selector 'status.breakerCounts!=false'`.
- **Editing by hand.** Setting the field to `false` hides that outcome from the breaker, like editing `phase`.
- **Deleting a request in `Succeeded`.** It waits for its finalizer to set `status.breakerCounts` back to `true` on the requests it cleared. If the finalizer keeps failing, the request stays `Terminating` and the executor logs the failed restore. When the API server refuses the restore for good (`Forbidden` or `Invalid`: the executor's role or the CRD does not match the release), the executor also logs an error, counts it in `shadewright_circuit_breaker_clear_errors_total` and tries again every 5 minutes; fix the role or the CRD and the next try lets the request go. To let it go on purpose instead, remove the finalizer (`kubectl -n shadewright-system patch rr <name> --type=json -p '[{"op":"remove","path":"/metadata/finalizers/0"}]'`, after checking with `get -o yaml` that it is the only finalizer). That skips the restore and, for a request that ended `NeedsIntervention` or `Expired`, the close of its pull request, which you then close by hand. The cost: the outcomes it cleared stay hidden, so the breaker may stay closed after the failures that came before that success. Restore them yourself: list them with `kubectl -n shadewright-system get rr --field-selector status.breakerClearedBy=<name>` and set `status.breakerCounts` to `true` on each (`kubectl patch rr <each> --subresource=status --type=merge -p '{"status":{"breakerCounts":true,"breakerClearedBy":null}}'`).

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
| `InvalidPath` | `spec.path`, a path in a kustomization file, or a name listed in a path without a kustomization file, is not a plain relative path inside the repository (a hidden manifest such as `.env.yaml`, or a manifest in a hidden directory, in such a path counts; hidden files that are not manifests and names Flux leaves out of its artifact are skipped) | Use a relative path; rename hidden manifests in a path Flux generates a kustomization for, or add a `kustomization.yaml` |
| `NoKustomizationFile` | A directory that a kustomization file lists as a resource has no kustomization file (kustomize would fail too), or the Kustomization's `spec.path` is not a directory at the branch head. A path without a kustomization file is searched as Flux builds it (#122) | Add a `kustomization.yaml` to the listed directory, or correct `spec.path` |
| `UnsupportedKustomization` | A kustomization file uses more than plain `resources`: patches, generators, components, transformers, prefixes, a namespace or remote resources | Flatten the layout |
| `FileNotReadable` | A file the build needs is missing, over 1 MiB or not valid YAML | Fix the file |
| `TooManyFiles` | The build is deeper than 8 kustomization directories (5 listed directories below a path without a kustomization file), lists more than 256 files, needs more than 512 reads and listings, or lists a directory of more than 1000 entries | Simplify the layout |
| `InputTooLarge` | The build reads more than 16 MiB in all, directory listings counted at 128 bytes per entry (exactly 16 MiB is accepted; a file past the budget is refused before it is downloaded, even when it is also over 1 MiB) or holds more than 4096 YAML documents | Split the Kustomization or remove what it does not need |
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

**A merge or a close is seen late.** The executor polls; it has no webhook. It looks at a waiting pull request every `--pull-request-poll-interval` (one minute by default, see the [configuration reference](reference/configuration.md#executor-only)), so the request should reach `Merged` within that interval plus one reconcile. Later than that: the executor may have restarted or changed leader (its first look at each request is a full recheck, a few seconds longer), writes may be paused (nothing is looked at until the pause ends; the first look after it removes `WritesPaused`), or the GitHub App may be rate limited (a rate-limited look waits as long as GitHub asks, at most 5 min, and the executor log says so). Each waiting pull request costs about 150 GitHub REST calls an hour at the default, against about 100 with no looks between the full rechecks; a shorter interval costs more.

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
| `PullRequestDeadline` | Phase `Expired`: no pull request within 30 minutes of the decision. If the repository of the plan can no longer be reached through the settings, the request keeps the lock on its target until the repository can be reached again and a look-up finds none of its pull requests open, or until it is deleted | Find why Git calls failed (log), apply a new request |
| `PullRequestDeadlineCleanupRefused` | Phase `Expired`: no pull request within 30 minutes of the decision, and the Git host refused to find or close one Shadewright may have opened on the branch. The request keeps the lock on its target until a look-up (every 10 minutes) finds none of its own open | Fix the permission the reason names, or close the pull request on the branch `shadewright/<request UID>` by hand; the next look-up then frees the target |
| `TargetBranchChanged` | The repository entry of the plan now names another target branch | Restore the setting or apply a new request |
| `RepositoryUnavailable` | The repository of the plan is no longer in the settings | Restore it. Any pull request is not closed |
| `GitForbidden` | The Git host refused the App: a permission it lacks, an installation, or a ruleset. Not retried | Compare with the four permissions of [ADR-0006](adr/0006-github-app-key-in-key-vault.md) and the rulesets; apply a new request. While a pull request waits it is `PlanFresh` `Unknown` and Shadewright writes nothing but keeps watching, and clears it when the host answers again |
| `GitRejected` | The Git host refused the request as invalid and Git state does not explain it. Not retried | Read the executor log, fix, apply a new request |
| `PullRequestOpened` | Progress, not a failure: the pull request is open on Shadewright's commit. Becomes the closed form below | None |
| `WaitingForReview` | `Approved` is `False`: the pull request waits for a person. With `approval` not required it still waits, because auto-merge is not built | Review and merge it |
| `PullRequestClosed` | Shadewright closed its pull request (stale plan or a change that no longer renders as planned); the message gives the number and why | See the freshness and render tables |

## Freshness

Before merge the executor rechecks every five minutes, and on any change to the request (once its pull request waits, only when none ran in the last five minutes), that the plan still matches the world. A stale plan ends `Superseded` with `PlanFresh=False`, and its pull request is closed (branch left in place). If the close cannot be done the phase is `NeedsIntervention` with `TargetBranchChanged`, `ForeignChangeRequest`, `RepositoryUnavailable`, `RepositoryUnusable`, `GitForbidden` or `GitRejected` as above.

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
| `InvalidReference` | A kustomization refers to a URL, remote base, absolute path or a path leaving the repository, or a path without a kustomization file holds a manifest or base whose name is not a plain path segment |
| `FileMissing` | The build needs a file that is missing, a listed directory has no kustomization file or more than one, or the path is not a directory (a path without a kustomization file is rendered from the one Flux would generate) |
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

An Incident that never gets analyzed shows why in its `AnalysisDue` condition. It is due (status `True`) with `FirstAnalysis`, `NewOccurrence`, `CooldownExpired` or `AnalysisTimedOut`. Not due (status `False`): `AnalysisDisabled` (`spec.analysis.maxAnalysesPerDay` is 0, or the deprecated `spec.maxAnalysesPerDay` of v0.1.0 is still set to 0: remove it, see [upgrade](upgrade-uninstall.md#upgrade)), `DailyBudgetReached`, `BudgetNotFound` (the ConfigMap `shadewright-analysis-budget` was deleted; recreate it empty), `BudgetUnreadable`, `BudgetReadFailed`, `SettingsNotFound`, `SettingsUnreadable`, `SettingsIncomplete` (the settings hold no `spec.analysis.maxAnalysesPerDay`, which the schema's defaults normally fill in), `NotRecent` (never analyzed and not seen within the recent window, by default the cooldown, 15 min: a symptom that stopped is not analyzed, a lasting state of a module's symptom source such as a KEDA ceiling never is skipped, and any new observation, an event series counting up again included, makes it due again once it has settled; the condition changing to `NotRecent` also writes a Normal event `AnalysisSkipped` on the Incident and counts in `shadewright_analysis_skipped_not_recent_total`, once until the Incident is next marked due; a pass writes at most 20 of these, after the other marks, so a backlog takes a few passes), `Debouncing`, `CoolingDown`, `NoNewData`, `Analyzing` (a run is in progress; on the first pass after the run wrote its outcome it changes to `Proposed` or `Escalated`, even while the budget is unknown, and on the first pass after the analysis timeout to the reason decided then, such as `CoolingDown`, `Debouncing` or a budget reason; while the settings or the budget cannot be read, a run taken as lost keeps `Analyzing`), `Proposed`, `Escalated`, `Recurred` (escalated by a recurrence after resolution, not by an analysis), `Resolved` (never analyzed again: a symptom seen again escalates it, with the condition `Recurred`, within 24 hours of `resolvedTime` or without one, and opens the next occurrence `incident-<64 hex>-<n>` after), `NotObserved`, `AnalysisStartUnknown` and `ForeignFingerprint` (the name is not the one derived from the spec's fingerprint and occurrence number). The settings status carries `UnknownModules` (a name in `spec.modules` no module has), `DetectionFailed` and `NoWatchedNamespaces` (the analyzer watches no namespace; see [No Incident appears](#no-incident-appears)). Without `--triage-model` no analysis runs, so `AnalysisDue` is a mark only.

## The Incident cap is reached

Symptoms: `shadewright_incident_observations_refused_total` rises, a Warning event with reason `IncidentCapReached` on the analyzer's Pod, and a new problem produces no Incident, nor does a symptom of a `Resolved` Incident seen again more than 24 hours after its resolution (its next occurrence is a new Incident too). The cap is `--max-incidents` (500) and counts every Incident, `Resolved` ones included. Watch the gauge `shadewright_incidents` by `phase` (built); an alert at 80% of the cap (400 Incidents in total) is planned, not shipped.

Clean-up is manual: the analyzer has no `delete` on Incidents. Follow [Clear Resolved Incidents at the cap](how-to/clear-resolved-incidents.md).

If the cap fills again at once, the namespaces are producing many fingerprints: see [Limit the namespaces it watches](how-to/limit-watched-namespaces.md).

## No Incident appears

The analyzer records Incidents only for symptoms in the namespaces it watches. The Helm chart's default `analyzer.watchNamespaces` is empty, which watches no namespace (never all of them): the analyzer starts and is Ready, but reads no events or pods. Check, in this order:

- `kubectl -n shadewright-system logs deploy/shadewright-analyzer | grep "Watching"` says `Watching no namespaces`, or lists the namespaces.
- The gauge `shadewright_analyzer_watched_namespaces` is `0`.
- `kubectl get remediationsettings default -o jsonpath='{.status.conditions[?(@.type=="NoWatchedNamespaces")]}'` shows `True` with reason `NoNamespacesWatched`. The condition appears only once a `RemediationSettings` object exists, and the chart creates none by default (`settings.create: false`).

To watch namespaces, upgrade with the list (`--set 'analyzer.watchNamespaces={team-a,team-b}'`); each one must exist and gets a read-only RoleBinding. With the kustomize install, set `--watch-namespaces` and add the RoleBindings as `config/watch` does. If the list is set and still nothing appears, the namespace may lack its RoleBinding (the analyzer's reads are refused there) or have no symptom the collector knows.
