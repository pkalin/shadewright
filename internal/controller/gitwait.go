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
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// A Git wait is recorded state (decisions of 2026-11-17 on #771): an entry of status.gitWaits per kind of bound
// (waitKind), with its start on the executor's clock, the read that failed, and, for the content wait, the pull
// request it is about. Starting or ending one kind never touches another (putGitWaits, dropGitWaits), and an entry
// is built whole (newWait, readOfContent): the content wait cannot be recorded without its pull request. The end
// of a kind is computed from recorded state alone (waitKind.end), and the check at the start of every reconcile
// (endPassedWait) ends each entry past its end before any read or step runs. A call site only starts or continues
// a wait (retryGitRead); it never looks at whether the end has passed, so an error, a timeout or an early return in
// a step cannot skip the end. An entry past its end always ends: by its kind's end, or, when the kind no longer
// applies to the request, by its removal; never by an error that repeats. Condition GitReadRetry summarizes the
// entries for people (gitWaitSummary).

// waitKind is the kind of bound of a Git wait.
type waitKind int

const (
	// waitNone has no end: the retry is paced, nothing ends it (entry kind Paced, condition reason GitReadFailed).
	waitNone waitKind = iota
	// waitRequestDeadline ends at status.deadline of a request in PolicyChecked whose stale plan's cleanup has not
	// started: the request expires (expire).
	waitRequestDeadline
	// waitHandoffGrace ends handoffRecoveryGrace after status.deadline of a request recovering a handoff: it ends
	// NeedsIntervention with HandoffRecoveryFailed.
	waitHandoffGrace
	// waitMergeCheck ends mergeCommitWait after status.mergeCommitAwaitedSince (the entry's start when that is
	// unset), the wait for the check at the merge: the plan is marked not shown fresh, and the merge is recorded
	// with that mark.
	waitMergeCheck
	// waitMergeContent ends mergeContentWait after the entry's start, the reads of what a merge into the target
	// branch the settings name now brought (#771): the request ends NeedsIntervention with MergeContentUnreadable,
	// with the pull request the entry names.
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

var gitWaitKinds = map[waitKind]api.GitWaitKind{
	waitNone:            api.GitWaitPaced,
	waitRequestDeadline: api.GitWaitRequestDeadline,
	waitHandoffGrace:    api.GitWaitHandoffGrace,
	waitMergeCheck:      api.GitWaitMergeCheck,
	waitMergeContent:    api.GitWaitMergeContent,
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

// kindOfEntry is the kind of an entry of status.gitWaits.
func kindOfEntry(w api.GitWait) waitKind {
	for k, a := range gitWaitKinds {
		if a == w.Kind {
			return k
		}
	}
	return waitNone
}

// gitWait is rr's entry of kind k, or nil.
func gitWait(rr *api.RemediationRequest, k waitKind) *api.GitWait {
	for i := range rr.Status.GitWaits {
		if rr.Status.GitWaits[i].Kind == gitWaitKinds[k] {
			return &rr.Status.GitWaits[i]
		}
	}
	return nil
}

// newWait is a whole entry of kind k for a read op that failed at now. The content wait is not built here: it needs
// its pull request (readOfContent), so ok is false for it.
func newWait(k waitKind, op string, now time.Time) (api.GitWait, bool) {
	if k == waitMergeContent {
		return api.GitWait{}, false
	}
	return api.GitWait{Kind: gitWaitKinds[k], Since: metav1.NewTime(now.Truncate(time.Second)), Operation: op}, true
}

// end is when a wait of kind k ends, from the recorded state of rr alone; ok is false when nothing recorded bounds
// it (the kind does not apply to rr any more, or it has no entry and no clock). A clock that lies more than
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
	entrySince := func() *metav1.Time {
		if w := gitWait(rr, k); w != nil {
			return &w.Since
		}
		return nil
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
		if mergeRecorded(rr) {
			return time.Time{}, false
		}
		if rr.Status.MergeCommitAwaitedSince != nil {
			return clocked(rr.Status.MergeCommitAwaitedSince, mergeCommitWait)
		}
		return clocked(entrySince(), mergeCommitWait)
	case waitMergeContent:
		if mergeRecorded(rr) {
			return time.Time{}, false
		}
		return clocked(entrySince(), mergeContentWait)
	}
	return time.Time{}, false
}

// startClock records the start of the merge wait (status.mergeCommitAwaitedSince) for a wait of kind waitMergeCheck
// when it is not started, as the check at the merge does. A failed write is returned: the wait is not started, and
// the read is retried with the error backoff.
func (r *RemediationRequestReconciler) startClock(ctx context.Context, rr *api.RemediationRequest, k waitKind) error {
	if k == waitMergeCheck && rr.Status.MergeCommitAwaitedSince == nil {
		if err := r.startMergeWait(ctx, rr); err != nil {
			return &clusterError{err: err}
		}
	}
	return nil
}

// withinWaits caps d, a wait a path chose after retryGitRead (a slower pace of its own), at the earliest end of
// rr's Git waits: a pace never carries a wait past its end.
func (r *RemediationRequestReconciler) withinWaits(rr *api.RemediationRequest, d time.Duration) time.Duration {
	now := r.now()
	for _, w := range rr.Status.GitWaits {
		if end, ok := kindOfEntry(w).end(rr, now); ok {
			d = min(d, max(end.Sub(now), boundPassedWait))
		}
	}
	return d
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

// gitWaitSummary sets condition GitReadRetry in conditions from waits: removed when there is none; otherwise True
// with the reason of the entry that ends first (a Paced entry ends never), the reads named in the message, and that
// entry's start as lastTransitionTime.
func gitWaitSummary(rr *api.RemediationRequest, now time.Time) {
	if len(rr.Status.GitWaits) == 0 {
		meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionGitReadRetry)
		return
	}
	first := rr.Status.GitWaits[0]
	firstEnd, firstOK := kindOfEntry(first).end(rr, now)
	ops := make([]string, 0, len(rr.Status.GitWaits))
	for _, w := range rr.Status.GitWaits {
		if !slices.Contains(ops, w.Operation) {
			ops = append(ops, w.Operation)
		}
		if e, ok := kindOfEntry(w).end(rr, now); ok && (!firstOK || e.Before(firstEnd)) {
			first, firstEnd, firstOK = w, e, true
		}
	}
	want := metav1.Condition{Type: api.ConditionGitReadRetry, Status: metav1.ConditionTrue,
		Reason: kindOfEntry(first).reason(), Message: "Retrying Git read: " + strings.Join(ops, "; "),
		ObservedGeneration: rr.Generation, LastTransitionTime: first.Since}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionGitReadRetry); c != nil &&
		(c.Reason != want.Reason || !c.LastTransitionTime.Equal(&want.LastTransitionTime)) {
		meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionGitReadRetry)
	}
	meta.SetStatusCondition(&rr.Status.Conditions, want)
}

// showsGitWait reports whether rr records a Git wait or shows condition GitReadRetry.
func showsGitWait(rr *api.RemediationRequest) bool {
	return len(rr.Status.GitWaits) > 0 || meta.FindStatusCondition(rr.Status.Conditions, api.ConditionGitReadRetry) != nil
}

// dropWait removes rr's entry of kind k in memory, with the summary.
func dropWait(rr *api.RemediationRequest, k waitKind, now time.Time) {
	rr.Status.GitWaits = slices.DeleteFunc(rr.Status.GitWaits, func(w api.GitWait) bool { return w.Kind == gitWaitKinds[k] })
	if len(rr.Status.GitWaits) == 0 {
		rr.Status.GitWaits = nil
	}
	gitWaitSummary(rr, now)
}

// putGitWaits records adds in status.gitWaits: an entry of a kind that stands keeps its start and its pull request
// and takes the read that failed now; any other is added whole. Only the entries and their summary are written, in
// a patch with the resourceVersion read, retried on a conflict from a fresh read.
func (r *RemediationRequestReconciler) putGitWaits(ctx context.Context, rr *api.RemediationRequest, adds []api.GitWait) error {
	return r.patchGitWaits(ctx, rr, func(live *api.RemediationRequest) {
		for _, a := range adds {
			if w := gitWait(live, kindOfEntry(a)); w != nil {
				w.Operation = a.Operation
				continue
			}
			live.Status.GitWaits = append(live.Status.GitWaits, a)
		}
	})
}

// dropGitWaits removes the entries drop names, and the summary with the last.
func (r *RemediationRequestReconciler) dropGitWaits(ctx context.Context, rr *api.RemediationRequest,
	drop func(api.GitWait) bool) error {
	return r.patchGitWaits(ctx, rr, func(live *api.RemediationRequest) {
		live.Status.GitWaits = slices.DeleteFunc(live.Status.GitWaits, drop)
		if len(live.Status.GitWaits) == 0 {
			live.Status.GitWaits = nil
		}
	})
}

// patchGitWaits applies change to the entries of a fresh read of rr, sets the summary, and writes both alone; rr
// takes the result. A request replaced under the same name is a cluster error.
func (r *RemediationRequestReconciler) patchGitWaits(ctx context.Context, rr *api.RemediationRequest,
	change func(live *api.RemediationRequest)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live := &api.RemediationRequest{}
		if err := r.reader().Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
			return &clusterError{err: err}
		}
		if live.UID != rr.UID {
			return &clusterError{err: errors.New("RemediationRequest was replaced")}
		}
		base := live.DeepCopy()
		change(live)
		gitWaitSummary(live, r.now())
		if !equality.Semantic.DeepEqual(base.Status, live.Status) {
			if err := r.Status().Patch(ctx, live, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return fmt.Errorf("record the Git waits: %w", err)
			}
		}
		rr.ResourceVersion = live.ResourceVersion
		rr.Status.GitWaits = live.Status.GitWaits
		if c := meta.FindStatusCondition(live.Status.Conditions, api.ConditionGitReadRetry); c != nil {
			meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionGitReadRetry)
			meta.SetStatusCondition(&rr.Status.Conditions, *c)
		} else {
			meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionGitReadRetry)
		}
		return nil
	})
}

// endPassedWait is the check at the start of every reconcile of a request that is not being deleted: every entry
// of status.gitWaits past its end is ended before any read or step runs, by the kind's own end, earliest first. An
// entry whose kind no longer applies to the request (waitKind.end has no end) is removed. done is true when the
// reconcile stops there; a merge wait ended by its mark lets the reconcile go on, which then records the merge. A
// failed status write is returned for the error backoff. Only recorded state is read: the entries, the clocks, the
// deadline, and the executor's clock.
func (r *RemediationRequestReconciler) endPassedWait(ctx context.Context, rr *api.RemediationRequest) (
	ctrl.Result, bool, error) {
	now := r.now()
	var stale []waitKind
	for _, w := range rr.Status.GitWaits {
		if k := kindOfEntry(w); k != waitNone {
			if _, ok := k.end(rr, now); !ok {
				stale = append(stale, k)
			}
		}
	}
	if len(stale) > 0 {
		// Nothing is left for these to end: removed, never ended again.
		if err := r.dropGitWaits(ctx, rr, func(w api.GitWait) bool { return slices.Contains(stale, kindOfEntry(w)) }); err != nil {
			return ctrl.Result{}, true, err
		}
	}
	for {
		kind, ok := passedWait(rr, now)
		if !ok {
			return ctrl.Result{}, false, nil
		}
		res, done, err := r.endWait(ctx, rr, kind, now)
		if done || err != nil {
			return res, done, err
		}
	}
}

// passedWait is the kind of rr's entry past its end that ended first.
func passedWait(rr *api.RemediationRequest, now time.Time) (waitKind, bool) {
	var kind waitKind
	var at time.Time
	found := false
	for _, w := range rr.Status.GitWaits {
		k := kindOfEntry(w)
		if end, ok := k.end(rr, now); k != waitNone && ok && !now.Before(end) && (!found || end.Before(at)) {
			kind, at, found = k, end, true
		}
	}
	return kind, found
}

// endWait ends rr's entry of kind, past its end (endPassedWait).
func (r *RemediationRequestReconciler) endWait(ctx context.Context, rr *api.RemediationRequest, kind waitKind,
	now time.Time) (ctrl.Result, bool, error) {
	entry := *gitWait(rr, kind)
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr), "wait", entry.Kind)
	log.Info("Ended the Git wait of RemediationRequest: its bound passed")
	ctx = expiringCtx(ctx)
	// The wait ends in the write of its end below; it is not left to end again.
	dropWait(rr, kind, now)
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
		return ctrl.Result{}, true, r.ignoreStopped(r.endContentUnreadable(ctx, rr, entry))
	}
	return ctrl.Result{}, false, nil
}

// endContentUnreadable ends a request whose reads of what a retargeted merge brought kept failing for
// mergeContentWait (waitMergeContent, entry w): NeedsIntervention with MergeContentUnreadable, the stale mark, no
// receipt. The pull request w names is recorded when the status holds none (its status write was lost and the read
// found it by the request's marker and checked its head), so the end never depends on a write that may have been
// lost. The branch is the one the settings name now; when the settings cannot say it, the message says so instead.
// Only a failed Kubernetes read is returned, for the error backoff.
func (r *RemediationRequestReconciler) endContentUnreadable(ctx context.Context, rr *api.RemediationRequest,
	w api.GitWait) error {
	if rr.Status.PullRequest == nil && w.PullRequest != nil {
		recordFound(rr, providers.ChangeRequest{Number: w.PullRequest.Number, URL: w.PullRequest.URL,
			HeadSHA: w.PullRequest.HeadSHA, State: providers.ChangeRequestMerged})
	}
	branch := api.GitBranch("the target branch the settings name now")
	switch repo, _, err := r.planEntry(ctx, rr); {
	case err == nil || errors.Is(err, errTargetBranchChanged):
		branch = repo.TargetBranch
	case !errors.Is(err, errRepositoryGone) && !errors.Is(err, freshness.ErrPaused):
		// The read of the settings failed (a Kubernetes read): retried with the error backoff.
		return err
	}
	markStaleForBranch(rr, branch)
	return r.mergedForeignInto(ctx, rr, branch, &renderRefusal{reason: ReasonMergeContentUnreadable,
		message: "the reads of the Git host kept failing for " + mergeContentWait.String()})
}
