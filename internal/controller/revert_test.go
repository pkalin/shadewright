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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

const (
	failedUID    = "13131313-1313-4313-8313-131313131313"
	whyUID       = "has another UID"
	whyIncident  = "names another Incident"
	whyTarget    = "names another target"
	whyNoInverse = "not exactly the inverse"
)

// failedAndRevert returns a request that ended Failed with Verified False and its exact revert.
func failedAndRevert() (failed, revert *remediationv1alpha1.RemediationRequest) {
	failed = fx.MemoryLimitRequest("default", "remediation-failed")
	failed.UID = failedUID
	failed.Status.Phase = remediationv1alpha1.PhaseFailed
	meta.SetStatusCondition(&failed.Status.Conditions, metav1.Condition{Type: remediationv1alpha1.ConditionVerified,
		Status: metav1.ConditionFalse, Reason: ReasonOOMKilled})
	revert = fx.MemoryLimitRequest("default", string(remediationv1alpha1.RevertName(failedUID)))
	m := revert.Spec.Action.SetContainerMemoryLimit
	m.Previous, m.Desired = m.Desired, m.Previous
	revert.Spec.Reverts = &remediationv1alpha1.RequestRef{Name: failed.Name, UID: failedUID}
	return failed, revert
}

func TestRevertLinkProblem(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(failed, revert *remediationv1alpha1.RemediationRequest)
		why  string
	}{
		"an exact revert": {func(_, _ *remediationv1alpha1.RemediationRequest) {}, ""},
		"the same values in other units": {func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.Action.SetContainerMemoryLimit.Previous = resource.MustParse("402653184")
		}, ""},
		"a failed request of another UID": {func(f, _ *remediationv1alpha1.RemediationRequest) { f.UID = types.UID("x") }, whyUID},
		"not Failed": {func(f, _ *remediationv1alpha1.RemediationRequest) {
			f.Status.Phase = remediationv1alpha1.PhaseInconclusive
		}, "did not end Failed"},
		"Failed without Verified": {func(f, _ *remediationv1alpha1.RemediationRequest) {
			f.Status.Conditions = nil
		}, "Verified False"},
		"Failed with Verified Unknown": {func(f, _ *remediationv1alpha1.RemediationRequest) {
			f.Status.Conditions[0].Status = metav1.ConditionUnknown
		}, "Verified False"},
		"a revert itself": {func(f, _ *remediationv1alpha1.RemediationRequest) {
			f.Spec.Reverts = &remediationv1alpha1.RequestRef{Name: "remediation-older", UID: failedUID}
		}, "never reverted"},
		"an Incident of another name": {func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.IncidentRef.Name = "incident-other"
		}, whyIncident},
		"another target name": {func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.TargetRef.Name = "web-elsewhere"
		}, whyTarget},
		"another target UID": {func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.TargetRef.UID = "14141414-1414-4414-8414-141414141414"
		}, whyTarget},
		"another container": {func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.Action.SetContainerMemoryLimit.Container = "sidecar"
		}, whyNoInverse},
		"the same direction": {func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.Action = fx.MemoryLimitAction()
		}, whyNoInverse},
		"another previous": {func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.Action.SetContainerMemoryLimit.Previous = resource.MustParse("512Mi")
		}, whyNoInverse},
		"another action type": {func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.Action = remediationv1alpha1.Action{Type: remediationv1alpha1.ActionAdjustKedaMaxReplicas,
				AdjustKedaMaxReplicas: &remediationv1alpha1.AdjustKedaMaxReplicas{PreviousValue: 2, DesiredValue: 1}}
		}, whyNoInverse},
		"a second action member": {func(_, r *remediationv1alpha1.RemediationRequest) {
			r.Spec.Action.AdjustKedaMaxReplicas = &remediationv1alpha1.AdjustKedaMaxReplicas{PreviousValue: 2, DesiredValue: 1}
		}, whyNoInverse},
	} {
		t.Run(name, func(t *testing.T) {
			failed, revert := failedAndRevert()
			tc.edit(failed, revert)
			got := revertLinkProblem(revert, failed)
			if (got == "") != (tc.why == "") || !strings.Contains(got, tc.why) {
				t.Errorf("revertLinkProblem = %q, want %q", got, tc.why)
			}
		})
	}
}

func TestInverseActionKeda(t *testing.T) {
	keda := func(prev, desired int32) remediationv1alpha1.Action {
		return remediationv1alpha1.Action{Type: remediationv1alpha1.ActionAdjustKedaMaxReplicas,
			AdjustKedaMaxReplicas: &remediationv1alpha1.AdjustKedaMaxReplicas{PreviousValue: prev, DesiredValue: desired}}
	}
	if !inverseAction(keda(10, 15), keda(15, 10)) {
		t.Error("15 to 10 is the inverse of 10 to 15")
	}
	if inverseAction(keda(10, 15), keda(15, 11)) {
		t.Error("15 to 11 is not the inverse of 10 to 15")
	}
	if inverseAction(remediationv1alpha1.Action{Type: "NoSuchAction"}, remediationv1alpha1.Action{Type: "NoSuchAction"}) {
		t.Error("an unknown action type is never an inverse")
	}
}

// TestRevertApproval: an allowed revert keeps approval required even when the policy's decision would lower it
// (a decision that no longer reads spec.approvalRequired); other requests keep the decision.
func TestRevertApproval(t *testing.T) {
	_, revert := failedAndRevert()
	revert.Spec.ApprovalRequired = new(false)
	if got := revertApproval(revert, outcome{approval: new(false)}).approval; got == nil || !*got {
		t.Errorf("revert approval = %v, want true", got)
	}
	plain := fx.MemoryLimitRequest("default", "plain")
	plain.Spec.ApprovalRequired = new(false)
	if got := revertApproval(plain, outcome{approval: new(false)}).approval; got == nil || *got {
		t.Errorf("plain approval = %v, want false", got)
	}
	if got := revertApproval(revert, outcome{}).approval; got != nil {
		t.Errorf("approval of an outcome without one = %v, want none", got)
	}
}
