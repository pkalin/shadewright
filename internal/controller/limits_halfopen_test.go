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
	"fmt"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// halfOpenCooldown is the cool-down of the breaker in halfOpenWorld: the latest of its failures is 10 minutes
// before limitsNow, so at limitsNow the cool-down is over and the breaker is half-open.
const halfOpenCooldown = 5 * time.Minute

// halfOpenWorld is limitsWorld with the breaker tripped by three failures in a row and its cool-down over at
// limitsNow, plus others. Besides rr it holds rr2, a second Pending request on another Deployment, so neither
// the lock nor the cap (20) holds one back behind the other.
func halfOpenWorld(t *testing.T, others ...*remediationv1alpha1.RemediationRequest) (
	*RemediationRequestReconciler, client.Client, *remediationv1alpha1.RemediationRequest,
	*remediationv1alpha1.RemediationRequest) {
	t.Helper()
	fail := func(name string, phase remediationv1alpha1.RemediationPhase, ago time.Duration) *remediationv1alpha1.RemediationRequest {
		return otherRequest(name, "uid-target-"+name, phase, ago)
	}
	others = append([]*remediationv1alpha1.RemediationRequest{
		fail("f1", remediationv1alpha1.PhaseFailed, 30*time.Minute),
		fail("f2", remediationv1alpha1.PhaseNeedsIntervention, 20*time.Minute),
		fail("f3", remediationv1alpha1.PhaseFailed, 10*time.Minute),
	}, others...)
	r, c, rr, _ := limitsWorld(t, 20, others...)
	r.BreakerCooldown = halfOpenCooldown
	ctx := context.Background()
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: fx.TargetNamespace, Name: appName}, dep); err != nil {
		t.Fatal(err)
	}
	dep2 := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: dep.Namespace, Name: appName + "-2",
		UID: "55555555-5555-4555-8555-555555555555"}, Spec: *dep.Spec.DeepCopy()}
	if err := c.Create(ctx, dep2); err != nil {
		t.Fatal(err)
	}
	rr2 := fx.MemoryLimitRequest(requestNamespace, "rr2")
	rr2.UID = "uid-rr2"
	rr2.Spec.PolicyRef = rr.Spec.PolicyRef
	rr2.Spec.TargetRef.Name = dep2.Name
	rr2.Spec.TargetRef.UID = remediationv1alpha1.UID(dep2.UID)
	if err := c.Create(ctx, rr2); err != nil {
		t.Fatal(err)
	}
	return r, c, rr, rr2
}

// endProbe moves the probe to phase at t, as the executor would.
func endProbe(t *testing.T, c client.Client, probe *remediationv1alpha1.RemediationRequest,
	phase remediationv1alpha1.RemediationPhase, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := c.Get(ctx, client.ObjectKeyFromObject(probe), probe); err != nil {
		t.Fatal(err)
	}
	probe.Status.Phase = phase
	mt := metav1.NewMicroTime(at)
	probe.Status.LastPhaseTransitionTime = &mt
	if err := c.Status().Update(ctx, probe); err != nil {
		t.Fatal(err)
	}
}

// expectProbe checks that got is marked as the breaker's probe, or not.
func expectProbe(t *testing.T, got *remediationv1alpha1.RemediationRequest, want bool) {
	t.Helper()
	if got.Status.BreakerProbe != want {
		t.Fatalf("%s: breakerProbe = %v, want %v", got.Name, got.Status.BreakerProbe, want)
	}
}

// TestBreakerHalfOpenLetsOneProbeThrough checks that after the cool-down one request goes through as the probe,
// marked so, and the next waits (CircuitHalfOpen) while it runs, also for a new reconciler on the same objects
// (a restart or a new leader). A request let through before the trip that still runs is not a probe, and a
// request let through while the breaker is closed is not marked.
func TestBreakerHalfOpenLetsOneProbeThrough(t *testing.T) {
	old := otherRequest("old", "uid-target-old", remediationv1alpha1.PhasePRCreated, 40*time.Minute)
	r, c, rr, rr2 := halfOpenWorld(t, old)
	_, got, err := reconcileLimits(t, r, rr)
	expectAdmitted(t, err, got)
	expectProbe(t, got, true)

	res, got2, err := reconcileLimits(t, r, rr2)
	expectHeld(t, res, err, got2, ReasonCircuitHalfOpen)
	expectProbe(t, got2, false)

	later := limitsNow.Add(time.Hour)
	restarted := &RemediationRequestReconciler{Client: c, APIReader: &apiServerReader{Reader: c}, Scheme: c.Scheme(), Source: r.Source,
		BreakerCooldown: halfOpenCooldown, Now: func() time.Time { return later }}
	for _, phase := range []remediationv1alpha1.RemediationPhase{remediationv1alpha1.PhasePRCreated,
		remediationv1alpha1.PhaseAwaitingApproval, remediationv1alpha1.PhaseAutoMergeQueued} {
		endProbe(t, c, rr, phase, later.Add(-time.Minute))
		res, got2, err = reconcileLimits(t, restarted, rr2)
		expectHeld(t, res, err, got2, ReasonCircuitHalfOpen)
	}

	// Closed: a request let through is not a probe.
	r2, _, rr3, _ := limitsWorld(t, 20)
	_, got3, err := reconcileLimits(t, r2, rr3)
	expectAdmitted(t, err, got3)
	expectProbe(t, got3, false)
}

// TestBreakerProbeOutcome checks what ends the half-open state: the probe's success (Merged or a later phase)
// closes the breaker, and anything else (a failure, or a final phase that is neither, such as Expired or
// Denied) opens it again for a full cool-down from the end of the probe, after which the next request goes
// through as the new probe.
func TestBreakerProbeOutcome(t *testing.T) {
	cases := map[remediationv1alpha1.RemediationPhase]bool{ // phase of the probe: whether it closes the breaker
		remediationv1alpha1.PhaseMerged:             true,
		remediationv1alpha1.PhaseWaitingForDelivery: true,
		remediationv1alpha1.PhaseVerifying:          true,
		remediationv1alpha1.PhaseSucceeded:          true,
		remediationv1alpha1.PhaseFailed:             false,
		remediationv1alpha1.PhaseNeedsIntervention:  false,
		remediationv1alpha1.PhaseExpired:            false,
		remediationv1alpha1.PhaseCancelled:          false,
		remediationv1alpha1.PhaseSuperseded:         false,
		remediationv1alpha1.PhaseDenied:             false,
		remediationv1alpha1.PhaseInconclusive:       false,
	}
	for phase, closes := range cases {
		t.Run(string(phase), func(t *testing.T) {
			r, c, rr, rr2 := halfOpenWorld(t)
			_, got, err := reconcileLimits(t, r, rr)
			expectAdmitted(t, err, got)
			res, got2, err := reconcileLimits(t, r, rr2)
			expectHeld(t, res, err, got2, ReasonCircuitHalfOpen)

			ended := limitsNow.Add(2 * time.Minute)
			endProbe(t, c, rr, phase, ended)
			expectProbeEnd(t, r, rr2, ended, closes)
		})
	}
}

// expectProbeEnd checks the breaker after a probe ended at ended: closed at once when it closes, else open for
// a full cool-down from ended and half-open after it, when rr2 goes through as the new probe.
func expectProbeEnd(t *testing.T, r *RemediationRequestReconciler, rr2 *remediationv1alpha1.RemediationRequest,
	ended time.Time, closes bool) {
	t.Helper()
	if closes {
		r.Now = func() time.Time { return ended }
		_, got2, err := reconcileLimits(t, r, rr2)
		expectAdmitted(t, err, got2)
		expectProbe(t, got2, false)
		return
	}
	r.Now = func() time.Time { return ended.Add(halfOpenCooldown - time.Millisecond) }
	res, got2, err := reconcileLimits(t, r, rr2)
	expectHeld(t, res, err, got2, ReasonCircuitOpen)
	r.Now = func() time.Time { return ended.Add(halfOpenCooldown) }
	_, got2, err = reconcileLimits(t, r, rr2)
	expectAdmitted(t, err, got2)
	expectProbe(t, got2, true)
}

// deleteProbe gives the probe the executor's finalizer and deletes it, so it stays listed while it is being
// deleted.
func deleteProbe(t *testing.T, c client.Client, probe *remediationv1alpha1.RemediationRequest) {
	t.Helper()
	ctx := context.Background()
	if err := c.Get(ctx, client.ObjectKeyFromObject(probe), probe); err != nil {
		t.Fatal(err)
	}
	controllerutil.AddFinalizer(probe, FinalizerName)
	if err := c.Update(ctx, probe); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, probe); err != nil {
		t.Fatal(err)
	}
}

// TestBreakerDeletedProbe checks a probe being deleted: one that still runs holds the others back, and one that
// ended without a success opens the breaker again from its end, as if it were not being deleted.
func TestBreakerDeletedProbe(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		r, c, rr, rr2 := halfOpenWorld(t)
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
		deleteProbe(t, c, rr)
		r.Now = func() time.Time { return limitsNow.Add(time.Hour) }
		res, got2, err := reconcileLimits(t, r, rr2)
		expectHeld(t, res, err, got2, ReasonCircuitHalfOpen)
	})
	t.Run("ended", func(t *testing.T) {
		r, c, rr, rr2 := halfOpenWorld(t)
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
		ended := limitsNow.Add(2 * time.Minute)
		endProbe(t, c, rr, remediationv1alpha1.PhaseExpired, ended)
		deleteProbe(t, c, rr)
		expectProbeEnd(t, r, rr2, ended, false)
	})
}

// TestBreakerProbeEndedWithoutTransitionTime checks that a probe that ended without lastPhaseTransitionTime
// (written by hand) is taken to have ended one cool-down after the trip: the breaker is open for one more
// cool-down, and half-open after it, not open for ever.
func TestBreakerProbeEndedWithoutTransitionTime(t *testing.T) {
	r, c, rr, rr2 := halfOpenWorld(t)
	_, got, err := reconcileLimits(t, r, rr)
	expectAdmitted(t, err, got)
	got.Status.Phase = remediationv1alpha1.PhaseExpired
	got.Status.LastPhaseTransitionTime = nil
	if err := c.Status().Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	// The trip is the third failure, 10 minutes before limitsNow: the probe counts as ended 5 minutes before
	// limitsNow, so the breaker is open until limitsNow.
	r.Now = func() time.Time { return limitsNow.Add(-time.Millisecond) }
	res, got2, err := reconcileLimits(t, r, rr2)
	expectHeld(t, res, err, got2, ReasonCircuitOpen)
	r.Now = func() time.Time { return limitsNow.Add(time.Hour) }
	_, got2, err = reconcileLimits(t, r, rr2)
	expectAdmitted(t, err, got2)
	expectProbe(t, got2, true)
}

// TestBreakerSkewedLeadersAdmitOneProbe checks that a leader whose clock differs from the one that let the
// probe through, behind or ahead, still finds the probe and holds the next request back: the probe is found by
// status.breakerProbe, not by comparing the times the two clocks wrote.
func TestBreakerSkewedLeadersAdmitOneProbe(t *testing.T) {
	for name, skew := range map[string]time.Duration{"behind": -4 * time.Minute, "ahead": 3 * time.Hour} {
		t.Run(name, func(t *testing.T) {
			leaderA, c, rr, rr2 := halfOpenWorld(t)
			leaderA.Now = func() time.Time { return limitsNow.Add(time.Minute) }
			_, got, err := reconcileLimits(t, leaderA, rr)
			expectAdmitted(t, err, got)
			leaderB := &RemediationRequestReconciler{Client: c, APIReader: &apiServerReader{Reader: c}, Scheme: c.Scheme(),
				Source: leaderA.Source, BreakerCooldown: halfOpenCooldown,
				Now: func() time.Time { return limitsNow.Add(skew) }}
			res, got2, err := reconcileLimits(t, leaderB, rr2)
			expectHeld(t, res, err, got2, ReasonCircuitHalfOpen)
		})
	}
}

// TestBreakerConcurrentReconcilesAdmitOneProbe checks that two workers deciding two requests at once in the
// half-open state let exactly one through.
func TestBreakerConcurrentReconcilesAdmitOneProbe(t *testing.T) {
	for i := range 10 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			r, c, rr, rr2 := halfOpenWorld(t)
			var wg sync.WaitGroup
			errs := make([]error, 2)
			for j, x := range []*remediationv1alpha1.RemediationRequest{rr, rr2} {
				wg.Go(func() {
					_, errs[j] = r.Reconcile(context.Background(),
						reconcile.Request{NamespacedName: client.ObjectKeyFromObject(x)})
				})
			}
			wg.Wait()
			admitted := 0
			for j, x := range []*remediationv1alpha1.RemediationRequest{rr, rr2} {
				if errs[j] != nil {
					t.Fatalf("Reconcile %s: %v", x.Name, errs[j])
				}
				got := &remediationv1alpha1.RemediationRequest{}
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(x), got); err != nil {
					t.Fatal(err)
				}
				if got.Status.Phase == remediationv1alpha1.PhasePolicyChecked {
					admitted++
				}
			}
			if admitted != 1 {
				t.Fatalf("%d requests went through in the half-open state, want 1", admitted)
			}
		})
	}
}
