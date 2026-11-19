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
	"bytes"
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
	"github.com/pkalin/shadewright/internal/controller/lockverdict"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// A decided request holds its target until settleTargetLock proves that none of its branch's pull requests
// can still merge. All stopped holders use that decision, including branch changes, Incident cancellation,
// expiry, stale cleanup and handoff recovery. It lists both target branches when they differ. Verification
// keeps the lock until its outcome, then uses the same read. Failed reads never release it. An admission
// refusal never took the lock, a completed handoff transfers it, and deletion releases it with the finalizer.

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
// only a merged request reaches, and Failed with a merge receipt, mergedAndEnded) mean the merge; Superseded is
// written only after the close, except Superseded after a handoff (handedOff), whose pull request its
// replacement holds and which so may be open (ADR-0028 section 4 item 8: no status rule releases a lock on its
// account; it released the lock in the status write that ended it); and PullRequestOpened with reason PullRequestClosed, ChangeRequestClosed or
// ChangeRequestMerged (markClosed) means Shadewright closed it or saw it closed or merged. Without a recorded
// pull request one may still exist, opened while the status write that would record it was lost, so that
// counts as may be open too, Superseded and Expired included (their cleanup closes only the pull requests with
// this request's marker, and anyone may open one from the branch), until a look-up of the branch says otherwise
// (checkHeld, openHolding). Anything
// else, a stop that left the pull request open (HeadMoved, a refused close), one the executor could not find
// (ChangeRequestGone), Failed or an unknown phase included, may still be open (fail closed). It speaks of the
// request's recorded pull request only: another one of its branch may be open, so a lock is released on a closed
// one only after the read of the branch (closedOnly, checkHeld), never by this rule alone (settleTargetLock).
func prMayBeOpen(rr *remediationv1alpha1.RemediationRequest) bool {
	if rr.Status.PullRequest == nil {
		return !slices.Contains(successPhases, rr.Status.Phase) && !undecided(rr.Status.Phase) &&
			rr.Status.Phase != remediationv1alpha1.PhaseInconclusive
	}
	if slices.Contains(successPhases, rr.Status.Phase) || undecided(rr.Status.Phase) || rr.Status.Phase == remediationv1alpha1.PhaseInconclusive ||
		(rr.Status.Phase == remediationv1alpha1.PhaseSuperseded && !handedOff(rr)) || mergedAndEnded(rr) {
		return false
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPROpened)
	return c == nil || !closedReasons[c.Reason]
}

// mergedAndEnded reports whether rr ended Failed after its merge: status.mergeReceipt is written only in the
// write that records the merge of the recorded pull request (recordMerge), so that pull request is not open. Before
// #719 such a request had released the lock at its merge; now one held through verification (heldForVerification)
// releases it in the write that ends it Failed, as Succeeded and Inconclusive do (settleRecorded).
func mergedAndEnded(rr *remediationv1alpha1.RemediationRequest) bool {
	return rr.Status.Phase == remediationv1alpha1.PhaseFailed && rr.Status.MergeReceipt != nil
}

// clearReleasedRead removes condition PullRequestUnreadable of a request whose lock is settled. Status alone
// releases a held lock only on a recorded merge (settleRecorded); any other release needs the read of the branch
// (checkHeld, settleTargetLock).
func clearReleasedRead(rr *remediationv1alpha1.RemediationRequest) {
	if !progresses(rr) && !ptr.Deref(rr.Status.HoldsTarget, false) {
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
	}
}

// heldForVerification reports whether a merged request keeps the lock on its target while the executor verifies
// its outcome (#719): with verify (the executor's --verify-outcomes) on, Merged with a merge receipt (the next
// reconcile moves it to WaitingForDelivery), WaitingForDelivery and Verifying. Its change is merged in Git but
// may not be applied yet, and whether it works is not known: a request queued behind it for the same field would
// otherwise go on to the policy check against that change on its way to the cluster (ValueMismatch), so it waits
// for the holder's end (Succeeded, Failed, Inconclusive) instead. It reads the recorded phase and receipt only,
// so a restart neither releases the lock early nor holds it longer. With verify off a merged request is not held:
// Merged is its end then, and a request found in WaitingForDelivery or Verifying ends Inconclusive at its next
// reconcile (VerificationDisabled), after which the common branch read settles the lock. Its pull request is merged, so
// nothing is left for the finalizer to close.
func heldForVerification(rr *remediationv1alpha1.RemediationRequest, verify bool) bool {
	if !verify {
		return false
	}
	return undecided(rr.Status.Phase) ||
		(rr.Status.Phase == remediationv1alpha1.PhaseMerged && rr.Status.MergeReceipt != nil)
}

// settleStopException handles, once a read worked, what checkHeld must record before the lock may be settled:
//   - a merge of the request's own pull request that the status does not record (mergeRecorded): it is recorded
//     first, through followHeldMerge, so no verdict releases the lock on it (settleByRead needs the record). A
//     request stoppedForBranchChange or cancelledForIncident whose pull request was merged into the target branch
//     the settings name now records it as #771 decided (recordRetargetedMerge, or MergedWithForeignChange for a head
//     Shadewright did not push); one merged into the recorded branch records it as #754 (recordHeldMerge) or, when
//     cancelled, as #794 (settleCancelled); any other stopped holder records it as a stale plan's merge
//     (recordStoppedMerge, decision of 2026-11-17 on #808);
//   - a request cancelledForIncident whose own pull request is found open is settled by settleCancelled (#794);
//   - a request whose stale plan's cleanup stopped on a refusal (staleCleanupStopped) whose own pull request is found
//     open tries the close again (closeAfterStaleStop, #808).
//
// A pull request found by the request's marker with none recorded (its status write was lost, #827) is adopted only
// after its head is proven to be one Shadewright pushed for this request (checkAdoptedHead, or checkMerged on the
// recorded branch): the marker is text of the pull request's body and proves nothing. done is false when nothing
// applies, and checkHeld goes on with the verdict.
func (r *RemediationRequestReconciler) settleStopException(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, v changeVerdict) (res reconcile.Result, done bool, err error) {
	// Read before anything changes the status in memory: recordFound rewrites PullRequestOpened, where a stop
	// without a recorded pull request keeps its reason.
	branchStop := stoppedForBranchChange(rr)
	cancelReason, cancelled := cancelledForIncident(rr)
	// A request handing its pull request over (an outgoing handoff) leaves the merge to the handoff, which gives it
	// to the replacement when the merged head is the replacement's commit (followHandoff).
	handingOver := rr.Status.Handoff != nil && rr.Status.Handoff.To != nil
	if expiredUncleaned(rr) && !handingOver && rr.Status.Source != nil {
		switch {
		case v.state == changeMerged && !mergeRecorded(rr) && v.pr.BaseBranch == string(rr.Status.Source.TargetBranch):
			// Its own pull request, opened while the write that would record it was lost, was merged: recorded as the
			// request's merge (endMerged checks its head and what it brought), never as a merge after a stop.
			res, err := r.followHeldMerge(ctx, rr, func(ctx context.Context) error {
				return r.recordOwnMerge(ctx, rr, v.pr)
			})
			return res, true, err
		case v.state == changeOpen:
			res, err := r.cleanUpExpired(ctx, rr)
			return res, true, err
		}
	}
	if staleCleanupStopped(rr) && !handingOver && v.state == changeOpen {
		// The cleanup of its stale plan stopped on a refusal of the Git host and its own pull request is open: the
		// close is tried again (#808).
		res, err := r.closeAfterStaleStop(ctx, rr)
		return res, true, err
	}
	if v.state == changeMerged && !mergeRecorded(rr) && rr.Status.Source != nil && !handingOver {
		if v.pr.BaseBranch != string(rr.Status.Source.TargetBranch) {
			// Moved to the target branch the settings name now and merged there: recorded as #771 decided, whatever
			// the stop (a request stopped for another reason is read on both branches too, lookHeld).
			res, err := r.followHeldMerge(ctx, rr, func(ctx context.Context) error {
				return r.recordMergeOnConfigured(ctx, rr, v.pr, cancelReason, cancelled)
			})
			return res, true, err
		}
		switch {
		case branchStop:
			res, err := r.followHeldMerge(ctx, rr, func(ctx context.Context) error {
				return r.recordHeldMerge(ctx, rr, v.pr, rr.Status.PullRequest == nil)
			})
			return res, true, err
		case !cancelled:
			res, err := r.followHeldMerge(ctx, rr, func(ctx context.Context) error {
				return r.recordStoppedMerge(ctx, rr, v.pr)
			})
			return res, true, err
		}
	}
	if cancelled && (v.state == changeOpen || v.state == changeMerged && !mergeRecorded(rr)) {
		// A branch change permits reads only. Keep an open PR, and follow a recorded-branch merge read-only.
		if repo, _, err := r.planEntry(ctx, rr); errors.Is(err, errTargetBranchChanged) {
			if v.state == changeOpen {
				return reconcile.Result{RequeueAfter: heldRecheckInterval}, false, nil
			}
			res, err := r.followHeldMerge(ctx, rr, func(ctx context.Context) error {
				git, err := r.openRecordedBranch(ctx, repo, rr.Status.Source.TargetBranch)
				if err != nil {
					return freshness.WrapProvider("open the repository", err)
				}
				return r.endMerged(ctx, git, rr, v.pr)
			})
			return res, true, err
		} else if err != nil {
			return reconcile.Result{}, true, err
		}
		// Its own pull request, from an open whose answer was lost.
		res, err := r.settleCancelled(ctx, rr, v, cancelReason)
		return res, true, err
	}
	return reconcile.Result{}, false, nil
}

// closeAfterStaleStop is the close of the request's own pull request, found open by the read after the stop, of a
// request whose stale plan's cleanup stopped on a refusal of the Git host (staleCleanupStopped, decision of 2026-11-17
// on #808). The request keeps its phase and its lock; only the read that follows releases the lock, with a verdict:
//   - a close that works: the next look, a second later, finds the pull request closed and settles the lock;
//   - a close still refused (403, 422): nothing is written, the stop already says so, and it is tried again at the
//     pace of a stopped holder's looks (heldRecheckInterval);
//   - a close or a read that fails otherwise: retried through the shared record of Git retries (retriedClose,
//     heldUnsettled), without an end, as every look of a stopped holder;
//   - a pause of the kill switch holds it (waitWhilePaused); a failure of the Kubernetes side, and the caller's
//     cancellation, go to the error backoff;
//   - a merge the close met, a pull request that is not this request's or is being handed over, or an entry that is
//     gone or can no longer be used (nothingToClose): nothing is closed, and the look that follows decides from what
//     it reads;
//   - any other error is returned to the work queue.
func (r *RemediationRequestReconciler) closeAfterStaleStop(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) (reconcile.Result, error) {
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr))
	cleanCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	err := r.closeChangeRequest(cleanCtx, rr)
	err = keepGitRead(ctx, cleanCtx, "close after the stop", retriedClose(err))
	cancel()
	switch reason, _, refused := refusal(err); {
	case err == nil:
		log.Info("Closed the pull request of a RemediationRequest whose stale plan's cleanup had stopped")
		return reconcile.Result{RequeueAfter: stepDelay}, nil
	case ctx.Err() != nil || isClusterError(err):
		return reconcile.Result{}, err
	case errors.Is(err, freshness.ErrPaused):
		return r.waitWhilePaused(ctx, rr)
	case gitReadFailure(err):
		return r.heldUnsettled(ctx, rr, changeVerdict{state: changeUnknown}, err, false)
	case refused:
		log.Info("The Git host still refuses to close the pull request of a stopped stale RemediationRequest",
			"reason", reason)
	case nothingToClose(err):
		log.Info("Did not close the pull request of a stopped stale RemediationRequest", "error", err.Error())
	default:
		// Not a failure this function knows: returned, so the work queue retries it and the retry is not silent.
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: r.withinWaits(rr, heldRecheckInterval)}, nil
}

// nothingToClose reports whether err, from the close after a stale stop, means there is nothing for that close to
// do: the pull request merged while it was being closed (a conflict), it is not this request's or its handoff is in
// flight, or the plan's repository entry is gone, changed its target branch or can no longer be used. The look that
// follows decides from what it reads. Any other error is a failure and is not one of these.
func nothingToClose(err error) bool {
	return providers.IsConflict(err) || errors.Is(err, errChangeRequestNotOurs) || errors.Is(err, errHandoffInFlight) ||
		errors.Is(err, errRepositoryGone) || errors.Is(err, errTargetBranchChanged) ||
		errors.Is(err, freshness.ErrRepositoryUnusable)
}

// recordMergeOnConfigured records cr, the request's pull request found merged into the target branch the settings
// name now, for a request stoppedForBranchChange or cancelled (cancelled, with the reason of the cancel): the merge
// counts (recordRetargetedMerge) only for the head Shadewright pushed, which is the recorded head or, with no pull
// request recorded, a head checkAdoptedHead proves; any other head ends NeedsIntervention with
// MergedWithForeignChange and no receipt. It returns what recordRetargetedMerge or mergedForeignInto returns.
func (r *RemediationRequestReconciler) recordMergeOnConfigured(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest, cancelReason string, cancelled bool) error {
	branch := remediationv1alpha1.GitBranch(cr.BaseBranch)
	repo, _, err := r.planEntry(ctx, rr)
	if err != nil && !errors.Is(err, errTargetBranchChanged) {
		return err
	}
	var foreign *renderRefusal
	if rr.Status.PullRequest == nil {
		if foreign, err = r.checkAdoptedHead(ctx, repo, rr, cr); err != nil {
			return err
		}
	} else if cr.Number != rr.Status.PullRequest.Number || cr.HeadSHA != rr.Status.PullRequest.HeadSHA {
		foreign = &renderRefusal{reason: ReasonMergedWithForeignChange, message: "The merged head differs from the approved head"}
	}
	if cancelled {
		markStaleAtMerge(rr, cancelReason, "The request was cancelled for its Incident before its pull request was found merged")
	}
	recordFound(rr, cr)
	if cancelled {
		keepCancelReason(rr, cancelReason, "found merged on the configured branch afterwards")
	}
	if foreign != nil {
		markStaleForBranch(rr, branch)
		return r.mergedForeignInto(ctx, rr, branch, foreign)
	}
	git, err := r.openRecordedBranch(ctx, repo, branch)
	if err != nil {
		return freshness.WrapProvider("open the repository", err)
	}
	return r.recordRetargetedMerge(ctx, git, rr, cr, branch)
}

// checkAdoptedHead checks the head of cr, a pull request found by the request's marker with none recorded, against
// what the executor pushes for this request, independently of the pull request's own claims (#827): on the recorded
// target branch (repo opened for it, reads only), the head is one commit that changes only the target file on top of
// that branch's history (CheckBranch), and the target file there is byte for byte the planned change of the file
// its parent holds, which holds the plan's previous value. A head that fails is returned as a refusal with reason
// MergedWithForeignChange; a read that fails as an error.
func (r *RemediationRequestReconciler) checkAdoptedHead(ctx context.Context, repo remediationv1alpha1.GitRepository,
	rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest) (*renderRefusal, error) {
	foreign := &renderRefusal{reason: ReasonMergedWithForeignChange, message: "the head of the pull request found " +
		"by the request's marker is not a commit Shadewright pushed for this request: one change of the target file " +
		"to the planned value, on the target branch the plan was made for"}
	if !shaPattern.MatchString(cr.HeadSHA) {
		return foreign, nil
	}
	git, err := r.openRecordedBranch(ctx, repo, rr.Status.Source.TargetBranch)
	if err != nil {
		return nil, freshness.WrapProvider("open the repository", err)
	}
	path := string(rr.Status.Source.Path)
	switch state, err := git.CheckBranch(ctx, cr.HeadSHA, path); {
	case err != nil:
		return nil, freshness.WrapProvider("check the head of the pull request found by the marker", err)
	case state != providers.BranchOneChange:
		return foreign, nil
	}
	parents, err := git.Parents(ctx, cr.HeadSHA)
	switch {
	case err != nil:
		return nil, freshness.WrapProvider("read the parents of the head of the pull request", err)
	case len(parents) != 1:
		return foreign, nil
	}
	to, err := git.ReadFile(ctx, cr.HeadSHA, path, providers.MaxFileBytes)
	if err != nil {
		return nil, freshness.WrapProvider("read the file of the pull request", err)
	}
	from, err := git.ReadFile(ctx, parents[0], path, providers.MaxFileBytes)
	if err != nil {
		return nil, freshness.WrapProvider("read the target file the pull request was made on", err)
	}
	want, state, err := planned(from.Data, rr)
	switch {
	case err != nil:
		return notCheckable("the target file at the parent of the head of the pull request found by the request's "+
			"marker", err), nil
	case state != fileHoldsPrevious || !bytes.Equal(want, to.Data):
		return foreign, nil
	}
	return nil, nil
}

// recordStoppedMerge records the merge of cr, the request's own pull request (recorded, or found by its marker),
// for a stopped holder that is neither stoppedForBranchChange nor cancelledForIncident (expired, superseded, stopped
// for another reason): as a stale plan's merge (decision of 2026-11-17 on #808), so the outcome is capped and the
// breaker counts nothing. The plan gets the stale mark with reason PlanChanged unless it carries one; a head that is
// not the recorded one, or that checkMerged does not prove the planned change alone, ends NeedsIntervention with
// MergedWithForeignChange and no receipt (endMerged). It returns what endMerged returns.
func (r *RemediationRequestReconciler) recordStoppedMerge(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest) error {
	repo, _, err := r.planEntry(ctx, rr)
	if err != nil && !errors.Is(err, errTargetBranchChanged) {
		return err
	}
	git, err := r.openRecordedBranch(ctx, repo, rr.Status.Source.TargetBranch)
	if err != nil {
		return freshness.WrapProvider("open the repository", err)
	}
	if !markedStale(rr) {
		markStaleAtMerge(rr, freshness.ReasonPlanChanged, fmt.Sprintf("The plan was stale when the merge was seen: "+
			"the request had stopped (%s) before its pull request was merged", rr.Status.Phase))
	}
	if pr := rr.Status.PullRequest; pr != nil && cr.HeadSHA != pr.HeadSHA {
		return r.mergedForeign(ctx, rr, &renderRefusal{reason: ReasonHeadMoved,
			message: "The merged head is not the head Shadewright pushed"})
	}
	return r.endMerged(ctx, git, rr, cr)
}

// recordOwnMerge records the merge of cr, the request's own pull request found merged into the recorded branch by a
// read that needs no write (an expired request's cleanup, expiredUncleaned; a cancel whose close waits, cancelLook):
// endMerged on that branch, which checks the head and what the merge brought. It returns what endMerged returns.
func (r *RemediationRequestReconciler) recordOwnMerge(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest) error {
	repo, _, err := r.planEntry(ctx, rr)
	if err != nil && !errors.Is(err, errTargetBranchChanged) {
		return err
	}
	git, err := r.openRecordedBranch(ctx, repo, rr.Status.Source.TargetBranch)
	if err != nil {
		return freshness.WrapProvider("open the repository", err)
	}
	return r.endMerged(ctx, git, rr, cr)
}

// closedOnly reports whether the status says no pull request of the request is open only because its recorded
// pull request is closed without a merge: PullRequestOpened with reason PullRequestClosed or ChangeRequestClosed,
// or Superseded (written after Shadewright's close) with a pull request recorded. Another pull request of its
// branch may still carry its change, so the status alone does not release the lock.
func closedOnly(rr *remediationv1alpha1.RemediationRequest) bool {
	if rr.Status.PullRequest == nil || slices.Contains(successPhases, rr.Status.Phase) || undecided(rr.Status.Phase) ||
		rr.Status.Phase == remediationv1alpha1.PhaseInconclusive || mergedAndEnded(rr) {
		return false
	}
	if rr.Status.Phase == remediationv1alpha1.PhaseSuperseded && !handedOff(rr) {
		return true
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPROpened)
	return c != nil && (c.Reason == ReasonPullRequestClosed || c.Reason == ReasonChangeRequestClosed)
}

// The verdicts a release of the target lock takes are built in package lockverdict (sealed: only its constructors
// build one, and Release refuses none). The functions below are the proving reads and states that call those
// constructors, and the only ones that may (TestLockSettlementBuiltByItsProofs).

// settleByRead is the verdict of the read of a stopped holder's pull requests (settleTargetLock): v what the read
// of its branch found, holding an open pull request of the branch that may still deliver the change (nil when none
// may). ok is false when the read proves nothing (open, unknown, another pull request open, or a merge the status
// does not record yet: the caller records it first, settleStopException).
func settleByRead(rr *remediationv1alpha1.RemediationRequest, v changeVerdict, holding *providers.ChangeRequest) (
	*lockverdict.Verdict, bool) {
	found := lockverdict.FoundOther
	switch v.state {
	case changeMerged:
		found = lockverdict.FoundMerged
	case changeClosed:
		found = lockverdict.FoundClosed
	case changeGone:
		found = lockverdict.FoundGone
	}
	return lockverdict.ByRead(found, mergeRecorded(rr), rr.Status.PullRequest != nil, holding != nil)
}

// settleNeverHeld is the verdict of a request that never took the lock: status.holdsTarget not set, or false. A
// request that holds it gets none.
func settleNeverHeld(rr *remediationv1alpha1.RemediationRequest) (*lockverdict.Verdict, bool) {
	return lockverdict.NeverHeld(&rr.Status)
}

// settleHandedOver is the verdict of pred, whose intent to hands its pull request over to by: to is Done and names
// by, and by holds the lock, so the target stays locked through the pull request.
func settleHandedOver(to *remediationv1alpha1.OutgoingHandoff, by *remediationv1alpha1.RemediationRequest) (
	*lockverdict.Verdict, bool) {
	return lockverdict.HandedOver(to, by.UID, holdsLock(by))
}

// settleHandoffAborted is the verdict of a replacement whose handoff did not happen: it reserved a predecessor and
// has no pull request of its own (it only ever takes the predecessor's over).
func settleHandoffAborted(rr *remediationv1alpha1.RemediationRequest) (*lockverdict.Verdict, bool) {
	return lockverdict.HandoffAborted(&rr.Status)
}

// settleRecordedMerge is the verdict of a request that no longer moves on and whose status records a merge that
// delivered the plan, as main's status rule told it less the merges that did not deliver it as shown
// (lockverdict.RecordedMerge): a phase after the merge (Merged, WaitingForDelivery, Verifying, Succeeded,
// Inconclusive), Failed with its receipt, or its recorded pull request marked merged (PullRequestOpened with reason
// ChangeRequestMerged, also Superseded after a handoff done), and that no verification holds (tenth decision of
// 2026-11-18 on #771, points 3 and 4). The recorded merge settles the lock by itself, without a read of the branch, so
// neither a pause nor a repository removed from the settings keeps it. A merge that did not deliver the plan as shown
// (undeliveredMerge) or whose read is still to be made (openReadWait) gets none: the read of the branch settles it.
func (r *RemediationRequestReconciler) settleRecordedMerge(rr *remediationv1alpha1.RemediationRequest) (
	*lockverdict.Verdict, bool) {
	if progresses(rr) {
		return nil, false
	}
	return lockverdict.RecordedMerge(&rr.Status, markedMerged(rr), undeliveredMerge(rr) || openReadWait(rr),
		handedOff(rr), heldForVerification(rr, r.VerifyOutcomes))
}

// settleUndeliveredGone is the verdict of a merge that did not deliver the plan as shown (undeliveredMerge) whose
// look found the repository of the plan gone from the settings (err from lookHeld): no read can show another open
// pull request of its branch any more, so it is released (tenth decision of 2026-11-18 on #771, point 3). A settings
// object that is missing (errNoSettings) is not that: the lock is kept until the settings exist again (eleventh
// decision, point 3).
func settleUndeliveredGone(rr *remediationv1alpha1.RemediationRequest, err error) (*lockverdict.Verdict, bool) {
	return lockverdict.UndeliveredRepositoryGone(undeliveredMerge(rr),
		errors.Is(err, errRepositoryGone) && !errors.Is(err, errNoSettings))
}

// undeliveredReasons are the reasons of Approved False of a request that ended NeedsIntervention on a merge that did
// not deliver the plan as shown: it brought another change, what it brought could not be read, or it could not be
// checked against the plan. The rule of 2026-11-17 on #771 holds for them: the lock is released only after a read
// shows that no other open pull request of the branch can still deliver the change (tenth decision of 2026-11-18,
// point 3, and the eleventh, point 2, for MergeNotCheckable, a stop main does not have).
var undeliveredReasons = map[string]bool{ReasonMergedWithForeignChange: true, ReasonMergeContentUnreadable: true,
	ReasonMergeNotCheckable: true}

// undeliveredMerge reports whether rr ended NeedsIntervention on a merge that did not deliver the plan as shown
// (undeliveredReasons).
func undeliveredMerge(rr *remediationv1alpha1.RemediationRequest) bool {
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionApproved)
	return rr.Status.Phase == remediationv1alpha1.PhaseNeedsIntervention && c != nil &&
		c.Status == metav1.ConditionFalse && undeliveredReasons[c.Reason]
}

// openReadWait reports whether rr records a wait for a read (readWaits: the merge check, the content of a retargeted
// merge) that still applies: its merge is not recorded, and the read that would record it is still to be made.
func openReadWait(rr *remediationv1alpha1.RemediationRequest) bool {
	for _, k := range readWaits {
		if gitWait(rr, k) != nil && k.applies(rr) {
			return true
		}
	}
	return false
}

// markedMerged reports whether rr's recorded pull request is marked merged in PullRequestOpened (markClosed with
// reason ChangeRequestMerged), as main's status rule read it (prMayBeOpen).
func markedMerged(rr *remediationv1alpha1.RemediationRequest) bool {
	if rr.Status.PullRequest == nil {
		return false
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPROpened)
	return c != nil && c.Reason == ReasonChangeRequestMerged
}

// settleRecorded is what main's settleLock did in every write that ends a phase: in the status about to be written,
// it releases the lock of a request whose status records a merge that delivered the plan (settleRecordedMerge), and
// removes condition PullRequestUnreadable of a request that is settled (clearReleasedRead). It reads nothing, so the
// write that records the merge or ends the verification releases the lock under a pause too. A request held for its verification keeps
// the lock until that ends. Every other release needs its read (checkHeld), the end of a merge that did not deliver
// the plan as shown (MergedWithForeignChange, MergeContentUnreadable, MergeNotCheckable) among them.
func (r *RemediationRequestReconciler) settleRecorded(rr *remediationv1alpha1.RemediationRequest) {
	if s, ok := r.settleRecordedMerge(rr); ok && ptr.Deref(rr.Status.HoldsTarget, false) {
		// Release refuses only a nil verdict, which ok rules out.
		_ = lockverdict.Release(&rr.Status, s)
	}
	clearReleasedRead(rr)
}

// mergeRecorded reports whether the status records the merge of the request's pull request: a merge receipt, a
// phase only a recorded merge reaches (Merged and after it, Inconclusive), Failed with a receipt, or the end of a
// merge that brought another change than the plan's (MergedWithForeignChange, MergeContentUnreadable,
// MergeNotCheckable: no receipt by design). PullRequestOpened with reason ChangeRequestMerged alone is not one: recordFound sets it on what a read
// found, before anything records the merge.
func mergeRecorded(rr *remediationv1alpha1.RemediationRequest) bool {
	if rr.Status.MergeReceipt != nil || slices.Contains(successPhases, rr.Status.Phase) || undecided(rr.Status.Phase) ||
		rr.Status.Phase == remediationv1alpha1.PhaseInconclusive || mergedAndEnded(rr) {
		return true
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionApproved)
	return rr.Status.Phase == remediationv1alpha1.PhaseNeedsIntervention && c != nil && c.Status == metav1.ConditionFalse &&
		uncountedMergeReasons[c.Reason]
}

// settleReleasedBeforeField is the verdict of a request decided before status.holdsTarget existed whose status
// shows the lock free (derivedHold false). A request with the field set, or one derivedHold holds, gets none, so it
// can never clear a held lock.
func settleReleasedBeforeField(rr *remediationv1alpha1.RemediationRequest) (*lockverdict.Verdict, bool) {
	return lockverdict.ReleasedBeforeField(&rr.Status, derivedHold(rr))
}

// acquire takes the lock on the request's target in the status about to be written: the policy decision that
// admits it (reconcile), or the backfill of a request decided before status.holdsTarget existed that derivedHold
// holds. With lockverdict.Release it is the only writer of status.holdsTarget (TestLockClearedInOnePlace).
func acquire(rr *remediationv1alpha1.RemediationRequest) {
	rr.Status.HoldsTarget = new(true)
}

// checkHeld persists the common target-lock decision for a stopped holder. Branch-change and Incident-cancel
// merges pass through settleStopException before any release is stored. A successful look is repeated every
// heldRecheckInterval while the lock stays held; Git failures use retryGitRead, Kubernetes errors remain errors.
func (r *RemediationRequestReconciler) checkHeld(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	reading := openReadWait(rr)
	if (!ptr.Deref(rr.Status.HoldsTarget, false) && !reading) || progresses(rr) ||
		heldForVerification(rr, r.VerifyOutcomes) {
		// A merged request in verification keeps the lock without a look-up: its end settles it. A stopped request
		// with an open wait for a read makes that read whether or not it still holds the lock (tenth decision of
		// 2026-11-18 on #771, point 2), so a merge commit that came meanwhile still gets its receipt.
		return reconcile.Result{}, nil
	}
	held := ptr.Deref(rr.Status.HoldsTarget, false)
	if held && expiredCleanupOver(rr, r.now()) {
		return r.closeExpiredOnceMore(ctx, rr)
	}
	if s, ok := r.settleRecordedMerge(rr); ok && held {
		// The recorded merge settles the lock: no read of the branch, so neither a repository that cannot be opened
		// nor a failing Git host keeps it. A pause does not hold it, as on main, where the release from recorded status
		// reads nothing either. The writes that record the merge or end the verification release it already
		// (settleRecorded); this one settles a lock one of them left, from a status written before. No wait for a read
		// is open: a merge whose read is still to be made gets no verdict (settleRecordedMerge).
		return reconcile.Result{}, r.releaseUnread(ctx, rr, s, "its merge is recorded")
	}
	before := rr.Status.DeepCopy()
	lookCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	v, holding, err := r.settleTargetLock(lookCtx, rr)
	if stepErr := err; lookCtx.Err() != nil {
		if stepErr == nil && v.state == changeUnknown {
			stepErr = v.err
		}
		// A read of the pull request that ran out of the look's time is still a failed Git read: a paced wait.
		err = keepGitRead(ctx, lookCtx, "look-up of the change request", stepErr)
	}
	cancel()
	if ctx.Err() != nil {
		return reconcile.Result{}, err
	}
	if errors.Is(err, freshness.ErrPaused) {
		// The kill switch holds the look: planEntry reads the settings live and returns ErrPaused before any read of
		// the Git host. The lock is kept, WritesPaused says why, and the look runs again after freshRecheckDelay,
		// never as an error for the rate limiter. The first look after the pause settles the lock and removes the
		// condition (resume below, or settleCancelled for a request cancelled for its Incident).
		return r.waitWhilePaused(ctx, rr)
	}
	if s, ok := settleUndeliveredGone(rr, err); ok && held {
		// The repository left the settings: the read that would show no other open pull request of the branch cannot
		// start, and nobody can see such a pull request any more.
		return reconcile.Result{}, r.releaseUnread(ctx, rr, s, "its merge did not deliver the plan as shown and its "+
			"repository is not in the settings any more")
	}
	c := repositoryRead(rr, err)
	if errors.Is(err, errBranchChangedUnread) {
		// The recorded branch of a request stopped with TargetBranchChanged could not be opened (lookHeld).
		c = branchChangedOpenRead(rr, err)
	}
	if c != nil {
		// No read can be made: the lock is kept, and condition PullRequestUnreadable says why as the settings stand
		// now (once per reason), until the repository can be opened again (changeOf removes it then) or the
		// request is deleted.
		if werr := r.setUnreadable(ctx, rr, c); werr != nil {
			return reconcile.Result{}, werr
		}
	}
	if isClusterError(err) || isClusterError(v.err) {
		return reconcile.Result{}, errors.Join(err, v.err)
	}
	if err != nil || v.state == changeUnknown {
		return r.heldUnsettled(ctx, rr, v, err, c != nil)
	}
	if res, done, err := r.settleStopException(ctx, rr, v); done {
		return res, err
	}
	// No pull request of an expired request's own is open any more: a close that was failing is over.
	closeNoLongerFailing(rr)
	reason, cancelled := cancelledForIncident(rr)
	if cancelled {
		// Closed (by Shadewright, its answer lost, or by a person) or not this request's: no close is left to make,
		// so a failed close is not left to say.
		clearNotClosed(rr)
	}
	// The read ran through settings that are not paused: a pause that held an earlier look is over.
	resume(rr)
	if v.state != changeGone {
		recordFound(rr, v.pr)
		if cancelled {
			// recordFound rewrote PullRequestOpened, where the reason of the cancel was while no pull request was
			// recorded: it moves to Approved in the same write, as for one found open (recordCancelledOpen).
			keepCancelReason(rr, reason, "found closed on the branch afterwards")
		}
	}
	free := !ptr.Deref(rr.Status.HoldsTarget, false)
	if equality.Semantic.DeepEqual(before, &rr.Status) {
		return reconcile.Result{RequeueAfter: heldRecheckInterval}, nil
	}
	if err := r.Status().Update(ctx, rr); err != nil {
		return reconcile.Result{}, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	return r.heldLookResult(ctx, rr, free, held, holding, v.pr), nil
}

// releaseUnread is checkHeld's release of the lock of rr by a verdict that needs no read of the branch (a recorded
// merge, or an undelivered merge whose repository left the settings), in one status write that also removes
// PullRequestUnreadable; why says what proves it, for the log.
func (r *RemediationRequestReconciler) releaseUnread(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	v *lockverdict.Verdict, why string) error {
	if err := lockverdict.Release(&rr.Status, v); err != nil {
		return err
	}
	clearReleasedRead(rr)
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	logf.FromContext(ctx).Info("Released the target lock of RemediationRequest: "+why, "remediationRequest",
		client.ObjectKeyFromObject(rr))
	return nil
}

// closeExpiredOnceMore is checkHeld's step for an expired request holding its lock whose cleanup passed its cutoff,
// decided from recorded state before any read. Nothing ends during a pause: the cutoff waits for it like the close it
// bounds. Past it one more close is tried (ninth decision of 2026-11-18 on #771, point 2): if it fails, by a write or
// by a read, the cleanup ends with PullRequestDeadlineCleanupFailed (expiredCloseFailed); if it works, the next look
// settles the lock.
func (r *RemediationRequestReconciler) closeExpiredOnceMore(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) (reconcile.Result, error) {
	switch paused, err := r.writesPaused(ctx); {
	case err != nil:
		return reconcile.Result{}, err
	case paused:
		return r.waitWhilePaused(ctx, rr)
	}
	logf.FromContext(ctx).Info("Trying the close of an expired RemediationRequest once more: its cleanup passed its "+
		"cutoff", "remediationRequest", client.ObjectKeyFromObject(rr))
	return r.cleanUpExpired(ctx, rr)
}

// heldLookResult is checkHeld's answer after a look that settled the lock and whose status write went through: free
// whether the lock is free now, held whether it was held before the look, holding another open pull request of the
// branch that keeps it, pr the pull request the look found.
func (r *RemediationRequestReconciler) heldLookResult(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	free, held bool, holding *providers.ChangeRequest, pr providers.ChangeRequest) reconcile.Result {
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr))
	switch {
	case free && !held:
		// The look was the read of an open wait of a request that holds no lock any more.
		return reconcile.Result{RequeueAfter: r.withinWaits(rr, heldRecheckInterval)}
	case free:
		log.Info("Released the target lock of RemediationRequest: none of its pull requests is open")
		return reconcile.Result{}
	case holding != nil:
		log.Info("Kept the target lock of RemediationRequest: a pull request from its branch is open",
			"pullRequest", holding.Number, "base", holding.BaseBranch)
	default:
		log.Info("Recorded the open pull request of a stopped RemediationRequest", "pullRequest", pr.Number)
	}
	return reconcile.Result{RequeueAfter: heldRecheckInterval}
}

// heldUnsettled is checkHeld's answer to a look that settled nothing: err is the look's failure, v its verdict, and
// said whether PullRequestUnreadable already says why no read can be made. Answers that disagree
// (errConflictingReads) or a 404 the branch does not confirm are no failed call: the lock is kept and looked at
// again with nothing written: every read behind such a verdict was answered. A failed Git read is retried visibly
// (retryGitRead), never sooner than heldRecheckInterval, the pace of a stopped holder's looks. Any other error without a condition is returned.
func (r *RemediationRequestReconciler) heldUnsettled(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	v changeVerdict, err error, said bool) (reconcile.Result, error) {
	if err == nil {
		err = v.err
	}
	switch {
	case v.state == changeUnknown && !said && errors.Is(err, v.err) && answeredUnknown(v.err):
		return reconcile.Result{RequeueAfter: r.withinWaits(rr, heldRecheckInterval)}, nil
	case gitReadFailure(err):
		res, err := r.retryGitRead(ctx, rr, err, unboundedRead(noBoundHeldLock))
		if err != nil {
			return reconcile.Result{}, err
		}
		// The pace of a stopped holder's looks, never past the end of a wait it records (the content wait).
		res.RequeueAfter = r.withinWaits(rr, max(res.RequeueAfter, heldRecheckInterval))
		return res, nil
	case !said && err != nil:
		return reconcile.Result{}, err
	}
	// Never past the end of a wait the request records (the content wait): its end decides from the entry alone.
	return reconcile.Result{RequeueAfter: r.withinWaits(rr, heldRecheckInterval)}, nil
}

// settleTargetLock is the only decision that releases a lock held for Git work.
// Both branches must answer, and no open pull request may still deliver the change.
// It changes only the in-memory lock and TargetHeld; the caller persists its phase and these fields together.
func (r *RemediationRequestReconciler) settleTargetLock(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	changeVerdict, *providers.ChangeRequest, error) {
	v, holding, err := r.lookHeld(ctx, rr)
	if err != nil || v.state == changeUnknown {
		return v, holding, err
	}
	if s, ok := settleByRead(rr, v, holding); ok && !heldForVerification(rr, r.VerifyOutcomes) {
		if err := lockverdict.Release(&rr.Status, s); err != nil {
			return v, holding, err
		}
	} else if holding != nil {
		msg := fmt.Sprintf("Pull request #%d is open on the request's branch and may still deliver its change; the target stays locked until it is closed or merged", holding.Number)
		if v.movedTo != "" && holding.BaseBranch == string(v.movedTo) {
			msg = fmt.Sprintf("Pull request #%d of the request is open on base branch %s, where it was moved, and may still merge; the target stays locked until it is closed or merged", holding.Number, v.movedTo)
		}
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
			Type: remediationv1alpha1.ConditionTargetHeld, Status: metav1.ConditionTrue, Reason: ReasonOpenOnBranch,
			Message: msg, ObservedGeneration: rr.Generation,
		})
	} else {
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionTargetHeld)
	}
	return v, holding, nil
}

// lookHeld reads both the recorded and configured branches of the plan's repository. When neither shows the
// request's own pull request, it also lists the open pull requests from its branch to every base (#892), and the
// request's own one open on another base joins the open listings. They are combined before openHolding decides
// whether another request already holds any of those pull requests.
func (r *RemediationRequestReconciler) lookHeld(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	changeVerdict, *providers.ChangeRequest, error) {
	if r.OpenRepository == nil {
		return changeVerdict{}, nil, errRepositoryGone
	}
	if rr.Status.PlanRepository == nil || rr.Status.Source == nil {
		return changeVerdict{}, nil, errRepositoryGone
	}
	repo, reinstalled, err := r.planEntry(ctx, rr)
	changed := errors.Is(err, errTargetBranchChanged) && rr.Status.Source != nil
	if err != nil && !changed {
		return changeVerdict{}, nil, err
	}
	var verdict changeVerdict
	var open []providers.ChangeRequest
	var git providers.GitProvider
	branches := []remediationv1alpha1.GitBranch{rr.Status.Source.TargetBranch}
	if changed {
		branches = append([]remediationv1alpha1.GitBranch{repo.TargetBranch}, branches...)
	}
	for _, branch := range branches {
		if changed {
			git, err = r.openRecordedBranch(ctx, repo, branch)
		} else {
			git, _, err = r.openPlanEntry(ctx, rr, repo, reinstalled)
		}
		if err != nil {
			if providers.IsInvalidConfig(err) || errors.Is(err, freshness.ErrRepositoryUnusable) {
				// openPlanEntry has mapped an entry that can never open a provider already.
				return changeVerdict{}, nil, freshness.ErrRepositoryUnusable
			}
			if changed {
				// Either branch of a changed target: checkHeld says so in PullRequestUnreadable (branchChangedOpenRead).
				return changeVerdict{}, nil, fmt.Errorf("%w: %w", errBranchChangedUnread,
					freshness.WrapProvider("open the repository", err))
			}
			// The token step, named so releaseReinstalled can say a failure of the new installation (#797).
			return changeVerdict{}, nil, &stepFailure{step: stepToken, err: freshness.WrapProvider("open the repository", err)}
		}
		q := changeQuery{marker: !closedOnly(rr), openOnBranch: true, reinstalled: reinstalled}
		switch {
		case changed && branch == rr.Status.Source.TargetBranch && verdict.state == changeGone:
			// The configured branch does not show the pull request either: the recorded branch confirms a missing
			// pull request as before #771 (settleGone), so one 404 the listing of the branch does not confirm yet
			// keeps the lock instead of releasing it on two unconfirmed answers.
			q.recordedBranch = true
		case changed:
			// The configured branch (read first), or the recorded one after the configured branch showed the
			// pull request there: a 404 on the recorded branch is then the move itself, not a missing pull request.
			q.configuredBranch = branch
		}
		v := r.changeOf(ctx, git, rr, q)
		if v.state == changeUnknown {
			return v, nil, nil
		}
		open = append(open, v.open...)
		// Prefer the request's own merge, then its own open or closed PR, over a missing answer on the other base.
		if verdict.state == changeUnknown || verdict.state == changeGone || v.state == changeMerged {
			verdict = v
		}
	}
	if failed, ok := r.openElsewhere(ctx, git, rr, branches, reinstalled, &verdict, &open); !ok {
		return failed, nil, nil
	}
	if err := r.disproveRepositoryReason(ctx, rr, changed); err != nil {
		return changeVerdict{}, nil, err
	}
	holding, err := r.openHolding(ctx, rr, open)
	if holding != nil && verdict.state == changeOpen && holding.Number == verdict.pr.Number {
		holding = nil
	}
	if err != nil {
		return verdict, nil, &clusterError{err: err}
	}
	return verdict, holding, nil
}

// openElsewhere is lookHeld's read after neither target branch in read showed the request's own pull request (or,
// with none recorded, one with its marker), when *verdict is gone: a person may have moved its base to another
// branch (#892). An open pull request can still merge, so it keeps the lock wherever it is: it joins *open, and
// verdict.movedTo names its base. Once it is closed or merged there, the listing no longer has it, and the lock goes.
// A merge there is not recorded as the request's: it delivers nothing the plan was made for. A failed listing returns
// its unknown verdict and false: the lock is kept, and the look retried as a failed Git read.
func (r *RemediationRequestReconciler) openElsewhere(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, read []remediationv1alpha1.GitBranch, reinstalled bool,
	verdict *changeVerdict, open *[]providers.ChangeRequest) (changeVerdict, bool) {
	if verdict.state != changeGone {
		return changeVerdict{}, true
	}
	elsewhere := r.changeOf(ctx, git, rr, changeQuery{otherBases: read, reinstalled: reinstalled})
	switch elsewhere.state {
	case changeUnknown:
		return elsewhere, false
	case changeOpen:
		*open = append(*open, elsewhere.pr)
		verdict.movedTo = remediationv1alpha1.GitBranch(elsewhere.pr.BaseBranch)
	}
	return changeVerdict{}, true
}

// disproveRepositoryReason is lookHeld's removal of condition PullRequestUnreadable with a reason about the
// repository after both branches of a changed target were read through the repository entry: such a reason (it
// left the settings, could not be used) is disproved, as noteRead removes it after a read on one branch. A failed
// write is a *clusterError.
func (r *RemediationRequestReconciler) disproveRepositoryReason(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, changed bool) error {
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
	if !changed || c == nil || !repositoryReasons[c.Reason] {
		return nil
	}
	if err := r.setUnreadable(ctx, rr, nil); err != nil {
		return &clusterError{err: err}
	}
	return nil
}

// ReasonOpenOnBranch is the reason of condition TargetHeld: a pull request from the request's branch is open and
// may still deliver its change, although the request's own is closed or gone, or the request's own is open on a
// base a person moved it to (#892).
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

// recordable reports whether cr passes the schema of status.pullRequest (recordFound).
func recordable(cr providers.ChangeRequest) bool {
	return shaPattern.MatchString(cr.HeadSHA) && cr.Number > 0 && strings.HasPrefix(cr.URL, "https://") &&
		len(cr.URL) <= 2048
}

// recordFound writes what a look-up found into the status: the pull request when none is recorded (and its
// number, URL and head pass the schema), and PullRequestOpened as closed or merged when it is no longer open,
// unless that condition already says it is (a close by Shadewright keeps its own reason).
func recordFound(rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest) {
	if rr.Status.PullRequest == nil && recordable(cr) {
		recordNumber(rr, remediationv1alpha1.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA})
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
