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
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/policy/freshness"
)

// Each live read of the other request of a handoff, or of the request itself, decides on the copy the API server
// holds (decision of 2026-11-19 on #786). Every test below gives the reconciler a cache that lags (lagging) and a
// stored object that differs from what the cache serves in the way that changes the decision; each fails when its
// read goes back to the cached client.

// laggingLive gives r a cache that serves serve for the names it holds, and the API server as its reader.
func laggingLive(r *RemediationRequestReconciler, serve map[string]*api.RemediationRequest) {
	live := r.Client
	r.Client, r.APIReader = lagging(live, serve), live
}

// mustStatus stores x's status.
func mustStatus(t *testing.T, r *RemediationRequestReconciler, x *api.RemediationRequest) {
	t.Helper()
	if err := r.Status().Update(context.Background(), x); err != nil {
		t.Fatal(err)
	}
}

// chainLink: the request it replaces is gone, though the cache still serves it handed over to this one.
func TestChainLinkReadsThePredecessorLive(t *testing.T) {
	r, _, p, rr, _, _ := gateWorld(t)
	ctx := context.Background()
	cached := p.DeepCopy()
	if err := r.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	laggingLive(r, map[string]*api.RemediationRequest{oldName: cached})
	reason, message, err := r.chainLink(ctx, rr)
	if err != nil || reason != api.ReasonChainLinkInvalid || message != "The request it replaces does not exist" {
		t.Fatalf("chainLink = %q, %q, %v; want the link refused for a predecessor that does not exist", reason,
			message, err)
	}
}

// handoffStep: the predecessor's intent for this request is stored Aborted, though the cache still serves it
// standing; the replacement ends instead of taking the pull request over.
func TestHandoffStepReadsThePredecessorLive(t *testing.T) {
	r, g, p, rr, _, head := gateWorld(t)
	ctx := context.Background()
	cached := p.DeepCopy()
	p.Status.Handoff.To.Phase = api.HandoffAborted
	mustStatus(t, r, p)
	laggingLive(r, map[string]*api.RemediationRequest{oldName: cached})
	if _, err := r.handoffStep(ctx, rr, freshness.Result{Head: head, Git: g}); err != nil && !errors.Is(err, errStopped) {
		t.Fatal(err)
	}
	stored := &api.RemediationRequest{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(rr), stored); err != nil {
		t.Fatal(err)
	}
	c := meta.FindStatusCondition(stored.Status.Conditions, api.ConditionPROpened)
	if stored.Status.Phase != api.PhaseCancelled || c == nil || c.Reason != api.ReasonHandoffAborted ||
		!strings.Contains(c.Message, "ended before the handoff") {
		t.Fatalf("phase %q, PullRequestOpened %+v; want Cancelled: the predecessor ended before the handoff",
			stored.Status.Phase, c)
	}
}

// predecessorMergeOwner: the replacement holds its record of the merge of its commit, which the cache has not seen
// (it serves no replacement at all); the merge is the replacement's.
func TestPredecessorMergeOwnerReadsTheReplacementLive(t *testing.T) {
	r, g, p, rr, _, head := tookOverWorld(t)
	ctx := context.Background()
	if err := r.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	rr.Status.PullRequest.HeadSHA, rr.Status.PullRequest.MergedAt = head, &metav1.MicroTime{Time: r.now()}
	mustStatus(t, r, rr)
	laggingLive(r, map[string]*api.RemediationRequest{newName: nil})
	owner, err := r.predecessorMergeOwner(ctx, g, p, head)
	if err != nil || owner != ownerReplacement {
		t.Fatalf("predecessorMergeOwner = %v, %v; want ownerReplacement (%v)", owner, err, ownerReplacement)
	}
}

// replacementMergeOwner: the predecessor holds its record of the merge of its own head, which the cache has not seen
// (it serves no predecessor); the merge is the predecessor's.
func TestReplacementMergeOwnerReadsThePredecessorLive(t *testing.T) {
	r, _, p, rr, _, _ := tookOverWorld(t)
	ctx := context.Background()
	if err := r.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	own := p.Status.PullRequest.HeadSHA
	p.Status.PullRequest.MergedAt = &metav1.MicroTime{Time: r.now()}
	mustStatus(t, r, p)
	laggingLive(r, map[string]*api.RemediationRequest{oldName: nil})
	owner, err := r.replacementMergeOwner(ctx, rr, own)
	if err != nil || owner != ownerPredecessorRecords {
		t.Fatalf("replacementMergeOwner = %v, %v; want ownerPredecessorRecords (%v)", owner, err,
			ownerPredecessorRecords)
	}
}

// supersedeReplacementAfterMerge: the replacement that took the pull request over exists, though the cache has not
// seen it; a predecessor being deleted with the record of its own merge ends it Superseded.
func TestSupersedeReplacementAfterMergeReadsTheReplacementLive(t *testing.T) {
	r, _, p, rr, _, _ := tookOverWorld(t)
	ctx := context.Background()
	if err := r.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	p.Status.PullRequest.MergedAt = &metav1.MicroTime{Time: r.now()}
	mustStatus(t, r, p)
	laggingLive(r, map[string]*api.RemediationRequest{newName: nil})
	if err := r.supersedeReplacementAfterMerge(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhaseSuperseded {
		t.Fatalf("replacement phase %q, want Superseded", rr.Status.Phase)
	}
}

// replacementTookOver: the replacement recorded its takeover, which the cache has not seen (it serves the
// replacement still Reserved).
func TestReplacementTookOverReadsTheReplacementLive(t *testing.T) {
	r, _, p, rr, _, _ := gateWorld(t)
	cached := rr.DeepCopy()
	rr.Status.Handoff.From.Phase = api.HandoffDone
	mustStatus(t, r, rr)
	laggingLive(r, map[string]*api.RemediationRequest{newName: cached})
	if ok, err := r.replacementTookOver(context.Background(), p); err != nil || !ok {
		t.Fatalf("replacementTookOver = %v, %v; want true", ok, err)
	}
}

// recordHeld: the request as stored waits for the other's record of the merge, and the cache serves the reconcile's
// older copy for its own name too; the write is held.
func TestRecordHeldReadsTheRequestItselfLive(t *testing.T) {
	r, _, _, rr, _, _ := gateWorld(t)
	ctx := context.Background()
	older := rr.DeepCopy()
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionApproved,
		Status: metav1.ConditionFalse, Reason: ReasonMergeRecordPending, Message: "holds for the record"})
	mustStatus(t, r, rr)
	older.ResourceVersion = rr.ResourceVersion
	laggingLive(r, map[string]*api.RemediationRequest{newName: older})
	if err := r.recordHeld(ctx, older); !errors.Is(err, errRecordHeld) {
		t.Fatalf("recordHeld = %v, want the write held", err)
	}
}

// recoverStep: the predecessor's intent for this request is stored Aborted, though the cache still serves it
// standing; nothing is recovered (done false) and the Git host is not read.
func TestRecoverStepReadsThePredecessorLive(t *testing.T) {
	r, g, p, rr, _, _ := gateWorld(t)
	ctx := context.Background()
	cached := p.DeepCopy()
	p.Status.Handoff.To.Phase = api.HandoffAborted
	mustStatus(t, r, p)
	laggingLive(r, map[string]*api.RemediationRequest{oldName: cached})
	refuseAll(g)
	res, done, err := r.recoverStep(ctx, rr)
	if err != nil || done || res != (reconcile.Result{}) {
		t.Fatalf("recoverStep = %+v, done %v, %v; want nothing to recover", res, done, err)
	}
}

// handoffHolds: the replacement recorded its takeover, which the cache has not seen; the predecessor's pull request
// is the replacement's and is kept.
func TestHandoffHoldsReadsTheReplacementLive(t *testing.T) {
	r, _, p, rr, _, _ := gateWorld(t)
	cached := rr.DeepCopy()
	rr.Status.Handoff.From.Phase = api.HandoffDone
	mustStatus(t, r, rr)
	laggingLive(r, map[string]*api.RemediationRequest{newName: cached})
	keep, wait, err := r.handoffHolds(context.Background(), p)
	if err != nil || !keep || wait {
		t.Fatalf("handoffHolds = keep %v, wait %v, %v; want keep", keep, wait, err)
	}
}

// takenOverMerge: the predecessor exists and records the pull request of the wait, though the cache has not seen it;
// the predecessor is returned, not a stand-in for one gone.
func TestTakenOverMergeReadsThePredecessorLive(t *testing.T) {
	r, _, p, rr, cr, _ := handoffAppendedWorld(t, nil)
	laggingLive(r, map[string]*api.RemediationRequest{oldName: nil})
	got, ok, err := r.takenOverMerge(context.Background(), rr, &api.GitWaitPullRequest{Number: cr.Number}, false)
	if err != nil || !ok || got == nil || got.UID != p.UID {
		t.Fatalf("takenOverMerge = %+v, %v, %v; want the predecessor as stored", got, ok, err)
	}
}

// abortPredecessor: the intent is stored aborted, though the cache still serves it standing; nothing is written
// and no retry is asked.
func TestAbortPredecessorReadsThePredecessorLive(t *testing.T) {
	r, _, p, rr, _ := boundWorld(t)
	laggingLive(r, map[string]*api.RemediationRequest{oldName: p})
	res, err := r.abortPredecessor(context.Background(), rr)
	if err != nil || res != (reconcile.Result{}) {
		t.Fatalf("abortPredecessor = %+v, %v; want nothing to abort", res, err)
	}
}

// observeStep: the predecessor no longer hands over to this request, though the cache still serves it naming it;
// the wait has nothing to read.
func TestObserveStepReadsThePredecessorLive(t *testing.T) {
	r, g, p, rr, _ := boundWorld(t)
	ctx := context.Background()
	cached := p.DeepCopy()
	if err := r.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	p.Status.Handoff.To = nil
	mustStatus(t, r, p)
	g.SetFail(nil)
	laggingLive(r, map[string]*api.RemediationRequest{oldName: cached})
	if _, _, err := r.observeStep(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if w := gitWait(rr, waitHandoffRecovery); w == nil || w.Operation != recoveryNothingToRead {
		t.Fatalf("HandoffRecovery wait = %+v, want it to have nothing to read", w)
	}
}

// heldRecovery: once the watch ended, the predecessor no longer hands over to this request, though the cache still
// serves it naming it; the look does not read the pull request for this request (done false).
func TestHeldRecoveryReadsThePredecessorLive(t *testing.T) {
	r, g, p, rr, move := boundWorld(t)
	ctx := context.Background()
	key := client.ObjectKeyFromObject(rr)
	move(recoveryWaitBound + time.Minute)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	cached := p.DeepCopy()
	if err := r.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	p.Status.Handoff.To = nil
	mustStatus(t, r, p)
	g.SetFail(nil)
	laggingLive(r, map[string]*api.RemediationRequest{oldName: cached})
	if _, done, err := r.heldRecovery(ctx, rr); err != nil || done {
		t.Fatalf("heldRecovery = done %v, %v; want the look left to the verdict", done, err)
	}
}
