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
// before the executor looks. The look at the pull request (watchStep) therefore reads it once more first, by its
// recorded number, in the recorded repository and on the recorded target branch (status.planRepository and
// status.source, invariant 5), through a provider that writes nothing:
//
//   - merged with the head Shadewright pushed: the merge is recorded as any other (recordMergeSeen), with its
//     receipt; the check at the merge finds the plan stale (SettingsChanged), so the request ends Inconclusive
//     with StalePlanMerged, neutral for the circuit breaker, like any merged stale plan;
//   - a read that fails, or a provider that fails to open: retried (errBranchChangedUnread), never an end, since
//     the pull request may be merged. Condition PullRequestUnreadable says so on every such failure (reason
//     TargetBranchChanged, or the refusal's reason when the Git host refuses for good), and the next read that
//     works removes it. As on the look at any recorded pull request, nothing bounds the retries in time: only the
//     error backoff, or the delay the host asked for, capped at freshRecheckInterval, paces them;
//   - anything else (open, closed, gone, another head): errTargetBranchChanged, handled as before.

// errBranchChangedUnread marks a look at the pull request of a plan whose target branch changed that could not
// read the pull request: recheck retries it (prBranchUnread) and does not go on to the freshness check, which
// would end the request in NeedsIntervention without knowing whether it was merged.
var errBranchChangedUnread = errors.New("the target branch changed and the pull request could not be read")

// watchBranchChanged is watchStep for a plan whose repository entry, repo, now names another target branch than
// the recorded one (see above). It returns what recordMergeSeen returns for a merge with the recorded head,
// errBranchChangedUnread wrapping a failed read, and errTargetBranchChanged for every other answer.
func (r *RemediationRequestReconciler) watchBranchChanged(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, repo remediationv1alpha1.GitRepository) error {
	pr, src := rr.Status.PullRequest, rr.Status.Source
	if pr == nil || src == nil {
		return errTargetBranchChanged
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
		err = freshness.WrapProvider("open the repository", err)
		want := branchChangedRead(rr)
		if reason, _, refused := refusal(err); refused {
			want = refusedRead(rr, reason, err, pr.Number)
		}
		if werr := r.setUnreadable(ctx, rr, want); werr != nil {
			err = errors.Join(err, werr)
		}
		return fmt.Errorf("%w: %w", errBranchChangedUnread, err)
	}
	// By its number, on the recorded target branch (changeOf reads status.source); a failed read sets condition
	// PullRequestUnreadable (noteRead, recordedBranch).
	v := r.changeOf(ctx, git, rr, changeQuery{recordedBranch: true})
	switch {
	case v.state == changeUnknown:
		return fmt.Errorf("%w: %w", errBranchChangedUnread, v.err)
	case v.pr.State == providers.ChangeRequestMerged && v.pr.Number == pr.Number && v.pr.HeadSHA == pr.HeadSHA:
		return r.recordMergeSeen(ctx, rr, v.pr)
	}
	return errTargetBranchChanged
}

// branchChangedRead is condition PullRequestUnreadable for a pull request that could not be read on the recorded
// target branch after the repository entry of the plan changed it (watchBranchChanged). The message is fixed and
// holds no text of the Git host.
func branchChangedRead(rr *remediationv1alpha1.RemediationRequest) *metav1.Condition {
	return &metav1.Condition{
		Type:   remediationv1alpha1.ConditionPullRequestUnreadable,
		Status: metav1.ConditionTrue,
		Reason: reasonTargetBranchChanged,
		Message: "The repository of the plan names another target branch in RemediationSettings now, and the " +
			"request's pull request could not be read on the branch the plan was made for, so it is not known " +
			"whether it was merged; it is read again, and nothing is decided on it until a read works",
		ObservedGeneration: rr.Generation,
	}
}

// openRecordedBranch opens repo, the plan's repository entry, for the recorded target branch instead of the one
// the entry names now. Every write of the provider returns errTargetBranchChanged before the kill switch is asked:
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
