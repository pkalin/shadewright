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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
)

// This file holds the way into Cancelled that a person takes (#289): spec.cancel. The spec field is the record
// that the cancel was asked for, so no status mark is written before the close: a close whose answer is lost (a
// timeout, a restart, a failed status write) is made again on the next reconcile, and closing a closed pull request
// succeeds. The branch is never deleted.

const (
	// ReasonCancelRequested is the reason of the condition that ends a request Cancelled because spec.cancel is
	// set: PullRequestOpened (False) before a pull request is recorded, Approved (False) after.
	ReasonCancelRequested = "CancelRequested"
	// EventCancelled is the Event recorded when spec.cancel ends a request Cancelled: Normal when no pull request
	// of the request is left open, Warning when one may be.
	EventCancelled = "Cancelled"
)

// Fixed messages of a cancelled request.
const (
	messageCancelledUndecided = "spec.cancel is set: the request was cancelled before the policy check decided it; " +
		"nothing was written to Git for it"
	messageCancelledClosed = "spec.cancel is set: the request was cancelled before its change was merged, and " +
		"Shadewright closed its pull request, if one was open; the branch is left as it is"
	messageCancelledFoundClosed = "spec.cancel is set: the request was cancelled before its change was merged; " +
		"its pull request was found closed; the branch is left as it is"
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
	if rr.Status.HoldsTarget == nil {
		// As for a refusal: false keeps it out of the limits' lock list (limitsSelectors).
		release(rr)
	}
	rr.Status.ObservedGeneration = rr.Generation
	meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionQueued)
	return r.endCancelled(ctx, rr, messageCancelledUndecided, false)
}

// cancelOpen is recheck's step for a request decided by the policy check and not merged whose spec.cancel is set.
// It runs after the look at a recorded pull request, so a merge seen there was recorded first and a cancel never
// hides it. outcome is that look's result (prOpen when no pull request is recorded).
//
//   - The recorded pull request is closed already (by a person, or by an earlier close of this cancel whose status
//     write was lost): the request ends Cancelled.
//   - While a handoff intent stands (ADR-0028 section 4), the replacement may hold the pull request: nothing is
//     closed, and the request is looked at again after the poll interval. The handoff then ends it (HandedOff) or
//     is aborted, and the cancel goes on.
//   - Otherwise the pull request is closed (closeChangeRequest: the recorded one, or every open one of the branch
//     with this request's marker when none is recorded) and the request ends Cancelled. A pull request of the
//     branch found merged before any status recorded it ends the request Merged instead (endUnrecordedMerge).
//   - The Git host refuses the close, or a step before it, for good (403 or 422), or the plan's repository is not
//     configured or usable any more, or the recorded pull request is not this request's: the request ends Cancelled
//     with a message that its pull request, if any, was not closed, and a Warning Event. The lock on the target
//     stays until a read of the branch finds none open (prMayBeOpen, checkHeld).
//   - A pause of writes waits (waitWhilePaused). A failed read of a pull request, whatever its class, and any
//     other failure are retried: a conflict on a recorded pull request (it may have been merged since the look) is
//     found on the next look, which records the merge.
func (r *RemediationRequestReconciler) cancelOpen(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	outcome prOutcome) (reconcile.Result, error) {
	if outcome == prClosed {
		markClosed(rr, ReasonPullRequestClosed, "is closed; the request was cancelled (spec.cancel)")
		return r.endCancelled(ctx, rr, messageCancelledFoundClosed, false)
	}
	if intent(rr) != nil {
		logf.FromContext(ctx).Info("Waiting for the handoff before cancelling RemediationRequest",
			"remediationRequest", client.ObjectKeyFromObject(rr))
		return reconcile.Result{RequeueAfter: r.pullRequestPollInterval()}, nil
	}
	cleanCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	err := r.closeChangeRequest(cleanCtx, rr)
	err = boundErr(ctx, cleanCtx, "cleanup of the change request", err)
	cancel()
	if done, endErr := r.endUnrecordedMerge(ctx, rr, err); done {
		// Its own pull request was merged before any status recorded it: the change landed, so the request ends
		// Merged (or MergedWithForeignChange), never Cancelled.
		if endErr != nil && !errors.Is(endErr, errStopped) {
			return r.waitOrFail(ctx, rr, endErr)
		}
		return reconcile.Result{}, nil
	}
	switch {
	case err == nil:
		closedByUs(rr, "the request was cancelled (spec.cancel)")
		return r.endCancelled(ctx, rr, messageCancelledClosed, false)
	case errors.Is(err, freshness.ErrPaused):
		return r.waitWhilePaused(ctx, rr)
	case errors.Is(err, errHandoffInFlight):
		// An intent written since this reconcile read the request (stillOwns read it fresh).
		return reconcile.Result{RequeueAfter: r.pullRequestPollInterval()}, nil
	case errors.Is(err, errRepositoryGone):
		return r.endCancelled(ctx, rr, notClosedMessage("the repository it was made for is not configured any more"), true)
	case errors.Is(err, errTargetBranchChanged):
		return r.endCancelled(ctx, rr, notClosedMessage("its repository now targets another branch"), true)
	case errors.Is(err, freshness.ErrRepositoryUnusable):
		return r.endCancelled(ctx, rr, notClosedMessage("the settings of its repository cannot be used"), true)
	case errors.Is(err, errChangeRequestNotOurs):
		return r.endCancelled(ctx, rr, notClosedMessage("the pull request recorded for it could not be confirmed "+
			"as this request's"), true)
	case isUnknownChange(err):
		// A failed read of a pull request is never a refusal: retried.
		return r.waitOrFail(ctx, rr, err)
	}
	if reason, _, ok := refusal(err); ok {
		return r.endCancelled(ctx, rr, notClosedMessage("the Git host refused ("+reason+") to close it"), true)
	}
	return r.waitOrFail(ctx, rr, err)
}

// notClosedMessage is the message of a request cancelled whose pull request, if it has one, was not closed, and why.
func notClosedMessage(why string) string {
	return "spec.cancel is set: the request was cancelled before its change was merged, but its pull request, if " +
		"one is open, was not closed: " + why + ". Close it by hand; the target stays locked until none is found open"
}

// endCancelled ends rr Cancelled with reason CancelRequested and message, in one status write (stop), and records
// the Event after it: a Warning when leftOpen says a pull request of the request may still be open. A failed status
// write is returned for a retry and records no Event; the next reconcile, which reads spec.cancel again, repeats
// the step. The end requeues nothing: a request that ended is woken only by a change.
func (r *RemediationRequestReconciler) endCancelled(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	message string, leftOpen bool) (reconcile.Result, error) {
	err := r.stop(ctx, rr, remediationv1alpha1.PhaseCancelled, ReasonCancelRequested, message)
	if !errors.Is(err, errStopped) {
		return reconcile.Result{}, err
	}
	if r.Recorder != nil {
		eventType := corev1.EventTypeNormal
		if leftOpen {
			eventType = corev1.EventTypeWarning
		}
		r.Recorder.Event(rr, eventType, EventCancelled, message)
	}
	return reconcile.Result{}, nil
}
