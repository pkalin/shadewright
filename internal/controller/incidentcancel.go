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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// Fixed messages of a request that ends Cancelled because of its Incident (ADR-0039 points 21 and 22). They name
// no Incident: the name in the spec may be anything of the right form, a secret included.
const (
	whyIncidentFolded = "The request's Incident was folded into the Incident of its cause (status.foldedInto)"
	whyIncidentGone   = "The Incident the request names is gone (not found, or made again under the same name " +
		"with another UID)"
	messageIncidentFolded = whyIncidentFolded + " before a pull request was opened; no pull request was opened " +
		"and the branch was left as it is"
	messageIncidentGone = whyIncidentGone + " before a pull request was opened; no pull request was opened and " +
		"the branch was left as it is"
)

// incidentReasons are the reasons of a request cancelled because of its Incident.
var incidentReasons = map[string]bool{
	remediationv1alpha1.ReasonIncidentFolded: true, remediationv1alpha1.ReasonIncidentGone: true,
}

// errIncidentUnread means the live read of the request's Incident before its pull request did not give an
// answer. The request is held and the step retried: it never opens a pull request on a failed read.
var errIncidentUnread = errors.New("the Incident of the request could not be read before its pull request")

// incidentStanding is the last step before the pull request is created (ADR-0039 points 21 and 22): it reads the
// Incident the request names (spec.incidentRef.name in the request's namespace) live from the API server, never
// from the cache, and fails closed. It returns the reason and message to end the request Cancelled with:
//   - IncidentGone when the API server answers NotFound for that Incident, or the Incident of that name has
//     another UID than the one the request names (deleted and opened again under the same name);
//   - IncidentFolded when it has status.foldedInto.
//
// Both empty means the request may open its pull request. Any read that does not give one of these answers (no
// API server reader, a failed or timed-out read, a NotFound that does not name the Incident) is returned as an
// error wrapping errIncidentUnread, for a retry with backoff; the request's deadline (expire) bounds the retries.
// The error carries the API reason only: the API error repeats the name.
func (r *RemediationRequestReconciler) incidentStanding(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) (reason, message string, err error) {
	if r.APIReader == nil {
		// The cache could still hold an Incident folded or deleted since; never decide on it.
		return "", "", fmt.Errorf("%w: no API server reader", errIncidentUnread)
	}
	ref := rr.Spec.IncidentRef
	readCtx, cancel := context.WithTimeout(ctx, incidentReadTimeout(ctx, r.IncidentReadTimeout))
	defer cancel()
	inc := &remediationv1alpha1.Incident{}
	err = r.APIReader.Get(readCtx, client.ObjectKey{Namespace: rr.Namespace, Name: ref.Name}, inc)
	switch {
	case err != nil && notFoundIncident(err, ref.Name):
		return remediationv1alpha1.ReasonIncidentGone, messageIncidentGone, nil
	case err != nil:
		apiReason := apierrors.ReasonForError(err)
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			apiReason = metav1.StatusReasonTimeout
		}
		return "", "", fmt.Errorf("%w: %s", errIncidentUnread, apiReason)
	case inc.UID != types.UID(ref.UID):
		return remediationv1alpha1.ReasonIncidentGone, messageIncidentGone, nil
	case inc.Status.FoldedInto != nil:
		return remediationv1alpha1.ReasonIncidentFolded, messageIncidentFolded, nil
	}
	return "", "", nil
}

// notFoundIncident reports whether err is the API server's NotFound for the Incident named name: a definite
// answer that it does not exist. A NotFound whose details name another object or none is not taken for one.
func notFoundIncident(err error, name string) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) || !apierrors.IsNotFound(err) {
		return false
	}
	d := status.Status().Details
	return d != nil && d.Name == name
}

// endForIncident ends a request whose Incident was folded or is gone (incidentStanding) Cancelled with reason and
// message: the kill switch's gate first, then one status write that sets the phase. Cancelled is a final phase,
// so the same write takes the request out of the count under maxConcurrentRemediations (active). The target lock
// is not released in that write (ADR-0039 points 21 and 22, amended 2026-11-16): the request has no recorded pull
// request, so one may still exist (an earlier open whose answer was lost and that the listing does not show yet,
// or one a person opens from the branch the cancel leaves), and the status rule keeps the lock (prMayBeOpen,
// settleLock in stop). The write wakes the request again, and the next reconcile reads the branch (afterStop,
// checkHeld), as after Expired: it releases the lock when no open pull request of the branch may deliver the
// change, and otherwise keeps it and looks again after heldRecheckInterval; a pull request of its own that the read
// finds open is closed then, and one found merged is followed (settleCancelled). The cancel itself writes nothing
// to the Git host. A conflict or a failed write is returned for a retry, which reads the request again and decides
// again; a write that went through but whose answer was lost is found Cancelled.
func (r *RemediationRequestReconciler) endForIncident(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	reason, message string) error {
	if err := r.writeAllowed(ctx); err != nil {
		return err
	}
	return r.stop(ctx, rr, remediationv1alpha1.PhaseCancelled, reason, message)
}

// cancelledForIncident reports whether rr ended Cancelled because of its Incident (endForIncident), with the reason
// of the cancel: on PullRequestOpened while no pull request is recorded, on Approved once the read of
// the branch recorded one (recordCancelledOpen), as stop places the reason of an end.
func cancelledForIncident(rr *remediationv1alpha1.RemediationRequest) (reason string, ok bool) {
	if rr.Status.Phase != remediationv1alpha1.PhaseCancelled {
		return "", false
	}
	for _, t := range []string{remediationv1alpha1.ConditionPROpened, remediationv1alpha1.ConditionApproved} {
		c := meta.FindStatusCondition(rr.Status.Conditions, t)
		if c != nil && c.Status == metav1.ConditionFalse && incidentReasons[c.Reason] {
			return c.Reason, true
		}
	}
	return "", false
}

// cancelRecorded reports whether the reason of the cancel is on Approved with a pull request recorded: the write of
// recordCancelledOpen went through, and the close may be made.
func cancelRecorded(rr *remediationv1alpha1.RemediationRequest, reason string) bool {
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionApproved)
	return rr.Status.PullRequest != nil && c != nil && c.Status == metav1.ConditionFalse && c.Reason == reason
}

// settleCancelled is checkHeld for a request cancelled because of its Incident whose read of the branch found a pull
// request of its own (with its marker) open or merged: one from an open whose answer was lost (ADR-0039 points 21
// and 22, amended 2026-11-16). Left open, it could be merged while the request is Cancelled, and nothing would
// follow the merge.
//   - merged: the merge is recorded as a stale plan's is (endMerged, with the mark of markCancelledMerge): Merged
//     with its receipt, then Inconclusive with StalePlanMerged, neutral for the circuit breaker, unless its
//     verification fails; a head that is not the planned change ends NeedsIntervention (MergedWithForeignChange).
//   - open, not recorded yet: recordCancelledOpen records it, and the next reconcile closes it.
//   - open and recorded: closeCancelled closes it, through the gated provider of the cleanup Expired uses
//     (closeChangeRequest), so the kill switch holds the close like any write.
//
// The lock is released only by a later read of the branch that finds no open pull request that may deliver the
// change (checkHeld, openHolding); the request stays Cancelled with the reason of the cancel, on Approved.
func (r *RemediationRequestReconciler) settleCancelled(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	v changeVerdict, reason string) (reconcile.Result, error) {
	switch {
	case v.state == changeMerged:
		return r.followCancelledMerge(ctx, rr, v.pr)
	case !cancelRecorded(rr, reason):
		return r.recordCancelledOpen(ctx, rr, v.pr, reason)
	}
	return r.closeCancelled(ctx, rr, reason)
}

// followCancelledMerge ends a cancelled request whose own pull request cr was found merged through endMerged, in
// the repository the plan was made for. errStopped is the end; a read that fails, or a merge commit not reported
// yet, is retried after the delay the host asked for or with the error backoff, with nothing recorded.
func (r *RemediationRequestReconciler) followCancelledMerge(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest) (reconcile.Result, error) {
	cleanCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	defer cancel()
	git, err := r.openPlanRepository(cleanCtx, rr)
	if err == nil {
		err = r.endMerged(cleanCtx, git, rr, cr)
	}
	err = boundErr(ctx, cleanCtx, "check of the merged change request", err)
	if errors.Is(err, errStopped) {
		return reconcile.Result{}, nil
	}
	logf.FromContext(ctx).Info("Could not record the merge of the pull request of a cancelled RemediationRequest yet; "+
		"tried again", "remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", cr.Number,
		"error", err.Error())
	return r.waitOrFail(ctx, rr, err)
}

// recordCancelledOpen records cr, the request's own pull request found open after the cancel, in one status write:
// the pull request, PullRequestOpened True saying it was found and is closed next, and the reason of the cancel on
// Approved (stop's place for it once a pull request is recorded), with a message that no longer says none was
// opened. The write wakes the request, and the next reconcile closes it; so a close that fails is retried with its
// own backoff, which this write does not cut short. A pull request whose number, URL or head do not pass the
// schema is not recorded and not closed: the lock is kept and looked at again after heldRecheckInterval. A failed
// write is returned for a retry, which reads the branch again.
func (r *RemediationRequestReconciler) recordCancelledOpen(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest, reason string) (reconcile.Result, error) {
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", cr.Number)
	recordFound(rr, cr)
	if rr.Status.PullRequest == nil {
		log.Info("Kept the target lock of a cancelled RemediationRequest: its open pull request could not be recorded")
		return reconcile.Result{RequeueAfter: heldRecheckInterval}, nil
	}
	n := rr.Status.PullRequest.Number
	why := whyIncidentGone
	if reason == remediationv1alpha1.ReasonIncidentFolded {
		why = whyIncidentFolded
	}
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:   remediationv1alpha1.ConditionApproved,
		Status: metav1.ConditionFalse,
		Reason: reason,
		Message: fmt.Sprintf("%s before a pull request was recorded; pull request #%d, from an earlier open whose "+
			"answer was lost, was found on the branch afterwards and is closed", why, n),
		ObservedGeneration: rr.Generation,
	})
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:   remediationv1alpha1.ConditionPROpened,
		Status: metav1.ConditionTrue,
		Reason: ReasonPullRequestOpened,
		Message: fmt.Sprintf("Pull request #%d, with this request's marker, was found open after the request was "+
			"cancelled (%s); Shadewright closes it, and the target stays locked until it is closed or merged", n, reason),
		ObservedGeneration: rr.Generation,
	})
	// The read went through, so writes are not paused now; a pause found before an earlier read is over.
	resume(rr)
	if err := r.Status().Update(ctx, rr); err != nil {
		return reconcile.Result{}, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	log.Info("Recorded the open pull request of a cancelled RemediationRequest; it is closed next")
	return reconcile.Result{RequeueAfter: stepDelay}, nil
}

// closeCancelled closes the recorded pull request of a request cancelled because of its Incident, with the cleanup
// Expired uses (closeChangeRequest: the pull request read by its number with this request's marker, stillOwns, the
// close through the provider the kill switch gates).
//   - closed: PullRequestOpened says Shadewright closed it (closedByUs), PullRequestNotClosed and WritesPaused go,
//     and the next read of the branch releases the lock (closedOnly, checkHeld).
//   - paused (spec.paused): nothing is closed, WritesPaused says so, and it is tried again (waitWhilePaused). A pause
//     found before the read sets it too (checkHeld); the first read after the pause removes it (here, in
//     recordCancelledOpen, or with the release when the pull request is found closed or gone).
//   - not this request's any more (no marker, or the request gone or replaced): nothing is closed, and the read after
//     heldRecheckInterval settles the lock by the usual rule.
//   - a step of the Git host that failed (the token, the read, the close, a refusal included): condition
//     PullRequestNotClosed says which and why, with a Warning Event when it is set or its reason changes, and the
//     lock is kept. A refusal is tried again after heldRecheckInterval, any other failure after the delay the host
//     asked for or with the error backoff. That write changes PullRequestNotClosed only, which wakes nothing
//     (skipOwnMarks), so it never cuts the delay short.
//   - any other failure (the settings or the request could not be read): returned for the error backoff.
func (r *RemediationRequestReconciler) closeCancelled(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	reason string) (reconcile.Result, error) {
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr),
		"pullRequest", rr.Status.PullRequest.Number)
	cleanCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	err := r.closeChangeRequest(cleanCtx, rr)
	err = boundErr(ctx, cleanCtx, "cleanup of the change request", err)
	cancel()
	if ctx.Err() != nil {
		return reconcile.Result{}, err
	}
	_, isCluster := errors.AsType[*clusterError](err)
	switch {
	case err == nil:
		closedByUs(rr, "the request was cancelled ("+reason+")")
		clearNotClosed(rr)
		resume(rr)
		if err := r.Status().Update(ctx, rr); err != nil {
			return reconcile.Result{}, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
		}
		log.Info("Closed the pull request of a cancelled RemediationRequest")
		return reconcile.Result{RequeueAfter: stepDelay}, nil
	case errors.Is(err, freshness.ErrPaused):
		return r.waitWhilePaused(ctx, rr)
	case errors.Is(err, errChangeRequestNotOurs):
		// Looked at again on the lock's recheck, as any open pull request of the branch: never at once, since the
		// same read would find it again.
		log.Info("Closed nothing for a cancelled RemediationRequest: its pull request is not its own any more")
		return reconcile.Result{RequeueAfter: heldRecheckInterval}, nil
	case isCluster, errors.Is(err, errRepositoryGone), errors.Is(err, errTargetBranchChanged),
		errors.Is(err, freshness.ErrRepositoryUnusable),
		stepOf(err).step == stepOther && !errors.Is(err, context.DeadlineExceeded):
		// Not a step of the Git host: the next read of the branch says what the repository allows
		// (PullRequestUnreadable), and a failed read of the cluster (the settings, the request) goes to the error
		// backoff. The cleanup's deadline is a step of the Git host that did not answer.
		return reconcile.Result{}, err
	}
	s := stepOf(err)
	s.reason, _ = reinstalledReason(err)
	_, _, refused := refusal(err)
	want := cancelNotClosedCondition(rr, reason, s)
	changed, event := r.setRefused(rr, want)
	if changed {
		if werr := r.Status().Update(ctx, rr); werr != nil {
			return reconcile.Result{}, errors.Join(err, fmt.Errorf("update status of RemediationRequest %s/%s: %w",
				rr.Namespace, rr.Name, werr))
		}
		if event {
			r.refusedEvent(rr, EventPullRequestNotClosed, want)
		}
	}
	log.Info("Could not close the pull request of a cancelled RemediationRequest; kept the target lock and tried "+
		"again", "reason", s.reason, "error", err.Error())
	if refused {
		return reconcile.Result{RequeueAfter: heldRecheckInterval}, nil
	}
	return r.waitOrFail(ctx, rr, err)
}

// cancelNotClosedCondition is condition PullRequestNotClosed for the recorded pull request of a request cancelled
// because of its Incident (reason) whose close failed at step s.
func cancelNotClosedCondition(rr *remediationv1alpha1.RemediationRequest, reason string,
	s refusedStep) metav1.Condition {
	pr := rr.Status.PullRequest
	return metav1.Condition{
		Type:   remediationv1alpha1.ConditionPullRequestNotClosed,
		Status: metav1.ConditionTrue,
		Reason: s.reason,
		Message: fmt.Sprintf("The request was cancelled (%s) and %s failed (%s), so pull request %s, from an open "+
			"before the cancel, is not known to be closed; Shadewright tries again, records a merge or a close by a "+
			"person, and keeps the target locked until it is closed or merged", reason, stepText(rr, s), s.reason,
			pullRequestName(pr.Number, pr.URL)),
		ObservedGeneration: rr.Generation,
	}
}

// markCancelledMerge sets, in the status about to be written, the stale mark of a request cancelled because of its
// Incident whose pull request was merged after the cancel: PlanFresh False with the reason of the cancel, so the
// merge is followed and its outcome capped as a merged stale plan's (stalePlanMerged, capStaleSuccess).
func markCancelledMerge(rr *remediationv1alpha1.RemediationRequest, reason string) {
	markStaleAtMerge(rr, reason, "The request was cancelled ("+reason+") before its pull request was recorded, "+
		"and the pull request was merged afterwards: the plan was stale before the merge")
}
