/*
Copyright 2026 The Shadewright Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package flux is the Flux cluster integration module (ADR-0018, design 2.5 and 7): failing Kustomizations,
// HelmReleases and GitRepositories become symptoms, with the error and the revision as evidence.
//
// Detection: the cluster serves kustomize.toolkit.fluxcd.io/v1, helm.toolkit.fluxcd.io/v2 or
// source.toolkit.fluxcd.io/v1, the GA versions of Flux 2.3 and later. Older beta versions are not read.
//
// Rules, at most one symptom per object, read only from the object itself:
//
//   - The object must have the watched apiVersion and kind. A suspended object (spec.suspend) shows nothing:
//     its status describes the past, and suspending is a person's choice.
//   - Stalled=True is reported first: Flux has stopped retrying. Otherwise the first Ready=False is reported.
//     Ready=True, Ready=Unknown (still reconciling) and no Ready condition show nothing.
//   - A condition whose observedGeneration is set and differs from metadata.generation describes an older spec
//     and shows nothing until Flux catches up.
//   - Ready=False with reason DependencyNotReady shows nothing: the dependency is the failing object, and it
//     has a symptom of its own.
//   - The symptom's reason and summary are written here. The reason is Stalled for Stalled=True, the Flux reason
//     when it is one this package knows for the kind (BuildFailed, ReconciliationFailed, UpgradeFailed,
//     GitOperationFailed and so on), and NotReady otherwise. Cluster text never goes into either.
//   - The evidence is the condition's reason and message, status.lastAttemptedRevision (Kustomization,
//     HelmRelease), and the revision still in use when it differs from that one: status.lastAppliedRevision
//     (Kustomization) or the chart version of the newest deployed release in status.history (HelmRelease), in
//     which case the summary says the cluster still runs an older revision than Git, which is how drift from
//     Git shows. A GitRepository gives status.artifact.revision, the artifact it still serves. Evidence is
//     untrusted: the evaluator redacts and bounds it, this package copies it as it is.
//
// The module has no actions and no tools: Flux is the delivery path, so a fix is a pull request to the
// manifest, values or Kustomization in Git, never a change to a Flux object (design 7). Nothing here writes to
// the cluster or holds a client; the caller reads the objects with read-only access and passes their content.
//
// This package imports only the standard library and this module (ADR-0013): Flux objects are read as plain
// JSON maps, so no Flux API package is needed.
package flux
