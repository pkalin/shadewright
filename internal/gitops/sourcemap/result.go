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

import "github.com/pkalin/shadewright/internal/gitops/sourcemap/fluxrules"

// The result types and reasons live in [fluxrules], which imports no Kubernetes client; the names below are
// aliases, so callers of this package do not change.
type (
	// Outcome is [fluxrules.Outcome].
	Outcome = fluxrules.Outcome
	// Reason is [fluxrules.Reason].
	Reason = fluxrules.Reason
	// Result is [fluxrules.Result].
	Result = fluxrules.Result
	// Scanned is [fluxrules.Scanned].
	Scanned = fluxrules.Scanned
	// Observed is [fluxrules.Observed].
	Observed = fluxrules.Observed
)

// Outcomes of a mapping, documented in [fluxrules].
const (
	Resolved          = fluxrules.Resolved
	NeedsIntervention = fluxrules.NeedsIntervention
	Denied            = fluxrules.Denied
)

// Reasons of a mapping result, documented in [fluxrules].
const (
	ReasonResolved                 = fluxrules.ReasonResolved
	ReasonInvalidInput             = fluxrules.ReasonInvalidInput
	ReasonTargetNotFound           = fluxrules.ReasonTargetNotFound
	ReasonTargetChanged            = fluxrules.ReasonTargetChanged
	ReasonHelmManaged              = fluxrules.ReasonHelmManaged
	ReasonNoFluxLabels             = fluxrules.ReasonNoFluxLabels
	ReasonInvalidFluxLabels        = fluxrules.ReasonInvalidFluxLabels
	ReasonKustomizationNotFound    = fluxrules.ReasonKustomizationNotFound
	ReasonKustomizationSuspended   = fluxrules.ReasonKustomizationSuspended
	ReasonKustomizationNotReady    = fluxrules.ReasonKustomizationNotReady
	ReasonKustomizationReconciling = fluxrules.ReasonKustomizationReconciling
	ReasonNotInInventory           = fluxrules.ReasonNotInInventory
	ReasonKustomizationTransforms  = fluxrules.ReasonKustomizationTransforms
	ReasonRemoteCluster            = fluxrules.ReasonRemoteCluster
	ReasonDecryption               = fluxrules.ReasonDecryption
	ReasonUnsupportedSource        = fluxrules.ReasonUnsupportedSource
	ReasonSourceNotFound           = fluxrules.ReasonSourceNotFound
	ReasonSourceNotBranch          = fluxrules.ReasonSourceNotBranch
	ReasonSourceIncludes           = fluxrules.ReasonSourceIncludes
	ReasonSourceOptions            = fluxrules.ReasonSourceOptions
	ReasonSourceNotReady           = fluxrules.ReasonSourceNotReady
	ReasonRevisionMismatch         = fluxrules.ReasonRevisionMismatch
	ReasonRepositoryNotConfigured  = fluxrules.ReasonRepositoryNotConfigured
	ReasonRepositoryNotInPolicy    = fluxrules.ReasonRepositoryNotInPolicy
	ReasonRepositoryUnusable       = fluxrules.ReasonRepositoryUnusable
	ReasonBranchMismatch           = fluxrules.ReasonBranchMismatch
	ReasonBranchNotFound           = fluxrules.ReasonBranchNotFound
	ReasonInvalidPath              = fluxrules.ReasonInvalidPath
	ReasonNoKustomizationFile      = fluxrules.ReasonNoKustomizationFile
	ReasonUnsupportedKustomization = fluxrules.ReasonUnsupportedKustomization
	ReasonFileNotReadable          = fluxrules.ReasonFileNotReadable
	ReasonTooManyFiles             = fluxrules.ReasonTooManyFiles
	ReasonInputTooLarge            = fluxrules.ReasonInputTooLarge
	ReasonDeclarationNotFound      = fluxrules.ReasonDeclarationNotFound
	ReasonAmbiguousDeclaration     = fluxrules.ReasonAmbiguousDeclaration
	ReasonFieldNotFound            = fluxrules.ReasonFieldNotFound
	ReasonValueMismatch            = fluxrules.ReasonValueMismatch
	ReasonPathNotAllowed           = fluxrules.ReasonPathNotAllowed
)

func needsIntervention(reason Reason, format string, args ...any) Result {
	return fluxrules.NewNeedsIntervention(reason, format, args...)
}

func denied(reason Reason, format string, args ...any) Result {
	return fluxrules.NewDenied(reason, format, args...)
}
