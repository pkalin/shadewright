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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
)

// A Git wait is recorded state (decision of 2026-11-17 on #771): condition GitReadRetry, whose reason is the kind of
// its bound (waitKind) and whose lastTransitionTime is its start on the executor's clock, and, for a kind with a
// clock of its own, that clock in the status. The end of a kind is computed from recorded state alone (waitKind.end),
// in one place, and the check at the start of every reconcile (endPassedWait) ends a wait past its end before any
// read or step runs. A call site only starts or continues a wait (retryGitRead with boundBy); it never looks at
// whether the end has passed, so an error, a timeout or an early return in a step cannot skip the end.

// waitKind is the kind of bound of a Git wait.
type waitKind int

const (
	// waitNone has no end: the retry is paced, nothing ends it (condition reason GitReadFailed).
	waitNone waitKind = iota
	// waitRequestDeadline ends at status.deadline of a request in PolicyChecked whose stale plan's cleanup has not
	// started: the request expires (expire).
	waitRequestDeadline
	// waitHandoffGrace ends handoffRecoveryGrace after status.deadline of a request recovering a handoff: it ends
	// NeedsIntervention with HandoffRecoveryFailed.
	waitHandoffGrace
	// waitMergeCheck ends mergeCommitWait after status.mergeCommitAwaitedSince, the wait for the check at the
	// merge: the plan is marked not shown fresh, and the merge is recorded with that mark.
	waitMergeCheck
	// waitMergeContent ends mergeContentWait after status.mergeContentUnreadSince, the reads of what a merge into
	// the target branch the settings name now brought (#771): the request ends NeedsIntervention with
	// MergeContentUnreadable.
	waitMergeContent
)

// Reasons of condition GitReadRetry True, one per kind of wait; ReasonGitReadFailed is the wait without an end.
const (
	ReasonGitReadUntilDeadline     = "GitReadFailedBeforeDeadline"
	ReasonGitReadUntilHandoffGrace = "GitReadFailedBeforeHandoffGrace"
	ReasonGitReadUntilMergeWait    = "GitReadFailedBeforeMergeWait"
	ReasonGitReadUntilContentWait  = "GitReadFailedBeforeContentWait"
)

var gitWaitReasons = map[waitKind]string{
	waitNone:            api.ReasonGitReadFailed,
	waitRequestDeadline: ReasonGitReadUntilDeadline,
	waitHandoffGrace:    ReasonGitReadUntilHandoffGrace,
	waitMergeCheck:      ReasonGitReadUntilMergeWait,
	waitMergeContent:    ReasonGitReadUntilContentWait,
}

// reason is the reason of condition GitReadRetry for a wait of kind k.
func (k waitKind) reason() string { return gitWaitReasons[k] }

// waitKindOf is the kind a GitReadRetry reason records; any other reason (one written by hand) is waitNone, which
// ends nothing.
func waitKindOf(reason string) waitKind {
	for k, r := range gitWaitReasons {
		if r == reason {
			return k
		}
	}
	return waitNone
}

// end is when a wait of kind k ends, from the recorded state of rr alone; ok is false when nothing recorded bounds
// it (the kind does not apply to rr any more, or its clock is not started). A clock that lies more than
// mergeCommitClockSlack after now (the clock went back) ends at now, as mergeCommitWaitRemaining does.
func (k waitKind) end(rr *api.RemediationRequest, now time.Time) (time.Time, bool) {
	clocked := func(since *metav1.Time, wait time.Duration) (time.Time, bool) {
		switch {
		case since == nil:
			return time.Time{}, false
		case since.After(now.Add(mergeCommitClockSlack)):
			return now, true
		}
		return since.Add(wait), true
	}
	switch k {
	case waitRequestDeadline:
		if rr.Status.Phase == api.PhasePolicyChecked && rr.Status.Deadline != nil && staleMark(rr) == nil &&
			intent(rr) == nil && !recoveringHandoff(rr) {
			return rr.Status.Deadline.Time, true
		}
	case waitHandoffGrace:
		if recoveringHandoff(rr) && rr.Status.Deadline != nil {
			return rr.Status.Deadline.Add(handoffRecoveryGrace), true
		}
	case waitMergeCheck:
		return clocked(rr.Status.MergeCommitAwaitedSince, mergeCommitWait)
	case waitMergeContent:
		return clocked(rr.Status.MergeContentUnreadSince, mergeContentWait)
	}
	return time.Time{}, false
}

// earlierKind is whichever of a and b ends first for rr at now; a kind with an end beats one without.
func earlierKind(rr *api.RemediationRequest, now time.Time, a, b waitKind) waitKind {
	ea, oka := a.end(rr, now)
	eb, okb := b.end(rr, now)
	switch {
	case !okb && b != waitNone && !oka:
		// b's clock is not started yet (startClock starts it now): it is the read that failed.
		return b
	case !okb:
		return a
	case !oka || eb.Before(ea):
		return b
	}
	return a
}

// startClock records the start of a wait of kind k when k has a clock of its own and it is not started: the merge
// wait (status.mergeCommitAwaitedSince) or the content wait (status.mergeContentUnreadSince). A failed write is
// returned: the wait is not started, and the read is retried with the error backoff.
func (r *RemediationRequestReconciler) startClock(ctx context.Context, rr *api.RemediationRequest, k waitKind) error {
	switch {
	case k == waitMergeCheck && rr.Status.MergeCommitAwaitedSince == nil:
		if err := r.startMergeWait(ctx, rr); err != nil {
			return &clusterError{err: err}
		}
	case k == waitMergeContent && rr.Status.MergeContentUnreadSince == nil:
		return r.startContentWait(ctx, rr)
	}
	return nil
}

// expiringKey marks the context of the path that ends a wait (endPassedWait, expire).
type expiringKey struct{}

// expiringCtx marks ctx as the path that ends a wait: a read that fails in it starts no bounded wait of its own
// (retryGitRead records waitNone), so it is retried at the poll interval and never at once.
func expiringCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, expiringKey{}, true)
}

// expiring reports whether ctx is the path that ends a wait (expiringCtx).
func expiring(ctx context.Context) bool {
	v, _ := ctx.Value(expiringKey{}).(bool)
	return v
}

// endPassedWait is the check at the start of every reconcile of a request that is not being deleted: when the Git
// wait the request records (condition GitReadRetry, its reason the kind) is past its end, it ends the wait before
// any read or step runs, by the kind's own end. done is true when the reconcile stops there; a merge wait ended by
// its mark lets the reconcile go on, which then records the merge. A failed status write is returned for the error
// backoff. Only recorded state is read: the condition, the clocks, the deadline, and the executor's clock.
func (r *RemediationRequestReconciler) endPassedWait(ctx context.Context, rr *api.RemediationRequest) (
	ctrl.Result, bool, error) {
	c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionGitReadRetry)
	if c == nil || c.Status != metav1.ConditionTrue {
		return ctrl.Result{}, false, nil
	}
	kind := waitKindOf(c.Reason)
	now := r.now()
	end, ok := kind.end(rr, now)
	if kind == waitNone || !ok || now.Before(end) {
		return ctrl.Result{}, false, nil
	}
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr), "wait", c.Reason)
	log.Info("Ended the Git wait of RemediationRequest: its bound passed")
	ctx = expiringCtx(ctx)
	// The wait ends in the write below; the kind is not left to end it again.
	meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionGitReadRetry)
	switch kind {
	case waitRequestDeadline:
		res, err := r.expire(ctx, rr)
		return res, true, err
	case waitHandoffGrace:
		return ctrl.Result{}, true, r.endRecoveryFailed(ctx, rr)
	case waitMergeCheck:
		markNotShownFresh(rr, "it did not answer")
		if r.Recorder != nil {
			r.Recorder.Event(rr, corev1.EventTypeWarning, ReasonStalePlanMerged, notShownFreshMessage("it did not answer"))
		}
		if err := r.Status().Update(ctx, rr); err != nil {
			return ctrl.Result{}, true, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace,
				rr.Name, err)
		}
		return ctrl.Result{}, false, nil
	case waitMergeContent:
		return ctrl.Result{}, true, r.ignoreStopped(r.endContentUnreadable(ctx, rr))
	}
	return ctrl.Result{}, false, nil
}

// endContentUnreadable ends a request whose reads of what a retargeted merge brought kept failing for
// mergeContentWait (waitMergeContent): NeedsIntervention with MergeContentUnreadable, the stale mark, no receipt.
// The pull request was recorded with the start of the wait (startContentWait); the branch is the one the settings
// name now.
func (r *RemediationRequestReconciler) endContentUnreadable(ctx context.Context, rr *api.RemediationRequest) error {
	if rr.Status.PullRequest == nil {
		// Never so: the clock is started only with the pull request recorded.
		return errors.New("the content wait of RemediationRequest has no pull request recorded")
	}
	branch := api.GitBranch("the target branch the settings name now")
	switch repo, _, err := r.planEntry(ctx, rr); {
	case err == nil || errors.Is(err, errTargetBranchChanged):
		branch = repo.TargetBranch
	case isClusterError(err) || !errors.Is(err, errRepositoryGone) && !errors.Is(err, freshness.ErrRepositoryUnusable):
		return err
	}
	markStaleForBranch(rr, branch)
	rr.Status.MergeContentUnreadSince = nil
	return r.mergedForeignInto(ctx, rr, branch, &renderRefusal{reason: ReasonMergeContentUnreadable,
		message: "the reads of the Git host kept failing for " + mergeContentWait.String()})
}
