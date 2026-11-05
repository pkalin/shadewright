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
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

var limitsNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// limitsTarget is the target UID of the request deadlineWorld makes.
const limitsTarget = "33333333-3333-4333-8333-333333333333"

const limitsDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// otherRequest is another request in the request namespace, on target uid, in phase, whose phase changed ago
// before limitsNow. A non-empty phase past Pending gets a plan digest, as one that went through the policy check.
func otherRequest(name, uid string, phase remediationv1alpha1.RemediationPhase, ago time.Duration) *remediationv1alpha1.RemediationRequest {
	rr := fx.MemoryLimitRequest(requestNamespace, name)
	rr.UID = types.UID("uid-" + name)
	rr.Spec.TargetRef.UID = remediationv1alpha1.UID(uid)
	rr.Status.Phase = phase
	if phase != "" && phase != remediationv1alpha1.PhasePending {
		rr.Status.PlanDigest = limitsDigest
	}
	at := metav1.NewMicroTime(limitsNow.Add(-ago))
	rr.Status.LastPhaseTransitionTime = &at
	return rr
}

// limitsWorld is deadlineWorld with maxConcurrentRemediations set to limit and the other requests added. The
// reconciler's resolver resolves, and counts its calls.
func limitsWorld(t *testing.T, limit int32, others ...*remediationv1alpha1.RemediationRequest) (
	*RemediationRequestReconciler, client.Client, *remediationv1alpha1.RemediationRequest, *stubResolver) {
	t.Helper()
	c, rr := deadlineWorld(t)
	ctx := context.Background()
	settings := &remediationv1alpha1.RemediationSettings{}
	if err := c.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, settings); err != nil {
		t.Fatal(err)
	}
	settings.Spec.MaxConcurrentRemediations = limit
	if err := c.Update(ctx, settings); err != nil {
		t.Fatal(err)
	}
	pol := &remediationv1alpha1.RemediationPolicy{}
	if err := c.Get(ctx, types.NamespacedName{Name: string(rr.Spec.PolicyRef)}, pol); err != nil {
		t.Fatal(err)
	}
	pol.Spec.RequestNamespaces = []remediationv1alpha1.Name{requestNamespace}
	pol.Spec.GitOps.AllowedPaths = []remediationv1alpha1.GitPathPattern{appsGlob}
	pol.Spec.Actions[0].SetContainerMemoryLimit.MaxIncreasePercent = 100
	if err := c.Update(ctx, pol); err != nil {
		t.Fatal(err)
	}
	for _, o := range others {
		status := o.Status
		if err := c.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
		o.Status = status
		if err := c.Status().Update(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	stub := &stubResolver{result: resolved()}
	r := &RemediationRequestReconciler{Client: c, APIReader: c, Scheme: c.Scheme(), Source: stub,
		Now: func() time.Time { return limitsNow }}
	return r, c, rr, stub
}

func reconcileLimits(t *testing.T, r *RemediationRequestReconciler, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, *remediationv1alpha1.RemediationRequest, error) {
	t.Helper()
	ctx := context.Background()
	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
	got := &remediationv1alpha1.RemediationRequest{}
	if gerr := r.Get(ctx, client.ObjectKeyFromObject(rr), got); gerr != nil {
		t.Fatal(gerr)
	}
	return res, got, err
}

// expectQueued checks that the request was held back with reason and did not go through the policy check, and
// that a second look at it while the limit holds checks only the limits: it does not read Git again.
func expectQueued(t *testing.T, r *RemediationRequestReconciler, rr *remediationv1alpha1.RemediationRequest,
	res reconcile.Result, err error, got *remediationv1alpha1.RemediationRequest, stub *stubResolver, reason string) {
	t.Helper()
	expectQueuedAfter(t, r, rr, res, err, got, stub, reason, admissionRecheckDelay)
}

// expectQueuedAfter is expectQueued with the delay before the next check.
func expectQueuedAfter(t *testing.T, r *RemediationRequestReconciler, rr *remediationv1alpha1.RemediationRequest,
	res reconcile.Result, err error, got *remediationv1alpha1.RemediationRequest, stub *stubResolver, reason string,
	delay time.Duration) {
	t.Helper()
	expectHeldAfter(t, res, err, got, reason, delay)
	calls := stub.calls.Load()
	res, got, err = reconcileLimits(t, r, rr)
	expectHeldAfter(t, res, err, got, reason, delay)
	if n := stub.calls.Load(); n != calls {
		t.Fatalf("a Queued request read Git: %d resolver calls, want %d", n, calls)
	}
}

func expectHeld(t *testing.T, res reconcile.Result, err error, got *remediationv1alpha1.RemediationRequest, reason string) {
	t.Helper()
	expectHeldAfter(t, res, err, got, reason, admissionRecheckDelay)
}

func expectHeldAfter(t *testing.T, res reconcile.Result, err error, got *remediationv1alpha1.RemediationRequest,
	reason string, delay time.Duration) {
	t.Helper()
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.Status.Phase != "" && got.Status.Phase != remediationv1alpha1.PhasePending {
		t.Fatalf("phase = %q, want the request held back", got.Status.Phase)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionQueued)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != reason {
		t.Fatalf("Queued = %+v, want True with reason %s", c, reason)
	}
	if res.RequeueAfter != delay {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, delay)
	}
}

func expectAdmitted(t *testing.T, err error, got *remediationv1alpha1.RemediationRequest) {
	t.Helper()
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.Status.Phase != remediationv1alpha1.PhasePolicyChecked {
		c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed)
		t.Fatalf("phase = %q (PolicyAllowed %+v), want PolicyChecked", got.Status.Phase, c)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionQueued); c != nil {
		t.Fatalf("a request let through keeps Queued: %+v", c)
	}
}

// TestLimitsLockPerTargetUID checks that a request waits while another active request names the same target
// UID, whatever the cap, and goes through once that one stops.
func TestLimitsLockPerTargetUID(t *testing.T) {
	_, rr0 := deadlineWorld(t)
	target := string(rr0.Spec.TargetRef.UID)

	for _, phase := range []remediationv1alpha1.RemediationPhase{
		remediationv1alpha1.PhasePolicyChecked, remediationv1alpha1.PhasePRCreated,
		remediationv1alpha1.PhaseAwaitingApproval, remediationv1alpha1.PhaseAutoMergeQueued,
	} {
		t.Run(string(phase), func(t *testing.T) {
			r, _, rr, stub := limitsWorld(t, 20, otherRequest("holder", target, phase, time.Minute))
			res, got, err := reconcileLimits(t, r, rr)
			expectQueued(t, r, rr, res, err, got, stub, ReasonTargetLocked)
		})
	}
	t.Run("another target", func(t *testing.T) {
		r, _, rr, _ := limitsWorld(t, 20, otherRequest("other", "44444444-4444-4444-8444-444444444444",
			remediationv1alpha1.PhasePolicyChecked, time.Minute))
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
	})
	t.Run("released", func(t *testing.T) {
		r, c, rr, _ := limitsWorld(t, 20, otherRequest("holder", target, remediationv1alpha1.PhasePRCreated, time.Minute))
		_, got, _ := reconcileLimits(t, r, rr)
		if meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionQueued) == nil {
			t.Fatal("the request was not held back while the target was locked")
		}
		holder := &remediationv1alpha1.RemediationRequest{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: requestNamespace, Name: "holder"}, holder); err != nil {
			t.Fatal(err)
		}
		holder.Status.Phase = remediationv1alpha1.PhaseMerged
		if err := c.Status().Update(context.Background(), holder); err != nil {
			t.Fatal(err)
		}
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
	})
	t.Run("pending and stopped requests hold no lock", func(t *testing.T) {
		others := make([]*remediationv1alpha1.RemediationRequest, 0, 6)
		for i, phase := range []remediationv1alpha1.RemediationPhase{"", remediationv1alpha1.PhasePending,
			remediationv1alpha1.PhaseMerged, remediationv1alpha1.PhaseDenied, remediationv1alpha1.PhaseSuperseded,
			remediationv1alpha1.PhaseCancelled} {
			others = append(others, otherRequest(fmt.Sprintf("o%d", i), target, phase, time.Minute))
		}
		r, _, rr, _ := limitsWorld(t, 1, others...)
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
	})
}

// withPullRequest records a pull request on o, with PullRequestOpened True and reason, as the pull request step
// leaves it (PullRequestOpened) or markClosed rewrites it (PullRequestClosed, ChangeRequestClosed,
// ChangeRequestMerged); an empty reason writes no PullRequestOpened at all.
func withPullRequest(o *remediationv1alpha1.RemediationRequest, reason string) *remediationv1alpha1.RemediationRequest {
	o.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: 7, URL: "https://github.com/o/r/pull/7",
		HeadSHA: "0123456789abcdef0123456789abcdef01234567"}
	if reason != "" {
		meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{Type: remediationv1alpha1.ConditionPROpened,
			Status: metav1.ConditionTrue, Reason: reason, Message: "Pull request #7"})
	}
	return o
}

// holding sets status.holdsTarget on o.
func holding(o *remediationv1alpha1.RemediationRequest, holds bool) *remediationv1alpha1.RemediationRequest {
	o.Status.HoldsTarget = new(holds)
	return o
}

// TestLimitsLockHeldWhilePullRequestMayBeOpen checks #294 and #376: status.holdsTarget decides the lock, and a
// request written before the field existed derives it from its status, keeping the lock while a recorded pull
// request is not shown closed or merged (fail closed). A request queued behind a stopped holder checks again
// after heldRecheckInterval, one behind a holder the executor moves on after admissionRecheckDelay. The longer
// lock holds no slot under maxConcurrentRemediations.
func TestLimitsLockHeldWhilePullRequestMayBeOpen(t *testing.T) {
	stopped := func(phase remediationv1alpha1.RemediationPhase, reason string) *remediationv1alpha1.RemediationRequest {
		return withPullRequest(otherRequest("stopped", limitsTarget, phase, time.Minute), reason)
	}
	held := map[string]*remediationv1alpha1.RemediationRequest{
		"derived: NeedsIntervention, head moved": stopped(remediationv1alpha1.PhaseNeedsIntervention, ReasonPullRequestOpened),
		// A look-up of checkHeld records the pull request of an Expired request whose cleanup was refused.
		"derived: Expired with a pull request found by a look-up": stopped(remediationv1alpha1.PhaseExpired, ReasonPullRequestDeadlineCleanupRefused),
		"derived: Failed":                               stopped(remediationv1alpha1.PhaseFailed, ReasonPullRequestOpened),
		"derived: unknown phase":                        stopped("SomethingNew", ReasonPullRequestOpened),
		"derived: NeedsIntervention, no PROpened":       stopped(remediationv1alpha1.PhaseNeedsIntervention, ""),
		"derived: NeedsIntervention, gone":              stopped(remediationv1alpha1.PhaseNeedsIntervention, ReasonChangeRequestGone),
		"derived: Cancelled, close not recorded":        stopped(remediationv1alpha1.PhaseCancelled, ReasonPullRequestOpened),
		"derived: no phase with a pull request somehow": stopped("", ReasonPullRequestOpened),
		"field: stopped without a recorded pull request": holding(otherRequest("stopped", limitsTarget,
			remediationv1alpha1.PhaseNeedsIntervention, time.Minute), true),
		"field: true although the status says closed": holding(stopped(remediationv1alpha1.PhaseNeedsIntervention,
			ReasonChangeRequestClosed), true),
	}
	for name, o := range held {
		t.Run("held: "+name, func(t *testing.T) {
			r, _, rr, stub := limitsWorld(t, 20, o)
			res, got, err := reconcileLimits(t, r, rr)
			expectQueuedAfter(t, r, rr, res, err, got, stub, ReasonTargetLocked, heldRecheckInterval)
		})
	}
	t.Run("held by a request the executor moves on: checked again soon", func(t *testing.T) {
		r, _, rr, stub := limitsWorld(t, 20, holding(otherRequest("moving", limitsTarget,
			remediationv1alpha1.PhaseAwaitingApproval, time.Minute), true))
		res, got, err := reconcileLimits(t, r, rr)
		expectQueued(t, r, rr, res, err, got, stub, ReasonTargetLocked)
	})
	released := map[string]*remediationv1alpha1.RemediationRequest{
		"derived: closed by Shadewright": stopped(remediationv1alpha1.PhaseNeedsIntervention, ReasonPullRequestClosed),
		"derived: closed by a person":    stopped(remediationv1alpha1.PhaseNeedsIntervention, ReasonChangeRequestClosed),
		"derived: merged after the stop": stopped(remediationv1alpha1.PhaseNeedsIntervention, ReasonChangeRequestMerged),
		"derived: Merged":                stopped(remediationv1alpha1.PhaseMerged, ReasonPullRequestOpened),
		"derived: Succeeded":             stopped(remediationv1alpha1.PhaseSucceeded, ReasonPullRequestOpened),
		"derived: Superseded":            stopped(remediationv1alpha1.PhaseSuperseded, ReasonPullRequestOpened),
		"derived: no pull request recorded": otherRequest("stopped", limitsTarget,
			remediationv1alpha1.PhaseNeedsIntervention, time.Minute),
		"field: false although a pull request is recorded open": holding(stopped(
			remediationv1alpha1.PhaseNeedsIntervention, ReasonPullRequestOpened), false),
		"another target": holding(withPullRequest(otherRequest("stopped", "44444444-4444-4444-8444-444444444444",
			remediationv1alpha1.PhaseNeedsIntervention, time.Minute), ReasonPullRequestOpened), true),
	}
	for name, o := range released {
		t.Run("released: "+name, func(t *testing.T) {
			r, _, rr, _ := limitsWorld(t, 20, o)
			_, got, err := reconcileLimits(t, r, rr)
			expectAdmitted(t, err, got)
		})
	}
	t.Run("released once holdsTarget is cleared", func(t *testing.T) {
		r, c, rr, stub := limitsWorld(t, 20, holding(stopped(remediationv1alpha1.PhaseNeedsIntervention,
			ReasonPullRequestOpened), true))
		res, got, err := reconcileLimits(t, r, rr)
		expectQueuedAfter(t, r, rr, res, err, got, stub, ReasonTargetLocked, heldRecheckInterval)
		holder := &remediationv1alpha1.RemediationRequest{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: requestNamespace, Name: "stopped"}, holder); err != nil {
			t.Fatal(err)
		}
		holder.Status.HoldsTarget = new(false)
		if err := c.Status().Update(context.Background(), holder); err != nil {
			t.Fatal(err)
		}
		_, got, err = reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
	})
	t.Run("deleting with the finalizer holds the lock, without it frees it", func(t *testing.T) {
		for _, finalizer := range []string{FinalizerName, "example.com/other"} {
			o := holding(stopped(remediationv1alpha1.PhaseNeedsIntervention, ReasonPullRequestOpened), true)
			o.Finalizers = []string{finalizer}
			r, c, rr, stub := limitsWorld(t, 20, o)
			if err := c.Delete(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			res, got, err := reconcileLimits(t, r, rr)
			if finalizer == FinalizerName {
				// A request being deleted is moved on by its finalizer: it is checked again soon.
				expectQueued(t, r, rr, res, err, got, stub, ReasonTargetLocked)
			} else {
				expectAdmitted(t, err, got)
			}
		}
	})
	t.Run("holds no slot", func(t *testing.T) {
		o := holding(withPullRequest(otherRequest("stopped", "44444444-4444-4444-8444-444444444444",
			remediationv1alpha1.PhaseNeedsIntervention, time.Minute), ReasonPullRequestOpened), true)
		r, _, rr, _ := limitsWorld(t, 1, o)
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
	})
	t.Run("a list that fails lets nothing through", func(t *testing.T) {
		r, base, rr, _ := limitsWorld(t, 20, holding(stopped(remediationv1alpha1.PhaseNeedsIntervention,
			ReasonPullRequestClosed), false))
		r.APIReader = interceptorClient(base, func() error { return errors.New("list refused") })
		_, got, err := reconcileLimits(t, r, rr)
		if err == nil || got.Status.Phase != "" {
			t.Fatalf("phase = %q, err = %v; want no decision and an error", got.Status.Phase, err)
		}
	})
}

// TestLimitsAdmissionTakesTheLock checks that the status write that lets a request through takes the lock.
func TestLimitsAdmissionTakesTheLock(t *testing.T) {
	r, _, rr, _ := limitsWorld(t, 20)
	_, got, err := reconcileLimits(t, r, rr)
	expectAdmitted(t, err, got)
	if !ptr.Deref(got.Status.HoldsTarget, false) {
		t.Fatalf("holdsTarget = %v, want true in the write of the allowed decision", got.Status.HoldsTarget)
	}
}

// TestSettleLock checks the release of the lock at a stop: only a status that proves no pull request of the
// request open releases it, and it never takes the lock.
func TestSettleLock(t *testing.T) {
	rr := func(phase remediationv1alpha1.RemediationPhase, holds *bool, prReason string, recorded bool) *remediationv1alpha1.RemediationRequest {
		o := otherRequest("o", limitsTarget, phase, time.Minute)
		o.Status.HoldsTarget = holds
		if recorded {
			withPullRequest(o, prReason)
		}
		return o
	}
	yes, no := new(true), new(false)
	for name, c := range map[string]struct {
		rr   *remediationv1alpha1.RemediationRequest
		want *bool
	}{
		"Merged releases":                   {rr(remediationv1alpha1.PhaseMerged, yes, ReasonPullRequestOpened, true), no},
		"Superseded releases":               {rr(remediationv1alpha1.PhaseSuperseded, yes, "", false), no},
		"closed without a merge releases":   {rr(remediationv1alpha1.PhaseNeedsIntervention, yes, ReasonChangeRequestClosed, true), no},
		"closed by Shadewright releases":    {rr(remediationv1alpha1.PhaseNeedsIntervention, yes, ReasonPullRequestClosed, true), no},
		"head moved keeps":                  {rr(remediationv1alpha1.PhaseNeedsIntervention, yes, ReasonPullRequestOpened, true), yes},
		"gone keeps":                        {rr(remediationv1alpha1.PhaseNeedsIntervention, yes, ReasonChangeRequestGone, true), yes},
		"none recorded keeps for a look-up": {rr(remediationv1alpha1.PhaseExpired, yes, "", false), yes},
		"still moved on keeps":              {rr(remediationv1alpha1.PhaseAwaitingApproval, yes, ReasonPullRequestClosed, true), yes},
		"released stays released":           {rr(remediationv1alpha1.PhaseNeedsIntervention, no, ReasonPullRequestOpened, true), no},
		"never taken is not taken":          {rr(remediationv1alpha1.PhaseNeedsIntervention, nil, ReasonPullRequestOpened, true), nil},
	} {
		t.Run(name, func(t *testing.T) {
			settleLock(c.rr)
			if got := c.rr.Status.HoldsTarget; (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
				t.Fatalf("holdsTarget = %v, want %v", ptr.Deref(got, false), ptr.Deref(c.want, false))
			}
		})
	}
}

// TestBackfillLock checks the upgrade: a request that went through the policy check before status.holdsTarget
// existed gets it once, from its status (fail closed), and one that never went through it gets none.
func TestBackfillLock(t *testing.T) {
	for name, c := range map[string]struct {
		rr   *remediationv1alpha1.RemediationRequest
		want *bool
	}{
		"stopped with an open recorded pull request": {withPullRequest(otherRequest("o", limitsTarget,
			remediationv1alpha1.PhaseNeedsIntervention, time.Minute), ReasonPullRequestOpened), new(true)},
		"stopped with a closed one": {withPullRequest(otherRequest("o", limitsTarget,
			remediationv1alpha1.PhaseNeedsIntervention, time.Minute), ReasonChangeRequestClosed), new(false)},
		"merged": {withPullRequest(otherRequest("o", limitsTarget, remediationv1alpha1.PhaseMerged, time.Minute),
			ReasonPullRequestOpened), new(false)},
		"moved on":       {otherRequest("o", limitsTarget, remediationv1alpha1.PhaseAwaitingApproval, time.Minute), new(true)},
		"never admitted": {otherRequest("o", limitsTarget, remediationv1alpha1.PhasePending, time.Minute), nil},
	} {
		t.Run(name, func(t *testing.T) {
			r, c2, _, _ := limitsWorld(t, 20, c.rr)
			if err := r.backfillLock(context.Background(), c.rr); err != nil {
				t.Fatal(err)
			}
			got := &remediationv1alpha1.RemediationRequest{}
			if err := c2.Get(context.Background(), client.ObjectKeyFromObject(c.rr), got); err != nil {
				t.Fatal(err)
			}
			if h := got.Status.HoldsTarget; (h == nil) != (c.want == nil) || (h != nil && *h != *c.want) {
				t.Fatalf("holdsTarget = %v (set %v), want %v", ptr.Deref(h, false), h != nil, ptr.Deref(c.want, false))
			}
		})
	}
}

// TestLockWaiters checks the wake-up: a change of a request enqueues the requests queued behind the lock on the
// same target, and no others.
func TestLockWaiters(t *testing.T) {
	queued := func(name, target, reason string) *remediationv1alpha1.RemediationRequest {
		o := otherRequest(name, target, "", time.Minute)
		meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{Type: remediationv1alpha1.ConditionQueued,
			Status: metav1.ConditionTrue, Reason: reason, Message: "m"})
		return o
	}
	holder := otherRequest("holder", limitsTarget, remediationv1alpha1.PhaseNeedsIntervention, time.Minute)
	r, _, _, _ := limitsWorld(t, 20, holder, queued("w1", limitsTarget, ReasonTargetLocked),
		queued("w2", limitsTarget, ReasonConcurrencyLimit), queued("w3", "44444444-4444-4444-8444-444444444444", ReasonTargetLocked))
	got := r.lockWaiters(context.Background(), holder)
	if len(got) != 1 || got[0].Name != "w1" {
		t.Fatalf("woken = %v, want only w1", got)
	}
	if r.lockWaiters(context.Background(), &remediationv1alpha1.RemediationSettings{}) != nil {
		t.Fatal("an object that is not a request woke someone")
	}
}

// TestLimitsMaxConcurrentRemediations checks the cap: a request waits while maxConcurrentRemediations other
// requests are active, and goes through while fewer are. A request being deleted holds no slot, and a phase
// the executor does not know holds one (fail closed).
func TestLimitsMaxConcurrentRemediations(t *testing.T) {
	a := func(name string, phase remediationv1alpha1.RemediationPhase) *remediationv1alpha1.RemediationRequest {
		return otherRequest(name, "uid-target-"+name, phase, time.Minute)
	}
	t.Run("full", func(t *testing.T) {
		r, _, rr, stub := limitsWorld(t, 2, a("a", remediationv1alpha1.PhasePolicyChecked),
			a("b", remediationv1alpha1.PhaseAwaitingApproval))
		res, got, err := reconcileLimits(t, r, rr)
		expectQueued(t, r, rr, res, err, got, stub, ReasonConcurrencyLimit)
	})
	t.Run("a slot free", func(t *testing.T) {
		r, _, rr, _ := limitsWorld(t, 2, a("a", remediationv1alpha1.PhasePolicyChecked),
			a("b", remediationv1alpha1.PhaseMerged), a("c", remediationv1alpha1.PhaseNeedsIntervention))
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
	})
	t.Run("unset limit means one", func(t *testing.T) {
		r, _, rr, stub := limitsWorld(t, 0, a("a", remediationv1alpha1.PhasePRCreated))
		res, got, err := reconcileLimits(t, r, rr)
		expectQueued(t, r, rr, res, err, got, stub, ReasonConcurrencyLimit)
	})
	t.Run("unset limit lets one through", func(t *testing.T) {
		r, _, rr, _ := limitsWorld(t, 0)
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
	})
	t.Run("requests the executor does not move on hold no slot", func(t *testing.T) {
		noPlan := a("noplan", remediationv1alpha1.PhasePolicyChecked)
		noPlan.Status.PlanDigest = ""
		r, _, rr, _ := limitsWorld(t, 1, noPlan, a("verifying", remediationv1alpha1.PhaseVerifying),
			a("delivery", remediationv1alpha1.PhaseWaitingForDelivery), a("unknown", "SomethingNew"))
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
	})
	t.Run("deleting with the finalizer holds its slot and lock", func(t *testing.T) {
		for _, phase := range []remediationv1alpha1.RemediationPhase{remediationv1alpha1.PhasePRCreated,
			remediationv1alpha1.PhaseNeedsIntervention} {
			o := otherRequest("deleting", limitsTarget, phase, time.Minute)
			o.Finalizers = []string{FinalizerName}
			r, c, rr, stub := limitsWorld(t, 20, o)
			if err := c.Delete(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			res, got, err := reconcileLimits(t, r, rr)
			expectQueued(t, r, rr, res, err, got, stub, ReasonTargetLocked)
		}
	})
	t.Run("deleting without the finalizer holds no slot", func(t *testing.T) {
		o := a("a", remediationv1alpha1.PhasePRCreated)
		o.Finalizers = []string{"example.com/other"}
		r, c, rr, _ := limitsWorld(t, 1, o)
		if err := c.Delete(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
	})
}

// TestLimitsCircuitBreaker checks the breaker: BreakerThreshold failures in a row among the outcomes of
// requests that went through the policy check open it for BreakerCooldown after the latest; a success, an
// expired cool-down or too few failures leave it closed, and outcomes that say nothing about a change are
// skipped.
func TestLimitsCircuitBreaker(t *testing.T) {
	o := func(name string, phase remediationv1alpha1.RemediationPhase, ago time.Duration) *remediationv1alpha1.RemediationRequest {
		return otherRequest(name, "uid-target-"+name, phase, ago)
	}
	failed := func() []*remediationv1alpha1.RemediationRequest {
		return []*remediationv1alpha1.RemediationRequest{
			o("f1", remediationv1alpha1.PhaseFailed, 30*time.Minute),
			o("f2", remediationv1alpha1.PhaseNeedsIntervention, 20*time.Minute),
			o("f3", remediationv1alpha1.PhaseFailed, 10*time.Minute),
		}
	}
	t.Run("open", func(t *testing.T) {
		r, _, rr, stub := limitsWorld(t, 20, failed()...)
		res, got, err := reconcileLimits(t, r, rr)
		expectQueued(t, r, rr, res, err, got, stub, ReasonCircuitOpen)
	})
	t.Run("neutral outcomes do not close it", func(t *testing.T) {
		others := append(failed(), o("d", remediationv1alpha1.PhaseDenied, time.Minute),
			o("s", remediationv1alpha1.PhaseSuperseded, 2*time.Minute),
			o("c", remediationv1alpha1.PhaseCancelled, 3*time.Minute))
		r, _, rr, stub := limitsWorld(t, 20, others...)
		res, got, err := reconcileLimits(t, r, rr)
		expectQueued(t, r, rr, res, err, got, stub, ReasonCircuitOpen)
	})
	t.Run("a success closes it", func(t *testing.T) {
		r, _, rr, _ := limitsWorld(t, 20, append(failed(), o("ok", remediationv1alpha1.PhaseMerged, time.Minute))...)
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
	})
	t.Run("a success before the failures does not", func(t *testing.T) {
		r, _, rr, stub := limitsWorld(t, 20, append(failed(), o("ok", remediationv1alpha1.PhaseMerged, time.Hour))...)
		res, got, err := reconcileLimits(t, r, rr)
		expectQueued(t, r, rr, res, err, got, stub, ReasonCircuitOpen)
	})
	t.Run("too few failures", func(t *testing.T) {
		r, _, rr, _ := limitsWorld(t, 20, failed()[1:]...)
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
	})
	t.Run("cool-down over", func(t *testing.T) {
		r, _, rr, _ := limitsWorld(t, 20, failed()...)
		r.BreakerCooldown = 10 * time.Minute
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
	})
	t.Run("one failure after the cool-down opens it again", func(t *testing.T) {
		others := append(failed(), o("f4", remediationv1alpha1.PhaseNeedsIntervention, time.Minute))
		r, _, rr, stub := limitsWorld(t, 20, others...)
		r.BreakerCooldown = 10 * time.Minute
		res, got, err := reconcileLimits(t, r, rr)
		expectQueued(t, r, rr, res, err, got, stub, ReasonCircuitOpen)
	})
	t.Run("expired is not a failure", func(t *testing.T) {
		r, _, rr, _ := limitsWorld(t, 20, o("e1", remediationv1alpha1.PhaseExpired, 3*time.Minute),
			o("e2", remediationv1alpha1.PhaseExpired, 2*time.Minute), o("e3", remediationv1alpha1.PhaseExpired, time.Minute))
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
	})
	t.Run("ordered below the second", func(t *testing.T) {
		// Three failures and then a success, all within one second: the success is the latest outcome, so the
		// breaker is closed. With times kept to the second they would tie, and the failures could come last.
		at := func(x *remediationv1alpha1.RemediationRequest, ms int) *remediationv1alpha1.RemediationRequest {
			mt := metav1.NewMicroTime(limitsNow.Add(-time.Second).Truncate(time.Second).Add(time.Duration(ms) * time.Millisecond))
			x.Status.LastPhaseTransitionTime = &mt
			return x
		}
		r, _, rr, _ := limitsWorld(t, 20, at(o("a1", remediationv1alpha1.PhaseFailed, 0), 100),
			at(o("a2", remediationv1alpha1.PhaseFailed, 0), 200), at(o("a3", remediationv1alpha1.PhaseFailed, 0), 300),
			at(o("a0", remediationv1alpha1.PhaseMerged, 0), 900))
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
	})
	t.Run("threshold", func(t *testing.T) {
		r, _, rr, stub := limitsWorld(t, 20, failed()[2:]...)
		r.BreakerThreshold = 1
		res, got, err := reconcileLimits(t, r, rr)
		expectQueued(t, r, rr, res, err, got, stub, ReasonCircuitOpen)
	})
	t.Run("requests stopped before the policy check do not count", func(t *testing.T) {
		others := failed()
		for _, x := range others {
			x.Status.PlanDigest = ""
		}
		r, _, rr, _ := limitsWorld(t, 20, others...)
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
	})
}

// TestLimitsCheckedAgainAfterPolicyCheck checks that the limits are read again right before the decision is
// written: another request that went through while this one's policy check ran holds the target.
func TestLimitsCheckedAgainAfterPolicyCheck(t *testing.T) {
	r, c, rr, stub := limitsWorld(t, 20)
	other := otherRequest("racer", string(rr.Spec.TargetRef.UID), "", 0)
	if err := c.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	stub.during = func() {
		other.Status.Phase = remediationv1alpha1.PhasePolicyChecked
		other.Status.PlanDigest = limitsDigest
		if err := c.Status().Update(context.Background(), other); err != nil {
			t.Error(err)
		}
	}
	res, got, err := reconcileLimits(t, r, rr)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.Status.Phase == remediationv1alpha1.PhasePolicyChecked {
		t.Fatal("the request went through although another one took its target during the policy check")
	}
	c2 := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionQueued)
	if c2 == nil || c2.Reason != ReasonTargetLocked || res.RequeueAfter != admissionRecheckDelay {
		t.Fatalf("Queued = %+v, RequeueAfter = %v, want TargetLocked", c2, res.RequeueAfter)
	}
}

// TestLimitsFailClosed checks that a request is not let through when the requests cannot be listed, whether
// it is checked for the first time or is Queued, and that a Queued request then does not read Git.
func TestLimitsFailClosed(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprintf("queued %v", queued), func(t *testing.T) {
			r, base, rr, stub := limitsWorld(t, 20)
			wantCalls := int32(1)
			if queued {
				markQueued(t, base, rr)
				wantCalls = 0
			}
			r.APIReader = interceptorClient(base, func() error { return errors.New("list refused") })
			before := testutil.ToFloat64(remediationWaits.WithLabelValues("limits_unknown"))
			_, got, err := reconcileLimits(t, r, rr)
			if err == nil {
				t.Fatal("Reconcile returned no error although the requests could not be listed")
			}
			if got.Status.Phase != "" {
				t.Fatalf("phase = %q, want none", got.Status.Phase)
			}
			if stub.calls.Load() != wantCalls {
				t.Fatalf("resolver calls = %d, want %d", stub.calls.Load(), wantCalls)
			}
			if got := testutil.ToFloat64(remediationWaits.WithLabelValues("limits_unknown")) - before; got != 1 {
				t.Fatalf("limits_unknown waits rose by %v, want 1", got)
			}
		})
	}
}

// TestLimitsNeedTheAPIReader checks that the limits never fall back to the client (and its cache): with no
// APIReader no request is let through, even when the client would show free slots.
func TestLimitsNeedTheAPIReader(t *testing.T) {
	r, _, rr, _ := limitsWorld(t, 20)
	r.APIReader = nil
	_, got, err := reconcileLimits(t, r, rr)
	if err == nil || got.Status.Phase == remediationv1alpha1.PhasePolicyChecked {
		t.Fatalf("phase = %q, err = %v; want no decision and an error", got.Status.Phase, err)
	}
}

// TestLimitsDoNotHoldRefusals checks that a limit holds back only a request the policy check lets through: a
// refusal is written at once even while the target is locked, the cap is reached and the breaker is open.
func TestLimitsDoNotHoldRefusals(t *testing.T) {
	blocking := func() []*remediationv1alpha1.RemediationRequest {
		return []*remediationv1alpha1.RemediationRequest{
			otherRequest("holder", limitsTarget, remediationv1alpha1.PhasePRCreated, time.Minute),
			otherRequest("f1", "uid-f1", remediationv1alpha1.PhaseFailed, 3*time.Minute),
			otherRequest("f2", "uid-f2", remediationv1alpha1.PhaseFailed, 2*time.Minute),
			otherRequest("f3", "uid-f3", remediationv1alpha1.PhaseFailed, time.Minute),
		}
	}
	cases := map[string]struct {
		mutate func(t *testing.T, c client.Client, rr *remediationv1alpha1.RemediationRequest, stub *stubResolver)
		phase  remediationv1alpha1.RemediationPhase
	}{
		"policy missing": {func(t *testing.T, c client.Client, rr *remediationv1alpha1.RemediationRequest, _ *stubResolver) {
			if err := c.Delete(context.Background(), &remediationv1alpha1.RemediationPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: string(rr.Spec.PolicyRef)}}); err != nil {
				t.Fatal(err)
			}
		}, remediationv1alpha1.PhaseDenied},
		"source mapping refuses": {func(_ *testing.T, _ client.Client, _ *remediationv1alpha1.RemediationRequest, stub *stubResolver) {
			stub.result = sourcemap.Result{Outcome: sourcemap.NeedsIntervention, Reason: sourcemap.ReasonFieldNotFound}
		}, remediationv1alpha1.PhaseNeedsIntervention},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, c, rr, stub := limitsWorld(t, 1, blocking()...)
			tc.mutate(t, c, rr, stub)
			_, got, err := reconcileLimits(t, r, rr)
			if err != nil || got.Status.Phase != tc.phase {
				t.Fatalf("phase = %q, err = %v, want %s at once", got.Status.Phase, err, tc.phase)
			}
			if c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionQueued); c != nil {
				t.Fatalf("a refused request is Queued: %+v", c)
			}
		})
	}
}

// TestLimitsClearQueuedWhenTheyPass checks that Queued goes as soon as the limits let a Queued request
// through, also when the policy check after it fails for a while.
func TestLimitsClearQueuedWhenTheyPass(t *testing.T) {
	r, c, rr, stub := limitsWorld(t, 20)
	markQueued(t, c, rr)
	stub.err = errors.New("the Git host is down")
	_, got, err := reconcileLimits(t, r, rr)
	if err == nil {
		t.Fatal("Reconcile returned no error although the policy check failed")
	}
	if stub.calls.Load() != 1 {
		t.Fatalf("resolver calls = %d, want the policy check to run once the limits pass", stub.calls.Load())
	}
	if q := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionQueued); q != nil {
		t.Fatalf("Queued was left behind after the limits passed: %+v", q)
	}
}

// TestLimitsMissingSettingsStillDenied checks that missing settings do not hold a request back, new or Queued:
// the policy check denies it.
func TestLimitsMissingSettingsStillDenied(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprintf("queued %v", queued), func(t *testing.T) {
			r, c, rr, _ := limitsWorld(t, 1)
			if queued {
				markQueued(t, c, rr)
			}
			if err := c.Delete(context.Background(), &remediationv1alpha1.RemediationSettings{
				ObjectMeta: metav1.ObjectMeta{Name: remediationv1alpha1.SettingsName}}); err != nil {
				t.Fatal(err)
			}
			_, got, err := reconcileLimits(t, r, rr)
			if err != nil || got.Status.Phase != remediationv1alpha1.PhaseDenied {
				t.Fatalf("phase = %q, err = %v, want Denied", got.Status.Phase, err)
			}
		})
	}
}

// markQueued writes Queued True on rr, as a limit that held it back would have.
func markQueued(t *testing.T, c client.Client, rr *remediationv1alpha1.RemediationRequest) {
	t.Helper()
	ctx := context.Background()
	if err := c.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: remediationv1alpha1.ConditionQueued,
		Status: metav1.ConditionTrue, Reason: ReasonConcurrencyLimit, Message: "test"})
	if err := c.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
}

// TestLimitsMetrics checks the metrics of a request held back by the cap, and of the breaker: a trip is
// counted when it is found open after being found closed, not on every check while it stays open.
func TestLimitsMetrics(t *testing.T) {
	r, _, rr, _ := limitsWorld(t, 1, otherRequest("a", "uid-a", remediationv1alpha1.PhasePRCreated, time.Minute))
	waits := testutil.ToFloat64(remediationWaits.WithLabelValues("concurrency_limit"))
	if _, _, err := reconcileLimits(t, r, rr); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(remediationWaits.WithLabelValues("concurrency_limit")) - waits; got != 1 {
		t.Fatalf("concurrency_limit waits rose by %v, want 1", got)
	}
	if testutil.ToFloat64(remediationsInFlight) != 1 || testutil.ToFloat64(concurrencyLimit) != 1 ||
		testutil.ToFloat64(breakerOpen) != 0 {
		t.Fatalf("in flight %v, limit %v, breaker open %v; want 1, 1, 0", testutil.ToFloat64(remediationsInFlight),
			testutil.ToFloat64(concurrencyLimit), testutil.ToFloat64(breakerOpen))
	}

	trips := testutil.ToFloat64(breakerTrips)
	open := limitsState{circuit: circuitOpen, reason: ReasonCircuitOpen, limit: 1}
	halfOpen := limitsState{circuit: circuitHalfOpen, probing: true, reason: ReasonCircuitHalfOpen, limit: 1}
	recordLimits(open, nil)
	recordLimits(open, nil)
	if got := testutil.ToFloat64(breakerTrips) - trips; got != 1 || testutil.ToFloat64(breakerOpen) != 1 {
		t.Fatalf("trips rose by %v (open %v), want 1 trip while it stays open", got, testutil.ToFloat64(breakerOpen))
	}
	recordLimits(limitsState{limit: 1}, nil)
	recordLimits(open, nil)
	if got := testutil.ToFloat64(breakerTrips) - trips; got != 2 {
		t.Fatalf("trips rose by %v, want 2 after it closed and opened again", got)
	}
	// Half-open is reported as open (1) and is no trip; a failed probe that opens it again is one.
	recordLimits(halfOpen, nil)
	if got := testutil.ToFloat64(breakerTrips) - trips; got != 2 || testutil.ToFloat64(breakerOpen) != 1 {
		t.Fatalf("trips rose by %v (open %v) when half-open, want 2 and 1", got, testutil.ToFloat64(breakerOpen))
	}
	recordLimits(open, nil)
	if got := testutil.ToFloat64(breakerTrips) - trips; got != 3 {
		t.Fatalf("trips rose by %v, want 3 after a failed probe opened it again", got)
	}
	recordLimits(limitsState{limit: 1}, nil)
	if testutil.ToFloat64(breakerOpen) != 0 {
		t.Fatal("breaker open gauge is not 0 once closed")
	}
}

// TestLimitsMetricLabelsAreClosed checks that every reason of the limits has a label value, and that the
// values are the documented closed set.
func TestLimitsMetricLabelsAreClosed(t *testing.T) {
	want := map[string]bool{"target_locked": true, "concurrency_limit": true, "circuit_open": true,
		"circuit_half_open": true, "limits_unknown": true}
	for _, reason := range []string{ReasonTargetLocked, ReasonConcurrencyLimit, ReasonCircuitOpen,
		ReasonCircuitHalfOpen, ReasonLimitsUnknown} {
		if !want[waitReasons[reason]] {
			t.Fatalf("reason %s has label value %q, not in the closed set", reason, waitReasons[reason])
		}
	}
	if len(waitReasons) != len(want) {
		t.Fatalf("waitReasons has %d values, want %d", len(waitReasons), len(want))
	}
}

// TestPhaseTransitionTimeRecorded checks that a phase write records when the phase changed, and that writing
// the same phase again keeps the time.
func TestPhaseTransitionTimeRecorded(t *testing.T) {
	r, _, rr, _ := limitsWorld(t, 1)
	_, got, err := reconcileLimits(t, r, rr)
	expectAdmitted(t, err, got)
	if got.Status.LastPhaseTransitionTime == nil || !got.Status.LastPhaseTransitionTime.Time.Equal(limitsNow) {
		t.Fatalf("lastPhaseTransitionTime = %v, want %v", got.Status.LastPhaseTransitionTime, limitsNow)
	}
	later := limitsNow.Add(time.Hour)
	r.Now = func() time.Time { return later }
	r.setPhase(got, remediationv1alpha1.PhasePolicyChecked)
	if !got.Status.LastPhaseTransitionTime.Time.Equal(limitsNow) {
		t.Fatal("setting the same phase again moved lastPhaseTransitionTime")
	}
	r.setPhase(got, remediationv1alpha1.PhaseExpired)
	if !got.Status.LastPhaseTransitionTime.Time.Equal(later) {
		t.Fatal("a new phase did not move lastPhaseTransitionTime")
	}
}

// interceptorClient wraps base so that each List first calls fail, and fails with its error.
func interceptorClient(base client.Client, fail func() error) client.Reader {
	return interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := fail(); err != nil {
				return err
			}
			return c.List(ctx, list, opts...)
		},
	})
}
