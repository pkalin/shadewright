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

// Package lockverdict holds the verdict a release of the target lock of a RemediationRequest takes (decisions of
// 2026-11-17 and 2026-11-18 on #771): the proof that no pull request of the request can still merge into its target,
// or that the request never held the lock or handed it over. A Verdict is sealed: its field is unexported, so only
// the constructors here build one, each from the facts that prove it, and Release refuses a nil or zero one with
// ErrNoVerdict instead of releasing nothing in silence. Release is the only code that writes status.holdsTarget
// false; the controller's guard test (TestLockClearedInOnePlace) keeps every other writer and every caller outside
// the lock rule out.
package lockverdict

import (
	"errors"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"

	api "github.com/pkalin/shadewright/api/v1alpha1"
)

// ErrNoVerdict is Release's answer to a nil or zero Verdict: nothing proves the release, so the lock is kept.
var ErrNoVerdict = errors.New("no verdict proves the release of the target lock")

// Verdict is the proof a release of the target lock takes. Its zero value proves nothing.
type Verdict struct {
	proof proof
}

// proof is what a Verdict proves.
type proof int

const (
	// none is the zero value: no proof.
	none proof = iota
	// mergeRecorded: the read of the branch found the request's pull request merged, and the status records that
	// merge (with its receipt, or the end that says it brought another change than the plan's).
	mergeRecorded
	// closedOrGone: the read found the recorded pull request closed without a merge, or gone (missing by its number
	// and from the listing of its branch).
	closedOrGone
	// noPullRequest: no pull request is recorded, and the search of the branch by the request's marker found none.
	noPullRequest
	// neverHeld: the request never took the lock (a refusal, a cancel before the decision, a queued request).
	neverHeld
	// handedOver: the request's pull request was taken over by its replacement, which holds the lock.
	handedOver
	// handoffAborted: a replacement whose handoff did not happen, which never had a pull request of its own.
	handoffAborted
	// releasedBeforeField: a request decided before status.holdsTarget existed, whose status alone shows the lock
	// released.
	releasedBeforeField
	// recordedMerge: the status records the merge of the request's pull request (a phase only a recorded merge
	// reaches, Failed with its receipt, or the pull request marked merged), with or without a receipt, and no
	// verification holds the lock: the merge settles the lock without a read of the branch, as on main.
	recordedMerge
	// undeliveredGone: a merge that did not deliver the plan as shown, whose repository is gone from the settings: no
	// read of its branch can start any more.
	undeliveredGone
)

var proofNames = map[proof]string{
	none: "none", mergeRecorded: "merge recorded", closedOrGone: "closed or gone", noPullRequest: "no pull request",
	neverHeld: "never held", handedOver: "handed over", handoffAborted: "handoff aborted",
	releasedBeforeField: "released before the field", recordedMerge: "recorded merge",
	undeliveredGone: "undelivered merge, repository gone",
}

// String names what v proves, for logs.
func (v *Verdict) String() string {
	if v == nil {
		return proofNames[none]
	}
	return proofNames[v.proof]
}

// Found is what a read of the request's branch found of its pull request.
type Found int

const (
	// FoundOther is any answer that proves nothing (open, unknown).
	FoundOther Found = iota
	// FoundMerged: the pull request is merged.
	FoundMerged
	// FoundClosed: the recorded pull request is closed without a merge.
	FoundClosed
	// FoundGone: the recorded pull request is gone, or none carries the request's marker, as confirmed.
	FoundGone
)

// ByRead is the verdict of a read of a stopped holder's pull requests: found what the read of its branch found,
// recorded whether the status records the merge the read found, pullRequest whether a pull request is recorded,
// otherOpen whether an open pull request of the branch may still deliver the change. ok is false when the read
// proves nothing.
func ByRead(found Found, recorded, pullRequest, otherOpen bool) (*Verdict, bool) {
	if otherOpen {
		return nil, false
	}
	switch found {
	case FoundMerged:
		if recorded {
			return &Verdict{proof: mergeRecorded}, true
		}
	case FoundClosed:
		return &Verdict{proof: closedOrGone}, true
	case FoundGone:
		if !pullRequest {
			return &Verdict{proof: noPullRequest}, true
		}
		return &Verdict{proof: closedOrGone}, true
	}
	return nil, false
}

// NeverHeld is the verdict of a request that never took the lock: status.holdsTarget not set, or false.
func NeverHeld(status *api.RemediationRequestStatus) (*Verdict, bool) {
	if status.HoldsTarget != nil && *status.HoldsTarget {
		return nil, false
	}
	return &Verdict{proof: neverHeld}, true
}

// HandedOver is the verdict of a predecessor whose intent to hands its pull request over to the request by (UID
// byUID, holding the lock when byHolds): to is Done and names it.
func HandedOver(to *api.OutgoingHandoff, byUID types.UID, byHolds bool) (*Verdict, bool) {
	if to == nil || to.Phase != api.HandoffDone || to.UID != api.UID(byUID) || !byHolds {
		return nil, false
	}
	return &Verdict{proof: handedOver}, true
}

// HandoffAborted is the verdict of a replacement whose handoff did not happen: it reserved a predecessor and has
// no pull request of its own.
func HandoffAborted(status *api.RemediationRequestStatus) (*Verdict, bool) {
	if status.Handoff == nil || status.Handoff.From == nil || status.PullRequest != nil {
		return nil, false
	}
	return &Verdict{proof: handoffAborted}, true
}

// ReleasedBeforeField is the verdict of a request decided before status.holdsTarget existed whose status shows the
// lock free: derivedHold is false.
func ReleasedBeforeField(status *api.RemediationRequestStatus, derivedHold bool) (*Verdict, bool) {
	if status.HoldsTarget != nil || derivedHold {
		return nil, false
	}
	return &Verdict{proof: releasedBeforeField}, true
}

// RecordedMerge is the verdict of a merge the status records as delivering the plan, as main's status rule told it
// (settleLock), less the merges that did not deliver it as shown (tenth decision of 2026-11-18 on #771, point 3): the
// request is in a phase only a recorded merge reaches (Merged, WaitingForDelivery, Verifying, Succeeded, Inconclusive:
// a merge recorded without a receipt ends there too), or Failed with its receipt and pull request (Failed before a
// merge has none), or, in any phase but Superseded, its recorded pull request is marked merged (markedMerged:
// PullRequestOpened with reason ChangeRequestMerged), or Superseded with its handoff done and the pull request marked
// merged (handedOff, as main's closedOnly let it through); and no verification holds the lock (heldForVerification
// false). undelivered is a merge that did not deliver the plan as shown (MergedWithForeignChange,
// MergeContentUnreadable) or whose read is still to be made (an open wait for a read): only a read that shows no other
// open pull request of the branch releases it (ByRead, or UndeliveredRepositoryGone). A merge that was recorded was
// seen, and its pull request cannot merge again. It needs no read of the branch, so neither a pause nor a repository
// that cannot be opened any more keeps the lock. The caller checks that the request no longer moves on.
func RecordedMerge(status *api.RemediationRequestStatus, markedMerged, undelivered, handedOff,
	heldForVerification bool) (*Verdict, bool) {
	if heldForVerification {
		return nil, false
	}
	switch status.Phase {
	case api.PhaseMerged, api.PhaseWaitingForDelivery, api.PhaseVerifying, api.PhaseSucceeded, api.PhaseInconclusive:
		return &Verdict{proof: recordedMerge}, true
	case api.PhaseFailed:
		if status.MergeReceipt != nil && status.PullRequest != nil {
			return &Verdict{proof: recordedMerge}, true
		}
	case api.PhaseSuperseded:
		// Written after Shadewright's close: another pull request of the branch may carry the change, so main's rule
		// left it to the read of the branch (closedOnly), unless the replacement took the pull request over (handoff
		// done) and it is marked merged.
		if !handedOff {
			return nil, false
		}
	}
	if !markedMerged || undelivered {
		return nil, false
	}
	return &Verdict{proof: recordedMerge}, true
}

// UndeliveredRepositoryGone is the verdict of a merge that did not deliver the plan as shown (undelivered:
// MergedWithForeignChange, MergeContentUnreadable, MergeNotCheckable) whose repository is gone from the settings
// (repositoryGone): the read that would show no other open pull request of the branch cannot start, and nobody can
// see such a pull request any more (tenth decision of 2026-11-18 on #771, point 3, and the eleventh, points 2 and 3).
// A settings object that is missing is not that: the caller keeps the lock (planEntry returns no errRepositoryGone).
func UndeliveredRepositoryGone(undelivered, repositoryGone bool) (*Verdict, bool) {
	if !undelivered || !repositoryGone {
		return nil, false
	}
	return &Verdict{proof: undeliveredGone}, true
}

// Release clears the lock in status, about to be written, for the verdict v, with the condition TargetHeld that
// said why it was kept. A nil or zero v releases nothing and returns ErrNoVerdict.
func Release(status *api.RemediationRequestStatus, v *Verdict) error {
	if v == nil || v.proof == none {
		return ErrNoVerdict
	}
	held := false
	status.HoldsTarget = &held
	meta.RemoveStatusCondition(&status.Conditions, api.ConditionTargetHeld)
	return nil
}
