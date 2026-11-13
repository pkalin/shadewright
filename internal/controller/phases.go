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
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/kedaready"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	verification "github.com/pkalin/shadewright/internal/verification/clearing"
	"github.com/pkalin/shadewright/internal/verification/rollout"
)

// The phases after the merge (ADR-0030 sections 2, 4 and 6, #681). A Merged request with a merge receipt moves
// to WaitingForDelivery on the next reconcile, with status.deadline at the end of the delivery window (one hour
// after mergeReceipt.mergedAt, extended by the time writes were paused). Delivery is asked against the receipt:
// its merge commit on its target branch, and the ancestry of a later applied commit of the receipt's repository,
// never the live settings' branch. Once the GitOps controller is seen applying the merge commit or a later commit
// that holds it, one status write records the applied time and the rollout fields, sets Delivered True and moves
// the request to Verifying with status.deadline at the timeout. Every reconcile in Verifying reads the target
// again: Configured, the rollout (RolledOut) and the template's freshness, and then judges Cleared from the
// verifier's report (clearing.go): Succeeded, Failed (OOMKilled at once; RolloutStuck, KedaNotReady or Restarted
// at the timeout) or Inconclusive.
//
// Every wait is measured on the executor's clock from a time it recorded itself (mergedAt, appliedObservedAt),
// never from a time of the Git host or of Flux. A clock that went back by more than mergeCommitClockSlack ends
// the wait at once as Inconclusive, so it never stretches a wait and never decides Failed or Succeeded. A failed
// read is retried, never a verdict, until the end of its step plus finalGrace, after which the request ends
// Inconclusive with a reason that says the evidence could not be read. A deadline past the merge never ends a
// request Expired. The kill switch gates writes only: while writes are paused every read still runs, and only
// the delivery window is extended by the paused time.

// Reasons of the conditions the phases after the merge write (ADR-0030 sections 2 to 5).
const (
	// ReasonDeliveryPending: Delivered Unknown while the executor waits for the GitOps controller to apply the
	// merge commit.
	ReasonDeliveryPending = "DeliveryPending"
	// ReasonMergeCommitApplied: Delivered True; the merge commit, or a later commit that holds it, is applied.
	ReasonMergeCommitApplied = "MergeCommitApplied"
	// ReasonDeliveryFailed: the GitOps controller failed on exactly the merge commit at the end of the delivery
	// window (Delivered False, Verified False, Failed).
	ReasonDeliveryFailed = "DeliveryFailed"
	// ReasonDeliveryBlocked: the GitOps controller failed on a later commit that holds the merge commit at the end
	// of the window; the failure cannot be pinned on this change (Delivered False, Inconclusive).
	ReasonDeliveryBlocked = "DeliveryBlocked"
	// ReasonDeliverySuspended: the GitOps controller's unit was suspended at the end of the window (Delivered
	// False, Inconclusive).
	ReasonDeliverySuspended = "DeliverySuspended"
	// ReasonDeliveryNotObserved: no applied revision that holds the merge commit was seen within the window
	// (Delivered False, Inconclusive).
	ReasonDeliveryNotObserved = rollout.ReasonDeliveryNotObserved
	// ReasonDeliveryNotChecked: the delivery could not be read to the end of the window and its grace, or the
	// executor's clock went back so the window could not be measured (Delivered Unknown, Inconclusive).
	ReasonDeliveryNotChecked = "DeliveryNotChecked"
	// ReasonTargetGone: the target is not found, in any phase after Merged (Inconclusive at once).
	ReasonTargetGone = "TargetGone"
	// ReasonTargetReplaced: the target exists with another UID, in any phase after Merged (Inconclusive at once).
	ReasonTargetReplaced = "TargetReplaced"
	// ReasonValueConfigured: Configured True; the live target holds the desired value.
	ReasonValueConfigured = "ValueConfigured"
	// ReasonValueChanged: Configured False; the live target does not hold the desired value, for example a later
	// commit overwrote it (Inconclusive at once).
	ReasonValueChanged = "ValueChanged"
	// ReasonClearingNotChecked: clearing is not checked for the target (a ScaledObject, whose symptom container
	// status does not show), or the timeout could not be measured (Inconclusive).
	ReasonClearingNotChecked = verification.ReasonClearingNotChecked
	// ReasonTargetNotChecked: the executor's own evidence on the target could not be read: the target could not
	// be read until the end of the timeout and its grace with no rollout verdict recorded, or the action's field
	// could not be read on it (Inconclusive).
	ReasonTargetNotChecked = "TargetNotChecked"
	// ReasonVerificationDisabled: the executor runs without --verify-outcomes and found the request in
	// WaitingForDelivery or Verifying, which nothing moves on with the flag off (Inconclusive at once).
	ReasonVerificationDisabled = "VerificationDisabled"
	// ReasonKedaNotReady: a ScaledObject target is not ready by the KedaReady rule at the timeout (Failed).
	ReasonKedaNotReady = verification.ReasonKedaNotReady
)

// DefaultVerificationTimeout is the default of spec.verification.timeout.
const DefaultVerificationTimeout = 10 * time.Minute

// DefaultNodeGrace is the default of the executor's --node-grace (ADR-0030 section 4).
const DefaultNodeGrace = verification.DefaultNodeGrace

// errNegativeNodeGrace refuses a negative NodeGrace at start.
var errNegativeNodeGrace = errors.New("the node grace (--node-grace) must not be negative")

// nodeGrace is NodeGrace, or verification.DefaultNodeGrace when it is zero or negative.
func (r *RemediationRequestReconciler) nodeGrace() time.Duration {
	if r.NodeGrace <= 0 {
		return verification.DefaultNodeGrace
	}
	return r.NodeGrace
}

// verificationTimeout is how long after appliedObservedAt a request may stay Verifying (ADR-0030 section 4): the
// larger of spec.verification.timeout (default 10 minutes) and the rollout deadline plus the effective stableFor
// plus the node grace. For a memory fix with defaults that is 21 minutes.
func (r *RemediationRequestReconciler) verificationTimeout(rr *remediationv1alpha1.RemediationRequest) time.Duration {
	requested := DefaultVerificationTimeout
	if t := rr.Spec.Verification.Timeout; t != nil && t.Duration > 0 {
		requested = t.Duration
	}
	stable := verification.StableFor(rr.Spec.Verification.StableFor, rr.Spec.Action.Type)
	return max(requested, rollout.ClampDeadline(r.RolloutDeadline)+stable+r.nodeGrace())
}

// clockWentBack reports whether start, a time the executor recorded on its own clock, lies more than
// mergeCommitClockSlack after now: the clock was set back, and a wait measured from start cannot be measured.
func clockWentBack(start, now time.Time) bool {
	return start.After(now.Add(mergeCommitClockSlack))
}

// afterReceipt runs the phases after the merge of a request with a merge receipt: Merged, WaitingForDelivery and
// Verifying. It returns when to look again and an error only for a failed read or status write, retried with
// backoff. A cancelled reconcile writes nothing.
func (r *RemediationRequestReconciler) afterReceipt(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	if rr.Status.MergeReceipt == nil || rr.Status.PullRequest == nil {
		// A phase after Merged without a receipt or a pull request is a status the executor never writes: never
		// retried and never passed.
		return reconcile.Result{}, r.endVerification(ctx, rr, remediationv1alpha1.PhaseInconclusive,
			ReasonNoMergeReceipt, fmt.Sprintf("Phase %s without a merge receipt; nothing after the merge can be "+
				"checked", rr.Status.Phase))
	}
	if rr.Status.MergeReceipt.MergedAt.IsZero() {
		// The API refuses a receipt without mergedAt; one read without it (written into an older schema, or a
		// decoding fault) would put the end of the delivery window in year 1.
		return reconcile.Result{}, r.endVerification(ctx, rr, remediationv1alpha1.PhaseInconclusive,
			ReasonNoMergeReceipt, "The merge receipt has no merge time, so it is unusable: no wait after the merge "+
				"can be measured")
	}
	now := r.now()
	stepCtx, cancel := bound(ctx, r.CheckTimeout, DefaultCheckTimeout)
	defer cancel()
	switch rr.Status.Phase {
	case remediationv1alpha1.PhaseMerged, remediationv1alpha1.PhaseWaitingForDelivery:
		if applied := rr.Status.PullRequest.AppliedObservedAt; applied != nil &&
			!now.Before(applied.Add(r.verificationTimeout(rr))) {
			// The applied time was recorded by the rollout report in Merged (#532, before #681 or with
			// --verify-outcomes off) and the timeout measured from it has passed: nothing was verified in time.
			return reconcile.Result{}, r.endVerification(ctx, rr, remediationv1alpha1.PhaseInconclusive,
				ReasonClearingNotChecked, "The verification timeout, measured from the applied time recorded "+
					"before outcome verification started for this request, had already passed; nothing was verified")
		}
		// The Incident's count at the merge is taken only in the write that set Merged (stop). A reading that
		// failed there stays absent: one taken later could describe a quiet period after the change arrived
		// (ADR-0030 section 4).
		if rr.Status.Phase == remediationv1alpha1.PhaseMerged {
			if err := r.enterDelivery(ctx, rr, now); err != nil {
				return reconcile.Result{}, err
			}
		}
		var fresh client.Object
		if rr.Status.PullRequest.AppliedObservedAt == nil {
			res, target, err := r.deliveryStep(ctx, stepCtx, rr, now)
			if target == nil || err != nil {
				return res, err
			}
			fresh = target
		}
		// The applied time was just recorded in rr, or, for a request whose rollout check ran while it was Merged
		// (#678), before: either way the move to Verifying is the next write.
		if err := r.enterVerifying(ctx, rr); err != nil {
			return reconcile.Result{}, err
		}
		return r.verifyStep(ctx, stepCtx, rr, now, fresh)
	case remediationv1alpha1.PhaseVerifying:
		return r.verifyStep(ctx, stepCtx, rr, now, nil)
	}
	return reconcile.Result{}, nil
}

// enterDelivery moves a Merged request with a receipt to WaitingForDelivery, with status.deadline at the end of
// the delivery window and Delivered Unknown, in one status write.
func (r *RemediationRequestReconciler) enterDelivery(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	now time.Time) error {
	mr := rr.Status.MergeReceipt
	r.setPhase(rr, remediationv1alpha1.PhaseWaitingForDelivery)
	rr.Status.Deadline = &metav1.Time{Time: deliveryWindowEnd(rr, mr.MergedAt.Time, now).Truncate(time.Second)}
	setCondition(rr, remediationv1alpha1.ConditionDelivered, metav1.ConditionUnknown, ReasonDeliveryPending,
		fmt.Sprintf("Waiting for the GitOps controller to apply merge commit %s of branch %s, or a later commit that "+
			"holds it", mr.MergeCommitSHA, mr.TargetBranch))
	if err := r.writeIf(ctx, rr, true); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("Waiting for the delivery of RemediationRequest", "remediationRequest",
		client.ObjectKeyFromObject(rr), "mergeCommit", mr.MergeCommitSHA, "deadline", rr.Status.Deadline.Time)
	return nil
}

// receiptAncestry answers whether one commit is in the history of another in the repository of the merge
// receipt, opened by the receipt's URL and installation, whatever the settings' entries name now. It only reads,
// so it is not gated by the kill switch (openRepository gates writes only).
type receiptAncestry struct {
	r       *RemediationRequestReconciler
	receipt *remediationv1alpha1.MergeReceipt
}

func (a receiptAncestry) IsAncestor(ctx context.Context, ancestor, commit string) (bool, error) {
	settings := &remediationv1alpha1.RemediationSettings{}
	switch err := a.r.getLive(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, settings); {
	case apierrors.IsNotFound(err):
		return false, errRepositoryGone
	case err != nil:
		return false, fmt.Errorf("get RemediationSettings %s: %w", remediationv1alpha1.SettingsName, err)
	}
	repo, ok := freshness.FindRepository(settings, &a.receipt.Repository)
	if !ok {
		return false, errRepositoryGone
	}
	git, err := a.r.openRepository(ctx, repo)
	switch {
	case providers.IsInvalidConfig(err):
		return false, freshness.ErrRepositoryUnusable
	case err != nil:
		return false, freshness.WrapProvider("open the repository", err)
	}
	checker, ok := git.(providers.AncestryChecker)
	if !ok {
		return false, errNoAncestry
	}
	return checker.IsAncestor(ctx, ancestor, commit)
}

// receiptQuery asks about the receipt's merge commit on the receipt's target branch.
func (r *RemediationRequestReconciler) receiptQuery(rr *remediationv1alpha1.RemediationRequest) deliveryQuery {
	mr := rr.Status.MergeReceipt
	return deliveryQuery{commit: mr.MergeCommitSHA, branch: string(mr.TargetBranch),
		ancestry: receiptAncestry{r: r, receipt: mr}}
}

// deliveryStep is one look at the delivery of a WaitingForDelivery request. When the merge commit is applied it
// records in rr (not written yet: the move to Verifying writes them) the applied time and the rollout fields and
// returns the target read right after. A target gone or replaced ends the request at once. Otherwise it waits
// within the delivery window and, at its end, ends the request by what the last read showed (endDelivery), or
// with DeliveryNotChecked when the delivery could not be read until the end of the grace period. The pause is
// tracked as before #681: time with writes paused extends the window, and a window that ended while a pause was
// first seen waits for the pause to end.
func (r *RemediationRequestReconciler) deliveryStep(ctx, stepCtx context.Context,
	rr *remediationv1alpha1.RemediationRequest, now time.Time) (reconcile.Result, client.Object, error) {
	start := rr.Status.MergeReceipt.MergedAt.Time
	paused, pauseErr := r.writesPaused(stepCtx)
	pauseErr = boundErr(ctx, stepCtx, "pause check", pauseErr)
	if ctx.Err() != nil {
		return reconcile.Result{}, nil, pauseErr
	}
	if pauseErr == nil && trackPause(rr, paused, now) {
		rr.Status.Deadline = &metav1.Time{Time: deliveryWindowEnd(rr, start, now).Truncate(time.Second)}
		if err := r.writeIf(ctx, rr, true); err != nil {
			return reconcile.Result{}, nil, err
		}
	}
	windowEnd := deliveryWindowEnd(rr, start, now)
	d, err := r.delivered(stepCtx, rr, r.receiptQuery(rr))
	err = boundErr(ctx, stepCtx, "delivery check", err)
	if ctx.Err() != nil {
		return reconcile.Result{}, nil, err
	}
	back := clockWentBack(start, now)
	switch {
	case errors.Is(err, errTargetReplaced):
		return reconcile.Result{}, nil, r.endTargetGone(ctx, rr, true)
	case errors.Is(err, errTargetGone):
		return reconcile.Result{}, nil, r.endTargetGone(ctx, rr, false)
	case errors.Is(err, errDeliveryMoved) && !back && now.Before(windowEnd):
		// Checked again at once within the window; past it, a delivery that keeps moving is a failed read like
		// any other, retried until the end of the grace period.
		return reconcile.Result{RequeueAfter: time.Second}, nil, nil
	}
	if err == nil && d.applied && recordApplied(rr, now) {
		r.recordCountAtApplied(stepCtx, rr)
		st := rolloutStatus(rr)
		st.TargetGeneration = max(d.target.GetGeneration(), 0)
		st.TemplateHash = rollout.TemplateHash(d.target)
		if isCommitID(d.commit) {
			st.AppliedRevision = d.commit
		}
		logf.FromContext(ctx).Info("Observed the merge commit of RemediationRequest applied",
			"remediationRequest", client.ObjectKeyFromObject(rr), "appliedObservedAt", now.Truncate(time.Second),
			"appliedRevision", st.AppliedRevision, "targetGeneration", st.TargetGeneration)
		return reconcile.Result{}, d.target, nil
	}
	if !back && now.Before(windowEnd) {
		if err != nil {
			return reconcile.Result{}, nil, r.deliveryReadFailed(ctx, rr, err)
		}
		return reconcile.Result{RequeueAfter: pollAfter(windowEnd.Sub(now))}, nil, nil
	}
	if !back && pauseErr == nil && paused {
		// The window ended before a pause was seen: the pause holds it open until it ends.
		return reconcile.Result{RequeueAfter: freshRecheckDelay}, nil, nil
	}
	if err == nil {
		// Not knowing whether writes are paused is a failed read, never a verdict.
		err = pauseErr
	}
	switch {
	case back:
		return reconcile.Result{}, nil, r.endDeliveryNotChecked(ctx, rr, "the executor's clock went back, so the "+
			"delivery window cannot be measured")
	case err == nil:
		return reconcile.Result{}, nil, r.endDelivery(ctx, rr, d)
	case now.After(windowEnd.Add(finalGrace)):
		logf.FromContext(ctx).Info("The delivery of RemediationRequest could not be checked",
			"remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error())
		why := "the executor log has the error"
		if cause := lastingCause(err); cause != "" {
			why = cause
		}
		return reconcile.Result{}, nil, r.endDeliveryNotChecked(ctx, rr, "it could not be read until the end of the "+
			"delivery window and its grace ("+why+")")
	}
	return reconcile.Result{}, nil, r.deliveryReadFailed(ctx, rr, err)
}

// lastingCause names, in a fixed text, an error of the delivery read that does not clear by itself while the
// settings or the executor stay as they are, and errDeliveryMoved; it is empty for any other error. Only this
// text goes into a condition, never the error, which may carry what the Git host or the API server said.
func lastingCause(err error) string {
	switch {
	case errors.Is(err, errRepositoryGone):
		return "the repository of the merge receipt is not in RemediationSettings any more"
	case errors.Is(err, freshness.ErrRepositoryUnusable):
		return "the settings of the merge receipt's repository cannot be used to open it"
	case errors.Is(err, errNoAncestry):
		return "the Git provider of the merge receipt's repository cannot check commit ancestry"
	case errors.Is(err, errNoDeliveryVerifier):
		return "the executor runs without a delivery verifier"
	case errors.Is(err, errDeliveryMoved):
		return "the GitOps controller kept moving to another commit while it was read"
	}
	return ""
}

// deliveryReadFailed returns err, a failed delivery read, for a retry with backoff. When it has a lasting cause
// (lastingCause, errDeliveryMoved aside), Delivered Unknown names it first, in a status write, so the cause is not
// only in the log.
func (r *RemediationRequestReconciler) deliveryReadFailed(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, err error) error {
	cause := lastingCause(err)
	if cause == "" || errors.Is(err, errDeliveryMoved) {
		return err
	}
	mr := rr.Status.MergeReceipt
	changed := setCondition(rr, remediationv1alpha1.ConditionDelivered, metav1.ConditionUnknown, ReasonDeliveryPending,
		fmt.Sprintf("Waiting for the GitOps controller to apply merge commit %s of branch %s, or a later commit that "+
			"holds it; the delivery cannot be read: %s", mr.MergeCommitSHA, mr.TargetBranch, cause))
	if werr := r.writeIf(ctx, rr, changed); werr != nil {
		return errors.Join(err, werr)
	}
	return err
}

// endDelivery ends a request whose delivery window ended without the merge commit seen applied, by what the
// last read of the GitOps controller's unit showed (ADR-0030 section 2): a failure of exactly the merge commit is
// Failed (DeliveryFailed), the one delivery result that counts against the change; a failure of a later commit
// that holds it is Inconclusive (DeliveryBlocked); a suspended unit Inconclusive (DeliverySuspended); anything
// else, a unit still on an older commit, one that applied a commit not holding the merge commit, or a target
// without the GitOps controller's labels, Inconclusive (DeliveryNotObserved).
func (r *RemediationRequestReconciler) endDelivery(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	d delivery) error {
	mr := rr.Status.MergeReceipt
	unit := "no Flux Kustomization (the target has no valid Kustomization labels)"
	if d.target != nil {
		if name, namespace, ok := kustomizationOf(d.target); ok {
			unit = "the Flux Kustomization " + namespace + "/" + name
		}
	}
	phase, reason := remediationv1alpha1.PhaseInconclusive, ReasonDeliveryNotObserved
	message := fmt.Sprintf("%s was not seen applying merge commit %s, or a later commit of branch %s that holds "+
		"it, within the delivery window. Check the GitOps controller", unit, mr.MergeCommitSHA, mr.TargetBranch)
	switch {
	case d.checked && d.outcome == providers.DeliveryFailed && d.status.AttemptedCommit == mr.MergeCommitSHA:
		phase, reason = remediationv1alpha1.PhaseFailed, ReasonDeliveryFailed
		message = fmt.Sprintf("%s failed to apply merge commit %s itself at the end of the delivery window; the "+
			"change does not apply", unit, mr.MergeCommitSHA)
	case d.checked && d.outcome == providers.DeliveryFailed:
		reason = ReasonDeliveryBlocked
		message = fmt.Sprintf("%s failed to apply commit %s, a later commit of branch %s that holds merge commit %s, "+
			"at the end of the delivery window; the failure cannot be pinned on this change", unit,
			d.status.AttemptedCommit, mr.TargetBranch, mr.MergeCommitSHA)
	case d.checked && d.status.Suspended:
		reason = ReasonDeliverySuspended
		message = fmt.Sprintf("%s was suspended at the end of the delivery window and did not apply merge commit %s",
			unit, mr.MergeCommitSHA)
	}
	delivered := metav1.Condition{Type: remediationv1alpha1.ConditionDelivered, Status: metav1.ConditionFalse,
		Reason: reason, Message: message, ObservedGeneration: rr.Generation}
	return r.endVerification(ctx, rr, phase, reason, message, delivered)
}

// endDeliveryNotChecked ends a request whose delivery could not be checked: Delivered Unknown and Inconclusive,
// reason DeliveryNotChecked, with why in the message.
func (r *RemediationRequestReconciler) endDeliveryNotChecked(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, why string) error {
	message := fmt.Sprintf("The delivery of merge commit %s was not checked: %s", rr.Status.MergeReceipt.MergeCommitSHA,
		why)
	delivered := metav1.Condition{Type: remediationv1alpha1.ConditionDelivered, Status: metav1.ConditionUnknown,
		Reason: ReasonDeliveryNotChecked, Message: message, ObservedGeneration: rr.Generation}
	return r.endVerification(ctx, rr, remediationv1alpha1.PhaseInconclusive, ReasonDeliveryNotChecked, message,
		delivered)
}

// endTargetGone ends a request whose target is gone (replaced false) or was replaced by an object with another
// UID: Inconclusive at once, in any phase after Merged, since there is nothing left to judge or revert.
func (r *RemediationRequestReconciler) endTargetGone(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	replaced bool) error {
	ref := rr.Spec.TargetRef
	reason, message := ReasonTargetGone, fmt.Sprintf("The target %s %s/%s is gone; there is nothing left to verify",
		ref.Kind, ref.Namespace, ref.Name)
	if replaced {
		reason, message = ReasonTargetReplaced, fmt.Sprintf("The target %s %s/%s was replaced by an object with "+
			"another UID; the change is not verified on it", ref.Kind, ref.Namespace, ref.Name)
	}
	return r.endVerification(ctx, rr, remediationv1alpha1.PhaseInconclusive, reason, message)
}

// enterVerifying moves a WaitingForDelivery request whose applied time is recorded in rr to Verifying, in one
// status write with that time and the rollout fields: Delivered True and status.deadline at the timeout, measured
// from appliedObservedAt (verificationTimeout).
func (r *RemediationRequestReconciler) enterVerifying(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	pr := rr.Status.PullRequest
	r.setPhase(rr, remediationv1alpha1.PhaseVerifying)
	rr.Status.Deadline = &metav1.Time{Time: pr.AppliedObservedAt.Add(r.verificationTimeout(rr))}
	message := "The merge commit " + rr.Status.MergeReceipt.MergeCommitSHA + " is applied"
	if st := rr.Status.Rollout; st != nil && st.AppliedRevision != "" && st.AppliedRevision != rr.Status.MergeReceipt.MergeCommitSHA {
		message = fmt.Sprintf("Commit %s of branch %s, which holds merge commit %s, is applied", st.AppliedRevision,
			rr.Status.MergeReceipt.TargetBranch, rr.Status.MergeReceipt.MergeCommitSHA)
	}
	setCondition(rr, remediationv1alpha1.ConditionDelivered, metav1.ConditionTrue, ReasonMergeCommitApplied, message)
	if err := r.writeIf(ctx, rr, true); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("Verifying RemediationRequest", "remediationRequest", client.ObjectKeyFromObject(rr),
		"deadline", rr.Status.Deadline.Time)
	return nil
}

// verifyStep is one check of a Verifying request, with the executor's own reads: the target, read again unless
// fresh is the one just read with the delivery, must be the request's (TargetGone, TargetReplaced at once); its
// field must hold the desired value (Configured; ValueChanged at once); and, for a Deployment or StatefulSet,
// its template must still be the one recorded (RolloutSuperseded at once) while RolledOut follows the rollout.
// These checks run on every reconcile, the last one included; clearStep then judges Cleared from the
// verifier's report and decides the end.
func (r *RemediationRequestReconciler) verifyStep(ctx, stepCtx context.Context,
	rr *remediationv1alpha1.RemediationRequest, now time.Time, fresh client.Object) (reconcile.Result, error) {
	pr, ref := rr.Status.PullRequest, rr.Spec.TargetRef
	if pr.AppliedObservedAt == nil || rr.Status.Deadline == nil {
		// A status the executor never writes: Verifying is entered in the write that records both.
		return reconcile.Result{}, r.endVerification(ctx, rr, remediationv1alpha1.PhaseInconclusive,
			ReasonClearingNotChecked, "Verifying without an applied time or a deadline; nothing can be verified")
	}
	appliedAt, deadline := pr.AppliedObservedAt.Time, rr.Status.Deadline.Time
	if clockWentBack(appliedAt, now) {
		return reconcile.Result{}, r.endVerification(ctx, rr, remediationv1alpha1.PhaseInconclusive,
			ReasonClearingNotChecked, "The executor's clock went back, so the verification timeout cannot be measured")
	}
	target := fresh
	if target == nil {
		target = newTarget(ref)
		var err error
		if target == nil {
			err = fmt.Errorf("get %s %s/%s: the API version %q cannot be parsed", ref.Kind, ref.Namespace, ref.Name,
				ref.APIVersion)
		} else {
			err = boundErr(ctx, stepCtx, "verification read", r.readTarget(stepCtx, ref, target))
		}
		if ctx.Err() != nil {
			return reconcile.Result{}, err
		}
		switch {
		case errors.Is(err, errTargetReplaced):
			return reconcile.Result{}, r.endTargetGone(ctx, rr, true)
		case errors.Is(err, errTargetGone):
			return reconcile.Result{}, r.endTargetGone(ctx, rr, false)
		case err != nil && !now.Before(deadline.Add(finalGrace)):
			logf.FromContext(ctx).Info("The target of RemediationRequest could not be read", "remediationRequest",
				client.ObjectKeyFromObject(rr), "error", err.Error())
			if c := stuckRecorded(rr); c != nil {
				// The rollout verdict recorded before the reads failed still stands.
				return reconcile.Result{}, r.endVerification(ctx, rr, remediationv1alpha1.PhaseFailed,
					rollout.ReasonRolloutStuck, "The rollout did not complete by the verification timeout: "+c.Message)
			}
			return reconcile.Result{}, r.endVerification(ctx, rr, remediationv1alpha1.PhaseInconclusive,
				ReasonTargetNotChecked, "The target could not be read until the end of the verification timeout "+
					"and its grace (the executor log has the error)")
		case err != nil:
			return reconcile.Result{}, err
		}
	}
	why, ok, err := configured(rr, target)
	if err != nil {
		// Evidence that cannot be read is not a changed value.
		logf.FromContext(ctx).Info("The action's field of RemediationRequest could not be read on its target",
			"remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error())
		return reconcile.Result{}, r.endVerification(ctx, rr, remediationv1alpha1.PhaseInconclusive,
			ReasonTargetNotChecked, why)
	}
	if !ok {
		cond := metav1.Condition{Type: remediationv1alpha1.ConditionConfigured, Status: metav1.ConditionFalse,
			Reason: ReasonValueChanged, Message: why, ObservedGeneration: rr.Generation}
		return reconcile.Result{}, r.endVerification(ctx, rr, remediationv1alpha1.PhaseInconclusive,
			ReasonValueChanged, why, cond)
	}
	changed := setCondition(rr, remediationv1alpha1.ConditionConfigured, metav1.ConditionTrue, ReasonValueConfigured,
		"The live target holds the desired value")
	var stuck *rollout.Result
	if rolloutTarget(ref) == nil {
		changed = setCondition(rr, rollout.ConditionRolledOut, metav1.ConditionUnknown, rollout.ReasonKindNotChecked,
			fmt.Sprintf("The rollout check covers Deployments and StatefulSets only; a %s is not checked", ref.Kind)) ||
			changed
	} else {
		superseded, result, ch := r.checkRollout(ctx, rr, target, appliedAt, now)
		if superseded != "" {
			cond := metav1.Condition{Type: rollout.ConditionRolledOut, Status: metav1.ConditionUnknown,
				Reason: rollout.ReasonRolloutSuperseded, Message: superseded, ObservedGeneration: rr.Generation}
			return reconcile.Result{}, r.endVerification(ctx, rr, remediationv1alpha1.PhaseInconclusive,
				rollout.ReasonRolloutSuperseded, superseded, cond)
		}
		changed = ch || changed
		stuck = result
	}
	return r.clearStep(ctx, stepCtx, rr, target, now, deadline, changed, stuck)
}

// stuckRecorded returns the RolledOut condition when it records the rollout stuck (False, RolloutStuck), else nil.
func stuckRecorded(rr *remediationv1alpha1.RemediationRequest) *metav1.Condition {
	c := meta.FindStatusCondition(rr.Status.Conditions, rollout.ConditionRolledOut)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != rollout.ReasonRolloutStuck {
		return nil
	}
	return c
}

// isScaledObject reports whether ref names a keda.sh/v1alpha1 ScaledObject, the one KEDA target whose readiness
// the KedaReady rule reads.
func isScaledObject(ref remediationv1alpha1.ObjectRef) bool {
	return ref.APIVersion == kedaready.APIVersion && ref.Kind == kedaready.KindScaledObject
}

// kedaState is the readiness of a ScaledObject target by the KedaReady rule (kedaready.Readiness, the rule the
// policy gate and the KEDA module use). An object that cannot be read as JSON is NotReady.
func kedaState(target client.Object) kedaready.State {
	if u, ok := target.(*unstructured.Unstructured); ok {
		return kedaready.Readiness(u.Object)
	}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(target)
	if err != nil {
		return kedaready.NotReady
	}
	return kedaready.Readiness(obj)
}

// checkRollout reads the rollout of a Verifying Deployment or StatefulSet into rr (not written). superseded is
// not empty, with why, when the target's template is no longer the one recorded with the applied time (its hash
// differs, or it names another revision than the one recorded) or Evaluate found a newer revision: the request
// then ends RolloutSuperseded. Otherwise it records the template revision once (recordRevision), and, until
// RolledOut is True, sets RolledOut from rollout.Evaluate; stuck is the result when this check first reports the
// rollout stuck, for its one Event. changed reports whether the status changed.
func (r *RemediationRequestReconciler) checkRollout(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	target client.Object, appliedAt, now time.Time) (superseded string, stuck *rollout.Result, changed bool) {
	st := rolloutStatus(rr)
	if st.TemplateHash != "" && rollout.TemplateHash(target) != st.TemplateHash {
		return "The target's pod template changed after the applied commit was recorded; this change is not " +
			"verified any more", nil, false
	}
	if rev, ok := rollout.Revision(target, 0); ok && st.TemplateRevision != "" && rev != st.TemplateRevision {
		return fmt.Sprintf("The target names template revision %q, not %q, the one recorded for the applied commit; "+
			"this change is not verified any more", rev, st.TemplateRevision), nil, false
	}
	changed, replaced := recordRevision(rr, target)
	if replaced {
		return fmt.Sprintf("The target's template changed again before its controller observed generation %d, the "+
			"one the applied commit produced; this change is not verified", st.TargetGeneration), nil, false
	}
	if meta.IsStatusConditionTrue(rr.Status.Conditions, rollout.ConditionRolledOut) {
		return "", nil, changed
	}
	result, err := rollout.Evaluate(target, st.TemplateRevision, appliedAt, now,
		rollout.ClampDeadline(r.RolloutDeadline))
	if err != nil {
		logf.FromContext(ctx).Info("The rollout of RemediationRequest could not be evaluated", "remediationRequest",
			client.ObjectKeyFromObject(rr), "error", err.Error())
		return "", nil, setCondition(rr, rollout.ConditionRolledOut, metav1.ConditionUnknown,
			rollout.ReasonRolloutNotDetermined, "The rollout cannot be evaluated") || changed
	}
	if result.State == rollout.Superseded {
		return result.Message, nil, false
	}
	previous := meta.FindStatusCondition(rr.Status.Conditions, rollout.ConditionRolledOut)
	cond := result.Condition(rr.Generation, now)
	if previous != nil && previous.Status == cond.Status && previous.Reason == cond.Reason {
		return "", nil, changed
	}
	meta.SetStatusCondition(&rr.Status.Conditions, cond)
	if result.State == rollout.Stuck {
		return "", &result, true
	}
	return "", nil, true
}

// configured reports whether the live target holds the desired value of the request's action, read with the
// field accessor of the source mapping, as the duplicate rule's changeInEffect does. When it does not, why says
// so in a fixed text: the live value is not copied into the status. err is set, with why, when the evidence
// cannot be read at all (the action's field, or the target as an object): that is no changed value.
func configured(rr *remediationv1alpha1.RemediationRequest, target client.Object) (why string, ok bool, err error) {
	field, err := sourcemap.FieldFor(rr.Spec.Action, rr.Spec.TargetRef)
	if err != nil {
		return "The action's field cannot be read on the target, so whether it holds the desired value was not " +
			"checked", false, fmt.Errorf("the field of action %s: %w", rr.Spec.Action.Type, err)
	}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(target)
	if err != nil {
		return "The live target cannot be read as an object, so whether it holds the desired value was not checked",
			false, fmt.Errorf("convert the target: %w", err)
	}
	desired, ok := sourcemap.DesiredValue(rr.Spec.Action)
	got, found := field.Get(obj)
	switch {
	case !ok:
		return "The action names no desired value to compare with", false, nil
	case !found:
		return "The live target has no value at the action's field: a later change removed it", false, nil
	case !field.Comparable(got) || !field.Equals(got, desired):
		return "The live target does not hold the desired value: a later commit or change overwrote it, so this " +
			"change never ran as merged", false, nil
	}
	return "", true, nil
}

// setCondition sets a condition in the status about to be written and reports whether its status, reason or
// message changed.
func setCondition(rr *remediationv1alpha1.RemediationRequest, typ string, status metav1.ConditionStatus,
	reason, message string) bool {
	previous := meta.FindStatusCondition(rr.Status.Conditions, typ)
	if previous != nil && previous.Status == status && previous.Reason == reason && previous.Message == message &&
		previous.ObservedGeneration == rr.Generation {
		return false
	}
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: typ, Status: status, Reason: reason,
		Message: message, ObservedGeneration: rr.Generation})
	return true
}

// endVerification ends a request after the merge in phase (Failed or Inconclusive) with reason, in one status
// write: the given conditions, Verified (False for Failed, Unknown otherwise) with reason and message, the phase
// and its breaker classification (setPhase), no deadline, and the target lock settled. One Event follows the
// write: Warning for Failed, Normal otherwise.
func (r *RemediationRequestReconciler) endVerification(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	phase remediationv1alpha1.RemediationPhase, reason, message string, conds ...metav1.Condition) error {
	if rr.Status.Phase == remediationv1alpha1.PhaseVerifying &&
		!slices.ContainsFunc(conds, func(c metav1.Condition) bool { return c.Type == remediationv1alpha1.ConditionCleared }) {
		// Every end from Verifying says what became of the clearing (ADR-0030 section 3).
		conds = append(conds, clearedAtEnd(rr, phase, reason, message))
	}
	for _, c := range conds {
		meta.SetStatusCondition(&rr.Status.Conditions, c)
	}
	status, eventType := metav1.ConditionUnknown, corev1.EventTypeNormal
	if phase == remediationv1alpha1.PhaseFailed {
		status, eventType = metav1.ConditionFalse, corev1.EventTypeWarning
	}
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: remediationv1alpha1.ConditionVerified,
		Status: status, Reason: reason, Message: message, ObservedGeneration: rr.Generation})
	r.setPhase(rr, phase)
	rr.Status.Deadline = nil
	settleLock(rr)
	if err := r.writeIf(ctx, rr, true); err != nil {
		return err
	}
	r.reportCounters.forget(client.ObjectKeyFromObject(rr))
	r.unwrittenResets.forget(client.ObjectKeyFromObject(rr))
	if r.Recorder != nil {
		r.Recorder.Event(rr, eventType, reason, message)
	}
	logf.FromContext(ctx).Info("Stopped RemediationRequest", "remediationRequest", client.ObjectKeyFromObject(rr),
		"phase", phase, "reason", reason)
	return nil
}
