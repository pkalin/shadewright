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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/pkalin/shadewright/internal/fluxstatus"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap/fluxrules"
	"github.com/pkalin/shadewright/internal/providers"
)

// The Flux rules of the mapping live in [fluxrules], which imports no Kubernetes client, so
// kubectl-shadewright applies them without linking controller-runtime. The names below are aliases.

// Labels Flux sets on the objects it applies; see [fluxrules].
const (
	// LabelKustomizationName is [fluxrules.LabelKustomizationName].
	LabelKustomizationName = fluxrules.LabelKustomizationName
	// LabelKustomizationNamespace is [fluxrules.LabelKustomizationNamespace].
	LabelKustomizationNamespace = fluxrules.LabelKustomizationNamespace
	// LabelHelmReleaseName is [fluxrules.LabelHelmReleaseName].
	LabelHelmReleaseName = fluxrules.LabelHelmReleaseName
)

// kindKustomization is the kind of both a Flux Kustomization and a kustomization file.
const kindKustomization = fluxstatus.KindKustomization

// The Flux kinds read; see [fluxrules].
var (
	// KustomizationGVK is [fluxrules.KustomizationGVK].
	KustomizationGVK = fluxrules.KustomizationGVK
	// GitRepositoryGVK is [fluxrules.GitRepositoryGVK].
	GitRepositoryGVK = fluxrules.GitRepositoryGVK
)

// kustomizationTransforms and gitRepositoryOptions are [fluxrules.KustomizationTransforms] and
// [fluxrules.GitRepositoryOptions].
var (
	kustomizationTransforms = fluxrules.KustomizationTransforms()
	gitRepositoryOptions    = fluxrules.GitRepositoryOptions()
)

// KustomizationRefusal is [fluxrules.KustomizationRefusal].
func KustomizationRefusal(ks *unstructured.Unstructured) *Result {
	return fluxrules.KustomizationRefusal(ks)
}

// KustomizationSpecRefusal is [fluxrules.KustomizationSpecRefusal].
func KustomizationSpecRefusal(ks *unstructured.Unstructured) *Result {
	return fluxrules.KustomizationSpecRefusal(ks)
}

// Suspended is [fluxrules.Suspended].
func Suspended(obj *unstructured.Unstructured) bool { return fluxrules.Suspended(obj) }

// CompareSource is [fluxrules.CompareSource].
func CompareSource(ks, src *unstructured.Unstructured) providers.SourceComparison {
	return fluxrules.CompareSource(ks, src)
}

// CheckGitRepository is [fluxrules.CheckGitRepository].
func CheckGitRepository(src *unstructured.Unstructured) (url, branch string, stop *Result) {
	return fluxrules.CheckGitRepository(src)
}

func checkKustomization(ks *unstructured.Unstructured, target schema.GroupVersionKind, ns, name string) (
	srcNamespace, srcName, path string, stop *Result) {
	return fluxrules.CheckKustomization(ks, target, ns, name)
}

func kustomizationRef(ks *unstructured.Unstructured) string { return fluxrules.KustomizationRef(ks) }

func kustomizationSuspended(ks *unstructured.Unstructured) *Result {
	return fluxrules.KustomizationSuspended(ks)
}

func kustomizationRemote(ks *unstructured.Unstructured) *Result {
	return fluxrules.KustomizationRemote(ks)
}

func kustomizationDecrypts(ks *unstructured.Unstructured) bool {
	return fluxrules.KustomizationDecrypts(ks)
}

func inInventory(ks *unstructured.Unstructured, target schema.GroupVersionKind, ns, name string) bool {
	return fluxrules.InInventory(ks, target, ns, name)
}

func gitRefBranch(src *unstructured.Unstructured, ref string) (string, *Result) {
	return fluxrules.GitRefBranch(src, ref)
}

func checkRevision(ks, src *unstructured.Unstructured, branch string) *Result {
	return fluxrules.CheckRevision(ks, src, branch)
}

func revisionOfBranch(revision, branch string) bool {
	return fluxrules.RevisionOfBranch(revision, branch)
}

func field(obj map[string]any, keys ...string) (any, bool) { return fluxrules.Field(obj, keys...) }

func empty(v any) bool { return fluxrules.Empty(v) }
