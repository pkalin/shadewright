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
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// onHotfix is what a stop's message says of the pull request moved to hotfix.
const onHotfix = "open on another base branch (hotfix)"

// opCloseAnyBase is the finalizer's close on any base.
const opCloseAnyBase = "CloseChangeRequestAnyBase"

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
	// ended is set when a person, after the move, closed (foundClosed) or merged (endMerged) the pull request on the
	// other base: it no longer keeps the lock, and nothing is opened next to it.
	ended string
}

// mergedOnHotfix is what a stop's message says of the pull request merged into hotfix.
const mergedOnHotfix = "merged into another base branch (hotfix)"

// TestStopPathsWithOwnPullRequestMoved walks every stop path with the request's own pull request moved by a person
// to another base branch (hotfix) before the stop is decided (fourth decision of 2026-11-19 on #892). The read of the
// pull request by its number answers open on another base (changeElsewhere) on each of them, and each caller
// handles it: the lock stays held, the request ends with a reason and a message that say the pull request is open on
// another base (a cancel ends Cancelled, whether or not the target branch changed), and nothing is written to the
// pull request. Deletion is the one exception: the finalizer closes the pull request there by its number
// (CloseChangeRequestAnyBase) and only then lets go.
//
// The rows with ended set have the pull request closed or merged on the other base after the move. The adoption
// lists every state on every base, so it finds one closed there and stops as for one closed on the target branch,
// and one merged there and stops as the watch does for a recorded one: NeedsIntervention with
// ChangeRequestOnOtherBase, naming it, since a merge into another branch delivers nothing the request can verify.
// Neither opens a second pull request (invariant 5).
//
// On 31ee66d2 every row failed (gone, "closed", or the finalizer letting go with the pull request open) but cancel
// after a branch change and deadline, which were already right there. On 27f1eded the adoption row (the write
// recording the pull request lost, then the move, then the pull request step) opened a second pull request to the
// target branch. On f6dc775c both adoption rows with ended set opened a second one, and the watch of a recorded one
// merged there stopped with ChangeRequestGone ("cannot be found any more").
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
		{name: "adoption, closed there", adopt: true, ended: foundClosed, phase: api.PhaseNeedsIntervention,
			reason: ReasonChangeRequestClosed, message: "closed without merging"},
		{name: "adoption, merged there", adopt: true, ended: endMerged, phase: api.PhaseNeedsIntervention,
			reason: reasonOnOtherBase, message: mergedOnHotfix, recorded: true},
		{name: "watch, merged there", ended: endMerged, phase: api.PhaseNeedsIntervention, reason: reasonOnOtherBase,
			message: mergedOnHotfix, recorded: true},
		{name: "cancel, merged there", prepare: cancelMoved, ended: endMerged, phase: api.PhaseNeedsIntervention,
			reason: reasonOnOtherBase, message: mergedOnHotfix, recorded: true},
		{name: "cancel after a branch change, merged there", prepare: cancelMoved, changed: true, ended: endMerged,
			phase: api.PhaseNeedsIntervention, reason: reasonOnOtherBase, message: mergedOnHotfix, recorded: true},
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
		setTargetMain(t, r)
	}
	n := rr.Status.PullRequest.Number
	moveTo(t, g, n, movedBranch)
	if p.prepare != nil {
		p.prepare(t, r, rr)
	}
	if p.adopt {
		adoptMoved(t, r, g, rr)
	}
	switch p.ended {
	case foundClosed:
		if err := g.Close(n); err != nil {
			t.Fatal(err)
		}
	case endMerged:
		if _, err := g.Merge(n); err != nil {
			t.Fatal(err)
		}
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
		if cr.State != providers.ChangeRequestClosed || !slices.Equal(writes, []string{opCloseAnyBase}) {
			t.Fatalf("pull request #%d is %s, writes %v: want it closed on %s by its number, once", n, cr.State,
				writes, movedBranch)
		}
		return
	}
	if err := r.Get(ctx, key, live); err != nil {
		t.Fatal(err)
	}
	if p.ended != "" {
		checkEndedMoved(t, p, live, cr, writes)
		return
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

// checkEndedMoved is checkMovedStop for a row whose pull request was closed or merged on the other base: the request
// stops with the row's reason and message, the lock is released, the pull request is left as the person left it, and
// nothing is written to it.
func checkEndedMoved(t *testing.T, p movedStopPath, live *api.RemediationRequest, cr providers.ChangeRequest,
	writes []string) {
	t.Helper()
	n := cr.Number
	if live.Status.Phase != p.phase || holdsLock(live) {
		t.Fatalf("phase %s, holds=%v, conditions %s: want %s with the lock released", live.Status.Phase,
			holdsLock(live), conditionsOf(live), p.phase)
	}
	if p.recorded && (live.Status.PullRequest == nil || live.Status.PullRequest.Number != n) {
		t.Fatalf("status.pullRequest %+v: want #%d recorded", live.Status.PullRequest, n)
	}
	if !hasReason(live, p.reason, p.message) {
		t.Fatalf("conditions %s: want reason %s saying %q", conditionsOf(live), p.reason, p.message)
	}
	if p.ended == endMerged && !hasReason(live, p.reason, fmt.Sprintf("#%d", n)) {
		t.Fatalf("conditions %s: want reason %s naming #%d", conditionsOf(live), p.reason, n)
	}
	for _, c := range live.Status.Conditions {
		if c.Reason == ReasonChangeRequestGone || strings.Contains(c.Message, "cannot be found") ||
			c.Reason == ReasonChangeRequestMerged || live.Status.MergeReceipt != nil {
			t.Errorf("condition %s says the pull request is gone or merged into the target: %s: %q", c.Type, c.Reason,
				c.Message)
		}
	}
	want := map[string]providers.ChangeRequestState{foundClosed: providers.ChangeRequestClosed,
		endMerged: providers.ChangeRequestMerged}[p.ended]
	if cr.State != want || cr.BaseBranch != movedBranch || len(writes) > 0 {
		t.Fatalf("pull request #%d is %s on %s, writes %v: want it %s on %s with nothing written", n, cr.State,
			cr.BaseBranch, writes, want, movedBranch)
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

// setTargetMain makes the settings name main as the repository's target branch.
func setTargetMain(t *testing.T, r *RemediationRequestReconciler) {
	t.Helper()
	ctx := context.Background()
	settings := &api.RemediationSettings{}
	if err := r.Get(ctx, client.ObjectKey{Name: api.SettingsName}, settings); err != nil {
		t.Fatal(err)
	}
	settings.Spec.Repositories[0].TargetBranch = api.GitBranch(mainBranch)
	if err := r.Update(ctx, settings); err != nil {
		t.Fatal(err)
	}
}

// TestAdoptionPicksOwnPullRequestOnEveryBase is the adoption with two pull requests with the request's marker on
// bases other than the target branch, one older (the request's, moved to hotfix) and one newer (opened by a person
// from the branch to a second base, with the marker copied). The rule is open, then merged, then closed, whatever
// their age: the request records and stops on the one that wins, and opens nothing.
func TestAdoptionPicksOwnPullRequestOnEveryBase(t *testing.T) {
	const second = "hotfix-2"
	for _, c := range []struct {
		name         string
		older, newer string // "", foundClosed or endMerged: left open, closed or merged
		winsNewer    bool
		reason, says string
	}{
		{name: "older open, newer closed", newer: foundClosed, reason: reasonOnOtherBase, says: onHotfix},
		{name: "older closed, newer open", older: foundClosed, winsNewer: true, reason: reasonOnOtherBase,
			says: "open on another base branch (" + second + ")"},
		{name: "older merged, newer closed", older: endMerged, newer: foundClosed, reason: reasonOnOtherBase,
			says: mergedOnHotfix},
		{name: "older closed, newer merged", older: foundClosed, newer: endMerged, winsNewer: true,
			reason: reasonOnOtherBase, says: "merged into another base branch (" + second + ")"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
			ctx := context.Background()
			setTargetMain(t, r)
			n := rr.Status.PullRequest.Number
			moveTo(t, g, n, movedBranch)
			adoptMoved(t, r, g, rr)
			endAs(t, g, n, c.older)
			head, err := g.BranchHead(ctx, mainBranch)
			if err != nil {
				t.Fatal(err)
			}
			if err := g.CreateBranch(ctx, second, head); err != nil {
				t.Fatal(err)
			}
			m, err := g.AddOpenChangeRequest(freshness.BranchName(rr), second, marker(rr))
			if err != nil {
				t.Fatal(err)
			}
			endAs(t, g, m, c.newer)
			key := client.ObjectKeyFromObject(rr)
			for i := range 4 {
				if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
					t.Logf("Reconcile %d: %v", i, err)
				}
				clockOf(r).advance(time.Minute)
			}
			live := &api.RemediationRequest{}
			if err := r.Get(ctx, key, live); err != nil {
				t.Fatal(err)
			}
			if crs := g.ChangeRequests(); len(crs) != 2 {
				t.Fatalf("pull requests %+v: want #%d and #%d alone, none opened", crs, n, m)
			}
			want := n
			if c.winsNewer {
				want = m
			}
			if pr := live.Status.PullRequest; pr == nil || pr.Number != want {
				t.Fatalf("status.pullRequest %+v: want #%d", pr, want)
			}
			if live.Status.Phase != api.PhaseNeedsIntervention || !hasReason(live, c.reason, c.says) ||
				!hasReason(live, c.reason, fmt.Sprintf("#%d", want)) {
				t.Fatalf("phase %s, conditions %s: want NeedsIntervention, %s saying %q of #%d", live.Status.Phase,
					conditionsOf(live), c.reason, c.says, want)
			}
		})
	}
}

// endAs closes (foundClosed) or merges (endMerged) pull request n, or leaves it open ("").
func endAs(t *testing.T, g *fakegit.Git, n int64, end string) {
	t.Helper()
	switch end {
	case foundClosed:
		if err := g.Close(n); err != nil {
			t.Fatal(err)
		}
	case endMerged:
		if _, err := g.Merge(n); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAdoptionListingPastTheBoundOpensNothing is the adoption when the branch has more pull requests on other bases
// than MaxBranchChangeRequests, all closed: the listing of every base in every state answers ErrTooLarge, and the
// adoption fails closed. Nothing is opened, the request stays where it is, the reconcile returns no error for the
// rate limiter but a wait of the poll interval at least, and condition GitReadRetry says the read is retried.
func TestAdoptionListingPastTheBoundOpensNothing(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
	ctx := context.Background()
	setTargetMain(t, r)
	n := rr.Status.PullRequest.Number
	moveTo(t, g, n, movedBranch)
	adoptMoved(t, r, g, rr)
	if err := g.Close(n); err != nil {
		t.Fatal(err)
	}
	for range providers.MaxBranchChangeRequests {
		m, err := g.AddOpenChangeRequest(freshness.BranchName(rr), movedBranch, "a person's")
		if err != nil {
			t.Fatal(err)
		}
		if err := g.Close(m); err != nil {
			t.Fatal(err)
		}
	}
	all := len(g.ChangeRequests())
	key := client.ObjectKeyFromObject(rr)
	live := &api.RemediationRequest{}
	// The first reconciles take the steps before the look (the finalizer); the looks after them wait.
	for i := range 6 {
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		if err != nil {
			t.Fatalf("Reconcile %d: %v; want a paced retry, not an error", i, err)
		}
		if i >= 3 && res.RequeueAfter < r.pullRequestPollInterval() {
			t.Fatalf("Reconcile %d: result %+v: a retry sooner than the poll interval", i, res)
		}
		clockOf(r).advance(time.Minute)
	}
	if err := r.Get(ctx, key, live); err != nil {
		t.Fatal(err)
	}
	if crs := g.ChangeRequests(); len(crs) != all {
		t.Fatalf("%d pull requests: want %d, none opened past the bound", len(crs), all)
	}
	if live.Status.Phase != api.PhasePolicyChecked || live.Status.PullRequest != nil {
		t.Fatalf("phase %s, pull request %+v: want the request waiting in PolicyChecked", live.Status.Phase,
			live.Status.PullRequest)
	}
	if c := meta.FindStatusCondition(live.Status.Conditions, api.ConditionGitReadRetry); c == nil ||
		c.Status != metav1.ConditionTrue {
		t.Fatalf("GitReadRetry %+v, conditions %s: want it saying the read is retried", c, conditionsOf(live))
	}
}

// TestDeletionClosesEveryOwnPullRequestOnEveryBase deletes a request whose own pull request a person moved to hotfix,
// while a second pull request from its branch with its marker is open on hotfix-2 and a third without the marker is
// open on hotfix-3. The finalizer closes both with the marker, not only the one the listing ranks first, leaves the
// third open, and only then goes, with the number recorded or lost. A close that fails keeps the finalizer, and the
// next reconcile closes what is left.
func TestDeletionClosesEveryOwnPullRequestOnEveryBase(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprintf("record lost %v", lost), func(t *testing.T) {
			r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
			ctx := context.Background()
			setTargetMain(t, r)
			n := rr.Status.PullRequest.Number
			if err := g.SetBody(n, marker(rr)); err != nil {
				t.Fatal(err)
			}
			moveTo(t, g, n, movedBranch)
			if lost {
				lostMoved(nil)(t, r, rr)
			}
			head, err := g.BranchHead(ctx, mainBranch)
			if err != nil {
				t.Fatal(err)
			}
			others := make([]int64, 0, 2)
			for _, c := range []struct{ base, body string }{{"hotfix-2", marker(rr)}, {"hotfix-3", "a person's"}} {
				if err := g.CreateBranch(ctx, c.base, head); err != nil {
					t.Fatal(err)
				}
				m, err := g.AddOpenChangeRequest(freshness.BranchName(rr), c.base, c.body)
				if err != nil {
					t.Fatal(err)
				}
				others = append(others, m)
			}
			second, foreign := others[0], others[1]
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
			key := client.ObjectKeyFromObject(rr)
			// Every close after the first fails: the finalizer stays while one of the two is open.
			closes := 0
			g.SetFail(func(op string) error {
				if op == opCloseAnyBase {
					if closes++; closes > 1 {
						return errors.New("status 502: bad gateway")
					}
				}
				return nil
			})
			for i := range 3 {
				if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
					t.Logf("Reconcile %d: %v", i, err)
				}
				clockOf(r).advance(confirmWait + time.Minute)
			}
			live := &api.RemediationRequest{}
			if err := r.Get(ctx, key, live); err != nil {
				t.Fatalf("the finalizer went while a close failed: %v", err)
			}
			if crs := g.ChangeRequests(); crs[n-1].State != providers.ChangeRequestOpen &&
				crs[second-1].State != providers.ChangeRequestOpen {
				t.Fatalf("pull requests #%d and #%d are closed while every close after the first fails", n, second)
			}
			g.SetFail(nil)
			for i := range 3 {
				if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
					t.Logf("Reconcile %d: %v", i, err)
				}
				clockOf(r).advance(confirmWait + time.Minute)
			}
			if err := r.Get(ctx, key, live); !apierrors.IsNotFound(err) {
				t.Fatalf("the request is still there (%v, finalizers %v)", err, live.Finalizers)
			}
			crs := g.ChangeRequests()
			for _, m := range []int64{n, second} {
				if crs[m-1].State != providers.ChangeRequestClosed {
					t.Errorf("pull request #%d with the marker on %s is %s: want it closed", m, crs[m-1].BaseBranch,
						crs[m-1].State)
				}
			}
			if crs[foreign-1].State != providers.ChangeRequestOpen {
				t.Errorf("pull request #%d without the marker is %s: want it left open", foreign, crs[foreign-1].State)
			}
		})
	}
}

// A stop's close of a recorded pull request that a person moved to hotfix and closed there has nothing to close: the
// close to the target branch would be refused there (as the GitHub adapter refuses it), so none is asked for.
func TestCloseOfOwnClosedOnMovedBaseClosesNothing(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
	ctx := context.Background()
	setTargetMain(t, r)
	n := rr.Status.PullRequest.Number
	if err := g.SetBody(n, marker(rr)); err != nil {
		t.Fatal(err)
	}
	moveTo(t, g, n, movedBranch)
	if err := g.Close(n); err != nil {
		t.Fatal(err)
	}
	var writes []string
	g.SetFail(func(op string) error {
		if !gitReads[op] {
			writes = append(writes, op)
		}
		return nil
	})
	if err := r.closeChangeRequest(ctx, rr); err != nil {
		t.Fatalf("closeChangeRequest: %v, want nothing to close", err)
	}
	if len(writes) > 0 {
		t.Fatalf("writes %v: want none", writes)
	}
}

// A pull request found by its marker and refused before it was recorded (refuseOpened) that a person moved to hotfix
// and merged there is not closed (the close to the target branch would be refused there, and retried without an
// end): the request stops as the watch stops it, NeedsIntervention with ChangeRequestOnOtherBase naming the merge.
func TestRefuseOpenedMergedOnMovedBaseStops(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
	ctx := context.Background()
	setTargetMain(t, r)
	n := rr.Status.PullRequest.Number
	if err := g.SetBody(n, marker(rr)); err != nil {
		t.Fatal(err)
	}
	moveTo(t, g, n, movedBranch)
	if _, err := g.Merge(n); err != nil {
		t.Fatal(err)
	}
	cr := g.ChangeRequests()[n-1]
	err := r.refuseOpened(ctx, g, rr, cr, &renderRefusal{reason: ReasonHeadMoved, message: headNotPlanned})
	if !errors.Is(err, errStopped) {
		t.Fatalf("refuseOpened: %v, want the request stopped", err)
	}
	live := &api.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
		t.Fatal(err)
	}
	if live.Status.Phase != api.PhaseNeedsIntervention || !hasReason(live, reasonOnOtherBase, mergedOnHotfix) {
		t.Fatalf("phase %s, conditions %s: want NeedsIntervention with %s saying %q", live.Status.Phase,
			conditionsOf(live), reasonOnOtherBase, mergedOnHotfix)
	}
}

// A cancel whose recorded pull request a person moved to hotfix after the look at it (outcome prOpen) reads it by its
// number before the failed review check is written: found open there, it gets the failed check on its head (#925,
// decision of 2026-11-19: moved back, it cannot merge on a check written before the cancel), and the request ends
// Cancelled saying it is open on another base. A failed read writes no check and ends nothing.
func TestCancelReadsBeforeTheCheckOnMovedBase(t *testing.T) {
	for _, failRead := range []bool{false, true} {
		t.Run(fmt.Sprintf("read fails %v", failRead), func(t *testing.T) {
			r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
			ctx := context.Background()
			r.ReviewerHints = true
			setTargetMain(t, r)
			meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionApproved)
			if err := r.Status().Update(ctx, rr); err != nil {
				t.Fatal(err)
			}
			cancelMoved(t, r, rr)
			n := rr.Status.PullRequest.Number
			moveTo(t, g, n, movedBranch)
			var writes []string
			g.SetFail(func(op string) error {
				if failRead && op == opGet {
					return errors.New("status 502: bad gateway")
				}
				if !gitReads[op] {
					writes = append(writes, op)
				}
				return nil
			})
			if _, err := r.cancelOpen(ctx, rr, prOpen); err != nil {
				t.Logf("cancelOpen: %v", err)
			}
			if wrote := slices.Contains(writes, "CreateCheckRun"); wrote == failRead {
				t.Fatalf("writes %v: want the failed check written on the head once the read finds the pull request "+
					"open on another base, and nothing written on a failed read", writes)
			}
			live := &api.RemediationRequest{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
				t.Fatal(err)
			}
			if failRead {
				if live.Status.Phase != api.PhaseAwaitingApproval {
					t.Fatalf("phase %s after a failed read: want the request kept", live.Status.Phase)
				}
				return
			}
			if live.Status.Phase != api.PhaseCancelled || !hasReason(live, ReasonCancelRequested, messageCancelledOpenElsewhere) {
				t.Fatalf("phase %s, conditions %s: want Cancelled saying the pull request is open on another base",
					live.Status.Phase, conditionsOf(live))
			}
		})
	}
}

// failedOnly reports whether runs, the check runs of a head, hold a failed shadewright/review of a stopped request
// (stoppedCheckTitle) and no run that passes.
func failedOnly(runs []providers.CheckRun) bool {
	failed := false
	for _, run := range runs {
		if run.Name != providers.CheckRunName {
			continue
		}
		if run.Status != providers.CheckRunFailure {
			return false
		}
		failed = failed || run.Title == stoppedCheckTitle
	}
	return failed
}

// #925, ADR-0043 limit (e): a request whose own pull request a person moved to another base stops in the step that
// finds it there, and that step fails its review check on the head: moved back to the target branch after the stop,
// the pull request keeps the lock and cannot merge on a check written before the stop. On main no check was written.
func TestStopOnMovedBaseFailsTheCheckAndMovedBackKeepsLock(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
	ctx := context.Background()
	r.ReviewerHints = true
	setTargetMain(t, r)
	meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionApproved)
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	n, head := rr.Status.PullRequest.Number, rr.Status.PullRequest.HeadSHA
	moveTo(t, g, n, movedBranch)
	key := client.ObjectKeyFromObject(rr)
	clock := clockOf(r)
	live := &api.RemediationRequest{}
	reconcileUntil := func(done func() bool) {
		t.Helper()
		for range 6 {
			if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
				t.Logf("Reconcile: %v", err)
			}
			if err := r.Get(ctx, key, live); err != nil {
				t.Fatal(err)
			}
			if done() {
				return
			}
			clock.advance(confirmWait + time.Minute)
		}
	}
	reconcileUntil(func() bool { return live.Status.Phase == api.PhaseNeedsIntervention })
	if live.Status.Phase != api.PhaseNeedsIntervention || !hasReason(live, ReasonChangeRequestOnOtherBase, "") {
		t.Fatalf("phase %s, conditions %s: want NeedsIntervention with ChangeRequestOnOtherBase", live.Status.Phase,
			conditionsOf(live))
	}
	// The stop's message says what it wrote to the pull request there: a failed review check, nothing else.
	if !hasReason(live, ReasonChangeRequestOnOtherBase, "nothing more to it there but a failed review check") {
		t.Fatalf("conditions %+v: want the stop's message to name the failed review check it wrote",
			live.Status.Conditions)
	}
	if !failedOnly(g.CheckRuns(head)) {
		t.Fatalf("check runs of the head %+v: want the review check failed in the step that stopped the request",
			g.CheckRuns(head))
	}
	if rc := live.Status.PullRequest.ReviewCheck; rc == nil || rc.State != api.ReviewCheckStale || rc.Head != head {
		t.Fatalf("review check %+v: want state Stale on the head", rc)
	}
	moveTo(t, g, n, mainBranch)
	for range 3 {
		reconcileUntil(func() bool { return false })
	}
	if !holdsLock(live) || live.Status.Phase != api.PhaseNeedsIntervention {
		t.Fatalf("moved back: phase %s, holds=%v; want the stopped request holding the lock", live.Status.Phase,
			holdsLock(live))
	}
	if !failedOnly(g.CheckRuns(head)) {
		t.Fatalf("moved back: check runs of the head %+v: want the failed check alone", g.CheckRuns(head))
	}
}

// #925: a request that stopped for another reason while its pull request is open on the target branch fails its review
// check at the first look of the stopped holder, and a write that fails is tried again at the next look with a
// Warning Event ReviewCheckNotFailed. A pull request that is closed gets no check. On main no check was written.
func TestStoppedHolderFailsTheCheckOfItsOpenPullRequest(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(fmt.Sprintf("closed %v", closed), func(t *testing.T) {
			r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
			ctx := context.Background()
			r.ReviewerHints = true
			setTargetMain(t, r)
			recorder := record.NewFakeRecorder(10)
			r.Recorder = recorder
			n, head := rr.Status.PullRequest.Number, rr.Status.PullRequest.HeadSHA
			if closed {
				if err := g.Close(n); err != nil {
					t.Fatal(err)
				}
			}
			failWrite := !closed
			g.SetFail(func(op string) error {
				if op == "CreateCheckRun" && failWrite {
					failWrite = false
					return &providers.RetryableError{Err: errors.New("status 502: bad gateway")}
				}
				return nil
			})
			look := func() *api.RemediationRequest {
				t.Helper()
				live := &api.RemediationRequest{}
				if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
					t.Fatal(err)
				}
				if _, err := r.checkHeld(ctx, live); err != nil {
					t.Fatalf("checkHeld: %v", err)
				}
				return live
			}
			look()
			if closed {
				if runs := g.CheckRuns(head); len(runs) != 0 {
					t.Fatalf("check runs %+v on a closed pull request: want none", runs)
				}
				return
			}
			if runs := g.CheckRuns(head); len(runs) != 0 || len(recorder.Events) != 1 ||
				!strings.Contains(<-recorder.Events, EventReviewCheckNotFailed) {
				t.Fatalf("check runs %+v after a failed write: want none and a Warning Event %s", runs,
					EventReviewCheckNotFailed)
			}
			live := look()
			if !failedOnly(g.CheckRuns(head)) || !holdsLock(live) {
				t.Fatalf("check runs %+v, holds=%v: want the failed check at the next look and the lock kept",
					g.CheckRuns(head), holdsLock(live))
			}
			if rc := live.Status.PullRequest.ReviewCheck; rc == nil || rc.State != api.ReviewCheckStale {
				t.Fatalf("review check %+v: want state Stale", rc)
			}
		})
	}
}

// dropRepository points the settings' repository entry at another repository, so the plan's repository is no longer
// in the settings.
func dropRepository(t *testing.T, r *RemediationRequestReconciler) {
	t.Helper()
	settings := &api.RemediationSettings{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: api.SettingsName}, settings); err != nil {
		t.Fatal(err)
	}
	settings.Spec.Repositories[0].URL = "https://github.com/example/elsewhere"
	if err := r.Update(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
}

// warningsOf counts the Warning Events with reason in recorder and empties it.
func warningsOf(recorder *record.FakeRecorder, reason string) int {
	n := 0
	for {
		select {
		case e := <-recorder.Events:
			if strings.HasPrefix(e, corev1.EventTypeWarning+" "+reason+" ") {
				n++
			}
		default:
			return n
		}
	}
}

// #925, review state of 2026-11-19: a stopped request whose failed review check cannot be written (the Git host
// fails the write, or the settings name another target branch, which permits reads only) says so where a person
// looks: condition ReviewCheckNotFailed with the cause, and one Warning Event ReviewCheckNotFailed over ten looks, not
// one per look. No look asks to run again sooner than heldRecheckInterval, and none returns an error. The first look
// that can write the check writes it, records Stale and removes the condition, with no further Event. On main the
// branch change left only a log line, and the Git host's failure an Event at every look.
func TestStoppedHolderSaysTheCheckWasNotFailed(t *testing.T) {
	for _, c := range []struct {
		name, reason string
		branch       bool
	}{
		{"Git host fails", ReasonGitFailed, false},
		{"target branch changed", reasonTargetBranchChanged, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
			ctx := context.Background()
			r.ReviewerHints = true
			if !c.branch {
				setTargetMain(t, r)
			}
			recorder := record.NewFakeRecorder(100)
			r.Recorder = recorder
			head := rr.Status.PullRequest.HeadSHA
			failing := !c.branch
			g.SetFail(func(op string) error {
				if op == opCreateCheckRun && failing {
					return &providers.RetryableError{Err: errors.New("status 502: bad gateway")}
				}
				return nil
			})
			look := func() *api.RemediationRequest {
				t.Helper()
				live := &api.RemediationRequest{}
				if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
					t.Fatal(err)
				}
				res, err := r.checkHeld(ctx, live)
				if err != nil {
					t.Fatalf("checkHeld: %v; want no error for the rate limiter", err)
				}
				if res.RequeueAfter < heldRecheckInterval {
					t.Fatalf("result %+v: want the look's pace of %v at least", res, heldRecheckInterval)
				}
				if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
					t.Fatal(err)
				}
				return live
			}
			var live *api.RemediationRequest
			for range 10 {
				live = look()
			}
			if runs := g.CheckRuns(head); len(runs) != 0 {
				t.Fatalf("check runs %+v: want none written", runs)
			}
			if got := warningsOf(recorder, EventReviewCheckNotFailed); got != 1 {
				t.Fatalf("%d Warning Events %s over ten looks: want one", got, EventReviewCheckNotFailed)
			}
			cond := meta.FindStatusCondition(live.Status.Conditions, api.ConditionReviewCheckNotFailed)
			if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != c.reason ||
				!strings.Contains(cond.Message, fmt.Sprintf("#%d", live.Status.PullRequest.Number)) {
				t.Fatalf("ReviewCheckNotFailed %+v: want it True with reason %s naming the pull request", cond, c.reason)
			}
			if c.branch {
				setTargetMain(t, r)
			}
			failing = false
			live = look()
			if !failedOnly(g.CheckRuns(head)) {
				t.Fatalf("check runs %+v: want the failed check once it can be written", g.CheckRuns(head))
			}
			if rc := live.Status.PullRequest.ReviewCheck; rc == nil || rc.State != api.ReviewCheckStale {
				t.Fatalf("review check %+v: want state Stale", rc)
			}
			if cond := meta.FindStatusCondition(live.Status.Conditions, api.ConditionReviewCheckNotFailed); cond != nil {
				t.Fatalf("ReviewCheckNotFailed %+v after the check was written: want it removed", cond)
			}
			if got := warningsOf(recorder, EventReviewCheckNotFailed); got != 0 {
				t.Fatalf("%d more Warning Events %s after the write: want none", got, EventReviewCheckNotFailed)
			}
		})
	}
}

// #925, #1008: the status write of a look's failed review check (checkHeld's exit, afterLook) that meets a conflict
// (another writer changed the request, an annotation, just before that write landed) is not lost: on the conflict the
// request is read again and the outcome carried over, so a condition ReviewCheckNotFailed is stored by that look, and
// so is Stale once the check is written, which removes the condition. An interceptor makes the first status write
// that carries the outcome conflict, once per look. Before the single exit the conflict was returned and the outcome
// waited for the next look.
func TestStoppedHolderCheckOutcomeSurvivesAConflict(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	r.ReviewerHints = true
	setTargetMain(t, r)
	r.Recorder = record.NewFakeRecorder(100)
	head := rr.Status.PullRequest.HeadSHA
	failing := true
	g.SetFail(func(op string) error {
		if op == opCreateCheckRun && failing {
			return &providers.RetryableError{Err: errors.New("status 502: bad gateway")}
		}
		return nil
	})
	key := client.ObjectKeyFromObject(rr)
	base := r.Client
	get := func() *api.RemediationRequest {
		t.Helper()
		live := &api.RemediationRequest{}
		if err := base.Get(ctx, key, live); err != nil {
			t.Fatal(err)
		}
		return live
	}
	note := func(live *api.RemediationRequest) *metav1.Condition {
		return meta.FindStatusCondition(live.Status.Conditions, api.ConditionReviewCheckNotFailed)
	}
	// carries reports whether a status write holds the look's outcome: the condition, or state Stale.
	carries := func(rr *api.RemediationRequest) bool {
		pr := rr.Status.PullRequest
		return note(rr) != nil || pr != nil && pr.ReviewCheck != nil && pr.ReviewCheck.State == api.ReviewCheckStale
	}
	armed, conflicts := false, 0
	var touched string
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			out, ok := obj.(*api.RemediationRequest)
			if !ok || !armed || !carries(out) {
				return c.SubResource(sub).Update(ctx, obj, opts...)
			}
			armed = false
			// Another writer changes the request between afterLook's read and its write.
			other := get()
			if other.Annotations == nil {
				other.Annotations = map[string]string{}
			}
			touched = fmt.Sprint(time.Now().UnixNano())
			other.Annotations["example.com/touched"] = touched
			if err := base.Update(ctx, other); err != nil {
				t.Fatal(err)
			}
			err := c.SubResource(sub).Update(ctx, obj, opts...)
			if apierrors.IsConflict(err) {
				conflicts++
			}
			return err
		},
	})
	look := func() {
		t.Helper()
		armed, conflicts = true, 0
		if _, err := r.checkHeld(ctx, get()); err != nil {
			t.Fatalf("checkHeld: %v; want the write carried over", err)
		}
		if conflicts != 1 {
			t.Fatalf("%d conflicts on the status write that carries the outcome: want one", conflicts)
		}
		if live := get(); live.Annotations["example.com/touched"] != touched {
			t.Fatalf("annotations %v: the write of the look put back the copy it read", live.Annotations)
		}
	}
	look()
	if c := note(get()); c == nil || c.Reason != ReasonGitFailed {
		t.Fatalf("ReviewCheckNotFailed %+v after a look whose write met a conflict: want it stored", c)
	}
	failing = false
	look()
	live := get()
	if rc := live.Status.PullRequest.ReviewCheck; !failedOnly(g.CheckRuns(head)) || rc == nil ||
		rc.State != api.ReviewCheckStale || note(live) != nil {
		t.Fatalf("check runs %+v, review check %+v, conditions %s: want the check written, Stale recorded and the "+
			"condition removed by that look", g.CheckRuns(head), rc, conditionsOf(live))
	}
}

// #925, review state of 2026-11-19: the step that stops a request for its pull request open on another base
// (stopOpenElsewhere) goes on when the failed review check cannot be written, and its status write says so with
// condition ReviewCheckNotFailed and one Warning Event: writes paused, the repository gone from the settings, or a
// target branch changed. After the pause, the stopped holder's look writes the check and removes the condition. On
// main the stop recorded nothing but a log line.
func TestStopOnMovedBaseSaysTheCheckWasNotFailed(t *testing.T) {
	for _, c := range []struct {
		name, reason string
		set          func(t *testing.T, r *RemediationRequestReconciler)
	}{
		{"paused", ReasonWritesPaused, func(t *testing.T, r *RemediationRequestReconciler) { setPaused(t, r, true) }},
		{"repository left the settings", reasonRepositoryUnavailable, dropRepository},
		{"settings name another branch", reasonTargetBranchChanged, func(t *testing.T, r *RemediationRequestReconciler) {
			settings := &api.RemediationSettings{}
			if err := r.Get(context.Background(), client.ObjectKey{Name: api.SettingsName}, settings); err != nil {
				t.Fatal(err)
			}
			settings.Spec.Repositories[0].TargetBranch = freshOtherBranch
			if err := r.Update(context.Background(), settings); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
			ctx := context.Background()
			r.ReviewerHints = true
			setTargetMain(t, r)
			recorder := record.NewFakeRecorder(100)
			r.Recorder = recorder
			n, head := rr.Status.PullRequest.Number, rr.Status.PullRequest.HeadSHA
			moveTo(t, g, n, movedBranch)
			c.set(t, r)
			cr, err := g.GetChangeRequest(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			live := &api.RemediationRequest{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
				t.Fatal(err)
			}
			if err := r.stopOpenElsewhere(ctx, live, cr, false); !errors.Is(err, errStopped) {
				t.Fatalf("stopOpenElsewhere: %v; want the stop", err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
				t.Fatal(err)
			}
			cond := meta.FindStatusCondition(live.Status.Conditions, api.ConditionReviewCheckNotFailed)
			if live.Status.Phase != api.PhaseNeedsIntervention || cond == nil || cond.Reason != c.reason {
				t.Fatalf("phase %s, ReviewCheckNotFailed %+v: want NeedsIntervention and the condition with reason %s",
					live.Status.Phase, cond, c.reason)
			}
			if runs := g.CheckRuns(head); len(runs) != 0 {
				t.Fatalf("check runs %+v: want none written", runs)
			}
			if got := warningsOf(recorder, EventReviewCheckNotFailed); got != 1 {
				t.Fatalf("%d Warning Events %s: want one", got, EventReviewCheckNotFailed)
			}
			if c.reason != ReasonWritesPaused {
				return
			}
			setPaused(t, r, false)
			if _, err := r.checkHeld(ctx, live); err != nil {
				t.Fatalf("checkHeld: %v", err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
				t.Fatal(err)
			}
			if !failedOnly(g.CheckRuns(head)) ||
				meta.FindStatusCondition(live.Status.Conditions, api.ConditionReviewCheckNotFailed) != nil {
				t.Fatalf("after the pause: check runs %+v, conditions %s: want the failed check and the condition "+
					"removed", g.CheckRuns(head), conditionsOf(live))
			}
		})
	}
}

// #925 case 2: deleting a request after the configured target branch changed closes its pull request by number and by
// marker, on the recorded target branch and on the base it was moved to, instead of letting the finalizer go with
// "target branch changed" before the Git host is asked. A pull request from the branch without the marker stays open.
// When the repository is no longer in the settings nothing can be closed, and the deletion says so with a Warning
// Event PullRequestLeftOpen. On main both pull requests stayed open and no Event was recorded.
func TestDeletionAfterBranchChangeClosesOwnPullRequests(t *testing.T) {
	const entryGone, settingsGone = "entry gone", "settings object gone"
	for _, mode := range []string{"settings reach it", entryGone, settingsGone} {
		t.Run(mode, func(t *testing.T) {
			gone := mode == entryGone
			var kept *api.RemediationSettings
			// settlementWorld leaves the settings naming freshOtherBranch, not the recorded main.
			r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
			ctx := context.Background()
			recorder := record.NewFakeRecorder(10)
			r.Recorder = recorder
			n := rr.Status.PullRequest.Number
			head, err := g.BranchHead(ctx, mainBranch)
			if err != nil {
				t.Fatal(err)
			}
			if err := g.CreateBranch(ctx, movedBranch, head); err != nil {
				t.Fatal(err)
			}
			second, err := g.AddOpenChangeRequest(freshness.BranchName(rr), movedBranch, marker(rr))
			if err != nil {
				t.Fatal(err)
			}
			foreign, err := g.AddOpenChangeRequest(freshness.BranchName(rr), freshOtherBranch, "a person's")
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case entryGone:
				dropRepository(t, r)
			case settingsGone:
				kept = &api.RemediationSettings{}
				if err := r.Get(ctx, client.ObjectKey{Name: api.SettingsName}, kept); err != nil {
					t.Fatal(err)
				}
				if err := r.Delete(ctx, &api.RemediationSettings{ObjectMeta: metav1.ObjectMeta{Name: api.SettingsName}}); err != nil {
					t.Fatal(err)
				}
			}
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
			key := client.ObjectKeyFromObject(rr)
			for i := range 3 {
				if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
					t.Logf("Reconcile %d: %v", i, err)
				}
			}
			if kept != nil {
				settingsBackCloses(t, r, g, recorder, key, n, kept)
			}
			if err := r.Get(ctx, key, &api.RemediationRequest{}); !apierrors.IsNotFound(err) {
				t.Fatalf("the request is still there (%v)", err)
			}
			crs := g.ChangeRequests()
			if crs[foreign-1].State != providers.ChangeRequestOpen {
				t.Errorf("pull request #%d without the marker is %s: want it left open", foreign, crs[foreign-1].State)
			}
			if gone {
				var events []string
				for len(recorder.Events) > 0 {
					events = append(events, <-recorder.Events)
				}
				if len(events) != 1 || !strings.Contains(events[0], EventPullRequestLeftOpen) ||
					!strings.Contains(events[0], fmt.Sprintf("#%d", n)) {
					t.Fatalf("events %q: want one Warning Event %s naming #%d", events, EventPullRequestLeftOpen, n)
				}
				// The Event says what is missing: the entry of the repository.
				if says := "no entry for the repository"; !strings.Contains(events[0], says) {
					t.Fatalf("event %q: want it saying %q", events[0], says)
				}
				return
			}
			for _, m := range []int64{n, second} {
				if crs[m-1].State != providers.ChangeRequestClosed {
					t.Errorf("pull request #%d with the marker on %s is %s: want it closed", m, crs[m-1].BaseBranch,
						crs[m-1].State)
				}
			}
		})
	}
}

// settingsBackCloses checks a deleted request whose RemediationSettings object is missing: a failure of the cleanup,
// not a repository that left the settings, so the request stays with CleanupFailing (SettingsNotFound), its pull
// request #n open and nothing says it was left open (#925). It then creates kept again and reconciles, so the
// deletion closes the pull requests and goes.
func settingsBackCloses(t *testing.T, r *RemediationRequestReconciler, g *fakegit.Git,
	recorder *record.FakeRecorder, key client.ObjectKey, n int64, kept *api.RemediationSettings) {
	t.Helper()
	ctx := context.Background()
	got := &api.RemediationRequest{}
	if err := r.Get(ctx, key, got); err != nil {
		t.Fatalf("the request was let go with RemediationSettings missing: %v", err)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionCleanupFailing)
	if c == nil || c.Reason != ReasonSettingsNotFound {
		t.Fatalf("condition CleanupFailing %+v: want reason %s", c, ReasonSettingsNotFound)
	}
	if st := g.ChangeRequests()[n-1].State; st != providers.ChangeRequestOpen {
		t.Fatalf("pull request #%d is %s while the settings are missing: want it open", n, st)
	}
	if left := warningsOf(recorder, EventPullRequestLeftOpen); left != 0 {
		t.Fatalf("%d Warning Events %s while the cleanup is retried", left, EventPullRequestLeftOpen)
	}
	kept.ResourceVersion = ""
	if err := r.Create(ctx, kept); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
			t.Logf("Reconcile %d after the settings came back: %v", i, err)
		}
	}
}
