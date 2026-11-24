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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/providers"
	fakegit "github.com/shadewright-labs/shadewright/internal/providers/fake"
)

// exitTouched is the annotation a test writes between the exit's read and its write.
const exitTouched = "test.shadewright.io/exit-touched"

// setByTest is the message of a condition a test sets.
const setByTest = "set by the test"

// The names of the cases of the three stop exceptions that close the request's own pull request.
const (
	expiredCase = "the cleanup of an expired request whose own pull request is open"
	staleCase   = "the retried close after a stale cleanup stopped on a refusal"
	cancelCase  = "a cancel settled for its Incident"
)

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
		{name: expiredCase, stop: expiredStop},
		{name: staleCase, stop: staleStop},
		{name: cancelCase, stop: cancelStop},
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
		{name: expiredCase, stop: expiredStop,
			fail: func(g *fakegit.Git) { failCall(g, opGet, 2) }, next: true},
		{name: staleCase, stop: staleStop,
			fail: func(g *fakegit.Git) { failCall(g, opGet, 2) }, next: true},
		{name: cancelCase, stop: cancelStop,
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

// #1008, decisions of 2026-11-21: the look of the one more close of an expired request past its cutoff writes nothing
// by choice (its close does read the pull request before it closes); the close works, and the next look, whose reads answer and which finds
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

// refuseCall makes the n-th call (from 1) of op on g refused (403), and every other call answer.
func refuseCall(g *fakegit.Git, op string, n int) {
	calls := 0
	g.SetFail(func(got string) error {
		if got != op {
			return nil
		}
		calls++
		if calls == n {
			return forbidden()
		}
		return nil
	})
}

// #1008, decision of 2026-11-21 (head adcddc6a): a stop exception's look writes the stopped holder's failed check only
// when its step says it may, after its read showed the request's own pull request open and not merged. A step whose
// read of the pull request the Git host refused (403) says nothing of it: that look writes no check, and the next look
// whose reads answer writes it. On the build before, the expired cleanup and the cancel wrote it in the look with the
// refused read, since no failed Git read marked that look.
func TestStopExceptionLookWithARefusedReadWritesNoCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(r *RemediationRequestReconciler, g *fakegit.Git, rr *api.RemediationRequest)
	}{
		{name: expiredCase, stop: expiredStop},
		{name: staleCase, stop: staleStop},
		{name: cancelCase, stop: cancelStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, g, get, head := stopExceptionWorld(t, tc.stop)
			// The look's own read answers; the step's read of the pull request before its close is refused.
			refuseCall(g, opGet, 2)
			wantNoStopCheck(t, g, runStopExceptionLook(t, r, get), head)
			g.SetFail(nil)
			if _, err := r.checkHeld(context.Background(), get()); err != nil {
				t.Fatalf("checkHeld after the reads answer again: %v", err)
			}
			wantStopCheckWritten(t, g, get(), head)
		})
	}
}

// #1008: a refusal (403) of the close itself comes after the step's read found the pull request open, so that look
// writes the stopped holder's failed check, one case per stop exception that closes. A close that stays refused would
// otherwise leave the pass written before the stop last on the head for as long as the refusal lasts.
func TestStopExceptionLookWithARefusedCloseFailsTheCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(r *RemediationRequestReconciler, g *fakegit.Git, rr *api.RemediationRequest)
	}{
		{name: expiredCase, stop: expiredStop},
		{name: staleCase, stop: staleStop},
		{name: cancelCase, stop: cancelStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, g, get, head := stopExceptionWorld(t, tc.stop)
			refuseCall(g, opClose, 1)
			wantStopCheckWritten(t, g, runStopExceptionLook(t, r, get), head)
		})
	}
}

// #1008, decision of 2026-11-21 (head 6372d593): a closing step may let the look write only when its own read, the one
// made just before it closes, showed the pull request open. A person who closes it between the look's read and that
// read leaves it closed at the step's read, and the Git host answers the close of a closed pull request as it answers
// a close that worked: that look writes no check, one case per stop exception that closes. On the build before, each
// took the close's answer for its own close and wrote the failed check on the closed pull request.
func TestStopExceptionLookWritesNoCheckOnAPullRequestAPersonClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(r *RemediationRequestReconciler, g *fakegit.Git, rr *api.RemediationRequest)
	}{
		{name: expiredCase, stop: expiredStop},
		{name: staleCase, stop: staleStop},
		{name: cancelCase, stop: cancelStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, g, get, head := stopExceptionWorld(t, tc.stop)
			n := get().Status.PullRequest.Number
			calls := 0
			// The look's own read finds it open; a person closes it just before the step's read.
			g.SetFail(func(op string) error {
				if op != opGet {
					return nil
				}
				if calls++; calls == 2 {
					if err := g.Close(n); err != nil {
						return err
					}
				}
				return nil
			})
			wantNoStopCheck(t, g, runStopExceptionLook(t, r, get), head)
		})
	}
}

// #1008: a reconcile cancelled while the exit writes the failed check returns the cancellation and makes no status
// write with the cancelled context; the next look records the outcome. On the build before, the exit swallowed the
// cancellation and wrote Stale with the cancelled context.
func TestStopExceptionExitCancelledDuringTheCheckWritesNoStatus(t *testing.T) {
	r, g, get, head := stopExceptionWorld(t, expiredStop)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.SetAfter(func(op string) error {
		if op == opCreateCheckRun {
			cancel()
		}
		return nil
	})
	if _, err := r.checkHeld(ctx, get()); !errors.Is(err, context.Canceled) {
		t.Fatalf("checkHeld: %v, want the cancellation returned", err)
	}
	if live := get(); staleOnHead(live) {
		t.Fatalf("review check %+v: want no status write after the cancel", live.Status.PullRequest.ReviewCheck)
	}
	g.SetAfter(nil)
	if _, err := r.checkHeld(context.Background(), get()); err != nil {
		t.Fatalf("checkHeld after the cancel: %v", err)
	}
	wantStopCheckWritten(t, g, get(), head)
}

// #1008, ADR-0046 amendment (decision of 2026-11-21): a cancel whose own pull request, from an open whose answer was
// lost, is found open with none recorded records it (recordCancelledOpen), and that look writes the stopped holder's
// failed check on the head it recorded. The recorded head is the one the pass was written on.
func TestStopExceptionCancelRecordingItsOpenPullRequestFailsTheCheck(t *testing.T) {
	r, g, get, head := stopExceptionWorld(t, func(_ *RemediationRequestReconciler, _ *fakegit.Git,
		rr *api.RemediationRequest) {
		rr.Status.Phase = api.PhaseCancelled
		rr.Status.PullRequest = nil
		meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionApproved)
		setFalseCondition(rr, api.ConditionPROpened, api.ReasonIncidentGone)
	})
	live := runStopExceptionLook(t, r, get)
	if live.Status.PullRequest == nil || live.Status.PullRequest.HeadSHA != head {
		t.Fatalf("pull request %+v: want the open one recorded with head %s", live.Status.PullRequest, head)
	}
	wantStopCheckWritten(t, g, live, head)
}

// #1008, ADR-0046 amendment (decision of 2026-11-21): the look reads the own pull request open on the target branch,
// and a person moves it to another base before the closing step's read. The step closes nothing there
// (errOpenElsewhere), and the look writes the stopped holder's failed check: the step's own read showed it open. One
// case per step that ends on it: the expired cleanup and the cancel.
func TestStopExceptionLookFindingItsPullRequestOnAnotherBaseFailsTheCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(r *RemediationRequestReconciler, g *fakegit.Git, rr *api.RemediationRequest)
	}{
		{name: expiredCase, stop: expiredStop},
		{name: cancelCase, stop: cancelStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, g, get, head := stopExceptionWorld(t, tc.stop)
			n := get().Status.PullRequest.Number
			if err := g.CreateBranch(context.Background(), movedBranch, head); err != nil {
				t.Fatal(err)
			}
			calls := 0
			g.SetFail(func(op string) error {
				if op != opGet {
					return nil
				}
				if calls++; calls == 2 {
					return g.SetBase(n, movedBranch)
				}
				return nil
			})
			live := runStopExceptionLook(t, r, get)
			if cr, err := g.GetChangeRequest(context.Background(), n); err != nil ||
				cr.State != providers.ChangeRequestOpen || cr.BaseBranch != movedBranch {
				t.Fatalf("pull request %+v, %v: want it left open on %s", cr, err, movedBranch)
			}
			wantStopCheckWritten(t, g, live, head)
		})
	}
}

// mergedStaleStop is the stop of a request whose stale plan's pull request was merged before its cleanup closed it:
// the look records the merge as a stale plan's (recordStoppedMerge). withCommit says whether the host reports the
// merge commit yet.
func mergedStaleStop(t *testing.T, withCommit bool) func(*RemediationRequestReconciler, *fakegit.Git,
	*api.RemediationRequest) {
	return func(_ *RemediationRequestReconciler, g *fakegit.Git, rr *api.RemediationRequest) {
		setFalseCondition(rr, api.ConditionApproved, ReasonPullRequestOpened)
		n := rr.Status.PullRequest.Number
		if _, err := g.MergeWithCommit(n); err != nil {
			t.Fatal(err)
		}
		if !withCommit {
			// A host that reports no merge commit yet.
			if err := g.SetMergeCommit(n, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// #1008, decision of 2026-11-21: the record of a merge that waits for the merge commit writes no failed check of the
// stop: the head is on the target branch, where neither ADR-0046 nor ADR-0028 section 5 puts a check. On the build
// before, that look wrote the stop's failed check on the merged head while GitReadRetry said the read was retried.
func TestStopExceptionLookWaitingForTheMergeCommitWritesNoCheck(t *testing.T) {
	r, g, get, head := stopExceptionWorld(t, mergedStaleStop(t, false))
	for range 2 {
		live := runStopExceptionLook(t, r, get)
		wantNoStopCheck(t, g, live, head)
		if mergeRecorded(live) {
			t.Fatalf("phase %s: the merge was recorded without its merge commit", live.Status.Phase)
		}
	}
}

// #1008, decision of 2026-11-21, the record of a merge and the read of who records it after a handoff: no look of
// such a stop exception writes the failed check, also once the reads answer, since the pull request is merged. The look
// whose reads answer records the merge (or leaves it to the request that records it) and writes nothing, and no
// ReviewCheckNotFailed says a check is owed.
func TestStopExceptionMergeLooksNeverWriteTheCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(r *RemediationRequestReconciler, g *fakegit.Git, rr *api.RemediationRequest)
		fail func(g *fakegit.Git)
	}{
		{name: "the record of a stale plan's merge", stop: mergedStaleStop(t, true),
			fail: func(g *fakegit.Git) { failOp(g, opCheckMerged) }},
		{name: "who records a merge after an aborted handoff", stop: abortedHandoffMerge(t),
			fail: func(g *fakegit.Git) { failOp(g, opParents) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, g, get, head := stopExceptionWorld(t, tc.stop)
			tc.fail(g)
			wantNoStopCheck(t, g, runStopExceptionLook(t, r, get), head)
			g.SetFail(nil)
			taken := 0
			r.stopExceptionTaken = func() { taken++ }
			if _, err := r.checkHeld(context.Background(), get()); err != nil {
				t.Fatalf("checkHeld after the reads answer again: %v", err)
			}
			if taken == 0 {
				t.Fatal("no stop exception took the look whose reads answered over")
			}
			live := get()
			wantNoStopCheck(t, g, live, head)
			if live.Status.Phase == api.PhaseNeedsIntervention && !mergeRecorded(live) &&
				meta.FindStatusCondition(live.Status.Conditions, api.ConditionGitReadRetry) != nil {
				t.Fatalf("conditions %v: the look whose reads answered still retries a read", live.Status.Conditions)
			}
		})
	}
}

// abortedHandoffMerge is the stop of a request whose handoff to a replacement was aborted, and whose pull request was
// merged with a commit on top of its recorded head: who records the merge is read from the branch (stoppedMergeOwner).
func abortedHandoffMerge(t *testing.T) func(*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest) {
	return func(r *RemediationRequestReconciler, g *fakegit.Git, rr *api.RemediationRequest) {
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
	}
}

// #1008, decision of 2026-11-21 (design 6, the handoff): a stopped replacement that took the pull request over, whose
// pull request was merged with the head its predecessor recorded while the predecessor still holds the lock and has
// not recorded that merge yet, waits for the predecessor's record (ownerPredecessorPending) and writes nothing to Git,
// the failed check of its stop included. On the build before, that look wrote the stop's failed check on the
// replacement's recorded head.
func TestStoppedReplacementWaitingForThePredecessorsRecordWritesNoCheck(t *testing.T) {
	r, g, get, head := stopExceptionWorld(t, func(r *RemediationRequestReconciler, g *fakegit.Git,
		rr *api.RemediationRequest) {
		ctx := context.Background()
		pred := &api.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Name: rr.Name + "-predecessor",
			Namespace: rr.Namespace}, Spec: rr.Spec}
		if err := r.Create(ctx, pred); err != nil {
			t.Fatal(err)
		}
		pred.UID = "5e771e00-0000-4000-8000-000000000003"
		if err := r.Update(ctx, pred); err != nil {
			t.Fatal(err)
		}
		cr, err := g.GetChangeRequest(ctx, rr.Status.PullRequest.Number)
		if err != nil {
			t.Fatal(err)
		}
		// The predecessor's own head: a commit a person put back on the branch, then merged.
		predHead, err := g.Push(cr.HeadBranch, sourcePath, []byte("the predecessor's change\n"))
		if err != nil {
			t.Fatal(err)
		}
		pred.Status = *rr.Status.DeepCopy()
		pred.Status.PullRequest.HeadSHA = predHead
		pred.Status.PullRequest.ReviewCheck = nil
		pred.Status.Handoff = nil
		if err := r.Status().Update(ctx, pred); err != nil {
			t.Fatal(err)
		}
		if _, err := g.MergeWithCommit(cr.Number); err != nil {
			t.Fatal(err)
		}
		rr.Status.Handoff = &api.Handoff{From: &api.IncomingHandoff{Name: api.Name(pred.Name), UID: api.UID(pred.UID),
			Hint:  api.HintRef{Source: api.HintSourceGitHub, CommentID: 1, Digest: strings.Repeat("a", 64)},
			Phase: api.HandoffDone}}
		setFalseCondition(rr, api.ConditionApproved, ReasonPullRequestOpened)
	})
	owner, err := r.stoppedMergeOwner(context.Background(), get(), changeVerdict{state: changeMerged,
		pr: providers.ChangeRequest{Number: get().Status.PullRequest.Number, HeadSHA: lastHead(t, g, get()),
			State: providers.ChangeRequestMerged, BaseBranch: mainBranch}})
	if err != nil || owner != ownerPredecessorPending {
		t.Fatalf("who records the merge: %v, %v; want ownerPredecessorPending, or the case does not reach its path",
			owner, err)
	}
	live := runStopExceptionLook(t, r, get)
	wantNoStopCheck(t, g, live, head)
	if mergeRecorded(live) {
		t.Fatalf("phase %s: the replacement recorded a merge its predecessor records", live.Status.Phase)
	}
}

// lastHead is the head of rr's recorded pull request as the host shows it now.
func lastHead(t *testing.T, g *fakegit.Git, rr *api.RemediationRequest) string {
	t.Helper()
	cr, err := g.GetChangeRequest(context.Background(), rr.Status.PullRequest.Number)
	if err != nil {
		t.Fatal(err)
	}
	return cr.HeadSHA
}
