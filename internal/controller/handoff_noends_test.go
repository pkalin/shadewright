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
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/providers"
	fakegit "github.com/shadewright-labs/shadewright/internal/providers/fake"
)

// The predecessor keeps no bound of its own on a standing intent (fourth decision of 2026-11-18 on #786): a
// replacement that pushed its commit before its deadline takes the pull request over whenever it comes back, in
// either order of the two reconciles, and a merge of its commit is recorded on it with its receipt, also when the
// predecessor object is gone by then.

// pastTheGrace is handoffAppendedWorld with the replacement's deadline an hour ahead, and the clock then moved past
// that deadline plus the handoff grace, with the Git host answering: the executor comes back late.
func pastTheGrace(t *testing.T) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest,
	*api.RemediationRequest, providers.ChangeRequest) {
	t.Helper()
	r, g, p, rr, cr, _ := handoffAppendedWorld(t, nil)
	move := movableClock(r)
	rr.Status.Deadline = &metav1.Time{Time: r.now().Add(time.Hour).Truncate(time.Second)}
	if err := r.Status().Update(context.Background(), rr); err != nil {
		t.Fatal(err)
	}
	move(time.Hour + handoffRecoveryGrace + time.Minute)
	return r, g, p, rr, cr
}

// storedRR reads obj as the fake API server holds it.
func storedRR(t *testing.T, r *RemediationRequestReconciler, obj *api.RemediationRequest) *api.RemediationRequest {
	t.Helper()
	got := &api.RemediationRequest{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(obj), got); err != nil {
		t.Fatal(err)
	}
	return got
}

// The replacement pushed its commit before its deadline and the executor comes back after the deadline plus the
// grace: whichever request reconciles first, the takeover completes and the predecessor leaves the pull request open.
func TestTakeoverAfterTheGraceInEitherOrder(t *testing.T) {
	for _, first := range []string{"replacement", "predecessor"} {
		t.Run(first+" first", func(t *testing.T) {
			r, g, p, rr, cr := pastTheGrace(t)
			order := []*api.RemediationRequest{rr, p}
			if first == "predecessor" {
				order = []*api.RemediationRequest{p, rr}
			}
			// Until the takeover is recorded: the replacement's own looks after it (its freshness check in this world
			// without policies) are not this test's.
		rounds:
			for range 3 {
				for _, obj := range order {
					_, _ = reconcileOnce(t, r, obj)
					if tookOver(storedRR(t, r, rr)) {
						break rounds
					}
				}
			}
			got := storedRR(t, r, rr)
			if !tookOver(got) || got.Status.PullRequest == nil || got.Status.PullRequest.Number != cr.Number {
				t.Fatalf("handoff %+v, pull request %+v: want the takeover of #%d completed", got.Status.Handoff.From,
					got.Status.PullRequest, cr.Number)
			}
			now, err := g.GetChangeRequest(context.Background(), cr.Number)
			if err != nil {
				t.Fatal(err)
			}
			if now.State != providers.ChangeRequestOpen {
				t.Fatalf("pull request #%d is %q: the predecessor closed the pull request it handed over", cr.Number,
					now.State)
			}
			if to := storedRR(t, r, p).Status.Handoff.To; to.Phase == api.HandoffAborted {
				t.Fatalf("the predecessor aborted the intent the replacement completed: %+v", to)
			}
		})
	}
}

// The replacement pushed its commit and the executor restarted before the takeover was recorded; time passes beyond
// the deadline plus the grace, the predecessor looks first, and a person merges the replacement's commit: the merge is
// recorded on the replacement with its receipt.
func TestMergeAfterTheGraceIsRecordedOnTheReplacement(t *testing.T) {
	r, g, p, rr, cr := pastTheGrace(t)
	for range 2 {
		_, _ = reconcileOnce(t, r, p)
	}
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_, _ = reconcileOnce(t, r, p)
		_, _ = reconcileOnce(t, r, rr)
	}
	mergeRecordedOnReplacement(t, storedRR(t, r, rr))
}

// A replacement that ended at the bound of its watch after a refusal, a person merges its commit, and the predecessor
// object is deleted before the replacement's next look: the look records the merge from the pull request and the
// commit alone, with its receipt, and that record settles the lock.
func TestRecoveryMergeRecordedWithThePredecessorGone(t *testing.T) {
	r, g, p, rr, cr, move := endedWatch(t)
	ctx := context.Background()
	g.SetFail(nil)
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	gone := storedRR(t, r, p)
	gone.Finalizers = nil
	if err := r.Update(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, gone); err != nil {
		t.Fatal(err)
	}
	move(time.Hour)
	for range 3 {
		_, _ = reconcileOnce(t, r, rr)
	}
	got := storedRR(t, r, rr)
	mergeRecordedOnReplacement(t, got)
	if got.Status.MergeReceipt.PullRequest != cr.Number {
		t.Fatalf("receipt %+v does not name pull request #%d", got.Status.MergeReceipt, cr.Number)
	}
	if ptr.Deref(got.Status.HoldsTarget, false) {
		t.Fatal("the recorded merge did not settle the lock")
	}
}

// With the predecessor gone, a merged head with a person's commit on top of the replacement's commit (a person pushed
// another change to the target file) is recorded on the replacement, which holds the lock, as a foreign change:
// NeedsIntervention with MergedWithForeignChange, no receipt, the pull request recorded, and the lock released by the
// look after that record (decision of 2026-11-20 on #870). Before, it ended Cancelled with nothing recorded.
func TestRecoveryForeignMergeWithThePredecessorGone(t *testing.T) {
	r, g, p, rr, cr, move := endedWatch(t)
	ctx := context.Background()
	g.SetFail(nil)
	branch := "shadewright/" + string(chainRootOf(p))
	if _, err := g.Push(branch, sourcePath, deploymentManifest(p.Spec.TargetRef.Name, p.Spec.TargetRef.Namespace,
		workerContainer, "768Mi")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	gone := storedRR(t, r, p)
	gone.Finalizers = nil
	if err := r.Update(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, gone); err != nil {
		t.Fatal(err)
	}
	move(time.Hour)
	got, err := reconcileOnce(t, r, rr)
	if err != nil {
		t.Fatal(err)
	}
	foreignRecordedOnReplacement(t, got, cr.Number)
	if !ptr.Deref(got.Status.HoldsTarget, false) {
		t.Fatal("the lock went in the write of the record; it is released by the look after it")
	}
	move(heldRecheckInterval)
	for range 2 {
		got, _ = reconcileOnce(t, r, rr)
	}
	foreignRecordedOnReplacement(t, got, cr.Number)
	if ptr.Deref(got.Status.HoldsTarget, false) {
		t.Fatal("the recorded merge did not settle the lock")
	}
}

// foreignRecordedOnReplacement fails unless rr records the merge of pull request number as a foreign change (#870):
// NeedsIntervention with MergedWithForeignChange, no receipt, the pull request recorded and the handoff Done.
func foreignRecordedOnReplacement(t *testing.T, rr *api.RemediationRequest, number int64) {
	t.Helper()
	c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionApproved)
	if rr.Status.Phase != api.PhaseNeedsIntervention || c == nil || c.Reason != ReasonMergedWithForeignChange ||
		rr.Status.MergeReceipt != nil || rr.Status.PullRequest == nil || rr.Status.PullRequest.Number != number ||
		!tookOver(rr) {
		t.Fatalf("phase %q, Approved %+v, receipt %v, pull request %+v, handoff %q: want the merge recorded as a "+
			"foreign change", rr.Status.Phase, c, rr.Status.MergeReceipt, rr.Status.PullRequest,
			rr.Status.Handoff.From.Phase)
	}
	if !strings.Contains(c.Message, "not the commit Shadewright pushed for this request") {
		t.Fatalf("Approved message %q does not say the merged head is not this request's commit", c.Message)
	}
}

// The plain case of an aborted intent: the handoff was aborted at a refusal, the predecessor resumed and later stopped
// holding the target lock, and a person merges its own head. The merge is recorded on the predecessor; the aborted
// intent hides nothing.
func TestAbortedIntentDoesNotHideTheOwnMerge(t *testing.T) {
	r, g, p, _, cr, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	branch := "shadewright/" + string(chainRootOf(p))
	if err := g.ForcePush(branch, p.Status.PullRequest.HeadSHA); err != nil {
		t.Fatal(err)
	}
	abortIntent(p, ReasonHintHandoffAborted)
	p.Status.Phase = api.PhaseNeedsIntervention
	p.Status.HoldsTarget = new(true)
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: api.ConditionPlanFresh,
		Status: metav1.ConditionFalse, Reason: ReasonGitForbidden, Message: "refused by the test"})
	if err := r.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_, _ = reconcileOnce(t, r, p)
	}
	got := storedRR(t, r, p)
	if !mergeRecorded(got) {
		t.Fatalf("phase %q, receipt %v, conditions %+v: the merge of the predecessor's own head is not recorded",
			got.Status.Phase, got.Status.MergeReceipt, got.Status.Conditions)
	}
}
