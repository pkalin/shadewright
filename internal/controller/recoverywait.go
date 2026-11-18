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
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
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
//     a read that fails is retried visibly (observeFailed). The end (endPassedWait, endRecoveryWait) is no verdict on
//     the lock: the replacement ends NeedsIntervention with RecoveryWaitExpired and keeps the lock until the look
//     that settles it (checkHeld, heldRecovery first).
//
// A read that fails without a refusal (a 5xx, a timeout) is as before #786: the intent stands until the deadline plus
// handoffRecoveryGrace (the HandoffGrace entry, retryRecovery). A refusal condition that stands without its entry (an
// executor that wrote the two apart) gets the entry from the condition's lastTransitionTime before any read
// (recordRefusalEntry), so every bound is read from recorded state.

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
		return reconcile.Result{RequeueAfter: stepDelay}, nil
	}
	r.unreadableEvent(rr, was, watchedRecovery(rr))
	logf.FromContext(ctx).Info("Aborted the handoff of RemediationRequest at a refused read of its recovery; it "+
		"watches for a merge of its commit until the end of its HandoffRecovery wait", "remediationRequest",
		client.ObjectKeyFromObject(rr), "reason", want.Reason, "error", err.Error())
	if res, perr := r.abortPredecessor(ctx, rr); perr != nil || res.RequeueAfter > 0 {
		return res, errors.Join(perr, clusterOnly(err))
	}
	return reconcile.Result{RequeueAfter: r.withinWaits(rr, r.pullRequestPollInterval())}, clusterOnly(err)
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
// (recordHandoff: Merged, or PRCreated while the merge commit or the check at the merge is awaited). Anything else
// writes nothing to Git and takes nothing over: a read that works removes condition PullRequestUnreadable, and the
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
	wait := reconcile.Result{RequeueAfter: r.withinWaits(rr, r.pullRequestPollInterval())}
	switch err := r.getLive(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: string(from.Name)}, pred); {
	case apierrors.IsNotFound(err):
		return wait, !cancelRequested(rr), nil
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
		return wait, !cancelRequested(rr), nil
	}
	git, err := r.openRecovery(ctx, rr)
	if err != nil {
		return reconcile.Result{}, true, err
	}
	v := r.changeOf(ctx, git, pred, changeQuery{})
	if v.state == changeUnknown {
		return reconcile.Result{}, true, v.err
	}
	if v.state == changeMerged && v.pr.HeadSHA != to.ExpectedHead {
		switch ours, err := r.isHandoffCommit(ctx, git, rr, pred, to, v.pr.HeadSHA); {
		case err != nil:
			return reconcile.Result{}, true, err
		case ours:
			if err := r.recordHandoff(ctx, rr, pred, v.pr, v.pr.HeadSHA); err != nil {
				return reconcile.Result{}, true, err
			}
			logf.FromContext(ctx).Info("Recorded the merge of the commit of RemediationRequest after its handoff "+
				"was aborted", "remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", v.pr.Number,
				"phase", rr.Status.Phase)
			return reconcile.Result{RequeueAfter: stepDelay}, true, nil
		}
	}
	if err := r.setUnreadable(ctx, rr, nil); err != nil {
		return reconcile.Result{}, true, err
	}
	return wait, !cancelRequested(rr), nil
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

// endRecoveryWait ends a replacement whose handoff was aborted at a refusal at the end of its waits: entry is its
// HandoffRecovery entry, and end the latest end of its waits (handoffWaitEnd), so the message states the span the
// entries recorded. The end is one status write: NeedsIntervention with ReasonRecoveryWaitExpired and its handoff
// Aborted (declineOnEnd). The target lock is kept: the end is no verdict, and the look that settles it (checkHeld,
// heldRecovery) keeps it while the pull request that may carry the request's commit is open. A merge after the end is
// not recorded on this request: its handoff is Aborted, which never becomes Done. PullRequestOpened is set before stop,
// so that the end keeps condition PullRequestUnreadable (dropRecoveryWait reads it).
func (r *RemediationRequestReconciler) endRecoveryWait(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	entry remediationv1alpha1.GitWait, end time.Time) error {
	logf.FromContext(ctx).Info("Ended the watch of RemediationRequest after the abort of its handoff: its waits passed",
		"remediationRequest", client.ObjectKeyFromObject(rr), "since", entry.Since.Time, "end", end)
	why := ""
	if c := watchedRecovery(rr); c != nil {
		why = fmt.Sprintf(" (%s, see condition %s)", c.Reason, remediationv1alpha1.ConditionPullRequestUnreadable)
	}
	msg := fmt.Sprintf("The handoff was aborted at a refused read of its recovery on %s%s; this request watched the "+
		"pull request it was to take over for %s, until %s, and saw no merge of its commit. It keeps the target lock "+
		"while that pull request may still be merged; a later merge is not recorded on this request",
		entry.Since.UTC().Format(time.RFC3339), why, end.Sub(entry.Since.Time), end.UTC().Format(time.RFC3339))
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type: remediationv1alpha1.ConditionPROpened, Status: metav1.ConditionFalse, Reason: ReasonRecoveryWaitExpired,
		Message: msg, ObservedGeneration: rr.Generation,
	})
	return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonRecoveryWaitExpired, msg)
}

// endedAtRecoveryBound reports whether rr ended NeedsIntervention at the end of the watch after its handoff was
// aborted at a refusal (endRecoveryWait), with no pull request recorded and its handoff Aborted. It reads the status
// only.
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

// heldRecovery is checkHeld's first read for a request endedAtRecoveryBound (#786): the pull request the predecessor
// recorded, by its number (changeOf, as the recovery reads it), which may carry this request's commit although no pull
// request of this request is recorded or marked. It never releases the lock; only a verdict of the look after it
// does. done is true when this read settles the look:
//   - open: the pull request may still deliver this request's commit, whatever head it has now, so the lock is kept,
//     PullRequestUnreadable goes (it was read), and it is read again after heldRecheckInterval;
//   - a failed Git read: the lock is kept and the read is retried visibly (retryGitRead, a stopped holder's paced
//     wait, never sooner than heldRecheckInterval); a final refusal or settings that cannot reach the repository set
//     PullRequestUnreadable (recoveryUnreadable); a pause waits (waitWhilePaused); a failure of the Kubernetes API,
//     or any other, is returned for the error backoff.
//
// done is false, and the look decides as for any stopped request, when rr does not hold the lock or did not end at the
// bound of its watch (endedAtRecoveryBound), or when the predecessor is gone or made again, no longer names this
// request in its handoff, or its pull request is closed, gone or merged. A merge is not recorded here: the handoff of
// a request that ended is Aborted, and an incoming handoff never moves from Aborted to Done (a rule of the CRD), so the
// predecessor's own look records what became of its pull request.
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
	res, done, err := r.heldRecoveryRead(lookCtx, rr, pred)
	err = recoveryStepErr(ctx, lookCtx, err)
	switch {
	case err == nil:
		return res, done, nil
	case ctx.Err() != nil, isClusterError(err):
		return reconcile.Result{}, true, err
	case errors.Is(err, freshness.ErrPaused):
		// The look waits for writes to resume; the lock is kept meanwhile.
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
func (r *RemediationRequestReconciler) heldRecoveryRead(ctx context.Context, rr, pred *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, bool, error) {
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
	}
	return reconcile.Result{}, false, nil
}
