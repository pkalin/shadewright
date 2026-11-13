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
//     Kustomization. The labels are untrusted, so the Kustomization must be Ready for its current generation,
//     apply to this cluster (no spec.kubeConfig) and list the object in status.inventory. It must not
//     decrypt SOPS files (spec.decryption), which cannot be edited without breaking their MAC.
//  2. The Kustomization's source must be a GitRepository that tracks only spec.ref.branch, with no
//     suspend, ignore, include, sparse checkout or submodules. It must be Ready for its current generation,
//     its artifact must come from that branch, and the Kustomization must have applied that revision. Its URL must equal,
//     byte for byte, exactly one repository in RemediationSettings, that repository must be the one the
//     policy names, and the branch must equal the repository's targetBranch.
//  3. The files the Kustomization builds are read through a [providers.GitProvider] at the head of that
//     branch, starting from the kustomization file in spec.path and following its resources. A spec.path
//     without one is listed as Flux generates its kustomization: every .yaml and .yml file below it, at most
//     [MaxListDepth] directories deep, sorted by name, and every directory below it with its own
//     kustomization file, followed as such. Exactly one
//     YAML document in them must declare the object (apiVersion, kind, namespace, name), the value the action
//     changes must equal the live value, and the file must match the policy's allowedPaths.
//     A .sourceignore file on the searched paths stops the mapping, since Flux would filter by it.
//
// Anything else (no labels, Helm, a tag or commit ref, Kustomize patches or generators, a listed directory
// without a kustomization file, two declarations, a value changed after the build) stops with [NeedsIntervention] or
// [Denied] and a [Reason]. [Resolver.Resolve] returns an error only for failures worth retrying, such as an
// unreachable API server or Git host, and for a Git read the host refuses for good ([RefusedError]).
//
// Git is read only through ReadFile, ListDir and BranchHead, relying on their contract: only regular files
// are returned, symbolic links and submodules are never followed, files over the read's limit are refused
// before they are downloaded, and a listing holds at most providers.MaxDirEntries entries. Every listed name
// that would be in the build must be a plain path segment; names Flux's default exclusions match are skipped,
// as Flux never sees them, and a .sourceignore file in a listed directory stops the mapping. Reads are bounded by [MaxDepth], [MaxFiles], [MaxReads] and [MaxFileBytes], whatever cap
// the provider enforces itself, and in all by [MaxTotalBytes] and [MaxDocuments] decoded. Each read's limit
// is what is left of [MaxTotalBytes], at most [MaxFileBytes], so the byte budget bounds the file contents
// downloaded, cached and parsed; a file over that limit is neither downloaded nor cached. A ListDir call
// counts as one read and [ListEntryBytes] per entry against the budget. The tree listings a provider reads
// inside ReadFile to find a file are bounded per call by the provider and in number by [MaxReads], not by the
// budget. Documents are decoded one at a time and only the declaration is kept. The scan stops at a second
// exact declaration or at the first near match (another version, or no namespace). A [ReadCache], when the
// [Resolver] has one, keeps reads at a full commit ID across mappings, keyed by the whole settings repository
// entry: files, listings, files over [MaxFileBytes], directories over providers.MaxDirEntries and paths not in
// the commit's tree (providers.ErrPathNotInTree),
// never a file refused only for what is left of a budget, a plain not found or a failed read. A kept file over
// what is left of the budget is refused as the provider would refuse it. Branch heads and cluster objects are
// read every time, and cached reads count against [MaxReads] like any other. Errors carry no provider text ([TransientError], [RefusedError]), since it may
// hold a repository URL with credentials.
//
// [FindManifestCandidate] is a separate, read-only lookup for reports, not part of the mapping: given a
// workload, its labels and the live Kustomization, it searches the same files with the same walker and bounds
// at the commit the Kustomization applied (status.lastAppliedRevision, parsed strictly), not at the branch
// head, and returns a [ManifestCandidate] (file and commit) or a [Reason]. It takes no action, compares no
// value, matches no settings repository and checks no policy, and when the Kustomization transforms its build
// it still searches and names the transform fields in the candidate. A candidate never authorizes a change:
// only [Resolver.Resolve] does.
//
// Everything read here is untrusted input: cluster objects, Flux status and Git contents. Paths are built
// only from validated segments with package path, never by concatenating strings, and stay inside the
// repository. Flux source URLs are compared exactly and never copied into messages, since they may carry
// credentials. Flux objects are read as unstructured objects through a read-only controller-runtime client,
// so the module does not depend on the Flux API packages.
package sourcemap
