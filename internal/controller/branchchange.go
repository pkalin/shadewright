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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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
//     that branch (recordRetargetedMerge) when the merge brought that branch the planned change and nothing else
//     (checkRetargeted), and ends the request NeedsIntervention with MergedWithForeignChange, without a receipt,
//     when it brought more, or with MergeContentUnreadable when what it brought could not be read. A merge into that branch is the only one recorded off the recorded branch: any other
//     base is not read as a merge;
//  2. on the recorded target branch (status.source): merged with the head Shadewright pushed, the merge is recorded
//     as any other (recordMergeSeen), with its receipt.
//
// Either way the check at the merge finds the plan stale (SettingsChanged), so the request ends Inconclusive with
// StalePlanMerged, neutral for the circuit breaker, like any merged stale plan. Otherwise:
//   - a read that fails, or a provider that fails to open: retried (errBranchChangedUnread), never an end, since
//     the pull request may be merged. Condition PullRequestUnreadable says so on every such failure (reason
//     TargetBranchChanged, or the refusal's reason when the Git host refuses for good), and the next read that
//     works removes it; on the configured branch only a merge that counts (retargetedMergeCounts) does. As on the look at any recorded pull request, nothing bounds the retries in time: only the
//     shared Git-read retry helper paces them at the poll interval or the host's longer delay;
//   - with spec.cancel set, open on a third base where a person moved it (changeElsewhere, #892): errOpenElsewhere,
//     and the cancel ends Cancelled saying so; closed or gone: errClosed, and the cancel ends Cancelled;
//   - anything else (open, closed, gone, open on a third base, another head, a merge into a third branch):
//     errTargetBranchChanged, handled as before: the request stops NeedsIntervention with TargetBranchChanged.
//
// The merge is recorded whenever it happens (decision of 2026-11-16): a request stopped that way keeps the target
// lock while its pull request is open on either branch, and the look that settles the lock (checkHeld, every
// heldRecheckInterval) reads the pull request by its number on the configured branch (lookHeld), then on
// the recorded one (lookHeld). A merge that counts there is recorded as above, and the request leaves
// NeedsIntervention for Merged (then Inconclusive with StalePlanMerged), or ends MergedWithForeignChange (or
// MergeContentUnreadable); a pull request closed without a merge releases the lock, and the request stays as it is.

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
	case cancelRequested(rr) && v.state == changeElsewhere && v.pr.BaseBranch == string(repo.TargetBranch):
		// Moved to the target branch the settings name now: open there, it keeps the cancel watch, as the cancel's
		// look decides (cancelLook).
		return errTargetBranchChanged
	case cancelRequested(rr) && v.state == changeElsewhere:
		// The request's own pull request, read by its number, is open on a third base, where a person moved it
		// (fourth decision of 2026-11-19 on #892): the cancel ends Cancelled saying so (prOpenElsewhere), and nothing
		// says it is closed or closes it.
		return fmt.Errorf("%w: %w", errOpenElsewhere, errTargetBranchChanged)
	case cancelRequested(rr) && (v.state == changeClosed || v.state == changeGone || v.state == changeMergedElsewhere):
		// A cancel settles like any other stop (fifth decision of 2026-11-18 on #771): the pull request, closed or
		// confirmed gone, cannot merge, so the cancel ends the request Cancelled (cancelOpen with prClosed).
		return fmt.Errorf("%w: %w", errClosed, errTargetBranchChanged)
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
		// Settings that can never open the provider for the configured branch: the look is left to the recorded
		// branch (watchBranchChanged, which says so when its open fails too), and a merge there is not looked up.
		logf.FromContext(ctx).Info("Could not open the repository of RemediationRequest for the target branch the "+
			"settings name now, so a merge of its pull request there was not looked up", "remediationRequest",
			client.ObjectKeyFromObject(rr), "targetBranch", repo.TargetBranch, "error", err.Error())
		return false, nil
	case err != nil:
		return true, r.branchChangedOpenFailed(ctx, rr, err)
	}
	v := r.changeOf(ctx, git, rr, changeQuery{configuredBranch: repo.TargetBranch})
	switch {
	case v.state == changeUnknown:
		return true, fmt.Errorf("%w: %w", errBranchChangedUnread, v.err)
	case retargetedMergeCounts(rr, v):
		return true, r.recordRetargetedMerge(ctx, git, rr, v.pr, repo.TargetBranch)
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
// branch the settings name now, after a person moved it there (#771), read through git, a provider opened for that
// branch. The merge counts only when what it brought to branch is the planned change and nothing else
// (checkRetargeted): the head was built on the recorded branch's history and may carry commits branch lacks.
//   - It brought more, or its shape does not show what it brought: the request ends NeedsIntervention with
//     MergedWithForeignChange, with no merge receipt, so nothing after the merge is judged on content the policy
//     never checked, and with the stale mark, so the circuit breaker counts no outcome (breakerOutcome) and no
//     revert is proposed (only Failed gets one).
//   - What it brought could not be read within mergeContentWait: the same end with reason MergeContentUnreadable,
//     which claims nothing about the content.
//   - The planned change alone: the plan was made for another branch, so it is stale whatever the check at the
//     merge finds: when that check finds nothing (the settings changed back meanwhile), the plan gets the stale mark
//     with reason SettingsChanged all the same, so the outcome is capped (StalePlanMerged). The receipt names branch.
//
// Both ends without a receipt mark the pull request merged. The common branch read then releases the lock
// only if no other pull request may still deliver the change; a person reviews what the merge brought.
//
// A request still waiting for review (watchStep) and one stopped with TargetBranchChanged (checkHeld) both come
// here. It returns errStopped once the write that ends the request went through, errMergeCommitPending while the
// merge commit is awaited, and any other failure wrapped in errMergeCheck with nothing recorded.
func (r *RemediationRequestReconciler) recordRetargetedMerge(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest, branch remediationv1alpha1.GitBranch) error {
	switch left, err := r.awaitMergeCommit(ctx, rr, cr); {
	case err != nil:
		return fmt.Errorf("%w: %w", errMergeCheck, err)
	case left > 0:
		return errMergeCommitPending
	}
	ref, err := r.checkRetargeted(ctx, git, rr, cr, branch)
	if err != nil {
		return fmt.Errorf("%w: %w", errMergeCheck, err)
	}
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest",
		cr.Number, "branch", branch)
	if ref != nil {
		markStaleForBranch(rr, branch)
		log.Info("The pull request of RemediationRequest was merged into the target branch the settings name now "+
			"without being shown to bring the planned change alone", "reason", ref.reason)
		return r.mergedForeignInto(ctx, rr, branch, ref)
	}
	if reason, cancelled := cancelledForIncident(rr); cancelled {
		markCancelledMerge(rr, reason)
	} else if err := r.checkAtMerge(ctx, rr, cr); err != nil {
		return err
	}
	markStaleForBranch(rr, branch)
	log.Info("The pull request of RemediationRequest was merged into the target branch the settings name now, " +
		"not the one the plan was made for")
	recordMergeInto(ctx, rr, cr, branch)
	err = r.stop(ctx, rr, remediationv1alpha1.PhaseMerged, ReasonChangeRequestMerged,
		"The pull request was merged into "+string(branch)+", the target branch the settings name now")
	if err != nil && !errors.Is(err, errStopped) {
		return fmt.Errorf("%w: %w", errMergeCheck, err)
	}
	return err
}

// markStaleForBranch gives the plan of a request whose pull request was merged into branch, not the target branch
// the plan was made for, the stale mark with reason SettingsChanged, unless PlanFresh already carries a stale
// reason (markedStale). It reads the condition, not the phase, so a request stopped in NeedsIntervention with
// TargetBranchChanged gets the mark too.
func markStaleForBranch(rr *remediationv1alpha1.RemediationRequest, branch remediationv1alpha1.GitBranch) {
	if !markedStale(rr) {
		markStaleAtMerge(rr, freshness.ReasonSettingsChanged, "The plan was stale when the merge was seen: the "+
			"pull request was merged into "+branchName(branch)+", not into the target branch the plan was made for")
	}
}

// branchName is branch for a message, or words that say it is not known when it is empty (a wait recorded before
// its entry named the branch, with the repository gone from the settings): never a placeholder read as a name.
func branchName(branch remediationv1alpha1.GitBranch) string {
	if branch == "" {
		return "a branch the status does not name"
	}
	return string(branch)
}

// markedStale reports whether PlanFresh is False with a stale reason (staleReasons), whatever the phase.
func markedStale(rr *remediationv1alpha1.RemediationRequest) bool {
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
	return c != nil && c.Status == metav1.ConditionFalse && (staleReasons[c.Reason] || incidentReasons[c.Reason])
}

// mergedForeignInto ends a request whose pull request was merged into branch, the target branch the settings name
// now, without being shown to bring the planned change alone (ref, checkRetargeted): NeedsIntervention with
// ref.reason (uncountedMergeReasons), the pull request marked merged, and no merge
// receipt. The common branch read settles the target lock. The caller has set the stale mark; the breaker's watermark is set again
// from it, as a request already in NeedsIntervention keeps its phase (setPhase does not run it then).
func (r *RemediationRequestReconciler) mergedForeignInto(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	branch remediationv1alpha1.GitBranch, ref *renderRefusal) error {
	where := branchName(branch)
	if branch != "" {
		where += ", the target branch the settings name now"
	}
	message := fmt.Sprintf("Pull request #%d was merged into %s, and the "+
		"merge did not bring the planned change alone: %s. Nothing after the merge is checked; review what the merge "+
		"brought", recordedNumber(rr), where, ref.message)
	switch ref.reason {
	case ReasonMergeContentUnreadable:
		message = fmt.Sprintf("Pull request #%d was merged into %s, and what "+
			"the merge brought could not be read: %s. Nothing after the merge is checked; review what the merge brought",
			recordedNumber(rr), where, ref.message)
	case ReasonMergeNotCheckable:
		message = fmt.Sprintf("Pull request #%d was merged into %s, and %s, so it is not known whether the merge "+
			"brought the planned change alone. Nothing after the merge is checked; review what the merge brought",
			recordedNumber(rr), where, ref.message)
	}
	markClosed(rr, ReasonChangeRequestMerged, "was merged into "+branchName(branch)+
		" without being shown to bring the planned change alone")
	// stop sets the same condition; set first, so the breaker's watermark below and in setPhase reads it.
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type: remediationv1alpha1.ConditionApproved, Status: metav1.ConditionFalse,
		Reason: ref.reason, Message: message, ObservedGeneration: rr.Generation,
	})
	if rr.Status.Phase == remediationv1alpha1.PhaseNeedsIntervention {
		classifyBreaker(rr)
	}
	err := r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ref.reason, message)
	if err != nil && !errors.Is(err, errStopped) {
		return fmt.Errorf("%w: %w", errMergeCheck, err)
	}
	return err
}

// mergedForeignStale reports whether rr ended NeedsIntervention with MergedWithForeignChange,
// MergeContentUnreadable or MergeNotCheckable (uncountedMergeReasons) while its plan carried the stale mark: a pull request merged into the target branch the
// settings name now that was not shown to bring the planned change alone (#771). Neither the change nor its failure
// is Shadewright's to count, as for any merged stale plan.
func mergedForeignStale(rr *remediationv1alpha1.RemediationRequest) bool {
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionApproved)
	return rr.Status.Phase == remediationv1alpha1.PhaseNeedsIntervention && markedStale(rr) && c != nil &&
		uncountedMergeReasons[c.Reason]
}

// mergeContentWait is how long the reads of what a merge into the target branch the settings name now brought are
// retried, from the start of its entry in status.gitWaits, before the request ends MergeContentUnreadable (waitMergeContent).
const mergeContentWait = mergeCommitWait

// checkRetargeted reports whether the merge of cr into branch (git is opened for it) brought that branch the
// planned change and nothing else (#771, retargetedContent). The answer is nil when the merge counts, a
// *renderRefusal with reason MergedWithForeignChange otherwise (the merge brought more, or its shape does not show
// what it brought). A merge commit that is not a full commit ID cannot be checked (MergedWithForeignChange).
//
// A read that fails is an error of the content wait about cr (readOfContent), for a retry: retryGitRead records its
// entry in status.gitWaits, with cr, with the parent context, so a read that runs out of the step's time starts it
// too (keepGitRead). Past its end the reconcile makes one more read, and the wait ends after it on evidence about that
// read (endAfterLastRead, not endPassedWait, which ends only the deadline and the handoff grace): MergeContentUnreadable,
// recording cr when the status holds no pull request. A successful content read removes the
// entry, so a later failure starts a new wait.
func (r *RemediationRequestReconciler) checkRetargeted(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest, branch remediationv1alpha1.GitBranch) (
	*renderRefusal, error) {
	if !shaPattern.MatchString(cr.MergeCommit) {
		return &renderRefusal{reason: ReasonMergedWithForeignChange, message: "the Git host reported no full merge " +
			"commit ID, so what the merge brought cannot be checked"}, nil
	}
	ref, err := retargetedContent(ctx, git, rr, cr, branch)
	if err != nil {
		logf.FromContext(ctx).Info("Could not read what the merge of the pull request of RemediationRequest brought; "+
			"tried again", "remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error())
		return nil, readOfContent(cr, err)
	}
	r.gitRetries().noteAnswered(client.ObjectKeyFromObject(rr))
	return ref, r.clearContentWait(ctx, rr)
}

// clearContentWait removes the content wait (status.gitWaits) after a read of what the merge brought worked, and no
// other entry: the read clears only the wait it times.
func (r *RemediationRequestReconciler) clearContentWait(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	if gitWait(rr, waitMergeContent) == nil {
		return nil
	}
	if err := r.dropGitWaits(ctx, rr, func(w remediationv1alpha1.GitWait) bool {
		return w.Kind == remediationv1alpha1.GitWaitMergeContent
	}); err != nil {
		return &clusterError{err: fmt.Errorf("remove the content wait: %w", err)}
	}
	return nil
}

// retargetedContent is checkRetargeted's reads. It compares what the merge brought as a whole, whatever the merge
// method: before, the head of branch right before the merge, against the merge result, cr.MergeCommit. Their
// trees must differ at most in the plan's target file (CheckBetween), the file at before must hold the plan's
// previous value, and the file at the merge result must be byte for byte the planned change of it. before is found
// from the shape of the merge:
//   - a merge commit (two parents): its second parent must be the head Shadewright pushed, and before is its first
//     parent. One Parents call.
//   - one parent, a squash or a rebase merge: the merge added n commits on top of before, one for a squash and one
//     per replayed commit for a rebase, and the head carries those n over before. n is taken as the number of
//     commits the head's history holds that the merge result's does not (CountAhead): a rebase replays exactly
//     those, a squash adds one of them. before is found by walking n first parents from the merge result, and
//     each commit the walk passes below the merge result must be a replay of the head's commit at the same depth:
//     one parent, a change of the target file alone (CheckCommit), and the same target file as that commit. A
//     squash merge with n above one walks into its real parent, a commit the branch held before the merge: when
//     that commit is no such replay the walk refuses it there; when it passes as one, the comparison starts below
//     it and so covers the change it made, so a change the branch made and the merge undid is never hidden either
//     way. n must be from one to MaxChainCommits, the most commits a Shadewright
//     branch carries. Parents, CountAhead, then per further commit CheckCommit, Parents and two ReadFile calls.
//
// A merge result equal to the head (the head taken in as it is) shows no before, and every other shape is refused
// too. The comparison then reads two trees per directory of the path and the file at both commits: about eight to
// sixteen calls of the GitHub API, once per merge. An error is a failed read, never an answer; every unknown shape
// is a refusal, so nothing unknown counts the merge.
func retargetedContent(ctx context.Context, git providers.GitProvider, rr *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest, branch remediationv1alpha1.GitBranch) (*renderRefusal, error) {
	foreign := func(message string) (*renderRefusal, error) {
		return &renderRefusal{reason: ReasonMergedWithForeignChange, message: message}, nil
	}
	path := string(rr.Status.Source.Path)
	if cr.MergeCommit == cr.HeadSHA {
		return foreign("the head was taken into " + string(branch) + " as it is, so what the branch held before " +
			"the merge is not known")
	}
	parents, err := git.Parents(ctx, cr.MergeCommit)
	if err != nil {
		return nil, freshness.WrapProvider("read the parents of the merge commit", err)
	}
	var before string
	switch {
	case len(parents) == 2 && parents[1] == cr.HeadSHA:
		before = parents[0]
	case len(parents) == 2:
		return foreign("the merge commit does not take in the head Shadewright pushed")
	case len(parents) == 1:
		added, err := git.CountAhead(ctx, cr.MergeCommit, cr.HeadSHA)
		switch {
		case err != nil:
			return nil, freshness.WrapProvider("count the commits the pull request carried over "+string(branch), err)
		case added < 1 || added > providers.MaxChainCommits:
			return foreign(fmt.Sprintf("the pull request carried %d commits over %s, not one to %d", added, branch,
				providers.MaxChainCommits))
		}
		before = parents[0]
		replayOf := cr.HeadSHA
		for range added - 1 {
			// before is a commit below the merge result. It counts as one the merge added only as a replay of
			// the head's commit at the same depth: one parent, a change of the target file alone, leaving the
			// file as that commit does. A commit the branch held before a squash merge that is no such replay
			// stops the walk at the merge's real parent with a refusal; one that passes as a replay is walked
			// past, and the comparison, which then starts below it, covers its change.
			state, up, err := git.CheckCommit(ctx, before, path)
			switch {
			case err != nil:
				return nil, freshness.WrapProvider("check a commit below the merge result", err)
			case state != providers.BranchOneChange:
				return foreign("a commit below the merge result on " + string(branch) + " is not a replay of a " +
					"commit of the pull request that changes the target file alone, so the merge brought more " +
					"than the head's commits or what the branch held before it is not known")
			}
			heads, err := git.Parents(ctx, replayOf)
			switch {
			case err != nil:
				return nil, freshness.WrapProvider("read the parents of a commit of the head", err)
			case len(heads) != 1:
				return foreign("a commit of the head Shadewright pushed does not have one parent")
			}
			replayOf = heads[0]
			if same, err := sameFile(ctx, git, before, replayOf, path); err != nil {
				return nil, err
			} else if !same {
				return foreign("a commit below the merge result on " + string(branch) + " does not hold the " +
					"target file as the head's commit it would replay, so it is not a commit the merge added")
			}
			before = up
		}
	default:
		return foreign("the merge commit has an unexpected number of parents")
	}
	state, err := git.CheckBetween(ctx, before, cr.MergeCommit, path)
	switch {
	case err != nil:
		return nil, freshness.WrapProvider("check what the merge brought to "+string(branch), err)
	case state != providers.BranchOneChange:
		return foreign("the merge changed more of " + string(branch) + " than the target file")
	}
	to, err := git.ReadFile(ctx, cr.MergeCommit, path, providers.MaxFileBytes)
	if err != nil {
		return nil, freshness.WrapProvider("read the target file at the merge commit", err)
	}
	from, err := git.ReadFile(ctx, before, path, providers.MaxFileBytes)
	if err != nil {
		return nil, freshness.WrapProvider("read the target file before the merge", err)
	}
	want, held, err := planned(from.Data, rr)
	switch {
	case err != nil:
		return notCheckable("the target file "+string(branch)+" held before the merge", err), nil
	case held != fileHoldsPrevious || !bytes.Equal(want, to.Data):
		return foreign("the target file the merge left on " + string(branch) + " is not exactly the planned change " +
			"of the file it held before")
	}
	return nil, nil
}

// sameFile reports whether the file at path is the same at commits a and b. A failed read is an error.
func sameFile(ctx context.Context, git providers.GitProvider, a, b, path string) (bool, error) {
	fa, err := git.ReadFile(ctx, a, path, providers.MaxFileBytes)
	if err != nil {
		return false, freshness.WrapProvider("read the target file at a commit the merge added", err)
	}
	fb, err := git.ReadFile(ctx, b, path, providers.MaxFileBytes)
	if err != nil {
		return false, freshness.WrapProvider("read the target file at a commit of the head", err)
	}
	return bytes.Equal(fa.Data, fb.Data), nil
}

// branchChangedOpenFailed handles a provider for a read after the target branch changed that failed to open, other
// than for good: condition PullRequestUnreadable (reason TargetBranchChanged, or the refusal's), and
// errBranchChangedUnread for a retry.
func (r *RemediationRequestReconciler) branchChangedOpenFailed(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, err error) error {
	err = freshness.WrapProvider("open the repository", err)
	if werr := r.setUnreadable(ctx, rr, branchChangedOpenRead(rr, err)); werr != nil {
		err = &clusterError{err: werr}
	}
	return fmt.Errorf("%w: %w", errBranchChangedUnread, err)
}

// branchChangedOpenRead is condition PullRequestUnreadable for err, a failure to open a provider for a read after
// the target branch changed: the refusal's reason when the Git host refuses for good, TargetBranchChanged
// (branchChangedRead) otherwise.
func branchChangedOpenRead(rr *remediationv1alpha1.RemediationRequest, err error) *metav1.Condition {
	if reason, _, refused := refusal(err); refused {
		// No pull request may be recorded yet (its status write was lost): the message then names none.
		return refusedRead(rr, reason, err, recordedNumber(rr))
	}
	return branchChangedRead(rr)
}

// recordedNumber is the number of the recorded pull request, zero when none is recorded.
func recordedNumber(rr *remediationv1alpha1.RemediationRequest) int64 {
	if rr.Status.PullRequest == nil {
		return 0
	}
	return rr.Status.PullRequest.Number
}

// stoppedForBranchChange reports whether rr stopped in NeedsIntervention because the target branch of its plan's
// repository changed, with its source recorded. Its pull request may be unrecorded, when the status write that
// would record it was lost: the read that settles the lock finds it by the request's marker and records it before
// its merge (heldMergeFollows). Every stop with reason TargetBranchChanged
// counts: the freshness check's (endFreshness, PlanFresh False with that reason) and a step's (retry, stop: Approved
// False with that reason once a pull request is recorded, as after the recovery of a handoff, retryRecovery). Such a
// request keeps the target lock while its pull request is open on the recorded or on the configured branch, and the
// read that settles the lock records a merge that counts (#771): settleTargetLock and checkHeld.
func stoppedForBranchChange(rr *remediationv1alpha1.RemediationRequest) bool {
	if rr.Status.Phase != remediationv1alpha1.PhaseNeedsIntervention || rr.Status.Source == nil {
		return false
	}
	// PullRequestOpened holds the reason while no pull request is recorded (stop): a pull request opened while the
	// status write that would record it was lost (#827).
	for _, t := range []string{remediationv1alpha1.ConditionPlanFresh, remediationv1alpha1.ConditionApproved,
		remediationv1alpha1.ConditionPROpened} {
		c := meta.FindStatusCondition(rr.Status.Conditions, t)
		if c != nil && c.Status == metav1.ConditionFalse && c.Reason == reasonTargetBranchChanged {
			return true
		}
	}
	return false
}

// recordHeldMerge records the merge of the recorded pull request, cr, seen merged with the head Shadewright pushed
// into the recorded target branch, for a request stoppedForBranchChange (#771, the merge #754 records while the
// request waits): as recordMergeSeen, and with the stale mark with reason SettingsChanged when the check at the
// merge left none (the settings name the recorded branch again), since the request stopped on a plan made stale by
// the change. A pull request adopted by the read that settles the lock (adopted: none was recorded, heldMergeFollows)
// has no recorded head to compare, so its head is checked as endMerged checks one (checkMerged): one that is not the
// planned change alone ends NeedsIntervention with MergedWithForeignChange and no receipt; a failed read of that
// check is one of the merge wait about cr (checkMerged), which ends it the same way at the wait's end (#889). It
// returns what recordMergeSeen returns.
func (r *RemediationRequestReconciler) recordHeldMerge(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest, adopted bool) error {
	if pr := rr.Status.PullRequest; !adopted && pr != nil && cr.HeadSHA != pr.HeadSHA {
		// Merged with a head Shadewright did not push: recorded as such, with no receipt.
		markStaleForBranchChange(rr)
		err := r.mergedForeign(ctx, rr, &renderRefusal{reason: ReasonHeadMoved,
			message: "The merged head is not the head Shadewright pushed"})
		if err != nil && !errors.Is(err, errStopped) {
			return fmt.Errorf("%w: %w", errMergeCheck, err)
		}
		return err
	}
	switch left, err := r.awaitMergeCommit(ctx, rr, cr); {
	case err != nil:
		return fmt.Errorf("%w: %w", errMergeCheck, err)
	case left > 0:
		return errMergeCommitPending
	}
	if adopted {
		repo, _, err := r.planEntry(ctx, rr)
		if err != nil && !errors.Is(err, errTargetBranchChanged) {
			return fmt.Errorf("%w: %w", errMergeCheck, err)
		}
		git, err := r.openRecordedBranch(ctx, repo, rr.Status.Source.TargetBranch)
		if err != nil {
			return fmt.Errorf("%w: %w", errMergeCheck, freshness.WrapProvider("open the repository", err))
		}
		ref, err := r.checkMerged(ctx, git, rr, cr)
		if err != nil {
			return fmt.Errorf("%w: %w", errMergeCheck, err)
		}
		// Checked: the pull request is the request's own, and is recorded in the write that ends it.
		recordFound(rr, cr)
		if ref != nil {
			if !markedStale(rr) {
				markStaleAtMerge(rr, freshness.ReasonSettingsChanged, "The plan was stale when the merge was seen: "+
					"the target branch of its repository changed in the settings after the pull request was opened")
			}
			err := r.mergedForeign(ctx, rr, ref)
			if err != nil && !errors.Is(err, errStopped) {
				return fmt.Errorf("%w: %w", errMergeCheck, err)
			}
			return err
		}
	}
	if err := r.checkAtMerge(ctx, rr, cr); err != nil {
		return err
	}
	if !markedStale(rr) {
		markStaleAtMerge(rr, freshness.ReasonSettingsChanged, "The plan was stale when the merge was seen: the "+
			"target branch of its repository changed in the settings after the pull request was opened")
	}
	recordMerge(ctx, rr, cr)
	err := r.stop(ctx, rr, remediationv1alpha1.PhaseMerged, ReasonChangeRequestMerged, "The pull request was merged")
	if err != nil && !errors.Is(err, errStopped) {
		return fmt.Errorf("%w: %w", errMergeCheck, err)
	}
	return err
}

// markStaleForBranchChange gives the plan of a request stoppedForBranchChange the stale mark with reason
// SettingsChanged at its merge, unless it carries one.
func markStaleForBranchChange(rr *remediationv1alpha1.RemediationRequest) {
	if !markedStale(rr) {
		markStaleAtMerge(rr, freshness.ReasonSettingsChanged, "The plan was stale when the merge was seen: the "+
			"target branch of its repository changed in the settings after the pull request was opened")
	}
}

// followHeldMerge runs record, the record of a merge that counts of a request stoppedForBranchChange, within the
// cleanup's deadline, as checkHeld's answer: the end written (errStopped) is done; a merge commit not reported yet
// is looked at again when what is left of its wait is over; failed Git reads use retryGitRead; a failed Kubernetes read or write remains an error. No failed read
// records the merge. The lock is kept meanwhile. The waits for the merge commit and for the check at the merge are
// bounded by mergeCommitWait from the start of their MergeCheck entry in status.gitWaits, the reads of what a
// retargeted merge brought by mergeContentWait from the start of their MergeContent entry (waitKind.end). Past that
// end, one more read is made at the first reconcile not under a pause, and the wait ends only on evidence about it
// (endAfterLastRead): it failed, it could not start, or it answered and settled nothing; otherwise it is kept. A read
// of the pull request itself that keeps failing shows in condition PullRequestUnreadable.
func (r *RemediationRequestReconciler) followHeldMerge(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	record func(context.Context) error) (reconcile.Result, error) {
	cleanCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	err := record(cleanCtx)
	// A read of the content wait or of the check at the merge that ran out of the cleanup's time is still that read:
	// retryGitRead starts its clock with ctx and the check at the start of a reconcile ends it (finding of #827).
	err = keepGitRead(ctx, cleanCtx, "record of the merge", err)
	cancel()
	switch {
	case errors.Is(err, errStopped):
		res, err := r.checkHeld(ctx, rr)
		return res, err
	case ctx.Err() != nil:
		return reconcile.Result{}, err
	case errors.Is(err, errMergeCommitPending):
		return reconcile.Result{RequeueAfter: r.mergeCommitWaitLeft(rr)}, nil
	}
	logf.FromContext(ctx).Info("Could not record the merge of the pull request of a stopped RemediationRequest "+
		"yet; tried again", "remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest",
		recordedNumber(rr), "error", err.Error())
	res, err := r.waitOrFail(ctx, rr, err, unboundedRead(noBoundHeldLock))
	return res, err
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
	outcome prOutcome, lookErr error, bound waitBound) (ctrl.Result, bool, error) {
	if res, merged, err := r.mergeNotRecorded(ctx, rr, outcome, lookErr, bound); merged {
		return res, true, err
	}
	if outcome == prBranchUnread {
		// Writes allowed again end WritesPaused here too: this path returns before the freshness check, which
		// removes it otherwise, and the reads may keep failing long after the pause.
		if err := r.resumeIfAllowed(ctx, rr); err != nil {
			return ctrl.Result{}, true, &clusterError{err: err}
		}
		// The delay the host asked for never passes the full recheck's period (bound), as on the look at an open one.
		if d := retryAfterOf(lookErr); d > freshRecheckInterval {
			lookErr = errors.Join(&freshness.ProviderError{Op: "wait for the delay the Git host asked for, capped",
				RetryAfter: freshRecheckInterval}, lookErr)
		}
		res, err := r.waitOrFail(ctx, rr, lookErr, bound)
		return res, true, err
	}

	return ctrl.Result{}, false, nil
}
