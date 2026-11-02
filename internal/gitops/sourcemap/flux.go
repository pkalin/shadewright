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
package sourcemap

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Labels Flux sets on the objects it applies.
const (
	// LabelKustomizationName names the Kustomization that applied an object.
	LabelKustomizationName = "kustomize.toolkit.fluxcd.io/name"
	// LabelKustomizationNamespace is the namespace of that Kustomization.
	LabelKustomizationNamespace = "kustomize.toolkit.fluxcd.io/namespace"
	// LabelHelmReleaseName names the HelmRelease that installed an object.
	LabelHelmReleaseName = "helm.toolkit.fluxcd.io/name"
)

// kindKustomization is the kind of both a Flux Kustomization and a kustomization file.
const kindKustomization = "Kustomization"

// The Flux API versions read. Flux objects are read as unstructured objects, so the module does not depend
// on the Flux API packages; only the fields below are used.
var (
	// KustomizationGVK is the Flux Kustomization kind.
	KustomizationGVK = schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: kindKustomization}
	// GitRepositoryGVK is the Flux GitRepository kind.
	GitRepositoryGVK = schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository"}
)

// kustomizationTransforms are Kustomization spec fields that change what the build produces, so the
// declaration in Git would not be what is live.
var kustomizationTransforms = []string{
	"patches", "patchesStrategicMerge", "patchesJson6902", "images", "components",
	"namePrefix", "nameSuffix", "targetNamespace", "postBuild",
}

// gitRefOthers are spec.ref fields that take precedence over spec.ref.branch in Flux.
var gitRefOthers = []string{"tag", "semver", "name", "commit"}

// inventoryID is the id Flux writes in status.inventory for an object: namespace, name, group and kind joined
// by underscores. It is only compared, never used as a path, ref or URL.
func inventoryID(namespace, name string, gvk schema.GroupVersionKind) string {
	return fmt.Sprintf("%s_%s_%s_%s", namespace, name, gvk.Group, gvk.Kind)
}

// checkKustomization applies the Kustomization rules of ADR-0012 step 2 and returns the source name and
// namespace and the path, or a stop result.
func checkKustomization(ks *unstructured.Unstructured, target schema.GroupVersionKind, ns, name string) (
	srcNamespace, srcName, path string, stop *Result) {
	ref := fmt.Sprintf("Kustomization %s/%s", ks.GetNamespace(), ks.GetName())
	if suspended, _, _ := unstructured.NestedBool(ks.Object, "spec", "suspend"); suspended {
		r := needsIntervention(ReasonKustomizationSuspended, "%s is suspended", ref)
		return "", "", "", &r
	}
	if !ready(ks) {
		r := needsIntervention(ReasonKustomizationNotReady, "%s is not Ready for its current generation", ref)
		return "", "", "", &r
	}
	if !inInventory(ks, target, ns, name) {
		r := denied(ReasonNotInInventory,
			"%s, named by the labels on %s %s/%s, does not list it in status.inventory", ref, target.Kind, ns, name)
		return "", "", "", &r
	}
	for _, f := range kustomizationTransforms {
		if v, ok := field(ks.Object, "spec", f); ok && !empty(v) {
			r := needsIntervention(ReasonKustomizationTransforms,
				"%s sets spec.%s, so the manifests in Git are not what is applied", ref, f)
			return "", "", "", &r
		}
	}
	kind, _, _ := unstructured.NestedString(ks.Object, "spec", "sourceRef", "kind")
	srcName, _, _ = unstructured.NestedString(ks.Object, "spec", "sourceRef", "name")
	srcNamespace, _, _ = unstructured.NestedString(ks.Object, "spec", "sourceRef", "namespace")
	if kind != GitRepositoryGVK.Kind || srcName == "" {
		r := needsIntervention(ReasonUnsupportedSource, "the source of %s is not a GitRepository", ref)
		return "", "", "", &r
	}
	if srcNamespace == "" {
		srcNamespace = ks.GetNamespace()
	}
	path, found, err := unstructured.NestedString(ks.Object, "spec", "path")
	if err != nil {
		r := needsIntervention(ReasonInvalidPath, "spec.path of %s is not a string", ref)
		return "", "", "", &r
	}
	if !found {
		path = ""
	}
	return srcNamespace, srcName, path, nil
}

// ready reports whether the Kustomization's status describes its current generation and says Ready=True.
func ready(ks *unstructured.Unstructured) bool {
	observed, found, err := unstructured.NestedInt64(ks.Object, "status", "observedGeneration")
	if err != nil || !found || observed != ks.GetGeneration() {
		return false
	}
	conds, _, err := unstructured.NestedSlice(ks.Object, "status", "conditions")
	if err != nil {
		return false
	}
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != "Ready" {
			continue
		}
		if g, ok := m["observedGeneration"].(int64); ok && g != ks.GetGeneration() {
			return false
		}
		return m["status"] == "True"
	}
	return false
}

// inInventory reports whether status.inventory lists the object with the target's group, kind and version.
func inInventory(ks *unstructured.Unstructured, target schema.GroupVersionKind, ns, name string) bool {
	entries, _, err := unstructured.NestedSlice(ks.Object, "status", "inventory", "entries")
	if err != nil {
		return false
	}
	want := inventoryID(ns, name, target)
	for _, e := range entries {
		m, ok := e.(map[string]any)
		if ok && m["id"] == want && m["v"] == target.Version {
			return true
		}
	}
	return false
}

// checkGitRepository applies the source rules of ADR-0012 step 2 and returns the URL and branch, or a stop
// result.
func checkGitRepository(src *unstructured.Unstructured) (url, branch string, stop *Result) {
	ref := fmt.Sprintf("GitRepository %s/%s", src.GetNamespace(), src.GetName())
	if inc, ok := field(src.Object, "spec", "include"); ok && !empty(inc) {
		r := needsIntervention(ReasonSourceIncludes, "%s includes other sources", ref)
		return "", "", &r
	}
	gitRef, found, err := unstructured.NestedMap(src.Object, "spec", "ref")
	branch, isString := gitRef["branch"].(string)
	if err != nil || !found || !isString || branch == "" {
		r := needsIntervention(ReasonSourceNotBranch, "%s does not track a branch in spec.ref.branch", ref)
		return "", "", &r
	}
	for _, f := range gitRefOthers {
		if v, ok := gitRef[f]; ok && !empty(v) {
			r := needsIntervention(ReasonSourceNotBranch, "%s sets spec.ref.%s, which takes precedence over the branch", ref, f)
			return "", "", &r
		}
	}
	url, _, err = unstructured.NestedString(src.Object, "spec", "url")
	if err != nil {
		url = ""
	}
	return url, branch, nil
}

// field returns the value at a path of keys, without copying it.
func field(obj map[string]any, keys ...string) (any, bool) {
	v, found, err := unstructured.NestedFieldNoCopy(obj, keys...)
	return v, found && err == nil
}

// empty reports whether a decoded value is absent in effect: nil, false, an empty string, list or map.
func empty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	default:
		return false
	}
}
