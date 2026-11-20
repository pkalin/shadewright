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
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
// before the write, and a stopped, released or deleted one gets nothing (model seed 35217). A failed live read writes
// nothing and is returned. A live request that still waits gets the write.
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
		{name: "live read fails", phase: api.PhaseAwaitingApproval, readFails: true},
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
			case c.readFails && !errors.Is(err, errReviewCheckLiveRead):
				t.Fatalf("a failed live read returned %v: want errReviewCheckLiveRead for a retry", err)
			case !c.readFails && !c.writes && err != nil:
				// A write that went through records its state from the older copy, which meets a conflict; the next
				// look records it from the host (syncReviewCheck), so only the skipped writes must return nil.
				t.Fatal(err)
			}
			got := g.CheckRuns(head)
			if c.writes {
				if len(got) != runs+1 {
					t.Fatalf("a request that still waits for review got no check: %+v", got)
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
// ReviewCheckNotFailed. A failed read writes nothing and leaves that condition as it was; a failed write again says
// so with it (decision of 2026-11-20 on #925).
func TestStoppedHolderWritesItsFailedCheckAgain(t *testing.T) {
	cases := []struct {
		name string
		// over writes a pending check after the stop's failed one.
		over bool
		fail string
		// noted: condition ReviewCheckNotFailed is set before the look, by an earlier look whose write failed.
		noted bool
		// writes: a failed check is written; note: condition ReviewCheckNotFailed is set; err: an error is returned.
		writes, note bool
	}{
		{name: "pending last", over: true, writes: true},
		{name: "failed last"},
		{name: "read fails", over: true, fail: "ReviewCheckRun"},
		{name: "write fails", over: true, fail: "CreateCheckRun", note: true},
		{name: "failed last ends the note", noted: true},
		{name: "read fails keeps the note", over: true, fail: "ReviewCheckRun", noted: true, note: true},
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
			runs := len(g.CheckRuns(head))
			if err := r.failStoppedCheck(ctx, p, string(api.PhaseNeedsIntervention)); err != nil && !c.note {
				t.Fatalf("failStoppedCheck: %v", err)
			}
			got := g.CheckRuns(head)
			if c.writes != (len(got) == runs+1) {
				t.Fatalf("check runs %+v: want a new one %t", got, c.writes)
			}
			if last := got[len(got)-1]; c.writes && (last.Status != providers.CheckRunFailure ||
				last.Title != stoppedCheckTitle) {
				t.Fatalf("the check written again is %+v: want the stop's failed check", last)
			}
			note := meta.FindStatusCondition(p.Status.Conditions, api.ConditionReviewCheckNotFailed) != nil
			if note != c.note {
				t.Fatalf("condition ReviewCheckNotFailed set %t: want %t", note, c.note)
			}
			if rc := p.Status.PullRequest.ReviewCheck; rc == nil || rc.State != api.ReviewCheckStale {
				t.Fatalf("review check %+v: want Stale kept", rc)
			}
		})
	}
}
