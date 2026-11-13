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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/verification/rollout"
)

// The check after the merge (#532, design 6 and 7.3): once a request is Merged, the executor asks the delivery
// verifier whether the Flux Kustomization that applies the target reports the merge commit (or a later commit of
// the target branch that holds it) applied, records the time it first sees that (status.pullRequest
// .appliedObservedAt, set once), and then reads the target's rollout status and sets the RolledOut condition:
// True when every pod runs the applied template, False with reason RolloutStuck when the rollout did not
// complete within the deadline measured from that time, with one core Event.
//
// It only reports (invariant 2): it never deletes, restarts or otherwise acts on a pod. It reads the target and
// the Kustomization with the get the executor already holds on them and asks the Git host, a read, whether a
// later applied commit holds the merge commit; it writes only the request's status and one Event (invariant 6).
// No Git write is made, so the kill switch has nothing to gate; the Git provider is opened through
// openPlanRepository, which refuses while writes are paused, so a paused executor also asks the Git host nothing
// and the check waits.
//
// A read that fails, a delivery not yet seen applied and a cancelled reconcile are never a verdict: the request
// keeps its status and is looked at again. The check is bounded in time and always ends with a verdict in
// RolledOut, after which status.rollout.ended is set and nothing more is read: True, RolloutStuck past the end
// of the check, DeliveryNotObserved, or Unknown with KindNotChecked (not a Deployment or StatefulSet),
// RolloutSuperseded (a newer template revision) or RolloutNotDetermined (gone or unreadable to the end).

// Timing of the check after the merge.
const (
	// deliveryWindow is how long after the merge (the request's move to Merged) the executor waits for the
	// delivery to be seen applied. Past it, a delivery checked and not seen ends the check with
	// DeliveryNotObserved; while writes are paused it waits.
	deliveryWindow = time.Hour
	// stuckWatch is how long after the deadline the executor keeps reading a rollout it reported stuck, so a
	// rollout a person unblocks turns RolledOut True.
	stuckWatch = time.Hour
	// afterMergePollInterval is how often the delivery and a rollout in progress are read.
	afterMergePollInterval = 30 * time.Second
	// stuckPollInterval is how often a rollout reported stuck is read again.
	stuckPollInterval = 2 * time.Minute
	// finalGrace is how long past the end of a step (the delivery window, or the deadline and stuckWatch) the
	// executor retries a read that fails before it ends the check with RolloutNotDetermined.
	finalGrace = 10 * time.Minute
)

// DeliveryVerifier says whether a delivery unit applied a merge commit merged into branch.
// The Flux adapter (internal/providers/delivery/flux) implements it.
type DeliveryVerifier interface {
	Verify(ctx context.Context, unit providers.DeliveryUnit, mergeCommit, branch string) (
		providers.DeliveryOutcome, providers.DeliveryStatus, error)
}

// errTargetGone means the target the request names is gone or was replaced (another UID).
var errTargetGone = errors.New("the target is gone or was replaced")

// errTargetReplaced means the object the request names exists with another UID: it was deleted and created
// again. It matches errTargetGone too, so a caller that does not tell them apart treats both alike.
var errTargetReplaced = fmt.Errorf("%w: an object of that name has another UID", errTargetGone)

// errDeliveryMoved means the GitOps controller applied another commit between its answer and the read of the
// target after it, or another Kustomization took the target over in between, so the target read may show what
// that commit or that Kustomization produced: nothing is recorded, the delivery is checked again at once.
var errDeliveryMoved = errors.New("the delivery moved to another commit while it was checked")

// errNoDeliveryVerifier means the executor runs without a delivery verifier (Delivery is nil), so the delivery
// of a merge commit cannot be read. It does not clear by itself.
var errNoDeliveryVerifier = errors.New("the executor has no delivery verifier")

// maxPaused bounds the paused time added to the delivery window, so a corrupt pausedSeconds cannot overflow it.
const maxPaused = 365 * 24 * time.Hour

// errNoAncestry means the Git provider of the plan's repository cannot say whether one commit is in the history
// of another.
var errNoAncestry = errors.New("the Git provider cannot check commit ancestry")

// planAncestry is the AncestryChecker of the plan's repository, opened only when the delivery check needs it:
// when the unit applied a later commit than the merge commit.
type planAncestry struct {
	r  *RemediationRequestReconciler
	rr *remediationv1alpha1.RemediationRequest
}

func (a planAncestry) IsAncestor(ctx context.Context, ancestor, commit string) (bool, error) {
	git, err := a.r.openPlanRepository(ctx, a.rr)
	if err != nil {
		return false, err
	}
	checker, ok := git.(providers.AncestryChecker)
	if !ok {
		return false, errNoAncestry
	}
	return checker.IsAncestor(ctx, ancestor, commit)
}

// rolloutTarget returns an empty Deployment or StatefulSet for the request's target, or nil when the target is
// another kind, which has no rollout to check.
func rolloutTarget(ref remediationv1alpha1.ObjectRef) client.Object {
	if ref.APIVersion != appsv1.SchemeGroupVersion.String() {
		return nil
	}
	switch ref.Kind {
	case "Deployment":
		return &appsv1.Deployment{}
	case "StatefulSet":
		return &appsv1.StatefulSet{}
	}
	return nil
}

// afterMerge runs the phases after the merge of a request with a merge receipt (afterReceipt, phases.go) when
// VerifyOutcomes is on, and otherwise the check after the merge of a Merged request; see the comment at the top
// of this file. It returns when to look again (zero when the check is over or does not apply) and an error only
// for a failed read or status write, which is retried with backoff.
func (r *RemediationRequestReconciler) afterMerge(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	switch rr.Status.Phase {
	case remediationv1alpha1.PhaseWaitingForDelivery, remediationv1alpha1.PhaseVerifying:
		if !r.VerifyOutcomes {
			// Off, nothing moves such a request on, and the breaker would wait for it for ever (a probe in it
			// still runs): it ends Inconclusive, read like any other Inconclusive end.
			return reconcile.Result{}, r.endVerification(ctx, rr, remediationv1alpha1.PhaseInconclusive,
				ReasonVerificationDisabled, fmt.Sprintf("The executor runs without --verify-outcomes, so the "+
					"outcome of a request in %s is not verified", rr.Status.Phase))
		}
		return r.afterReceipt(ctx, rr)
	case remediationv1alpha1.PhaseMerged:
		if r.VerifyOutcomes && rr.Status.MergeReceipt != nil {
			return r.afterReceipt(ctx, rr)
		}
	default:
		return reconcile.Result{}, nil
	}
	// With VerifyOutcomes off, and for a request merged before merge receipts existed (MergedBeforeVerification),
	// the request keeps Merged and its rollout is reported once (#532).
	if r.Delivery == nil || rr.Status.Source == nil || rr.Status.PullRequest == nil ||
		rr.Status.PullRequest.MergeCommitSHA == "" || rolloutEnded(rr) {
		return reconcile.Result{}, nil
	}
	now := r.now()
	if rolloutTarget(rr.Spec.TargetRef) == nil {
		// Said once, so a person does not wait for a report that never comes.
		return reconcile.Result{}, r.endRollout(ctx, rr, now, metav1.ConditionUnknown, rollout.ReasonKindNotChecked,
			fmt.Sprintf("The rollout check covers Deployments and StatefulSets only; a %s is not checked",
				rr.Spec.TargetRef.Kind), "")
	}
	stepCtx, cancel := bound(ctx, r.CheckTimeout, DefaultCheckTimeout)
	defer cancel()
	var fresh client.Object
	if rr.Status.PullRequest.AppliedObservedAt == nil {
		var (
			res reconcile.Result
			err error
		)
		if res, fresh, err = r.waitForDelivery(ctx, stepCtx, rr, now); fresh == nil {
			return res, err
		}
	}
	return r.judgeRollout(ctx, stepCtx, rr, now, fresh)
}

// waitForDelivery asks whether the merge commit is applied. When it is, it records in rr (not written yet; the
// rollout's first status write carries them all) now as the applied time, the commit applied, and the generation
// and template hash of the target read right after, and returns that target. Otherwise it waits for the delivery
// within the window after the merge and, past the window, ends the check: DeliveryNotObserved when the
// delivery was checked and not seen (or the target is gone), RolloutNotDetermined when it could not be checked
// until the end of the grace period. The pause is read directly on every look: the time writes are paused does
// not count against the window (status.rollout.pausedSince and pausedSeconds), and a window that ended before a
// pause was seen waits for the pause to end, since the Git host is not asked while it lasts.
func (r *RemediationRequestReconciler) waitForDelivery(ctx, stepCtx context.Context,
	rr *remediationv1alpha1.RemediationRequest, now time.Time) (reconcile.Result, client.Object, error) {
	start, err := r.deliveryStart(ctx, rr, now)
	if err != nil {
		return reconcile.Result{}, nil, err
	}
	paused, pauseErr := r.writesPaused(stepCtx)
	pauseErr = boundErr(ctx, stepCtx, "pause check", pauseErr)
	if ctx.Err() != nil {
		return reconcile.Result{}, nil, pauseErr
	}
	if pauseErr == nil && trackPause(rr, paused, now) {
		if err := r.writeIf(ctx, rr, true); err != nil {
			return reconcile.Result{}, nil, err
		}
	}
	windowEnd := deliveryWindowEnd(rr, start, now)
	d, err := r.delivered(stepCtx, rr, r.legacyQuery(rr))
	err = boundErr(ctx, stepCtx, "delivery check", err)
	if ctx.Err() != nil {
		return reconcile.Result{}, nil, err
	}
	if err == nil && d.applied && recordApplied(rr, now) {
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
	switch {
	case errors.Is(err, freshness.ErrPaused):
		// The Git host is not asked while writes are paused; the pause does not count against the window.
		return reconcile.Result{RequeueAfter: freshRecheckDelay}, nil, nil
	case errors.Is(err, errDeliveryMoved):
		return reconcile.Result{RequeueAfter: time.Second}, nil, nil
	}
	if now.Before(windowEnd) {
		if err != nil {
			return reconcile.Result{}, nil, err
		}
		return reconcile.Result{RequeueAfter: pollAfter(windowEnd.Sub(now))}, nil, nil
	}
	// The window ended before a pause was seen: the pause holds it open until it ends.
	if pauseErr == nil && paused {
		return reconcile.Result{RequeueAfter: freshRecheckDelay}, nil, nil
	}
	if err == nil {
		// Not knowing whether writes are paused is a failed read, never a verdict.
		err = pauseErr
	}
	switch {
	case err == nil:
		return reconcile.Result{}, nil, r.deliveryNotObserved(ctx, rr, now, d.target)
	case errors.Is(err, errTargetGone):
		return reconcile.Result{}, nil, r.deliveryNotObserved(ctx, rr, now, nil)
	case now.After(windowEnd.Add(finalGrace)):
		logf.FromContext(ctx).Info("The delivery of RemediationRequest could not be checked",
			"remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error())
		return reconcile.Result{}, nil, r.endRollout(ctx, rr, now, metav1.ConditionUnknown,
			rollout.ReasonRolloutNotDetermined, "The rollout was not checked: the delivery of merge commit "+
				rr.Status.PullRequest.MergeCommitSHA+" could not be checked until the end of the check (the "+
				"executor log has the error)", corev1.EventTypeWarning)
	}
	return reconcile.Result{}, nil, err
}

// trackPause records in rr when writes were first seen paused, and on the resume adds the paused time to
// pausedSeconds. It reports whether it changed the status.
func trackPause(rr *remediationv1alpha1.RemediationRequest, paused bool, now time.Time) bool {
	st := rolloutStatus(rr)
	switch {
	case paused && st.PausedSince == nil:
		st.PausedSince = new(metav1.NewTime(now.Truncate(time.Second)))
		return true
	case !paused && st.PausedSince != nil:
		st.PausedSeconds += max(int64(now.Sub(st.PausedSince.Time)/time.Second), 0)
		st.PausedSince = nil
		return true
	}
	return false
}

// deliveryWindowEnd returns when the delivery window that started at start ends: an hour of time not paused,
// so the paused seconds recorded and a pause still running extend it.
func deliveryWindowEnd(rr *remediationv1alpha1.RemediationRequest, start, now time.Time) time.Time {
	var paused time.Duration
	if st := rr.Status.Rollout; st != nil {
		paused = time.Duration(min(max(st.PausedSeconds, 0), int64(maxPaused/time.Second))) * time.Second
		if st.PausedSince != nil && now.After(st.PausedSince.Time) {
			paused += min(now.Sub(st.PausedSince.Time), maxPaused)
		}
	}
	return start.Add(deliveryWindow + paused)
}

// writesPaused reads RemediationSettings and reports whether writes are paused (spec.paused). Missing settings
// are not a pause.
func (r *RemediationRequestReconciler) writesPaused(ctx context.Context) (bool, error) {
	settings := &remediationv1alpha1.RemediationSettings{}
	switch err := r.getLive(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, settings); {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("get RemediationSettings %s: %w", remediationv1alpha1.SettingsName, err)
	}
	return freshness.Paused(settings), nil
}

// isCommitID reports whether s is a full lower-case hex SHA-1 object ID, the form status.rollout.appliedRevision
// accepts.
func isCommitID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// deliveryStart returns when the delivery window starts: the move to Merged, or, for a request without that
// time, status.rollout.waitStartedAt, recorded now (and written) the first time.
func (r *RemediationRequestReconciler) deliveryStart(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	now time.Time) (time.Time, error) {
	if t := rr.Status.LastPhaseTransitionTime; t != nil {
		return t.Time, nil
	}
	st := rolloutStatus(rr)
	if st.WaitStartedAt == nil {
		st.WaitStartedAt = new(metav1.NewTime(now.Truncate(time.Second)))
		if err := r.writeIf(ctx, rr, true); err != nil {
			return time.Time{}, err
		}
	}
	return st.WaitStartedAt.Time, nil
}

// judgeRollout reads the target after the delivery was seen applied (never before: an earlier read could show
// the old template fully rolled out), or uses fresh, the target read when the delivery was just seen applied
// (nil otherwise), records the template revision it judges, and reports. The check ends with a
// verdict: Complete, Superseded, Stuck once read after the end of the check (applied time, deadline and
// stuckWatch), or RolloutNotDetermined when the target is gone or cannot be read until the end of the grace
// period. Until then a failed read is retried and a stuck rollout is read again, so one a person unblocks turns
// True.
func (r *RemediationRequestReconciler) judgeRollout(ctx, stepCtx context.Context,
	rr *remediationv1alpha1.RemediationRequest, now time.Time, fresh client.Object) (reconcile.Result, error) {
	deadline := rollout.ClampDeadline(r.RolloutDeadline)
	appliedAt := rr.Status.PullRequest.AppliedObservedAt.Time
	end := appliedAt.Add(deadline + stuckWatch)
	recorded := fresh != nil
	target := fresh
	if target == nil {
		target = rolloutTarget(rr.Spec.TargetRef)
		readErr := boundErr(ctx, stepCtx, "rollout check", r.readTarget(stepCtx, rr.Spec.TargetRef, target))
		if ctx.Err() != nil {
			// Cancelled: no verdict and no write; the next reconcile sees the same.
			return reconcile.Result{}, readErr
		}
		if readErr != nil {
			if errors.Is(readErr, errTargetGone) || now.After(end.Add(finalGrace)) {
				return reconcile.Result{}, r.endRollout(ctx, rr, now, metav1.ConditionUnknown,
					rollout.ReasonRolloutNotDetermined, "The rollout was not judged: the target is gone or was "+
						"replaced, or could not be read until the end of the check", corev1.EventTypeWarning)
			}
			return reconcile.Result{}, readErr
		}
	}
	changed, replaced := recordRevision(rr, target)
	if replaced {
		return reconcile.Result{}, r.endRollout(ctx, rr, now, metav1.ConditionUnknown,
			rollout.ReasonRolloutSuperseded, fmt.Sprintf("The target's template changed again before its "+
				"controller observed generation %d, the one the applied commit produced; this rollout is not judged",
				rolloutStatus(rr).TargetGeneration), corev1.EventTypeNormal)
	}
	result, err := rollout.Evaluate(target, rolloutStatus(rr).TemplateRevision, appliedAt, now, deadline)
	if err != nil {
		return reconcile.Result{}, r.endRollout(ctx, rr, now, metav1.ConditionUnknown,
			rollout.ReasonRolloutNotDetermined, "The rollout cannot be evaluated", corev1.EventTypeWarning)
	}
	final := result.State == rollout.Complete || result.State == rollout.Superseded ||
		(result.State == rollout.Stuck && !now.Before(end))
	if final {
		rolloutStatus(rr).Ended = true
	}
	if err := r.report(ctx, rr, result, now, recorded || changed || final); err != nil {
		return reconcile.Result{}, err
	}
	switch {
	case final:
		return reconcile.Result{}, nil
	case result.State == rollout.Stuck:
		return reconcile.Result{RequeueAfter: max(min(stuckPollInterval, end.Sub(now)), time.Second)}, nil
	}
	return reconcile.Result{RequeueAfter: pollAfter(appliedAt.Add(deadline).Sub(now))}, nil
}

// recordRevision records, once, the template revision the target's controller rolls out for targetGeneration,
// the generation the applied commit produced (recorded with the applied time). It waits until the controller
// observed that generation and the target's own generation (a later generation's revision annotation can be
// written before its status). When the controller reports a later one first, that revision is ours only if the
// template is still the one recorded (a scale keeps it); otherwise replaced is true and no revision is recorded.
// changed reports whether it changed the status.
func recordRevision(rr *remediationv1alpha1.RemediationRequest, target client.Object) (changed, replaced bool) {
	st := rolloutStatus(rr)
	if st.TemplateRevision != "" {
		return false, false
	}
	observed := rollout.ObservedGeneration(target)
	switch {
	case observed < st.TargetGeneration:
		return false, false
	case target.GetGeneration() != observed:
		// A later generation waits to be observed, and the Deployment controller may already have written its
		// revision annotation: the revision named now may not be ours. Look again once it is observed.
		return false, false
	case observed > st.TargetGeneration && (st.TemplateHash == "" || rollout.TemplateHash(target) != st.TemplateHash):
		return false, true
	}
	if rev, ok := rollout.Revision(target, st.TargetGeneration); ok && len(rev) <= 253 {
		st.TemplateRevision = rev
		return true, false
	}
	return false, false
}

// rolloutStatus returns status.rollout, created when missing.
func rolloutStatus(rr *remediationv1alpha1.RemediationRequest) *remediationv1alpha1.RolloutStatus {
	if rr.Status.Rollout == nil {
		rr.Status.Rollout = &remediationv1alpha1.RolloutStatus{}
	}
	return rr.Status.Rollout
}

// rolloutEnded reports whether the check after the merge gave its final verdict: nothing is read after it.
func rolloutEnded(rr *remediationv1alpha1.RemediationRequest) bool {
	return (rr.Status.Rollout != nil && rr.Status.Rollout.Ended) ||
		meta.IsStatusConditionTrue(rr.Status.Conditions, rollout.ConditionRolledOut)
}

// endRollout gives the check its final verdict: RolledOut with status, reason and message, status.rollout.ended,
// one status write, and one Event of eventType unless it is empty.
func (r *RemediationRequestReconciler) endRollout(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	now time.Time, status metav1.ConditionStatus, reason, message, eventType string) error {
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: rollout.ConditionRolledOut, Status: status,
		Reason: reason, Message: message, ObservedGeneration: rr.Generation, LastTransitionTime: metav1.NewTime(now)})
	rolloutStatus(rr).Ended = true
	if err := r.writeIf(ctx, rr, true); err != nil {
		return err
	}
	if eventType != "" && r.Recorder != nil {
		r.Recorder.Event(rr, eventType, reason, message)
	}
	logf.FromContext(ctx).Info("Ended the rollout check of RemediationRequest",
		"remediationRequest", client.ObjectKeyFromObject(rr), "reason", reason)
	return nil
}

// deliveryNotObserved ends the check of a request whose delivery was checked after the window and not seen
// applied: RolledOut False with reason DeliveryNotObserved, naming the Kustomization (from the labels of target,
// nil when it is gone) and the merge commit, and one Warning Event.
func (r *RemediationRequestReconciler) deliveryNotObserved(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, now time.Time, target client.Object) error {
	unit := "no Flux Kustomization (the target is gone or was replaced)"
	if target != nil {
		unit = "no Flux Kustomization (the target has no valid Kustomization labels)"
		if name, namespace, ok := kustomizationOf(target); ok {
			unit = "the Flux Kustomization " + namespace + "/" + name
		}
	}
	return r.endRollout(ctx, rr, now, metav1.ConditionFalse, rollout.ReasonDeliveryNotObserved, fmt.Sprintf(
		"The rollout was not checked: %s was not seen applying merge commit %s, or a later commit of branch %s "+
			"that holds it, within %s of the merge. Check the GitOps controller", unit,
		rr.Status.PullRequest.MergeCommitSHA, rr.Status.Source.TargetBranch, deliveryWindow), corev1.EventTypeWarning)
}

// kustomizationOf returns the Kustomization the target's Flux labels name, when both are valid object names.
func kustomizationOf(target client.Object) (name, namespace string, ok bool) {
	labels := target.GetLabels()
	name, namespace = labels[sourcemap.LabelKustomizationName], labels[sourcemap.LabelKustomizationNamespace]
	if len(validation.IsDNS1123Subdomain(name)) > 0 || len(validation.IsDNS1123Label(namespace)) > 0 {
		return "", "", false
	}
	return name, namespace, true
}

// report writes the RolledOut condition when its status or reason changes, or the status when the applied time
// was just recorded, and records one Event on the write that first reports the rollout stuck.
func (r *RemediationRequestReconciler) report(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	result rollout.Result, now time.Time, recorded bool) error {
	previous := meta.FindStatusCondition(rr.Status.Conditions, rollout.ConditionRolledOut)
	cond := result.Condition(rr.Generation, now)
	changed := previous == nil || previous.Status != cond.Status || previous.Reason != cond.Reason
	if changed {
		meta.SetStatusCondition(&rr.Status.Conditions, cond)
	}
	if err := r.writeIf(ctx, rr, changed || recorded); err != nil {
		return err
	}
	if !changed || result.State == rollout.Progressing {
		return nil
	}
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr))
	switch result.State {
	case rollout.Complete:
		log.Info("Observed the rollout of RemediationRequest complete")
		return nil
	case rollout.Superseded:
		if r.Recorder != nil {
			r.Recorder.Event(rr, corev1.EventTypeNormal, rollout.ReasonRolloutSuperseded, result.Message)
		}
		log.Info("Ended the rollout check of RemediationRequest: a newer template revision")
		return nil
	}
	if r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeWarning, rollout.ReasonRolloutStuck, result.Message)
	}
	log.Info("Reported the rollout of RemediationRequest stuck", "oldPods", result.OldPods)
	return nil
}

// writeIf writes the request's status when write is true.
func (r *RemediationRequestReconciler) writeIf(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	write bool) error {
	if !write {
		return nil
	}
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	return nil
}

// delivery is what delivered found: whether the merge commit is applied, the commit the unit applied, the
// target (read again after an Applied answer, so it shows what the applied commit produced; nil when the first
// read failed), and, when the unit was read (checked), its outcome and status, so a caller can tell a failure
// of the merge commit itself from one of a later commit, and a suspended unit.
type delivery struct {
	applied bool
	commit  string
	target  client.Object
	checked bool
	outcome providers.DeliveryOutcome
	status  providers.DeliveryStatus
}

// deliveryQuery is what delivered asks about: the merge commit, the branch it was merged into and the Git host
// that answers whether a later commit holds it. A request with a merge receipt asks about the receipt's
// (receiptQuery); one merged before receipts existed about its recorded pull request and source (legacyQuery).
type deliveryQuery struct {
	commit, branch string
	ancestry       providers.AncestryChecker
}

// legacyQuery is the question of a request merged before receipts existed: its recorded merge commit and target
// branch, and the plan's repository opened through the live settings.
func (r *RemediationRequestReconciler) legacyQuery(rr *remediationv1alpha1.RemediationRequest) deliveryQuery {
	return deliveryQuery{commit: rr.Status.PullRequest.MergeCommitSHA, branch: string(rr.Status.Source.TargetBranch),
		ancestry: planAncestry{r: r, rr: rr}}
}

// delivered reports whether the Flux Kustomization named by the target's labels applied the merge commit, or a
// later commit of the target branch that holds it. Every outcome but Applied is not applied: Pending, Failed
// (Flux may still retry; the caller decides at the end of the window), a target without Kustomization labels.
// A read that fails is an error.
func (r *RemediationRequestReconciler) delivered(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	q deliveryQuery) (delivery, error) {
	ref := rr.Spec.TargetRef
	target := newTarget(ref)
	if target == nil {
		return delivery{}, fmt.Errorf("get %s %s/%s: the API version %q cannot be parsed", ref.Kind, ref.Namespace,
			ref.Name, ref.APIVersion)
	}
	if err := r.readTarget(ctx, ref, target); err != nil {
		return delivery{}, err
	}
	name, namespace, ok := kustomizationOf(target)
	if !ok {
		return delivery{target: target}, nil
	}
	if r.Delivery == nil {
		return delivery{target: target}, errNoDeliveryVerifier
	}
	verifier, err := r.Delivery(q.ancestry)
	if err != nil {
		return delivery{target: target}, fmt.Errorf("build the delivery verifier: %w", err)
	}
	unit := providers.DeliveryUnit{Kind: "Kustomization", Namespace: namespace, Name: name}
	outcome, status, err := verifier.Verify(ctx, unit, q.commit, q.branch)
	if err != nil {
		return delivery{target: target}, fmt.Errorf("verify the delivery of the merge commit: %w", err)
	}
	if outcome != providers.DeliveryApplied {
		return delivery{target: target, checked: true, outcome: outcome, status: status}, nil
	}
	// The first read may predate the apply Flux just reported: read the target again, so the generation
	// recorded is the one the applied commit produced. Then ask Flux again: had it applied another commit in
	// between, the target could show that commit's template, so nothing is recorded and the delivery is checked
	// again.
	after := newTarget(ref)
	if err := r.readTarget(ctx, ref, after); err != nil {
		return delivery{target: target}, err
	}
	// Another Kustomization may have taken the target over in between: its template is not ours, whatever
	// the Kustomization of the first read reports.
	if n2, ns2, ok2 := kustomizationOf(after); !ok2 || n2 != name || ns2 != namespace {
		return delivery{target: target}, errDeliveryMoved
	}
	again, status2, err := verifier.Verify(ctx, unit, q.commit, q.branch)
	if err != nil {
		return delivery{target: target}, fmt.Errorf("verify the delivery of the merge commit again: %w", err)
	}
	if again != providers.DeliveryApplied || status2.AppliedCommit != status.AppliedCommit {
		return delivery{target: target}, errDeliveryMoved
	}
	return delivery{applied: true, commit: status.AppliedCommit, target: after, checked: true, outcome: again,
		status: status2}, nil
}

// newTarget returns an empty object to read the request's target into: a Deployment or StatefulSet
// (rolloutTarget), or an unstructured object of the target's kind, read with the get the executor holds on the
// target kinds; nil when the API version cannot be parsed.
func newTarget(ref remediationv1alpha1.ObjectRef) client.Object {
	if t := rolloutTarget(ref); t != nil {
		return t
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gv.WithKind(ref.Kind))
	return u
}

// readTarget reads the target into obj from the API server and checks that it is the object the request names
// (its UID, when the request names one). A missing or replaced target is an error, retried, never a verdict.
func (r *RemediationRequestReconciler) readTarget(ctx context.Context, ref remediationv1alpha1.ObjectRef,
	obj client.Object) error {
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, obj)
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("get %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, errTargetGone)
	case err != nil:
		return fmt.Errorf("get %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, err)
	case ref.UID != "" && string(obj.GetUID()) != string(ref.UID):
		return fmt.Errorf("get %s %s/%s: %w", ref.Kind, ref.Namespace, ref.Name, errTargetReplaced)
	}
	return nil
}

// pollAfter returns the poll interval, or left when that is shorter, and never less than a second.
func pollAfter(left time.Duration) time.Duration {
	return max(min(afterMergePollInterval, left), time.Second)
}

// earlier returns the result that looks again sooner: a zero RequeueAfter means not again.
func earlier(a, b reconcile.Result) reconcile.Result {
	switch {
	case a.RequeueAfter <= 0:
		return b
	case b.RequeueAfter <= 0:
		return a
	case b.RequeueAfter < a.RequeueAfter:
		return b
	}
	return a
}
