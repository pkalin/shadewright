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
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
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

// After the takeover a person puts the predecessor's commit back and closes the pull request; the predecessor's look is
// cut after its first write, then a person reopens and merges it. The replacement waits for the predecessor's record,
// and the status write that shows the wait is lost (or meets a conflict): the look must end there, with nothing written
// to the merged pull request (no check run, no close, no stale cleanup marked), not go on to the full freshness check:
// the reconcile returns the failed write as a merge seen and not recorded (errMergeCheck), and P4 holds.
func TestHandoffModelLostWaitWriteWritesNothing(t *testing.T) {
	for _, fault := range []hmKind{hLose, hConflict} {
		t.Run(string(fault), func(t *testing.T) {
			steps := []hmStep{stRecRepl, {Kind: hForce, Variant: forceDrop}, {Kind: hClose}, {Kind: hCrash, N: 1},
				stRecPred, {Kind: hReopen}, stMerge, {Kind: fault, Obj: hmRepl}, stRecRepl}
			m := newHMWorld(t, requestStatusSchema(t), hmStartPushed)
			for i, s := range steps {
				m.stepNo = i + 1
				m.apply(s)
				m.observe()
			}
			if err := m.lastErr[hmRepl]; !errors.Is(err, errMergeCheck) || !isClusterError(err) {
				t.Fatalf("the look after the failed write of the wait returned %v, want the write's failure under "+
					"errMergeCheck from the Kubernetes API\n  %s", err, strings.Join(m.trace, "\n  "))
			}
			hmDirected(t, hmStartPushed, steps)
		})
	}
}

// The recognisers of the #288 cleanup bound and of a deletion while the repository is missing (#877) excuse only what
// that shape leaves: the open pull request of the request that let go, with no live request on it. A later violation
// by the request that is still there (here a P3 release and a P1 double record of its own) is a violation, not
// excused for the rest of the sequence.
func TestHandoffModelGoneShapesExcuseOnlyTheirOwn(t *testing.T) {
	for name, steps := range map[string][]hmStep{
		"#288 cleanup bound": {stRecRepl, {Kind: hDelete, Obj: hmRepl}, stSticky(503), stRecRepl, stClock(clockDay),
			stRecRepl},
		"#877 deleted while the repository was missing": {stRecRepl, stRecPred,
			{Kind: hSettings, Variant: settingsRemoved}, {Kind: hDelete, Obj: hmRepl}, stRecRepl,
			{Kind: hSettings, Variant: settingsRestored}},
	} {
		t.Run(name, func(t *testing.T) {
			m := newHMWorld(t, requestStatusSchema(t), hmStartPushed)
			for i, s := range steps {
				m.stepNo = i + 1
				m.apply(s)
				m.observe()
			}
			if len(m.violations) > 0 {
				t.Fatalf("violations before the injected ones: %v", m.violations)
			}
			m.fail("P3", "the predecessor released the lock on a merge of its commit it does not record (injected)")
			m.fail("P1", "the merge of the predecessor's commit is recorded on both requests (injected)")
			if len(m.violations) != 2 {
				t.Fatalf("violations %v, excused %v: the shape excused a violation of the request still there",
					m.violations, m.excused)
			}
		})
	}
}
