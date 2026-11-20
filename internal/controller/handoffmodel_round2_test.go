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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/utils/ptr"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
)

// The second round of #786 against the model: one directed sequence per mechanism changed, each failing without it.

// The replacement records the takeover, its write that ends the predecessor is not made yet, and a person pushes on
// top: the predecessor reads the replacement first and does not abort a recorded takeover, so it neither stores an
// abort nor comments, and the two never both write the pull request (P4; seeds 5038, 5674, 6486, 7032).
func TestHandoffModelRecordedTakeoverIsNotAborted(t *testing.T) {
	m := hmDirected(t, hmStartPushed, []hmStep{stRecRepl, {Kind: hPush}, stRecPred, stRecRepl})
	if m.abortStored {
		t.Fatal("the predecessor aborted an intent whose takeover the replacement recorded")
	}
}

// A takeover recorded, a merge of the replacement's commit, and the replacement deleted before it recorded the merge:
// the merge ends with exactly one record, on the predecessor as a foreign change (P1).
func TestHandoffModelTakeoverMergeDeletedReplacement(t *testing.T) {
	m := hmDirected(t, hmStartPushed, []hmStep{stRecRepl, {Kind: hMerge}, {Kind: hDelete, Obj: hmRepl}})
	if !m.foreignForGone(map[string]*api.RemediationRequest{hmPred: m.get(hmPred), hmRepl: m.get(hmRepl)}) {
		t.Fatalf("the predecessor did not record the merge of the deleted replacement's commit: %s",
			describe(m.get(hmPred)))
	}
}

// A replacement stopped after its recovery watch whose repository leaves the settings keeps the lock and shows
// PullRequestUnreadable with reason RepositoryUnavailable; a person merges its commit meanwhile, and once the
// repository is back the merge is recorded on the replacement with its receipt, which frees the lock (decision of
// 2026-11-19 on #786; seeds 2 and 2542 of the model with the repository put back, where the release lost that record).
func TestHandoffModelRecoveryRepositoryGoneKeepsTheLock(t *testing.T) {
	steps := []hmStep{{Kind: hSettings, Variant: settingsRemoved}, stRecRepl, stRecPred, stClock(clockDay), stRecRepl,
		stRecRepl, stMerge, stRecRepl, stRecPred}
	m := newHMWorld(t, requestStatusSchema(t), hmStartPushed)
	for i, s := range steps {
		m.stepNo = i + 1
		m.apply(s)
		m.observe()
	}
	rr := m.get(hmRepl)
	c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable)
	if !ptr.Deref(rr.Status.HoldsTarget, false) || c == nil || c.Reason != reasonRepositoryUnavailable {
		t.Fatalf("want the lock kept with PullRequestUnreadable %s; got %s, PullRequestUnreadable %v\n  %s",
			reasonRepositoryUnavailable, describe(rr), c, strings.Join(m.trace, "\n  "))
	}
	m.apply(hmStep{Kind: hSettings, Variant: settingsRestored})
	m.apply(stRecRepl)
	m.apply(stRecPred)
	m.observe()
	rr = m.get(hmRepl)
	if rr.Status.MergeReceipt == nil || rr.Status.MergeReceipt.HeadSHA != m.merge.head ||
		ptr.Deref(rr.Status.HoldsTarget, false) {
		t.Fatalf("want the merge recorded on the replacement with its receipt and the lock free; got %s\n  %s",
			describe(rr), strings.Join(m.trace, "\n  "))
	}
	if len(m.violations) > 0 {
		t.Fatalf("violations: %s", strings.Join(m.violations, "; "))
	}
}

// A replacement stopped after its recovery watch whose target branch changed in the settings reads the pull request
// on the recorded branch, and its close there releases the lock (P6).
func TestHandoffModelRecoveryBranchChangedCloseReleases(t *testing.T) {
	hmDirected(t, hmStartPushed, []hmStep{{Kind: hSettings, Variant: settingsBranch}, stRecRepl, stRecPred,
		stClock(clockDay), stRecRepl, {Kind: hClose}, stRecRepl})
}

// A replacement stopped after its recovery watch keeps the lock while the pull request is open with a person's commit
// on top of its own, and says so in TargetHeld.
func TestHandoffModelPersonCommitKeepsTheLockVisibly(t *testing.T) {
	m := hmDirected(t, hmStartPushed, []hmStep{stSticky(403), stRecRepl, stAnswer, {Kind: hPush}, stClock(clockDay),
		stRecRepl, stRecRepl})
	rr := m.get(hmRepl)
	c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionTargetHeld)
	if !ptr.Deref(rr.Status.HoldsTarget, false) || c == nil || c.Reason != ReasonPersonCommitOnPullRequest {
		t.Fatalf("want the lock kept with TargetHeld %s; got %s, TargetHeld %v", ReasonPersonCommitOnPullRequest,
			describe(rr), c)
	}
}

// A refusal aborts the intent, a person merges the replacement's commit, the predecessor looks before the replacement
// recorded the merge, and the replacement is then deleted: the predecessor waits for the receipt instead of ending
// MergedForReplacement, so it records the merge as a foreign change once the replacement is gone (P1; seeds 6188,
// 6825 of the first model).
func TestHandoffModelPredecessorWaitsForTheReceipt(t *testing.T) {
	m := hmDirected(t, hmStartPushed, []hmStep{stSticky(403), stRecRepl, stAnswer, stMerge, stRecPred,
		{Kind: hDelete, Obj: hmRepl}})
	if !m.foreignForGone(map[string]*api.RemediationRequest{hmPred: m.get(hmPred), hmRepl: m.get(hmRepl)}) {
		t.Fatalf("the predecessor did not record the merge of the deleted replacement's commit: %s",
			describe(m.get(hmPred)))
	}
}

// After the takeover a person puts the predecessor's commit back and merges it: the predecessor records its merge, and
// the replacement leaves it, whichever looks first, also when the predecessor is deleted after it recorded (P1).
func TestHandoffModelPredecessorHeadPutBack(t *testing.T) {
	drop := hmStep{Kind: hForce, Variant: forceDrop}
	for name, steps := range map[string][]hmStep{
		"replacement first":           {stRecRepl, drop, stMerge, stRecRepl, stRecPred},
		"predecessor first":           {stRecRepl, drop, stMerge, stRecPred, stRecRepl},
		"predecessor deleted":         {stRecRepl, drop, stMerge, stRecPred, {Kind: hDelete, Obj: hmPred}},
		"predecessor deleted, unread": {stRecRepl, drop, stMerge, {Kind: hDelete, Obj: hmPred}},
	} {
		t.Run(name, func(t *testing.T) { hmDirected(t, hmStartPushed, steps) })
	}
}

// The pull request is closed, the replacement settles its handoff (Aborted) and releases the lock, a person reopens
// the pull request and merges the replacement's commit: the replacement records nothing of it (a reopen after a
// release), so the predecessor records it as a foreign change and lets go of the lock (P6).
func TestHandoffModelSettledReplacementLeavesTheMerge(t *testing.T) {
	m := hmDirected(t, hmStartPushed, []hmStep{{Kind: hClose}, stRecRepl, stRecRepl, {Kind: hReopen}, stMerge})
	if p := m.get(hmPred); p == nil || ptr.Deref(p.Status.HoldsTarget, false) {
		t.Fatalf("the predecessor keeps the lock of the merged pull request: %s", describe(p))
	}
}

// A refusal aborts the intent, a person merges the replacement's commit, the replacement records the merge with its
// receipt once the host answers and is deleted: the predecessor, looking while the replacement's finalizer has not run
// yet, leaves the merge to it instead of recording it a second time as a foreign change (P1, P2).
func TestHandoffModelDeletingReplacementKeepsItsRecordedMerge(t *testing.T) {
	hmDirected(t, hmStartPushed, []hmStep{stSticky(403), stMerge, stRecRepl, stAnswer, stRecRepl,
		{Kind: hDelete, Obj: hmRepl}})
}

// The predecessor is deleted while its replacement gives up the handoff: the finalizer stores the abort of the intent
// before the close, so the close passes its fresh read at once and no cleanup failure starts the bound that a later
// refusal of the Git host would end with the pull request open (P3).
func TestHandoffModelDeletedPredecessorStoresTheAbortBeforeTheClose(t *testing.T) {
	m := hmDirected(t, hmStartIntent, []hmStep{{Kind: hDelete, Obj: hmPred}, stRecRepl, stRecPred,
		{Kind: hFault, Op: "ListChangeRequests", Code: 403}, stClock("past-24h"), stRecRepl, stRecRepl, stRecPred})
	if m.pr().State == providers.ChangeRequestOpen {
		t.Fatalf("the deleted predecessor left its pull request open")
	}
}
