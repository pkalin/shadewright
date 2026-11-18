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
	"fmt"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/pkalin/shadewright/api/v1alpha1"
)

// The ninth decision of 2026-11-18 on #771, point 5, as the tenth narrowed it: the release of the target lock from
// recorded status is main's rule (settleLock: prMayBeOpen and closedOnly), case by case, less the two stops whose
// merge did not deliver the plan as shown (MergedWithForeignChange, MergeContentUnreadable). mainReleasesFromStatus is that rule as origin/main
// holds it, kept here to compare the branch's verdict (settleRecordedMerge) with.
func mainReleasesFromStatus(rr *api.RemediationRequest, verify bool) bool {
	return holdsLock(rr) && !progresses(rr) && !mainPRMayBeOpen(rr) && !mainClosedOnly(rr) &&
		!heldForVerification(rr, verify)
}

// mainPRMayBeOpen is prMayBeOpen of origin/main.
func mainPRMayBeOpen(rr *api.RemediationRequest) bool {
	if rr.Status.PullRequest == nil {
		return !slices.Contains(successPhases, rr.Status.Phase) && !undecided(rr.Status.Phase) &&
			rr.Status.Phase != api.PhaseInconclusive
	}
	if slices.Contains(successPhases, rr.Status.Phase) || undecided(rr.Status.Phase) || rr.Status.Phase == api.PhaseInconclusive ||
		(rr.Status.Phase == api.PhaseSuperseded && !handedOff(rr)) || mainMergedAndEnded(rr) {
		return false
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPROpened)
	mainClosedReasons := map[string]bool{ReasonPullRequestClosed: true, ReasonChangeRequestClosed: true,
		ReasonChangeRequestMerged: true}
	return c == nil || !mainClosedReasons[c.Reason]
}

// mainClosedOnly is closedOnly of origin/main.
func mainClosedOnly(rr *api.RemediationRequest) bool {
	if rr.Status.PullRequest == nil || slices.Contains(successPhases, rr.Status.Phase) || undecided(rr.Status.Phase) ||
		rr.Status.Phase == api.PhaseInconclusive || mainMergedAndEnded(rr) {
		return false
	}
	if rr.Status.Phase == api.PhaseSuperseded && !handedOff(rr) {
		return true
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPROpened)
	return c != nil && (c.Reason == ReasonPullRequestClosed || c.Reason == ReasonChangeRequestClosed)
}

func mainMergedAndEnded(rr *api.RemediationRequest) bool {
	return rr.Status.Phase == api.PhaseFailed && rr.Status.MergeReceipt != nil
}

// TestRecordedMergeIsMainsRule builds every combination of phase, recorded pull request, reason of PullRequestOpened,
// reason of Approved, receipt, plan digest, handoff and verification, and checks that the branch releases the lock from
// recorded status exactly where main's settleLock did, less the two stops whose merge did not deliver the plan as shown
// (tenth decision of 2026-11-18 on #771, point 3): NeedsIntervention with MergedWithForeignChange or
// MergeContentUnreadable, which only a read of the branch releases. No more (a closed pull request needs the read of its
// branch) and no less (a Superseded request whose handoff is done and whose pull request is marked merged is released,
// point 4).
func TestRecordedMergeIsMainsRule(t *testing.T) {
	phases := []api.RemediationPhase{api.PhasePending, api.PhasePolicyChecked, api.PhasePRCreated,
		api.PhaseAwaitingApproval, api.PhaseAutoMergeQueued, api.PhaseMerged, api.PhaseWaitingForDelivery,
		api.PhaseVerifying, api.PhaseSucceeded, api.PhaseDenied, api.PhaseFailed, api.PhaseExpired, api.PhaseCancelled,
		api.PhaseSuperseded, api.PhaseNeedsIntervention, api.PhaseInconclusive}
	reasons := []string{"", ReasonPullRequestOpened, ReasonPullRequestClosed, ReasonChangeRequestClosed,
		ReasonChangeRequestMerged, ReasonHeadMoved}
	approvals := []string{"", ReasonMergedWithForeignChange, ReasonMergeContentUnreadable, ReasonMergeNotCheckable,
		ReasonGitForbidden}
	handoffs := []*api.OutgoingHandoff{nil, {Phase: api.HandoffDone, UID: "u"}, {Phase: api.HandoffIntent, UID: "u"}}
	r := &RemediationRequestReconciler{}
	n := 0
	for _, phase := range phases {
		for _, pr := range []bool{false, true} {
			for _, reason := range reasons {
				for _, approval := range approvals {
					for _, receipt := range []bool{false, true} {
						for _, digest := range []bool{false, true} {
							for _, handoff := range handoffs {
								for _, verify := range []bool{false, true} {
									rr := &api.RemediationRequest{}
									rr.Status.Phase, rr.Status.HoldsTarget = phase, new(true)
									if pr {
										rr.Status.PullRequest = &api.PullRequestStatus{Number: 7}
									}
									if reason != "" {
										meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
											Type: api.ConditionPROpened, Status: metav1.ConditionTrue, Reason: reason})
									}
									if approval != "" {
										meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
											Type: api.ConditionApproved, Status: metav1.ConditionFalse, Reason: approval})
									}
									if receipt {
										rr.Status.MergeReceipt = &api.MergeReceipt{}
									}
									if digest {
										rr.Status.PlanDigest = "sha256:" + fmt.Sprint(n)
									}
									if handoff != nil {
										rr.Status.Handoff = &api.Handoff{To: handoff.DeepCopy()}
									}
									r.VerifyOutcomes = verify
									_, got := r.settleRecordedMerge(rr)
									got = got && !progresses(rr)
									undelivered := phase == api.PhaseNeedsIntervention &&
										(approval == ReasonMergedWithForeignChange || approval == ReasonMergeContentUnreadable)
									if want := mainReleasesFromStatus(rr, verify) && !undelivered; got != want {
										t.Errorf("phase %s, pull request %t, PullRequestOpened %q, Approved %q, receipt %t, "+
											"digest %t, handoff %v, verify %t: the branch releases %t, main less the two "+
											"stops %t", phase, pr, reason, approval, receipt, digest, handoff, verify, got, want)
									}
									n++
								}
							}
						}
					}
				}
			}
		}
	}
	t.Logf("%d combinations", n)
}
