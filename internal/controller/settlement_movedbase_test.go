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
	"strings"
	"testing"
	"time"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// stopLost is a stop for a branch change whose write that recorded the pull request was lost.
	stopLost = "lost"
	// endMerged is a pull request merged by hand.
	endMerged = "merged"
)

// opListAnyBase is the fake's name of the listing of the pull requests of a branch to every base.
const opListAnyBase = "ListChangeRequestsAnyBase"

// plusBase is a base name a code host allows and the rule for target branches does not.
const plusBase = "release+1"

// movedBranch is a third base: neither the recorded target branch (main) nor the configured one (release).
const movedBranch = "hotfix"

// stopBranch is the stop of movedBaseWorld for TargetBranchChanged.
const stopBranch = "branch"

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
	for _, stop := range []string{stopBranch, pathCancel, stopLost} {
		for _, end := range []string{foundClosed, endMerged} {
			t.Run(stop+"/"+end, func(t *testing.T) {
				r, g, rr, clock, n := movedBaseWorld(t, stop)
				live := lookMoved(t, r, rr, clock)
				held := meta.FindStatusCondition(live.Status.Conditions, api.ConditionTargetHeld)
				if !holdsLock(live) || held == nil || held.Reason != ReasonOpenOnBranch ||
					!strings.Contains(held.Message, movedBranch) {
					t.Fatalf("open on %s: holds=%v TargetHeld=%+v; want the lock held and the condition naming the base",
						movedBranch, holdsLock(live), held)
				}
				phase := live.Status.Phase
				if end == foundClosed {
					if err := g.Close(n); err != nil {
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

// #892: a failed read of the request's own pull request moved to another base keeps the lock with no verdict, and is
// retried as a failed Git read: no error for the rate limiter, a wait of the poll interval at least, and a Git wait
// recorded in status (condition GitReadRetry), never at once. With a number recorded that is the read by its number;
// with none recorded (the write that recorded it lost), the listing of the open pull requests of the branch to every
// base, whose untrusted answer (an entry not asked for, such as one from another branch) is a failed read too.
func TestSettlementMovedBaseFailedReadKeepsLock(t *testing.T) {
	for name, c := range map[string]struct {
		stop, op string
		fail     error
	}{
		"by number, host error": {stopBranch, opGet, &providers.RetryableError{Err: errors.New("server error")}},
		"listing, host error":   {stopLost, opListAnyBase, &providers.RetryableError{Err: errors.New("server error")}},
		"listing, untrusted": {stopLost, opListAnyBase,
			fmt.Errorf("an entry from another branch: %w", providers.ErrUntrustedAnswer)},
	} {
		t.Run(name, func(t *testing.T) { movedBaseFailedRead(t, c.stop, c.op, c.fail) })
	}
}

// movedBaseFailedRead is TestSettlementMovedBaseFailedReadKeepsLock in the world of stop with op failing with fail:
// from the first look for the listing of every base (it runs only while no number is recorded), after the first
// looks for the read by number.
func movedBaseFailedRead(t *testing.T, stop, op string, fail error) {
	r, g, rr, clock, _ := movedBaseWorld(t, stop)
	ctx := context.Background()
	failNow := func() {
		g.SetFail(func(got string) error {
			if got == op {
				return fail
			}
			return nil
		})
	}
	if op == opListAnyBase {
		failNow()
	} else {
		// The first looks find the pull request on hotfix.
		if live := lookMoved(t, r, rr, clock); !holdsLock(live) {
			t.Fatal("the lock was released while the pull request is open on its moved base")
		}
		failNow()
	}
	live := &api.RemediationRequest{}
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
			t.Fatal("a failed read of the moved pull request released the lock")
		}
		if res.RequeueAfter < r.pullRequestPollInterval() {
			t.Fatalf("result %+v: a retry sooner than the poll interval", res)
		}
		clock.advance(confirmWait + time.Minute)
	}
	if len(live.Status.GitWaits) == 0 {
		t.Fatalf("no Git wait recorded for the failed read: %+v", live.Status)
	}
	if c := meta.FindStatusCondition(live.Status.Conditions, api.ConditionGitReadRetry); c == nil ||
		c.Status != metav1.ConditionTrue {
		t.Fatalf("GitReadRetry %+v: want it saying the read is retried", c)
	}
}

// #925, review state of 2026-11-19: a stopped holder whose own pull request is merged on the target branch lists the
// open pull requests of its branch to every base before the lock may go (lockLooksElsewhere). A listing that keeps
// failing (the host's error, an answer too large or not trusted) holds the record of that merge, and says so: no
// error for the rate limiter, the look's pace of heldRecheckInterval, and condition GitReadRetry naming the listing,
// with a Paced wait and no end (noBoundHeldLock), as every failed read of a stopped holder. The first look whose
// listing works records the merge. This pins today's behaviour; it changes nothing.
func TestSettlementListingFailsMergeOnTargetIsSaid(t *testing.T) {
	for name, fail := range map[string]error{
		"host error": &providers.RetryableError{Err: errors.New("server error")},
		"too large":  fmt.Errorf("more than the adapter reads: %w", providers.ErrTooLarge),
		"untrusted":  fmt.Errorf("an entry from another branch: %w", providers.ErrUntrustedAnswer),
	} {
		for _, stop := range []string{stopBranch, "another stop"} {
			t.Run(name+"/"+stop, func(t *testing.T) {
				r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
				ctx := context.Background()
				if stop != stopBranch {
					setTargetMain(t, r)
					meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionApproved,
						Status: metav1.ConditionFalse, Reason: ReasonChangeRequestOnOtherBase, Message: "stopped"})
					if err := r.Status().Update(ctx, rr); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := g.Merge(rr.Status.PullRequest.Number); err != nil {
					t.Fatal(err)
				}
				failing := true
				g.SetFail(func(op string) error {
					if op == opListAnyBase && failing {
						return fail
					}
					return nil
				})
				clock := clockOf(r)
				live := &api.RemediationRequest{}
				for range 4 {
					if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
						t.Fatal(err)
					}
					res, err := r.checkHeld(ctx, live)
					if err != nil || res.RequeueAfter < heldRecheckInterval {
						t.Fatalf("checkHeld: %+v, %v; want the look's pace and no error", res, err)
					}
					clock.advance(confirmWait + time.Minute)
				}
				if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
					t.Fatal(err)
				}
				c := meta.FindStatusCondition(live.Status.Conditions, api.ConditionGitReadRetry)
				if mergeRecorded(live) || !holdsLock(live) || c == nil || c.Status != metav1.ConditionTrue ||
					!strings.Contains(c.Message, "every base") || len(live.Status.GitWaits) != 1 ||
					live.Status.GitWaits[0].Kind != api.GitWaitPaced {
					t.Fatalf("merged %v, holds %v, GitReadRetry %+v, waits %+v: want the merge not yet recorded, "+
						"the lock kept and the failed listing said", mergeRecorded(live), holdsLock(live), c,
						live.Status.GitWaits)
				}
				failing = false
				for range 3 {
					if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
						t.Fatal(err)
					}
					if _, err := r.checkHeld(ctx, live); err != nil {
						t.Logf("checkHeld: %v", err)
					}
					clock.advance(confirmWait + time.Minute)
				}
				if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
					t.Fatal(err)
				}
				if !mergeRecorded(live) {
					t.Fatalf("phase %s, conditions %s: want the merge recorded once the listing works",
						live.Status.Phase, conditionsOf(live))
				}
			})
		}
	}
}

// #892, decision of 2026-11-19: with no pull request recorded, a closed pull request with the request's marker on
// the target branch is no verdict by itself. A second one with the marker, open on another base, keeps the lock:
// the listing over every base runs before any release.
func TestSettlementMovedBaseClosedOnTargetOpenElsewhereKeepsLock(t *testing.T) {
	r, g, rr, clock, n := movedBaseWorld(t, stopLost)
	ctx := context.Background()
	// The first pull request is closed on the target branch; a second one with the marker is open on hotfix.
	if err := g.Close(n); err != nil {
		t.Fatal(err)
	}
	if err := g.SetBase(n, mainBranch); err != nil {
		t.Fatal(err)
	}
	second, err := g.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: freshness.BranchName(rr),
		BaseBranch: mainBranch, Body: marker(rr)})
	if err != nil {
		t.Fatal(err)
	}
	if second.Number == n {
		t.Fatal("no second pull request was opened")
	}
	if err := g.SetBase(second.Number, movedBranch); err != nil {
		t.Fatal(err)
	}
	closed := providers.ChangeRequest{Number: n}
	live := lookMoved(t, r, rr, clock)
	held := meta.FindStatusCondition(live.Status.Conditions, api.ConditionTargetHeld)
	if !holdsLock(live) || held == nil || held.Reason != ReasonOpenOnBranch || !strings.Contains(held.Message, movedBranch) {
		t.Fatalf("closed #%d on %s, the other open on %s: holds=%v TargetHeld=%+v; want the lock held",
			closed.Number, mainBranch, movedBranch, holdsLock(live), held)
	}
}

// #892, decision of 2026-11-19: the base of a pull request is text from the code host (invariant 4). The provider
// leaves out a base name outside its rule for target branches; without that check too, TargetHeld shows it only redacted, without control
// characters and within maxHostBranchBytes, so a long one cannot push the message past the schema's bound and fail
// every status write.
func TestSettlementMovedBaseMessageBoundsTheBase(t *testing.T) {
	// #nosec G101 -- a made-up value that a redaction rule must catch, not a credential
	for name, base := range map[string]string{
		"secret":  "hotfix/credential=review-secret",
		"control": "x\nReleased the lock\x1b[31m",
		"long":    strings.Repeat("b", 40000),
	} {
		t.Run(name, func(t *testing.T) {
			r, g, rr, clock, n := movedBaseWorld(t, stopBranch)
			ctx := context.Background()
			head, err := g.BranchHead(ctx, mainBranch)
			if err != nil {
				t.Fatal(err)
			}
			if err := g.CreateBranch(ctx, base, head); err != nil {
				t.Fatal(err)
			}
			if err := g.SetBase(n, base); err != nil {
				t.Fatal(err)
			}
			live := lookMoved(t, r, rr, clock)
			held := meta.FindStatusCondition(live.Status.Conditions, api.ConditionTargetHeld)
			if !holdsLock(live) || held == nil || held.Reason != ReasonOpenOnBranch {
				t.Fatalf("holds=%v TargetHeld=%+v; want the lock held with TargetHeld", holdsLock(live), held)
			}
			if strings.Contains(held.Message, "review-secret") || strings.ContainsAny(held.Message, "\n\x1b") ||
				len(held.Message) > 1024 {
				t.Fatalf("TargetHeld shows the base as answered (%d bytes): %q", len(held.Message), held.Message)
			}
		})
	}
}

// #892, decision of 2026-11-19: a cancel after a change of the target branch whose look finds the request's own pull
// request open on another base ends Cancelled as before, but never says it is closed: it says the pull request is
// open on another base and the target stays locked, as TargetHeld does, and it closes nothing.
func TestCancelLookOpenOnMovedBaseSaysSo(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
	ctx := context.Background()
	meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionApproved)
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	rr.Spec.Cancel = true
	if err := r.Update(ctx, rr); err != nil {
		t.Fatal(err)
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
	// The looks confirm the pull request gone from both target branches over confirmWait, as the requeue would.
	clock := clockOf(r)
	live := &api.RemediationRequest{}
	for range 4 {
		if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
			t.Fatal(err)
		}
		if live.Status.Phase == api.PhaseCancelled {
			break
		}
		if _, _, err := r.cancelLook(ctx, live, nil); err != nil {
			t.Fatalf("cancelLook: %v", err)
		}
		clock.advance(confirmWait + time.Minute)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
		t.Fatal(err)
	}
	if live.Status.Phase != api.PhaseCancelled || !holdsLock(live) {
		t.Fatalf("phase %s, holds=%v: want Cancelled with the lock kept", live.Status.Phase, holdsLock(live))
	}
	for _, c := range live.Status.Conditions {
		if strings.Contains(c.Message, "closed") && !strings.Contains(c.Message, "until") {
			t.Errorf("condition %s says the pull request is closed: %q", c.Type, c.Message)
		}
	}
	approved := meta.FindStatusCondition(live.Status.Conditions, api.ConditionApproved)
	if approved == nil || !strings.Contains(approved.Message, "open on another base branch") ||
		!strings.Contains(approved.Message, "locked") {
		t.Fatalf("Approved %+v: want it saying the pull request is open on another base and the target stays locked",
			approved)
	}
	if cr := g.ChangeRequests()[n-1]; cr.State != providers.ChangeRequestOpen {
		t.Fatalf("pull request #%d is %s: the cancel closed it", n, cr.State)
	}
}

// A cancel after a change of the target branch whose look finds the request's own pull request merged into another
// base, where a person moved it, says so as the watch does: NeedsIntervention with ChangeRequestOnOtherBase naming the
// merge, never Cancelled with the pull request "found closed".
func TestCancelLookMergedOnMovedBaseSaysSo(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
	ctx := context.Background()
	meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionApproved)
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	rr.Spec.Cancel = true
	if err := r.Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	n := rr.Status.PullRequest.Number
	moveTo(t, g, n, movedBranch)
	if _, err := g.Merge(n); err != nil {
		t.Fatal(err)
	}
	clock := clockOf(r)
	live := &api.RemediationRequest{}
	for range 4 {
		if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
			t.Fatal(err)
		}
		if live.Status.Phase != api.PhaseAwaitingApproval {
			break
		}
		if _, _, err := r.cancelLook(ctx, live, nil); err != nil {
			t.Fatalf("cancelLook: %v", err)
		}
		clock.advance(confirmWait + time.Minute)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
		t.Fatal(err)
	}
	if live.Status.Phase != api.PhaseNeedsIntervention ||
		!hasReason(live, reasonOnOtherBase, fmt.Sprintf("#%d of the request was merged into another base branch (%s)",
			n, movedBranch)) {
		t.Fatalf("phase %s, conditions %s: want NeedsIntervention with %s naming the merge of #%d on %s",
			live.Status.Phase, conditionsOf(live), reasonOnOtherBase, n, movedBranch)
	}
	for _, c := range live.Status.Conditions {
		if c.Reason == ReasonPullRequestClosed || strings.Contains(c.Message, "found closed") {
			t.Errorf("condition %s says the pull request is closed: %s: %q", c.Type, c.Reason, c.Message)
		}
	}
}

// unnamedBases are base names a code host allows and the rule for target branches does not (decision of 2026-11-19
// on #892): the provider answers a pull request to one of them with no base name.
var unnamedBases = map[string]string{
	"shadewright branch": "shadewright/other-request",
	"plus sign":          plusBase,
	"300 bytes":          strings.Repeat("r", 300),
}

// moveTo creates branch base at main's head and moves pull request n there.
func moveTo(t *testing.T, g *fakegit.Git, n int64, base string) {
	t.Helper()
	ctx := context.Background()
	head, err := g.BranchHead(ctx, mainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.CreateBranch(ctx, base, head); err != nil {
		t.Fatal(err)
	}
	if err := g.SetBase(n, base); err != nil {
		t.Fatal(err)
	}
}

// #892, decision of 2026-11-19: the request's own pull request moved to a base whose name the provider leaves out
// keeps the lock on its number, with no failed read. TargetHeld names the pull request, says it is open on another
// base and what releases the lock; its close there releases the lock.
func TestSettlementMovedBaseUnnamedKeepsLock(t *testing.T) {
	for name, base := range unnamedBases {
		t.Run(name, func(t *testing.T) {
			r, g, rr, clock, n := movedBaseWorld(t, stopBranch)
			moveTo(t, g, n, base)
			g.HideBaseName(base)
			live := lookMoved(t, r, rr, clock)
			held := meta.FindStatusCondition(live.Status.Conditions, api.ConditionTargetHeld)
			if !holdsLock(live) || held == nil || held.Reason != ReasonOpenOnBranch ||
				!strings.Contains(held.Message, fmt.Sprintf("#%d", n)) ||
				!strings.Contains(held.Message, "another base branch") || !strings.Contains(held.Message, "closing it releases the lock") {
				t.Fatalf("holds=%v TargetHeld=%+v; want the lock held and TargetHeld naming #%d on another base",
					holdsLock(live), held, n)
			}
			if c := meta.FindStatusCondition(live.Status.Conditions, api.ConditionGitReadRetry); c != nil {
				t.Fatalf("GitReadRetry %+v: a base name left out is no failed read", c)
			}
			if err := g.Close(n); err != nil {
				t.Fatal(err)
			}
			if live = lookMoved(t, r, rr, clock); holdsLock(live) {
				t.Fatalf("closed on its moved base: the lock is still held: %+v", live.Status.Conditions)
			}
		})
	}
}

// #925, decision of 2026-11-19: a pull request from the request's branch that is not its own (another number, no
// marker) open on another base has the branch head, which carries the request's commit, and a person can move it to
// the target branch and merge it: it keeps the lock as one open on the target branch does, whatever its base is
// called. It is never adopted or closed. Its close releases the lock. On main it was released at once.
func TestSettlementForeignOnOtherBaseKeepsLock(t *testing.T) {
	for name, base := range map[string]string{"named": movedBranch, "unnamed": plusBase} {
		t.Run(name, func(t *testing.T) {
			r, g, rr, clock, n := movedBaseWorld(t, stopBranch)
			ctx := context.Background()
			if err := g.SetBase(n, mainBranch); err != nil {
				t.Fatal(err)
			}
			if err := g.CloseChangeRequest(ctx, n); err != nil {
				t.Fatal(err)
			}
			head, err := g.BranchHead(ctx, mainBranch)
			if err != nil {
				t.Fatal(err)
			}
			if base != movedBranch {
				if err := g.CreateBranch(ctx, base, head); err != nil {
					t.Fatal(err)
				}
				g.HideBaseName(base)
			}
			foreign, err := g.AddOpenChangeRequest(freshness.BranchName(rr), base, "a person's change")
			if err != nil {
				t.Fatal(err)
			}
			live := lookMoved(t, r, rr, clock)
			held := meta.FindStatusCondition(live.Status.Conditions, api.ConditionTargetHeld)
			if !holdsLock(live) || held == nil || !strings.Contains(held.Message, fmt.Sprintf("#%d", foreign)) {
				t.Fatalf("a foreign pull request open on %s: holds=%v TargetHeld=%+v; want the lock held and the "+
					"pull request named", base, holdsLock(live), held)
			}
			if live.Status.PullRequest.Number != n {
				t.Fatalf("the foreign pull request was taken as the request's own: %+v", live.Status.PullRequest)
			}
			if got, _ := g.GetChangeRequest(ctx, foreign); got.State != providers.ChangeRequestOpen {
				t.Fatalf("the foreign pull request was closed: %+v", got)
			}
			if err := g.Close(foreign); err != nil {
				t.Fatal(err)
			}
			if live := lookMoved(t, r, rr, clock); holdsLock(live) {
				t.Fatalf("the foreign pull request on %s is closed and the lock is held: %+v", base,
					live.Status.Conditions)
			}
		})
	}
}

// #925, case 1: the read of the recorded pull request by its number answers 404 again and again, while the pull
// request is open on a base a person moved it to: the listings of the recorded and the configured target branch
// confirm it gone there, but "gone" is concluded only after every base was looked at, so the lock is kept. On main
// the lock was released on the target branches' listings alone.
func TestSettlementGoneByNumberOpenElsewhereKeepsLock(t *testing.T) {
	for _, stop := range []string{stopBranch, pathCancel} {
		t.Run(stop, func(t *testing.T) {
			r, g, rr, clock, n := movedBaseWorld(t, stop)
			g.SetFail(func(op string) error {
				if op == opGet {
					return fmt.Errorf("pull request %d: %w", n, providers.ErrNotFound)
				}
				return nil
			})
			live := lookMoved(t, r, rr, clock)
			if !holdsLock(live) {
				t.Fatalf("pull request #%d open on %s and missing by its number: the lock was released: %+v", n,
					movedBranch, live.Status.Conditions)
			}
		})
	}
}

// #892, decision of 2026-11-19: a cancel after a change of the target branch, run through Reconcile, whose own pull
// request a person moved to another base: the look at the recorded branch finds it gone there, and the cancel must
// not conclude "closed" from that one branch. The request ends Cancelled saying the pull request is open on another
// base, PullRequestOpened is not marked closed, the lock stays and nothing closes the pull request.
func TestCancelMovedBaseThroughReconcile(t *testing.T) {
	for name, base := range map[string]string{"named": movedBranch, "unnamed": plusBase} {
		t.Run(name, func(t *testing.T) {
			r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
			ctx := context.Background()
			meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionApproved)
			if err := r.Status().Update(ctx, rr); err != nil {
				t.Fatal(err)
			}
			rr.Spec.Cancel = true
			if err := r.Update(ctx, rr); err != nil {
				t.Fatal(err)
			}
			n := rr.Status.PullRequest.Number
			moveTo(t, g, n, base)
			if base != movedBranch {
				g.HideBaseName(base)
			}
			clock := clockOf(r)
			live := &api.RemediationRequest{}
			for range 8 {
				if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
					t.Logf("Reconcile: %v", err)
				}
				clock.advance(confirmWait + time.Minute)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
				t.Fatal(err)
			}
			if live.Status.Phase != api.PhaseCancelled || !holdsLock(live) {
				t.Fatalf("phase %s, holds=%v, conditions %+v: want Cancelled with the lock kept", live.Status.Phase,
					holdsLock(live), live.Status.Conditions)
			}
			if c := meta.FindStatusCondition(live.Status.Conditions, api.ConditionPROpened); c != nil &&
				(c.Reason == ReasonPullRequestClosed || c.Reason == ReasonChangeRequestClosed ||
					strings.Contains(c.Message, "is closed")) {
				t.Errorf("PullRequestOpened %+v says the pull request is closed", c)
			}
			if approved := meta.FindStatusCondition(live.Status.Conditions, api.ConditionApproved); approved == nil ||
				approved.Message != messageCancelledOpenElsewhere {
				t.Fatalf("Approved %+v: want %q", approved, messageCancelledOpenElsewhere)
			}
			if closedOnly(live) {
				t.Error("closedOnly is set for a pull request open on another base")
			}
			if cr := g.ChangeRequests()[n-1]; cr.State != providers.ChangeRequestOpen {
				t.Fatalf("pull request #%d is %s: the cancel closed it", n, cr.State)
			}
		})
	}
}

// #892: on the cancel's look after a change of the target branch, a failed read of the moved pull request by its
// number is a failed Git read: the request is not ended (closed is not concluded from one branch), no error goes to
// the rate limiter, and the retry waits the poll interval at least with a condition that says the read fails.
func TestCancelMovedBaseFailedListingThroughReconcile(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
	ctx := context.Background()
	meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionApproved)
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	rr.Spec.Cancel = true
	if err := r.Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	moveTo(t, g, rr.Status.PullRequest.Number, movedBranch)
	g.SetFail(func(op string) error {
		if op == opGet {
			return &providers.RetryableError{Err: errors.New("server error")}
		}
		return nil
	})
	clock := clockOf(r)
	live := &api.RemediationRequest{}
	for i := range 8 {
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
		if i >= 2 && (err != nil || res.RequeueAfter < r.pullRequestPollInterval()) {
			t.Fatalf("look %d: %+v, %v; want a wait of the poll interval at least and no error", i, res, err)
		}
		clock.advance(confirmWait + time.Minute)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
		t.Fatal(err)
	}
	if live.Status.Phase == api.PhaseCancelled || !holdsLock(live) {
		t.Fatalf("phase %s, holds=%v: a failed listing ended the cancel", live.Status.Phase, holdsLock(live))
	}
	if c := meta.FindStatusCondition(live.Status.Conditions, api.ConditionGitReadRetry); c == nil ||
		c.Status != metav1.ConditionTrue {
		t.Fatalf("GitReadRetry %+v: want it saying the read is retried", c)
	}
}

// #892: a cancel after a change of the target branch whose own pull request a person moved to the branch the
// settings name now is not ended as closed by the look at the recorded branch: open there, it keeps the cancel
// watch and the lock, and nothing says it is closed.
func TestCancelMovedToConfiguredThroughReconcile(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
	ctx := context.Background()
	meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionApproved)
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	rr.Spec.Cancel = true
	if err := r.Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	n := rr.Status.PullRequest.Number
	if err := g.SetBase(n, freshOtherBranch); err != nil {
		t.Fatal(err)
	}
	clock := clockOf(r)
	for range 8 {
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
			t.Logf("Reconcile: %v", err)
		}
		clock.advance(confirmWait + time.Minute)
	}
	live := &api.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
		t.Fatal(err)
	}
	if live.Status.Phase == api.PhaseCancelled || !holdsLock(live) ||
		meta.FindStatusCondition(live.Status.Conditions, api.ConditionCancelPending) == nil {
		t.Fatalf("phase %s, holds=%v, conditions %+v: want the cancel watch with the lock kept", live.Status.Phase,
			holdsLock(live), live.Status.Conditions)
	}
	if cr := g.ChangeRequests()[n-1]; cr.State != providers.ChangeRequestOpen {
		t.Fatalf("pull request #%d is %s", n, cr.State)
	}
}
