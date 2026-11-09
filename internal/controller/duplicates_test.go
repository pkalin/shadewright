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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
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
		"not merged": func(o *remediationv1alpha1.RemediationRequest) {
			o.Status.Phase = remediationv1alpha1.PhaseNeedsIntervention
		},
		"no pull request": func(o *remediationv1alpha1.RemediationRequest) { o.Status.PullRequest = nil },
		"merged before the duplicate was created": func(o *remediationv1alpha1.RemediationRequest) {
			at := metav1.NewMicroTime(created.Add(-time.Second))
			o.Status.LastPhaseTransitionTime = &at
		},
	} {
		o := merged()
		mutate(o)
		if mergesBefore(o, dup) {
			t.Errorf("%s: supersedes the duplicate, want not", name)
		}
	}
}
