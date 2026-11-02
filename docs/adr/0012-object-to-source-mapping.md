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

### Implementation details (clarified 2026-11-02)

`internal/gitops/sourcemap` implements this decision. It refines details the steps above leave open, always failing closed:

- **Which files are in the path.** `GitProvider` reads single files and cannot list a directory, so the path must hold one kustomization file (`kustomization.yaml`, `kustomization.yml` or `Kustomization`), and the files searched are the ones its `resources` list, following nested directories that have their own kustomization file. A directory without one (where Flux would generate it from every manifest) stops with `NeedsIntervention`. A kustomization file may use only `apiVersion`, `kind`, `metadata` and `resources`; anything else (patches, generators, components, Helm charts, a namespace or name prefix, images, labels) and remote resources stop with `NeedsIntervention`. Resource paths must stay inside the repository. Reads are bounded in depth, manifest count, total `ReadFile` calls (kustomization lookups included) and file size; `ReadFile` returns only regular files and never follows symbolic links.
- **What counts as a declaration.** A document matches only if its apiVersion, kind, `metadata.namespace` and `metadata.name` all equal the live target's. A document with the same group, kind and name but another version or no namespace could also produce the target, so it makes the match ambiguous. Multi-document files are supported; duplicate keys, invalid YAML and `List` objects stop the mapping.
- **Kustomization.** It must not be suspended, its status must describe its current generation, and spec fields that change what is built (`patches`, `images`, `components`, `namePrefix`, `nameSuffix`, `targetNamespace`, `postBuild`) stop with `NeedsIntervention`. The inventory entry must match namespace, name, group, kind and version.
- **Target.** It must be pinned by UID and have a kind the action may change (a `keda.sh` ScaledObject, or an `apps/v1` Deployment or StatefulSet); both are checked before anything is read.
- **GitRepository.** `spec.ref` must set only `branch` (Flux gives `tag`, `semver`, `name` and `commit` precedence over it). `suspend`, `ignore`, `include`, `sparseCheckout` and `recurseSubmodules` must be unset, since each can make the artifact differ from the branch head. The Kustomization's `status.lastAppliedRevision` must equal the GitRepository's `status.artifact.revision`, so the live object comes from the latest fetched commit. `spec.sourceRef` must be a valid object reference. The URL is compared byte for byte, so a `.git` suffix, a trailing slash, other letter case or an SSH URL do not match a settings entry. URLs are never written to messages, and provider errors are replaced by a transient error without their text, because a URL may carry credentials.
- **Value check.** The value is compared as JSON, so `10` and `"10"` differ; memory limits are compared as quantities, so `256Mi` equals `268435456`.
- **Outcomes.** `Denied`: the inventory does not list the object (the labels point at a Kustomization that does not deliver it), the URL matches no settings repository or more than one, the repository is not the policy's `repositoryRef`, the branch is not its `targetBranch`, or the file matches no `allowedPaths`. Everything else that stops is `NeedsIntervention`. Each result carries a reason (for example `NotInInventory`, `SourceNotBranch`, `AmbiguousDeclaration`, `ValueMismatch`) for the request's conditions.

## Consequences

- The supported case is unambiguous and easy to test.
- Many real repositories use overlays or charts; they get diagnoses and issues, not PRs, until support is added.
- A request author (or a prompt-injected analysis) cannot aim a change at a file, repository or branch other than the one that actually delivers the target.

## Alternatives considered

- Require a `shadewright.io/source` annotation on every object: precise, but a burden on users and itself untrusted input.
- Render everything and diff back to the source: correct in general, much more work.
