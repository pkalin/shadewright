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

// exitTouched is the annotation a test writes between the exit's read and its write.
const exitTouched = "test.shadewright.io/exit-touched"

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

// badGateway is a failed Git read of the fake host that the executor retries (GitReadRetry).
func badGateway() error {
	return &providers.RetryableError{Err: errors.New("status 502: bad gateway")}
}

// failCall makes the n-th call (from 1) of op on g fail with a failed Git read, and every other call answer.
func failCall(g *fakegit.Git, op string, n int) {
	calls := 0
	g.SetFail(func(got string) error {
		if got != op {
			return nil
		}
		calls++
		if calls == n {
			return badGateway()
		}
		return nil
	})
}

// failOp makes every call of op on g fail with a failed Git read.
func failOp(g *fakegit.Git, op string) {
	g.SetFail(func(got string) error {
		if got == op {
			return badGateway()
		}
		return nil
	})
}

// expiredStop is the stop of an expired request whose own pull request is open.
func expiredStop(_ *RemediationRequestReconciler, _ *fakegit.Git, rr *api.RemediationRequest) {
	rr.Status.Phase = api.PhaseExpired
	setFalseCondition(rr, api.ConditionApproved, ReasonPullRequestDeadline)
}

// staleStop is the stop of a request whose stale plan's cleanup stopped on a refusal.
func staleStop(_ *RemediationRequestReconciler, _ *fakegit.Git, rr *api.RemediationRequest) {
	setFalseCondition(rr, api.ConditionApproved, ReasonPullRequestOpened)
	setFalseCondition(rr, api.ConditionPlanFresh, ReasonGitForbidden)
}

// cancelStop is the stop of a request cancelled for its Incident.
func cancelStop(_ *RemediationRequestReconciler, _ *fakegit.Git, rr *api.RemediationRequest) {
	rr.Status.Phase = api.PhaseCancelled
	setFalseCondition(rr, api.ConditionApproved, api.ReasonIncidentGone)
}

// pastCutoffStop is the stop of an expired request whose cleanup passed its cutoff.
func pastCutoffStop(r *RemediationRequestReconciler, g *fakegit.Git, rr *api.RemediationRequest) {
	expiredStop(r, g, rr)
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionCleanupFailing,
		Status: metav1.ConditionTrue, Reason: ReasonCleanupFailed, Message: setByTest,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-cleanupFailingBound - time.Hour))})
}

// runStopExceptionLook runs one look of checkHeld and fails the test unless a stop exception took it over and the
// lock is still held; it returns the request as stored after the look.
func runStopExceptionLook(t *testing.T, r *RemediationRequestReconciler,
	get func() *api.RemediationRequest) *api.RemediationRequest {
	t.Helper()
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
	return live
}

// wantStopCheckWritten fails the test unless the stop's failed check is the last on head, Stale is recorded and
// ReviewCheckNotFailed is not set.
func wantStopCheckWritten(t *testing.T, g *fakegit.Git, live *api.RemediationRequest, head string) {
	t.Helper()
	last := lastReviewCheck(g.CheckRuns(head))
	if last == nil || last.Status != providers.CheckRunFailure || last.Title != stoppedCheckTitle {
		t.Fatalf("check runs on the recorded head %+v: want the stop's failed check last", g.CheckRuns(head))
	}
	if !staleOnHead(live) || meta.FindStatusCondition(live.Status.Conditions,
		api.ConditionReviewCheckNotFailed) != nil {
		t.Fatalf("Stale on head %t, conditions %v: want Stale recorded and no ReviewCheckNotFailed",
			staleOnHead(live), live.Status.Conditions)
	}
}

// wantNoStopCheck fails the test unless the pass written before the stop is still the last check on head, Stale is
// not recorded and ReviewCheckNotFailed is not set.
func wantNoStopCheck(t *testing.T, g *fakegit.Git, live *api.RemediationRequest, head string) {
	t.Helper()
	if last := lastReviewCheck(g.CheckRuns(head)); last == nil || last.Status != providers.CheckRunSuccess {
		t.Fatalf("check runs on the recorded head %+v: want nothing written in this look", g.CheckRuns(head))
	}
	if staleOnHead(live) || meta.FindStatusCondition(live.Status.Conditions,
		api.ConditionReviewCheckNotFailed) != nil {
		t.Fatalf("Stale on head %t, conditions %v: want neither Stale nor ReviewCheckNotFailed",
			staleOnHead(live), live.Status.Conditions)
	}
}

// #1008, closing ADR-0046 limit (c): a look of a stopped holder that a stop exception takes over, whose Git reads
// answered, and that leaves the lock held through another open pull request from the branch, writes the stop's failed
// review check on the recorded head in that same look (checkHeld's single exit), one case per stop exception. On the
// build before, each of these looks wrote nothing and left the pass written before the stop last for as long as the
// stop exception took the looks over.
func TestStopExceptionLookFailsTheCheckInThatLook(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(r *RemediationRequestReconciler, g *fakegit.Git, rr *api.RemediationRequest)
	}{
		{name: "the cleanup of an expired request whose own pull request is open", stop: expiredStop},
		{name: "the retried close after a stale cleanup stopped on a refusal", stop: staleStop},
		{name: "a cancel settled for its Incident", stop: cancelStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, g, get, head := stopExceptionWorld(t, tc.stop)
			wantStopCheckWritten(t, g, runStopExceptionLook(t, r, get), head)
		})
	}
}

// #1008, ADR-0046 limit (b) (decision of 2026-11-21): a look that a stop exception takes over and whose step ends in a
// failed Git read writes no failed review check and sets no ReviewCheckNotFailed; GitReadRetry says why. The first
// later look whose reads answer writes it where the rule still owes it. One case per stop exception that reads.
func TestStopExceptionLookWithAFailedReadWritesNoCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(r *RemediationRequestReconciler, g *fakegit.Git, rr *api.RemediationRequest)
		// fail makes the step's read fail after the look's own reads answered.
		fail func(g *fakegit.Git)
		// next is whether the next look, with the reads answering, still owes the check (no merge is recorded).
		next bool
	}{
		{name: "the cleanup of an expired request whose own pull request is open", stop: expiredStop,
			fail: func(g *fakegit.Git) { failCall(g, opGet, 2) }, next: true},
		{name: "the retried close after a stale cleanup stopped on a refusal", stop: staleStop,
			fail: func(g *fakegit.Git) { failCall(g, opGet, 2) }, next: true},
		{name: "a cancel settled for its Incident", stop: cancelStop,
			fail: func(g *fakegit.Git) { failCall(g, opGet, 2) }, next: true},
		{name: "the record of a merge whose check at the merge fails to read", stop: func(
			_ *RemediationRequestReconciler, g *fakegit.Git, rr *api.RemediationRequest) {
			// A stop for another reason than a branch change: the merge is recorded as a stale plan's
			// (recordStoppedMerge), which reads the merge.
			setFalseCondition(rr, api.ConditionApproved, ReasonPullRequestOpened)
			if _, err := g.MergeWithCommit(rr.Status.PullRequest.Number); err != nil {
				t.Fatal(err)
			}
		}, fail: func(g *fakegit.Git) { failOp(g, opCheckMerged) }},
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
		}, fail: func(g *fakegit.Git) { failOp(g, opParents) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, g, get, head := stopExceptionWorld(t, tc.stop)
			tc.fail(g)
			live := runStopExceptionLook(t, r, get)
			if c := meta.FindStatusCondition(live.Status.Conditions, api.ConditionGitReadRetry); c == nil {
				t.Fatalf("conditions %v: want GitReadRetry for the failed read", live.Status.Conditions)
			}
			wantNoStopCheck(t, g, live, head)
			if !tc.next {
				return
			}
			g.SetFail(nil)
			if _, err := r.checkHeld(context.Background(), get()); err != nil {
				t.Fatalf("checkHeld after the reads answer again: %v", err)
			}
			wantStopCheckWritten(t, g, get(), head)
		})
	}
}

// #1008, decision of 2026-11-21: the one more close of an expired request past its cutoff makes no read the check
// could follow, so its look writes nothing; the close works, and the next look, whose reads answer and which finds
// another pull request from the branch open, writes the failed check.
func TestStopExceptionOnceMoreCloseWritesAtTheNextLook(t *testing.T) {
	r, g, get, head := stopExceptionWorld(t, pastCutoffStop)
	wantNoStopCheck(t, g, runStopExceptionLook(t, r, get), head)
	r.stopExceptionTaken = nil
	if _, err := r.checkHeld(context.Background(), get()); err != nil {
		t.Fatalf("checkHeld after the close: %v", err)
	}
	live := get()
	if !holdsLock(live) {
		t.Fatalf("the lock was released while another pull request from the branch is open: %v",
			live.Status.Conditions)
	}
	wantStopCheckWritten(t, g, live, head)
}

// #1008, the amendment's limit on an error return (decision of 2026-11-21): a stop exception's step that returns an
// error that is no failed Git read (here the failed status write of the expired cleanup whose close works again)
// skips checkHeld's exit, so that look writes no failed check; the reconcile is retried by the error backoff, and the
// next look writes it.
func TestStopExceptionLookThatReturnsAnErrorWritesAtTheNextLook(t *testing.T) {
	r, g, get, head := stopExceptionWorld(t, func(r *RemediationRequestReconciler, g *fakegit.Git,
		rr *api.RemediationRequest) {
		expiredStop(r, g, rr)
		// A close that failed a moment ago, well within its cutoff: the close that works now ends it in a status
		// write of the step (cleanUpExpired).
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionCleanupFailing,
			Status: metav1.ConditionTrue, Reason: ReasonCleanupFailed, Message: setByTest,
			LastTransitionTime: metav1.Now()})
	})
	inner, ok := r.Client.(client.WithWatch)
	if !ok {
		t.Fatal("the test client is not a client.WithWatch")
	}
	broken := true
	r.Client = interceptor.NewClient(inner, interceptor.Funcs{SubResourceUpdate: func(ctx context.Context,
		c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if broken {
			return errors.New("the API server is unavailable")
		}
		return c.SubResource(sub).Update(ctx, obj, opts...)
	}})
	r.APIReader = inner
	taken := 0
	r.stopExceptionTaken = func() { taken++ }
	if _, err := r.checkHeld(context.Background(), get()); err == nil {
		t.Fatal("checkHeld: want the failed status write returned for the error backoff")
	}
	if taken == 0 {
		t.Fatal("no stop exception took the look over: the case does not reach its path")
	}
	wantNoStopCheck(t, g, get(), head)
	broken = false
	if _, err := r.checkHeld(context.Background(), get()); err != nil {
		t.Fatalf("checkHeld after the error: %v", err)
	}
	live := get()
	if !holdsLock(live) {
		t.Fatalf("the lock was released while another pull request from the branch is open: %v",
			live.Status.Conditions)
	}
	wantStopCheckWritten(t, g, live, head)
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
			other.Annotations = map[string]string{exitTouched: "1008"}
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
	if !staleOnHead(live) || live.Annotations[exitTouched] != "1008" {
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
