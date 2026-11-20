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
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// A merge of the pull request a replacement was to take over, with a head that is not its commit, while the
// replacement holds the target lock before its takeover (decision of 2026-11-20 on #870): the replacement records it,
// whatever head was merged, as a foreign change after the read of what it brought; the predecessor records none.

// watchingAfterRefusal is a replacement whose recovery met a refusal and that watches the pull request (its handoff
// aborted, the lock kept), with a predecessor that resumed and stopped BranchDiverged on the replacement's commit, as
// after its comment handed the pull request to a person. Reads answer again.
func watchingAfterRefusal(t *testing.T) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest,
	*api.RemediationRequest, providers.ChangeRequest, func(time.Duration)) {
	t.Helper()
	r, g, p, rr, cr, _ := handoffAppendedWorld(t, nil)
	move := movableClock(r)
	refuseAll(g)
	if _, err := reconcileOnce(t, r, rr); err != nil {
		t.Fatal(err)
	}
	g.SetFail(nil)
	for range 2 {
		_, _ = reconcileOnce(t, r, p)
	}
	pred := storedRR(t, r, p)
	if c := meta.FindStatusCondition(pred.Status.Conditions, api.ConditionApproved); pred.Status.Phase !=
		api.PhaseNeedsIntervention || c == nil || c.Reason != ReasonBranchDiverged {
		t.Fatalf("predecessor %q, Approved %+v: want it stopped BranchDiverged", pred.Status.Phase, c)
	}
	return r, g, pred, storedRR(t, r, rr), cr, move
}

// noRecordOnPredecessor fails if p records the merge in any form (recordsMerge: a receipt, mergedAt, Merged, or a
// merge that did not deliver the plan as shown).
func noRecordOnPredecessor(t *testing.T, p *api.RemediationRequest) {
	t.Helper()
	if recordsMerge(p) {
		c := meta.FindStatusCondition(p.Status.Conditions, api.ConditionApproved)
		t.Fatalf("predecessor %q, Approved %+v, receipt %v: it records the merge, which the replacement records",
			p.Status.Phase, c, p.Status.MergeReceipt)
	}
}

// A person pushes on top of the replacement's commit and merges, both requests look in turn: the replacement records
// the merge once as a foreign change, the predecessor waits for that record and ends MergedForReplacement, and both
// locks are released once the record is stored (the model's P12 checks that the replacement never releases before).
func TestOtherHeadOnTopOfTheReplacementsCommitRecordedOnce(t *testing.T) {
	r, g, p, rr, cr, move := watchingAfterRefusal(t)
	branch := "shadewright/" + string(chainRootOf(p))
	if _, err := g.Push(branch, "person.yaml", []byte("by: a person\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	// The predecessor looks first: the merge is the replacement's to record, so it waits and records nothing.
	gotP, err := reconcileOnce(t, r, p)
	if err != nil {
		t.Fatal(err)
	}
	noRecordOnPredecessor(t, gotP)
	got, err := reconcileOnce(t, r, rr)
	if err != nil {
		t.Fatal(err)
	}
	foreignRecordedOnReplacement(t, got, cr.Number)
	for range 2 {
		move(heldRecheckInterval)
		gotP, _ = reconcileOnce(t, r, p)
		got, _ = reconcileOnce(t, r, rr)
	}
	noRecordOnPredecessor(t, gotP)
	if c := meta.FindStatusCondition(gotP.Status.Conditions, api.ConditionApproved); gotP.Status.Phase !=
		api.PhaseSuperseded || c == nil || c.Reason != ReasonMergedForReplacement {
		t.Fatalf("predecessor %q, Approved %+v: want Superseded with MergedForReplacement", gotP.Status.Phase, c)
	}
	foreignRecordedOnReplacement(t, got, cr.Number)
	if ptr.Deref(got.Status.HoldsTarget, false) || ptr.Deref(gotP.Status.HoldsTarget, false) {
		t.Fatalf("lock after the record: replacement %v, predecessor %v; want both released",
			ptr.Deref(got.Status.HoldsTarget, false), ptr.Deref(gotP.Status.HoldsTarget, false))
	}
}

// A merged head that does not carry the replacement's commit (a person force pushed the predecessor's commit back
// and pushed another change on it) while the replacement holds the lock after the end of its watch: the replacement
// records it once as a foreign change, the predecessor none.
func TestOtherHeadWithoutTheReplacementsCommitRecordedOnce(t *testing.T) {
	r, g, p, rr, cr, move := endedWatch(t)
	g.SetFail(nil)
	branch := "shadewright/" + string(chainRootOf(p))
	if err := g.ForcePush(branch, p.Status.PullRequest.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Push(branch, "person.yaml", []byte("by: a person\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	var got, gotP *api.RemediationRequest
	for range 3 {
		move(heldRecheckInterval)
		gotP, _ = reconcileOnce(t, r, p)
		got, _ = reconcileOnce(t, r, rr)
	}
	foreignRecordedOnReplacement(t, got, cr.Number)
	noRecordOnPredecessor(t, gotP)
	if ptr.Deref(got.Status.HoldsTarget, false) {
		t.Fatal("the recorded merge did not settle the replacement's lock")
	}
}

// The read of what the merge brought fails: the replacement records nothing yet, keeps the lock, holds a MergeContent
// wait that names the merge (a claim the predecessor waits for, recording nothing either), and returns a wait, no
// error. Past mergeContentWait the merge is recorded on the replacement as MergeContentUnreadable.
func TestOtherHeadContentReadFailsWaitsThenRecords(t *testing.T) {
	r, g, p, rr, cr, move := watchingAfterRefusal(t)
	branch := "shadewright/" + string(chainRootOf(p))
	if _, err := g.Push(branch, "person.yaml", []byte("by: a person\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	// CheckBetween is read only by the content read (retargetedContent), not by the owner check before it.
	g.SetFail(func(op string) error {
		if op == opCheckBetween {
			return unavailable()
		}
		return nil
	})
	res, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
	if err != nil {
		t.Fatalf("a failed content read returned %v; want a wait without an error", err)
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("result %+v: want a wait", res)
	}
	got := storedRR(t, r, rr)
	w := gitWait(got, waitMergeContent)
	if recordsMerge(got) || got.Status.PullRequest != nil || w == nil || w.PullRequest == nil ||
		w.PullRequest.Number != cr.Number || !ptr.Deref(got.Status.HoldsTarget, false) {
		t.Fatalf("phase %q, pull request %+v, waits %+v: want no record yet, a MergeContent wait about #%d and the lock",
			got.Status.Phase, got.Status.PullRequest, got.Status.GitWaits, cr.Number)
	}
	gotP, _ := reconcileOnce(t, r, p)
	noRecordOnPredecessor(t, gotP)
	for range 3 {
		move(mergeContentWait)
		_, _ = reconcileOnce(t, r, rr)
	}
	got = storedRR(t, r, rr)
	c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionApproved)
	if got.Status.Phase != api.PhaseNeedsIntervention || c == nil || c.Reason != ReasonMergeContentUnreadable ||
		got.Status.PullRequest == nil || got.Status.PullRequest.Number != cr.Number || !tookOver(got) {
		t.Fatalf("phase %q, Approved %+v, pull request %+v: want the merge recorded as MergeContentUnreadable at the "+
			"end of the wait", got.Status.Phase, c, got.Status.PullRequest)
	}
	gotP, _ = reconcileOnce(t, r, p)
	noRecordOnPredecessor(t, gotP)
}

// The content read of recordOtherHeadMerge is bounded by the MergeContent wait, the same entry and end as the read of
// a retargeted merge (checkRetargeted): the bound is checked by that site's case.
func init() {
	gitReadSites["recoverywait.go:recordOtherHeadMerge:1"] = gitReadSites["branchchange.go:checkRetargeted:1"]
}
