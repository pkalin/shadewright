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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// setByTest is the message of a condition a test sets.
const setByTest = "set by the test"

// stopExceptionWorld is a stopped holder (lock held, reviewer hints on) whose own pull request is open on the target
// branch with a passing review check on its recorded head, written before the stop, and another pull request from its
// branch, without the marker, open on the target branch too: that one keeps the lock once the own one is closed or
// merged. stop puts the request in the state of the stop exception under test, in the API server.
func stopExceptionWorld(t *testing.T, stop func(r *RemediationRequestReconciler, g *fakegit.Git,
	rr *api.RemediationRequest)) (*RemediationRequestReconciler, *fakegit.Git, func() *api.RemediationRequest, string) {
	t.Helper()
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	r.ReviewerHints = true
	r.Recorder = record.NewFakeRecorder(100)
	setTargetMain(t, r)
	pr := rr.Status.PullRequest
	if err := g.CreateCheckRun(ctx, pr.HeadSHA, providers.CheckRun{Name: providers.CheckRunName,
		Status: providers.CheckRunSuccess, Title: "passed before the stop"}); err != nil {
		t.Fatal(err)
	}
	cr, err := g.GetChangeRequest(ctx, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.AddOpenChangeRequest(cr.HeadBranch, mainBranch, "opened by a person"); err != nil {
		t.Fatal(err)
	}
	get := func() *api.RemediationRequest {
		t.Helper()
		live := &api.RemediationRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
			t.Fatal(err)
		}
		return live
	}
	stop(r, g, rr)
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	return r, g, get, pr.HeadSHA
}

// setFalseCondition sets a False condition of type kind with reason on rr, in memory.
func setFalseCondition(rr *api.RemediationRequest, kind, reason string) {
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: kind, Status: metav1.ConditionFalse,
		Reason: reason, Message: setByTest})
}

// #1008, closing ADR-0046 limit (c): a look of a stopped holder that a stop exception takes over, and that leaves the
// lock held through another open pull request from the branch, writes the stop's failed review check on the recorded
// head in that same look (checkHeld's single exit), one case per stop exception. On the build before, each of these
// looks wrote nothing and left the pass written before the stop last until the next look.
func TestStopExceptionLookFailsTheCheckInThatLook(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(r *RemediationRequestReconciler, g *fakegit.Git, rr *api.RemediationRequest)
	}{
		{name: "the cleanup of an expired request whose own pull request is open", stop: func(
			_ *RemediationRequestReconciler, _ *fakegit.Git, rr *api.RemediationRequest) {
			rr.Status.Phase = api.PhaseExpired
			setFalseCondition(rr, api.ConditionApproved, ReasonPullRequestDeadline)
		}},
		{name: "the one more close of an expired request past its cutoff", stop: func(
			_ *RemediationRequestReconciler, _ *fakegit.Git, rr *api.RemediationRequest) {
			rr.Status.Phase = api.PhaseExpired
			setFalseCondition(rr, api.ConditionApproved, ReasonPullRequestDeadline)
			meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionCleanupFailing,
				Status: metav1.ConditionTrue, Reason: ReasonCleanupFailed, Message: setByTest,
				LastTransitionTime: metav1.NewTime(time.Now().Add(-cleanupFailingBound - time.Hour))})
		}},
		{name: "the retried close after a stale cleanup stopped on a refusal", stop: func(
			_ *RemediationRequestReconciler, _ *fakegit.Git, rr *api.RemediationRequest) {
			setFalseCondition(rr, api.ConditionApproved, ReasonPullRequestOpened)
			setFalseCondition(rr, api.ConditionPlanFresh, ReasonGitForbidden)
		}},
		{name: "a cancel settled for its Incident", stop: func(
			_ *RemediationRequestReconciler, _ *fakegit.Git, rr *api.RemediationRequest) {
			rr.Status.Phase = api.PhaseCancelled
			setFalseCondition(rr, api.ConditionApproved, api.ReasonIncidentGone)
		}},
		{name: "the record of a merge whose check at the merge fails to read", stop: func(
			_ *RemediationRequestReconciler, g *fakegit.Git, rr *api.RemediationRequest) {
			// A stop for another reason than a branch change: the merge is recorded as a stale plan's
			// (recordStoppedMerge), which reads the merge.
			setFalseCondition(rr, api.ConditionApproved, ReasonPullRequestOpened)
			if _, err := g.MergeWithCommit(rr.Status.PullRequest.Number); err != nil {
				t.Fatal(err)
			}
			g.SetFail(func(op string) error {
				if op == opCheckMerged {
					return &providers.RetryableError{Err: errors.New("status 502: bad gateway")}
				}
				return nil
			})
		}},
		{name: "a failed read of who records a merge after a handoff", stop: func(r *RemediationRequestReconciler,
			g *fakegit.Git, rr *api.RemediationRequest) {
			ctx := context.Background()
			repl := &api.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Name: rr.Name + "-replacement",
				Namespace: rr.Namespace}, Spec: rr.Spec}
			if err := r.Create(ctx, repl); err != nil {
				t.Fatal(err)
			}
			repl.UID = "5e771e00-0000-4000-8000-000000000002"
			if err := r.Update(ctx, repl); err != nil {
				t.Fatal(err)
			}
			rr.Status.Handoff = &api.Handoff{To: &api.OutgoingHandoff{Name: api.Name(repl.Name),
				UID: api.UID(repl.UID), Phase: api.HandoffAborted, ExpectedHead: rr.Status.PullRequest.HeadSHA}}
			// A commit on top of the recorded head, then the merge: who records it is read from the branch.
			cr, err := g.GetChangeRequest(ctx, rr.Status.PullRequest.Number)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := g.Push(cr.HeadBranch, sourcePath, []byte("pushed by the replacement\n")); err != nil {
				t.Fatal(err)
			}
			if _, err := g.MergeWithCommit(cr.Number); err != nil {
				t.Fatal(err)
			}
			g.SetFail(func(op string) error {
				if op == opParents {
					return &providers.RetryableError{Err: errors.New("status 502: bad gateway")}
				}
				return nil
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, g, get, head := stopExceptionWorld(t, tc.stop)
			taken := 0
			r.stopExceptionTaken = func() { taken++ }
			if _, err := r.checkHeld(context.Background(), get()); err != nil {
				t.Fatalf("checkHeld: %v", err)
			}
			live := get()
			if taken == 0 {
				t.Fatalf("no stop exception took the look over (conditions %v): the case does not reach its path",
					live.Status.Conditions)
			}
			if !holdsLock(live) {
				t.Fatalf("the lock was released while another pull request from the branch is open: %v",
					live.Status.Conditions)
			}
			last := lastReviewCheck(g.CheckRuns(head))
			if last == nil || last.Status != providers.CheckRunFailure || last.Title != stoppedCheckTitle {
				t.Fatalf("check runs on the recorded head %+v: want the stop's failed check last after the look a stop "+
					"exception took over", g.CheckRuns(head))
			}
			if !staleOnHead(live) || meta.FindStatusCondition(live.Status.Conditions,
				api.ConditionReviewCheckNotFailed) != nil {
				t.Fatalf("Stale on head %t, conditions %v: want Stale recorded and no ReviewCheckNotFailed",
					staleOnHead(live), live.Status.Conditions)
			}
		})
	}
}

// #1008, ADR-0046 limit (e): the one more close of an expired request past its cutoff waits under a pause of the kill
// switch, and checkHeld's exit then writes no failed review check in that look: WritesPaused says why, not
// ReviewCheckNotFailed. Without the pause the same look writes it (TestStopExceptionLookFailsTheCheckInThatLook).
func TestStopExceptionLookUnderAPauseWritesNoCheck(t *testing.T) {
	r, g, get, head := stopExceptionWorld(t, func(_ *RemediationRequestReconciler, _ *fakegit.Git,
		rr *api.RemediationRequest) {
		rr.Status.Phase = api.PhaseExpired
		setFalseCondition(rr, api.ConditionApproved, ReasonPullRequestDeadline)
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionCleanupFailing,
			Status: metav1.ConditionTrue, Reason: ReasonCleanupFailed, Message: setByTest,
			LastTransitionTime: metav1.NewTime(time.Now().Add(-cleanupFailingBound - time.Hour))})
	})
	setPaused(t, r, true)
	res, err := r.checkHeld(context.Background(), get())
	if err != nil || res.RequeueAfter != freshRecheckDelay {
		t.Fatalf("checkHeld under a pause: %+v, %v; want the pause's wait %v", res, err, freshRecheckDelay)
	}
	live := get()
	if c := meta.FindStatusCondition(live.Status.Conditions, api.ConditionWritesPaused); c == nil {
		t.Fatalf("conditions %v: want WritesPaused", live.Status.Conditions)
	}
	if c := meta.FindStatusCondition(live.Status.Conditions, api.ConditionReviewCheckNotFailed); c != nil {
		t.Fatalf("ReviewCheckNotFailed %+v under a pause: WritesPaused says why", c)
	}
	if last := lastReviewCheck(g.CheckRuns(head)); last == nil || last.Status != providers.CheckRunSuccess {
		t.Fatalf("check runs %+v: want nothing written under the pause", g.CheckRuns(head))
	}
}

// #1008: the exit's status write that records Stale meets a conflict (an annotation written between its read and its
// write): it reads the request again and carries the outcome over, so Stale is recorded in that look, the annotation
// is kept, and the failed check run is written once, not again for the retry.
func TestStopExceptionExitCarriesItsOutcomeOverAConflict(t *testing.T) {
	r, g, get, head := stoppedHolderWorld(t, ownClosed)
	inner, ok := r.Client.(client.WithWatch)
	if !ok {
		t.Fatal("the test client is not a client.WithWatch")
	}
	touched := false
	r.Client = interceptor.NewClient(inner, interceptor.Funcs{SubResourceUpdate: func(ctx context.Context,
		c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if rr, ok := obj.(*api.RemediationRequest); ok && !touched && rr.Status.PullRequest != nil &&
			rr.Status.PullRequest.ReviewCheck != nil && rr.Status.PullRequest.ReviewCheck.State == api.ReviewCheckStale {
			touched = true
			other := &api.RemediationRequest{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(rr), other); err != nil {
				return err
			}
			other.Annotations = map[string]string{"example.com/touched": "yes"}
			if err := c.Update(ctx, other); err != nil {
				return err
			}
		}
		return c.SubResource(sub).Update(ctx, obj, opts...)
	}})
	r.APIReader = inner
	if _, err := r.checkHeld(context.Background(), get()); err != nil {
		t.Fatalf("checkHeld: %v", err)
	}
	live := get()
	if !touched {
		t.Fatal("no status write recorded Stale: the case does not reach the exit's write")
	}
	if !staleOnHead(live) || live.Annotations["example.com/touched"] != "yes" {
		t.Fatalf("Stale on head %t, annotations %v: want Stale recorded over the conflict and the annotation kept",
			staleOnHead(live), live.Annotations)
	}
	failed := 0
	for _, run := range g.CheckRuns(head) {
		if run.Status == providers.CheckRunFailure {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("check runs %+v: want the failed check written once", g.CheckRuns(head))
	}
}
