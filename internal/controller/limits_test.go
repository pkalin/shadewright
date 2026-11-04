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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

var limitsNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

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
	rr.Status.LastPhaseTransitionTime = &metav1.Time{Time: limitsNow.Add(-ago)}
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
	r := &RemediationRequestReconciler{Client: c, Scheme: c.Scheme(), Source: stub, Now: func() time.Time { return limitsNow }}
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

// expectQueued checks that the request was held back with reason, did not go through the policy check and
// did not read Git.
func expectQueued(t *testing.T, res reconcile.Result, err error, got *remediationv1alpha1.RemediationRequest,
	stub *stubResolver, reason string) {
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
	if res.RequeueAfter != admissionRecheckDelay {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, admissionRecheckDelay)
	}
	if n := stub.calls.Load(); n != 0 {
		t.Fatalf("the source resolver was called %d times while the request was held back", n)
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
		remediationv1alpha1.PhaseAwaitingApproval, remediationv1alpha1.PhaseVerifying,
	} {
		t.Run(string(phase), func(t *testing.T) {
			r, _, rr, stub := limitsWorld(t, 20, otherRequest("holder", target, phase, time.Minute))
			res, got, err := reconcileLimits(t, r, rr)
			expectQueued(t, res, err, got, stub, ReasonTargetLocked)
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
		expectQueued(t, res, err, got, stub, ReasonConcurrencyLimit)
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
		expectQueued(t, res, err, got, stub, ReasonConcurrencyLimit)
	})
	t.Run("unset limit lets one through", func(t *testing.T) {
		r, _, rr, _ := limitsWorld(t, 0)
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
	})
	t.Run("unknown phase is active", func(t *testing.T) {
		r, _, rr, stub := limitsWorld(t, 1, a("a", "SomethingNew"))
		res, got, err := reconcileLimits(t, r, rr)
		expectQueued(t, res, err, got, stub, ReasonConcurrencyLimit)
	})
	t.Run("deleting holds no slot", func(t *testing.T) {
		o := a("a", remediationv1alpha1.PhasePRCreated)
		o.Finalizers = []string{"remediation.shadewright.io/test"}
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
			o("f1", remediationv1alpha1.PhaseExpired, 30*time.Minute),
			o("f2", remediationv1alpha1.PhaseNeedsIntervention, 20*time.Minute),
			o("f3", remediationv1alpha1.PhaseFailed, 10*time.Minute),
		}
	}
	t.Run("open", func(t *testing.T) {
		r, _, rr, stub := limitsWorld(t, 20, failed()...)
		res, got, err := reconcileLimits(t, r, rr)
		expectQueued(t, res, err, got, stub, ReasonCircuitOpen)
	})
	t.Run("neutral outcomes do not close it", func(t *testing.T) {
		others := append(failed(), o("d", remediationv1alpha1.PhaseDenied, time.Minute),
			o("s", remediationv1alpha1.PhaseSuperseded, 2*time.Minute),
			o("c", remediationv1alpha1.PhaseCancelled, 3*time.Minute))
		r, _, rr, stub := limitsWorld(t, 20, others...)
		res, got, err := reconcileLimits(t, r, rr)
		expectQueued(t, res, err, got, stub, ReasonCircuitOpen)
	})
	t.Run("a success closes it", func(t *testing.T) {
		r, _, rr, _ := limitsWorld(t, 20, append(failed(), o("ok", remediationv1alpha1.PhaseMerged, time.Minute))...)
		_, got, err := reconcileLimits(t, r, rr)
		expectAdmitted(t, err, got)
	})
	t.Run("a success before the failures does not", func(t *testing.T) {
		r, _, rr, stub := limitsWorld(t, 20, append(failed(), o("ok", remediationv1alpha1.PhaseMerged, time.Hour))...)
		res, got, err := reconcileLimits(t, r, rr)
		expectQueued(t, res, err, got, stub, ReasonCircuitOpen)
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
		others := append(failed(), o("f4", remediationv1alpha1.PhaseExpired, time.Minute))
		r, _, rr, stub := limitsWorld(t, 20, others...)
		r.BreakerCooldown = 10 * time.Minute
		res, got, err := reconcileLimits(t, r, rr)
		expectQueued(t, res, err, got, stub, ReasonCircuitOpen)
	})
	t.Run("threshold", func(t *testing.T) {
		r, _, rr, stub := limitsWorld(t, 20, failed()[2:]...)
		r.BreakerThreshold = 1
		res, got, err := reconcileLimits(t, r, rr)
		expectQueued(t, res, err, got, stub, ReasonCircuitOpen)
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

// TestLimitsFailClosed checks that a request is not let through when the requests cannot be listed, before or
// after the policy check.
func TestLimitsFailClosed(t *testing.T) {
	for _, failAfter := range []int{0, 1} {
		t.Run(fmt.Sprintf("list fails after %d", failAfter), func(t *testing.T) {
			r, base, rr, stub := limitsWorld(t, 20)
			lists := 0
			r.Client = interceptorClient(base, func() error {
				lists++
				if lists > failAfter {
					return errors.New("list refused")
				}
				return nil
			})
			obs := &recordingObserver{}
			r.Limits = obs
			_, got, err := reconcileLimits(t, r, rr)
			if err == nil {
				t.Fatal("Reconcile returned no error although the requests could not be listed")
			}
			if got.Status.Phase != "" {
				t.Fatalf("phase = %q, want none", got.Status.Phase)
			}
			if want := int32(failAfter); stub.calls.Load() != want {
				t.Fatalf("resolver calls = %d, want %d", stub.calls.Load(), want)
			}
			if len(obs.waited) == 0 || obs.waited[len(obs.waited)-1] != ReasonLimitsUnknown {
				t.Fatalf("observer saw %v, want %s last", obs.waited, ReasonLimitsUnknown)
			}
		})
	}
}

// TestLimitsMissingSettingsStillDenied checks that missing settings do not hold a request back before the
// policy check, which denies it.
func TestLimitsMissingSettingsStillDenied(t *testing.T) {
	r, c, rr, _ := limitsWorld(t, 1)
	if err := c.Delete(context.Background(), &remediationv1alpha1.RemediationSettings{
		ObjectMeta: metav1.ObjectMeta{Name: remediationv1alpha1.SettingsName}}); err != nil {
		t.Fatal(err)
	}
	_, got, err := reconcileLimits(t, r, rr)
	if err != nil || got.Status.Phase != remediationv1alpha1.PhaseDenied {
		t.Fatalf("phase = %q, err = %v, want Denied", got.Status.Phase, err)
	}
}

// TestLimitsReportToObserver checks what the observer is told about a request held back by the cap.
func TestLimitsReportToObserver(t *testing.T) {
	r, _, rr, _ := limitsWorld(t, 1, otherRequest("a", "uid-a", remediationv1alpha1.PhasePRCreated, time.Minute))
	obs := &recordingObserver{}
	r.Limits = obs
	if _, _, err := reconcileLimits(t, r, rr); err != nil {
		t.Fatal(err)
	}
	if obs.active != 1 || obs.limit != 1 || obs.open || len(obs.waited) != 1 || obs.waited[0] != ReasonConcurrencyLimit {
		t.Fatalf("observer = %+v", obs)
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
func interceptorClient(base client.Client, fail func() error) client.WithWatch {
	return interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := fail(); err != nil {
				return err
			}
			return c.List(ctx, list, opts...)
		},
	})
}

type recordingObserver struct {
	waited        []string
	active, limit int
	open          bool
	failures      int
}

func (o *recordingObserver) AdmissionWaited(reason string) { o.waited = append(o.waited, reason) }
func (o *recordingObserver) ActiveRemediations(active, limit int) {
	o.active, o.limit = active, limit
}
func (o *recordingObserver) Breaker(open bool, failures int) { o.open, o.failures = open, failures }
