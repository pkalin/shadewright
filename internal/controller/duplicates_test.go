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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
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
	if !mergesBefore(context.Background(), merged(), dup) {
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
		if mergesBefore(context.Background(), o, dup) {
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

// queuedBehindOf records o in rr's status.queuedBehind, as waitForLimits does.
func queuedBehindOf(rr, o *remediationv1alpha1.RemediationRequest) {
	rr.Status.QueuedBehind = []remediationv1alpha1.RequestRef{{Name: o.Name, UID: remediationv1alpha1.UID(o.UID)}}
}

// TestClosedHolderOf checks the holder whose closed pull request ends a request that waited behind it (#643):
// recorded in status.queuedBehind, the same target and field, of any Incident, ended NeedsIntervention because
// its pull request was closed without a merge, whenever that was.
func TestClosedHolderOf(t *testing.T) {
	queued := otherRequest("queued", limitsTarget, "", 0)
	queued.CreationTimestamp = metav1.NewTime(limitsNow)
	closed := func() *remediationv1alpha1.RemediationRequest {
		// Ended an hour before the request was created: the record decides, not the clock.
		return closeUnmerged(otherRequest(closedHolder, limitsTarget, "", 0), limitsNow.Add(-time.Hour))
	}
	find := func(t *testing.T, holder *remediationv1alpha1.RemediationRequest, record bool) *remediationv1alpha1.RemediationRequest {
		t.Helper()
		status := holder.Status
		c := clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).
			WithStatusSubresource(&remediationv1alpha1.RemediationRequest{}).Build()
		if err := c.Create(context.Background(), holder); err != nil {
			t.Fatal(err)
		}
		holder.Status = status
		if err := c.Status().Update(context.Background(), holder); err != nil {
			t.Fatal(err)
		}
		rr := queued.DeepCopy()
		if record {
			queuedBehindOf(rr, holder)
		}
		r := &RemediationRequestReconciler{Client: c, APIReader: c, Scheme: c.Scheme()}
		got, err := r.closedHolderOf(context.Background(), rr)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := find(t, closed(), true); got == nil || got.Name != closedHolder {
		t.Fatalf("got %v, want the recorded holder whose pull request for the same field was closed", got)
	}
	if find(t, ofIncident(closed()), true) == nil {
		t.Fatal("a recorded holder of another Incident for the same field does not end it")
	}
	// Closed before it was recorded (adoptChangeRequest): the stop is on PullRequestOpened.
	unrecorded := closed()
	unrecorded.Status.PullRequest = nil
	unrecorded.Status.Conditions = []metav1.Condition{{Type: remediationv1alpha1.ConditionPROpened,
		Status: metav1.ConditionFalse, Reason: ReasonChangeRequestClosed, Message: "closed before it was recorded"}}
	if find(t, unrecorded, true) == nil {
		t.Fatal("a pull request closed before it was recorded does not end it")
	}
	if find(t, closed(), false) != nil {
		t.Fatal("a closed pull request ends a request that never waited behind its request")
	}
	for name, mutate := range map[string]func(*remediationv1alpha1.RemediationRequest){
		"on another target": func(o *remediationv1alpha1.RemediationRequest) {
			o.Spec.TargetRef.UID = "44444444-4444-4444-8444-444444444444"
		},
		"on another container": func(o *remediationv1alpha1.RemediationRequest) {
			o.Spec.Action.SetContainerMemoryLimit.Container = sidecarContainer
		},
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
		if find(t, o, true) != nil {
			t.Errorf("%s: ends the request, want not", name)
		}
	}
	t.Run("recreated under the same name", func(t *testing.T) {
		rr := queued.DeepCopy()
		rr.Status.QueuedBehind = []remediationv1alpha1.RequestRef{{Name: closedHolder,
			UID: "77777777-7777-4777-8777-777777777777"}}
		c := clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).WithObjects(closed()).Build()
		r := &RemediationRequestReconciler{Client: c, APIReader: c, Scheme: c.Scheme()}
		if got, err := r.closedHolderOf(context.Background(), rr); err != nil || got != nil {
			t.Fatalf("got %v, %v; want nothing for a request recreated under the recorded name", got, err)
		}
	})
	t.Run("deleted", func(t *testing.T) {
		rr := queued.DeepCopy()
		queuedBehindOf(rr, closed())
		c := clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).Build()
		r := &RemediationRequestReconciler{Client: c, APIReader: c, Scheme: c.Scheme()}
		if got, err := r.closedHolderOf(context.Background(), rr); err != nil || got != nil {
			t.Fatalf("got %v, %v; want nothing for a recorded request that no longer exists", got, err)
		}
	})
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

// TestEndingRequestReadFailsIsRetried checks the error path of #643: when reading the recorded holder from the
// API server fails, the look-up returns the error (retried with backoff) and the request is neither ended nor
// let through; once the read succeeds and shows the pull request closed without a merge, it ends Superseded.
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
	queuedBehindOf(rr, first)
	if err := c.Status().Update(ctx, rr); err != nil {
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

// createdAt sets the creation time of rr in c to at, as the API server stores it: the object goes through JSON,
// which keeps whole seconds only.
func createdAt(t *testing.T, c client.Client, rr *remediationv1alpha1.RemediationRequest, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := c.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	rr.CreationTimestamp = metav1.NewTime(at)
	raw, err := json.Marshal(rr)
	if err != nil {
		t.Fatal(err)
	}
	stored := &remediationv1alpha1.RemediationRequest{}
	if err := json.Unmarshal(raw, stored); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if !rr.CreationTimestamp.Equal(&stored.CreationTimestamp) {
		t.Fatalf("creationTimestamp = %v, want %v", rr.CreationTimestamp, stored.CreationTimestamp)
	}
}

// expectEndedByClosed checks that got ended Superseded with AnotherRequestClosed, naming closedHolder.
func expectEndedByClosed(t *testing.T, err error, got *remediationv1alpha1.RemediationRequest) {
	t.Helper()
	const by = closedHolder
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
	if got.Status.Phase != remediationv1alpha1.PhaseSuperseded || c == nil ||
		c.Reason != remediationv1alpha1.ReasonAnotherRequestClosed || !strings.Contains(c.Message, by) {
		t.Fatalf("phase %q, PlanFresh %+v; want Superseded with AnotherRequestClosed naming %s", got.Status.Phase, c, by)
	}
}

// stoppedHolder is a request for the same field that stopped at at (NeedsIntervention, HeadMoved) with its
// pull request still open, so it keeps the target lock until checkHeld sees the pull request closed.
func stoppedHolder(at time.Time) *remediationv1alpha1.RemediationRequest {
	o := ofIncident(holding(withPullRequest(otherRequest(closedHolder, limitsTarget,
		remediationv1alpha1.PhaseNeedsIntervention, limitsNow.Sub(at)), ReasonHeadMoved), true))
	return o
}

// seenClosed is what checkHeld writes when it finds the pull request of a stopped holder closed without a
// merge: PullRequestOpened ChangeRequestClosed and the lock released, the phase and its time unchanged.
func seenClosed(t *testing.T, c client.Client, name string) {
	t.Helper()
	ctx := context.Background()
	o := &remediationv1alpha1.RemediationRequest{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: requestNamespace, Name: name}, o); err != nil {
		t.Fatal(err)
	}
	markClosed(o, ReasonChangeRequestClosed, "was closed without a merge")
	o.Status.HoldsTarget = new(false)
	if err := c.Status().Update(ctx, o); err != nil {
		t.Fatal(err)
	}
}

// TestClosedPullRequestSparesRequestsThatNeverWaited checks that a pull request closed without a merge ends only
// the requests that waited behind its request (#643), whatever the clocks say: a request that first looked at
// the target after the close never waited behind it and goes through, also when its creation time (whole
// seconds) reads earlier than the close (sub-seconds), or when the close was observed, and the phase time
// written, only after the request was created.
func TestClosedPullRequestSparesRequestsThatNeverWaited(t *testing.T) {
	for name, tc := range map[string]struct{ closed, created time.Time }{
		"closed in the same second": {
			closed:  limitsNow.Add(200 * time.Millisecond),
			created: limitsNow.Add(800 * time.Millisecond),
		},
		"close observed after the request was created": {
			closed:  limitsNow.Add(2 * time.Minute),
			created: limitsNow.Add(time.Minute),
		},
	} {
		t.Run(name, func(t *testing.T) {
			first := closeUnmerged(ofIncident(otherRequest(closedHolder, limitsTarget, "", 0)), tc.closed)
			r, c, rr, _ := limitsWorld(t, 20, first)
			createdAt(t, c, rr, tc.created)
			_, got, err := reconcileLimits(t, r, rr)
			expectAdmitted(t, err, got)
		})
	}
}

// TestQueuedBehindStoppedHolderEndsWhenItsPullRequestCloses checks #643 for a holder that stopped before the
// request was created (NeedsIntervention since 11:58, HeadMoved) and whose pull request checkHeld later finds
// closed without a merge: the phase and its time stay, yet the request queued behind it at 11:59 ends
// Superseded. It also ends when another request took the lock before it looked again: the record of the holder
// it waited behind is checked before it is replaced.
func TestQueuedBehindStoppedHolderEndsWhenItsPullRequestCloses(t *testing.T) {
	run := func(t *testing.T, between func(*testing.T, client.Client)) {
		t.Helper()
		r, c, rr, stub := limitsWorld(t, 20, stoppedHolder(limitsNow.Add(-2*time.Minute)))
		createdAt(t, c, rr, limitsNow.Add(-time.Minute))
		res, got, err := reconcileLimits(t, r, rr)
		expectQueuedAfter(t, r, rr, res, err, got, stub, ReasonTargetLocked, heldRecheckInterval)
		seenClosed(t, c, closedHolder)
		between(t, c)
		calls := stub.calls.Load()
		_, got, err = reconcileLimits(t, r, rr)
		expectEndedByClosed(t, err, got)
		if n := stub.calls.Load(); n != calls {
			t.Fatalf("the ended request read Git: %d resolver calls, want %d", n, calls)
		}
	}
	t.Run("lock released", func(t *testing.T) { run(t, func(*testing.T, client.Client) {}) })
	t.Run("another request took the lock meanwhile", func(t *testing.T) {
		run(t, func(t *testing.T, c client.Client) {
			next := ofIncident(holding(otherRequest("next", limitsTarget, remediationv1alpha1.PhasePolicyChecked, 0), true))
			status := next.Status
			if err := c.Create(context.Background(), next); err != nil {
				t.Fatal(err)
			}
			next.Status = status
			if err := c.Status().Update(context.Background(), next); err != nil {
				t.Fatal(err)
			}
		})
	})
}

// TestClosedHolderIsReadLive checks that the holder a request waited behind is read from the API server, not
// the cache: checkHeld records the close and releases the lock without a phase change, the limits (which read
// the API server) see the lock free, and a cache that still shows the holder without the close must not let the
// request through.
func TestClosedHolderIsReadLive(t *testing.T) {
	r, c, rr, stub := limitsWorld(t, 20, stoppedHolder(limitsNow))
	createdAt(t, c, rr, limitsNow.Add(-time.Hour))
	res, got, err := reconcileLimits(t, r, rr)
	expectQueuedAfter(t, r, rr, res, err, got, stub, ReasonTargetLocked, heldRecheckInterval)
	stale := &remediationv1alpha1.RemediationRequest{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: requestNamespace, Name: closedHolder}, stale); err != nil {
		t.Fatal(err)
	}
	seenClosed(t, c, closedHolder)
	// The cache still shows the holder as it was before the close.
	staleHolder := func(o *remediationv1alpha1.RemediationRequest) {
		if o.Name == closedHolder {
			stale.DeepCopyInto(o)
		}
	}
	r.Client = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := cl.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if o, ok := obj.(*remediationv1alpha1.RemediationRequest); ok {
				staleHolder(o)
			}
			return nil
		},
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := cl.List(ctx, list, opts...); err != nil {
				return err
			}
			if l, ok := list.(*remediationv1alpha1.RemediationRequestList); ok {
				for i := range l.Items {
					staleHolder(&l.Items[i])
				}
			}
			return nil
		},
	})
	_, got, err = reconcileLimits(t, r, rr)
	expectEndedByClosed(t, err, got)
}

// TestQueuedBehindRecordedWhateverTheReason checks that the holders of the lock are recorded when another limit
// (here the open circuit breaker) holds the request back first, so a pull request closed while it waited for the
// breaker still ends it once the breaker lets it through (#643).
func TestQueuedBehindRecordedWhateverTheReason(t *testing.T) {
	failed := func(name string, ago time.Duration) *remediationv1alpha1.RemediationRequest {
		return otherRequest(name, "uid-target-"+name, remediationv1alpha1.PhaseFailed, ago)
	}
	first := ofIncident(holding(withPullRequest(otherRequest(closedHolder, limitsTarget,
		remediationv1alpha1.PhaseAwaitingApproval, time.Hour), ReasonPullRequestOpened), true))
	r, c, rr, stub := limitsWorld(t, 20, failed("f1", 30*time.Minute), failed("f2", 20*time.Minute),
		failed("f3", 10*time.Minute), first)
	res, got, err := reconcileLimits(t, r, rr)
	expectQueued(t, r, rr, res, err, got, stub, ReasonCircuitOpen)
	got = &remediationv1alpha1.RemediationRequest{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(rr), got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.QueuedBehind) != 1 || got.Status.QueuedBehind[0].Name != closedHolder {
		t.Fatalf("queuedBehind = %+v, want the holder %s", got.Status.QueuedBehind, closedHolder)
	}
	ctx := context.Background()
	holder := &remediationv1alpha1.RemediationRequest{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: requestNamespace, Name: closedHolder}, holder); err != nil {
		t.Fatal(err)
	}
	closeUnmerged(holder, limitsNow.Add(-2*time.Minute))
	if err := c.Status().Update(ctx, holder); err != nil {
		t.Fatal(err)
	}
	// A success after every failure closes the breaker.
	ok := otherRequest("ok", "uid-target-ok", remediationv1alpha1.PhaseMerged, time.Minute)
	status := ok.Status
	if err := c.Create(ctx, ok); err != nil {
		t.Fatal(err)
	}
	ok.Status = status
	if err := c.Status().Update(ctx, ok); err != nil {
		t.Fatal(err)
	}
	_, got, err = reconcileLimits(t, r, rr)
	expectEndedByClosed(t, err, got)
}

// TestQueuedBehindRecordsSameFieldHoldersFirst checks the cut of status.queuedBehind to MaxQueuedBehind (#643):
// with five holders of the lock, the one for the waiter's field is recorded although it sorts last by name, so
// its pull request closed without a merge still ends the waiter.
func TestQueuedBehindRecordsSameFieldHoldersFirst(t *testing.T) {
	others := make([]*remediationv1alpha1.RemediationRequest, 0, 1+remediationv1alpha1.MaxQueuedBehind)
	others = append(others, ofIncident(holding(withPullRequest(otherRequest(closedHolder,
		limitsTarget, remediationv1alpha1.PhaseAwaitingApproval, time.Hour), ReasonPullRequestOpened), true)))
	for i := range remediationv1alpha1.MaxQueuedBehind {
		others = append(others, otherField(holding(withPullRequest(otherRequest(fmt.Sprintf("a%d", i), limitsTarget,
			remediationv1alpha1.PhaseAwaitingApproval, time.Hour), ReasonPullRequestOpened), true)))
	}
	r, c, rr, stub := limitsWorld(t, 20, others...)
	res, got, err := reconcileLimits(t, r, rr)
	expectQueued(t, r, rr, res, err, got, stub, ReasonTargetLocked)
	if n := len(got.Status.QueuedBehind); n != remediationv1alpha1.MaxQueuedBehind ||
		got.Status.QueuedBehind[0].Name != closedHolder {
		t.Fatalf("queuedBehind = %+v, want %d holders, %s first", got.Status.QueuedBehind,
			remediationv1alpha1.MaxQueuedBehind, closedHolder)
	}
	ctx := context.Background()
	holder := &remediationv1alpha1.RemediationRequest{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: requestNamespace, Name: closedHolder}, holder); err != nil {
		t.Fatal(err)
	}
	closeUnmerged(holder, limitsNow.Add(-2*time.Minute))
	if err := c.Status().Update(ctx, holder); err != nil {
		t.Fatal(err)
	}
	_, got, err = reconcileLimits(t, r, rr)
	expectEndedByClosed(t, err, got)
}
