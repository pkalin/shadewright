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

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/pkalin/shadewright/api/v1alpha1"
)

// Outcome says whether the mapping found the source or why it stopped.
type Outcome string

const (
	// Resolved means the source was found; Result.Source is set.
	Resolved Outcome = "Resolved"
	// NeedsIntervention means the mapping is ambiguous or unsupported; a human must look.
	NeedsIntervention Outcome = "NeedsIntervention"
	// Denied means the change is not allowed: the source is not a repository, branch or file that settings
	// and policy permit, or the labels point at a Kustomization that does not deliver the object.
	Denied Outcome = "Denied"
)

// Reason is a machine-readable cause, in the CamelCase form of a condition reason.
type Reason string

// Reasons for a result. Each stop condition of ADR-0012 has its own reason.
const (
	// ReasonResolved goes with the Resolved outcome.
	ReasonResolved Reason = "SourceResolved"

	// ReasonInvalidInput means the target reference is not usable: not namespaced, not pinned by UID, or of a
	// kind the action may not change (NeedsIntervention).
	ReasonInvalidInput Reason = "InvalidInput"
	// ReasonTargetNotFound means the target object does not exist (NeedsIntervention).
	ReasonTargetNotFound Reason = "TargetNotFound"
	// ReasonTargetChanged means the live object's UID differs from the request's (NeedsIntervention).
	ReasonTargetChanged Reason = "TargetChanged"
	// ReasonHelmManaged means a Flux HelmRelease manages the object (NeedsIntervention).
	ReasonHelmManaged Reason = "HelmManaged"
	// ReasonNoFluxLabels means the object has no Flux Kustomization labels (NeedsIntervention).
	ReasonNoFluxLabels Reason = "NoFluxLabels"
	// ReasonInvalidFluxLabels means the labels are not valid object names (NeedsIntervention).
	ReasonInvalidFluxLabels Reason = "InvalidFluxLabels"
	// ReasonKustomizationNotFound means the labelled Kustomization does not exist (NeedsIntervention).
	ReasonKustomizationNotFound Reason = "KustomizationNotFound"
	// ReasonKustomizationSuspended means the Kustomization is suspended, so a merge would not be applied
	// (NeedsIntervention).
	ReasonKustomizationSuspended Reason = "KustomizationSuspended"
	// ReasonKustomizationNotReady means the Kustomization is not Ready for its current generation
	// (NeedsIntervention).
	ReasonKustomizationNotReady Reason = "KustomizationNotReady"
	// ReasonNotInInventory means the Kustomization named by the labels does not list the object in
	// status.inventory, so the labels cannot be trusted (Denied).
	ReasonNotInInventory Reason = "NotInInventory"
	// ReasonKustomizationTransforms means the Kustomization patches, renames, re-namespaces or substitutes
	// what it builds (NeedsIntervention).
	ReasonKustomizationTransforms Reason = "KustomizationTransforms"
	// ReasonRemoteCluster means the Kustomization applies to another cluster through spec.kubeConfig, whose
	// inventory cannot be tied to the target (NeedsIntervention).
	ReasonRemoteCluster Reason = "RemoteCluster"
	// ReasonDecryption means the Kustomization decrypts SOPS files, which the mapping cannot edit without
	// breaking their MAC (NeedsIntervention).
	ReasonDecryption Reason = "Decryption"
	// ReasonUnsupportedSource means the Kustomization's source is not a GitRepository (NeedsIntervention).
	ReasonUnsupportedSource Reason = "UnsupportedSource"
	// ReasonSourceNotFound means the GitRepository does not exist (NeedsIntervention).
	ReasonSourceNotFound Reason = "SourceNotFound"
	// ReasonSourceNotBranch means the GitRepository does not track only spec.ref.branch: no ref, a tag, semver
	// range, ref name or commit (NeedsIntervention).
	ReasonSourceNotBranch Reason = "SourceNotBranch"
	// ReasonSourceIncludes means the GitRepository includes other sources (NeedsIntervention).
	ReasonSourceIncludes Reason = "SourceIncludes"
	// ReasonSourceOptions means the GitRepository is suspended, or ignores, sparsely checks out or recurses
	// into files, or the repository holds a .sourceignore file on the searched paths, so the branch head may
	// not be what Flux applies (NeedsIntervention).
	ReasonSourceOptions Reason = "SourceOptions"
	// ReasonSourceNotReady means the GitRepository is not Ready for its current generation, so its artifact
	// may come from an older URL or branch (NeedsIntervention).
	ReasonSourceNotReady Reason = "SourceNotReady"
	// ReasonRevisionMismatch means the Kustomization's lastAppliedRevision is not the revision its
	// GitRepository last fetched, or that revision is not from the tracked branch (NeedsIntervention).
	ReasonRevisionMismatch Reason = "RevisionMismatch"
	// ReasonRepositoryNotConfigured means the source URL equals no repository in RemediationSettings, or more
	// than one (Denied).
	ReasonRepositoryNotConfigured Reason = "RepositoryNotConfigured"
	// ReasonRepositoryNotInPolicy means the matching repository is not the policy's gitOps.repositoryRef
	// (Denied).
	ReasonRepositoryNotInPolicy Reason = "RepositoryNotInPolicy"
	// ReasonBranchMismatch means the branch Flux tracks is not the repository's targetBranch (Denied).
	ReasonBranchMismatch Reason = "BranchMismatch"
	// ReasonBranchNotFound means the target branch does not exist in the repository (NeedsIntervention).
	ReasonBranchNotFound Reason = "BranchNotFound"
	// ReasonInvalidPath means a path in the Kustomization or a kustomization file is not a plain relative
	// path inside the repository (NeedsIntervention).
	ReasonInvalidPath Reason = "InvalidPath"
	// ReasonNoKustomizationFile means a directory has no kustomization file. Flux would generate one from
	// every manifest in it, which cannot be enumerated through GitProvider reads (NeedsIntervention).
	ReasonNoKustomizationFile Reason = "NoKustomizationFile"
	// ReasonUnsupportedKustomization means a kustomization file uses more than plain resources: patches,
	// generators, components, transformers, a namespace or name prefix, or remote resources
	// (NeedsIntervention).
	ReasonUnsupportedKustomization Reason = "UnsupportedKustomization"
	// ReasonFileNotReadable means a file the build needs is missing, too large or not valid YAML
	// (NeedsIntervention).
	ReasonFileNotReadable Reason = "FileNotReadable"
	// ReasonTooManyFiles means the build is deeper or wider than the mapping reads: too many nested
	// directories, manifests or file reads (NeedsIntervention).
	ReasonTooManyFiles Reason = "TooManyFiles"
	// ReasonDeclarationNotFound means no document declares the object, for example because a generator or
	// patch produces it (NeedsIntervention).
	ReasonDeclarationNotFound Reason = "DeclarationNotFound"
	// ReasonAmbiguousDeclaration means more than one document could declare the object (NeedsIntervention).
	ReasonAmbiguousDeclaration Reason = "AmbiguousDeclaration"
	// ReasonFieldNotFound means the field the action changes is missing from the file or the live object
	// (NeedsIntervention).
	ReasonFieldNotFound Reason = "FieldNotFound"
	// ReasonValueMismatch means the value in the file differs from the live value, so something such as an
	// inline patch changes it (NeedsIntervention).
	ReasonValueMismatch Reason = "ValueMismatch"
	// ReasonPathNotAllowed means the file matches none of the policy's allowedPaths (Denied).
	ReasonPathNotAllowed Reason = "PathNotAllowed"
)

// Result is the outcome of a mapping. Message explains it for humans; it may quote object names and file
// paths, never file contents or URLs.
type Result struct {
	Outcome Outcome
	Reason  Reason
	Message string
	// Source is set only when Outcome is Resolved.
	Source *v1alpha1.ResolvedSource
	// Observed lists the Flux objects (Kustomization, GitRepository) the mapping read, as it read them. It is
	// set only when Outcome is Resolved. A caller that does slow work after the mapping reads them again and
	// compares UID and resource version.
	Observed []Observed
}

// Observed is a Flux object as the mapping read it: its identity and version.
type Observed struct {
	GVK                  schema.GroupVersionKind
	Namespace, Name      string
	UID, ResourceVersion string
}

func needsIntervention(reason Reason, format string, args ...any) Result {
	return Result{Outcome: NeedsIntervention, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

func denied(reason Reason, format string, args ...any) Result {
	return Result{Outcome: Denied, Reason: reason, Message: fmt.Sprintf(format, args...)}
}
