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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// A revert (ADR-0030 section 8, #96) is a request with spec.reverts: it undoes the change of a request that
// ended Failed with Verified False. The analyzer's deterministic revert proposer creates it; the analysis loop
// cannot (a model proposal has no reverts field), but both run as the analyzer, so RBAC cannot tell them apart
// and the executor's link check below is the control. The check runs before the policy check, once: a request
// whose link does not hold ends Denied with InvalidRevert before any Git write and is never treated as a revert.
// A valid revert then goes through the policy check, the freshness check, the target lock and the pull request
// like any request, always with approval required, and after the merge it is never checked for clearing: the
// verifier writes no report for it, Cleared stays Unknown with RevertApplied, and it ends Inconclusive with
// RevertApplied once rolled out, configured and available held for stableFor (clearRevertStep), never
// Succeeded.

// checkRevertLink is the revert link check of check: refused is true with the Denied outcome (InvalidRevert)
// when the request carries spec.reverts and the link does not hold. A failed read of the named request is an
// error, retried, never a refusal.
func (r *RemediationRequestReconciler) checkRevertLink(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	out outcome, refused bool, err error) {
	ref := rr.Spec.Reverts
	if ref == nil {
		return outcome{}, false, nil
	}
	deny := func(why string) (outcome, bool, error) {
		return denied(remediationv1alpha1.ReasonInvalidRevert, why+"; it is not treated as a revert"), true, nil
	}
	if rr.Name != string(remediationv1alpha1.RevertName(ref.UID)) {
		// The name derives from the failed request's UID, so a failed request has at most one revert.
		return deny("The request's name is not the one derived from the UID in spec.reverts")
	}
	failed := &remediationv1alpha1.RemediationRequest{}
	switch err := r.getLive(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: ref.Name}, failed); {
	case apierrors.IsNotFound(err):
		return deny("The request it reverts does not exist")
	case err != nil:
		return outcome{}, false, fmt.Errorf("get the RemediationRequest %s reverts: %w", rr.Name, err)
	}
	if why := revertLinkProblem(rr, failed); why != "" {
		return deny(why)
	}
	return outcome{}, false, nil
}

// revertLinkProblem says why rr is not a valid revert of failed, the request its spec.reverts names as read
// from the API server, or "" when it is: failed has the UID in spec.reverts, ended Failed with Verified False,
// is not a revert itself, has the same Incident and target, and rr's action is exactly the inverse of failed's.
func revertLinkProblem(rr, failed *remediationv1alpha1.RemediationRequest) string {
	ref := rr.Spec.Reverts
	verified := meta.FindStatusCondition(failed.Status.Conditions, remediationv1alpha1.ConditionVerified)
	switch {
	case ref == nil:
		return "The request has no spec.reverts"
	case failed.UID != types.UID(ref.UID):
		return "The request it reverts has another UID"
	case failed.Status.Phase != remediationv1alpha1.PhaseFailed:
		return "The request it reverts did not end Failed"
	case verified == nil || verified.Status != metav1.ConditionFalse:
		return "The request it reverts did not end with Verified False"
	case remediationv1alpha1.IsRevert(failed):
		return "The request it reverts is a revert itself, and a revert is never reverted"
	case failed.Spec.IncidentRef != rr.Spec.IncidentRef:
		return "The request it reverts names another Incident"
	case failed.Spec.TargetRef != rr.Spec.TargetRef:
		return "The request it reverts names another target"
	case !remediationv1alpha1.IsInverseAction(failed.Spec.Action, rr.Spec.Action):
		return "The action is not exactly the inverse of the reverted request's: the same type and container, " +
			"with previous and desired swapped"
	}
	return ""
}

// revertApproval keeps approval required on an allowed revert, whatever the policy decided. Through the API it
// changes nothing today: CEL requires approvalRequired: true on every revert the API server accepts, and
// policy.Decide ORs spec.approvalRequired into its decision. It guards the executor's own layer against a change
// in either (a policy package that stops reading the field, a rule that is relaxed), so that a revert is never
// merged without a person whatever the policy code says. TestRevertApproval covers it; an envtest cannot,
// since no revert with approvalRequired false reaches the executor.
func revertApproval(rr *remediationv1alpha1.RemediationRequest, o outcome) outcome {
	if remediationv1alpha1.IsRevert(rr) && o.approval != nil && !*o.approval {
		o.approval = new(true)
	}
	return o
}
