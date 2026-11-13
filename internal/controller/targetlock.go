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

	"k8s.io/apimachinery/pkg/api/equality"
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
// (checkHeld): its pull request is found by its recorded number, or by its branch when none is recorded, and its
// marker (invariant 5, ADR-0030 section 1), recorded when its status write was lost, and the lock is released
// once that pull request is closed or merged, or none of the request's own exists. A recorded pull request that is
// closed without a merge releases the lock only when no other pull request open on the branch may still deliver this
// request's change (openHolding, the one rule for it): the branch is shared by a supersede chain and anyone may
// open a pull request from it. The status alone never releases the lock on a closed pull request (settleLock), so
// every such release, the watch's included, goes through that read. A look-up that fails keeps the lock (fail
// closed). Deleting the request releases the lock once its finalizer is gone.

// heldRecheckInterval is how often a stopped request that still holds the lock looks its pull request up, and
// how often a request queued behind such a request checks the limits again (besides the wake-up when the
// holder changes, lockWaiters).
const heldRecheckInterval = 10 * time.Minute

// errRecordedNotFound means the recorded pull request cannot be confirmed as this request's: the Git host has no
// Shadewright pull request with the recorded number (maybe for a moment only), answered with another one, or the
// one with that number is from another branch, to another base branch or lacks this request's marker. The lock is
// kept.
var errRecordedNotFound = errors.New("the recorded pull request could not be confirmed as this request's")

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
// known not to be only when the status says so: Merged and the phases after it (Inconclusive included, which
// only a merged request reaches) mean the merge; Superseded is
// written only after the close, except Superseded after a handoff (handedOff), whose pull request its
// replacement holds and which so may be open (ADR-0028 section 4 item 8: no status rule releases a lock on its
// account; it released the lock in the status write that ended it); and PullRequestOpened with reason PullRequestClosed, ChangeRequestClosed or
// ChangeRequestMerged (markClosed) means Shadewright closed it or saw it closed or merged. Without a recorded
// pull request one may still exist, opened while the status write that would record it was lost, so that
// counts as may be open too, until a look-up by the branch and marker says otherwise (checkHeld). Anything
// else, a stop that left the pull request open (HeadMoved, a refused close), one the executor could not find
// (ChangeRequestGone), Failed or an unknown phase included, may still be open (fail closed). It speaks of the
// request's recorded pull request only: another one of its branch may be open, so a lock is released on a closed
// one only after the read of the branch (closedOnly, checkHeld), never by this rule alone (settleLock).
func prMayBeOpen(rr *remediationv1alpha1.RemediationRequest) bool {
	if slices.Contains(successPhases, rr.Status.Phase) || undecided(rr.Status.Phase) || rr.Status.Phase == remediationv1alpha1.PhaseInconclusive ||
		(rr.Status.Phase == remediationv1alpha1.PhaseSuperseded && !handedOff(rr)) {
		return false
	}
	if rr.Status.PullRequest == nil {
		return true
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPROpened)
	return c == nil || !closedReasons[c.Reason]
}

// settleLock releases, in the status about to be written, the lock of a request that holds it, no longer moves
// on and whose pull request the status rule says cannot be open, except when the rule says so only because the
// recorded pull request is closed (closedOnly): that release needs the read of the branch (checkHeld). It never
// takes the lock.
func settleLock(rr *remediationv1alpha1.RemediationRequest) {
	if ptr.Deref(rr.Status.HoldsTarget, false) && !progresses(rr) && !prMayBeOpen(rr) && !closedOnly(rr) {
		release(rr)
	}
}

// closedOnly reports whether the status says no pull request of the request is open only because its recorded
// pull request is closed without a merge: PullRequestOpened with reason PullRequestClosed or ChangeRequestClosed,
// or Superseded (written after Shadewright's close) with a pull request recorded. Another pull request of its
// branch may still carry its change, so the status alone does not release the lock.
func closedOnly(rr *remediationv1alpha1.RemediationRequest) bool {
	if rr.Status.PullRequest == nil || slices.Contains(successPhases, rr.Status.Phase) || undecided(rr.Status.Phase) ||
		rr.Status.Phase == remediationv1alpha1.PhaseInconclusive {
		return false
	}
	if rr.Status.Phase == remediationv1alpha1.PhaseSuperseded && !handedOff(rr) {
		return true
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPROpened)
	return c != nil && (c.Reason == ReasonPullRequestClosed || c.Reason == ReasonChangeRequestClosed)
}

// release clears the lock in the status about to be written, with the condition that said why it was kept.
func release(rr *remediationv1alpha1.RemediationRequest) {
	rr.Status.HoldsTarget = new(false)
	meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionTargetHeld)
}

// checkHeld looks up the pull request of a request that stopped and still holds the lock, by its recorded number
// (its branch when none is recorded) and marker. It records one the status does not record yet, and marks a
// closed or merged one so in PullRequestOpened (unless a closed reason is there already); it releases the lock
// when that pull request is closed or merged, or when the branch has none of the request's own (and none is
// recorded). A recorded one that is closed without a merge releases the lock only when the open pull requests of
// the branch hold none that may still deliver this request's change (openHolding; ADR-0028 section 4, ADR-0030
// section 1); a merged one delivered it, and releases the lock as the status rule does for a merge. When one does, the lock is kept and condition TargetHeld names it, written when it changes. An open
// one keeps the lock and is looked at again after heldRecheckInterval. A look-up that fails for any reason (the
// repository left the settings, writes are paused, the Git host refused or failed, or answered that it has no
// pull request with the recorded number) keeps the lock and is tried again after the same interval; only the
// caller's cancellation is returned as an error.
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
	recorded := rr.Status.PullRequest != nil
	lookCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	// A status that says the recorded pull request is closed (closedOnly) was written from a read of it, or from
	// Shadewright's own close by its number: the read by number then needs no marker, which a person may have
	// edited out of the description since.
	git, cr, err := r.findOwnWith(lookCtx, rr, !closedOnly(rr))
	free := err == nil && (cr == nil || cr.State != providers.ChangeRequestOpen)
	var holding *providers.ChangeRequest
	if free && recorded && cr.State == providers.ChangeRequestClosed {
		holding, err = r.openHolding(lookCtx, git, rr)
		free = err == nil && holding == nil
	}
	err = boundErr(ctx, lookCtx, "look-up of the change request", err)
	cancel()
	if ctx.Err() != nil {
		return reconcile.Result{}, err
	}
	if err != nil {
		log.Info("Kept the target lock of RemediationRequest: its pull request, or the open pull requests of its "+
			"branch, could not be looked up", "error", err.Error())
		return reconcile.Result{RequeueAfter: heldRecheckInterval}, nil
	}
	before := rr.Status.DeepCopy()
	if cr != nil {
		recordFound(rr, *cr)
	}
	switch {
	case free:
		release(rr)
	case holding != nil:
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
			Type:   remediationv1alpha1.ConditionTargetHeld,
			Status: metav1.ConditionTrue,
			Reason: ReasonOpenOnBranch,
			Message: fmt.Sprintf("Pull request #%d is open on the request's branch and may still deliver its change; "+
				"the target stays locked until it is closed or merged", holding.Number),
			ObservedGeneration: rr.Generation,
		})
	}
	if equality.Semantic.DeepEqual(before, &rr.Status) {
		return reconcile.Result{RequeueAfter: heldRecheckInterval}, nil
	}
	if err := r.Status().Update(ctx, rr); err != nil {
		return reconcile.Result{}, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	switch {
	case free:
		log.Info("Released the target lock of RemediationRequest: none of its pull requests is open")
		return reconcile.Result{}, nil
	case holding != nil:
		log.Info("Kept the target lock of RemediationRequest: another pull request is open on its branch",
			"pullRequest", holding.Number)
	default:
		log.Info("Recorded the open pull request of a stopped RemediationRequest", "pullRequest", cr.Number)
	}
	return reconcile.Result{RequeueAfter: heldRecheckInterval}, nil
}

// ReasonOpenOnBranch is the reason of condition TargetHeld: a pull request other than the recorded one is open on
// the request's branch and may still deliver its change.
const ReasonOpenOnBranch = "PullRequestOpenOnBranch"

// openHolding is the one rule for which open pull requests of a request's branch keep the lock of a request whose
// recorded pull request is closed without a merge. It lists the open pull requests of the branch to the target branch
// (ListChangeRequests, the only proof that none is open) and returns the first that may still deliver this
// request's change, or nil when none may.
//
// The branch is shared by a supersede chain and anyone with write access may open a pull request from it, and
// every open one has the branch head, which carries this request's commit unless someone rewrote the branch. So
// every open one counts, whoever opened it and whatever marker its body holds, except the recorded pull request
// of another request on the same target, branch and repository that holds the lock itself (a replacement that
// took the pull request over, ADR-0028 section 4): that request's own lock keeps the target locked while the
// pull request is open, since a request never releases its lock while its recorded pull request is open, so a
// predecessor and its live successor never wait on each other and never both let go. The other requests are read
// from the API server, not the cache. A read that fails is returned, and keeps the lock.
func (r *RemediationRequestReconciler) openHolding(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest) (*providers.ChangeRequest, error) {
	open, err := git.ListChangeRequests(ctx, freshness.BranchName(rr), true)
	if err != nil {
		return nil, freshness.WrapProvider("list the open change requests of the branch", err)
	}
	if len(open) == 0 {
		return nil, nil
	}
	list := &remediationv1alpha1.RemediationRequestList{}
	if err := r.reader().List(ctx, list, client.InNamespace(rr.Namespace)); err != nil {
		return nil, fmt.Errorf("list the RemediationRequests of the target: %w", err)
	}
	heldByOther := map[int64]bool{}
	for i := range list.Items {
		o := &list.Items[i]
		if o.UID == rr.UID || o.Spec.TargetRef.UID != rr.Spec.TargetRef.UID || o.Status.PullRequest == nil ||
			freshness.BranchName(o) != freshness.BranchName(rr) || o.Status.PlanRepository == nil ||
			rr.Status.PlanRepository == nil || *o.Status.PlanRepository != *rr.Status.PlanRepository || !holdsLock(o) {
			continue
		}
		heldByOther[o.Status.PullRequest.Number] = true
	}
	for i := range open {
		if !heldByOther[open[i].Number] {
			return &open[i], nil
		}
	}
	return nil, nil
}

// recordFound writes what a look-up found into the status: the pull request when none is recorded (and its
// number, URL and head pass the schema), and PullRequestOpened as closed or merged when it is no longer open,
// unless that condition already says it is (a close by Shadewright keeps its own reason).
func recordFound(rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest) {
	if rr.Status.PullRequest == nil && shaPattern.MatchString(cr.HeadSHA) && cr.Number > 0 &&
		strings.HasPrefix(cr.URL, "https://") && len(cr.URL) <= 2048 {
		rr.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA}
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPROpened); c != nil &&
		closedReasons[c.Reason] {
		return
	}
	switch cr.State {
	case providers.ChangeRequestClosed:
		markClosed(rr, ReasonChangeRequestClosed, "was closed without a merge")
	case providers.ChangeRequestMerged:
		markClosed(rr, ReasonChangeRequestMerged, "was merged")
	}
}

// findOwnChangeRequest finds the request's pull request by its recorded number (lookUpRecorded), or by its branch
// when none is recorded, in the repository the plan was made for and nowhere else, reading only. It returns nil
// and no error when the branch has no pull request with this request's marker and none is recorded (none of its
// own was opened), and an error wrapping errRecordedNotFound when the recorded one cannot be confirmed as this
// request's (a 404 included). Every other failure is returned as it is.
func (r *RemediationRequestReconciler) findOwnChangeRequest(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	*providers.ChangeRequest, error) {
	_, cr, err := r.findOwn(ctx, rr)
	return cr, err
}

// findOwn is findOwnChangeRequest that also returns the repository it read.
func (r *RemediationRequestReconciler) findOwn(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	providers.GitProvider, *providers.ChangeRequest, error) {
	return r.findOwnWith(ctx, rr, true)
}

// findOwnWith is findOwn; with marker false a recorded pull request is not checked for this request's marker.
func (r *RemediationRequestReconciler) findOwnWith(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	marker bool) (providers.GitProvider, *providers.ChangeRequest, error) {
	if r.OpenRepository == nil {
		return nil, nil, errors.New("cannot look up the change request: no Git provider")
	}
	if rr.Status.PlanRepository == nil {
		return nil, nil, errRepositoryGone
	}
	git, err := r.openPlanRepository(ctx, rr)
	if err != nil {
		return nil, nil, err
	}
	if rr.Status.PullRequest != nil {
		// openPlanRepository returned, so the source is recorded and its target branch is the repository's.
		cr, err := lookUpRecorded(ctx, git, rr, string(rr.Status.Source.TargetBranch))
		switch {
		case errors.Is(err, errRecordedNotFound):
			return nil, nil, err
		case err != nil:
			return nil, nil, freshness.WrapProvider("find the change request", err)
		case !marker:
			return git, &cr, nil
		}
		switch ours, err := r.ownsMarker(ctx, cr.Body, rr); {
		case err != nil:
			return nil, nil, err
		case !ours:
			return nil, nil, fmt.Errorf("%w: pull request #%d lacks this request's marker", errRecordedNotFound, cr.Number)
		}
		return git, &cr, nil
	}
	cr, err := git.FindChangeRequest(ctx, freshness.BranchName(rr))
	switch {
	case providers.IsNotFound(err):
		return git, nil, nil
	case err != nil:
		return nil, nil, freshness.WrapProvider("find the change request", err)
	}
	switch ours, err := r.ownsMarker(ctx, cr.Body, rr); {
	case err != nil:
		return nil, nil, err
	case !ours:
		// Someone else's pull request on the branch name: none of this request's own was opened, as the close of
		// closeChangeRequest also finds.
		return git, nil, nil
	}
	return git, &cr, nil
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
