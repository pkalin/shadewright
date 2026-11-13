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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy"
)

// TestSupersedableOnlyWithoutPullRequestOrChain checks that only an undecided request without a lock, a pull
// request of its own or a supersede chain can end as the duplicate of a merged request (#556).
func TestSupersedableOnlyWithoutPullRequestOrChain(t *testing.T) {
	base := func() *remediationv1alpha1.RemediationRequest {
		return otherRequest("dup", limitsTarget, "", time.Minute)
	}
	if !supersedable(base()) {
		t.Fatal("an undecided request is not supersedable")
	}
	for name, mutate := range map[string]func(*remediationv1alpha1.RemediationRequest){
		"decided": func(o *remediationv1alpha1.RemediationRequest) {
			o.Status.Phase = remediationv1alpha1.PhasePolicyChecked
			o.Status.PlanDigest = limitsDigest
		},
		"holds the lock":   func(o *remediationv1alpha1.RemediationRequest) { o.Status.HoldsTarget = new(true) },
		"own pull request": func(o *remediationv1alpha1.RemediationRequest) { withPullRequest(o, ReasonPullRequestOpened) },
		"replacement": func(o *remediationv1alpha1.RemediationRequest) {
			o.Spec.Replaces = &remediationv1alpha1.RequestReplacement{}
		},
		"in a handoff": func(o *remediationv1alpha1.RemediationRequest) {
			o.Status.Handoff = &remediationv1alpha1.Handoff{}
		},
	} {
		o := base()
		mutate(o)
		if supersedable(o) {
			t.Errorf("%s: supersedable, want not", name)
		}
	}
}

// TestMergesBefore checks the merged request a duplicate is ended for: same Incident and target, merged with a
// recorded pull request, after the duplicate was created.
func TestMergesBefore(t *testing.T) {
	created := limitsNow.Add(-time.Hour)
	dup := otherRequest("dup", limitsTarget, "", 0)
	dup.CreationTimestamp = metav1.NewTime(created)
	merged := func() *remediationv1alpha1.RemediationRequest {
		return withPullRequest(otherRequest("won", limitsTarget, remediationv1alpha1.PhaseMerged, time.Minute), ReasonChangeRequestMerged)
	}
	if !mergesBefore(merged(), dup) {
		t.Fatal("a request merged after the duplicate was created does not supersede it")
	}
	for name, mutate := range map[string]func(*remediationv1alpha1.RemediationRequest){
		"itself":              func(o *remediationv1alpha1.RemediationRequest) { o.UID = dup.UID },
		"of another Incident": func(o *remediationv1alpha1.RemediationRequest) { ofIncident(o) },
		"on another target": func(o *remediationv1alpha1.RemediationRequest) {
			o.Spec.TargetRef.UID = "44444444-4444-4444-8444-444444444444"
		},
		// Another container of the same Deployment is another field: its merge fixes nothing for this one
		// (#556 design review).
		"on another container": func(o *remediationv1alpha1.RemediationRequest) {
			o.Spec.Action.SetContainerMemoryLimit.Container = sidecarContainer
		},
		"another action type": func(o *remediationv1alpha1.RemediationRequest) {
			o.Spec.Action = remediationv1alpha1.Action{Type: remediationv1alpha1.ActionAdjustKedaMaxReplicas,
				AdjustKedaMaxReplicas: &remediationv1alpha1.AdjustKedaMaxReplicas{PreviousValue: 5, DesiredValue: 10}}
		},
		"not merged": func(o *remediationv1alpha1.RemediationRequest) {
			o.Status.Phase = remediationv1alpha1.PhaseNeedsIntervention
		},
		"no pull request": func(o *remediationv1alpha1.RemediationRequest) { o.Status.PullRequest = nil },
		"merged before the duplicate was created": func(o *remediationv1alpha1.RemediationRequest) {
			at := metav1.NewMicroTime(created.Add(-time.Second))
			o.Status.LastPhaseTransitionTime = &at
		},
		"merged at mergedAt before the duplicate was created": func(o *remediationv1alpha1.RemediationRequest) {
			at := metav1.NewMicroTime(created.Add(-time.Second))
			o.Status.PullRequest.MergedAt = &at
		},
		// A later phase moves lastPhaseTransitionTime, not the merge: a request proposed after the merge but
		// before WaitingForDelivery, Verifying or Succeeded is no duplicate (#556 review).
		"merged before, in a later phase since": func(o *remediationv1alpha1.RemediationRequest) {
			merged := metav1.NewMicroTime(created.Add(-time.Second))
			o.Status.PullRequest.MergedAt = &merged
			o.Status.Phase = remediationv1alpha1.PhaseVerifying
		},
		"in a later phase with no merge time": func(o *remediationv1alpha1.RemediationRequest) {
			o.Status.Phase = remediationv1alpha1.PhaseSucceeded
		},
	} {
		o := merged()
		mutate(o)
		if mergesBefore(o, dup) {
			t.Errorf("%s: supersedes the duplicate, want not", name)
		}
	}
}

// TestMergedAtIsSetOnceWithMerged checks that the phase Merged records status.pullRequest.mergedAt in the same
// write, and that no later phase moves it, while lastPhaseTransitionTime moves with every phase (#556).
func TestMergedAtIsSetOnceWithMerged(t *testing.T) {
	now := limitsNow
	r := &RemediationRequestReconciler{Now: func() time.Time { return now }}
	rr := withPullRequest(otherRequest("won", limitsTarget, remediationv1alpha1.PhaseAutoMergeQueued, time.Hour), "")
	r.setPhase(rr, remediationv1alpha1.PhaseMerged)
	if rr.Status.PullRequest.MergedAt == nil || !rr.Status.PullRequest.MergedAt.Time.Equal(limitsNow) {
		t.Fatalf("mergedAt %v, want %v", rr.Status.PullRequest.MergedAt, limitsNow)
	}
	for _, p := range []remediationv1alpha1.RemediationPhase{remediationv1alpha1.PhaseWaitingForDelivery,
		remediationv1alpha1.PhaseVerifying, remediationv1alpha1.PhaseSucceeded} {
		now = now.Add(time.Minute)
		r.setPhase(rr, p)
		if !rr.Status.LastPhaseTransitionTime.Time.Equal(now) {
			t.Fatalf("%s: lastPhaseTransitionTime %v, want %v", p, rr.Status.LastPhaseTransitionTime, now)
		}
		if !rr.Status.PullRequest.MergedAt.Time.Equal(limitsNow) {
			t.Fatalf("%s: mergedAt moved to %v", p, rr.Status.PullRequest.MergedAt)
		}
		if at, ok := mergeTime(rr); !ok || !at.Equal(limitsNow) {
			t.Fatalf("%s: merge time %v %v, want %v", p, at, ok, limitsNow)
		}
	}
}

// TestWaitEpisodeIsBoundedAcrossReasons checks that a wait whose reason alternates between KEDA not having
// reported (ScaledObjectReadinessUnknown) and Flux reconciling (KustomizationReconciling) keeps one clock, so it
// still ends after TargetStateTimeout (#556 review): a new reason used to start the clock again.
func TestWaitEpisodeIsBoundedAcrossReasons(t *testing.T) {
	start := limitsNow
	now := start
	r := &RemediationRequestReconciler{Now: func() time.Time { return now }}
	rr := otherRequest("waits", limitsTarget, "", 0)
	keda := outcome{wait: true, status: metav1.ConditionUnknown, reason: string(policy.ReasonScaledObjectReadinessUnknown),
		message: "KEDA has not reported", timeoutReason: string(policy.ReasonScaledObjectNotReady),
		timeoutMessage: messageTargetStateTimeout}
	flux := fluxWait(sourcemap.Result{Reason: sourcemap.ReasonKustomizationReconciling, Message: "Flux is reconciling"})
	step := DefaultTargetStateTimeout / 4
	for i := range 4 {
		out := keda
		if i%2 == 1 {
			out = flux
		}
		got, delay := r.waitForTarget(rr, out)
		if delay <= 0 || got.phase != "" {
			t.Fatalf("after %v (%s): the wait ended early: %+v", now.Sub(start), out.reason, got)
		}
		setPolicyAllowed(&rr.Status.Conditions, metav1.Condition{Status: metav1.ConditionUnknown, Reason: out.reason,
			Message: out.message}, now)
		if c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed); !c.LastTransitionTime.Time.Equal(start) {
			t.Fatalf("after %v (%s): the wait clock started again at %v", now.Sub(start), out.reason, c.LastTransitionTime)
		}
		now = now.Add(step)
	}
	got, delay := r.waitForTarget(rr, keda)
	if delay != 0 || got.phase != remediationv1alpha1.PhaseNeedsIntervention ||
		got.reason != string(policy.ReasonScaledObjectNotReady) {
		t.Fatalf("after %v: %+v, delay %v; want NeedsIntervention", now.Sub(start), got, delay)
	}
	// A condition that is not a wait (another reason) still lends no age to one.
	rr.Status.Conditions = nil
	setPolicyAllowed(&rr.Status.Conditions, metav1.Condition{Status: metav1.ConditionUnknown, Reason: "SomeOtherReason"}, start)
	setPolicyAllowed(&rr.Status.Conditions, metav1.Condition{Status: metav1.ConditionUnknown, Reason: flux.reason}, now)
	if c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed); !c.LastTransitionTime.Time.Equal(now) {
		t.Fatalf("a condition with another reason lent its time %v to a wait", c.LastTransitionTime)
	}
}

// closedHolder is the request whose pull request a reviewer closes in the tests of #643.
const closedHolder = "first"

// closeUnmerged moves o to the status the pull request step leaves when a person closed its recorded pull request
// without a merge (rejected): NeedsIntervention at at, PullRequestOpened rewritten by markClosed, Approved False
// with ChangeRequestClosed, and the lock released by settleLock.
func closeUnmerged(o *remediationv1alpha1.RemediationRequest, at time.Time) *remediationv1alpha1.RemediationRequest {
	if o.Status.PullRequest == nil {
		withPullRequest(o, ReasonPullRequestOpened)
	}
	o.Status.Phase = remediationv1alpha1.PhaseNeedsIntervention
	t := metav1.NewMicroTime(at)
	o.Status.LastPhaseTransitionTime = &t
	markClosed(o, ReasonChangeRequestClosed, "was closed without a merge")
	meta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{Type: remediationv1alpha1.ConditionApproved,
		Status: metav1.ConditionFalse, Reason: ReasonChangeRequestClosed, Message: "closed"})
	o.Status.HoldsTarget = new(false)
	return o
}

// TestClosedBefore checks the request whose closed pull request ends a waiting one (#643): same target and field,
// of any Incident, ended NeedsIntervention because its pull request was closed without a merge, after the
// waiting request was created.
func TestClosedBefore(t *testing.T) {
	created := limitsNow.Add(-time.Hour)
	queued := otherRequest("queued", limitsTarget, "", 0)
	queued.CreationTimestamp = metav1.NewTime(created)
	closed := func() *remediationv1alpha1.RemediationRequest {
		return closeUnmerged(otherRequest(closedHolder, limitsTarget, "", 0), created.Add(time.Minute))
	}
	if !closedBefore(closed(), queued) {
		t.Fatal("a pull request for the same field closed after the request was created does not end it")
	}
	if !closedBefore(ofIncident(closed()), queued) {
		t.Fatal("a pull request of another Incident for the same field does not end it")
	}
	// Closed before it was recorded (adoptChangeRequest): the stop is on PullRequestOpened.
	unrecorded := closed()
	unrecorded.Status.PullRequest = nil
	unrecorded.Status.Conditions = []metav1.Condition{{Type: remediationv1alpha1.ConditionPROpened,
		Status: metav1.ConditionFalse, Reason: ReasonChangeRequestClosed}}
	if !closedBefore(unrecorded, queued) {
		t.Fatal("a pull request closed before it was recorded does not end it")
	}
	for name, mutate := range map[string]func(*remediationv1alpha1.RemediationRequest){
		"itself": func(o *remediationv1alpha1.RemediationRequest) { o.UID = queued.UID },
		"on another target": func(o *remediationv1alpha1.RemediationRequest) {
			o.Spec.TargetRef.UID = "44444444-4444-4444-8444-444444444444"
		},
		"on another container": func(o *remediationv1alpha1.RemediationRequest) {
			o.Spec.Action.SetContainerMemoryLimit.Container = sidecarContainer
		},
		"closed before the request was created": func(o *remediationv1alpha1.RemediationRequest) {
			at := metav1.NewMicroTime(created.Add(-time.Second))
			o.Status.LastPhaseTransitionTime = &at
		},
		"no phase time": func(o *remediationv1alpha1.RemediationRequest) { o.Status.LastPhaseTransitionTime = nil },
		// Shadewright closed it because its plan went stale: no reviewer said no.
		"closed by Shadewright": func(o *remediationv1alpha1.RemediationRequest) {
			o.Status.Phase = remediationv1alpha1.PhaseSuperseded
			o.Status.Conditions = nil
			markClosed(o, ReasonPullRequestClosed, "is closed: stale")
		},
		"stopped for another reason": func(o *remediationv1alpha1.RemediationRequest) {
			o.Status.Conditions = nil
			withPullRequest(o, ReasonHeadMoved)
		},
		"ended Cancelled": func(o *remediationv1alpha1.RemediationRequest) {
			o.Status.Phase = remediationv1alpha1.PhaseCancelled
		},
	} {
		o := closed()
		mutate(o)
		if closedBefore(o, queued) {
			t.Errorf("%s: ends the request, want not", name)
		}
	}
}

// TestQueuedRequestAfterSameFieldPullRequestClosed reproduces the lab run of 2026-11-10 (#643): requests of
// three Incidents proposed a memory limit for the same container; the first opened a pull request, the others
// waited Queued with TargetLocked, and when a reviewer closed the first pull request without a merge, a queued
// one took the lock and opened a second pull request for the same field. It must end Superseded with
// AnotherRequestClosed instead, reading nothing from Git, while a request for another field of the same target
// still goes through.
func TestQueuedRequestAfterSameFieldPullRequestClosed(t *testing.T) {
	run := func(t *testing.T, mutate func(*remediationv1alpha1.RemediationRequest)) (
		*remediationv1alpha1.RemediationRequest, *stubResolver, int32, error) {
		t.Helper()
		first := ofIncident(holding(withPullRequest(otherRequest(closedHolder, limitsTarget,
			remediationv1alpha1.PhaseAwaitingApproval, time.Minute), ReasonPullRequestOpened), true))
		mutate(first)
		r, c, rr, stub := limitsWorld(t, 20, first)
		res, got, err := reconcileLimits(t, r, rr)
		expectQueuedAfter(t, r, rr, res, err, got, stub, ReasonTargetLocked, admissionRecheckDelay)

		ctx := context.Background()
		holder := &remediationv1alpha1.RemediationRequest{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: requestNamespace, Name: closedHolder}, holder); err != nil {
			t.Fatal(err)
		}
		closeUnmerged(holder, got.CreationTimestamp.Add(time.Minute))
		if err := c.Status().Update(ctx, holder); err != nil {
			t.Fatal(err)
		}
		calls := stub.calls.Load()
		_, got, err = reconcileLimits(t, r, rr)
		return got, stub, calls, err
	}
	t.Run("same field: ends Superseded and opens nothing", func(t *testing.T) {
		got, stub, calls, err := run(t, func(*remediationv1alpha1.RemediationRequest) {})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if got.Status.Phase != remediationv1alpha1.PhaseSuperseded {
			t.Fatalf("phase = %q, want Superseded", got.Status.Phase)
		}
		c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
		if c == nil || c.Status != metav1.ConditionFalse || c.Reason != remediationv1alpha1.ReasonAnotherRequestClosed ||
			!strings.Contains(c.Message, closedHolder) || !strings.Contains(c.Message, "#7") {
			t.Fatalf("PlanFresh = %+v, want False with AnotherRequestClosed naming the request and its pull request", c)
		}
		if meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionQueued) != nil {
			t.Fatal("the ended request keeps Queued")
		}
		if holdsLock(got) || got.Status.PlanDigest != "" {
			t.Fatalf("the ended request holds the lock or a plan: %+v", got.Status)
		}
		if n := stub.calls.Load(); n != calls {
			t.Fatalf("the ended request read Git: %d resolver calls, want %d", n, calls)
		}
	})
	t.Run("another field of the same target: goes through", func(t *testing.T) {
		got, _, _, err := run(t, func(o *remediationv1alpha1.RemediationRequest) {
			o.Spec.Action.SetContainerMemoryLimit.Container = sidecarContainer
		})
		expectAdmitted(t, err, got)
	})
}

// TestEndingRequestReadFailsIsRetried checks the error path of #643: when the cache does not show the other
// request ended yet and reading it from the API server fails, the look-up returns the error (retried with
// backoff) and the request is not ended; once the read succeeds and shows the pull request closed without a
// merge, it ends Superseded.
func TestEndingRequestReadFailsIsRetried(t *testing.T) {
	c, rr := deadlineWorld(t)
	ctx := context.Background()
	// The cache still shows the other request waiting for review, as before the status write that closed it.
	first := ofIncident(holding(withPullRequest(otherRequest(closedHolder, limitsTarget,
		remediationv1alpha1.PhaseAwaitingApproval, time.Minute), ReasonPullRequestOpened), true))
	status := first.Status
	if err := c.Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	first.Status = status
	if err := c.Status().Update(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	fail := true
	api := interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name != closedHolder {
				return cl.Get(ctx, key, obj, opts...)
			}
			if fail {
				return errors.New("the API server is unavailable")
			}
			if err := cl.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			closeUnmerged(obj.(*remediationv1alpha1.RemediationRequest), rr.CreationTimestamp.Add(time.Minute))
			return nil
		},
	})
	r := &RemediationRequestReconciler{Client: c, APIReader: api, Scheme: c.Scheme(),
		Now: func() time.Time { return limitsNow }}

	ended, err := r.endIfDuplicate(ctx, rr)
	if err == nil || !strings.Contains(err.Error(), "unavailable") || ended {
		t.Fatalf("ended %v, error %v; want the read's error and not ended", ended, err)
	}
	got := &remediationv1alpha1.RemediationRequest{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(rr), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != "" {
		t.Fatalf("phase = %q after a failed read, want the request left undecided", got.Status.Phase)
	}

	fail = false
	ended, err = r.endIfDuplicate(ctx, got)
	if err != nil || !ended {
		t.Fatalf("ended %v, error %v; want ended", ended, err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(rr), got); err != nil {
		t.Fatal(err)
	}
	c2 := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
	if got.Status.Phase != remediationv1alpha1.PhaseSuperseded || c2 == nil ||
		c2.Reason != remediationv1alpha1.ReasonAnotherRequestClosed {
		t.Fatalf("phase %q, PlanFresh %+v; want Superseded with AnotherRequestClosed", got.Status.Phase, c2)
	}
}
