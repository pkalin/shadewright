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
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
)

// expiredWorld is a request in PolicyChecked whose pull request was opened while the status write that would record
// it was lost (startPolicyChecked of the model), with its deadline passed.
func expiredWorld(t *testing.T) *modelWorld {
	t.Helper()
	// #nosec G404 -- a seeded generator for the model's world; the test draws nothing from it.
	m := newModelFrom(t, 1, rand.New(rand.NewPCG(1, 2)), requestStatusSchema(t),
		modelStart{start: startPolicyChecked, focus: focusNone})
	m.clock.advance(modelPolicyDeadline + time.Minute)
	return m
}

// reconcileExpired runs one reconcile of the model's request and returns its result.
func (m *modelWorld) reconcileExpired(t *testing.T) (reconcile.Result, *api.RemediationRequest, error) {
	t.Helper()
	res, err := m.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: m.key})
	return res, m.live(), err
}

// deadlineCondition is the condition of an expired request that holds reason PullRequestDeadline.
func deadlineCondition(rr *api.RemediationRequest) string {
	for _, t := range []string{api.ConditionPROpened, api.ConditionApproved} {
		if c := meta.FindStatusCondition(rr.Status.Conditions, t); c != nil && strings.HasPrefix(c.Reason, ReasonPullRequestDeadline) {
			return c.Reason + ": " + c.Message
		}
	}
	return ""
}

// Sixth decision of 2026-11-18 on #771, point 5: the cleanup of an expired request whose close of its own pull
// request keeps failing (its record was lost, so the read after the stop found it by its marker) shows condition
// CleanupFailing, says so in the deadline's message, is retried at the pace of a stopped holder and never as an error
// for the rate limiter, and ends 24 hours after the first failure with PullRequestDeadlineCleanupFailed.
func TestExpiredCloseFailingIsShownAndBounded(t *testing.T) {
	m := expiredWorld(t)
	m.g.SetFail(func(op string) error {
		if op == opClose {
			return &providers.RetryableError{Err: errors.New("status 503")}
		}
		return nil
	})
	res, rr, err := m.reconcileExpired(t)
	if err != nil || rr.Status.Phase != api.PhaseExpired {
		t.Fatalf("the end: %+v, %v, phase %s; want Expired", res, err, rr.Status.Phase)
	}
	// The first write of the failure is lost: nothing is recorded, and the next try records it.
	m.loseNext = true
	if _, _, err := m.reconcileExpired(t); err == nil {
		t.Fatal("a lost status write is returned for the error backoff")
	}
	first := m.clock.at
	for i := range 3 {
		res, rr, err = m.reconcileExpired(t)
		if err != nil {
			t.Fatalf("try %d: a failed close is handed to the rate limiter: %v", i, err)
		}
		if res.RequeueAfter != heldRecheckInterval {
			t.Fatalf("try %d: RequeueAfter = %v, want the pace of a stopped holder %v", i, res.RequeueAfter,
				heldRecheckInterval)
		}
		c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionCleanupFailing)
		if c == nil || c.Reason != ReasonCleanupFailed {
			t.Fatalf("try %d: CleanupFailing = %+v, want it set while the close fails", i, c)
		}
		if !c.LastTransitionTime.Time.Equal(first.Truncate(time.Second)) {
			t.Fatalf("try %d: CleanupFailing since %v, want the first recorded failure %v", i, c.LastTransitionTime, first)
		}
		if got := deadlineCondition(rr); !strings.Contains(got, "failed and is tried again") {
			t.Fatalf("try %d: the deadline's condition says %q while the close fails", i, got)
		}
		if rr.Status.Phase != api.PhaseExpired || !holdsLock(rr) {
			t.Fatalf("try %d: phase %s, holds %t; want Expired holding", i, rr.Status.Phase, holdsLock(rr))
		}
		m.clock.advance(res.RequeueAfter)
	}
	m.clock.at = first.Add(cleanupFailingBound + time.Second)
	res, rr, err = m.reconcileExpired(t)
	if err != nil || res != (reconcile.Result{}) {
		t.Fatalf("the end of the bound: %+v, %v", res, err)
	}
	if got := deadlineCondition(rr); !strings.HasPrefix(got, ReasonPullRequestDeadlineCleanupFailed+": ") ||
		!strings.Contains(got, "close it by hand") {
		t.Fatalf("after the bound the deadline's condition says %q", got)
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionCleanupFailing); c != nil {
		t.Fatalf("CleanupFailing = %+v after the cleanup ended", c)
	}
	if !holdsLock(rr) {
		t.Fatal("the lock goes only when the read of the branch finds none open")
	}
	cr, _ := m.pr()
	if cr.State != providers.ChangeRequestOpen {
		t.Fatalf("pull request %s; the executor closed nothing", cr.State)
	}
}

// A close that failed and then works removes CleanupFailing and puts the deadline's message back.
func TestExpiredCloseFailingClearsWhenTheCloseWorks(t *testing.T) {
	m := expiredWorld(t)
	failing := true
	m.g.SetFail(func(op string) error {
		if op == opClose && failing {
			return &providers.RetryableError{Err: errors.New("status 503")}
		}
		return nil
	})
	for range 2 {
		if _, _, err := m.reconcileExpired(t); err != nil {
			t.Fatal(err)
		}
	}
	if c := meta.FindStatusCondition(m.live().Status.Conditions, api.ConditionCleanupFailing); c == nil {
		t.Fatal("CleanupFailing not set")
	}
	failing = false
	for range 3 {
		m.clock.advance(heldRecheckInterval)
		if _, _, err := m.reconcileExpired(t); err != nil {
			t.Fatal(err)
		}
	}
	rr := m.live()
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionCleanupFailing); c != nil {
		t.Fatalf("CleanupFailing = %+v after the close worked", c)
	}
	for _, c := range rr.Status.Conditions {
		if strings.Contains(c.Message, "failed and is tried again") {
			t.Fatalf("condition %s still says the close is failing: %q", c.Type, c.Message)
		}
	}
	if holdsLock(rr) {
		t.Fatal("the lock is held after the close worked and the branch was read")
	}
}

// Sixth decision of 2026-11-18 on #771, point 5: an expired request whose own pull request (its record lost) is found
// merged while the check of the plan at the merge does not answer keeps its phase and its deadline: Expired leaves
// that phase only for the record of the merge, never back to PRCreated. The merge is recorded once the check answers.
func TestExpiredOwnMergeUncheckedStaysExpired(t *testing.T) {
	m := expiredWorld(t)
	if _, err := m.g.Merge(m.number); err != nil {
		t.Fatal(err)
	}
	if _, rr, err := m.reconcileExpired(t); err != nil || rr.Status.Phase != api.PhaseExpired {
		t.Fatalf("the end: %v, phase %s", err, rr.Status.Phase)
	}
	target := m.template.Spec.TargetRef
	failing := true
	reader := m.r.APIReader.(client.WithWatch)
	m.r.APIReader = interceptor.NewClient(reader, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if failing && key.Name == target.Name && key.Namespace == target.Namespace {
				return apierrors.NewServiceUnavailable("the target cannot be read")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	deadline := m.live().Status.Deadline
	_, rr, _ := m.reconcileExpired(t)
	if rr.Status.Phase != api.PhaseExpired {
		t.Fatalf("phase %s after a check at the merge that did not answer; want Expired", rr.Status.Phase)
	}
	if rr.Status.Deadline == nil || !rr.Status.Deadline.Equal(deadline) {
		t.Fatalf("deadline %v, want %v kept", rr.Status.Deadline, deadline)
	}
	if rr.Status.PullRequest == nil || rr.Status.PullRequest.Number != m.number {
		t.Fatalf("pull request %+v, want #%d recorded", rr.Status.PullRequest, m.number)
	}
	if !expiredUncleaned(rr) {
		t.Fatalf("the read after the stop no longer sees the request expired: %+v", rr.Status.Conditions)
	}
	failing = false
	for range 3 {
		m.clock.advance(time.Second)
		if _, _, err := m.reconcileExpired(t); err != nil {
			t.Fatal(err)
		}
	}
	rr = m.live()
	if rr.Status.Phase != api.PhaseMerged || rr.Status.MergeReceipt == nil {
		t.Fatalf("phase %s, receipt %v; want Merged with its receipt once the check answers", rr.Status.Phase,
			rr.Status.MergeReceipt != nil)
	}
}

// Sixth decision of 2026-11-18 on #771, points 1 and 2: a deadline that passes while writes are paused does not end
// the request during the pause, and no call reaches the Git host; the first reconcile after the lift ends it.
func TestDeadlineWaitsForThePause(t *testing.T) {
	m := expiredWorld(t)
	m.pause(true)
	for range 2 {
		m.calls = 0
		res, rr, err := m.reconcileExpired(t)
		if err != nil || res.RequeueAfter != freshRecheckDelay {
			t.Fatalf("paused: %+v, %v; want the pause's recheck", res, err)
		}
		if rr.Status.Phase != api.PhasePolicyChecked {
			t.Fatalf("phase %s while writes are paused; want PolicyChecked", rr.Status.Phase)
		}
		if !meta.IsStatusConditionTrue(rr.Status.Conditions, api.ConditionWritesPaused) {
			t.Fatal("WritesPaused does not say why the request waits")
		}
		if m.calls > 0 {
			t.Fatalf("%d calls to the Git host while writes are paused", m.calls)
		}
		m.clock.advance(res.RequeueAfter)
	}
	m.pause(false)
	m.calls = 0
	_, rr, err := m.reconcileExpired(t)
	if err != nil || rr.Status.Phase != api.PhaseExpired {
		t.Fatalf("the first reconcile after the lift: %v, phase %s; want Expired", err, rr.Status.Phase)
	}
	if m.calls > 0 {
		t.Fatalf("the end after the lift called the Git host %d times; it reads nothing", m.calls)
	}
}
