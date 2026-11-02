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
// Package sourcemap maps a live Kubernetes object to the one file in Git that declares it, as ADR-0012
// decides, or stops with a typed reason.
//
// The mapping only supports plain manifests delivered by a Flux Kustomization from a GitRepository that tracks
// a branch:
//
//  0. The target must be pinned by UID and have a kind the action may change ([FieldFor]); this is checked
//     before anything is read.
//  1. The Flux labels on the live object (kustomize.toolkit.fluxcd.io/name and /namespace) name its
//     Kustomization. The labels are untrusted, so the Kustomization must be Ready for its current generation
//     and list the object in status.inventory.
//  2. The Kustomization's source must be a GitRepository that tracks only spec.ref.branch, with no
//     suspend, ignore, include, sparse checkout or submodules. It must be Ready for its current generation,
//     its artifact must come from that branch, and the Kustomization must have applied that revision. Its URL must equal,
//     byte for byte, exactly one repository in RemediationSettings, that repository must be the one the
//     policy names, and the branch must equal the repository's targetBranch.
//  3. The files the Kustomization builds are read through a [providers.GitProvider] at the head of that
//     branch, starting from the kustomization file in spec.path and following its resources. Exactly one
//     YAML document in them must declare the object (apiVersion, kind, namespace, name), the value the action
//     changes must equal the live value, and the file must match the policy's allowedPaths.
//     A .sourceignore file on the searched paths stops the mapping, since Flux would filter by it.
//
// Anything else (no labels, Helm, a tag or commit ref, Kustomize patches or generators, a directory without a
// kustomization file, two declarations, a value changed after the build) stops with [NeedsIntervention] or
// [Denied] and a [Reason]. [Resolver.Resolve] returns an error only for failures worth retrying, such as an
// unreachable API server or Git host.
//
// Git is read only through ReadFile and BranchHead, relying on the ReadFile contract: only regular files
// are returned and symbolic links are never followed. Reads are bounded by [MaxDepth], [MaxFiles],
// [MaxReads] and [MaxFileBytes], whatever cap the provider enforces itself. Errors carry no provider text
// ([TransientError]), since it may hold a repository URL with credentials.
//
// Everything read here is untrusted input: cluster objects, Flux status and Git contents. Paths are built
// only from validated segments with package path, never by concatenating strings, and stay inside the
// repository. Flux source URLs are compared exactly and never copied into messages, since they may carry
// credentials. Flux objects are read as unstructured objects through a read-only controller-runtime client,
// so the module does not depend on the Flux API packages.
package sourcemap
