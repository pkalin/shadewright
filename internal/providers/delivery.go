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

package providers

import (
	"context"
	"errors"
	"fmt"
)

// DeliveryUnit names the object of a delivery system that applies a source path to the cluster, for
// example a Flux Kustomization or HelmRelease.
type DeliveryUnit struct {
	Kind      string
	Namespace string
	Name      string
}

// DeliveryStatus is what a delivery system reports about one unit. Every field is read from the unit's own
// status, which anyone who may write it can forge, so [DeliveryStatus.Outcome] only says Applied when all of
// them agree.
type DeliveryStatus struct {
	// AppliedRevision is the source revision the unit last applied successfully, as the delivery system writes
	// it.
	AppliedRevision string
	// AppliedCommit is the Git commit ID that AppliedRevision names, or empty when it names none: a revision
	// of another format, of a source that is not a Git repository, or a chart version.
	AppliedCommit string
	// AppliedRef is the branch or tag AppliedRevision names, or empty when it names only a commit.
	AppliedRef string
	// AttemptedCommit is the Git commit ID of the revision the unit last tried to apply, or empty as for
	// AppliedCommit.
	AttemptedCommit string
	// AttemptedRef is the branch or tag of that revision, or empty as for AppliedRef.
	AttemptedRef string
	// Ready is true when the unit reports itself healthy for its current generation.
	Ready bool
	// Failed is true when the unit reports that its last attempt failed or that it stopped retrying, in a
	// condition that is not about an older generation. A delivery system may still retry a failed attempt.
	Failed bool
	// Generation is the unit's metadata.generation; 0 when it has none.
	Generation int64
	// ObservedGeneration is the generation the unit's status describes; 0 when it says none.
	ObservedGeneration int64
	// Message is the delivery system's own summary; it is untrusted text.
	Message string
	// Suspended is true when the unit's spec says the delivery system does not reconcile it (Flux's
	// spec.suspend), so it applies no new revision. [DeliveryStatus.Outcome] does not read it.
	Suspended bool
	// Source is what a comparison of the unit with its source found, or [SourceNotCompared] when the reader
	// did not compare: the executor's verifier never does (it compares with a merge commit instead), and the
	// analyzer's Flux status reader does only for a Kustomization of a GitRepository that would otherwise
	// count as applied. [DeliveryStatus.Outcome] does not read it.
	Source SourceComparison
}

// SourceComparison is what a comparison of a delivery unit with its source found.
type SourceComparison string

// Values of [DeliveryStatus.Source].
const (
	// SourceNotCompared: the reader did not compare the unit with its source.
	SourceNotCompared SourceComparison = ""
	// SourceAtRevision: the unit applied the revision its source last fetched.
	SourceAtRevision SourceComparison = "AtRevision"
	// SourceBehind: the unit has not applied the revision its source last fetched.
	SourceBehind SourceComparison = "Behind"
	// SourceUnchecked: the source could not be compared with, since it is refused by the rules a write
	// follows (suspended, for example) or not Ready for its current generation. Whether the unit is behind it
	// is unknown, so the unit never counts as applied by its source.
	SourceUnchecked SourceComparison = "Unchecked"
)

// DeliveryOutcome is what a unit's status says about one commit.
type DeliveryOutcome string

// Outcomes of [DeliveryStatus.Outcome].
const (
	// DeliveryPending: the commit is not applied yet, or the status cannot say whether it is.
	DeliveryPending DeliveryOutcome = "Pending"
	// DeliveryApplied: the unit applied the commit, or a later one of the branch that holds it, and is Ready
	// for its current generation.
	DeliveryApplied DeliveryOutcome = "Applied"
	// DeliveryFailed: the unit's last attempt was the commit, or a later one of the branch that holds it, and
	// it failed.
	DeliveryFailed DeliveryOutcome = "Failed"
)

// Errors of [DeliveryStatus.Outcome], [DeliveryStatus.Verify] and [AncestryChecker]. Both are permanent: the
// same arguments fail the same way, so a caller ends the check instead of waiting.
var (
	// ErrInvalidCommit means a commit to compare is not a full Git object ID, or not one the host can compare
	// (a SHA-256 ID on a SHA-1 host).
	ErrInvalidCommit = errors.New("invalid commit ID")
	// ErrInvalidBranch means no target branch was given to compare a revision's branch with.
	ErrInvalidBranch = errors.New("invalid target branch")
)

// Current reports whether the status describes the unit's current spec: the unit has a generation and its
// status observed that one.
func (s DeliveryStatus) Current() bool {
	return s.Generation > 0 && s.ObservedGeneration == s.Generation
}

// onBranch reports whether a revision's ref is the target branch. A revision that names only a commit has no
// ref and is accepted: its commit is still compared.
func onBranch(ref, branch string) bool {
	return ref == "" || ref == branch
}

// appliedReady reports whether the unit is Ready, Current, reports no failure and applied a revision of branch.
func (s DeliveryStatus) appliedReady(branch string) bool {
	return !s.Failed && s.Ready && s.Current() && onBranch(s.AppliedRef, branch)
}

// Outcome compares the status with commit, a full Git object ID such as a pull request's merge commit, merged
// into branch. It is Applied only when the unit is Ready, Current, reports no failure and applied exactly
// that commit from branch (or from a revision naming only the commit). It is Failed when the unit reports a
// failure and its last attempt was that commit from branch, and Pending otherwise; [DeliveryStatus.Verify]
// also counts later commits. A commit that is not a full object ID gives ErrInvalidCommit, an empty branch
// ErrInvalidBranch, and never an outcome.
func (s DeliveryStatus) Outcome(commit, branch string) (DeliveryOutcome, error) {
	if !IsObjectID(commit) {
		return "", ErrInvalidCommit
	}
	if branch == "" {
		return "", ErrInvalidBranch
	}
	switch {
	case s.Failed && onBranch(s.AttemptedRef, branch) && s.AttemptedCommit == commit:
		return DeliveryFailed, nil
	case s.appliedReady(branch) && s.AppliedCommit == commit:
		return DeliveryApplied, nil
	}
	return DeliveryPending, nil
}

// Verify is [DeliveryStatus.Outcome] for a target branch that moves on: a commit of branch merged after
// commit carries commit too. So when the unit applied such a later commit and is Ready, Current and reports
// no failure, the outcome is Applied; when its failing last attempt was such a commit, it is Failed. Whether
// commit is in a later commit's history is asked of ancestry, only in those two cases; equal commits need no
// call. When ancestry fails, the outcome is Pending and the error is returned with it, never Applied; an
// error that wraps ErrInvalidCommit is permanent. Whether the later commit still holds the change (or
// reverted it) is not delivery's concern: the verification checks after delivery read the value.
func (s DeliveryStatus) Verify(ctx context.Context, commit, branch string, ancestry AncestryChecker) (DeliveryOutcome, error) {
	outcome, err := s.Outcome(commit, branch)
	if err != nil || outcome != DeliveryPending || ancestry == nil {
		return outcome, err
	}
	var (
		later string
		found DeliveryOutcome
	)
	switch {
	case s.Failed && onBranch(s.AttemptedRef, branch) && IsObjectID(s.AttemptedCommit):
		later, found = s.AttemptedCommit, DeliveryFailed
	case s.appliedReady(branch) && IsObjectID(s.AppliedCommit):
		later, found = s.AppliedCommit, DeliveryApplied
	default:
		return DeliveryPending, nil
	}
	if len(later) != len(commit) {
		// A SHA-1 and a SHA-256 ID are of different repositories' formats: no history can relate them.
		return DeliveryPending, fmt.Errorf("compare the delivered commit with the merge commit: %w", ErrInvalidCommit)
	}
	descends, err := ancestry.IsAncestor(ctx, commit, later)
	if err != nil {
		return DeliveryPending, fmt.Errorf("check that the delivered commit descends from the merge commit: %w", err)
	}
	if descends {
		return found, nil
	}
	return DeliveryPending, nil
}

// AncestryChecker answers whether one commit is in the history of another. A Git host can implement it.
type AncestryChecker interface {
	// IsAncestor reports whether ancestor is commit itself or one of its ancestors. Both are full object IDs.
	// ErrNotFound when the host does not know one of them; ErrInvalidCommit (permanent) when one is not an ID
	// the host can compare.
	IsAncestor(ctx context.Context, ancestor, commit string) (bool, error)
}

// DeliveryVerifier reports whether a delivery system has applied a revision.
type DeliveryVerifier interface {
	// Status returns the current status of a unit. ErrNotFound when the unit does not exist.
	Status(ctx context.Context, unit DeliveryUnit) (DeliveryStatus, error)
}
