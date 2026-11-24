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

package incidents

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
)

// The executor's AnotherRequestClosed message (internal/controller, supersedeMessage, checked there by
// TestAnotherRequestClosedMessageMatchesTheIncidentsNextStep) states the Incident's next step: unless a newer
// request names it, it moves to Escalated, and it is not analyzed again while the symptom continues, while one
// not yet analyzed (Open) gets its first analysis (#930). These tests hold the rules to that statement; a change
// here that makes it false must change the message too.
func TestAnotherRequestClosedIncidentIsEscalatedAndNotAnalyzedAgain(t *testing.T) {
	inc := closureIncident(remediationv1alpha1.IncidentProposed, time.Minute)
	inc.Status.LastAnalysisTime = ago(2 * time.Hour)
	waiter := request("waiter", time.Hour, remediationv1alpha1.PhaseSuperseded)
	waiter.Status.Conditions = []metav1.Condition{{Type: remediationv1alpha1.ConditionPlanFresh,
		Status: metav1.ConditionFalse, Reason: remediationv1alpha1.ReasonAnotherRequestClosed}}

	// Unless a newer request names it: an active newer request keeps the Incident where it is.
	newer := request("newer", time.Minute, remediationv1alpha1.PhasePending)
	if phase, reason, ok := Closure(inc.DeepCopy(), []remediationv1alpha1.RemediationRequest{waiter, newer},
		time.Time{}, triggerNow); ok {
		t.Errorf("with a newer active request the Incident moved to %s (%s)", phase, reason)
	}

	// Otherwise it moves to Escalated.
	phase, reason, ok := Closure(inc.DeepCopy(), []remediationv1alpha1.RemediationRequest{waiter}, time.Time{}, triggerNow)
	if !ok || phase != remediationv1alpha1.IncidentEscalated || reason != ReasonRequestSuperseded {
		t.Fatalf("closure gave %q (%s, moved %v), want Escalated (%s)", phase, reason, ok, ReasonRequestSuperseded)
	}
	inc.Status.Phase = phase
	meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{Type: ConditionLifecycle,
		Status: metav1.ConditionTrue, Reason: reason, LastTransitionTime: *ago(0)})

	// And it is not analyzed again while the symptom continues, past any debounce or cooldown.
	for _, later := range []time.Duration{time.Hour, 24 * time.Hour} {
		seen := inc.DeepCopy()
		seen.Status.LastSeen = &metav1.Time{Time: triggerNow.Add(later - 5*time.Minute)}
		seen.Status.Count += 10
		if due, why := Due(seen, Memory{}, triggerNow.Add(later), triggerSettings, 0); due {
			t.Errorf("the Escalated Incident is due %s later (%s); the AnotherRequestClosed message says it is not", later, why)
		}
	}
}

// An Incident still Open (never analyzed, for example a hand-applied request while the analysis budget is 0)
// does not move to Escalated when its request is superseded, and it is due FirstAnalysis: the message must not
// say flatly that no analysis follows.
func TestAnotherRequestClosedOpenIncidentGetsItsFirstAnalysis(t *testing.T) {
	inc := closureIncident(remediationv1alpha1.IncidentOpen, 10*time.Minute)
	waiter := request("waiter", time.Hour, remediationv1alpha1.PhaseSuperseded)
	waiter.Status.Conditions = []metav1.Condition{{Type: remediationv1alpha1.ConditionPlanFresh,
		Status: metav1.ConditionFalse, Reason: remediationv1alpha1.ReasonAnotherRequestClosed}}

	if phase, reason, ok := Closure(inc.DeepCopy(), []remediationv1alpha1.RemediationRequest{waiter}, time.Time{},
		triggerNow); ok {
		t.Errorf("the Open Incident moved to %s (%s)", phase, reason)
	}
	if due, why := Due(inc, Memory{}, triggerNow, triggerSettings, 0); !due || why != ReasonFirstAnalysis {
		t.Errorf("the Open Incident is due %v (%s), want due with %s", due, why, ReasonFirstAnalysis)
	}
}
