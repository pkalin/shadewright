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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// stopLive stops p in the API server the way a stop does (decision of 2026-11-20 on #925): the failed review check
// on its head, state Stale recorded, phase NeedsIntervention, the lock kept for its open pull request. It returns the
// copy of p from before the stop, which a cache can still serve.
func stopLive(t *testing.T, r *RemediationRequestReconciler, g *fakegit.Git, p *api.RemediationRequest) *api.RemediationRequest {
	t.Helper()
	ctx := context.Background()
	older := p.DeepCopy()
	pr := p.Status.PullRequest
	if err := g.CreateCheckRun(ctx, pr.HeadSHA, stoppedCheckRun(ReasonChangeRequestOnOtherBase)); err != nil {
		t.Fatal(err)
	}
	p.Status.Phase = api.PhaseNeedsIntervention
	p.Status.HoldsTarget = new(true)
	pr.ReviewCheck = &api.ReviewCheck{Head: pr.HeadSHA, State: api.ReviewCheckStale}
	if err := r.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	return older
}

// deleteLive removes p from the API server: a stopped request deleted with no finalizer left.
func deleteLive(t *testing.T, r *RemediationRequestReconciler, p *api.RemediationRequest) {
	t.Helper()
	if err := r.Delete(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(p), &api.RemediationRequest{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the deleted request can still be read: %v", err)
	}
}

// cachedCopy is a client whose cache still serves copy for its key: every other read and every write goes to Client.
type cachedCopy struct {
	client.Client
	copy *api.RemediationRequest
}

func (c cachedCopy) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if rr, ok := obj.(*api.RemediationRequest); ok && key == client.ObjectKeyFromObject(c.copy) {
		c.copy.DeepCopyInto(rr)
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// A reconcile that works from an older cached copy of a request (waiting for review, or with its pull request just
// opened) never writes a pending or passing review check over the failed one a stop wrote: the live request is read
// before the write, and a stopped, released or deleted one gets nothing (model seed 35217). A failed live read over
// the stop's failed check writes nothing, a pending check no more than a pass, and is returned for a retry: without
// the live read only a pass on the head is taken down (decision of 2026-11-20 on #925, after the fifth pass). A live
// request that still waits gets the write.
func TestReviewCheckFromAStaleCopyLeavesTheStopsCheck(t *testing.T) {
	cases := []struct {
		name  string
		phase api.RemediationPhase
		// approved: the head is approved, so the write would pass the check.
		approved bool
		// live changes the live request after the stop, or replaces the reader; nil keeps the stop.
		live func(t *testing.T, r *RemediationRequestReconciler, p *api.RemediationRequest)
		// readFails: the live read fails.
		readFails bool
		// writes: the look still writes (the live request waits for review).
		writes bool
	}{
		{name: "pending over a stop", phase: api.PhaseAwaitingApproval},
		{name: "passing over a stop", phase: api.PhaseAwaitingApproval, approved: true},
		{name: "pending on a just opened pull request over a stop", phase: api.PhasePRCreated},
		{name: "released", phase: api.PhaseAwaitingApproval, live: func(t *testing.T, r *RemediationRequestReconciler,
			p *api.RemediationRequest) {
			p.Status.HoldsTarget = new(false)
			if err := r.Status().Update(context.Background(), p); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "being deleted", phase: api.PhaseAwaitingApproval, live: func(t *testing.T, r *RemediationRequestReconciler,
			p *api.RemediationRequest) {
			ctx := context.Background()
			// Waiting for review again in the API server, so only the deletion keeps the write away.
			p.Status.Phase, p.Status.PullRequest.ReviewCheck = api.PhaseAwaitingApproval, nil
			if err := r.Status().Update(ctx, p); err != nil {
				t.Fatal(err)
			}
			p.Finalizers = []string{FinalizerName}
			if err := r.Update(ctx, p); err != nil {
				t.Fatal(err)
			}
			if err := r.Delete(ctx, p); err != nil {
				t.Fatal(err)
			}
		}},
		// Stopped and then deleted: the live read answers NotFound, which is no failed read but a request that is gone,
		// so nothing is written for it, a pending check no more than a pass, and no request is left to write the
		// failed check again (decision of 2026-11-20 on #925, after the fourth pass).
		{name: "pending over a stop, then deleted", phase: api.PhaseAwaitingApproval, live: deleteLive},
		{name: "passing over a stop, then deleted", phase: api.PhaseAwaitingApproval, approved: true, live: deleteLive},
		{name: "live read fails before a pass", phase: api.PhaseAwaitingApproval, approved: true, readFails: true},
		{name: "live read fails before a pending check", phase: api.PhaseAwaitingApproval, readFails: true},
		{name: "live read fails on a just opened pull request", phase: api.PhasePRCreated, readFails: true},
		{name: "still waiting", phase: api.PhaseAwaitingApproval, writes: true, live: func(t *testing.T,
			r *RemediationRequestReconciler, p *api.RemediationRequest) {
			p.Status.Phase, p.Status.PullRequest.ReviewCheck = api.PhaseAwaitingApproval, nil
			if err := r.Status().Update(context.Background(), p); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, g, p, _, cr, head := gateWorld(t)
			ctx := context.Background()
			plainWaiting(t, r, p, head)
			p.Status.Phase = c.phase
			if err := r.Status().Update(ctx, p); err != nil {
				t.Fatal(err)
			}
			if c.approved {
				approve(g, cr.Number, 90, 3003, head)
			}
			older := stopLive(t, r, g, p)
			if c.live != nil {
				c.live(t, r, p)
			}
			// The cache still serves the copy from before the stop; only the live read sees the stop.
			live := r.Client
			r.Client, r.APIReader = cachedCopy{Client: live, copy: older}, live
			if c.readFails {
				r.APIReader = failingRequestReader{Reader: live, name: p.Name}
			}
			runs := len(g.CheckRuns(head))
			err := r.reviewCheck(ctx, older)
			switch {
			case c.readFails && !c.writes && !errors.Is(err, errReviewCheckLiveRead):
				t.Fatalf("a failed live read returned %v: want errReviewCheckLiveRead for a retry", err)
			case !c.readFails && !c.writes && err != nil:
				// A write that went through records its state from the older copy, which meets a conflict; the next
				// look records it from the host (syncReviewCheck), so only the skipped writes must return nil.
				t.Fatal(err)
			}
			got := g.CheckRuns(head)
			if c.writes {
				if len(got) != runs+1 || got[len(got)-1].Status != providers.CheckRunInProgress {
					t.Fatalf("no pending check was written: %+v", got)
				}
				return
			}
			if len(got) != runs || got[len(got)-1].Status != providers.CheckRunFailure {
				t.Fatalf("a reconcile from an older copy wrote over the stop's failed check: %+v", got)
			}
		})
	}
}

// A stopped holder whose failed check is recorded Stale looks at the latest review check of the head at each look
// that reads its pull request open (failStoppedCheck): the failed check is written again when the Git host shows
// another state last, and nothing is written when it shows the failed one, which also ends condition
// ReviewCheckNotFailed. A failed write says so with that condition (decision of 2026-11-20 on #925). A look that
// cannot read the check (a failed read, or settings that permit no write to the repository) writes nothing, leaves
// that condition as it was, returns no error, so the look keeps its pace, and records one Warning Event
// StoppedCheckUnread over two such looks; on the build before this round it was only logged.
func TestStoppedHolderWritesItsFailedCheckAgain(t *testing.T) {
	cases := []struct {
		name string
		// over writes a pending check after the stop's failed one.
		over bool
		fail string
		// noted: condition ReviewCheckNotFailed is set before the look, by an earlier look whose write failed.
		noted bool
		// branchChanged: the settings name another target branch, which permits no write.
		branchChanged bool
		// writes: a failed check is written; note: condition ReviewCheckNotFailed is set; unread: the look cannot
		// read the check and says so in one Event over two looks.
		writes, note, unread bool
	}{
		{name: "pending last", over: true, writes: true},
		{name: "failed last"},
		{name: "read fails", over: true, fail: opReviewCheckRun, unread: true},
		{name: "read fails on the failed check", fail: opReviewCheckRun, unread: true},
		{name: "settings name another branch", branchChanged: true, unread: true},
		{name: "write fails", over: true, fail: "CreateCheckRun", note: true},
		{name: "failed last ends the note", noted: true},
		{name: "read fails keeps the note", over: true, fail: opReviewCheckRun, noted: true, note: true, unread: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, g, p, _, _, head := gateWorld(t)
			ctx := context.Background()
			plainWaiting(t, r, p, head)
			stopLive(t, r, g, p)
			if c.over {
				if err := g.CreateCheckRun(ctx, head, checkRunFor(providers.CheckRunInProgress)); err != nil {
					t.Fatal(err)
				}
			}
			if c.noted {
				meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
					Type: api.ConditionReviewCheckNotFailed, Status: metav1.ConditionTrue, Reason: "GitFailed",
					Message: "earlier write failed"})
			}
			g.SetFail(func(op string) error {
				if op == c.fail {
					return fmt.Errorf("status 502: %w", providers.ErrRetryable)
				}
				return nil
			})
			if c.branchChanged {
				settings := &api.RemediationSettings{}
				if err := r.Get(ctx, client.ObjectKey{Name: api.SettingsName}, settings); err != nil {
					t.Fatal(err)
				}
				settings.Spec.Repositories[0].TargetBranch = freshOtherBranch
				if err := r.Update(ctx, settings); err != nil {
					t.Fatal(err)
				}
			}
			recorder := record.NewFakeRecorder(10)
			r.Recorder = recorder
			runs := len(g.CheckRuns(head))
			looks := 1
			if c.unread {
				looks = 2
			}
			for range looks {
				err := r.failStoppedCheck(ctx, p, string(api.PhaseNeedsIntervention))
				if err != nil && (!c.note || c.unread) {
					t.Fatalf("failStoppedCheck: %v", err)
				}
			}
			got := g.CheckRuns(head)
			if c.writes != (len(got) == runs+1) {
				t.Fatalf("check runs %+v: want a new one %t", got, c.writes)
			}
			if last := got[len(got)-1]; c.writes && (last.Status != providers.CheckRunFailure ||
				last.Title != stoppedCheckTitle) {
				t.Fatalf("the check written again is %+v: want the stop's failed check", last)
			}
			if note := meta.FindStatusCondition(p.Status.Conditions, api.ConditionReviewCheckNotFailed) != nil; note != c.note {
				t.Fatalf("condition ReviewCheckNotFailed set %t: want %t", note, c.note)
			}
			want := 0
			if c.unread {
				want = 1
			}
			if n := warningsOf(recorder, EventStoppedCheckUnread); n != want {
				t.Fatalf("%d Warning Events %s over %d looks: want %d", n, EventStoppedCheckUnread, looks, want)
			}
			if rc := p.Status.PullRequest.ReviewCheck; rc == nil || rc.State != api.ReviewCheckStale {
				t.Fatalf("review check %+v: want Stale kept", rc)
			}
		})
	}
}

// A live read before a passing review check write that keeps failing shows in status while it lasts, not only in the one
// ReviewCheckStuck Event after ten looks: condition Approved False with reason ReviewCheckLiveReadFailed, written once
// over two looks, and set back to WaitingForReview by the look whose live read works. The look returns no error, so
// its pace is kept, and the write of the note does not wake the request (onlyOwnMarksChanged).
func TestLiveReadThatKeepsFailingShowsInStatus(t *testing.T) {
	r, g, p, _, cr, head := gateWorld(t)
	ctx := context.Background()
	plainWaiting(t, r, p, head)
	approve(g, cr.Number, 90, 3003, head)
	live := r.Client
	r.APIReader = failingRequestReader{Reader: live, name: p.Name}
	key := client.ObjectKeyFromObject(p)
	versions := make([]string, 0, 2)
	for range 2 {
		before := p.DeepCopy()
		if err := r.watchReviewCheck(ctx, p, commentsCaughtUp); err != nil {
			t.Fatalf("a look whose live read failed returned %v: want nil, so the look keeps its pace", err)
		}
		got := &api.RemediationRequest{}
		if err := live.Get(ctx, key, got); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, got.ResourceVersion)
		c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionApproved)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonReviewCheckLiveReadFailed {
			t.Fatalf("Approved %+v: want False with reason %s while the live read fails", c,
				ReasonReviewCheckLiveReadFailed)
		}
		if len(versions) == 1 && !onlyOwnMarksChanged(before, got) {
			t.Fatal("the write of the note wakes the request")
		}
	}
	if versions[0] != versions[1] {
		t.Fatalf("the note was written again on the second look (%v): want it once", versions)
	}
	if runs := g.CheckRuns(head); len(runs) != 0 {
		t.Fatalf("check runs %+v: want none written without the live read", runs)
	}
	r.APIReader = live
	if err := r.watchReviewCheck(ctx, p, commentsCaughtUp); err != nil {
		t.Fatal(err)
	}
	got := &api.RemediationRequest{}
	if err := live.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionApproved); c == nil ||
		c.Reason != ReasonWaitingForReview {
		t.Fatalf("Approved %+v: want WaitingForReview once the live read works", c)
	}
	if runs := g.CheckRuns(head); len(runs) != 1 {
		t.Fatalf("check runs %+v: want the check written once the live read works", runs)
	}
}

// The fifth overwritten verdict on #925: a stop writes the failed check and records Stale; the recorded pull request
// closes while another open pull request from the branch, on the same head, keeps the lock; a reconcile from an
// older copy meets a failed live read. It writes nothing over the failed check (decision of 2026-11-20 on #925, after
// the fifth pass): the stopped holder's looks no longer try the failed check once the recorded pull request is
// closed and nothing is noted (failCheckAfterLook), so a pending check written then would stay last while the other
// pull request is open.
func TestFailedLiveReadLeavesTheStopsCheckWhileAnotherPullRequestHolds(t *testing.T) {
	r, g, p, _, cr, head := gateWorld(t)
	ctx := context.Background()
	plainWaiting(t, r, p, head)
	older := stopLive(t, r, g, p)
	if err := g.Close(cr.Number); err != nil {
		t.Fatal(err)
	}
	closed, err := g.GetChangeRequest(ctx, cr.Number)
	if err != nil {
		t.Fatal(err)
	}
	n, err := g.AddOpenChangeRequest(cr.HeadBranch, mainBranch, "opened by a person")
	if err != nil {
		t.Fatal(err)
	}
	holding, err := g.GetChangeRequest(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	if holding.HeadSHA != head {
		t.Fatalf("the other pull request is on %s: want the stopped head %s", holding.HeadSHA, head)
	}
	live := r.Client
	r.Client = cachedCopy{Client: live, copy: older}
	r.APIReader = failingRequestReader{Reader: live, name: p.Name}
	runs := len(g.CheckRuns(head))
	if err := r.reviewCheck(ctx, older); !errors.Is(err, errReviewCheckLiveRead) {
		t.Fatalf("a failed live read returned %v: want errReviewCheckLiveRead for a retry", err)
	}
	if got := g.CheckRuns(head); len(got) != runs {
		t.Fatalf("a reconcile from an older copy wrote over the stop's failed check without the live read: %+v", got)
	}
	// The stopped holder's look reads its own pull request closed and the other one open: it tries nothing more.
	r.Client, r.APIReader = live, live
	stopped := &api.RemediationRequest{}
	if err := live.Get(ctx, client.ObjectKeyFromObject(p), stopped); err != nil {
		t.Fatal(err)
	}
	if err := r.failCheckAfterLook(ctx, stopped, changeVerdict{state: changeClosed, pr: closed}, &holding); err != nil {
		t.Fatal(err)
	}
	if got := g.CheckRuns(head); got[len(got)-1].Status != providers.CheckRunFailure {
		t.Fatalf("the stop's failed check is not last while another pull request of the branch is open: %+v", got)
	}
}

// A stopped holder whose own pull request was moved by a person to another base and closed there, while another open
// pull request from its branch keeps the lock and the failed check is still owed (ReviewCheckNotFailed set): its look
// writes the failed check on the recorded head (decision of 2026-11-20 on #925, point 3, ADR-0045). The check belongs
// to the request's own commit on its own branch, and the other pull request shows it. Its summary promises no close
// (point 4): the stop's summary says only that the request stopped. Without the other pull request, or without the
// note, the look writes nothing and ends the note.
func TestStoppedHolderFailsItsCheckWhileAnotherPullRequestHoldsAfterItsOwnClosedElsewhere(t *testing.T) {
	cases := []struct {
		name          string
		holding, note bool
		writes        bool
	}{
		{name: "another open pull request holds and the check is owed", holding: true, note: true, writes: true},
		{name: "no other open pull request", holding: false, note: true},
		{name: "the check is not owed", holding: true, note: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, g, p, _, cr, head := gateWorld(t)
			ctx := context.Background()
			plainWaiting(t, r, p, head)
			if err := g.CreateBranch(ctx, "hotfix", head); err != nil {
				t.Fatal(err)
			}
			if err := g.SetBase(cr.Number, "hotfix"); err != nil {
				t.Fatal(err)
			}
			if err := g.Close(cr.Number); err != nil {
				t.Fatal(err)
			}
			closed, err := g.GetChangeRequest(ctx, cr.Number)
			if err != nil {
				t.Fatal(err)
			}
			var holding *providers.ChangeRequest
			if tc.holding {
				n, err := g.AddOpenChangeRequest(cr.HeadBranch, mainBranch, "opened by a person")
				if err != nil {
					t.Fatal(err)
				}
				other, err := g.GetChangeRequest(ctx, n)
				if err != nil {
					t.Fatal(err)
				}
				holding = &other
			}
			p.Status.Phase = api.PhaseNeedsIntervention
			p.Status.HoldsTarget = new(true)
			if tc.note {
				meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
					Type: api.ConditionReviewCheckNotFailed, Status: metav1.ConditionTrue, Reason: ReasonGitFailed,
					Message: "not written yet"})
			}
			runs := len(g.CheckRuns(head))
			v := changeVerdict{state: changeClosedElsewhere, pr: closed}
			if err := r.failCheckAfterLook(ctx, p, v, holding); err != nil {
				t.Fatal(err)
			}
			got := g.CheckRuns(head)[runs:]
			noted := meta.FindStatusCondition(p.Status.Conditions, api.ConditionReviewCheckNotFailed) != nil
			if !tc.writes {
				if len(got) != 0 || noted {
					t.Fatalf("wrote %+v (note left: %v): want nothing written and the note ended", got, noted)
				}
				return
			}
			if len(got) != 1 || got[0].Status != providers.CheckRunFailure || got[0].Title != stoppedCheckTitle {
				t.Fatalf("check runs written: %+v: want the stop's failed check", got)
			}
			if strings.Contains(got[0].Summary, "closes") || strings.Contains(got[0].Summary, "hotfix") {
				t.Fatalf("the summary promises a close or shows host text: %q", got[0].Summary)
			}
			if noted || !staleOnHead(p) {
				t.Fatalf("note left: %v, Stale on head: %v: want the note removed and Stale recorded", noted, staleOnHead(p))
			}
		})
	}
}
