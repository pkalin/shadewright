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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// halfOpenCooldown is the cool-down of the breaker in halfOpenWorld: the latest of its failures is 10 minutes
// before limitsNow, so at limitsNow the cool-down is over and the breaker is half-open.
const halfOpenCooldown = 5 * time.Minute

// admittedAt gives rr the PolicyAllowed condition the policy check writes when it lets a request through at t.
func admittedAt(rr *remediationv1alpha1.RemediationRequest, t time.Time) *remediationv1alpha1.RemediationRequest {
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: remediationv1alpha1.ConditionPolicyAllowed,
		Status: metav1.ConditionTrue, Reason: "Allowed", Message: "test", LastTransitionTime: metav1.NewTime(t)})
	return rr
}

// halfOpenWorld is limitsWorld with the breaker tripped by three failures in a row and its cool-down over at
// limitsNow, plus others. Besides rr it holds rr2, a second Pending request on another Deployment, so neither
// the lock nor the cap (20) holds one back behind the other.
func halfOpenWorld(t *testing.T, others ...*remediationv1alpha1.RemediationRequest) (
	*RemediationRequestReconciler, client.Client, *remediationv1alpha1.RemediationRequest,
	*remediationv1alpha1.RemediationRequest) {
	t.Helper()
	fail := func(name string, phase remediationv1alpha1.RemediationPhase, ago time.Duration) *remediationv1alpha1.RemediationRequest {
		return admittedAt(otherRequest(name, "uid-target-"+name, phase, ago), limitsNow.Add(-ago-time.Minute))
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

// TestBreakerHalfOpenLetsOneProbeThrough checks that after the cool-down one request goes through as the probe
// and the next waits (CircuitHalfOpen) while it runs, also for a new reconciler on the same objects (a restart
// or a new leader). A request let through before the trip that still runs is not a probe.
func TestBreakerHalfOpenLetsOneProbeThrough(t *testing.T) {
	old := admittedAt(otherRequest("old", "uid-target-old", remediationv1alpha1.PhasePRCreated, 40*time.Minute),
		limitsNow.Add(-40*time.Minute))
	r, c, rr, rr2 := halfOpenWorld(t, old)
	_, got, err := reconcileLimits(t, r, rr)
	expectAdmitted(t, err, got)

	res, got2, err := reconcileLimits(t, r, rr2)
	expectHeld(t, res, err, got2, ReasonCircuitHalfOpen)

	later := limitsNow.Add(time.Hour)
	restarted := &RemediationRequestReconciler{Client: c, APIReader: c, Scheme: c.Scheme(), Source: r.Source,
		BreakerCooldown: halfOpenCooldown, Now: func() time.Time { return later }}
	for _, phase := range []remediationv1alpha1.RemediationPhase{remediationv1alpha1.PhasePRCreated,
		remediationv1alpha1.PhaseAwaitingApproval, remediationv1alpha1.PhaseWaitingForDelivery} {
		endProbe(t, c, rr, phase, later.Add(-time.Minute))
		res, got2, err = reconcileLimits(t, restarted, rr2)
		expectHeld(t, res, err, got2, ReasonCircuitHalfOpen)
	}
}

// TestBreakerProbeOutcome checks what ends the half-open state: the probe's success closes the breaker, and
// anything else (a failure, or a final phase that is neither, such as Expired) opens it again for a full
// cool-down from the end of the probe, after which the next request goes through as the new probe.
func TestBreakerProbeOutcome(t *testing.T) {
	cases := map[remediationv1alpha1.RemediationPhase]bool{ // phase of the probe: whether it closes the breaker
		remediationv1alpha1.PhaseMerged:            true,
		remediationv1alpha1.PhaseSucceeded:         true,
		remediationv1alpha1.PhaseFailed:            false,
		remediationv1alpha1.PhaseNeedsIntervention: false,
		remediationv1alpha1.PhaseExpired:           false,
		remediationv1alpha1.PhaseCancelled:         false,
		remediationv1alpha1.PhaseSuperseded:        false,
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
			if closes {
				r.Now = func() time.Time { return ended }
				_, got2, err = reconcileLimits(t, r, rr2)
				expectAdmitted(t, err, got2)
				return
			}
			r.Now = func() time.Time { return ended.Add(halfOpenCooldown - time.Millisecond) }
			res, got2, err = reconcileLimits(t, r, rr2)
			expectHeld(t, res, err, got2, ReasonCircuitOpen)
			r.Now = func() time.Time { return ended.Add(halfOpenCooldown) }
			_, got2, err = reconcileLimits(t, r, rr2)
			expectAdmitted(t, err, got2)
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

// TestBreakerProbeIdentification checks which requests count as probes: those let through at or after the trip
// by their PolicyAllowed time, kept to the second, so one in the same second counts, and one without the
// condition counts (fail closed).
func TestBreakerProbeIdentification(t *testing.T) {
	tripped := limitsNow.Add(500 * time.Millisecond)
	rr := func() *remediationv1alpha1.RemediationRequest {
		return otherRequest("p", "uid-p", remediationv1alpha1.PhasePRCreated, 0)
	}
	cases := []struct {
		name string
		rr   *remediationv1alpha1.RemediationRequest
		want bool
	}{
		{"after", admittedAt(rr(), limitsNow.Add(time.Minute)), true},
		{"same second", admittedAt(rr(), limitsNow), true},
		{"before", admittedAt(rr(), limitsNow.Add(-time.Second)), false},
		{"no condition", rr(), true},
	}
	for _, tc := range cases {
		if got := probe(tc.rr, tripped); got != tc.want {
			t.Errorf("%s: probe = %v, want %v", tc.name, got, tc.want)
		}
	}
}
