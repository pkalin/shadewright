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
	"context"
	"crypto/sha256"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/pkalin/shadewright/internal/fluxstatus"
	"github.com/pkalin/shadewright/internal/gitops/repopath"
	"github.com/pkalin/shadewright/internal/providers"
)

// CandidateInput is what [FindManifestCandidate] reads: the workload and the live Flux Kustomization its
// labels name, both as read from the cluster and both untrusted.
type CandidateInput struct {
	// Target is the group, version and kind of the workload.
	Target schema.GroupVersionKind
	// Namespace and Name are the workload's.
	Namespace, Name string
	// Labels are the live workload's labels.
	Labels map[string]string
	// Kustomization is the live Flux Kustomization the labels name. Its spec.path, spec.sourceRef,
	// status.inventory, status.lastAppliedRevision and transform fields are read.
	Kustomization *unstructured.Unstructured
}

// ManifestCandidate is the file at the applied commit that holds the one plain document declaring a workload.
// It is a candidate, not a proof: nothing compared it with the live object, no policy checked its path, and
// when Transforms is not empty the build changes it before Flux applies it. It never authorizes a change; only
// [Resolver.Resolve] decides which file a change may edit (ADR-0012).
type ManifestCandidate struct {
	// Path is the file's path in the repository, relative to its root.
	Path string
	// Commit is the full commit ID the file was read at: the one the Kustomization applied.
	Commit string
	// Transforms names the Kustomization spec fields that transform the build (for example "patches" or
	// "postBuild"), in a fixed order; empty when it sets none. When set, the file is where the workload is
	// declared, not what is live.
	Transforms []string
}

// CandidateResult is the outcome of [FindManifestCandidate]: a candidate, or the reason there is none.
type CandidateResult struct {
	// Candidate is set when a candidate was found; Reason and Message are then empty.
	Candidate *ManifestCandidate
	// Reason says why there is no candidate. It reuses the mapping's reasons where they mean the same.
	Reason Reason
	// Message explains Reason for humans. It may quote object names and file paths, never file contents.
	Message string
}

// FindManifestCandidate finds the file that declares a workload at the revision its Kustomization applied,
// for reports that ask where a workload comes from (triage, `doctor`, delivery verification). It is read-only
// and calls git only through ReadFile and ListDir at that commit; git must be the repository the
// Kustomization's GitRepository fetches, which the caller chooses.
//
// It shares the mapping's walker and declaration search, so the same files are searched with the same bounds
// and stops, but it is not the mapping: it takes no action, compares no value, reads no branch head and checks
// no policy, and it searches even when the Kustomization transforms the build, naming the transform fields in
// the candidate. Its result is never authorization to change anything (invariant 1).
//
// The error is non-nil only when a Git read fails, a [*TransientError] matching [ErrTransient] or a
// [*RefusedError]; a failed read is never reported as "not found". Every other way to have no candidate is a
// Reason.
func FindManifestCandidate(ctx context.Context, git providers.GitProvider, in CandidateInput) (CandidateResult, error) {
	id, ks, path, stop := candidateDelivery(in)
	if stop != nil {
		return *stop, nil
	}
	_, commit, ok := fluxstatus.ParseCommitRevision(fluxstatus.AppliedRevision(fluxstatus.Status(ks.Object)))
	if !ok {
		// The mapping stops on such a revision with the same reason, since it never equals a fetched one.
		return noCandidate(ReasonRevisionMismatch,
			"status.lastAppliedRevision of %s does not name a Git commit", kustomizationRef(ks)), nil
	}
	dir := "."
	if path != "" {
		if dir, ok = repopath.Join(".", path); !ok {
			return noCandidate(ReasonInvalidPath,
				"spec.path of %s is not a relative path inside the repository", kustomizationRef(ks)), nil
		}
	}
	// No cache: the read cache is keyed by a settings repository entry, which this lookup does not have.
	w := newWalker(git, commit, nil, [sha256.Size]byte{})
	if stop, err := w.walk(ctx, dir, 0); stop != nil || err != nil {
		return fromStop(stop), err
	}
	decl, none, err := w.findDeclaration(ctx, id)
	if none != nil || err != nil {
		return fromStop(none), err
	}
	return CandidateResult{Candidate: &ManifestCandidate{
		Path: decl.path, Commit: commit, Transforms: kustomizationTransformFields(ks),
	}}, nil
}

// candidateDelivery checks the workload and its Kustomization as ADR-0012 steps 1 and 2 do for what a
// candidate needs: valid names, the Flux labels naming this Kustomization, a local cluster, the workload in its
// inventory and a GitRepository source. Readiness, the GitRepository and the transform fields are not checked.
// It returns the identity, the Kustomization and its spec.path, or the reason there is no candidate.
func candidateDelivery(in CandidateInput) (identity, *unstructured.Unstructured, string, *CandidateResult) {
	id := identity{gvk: in.Target, namespace: in.Namespace, name: in.Name}
	if in.Target.Version == "" || in.Target.Kind == "" ||
		len(validation.IsDNS1123Label(in.Namespace)) > 0 || len(validation.IsDNS1123Subdomain(in.Name)) > 0 {
		return id, nil, "", new(noCandidate(ReasonInvalidInput, "the workload must be a namespaced object with a valid name"))
	}
	if _, ok := in.Labels[LabelHelmReleaseName]; ok {
		return id, nil, "", new(noCandidate(ReasonHelmManaged,
			"%s is installed by a HelmRelease; Helm values are not searched", id))
	}
	ksName, ksNamespace := in.Labels[LabelKustomizationName], in.Labels[LabelKustomizationNamespace]
	if ksName == "" && ksNamespace == "" {
		return id, nil, "", new(noCandidate(ReasonNoFluxLabels, "%s has no Flux Kustomization labels", id))
	}
	if len(validation.IsDNS1123Subdomain(ksName)) > 0 || len(validation.IsDNS1123Label(ksNamespace)) > 0 {
		return id, nil, "", new(noCandidate(ReasonInvalidFluxLabels,
			"the Flux labels on %s are not a valid object reference", id))
	}
	ks := in.Kustomization
	if ks == nil || ks.GetName() != ksName || ks.GetNamespace() != ksNamespace {
		// The names are valid DNS names, so they can be quoted.
		return id, nil, "", new(noCandidate(ReasonInvalidInput,
			"the Kustomization given is not %s/%s, which the labels on %s name", ksNamespace, ksName, id))
	}
	// Checked before the inventory, as the mapping does: a remote cluster's entry could match a local object.
	if r := kustomizationRemote(ks); r != nil {
		return id, nil, "", new(fromStop(r))
	}
	if !inInventory(ks, id.gvk, id.namespace, id.name) {
		return id, nil, "", new(noCandidate(ReasonNotInInventory,
			"%s, named by the labels on %s, does not list it in status.inventory", kustomizationRef(ks), id))
	}
	kind, _, _ := unstructured.NestedString(ks.Object, "spec", "sourceRef", "kind")
	if kind != GitRepositoryGVK.Kind {
		// Another source's revision is not a commit of a Git repository.
		return id, nil, "", new(noCandidate(ReasonUnsupportedSource,
			"the source of %s is not a GitRepository", kustomizationRef(ks)))
	}
	path, found, err := unstructured.NestedString(ks.Object, "spec", "path")
	if err != nil {
		return id, nil, "", new(noCandidate(ReasonInvalidPath, "spec.path of %s is not a string", kustomizationRef(ks)))
	}
	if !found {
		path = ""
	}
	return id, ks, path, nil
}

// kustomizationTransformFields returns the transform fields the Kustomization sets, in the order of
// kustomizationTransforms. The names are the package's own constants, never text from the cluster.
func kustomizationTransformFields(ks *unstructured.Unstructured) []string {
	var set []string
	for _, f := range kustomizationTransforms {
		if v, ok := field(ks.Object, "spec", f); ok && !empty(v) {
			set = append(set, f)
		}
	}
	return set
}

func noCandidate(reason Reason, format string, args ...any) CandidateResult {
	return CandidateResult{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// fromStop keeps the reason and message of a mapping stop, dropping its outcome: a lookup that authorizes
// nothing denies nothing either. A nil stop gives the zero result, for an error.
func fromStop(stop *Result) CandidateResult {
	if stop == nil {
		return CandidateResult{}
	}
	return CandidateResult{Reason: stop.Reason, Message: stop.Message}
}
