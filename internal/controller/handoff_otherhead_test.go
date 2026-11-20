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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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

// predecessorHeadMerged is endedWatch with the predecessor's own recorded head put back on the pull request by a force
// push and merged, before the replacement took it over.
func predecessorHeadMerged(t *testing.T) (*RemediationRequestReconciler, *api.RemediationRequest, *api.RemediationRequest,
	providers.ChangeRequest, func(time.Duration)) {
	t.Helper()
	r, g, p, rr, cr, move := endedWatch(t)
	g.SetFail(nil)
	if err := g.ForcePush("shadewright/"+string(chainRootOf(p)), p.Status.PullRequest.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	return r, p, rr, cr, move
}

// The predecessor's own head is merged and the predecessor is gone before it recorded it, while the replacement holds
// the lock before its takeover (third case of the decision of 2026-11-20 on #870): the replacement records the merge
// as a foreign change and then settles its lock. Before, it ended with the lock released and nothing recorded.
func TestPredecessorHeadMergeRecordedWithThePredecessorGone(t *testing.T) {
	r, p, rr, cr, move := predecessorHeadMerged(t)
	ctx := t.Context()
	gone := storedRR(t, r, p)
	gone.Finalizers = nil
	if err := r.Update(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, gone); err != nil {
		t.Fatal(err)
	}
	move(heldRecheckInterval)
	got, err := reconcileOnce(t, r, rr)
	if err != nil {
		t.Fatal(err)
	}
	foreignRecordedOnReplacement(t, got, cr.Number)
	move(heldRecheckInterval)
	for range 2 {
		got, _ = reconcileOnce(t, r, rr)
	}
	foreignRecordedOnReplacement(t, got, cr.Number)
	if ptr.Deref(got.Status.HoldsTarget, false) {
		t.Fatal("the recorded merge did not settle the replacement's lock")
	}
}

// The same merge while the predecessor is being deleted (its finalizer pending) and holds no record of it: the
// replacement records it, and the predecessor's finalizer records none and lets go.
func TestPredecessorHeadMergeRecordedWhileThePredecessorIsDeleted(t *testing.T) {
	r, p, rr, cr, move := predecessorHeadMerged(t)
	deleted := storedRR(t, r, p)
	if controllerutil.AddFinalizer(deleted, FinalizerName) {
		if err := r.Update(t.Context(), deleted); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Delete(t.Context(), deleted); err != nil {
		t.Fatal(err)
	}
	if deleting := storedRR(t, r, p); deleting.DeletionTimestamp.IsZero() || recordsMerge(deleting) {
		t.Fatalf("predecessor %q, deleting %v: want it being deleted with no record of the merge",
			deleting.Status.Phase, deleting.DeletionTimestamp)
	}
	move(heldRecheckInterval)
	got, err := reconcileOnce(t, r, rr)
	if err != nil {
		t.Fatal(err)
	}
	foreignRecordedOnReplacement(t, got, cr.Number)
	for range 3 {
		move(heldRecheckInterval)
		_, _ = r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(p)})
		got, _ = reconcileOnce(t, r, rr)
	}
	foreignRecordedOnReplacement(t, got, cr.Number)
	left := &api.RemediationRequest{}
	switch err := r.Get(t.Context(), client.ObjectKeyFromObject(p), left); {
	case apierrors.IsNotFound(err):
	case err != nil:
		t.Fatal(err)
	default:
		noRecordOnPredecessor(t, left)
	}
}

// A present predecessor keeps its own head: the replacement records nothing of that merge and the predecessor
// records it, so exactly one request holds the record.
func TestPredecessorHeadMergeKeptByThePresentPredecessor(t *testing.T) {
	r, p, rr, _, move := predecessorHeadMerged(t)
	var got, gotP *api.RemediationRequest
	for range 3 {
		move(heldRecheckInterval)
		got, _ = reconcileOnce(t, r, rr)
		gotP, _ = reconcileOnce(t, r, p)
	}
	if recordsMerge(got) {
		c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionApproved)
		t.Fatalf("replacement %q, Approved %+v: it records the merge of the predecessor's head", got.Status.Phase, c)
	}
	if !recordsMerge(gotP) {
		t.Fatalf("predecessor %q: want it to record the merge of its own head", gotP.Status.Phase)
	}
}

// The wait for the merge commit and the content read of recordOtherHeadMerge are bounded by the MergeContent wait, the
// same entry and end as the read of a retargeted merge (checkRetargeted): the bound is checked by that site's case.
func init() {
	gitReadSites["recoverywait.go:recordOtherHeadMerge:1"] = gitReadSites["branchchange.go:checkRetargeted:1"]
	gitReadSites["recoverywait.go:recordOtherHeadMerge:2"] = gitReadSites["branchchange.go:checkRetargeted:1"]
}

// The predecessor records the merge of its own head while the replacement holds the lock before its takeover, and is
// then deleted; the replacement looks only after that (decision of 2026-11-20 on #870 replacing the third row): the
// predecessor ended the replacement in the pass that recorded the merge, so the replacement never records it a second
// time as a foreign change, counts no failure, and lets go of the lock.
func TestPredecessorRecordedItsHeadThenGoneRecordedOnce(t *testing.T) {
	r, p, rr, _, move := predecessorHeadMerged(t)
	ctx := t.Context()
	gotP, err := reconcileOnce(t, r, p)
	if err != nil {
		t.Fatal(err)
	}
	if !recordsMerge(gotP) {
		t.Fatalf("predecessor %q: want it to record the merge of its own head", gotP.Status.Phase)
	}
	// The pass that stored the record ended the replacement: its handoff Aborted and its lock released.
	if ended := storedRR(t, r, rr); ptr.Deref(ended.Status.HoldsTarget, false) ||
		ended.Status.Handoff.From.Phase != api.HandoffAborted {
		t.Fatalf("replacement %q, handoff %q, lock %v after the predecessor's record: want it ended in that pass",
			ended.Status.Phase, ended.Status.Handoff.From.Phase, ptr.Deref(ended.Status.HoldsTarget, false))
	}
	if err := r.Delete(ctx, gotP); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(p)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(p), &api.RemediationRequest{}); !apierrors.IsNotFound(err) {
		t.Fatalf("predecessor still there (%v): want its finalizer done", err)
	}
	var got *api.RemediationRequest
	for range 3 {
		move(heldRecheckInterval)
		got, _ = reconcileOnce(t, r, rr)
	}
	if recordsMerge(got) {
		c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionApproved)
		t.Fatalf("replacement %q, Approved %+v: it records a merge the predecessor recorded", got.Status.Phase, c)
	}
	if breakerOutcome(got) == outcomeFailure {
		t.Fatalf("replacement %q: counted as a failure for a merge already recorded", got.Status.Phase)
	}
	if ptr.Deref(got.Status.HoldsTarget, false) {
		t.Fatal("replacement keeps the target lock after the merge was recorded")
	}
}

// The same record while the replacement still watches (its handoff aborted at a refusal, not ended): the pass that
// stores the predecessor's record ends it Superseded with MergedForPredecessor, its handoff Aborted and its lock
// released, no failure for the circuit breaker.
func TestPredecessorRecordedItsHeadEndsTheWatchingReplacement(t *testing.T) {
	r, g, p, rr, cr, _ := watchingAfterRefusal(t)
	if err := g.ForcePush("shadewright/"+string(chainRootOf(p)), p.Status.PullRequest.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	gotP, err := reconcileOnce(t, r, p)
	if err != nil {
		t.Fatal(err)
	}
	if !recordsMerge(gotP) {
		t.Fatalf("predecessor %q: want it to record the merge of its own head", gotP.Status.Phase)
	}
	got := storedRR(t, r, rr)
	c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionPROpened)
	if got.Status.Phase != api.PhaseSuperseded || c == nil || c.Reason != ReasonMergedForPredecessor ||
		got.Status.Handoff.From.Phase != api.HandoffAborted || ptr.Deref(got.Status.HoldsTarget, false) ||
		recordsMerge(got) || breakerOutcome(got) == outcomeFailure {
		t.Fatalf("replacement %q, PullRequestOpened %+v, handoff %q, lock %v: want Superseded with "+
			"MergedForPredecessor, its handoff Aborted, the lock released and no record", got.Status.Phase, c,
			got.Status.Handoff.From.Phase, ptr.Deref(got.Status.HoldsTarget, false))
	}
}

// The replacement that holds the lock sees the predecessor's own head merged first, while the predecessor is present
// without a record of it, and the predecessor is then deleted before it reads the merge (decision of 2026-11-20 on
// #870, one rule over the state): the replacement keeps the lock and waits, visibly (TargetHeld with
// MergeRecordPending), and records the merge itself once the predecessor is gone. Before, it released the lock at its
// first look and the merge was recorded on neither request.
func TestReplacementSeesPredecessorHeadFirstThenPredecessorGone(t *testing.T) {
	r, p, rr, cr, move := predecessorHeadMerged(t)
	ctx := t.Context()
	move(heldRecheckInterval)
	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
	if err != nil {
		t.Fatalf("the look returned %v; want a wait without an error", err)
	}
	waiting := storedRR(t, r, rr)
	held := meta.FindStatusCondition(waiting.Status.Conditions, api.ConditionTargetHeld)
	if !ptr.Deref(waiting.Status.HoldsTarget, false) || waiting.Status.Handoff.From.Phase != api.HandoffReserved ||
		held == nil || held.Reason != ReasonMergeRecordPending || recordsMerge(waiting) || res.RequeueAfter <= 0 {
		t.Fatalf("replacement %q, handoff %q, lock %v, TargetHeld %+v, result %+v: want the lock kept with "+
			"MergeRecordPending and a wait while the present predecessor holds no record", waiting.Status.Phase,
			waiting.Status.Handoff.From.Phase, ptr.Deref(waiting.Status.HoldsTarget, false), held, res)
	}
	gone := storedRR(t, r, p)
	if recordsMerge(gone) {
		t.Fatalf("predecessor %q records the merge already: the order under test is look, delete, look", gone.Status.Phase)
	}
	gone.Finalizers = nil
	if err := r.Update(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, gone); err != nil {
		t.Fatal(err)
	}
	var got *api.RemediationRequest
	for range 3 {
		move(heldRecheckInterval)
		got, _ = reconcileOnce(t, r, rr)
	}
	foreignRecordedOnReplacement(t, got, cr.Number)
	if ptr.Deref(got.Status.HoldsTarget, false) {
		t.Fatal("the recorded merge did not settle the replacement's lock")
	}
}

// The Git host reports the merge with another head without a full merge commit ID: the replacement records nothing
// yet, keeps the lock, holds a MergeContent wait about the merge (a claim the predecessor waits for) and returns a
// wait, no error, as the other merge paths wait for the commit. Once the host reports it, what the merge brought is
// read and the merge is recorded as a foreign change.
func TestOtherHeadWithoutMergeCommitWaitsForIt(t *testing.T) {
	r, g, p, rr, cr, move := watchingAfterRefusal(t)
	branch := "shadewright/" + string(chainRootOf(p))
	if _, err := g.Push(branch, "person.yaml", []byte("by: a person\n")); err != nil {
		t.Fatal(err)
	}
	commit, err := g.MergeWithCommit(cr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.SetMergeCommit(cr.Number, ""); err != nil {
		t.Fatal(err)
	}
	res, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
	if err != nil {
		t.Fatalf("a merge without its merge commit ID returned %v; want a wait without an error", err)
	}
	got := storedRR(t, r, rr)
	w := gitWait(got, waitMergeContent)
	if res.RequeueAfter <= 0 || recordsMerge(got) || got.Status.Phase == api.PhaseNeedsIntervention || w == nil ||
		w.PullRequest == nil || w.PullRequest.Number != cr.Number || !ptr.Deref(got.Status.HoldsTarget, false) {
		t.Fatalf("result %+v, phase %q, waits %+v: want no record yet, a wait about #%d and the lock", res,
			got.Status.Phase, got.Status.GitWaits, cr.Number)
	}
	gotP, _ := reconcileOnce(t, r, p)
	noRecordOnPredecessor(t, gotP)
	if err := g.SetMergeCommit(cr.Number, commit); err != nil {
		t.Fatal(err)
	}
	move(time.Minute)
	got, _ = reconcileOnce(t, r, rr)
	foreignRecordedOnReplacement(t, got, cr.Number)
	if c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionApproved); strings.Contains(c.Message,
		"no full merge commit ID") {
		t.Fatalf("Approved message %q: the merge was recorded on the missing ID", c.Message)
	}
}

// The Git host never reports the merge commit ID: past mergeContentWait the merge is recorded on the replacement as
// MergeContentUnreadable, which claims nothing about what the merge brought, and the predecessor records none.
func TestOtherHeadMergeCommitNeverReportedRecordsUnreadable(t *testing.T) {
	r, g, p, rr, cr, move := watchingAfterRefusal(t)
	if _, err := g.Push("shadewright/"+string(chainRootOf(p)), "person.yaml", []byte("by: a person\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	if err := g.SetMergeCommit(cr.Number, ""); err != nil {
		t.Fatal(err)
	}
	var got *api.RemediationRequest
	for range 3 {
		got, _ = reconcileOnce(t, r, rr)
		move(mergeContentWait)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionApproved)
	if got.Status.Phase != api.PhaseNeedsIntervention || c == nil || c.Reason != ReasonMergeContentUnreadable ||
		got.Status.PullRequest == nil || got.Status.PullRequest.Number != cr.Number || !tookOver(got) {
		t.Fatalf("phase %q, Approved %+v: want the merge recorded as MergeContentUnreadable at the end of the wait",
			got.Status.Phase, c)
	}
	gotP, _ := reconcileOnce(t, r, p)
	noRecordOnPredecessor(t, gotP)
}

// The kill switch pauses Git writes between the predecessor's record of the merge of its own head and the end of the
// replacement that holds the lock before its takeover (endReplacementAfterRecord, run at the end of the predecessor's
// reconcile on the predecessor as that reconcile stored it): the replacement keeps the lock and the reconcile is
// requeued no later than freshRecheckDelay, never with an error; once writes resume the same step ends the
// replacement.
func TestPredecessorRecordUnderPauseEndsTheReplacementLater(t *testing.T) {
	r, p, rr, _, _ := predecessorHeadMerged(t)
	recorded := storedRR(t, r, p)
	now := metav1.NowMicro()
	recorded.Status.PullRequest.MergedAt = &now
	if !holdsMergeRecord(recorded, recorded.Status.PullRequest.Number, recorded.Status.PullRequest.HeadSHA) {
		t.Fatal("the predecessor as stored by its record does not hold the record")
	}
	setPaused(t, r, true)
	var res reconcile.Result
	var err error
	r.endReplacementAfterRecord(t.Context(), recorded, &res, &err)
	if err != nil {
		t.Fatalf("the paused step returned %v; want a requeue without an error", err)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > freshRecheckDelay {
		t.Fatalf("result %+v: want a requeue within %v, so the end of the replacement is tried again", res,
			freshRecheckDelay)
	}
	if held := storedRR(t, r, rr); !ptr.Deref(held.Status.HoldsTarget, false) {
		t.Fatal("the replacement let go of the lock while writes are paused")
	}
	// A longer wait the reconcile had is shortened only to the pause's recheck, and a shorter one is kept.
	res, err = reconcile.Result{RequeueAfter: time.Second}, nil
	r.endReplacementAfterRecord(t.Context(), recorded, &res, &err)
	if err != nil || res.RequeueAfter != time.Second {
		t.Fatalf("result %+v, error %v: want the shorter wait of the reconcile kept", res, err)
	}
	setPaused(t, r, false)
	res, err = reconcile.Result{}, nil
	r.endReplacementAfterRecord(t.Context(), recorded, &res, &err)
	if err != nil {
		t.Fatal(err)
	}
	if ended := storedRR(t, r, rr); ptr.Deref(ended.Status.HoldsTarget, false) ||
		ended.Status.Handoff.From.Phase != api.HandoffAborted || recordsMerge(ended) {
		t.Fatalf("replacement %q, handoff %q, lock %v: want it ended on the predecessor's record once writes resume",
			ended.Status.Phase, ended.Status.Handoff.From.Phase, ptr.Deref(ended.Status.HoldsTarget, false))
	}
}
