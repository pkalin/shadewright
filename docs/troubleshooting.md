---
title: Troubleshooting
---

# Troubleshooting

A `RemediationRequest` that stops says why in its conditions. Find the reason, then the row below. Every reason in the executor and the policy code is listed, taken from the code on `main`.

For an overview first, run [`kubectl shadewright status`](how-to/check-status.md): per Incident its due reason, the outcome of the last analysis and its requests with phase, policy decision and pull request, then the day's analysis budget and the kill switch, read-only with your own permissions. For one request, [`kubectl shadewright explain <name>`](how-to/check-status.md#explain-one-request) prints its condition timeline and the condition that stopped it, with a link to the section below that lists the reason. When nothing works yet, or after an upgrade, [`kubectl shadewright doctor`](how-to/check-install.md) checks the install itself: CRDs, settings, kill switch, Deployments, repositories and the Flux objects that deliver them, against the executor's own source rules.

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
| `Superseded` with reason `AnotherRequestMerged` | Another request of the same Incident, target and field (the same container, for a memory limit) was merged after this one was created (the message names it and its pull request). This one had not taken the target lock and opened no pull request | No. The Incident has its fix in the merged request; nothing to do here |
| `Superseded` with reason `HandedOff` ([ADR-0028](adr/0028-reviewer-hints-through-the-pull-request.md)) | A reviewer's hint produced a replacement request, which took over this request's pull request and branch; the pull request is not closed | No. Follow the replacement request, which holds the pull request and the target lock |
| `Denied` with reason `ChainLinkInvalid` ([ADR-0028](adr/0028-reviewer-hints-through-the-pull-request.md)) | A replacement request names a request it does not replace: another Incident or target, another chain, a request not waiting for approval, or a hint that is not pending | No. Nothing was taken over and the old pull request is untouched; look at who created the request |
| `Cancelled` with reason `HandoffRefused` | Another replacement reserved the same hint first, or the hint or the old request changed before the handoff began | No. The old pull request is untouched; post the hint again if it is still wanted |
| `Cancelled` with reason `HandoffAborted` | The pull request was merged or closed, or someone pushed to the branch, while the handoff ran, or the new plan changes another file than the pull request does | No. If it was merged, the request that owns the merged plan is `Merged`; if someone pushed, the old request is `NeedsIntervention` and the pull request is a person's |
| `Expired` | No pull request was opened within 30 minutes of the policy decision | No |
| `Pending`, `PolicyChecked`, `PRCreated`, `AwaitingApproval` | Working or waiting for a person | Yes |
| `Merged` | The pull request was merged. Delivery and verification are **planned** | Ends here |

`Cancelled`, `AutoMergeQueued`, `WaitingForDelivery`, `Verifying`, `Succeeded` and `Failed` exist in the API but nothing sets them yet (**planned**).

The reason is on one of these conditions:

- `PolicyAllowed`: the policy check. `False` is `Denied`, `Unknown` is `NeedsIntervention` or a wait, `True` is allowed.
- `PullRequestOpened`: a stop before a pull request is recorded, and the history of one that was closed.
- `Approved`: a stop after a pull request is recorded; `WaitingForReview` while it waits, or `ReviewCheckRefused` or `ReviewListTooLong` while it waits on a check that does not pass on the current head (both are final for that head; a replacement after a handoff starts again at `WaitingForReview`).
- `PlanFresh`: `False` when the plan went stale or another request of the same Incident was merged (`Superseded`), `Unknown` while the Git host refuses a freshness check, or while Flux is still reconciling the Kustomization (`KustomizationReconciling`, at most 10 minutes, then the plan is stale).
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
| `ValueMismatch` | NeedsIntervention | The value in the file differs from the live one: someone or something changed the live object (a manual edit, an autoscaler) or Flux has not applied the file yet. Also when the memory value in the file is not a quantity Shadewright reads: at most 40 characters, a decimal number with an optional suffix (`Ki` to `Ei`, `n` to `E`) or an exponent of at most two digits (`1e9`, not `1e100`; #593), in the file or on the live object. Some valid but unusual spellings are refused too, on purpose: `536870912e000` (a three-digit exponent) or a number with over 34 trailing zeros (longer than 40 characters); write `512Mi` instead | Make the live value and the file agree (let Flux apply, or revert the live change), then apply a new request with `previous` equal to that value |
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
| `KustomizationReconciling` | **A wait, not a stop.** The Kustomization is not `Ready` for its current generation but reports no failure: Flux is still reconciling it, for example applying a pull request just merged. The request keeps no phase, `PolicyAllowed` is `Unknown` with this reason, and it is checked again after 15 s to 2 min | Nothing. After 10 minutes it ends `NeedsIntervention` with `KustomizationNotReady` |
| `KustomizationNotReady` | The Kustomization is not `Ready` for its current generation and fails (`Ready` `False` with a reason other than `HealthCheckCanceled`, or `Stalled`), or it was still reconciling after a 10 minute wait (`KustomizationReconciling`) | Fix the Kustomization (`flux get kustomizations`), wait until it is ready, then apply a new request |
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
| `FileNotReadable` | A file the build needs is missing, over 1 MiB or not valid YAML, or uses YAML anchors, aliases, merge keys or text after `---` on a separator line, a line break other than LF or CRLF, or a YAML directive (the message names the file and the line) | Fix the file; see [YAML the render check refuses](#yaml-the-render-check-refuses) |
| `TooManyFiles` | The build is deeper than 8 kustomization directories (5 listed directories below a path without a kustomization file), lists more than 256 files, needs more than 512 reads and listings, or lists a directory of more than 1000 entries | Simplify the layout |
| `InputTooLarge` | The build reads more than 16 MiB in all, directory listings counted at 128 bytes per entry (exactly 16 MiB is accepted; a file past the budget is refused before it is downloaded, even when it is also over 1 MiB) or holds more than 4096 YAML documents (also one kustomization file, or one manifest document holding a List of more than 4096 objects), or a kustomization file or manifest document of more than 131,072 YAML nodes, counted from the bytes before anything parses it (text in scalars and comments counts too, #595) | Split the Kustomization or remove what it does not need |
| `TooLarge` | The build is over the render check's input limits: more than 4 MiB or 4,096 YAML documents counted with repetition, 256 directory visits, 256 files, one file of more than 131,072 YAML nodes (#595), or patches and transformers that can add more than 4 MiB to the render. Checked once the mapping resolved, so no plan or approval is asked for a change that can never render | See [A build over the budget](#a-build-over-the-budget) |
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
| `InvalidParameters` | Parameters missing, from another action, outside the catalog's range, or `desired` equals `previous` (memory: whole bytes between 1Mi and 1Ti, written in the quantity form described under `ValueMismatch`; replicas: 1 to 1000) | Correct the parameters |
| `KindExcluded` | The target's kind is never remediated: Shadewright's own, admission, CRD, RBAC and Flux API groups; Secrets, ServiceAccounts, Namespaces, PersistentVolumes and Claims, NetworkPolicies | By design. Change it by hand |
| `KindNotAllowed` | The action cannot target that apiVersion and kind | Use the kinds above |
| `TargetNamespaceNotAllowed` | The target's namespace is not in `targets.namespaces` | Add it, or the change is out of scope |
| `TargetLabelsNotMatched` | The labels do not match the policy's `objectSelector` | Label the object or adjust the selector |
| `ActionNotAllowed` | The policy does not list the action | Add it with bounds |
| `PolicyInvalid` | The policy is unusable: a malformed selector, an action listed twice, or bounds missing or outside the schema | Fix the policy |
| `BelowMinimum` | `desired` is below `min` | Choose a value in bounds |
| `AboveMaximum` | `desired` is above `max` | As above |
| `IncreaseTooLarge` | The increase is more than `maxIncreasePercent` of `previous` | Smaller step, or raise the limit deliberately. On a request the analyzer proposed, its context showed these bounds (design 3.3, #493): check the analyzer's `Context sources` line says `policyBounds=true`, and that it may `get` the policy (`analyzer-policy-reader`; an analyzer whose role leaves out a policy of `--action-policies` refuses to start, and a failed read at analysis time is logged as `Failed to read RemediationPolicy`) |
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
| `BranchDiverged` | The Shadewright branch holds neither the previous nor the planned value, or holds more than Shadewright's one commit on top of the target branch (someone else pushed to it). Before the pull request is opened, the file on the branch must also be byte for byte the planned change. Nothing is committed on such a branch and no pull request is opened; one that is open (opened while someone pushed, or found before it was recorded, also when its file holds another value) is recorded and closed by its number; a close that fails for a reason worth a retry (a host failure, paused writes, the deadline) is retried; if the Git host refuses it for good, it stays recorded as open, the message says to close it by hand, and the target stays locked until it is closed. A waiting pull request is closed. When someone pushes to a recorded pull request and its new head holds more than the planned change, Shadewright posts one comment (marked, so a retry does not post it again) that hands it to a person and stops, leaving it open (the target stays locked until it is merged or closed) | Review what was pushed to the branch, delete it if it is not needed, apply a new request |
| `ForeignChangeRequest` | A pull request for the branch exists without this request's marker; it is not touched | Close or rename it, apply a new request |
| `HeadMoved` | The pull request's head is not the commit Shadewright made, but it still holds only the planned change (for example the same change committed again), or the pull request is another one or closed | Review the branch by hand. Automation stopped |
| `ChangeRequestClosed` | The pull request was closed without a merge by a person (or a close whose status write was lost). It is never reopened | Intended if you rejected it. Otherwise apply a new request |
| `MergedWithForeignChange` | Shadewright's pull request failed a check (for example someone pushed to the branch while it was opened or while it waited), and a person merged it before Shadewright closed it or stopped. A merged head is judged against its own parent on the target branch, so a merge commit alone does not cause this. The message names the pull request; the target is released | Review what the merged change holds and revert what was not planned |
| `ChangeRequestGone` | The recorded pull request cannot be found | Check the repository |
| `ChangeRequestMerged` | Phase `Merged`. The pull request was merged | Done. Delivery is Flux's job and verification is **planned** |
| `PullRequestDeadline` | Phase `Expired`: no pull request within 30 minutes of the decision. If the repository of the plan can no longer be reached through the settings, the request keeps the lock on its target until the repository can be reached again and a look-up finds none of its pull requests open, or until it is deleted | Find why Git calls failed (log), apply a new request |
| `PullRequestDeadlineCleanupRefused` | Phase `Expired`: no pull request within 30 minutes of the decision, and the Git host refused to find or close one Shadewright may have opened on the branch. The request keeps the lock on its target until a look-up (every 10 minutes) finds none of its own open | Fix the permission the reason names, or close the pull request on the branch `shadewright/<request UID>` by hand; the next look-up then frees the target |
| `TargetBranchChanged` | The repository entry of the plan now names another target branch | Restore the setting or apply a new request |
| `RepositoryUnavailable` | The repository of the plan is no longer in the settings | Restore it. Any pull request is not closed |
| `GitForbidden` | The Git host refused the App: a permission it lacks, an installation, or a ruleset. Not retried | Compare with the five permissions of [ADR-0006](adr/0006-github-app-key-in-key-vault.md) and the rulesets; apply a new request. While a pull request waits it is `PlanFresh` `Unknown` and Shadewright writes nothing but keeps watching, and clears it when the host answers again |
| `GitRejected` | The Git host refused the request as invalid and Git state does not explain it. Not retried | Read the executor log, fix, apply a new request |
| `PullRequestOpened` | Progress, not a failure: the pull request is open on Shadewright's commit. Becomes the closed form below | None |
| `WaitingForReview` | `Approved` is `False`: the pull request waits for a person. With `approval` not required it still waits, because auto-merge is not built | Review and merge it |
| `ReviewCheckRefused` | `Approved` is `False`, with a Warning Event of the same reason: with reviewer hints on, the Git host refused for good to create or complete the check run `shadewright/review` on the pull request's head ([ADR-0028](adr/0028-reviewer-hints-through-the-pull-request.md) section 5), so a ruleset that requires it blocks the merge. Not an end: the request keeps waiting, and the check is not asked again on that head | Give the App `checks: write` ([ADR-0006](adr/0006-github-app-key-in-key-vault.md)) and check the rulesets; a new head creates the check again and sets `Approved` back to `WaitingForReview`, or a repository admin merges past it |
| `ReviewListTooLong` | `Approved` is `False`, with a Warning Event of the same reason: with reviewer hints on, the pull request has more reviews than the merge gate reads (4 looks of the reviews list), so the gate failed the check run `shadewright/review` on that head (`completed`, `failure`, recorded `Failed`) and does not read the reviews again there. Not an end: the request keeps waiting | A person reviews the pull request and decides the merge; a repository admin merges past the required check, or a new head starts the check again and sets `Approved` back to `WaitingForReview` |
| `PullRequestRewriteRefused` | `PullRequestOpened` stays `True` with this reason, with a Warning Event of the same reason: a replacement took the pull request over, but the Git host refused for good to rewrite its title and body (a permission or rule, `GitForbidden`, or text it refuses as invalid, `GitRejected`), so they still describe the plan it replaced. Not asked again | Read the branch, which carries the new plan; edit the title and body by hand if needed |
| `HandoffRecoveryFailed` | Phase `NeedsIntervention`: a replacement could not read the branch to learn whether its handoff commit is on the pull request it replaces, and kept failing until 15 minutes after its deadline. Its handoff is `Aborted` | Find why Git calls failed (log), then review the branch and the pull request by hand: the commit may be on it |
| `PullRequestClosed` | Shadewright closed its pull request (stale plan or a change that no longer renders as planned); the message gives the number and why | See the freshness and render tables |

## Rollout after the merge

<span class="sw-status sw-built">built</span> On `RolledOut`, for a Deployment or StatefulSet target, after the Flux Kustomization named by the target's labels reports the merge commit (or a later commit of the target branch that holds it) applied; that time is `status.pullRequest.appliedObservedAt`. The phase stays `Merged`. Until the delivery is seen applied the request has no `RolledOut`; after an hour without it, see `DeliveryNotObserved` below. The check always ends with one of the verdicts below; `status.rollout` records the commit Flux applied, the target generation and template it produced (read right after the apply), the template revision it judged, and `ended` once it is over; each is set once. A later commit of the target branch that holds the merge commit counts as ours; a change made outside Git before that read is not told apart. Shadewright only reports: it never deletes or restarts a pod (invariant 2), and it reads only the target object and its Kustomization with the `get` it already has, never the pods. While writes are paused (`spec.paused`), a later applied commit is not checked against the merge commit on the Git host, so the check waits, and the time paused does not count against the hour: after a resume the check has what was left of it.

| Reason | Status | Cause | Fix |
| --- | --- | --- | --- |
| `RolloutComplete` | `True` | Every pod runs the applied template and is ready | Nothing |
| `RolloutProgressing` | `Unknown` | The rollout is under way and the deadline (10 minutes after the revision was applied) has not passed | Wait |
| `DeliveryNotObserved` | `False` | The merge commit (or a later commit of the target branch that holds it) was not seen applied by the Kustomization named by the target's labels within an hour of the merge: Flux has not applied it, its apply fails, the target has no Kustomization labels or is installed by a HelmRelease. The message names the Kustomization and the merge commit; one Event is recorded and the rollout is not checked | Check the Kustomization (`flux get kustomizations`, its `Ready` message), then the rollout yourself as below |
| `KindNotChecked` | `Unknown` | The target is not a Deployment or StatefulSet (for example a ScaledObject), whose rollout the check does not cover. No Event | Check the target yourself |
| `RolloutStuck` | `False` | The rollout did not complete within the deadline. For a StatefulSet the message gives how many pods are not Ready and names the pods on the old revision, highest ordinal first, inferred from the StatefulSet status (`spec.ordinals.start` honoured); it names a pod to delete only when every pod is not Ready, since the status does not say which pod is. For a Deployment it gives the counts. One Event is recorded; the rollout is read again for an hour, so one you unblock turns `True` | See below |
| `RolloutSuperseded` | `Unknown` | The target moved on to a newer template revision than the one applied with the merge commit (another change was applied), or its template changed again before its controller observed the generation the applied commit produced, so this rollout is not judged. One Normal Event | Check the newer rollout yourself |
| `RolloutNotDetermined` | `Unknown` | The check ended without a verdict: the target is gone or was replaced, or it (or the delivery) could not be read until the end of the check. One Event; the executor log has the error | Check the target and the executor log |

**A StatefulSet stuck on the old revision.** A StatefulSet with the default `OrderedReady` policy does not replace a pod that is not Ready on the old revision, so a fix for a crashing pod (for example a larger memory limit) is applied to the template but never reaches the pod. Kubernetes documents this as [forced rollback](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/#forced-rollback). Find the pod that is not Ready. If it runs the old revision, check that the new template is the one you want, then delete that pod yourself, never a Ready one. If a pod on the new revision is not Ready, the new template fails: fix it instead.

```bash
kubectl -n <namespace> get statefulset <name> -o jsonpath='{.status.currentRevision} {.status.updateRevision} {.status.updatedReplicas}/{.spec.replicas}{"\n"}'
kubectl -n <namespace> get pods -L controller-revision-hash
kubectl -n <namespace> delete pod <name>-<ordinal>
```

The pod is recreated from the new revision and the rollout continues. With `updateStrategy: OnDelete` every pod needs this step. **A Deployment stuck**: read `kubectl rollout status deployment/<name>` and the events of its new ReplicaSet (image pull, quota, a failing readiness probe).

## Freshness

Before merge the executor rechecks every five minutes, and on any change to the request (once its pull request waits, only when none ran in the last five minutes), that the plan still matches the world. A stale plan ends `Superseded` with `PlanFresh=False`, and its pull request is closed (branch left in place). If the close cannot be done the phase is `NeedsIntervention` with `TargetBranchChanged`, `ForeignChangeRequest`, `RepositoryUnavailable`, `RepositoryUnusable`, `GitForbidden` or `GitRejected` as above.

| Reason | Cause | Fix |
| --- | --- | --- |
| `PolicyChanged` | The policy was deleted, recreated or edited (any change of its generation) | Apply a new request against the new policy |
| `SettingsChanged` | The settings or the repository entry were removed, or its URL, installation or branch changed | As above |
| `TargetChanged` | The target is gone, has another UID, or no longer holds the previous value live | As above |
| `TargetFileChanged` | The file on the target branch is gone, no longer declares the target alone, no longer holds the previous value, or now uses YAML anchors, aliases, merge keys or text on a separator line, another line break or a directive (#594), or holds more than 131,072 YAML nodes in one document (#595) | As above |
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
| `UnsupportedKustomization` | A kustomization file uses generators, Helm charts, plugins, replacements or anything outside the allow-list, or a patch holds a JSON patch `copy` operation (each copy can double a value; write the value out, or use `add`) |
| `InvalidReference` | A kustomization refers to a URL, remote base, absolute path or a path leaving the repository, or a path without a kustomization file holds a manifest or base whose name is not a plain path segment |
| `FileMissing` | The build needs a file that is missing, a listed directory has no kustomization file or more than one, or the path is not a directory (a path without a kustomization file is rendered from the one Flux would generate) |
| `TooLarge` | A limit was hit: file size, number of files or reads, depth, directory visits, the build budget of 4 MiB or 4,096 YAML documents (counted with repetition, the tree after the change included; a List counts 1 plus its items), 131,072 YAML nodes in one file (counted from the bytes before anything parses it, #595), 4 MiB that patches and transformers can add to the render, or 1,024 rendered objects. The input limits are checked at the policy check already; see [A build over the budget](#a-build-over-the-budget) for what the request shows |
| `InvalidYAML` | A kustomization file does not parse or has a duplicate key, or a file of the build is not UTF-8, does not parse, or uses YAML anchors, aliases, merge keys or text after `---` on a separator line, a line break other than LF or CRLF, or a YAML directive; see [YAML the render check refuses](#yaml-the-render-check-refuses) |
| `RenderFailed` | Kustomize cannot build the path, before or after the change |
| `FileNotInBuild` | The changed file is not a resource of the build |
| `TargetNotFound` | The rendered output does not hold the target |
| `AmbiguousTarget` | The target may appear more than once, or two objects share an identity |
| `ObjectsAddedOrRemoved` | The change adds or removes a rendered object |
| `OtherObjectChanged` | The change alters another rendered object, for example through a shared base |
| `OtherFieldChanged` | The change alters another field of the target |
| `PreviousValueDiffers` | The rendered target before the change does not hold the previous value: something else sets the field, or the rendered memory value is outside the quantity form described under `ValueMismatch` |
| `DesiredValueMissing` | The rendered target after the change lacks the desired value, for example an overlay overrides it |
| `FieldNotFound` | The rendered target does not set the field |
| `SourceIgnored` | A `.sourceignore` applies to a directory of the build |
| `SchemaInvalid` | The changed object fails the basic schema checks, for example its memory request is outside the quantity form described under `ValueMismatch` |

Fix for all: make the build plain, so the one value is declared once and nothing overrides it, then apply a new request.

### A build over the budget

The render runs in the executor, inside its memory limit (256 Mi since #615; the limits below were measured against 128 Mi and stay the bound), so the build of one Flux Kustomization path may hold at most 4 MiB of input and 1,024 objects ([design 8.2.2](design/8-gitops-on-github.md#822-steps-4-and-5-the-semantic-diff-check)). Input counts with repetition: every path to a directory adds its kustomization file and the files it lists, so a 1 MiB base under four overlays counts 4 MiB. The YAML documents of the resource and patch files (and inline patches) count the same way, at most 4,096, so that many small objects are refused before kustomize builds them; the 1,024 objects are counted on the render's output. Patches with a target and the label, annotation, namespace, name prefix and suffix, image and replica transformers copy their value into every object they can reach, so the cost of each value times the places it can reach (every object of the kind a patch's target names, or every document of the build, the fields Kustomize writes in each object and every list item on their paths for a transformer, every value of the build for a name change) may add up to at most 4 MiB, counted with repetition too. A value costs its bytes plus 128 for each YAML node (each key, value and list item), since the render's memory follows nodes more than bytes. A large value applied to every object (a long annotation, an embedded file in a patch with a target) or one of many small entries (dozens of labels, a long list) is the usual cause: move it into the objects that need it, or name the kind in the patch target. A large build with common labels, a namespace, a name prefix, image overrides and patches with a target on many objects can reach the limit too (about 800 objects with a patch on every Deployment). The limits are fixed in the code; nothing in the settings raises them.

What the request shows (no Event is recorded for either case):

- **At the policy check**, for a build over the input limits (bytes, YAML documents, directory visits, files, what patches and transformers can add): phase `NeedsIntervention`, condition `PolicyAllowed` with status `Unknown`, reason `TooLarge` and the message "The build is over a limit of the render check (at most 4194304 bytes of input and 4096 YAML documents counted with repetition, 256 directory visits, 256 files of at most 1048576 bytes, 4194304 bytes that patches and transformers can add to the render, counting 128 for each YAML node); no plan was made". No plan digest, branch or pull request is made, and no approval is asked for.
- **At the render**, for a build over 1,024 objects, a change that takes the tree over the byte, document or transform budget (a value written with one more digit in a build at the budget), or a pull request that was waiting when you upgraded (see [upgrade](upgrade-uninstall.md#upgrade)): condition `PullRequestOpened` (before a pull request) or `Approved` (after) with status `False`, reason `TooLarge` and the message "The rendered change is not exactly the planned one (TooLarge)", followed by "; nothing was committed" or "; the pull request was closed". The executor logs `Stopped RemediationRequest` with `reason` `TooLarge`.

The render message does not name the limit, since the render check's own text may name files of the repository; the reason and this page say which limits apply. To bring the target's build under them:

- Split the path. Give the target's application its own directory and its own Flux `Kustomization` (its own `spec.path`), instead of one path that builds every application of the cluster. The check renders only the path that delivers the target.
- Cut repetition. A large base reached through many overlays counts once per overlay; move what only one overlay needs out of the shared base, or give each environment its own Kustomization.
- Move bulk data out of the build. Large ConfigMaps or CRD bundles that rarely change can live under a separate Kustomization the target's path does not include.

Then apply a new request; the refused one stays `NeedsIntervention`.

### YAML the render check refuses

Every file the render check reads (kustomization files, the resource and patch files they list, and inline patches) must be UTF-8 YAML without anchors (`&name`), aliases (`*name`) or merge keys (`<<: *name`, or a key with the `!!merge` tag; `<<` as a plain value is accepted), a document separator line may hold only `---` and a comment after a space or a tab, lines end in LF or CRLF only (no bare CR, NEL, LS or PS), and no line starts with a YAML directive (`%YAML`, `%TAG`). Kustomize expands these before it counts or builds anything, so a few bytes can become thousands of objects or crash the executor; the check refuses them with `InvalidYAML` instead (#488, #492). The source mapping, which runs first, reads kustomization files and manifests with a parser that expands them too, so it refuses the same with `FileNotReadable` (#594). The typed patcher checks the target file the same way before it reads it, and refuses it with the patch reason `InvalidYAML` inside `PatchRefused` (#595). The request shows only the reason and where, not the file; look for these constructs in the files of the target's build.

To fix a file, write out what the anchor stood for:

```yaml
# Refused
defaults: &limits {cpu: 500m, memory: 256Mi}
resources:
  limits: *limits
  requests:
    <<: *limits
    cpu: 100m

# Accepted
resources:
  limits: {cpu: 500m, memory: 256Mi}
  requests: {cpu: 100m, memory: 256Mi}
```

For values shared by many objects, use a Kustomize patch or a component instead of an anchor. Save a file in UTF-16 or UTF-32 as UTF-8 (a UTF-8 byte order mark is accepted). Then apply a new request.

The patcher reasons named inside a `PatchRefused` message are `PreviousValueDiffers` (the file no longer holds the planned baseline), `FieldMissing` (object, field or container absent), `AmbiguousContainer` (two containers with the name), `TargetNotFound`, `AmbiguousTarget`, `Unsupported` (YAML the in-place edit does not handle, or a memory value outside the quantity form described under `ValueMismatch`), `InvalidInput`, `InvalidYAML` (does not parse, duplicate key, over 1 MiB, more than 4096 objects or 131,072 YAML nodes, or YAML the check above refuses anywhere in the file: anchors, aliases, merge keys, text on a separator line, another line break, a directive or text that is not UTF-8, #595) and `VerificationFailed` (the patched file differs from the original in more than the one value).

## Not request reasons

An Incident that never gets analyzed shows why in its `AnalysisDue` condition. It is due (status `True`) with `FirstAnalysis`, `NewOccurrence`, `CooldownExpired`, `AnalysisTimedOut` or `ReviewerHint` (a `Proposed` Incident whose waiting request holds a reviewer hint no analysis has answered, [ADR-0028](adr/0028-reviewer-hints-through-the-pull-request.md)). Not due (status `False`): `AnalysisDisabled` (`spec.analysis.maxAnalysesPerDay` is 0, or the deprecated `spec.maxAnalysesPerDay` of v0.1.0 is still set to 0: remove it, see [upgrade](upgrade-uninstall.md#upgrade)), `DailyBudgetReached`, `BudgetNotFound` (the ConfigMap `shadewright-analysis-budget` was deleted; recreate it empty), `BudgetUnreadable`, `BudgetReadFailed`, `SettingsNotFound`, `SettingsUnreadable`, `SettingsIncomplete` (the settings hold no `spec.analysis.maxAnalysesPerDay`, which the schema's defaults normally fill in), `NotRecent` (never analyzed and not seen within the recent window, by default the cooldown, 15 min: a symptom that stopped is not analyzed, a lasting state of a module's symptom source such as a KEDA ceiling never is skipped, and any new observation, an event series counting up again included, makes it due again once it has settled; the condition changing to `NotRecent` also writes a Normal event `AnalysisSkipped` on the Incident and counts in `shadewright_analysis_skipped_not_recent_total`, once until the Incident is next marked due; a pass writes at most 20 of these, after the other marks, so a backlog takes a few passes), `Debouncing`, `CoolingDown`, `NoNewData`, `Analyzing` (a run is in progress; on the first pass after the run wrote its outcome it changes to `Proposed` or `Escalated`, even while the budget is unknown, and on the first pass after the analysis timeout to the reason decided then, such as `CoolingDown`, `Debouncing` or a budget reason; while the settings or the budget cannot be read, a run taken as lost keeps `Analyzing`), `Proposed`, `Escalated` (the condition `Lifecycle` says when a request that ended without success escalated it: `RequestDenied`, `RequestFailed`, ...), `Recurred` (escalated by a recurrence after resolution, not by an analysis), `Resolved` (never analyzed again: a symptom seen again escalates it, with the condition `Recurred`, within 24 hours of `resolvedTime` or without one, and opens the next occurrence `incident-<64 hex>-<n>` after), `NotObserved`, `AnalysisStartUnknown` and `ForeignFingerprint` (the name is not the one derived from the spec's fingerprint and occurrence number). The settings status carries `UnknownModules` (a name in `spec.modules` no module has), `DetectionFailed` and `NoWatchedNamespaces` (the analyzer watches no namespace; see [No Incident appears](#no-incident-appears)). Without `--triage-model` no analysis runs, so `AnalysisDue` is a mark only.

A reviewer hint shows in `status.handledHint` (its source, comment ID and digest) with the `Analyzed` reason that answers it: `Proposed` (a replacement request was created), `PlanUnchanged` (the analysis proposed the plan of the request it would replace, so nothing was created), another reason of the analysis such as `NoProposal`, `HintNotPending` (the hint was no longer pending, so no replacement was created: a lost analysis of it was not run again, or the executor answered or declined it while the model ran), or, without an analysis, `CoolingDown` (the hint came within the cooldown since the last analysis) or `BudgetSpent` (the daily budget is used up or 0). After an analysis with a hint that created no request the Incident stays `Proposed`: the request the hint was given on still stands. A refused hint is not retried: post a new one.

An analysis that ends without a request shows why in the reason of its `Analyzed` condition. Three of them are about the previous value of a memory limit change, which the analyzer takes from the subject's pod template, never from the model ([design 3.3](design/3-observation-and-deduplication.md#the-prompt-and-the-result)):

| Reason | Meaning | What to do |
| --- | --- | --- |
| `PreviousValueUnknown` | The pod template the analyzer read has no container of the proposed name, sets no memory limit for it, sets one outside the quantity form described under `ValueMismatch`, or could not be read | The action changes an existing limit: set one in Git first, or handle the incident by hand |
| `NoChange` | The proposed limit is the one the pod template already sets; during a rollout, older pods may still run with another | Let the rollout finish, then check whether the symptom remains |
| `DirectionReversed` | Measured from the pod template's limit, the proposed change goes the other way than the model reasoned (an increase becomes a decrease, or back), or the model wrote the same previous and desired value, usually because older pods of a rollout hold another limit | Read the template's limit and decide by hand, or wait for the rollout and the next analysis |

## The Incident cap is reached

Symptoms: `shadewright_incident_observations_refused_total` rises, a Warning event with reason `IncidentCapReached` on the analyzer's Pod, and a new problem produces no Incident, nor does a symptom of a `Resolved` Incident seen again more than 24 hours after its resolution (its next occurrence is a new Incident too). The cap is `--max-incidents` (500) and counts every Incident, `Resolved` ones included. Before it is reached, at 80% of the cap (400 Incidents), the gauge `shadewright_incident_cap_near` turns 1 and the analyzer's Pod gets a Warning event `IncidentCapNear`; watch `shadewright_incidents` by `phase` to see how many are `Resolved` and safe to clear. The analyzer resolves an Incident after an hour with nothing new and no active request, or when its request `Succeeded`; a request in `Merged` counts as ended until verification is built (#92). An Incident whose newest occurrence is a lasting state of a module never goes quiet, so it leaves only by a request or a person.

Clean-up is manual: the analyzer has no `delete` on Incidents. Follow [Clear Resolved Incidents at the cap](how-to/clear-resolved-incidents.md).

If the cap fills again at once, the namespaces are producing many fingerprints: see [Limit the namespaces it watches](how-to/limit-watched-namespaces.md).

## No Incident appears

The analyzer records Incidents only for symptoms in the namespaces it watches. The Helm chart's default `analyzer.watchNamespaces` is empty, which watches no namespace (never all of them): the analyzer starts and is Ready, but reads no events or pods. Check, in this order:

- `kubectl -n shadewright-system logs deploy/shadewright-analyzer | grep "Watching"` says `Watching no namespaces`, or lists the namespaces.
- The gauge `shadewright_analyzer_watched_namespaces` is `0`.
- `kubectl get remediationsettings default -o jsonpath='{.status.conditions[?(@.type=="NoWatchedNamespaces")]}'` shows `True` with reason `NoNamespacesWatched`. The condition appears only once a `RemediationSettings` object exists, and the chart creates none by default (`settings.create: false`).

To watch namespaces, upgrade with the list (`--set 'analyzer.watchNamespaces={team-a,team-b}'`); each one must exist and gets a read-only RoleBinding. With the kustomize install, set `--watch-namespaces` and add the RoleBindings as `config/watch` does. If the list is set and still nothing appears, the namespace may lack its RoleBinding (the analyzer's reads are refused there) or have no symptom the collector knows.

For the symptoms of a module (KEDA, Flux, Kyverno, cloudflared), the module must be enabled: `spec.modules.integrations` of the `RemediationSettings` lists it. Detection alone does not turn a module on. `kubectl get remediationsettings default -o jsonpath='{.status.integrations}'` shows `"detected":true,"active":false` for a module that is installed but not listed; its symptom sources then open no Incident, and `SymptomSourcesNotWatched` does not report it, since that condition covers active modules only. Add the name, for example `spec.modules.integrations: [keda]`.

For an enabled module, the analyzer must also be allowed to read the add-on's kinds:

- `kubectl get remediationsettings default -o jsonpath='{.status.conditions[?(@.type=="SymptomSourcesNotWatched")]}'` shows `True` with reason `SourcesNotWatched` and names each kind with `NotGranted` (turn on the module's value under `analyzer.modules` in the chart), `NotServed` (the add-on's CRD is not installed) or `Failed` (the check or the watch failed, for example `list` is granted but not `watch`; the analyzer's log has the error, and a failed watch is started again after a backoff of one minute, doubled after each failure in a row up to 30 minutes). `Unknown` with reason `SourcesNotChecked` means a kind has not been checked yet: it changes within 30 seconds.
- The gauge `shadewright_analyzer_symptom_source_watched` is `0` for that kind, and the analyzer logs `Not watching a symptom source kind` once per change (`Watch of a symptom source kind failed` for each failed watch).
- With the Kyverno module enabled and `analyzer.modules.kyverno.clusterWide.enabled` off (the least-privilege default), `ClusterPolicy` and `ClusterPolicyReport` are left out on purpose: they are not named in the condition, the gauge has no series for them, and the log says `the chart leaves its optional grant out` once. Turn the value on to read them. The chart tells the analyzer which grants it rendered (`--module-grants`); a kustomize install passes no such flag, so there every module kind is checked and one without its role is reported as `NotGranted`.
