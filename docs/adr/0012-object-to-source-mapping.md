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

## Consequences

- The supported case is unambiguous and easy to test.
- Many real repositories use overlays or charts; they get diagnoses and issues, not PRs, until support is added.
- A request author (or a prompt-injected analysis) cannot aim a change at a file, repository or branch other than the one that actually delivers the target.

## Alternatives considered

- Require a `shadewright.io/source` annotation on every object: precise, but a burden on users and itself untrusted input.
- Render everything and diff back to the source: correct in general, much more work.
