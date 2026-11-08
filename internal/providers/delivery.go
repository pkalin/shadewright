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
	// AttemptedCommit is the Git commit ID of the revision the unit last tried to apply, or empty as for
	// AppliedCommit.
	AttemptedCommit string
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
}

// DeliveryOutcome is what a unit's status says about one commit.
type DeliveryOutcome string

// Outcomes of [DeliveryStatus.Outcome].
const (
	// DeliveryPending: the commit is not applied yet, or the status cannot say whether it is.
	DeliveryPending DeliveryOutcome = "Pending"
	// DeliveryApplied: the unit applied exactly the commit and is Ready for its current generation.
	DeliveryApplied DeliveryOutcome = "Applied"
	// DeliveryFailed: the unit's last attempt was the commit, and it failed.
	DeliveryFailed DeliveryOutcome = "Failed"
)

// ErrInvalidCommit means a commit to compare is not a full Git object ID.
var ErrInvalidCommit = errors.New("invalid commit ID")

// Current reports whether the status describes the unit's current spec: the unit has a generation and its
// status observed that one.
func (s DeliveryStatus) Current() bool {
	return s.Generation > 0 && s.ObservedGeneration == s.Generation
}

// Outcome compares the status with commit, a full Git object ID such as a pull request's merge commit. It is
// Applied only when the unit is Ready, Current, reports no failure and applied exactly that commit: a later
// commit applied over it is not this one, so it stays Pending. It is Failed when the unit reports a failure
// and its last attempt was that commit, and Pending otherwise. A commit that is not a full object ID gives
// ErrInvalidCommit and never an outcome.
func (s DeliveryStatus) Outcome(commit string) (DeliveryOutcome, error) {
	if !IsObjectID(commit) {
		return "", ErrInvalidCommit
	}
	switch {
	case s.Failed && s.AttemptedCommit == commit:
		return DeliveryFailed, nil
	case !s.Failed && s.Ready && s.Current() && s.AppliedCommit == commit:
		return DeliveryApplied, nil
	}
	return DeliveryPending, nil
}

// Verify is [DeliveryStatus.Outcome] for a target branch that moves on: a commit merged after commit and
// applied over it carries commit too, so it counts as Applied when ancestry says commit is an ancestor of
// the applied one. Equal commits need no call to ancestry. Ancestry is asked only when the unit is Ready,
// Current, reports no failure and applied another commit. When ancestry fails, the outcome is Pending and
// the error is returned with it, never Applied.
func (s DeliveryStatus) Verify(ctx context.Context, commit string, ancestry AncestryChecker) (DeliveryOutcome, error) {
	outcome, err := s.Outcome(commit)
	if err != nil || outcome != DeliveryPending {
		return outcome, err
	}
	if s.Failed || !s.Ready || !s.Current() || !IsObjectID(s.AppliedCommit) || ancestry == nil {
		return DeliveryPending, nil
	}
	descends, err := ancestry.IsAncestor(ctx, commit, s.AppliedCommit)
	if err != nil {
		return DeliveryPending, fmt.Errorf("check that the applied commit descends from the merge commit: %w", err)
	}
	if descends {
		return DeliveryApplied, nil
	}
	return DeliveryPending, nil
}

// AncestryChecker answers whether one commit is in the history of another. A Git host can implement it.
type AncestryChecker interface {
	// IsAncestor reports whether ancestor is commit itself or one of its ancestors. Both are full object IDs.
	// ErrNotFound when the host does not know one of them.
	IsAncestor(ctx context.Context, ancestor, commit string) (bool, error)
}

// DeliveryVerifier reports whether a delivery system has applied a revision.
type DeliveryVerifier interface {
	// Status returns the current status of a unit. ErrNotFound when the unit does not exist.
	Status(ctx context.Context, unit DeliveryUnit) (DeliveryStatus, error)
}
