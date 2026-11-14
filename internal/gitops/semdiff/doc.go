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
// Package semdiff proves, before a commit, that a patched manifest changes only what the plan says
// (design 8.2, steps 4 and 5).
//
// [Check] renders the Kustomization path twice with the kustomize API, in a child process of the binary
// ([RunRenderProcess]), from an in-memory file system: once from the files at the commit the change builds on and once with the patched file in place. It
// compares the rendered objects by identity (apiVersion, kind, namespace, name) and passes only when they are the same
// set, every object other than the target is unchanged, and the target differs in exactly the action's
// field, from the previous to the desired value. Then it runs basic schema checks on the changed target.
// Because the whole path is rendered, indirect changes are caught too: a base that another object is also
// built from, or a patch in an overlay that overrides the new value.
//
// [Load] reads the files of a build from a [providers.GitProvider]. When the Kustomization path has no
// kustomization file, it lists the path as the source mapping does (internal/gitops/repopath.Generate) and
// adds the kustomization Flux would generate, resources only, to the files, so [Check] renders what Flux
// renders; what Flux's default exclusions match is skipped, as there. It is bounded by [MaxReads], [MaxFiles],
// [MaxFileBytes], [MaxDepth], [MaxVisits], [MaxBuildBytes], [MaxBuildDocuments], [MaxTransformBytes] (what
// patches and transformers can add to the render) and [MaxMergedKeys] (what patches can merge into one mapping);
// [Check] applies the same walk and limits to the
// files it is given, and caps a render at [MaxObjects]. Visits, build bytes and
// documents count with repetition, so a diamond of overlays over a large base is refused before kustomize builds it once per path. A
// .sourceignore file in any directory of the build, or above one, is refused as in the source mapping,
// since Flux would filter the files by it. Path rules are shared with the source mapping
// (internal/gitops/repopath).
//
// The walk, both renders and the comparison of [Check] run in that render process, one at a time, with limits
// on its memory and CPU time: its own memory sampling ([MaxRenderMemory]), the parent's reading of its resident
// memory ([MaxRenderResident]) and operating system limits ([MaxRenderData], a partial guard, and
// [MaxRenderCPUSeconds]). The walk's budgets are estimates of what a render grows to; the process limits bound
// it whatever the estimate misses, and a render that reaches them is refused with ReasonTooLarge instead of
// ending the executor (#614, ADR-0035). The wait for a turn and the render process honour the context.
//
// Rendering is locked down. Every kustomization file is checked against an allow-list before kustomize
// sees it: plain resources, components, patches (inline or from a local file) and the built-in name,
// namespace, label, annotation, image and replica transformers. A JSON patch copy operation is refused in
// every patch, since each copy can double a value past the limits checked before the render. Generators, Helm
// charts, plugins, KRM functions, OpenAPI schemas and every other field are refused, and so is every reference
// kustomize could read as remote (a URL, an SCP-style Git address, a github.com path) or that leaves the
// repository.
// kustomize runs with built-in plugins only and Helm off, on a file system that holds only the build's files,
// so it cannot execute anything, fetch a remote base or read a file from disk.
//
// Everything in the files is untrusted. Errors are an [*Error] with a [Reason]; their text names only
// validated paths and the target, never file contents or kustomize's own messages. A panic inside kustomize
// is turned into a refusal.
package semdiff
