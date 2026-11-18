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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// The wait of a handoff recovery that cannot read (#786, decisions of 2026-11-16 and 2026-11-17). A final refusal of
// the Git host, or settings that cannot reach the plan's repository, no longer ends a replacement whose commit may
// already be on the pull request it was to take over. The recovery waits on the shared path of Git waits
// (gitwait.go): condition PullRequestUnreadable says why for people (one Warning Event per change of reason), and the
// wait is the HandoffGrace entry of status.gitWaits, recorded and paced by retryGitRead. Its end is
// recoveryWaitBound after the first refusal (handoffGraceEnd), read from recorded state alone; the check at the start
// of every reconcile ends it (endPassedWait, endRecoveryFailed, endRecoveryWait), so no error, timeout or early return
// of the recovery skips it. The end aborts the handoff and keeps the target lock; the lock is released only by a
// verdict of lockverdict, after the read that settles it (checkHeld, heldRecovery first).

// recoveryWaitBound is how long a recovery waits after its first refusal: the bound of a cancel and of a cleanup.
const recoveryWaitBound = cleanupFailingBound

// ReasonRecoveryWaitExpired is the reason of NeedsIntervention (condition PullRequestOpened False) when the
// recovery of a handoff waited recoveryWaitBound since its first refusal without a read that works (#786, decision of
// 2026-11-17, endRecoveryWait). Condition PullRequestUnreadable stays and names what is refused.
const ReasonRecoveryWaitExpired = "RecoveryWaitExpired"

// ReasonRecoveryReadFailed is the reason of condition PullRequestUnreadable on a replacement whose handoff recovery
// waits (#786) when the cause its condition named is gone (the repository opens, or the refused read is answered)
// and another read of the recovery fails now: the condition names that read (recoveryReadFailed). Only the recovery
// sets it, so it keeps the request in the wait (watchedRecovery) as the reason it replaced did.
const ReasonRecoveryReadFailed = "RecoveryReadFailed"

// recoveryWaitReasons are the reasons of condition PullRequestUnreadable that keep a recovery waiting
// (watchedRecovery): a final refusal, settings that cannot reach the plan's repository (repositoryReasons, and a
// settings object that does not exist), and ReasonRecoveryReadFailed, which only replaces one of those.
var recoveryWaitReasons = map[string]bool{
	ReasonGitForbidden: true, ReasonGitRejected: true, ReasonRecoveryReadFailed: true, ReasonSettingsNotFound: true,
	reasonRepositoryUnavailable: true, reasonTargetBranchChanged: true, reasonRepositoryUnusable: true,
}

// handoffGraceEnd is when a HandoffGrace wait of rr ends: handoffRecoveryGrace after its deadline, or, for a recovery
// that waits (watchedRecovery), recoveryWaitBound after the start of that wait (the condition's lastTransitionTime,
// set from the executor's clock by setRecoveryWait) when that is later. Both are recorded state written by the
// executor; ok is false when rr records neither.
func handoffGraceEnd(rr *remediationv1alpha1.RemediationRequest) (time.Time, bool) {
	var end time.Time
	ok := false
	if rr.Status.Deadline != nil {
		end, ok = rr.Status.Deadline.Add(handoffRecoveryGrace), true
	}
	if c := watchedRecovery(rr); c != nil && !c.LastTransitionTime.IsZero() {
		if e := c.LastTransitionTime.Add(recoveryWaitBound); !ok || e.After(end) {
			end, ok = e, true
		}
	}
	return end, ok
}

// recoveryStepErr is keepGitRead for the step of the recovery: a Git read that ran out of the step's time stays that
// read, so its wait is recorded and paced like any other failure of it, and the condition of a recovery that waits
// names it. A failure of the Kubernetes API that ran into the deadline stays marked as the cluster's (*clusterError).
func recoveryStepErr(parent, step context.Context, err error) error {
	kept := keepGitRead(parent, step, "recovery of the handoff", err)
	if kept != nil && isClusterError(err) && !isClusterError(kept) {
		return &clusterError{err: kept}
	}
	return kept
}

// clusterSideErr marks err, the failure of a read or write of the Kubernetes API, as a *clusterError. nil, the
// end of a request (errStopped) and an error already marked are returned as they are.
func clusterSideErr(err error) error {
	if err == nil || errors.Is(err, errStopped) || isClusterError(err) {
		return err
	}
	return &clusterError{err: err}
}

// recoveryRead is err as a failed Git read for retryGitRead: settings that cannot reach the plan's repository are
// the open of the repository, which no read could pass, and a failure of a named provider call keeps that name. Any
// other error, a failure of the Kubernetes API included, is returned as it is.
func recoveryRead(err error) error {
	switch {
	case err == nil, isClusterError(err), gitReadFailure(err):
		return err
	case errors.Is(err, errRepositoryGone), errors.Is(err, errTargetBranchChanged),
		errors.Is(err, freshness.ErrRepositoryUnusable):
		return readFailure("open the repository", err)
	}
	if pe, ok := errors.AsType[*freshness.ProviderError](err); ok {
		return readFailure(pe.Op, err)
	}
	return err
}

// nowUnreadable is condition PullRequestUnreadable for err, a failure of a recovery that waits on c (watchedRecovery)
// and that is neither a refusal, nor settings that cannot reach the repository, nor a failure of the Kubernetes API,
// when the cause c names is gone: nil while it may still hold. A reason about the repository is gone, since the
// settings entry was read and is fine again (recoveryUnreadable would have named it otherwise), and the failure is
// the open of the provider or a read after it; a refusal holds while the read it names is the one failing now;
// ReasonRecoveryReadFailed holds while it names the read failing now. Otherwise the condition is replaced by
// recoveryReadFailed, which names that read (#786, decisions of 2026-11-16). It reads the status and err only.
func nowUnreadable(rr *remediationv1alpha1.RemediationRequest, c *metav1.Condition, err error) *metav1.Condition {
	want := recoveryReadFailed(rr, err)
	switch {
	case c.Reason == ReasonGitForbidden || c.Reason == ReasonGitRejected:
		// The read is named as retryGitRead names it, also one that ran out of the step's time (keepGitRead).
		named := err
		if gitReadFailure(err) {
			named = freshness.WrapProvider(readOp(err), err)
		}
		if refusedRead(rr, c.Reason, named, 0).Message == c.Message {
			return nil
		}
	case c.Reason == ReasonRecoveryReadFailed && c.Message == want.Message:
		return nil
	}
	return want
}

// recoveryReadFailed is condition PullRequestUnreadable with ReasonRecoveryReadFailed for err, a failed read of a
// recovery that waits. It names the read by its fixed operation text (readOp), never the host's answer.
func recoveryReadFailed(rr *remediationv1alpha1.RemediationRequest, err error) *metav1.Condition {
	what := "read the branch and the pull request of the handoff"
	if gitReadFailure(err) {
		what = readOp(err)
	}
	return &metav1.Condition{
		Type:   remediationv1alpha1.ConditionPullRequestUnreadable,
		Status: metav1.ConditionTrue,
		Reason: ReasonRecoveryReadFailed,
		Message: "The attempt to " + what + " for the recovery of the handoff failed; it is read again, and " +
			"nothing is decided on it until a read works",
		ObservedGeneration: rr.Generation,
	}
}

// recoveryUnreadable is condition PullRequestUnreadable for err, a failure of recoverStep that the recovery waits
// out instead of ending the request (#786): a final refusal of the Git host, of a read of the branch or of a pull
// request alike (refusedRead, which names the refused read by its operation text), or settings that cannot reach
// the plan's repository (repositoryRead). Any other failure gives nil.
func recoveryUnreadable(rr *remediationv1alpha1.RemediationRequest, err error) *metav1.Condition {
	if reason, _, final := refusal(err); final {
		return refusedRead(rr, reason, err, 0)
	}
	return repositoryRead(rr, err)
}

// watchedRecovery is condition PullRequestUnreadable of rr when it records a recovery that waits (waitRecovery): one
// with a reason of recoveryWaitReasons. Any other, or none, gives nil. It reads the status only, never a time.
func watchedRecovery(rr *remediationv1alpha1.RemediationRequest) *metav1.Condition {
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
	if c == nil || c.Status != metav1.ConditionTrue || !recoveryWaitReasons[c.Reason] {
		return nil
	}
	return c
}

// waitRecovery starts or continues the wait of a recovery that cannot read (#786): condition PullRequestUnreadable
// (want) says why (setRecoveryWait), and the wait is the HandoffGrace entry of status.gitWaits, recorded and paced by
// retryGitRead (the poll interval, or the delay the host asked for when longer, never past the end of the wait). A
// failure of the Kubernetes API joined to err is returned once the condition is written, for the error backoff; a
// failed write is returned with err.
func (r *RemediationRequestReconciler) waitRecovery(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	want *metav1.Condition, err error) (reconcile.Result, error) {
	if werr := r.setRecoveryWait(ctx, rr, want); werr != nil {
		return reconcile.Result{}, errors.Join(err, werr)
	}
	if isClusterError(err) {
		return reconcile.Result{}, err
	}
	logf.FromContext(ctx).Info("Could not recover the handoff of RemediationRequest; it waits for a read that works",
		"remediationRequest", client.ObjectKeyFromObject(rr), "reason", want.Reason, "error", err.Error())
	return r.retryGitRead(ctx, rr, recoveryRead(err), boundBy(waitHandoffGrace))
}

// waitingRecovery is retryRecovery for err, a failure of a recovery that waits on c (watchedRecovery, #786, decision
// of 2026-11-16: sort by where the error comes from, and let nothing unknown end a waiting recovery). It never ends
// the request; only the end of the HandoffGrace wait does (endPassedWait):
//   - the end of the reconcile's own context is returned as it is;
//   - a final refusal of the Git host, or settings that cannot reach the repository, continues the wait
//     (waitRecovery) with the condition that names it;
//   - a failure of the Kubernetes API (marked where it is returned) goes to the error backoff, and the condition
//     stays as it is: it names a read of the Git host, never a read of the cluster;
//   - a failure of the Git host worth retrying (a timeout, a 5xx, a rate limit) or a read that ran into the step's own
//     deadline (ErrTimeout) first gets a condition that names the read failing now (nowUnreadable), then continues
//     the wait (retryGitRead);
//   - every other failure, one not recognised included, goes to the error backoff, so it is logged and counted.
func (r *RemediationRequestReconciler) waitingRecovery(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	c *metav1.Condition, err error) (reconcile.Result, error) {
	if ctx.Err() != nil {
		return reconcile.Result{}, err
	}
	if want := recoveryUnreadable(rr, err); want != nil {
		return r.waitRecovery(ctx, rr, want, err)
	}
	if isClusterError(err) {
		return reconcile.Result{}, err
	}
	if !providers.IsRetryable(err) && !errors.Is(err, ErrTimeout) {
		logf.FromContext(ctx).Info("Could not recover the handoff of RemediationRequest; it still waits, retried "+
			"with the error backoff", "remediationRequest", client.ObjectKeyFromObject(rr), "reason", c.Reason,
			"error", err.Error())
		return reconcile.Result{}, err
	}
	if want := nowUnreadable(rr, c, err); want != nil {
		if werr := r.setUnreadable(ctx, rr, want); werr != nil {
			return reconcile.Result{}, errors.Join(err, werr)
		}
	}
	return r.retryGitRead(ctx, rr, recoveryRead(err), boundBy(waitHandoffGrace))
}

// cancelRecovery is recoverHandoff's answer to err, a failure of the recovery of a request whose spec.cancel is set:
// the cancel's (cancelRecoveryFailed, which never ends the request, #289). A refusal or settings that cannot reach the
// repository, and a failure worth retrying while the recovery waits, also start or continue the recovery's own wait
// (retryRecovery), so its end holds for a cancelled request too (#786); the cancel's pace is then never later than
// the end of the wait (withinWaits). A failed write of the wait is returned.
func (r *RemediationRequestReconciler) cancelRecovery(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	err error) (reconcile.Result, error) {
	if recoveryWaits(ctx, rr, err) {
		if _, werr := r.retryRecovery(ctx, rr, err); werr != nil {
			return reconcile.Result{}, werr
		}
	}
	res, err := r.cancelRecoveryFailed(ctx, rr, err)
	if err == nil && res.RequeueAfter > 0 {
		res.RequeueAfter = r.withinWaits(rr, res.RequeueAfter)
	}
	return res, err
}

// recoveryWaits reports whether err, a failure of the recovery of rr, starts or continues its wait (retryRecovery
// records it and returns no error then): a refusal or settings that cannot reach the repository, or, while the
// recovery waits, a failure of the Git host worth retrying or a read past the step's deadline. A pause, the caller's
// cancellation and a failure of the Kubernetes API do not.
func recoveryWaits(ctx context.Context, rr *remediationv1alpha1.RemediationRequest, err error) bool {
	if ctx.Err() != nil || errors.Is(err, freshness.ErrPaused) || isClusterError(err) {
		return false
	}
	return recoveryUnreadable(rr, err) != nil ||
		(watchedRecovery(rr) != nil && (providers.IsRetryable(err) || errors.Is(err, ErrTimeout)))
}

// setRecoveryWait writes want, condition PullRequestUnreadable of a recovery that waits (setUnreadable, one Warning
// Event per change of reason). The write that starts the wait (no watchedRecovery before it) sets its
// lastTransitionTime to the executor's clock, also over a condition that was True for another reason, so the end of
// the wait (handoffGraceEnd) counts from the first refusal; a later change of reason keeps that time.
func (r *RemediationRequestReconciler) setRecoveryWait(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	want *metav1.Condition) error {
	if watchedRecovery(rr) == nil {
		want = want.DeepCopy()
		want.LastTransitionTime = metav1.NewTime(r.now().Truncate(time.Second))
	}
	return r.setUnreadable(ctx, rr, want)
}

// dropRecoveryWait removes condition PullRequestUnreadable from rr, a request that ends, when the recovery of its
// handoff set it with a reason that says the read is made again (a refusal, or ReasonRecoveryReadFailed): only a
// replacement with no pull request of its own is recovering, and an ended request recovers nothing, so the message
// would no longer hold. A request that keeps the lock on its target says what its own reads find from then on
// (checkHeld, noteRead). A reason about the repository stays: it says why nothing can be read, which still holds.
func dropRecoveryWait(rr *remediationv1alpha1.RemediationRequest) {
	if rr.Spec.Replaces == nil || rr.Status.Handoff == nil || rr.Status.Handoff.From == nil ||
		rr.Status.PullRequest != nil {
		return
	}
	switch c := watchedRecovery(rr); {
	case c == nil, repositoryReasons[c.Reason], c.Reason == ReasonSettingsNotFound, recoveryWaitExpired(rr):
		// The end at the bound of the wait keeps it: that request reads on while it keeps the lock (heldRecovery).
		return
	}
	meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
}

// endRecoveryWait ends a replacement whose recovery waited on c (watchedRecovery) until the end of its HandoffGrace
// wait, recoveryWaitBound after its first refusal (#786, decision of 2026-11-17). While the handoff's intent stands,
// the request it replaces runs no freshness check and no merge gate, and its deletion waits; the bound keeps that
// from lasting. The end is one status write: NeedsIntervention with ReasonRecoveryWaitExpired, the handoff Aborted
// (declineOnEnd), so the predecessor aborts its intent on its next look (settleHandoff), checks its pull request
// again and its finalizer no longer waits (handoffHolds). The target lock is kept: no verdict proves a release while
// a pull request of the chain may be open, and the look that settles it (checkHeld, heldRecovery) reads on and
// records a merge of the request's commit. PullRequestOpened is set before stop, so that the end keeps condition
// PullRequestUnreadable (dropRecoveryWait reads it).
func (r *RemediationRequestReconciler) endRecoveryWait(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	c *metav1.Condition) error {
	logf.FromContext(ctx).Info("Ended the recovery of the handoff of RemediationRequest: it waited out its bound",
		"remediationRequest", client.ObjectKeyFromObject(rr), "reason", c.Reason)
	msg := fmt.Sprintf("The recovery of the handoff could not read for %s (%s, see condition %s), so the "+
		"handoff is aborted and the request it replaces is checked again; this request keeps the target lock while a "+
		"pull request of its chain may be open, and records a merge of its commit once a read shows one",
		recoveryWaitBound, c.Reason, remediationv1alpha1.ConditionPullRequestUnreadable)
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type: remediationv1alpha1.ConditionPROpened, Status: metav1.ConditionFalse, Reason: ReasonRecoveryWaitExpired,
		Message: msg, ObservedGeneration: rr.Generation,
	})
	return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonRecoveryWaitExpired, msg)
}

// endedAtRecoveryBound reports whether rr ended NeedsIntervention because its recovery waited out its bound
// (endRecoveryWait), with no pull request recorded and its handoff Aborted. It reads the status only.
func endedAtRecoveryBound(rr *remediationv1alpha1.RemediationRequest) bool {
	ho := rr.Status.Handoff
	if rr.Spec.Replaces == nil || ho == nil || ho.From == nil || ho.From.Phase != remediationv1alpha1.HandoffAborted ||
		rr.Status.PullRequest != nil || rr.Status.Phase != remediationv1alpha1.PhaseNeedsIntervention ||
		rr.Status.Source == nil || rr.Status.PlanRepository == nil {
		return false
	}
	return recoveryWaitExpired(rr)
}

// recoveryWaitExpired reports whether condition PullRequestOpened of rr has ReasonRecoveryWaitExpired, which only
// endRecoveryWait sets, before the phase it writes: dropRecoveryWait reads it in that same write.
func recoveryWaitExpired(rr *remediationv1alpha1.RemediationRequest) bool {
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPROpened)
	return c != nil && c.Reason == ReasonRecoveryWaitExpired
}

// openRecovery opens the repository of the plan for the recovery of a handoff. The read of the settings is the
// Kubernetes API's, and its failure is marked so (a *clusterError); the entry they hold (a pause, a repository gone, a
// missing settings object, another target branch) and the open of the provider (the token, settings that cannot be
// used) are not.
func (r *RemediationRequestReconciler) openRecovery(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	providers.GitProvider, error) {
	repo, reinstalled, err := r.planEntry(ctx, rr)
	switch {
	case errors.Is(err, freshness.ErrPaused), errors.Is(err, errRepositoryGone), errors.Is(err, errTargetBranchChanged):
		return nil, err
	case err != nil:
		return nil, &clusterError{err: err}
	}
	git, _, err := r.openPlanEntry(ctx, rr, repo, reinstalled)
	return git, err
}

// heldRecovery is checkHeld's first read for a request endedAtRecoveryBound (#786, decision of 2026-11-17: the end
// hides no merge): the pull request the predecessor recorded, by its number (changeOf, as the recovery reads it).
// It never releases the lock; only a verdict of the look after it does. done is true when this read settles the
// look:
//   - merged with a head that is this request's handoff commit (isHandoffCommit): the takeover and the merge are
//     recorded (recordHandoff), and the request leaves NeedsIntervention for Merged (or PRCreated, when the check at
//     the merge or the merge commit is still awaited: the look at the recorded pull request then records the merge);
//     the recorded merge then settles the lock (settleRecorded);
//   - open: the pull request may still deliver this request's commit, whatever head it has now, so the lock is kept,
//     PullRequestUnreadable goes (it was read), and it is read again after heldRecheckInterval;
//   - a failed Git read: the lock is kept and the read is retried visibly (retryGitRead, a stopped holder's paced
//     wait, never sooner than heldRecheckInterval); a final refusal or settings that cannot reach the repository set
//     PullRequestUnreadable (recoveryUnreadable); a pause waits (waitWhilePaused); a failure of the Kubernetes API,
//     or any other, is returned for the error backoff.
//
// done is false, and the look decides as for any stopped request, when rr does not hold the lock or did not end at the
// bound of its recovery (endedAtRecoveryBound), or when the predecessor is gone or made again, no longer names this
// request in its handoff, or its pull request is closed, gone or merged with another head.
func (r *RemediationRequestReconciler) heldRecovery(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, bool, error) {
	if !ptr.Deref(rr.Status.HoldsTarget, false) || !endedAtRecoveryBound(rr) {
		return reconcile.Result{}, false, nil
	}
	from := rr.Status.Handoff.From
	pred := &remediationv1alpha1.RemediationRequest{}
	switch err := r.getLive(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: string(from.Name)}, pred); {
	case apierrors.IsNotFound(err):
		return reconcile.Result{}, false, nil
	case err != nil:
		return reconcile.Result{}, true, fmt.Errorf("get the RemediationRequest %s replaces: %w", rr.Name, err)
	}
	var to *remediationv1alpha1.OutgoingHandoff
	if pred.Status.Handoff != nil {
		to = pred.Status.Handoff.To
	}
	if pred.UID != types.UID(from.UID) || to == nil || to.UID != remediationv1alpha1.UID(rr.UID) ||
		pred.Status.PullRequest == nil {
		return reconcile.Result{}, false, nil
	}
	lookCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	defer cancel()
	res, done, err := r.heldRecoveryRead(lookCtx, rr, pred, to)
	err = recoveryStepErr(ctx, lookCtx, err)
	switch {
	case err == nil:
		return res, done, nil
	case ctx.Err() != nil, isClusterError(err):
		return reconcile.Result{}, true, err
	case errors.Is(err, freshness.ErrPaused):
		// The record of a merge waits for writes to resume; the lock is kept meanwhile.
		res, err := r.waitWhilePaused(ctx, rr)
		return res, true, err
	}
	if want := recoveryUnreadable(rr, err); want != nil {
		if werr := r.setUnreadable(ctx, rr, want); werr != nil {
			return reconcile.Result{}, true, werr
		}
	}
	read := recoveryRead(err)
	if !gitReadFailure(read) {
		return reconcile.Result{}, true, err
	}
	res, err = r.retryGitRead(ctx, rr, read, unboundedRead(noBoundHeldLock))
	if err != nil {
		return reconcile.Result{}, true, err
	}
	// The pace of a stopped holder's looks, never past the end of a wait it records.
	res.RequeueAfter = r.withinWaits(rr, max(res.RequeueAfter, heldRecheckInterval))
	return res, true, nil
}

// heldRecoveryRead is heldRecovery's read through the plan's repository. A failure is returned; a failure of the
// Kubernetes API is marked so (a *clusterError).
func (r *RemediationRequestReconciler) heldRecoveryRead(ctx context.Context, rr, pred *remediationv1alpha1.RemediationRequest,
	to *remediationv1alpha1.OutgoingHandoff) (reconcile.Result, bool, error) {
	git, err := r.openRecovery(ctx, rr)
	if err != nil {
		return reconcile.Result{}, true, err
	}
	v := r.changeOf(ctx, git, pred, changeQuery{})
	switch v.state {
	case changeUnknown:
		return reconcile.Result{}, true, v.err
	case changeOpen:
		if err := r.setUnreadable(ctx, rr, nil); err != nil {
			return reconcile.Result{}, true, err
		}
		logf.FromContext(ctx).Info("Kept the target lock of RemediationRequest: the pull request that may carry its "+
			"commit is open", "remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", v.pr.Number)
		return reconcile.Result{RequeueAfter: r.withinWaits(rr, heldRecheckInterval)}, true, nil
	case changeMerged:
		if v.pr.HeadSHA == to.ExpectedHead {
			return reconcile.Result{}, false, nil
		}
	default:
		return reconcile.Result{}, false, nil
	}
	switch ours, err := r.isHandoffCommit(ctx, git, rr, pred, to, v.pr.HeadSHA); {
	case err != nil:
		return reconcile.Result{}, true, err
	case !ours:
		return reconcile.Result{}, false, nil
	}
	if err := r.recordHandoff(ctx, rr, pred, v.pr, v.pr.HeadSHA); err != nil {
		return reconcile.Result{}, true, err
	}
	logf.FromContext(ctx).Info("Recorded the merge of the commit of RemediationRequest found after its recovery "+
		"ended", "remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", v.pr.Number,
		"phase", rr.Status.Phase)
	return reconcile.Result{RequeueAfter: stepDelay}, true, nil
}
