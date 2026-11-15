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

package fluxrules

import (
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The helpers below are the parts of the rules the mapping in internal/gitops/sourcemap combines with its
// reads of the cluster and of Git. They are exported for that package only.

// CheckKustomization applies the Kustomization rules of ADR-0012 step 2 for a target object, the
// status.inventory rule included, and returns the source namespace and name and the path, or a stop result.
func CheckKustomization(ks *unstructured.Unstructured, target schema.GroupVersionKind, ns, name string) (
	srcNamespace, srcName, path string, stop *Result) {
	return checkKustomization(ks, target, ns, name)
}

// KustomizationRef names a Kustomization in a message: "Kustomization <namespace>/<name>".
func KustomizationRef(ks *unstructured.Unstructured) string { return kustomizationRef(ks) }

// KustomizationSuspended returns the stop for a suspended Kustomization, or nil.
func KustomizationSuspended(ks *unstructured.Unstructured) *Result { return kustomizationSuspended(ks) }

// KustomizationRemote returns the stop for a Kustomization with spec.kubeConfig, or nil.
func KustomizationRemote(ks *unstructured.Unstructured) *Result { return kustomizationRemote(ks) }

// KustomizationDecrypts reports whether the Kustomization sets spec.decryption.
func KustomizationDecrypts(ks *unstructured.Unstructured) bool { return kustomizationDecrypts(ks) }

// InInventory reports whether status.inventory of ks lists the object.
func InInventory(ks *unstructured.Unstructured, target schema.GroupVersionKind, ns, name string) bool {
	return inInventory(ks, target, ns, name)
}

// GitRefBranch returns the branch spec.ref of a GitRepository names, or a SourceNotBranch stop.
func GitRefBranch(src *unstructured.Unstructured, ref string) (string, *Result) {
	return gitRefBranch(src, ref)
}

// CheckRevision requires the GitRepository to be Ready and the Kustomization to have applied the revision it
// last fetched from branch, or returns the stop.
func CheckRevision(ks, src *unstructured.Unstructured, branch string) *Result {
	return checkRevision(ks, src, branch)
}

// RevisionOfBranch reports whether a Flux artifact revision was fetched from branch.
func RevisionOfBranch(revision, branch string) bool { return revisionOfBranch(revision, branch) }

// KustomizationTransforms returns the Kustomization spec fields that transform the build.
func KustomizationTransforms() []string { return slices.Clone(kustomizationTransforms) }

// GitRepositoryOptions returns the GitRepository spec fields that change which files the artifact holds.
func GitRepositoryOptions() []string { return slices.Clone(gitRepositoryOptions) }

// Field returns the value at a path of keys, without copying it.
func Field(obj map[string]any, keys ...string) (any, bool) { return field(obj, keys...) }

// Empty reports whether a decoded value is absent in effect: nil, false, an empty string, list or map.
func Empty(v any) bool { return empty(v) }
