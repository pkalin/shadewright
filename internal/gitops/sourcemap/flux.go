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
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/pkalin/shadewright/internal/fluxstatus"
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
const kindKustomization = fluxstatus.KindKustomization

// The Flux API versions read. Flux objects are read as unstructured objects, so the module does not depend
// on the Flux API packages; only the fields below are used.
var (
	// KustomizationGVK is the Flux Kustomization kind.
	KustomizationGVK = schema.GroupVersionKind{
		Group: fluxstatus.KustomizeGroup, Version: fluxstatus.KustomizeVersion, Kind: kindKustomization}
	// GitRepositoryGVK is the Flux GitRepository kind.
	GitRepositoryGVK = schema.GroupVersionKind{
		Group: fluxstatus.SourceGroup, Version: fluxstatus.SourceVersion, Kind: fluxstatus.KindGitRepository}
)

// kustomizationTransforms are Kustomization spec fields that change what the build produces, so the
// declaration in Git would not be what is live.
var kustomizationTransforms = []string{
	"patches", "patchesStrategicMerge", "patchesJson6902", "images", "components",
	"namePrefix", "nameSuffix", "targetNamespace", "postBuild",
}

// gitRepositoryOptions are GitRepository spec fields that change which files the artifact holds, or stop it
// from following the branch, so the file read from the branch head might not be what Flux applies.
var gitRepositoryOptions = []string{"suspend", "ignore", "recurseSubmodules", "sparseCheckout"}

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
	if !fluxstatus.Ready(ks.Object) {
		r := needsIntervention(ReasonKustomizationNotReady, "%s is not Ready for its current generation", ref)
		return "", "", "", &r
	}
	// A Kustomization with spec.kubeConfig applies to another cluster. Its inventory does not say which
	// cluster an entry is in, so a local object could match a remote entry; check this before the inventory.
	if v, ok := field(ks.Object, "spec", "kubeConfig"); ok && !empty(v) {
		r := needsIntervention(ReasonRemoteCluster, "%s applies to another cluster (spec.kubeConfig)", ref)
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
	// With spec.decryption, files may be SOPS-encrypted, even partly with plaintext fields left in, and
	// editing them would break the SOPS MAC. Encrypted files are not supported.
	if v, ok := field(ks.Object, "spec", "decryption"); ok && !empty(v) {
		r := needsIntervention(ReasonDecryption, "%s decrypts its manifests (spec.decryption)", ref)
		return "", "", "", &r
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
	if len(validation.IsDNS1123Subdomain(srcName)) > 0 || len(validation.IsDNS1123Label(srcNamespace)) > 0 {
		r := needsIntervention(ReasonUnsupportedSource, "spec.sourceRef of %s is not a valid object reference", ref)
		return "", "", "", &r
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
	for _, f := range gitRepositoryOptions {
		if v, ok := field(src.Object, "spec", f); ok && !empty(v) {
			r := needsIntervention(ReasonSourceOptions,
				"%s sets spec.%s, so the branch head may not be what Flux applies", ref, f)
			return "", "", &r
		}
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

// checkRevision requires the Kustomization to have applied the revision its GitRepository last fetched, so
// the object's live state comes from the source as it is now and not from an older or failed fetch.
//
// The GitRepository must also be Ready for its current generation, so its artifact was fetched for its
// current spec (a changed URL or branch raises the generation), and the artifact revision must name the
// branch: Flux writes it as <branch>@sha1:<commit> or <branch>@sha256:<commit>. Flux status records no
// observed URL, so the generation is what ties the artifact to spec.url.
func checkRevision(ks, src *unstructured.Unstructured, branch string) *Result {
	if !fluxstatus.Ready(src.Object) {
		r := needsIntervention(ReasonSourceNotReady, "GitRepository %s/%s is not Ready for its current generation",
			src.GetNamespace(), src.GetName())
		return &r
	}
	if !fluxstatus.AppliedFetched(ks.Object, src.Object, branch) {
		r := needsIntervention(ReasonRevisionMismatch,
			"Kustomization %s/%s has not applied the revision GitRepository %s/%s last fetched",
			ks.GetNamespace(), ks.GetName(), src.GetNamespace(), src.GetName())
		return &r
	}
	return nil
}

// revisionOfBranch reports whether a Flux artifact revision was fetched from branch: the revision is the
// branch, "@", then the commit digest (sha1:<40 hex> or sha256:<64 hex>). See [fluxstatus.RevisionOfBranch].
func revisionOfBranch(revision, branch string) bool {
	return fluxstatus.RevisionOfBranch(revision, branch)
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
