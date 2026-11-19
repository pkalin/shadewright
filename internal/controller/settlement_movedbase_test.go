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

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// stopLost is a stop for a branch change whose write that recorded the pull request was lost.
	stopLost = "lost"
	// endMerged is a pull request merged by hand.
	endMerged = "merged"
)

// movedBranch is a third base: neither the recorded target branch (main) nor the configured one (release).
const movedBranch = "hotfix"

// movedBaseWorld is a stopped holder (stop: "branch" for TargetBranchChanged, "cancel" for a cancel for its
// Incident, "lost" for TargetBranchChanged with the write that recorded the pull request lost) whose recorded target
// branch is main, with the settings naming release, and whose open pull request a person moved to base hotfix. It
// returns the pull request's number too.
func movedBaseWorld(t *testing.T, stop string) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest,
	*testClock, int64) {
	t.Helper()
	ctx := context.Background()
	phase := api.PhaseNeedsIntervention
	if stop == pathCancel {
		phase = api.PhaseCancelled
	}
	r, g, rr := settlementWorld(t, phase)
	if stop == pathCancel {
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionApproved,
			Status: metav1.ConditionFalse, Reason: api.ReasonIncidentGone, Message: whyIncidentGone})
		if err := r.Status().Update(ctx, rr); err != nil {
			t.Fatal(err)
		}
	}
	head, err := g.BranchHead(ctx, mainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.CreateBranch(ctx, movedBranch, head); err != nil {
		t.Fatal(err)
	}
	n := rr.Status.PullRequest.Number
	if err := g.SetBase(n, movedBranch); err != nil {
		t.Fatal(err)
	}
	if stop == stopLost {
		lostRecord(t, r, rr, api.ConditionPROpened)
	}
	return r, g, rr, clockOf(r), n
}

// lookMoved runs the looks of a stopped holder a few times, each confirmWait and more after the last, as the
// requeue would, on the request as the API server holds it, and returns it.
func lookMoved(t *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest,
	clock *testClock) *api.RemediationRequest {
	t.Helper()
	ctx := context.Background()
	for range 4 {
		live := &api.RemediationRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
			t.Fatal(err)
		}
		if _, err := r.checkHeld(ctx, live); err != nil {
			t.Fatalf("checkHeld: %v", err)
		}
		clock.advance(confirmWait + time.Minute)
	}
	live := &api.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
		t.Fatal(err)
	}
	return live
}

// #892: the request's own pull request, moved by hand to a base that is neither the recorded nor the configured
// target branch, can still merge: the lock stays held while it is open there, and TargetHeld names it, also when
// the write that recorded it was lost (found by its marker). Its close
// there releases the lock; so does its merge there, which delivers nothing the plan was made for: it is released as
// before, and nothing records it as the request's merge (no receipt, no phase after a merge, PullRequestOpened not
// marked merged).
func TestSettlementMovedBaseKeepsLockWhileOpen(t *testing.T) {
	for _, stop := range []string{"branch", pathCancel, stopLost} {
		for _, end := range []string{foundClosed, endMerged} {
			t.Run(stop+"/"+end, func(t *testing.T) {
				r, g, rr, clock, n := movedBaseWorld(t, stop)
				ctx := context.Background()
				live := lookMoved(t, r, rr, clock)
				held := meta.FindStatusCondition(live.Status.Conditions, api.ConditionTargetHeld)
				if !holdsLock(live) || held == nil || held.Reason != ReasonOpenOnBranch ||
					!strings.Contains(held.Message, movedBranch) {
					t.Fatalf("open on %s: holds=%v TargetHeld=%+v; want the lock held and the condition naming the base",
						movedBranch, holdsLock(live), held)
				}
				phase := live.Status.Phase
				if end == foundClosed {
					if err := g.CloseChangeRequest(ctx, n); err != nil {
						t.Fatal(err)
					}
				} else if _, err := g.Merge(n); err != nil {
					t.Fatal(err)
				}
				live = lookMoved(t, r, rr, clock)
				if holdsLock(live) || meta.FindStatusCondition(live.Status.Conditions, api.ConditionTargetHeld) != nil {
					t.Fatalf("%s on %s: holds=%v conditions=%+v; want the lock released", end, movedBranch,
						holdsLock(live), live.Status.Conditions)
				}
				if live.Status.MergeReceipt != nil || live.Status.Phase != phase || markedMerged(live) || mergeRecorded(live) {
					t.Fatalf("%s on %s: phase %s (was %s), receipt %v, marked merged %v: want nothing recorded",
						end, movedBranch, live.Status.Phase, phase, live.Status.MergeReceipt, markedMerged(live))
				}
			})
		}
	}
}

// #892: a failed listing of the open pull requests of the branch to every base keeps the lock with no verdict, and
// is retried as a failed Git read: no error for the rate limiter, a wait of the poll interval at least, and a Git wait recorded in
// status (condition GitReadRetry), never at once.
func TestSettlementMovedBaseFailedReadKeepsLock(t *testing.T) {
	r, g, rr, clock, _ := movedBaseWorld(t, "branch")
	ctx := context.Background()
	// The first looks find the pull request on hotfix and confirm it gone from both target branches.
	live := lookMoved(t, r, rr, clock)
	if !holdsLock(live) {
		t.Fatal("the lock was released while the pull request is open on its moved base")
	}
	// Now the listing of the open pull requests of the branch to every base fails.
	g.SetFail(func(op string) error {
		if op == "ListOpenChangeRequestsAnyBase" {
			return &providers.RetryableError{Err: errors.New("server error")}
		}
		return nil
	})
	for range 3 {
		live = &api.RemediationRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
			t.Fatal(err)
		}
		res, err := r.checkHeld(ctx, live)
		if err != nil {
			t.Fatalf("checkHeld: %v; want a paced retry, not an error", err)
		}
		if !holdsLock(live) {
			t.Fatal("a failed listing of the other bases released the lock")
		}
		if res.RequeueAfter < r.pullRequestPollInterval() {
			t.Fatalf("result %+v: a retry sooner than the poll interval", res)
		}
		clock.advance(confirmWait + time.Minute)
	}
	if len(live.Status.GitWaits) == 0 {
		t.Fatalf("no Git wait recorded for the failed listing of the other bases: %+v", live.Status)
	}
}
