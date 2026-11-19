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
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
)

// onHotfix is what a stop's message says of the pull request moved to hotfix.
const onHotfix = "open on another base branch (hotfix)"

// reasonOnOtherBase is the stop reason of a request whose own pull request is open on another base (#892).
const reasonOnOtherBase = "ChangeRequestOnOtherBase"

// movedStopPath is one row of TestStopPathsWithOwnPullRequestMoved: how the request is stopped, and what each look
// must leave once the request's own pull request is open on another base.
type movedStopPath struct {
	name string
	// prepare stops the request (or sets up the stop) in the world of settlementWorld, with the settings naming the
	// recorded target branch unless changed is set.
	prepare func(t *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest)
	changed bool
	// paused keeps the kill switch on for the first reconciles, then lifts it.
	paused bool
	// deleted is the deletion of the request: its finalizer closes the pull request and goes.
	deleted bool
	// phase and reason are what the request ends with, message a text a condition with that reason must hold.
	phase   api.RemediationPhase
	reason  string
	message string
}

// TestStopPathsWithOwnPullRequestMoved walks every stop path with the request's own pull request moved by a person
// to another base branch (hotfix) before the stop is decided (fourth decision of 2026-11-19 on #892). The read of the
// pull request by its number answers open on another base (changeElsewhere) on each of them, and each caller
// handles it: the lock stays held, the request ends with a reason and a message that say the pull request is open on
// another base (a cancel ends Cancelled, whether or not the target branch changed), and nothing is written to the
// pull request. Deletion is the one exception: the finalizer closes the pull request there by its number
// (CloseChangeRequestAnyBase) and only then lets go.
//
// On 31ee66d2 every row failed (gone, "closed", or the finalizer letting go with the pull request open) but cancel
// after a branch change and deadline, which were already right there.
func TestStopPathsWithOwnPullRequestMoved(t *testing.T) {
	for _, p := range []movedStopPath{
		{name: "watch", phase: api.PhaseNeedsIntervention, reason: reasonOnOtherBase, message: onHotfix},
		{name: "cancel", prepare: cancelMoved, phase: api.PhaseCancelled, reason: ReasonCancelRequested,
			message: messageCancelledOpenElsewhere},
		{name: "cancel after a branch change", prepare: cancelMoved, changed: true, phase: api.PhaseCancelled,
			reason: ReasonCancelRequested, message: messageCancelledOpenElsewhere},
		{name: "supersede", prepare: staleMoved, phase: api.PhaseNeedsIntervention, reason: reasonOnOtherBase,
			message: onHotfix},
		{name: focusDeadline, prepare: expireMoved, phase: api.PhaseExpired, reason: ReasonPullRequestDeadline},
		{name: "kill switch", paused: true, phase: api.PhaseNeedsIntervention, reason: reasonOnOtherBase,
			message: onHotfix},
		{name: "cancel, none recorded", prepare: lostMoved(cancelMoved), phase: api.PhaseCancelled,
			reason: ReasonCancelRequested, message: messageCancelledOpenElsewhere},
		{name: "delete", deleted: true},
		{name: "delete, none recorded", prepare: lostMoved(nil), deleted: true},
	} {
		t.Run(p.name, func(t *testing.T) { walkMovedStop(t, p) })
	}
}

// walkMovedStop is one row of TestStopPathsWithOwnPullRequestMoved.
func walkMovedStop(t *testing.T, p movedStopPath) {
	r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
	ctx := context.Background()
	meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionApproved)
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if !p.changed {
		setTarget(t, r, mainBranch)
	}
	n := rr.Status.PullRequest.Number
	moveTo(t, g, n, movedBranch)
	if p.prepare != nil {
		p.prepare(t, r, rr)
	}
	if p.deleted {
		if err := clientfake.AddIndex(r.Client, &api.RemediationRequest{}, clearedField, func(obj client.Object) []string {
			return []string{obj.(*api.RemediationRequest).Status.BreakerClearedBy}
		}); err != nil {
			t.Fatal(err)
		}
		rr.Finalizers = append(rr.Finalizers, FinalizerName)
		if err := r.Update(ctx, rr); err != nil {
			t.Fatal(err)
		}
		if err := r.Delete(ctx, rr); err != nil {
			t.Fatal(err)
		}
	}
	if p.paused {
		setPaused(t, r, true)
	}
	var mu sync.Mutex
	var writes []string
	g.SetFail(func(op string) error {
		if !gitReads[op] {
			mu.Lock()
			writes = append(writes, op)
			mu.Unlock()
		}
		return nil
	})
	clock := clockOf(r)
	key := client.ObjectKeyFromObject(rr)
	live := &api.RemediationRequest{}
	for i := range 10 {
		if p.paused && i == 4 {
			if err := r.Get(ctx, key, live); err != nil {
				t.Fatal(err)
			}
			if live.Status.Phase != api.PhaseAwaitingApproval || !holdsLock(live) || len(writes) > 0 {
				t.Fatalf("paused: phase %s, holds=%v, writes %v: want the request waiting, the lock held, nothing written",
					live.Status.Phase, holdsLock(live), writes)
			}
			setPaused(t, r, false)
		}
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
			t.Logf("Reconcile %d: %v", i, err)
		}
		clock.advance(confirmWait + time.Minute)
	}
	checkMovedStop(t, p, r, key, g.ChangeRequests()[n-1], writes)
}

// checkMovedStop checks what a row of TestStopPathsWithOwnPullRequestMoved left: the request (gone for a deletion)
// and cr, its pull request, after writes, the Git writes made.
func checkMovedStop(t *testing.T, p movedStopPath, r *RemediationRequestReconciler, key client.ObjectKey,
	cr providers.ChangeRequest, writes []string) {
	t.Helper()
	ctx := context.Background()
	n := cr.Number
	live := &api.RemediationRequest{}
	if p.deleted {
		err := r.Get(ctx, key, live)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("the request is still there (%v, finalizers %v): the finalizer did not go", err, live.Finalizers)
		}
		if cr.State != providers.ChangeRequestClosed || !slices.Equal(writes, []string{"CloseChangeRequestAnyBase"}) {
			t.Fatalf("pull request #%d is %s, writes %v: want it closed on %s by its number, once", n, cr.State,
				writes, movedBranch)
		}
		return
	}
	if err := r.Get(ctx, key, live); err != nil {
		t.Fatal(err)
	}
	if live.Status.Phase != p.phase || !holdsLock(live) {
		t.Fatalf("phase %s, holds=%v, conditions %s: want %s with the lock held", live.Status.Phase, holdsLock(live),
			conditionsOf(live), p.phase)
	}
	if !hasReason(live, p.reason, p.message) {
		t.Fatalf("conditions %s: want reason %s saying %q", conditionsOf(live), p.reason, p.message)
	}
	for _, c := range live.Status.Conditions {
		if c.Reason == ReasonChangeRequestGone || strings.Contains(c.Message, "cannot be found") ||
			c.Reason == ReasonPullRequestClosed || c.Reason == ReasonChangeRequestClosed {
			t.Errorf("condition %s says the pull request is gone or closed: %s: %q", c.Type, c.Reason, c.Message)
		}
	}
	held := meta.FindStatusCondition(live.Status.Conditions, api.ConditionTargetHeld)
	if held == nil || !strings.Contains(held.Message, fmt.Sprintf("#%d", n)) || !strings.Contains(held.Message, movedBranch) {
		t.Fatalf("TargetHeld %+v: want it naming #%d on %s", held, n, movedBranch)
	}
	if cr.State != providers.ChangeRequestOpen || len(writes) > 0 {
		t.Fatalf("pull request #%d is %s, writes %v: want it open with nothing written", n, cr.State, writes)
	}
}

// hasReason reports whether a condition of rr has reason and a message that holds message.
func hasReason(rr *api.RemediationRequest, reason, message string) bool {
	return slices.ContainsFunc(rr.Status.Conditions, func(c metav1.Condition) bool {
		return c.Reason == reason && strings.Contains(c.Message, message)
	})
}

// lostMoved is then (when set) on a request whose write that recorded its pull request was lost: only its marker
// finds it.
func lostMoved(then func(*testing.T, *RemediationRequestReconciler, *api.RemediationRequest)) func(*testing.T,
	*RemediationRequestReconciler, *api.RemediationRequest) {
	return func(t *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest) {
		t.Helper()
		rr.Status.PullRequest = nil
		meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionPROpened)
		if err := r.Status().Update(context.Background(), rr); err != nil {
			t.Fatal(err)
		}
		if then != nil {
			then(t, r, rr)
		}
	}
}

// cancelMoved sets spec.cancel.
func cancelMoved(t *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest) {
	t.Helper()
	rr.Spec.Cancel = true
	if err := r.Update(context.Background(), rr); err != nil {
		t.Fatal(err)
	}
}

// staleMoved makes the plan stale: its policy is deleted.
func staleMoved(t *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest) {
	t.Helper()
	pol := &api.RemediationPolicy{ObjectMeta: metav1.ObjectMeta{Name: string(rr.Spec.PolicyRef)}}
	if err := r.Delete(context.Background(), pol); err != nil {
		t.Fatal(err)
	}
}

// expireMoved is a request in PolicyChecked past its deadline whose pull request was opened while the write that
// would record it was lost: only its marker finds it.
func expireMoved(t *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest) {
	t.Helper()
	rr.Status.Phase = api.PhasePolicyChecked
	rr.Status.PullRequest = nil
	rr.Status.Conditions = nil
	rr.Status.Deadline = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	if err := r.Status().Update(context.Background(), rr); err != nil {
		t.Fatal(err)
	}
}

// setTarget makes the settings name branch as the repository's target branch.
func setTarget(t *testing.T, r *RemediationRequestReconciler, branch string) {
	t.Helper()
	ctx := context.Background()
	settings := &api.RemediationSettings{}
	if err := r.Get(ctx, client.ObjectKey{Name: api.SettingsName}, settings); err != nil {
		t.Fatal(err)
	}
	settings.Spec.Repositories[0].TargetBranch = api.GitBranch(branch)
	if err := r.Update(ctx, settings); err != nil {
		t.Fatal(err)
	}
}
