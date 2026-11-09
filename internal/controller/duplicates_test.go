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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
			o.Spec.Action.SetContainerMemoryLimit.Container = "sidecar"
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
