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
	"regexp"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// The lock per target (design section 10.7, #88, #294, #376). A request takes the lock in the status write of
// its allowed decision (status.holdsTarget true, under the same mutex as the count of the limits) and keeps it
// until its pull request is known merged or closed. While the executor moves the request on, the lock is
// simply held. When the request stops (stop, endFreshness) the status rule prMayBeOpen decides at once whether
// it is released; a request that stopped and still holds it is looked at again every heldRecheckInterval
// (checkHeld): its pull request is found by its branch and marker (invariant 5), recorded when its status
// write was lost, and the lock is released once that pull request is closed or merged, or none of the
// request's own exists. A look-up that fails keeps the lock (fail closed). Deleting the request releases the
// lock once its finalizer is gone.

// heldRecheckInterval is how often a stopped request that still holds the lock looks its pull request up, and
// how often a request queued behind such a request checks the limits again (besides the wake-up when the
// holder changes, lockWaiters).
const heldRecheckInterval = 10 * time.Minute

// errRecordedNotFound means the branch has no pull request with the recorded number and this request's
// marker: the recorded one cannot be seen, so the lock is kept.
var errRecordedNotFound = errors.New("the recorded pull request is not the branch's pull request with this request's marker")

// shaPattern is the schema's pattern of status.pullRequest.headSHA.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// holdsLock reports whether a request holds the lock on its target: status.holdsTarget, while the request is not
// deleted or still has the finalizer that closes its pull request. A request written before the field existed
// has none; its lock is then derived from its status as the release before this field did (derivedHold). A
// request the executor still moves on holds the lock whatever the field says, so a false written by hand can
// only release a request that has stopped (fail closed).
func holdsLock(rr *remediationv1alpha1.RemediationRequest) bool {
	if !rr.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(rr, FinalizerName) {
		return false
	}
	if rr.Status.HoldsTarget != nil {
		return *rr.Status.HoldsTarget || progresses(rr)
	}
	return derivedHold(rr)
}

// derivedHold is the lock of a request without status.holdsTarget (written before the field existed): it is
// being deleted with the finalizer, the executor still moves it on, or it recorded a pull request that may
// still be open. Fail closed: only a status that proves the pull request merged or closed releases it.
func derivedHold(rr *remediationv1alpha1.RemediationRequest) bool {
	if !rr.DeletionTimestamp.IsZero() {
		return controllerutil.ContainsFinalizer(rr, FinalizerName)
	}
	return progresses(rr) || (rr.Status.PullRequest != nil && prMayBeOpen(rr))
}

// closedReasons are the reasons of PullRequestOpened that say the recorded pull request is no longer open
// (markClosed).
var closedReasons = map[string]bool{
	ReasonPullRequestClosed: true, ReasonChangeRequestClosed: true, ReasonChangeRequestMerged: true,
}

// prMayBeOpen is the status rule of the lock: whether a pull request of the request may still be open. It is
// known not to be only when the status says so: Merged and the phases after it mean the merge; Superseded is
// written only after the close; and PullRequestOpened with reason PullRequestClosed, ChangeRequestClosed or
// ChangeRequestMerged (markClosed) means Shadewright closed it or saw it closed or merged. Without a recorded
// pull request one may still exist, opened while the status write that would record it was lost, so that
// counts as may be open too, until a look-up by the branch and marker says otherwise (checkHeld). Anything
// else, a stop that left the pull request open (HeadMoved, a refused close), one the executor could not find
// (ChangeRequestGone), Failed or an unknown phase included, may still be open (fail closed).
func prMayBeOpen(rr *remediationv1alpha1.RemediationRequest) bool {
	if slices.Contains(successPhases, rr.Status.Phase) || rr.Status.Phase == remediationv1alpha1.PhaseSuperseded {
		return false
	}
	if rr.Status.PullRequest == nil {
		return true
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPROpened)
	return c == nil || !closedReasons[c.Reason]
}

// settleLock releases, in the status about to be written, the lock of a request that holds it, no longer moves
// on and whose pull request the status rule says cannot be open. It never takes the lock.
func settleLock(rr *remediationv1alpha1.RemediationRequest) {
	if ptr.Deref(rr.Status.HoldsTarget, false) && !progresses(rr) && !prMayBeOpen(rr) {
		rr.Status.HoldsTarget = new(false)
	}
}

// backfillLock writes status.holdsTarget once on a request decided before the field existed, from derivedHold,
// so the requests of an older release keep the lock their status gives them; one refused then (ended without a
// plan digest) gets false, so the limits' lock list (status.holdsTarget!=false) stops reading it.
func (r *RemediationRequestReconciler) backfillLock(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	if rr.Status.HoldsTarget != nil || (rr.Status.PlanDigest == "" && !ended(rr.Status.Phase)) {
		return nil
	}
	rr.Status.HoldsTarget = new(derivedHold(rr))
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	return nil
}

// checkHeld looks up the pull request of a request that stopped and still holds the lock, by its branch and
// marker. It records one the status does not record yet; it releases the lock when that pull request is
// closed or merged, or when the branch has none of the request's own (and none is recorded). An open one keeps
// the lock and is looked at again after heldRecheckInterval. A look-up that fails for any reason (the
// repository left the settings, writes are paused, the Git host refused or failed) keeps the lock and is
// tried again after the same interval; only the caller's cancellation is returned as an error.
func (r *RemediationRequestReconciler) checkHeld(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	if !ptr.Deref(rr.Status.HoldsTarget, false) || progresses(rr) {
		return reconcile.Result{}, nil
	}
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr))
	if settleLock(rr); !*rr.Status.HoldsTarget {
		// The status already proves the release (a request backfilled or written before settleLock ran): no
		// look-up is needed.
		if err := r.Status().Update(ctx, rr); err != nil {
			return reconcile.Result{}, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
		}
		log.Info("Released the target lock of RemediationRequest: its status shows the pull request closed or merged")
		return reconcile.Result{}, nil
	}
	lookCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	cr, err := r.findOwnChangeRequest(lookCtx, rr)
	err = boundErr(ctx, lookCtx, "look-up of the change request", err)
	cancel()
	if ctx.Err() != nil {
		return reconcile.Result{}, err
	}
	if err != nil {
		log.Info("Kept the target lock of RemediationRequest: its pull request could not be looked up",
			"error", err.Error())
		return reconcile.Result{RequeueAfter: heldRecheckInterval}, nil
	}
	changed := false
	if cr != nil {
		changed = recordFound(rr, *cr)
	}
	release := cr == nil || cr.State != providers.ChangeRequestOpen
	if release {
		rr.Status.HoldsTarget = new(false)
		changed = true
	}
	if !changed {
		return reconcile.Result{RequeueAfter: heldRecheckInterval}, nil
	}
	if err := r.Status().Update(ctx, rr); err != nil {
		return reconcile.Result{}, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	if release {
		log.Info("Released the target lock of RemediationRequest: none of its pull requests is open")
		return reconcile.Result{}, nil
	}
	log.Info("Recorded the open pull request of a stopped RemediationRequest", "pullRequest", cr.Number)
	return reconcile.Result{RequeueAfter: heldRecheckInterval}, nil
}

// recordFound writes what a look-up found into the status: the pull request when none is recorded (and its
// number, URL and head pass the schema), and PullRequestOpened as closed or merged when it is no longer open.
// It reports whether it changed the status.
func recordFound(rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest) bool {
	changed := false
	if rr.Status.PullRequest == nil && shaPattern.MatchString(cr.HeadSHA) && cr.Number > 0 &&
		strings.HasPrefix(cr.URL, "https://") && len(cr.URL) <= 2048 {
		rr.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA}
		changed = true
	}
	switch cr.State {
	case providers.ChangeRequestClosed:
		markClosed(rr, ReasonChangeRequestClosed, "was closed without a merge")
		changed = true
	case providers.ChangeRequestMerged:
		markClosed(rr, ReasonChangeRequestMerged, "was merged")
		changed = true
	}
	return changed
}

// findOwnChangeRequest finds the request's pull request by its branch, in the repository the plan was made for
// and nowhere else, reading only. It returns nil and no error when the branch has no pull request with this
// request's marker and none is recorded (none of its own was opened), and errRecordedNotFound when a recorded
// one is not the branch's pull request with the marker. Every other failure is returned as it is.
func (r *RemediationRequestReconciler) findOwnChangeRequest(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	*providers.ChangeRequest, error) {
	if r.OpenRepository == nil {
		return nil, errors.New("cannot look up the change request: no Git provider")
	}
	if rr.Status.PlanRepository == nil {
		return nil, errRepositoryGone
	}
	git, err := r.openPlanRepository(ctx, rr)
	if err != nil {
		return nil, err
	}
	cr, err := git.FindChangeRequest(ctx, freshness.BranchName(rr))
	recorded := rr.Status.PullRequest
	switch {
	case errors.Is(err, providers.ErrNotFound) && recorded == nil:
		return nil, nil
	case errors.Is(err, providers.ErrNotFound):
		return nil, errRecordedNotFound
	case err != nil:
		return nil, freshness.WrapProvider("find the change request", err)
	}
	if !hasMarker(cr.Body, rr) || (recorded != nil && cr.Number != recorded.Number) {
		if recorded == nil {
			// Someone else's pull request on the branch name: none of this request's own was opened, as the
			// close of closeChangeRequest also finds.
			return nil, nil
		}
		return nil, errRecordedNotFound
	}
	return &cr, nil
}

// lockWaiters maps a change of a request to the requests queued behind the lock on the same target, so they
// check the limits again at once when the holder releases the lock, ends or is deleted, rather than at their
// next recheck. It reads the cache; a failed list wakes nobody, and the waiters' own rechecks still run.
func (r *RemediationRequestReconciler) lockWaiters(ctx context.Context, obj client.Object) []reconcile.Request {
	changed, ok := obj.(*remediationv1alpha1.RemediationRequest)
	if !ok {
		return nil
	}
	list := &remediationv1alpha1.RemediationRequestList{}
	if err := r.List(ctx, list, client.InNamespace(changed.Namespace)); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list RemediationRequests waiting for a target lock")
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		w := &list.Items[i]
		if w.UID == changed.UID || w.Spec.TargetRef.UID != changed.Spec.TargetRef.UID {
			continue
		}
		if c := meta.FindStatusCondition(w.Status.Conditions, remediationv1alpha1.ConditionQueued); c != nil &&
			c.Status == metav1.ConditionTrue && c.Reason == ReasonTargetLocked {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(w)})
		}
	}
	return out
}
