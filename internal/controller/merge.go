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
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// Reasons of condition Verified on a merged request that has no merge receipt (ADR-0030 sections 1 and 5).
const (
	// ReasonNoMergeReceipt: the merge could not be bound to a merge receipt (the Git host reported no merge
	// commit, or one that is not a full commit ID), so nothing after the merge can be checked against it. Set
	// Unknown in the status write that records the merge; the next reconcile ends the request Inconclusive.
	ReasonNoMergeReceipt = "NoMergeReceipt"
	// ReasonMergedBeforeVerification: the request was merged before the executor wrote merge receipts. It keeps
	// Merged, gets Verified Unknown with this reason once, and no verification starts.
	ReasonMergedBeforeVerification = "MergedBeforeVerification"
)

// recordMerge writes the merge commit of the recorded pull request and the merge receipt (status.mergeReceipt,
// ADR-0030 section 1) into the status, in the same write that ends the request in Merged, so delivery can later
// be checked against exactly that commit (design 8.2 step 8, #92). setPhase completes the receipt with the time
// of that write (mergedAt). Each caller vouches for cr in its own way:
//   - watchStep passes the recorded pull request, found by its number with the recorded branch, target branch
//     and head, merged with the head Shadewright pushed; recordHandoff passes the pull request it has just
//     recorded with the replacement's commit as head, merged with that commit.
//   - endMerged passes a pull request that carries this request's marker but was never recorded (its open's
//     answer or status write was lost, #524, #561), reached from the open step or from a stale plan's cleanup.
//     It records the pull request first, and calls recordMerge only after checkMerged found that the merged
//     head held exactly the planned change; a merge with a foreign change never reaches it.
//
// The receipt is checked first and the merge commit written only with it: status.pullRequest.mergeCommitSHA is
// set once and never changed, so a commit of a pull request that is not the recorded one (number, head, target
// branch) is never written. A commit that is not a full commit ID is not written either, and a V(1) log line
// says so. Without a receipt, for that or any other reason (no pull request recorded, cr not merged, the
// recorded plan repository or source missing), condition Verified is set Unknown with reason NoMergeReceipt in
// the same write, and the next reconcile ends the request Inconclusive (settleReceipt): the write that sets
// Merged always carries the receipt or that mark, never neither. A receipt already recorded is never replaced.
func recordMerge(ctx context.Context, rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest) {
	if rr.Status.MergeReceipt != nil {
		return
	}
	if !shaPattern.MatchString(cr.MergeCommit) {
		logf.FromContext(ctx).V(1).Info("Refused the merge commit of RemediationRequest, which is not a full commit ID",
			"remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", cr.Number, "length", len(cr.MergeCommit))
	}
	if receipt := mergeReceipt(rr, cr); receipt != nil {
		rr.Status.PullRequest.MergeCommitSHA = receipt.MergeCommitSHA
		rr.Status.MergeReceipt = receipt
		return
	}
	markNoMergeReceipt(rr)
}

// markNoMergeReceipt sets condition Verified Unknown with reason NoMergeReceipt in the status about to be written.
func markNoMergeReceipt(rr *remediationv1alpha1.RemediationRequest) {
	setVerified(rr, metav1.ConditionUnknown, ReasonNoMergeReceipt,
		"The merge cannot be bound to a merge receipt: the Git host reported no full merge commit ID for the "+
			"pull request, or it is not the recorded one; nothing after the merge can be checked")
}

// noReceiptMerge reports whether rr is Merged without a receipt and carries the NoMergeReceipt mark: the next
// reconcile ends it Inconclusive (settleReceipt), and until then it is read as that, never as a success.
func noReceiptMerge(rr *remediationv1alpha1.RemediationRequest) bool {
	if rr.Status.Phase != remediationv1alpha1.PhaseMerged || rr.Status.MergeReceipt != nil {
		return false
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionVerified)
	return c != nil && c.Reason == ReasonNoMergeReceipt
}

// mergeReceipt builds the receipt of the merge of cr from the recorded state and cr, without mergedAt (setPhase
// sets it in the status write that sets Merged), or returns nil when the merge cannot be bound to one: cr not
// merged, no pull request, plan repository or source recorded, a pull request that is not the recorded one
// (number, head, target branch), a merge commit that is not a full commit ID, or one other than a merge commit
// already recorded.
func mergeReceipt(rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest) *remediationv1alpha1.MergeReceipt {
	st := rr.Status
	pr := st.PullRequest
	switch {
	case cr.State != providers.ChangeRequestMerged, st.PlanRepository == nil, st.Source == nil, pr == nil,
		!shaPattern.MatchString(cr.MergeCommit), pr.MergeCommitSHA != "" && pr.MergeCommitSHA != cr.MergeCommit,
		cr.Number != pr.Number, cr.HeadSHA != pr.HeadSHA, !shaPattern.MatchString(pr.HeadSHA),
		cr.BaseBranch != string(st.Source.TargetBranch):
		return nil
	}
	return &remediationv1alpha1.MergeReceipt{
		Repository:     *st.PlanRepository,
		TargetBranch:   st.Source.TargetBranch,
		PullRequest:    pr.Number,
		HeadSHA:        pr.HeadSHA,
		MergeCommitSHA: cr.MergeCommit,
	}
}

// setVerified sets condition Verified in the status about to be written.
func setVerified(rr *remediationv1alpha1.RemediationRequest, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionVerified,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: rr.Generation,
	})
}

// findRecorded finds the recorded pull request again by its number, in the repository git is bound to (the
// plan's), and checks that it is from the request's branch to its target branch (ADR-0030 section 1). The look-up
// by branch would return the newest pull request of the branch, which after a merge may be another one (a
// supersede chain shares its branch), so the merge of the recorded one would never be recorded. A pull request
// with that number from another branch or to another target branch is not the recorded one: ErrNotFound, as
// when the host has none. Any other failure is returned as it is, for a retry.
func findRecorded(ctx context.Context, git providers.GitProvider, rr *remediationv1alpha1.RemediationRequest) (
	providers.ChangeRequest, error) {
	pr, src := rr.Status.PullRequest, rr.Status.Source
	if pr == nil || src == nil {
		return providers.ChangeRequest{}, errors.New("no pull request or source recorded")
	}
	cr, err := git.GetChangeRequest(ctx, pr.Number)
	if err != nil {
		return providers.ChangeRequest{}, err
	}
	if cr.Number != pr.Number || cr.HeadBranch != freshness.BranchName(rr) || cr.BaseBranch != string(src.TargetBranch) {
		return providers.ChangeRequest{}, fmt.Errorf("pull request #%d is not from the request's branch to its "+
			"target branch: %w", pr.Number, providers.ErrNotFound)
	}
	return cr, nil
}

// settleReceipt runs on a Merged request before the check after the merge, and reports whether it ended the
// request. It decides from the recorded state only:
//   - a receipt is recorded: nothing to do here (the phases after the merge are #681);
//   - no receipt, and Verified says NoMergeReceipt (written with the merge): the request ends Inconclusive with
//     that reason, in one status write;
//   - no receipt and no Verified condition: the request was merged before receipts existed, since every status
//     write that sets Merged now carries the receipt or the NoMergeReceipt mark (recordMerge, and setPhase for
//     any other path). It keeps Merged and gets Verified Unknown with reason MergedBeforeVerification, once; its
//     phase, and so the circuit breaker's reading of it, does not change;
//   - no receipt and Verified says MergedBeforeVerification: marked already, nothing to do;
//   - no receipt and Verified with any other reason: a status the executor never writes. Like an unknown
//     phase or answer elsewhere, it is never retried and never passes: the request ends Inconclusive with
//     reason NoMergeReceipt, and the message names the reason it found.
//
// A status write that fails is returned for a retry; the next reconcile reads the same state and decides the same.
func (r *RemediationRequestReconciler) settleReceipt(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	bool, error) {
	if rr.Status.Phase != remediationv1alpha1.PhaseMerged || rr.Status.MergeReceipt != nil {
		return false, nil
	}
	verified := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionVerified)
	switch {
	case verified == nil:
		setVerified(rr, metav1.ConditionUnknown, ReasonMergedBeforeVerification,
			"The request was merged before the executor recorded merge receipts; no verification is started")
		if err := r.Status().Update(ctx, rr); err != nil {
			return false, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
		}
		return false, nil
	case verified.Reason == ReasonMergedBeforeVerification:
		return false, nil
	case verified.Reason == ReasonNoMergeReceipt:
	default:
		logf.FromContext(ctx).Info("Found a merged RemediationRequest without a merge receipt and with an unknown "+
			"Verified reason", "remediationRequest", client.ObjectKeyFromObject(rr), "reason", verified.Reason)
		setVerified(rr, metav1.ConditionUnknown, ReasonNoMergeReceipt, fmt.Sprintf("The merge has no merge "+
			"receipt, and condition Verified had reason %q, which the executor does not write; nothing after the "+
			"merge can be checked", verified.Reason))
	}
	r.setPhase(rr, remediationv1alpha1.PhaseInconclusive)
	settleLock(rr)
	if err := r.Status().Update(ctx, rr); err != nil {
		return false, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	logf.FromContext(ctx).Info("Stopped RemediationRequest", "remediationRequest", client.ObjectKeyFromObject(rr),
		"phase", remediationv1alpha1.PhaseInconclusive, "reason", ReasonNoMergeReceipt)
	return true, nil
}

// recordApplied writes the time the executor first saw the merge commit applied (status.pullRequest
// .appliedObservedAt) and reports whether it changed the status. It writes only once, for a request with a
// recorded merge commit; a time already recorded is kept, never moved, whatever now is. The time is cut to
// whole seconds, as the API stores it, so the value read back is the one written.
//
// Planned use: the delivery check after the merge calls it when the outcome is first Applied, and the rollout
// check of #532 measures its deadline from the time recorded. Nothing calls it yet.
func recordApplied(rr *remediationv1alpha1.RemediationRequest, now time.Time) bool {
	pr := rr.Status.PullRequest
	if pr == nil || pr.MergeCommitSHA == "" || pr.AppliedObservedAt != nil || now.IsZero() {
		return false
	}
	t := metav1.NewTime(now.Truncate(time.Second))
	pr.AppliedObservedAt = &t
	return true
}
