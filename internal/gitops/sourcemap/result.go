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
	// ReasonKustomizationNotReady means the Kustomization is not Ready for its current generation and fails
	// (NeedsIntervention).
	ReasonKustomizationNotReady Reason = "KustomizationNotReady"
	// ReasonKustomizationReconciling means the Kustomization is not Ready for its current generation because
	// Flux is still reconciling it, for example applying a revision just merged, and reports no failure
	// (fluxstatus.Reconciling). The outcome is NeedsIntervention, so a caller that does not know it fails
	// closed; the policy check waits on it for a bounded time and then ends the request with
	// ReasonKustomizationNotReady, and the freshness check retries it (#556).
	ReasonKustomizationReconciling Reason = "KustomizationReconciling"
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
	// ReasonRepositoryUnusable means the matching repository's settings cannot be used to open a Git provider
	// (NeedsIntervention). The schema refuses what the adapter refuses, so this is the second layer: it catches an
	// entry stored before a schema rule existed.
	ReasonRepositoryUnusable Reason = "RepositoryUnusable"
	// ReasonBranchMismatch means the branch Flux tracks is not the repository's targetBranch (Denied).
	ReasonBranchMismatch Reason = "BranchMismatch"
	// ReasonBranchNotFound means the target branch does not exist in the repository (NeedsIntervention).
	ReasonBranchNotFound Reason = "BranchNotFound"
	// ReasonInvalidPath means a path in the Kustomization or a kustomization file, or a manifest or base that
	// the kustomization Flux generates for a path without one would hold, is not a plain relative path inside
	// the repository, or a listing holds an entry no tree can hold (NeedsIntervention).
	ReasonInvalidPath Reason = "InvalidPath"
	// ReasonNoKustomizationFile means a directory a kustomization file lists as a resource has no kustomization
	// file, as kustomize would refuse, or the Kustomization's path is not a directory in the tree
	// (NeedsIntervention). A path without one is listed and searched as Flux would generate its kustomization.
	ReasonNoKustomizationFile Reason = "NoKustomizationFile"
	// ReasonUnsupportedKustomization means a kustomization file uses more than plain resources: patches,
	// generators, components, transformers, a namespace or name prefix, or remote resources
	// (NeedsIntervention).
	ReasonUnsupportedKustomization Reason = "UnsupportedKustomization"
	// ReasonFileNotReadable means a file the build needs is missing, too large or not valid YAML
	// (NeedsIntervention).
	ReasonFileNotReadable Reason = "FileNotReadable"
	// ReasonTooManyFiles means the build is deeper or wider than the mapping reads: too many nested
	// directories (MaxDepth, or MaxListDepth below a path without a kustomization file), manifests, file reads
	// and listings, or a directory over providers.MaxDirEntries entries (NeedsIntervention).
	ReasonTooManyFiles Reason = "TooManyFiles"
	// ReasonInputTooLarge means the build holds more than the mapping reads in all: more than MaxTotalBytes
	// of files or MaxDocuments YAML documents, or a file of more than gityaml.MaxNodes YAML nodes or with a
	// mapping of more than gityaml.MaxMappingKeys keys (NeedsIntervention). A file over what is left of the
	// byte budget is refused before it is downloaded or cached, even when it is also over MaxFileBytes, so a
	// cold and a warm cache give the same reason.
	ReasonInputTooLarge Reason = "InputTooLarge"
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

// Reconciling reports whether the reason says the mapping stopped only because Flux is still reconciling the
// Kustomization, with no failure reported: a wait, not a verdict (#556).
func (r Reason) Reconciling() bool {
	return r == ReasonKustomizationReconciling
}

// Result is the outcome of a mapping. Message explains it for humans; it may quote object names and file
// paths, never file contents or URLs.
type Result struct {
	Outcome Outcome
	Reason  Reason
	Message string
	// Source is set only when Outcome is Resolved.
	Source *v1alpha1.ResolvedSource
	// Observed lists the Flux objects (Kustomization, GitRepository) the mapping read, as it read them, for
	// every outcome, an object found missing included; the mapping has read them again after its last Git read. A
	// caller that does slow work after the mapping, or acts on a stop, reads them again and compares them.
	Observed []Observed
	// Scanned is the repository, branch and commit whose files a stop was found in, for a stop reached after
	// the mapping matched the repository; nil otherwise. For Resolved it is nil: Source names the tree. Use
	// [Result.Tree], which covers both.
	Scanned *Scanned
}

// Tree returns the tree the mapping read: Source's repository, branch and base commit when Resolved, Scanned
// for a stop. Nil when the mapping stopped before it matched a repository. A caller that acts on the result
// compares the commit with the branch head, since a stop found in an older tree may not hold at the head.
func (r Result) Tree() *Scanned {
	if r.Outcome == Resolved && r.Source != nil {
		return &Scanned{RepositoryRef: r.Source.RepositoryRef, Branch: r.Source.TargetBranch, Commit: r.Source.BaseCommit}
	}
	return r.Scanned
}

// Scanned names the tree a mapping read: the settings repository, the branch and the commit at its head. Commit
// is empty when the branch did not exist (ReasonBranchNotFound).
type Scanned struct {
	RepositoryRef v1alpha1.Name
	Branch        v1alpha1.GitBranch
	Commit        string
}

// Observed is a Flux object as the mapping read it: its identity and version, or that it was missing (or its
// kind was not served), in which case its absence is what a caller checks again.
type Observed struct {
	GVK                  schema.GroupVersionKind
	Namespace, Name      string
	UID, ResourceVersion string
	// Missing is true when the mapping found no such object; UID and ResourceVersion are then empty.
	Missing bool
}

func needsIntervention(reason Reason, format string, args ...any) Result {
	return Result{Outcome: NeedsIntervention, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

func denied(reason Reason, format string, args ...any) Result {
	return Result{Outcome: Denied, Reason: reason, Message: fmt.Sprintf(format, args...)}
}
