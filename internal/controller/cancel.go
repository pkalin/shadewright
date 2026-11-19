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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/controller/lockverdict"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// This file holds the way into Cancelled that a person takes (#289): spec.cancel. The spec field is the record
// that the cancel was asked for, so no status mark is written before the close: a close whose answer is lost (a
// timeout, a restart, a failed status write) is made again on the next reconcile, and closing a closed pull request
// succeeds. The branch is never deleted. Nothing but the failed check run and the close is written to the pull
// request once spec.cancel is set.

const (
	// ReasonCancelRequested is the reason of the condition that ends a request Cancelled because spec.cancel is
	// set: PullRequestOpened (False) before a pull request is recorded, Approved (False) after.
	ReasonCancelRequested = "CancelRequested"
	// EventCancelled is the Normal Event recorded when spec.cancel ends a request Cancelled. The request ends only
	// once its pull request is known closed or known never opened (CancelPending says what keeps it from ending).
	EventCancelled = "Cancelled"
	// EventCancelPending is the Warning Event recorded when condition CancelPending is set, or its reason changes.
	EventCancelPending = "CancelPending"
	// EventCancelCheckNotFailed is the Warning Event recorded, next to EventCancelled, when a request ends
	// Cancelled on a closed pull request whose review check could not be failed.
	EventCancelCheckNotFailed = "CancelCheckNotFailed"

	// ReasonHandoffInFlight is the reason of CancelPending while a handoff intent of the request stands.
	ReasonHandoffInFlight = "HandoffInFlight"
	// ReasonCancelFailing is the reason of CancelPending while a step of the cancel fails in a way worth a retry.
	ReasonCancelFailing = "CancelFailing"
	// ReasonCancelNotFinished is the reason of CancelPending once a wait or a failure worth a retry has lasted
	// cleanupFailingBound.
	ReasonCancelNotFinished = "CancelNotFinished"
)

// cancelCheckTitle is the title of the failed check run of a cancelled request.
const cancelCheckTitle = "The request was cancelled"

// Fixed messages of a cancelled request.
const (
	messageCancelledUndecided = "spec.cancel is set: the request was cancelled before the policy check decided it; " +
		"nothing was written to Git for it"
	messageCancelledClosed = "spec.cancel is set: the request was cancelled before its change was merged, and " +
		"Shadewright closed its pull request, if one was open; the branch is left as it is"
	messageCancelledFoundClosed = "spec.cancel is set: the request was cancelled before its change was merged; " +
		"its pull request was found closed; the branch is left as it is"
	// messageCancelledOpenElsewhere ends a cancelled request whose own pull request is open on another base branch,
	// where a person moved it (#892), whether or not the target branch changed: it is not closed by Shadewright, and
	// the target stays locked until it is closed or merged.
	messageCancelledOpenElsewhere = "spec.cancel is set: the request was cancelled before its change was merged; " +
		"its pull request is open on another base branch, where it was moved, and Shadewright does not close it " +
		"there; the target stays locked until a person closes or merges it"
	// messageCheckNotFailed ends the message of a request that ended Cancelled although its review check could not
	// be failed, and is the text of EventCancelCheckNotFailed.
	messageCheckNotFailed = "the failed review check " + providers.CheckRunName + " could not be written on " +
		"the pull request's head, so a check on it that passed is not to be trusted if the pull request is reopened"
)

// cancelRequested reports whether spec.cancel asks to cancel rr.
func cancelRequested(rr *remediationv1alpha1.RemediationRequest) bool {
	return rr.Spec.Cancel
}

// cancelUndecided ends a request that the policy check has not decided yet (no phase, or Pending) Cancelled. No
// step before the policy check writes to Git, so there is nothing to close. The request takes no lock: a request
// in this state holds none. The write goes through the status subresource with the resourceVersion that was read,
// so a check that decided the request meanwhile makes it a conflict, retried with backoff.
func (r *RemediationRequestReconciler) cancelUndecided(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	if s, ok := settleNeverHeld(rr); ok && rr.Status.HoldsTarget == nil {
		// As for a refusal: false keeps it out of the limits' lock list (limitsSelectors).
		if err := lockverdict.Release(&rr.Status, s); err != nil {
			return reconcile.Result{}, err
		}
	}
	rr.Status.ObservedGeneration = rr.Generation
	meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionQueued)
	return r.endCancelled(ctx, rr, messageCancelledUndecided, false)
}

// cancelOpen is recheck's step for a request decided by the policy check and not merged whose spec.cancel is set.
// It runs after the look at a recorded pull request, so a merge seen there was recorded first and a cancel never
// hides it, and after the recovery of a handoff (recoverHandoff), so a replacement whose commit was pushed and
// merged before its status recorded the takeover ends Merged. outcome is that look's result (prOpen when no pull
// request is recorded).
//
// Cancelled is final only when the pull request cannot merge any more: it is known closed, or known never opened.
// Before each close, and before the end on a pull request found closed, the check run shadewright/review of the
// recorded head is failed (failCancelCheck, as for a stale plan, #713), so a pull request reopened later cannot be
// merged on a check that is still green. The check reads the recorded pull request by its number first: one open on a
// base a person moved it to gets no check, and the request ends Cancelled saying so. A closed pull request ends the
// request even when that check cannot be
// written: at once on a refusal, after cleanupFailingBound on a failure worth a retry, with a Warning Event that
// says so (endCancelClosed).
//
//   - The recorded pull request is closed already (by a person, or by an earlier close of this cancel whose status
//     write was lost): the request ends Cancelled.
//   - While a handoff intent stands (ADR-0028 section 4), the replacement may hold the pull request: nothing is
//     closed, CancelPending says so (HandoffInFlight), and the request is looked at again after the poll interval.
//   - Otherwise the pull request is closed (closeChangeRequest: the recorded one, or every open one of the branch
//     with this request's marker when none is recorded) and the request ends Cancelled. A pull request of the
//     branch found merged before any status recorded it ends the request Merged instead (endUnrecordedMerge).
//   - The Git host refuses a step of the close for good (403 or 422), the plan's repository is not configured or
//     usable any more, its target branch changed, or the recorded pull request is not this request's: the request
//     is not ended. It keeps its phase with CancelPending (cancelWatch): every look reads the pull request, so a
//     merge ends it Merged and a close Cancelled, and tries the close again.
//   - A failed read of a pull request, whatever its class, and any other failure are retried with backoff, with
//     CancelPending (cancelFailed), for cleanupFailingBound; then the request is watched as for a refusal. While a
//     refusal is the reason of CancelPending, such a failure writes nothing and records no Event, except a failed
//     check of a merged head, which records its merge wait in both watches (cancelFailed, #889).
//   - A pause of writes waits (waitWhilePaused) and starts no bound.
func (r *RemediationRequestReconciler) cancelOpen(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	outcome prOutcome) (reconcile.Result, error) {
	switch outcome {
	case prClosed:
		return r.endCancelFound(ctx, rr, r.failCancelCheck(ctx, rr), false)
	case prOpenElsewhere:
		// Open on a base a person moved it to (#892): nothing is written to it there, the failed check included.
		return r.endCancelFound(ctx, rr, nil, true)
	case prOpen, prUnchecked, prStopped, prConflicted, prMergeCommitPending, prMergeUnchecked, prBranchUnread:
	}
	if intent(rr) != nil {
		return r.cancelHandoffWait(ctx, rr)
	}
	// The check fails first, whatever becomes of the close; its failure does not stop the close, and it is tried
	// again with the next try. Its read of the pull request by number comes before the write: one found open on a
	// base a person moved it to gets no check, and the cancel ends saying so.
	checkErr := r.failCancelCheck(ctx, rr)
	if errors.Is(checkErr, errOpenElsewhere) {
		return r.endCancelFound(ctx, rr, nil, true)
	}
	cleanCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	err := r.closeChangeRequest(cleanCtx, rr)
	// A Git read that ran out of the cleanup's time is still that read, retried as one (keepGitRead).
	err = keepGitRead(ctx, cleanCtx, "cleanup of the change request", err)
	cancel()
	if errors.Is(err, errTargetBranchChanged) {
		// The close cannot be made on this entry: the pull request is read, read-only, on both branches, so a merge
		// or a close is settled after the target branch changed (fifth decision of 2026-11-18 on #771, point 3). A
		// pause never gets here: closeChangeRequest stops at it first, and the look would stop at it too.
		if res, done, lookErr := r.cancelLook(ctx, rr, checkErr); done {
			return res, lookErr
		}
	}
	if done, endErr := r.endUnrecordedMerge(ctx, rr, err); done {
		// Its own pull request was merged before any status recorded it: the change landed, so the request ends
		// Merged (or MergedWithForeignChange), never Cancelled. A failed check of that merge is retried.
		if endErr != nil && !errors.Is(endErr, errStopped) {
			return r.cancelFailed(ctx, rr, endErr, "the check of a merged pull request of branch "+
				freshness.BranchName(rr)+" failed")
		}
		return reconcile.Result{}, nil
	}
	switch {
	case err == nil:
		return r.endCancelClosed(ctx, rr, checkErr, func() {
			closedByUs(rr, "the request was cancelled (spec.cancel)")
		}, messageCancelledClosed)
	case errors.Is(err, freshness.ErrPaused):
		return r.waitWhilePaused(ctx, rr)
	case ctx.Err() != nil:
		return reconcile.Result{}, err
	case errors.Is(err, errHandoffInFlight):
		// An intent written since this reconcile read the request (stillOwns read it fresh).
		return r.cancelHandoffWait(ctx, rr)
	case errors.Is(err, errOpenElsewhere):
		// Its own pull request is open on a base a person moved it to since the look (#892): nothing is closed there.
		return r.endCancelFound(ctx, rr, checkErr, true)
	case errors.Is(err, errRepositoryGone):
		return r.cancelWatch(ctx, rr, reasonRepositoryUnavailable, "the repository it was made for is not configured any more")
	case errors.Is(err, errTargetBranchChanged):
		return r.cancelWatchBranchChanged(ctx, rr)
	case errors.Is(err, freshness.ErrRepositoryUnusable):
		return r.cancelWatch(ctx, rr, reasonRepositoryUnusable, "the settings of its repository cannot be used")
	case errors.Is(err, errChangeRequestNotOurs):
		return r.cancelWatch(ctx, rr, ReasonForeignChangeRequest, "the pull request recorded for it could not be "+
			"confirmed as this request's")
	case isUnknownChange(err):
		// A failed read of a pull request, a refused one included, is never a refusal of the close: retried.
		return r.cancelFailed(ctx, rr, err, stepText(rr, stepOf(err))+" failed")
	}
	if s, ok := refusedCleanup(err); ok {
		return r.cancelWatch(ctx, rr, s.reason, "the Git host refused ("+s.reason+") "+stepText(rr, s))
	}
	return r.cancelFailed(ctx, rr, err, stepText(rr, stepOf(err))+" failed")
}

// cancelLook is the read-only look of a cancel whose close cannot be made because the target branch of the plan's
// repository changed: the request's pull requests are read on the recorded and on the configured branch (lookHeld,
// the read that settles the target lock; one whose record was lost is found by its marker). checkErr is the result of the write of the failed review check (failCancelCheck).
//   - Merged and not recorded: recorded as the request's merge, into the recorded branch (recordOwnMerge) or, moved
//     to the configured branch, as #771 decided on 2026-11-16 (recordMergeOnConfigured).
//   - Closed or gone: the request ends Cancelled (endCancelClosed); the read of the branch after the end settles the
//     lock. A review check that waits for a pause keeps the request in its phase until the pause ends.
//   - Merged into a base a person moved it to (changeMergedElsewhere): stopped as the watch stops it, in
//     NeedsIntervention with ChangeRequestOnOtherBase naming the merge (stopMergedElsewhere).
//   - Open on a base a person moved it to (changeElsewhere, #892): it ends Cancelled with
//     messageCancelledOpenElsewhere and nothing marks the pull request closed: the lock stays while it is open, and
//     nothing closes it there.
//
// A pause set since the close was tried stops the look before any read of the Git host (planEntry): the request
// waits for the pause (waitWhilePaused). A failed Git read is retried visibly and without an end, with the cancel
// watch recorded (cancelLookFailed); a failed Kubernetes read and the caller's cancellation are returned. done is
// false when the look settles nothing (the pull request is open, or the answers disagree): the caller's cancel watch
// after a branch change stands.
func (r *RemediationRequestReconciler) cancelLook(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	checkErr error) (reconcile.Result, bool, error) {
	if rr.Status.Source == nil || intent(rr) != nil {
		return reconcile.Result{}, false, nil
	}
	lookCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	v, _, err := r.lookHeld(lookCtx, rr)
	cancel()
	if errors.Is(err, freshness.ErrPaused) && ctx.Err() == nil {
		res, waitErr := r.waitWhilePaused(ctx, rr)
		return res, true, waitErr
	}
	if err != nil || v.state == changeUnknown || ctx.Err() != nil {
		if err == nil {
			err = v.err
		}
		switch {
		case ctx.Err() != nil:
			return reconcile.Result{}, true, ctx.Err()
		case isClusterError(err):
			return reconcile.Result{}, true, err
		case gitReadFailure(err) && !answeredUnknown(err):
			// A failed read is recorded like every other (seventh decision of 2026-11-18 on #771, point 5): a Paced
			// wait with GitReadRetry, and the cancel watch with CancelPending, retried at the longer of the two
			// paces (the poll interval, or the delay the host asked for).
			return r.cancelLookFailed(ctx, rr, err)
		}
		// Answers that disagree, or a 404 the branch does not confirm: every read was answered and nothing is
		// settled; the caller's cancel watch stands.
		return reconcile.Result{}, false, nil
	}
	switch v.state {
	case changeMerged:
		if mergeRecorded(rr) {
			return reconcile.Result{}, false, nil
		}
		res, err := r.followHeldMerge(ctx, rr, func(ctx context.Context) error {
			if v.pr.BaseBranch != string(rr.Status.Source.TargetBranch) {
				return r.recordMergeOnConfigured(ctx, rr, v.pr, "", false)
			}
			return r.recordOwnMerge(ctx, rr, v.pr)
		})
		return res, true, err
	case changeClosed, changeGone:
		res, err := r.endCancelFound(ctx, rr, checkErr, false)
		return res, true, err
	case changeMergedElsewhere:
		// Merged into a base a person moved it to: stopped as the watch stops it (stopMergedElsewhere), never
		// Cancelled as found closed.
		if err := r.stopMergedElsewhere(ctx, rr, v.pr); !errors.Is(err, errStopped) {
			return reconcile.Result{}, true, err
		}
		return reconcile.Result{}, true, nil
	case changeElsewhere:
		res, err := r.endCancelFound(ctx, rr, checkErr, true)
		return res, true, err
	case changeOpen, changeUnknown:
	}
	return reconcile.Result{}, false, nil
}

// endCancelFound ends a cancel whose pull request a look found closed or gone (cancelOpen with prClosed, cancelLook),
// or, with movedOpen, whose own pull request it found open on a base a person moved it to (#892): the request ends
// Cancelled as for one gone from both target branches, but nothing says it is closed. The lock stays while it is
// open (lookHeld, TargetHeld) and no close is tried on that entry; a person closes or merges it. Both looks decide
// through this one function.
func (r *RemediationRequestReconciler) endCancelFound(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	checkErr error, movedOpen bool) (reconcile.Result, error) {
	if movedOpen {
		return r.endCancelClosed(ctx, rr, checkErr, func() {}, messageCancelledOpenElsewhere)
	}
	return r.endCancelClosed(ctx, rr, checkErr, func() {
		markClosed(rr, ReasonPullRequestClosed, "is closed; the request was cancelled (spec.cancel)")
	}, messageCancelledFoundClosed)
}

// cancelLookFailed is cancelLook's answer to a failed Git read: the read is retried without an end
// (retryGitRead, noBoundCancelWatch), the cancel watch after the change of the target branch is recorded
// (CancelPending), and the request is looked at again after the longer of the two waits, so neither a Retry-After
// nor the poll interval is shortened. Failed writes are returned for the error backoff.
func (r *RemediationRequestReconciler) cancelLookFailed(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	readErr error) (reconcile.Result, bool, error) {
	res, err := r.retryGitRead(ctx, rr, readErr, unboundedRead(noBoundCancelWatch))
	if err != nil {
		return reconcile.Result{}, true, err
	}
	watch, err := r.cancelWatchBranchChanged(ctx, rr)
	if err != nil {
		return reconcile.Result{}, true, err
	}
	res.RequeueAfter = max(res.RequeueAfter, watch.RequeueAfter)
	return res, true, nil
}

// cancelWatchBranchChanged is the cancel watch of a request whose close cannot be made because the target branch of
// its repository changed (cancelWatch, reason TargetBranchChanged).
func (r *RemediationRequestReconciler) cancelWatchBranchChanged(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) (reconcile.Result, error) {
	return r.cancelWatch(ctx, rr, reasonTargetBranchChanged, "its repository now targets another branch")
}

// endCancelClosed ends rr, whose pull request is known closed (found closed, or just closed by the cancel),
// Cancelled with message; mark records how it was closed. checkErr is the result of the write of the failed review
// check (failCancelCheck):
//   - nil: the check is failed, or was not pending; the request ends.
//   - A pause waits (waitWhilePaused); the caller's cancellation is returned.
//   - A final refusal of the Git host (403, 422): the check cannot be written. The request ends Cancelled all the
//     same, as the closed pull request cannot merge, with the Warning Event EventCancelCheckNotFailed and the same
//     words in its message, so a person who reopens the pull request knows that its check is not to be trusted;
//     the target is not kept locked for a check that cannot be written.
//   - Anything else is a failure of the cancel (cancelFailed), and the check is tried again on the next look,
//     which finds the pull request closed. Once cleanupFailingBound has passed, the request ends as for a refusal.
//
// A request whose stale plan's repository the settings reach only through a new installation of the GitHub App
// (#290) settles its target lock first (releaseReinstalled), as endStaleClosed does: while it cannot (a failed or
// refused read, reported with PullRequestUnreadable, or another open pull request of the branch, recorded in
// TargetHeld), the request keeps its phase and is looked at again after the poll interval, with no other bound.
func (r *RemediationRequestReconciler) endCancelClosed(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	checkErr error, mark func(), message string) (reconcile.Result, error) {
	if checkErr != nil {
		switch {
		case errors.Is(checkErr, freshness.ErrPaused):
			return r.waitWhilePaused(ctx, rr)
		case ctx.Err() != nil:
			return reconcile.Result{}, checkErr
		}
		// After a change of the target branch the check of the recorded head cannot be written through the plan's
		// entry any more: as final as a refusal.
		if _, _, final := refusal(checkErr); !final && !errors.Is(checkErr, errTargetBranchChanged) {
			if _, past := r.cancelBound(rr); !past {
				return r.cancelFailed(ctx, rr, checkErr, "the failed review check "+providers.CheckRunName+
					" could not be written")
			}
		}
		logf.FromContext(ctx).Info("Could not fail the review check of a cancelled RemediationRequest; ending it, "+
			"as its pull request is closed", "remediationRequest", client.ObjectKeyFromObject(rr),
			"error", checkErr.Error())
	}
	// A stale plan's cleanup closed or found closed through a new installation of the GitHub App (#290): the lock is
	// settled before the end, which no later look through that installation could settle.
	switch err := r.releaseReinstalled(ctx, rr); {
	case err == nil:
	case errors.Is(err, freshness.ErrPaused):
		return r.waitWhilePaused(ctx, rr)
	case ctx.Err() != nil:
		return reconcile.Result{}, err
	default:
		return reconcile.Result{RequeueAfter: r.pullRequestPollInterval()}, nil
	}
	mark()
	return r.endCancelled(ctx, rr, message, checkErr != nil)
}

// unstartableRecovery is why no read can start for a cancelled replacement whose handoff recovery meets a changed
// target branch (cancelRecoveryFailed): the end of a wait for a read says it.
const unstartableRecovery = "the repository entry of the plan names another target branch, and the recovery of " +
	"the handoff reads only through it"

// cancelRecoveryFailed handles err, a failure of the recovery of a handoff (recoverHandoff) of a request whose
// spec.cancel is set: the cancel cannot finish while it is not known whether the replacement's own commit is on the
// pull request, and the request is never ended on it, so a merge of that commit is still recorded once the
// recovery can read again. A pause waits and the caller's cancellation is returned. A final refusal of the Git
// host, a repository that is not configured or usable any more and a changed target branch are watched
// (cancelWatch), as for a refused close; anything else, a failed read of a pull request included, is retried
// (cancelFailed) within cleanupFailingBound, then watched. The recovery's own deadline grace does not apply. A
// changed target branch also notes that no read can start (unstartableRecovery), so a MergeCheck or MergeContent
// wait recorded before the change ends at its end, as with a repository gone, and never stands past it.
func (r *RemediationRequestReconciler) cancelRecoveryFailed(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, err error) (reconcile.Result, error) {
	const what = "the recovery of its handoff, which learns whether its own commit is on the pull request it replaces"
	switch {
	case errors.Is(err, freshness.ErrPaused):
		return r.waitWhilePaused(ctx, rr)
	case ctx.Err() != nil:
		return reconcile.Result{}, err
	case errors.Is(err, errRepositoryGone):
		return r.cancelWatch(ctx, rr, reasonRepositoryUnavailable, "the repository it was made for is not configured any more")
	case errors.Is(err, errTargetBranchChanged):
		// The recovery reads only through the plan's repository entry, which names another target branch now, and
		// nothing else reads for the request while it recovers: no read can start, as with a repository gone
		// (planEntry), so a wait for a read past its end ends on this note (endAfterLastRead) instead of standing.
		r.gitRetries().noteUnstartable(client.ObjectKeyFromObject(rr), unstartableRecovery)
		return r.cancelWatchBranchChanged(ctx, rr)
	case errors.Is(err, freshness.ErrRepositoryUnusable):
		return r.cancelWatch(ctx, rr, reasonRepositoryUnusable, "the settings of its repository cannot be used")
	case isUnknownChange(err):
		return r.cancelFailed(ctx, rr, err, what+", failed")
	}
	if reason, _, ok := refusal(err); ok {
		return r.cancelWatch(ctx, rr, reason, "the Git host refused ("+reason+") "+what)
	}
	return r.cancelFailed(ctx, rr, err, what+", failed")
}

// cancelHandoffWait is the wait of a cancel while the request's handoff intent stands: nothing is closed,
// CancelPending says why (HandoffInFlight), and the request is looked at again after the poll interval, never
// later than the end of cleanupFailingBound. After the bound it is watched (CancelNotFinished).
func (r *RemediationRequestReconciler) cancelHandoffWait(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	why := "a handoff of its pull request to a replacement is in flight, and nothing is closed until it is done " +
		"or aborted"
	left, past := r.cancelBound(rr)
	if past {
		return r.cancelWatch(ctx, rr, ReasonCancelNotFinished, why)
	}
	if err := r.setCancelPending(ctx, rr, ReasonHandoffInFlight, why, false); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: min(r.pullRequestPollInterval(), left)}, nil
}

// cancelFailed handles err, a failure worth a retry of a cancel (why says which step, never the Git host's
// answer). Within cleanupFailingBound after CancelPending was first set, it records CancelPending (CancelFailing)
// and retries as waitOrFail says (the error backoff, or the delay the host asked for), never later than the end of
// the bound. After it, the request is watched (CancelNotFinished). While a refusal is the reason of CancelPending
// (cancelRefused), the reason does not change: no condition is written, no Event is recorded, and the request is
// looked at again after the poll interval (or the delay the host asked for, when longer), as the refusal's watch
// does. In both watches a failed read of a wait of its own (waitedRead: the check of a merged head, what a merge
// brought) is recorded with that wait (retryGitRead, noBoundWatchedCancel), so the request ends at that wait's end
// as with every other caller, and the retry is never later than it (#889). The caller's cancellation records
// nothing and is returned.
func (r *RemediationRequestReconciler) cancelFailed(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	err error, why string) (reconcile.Result, error) {
	if ctx.Err() != nil {
		return reconcile.Result{}, err
	}
	if c := cancelRefused(rr); c != nil {
		// A refusal stays the reason until the cancel finishes or the refusal is gone: a failure in between writes
		// nothing and records no Event, and the request stays watched.
		logf.FromContext(ctx).Info("Could not finish the cancel of RemediationRequest; still watching it",
			"remediationRequest", client.ObjectKeyFromObject(rr), "refusal", c.Reason, "error", err.Error())
		if waitedRead(err) {
			return r.retryGitRead(ctx, rr, err, unboundedRead(noBoundWatchedCancel))
		}
		return reconcile.Result{RequeueAfter: max(r.pullRequestPollInterval(), retryAfterOf(err))}, nil
	}
	left, past := r.cancelBound(rr)
	if past {
		logf.FromContext(ctx).Info("Could not finish the cancel of RemediationRequest; watching it",
			"remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error())
		res, werr := r.cancelWatch(ctx, rr, ReasonCancelNotFinished, why)
		if werr != nil || !waitedRead(err) {
			return res, werr
		}
		return r.retryGitRead(ctx, rr, err, unboundedRead(noBoundWatchedCancel))
	}
	if werr := r.setCancelPending(ctx, rr, ReasonCancelFailing, why, false); werr != nil {
		return reconcile.Result{}, werr
	}
	res, werr := r.waitOrFail(ctx, rr, err, paceWithin(left))
	if res.RequeueAfter > left {
		res.RequeueAfter = left
	}
	return res, werr
}

// waitedRead reports whether err is a failed Git read that belongs to a wait of its own with an end (the check at the
// merge, readOfMergeCheck, or what a merge brought, readOfContent).
func waitedRead(err error) bool {
	_, ok := errors.AsType[*kindedReadError](err)
	return ok && gitReadFailure(err)
}

// cancelWatch keeps a cancelled request that cannot be ended in its phase (reason and why say why) and watches its
// pull request, as for a stale plan whose close is refused (#199): CancelPending is recorded, and the request is
// looked at again after the poll interval, with no error, so each look reads the pull request (a merge ends it
// Merged, a close Cancelled) and tries the close again. Nothing else is written to it.
func (r *RemediationRequestReconciler) cancelWatch(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	reason, why string) (reconcile.Result, error) {
	if err := r.setCancelPending(ctx, rr, reason, why, true); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: r.pullRequestPollInterval()}, nil
}

// cancelRefused returns condition CancelPending of rr when it is True with the reason of a refusal (a refused
// step, a repository gone or unusable, a changed target branch, a pull request not this request's), nil otherwise
// (no condition, a wait for a handoff, a failure worth a retry, the end of the bound).
func cancelRefused(rr *remediationv1alpha1.RemediationRequest) *metav1.Condition {
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionCancelPending)
	if c == nil || c.Status != metav1.ConditionTrue {
		return nil
	}
	switch c.Reason {
	case ReasonHandoffInFlight, ReasonCancelFailing, ReasonCancelNotFinished:
		return nil
	}
	return c
}

// cancelBound reports what is left of cleanupFailingBound since CancelPending was set, and whether it has passed.
// Without the condition the bound starts now. Both times are the executor's clock.
func (r *RemediationRequestReconciler) cancelBound(rr *remediationv1alpha1.RemediationRequest) (time.Duration, bool) {
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionCancelPending)
	if c == nil || c.Status != metav1.ConditionTrue {
		return cleanupFailingBound, false
	}
	left := boundLeft(c, cleanupFailingBound, r.now())
	return left, left <= 0
}

// setCancelPending records CancelPending with reason and a message built from why, in one status write when it
// changes, and one Warning Event CancelPending after the write when it is set or its reason changes. The
// lastTransitionTime is kept while it stays True (setRefused). A failed write is returned for a retry and records
// no Event.
func (r *RemediationRequestReconciler) setCancelPending(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	reason, why string, watching bool) error {
	want := cancelPendingCondition(rr, reason, why, watching)
	changed, event := r.setRefused(rr, want)
	if !changed {
		return nil
	}
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	if event {
		r.refusedEvent(rr, EventCancelPending, want)
		logf.FromContext(ctx).Info("Could not finish the cancel of RemediationRequest yet",
			"remediationRequest", client.ObjectKeyFromObject(rr), "reason", reason)
	}
	return nil
}

// cancelPendingCondition is condition CancelPending with reason. why says what keeps the cancel from finishing;
// watching says that the request is now watched (a refusal, or the end of cleanupFailingBound) rather than retried
// with backoff.
func cancelPendingCondition(rr *remediationv1alpha1.RemediationRequest, reason, why string,
	watching bool) metav1.Condition {
	what := "Shadewright tries again with backoff; if the cancel still cannot finish 24 hours after the first " +
		"failure, it stops the retries and watches the pull request"
	if watching {
		what = "the request keeps its phase: Shadewright watches the pull request, so a merge ends the request " +
			"Merged and a close ends it Cancelled, and tries again on every look; give the App its access back, or " +
			"close the pull request by hand where the close is what was refused"
	}
	return metav1.Condition{
		Type:   remediationv1alpha1.ConditionCancelPending,
		Status: metav1.ConditionTrue,
		Reason: reason,
		Message: fmt.Sprintf("spec.cancel is set, but the cancel cannot finish: %s (%s); %s", why,
			recordedText(rr), what),
		ObservedGeneration: rr.Generation,
	}
}

// cancelCheckRun is the failed check run shadewright/review of a cancelled request.
func cancelCheckRun() providers.CheckRun {
	return providers.CheckRun{Name: providers.CheckRunName, Status: providers.CheckRunFailure, Title: cancelCheckTitle,
		Summary: "This request was cancelled (spec.cancel), so its change must not be merged. Shadewright closes " +
			"the pull request; if the Git host refuses the close, a person closes it. This check stays failed."}
}

// failCancelCheck completes the check run shadewright/review on the recorded head of a cancelled request's pull
// request as failed, as failStaleCheck does for a stale plan (#713), and records it (state Stale, which no later
// write changes). It does nothing when the check is not pending (reviewer hints off, no recorded pull request, a
// handoff, or the state already recorded on the head).
func (r *RemediationRequestReconciler) failCancelCheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	return r.failCheck(ctx, rr, cancelCheckRun())
}

// endCancelled ends rr Cancelled with reason CancelRequested and message, in one status write (stop). The request
// ends only once its pull request is known closed or known never opened, so the Event is Normal. checkNotFailed
// says that the review check of the closed pull request could not be failed (endCancelClosed): the message says so
// too, and a Warning Event EventCancelCheckNotFailed goes with it.
//
// The Events are recorded before the write, as the finalizer reports a release before its patch: a restart right
// after the write would lose an Event recorded after it, and the ended request is never looked at again. So that
// no Event is recorded for an end that cannot be written, the request is read from the API server first (not the
// cache): a request changed since rr was read (a stale cache, a merge or a handoff recorded meanwhile) is returned
// as a conflict for a retry, and so is a failed read. A failed write is returned for a retry too, and the retry
// records the Events again: at least once, maybe twice. The end requeues nothing: a request that ended is woken
// only by a change.
func (r *RemediationRequestReconciler) endCancelled(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	message string, checkNotFailed bool) (reconcile.Result, error) {
	live := &remediationv1alpha1.RemediationRequest{}
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
		return reconcile.Result{}, fmt.Errorf("read RemediationRequest %s/%s before ending it Cancelled: %w",
			rr.Namespace, rr.Name, err)
	}
	if live.UID != rr.UID || live.ResourceVersion != rr.ResourceVersion {
		return reconcile.Result{}, fmt.Errorf("end RemediationRequest %s/%s Cancelled: %w", rr.Namespace, rr.Name,
			apierrors.NewConflict(remediationv1alpha1.GroupVersion.WithResource("remediationrequests").GroupResource(),
				rr.Name, errors.New("it changed since it was read")))
	}
	if checkNotFailed {
		message += "; " + messageCheckNotFailed
	}
	if r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeNormal, EventCancelled, message)
		if checkNotFailed {
			r.Recorder.Event(rr, corev1.EventTypeWarning, EventCancelCheckNotFailed, messageCheckNotFailed)
		}
	}
	err := r.stop(ctx, rr, remediationv1alpha1.PhaseCancelled, ReasonCancelRequested, message)
	if !errors.Is(err, errStopped) {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, nil
}
