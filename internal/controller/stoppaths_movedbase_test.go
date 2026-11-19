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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
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
	// adopt is a request in PolicyChecked that lost the write recording its pull request (adoptMoved).
	adopt bool
	// recorded is a request that lost the write recording its pull request and must record it again from the read.
	recorded bool
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
// after a branch change and deadline, which were already right there. On 27f1eded the adoption row (the write
// recording the pull request lost, then the move, then the pull request step) opened a second pull request to the
// target branch.
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
		{name: "adoption", adopt: true, phase: api.PhaseNeedsIntervention, reason: reasonOnOtherBase,
			message: onHotfix, recorded: true},
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
	if p.adopt {
		adoptMoved(t, r, g, rr)
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
	if crs := g.ChangeRequests(); len(crs) != 1 {
		t.Fatalf("pull requests %+v: want #%d alone, no second one (invariant 5)", crs, n)
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
	if p.recorded && (live.Status.PullRequest == nil || live.Status.PullRequest.Number != n) {
		t.Fatalf("status.pullRequest %+v: want #%d recorded again", live.Status.PullRequest, n)
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

// adoptMoved is a request in PolicyChecked, on a fresh plan, whose pull request was opened while the write that
// would record it was lost: only its marker finds it, and the pull request step (adoption) reads the branch before
// it opens one. Its Incident stands.
func adoptMoved(t *testing.T, r *RemediationRequestReconciler, g *fakegit.Git, rr *api.RemediationRequest) {
	t.Helper()
	ctx := context.Background()
	freshPlanOf(t, r, rr)
	if err := g.SetBody(rr.Status.PullRequest.Number, marker(rr)); err != nil {
		t.Fatal(err)
	}
	ref := rr.Spec.IncidentRef
	inc := &api.Incident{ObjectMeta: metav1.ObjectMeta{Namespace: rr.Namespace, Name: ref.Name, UID: types.UID(ref.UID)}}
	if err := r.Create(ctx, inc); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	rr.Status.Phase = api.PhasePolicyChecked
	rr.Status.PullRequest = nil
	rr.Status.Conditions = nil
	rr.Status.Deadline = &metav1.Time{Time: time.Now().Add(time.Hour)}
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
}

// freshPlanOf makes rr's plan fresh at the head of its target branch, as the freshness check computes it, and the
// source mapping find its file there.
func freshPlanOf(t *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest) {
	t.Helper()
	ctx := context.Background()
	git, err := r.OpenRepository(ctx, api.GitRepository{TargetBranch: rr.Status.Source.TargetBranch})
	if err != nil {
		t.Fatal(err)
	}
	head, err := git.BranchHead(ctx, string(rr.Status.Source.TargetBranch))
	if err != nil {
		t.Fatal(err)
	}
	pol := &api.RemediationPolicy{}
	settings := &api.RemediationSettings{}
	if err := r.Get(ctx, client.ObjectKey{Name: string(rr.Spec.PolicyRef)}, pol); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKey{Name: api.SettingsName}, settings); err != nil {
		t.Fatal(err)
	}
	rr.Status.Source.BaseCommit = head
	res := resolved()
	res.Source.BaseCommit = head
	r.Source = &stubResolver{result: res}
	if rr.Status.PlanDigest, err = freshness.ComputeDigest(rr, rr.Status.Source, pol, settings); err != nil {
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

// TestAdoptionOfOwnPullRequestEndedOnAnotherBase is the adoption (a request in PolicyChecked that lost the write
// recording its pull request) when a person moved that pull request to another base (hotfix) and closed or merged it
// there. The listing of the branch reads the target branch only, and the listing of every base reads open pull
// requests only (ListOpenChangeRequestsAnyBase), so neither shows it: the request opens a pull request to the target
// branch from its branch, which holds the planned commit, and waits for its review. The pull request on hotfix is
// left as it is. It pins what the code does; a request that recorded its pull request stops instead (closed: the
// watch reads it closed; merged elsewhere: gone).
func TestAdoptionOfOwnPullRequestEndedOnAnotherBase(t *testing.T) {
	for _, merged := range []bool{false, true} {
		t.Run(map[bool]string{false: foundClosed, true: endMerged}[merged], func(t *testing.T) {
			r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
			ctx := context.Background()
			setTarget(t, r, mainBranch)
			n := rr.Status.PullRequest.Number
			moveTo(t, g, n, movedBranch)
			adoptMoved(t, r, g, rr)
			if merged {
				if _, err := g.Merge(n); err != nil {
					t.Fatal(err)
				}
			} else if err := g.CloseChangeRequest(ctx, n); err != nil {
				t.Fatal(err)
			}
			key := client.ObjectKeyFromObject(rr)
			clock := clockOf(r)
			for i := range 6 {
				if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
					t.Logf("Reconcile %d: %v", i, err)
				}
				clock.advance(time.Minute)
			}
			live := &api.RemediationRequest{}
			if err := r.Get(ctx, key, live); err != nil {
				t.Fatal(err)
			}
			crs := g.ChangeRequests()
			if len(crs) != 2 || crs[1].BaseBranch != mainBranch || crs[1].State != providers.ChangeRequestOpen ||
				!hasMarker(crs[1].Body, live) {
				t.Fatalf("pull requests %+v: want #%d left on %s and one opened to %s with the marker", crs, n,
					movedBranch, mainBranch)
			}
			if crs[0].BaseBranch != movedBranch || (crs[0].State == providers.ChangeRequestMerged) != merged {
				t.Fatalf("pull request #%d is %s on %s: want it left as it was", n, crs[0].State, crs[0].BaseBranch)
			}
			if pr := live.Status.PullRequest; pr == nil || pr.Number != crs[1].Number ||
				live.Status.Phase != api.PhaseAwaitingApproval {
				t.Fatalf("phase %s, pull request %+v: want #%d recorded and waiting for review", live.Status.Phase,
					pr, crs[1].Number)
			}
		})
	}
}
