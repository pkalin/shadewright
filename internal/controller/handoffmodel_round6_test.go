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

package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"

	api "github.com/pkalin/shadewright/api/v1alpha1"
)

// The sixth round of #786 against the model: one directed sequence per defect, each failing without its fix.

var stMergeNoID = hmStep{Kind: hMerge, Commit: true, NoID: true}

// A refusal aborts the handoff (every read refused), a person merges the replacement's commit and the host reports the
// merged pull request without a merge commit ID. After the wait for the commit the replacement records the merge
// without a receipt, releases its lock and ends Inconclusive; the predecessor, which waited for that record, must leave
// the merge to it (MergedForReplacement), never record it again as a foreign change counted by the breaker (P1, P2;
// seeds 129, 149 and 1157 of the model with the fault).
func TestHandoffModelMergeWithoutCommitIDIsRecordedOnce(t *testing.T) {
	m := hmDirected(t, hmStartPushed, []hmStep{stSticky(403), stRecRepl, stMergeNoID, stAnswer, stRecPred, stRecRepl,
		stClock(clockDeadline), stRecRepl, stRecRepl, stRecPred})
	p, rr := m.get(hmPred), m.get(hmRepl)
	if rr.Status.PullRequest == nil || rr.Status.PullRequest.MergedAt == nil || rr.Status.MergeReceipt != nil {
		t.Fatalf("want the merge recorded on the replacement without a receipt; got %s", describe(rr))
	}
	if c := meta.FindStatusCondition(p.Status.Conditions, api.ConditionApproved); c == nil ||
		c.Reason != ReasonMergedForReplacement {
		t.Fatalf("want the predecessor Superseded with %s; got %s", ReasonMergedForReplacement, describe(p))
	}
}

// The same merge recorded without a receipt, and the replacement deleted before the predecessor looks: its finalizer
// ends the predecessor on that record (supersedeAfterAbort), so the predecessor, which would read the replacement
// gone, never records the merge as a foreign change (P1, P2).
func TestHandoffModelMergeWithoutCommitIDReplacementDeleted(t *testing.T) {
	hmDirected(t, hmStartPushed, []hmStep{stSticky(403), stRecRepl, stMergeNoID, stAnswer, stRecRepl,
		stClock(clockDeadline), stRecRepl, {Kind: hClock, Clock: clockSmall, Mins: 6}, stRecRepl, stRecRepl,
		{Kind: hDelete, Obj: hmRepl}, stRecRepl, stRecPred})
}

// The mirror after a takeover: a person puts the predecessor's commit back and merges it, the host reports no merge
// commit ID, and the predecessor records the merge without a receipt after the wait, ends Inconclusive and releases its
// lock. The replacement leaves that merge to it (MergedForPredecessor) instead of recording it as a foreign change (P1).
func TestHandoffModelPredecessorMergeWithoutCommitID(t *testing.T) {
	hmDirected(t, hmStartIntent, []hmStep{stRecRepl, {Kind: hForce, Variant: forceDrop}, stMergeNoID, stRecPred,
		{Kind: hClock, Clock: clockSmall, Mins: 6}, stRecPred, stRecPred, stRecRepl})
}
