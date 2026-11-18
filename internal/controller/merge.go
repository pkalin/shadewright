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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
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
	var branch remediationv1alpha1.GitBranch
	if rr.Status.Source != nil {
		branch = rr.Status.Source.TargetBranch
	}
	recordMergeInto(ctx, rr, cr, branch)
}

// recordMergeInto is recordMerge for a merge into branch: the recorded target branch (recordMerge), or, for a pull
// request a person moved to the target branch the settings name after a change, that branch (#771). The receipt
// then names branch, so delivery is checked on the branch the merge went into.
func recordMergeInto(ctx context.Context, rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest,
	branch remediationv1alpha1.GitBranch) {
	if rr.Status.MergeReceipt != nil {
		return
	}
	if !shaPattern.MatchString(cr.MergeCommit) {
		logf.FromContext(ctx).V(1).Info("Refused the merge commit of RemediationRequest, which is not a full commit ID",
			"remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", cr.Number, "length", len(cr.MergeCommit))
	}
	if receipt := mergeReceipt(rr, cr, branch); receipt != nil {
		rr.Status.PullRequest.MergeCommitSHA = receipt.MergeCommitSHA
		rr.Status.MergeReceipt = receipt
		return
	}
	markNoMergeReceipt(rr)
}

// errMergeCheck marks a pull request seen merged whose merge was not recorded because the check of the plan at the
// merge (checkAtMerge) did not answer, or because a later step of the same look failed (the wait for the merge
// commit, the write that sets Merged). The look at the pull request is retried; the caller never goes on to a
// check that reads Git, since a merged change makes the plan look stale there. watchPullRequest keeps it on the
// error when the step's deadline or the reconcile's cancellation replaces the error (boundErr).
var errMergeCheck = errors.New("the pull request was merged, and its merge is not recorded yet")

// checkAtMerge runs the reduced freshness check of ADR-0041 section 2 (freshness.Checker.CheckAtMerge, #737) on a
// request whose pull request was seen merged, right before the caller records the merge, in every path that does
// (watchStep, endMerged, recordHandoff). The periodic check marks a stale plan only every freshRecheckInterval, so a
// person who merges after a change of the policy, the settings or the target and before that check would
// otherwise end a stale plan as a fresh one. It reads the API server only, compares the recorded plan with what
// it holds now, and compares no timestamp of two sources.
//
// A stale plan gets the stale mark (PlanFresh False with the stale reason) in rr, not written: the caller's write
// that sets Merged carries it, and capStaleSuccess caps the good end as for a stale plan Shadewright could not
// close (#199). An input that can never be checked (no plan digest or source, a malformed target apiVersion) is
// such a verdict too (freshness.CheckAtMerge). A request that already carries the mark keeps it, unchecked.
//
// A read that fails, or a context that ended, is retried within the wait of the merge commit (mergeCommitWait,
// from status.mergeCommitAwaitedSince, which the first failure records when awaitMergeCommit did not): until the
// wait is over it returns the error wrapped in errMergeCheck with nothing set in rr; once it is over, the plan
// was not shown fresh, and it gets the mark with reason PlanChanged and a Warning Event, so the merge is recorded,
// the target lock goes, and the outcome is capped. A failed write of the start is returned wrapped the same way.
// It is never taken as fresh. The decision reads only the recorded start and the executor's own clock. The reads
// run within half of what is left of ctx (mergeCheckContext), so a check that runs out of time still records
// the start and lets the caller write, in every path.
func (r *RemediationRequestReconciler) checkAtMerge(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest) error {
	if staleMark(rr) != nil || markedStale(rr) {
		// Marked already (a stale plan, or not shown fresh after the wait for the merge): nothing to check.
		return nil
	}
	checkCtx, cancel := r.mergeCheckContext(ctx)
	err := freshness.Checker{Reader: r.reader()}.CheckAtMerge(checkCtx, rr)
	cancel()
	if err == nil {
		return nil
	}
	staleErr, ok := errors.AsType[*freshness.StaleError](err)
	if !ok {
		return r.mergeCheckFailed(ctx, rr, cr, err)
	}
	markStaleAtMerge(rr, staleErr.Reason, "The plan was stale when the merge was seen: "+staleErr.Message)
	logf.FromContext(ctx).Info("Found the plan of RemediationRequest stale at the merge",
		"remediationRequest", client.ObjectKeyFromObject(rr), "reason", staleErr.Reason)
	return nil
}

// mergeCheckContext is the context of the reads of the check at the merge: it ends halfway to the deadline of ctx
// (or after WriteTimeout when ctx has none), so a check that runs out of time leaves ctx the other half for the
// writes that follow in every caller: the start of the wait (mergeCheckFailed), and the status write that records
// the pull request or the merge. A check that always runs out of time is so bounded by the wait like any other
// failure. A cancelled ctx cancels it too, and then nothing is written.
func (r *RemediationRequestReconciler) mergeCheckContext(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	}
	return context.WithTimeout(ctx, time.Until(deadline)/2)
}

// mergeCheckFailed handles a check at the merge of cr that did not answer (checkAtMerge). It records the wait for
// the merge (its entry in status.gitWaits, startMergeWait) when there is none and the context still allows a
// write, as the check of any kind of failure is bounded by that wait:
//   - a failed Git read is one of the wait (readOfMergeCheck), returned wrapped in errMergeCheck for a retry: the
//     check at the start of a reconcile past its end records the merge with the plan not shown fresh, from the
//     entry alone (endPassedWait, endMergeWait). Nothing here looks at the end of that wait;
//   - any other failure (a Kubernetes read, a timeout of one) is no Git wait and goes to the error backoff: it is
//     returned wrapped in errMergeCheck while the wait lasts, and once it is over the plan is marked not shown fresh
//     here (markNotShownFresh) and nil is returned, so the merge is recorded.
func (r *RemediationRequestReconciler) mergeCheckFailed(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest, checkErr error) error {
	if gitWait(rr, waitMergeCheck) == nil && ctx.Err() == nil {
		if err := r.startMergeWait(ctx, rr, cr, "check the plan at the merge"); err != nil {
			return fmt.Errorf("%w: %w (%w)", errMergeCheck, checkErr, err)
		}
	}
	if gitReadFailure(checkErr) {
		return fmt.Errorf("%w: %w", errMergeCheck, readOfMergeCheck(cr, checkErr))
	}
	if gitWait(rr, waitMergeCheck) == nil || mergeWaitRemaining(rr, r.now()) > 0 {
		return fmt.Errorf("%w: %w", errMergeCheck, checkErr)
	}
	markNotShownFresh(rr, checkErr.Error())
	logf.FromContext(ctx).Error(checkErr, "Recorded the merge of RemediationRequest with a plan not shown fresh: "+
		"the check at the merge did not answer within the wait", "remediationRequest", client.ObjectKeyFromObject(rr))
	if r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeWarning, ReasonStalePlanMerged, notShownFreshMessage(checkErr.Error()))
	}
	return nil
}

// notShownFreshMessage is the PlanFresh message of a plan whose check at the merge did not answer within the wait
// for the merge, why saying how it failed.
func notShownFreshMessage(why string) string {
	return "The plan could not be checked at the merge within the wait for the merge (" + why +
		"), so it is not shown fresh"
}

// markNotShownFresh marks the plan of rr not shown fresh at the merge (PlanChanged) in the status about to be
// written: the check at the merge did not answer within the wait for the merge (waitMergeCheck).
func markNotShownFresh(rr *remediationv1alpha1.RemediationRequest, why string) {
	markStaleAtMerge(rr, freshness.ReasonPlanChanged, notShownFreshMessage(why))
}

// markStaleAtMerge sets the stale mark (PlanFresh False with a stale reason) in the status about to be written.
func markStaleAtMerge(rr *remediationv1alpha1.RemediationRequest, reason, message string) {
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionPlanFresh,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: rr.Generation,
	})
}

// markNoMergeReceipt sets condition Verified Unknown with reason NoMergeReceipt in the status about to be written.
func markNoMergeReceipt(rr *remediationv1alpha1.RemediationRequest) {
	setVerifiedUnknown(rr, ReasonNoMergeReceipt,
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
// (number, head), a merge into another branch than branch, a merge commit that is not a full commit ID, or one
// other than a merge commit already recorded.
func mergeReceipt(rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest,
	branch remediationv1alpha1.GitBranch) *remediationv1alpha1.MergeReceipt {
	st := rr.Status
	pr := st.PullRequest
	switch {
	case cr.State != providers.ChangeRequestMerged, st.PlanRepository == nil, st.Source == nil, pr == nil,
		!shaPattern.MatchString(cr.MergeCommit), pr.MergeCommitSHA != "" && pr.MergeCommitSHA != cr.MergeCommit,
		cr.Number != pr.Number, cr.HeadSHA != pr.HeadSHA, !shaPattern.MatchString(pr.HeadSHA),
		branch == "", cr.BaseBranch != string(branch):
		return nil
	}
	return &remediationv1alpha1.MergeReceipt{
		Repository:     *st.PlanRepository,
		TargetBranch:   branch,
		PullRequest:    pr.Number,
		HeadSHA:        pr.HeadSHA,
		MergeCommitSHA: cr.MergeCommit,
	}
}

// mergeCommitWait is how long the executor waits, on its own clock, for the Git host to report the merge commit
// of a pull request it saw merged without one, before it records the merge without a receipt. GitHub computes
// the merge commit after the merge, so for a moment a merged pull request may have none.
const mergeCommitWait = 5 * time.Minute

// errMergeCommitPending is returned by watchStep for a merged pull request whose merge commit the executor still
// waits for (awaitMergeCommit): nothing is decided, and the request keeps its phase.
var errMergeCommitPending = errors.New("waiting for the Git host to report the merge commit")

// awaitMergeCommit returns how much longer to wait for the merge commit of cr, merged without a full merge
// commit ID, before its merge is recorded anyway; zero when cr is not merged, has one, or the wait is over.
// The wait starts when the executor first sees that state: it records the wait as an entry of status.gitWaits
// (startMergeWait), with its start on its own clock and its end, so the start is kept over a restart and never
// moved (a validation rule), and the check at the start of a reconcile past its end records the merge without a
// receipt from the entry alone, also while the reads fail (endMergeWait). It never reads a time of the Git host. A
// failed write is returned for a retry and decides nothing. A clock that went back never makes the whole wait
// longer than mergeCommitWait plus mergeCommitClockSlack (mergeWaitRemaining).
func (r *RemediationRequestReconciler) awaitMergeCommit(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest) (time.Duration, error) {
	if cr.State != providers.ChangeRequestMerged || shaPattern.MatchString(cr.MergeCommit) {
		return 0, nil
	}
	if gitWait(rr, waitMergeCheck) != nil {
		return mergeWaitRemaining(rr, r.now()), nil
	}
	if err := r.startMergeWait(ctx, rr, cr, "wait for the merge commit"); err != nil {
		return 0, err
	}
	logf.FromContext(ctx).Info("Waiting for the merge commit of RemediationRequest", "remediationRequest",
		client.ObjectKeyFromObject(rr), "pullRequest", cr.Number, "wait", mergeCommitWait)
	return mergeWaitRemaining(rr, r.now()), nil
}

// startMergeWait records the wait for a merge of cr seen but not recorded (the merge commit, or the check of the plan
// at the merge, op naming which), on the executor's own clock: its entry in status.gitWaits (wholeWait), with the
// pull request and its end, and status.mergeCommitAwaitedSince when unset (kept as before the entry existed; the
// entry's end counts from it when it records an earlier start). Both go in one patch with the resourceVersion of rr,
// so a copy read earlier than the server's is refused as a conflict, and rr takes only these fields and the new
// resourceVersion: a caller's unwritten changes to rr stay for its own write. A failed write leaves rr as it was
// and is returned for a retry.
func (r *RemediationRequestReconciler) startMergeWait(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest, op string) error {
	now := r.now()
	base := rr.DeepCopy()
	upd := rr.DeepCopy()
	if upd.Status.MergeCommitAwaitedSince == nil {
		upd.Status.MergeCommitAwaitedSince = &metav1.Time{Time: now.Truncate(time.Second)}
	}
	w, ok := wholeWait(upd, remediationv1alpha1.GitWait{Kind: remediationv1alpha1.GitWaitMergeCheck,
		PullRequest: waitPullRequest(cr)}, op, now)
	if ok && gitWait(upd, waitMergeCheck) == nil {
		upd.Status.GitWaits = append(upd.Status.GitWaits, w)
		gitWaitSummary(upd, now)
	}
	if err := r.Status().Patch(ctx, upd, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("record the wait for the merge of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	rr.Status.MergeCommitAwaitedSince = upd.Status.MergeCommitAwaitedSince
	rr.Status.GitWaits = upd.Status.GitWaits
	meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionGitReadRetry)
	if c := meta.FindStatusCondition(upd.Status.Conditions, remediationv1alpha1.ConditionGitReadRetry); c != nil {
		meta.SetStatusCondition(&rr.Status.Conditions, *c)
	}
	rr.ResourceVersion = upd.ResourceVersion
	return nil
}

// mergeCommitClockSlack is how far the recorded start of the wait may lie after the executor's clock before the
// clock is taken to have gone back. A start is written truncated to the second from the same clock, so it lies
// after now only when the clock was set back.
const mergeCommitClockSlack = time.Minute

// mergeWaitRemaining is what is left of rr's merge wait (its entry of status.gitWaits) at now, never negative;
// mergeCommitWait when none is recorded. When the entry's start lies more than mergeCommitClockSlack after now, the
// clock went back and the wait can no longer be measured: it is over (waitKind.end), so the whole wait is bounded
// by mergeCommitWait plus the slack and never holds the target lock for as long as the clock was set back. Ending
// it early records the merge without a receipt, which is the inconclusive end and never a success.
func mergeWaitRemaining(rr *remediationv1alpha1.RemediationRequest, now time.Time) time.Duration {
	if gitWait(rr, waitMergeCheck) == nil {
		return mergeCommitWait
	}
	end, ok := waitMergeCheck.end(rr, now)
	if !ok {
		return 0
	}
	return max(end.Sub(now), 0)
}

// mergeCommitWaitLeft is what is left of the wait awaitMergeCommit recorded, at least stepDelay so a request
// whose wait just ended is looked at again soon.
func (r *RemediationRequestReconciler) mergeCommitWaitLeft(rr *remediationv1alpha1.RemediationRequest) time.Duration {
	return max(mergeWaitRemaining(rr, r.now()), stepDelay)
}

// mergeNotRecorded is recheck's answer to a look that found the pull request merged without recording the merge:
// merged is true, and nothing else runs, for prMergeCommitPending (looked at again until the host reports the
// merge commit or the wait ends) and prMergeUnchecked (the plan could not be checked at the merge: Git reads use the common visible poll, while Kubernetes errors are returned). The full freshness check never runs on a merged change, which makes the plan
// look stale.
func (r *RemediationRequestReconciler) mergeNotRecorded(ctx context.Context, rr *remediationv1alpha1.RemediationRequest, outcome prOutcome,
	lookErr error, bound waitBound) (res ctrl.Result, merged bool, err error) {
	switch outcome {
	case prMergeCommitPending:
		return ctrl.Result{RequeueAfter: min(r.mergeCommitWaitLeft(rr), r.pullRequestPollInterval())}, true, nil
	case prMergeUnchecked:
		res, err := r.waitOrFail(ctx, rr, lookErr, bound)
		return res, true, err
	}
	return ctrl.Result{}, false, nil
}

// setVerifiedUnknown sets condition Verified Unknown in the status about to be written.
func setVerifiedUnknown(rr *remediationv1alpha1.RemediationRequest, reason, message string) {
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionVerified,
		Status:             metav1.ConditionUnknown,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: rr.Generation,
	})
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
		setVerifiedUnknown(rr, ReasonMergedBeforeVerification,
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
		setVerifiedUnknown(rr, ReasonNoMergeReceipt, fmt.Sprintf("The merge has no merge "+
			"receipt, and condition Verified had reason %q, which the executor does not write; nothing after the "+
			"merge can be checked", verified.Reason))
	}
	r.setPhase(rr, remediationv1alpha1.PhaseInconclusive)
	clearReleasedRead(rr)
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
