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
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/controller/lockverdict"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// The abort of a handoff at a refused read of its recovery (#786, decision of 2026-11-18, which replaced keeping the
// intent standing while the recovery waits). While the intent stands, the predecessor runs no freshness check and no
// merge gate; keeping it for the 24 hours of a refused recovery would leave the predecessor's old head with a passed
// check for a day. So at the first final refusal of a read of the recovery (or settings that cannot reach the plan's
// repository, recoveryUnreadable), the replacement:
//   - records, in one status write (abortOnRefusal), condition PullRequestUnreadable that names the refused read and
//     the HandoffRecovery entry of status.gitWaits, from that refusal to recoveryWaitBound later. The entry is the
//     recorded state of the abort: a replacement that still recovers its handoff and records it (abortedOnRefusal)
//     never takes the pull request over again, and its end never moves;
//   - aborts the predecessor's intent (abortPredecessor) before it goes on, and on every later reconcile while the
//     intent still stands (a write that was lost); the predecessor aborts it too when it sees the replacement so
//     (settleHandoff, handoffHolds). The predecessor then runs its freshness check and merge gate again, or stops
//     BranchDiverged with its comment when the branch carries the replacement's commit;
//   - keeps the target lock, writes nothing to Git, and reads the predecessor's pull request every poll interval
//     until the end of its HandoffRecovery wait (observeStep): a merge of its commit seen in that time is recorded;
//     a merge into a base a person moved the pull request to ends the wait at once (endMergedElsewhere); a read that
//     fails is retried visibly (observeFailed). The end (endPassedWait, endRecoveryWait) is no verdict on
//     the lock and does not end the observation: the replacement ends NeedsIntervention with RecoveryWaitExpired, so
//     a person sees it, keeps status.handoff.from Reserved and the lock, and the look at stopped holders (checkHeld,
//     heldRecovery first) goes on reading that pull request: a merge of its commit is recorded with its receipt
//     whenever it comes (recordWatchedMerge, the same record as during the watch), and the lock is released only on a
//     verdict (merged, closed, open with the predecessor's own head, the branch or the repository gone).
//
// The same holds for any end of a replacement before it took the pull request over, whatever ended it
// (HandoffRecoveryFailed, Expired, Cancelled; endedBeforeTakeover, decision 3 of the model result of 2026-11-18): its
// commit may sit on the open pull request, so it keeps status.handoff.from Reserved and the lock until that verdict.
// Who records a merged head is decided once, by mergedHeadOwner, which both requests call (decision 1).
//
// A read that fails without a refusal (a 5xx, a timeout) is as before #786: the intent stands until the deadline plus
// handoffRecoveryGrace (the HandoffGrace entry, retryRecovery). The write that records the HandoffRecovery entry
// removes the HandoffGrace entry, which bounded the intent the refusal aborts, so each entry ends at its own end. A
// refusal condition that stands without its entry (an executor that wrote the two apart) gets the entry from the
// condition's lastTransitionTime before any read (recordRefusalEntry), so every bound is read from recorded state.
// The predecessor keeps no bound of its own on a standing intent: a replacement that cannot store its refusal behaves
// as before #786 (#869).

// recoveryWaitBound is how long a replacement whose handoff was aborted at a refusal watches for a merge of its
// commit: the bound of a cancel and of a cleanup.
const recoveryWaitBound = cleanupFailingBound

// ReasonRecoveryWaitExpired is the reason of NeedsIntervention (condition PullRequestOpened False) when a replacement
// whose handoff was aborted at a refusal reached the end of its waits without seeing a merge of its commit (#786,
// endRecoveryWait).
const ReasonRecoveryWaitExpired = "RecoveryWaitExpired"

// ReasonRecoveryReadFailed is the reason of condition PullRequestUnreadable on a replacement whose handoff was aborted
// at a refusal when the cause its condition named is gone (the repository opens, or the refused read is answered) and
// another read of the Git host fails now: the condition names that read (recoveryReadFailed).
const ReasonRecoveryReadFailed = "RecoveryReadFailed"

// recoveryWaitReasons are the reasons of condition PullRequestUnreadable that a recovery records at a refusal
// (watchedRecovery): a final refusal, settings that cannot reach the plan's repository (repositoryReasons, and a
// settings object that does not exist), and ReasonRecoveryReadFailed, which only replaces one of those.
var recoveryWaitReasons = map[string]bool{
	ReasonGitForbidden: true, ReasonGitRejected: true, ReasonRecoveryReadFailed: true, ReasonSettingsNotFound: true,
	reasonRepositoryUnavailable: true, reasonTargetBranchChanged: true, reasonRepositoryUnusable: true,
}

// recoveryStepErr is keepGitRead for the step of the recovery: a Git read that ran out of the step's time stays that
// read, so its wait is recorded and paced like any other failure of it. A failure of the Kubernetes API that ran into
// the deadline stays marked as the cluster's (*clusterError).
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

// nowUnreadable is condition PullRequestUnreadable for err, a failure worth retrying of a replacement that watches
// after the abort with condition c standing, when the cause c names is gone: nil while it may still hold. A reason
// about the repository is gone, since the settings entry was read and is fine again (recoveryUnreadable would have
// named it otherwise); a refusal holds while the read it names is the one failing now; ReasonRecoveryReadFailed holds
// while it names the read failing now. Otherwise the condition is replaced by recoveryReadFailed, which names that
// read (#786, decisions of 2026-11-16). It reads the status and err only.
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
// replacement that watches after the abort. It names the read by its fixed operation text (readOp), never the host's
// answer.
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

// recoveryUnreadable is condition PullRequestUnreadable for err, a failure of recoverStep that aborts the handoff
// (#786): a final refusal of the Git host, of a read of the branch or of a pull request alike (refusedRead, which names
// the refused read by its operation text), or settings that cannot reach the plan's repository (repositoryRead). Any
// other failure gives nil.
func recoveryUnreadable(rr *remediationv1alpha1.RemediationRequest, err error) *metav1.Condition {
	if reason, _, final := refusal(err); final {
		return refusedRead(rr, reason, err, 0)
	}
	return repositoryRead(rr, err)
}

// watchedRecovery is condition PullRequestUnreadable of rr when a recovery recorded it at a refusal: one with a reason
// of recoveryWaitReasons. Any other, or none, gives nil. It reads the status only, never a time.
func watchedRecovery(rr *remediationv1alpha1.RemediationRequest) *metav1.Condition {
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
	if c == nil || c.Status != metav1.ConditionTrue || !recoveryWaitReasons[c.Reason] {
		return nil
	}
	return c
}

// abortedOnRefusal reports whether rr is a replacement whose handoff was aborted at a refused read of its recovery:
// it still recovers its handoff (recoveringHandoff) and records its HandoffRecovery entry. It reads the status only.
func abortedOnRefusal(rr *remediationv1alpha1.RemediationRequest) bool {
	return recoveringHandoff(rr) && gitWait(rr, waitHandoffRecovery) != nil
}

// refusalEntry is the HandoffRecovery entry of a refusal at since of the read op.
func refusalEntry(since time.Time, op string) remediationv1alpha1.GitWait {
	at := metav1.NewTime(since.Truncate(time.Second))
	return remediationv1alpha1.GitWait{Kind: remediationv1alpha1.GitWaitHandoffRecovery, Since: at,
		Operation: cutOperation(op), Ends: &metav1.Time{Time: at.Add(recoveryWaitBound)}}
}

// abortOnRefusal is the first refusal of a recovery (want, for err): one status write records condition
// PullRequestUnreadable and the HandoffRecovery entry from now, so neither stands without the other, and the
// predecessor's intent is aborted after it (abortPredecessor). A request written meanwhile is read again (a conflict
// is retried on a fresh read); one that no longer recovers its handoff writes nothing. A failure of the Kubernetes API
// joined to err is returned once the abort is recorded, for the error backoff; a failed write is returned with err. It
// comes back after the poll interval, never later than the end of the entry.
func (r *RemediationRequestReconciler) abortOnRefusal(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	want *metav1.Condition, err error) (reconcile.Result, error) {
	op := readOp(recoveryRead(err))
	was := watchedRecovery(rr)
	if was != nil {
		was = was.DeepCopy()
	}
	werr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live := &remediationv1alpha1.RemediationRequest{}
		if err := r.reader().Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
			return &clusterError{err: err}
		}
		if live.UID != rr.UID {
			return &clusterError{err: errors.New("RemediationRequest was replaced")}
		}
		if !recoveringHandoff(live) {
			// Moved on meanwhile (ended, or took the pull request over): nothing to abort from here.
			live.DeepCopyInto(rr)
			return nil
		}
		base := live.DeepCopy()
		c := want.DeepCopy()
		if gitWait(live, waitHandoffRecovery) == nil {
			removeInapplicable(live)
			entry := refusalEntry(r.now(), op)
			live.Status.GitWaits = append(live.Status.GitWaits, entry)
			c.LastTransitionTime = entry.Since
			// The refusal aborts the intent, and the grace that bounded it goes in the same write (third decision of
			// 2026-11-18 on #786): no entry ends at another's end.
			removeInapplicable(live)
		}
		putUnreadable(&live.Status.Conditions, c)
		gitWaitSummary(live, r.now())
		if err := r.Status().Patch(ctx, live, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		live.DeepCopyInto(rr)
		return nil
	})
	if werr != nil {
		return reconcile.Result{}, errors.Join(err, clusterSideErr(fmt.Errorf("record the abort of the handoff of "+
			"RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, werr)))
	}
	if !abortedOnRefusal(rr) {
		// The request moved on meanwhile (it no longer recovers its handoff, read live): nothing is aborted, and the
		// refusal is logged so it is not lost; the next reconcile decides from the request as it is now.
		logf.FromContext(ctx).Info("Met a refused read of the recovery of RemediationRequest after it moved on; "+
			"nothing was aborted", "remediationRequest", client.ObjectKeyFromObject(rr), "phase", rr.Status.Phase,
			"reason", want.Reason, "error", err.Error())
		return reconcile.Result{RequeueAfter: stepDelay}, nil
	}
	r.unreadableEvent(rr, was, watchedRecovery(rr))
	logf.FromContext(ctx).Info("Aborted the handoff of RemediationRequest at a refused read of its recovery; it "+
		"watches for a merge of its commit until the end of its HandoffRecovery wait", "remediationRequest",
		client.ObjectKeyFromObject(rr), "reason", want.Reason, "error", err.Error())
	if res, perr := r.abortPredecessor(ctx, rr); perr != nil || res.RequeueAfter > 0 {
		return res, errors.Join(perr, clusterOnly(err))
	}
	d, werr := r.withinWaits(ctx, rr, r.pullRequestPollInterval())
	if werr != nil {
		return reconcile.Result{}, errors.Join(clusterSideErr(werr), clusterOnly(err))
	}
	return reconcile.Result{RequeueAfter: d}, clusterOnly(err)
}

// recoveryPace is heldPace for the looks of a replacement's recovery: a failed read of the request, for the ends of
// its waits, is the Kubernetes API's and is marked so (clusterSideErr), for the error backoff.
func (r *RemediationRequestReconciler) recoveryPace(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	d time.Duration) (reconcile.Result, error) {
	res, err := r.heldPace(ctx, rr, d)
	return res, clusterSideErr(err)
}

// clusterOnly is err when it carries a failure of the Kubernetes API, for the error backoff, else nil.
func clusterOnly(err error) error {
	if isClusterError(err) {
		return err
	}
	return nil
}

// abortPredecessor aborts the intent of the request rr replaces when it still names rr (abortIntent: to Aborted, its
// hint Declined), in one status write with the resourceVersion read, so the predecessor's next look runs its freshness
// check and merge gate again. A predecessor that is gone, made again, or whose intent does not stand needs nothing. A
// conflict comes back after stepDelay to read it again; while writes are paused it is left to the predecessor's own
// look (settleHandoff), which aborts it as well. Every failure is the Kubernetes API's (a *clusterError).
func (r *RemediationRequestReconciler) abortPredecessor(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	from := rr.Status.Handoff.From
	pred := &remediationv1alpha1.RemediationRequest{}
	switch err := r.getLive(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: string(from.Name)}, pred); {
	case apierrors.IsNotFound(err):
		return reconcile.Result{}, nil
	case err != nil:
		return reconcile.Result{}, &clusterError{err: fmt.Errorf("get the RemediationRequest %s replaces: %w", rr.Name, err)}
	}
	to := intent(pred)
	if pred.UID != types.UID(from.UID) || to == nil || to.UID != remediationv1alpha1.UID(rr.UID) {
		return reconcile.Result{}, nil
	}
	switch err := r.writeAllowed(ctx); {
	case errors.Is(err, freshness.ErrPaused):
		return reconcile.Result{}, nil
	case err != nil:
		return reconcile.Result{}, clusterSideErr(err)
	}
	abortIntent(pred, ReasonHintHandoffAborted)
	if err := r.Status().Update(ctx, pred); err != nil {
		if apierrors.IsConflict(err) {
			return reconcile.Result{RequeueAfter: stepDelay}, nil
		}
		return reconcile.Result{}, &clusterError{err: fmt.Errorf("abort the handoff intent of RemediationRequest %s/%s: %w",
			pred.Namespace, pred.Name, err)}
	}
	logf.FromContext(ctx).Info("Aborted the handoff intent of RemediationRequest: a read of its replacement's "+
		"recovery was refused", "remediationRequest", client.ObjectKeyFromObject(pred), "replacement", rr.Name)
	return reconcile.Result{}, nil
}

// recordRefusalEntry runs at the start of every reconcile, before any read: a replacement that still recovers its
// handoff with a refusal condition standing (watchedRecovery) and no HandoffRecovery entry (an executor that wrote the
// two apart, and lost the write of the entry) gets the entry from the condition's lastTransitionTime, so the bound is
// recorded state before any read can fail. Its failure is the Kubernetes API's, for the error backoff.
func (r *RemediationRequestReconciler) recordRefusalEntry(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	c := watchedRecovery(rr)
	if c == nil || !recoveringHandoff(rr) || gitWait(rr, waitHandoffRecovery) != nil {
		return nil
	}
	since := c.LastTransitionTime.Time
	if since.IsZero() {
		since = r.now()
	}
	entry := refusalEntry(since, "the recovery of the handoff (condition "+
		remediationv1alpha1.ConditionPullRequestUnreadable+", "+c.Reason+")")
	logf.FromContext(ctx).Info("Recorded the HandoffRecovery wait of RemediationRequest from its standing refusal",
		"remediationRequest", client.ObjectKeyFromObject(rr), "since", entry.Since.Time)
	return r.putGitWaits(ctx, rr, []remediationv1alpha1.GitWait{entry})
}

// observeStep is the recovery of a replacement whose handoff was aborted at a refusal (abortedOnRefusal): it aborts the
// predecessor's intent while it still stands (abortPredecessor), then reads the predecessor's pull request by its
// recorded number (changeOf). A merge whose head is this request's handoff commit (isHandoffCommit) is recorded
// (recordHandoff: Merged, or PRCreated while the merge commit or the check at the merge is awaited). A merge into a
// base a person moved the pull request to (changeMergedElsewhere), with any head, ends the wait at once
// (endMergedElsewhere). Anything else writes nothing to Git and takes nothing over: a read that works removes condition PullRequestUnreadable, and the
// request is read again after the poll interval, never later than the end of its waits, which ends it
// (endPassedWait). With spec.cancel set, a read that works and finds no merge of its commit returns done false, so the
// cancel finishes. A predecessor that is gone, made again, or no longer names this request has nothing to read: the
// request waits for the end. A failure is returned (observeFailed).
func (r *RemediationRequestReconciler) observeStep(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, bool, error) {
	if res, err := r.abortPredecessor(ctx, rr); err != nil || res.RequeueAfter > 0 {
		return res, true, err
	}
	from := rr.Status.Handoff.From
	pred := &remediationv1alpha1.RemediationRequest{}
	// The poll, never past the end of a wait rr records (withinWaits), read only where it is the answer.
	wait := func() (reconcile.Result, bool, error) {
		res, err := r.recoveryPace(ctx, rr, r.pullRequestPollInterval())
		if err != nil {
			return reconcile.Result{}, true, err
		}
		return res, !cancelRequested(rr), nil
	}
	// Nothing to read: the wait says so and drops the refusal it no longer reads past (noteNothingToRead).
	idle := func() (reconcile.Result, bool, error) {
		if err := r.noteNothingToRead(ctx, rr); err != nil {
			return reconcile.Result{}, true, err
		}
		return wait()
	}
	switch err := r.getLive(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: string(from.Name)}, pred); {
	case apierrors.IsNotFound(err):
		return idle()
	case err != nil:
		return reconcile.Result{}, true, &clusterError{err: fmt.Errorf("get the RemediationRequest %s replaces: %w",
			rr.Name, err)}
	}
	var to *remediationv1alpha1.OutgoingHandoff
	if pred.Status.Handoff != nil {
		to = pred.Status.Handoff.To
	}
	if pred.UID != types.UID(from.UID) || to == nil || to.UID != remediationv1alpha1.UID(rr.UID) ||
		pred.Status.PullRequest == nil {
		return idle()
	}
	git, err := r.openRecovery(ctx, rr)
	if err != nil {
		return reconcile.Result{}, true, err
	}
	v := r.changeOf(ctx, git, pred, changeQuery{})
	switch v.state {
	case changeUnknown:
		return reconcile.Result{}, true, v.err
	case changeMergedElsewhere:
		// Nothing left to recover, and the target branch did not get the change: the wait ends at once (decision of
		// 2026-11-19 on #786).
		return reconcile.Result{}, true, clusterSideErr(r.endMergedElsewhere(ctx, rr, v.pr))
	case changeOpen, changeMerged, changeClosed, changeGone, changeElsewhere:
		// Only a merge of this request's commit is recorded; anything else waits for the end of the wait. Closed, here
		// or on another base, may be reopened, and open on another base may still be moved back.
	}
	switch recorded, err := r.recordWatchedMerge(ctx, git, rr, pred, to, v); {
	case err != nil:
		return reconcile.Result{}, true, err
	case recorded:
		return reconcile.Result{RequeueAfter: stepDelay}, true, nil
	}
	if err := r.setUnreadable(ctx, rr, nil); err != nil {
		return reconcile.Result{}, true, err
	}
	return wait()
}

// recoveryNothingToRead is the operation of a HandoffRecovery entry whose watch has nothing to read
// (noteNothingToRead): the predecessor is gone, was made again, or no longer hands over to this request, so no pull
// request of the handoff can be named. gitWaitSummary and endRecoveryWait read it to say so. Fixed text only. Nothing
// names the request again once it was noted: a predecessor hands over only from the reservation of its successor.
const recoveryNothingToRead = "none: the request it replaces is gone, was made again or no longer hands over to " +
	"this request"

// noteNothingToRead records, on rr, that its recovery wait has nothing to read (observeStep found no predecessor
// that hands over to it): condition PullRequestUnreadable of the recovery (watchedRecovery) is removed, since no
// read is made that it could name, and the HandoffRecovery entry takes recoveryNothingToRead as its operation, so
// condition GitReadRetry says in fixed wording that the request waits with nothing to read until the entry's end.
// The entry keeps its start and end, and the lock is kept. One status write, only when something changes, on a fresh
// read and retried on a conflict; a request that no longer watches after an abort writes nothing. A failure is the
// Kubernetes API's (a *clusterError).
func (r *RemediationRequestReconciler) noteNothingToRead(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) error {
	if w := gitWait(rr, waitHandoffRecovery); watchedRecovery(rr) == nil && (w == nil ||
		w.Operation == recoveryNothingToRead) {
		return nil
	}
	var unreadable *metav1.Condition
	noted := false
	err := r.patchGitWaits(ctx, rr, func(live *remediationv1alpha1.RemediationRequest) error {
		unreadable = meta.FindStatusCondition(live.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
		noted = abortedOnRefusal(live)
		if !noted {
			return nil
		}
		if watchedRecovery(live) != nil {
			meta.RemoveStatusCondition(&live.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
			unreadable = nil
		}
		gitWait(live, waitHandoffRecovery).Operation = recoveryNothingToRead
		return nil
	})
	if err != nil {
		return clusterSideErr(fmt.Errorf("record that the recovery wait of RemediationRequest %s/%s has nothing to "+
			"read: %w", rr.Namespace, rr.Name, err))
	}
	if unreadable != nil {
		putUnreadable(&rr.Status.Conditions, unreadable)
	} else {
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable)
	}
	if !noted {
		return nil
	}
	// No Event for the removal: EventPullRequestReadable says the pull requests can be read again, which is not
	// what happened; condition GitReadRetry says it.
	logf.FromContext(ctx).Info("Recorded that the recovery wait of RemediationRequest has nothing to read: the request "+
		"it replaces is gone, was made again or no longer hands over to it", "remediationRequest",
		client.ObjectKeyFromObject(rr), "replaces", rr.Status.Handoff.From.Name)
	return nil
}

// recordWatchedMerge records, on rr, a merge of the pull request of pred, v as read through git, that rr owns
// (mergedHeadOwner against pred's outgoing handoff to). With rr's handoff commit as the merged head (isHandoffCommit):
// recordHandoff, which moves rr's handoff from Reserved to Done and records the merge with its receipt (Merged), or the
// pull request while its merge commit or the check at the merge is awaited (PRCreated). With any other head but the
// predecessor's own, while rr holds the target lock (recordsEveryMerge, decision of 2026-11-20 on #870): the merge as
// a foreign change after the read of what it brought (recordOtherHeadMerge). It is the one record of such a merge,
// during the watch after the abort (observeStep) and after its end (heldRecovery), so verification and the circuit
// breaker see it. recorded is false, with nothing written, for any other verdict or owner; a merged head that is the
// predecessor's own is its merge, not this request's. A request that ended at the bound of its watch leaves its final
// phase here, so the breaker's counts of that end go with it (classifyBreaker counts again at the next end).
func (r *RemediationRequestReconciler) recordWatchedMerge(ctx context.Context, git providers.GitProvider,
	rr, pred *remediationv1alpha1.RemediationRequest, to *remediationv1alpha1.OutgoingHandoff, v changeVerdict) (
	bool, error) {
	if v.state != changeMerged {
		return false, nil
	}
	owner, ours, err := r.mergedHeadOwnerOf(ctx, git, pred, rr, to, v.pr.HeadSHA, true)
	switch {
	case err != nil:
		return false, err
	case owner != ownerReplacement:
		return false, nil
	case !ours:
		if err := r.ignoreStopped(r.recordOtherHeadMerge(ctx, git, rr, pred, v.pr)); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := r.recordFoundMerge(ctx, rr, pred, v.pr); err != nil {
		return false, err
	}
	return true, nil
}

// recordOtherHeadMerge records on rr, a replacement that holds the target lock before its takeover (recordsEveryMerge),
// the merge of cr, the pull request it was to take over, whose merged head is not its handoff commit: a person pushed
// on top of rr's commit after the predecessor handed the pull request to a person, or replaced it (decision of
// 2026-11-20 on #870), or the predecessor's own head once the predecessor is gone or being deleted without a record
// of it (predecessorLeftItsMerge, heldRecoveryAlone; third case of that decision). One writer of the merge record per pull request, the one
// that holds the lock, so the predecessor records none. First what the merge brought to the target branch is read,
// as for a merge into a retargeted branch (retargetedContent, #771); a read that fails is an error of the content wait
// about cr (MergeContent in status.gitWaits, a claim on the merge the predecessor waits for), retried by the caller
// and ended after mergeContentWait with MergeContentUnreadable (endContentUnreadable), with nothing recorded before.
// Then one status write (endOtherHeadMerge) records the pull request on rr and ends it NeedsIntervention with
// MergedWithForeignChange, no receipt, a failure for the circuit breaker like any foreign change: the head is not the
// one Shadewright pushed for this plan, whatever the merge brought. The lock is released after that record, by the
// read of the look at stopped holders (undeliveredMerge). A failed write is returned for a retry.
func (r *RemediationRequestReconciler) recordOtherHeadMerge(ctx context.Context, git providers.GitProvider,
	rr, pred *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest) error {
	if err := r.writeAllowed(ctx); err != nil {
		return clusterSideErr(err)
	}
	// The content read alone, as checkRetargeted makes it but without its removal of the content wait: that wait is
	// this request's claim on the merge, so it goes only in the status write that stores the record (stop removes a
	// wait that no longer applies), never in a write of its own that a lost record write would leave without either.
	brought := "the Git host reported no full merge commit ID, so what it brought cannot be checked"
	if shaPattern.MatchString(cr.MergeCommit) {
		ref, err := retargetedContent(ctx, git, rr, cr, remediationv1alpha1.GitBranch(cr.BaseBranch))
		if err != nil {
			logf.FromContext(ctx).Info("Could not read what the merge of the pull request RemediationRequest was to "+
				"take over brought; tried again", "remediationRequest", client.ObjectKeyFromObject(rr),
				"pullRequest", cr.Number, "mergeCommit", cr.MergeCommit, "error", err.Error())
			return readOfContent(cr, err)
		}
		r.gitRetries().noteAnswered(client.ObjectKeyFromObject(rr))
		brought = "it brought only the planned change of the target file"
		if ref != nil {
			brought = ref.message
		}
	}
	return clusterSideErr(r.endOtherHeadMerge(ctx, rr, pred, cr, ReasonMergedWithForeignChange, brought))
}

// endOtherHeadMerge is the status write of recordOtherHeadMerge, and of the end of its content wait
// (endContentUnreadable, reason ReasonMergeContentUnreadable): rr takes the pull request over in the same write
// (takeOverInto: its handoff Done, the pull request recorded with the merged head), so the record names the pull
// request and the predecessor reads it (holdsMergeRecord), and rr ends NeedsIntervention with reason and a message
// that says the merged head was not its commit and what the read of the merge found (brought). A request that ended
// before (endedBeforeTakeover) leaves its final phase here, so the breaker's counts of that end go with it. A failed
// write is returned for a retry; errStopped means it went through.
func (r *RemediationRequestReconciler) endOtherHeadMerge(ctx context.Context, rr, pred *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest, reason, brought string) error {
	if endedBeforeTakeover(rr) {
		rr.Status.BreakerCounts = nil
	}
	unreadable := takeOverInto(ctx, rr, pred, cr, cr.HeadSHA)
	markClosed(rr, ReasonChangeRequestMerged, "was merged with a head that is not this request's commit")
	message := fmt.Sprintf("Pull request #%d, which this request was to take over, was merged with head %s, which is "+
		"not the commit Shadewright pushed for this request (a person's commit is on it, or another commit is in its "+
		"place); this request records the merge as it held the target. What the merge brought: %s. Nothing after the merge is checked; "+
		"review what the merge brought", cr.Number, cr.HeadSHA, brought)
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type: remediationv1alpha1.ConditionApproved, Status: metav1.ConditionFalse,
		Reason: reason, Message: message, ObservedGeneration: rr.Generation,
	})
	if rr.Status.Phase == remediationv1alpha1.PhaseNeedsIntervention {
		classifyBreaker(rr)
	}
	err := r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, reason, message)
	if errors.Is(err, errStopped) {
		r.unreadableEvent(rr, unreadable, nil)
		logf.FromContext(ctx).Info("Recorded the merge of the pull request RemediationRequest was to take over, "+
			"merged with a head that is not its commit, as a foreign change", "remediationRequest",
			client.ObjectKeyFromObject(rr), "pullRequest", cr.Number, "head", cr.HeadSHA, "reason", reason)
	}
	return err
}

// recordFoundMerge records on rr the merge of cr, whose merged head is rr's handoff commit, after its handoff was
// aborted: recordHandoff from pred (the predecessor, or only its name once it is gone). A request that ended at the
// bound of its watch leaves its final phase here, so the breaker's counts of that end go with it.
func (r *RemediationRequestReconciler) recordFoundMerge(ctx context.Context,
	rr, pred *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest) error {
	ended := endedBeforeTakeover(rr)
	if ended {
		rr.Status.BreakerCounts = nil
	}
	if err := r.recordHandoff(ctx, rr, pred, cr, cr.HeadSHA); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("Recorded the merge of the commit of RemediationRequest after its handoff was aborted",
		"remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", cr.Number, "phase", rr.Status.Phase,
		"afterTheEndOfTheWatch", ended)
	return nil
}

// observeFailed is the answer to err, a failure of observeStep. It never ends the request; only the end of its waits
// does (endPassedWait):
//   - the end of the reconcile's own context is returned as it is, and a pause waits (waitWhilePaused);
//   - a failure of the Kubernetes API (marked where it is returned) goes to the error backoff, after the condition
//     of a refusal that came with it;
//   - a final refusal of the Git host, or settings that cannot reach the repository, sets condition
//     PullRequestUnreadable that names it; a failure worth retrying, or a read past the step's own deadline, names the
//     read failing now when the cause the condition named is gone (nowUnreadable); either way the failed read is
//     retried visibly within the HandoffRecovery entry (retryGitRead);
//   - every other failure goes to the error backoff, so it is logged and counted.
func (r *RemediationRequestReconciler) observeFailed(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	err error) (reconcile.Result, error) {
	switch {
	case ctx.Err() != nil:
		return reconcile.Result{}, err
	case errors.Is(err, freshness.ErrPaused):
		return r.waitWhilePaused(ctx, rr)
	case isClusterError(err):
		// A refusal that came with it (the predecessor's own condition lost to a conflict) is still shown.
		if want := recoveryUnreadable(rr, err); want != nil {
			if werr := r.setUnreadable(ctx, rr, want); werr != nil {
				return reconcile.Result{}, errors.Join(err, werr)
			}
		}
		return reconcile.Result{}, err
	}
	read := recoveryRead(err)
	want := recoveryUnreadable(rr, err)
	if want == nil && (providers.IsRetryable(err) || errors.Is(err, ErrTimeout)) {
		if c := watchedRecovery(rr); c != nil {
			want = nowUnreadable(rr, c, err)
		}
	}
	if want != nil && gitReadFailure(read) {
		if werr := r.setUnreadable(ctx, rr, want); werr != nil {
			return reconcile.Result{}, errors.Join(err, werr)
		}
	}
	if !gitReadFailure(read) {
		logf.FromContext(ctx).Info("Could not read the pull request of the aborted handoff of RemediationRequest; "+
			"retried with the error backoff", "remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error())
		return reconcile.Result{}, err
	}
	return r.retryGitRead(ctx, rr, read, boundBy(waitHandoffRecovery))
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

// endRecoveryWait ends a replacement whose handoff was aborted at a refusal at the end of its HandoffRecovery wait,
// entry, so the message states the span that entry recorded. The end is one status write: NeedsIntervention with
// ReasonRecoveryWaitExpired, so a person sees it. It is no verdict and does not end the observation: the target lock
// is kept, and status.handoff.from stays Reserved (declineOnEnd leaves it, recoveryWaitExpired), so the look at
// stopped holders (checkHeld, heldRecovery) goes on reading the pull request that may carry the request's commit and
// records a merge of it, whenever it comes (recordWatchedMerge). PullRequestOpened is set before stop, so that the end
// keeps condition PullRequestUnreadable (dropRecoveryWait reads it) and its handoff Reserved.
func (r *RemediationRequestReconciler) endRecoveryWait(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	entry remediationv1alpha1.GitWait) error {
	end := entry.Ends.Time
	logf.FromContext(ctx).Info("Ended the watch of RemediationRequest after the abort of its handoff: its wait passed",
		"remediationRequest", client.ObjectKeyFromObject(rr), "since", entry.Since.Time, "end", end)
	why := ""
	if c := watchedRecovery(rr); c != nil {
		why = fmt.Sprintf(" (%s, see condition %s)", c.Reason, remediationv1alpha1.ConditionPullRequestUnreadable)
	}
	saw := "watched the pull request it was to take over for %s, until %s, and saw no merge of its commit"
	if entry.Operation == recoveryNothingToRead {
		saw = "waited for %s, until %s, with nothing to read: the request it replaces is gone, was made again or no " +
			"longer hands over to this request"
	}
	msg := fmt.Sprintf("The handoff was aborted at a refused read of its recovery on %s%s; this request "+saw+
		". It keeps the target lock while that pull request may still be merged, and a later merge of its commit is "+
		"recorded on this request",
		entry.Since.UTC().Format(time.RFC3339), why, end.Sub(entry.Since.Time), end.UTC().Format(time.RFC3339))
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type: remediationv1alpha1.ConditionPROpened, Status: metav1.ConditionFalse, Reason: ReasonRecoveryWaitExpired,
		Message: msg, ObservedGeneration: rr.Generation,
	})
	return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonRecoveryWaitExpired, msg)
}

// endedBeforeTakeover reports whether rr is a replacement that ended without taking the pull request over while its
// commit may be on it (mayCarryHandoffCommit), its handoff still Reserved: whatever ended it (the end of the watch
// after a refusal, RecoveryWaitExpired; HandoffRecoveryFailed; Cancelled; Expired; a stop of a retry). Such a request
// keeps the target lock, and the look at stopped holders reads its predecessor's pull request (heldRecovery). It reads
// the status only.
func endedBeforeTakeover(rr *remediationv1alpha1.RemediationRequest) bool {
	return mayCarryHandoffCommit(rr) && rr.Status.Handoff.From.Phase == remediationv1alpha1.HandoffReserved &&
		ended(rr.Status.Phase)
}

// recoveryWaitExpired reports whether condition PullRequestOpened of rr has ReasonRecoveryWaitExpired, which only
// endRecoveryWait sets, before the phase it writes: dropRecoveryWait and declineOnEnd read it in that same write.
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

// openStoppedRead opens the plan's repository for a read of a stopped holder's look that is not lookHeld's: the read of
// the pull request a replacement that ended before its takeover was to take over (heldRecovery), and the read of who
// records a merge a stopped predecessor found (stoppedMergeOwner). When the settings name another target branch, the
// pull request is read on the branch the plan recorded, as lookHeld reads it, through a provider that writes nothing
// (openRecordedBranch), so a merge or a close there settles the lock (second round of #786). Anything else is
// openRecovery's: a repository gone from the settings is errRepositoryGone (heldRecovery keeps the lock on it), and a
// failed read of the settings is the Kubernetes API's.
func (r *RemediationRequestReconciler) openStoppedRead(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	providers.GitProvider, error) {
	repo, _, err := r.planEntry(ctx, rr)
	if !errors.Is(err, errTargetBranchChanged) || rr.Status.Source == nil {
		return r.openRecovery(ctx, rr)
	}
	git, err := r.openRecordedBranch(ctx, repo, rr.Status.Source.TargetBranch)
	switch {
	case providers.IsInvalidConfig(err):
		return nil, freshness.ErrRepositoryUnusable
	case err != nil:
		return nil, freshness.WrapProvider("open the repository", err)
	}
	return git, nil
}

// heldRecovery is checkHeld's first read for a request that ended before it took the pull request over
// (endedBeforeTakeover, #786, decision 3 of the model result of 2026-11-18): the pull request the predecessor recorded,
// by its number (changeOf, as the recovery reads it), which may carry this request's commit although no pull request
// of this request is recorded or marked. done is true when this read settles the look:
//   - merged with this request's commit (mergedHeadOwner says the replacement): the merge is recorded on this request
//     with its receipt, however long after the end, and the write that records it settles the lock as for any
//     recorded merge;
//   - merged with any other head: recorded on this request as a foreign change when mergedHeadOwner gives it the
//     merge (recordWatchedMerge, #870: any head but the predecessor's own, and that one too once the predecessor is
//     being deleted without a record of it); otherwise the handoff can no longer happen, so it moves to Aborted and
//     the lock is released (endOnOtherMerge);
//   - open with the predecessor's own head: this request's commit is not on it and, since only this request pushes it,
//     never will be, so the handoff moves to Aborted and the lock is released (endWithoutCommit);
//   - open with any other head: the pull request may still deliver this request's commit, so the lock is kept,
//     PullRequestUnreadable goes (it was read), and it is read again after heldRecheckInterval;
//   - a failed Git read: the lock is kept and the read is retried visibly (retryGitRead, a stopped holder's paced
//     wait, never sooner than heldRecheckInterval); a final refusal or settings that cannot reach the repository set
//     PullRequestUnreadable (recoveryUnreadable); a pause waits (waitWhilePaused); a failure of the Kubernetes API,
//     or any other, is returned for the error backoff.
//
// done is false, and the look decides as for any stopped request, when rr does not hold the lock or did not end before
// its takeover, or when the predecessor is made again, no longer names this request in its handoff, or its pull
// request is closed or gone. A predecessor that is gone (NotFound) is read through the branch instead
// (heldRecoveryAlone, fourth decision of 2026-11-18). The predecessor leaves a merge of this request's commit to this
// request (mergedHeadOwner, decision 1).
func (r *RemediationRequestReconciler) heldRecovery(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, bool, error) {
	if !ptr.Deref(rr.Status.HoldsTarget, false) || !endedBeforeTakeover(rr) {
		return reconcile.Result{}, false, nil
	}
	from := rr.Status.Handoff.From
	pred := &remediationv1alpha1.RemediationRequest{}
	var look func(context.Context) (reconcile.Result, bool, error)
	switch err := r.getLive(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: string(from.Name)}, pred); {
	case apierrors.IsNotFound(err):
		// The predecessor is gone (fourth decision of 2026-11-18 on #786): the merge is found from the pull requests
		// of the chain's branch and the commit alone.
		look = func(ctx context.Context) (reconcile.Result, bool, error) { return r.heldRecoveryAlone(ctx, rr) }
	case err != nil:
		return reconcile.Result{}, true, fmt.Errorf("get the RemediationRequest %s replaces: %w", rr.Name, err)
	default:
		var to *remediationv1alpha1.OutgoingHandoff
		if pred.Status.Handoff != nil {
			to = pred.Status.Handoff.To
		}
		if pred.UID != types.UID(from.UID) || to == nil || to.UID != remediationv1alpha1.UID(rr.UID) ||
			pred.Status.PullRequest == nil {
			return reconcile.Result{}, false, nil
		}
		look = func(ctx context.Context) (reconcile.Result, bool, error) {
			return r.heldRecoveryRead(ctx, rr, pred, to)
		}
	}
	lookCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	defer cancel()
	res, done, err := look(lookCtx)
	err = recoveryStepErr(ctx, lookCtx, err)
	switch {
	case err == nil:
		return res, done, nil
	case ctx.Err() != nil:
		return reconcile.Result{}, true, err
	case errors.Is(err, freshness.ErrPaused):
		// The look, or the record of a merge it found, waits for writes to resume; the lock is kept meanwhile.
		res, err := r.waitWhilePaused(ctx, rr)
		return res, true, err
	case isClusterError(err):
		return reconcile.Result{}, true, err
	}
	// A repository that left the settings releases nothing: no read of the pull request can start, so no verdict
	// can be stored, and the replacement keeps the lock with PullRequestUnreadable (RepositoryUnavailable) until the
	// repository is back and a read settles it, or the request is deleted, as every stopped holder (decision of
	// 2026-11-19 on #786: a release there lost the record of a merge of its commit made meanwhile).
	if want := recoveryUnreadable(rr, err); want != nil {
		if werr := r.setUnreadable(ctx, rr, want); werr != nil {
			// The refusal goes with the failed write, so the error log of this attempt names it (as observeFailed).
			return reconcile.Result{}, true, errors.Join(err, werr)
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
	res, err = r.recoveryPace(ctx, rr, max(res.RequeueAfter, heldRecheckInterval))
	return res, true, err
}

// heldRecoveryAlone is heldRecovery's read once the predecessor is gone (NotFound), so neither its recorded pull
// request nor its intent can be read: the pull requests of the chain's branch are read through changeOf, as rr would
// read them had it taken the pull request over (the predecessor's marker counts as its own, inheritsMarker, in memory
// only), and one merged whose head is rr's handoff commit (isOwnCommitAlone, the commit alone) is recorded on rr with
// its receipt (recordFoundMerge); one merged with any other head, the predecessor's own included, is recorded on rr
// as a foreign change (recordOtherHeadMerge, decisions of 2026-11-20 on #870), since rr holds the lock and nobody
// else can: a predecessor that recorded the merge of its own head ended rr in that pass (supersedeReplacementAfterMerge),
// so rr still holds the lock here only when the predecessor went without a record. An open one keeps the lock, as in heldRecoveryRead, since it may still deliver rr's commit. Anything else
// settles nothing here (done false), and the verdict of the look decides. A failure is returned;
// a failure of the Kubernetes API is marked so (a *clusterError).
func (r *RemediationRequestReconciler) heldRecoveryAlone(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, bool, error) {
	git, err := r.openStoppedRead(ctx, rr)
	if err != nil {
		return reconcile.Result{}, true, err
	}
	inheriting := rr.DeepCopy()
	inheriting.Status.Handoff.From.Phase = remediationv1alpha1.HandoffDone
	v := r.changeOf(ctx, git, inheriting, changeQuery{})
	// changeOf may record PullRequestUnreadable through the copy: rr takes that write, never the copy's handoff.
	rr.ResourceVersion, rr.Status.Conditions = inheriting.ResourceVersion, inheriting.Status.Conditions
	if v.state == changeUnknown {
		return reconcile.Result{}, true, v.err
	}
	var other *providers.ChangeRequest
	pred := &remediationv1alpha1.RemediationRequest{}
	pred.Name, pred.Namespace = string(rr.Status.Handoff.From.Name), rr.Namespace
	for i, cr := range v.own {
		if cr.State != providers.ChangeRequestMerged {
			continue
		}
		switch ours, err := r.isOwnCommitAlone(ctx, git, rr, cr.HeadSHA); {
		case err != nil:
			return reconcile.Result{}, true, err
		case ours:
			return reconcile.Result{RequeueAfter: stepDelay}, true, r.recordFoundMerge(ctx, rr, pred, cr)
		case other == nil:
			other = &v.own[i]
		}
	}
	if v.state == changeMerged && other != nil {
		// Merged with any other head while this request holds the lock, the predecessor gone: this request records it
		// as a foreign change (decisions of 2026-11-20 on #870), the predecessor's own head included, since nobody
		// else can. One writer of the merge record, the one that holds the lock.
		return reconcile.Result{RequeueAfter: stepDelay}, true,
			r.ignoreStopped(r.recordOtherHeadMerge(ctx, git, rr, pred, *other))
	}
	if v.state != changeOpen {
		return reconcile.Result{}, false, nil
	}
	if err := r.setUnreadable(ctx, rr, nil); err != nil {
		return reconcile.Result{}, true, err
	}
	ours, err := r.isOwnCommitAlone(ctx, git, rr, v.pr.HeadSHA)
	if err != nil {
		return reconcile.Result{}, true, err
	}
	if err := r.noteHeldOpen(ctx, rr, v.pr.Number, ours); err != nil {
		return reconcile.Result{}, true, err
	}
	logf.FromContext(ctx).Info("Kept the target lock of RemediationRequest: a pull request of its branch that may carry "+
		"its commit is open", "remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", v.pr.Number)
	res, err := r.recoveryPace(ctx, rr, heldRecheckInterval)
	return res, true, err
}

// endOnOtherMerge settles rr, a replacement that ended before it took the pull request over (endedBeforeTakeover),
// once that pull request, cr, is found merged with a head that is not its commit (mergedHeadOwner: the predecessor's
// own merge, or a foreign one, which the predecessor records; with the predecessor gone, any merged head that is not
// rr's commit). The handoff did not happen and can no longer happen through that pull request, so rr's handoff moves
// to Aborted and its lock is released (settleHandoffAborted) in one status write. An end that the circuit breaker
// counts as a failure (the end of a wait, RecoveryWaitExpired or HandoffRecoveryFailed, decided from recorded state
// while the Git host could not be read) becomes Cancelled with ReasonHandoffAborted, as a handoff ends when the pull
// request is merged around it (afterUpdate): that merge is no failure of this request. A failed write is returned for
// a retry.
func (r *RemediationRequestReconciler) endOnOtherMerge(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest) error {
	rr.Status.Handoff.From.Phase = remediationv1alpha1.HandoffAborted
	if s, ok := settleHandoffAborted(rr); ok {
		if err := lockverdict.Release(&rr.Status, s); err != nil {
			return err
		}
	}
	clearReleasedRead(rr)
	logf.FromContext(ctx).Info("Released the target lock of RemediationRequest: the pull request it was to take over "+
		"was merged with a head that is not its commit", "remediationRequest", client.ObjectKeyFromObject(rr),
		"pullRequest", cr.Number, "head", cr.HeadSHA)
	if breakerOutcome(rr) != outcomeFailure {
		if err := r.Status().Update(ctx, rr); err != nil {
			return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
		}
		return nil
	}
	// It leaves its final phase, so the breaker's counts of that end go with it (classifyBreaker counts the new end).
	rr.Status.BreakerCounts = nil
	return r.ignoreStopped(r.stop(ctx, rr, remediationv1alpha1.PhaseCancelled, remediationv1alpha1.ReasonHandoffAborted,
		fmt.Sprintf("Pull request #%d, which this request was to take over, was merged with head %s, which is not "+
			"this request's commit; nothing was taken over", cr.Number, cr.HeadSHA)))
}

// endMergedElsewhere ends rr, a replacement whose handoff was aborted at a refusal and that watches for a merge of its
// commit (observeStep), once the pull request it was to take over, cr, is found merged into a base a person moved it
// to, not the target branch (changeMergedElsewhere; decision of 2026-11-19 on #786). Nothing is left to recover, and
// the target branch did not get the change, so the request ends as a cancel or a watch does on such a merge
// (stopMergedElsewhere): NeedsIntervention with ChangeRequestOnOtherBase and a message that names the pull request and
// its base. Its handoff moves to Aborted and its lock is released (settleHandoffAborted) in that one status write, which
// also drops its HandoffRecovery entry. Nothing is written to the pull request. A failed write is returned for a retry:
// nothing is recorded then, and the next reconcile reads the pull request again.
func (r *RemediationRequestReconciler) endMergedElsewhere(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest) error {
	rr.Status.Handoff.From.Phase = remediationv1alpha1.HandoffAborted
	s, released := settleHandoffAborted(rr)
	if released {
		if err := lockverdict.Release(&rr.Status, s); err != nil {
			return err
		}
	}
	err := r.ignoreStopped(r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonChangeRequestOnOtherBase,
		fmt.Sprintf("Pull request #%d, which this request was to take over, was merged into %s, where it was moved, "+
			"not into the target branch, so its change did not reach the target branch; automation stopped and "+
			"Shadewright writes nothing to it", cr.Number, otherBaseText(cr))))
	if err != nil {
		return err
	}
	if !released {
		// No verdict releases it (its status records a pull request): the look at stopped holders decides the lock.
		logf.FromContext(ctx).Info("Stopped RemediationRequest with its target lock kept: the pull request it was to "+
			"take over was merged into another base", "remediationRequest", client.ObjectKeyFromObject(rr),
			"pullRequest", cr.Number, "base", cr.BaseBranch)
		return nil
	}
	logf.FromContext(ctx).Info("Released the target lock of RemediationRequest: the pull request it was to take over "+
		"was merged into another base", "remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", cr.Number,
		"base", cr.BaseBranch)
	return nil
}

// endWithoutCommit settles rr, a replacement that ended before it took the pull request over (endedBeforeTakeover),
// once that pull request, cr, is found open with the predecessor's own head: rr's commit is not on it and cannot get
// there, since only rr pushes it and rr has ended. Its handoff moves to Aborted and its lock is released
// (settleHandoffAborted) in one status write; its phase and reason stay. A failed write is returned for a retry.
func (r *RemediationRequestReconciler) endWithoutCommit(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest) error {
	rr.Status.Handoff.From.Phase = remediationv1alpha1.HandoffAborted
	if s, ok := settleHandoffAborted(rr); ok {
		if err := lockverdict.Release(&rr.Status, s); err != nil {
			return err
		}
	}
	clearReleasedRead(rr)
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	logf.FromContext(ctx).Info("Released the target lock of RemediationRequest: the pull request it was to take over "+
		"is open without its commit", "remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", cr.Number)
	return nil
}

// isOwnCommitAlone reports whether head is rr's handoff commit, judged from the commit alone when the predecessor is
// gone: it has one parent and changes nothing but the content of rr's target file (CheckCommit, which also judges a
// commit a merge took into the target branch), and the file at head is the file at that parent with rr's value set
// over the value the parent holds, by the typed patcher. A file the patcher refuses is not rr's commit; an error is a
// read worth retrying.
func (r *RemediationRequestReconciler) isOwnCommitAlone(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, head string) (bool, error) {
	path := string(rr.Status.Source.Path)
	state, parent, err := git.CheckCommit(ctx, head, path)
	if err != nil {
		return false, freshness.WrapProvider("check what the merged branch head holds", err)
	}
	if state != providers.BranchOneChange {
		return false, nil
	}
	before, err := git.ReadFile(ctx, parent, path, providers.MaxFileBytes)
	if err != nil {
		return false, freshness.WrapProvider("read the file of the pull request", err)
	}
	after, err := git.ReadFile(ctx, head, path, providers.MaxFileBytes)
	if err != nil {
		return false, freshness.WrapProvider("read the file of the pull request", err)
	}
	field, err := sourcemap.FieldFor(rr.Spec.Action, rr.Spec.TargetRef)
	if err != nil {
		return false, nil
	}
	v, found, err := sourcemap.ReadField(before.Data, rr.Spec.TargetRef, field)
	if err != nil || !found {
		return false, nil
	}
	a, ok := sourcemap.WithPrevious(rr.Spec.Action, v)
	if !ok {
		return false, nil
	}
	want, err := applyAction(before.Data, rr.Spec.TargetRef, a, false)
	if err != nil {
		return false, nil
	}
	return bytes.Equal(want, after.Data), nil
}

// heldRecoveryRead is heldRecovery's read through the plan's repository. A failure is returned; a failure of the
// Kubernetes API is marked so (a *clusterError).
func (r *RemediationRequestReconciler) heldRecoveryRead(ctx context.Context, rr, pred *remediationv1alpha1.RemediationRequest,
	to *remediationv1alpha1.OutgoingHandoff) (reconcile.Result, bool, error) {
	git, err := r.openStoppedRead(ctx, rr)
	if err != nil {
		return reconcile.Result{}, true, err
	}
	v := r.changeOf(ctx, git, pred, changeQuery{})
	switch v.state {
	case changeUnknown:
		return reconcile.Result{}, true, v.err
	case changeMerged:
		switch recorded, err := r.recordWatchedMerge(ctx, git, rr, pred, to, v); {
		case err != nil:
			return reconcile.Result{}, true, err
		case recorded:
			return reconcile.Result{RequeueAfter: stepDelay}, true, nil
		}
		return reconcile.Result{}, true, r.endOnOtherMerge(ctx, rr, v.pr)
	case changeOpen:
		if v.pr.HeadSHA == pred.Status.PullRequest.HeadSHA || v.pr.HeadSHA == to.ExpectedHead {
			// The head is the predecessor's own: rr never pushed its commit, and as an ended request it never will
			// (only its own takeOver pushes it), so nothing on this pull request is rr's to hold.
			return reconcile.Result{}, true, r.endWithoutCommit(ctx, rr, v.pr)
		}
		if err := r.setUnreadable(ctx, rr, nil); err != nil {
			return reconcile.Result{}, true, err
		}
		ours, err := r.isHandoffCommit(ctx, git, rr, pred, to, v.pr.HeadSHA)
		if err != nil {
			return reconcile.Result{}, true, err
		}
		if err := r.noteHeldOpen(ctx, rr, v.pr.Number, ours); err != nil {
			return reconcile.Result{}, true, err
		}
		logf.FromContext(ctx).Info("Kept the target lock of RemediationRequest: the pull request that may carry its "+
			"commit is open", "remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", v.pr.Number)
		res, err := r.recoveryPace(ctx, rr, heldRecheckInterval)
		return res, true, err
	case changeClosed, changeGone, changeElsewhere, changeMergedElsewhere:
		// Settled by the verdict of the look (settleTargetLock): closed or gone release; open on a base a person
		// moved it to holds the target through the predecessor, whose own pull request it is (stopOpenElsewhere),
		// not through this request; merged there keeps none (ADR-0043). Nothing here records a merge into another
		// base. A merge there seen during the watch ended the request before this look (endMergedElsewhere).
	}
	return reconcile.Result{}, false, nil
}

// ReasonPersonCommitOnPullRequest is the reason of condition TargetHeld on a replacement that ended before it took the
// pull request over (endedBeforeTakeover) and keeps the lock while that pull request is open with a head that is not
// its handoff commit alone: a person's commit is on it (on top of the replacement's commit, or in its place), and the
// pull request may still deliver the replacement's commit until it is merged or closed (second round of #786).
const ReasonPersonCommitOnPullRequest = "PersonCommitOnPullRequest"

// noteHeldOpen records why rr, a replacement that ended before its takeover, keeps the lock for the open pull request
// number: with a person's commit on it (ours false), condition TargetHeld with ReasonPersonCommitOnPullRequest; with
// its handoff commit alone as the head (ours true), no TargetHeld of that reason. The write is made only when the
// condition changes, on a fresh read, and is retried on a conflict; a failure is the Kubernetes API's (a
// *clusterError).
func (r *RemediationRequestReconciler) noteHeldOpen(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	number int64, ours bool) error {
	change := func(conds *[]metav1.Condition) {
		if ours {
			if c := meta.FindStatusCondition(*conds, remediationv1alpha1.ConditionTargetHeld); c != nil &&
				c.Reason == ReasonPersonCommitOnPullRequest {
				meta.RemoveStatusCondition(conds, remediationv1alpha1.ConditionTargetHeld)
			}
			return
		}
		meta.SetStatusCondition(conds, metav1.Condition{
			Type: remediationv1alpha1.ConditionTargetHeld, Status: metav1.ConditionTrue,
			Reason: ReasonPersonCommitOnPullRequest, ObservedGeneration: rr.Generation,
			Message: fmt.Sprintf("Pull request #%d, which this request was to take over, is open with a commit "+
				"Shadewright did not push on its branch, and may still deliver this request's commit; the target stays "+
				"locked until it is merged or closed", number),
		})
	}
	probe := slices.Clone(rr.Status.Conditions)
	if change(&probe); equality.Semantic.DeepEqual(probe, rr.Status.Conditions) {
		return nil
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live := &remediationv1alpha1.RemediationRequest{}
		if err := r.reader().Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
			return err
		}
		if live.UID != rr.UID {
			return errors.New("RemediationRequest was replaced")
		}
		base := live.DeepCopy()
		change(&live.Status.Conditions)
		if equality.Semantic.DeepEqual(base.Status, live.Status) {
			rr.ResourceVersion, rr.Status.Conditions = live.ResourceVersion, live.Status.Conditions
			return nil
		}
		if err := r.Status().Patch(ctx, live, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		rr.ResourceVersion, rr.Status.Conditions = live.ResourceVersion, live.Status.Conditions
		return nil
	})
	if err != nil {
		return &clusterError{err: fmt.Errorf("record why RemediationRequest %s/%s keeps its target lock: %w",
			rr.Namespace, rr.Name, err)}
	}
	return nil
}
