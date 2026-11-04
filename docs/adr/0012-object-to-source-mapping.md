# ADR-0012: Mapping an object to its source in Git

- Status: Accepted
- Date: 2026-11-01

## Context

A fix must change the file that declares the object, not the live object. With Flux, an object can come from a plain manifest, a Kustomize overlay or patch, a generator, or a Helm chart rendered by a HelmRelease.

## Decision

For the MVP, support only plain manifests:

1. Read the Flux labels on the live object (`kustomize.toolkit.fluxcd.io/name` and `/namespace`) to find its Kustomization.
2. From the Kustomization, take the source (`GitRepository`) and path. The Kustomization must be Ready and list the object in `status.inventory`, because labels on a live object are untrusted and anyone who can patch it could point them elsewhere. The source must track a branch (`spec.ref.branch`). Its URL must equal exactly one repository in RemediationSettings, and that repository must be the one the policy names in `gitOps.repositoryRef`; otherwise the request is `Denied`.
3. Find the one YAML document in that path that declares the object (apiVersion, kind, namespace, name), and check that the value to change in it equals the live value; if not, something such as an inline patch changes it, so stop.

The resolved repository, branch, file and base commit are written to the request's `status.source` and covered by the plan digest. They are never fields of the request spec: a request names its target, and only this mapping decides which file changes. The Flux source branch must equal the repository's `targetBranch` in RemediationSettings, and the file must match the policy's `allowedPaths`.

If any step is ambiguous (no labels, several matches, the object comes from a patch, generator or Helm chart), stop with `NeedsIntervention` and explain why. Helm values and overlays are designed later, when a real case needs them.

### Implementation details

`internal/gitops/sourcemap` implements this decision. It refines details the steps above leave open, always failing closed:

- **Which files are in the path.** `GitProvider` reads single files and cannot list a directory, so the path must hold one kustomization file (`kustomization.yaml`, `kustomization.yml` or `Kustomization`), and the files searched are the ones its `resources` list, following nested directories that have their own kustomization file. A directory without one (where Flux would generate it from every manifest) stops with `NeedsIntervention`. A kustomization file may use only `apiVersion`, `kind`, `metadata` and `resources`; anything else (patches, generators, components, Helm charts, a namespace or name prefix, images, labels) and remote resources stop with `NeedsIntervention`. Resource paths must stay inside the repository. Reads are bounded in depth, manifest count, total `ReadFile` calls (kustomization lookups included) and file size. `ReadFile` returns only regular files, never follows symbolic links, and refuses a file over `providers.MaxFileBytes` (1 MiB) with `ErrTooLarge` before downloading it; the mapping checks the size again in case an adapter does not.
- **What counts as a declaration.** A document matches only if its apiVersion, kind, `metadata.namespace` and `metadata.name` all equal the live target's. A document with the same group, kind and name but another version or no namespace could also produce the target, so it makes the match ambiguous. Multi-document files are supported; duplicate keys, invalid YAML and `List` objects stop the mapping.
- **Kustomization.** It must not be suspended, its status must describe its current generation, and spec fields that change what is built (`patches`, `images`, `components`, `namePrefix`, `nameSuffix`, `targetNamespace`, `postBuild`) stop with `NeedsIntervention`. So do `spec.decryption`, because a SOPS file can keep plaintext fields that look editable while any edit breaks its MAC, and `spec.kubeConfig`, because the inventory of a Kustomization that applies to another cluster does not say which cluster an entry is in; the `kubeConfig` check runs before the inventory is read. The inventory entry must match namespace, name, group, kind and version.
- **Target.** It must be pinned by UID and have a kind the action may change (a `keda.sh` ScaledObject, or an `apps/v1` Deployment or StatefulSet); both are checked before anything is read.
- **GitRepository.** `spec.ref` must set only `branch` (Flux gives `tag`, `semver`, `name` and `commit` precedence over it). `suspend`, `ignore`, `include`, `sparseCheckout` and `recurseSubmodules` must be unset, since each can make the artifact differ from the branch head. The GitRepository must be Ready for its current generation (a changed URL or branch raises the generation, and Flux status records no observed URL), its `status.artifact.revision` must name the tracked branch (`<branch>@sha1:<commit>`), and the Kustomization's `status.lastAppliedRevision` must equal it, so the live object comes from the latest commit fetched for the current spec. A `.sourceignore` file at the repository root or in any directory on the searched paths (or above them) stops the mapping, because Flux would filter the artifact by it; the rules are not interpreted. `spec.sourceRef` must be a valid object reference. The URL is compared byte for byte, so a `.git` suffix, a trailing slash, other letter case or an SSH URL do not match a settings entry. URLs are never written to messages, and provider errors are replaced by a transient error without their text, because a URL may carry credentials.
- **Total input.** One mapping reads at most 16 MiB in all (`MaxTotalBytes`; kustomization and `.sourceignore` lookups and cached reads included) and decodes at most 4096 YAML documents (`MaxDocuments`; ones holding only comments or blank lines included, each counted before it is decoded; a separator with nothing before it is skipped by the reader and never decoded); past either it stops with `NeedsIntervention` and reason `InputTooLarge`. The scan keeps only the declaration and stops with `AmbiguousDeclaration` at a second exact declaration or the first near match, without reading the rest of the build.
- **Value check.** The value is compared as JSON, so `10` and `"10"` differ; memory limits are compared as quantities, so `256Mi` equals `268435456`.
- **Outcomes.** `Denied`: the inventory does not list the object (the labels point at a Kustomization that does not deliver it), the URL matches no settings repository or more than one, the repository is not the policy's `repositoryRef`, the branch is not its `targetBranch`, or the file matches no `allowedPaths`. Everything else that stops is `NeedsIntervention`. Each result carries a reason (for example `NotInInventory`, `SourceNotBranch`, `AmbiguousDeclaration`, `ValueMismatch`) for the request's conditions.

## Consequences

- The supported case is unambiguous and easy to test.
- Many real repositories use overlays or charts; they get diagnoses and issues, not PRs, until support is added.
- Kustomizations that wait on workload health (`wait: true` or `healthChecks`) are refused while the workload is unhealthy, so users who want pull requests for those workloads must not set them (amendment of 2026-11-04).
- A request author (or a prompt-injected analysis) cannot aim a change at a file, repository or branch other than the one that actually delivers the target.

## Alternatives considered

- Require a `shadewright.io/source` annotation on every object: precise, but a burden on users and itself untrusted input.
- Render everything and diff back to the source: correct in general, much more work.

## Amendments

- 2026-11-02: added the "Implementation details" section, which fixes details the decision left open as `internal/gitops/sourcemap` was built ([#120](https://github.com/pkalin/shadewright/pull/120)). The decision is unchanged.
- 2026-11-04: clarified what the Ready check in step 2 means for Kustomizations that wait ([#237](https://github.com/pkalin/shadewright/issues/237), [shadewright-fleet#24](https://github.com/pkalin/shadewright-fleet/pull/24)). The mapper requires `Ready=True` for the current generation and, separately, a `status.lastAppliedRevision` equal to the GitRepository's artifact revision. Flux folds workload health into Ready only when the Kustomization sets `wait: true` or `healthChecks`; without them Ready=True means the revision was applied. With them, a workload that never becomes healthy keeps Ready at Unknown, then False after the timeout, and `lastAppliedRevision` does not advance, so the mapper refuses it with `KustomizationNotReady` (and the revision check would refuse it too). This is the intended behaviour and is unchanged. Consequence for users: a Kustomization that delivers workloads Shadewright should fix must not wait on their health, or every unhealthy-workload incident it delivers ends `NeedsIntervention` instead of a pull request; the fleet's `apps` Kustomizations do not wait for this reason, and workload health is Shadewright's to judge. Accepting applied-but-unhealthy objects would weaken the provenance checks (Ready and `lastAppliedRevision`) and needs its own ADR ([#259](https://github.com/pkalin/shadewright/issues/259)). The decision is unchanged.
- 2026-11-04: added the "Total input" limits to the implementation details (`MaxTotalBytes`, `MaxDocuments`, reason `InputTooLarge`) and the early stop at the first document that makes the match ambiguous ([#296](https://github.com/pkalin/shadewright/pull/296), [#240](https://github.com/pkalin/shadewright/issues/240)). Every new stop is `NeedsIntervention`. The decision is unchanged.
