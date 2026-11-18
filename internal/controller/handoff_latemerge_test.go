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
	"fmt"
	"slices"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// The end of the watch after a refused recovery is not a verdict and does not end the observation (third decision of
// 2026-11-18 on #786, point 2): the replacement stops NeedsIntervention with RecoveryWaitExpired, keeps the target lock
// and its handoff Reserved, and the look at stopped holders records a merge of its commit with its receipt whenever it
// comes; the lock goes on a verdict only. And the predecessor bounds a standing intent from its own recorded state
// (point 3).

// endedWatch is a replacement whose recovery met a refusal and whose watch then reached its end, with the clock the
// test moves.
func endedWatch(t *testing.T) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest,
	*api.RemediationRequest, providers.ChangeRequest, func(time.Duration)) {
	t.Helper()
	r, g, p, rr, cr, _ := handoffAppendedWorld(t, nil)
	move := movableClock(r)
	refuseAll(g)
	if _, err := reconcileOnce(t, r, rr); err != nil {
		t.Fatal(err)
	}
	move(recoveryWaitBound + time.Minute)
	got, err := reconcileOnce(t, r, rr)
	if err != nil {
		t.Fatal(err)
	}
	endedAtTheBound(t, got)
	if got.Status.Handoff.From.Phase != api.HandoffReserved {
		t.Fatalf("the end moved status.handoff.from to %q; it stays Reserved", got.Status.Handoff.From.Phase)
	}
	pred := &api.RemediationRequest{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(p), pred); err != nil {
		t.Fatal(err)
	}
	if pred.Status.Handoff.To.Phase != api.HandoffAborted {
		t.Fatalf("the predecessor's intent is %q at the end of the watch, want Aborted", pred.Status.Handoff.To.Phase)
	}
	return r, g, pred, got, cr, move
}

// mergeRecordedOnReplacement fails unless the merge of the replacement's commit is recorded on rr with its receipt,
// as the takeover of a merged pull request records it.
func mergeRecordedOnReplacement(t *testing.T, rr *api.RemediationRequest) {
	t.Helper()
	merged := []api.RemediationPhase{api.PhaseMerged, api.PhaseInconclusive, api.PhaseWaitingForDelivery,
		api.PhaseVerifying, api.PhaseSucceeded}
	if !slices.Contains(merged, rr.Status.Phase) || rr.Status.MergeReceipt == nil || !tookOver(rr) ||
		rr.Status.PullRequest == nil {
		t.Fatalf("phase %q, receipt %v, handoff %q: want the merge of the replacement's commit recorded with its "+
			"receipt", rr.Status.Phase, rr.Status.MergeReceipt, rr.Status.Handoff.From.Phase)
	}
}

// A merge of the replacement's commit one hour, or three days, after the end of the watch: the look at the stopped
// holder records it on the replacement with its receipt, and releases the lock with that record. The predecessor,
// whose intent is Aborted, looks at the same merged pull request first and records nothing of it as its own.
func TestRecoveryMergeAfterTheEndIsRecorded(t *testing.T) {
	for _, after := range []time.Duration{time.Hour, 72 * time.Hour} {
		t.Run(after.String(), func(t *testing.T) {
			r, g, p, rr, cr, move := endedWatch(t)
			ctx := context.Background()
			g.SetFail(nil)
			// Looks while the pull request is open keep the lock and record nothing.
			move(after / 2)
			got, err := reconcileOnce(t, r, rr)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status.Phase != api.PhaseNeedsIntervention || !ptr.Deref(got.Status.HoldsTarget, false) {
				t.Fatalf("phase %q, holdsTarget %v: an open pull request keeps the ended replacement and its lock",
					got.Status.Phase, got.Status.HoldsTarget)
			}
			if _, err := g.MergeWithCommit(cr.Number); err != nil {
				t.Fatal(err)
			}
			move(after / 2)
			for range 3 {
				_, _ = reconcileOnce(t, r, p)
			}
			for range 3 {
				_, _ = reconcileOnce(t, r, rr)
			}
			got, _ = reconcileOnce(t, r, rr)
			mergeRecordedOnReplacement(t, got)
			if ptr.Deref(got.Status.HoldsTarget, false) {
				t.Fatal("the recorded merge did not settle the lock")
			}
			if got.Status.MergeReceipt.PullRequest != cr.Number {
				t.Fatalf("receipt %+v does not name pull request #%d", got.Status.MergeReceipt, cr.Number)
			}
			pred := &api.RemediationRequest{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(p), pred); err != nil {
				t.Fatal(err)
			}
			if pred.Status.MergeReceipt != nil || pred.Status.Phase == api.PhaseMerged ||
				pred.Status.Phase == api.PhaseInconclusive || pred.Status.Phase == api.PhaseSucceeded {
				t.Fatalf("the predecessor recorded the replacement's merge as its own: phase %q, receipt %v",
					pred.Status.Phase, pred.Status.MergeReceipt)
			}
		})
	}
}

// A close after the end of the watch is a verdict: the lock is released, nothing is recorded as merged, and the
// replacement stays NeedsIntervention with RecoveryWaitExpired and its handoff Reserved.
func TestRecoveryCloseAfterTheEndReleasesTheLock(t *testing.T) {
	r, g, _, rr, cr, move := endedWatch(t)
	g.SetFail(nil)
	if err := g.CloseChangeRequest(context.Background(), cr.Number); err != nil {
		t.Fatal(err)
	}
	move(time.Hour)
	for range 2 {
		_, _ = reconcileOnce(t, r, rr)
	}
	got, _ := reconcileOnce(t, r, rr)
	opened := meta.FindStatusCondition(got.Status.Conditions, api.ConditionPROpened)
	if ptr.Deref(got.Status.HoldsTarget, false) || got.Status.MergeReceipt != nil ||
		got.Status.Phase != api.PhaseNeedsIntervention || opened == nil || opened.Reason != ReasonRecoveryWaitExpired ||
		got.Status.Handoff.From.Phase != api.HandoffReserved {
		t.Fatalf("phase %q, PullRequestOpened %+v, handoff %q, holdsTarget %v, receipt %v: want the lock released "+
			"on the close and nothing else changed", got.Status.Phase, opened, got.Status.Handoff.From.Phase,
			got.Status.HoldsTarget, got.Status.MergeReceipt)
	}
}

// invalidReplacementWrites makes every status write of the replacement fail as the API server refuses an invalid
// object.
func invalidReplacementWrites(r *RemediationRequestReconciler) {
	base := r.Client.(client.WithWatch)
	invalid := func(obj client.Object) error {
		return apierrors.NewInvalid(api.GroupVersion.WithKind("RemediationRequest").GroupKind(), obj.GetName(),
			field.ErrorList{field.Invalid(field.NewPath("status"), nil, "refused by the test")})
	}
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption) error {
			if obj.GetName() == newName {
				return invalid(obj)
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if obj.GetName() == newName {
				return invalid(obj)
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	r.Client, r.APIReader = c, c
}

// The replacement cannot store its refusal (every status write of it is refused), so it never aborts the intent. The
// predecessor ends the standing intent itself at its own end: status.handoff.to.ends when it records one, else the
// replacement's deadline plus the grace (an intent recorded before the field), never earlier, and its finalizer
// stops waiting for the replacement then.
func TestPredecessorEndsAStandingIntentAtItsOwnEnd(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		t.Run(fmt.Sprintf("ends=%t", recorded), func(t *testing.T) {
			r, g, p, rr, _, _ := handoffAppendedWorld(t, nil)
			move := movableClock(r)
			ctx := context.Background()
			end := r.now().Add(time.Hour + handoffRecoveryGrace).Truncate(time.Second)
			rr.Status.Deadline = &metav1.Time{Time: r.now().Add(time.Hour).Truncate(time.Second)}
			if recorded {
				// The predecessor's own record decides, not the replacement's deadline.
				end = r.now().Add(30 * time.Minute).Truncate(time.Second)
				rr.Status.Deadline = &metav1.Time{Time: r.now().Add(5 * time.Hour)}
				p.Status.Handoff.To.Ends = &metav1.Time{Time: end}
				if err := r.Status().Update(ctx, p); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Status().Update(ctx, rr); err != nil {
				t.Fatal(err)
			}
			invalidReplacementWrites(r)
			g.SetFail(func(op string) error {
				if op == opBranchHead {
					return fmt.Errorf("status 403: %w", providers.ErrForbidden)
				}
				return nil
			})
			standing := func() *api.RemediationRequest {
				t.Helper()
				for range 2 {
					_, _ = reconcileOnce(t, r, rr)
				}
				_, _ = reconcileOnce(t, r, p)
				got := &api.RemediationRequest{}
				if err := r.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
					t.Fatal(err)
				}
				return got
			}
			move(end.Sub(r.now()) - time.Minute)
			if got := standing(); intent(got) == nil {
				t.Fatalf("the intent ended before its end: %+v", got.Status.Handoff.To)
			} else if _, wait, err := r.handoffHolds(ctx, got.DeepCopy()); err != nil || !wait {
				t.Fatalf("handoffHolds before the end = wait %v, %v; want it to wait for the replacement", wait, err)
			}
			repl := &api.RemediationRequest{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), repl); err != nil {
				t.Fatal(err)
			}
			if abortedOnRefusal(repl) {
				t.Fatal("the replacement stored its refusal; the test wants every write of it refused")
			}
			move(time.Minute)
			got := standing()
			if intent(got) != nil || got.Status.Handoff.To.Phase != api.HandoffAborted {
				t.Fatalf("the intent stands past its end %v: %+v", end, got.Status.Handoff.To)
			}
			if h := hintByRef(got, got.Status.Handoff.To.Hint); h == nil || h.State != api.HintDeclined {
				t.Fatalf("hint %+v, want it Declined with the aborted intent", h)
			}
		})
	}
}

// The intent records its end: the replacement's deadline plus the grace.
func TestReserveRecordsTheEndOfTheIntent(t *testing.T) {
	r, _, p, rr, _, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	p.Status.Handoff = nil
	p.Status.Hints[0].State = api.HintPending
	if err := r.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	deadline := r.now().Add(time.Hour).Truncate(time.Second)
	rr.Status.Deadline = &metav1.Time{Time: deadline}
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reserve(ctx, rr, p); err != nil {
		t.Fatal(err)
	}
	got := &api.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
		t.Fatal(err)
	}
	to := intent(got)
	if to == nil || to.Ends == nil || !to.Ends.Time.Equal(deadline.Add(handoffRecoveryGrace)) {
		t.Fatalf("intent %+v, want it to end at the replacement's deadline plus the grace", to)
	}
}
