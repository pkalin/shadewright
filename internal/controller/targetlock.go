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

// checkHeld looks at the pull requests of a request that stopped and still holds the lock (changeOf, with the
// open pull requests of the branch), by its recorded number, or by every pull request of its branch with its
// marker when none is recorded. It records one the status does not record yet, and marks a closed or merged one
// so in PullRequestOpened (unless a closed reason is there already). The rule per verdict (lockRule):
//   - open: the request's own pull request is open; the lock is kept, and TargetHeld, which names another pull
//     request, is cleared.
//   - merged: the change was delivered; the lock is released, as the status rule does for a merge.
//   - closed, or gone (confirmed not this request's: a 404 the listing of the branch confirms, another branch or
//     base, no marker, or no pull request of its own on the branch): the lock is released only when the open pull
//     requests of the branch hold none that may still deliver this request's change (openHolding; ADR-0028
//     section 4, ADR-0030 section 1). When one does, the lock is kept and condition TargetHeld names it.
//   - unknown (a read that failed, conflicting reads such as the recorded number read closed while the branch
//     lists it open, or an answer about another pull request), or a repository that cannot be opened: the lock
//     is kept and nothing is written.
//
// TargetHeld is written when it changes. Every outcome but unknown is looked at again after heldRecheckInterval
// while the lock is kept, and so is unknown; only the caller's cancellation is returned as an error.
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
	v, holding, err := r.lookHeld(lookCtx, rr)
	err = boundErr(ctx, lookCtx, "look-up of the change request", err)
	cancel()
	if ctx.Err() != nil {
		return reconcile.Result{}, err
	}
	rule := lockRule[v.state]
	if err != nil || v.state == changeUnknown {
		if err == nil {
			err = v.err
		}
		log.Info("Kept the target lock of RemediationRequest: its pull request, or the open pull requests of its "+
			"branch, could not be looked up", "error", err.Error())
		return reconcile.Result{RequeueAfter: heldRecheckInterval}, nil
	}
	before := rr.Status.DeepCopy()
	if v.state != changeGone {
		recordFound(rr, v.pr)
	}
	free := rule == lockRelease || rule == lockReleaseUnlessOpen && holding == nil
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
	default:
		// The request's own pull request is open: it holds the lock itself, and TargetHeld, which names another
		// one, no longer says why (one that held it closed, and the recorded one was reopened).
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionTargetHeld)
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
		log.Info("Recorded the open pull request of a stopped RemediationRequest", "pullRequest", v.pr.Number)
	}
	return reconcile.Result{RequeueAfter: heldRecheckInterval}, nil
}

// lockOutcome is what checkHeld does with the lock for a verdict.
type lockOutcome int

const (
	lockKeep lockOutcome = iota
	lockRelease
	lockReleaseUnlessOpen
)

// lockRule is checkHeld's rule per verdict: unknown and open keep the lock, merged releases it, closed and gone
// release it unless an open pull request of the branch may still deliver the change.
var lockRule = map[changeState]lockOutcome{
	changeOpen: lockKeep, changeMerged: lockRelease, changeClosed: lockReleaseUnlessOpen,
	changeGone: lockReleaseUnlessOpen, changeUnknown: lockKeep,
}

// lookHeld is checkHeld's read: the verdict on the request's pull requests, in the repository the plan was made
// for and nowhere else, and for a closed or gone one the open pull request that holds the lock (openHolding).
// A status that says the recorded pull request is closed (closedOnly) was written from a read of it, or from
// Shadewright's own close by its number: the read by number then needs no marker, which a person may have edited
// out of the description since. err is a failure outside the verdict (no repository, the list of requests).
func (r *RemediationRequestReconciler) lookHeld(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	changeVerdict, *providers.ChangeRequest, error) {
	if r.OpenRepository == nil {
		return changeVerdict{}, nil, errors.New("cannot look up the change request: no Git provider")
	}
	if rr.Status.PlanRepository == nil {
		return changeVerdict{}, nil, errRepositoryGone
	}
	git, err := r.openPlanRepository(ctx, rr)
	if err != nil {
		return changeVerdict{}, nil, err
	}
	v := r.changeOf(ctx, git, rr, changeQuery{marker: !closedOnly(rr), openOnBranch: true})
	if lockRule[v.state] != lockReleaseUnlessOpen {
		return v, nil, nil
	}
	holding, err := r.openHolding(ctx, rr, v.open)
	return v, holding, err
}

// ReasonOpenOnBranch is the reason of condition TargetHeld: a pull request is open on the request's branch and
// may still deliver its change, although the request's own is closed or gone.
const ReasonOpenOnBranch = "PullRequestOpenOnBranch"

// openHolding is the one rule for which open pull requests of a request's branch keep the lock of a request whose
// pull request is closed without a merge or gone. open is every open pull request of the branch to the target
// branch (changeOf's listing, the only proof that none is open), and it returns the first that may still deliver
// this request's change, or nil when none may.
//
// The branch is shared by a supersede chain and anyone with write access may open a pull request from it, and
// every open one has the branch head, which carries this request's commit unless someone rewrote the branch. So
// every open one counts, whoever opened it and whatever marker its body holds, the request's own number included
// (changeOf has already answered unknown when the listing has it open while the read by number did not), except
// the recorded pull request of another request on the same target, branch and repository that holds the lock
// itself (a replacement that took the pull request over, ADR-0028 section 4): that request's own lock keeps the
// target locked while the pull request is open, since a request never releases its lock while its recorded pull
// request is open, so a predecessor and its live successor never wait on each other and never both let go. The
// other requests are read from the API server, not the cache. A read that fails is returned, and keeps the lock.
func (r *RemediationRequestReconciler) openHolding(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	open []providers.ChangeRequest) (*providers.ChangeRequest, error) {
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
