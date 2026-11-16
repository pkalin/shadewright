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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// A target branch that changed in the settings after the pull request was opened (#754). The plan is then stale
// (freshness: SettingsChanged), and nothing more is written for it: the request stops in NeedsIntervention with
// TargetBranchChanged, as before. A merge of its pull request is still a merge, though, and a person may make it
// before the executor looks, on the recorded branch or after moving the pull request to the branch the settings
// name now. The look at the pull request (watchStep) therefore reads it twice first, by its recorded number, in
// the recorded repository (status.planRepository, invariant 5), through providers that write nothing:
//
//  1. on the target branch the repository entry names now (#771, mergedRetargeted): merged with the recorded
//     number, the head Shadewright pushed and this request's marker, it is recorded with a receipt that names
//     that branch (recordRetargetedMerge). A merge into that branch is the only one recorded off the recorded
//     branch: any other base is not read as a merge;
//  2. on the recorded target branch (status.source): merged with the head Shadewright pushed, the merge is recorded
//     as any other (recordMergeSeen), with its receipt.
//
// Either way the check at the merge finds the plan stale (SettingsChanged), so the request ends Inconclusive with
// StalePlanMerged, neutral for the circuit breaker, like any merged stale plan. Otherwise:
//   - a read that fails, or a provider that fails to open: retried (errBranchChangedUnread), never an end, since
//     the pull request may be merged. Condition PullRequestUnreadable says so on every such failure (reason
//     TargetBranchChanged, or the refusal's reason when the Git host refuses for good), and the next read that
//     works removes it; on the configured branch only a merge that counts (retargetedMergeCounts) does. As on the look at any recorded pull request, nothing bounds the retries in time: only the
//     error backoff, or the delay the host asked for, capped at freshRecheckInterval, paces them;
//   - anything else (open, closed, gone, another head, a merge into a third branch): errTargetBranchChanged,
//     handled as before.

// errBranchChangedUnread marks a look at the pull request of a plan whose target branch changed that could not
// read the pull request: recheck retries it (prBranchUnread) and does not go on to the freshness check, which
// would end the request in NeedsIntervention without knowing whether it was merged.
var errBranchChangedUnread = errors.New("the target branch changed and the pull request could not be read")

// watchBranchChanged is watchStep for a plan whose repository entry, repo, now names another target branch than
// the recorded one (see above). It returns what recordRetargetedMerge or recordMergeSeen returns for a merge
// that counts, errBranchChangedUnread wrapping a failed read, and errTargetBranchChanged for every other answer.
func (r *RemediationRequestReconciler) watchBranchChanged(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, repo remediationv1alpha1.GitRepository) error {
	pr, src := rr.Status.PullRequest, rr.Status.Source
	if pr == nil || src == nil {
		return errTargetBranchChanged
	}
	if done, err := r.mergedRetargeted(ctx, rr, repo); done {
		return err
	}
	git, err := r.openRecordedBranch(ctx, repo, src.TargetBranch)
	switch {
	case providers.IsInvalidConfig(err):
		// Settings that can never open the repository read nothing, now or later: as before.
		logf.FromContext(ctx).Info("Could not open the recorded repository of RemediationRequest after its target "+
			"branch changed, so a merge of its pull request, if any, was not looked up", "remediationRequest",
			client.ObjectKeyFromObject(rr), "pullRequest", pr.Number)
		return errTargetBranchChanged
	case err != nil:
		return r.branchChangedOpenFailed(ctx, rr, err)
	}
	// By its number, on the recorded target branch (changeOf reads status.source); a failed read sets condition
	// PullRequestUnreadable (noteRead, recordedBranch).
	v := r.changeOf(ctx, git, rr, changeQuery{recordedBranch: true})
	switch {
	case v.state == changeUnknown:
		return fmt.Errorf("%w: %w", errBranchChangedUnread, v.err)
	case v.state == changeMerged && v.pr.Number == pr.Number && v.pr.HeadSHA == pr.HeadSHA:
		return r.recordMergeSeen(ctx, rr, v.pr)
	}
	return errTargetBranchChanged
}

// mergedRetargeted reads the recorded pull request on the target branch repo names now (#771): done is true, with
// what recordRetargetedMerge returns, when it is merged there with the recorded number, the head Shadewright pushed
// and this request's marker, and with errBranchChangedUnread when the read or the open of the provider failed.
// Any other answer, and settings that can never open the repository, leave the look to the recorded branch.
func (r *RemediationRequestReconciler) mergedRetargeted(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	repo remediationv1alpha1.GitRepository) (bool, error) {
	git, err := r.openRecordedBranch(ctx, repo, repo.TargetBranch)
	switch {
	case providers.IsInvalidConfig(err):
		// The open for the recorded branch fails the same way and says so (watchBranchChanged).
		return false, nil
	case err != nil:
		return true, r.branchChangedOpenFailed(ctx, rr, err)
	}
	v := r.changeOf(ctx, git, rr, changeQuery{configuredBranch: repo.TargetBranch})
	switch {
	case v.state == changeUnknown:
		return true, fmt.Errorf("%w: %w", errBranchChangedUnread, v.err)
	case retargetedMergeCounts(rr, v):
		return true, r.recordRetargetedMerge(ctx, rr, v.pr, repo.TargetBranch)
	}
	return false, nil
}

// retargetedMergeCounts reports whether v, an answer of the read on the configured target branch (changeOf with
// q.configuredBranch, which binds it to that branch and this request's marker), is a merge that counts: merged,
// with the recorded number and the head Shadewright pushed. Only such an answer is recorded (mergedRetargeted) and
// removes condition PullRequestUnreadable (noteRead).
func retargetedMergeCounts(rr *remediationv1alpha1.RemediationRequest, v changeVerdict) bool {
	pr := rr.Status.PullRequest
	return pr != nil && v.state == changeMerged && v.pr.Number == pr.Number && v.pr.HeadSHA == pr.HeadSHA
}

// recordRetargetedMerge is recordMergeSeen for cr, the request's pull request merged into branch, the target
// branch the settings name now, after a person moved it there (#771). The plan was made for another branch, so it
// is stale whatever the check at the merge finds: when that check finds nothing (the settings changed back
// meanwhile), the plan gets the stale mark with reason SettingsChanged all the same, so the outcome is capped
// (StalePlanMerged). The receipt names branch.
func (r *RemediationRequestReconciler) recordRetargetedMerge(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest, branch remediationv1alpha1.GitBranch) error {
	switch left, err := r.awaitMergeCommit(ctx, rr, cr); {
	case err != nil:
		return fmt.Errorf("%w: %w", errMergeCheck, err)
	case left > 0:
		return errMergeCommitPending
	}
	if err := r.checkAtMerge(ctx, rr); err != nil {
		return err
	}
	if staleMark(rr) == nil {
		markStaleAtMerge(rr, freshness.ReasonSettingsChanged, "The plan was stale when the merge was seen: the "+
			"pull request was merged into "+string(branch)+", not into the target branch the plan was made for")
	}
	logf.FromContext(ctx).Info("The pull request of RemediationRequest was merged into the target branch the "+
		"settings name now, not the one the plan was made for", "remediationRequest", client.ObjectKeyFromObject(rr),
		"pullRequest", cr.Number, "branch", branch)
	recordMergeInto(ctx, rr, cr, branch)
	err := r.stop(ctx, rr, remediationv1alpha1.PhaseMerged, ReasonChangeRequestMerged,
		"The pull request was merged into "+string(branch)+", the target branch the settings name now")
	if err != nil && !errors.Is(err, errStopped) {
		return fmt.Errorf("%w: %w", errMergeCheck, err)
	}
	return err
}

// branchChangedOpenFailed handles a provider for a read after the target branch changed that failed to open, other
// than for good: condition PullRequestUnreadable (reason TargetBranchChanged, or the refusal's), and
// errBranchChangedUnread for a retry.
func (r *RemediationRequestReconciler) branchChangedOpenFailed(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, err error) error {
	err = freshness.WrapProvider("open the repository", err)
	want := branchChangedRead(rr)
	if reason, _, refused := refusal(err); refused {
		want = refusedRead(rr, reason, err, rr.Status.PullRequest.Number)
	}
	if werr := r.setUnreadable(ctx, rr, want); werr != nil {
		err = errors.Join(err, werr)
	}
	return fmt.Errorf("%w: %w", errBranchChangedUnread, err)
}

// branchChangedRead is condition PullRequestUnreadable for a pull request that could not be read on the recorded
// target branch, or on the one the repository entry of the plan names after a change (watchBranchChanged). The message is fixed and
// holds no text of the Git host.
func branchChangedRead(rr *remediationv1alpha1.RemediationRequest) *metav1.Condition {
	return &metav1.Condition{
		Type:   remediationv1alpha1.ConditionPullRequestUnreadable,
		Status: metav1.ConditionTrue,
		Reason: reasonTargetBranchChanged,
		Message: "The repository of the plan names another target branch in RemediationSettings now, and the " +
			"request's pull request could not be read on the branch the plan was made for or on the one the " +
			"settings name now, so it is not known whether it was merged; it is read again, and nothing is " +
			"decided on it until a read works",
		ObservedGeneration: rr.Generation,
	}
}

// openRecordedBranch opens repo, the plan's repository entry, for branch: the recorded target branch instead of the
// one the entry names now, or the one it names (mergedRetargeted). Every write of the provider returns errTargetBranchChanged before the kill switch is asked:
// it only reads.
func (r *RemediationRequestReconciler) openRecordedBranch(ctx context.Context, repo remediationv1alpha1.GitRepository,
	branch remediationv1alpha1.GitBranch) (providers.GitProvider, error) {
	if r.OpenRepository == nil {
		return nil, errors.New("no Git provider")
	}
	repo.TargetBranch = branch
	git, err := r.OpenRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	return &gatedGit{git: git, allow: func(context.Context) error { return errTargetBranchChanged }}, nil
}

// lookUnsettled reports whether the look at the pull request left something that nothing else may run on, and
// what recheck returns then: a merge seen and not recorded (mergeNotRecorded), or a pull request not read after
// its target branch changed (prBranchUnread), which is read again and never ended on the failed read.
func (r *RemediationRequestReconciler) lookUnsettled(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	outcome prOutcome, lookErr error) (ctrl.Result, bool, error) {
	if res, merged, err := r.mergeNotRecorded(rr, outcome, lookErr); merged {
		return res, true, err
	}
	if outcome == prBranchUnread {
		// Writes allowed again end WritesPaused here too: this path returns before the freshness check, which
		// removes it otherwise, and the reads may keep failing long after the pause.
		if err := r.resumeIfAllowed(ctx, rr); err != nil {
			return ctrl.Result{}, true, errors.Join(lookErr, err)
		}
		// The delay the host asked for never passes the full recheck's period, as on the look at an open one.
		if d := retryAfterOf(lookErr); d > freshRecheckInterval {
			lookErr = errors.Join(&freshness.ProviderError{Op: "wait for the delay the Git host asked for, capped",
				RetryAfter: freshRecheckInterval}, lookErr)
		}
		res, err := r.waitOrFail(ctx, rr, lookErr)
		return res, true, err
	}
	return ctrl.Result{}, false, nil
}
