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
//   - When metadata.generation is an integer, conditions whose observedGeneration is set and differs from it
//     describe an older spec and are skipped before anything is chosen. Without a usable generation no
//     condition is skipped for its generation.
//   - Stalled=True is reported first: Flux has stopped retrying. Otherwise the first Ready=False is reported.
//     Ready=True, Ready=Unknown (still reconciling), no Ready condition and HealthCheckCanceled (a new
//     reconciliation replaced the running one) show nothing.
//   - The symptom's reason and summary are written here, never taken from the cluster. The reason is Stalled
//     for Stalled=True, DependencyNotReady for a Kustomization or HelmRelease waiting for a dependency (so a
//     missing, suspended or unwatched dependency does not fail silently), the Flux reason when it is one the
//     controllers set for the kind (see [reasons] for the sources and tags they were read from), and
//     NotReady otherwise.
//   - Evidence is the condition's reason and message and the revisions: for a Kustomization
//     status.lastAttemptedRevision, plus status.lastAppliedRevision when it differs; for a HelmRelease
//     status.lastAttemptedRevision, plus the chart version of the newest deployed release in status.history
//     when it differs without its "+" build suffix, and status.lastAttemptedConfigDigest with that release's
//     configDigest when they differ; for a GitRepository status.artifact.revision, the artifact it still
//     serves. When a Kustomization or HelmRelease comparison differs, the summary says the cluster runs a
//     different revision than its source declares, naming the source kind when it is a Flux one: that is how
//     drift from the source shows.
//   - Evidence is untrusted and copied as it is: the evaluator redacts and bounds it. Symptoms meet
//     [modules.ValidSymptom].
//
// The module has no actions and no tools: Flux is the delivery path, so a fix is a pull request to the
// manifest, values or Kustomization in Git, never a change to a Flux object (design 7). Nothing here writes to
// the cluster or holds a client; the caller reads the objects with read-only access and passes their content.
//
// This package imports only the standard library and this module (ADR-0013): Flux objects are read as plain
// JSON maps, so no Flux API package is needed.
package flux
