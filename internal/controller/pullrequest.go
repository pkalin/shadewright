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
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/patch"
	"github.com/pkalin/shadewright/internal/gitops/semdiff"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
)

// The pull request step (design sections 6 and 8.2, steps 2 to 7): an allowed request becomes one branch
// shadewright/<request-uid>, one commit that changes the one value in the one file, and one pull request whose
// body carries a marker with the request UID and the plan digest.
//
// Each reconcile rebuilds the external state from Git (the pull request for the branch, the branch, the file
// on it), checks the plan's freshness, and makes at most one write. A restart or a second reconcile at any
// point finds the work already done instead of repeating it: the branch name and the marker come from the
// request, the branch is created only at the target branch's head, the commit names the head it expects, and
// opening a pull request for a branch that has one returns it. The policy check stores status.deadline with an
// allowed decision, so the deadline also ends a request whose freshness check never passes; a request decided
// while writes were paused gets it from recheck (startDeadline) once the pause has ended.
//
// The finalizer that closes the pull request on deletion is added before the first Git write, so no branch or
// pull request exists without it.
//
// +kubebuilder:rbac:groups=remediation.shadewright.io,namespace=system,resources=remediationrequests,verbs=patch

// FinalizerName closes the request's open pull request when the request is deleted (design section 6). It is
// the only finalizer: deleting a request never touches the cluster.
const FinalizerName = "remediation.shadewright.io/close-pull-request"

// DefaultPullRequestTimeout bounds the time from PolicyChecked to an open pull request. It is stored in
// status.deadline when the step starts; a request that has not opened its pull request by then is Expired.
const DefaultPullRequestTimeout = 30 * time.Minute

// DefaultWriteTimeout bounds one Git step: opening the repository, rebuilding the state and one write.
const DefaultWriteTimeout = 2 * time.Minute

// DefaultIncidentReadTimeout bounds the read of the Incident whose name the pull request body shows. One get
// of a small object takes milliseconds; a read that fails only redacts the name, so a short deadline loses
// nothing and leaves the rest of the step's deadline to the Git host.
const DefaultIncidentReadTimeout = 5 * time.Second

// stepDelay is how soon the next step runs after a single write of the step.
const stepDelay = time.Second

// Reasons of the pull request step.
const (
	// ReasonPullRequestOpened: the pull request is open and its head is the commit Shadewright made.
	ReasonPullRequestOpened = "PullRequestOpened"
	// ReasonPullRequestClosed: Shadewright closed the recorded pull request without a merge (or found it closed
	// while closing it), because the plan is stale or the change no longer renders as planned.
	// PullRequestOpened stays True (the pull request was opened) and its message names the number and why.
	ReasonPullRequestClosed = "PullRequestClosed"
	// ReasonWaitingForReview: the pull request waits for a human review.
	ReasonWaitingForReview = "WaitingForReview"
	// ReasonHeadMoved: someone else pushed to the branch. Automation stops; the plan no longer describes the
	// pull request.
	ReasonHeadMoved = "HeadMoved"
	// ReasonBranchDiverged: the branch holds neither the previous nor the planned value of the file, or holds
	// more than Shadewright's one commit on top of the target branch: before the commit, commits that are not
	// on the target branch; before the pull request is opened or recorded, or while it waits, anything beside
	// the one commit that changes the target file, or a file that is not byte for byte the planned change. A
	// pull request open at that point carries the marker and is closed.
	ReasonBranchDiverged = "BranchDiverged"
	// ReasonForeignChangeRequest: a pull request for the branch exists without this request's marker.
	ReasonForeignChangeRequest = "ForeignChangeRequest"
	// ReasonChangeRequestClosed: the pull request was closed without a merge, not by a close the request recorded,
	// before or after the request recorded the pull request.
	ReasonChangeRequestClosed = "ChangeRequestClosed"
	// ReasonChangeRequestConflict: the Git host reports that the open pull request cannot be merged into the
	// target branch because of a merge conflict, and the plan is still fresh. The pull request is left open for
	// a person to resolve or close; it is not a rejection (ChangeRequestClosed).
	ReasonChangeRequestConflict = "ChangeRequestConflict"
	// ReasonChangeRequestMerged: the pull request was merged.
	ReasonChangeRequestMerged = "ChangeRequestMerged"
	// ReasonMergedWithForeignChange: Shadewright's pull request failed a check before it was recorded (#524),
	// and a person merged it before Shadewright could close it. The merged change holds something no check saw.
	ReasonMergedWithForeignChange = "MergedWithForeignChange"
	// ReasonMergeContentUnreadable: the pull request was merged into the target branch the settings name now after
	// the plan's branch changed (#771), and what the merge brought could not be read within mergeContentWait. It
	// says nothing about that content: it is not known to be the planned change alone.
	ReasonMergeContentUnreadable = "MergeContentUnreadable"
	// ReasonMergeNotCheckable: a merge of the request's own pull request was found, but the target file it was
	// compared against (before the merge, or at the parent of an adopted head) is one the typed patcher refuses (it
	// does not parse, the object is not in it), so the merge cannot be checked against the plan. It says nothing
	// about the content: it is neither shown foreign nor the planned change alone, and is treated as
	// MergedWithForeignChange is (no merge receipt, no verification, no circuit breaker outcome).
	ReasonMergeNotCheckable = "MergeNotCheckable"
	// ReasonPatchRefused: the typed patcher refused the file on the branch, or the target file at the head of
	// the target branch while the pull request waits.
	ReasonPatchRefused = "PatchRefused"
	// ReasonPullRequestDeadline: the pull request was not opened before status.deadline.
	ReasonPullRequestDeadline = "PullRequestDeadline"
	// ReasonPullRequestDeadlineCleanupRefused: the pull request was not opened before status.deadline, and the
	// Git host refused to find or close one Shadewright may have opened on the branch (its status write lost).
	// The request keeps the lock on its target until a look-up finds none of its own open (targetlock.go).
	ReasonPullRequestDeadlineCleanupRefused = "PullRequestDeadlineCleanupRefused"
	// ReasonPullRequestDeadlineCleanupFailed: the pull request was not opened before status.deadline, and the close
	// of one Shadewright opened on the branch kept failing (not a refusal) for 24 hours after the first failure
	// (condition CleanupFailing). The request keeps the lock on its target until a look-up finds none of its own open.
	ReasonPullRequestDeadlineCleanupFailed = "PullRequestDeadlineCleanupFailed"
	// ReasonChangeRequestGone: the recorded pull request cannot be found any more.
	ReasonChangeRequestGone = "ChangeRequestGone"
	// ReasonChangeRequestOnOtherBase: the request's own pull request is open on, or was merged into, another base
	// branch than the target branch, where a person moved it (#892): automation stopped and nothing more is written
	// to it. Open, the target stays locked while it is; merged, its change did not reach the target branch.
	ReasonChangeRequestOnOtherBase = "ChangeRequestOnOtherBase"
	// ReasonPlanIncomplete: the request's status.source has no buildPath, so the change cannot be rendered.
	ReasonPlanIncomplete = "PlanIncomplete"
	// ReasonTargetFileDiverged: the target file at the head of the target branch is not the file the change
	// was made from (before the commit), or with the planned change applied is not the file of the pull request
	// (while it waits). Merging would combine edits that no render saw, so automation stops.
	ReasonTargetFileDiverged = "TargetFileDiverged"
	// ReasonGitForbidden: the Git host refused the request for Shadewright's identity (a permission the App
	// lacks, or a rule of the repository). A person must change the permission or the rule; nothing is retried.
	ReasonGitForbidden = "GitForbidden"
	// ReasonGitRejected: the Git host refused the request as invalid and the state in Git does not explain the
	// refusal. Nothing is retried.
	ReasonGitRejected = "GitRejected"
	// ReasonGitNotFound (conditions PullRequestNotClosed and PullRequestUnreadable, #797): the Git host answered 404
	// to a step of a stale plan's cleanup through a new installation of the GitHub App (#290), such as an
	// installation ID with a typing mistake or one not granted the repository. It is tried again.
	ReasonGitNotFound = "GitNotFound"
	// ReasonGitFailed (conditions PullRequestNotClosed and PullRequestUnreadable, #797): a step of a stale plan's
	// cleanup through a new installation of the GitHub App (#290) failed without a refusal or a 404: a server error,
	// a timeout, a rate limit, an answer that settles nothing or an unclassified failure. It is tried again.
	ReasonGitFailed = "GitFailed"

	// ReasonRecordedMissingOtherListed (condition PullRequestUnreadable): the Git host has no pull request of this
	// request with the recorded number, but the request's branch lists another one with its marker, so the request
	// cannot tell whether its change is gone and is read again until a person acts (#688).
	ReasonRecordedMissingOtherListed = "RecordedMissingOtherListed"

	reasonTargetBranchChanged   = "TargetBranchChanged"
	reasonRepositoryUnavailable = "RepositoryUnavailable"
)

// errStopped means the step ended the request in a final phase; the caller returns without requeue.
var errStopped = errors.New("the pull request step stopped the request")

// errClosed means the recorded pull request is closed without a merge; see prClosed.
var errClosed = errors.New("the pull request is closed")

// errOpenElsewhere means the request's own pull request is open on a base a person moved it to (changeElsewhere,
// #892): on the watch of a cancelled request (watchStep, watchBranchChanged; see prOpenElsewhere), and, as an
// *openElsewhereError, from the close of a stop (closeOwn), which closes nothing there.
var errOpenElsewhere = errors.New("the pull request is open on another base branch")

// openElsewhereError is errOpenElsewhere for pull request pr, the request's own, read by its number open on another
// base (changeElsewhere): its number is trusted, its base is shown only through hostBranchText.
type openElsewhereError struct{ pr providers.ChangeRequest }

func (e *openElsewhereError) Error() string { return openElsewhereText(e.pr) }

func (e *openElsewhereError) Is(target error) bool { return target == errOpenElsewhere }

// openElsewhereText says that pull request cr of the request is open on another base, where a person moved it: its
// number, and its base through hostBranchText when the provider answered a name (#892).
func openElsewhereText(cr providers.ChangeRequest) string {
	where := "another base branch"
	if cr.BaseBranch != "" {
		where += " (" + hostBranchText(cr.BaseBranch) + ")"
	}
	return fmt.Sprintf("pull request #%d of the request is open on %s, where it was moved", cr.Number, where)
}

// mergedElsewhereText says that pull request cr of the request was merged into another base than the target branch,
// where a person moved it: its number, and its base through hostBranchText when the provider answered a name (#892).
func mergedElsewhereText(cr providers.ChangeRequest) string {
	where := "another base branch"
	if cr.BaseBranch != "" {
		where += " (" + hostBranchText(cr.BaseBranch) + ")"
	}
	return fmt.Sprintf("pull request #%d of the request was merged into %s, where it was moved, not into the "+
		"target branch", cr.Number, where)
}

// messageOpenElsewhere is the tail of every message that ends a stop on the request's own pull request open on
// another base (#892): nothing is written to it there, and the lock stays while it is open.
const messageOpenElsewhere = "; Shadewright writes nothing more to it there, and the target stays locked until it " +
	"is closed or merged (condition TargetHeld)"

// openElsewhereOf returns the pull request of err when err is or wraps an *openElsewhereError, the zero one otherwise.
func openElsewhereOf(err error) providers.ChangeRequest {
	if e, ok := errors.AsType[*openElsewhereError](err); ok {
		return e.pr
	}
	return providers.ChangeRequest{}
}

// errConflicted means the Git host reports the recorded pull request open with a merge conflict; see
// prConflicted.
var errConflicted = errors.New("the pull request has a merge conflict")

// pullRequestTimeout returns the configured deadline of the step.
func (r *RemediationRequestReconciler) pullRequestTimeout() time.Duration {
	if r.PullRequestTimeout > 0 {
		return r.PullRequestTimeout
	}
	return DefaultPullRequestTimeout
}

// now returns the reconciler's clock.
func (r *RemediationRequestReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// openPlanRepository opens the repository the plan was made for, and nothing else. It returns the provider,
// or the same errors as closeChangeRequest.
func (r *RemediationRequestReconciler) openPlanRepository(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	providers.GitProvider, error) {
	git, _, err := r.openPlanRepositoryOf(ctx, rr)
	return git, err
}

// openPlanRepositoryOf is openPlanRepository that also returns the type of the provider that hosts it. While rr
// carries the stale mark, every write of the provider but the close and a failed check run returns
// errStalePlanWrite.
func (r *RemediationRequestReconciler) openPlanRepositoryOf(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) (providers.GitProvider, remediationv1alpha1.GitProviderType, error) {
	git, host, _, err := r.openPlanRepositoryFor(ctx, rr)
	return git, host, err
}

// openPlanRepositoryFor is openPlanRepositoryOf that also says whether the repository was reached through a new
// installation of the same URL (reinstalled, see planEntry), so the caller checks the pull request's marker first.
func (r *RemediationRequestReconciler) openPlanRepositoryFor(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) (providers.GitProvider, remediationv1alpha1.GitProviderType, bool, error) {
	repo, reinstalled, err := r.planEntry(ctx, rr)
	if err != nil {
		return nil, "", false, err
	}
	git, host, err := r.openPlanEntry(ctx, rr, repo, reinstalled)
	return git, host, reinstalled, err
}

// planEntry returns the entry of the plan's repository in RemediationSettings, read live, or the same errors as
// openPlanRepositoryOf but for the provider's. A missing settings object is errNoSettings, which is errRepositoryGone
// too. With errTargetBranchChanged it also returns the entry, which then
// names another target branch than the recorded one (status.source), for the look at a merge (watchBranchChanged).
// reinstalled says that the entry is the plan's URL under a new installation of the GitHub App (#290): only while rr
// carries the stale mark, so only for the cleanup of a stale plan, or once that cleanup stopped on a refusal of the
// Git host (staleCleanupStopped, #808), so the read after the stop settles the lock where the cleanup worked
// (cleanupRepository).
func (r *RemediationRequestReconciler) planEntry(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) (remediationv1alpha1.GitRepository, bool, error) {
	settings := &remediationv1alpha1.RemediationSettings{}
	switch err := r.getLive(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, settings); {
	case apierrors.IsNotFound(err):
		// As errRepositoryGone everywhere main decided on it, but no evidence that the repository left the settings:
		// no end of a wait as not startable, no release of an undelivered merge (errNoSettings).
		return remediationv1alpha1.GitRepository{}, false, errNoSettings
	case err != nil:
		return remediationv1alpha1.GitRepository{}, false, fmt.Errorf("get RemediationSettings %s: %w",
			remediationv1alpha1.SettingsName, err)
	}
	// A pause of the kill switch stops every path that opens the plan's repository, reads included (sixth decision of
	// 2026-11-18 on #771): while it lasts the executor asks the Git host nothing for the request.
	if freshness.Paused(settings) {
		return remediationv1alpha1.GitRepository{}, false, freshness.ErrPaused
	}
	repo, reinstalled, ok := cleanupRepository(settings, rr, staleMark(rr) != nil || staleCleanupStopped(rr))
	if !ok {
		// Evidence that no read of the Git host can start for the request (endAfterLastRead).
		r.gitRetries().noteUnstartable(client.ObjectKeyFromObject(rr), unstartableReads[reasonRepositoryUnavailable])
		return remediationv1alpha1.GitRepository{}, false, errRepositoryGone
	}
	if src := rr.Status.Source; src == nil || repo.TargetBranch != src.TargetBranch {
		return repo, reinstalled, errTargetBranchChanged
	}
	return repo, reinstalled, nil
}

// cleanupRepository returns the settings entry of the repository rr's plan was made for (freshness.FindRepository).
// When there is none and stale is true (the cleanup of a stale plan, or its stop on a refusal), it also accepts the
// entry with the same URL under a new installation of the GitHub App (freshness.FindReinstalled, #290), and reports
// reinstalled. A changed
// installation makes the plan stale (the digest covers it), and the same URL is the same repository, so its pull
// request can still be closed there; but every caller reads the pull request through that entry with this request's
// marker (request UID and plan digest, invariant 5) before it fails the review check or closes it, and the provider
// opened for it writes nothing else (openPlanEntry). Another URL is never accepted: closing a number elsewhere could
// close someone else's pull request.
func cleanupRepository(settings *remediationv1alpha1.RemediationSettings, rr *remediationv1alpha1.RemediationRequest,
	stale bool) (repo remediationv1alpha1.GitRepository, reinstalled, ok bool) {
	if repo, ok := freshness.FindRepository(settings, rr.Status.PlanRepository); ok {
		return repo, false, true
	}
	if !stale {
		return remediationv1alpha1.GitRepository{}, false, false
	}
	repo, ok = freshness.FindReinstalled(settings, rr.Status.PlanRepository)
	return repo, ok, ok
}

// openPlanEntry opens repo, the entry planEntry returned without an error, for rr (see openPlanRepositoryOf).
// reinstalled is planEntry's: such a provider writes nothing but the close and a failed check run, whatever the mark.
func (r *RemediationRequestReconciler) openPlanEntry(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	repo remediationv1alpha1.GitRepository, reinstalled bool) (providers.GitProvider, remediationv1alpha1.GitProviderType, error) {
	// While the plan is stale (staleMark, read at each write) the provider writes nothing but the close and a failed
	// check run (#713): the one guard every step that writes to the plan's pull request passes (#199).
	git, err := r.openGated(ctx, repo, func() bool { return reinstalled || staleMark(rr) != nil })
	if providers.IsInvalidConfig(err) {
		return nil, "", freshness.ErrRepositoryUnusable
	}
	if err != nil {
		return nil, "", freshness.WrapProvider("open the repository", err)
	}
	return git, repo.Provider, nil
}

// advance runs the pull request step for a fresh request in PolicyChecked, PRCreated or AwaitingApproval. It
// is called by recheck after the freshness check passed; fresh is what that check read: the head of the target
// branch the plan was found fresh at, the target file there and the repository.
func (r *RemediationRequestReconciler) advance(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	fresh freshness.Result) (reconcile.Result, error) {
	switch rr.Status.Phase {
	case remediationv1alpha1.PhasePolicyChecked:
		return r.openPullRequest(ctx, rr, fresh)
	case remediationv1alpha1.PhasePRCreated, remediationv1alpha1.PhaseAwaitingApproval:
		// recheck looked at the pull request before the freshness check; the change is rendered again if the
		// target branch moved, and the next look is the periodic one.
		return r.renderWaiting(ctx, rr, fresh)
	}
	return reconcile.Result{RequeueAfter: freshRecheckInterval}, nil
}

// errNoHead means the freshness check passed without returning the commit it checked. It is unreachable
// through ensureFresh; the step fails closed on it and is retried, it never skips the render check.
var errNoHead = errors.New("the freshness check returned no target branch head")

// renderWaiting renders the change of a recorded pull request again at the head of the target branch, but only
// when that head is not the one of the last render (status.pullRequest.renderedTargetHead), so a quiet target
// branch costs no Git read beyond the freshness check. A pass records the head; a refusal closes the pull
// request and stops automation in NeedsIntervention (refuseWaiting).
func (r *RemediationRequestReconciler) renderWaiting(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	fresh freshness.Result) (reconcile.Result, error) {
	pr := rr.Status.PullRequest
	if pr == nil {
		return reconcile.Result{RequeueAfter: freshRecheckInterval}, nil
	}
	if fresh.Head == "" || fresh.Git == nil {
		return r.retry(ctx, rr, errNoHead)
	}
	if pr.RenderedTargetHead == fresh.Head {
		return r.waited(rr), nil
	}
	stepCtx, cancel := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	err := r.renderWaitingStep(stepCtx, rr, fresh)
	err = boundErr(ctx, stepCtx, "render check", err)
	cancel()
	if ref, ok := errors.AsType[*renderRefusal](err); ok {
		// The close has a deadline of its own, as the close of a stale plan's change request has.
		err = r.refuseWaiting(ctx, rr, ref)
	}
	switch {
	case errors.Is(err, errStopped):
		return reconcile.Result{}, nil
	case err != nil:
		return r.retry(ctx, rr, err)
	}
	return r.waited(rr), nil
}

// pollState is what PullRequestPollInterval needs to know of a request: when its last full recheck while its
// pull request waits ended, and the UID of the request it was, so a request made again with the same name starts
// with a full recheck.
type pollState struct {
	uid types.UID
	at  time.Time
}

// pullRequestPollInterval returns PullRequestPollInterval, or its default when zero. SetupWithManager has
// refused any other value outside its bounds, so nothing is clamped here.
func (r *RemediationRequestReconciler) pullRequestPollInterval() time.Duration {
	if r.PullRequestPollInterval == 0 {
		return DefaultPullRequestPollInterval
	}
	return r.PullRequestPollInterval
}

// validatePullRequestPollInterval returns an error unless the interval in use (zero gives the default) is within
// [MinPullRequestPollInterval, MaxPullRequestPollInterval]. It is the only check of the bounds; SetupWithManager
// runs it, so a value out of bounds stops the executor at start instead of being changed without a word.
func (r *RemediationRequestReconciler) validatePullRequestPollInterval() error {
	if d := r.pullRequestPollInterval(); d < MinPullRequestPollInterval || d > MaxPullRequestPollInterval {
		return fmt.Errorf("the pull request poll interval %s is not between %s and %s", d,
			ShortDuration(MinPullRequestPollInterval), ShortDuration(MaxPullRequestPollInterval))
	}
	return nil
}

// ShortDuration formats d as the flags are written: whole minutes as "5m", anything else as time.Duration does.
func ShortDuration(d time.Duration) string {
	if d > 0 && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

// waited ends a full recheck of a request whose pull request waits and needs nothing retried: it notes when, and
// requeues after the poll interval. The looks until freshRecheckInterval has passed read only the pull request
// (pollOnly).
func (r *RemediationRequestReconciler) waited(rr *remediationv1alpha1.RemediationRequest) reconcile.Result {
	r.checked.Store(client.ObjectKeyFromObject(rr), pollState{uid: rr.UID, at: r.now()})
	return reconcile.Result{RequeueAfter: r.pullRequestPollInterval()}
}

// lookOnly reports whether a request whose pull request was looked at may skip the full recheck now: it waits
// for review, and carries nothing the full recheck must settle at once, neither the mark of a stale plan's
// cleanup (staleMark), nor spec.cancel (#289), nor WritesPaused, which resumeIfAllowed removes with the
// WritesResumed Event at the first reconcile after the pause lifts. A PlanFresh Unknown of a refusal is left to the next full recheck, as before
// the looks: asking a host that refused again every look would only cost calls.
func lookOnly(rr *remediationv1alpha1.RemediationRequest) bool {
	if p := rr.Status.Phase; p != remediationv1alpha1.PhasePRCreated && p != remediationv1alpha1.PhaseAwaitingApproval {
		return false
	}
	return staleMark(rr) == nil && !cancelRequested(rr) &&
		meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionWritesPaused) == nil
}

// pollOnly reports whether recheck may stop after looking at the pull request, and the requeue then: the pull
// request was read and is still open (a failed look gets the full recheck and its error handling), lookOnly
// holds, and the last full recheck of this request (same name and UID) in this process ended less than
// freshRecheckInterval ago. Nothing is written to Git on a look, so skipping the freshness check there never lets
// a write through on a stale plan: every write still comes right after a full check (design section 6).
// recheck calls it only for a request with a recorded pull request.
func (r *RemediationRequestReconciler) pollOnly(rr *remediationv1alpha1.RemediationRequest, outcome prOutcome) (
	time.Duration, bool) {
	if outcome != prOpen || !lookOnly(rr) {
		return 0, false
	}
	v, ok := r.checked.Load(client.ObjectKeyFromObject(rr))
	if !ok {
		return 0, false
	}
	last, ok := v.(pollState)
	if !ok || last.uid != rr.UID {
		return 0, false
	}
	left := last.at.Add(freshRecheckInterval).Sub(r.now())
	if left <= 0 || left > freshRecheckInterval {
		// Due, or the clock went back: a full recheck.
		return 0, false
	}
	return min(left, r.pullRequestPollInterval()), true
}

// renderWaitingStep reads the target file at the recorded head of the pull request, checks that merging it
// into the target branch head the freshness check read gives exactly that file, renders it there and records
// the head as rendered. A change that does not pass is returned as a [*renderRefusal].
func (r *RemediationRequestReconciler) renderWaitingStep(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	fresh freshness.Result) error {
	git, src := fresh.Git, rr.Status.Source
	// The recorded head is a commit, whose content never changes; whether the pull request still points at it
	// is watchPullRequest's check (HeadMoved). It is read once per step, and this step runs only when the
	// target branch head moved since the last render, so a quiet branch never reads it. It is not kept across
	// reconciles: that would be state in memory that a restart loses, to save one read of a small file next to
	// the build directory the render reads.
	file, err := git.ReadFile(ctx, rr.Status.PullRequest.HeadSHA, string(src.Path), providers.MaxFileBytes)
	if err != nil {
		return freshness.WrapProvider("read the file of the pull request", err)
	}
	// planned splices the bytes of one scalar and keeps every other byte, so equal bytes mean the target file
	// at the head and the file of the pull request differ in that scalar only. The patcher accepts more than one
	// spelling of the previous value (256Mi and 268435456), so the file the branch was cut from may spell it
	// otherwise than the head; that difference is on the line the pull request changes, so Git cannot merge the
	// two without a conflict, and resolving one pushes to the branch, which stops automation (HeadMoved). That
	// is why the file at the cut point, which the status does not record, need not be read.
	want, state, err := planned(fresh.File, rr)
	switch {
	case err != nil:
		return &renderRefusal{reason: ReasonPatchRefused,
			message: "The typed patcher refused the target file on the target branch: " + patchReason(err)}
	case state != fileHoldsPrevious || !bytes.Equal(want, file.Data):
		return &renderRefusal{reason: ReasonTargetFileDiverged, message: "The target file changed on the target branch " +
			"while the pull request waited, so the change was not rendered as it would merge"}
	}
	// The render below sees the target branch with the file replaced, which is what merging gives only for a
	// head that is the one planned commit on top of the target branch (#524). A pull request recorded before
	// that was checked at its opening is checked here too.
	switch bs, err := branchState(ctx, git, rr, rr.Status.PullRequest.HeadSHA); {
	case err != nil:
		return err
	case bs != providers.BranchOneChange:
		return &renderRefusal{reason: ReasonBranchDiverged,
			message: "The pull request's branch holds more than the planned change on top of the target branch"}
	}
	// A pass records the head in the status write that removes the render's mark (checkRender).
	return r.checkRender(ctx, git, fresh.Head, file.Data, rr, func(rr *remediationv1alpha1.RemediationRequest) bool {
		if rr.Status.PullRequest == nil { // only in a request read again after a conflict (unmarkRender)
			return false
		}
		rr.Status.PullRequest.RenderedTargetHead = fresh.Head
		return true
	})
}

// refuseWaiting ends a waiting request whose change no longer renders as planned at the head of the target
// branch (design section 6). It closes the pull request first, through the same close as a stale plan's
// (closeChangeRequest: only in the plan's repository, with no comment and nothing from the file), and then
// stops the request in NeedsIntervention with the refusal's reason. A close that fails for a reason worth a
// retry (a Git host failure, paused writes, the deadline) is returned for a retry, which renders and closes
// again; settings that cannot reach the repository any more stop the request with the pull request left open.
func (r *RemediationRequestReconciler) refuseWaiting(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	ref *renderRefusal) error {
	cleanCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	err := r.closeChangeRequest(cleanCtx, rr)
	// A Git read that ran out of the cleanup's time is still that read, retried as one (keepGitRead).
	err = keepGitRead(ctx, cleanCtx, "cleanup of the change request", err)
	cancel()
	outcome := "; the pull request was closed"
	switch {
	case err == nil:
		closedByUs(rr, "the change no longer renders as planned ("+ref.reason+")")
	case errors.Is(err, freshness.ErrRepositoryUnusable), errors.Is(err, errTargetBranchChanged),
		errors.Is(err, errRepositoryGone):
		outcome = "; the settings no longer reach its repository, so the pull request was not closed"
	case errors.Is(err, errOpenElsewhere):
		pr := openElsewhereOf(err)
		return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonChangeRequestOnOtherBase,
			ref.message+"; the "+openElsewhereText(pr)+", so it was not closed"+messageOpenElsewhere)
	case errors.Is(err, errChangeRequestNotOurs):
		outcome = "; the recorded pull request could not be confirmed as this request's, so it was not closed"
	default:
		return err
	}
	return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ref.reason, ref.message+outcome)
}

// startDeadline stores status.deadline of the pull request step, in its own status write. The policy check
// stores it with the decision; recheck calls this only for a request decided while writes were paused, once
// the freshness check finds them allowed again.
func (r *RemediationRequestReconciler) startDeadline(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	rr.Status.Deadline = &metav1.Time{Time: r.now().Add(r.pullRequestTimeout())}
	wasPaused := resume(rr)
	if err := r.Status().Update(ctx, rr); err != nil {
		return reconcile.Result{}, fmt.Errorf("set the deadline of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	if wasPaused {
		r.resumed(ctx, rr)
	}
	return reconcile.Result{RequeueAfter: stepDelay}, nil
}

// openPullRequest does the next step towards an open pull request: add the finalizer, create the branch,
// commit the change or open the pull request, one of them per call. recheck has stored the deadline.
func (r *RemediationRequestReconciler) openPullRequest(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	fresh freshness.Result) (reconcile.Result, error) {
	if rr.Status.Deadline == nil {
		// Unreachable through recheck; no Git write without a deadline.
		return reconcile.Result{}, errors.New("the pull request step has no deadline")
	}
	if !controllerutil.ContainsFinalizer(rr, FinalizerName) {
		base := rr.DeepCopy()
		controllerutil.AddFinalizer(rr, FinalizerName)
		// A merge patch with the resourceVersion: a concurrent change makes it a conflict, never a lost write.
		if err := r.Patch(ctx, rr, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return reconcile.Result{}, fmt.Errorf("add the finalizer to RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
		}
		return reconcile.Result{RequeueAfter: stepDelay}, nil
	}
	if r.now().After(rr.Status.Deadline.Time) {
		return r.expire(ctx, rr)
	}

	stepCtx, cancel := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	defer cancel()
	var res reconcile.Result
	var err error
	if ho := rr.Status.Handoff; rr.Spec.Replaces != nil && ho != nil && ho.From != nil {
		// A replacement takes its predecessor's pull request over instead of opening one (handoff.go).
		res, err = r.handoffStep(stepCtx, rr, fresh)
	} else if rr.Spec.Replaces != nil {
		// Admitted without the handoff's record: never taken over (ADR-0028 section 4 item 2).
		err = r.stop(stepCtx, rr, remediationv1alpha1.PhaseCancelled, remediationv1alpha1.ReasonHandoffRefused,
			"The request was admitted without the record of its handoff; nothing was taken over")
	} else {
		res, err = r.pullRequestStep(stepCtx, rr, fresh)
	}
	err = boundErr(ctx, stepCtx, "pull request step", err)
	switch {
	case errors.Is(err, errStopped):
		return reconcile.Result{}, nil
	case err != nil:
		return r.retry(ctx, rr, err)
	}
	return res, nil
}

// pullRequestStep rebuilds the state from Git and makes at most one write, in the repository the freshness
// check opened and at the head of the target branch it found the plan fresh at.
func (r *RemediationRequestReconciler) pullRequestStep(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	fresh freshness.Result) (reconcile.Result, error) {
	if fresh.Head == "" || fresh.Git == nil {
		return reconcile.Result{}, errNoHead
	}
	git := fresh.Git
	branch := freshness.BranchName(rr)
	src := rr.Status.Source

	// The pull request may exist although the status write that records it failed (design section 6). Every pull
	// request of the branch is read (changeOf), since the newest may hide an older one of this request: an open one
	// of its own is adopted, else a merged one, else a closed one. With none of its own open or merged on the target
	// branch, the pull requests of the branch to every base, in every state, are listed too (openElsewhere): a person
	// may have moved its own to another base, and closed or merged it there (#892, decisions of 2026-11-19;
	// invariant 5). Open there, it is recorded and stops the request as the watch does; merged there, it is recorded
	// and stops the request as the watch does for a recorded one (stopMergedElsewhere); closed there, it stops the
	// request as one closed on the target branch does. None is opened next to it in any case. With none of its own, a
	// pull request of the branch without its marker stops the request; with none at all the branch steps go on.
	// Unknown is retried, a listing past its bound included, and opens nothing.
	v := r.changeOf(ctx, git, rr, changeQuery{})
	var open []providers.ChangeRequest
	if failed, ok := r.openElsewhere(ctx, git, rr, []remediationv1alpha1.GitBranch{src.TargetBranch}, false, true, &v,
		&open); !ok {
		v = failed
	}
	if done, err := r.adoptFound(ctx, git, rr, v, fresh.File); done {
		return reconcile.Result{}, err
	}

	head, err := git.BranchHead(ctx, branch)
	switch {
	case errors.Is(err, providers.ErrNotFound):
		// Step 2: the branch starts at the head the freshness check has just found the file holding the
		// previous value at, not at a head read again, which may hold a file no check saw. A merge since then
		// does not fail the creation (CreateBranch takes any commit of the target branch); the commit step
		// compares the file again at the head of its own check.
		if err := git.CreateBranch(ctx, branch, fresh.Head); err != nil {
			return reconcile.Result{}, freshness.WrapProvider("create the branch", err)
		}
		logf.FromContext(ctx).Info("Created branch for RemediationRequest", "remediationRequest", client.ObjectKeyFromObject(rr))
		return reconcile.Result{RequeueAfter: stepDelay}, nil
	case err != nil:
		return reconcile.Result{}, freshness.WrapProvider("read the head of the branch", err)
	}

	file, err := git.ReadFile(ctx, head, string(src.Path), providers.MaxFileBytes)
	if err != nil {
		return reconcile.Result{}, freshness.WrapProvider("read the file on the branch", err)
	}
	changed, state, err := planned(file.Data, rr)
	if err != nil {
		return reconcile.Result{}, r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonPatchRefused,
			"The typed patcher refused the file on the branch: "+patchReason(err))
	}
	switch state {
	case fileHoldsPrevious:
		// The branch must carry nothing of its own yet: the render below sees the target branch, never what
		// someone else pushed to the branch (#524). The head the freshness check read needs no further read; a
		// branch cut at an earlier head must still be on the target branch.
		if head != fresh.Head {
			if err := r.requireOnTarget(ctx, git, rr, head); err != nil {
				return reconcile.Result{}, err
			}
		}
		// Steps 3 to 6: one commit that changes the one value, on top of the head just read. The change is
		// rendered at the target branch head, which may have moved since the branch was cut. The target file
		// there must be byte for byte the file the change was made from, so merging gives exactly changed: a
		// merge elsewhere passes, an edit of the file (another field, a comment, another spelling of the
		// previous value) stops the request.
		if !bytes.Equal(fresh.File, file.Data) {
			return reconcile.Result{}, r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonTargetFileDiverged,
				"The target file on the target branch is not the file the branch was cut from, so the change was "+
					"not rendered as it would merge; nothing was committed")
		}
		if err := r.checkRender(ctx, git, fresh.Head, changed, rr, nil); err != nil {
			if ref, ok := errors.AsType[*renderRefusal](err); ok {
				return reconcile.Result{}, r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ref.reason,
					ref.message+"; nothing was committed")
			}
			return reconcile.Result{}, err
		}
		if _, err := git.CommitFile(ctx, branch, head, providers.FileChange{
			Path: string(src.Path), Content: changed, Message: commitTitle(rr),
		}); err != nil {
			return reconcile.Result{}, freshness.WrapProvider("commit the change", err)
		}
		logf.FromContext(ctx).Info("Committed change for RemediationRequest", "remediationRequest", client.ObjectKeyFromObject(rr))
		return reconcile.Result{RequeueAfter: stepDelay}, nil
	case fileHoldsDesired:
		// The commit is there (a restart after the commit, or a concurrent reconcile); open the pull request,
		// but only for a branch that is exactly the planned commit on top of the target branch. The open below
		// records the pull request only if its head is still the one checked here.
		switch ref, err := r.checkPlanned(ctx, git, rr, fresh.File, head, file.Data); {
		case err != nil:
			return reconcile.Result{}, err
		case ref != nil:
			return reconcile.Result{}, r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ref.reason,
				ref.message+"; no pull request was opened")
		}
	default:
		return reconcile.Result{}, r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonBranchDiverged,
			"The branch holds neither the previous nor the planned value of the file")
	}

	title, body, err := r.pullRequestText(ctx, rr)
	if err != nil {
		return reconcile.Result{}, err
	}
	// The last step before the create, after every slow one (ADR-0039 points 21 and 22): a request whose
	// Incident was folded into its cause's, or is gone, ends Cancelled and opens no pull request, leaving its slot
	// in the write that ends it and its lock after the read of its branch (checkHeld); a read that fails holds it
	// for a retry.
	switch reason, message, err := r.incidentStanding(ctx, rr); {
	case err != nil:
		return reconcile.Result{}, err
	case reason != "":
		return reconcile.Result{}, r.endForIncident(ctx, rr, reason, message)
	}
	cr, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{
		HeadBranch: branch,
		BaseBranch: string(src.TargetBranch),
		Title:      title,
		Body:       body,
	})
	if err != nil {
		return reconcile.Result{}, freshness.WrapProvider("open the pull request", err)
	}
	if !hasMarker(cr.Body, rr) {
		// The host returned an open pull request of the branch that someone else opened since the look-up above
		// (GitHub answers such a create with 422 and the adapter returns the open one): never ours to record.
		return reconcile.Result{}, r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonForeignChangeRequest,
			"A pull request for the branch exists without this request's marker; it was not touched")
	}
	if cr.HeadSHA != head {
		// The branch moved between the check and the open: someone else pushed, and the pull request now holds
		// what no check saw. It is ours (it carries the marker), so it is closed and recorded (#524).
		return reconcile.Result{}, r.refuseOpened(ctx, git, rr, cr, &renderRefusal{reason: ReasonBranchDiverged,
			message: "The branch moved between its check and the opening of the pull request"})
	}
	return reconcile.Result{}, r.recordPullRequest(ctx, rr, cr)
}

// adoptFound is pullRequestStep's answer to v, the read of the request's pull requests on every base: done is true
// when it decided (adopted, stopped or a failed read to retry, err), false when the branch steps go on.
func (r *RemediationRequestReconciler) adoptFound(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, v changeVerdict, freshFile []byte) (bool, error) {
	switch v.rule().adopt {
	case adoptRetry:
		return true, v.err
	case adoptIt:
		return true, r.adoptChangeRequest(ctx, git, rr, v.pr, freshFile)
	case adoptElsewhere:
		// Recorded in the write that stops the request, so the watch, the lock and the finalizer read it by its
		// number from then on. A lost write is found again by the next look.
		recordFound(rr, v.pr)
		return true, r.stopOpenElsewhere(ctx, rr, v.pr)
	case adoptMergedElsewhere:
		// Recorded in the write that stops the request, as the watch has it, without a merge: nothing reached the
		// target branch. A lost write is found again by the next look.
		if recordable(v.pr) {
			recordNumber(rr, remediationv1alpha1.PullRequestStatus{Number: v.pr.Number, URL: v.pr.URL, HeadSHA: v.pr.HeadSHA})
		}
		return true, r.stopMergedElsewhere(ctx, rr, v.pr)
	case adoptNone:
		if v.foreign {
			return true, r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonForeignChangeRequest,
				"A pull request for the branch exists without this request's marker; it was not touched")
		}
	}
	return false, nil
}

// adoptOutcome is what pullRequestStep does per verdict on the pull requests of the branch.
type adoptOutcome int

const (
	adoptRetry adoptOutcome = iota
	adoptIt
	adoptNone
	// adoptElsewhere: the request's own pull request is open on a base a person moved it to (#892): read by its
	// recorded number, or, with none recorded, found by its marker in the listing of the branch to every base
	// (openElsewhere). The request records it and stops as the watch does (stopOpenElsewhere), and nothing is
	// opened on the target branch next to it.
	adoptElsewhere
	// adoptMergedElsewhere: the request's own pull request was merged into a base a person moved it to, found by its
	// marker in the listing of every base in every state. The request records it and stops as the watch does for a
	// recorded one (stopMergedElsewhere), and nothing is opened on the target branch.
	adoptMergedElsewhere
)

// pullRequestStep's rule per verdict is the column adopt of verdictTable: open, merged and closed adopt the pull
// request (record it, end Merged, or stop as closed; closed on another base as well), gone goes on to the branch
// steps, unknown is retried.

// checkPlanned checks a branch head whose file holds the planned value before its pull request is opened or
// recorded (#524): data, the file at head, must be byte for byte the planned change of the target file the
// freshness check read (freshFile), exactly what Shadewright commits, and head must be one commit on top of
// the target branch that changes only that file. A head that does not pass is returned as a [*renderRefusal];
// a failure to find out is returned as an error for a retry.
func (r *RemediationRequestReconciler) checkPlanned(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, freshFile []byte, head string, data []byte) (*renderRefusal, error) {
	switch bs, err := branchState(ctx, git, rr, head); {
	case err != nil:
		return nil, err
	case bs == providers.BranchAtTarget:
		return &renderRefusal{reason: ReasonBranchDiverged,
			message: "The branch carries no commit of its own on top of the target branch"}, nil
	case bs != providers.BranchOneChange:
		return &renderRefusal{reason: ReasonBranchDiverged,
			message: "The branch holds more than one commit on top of the target branch, or a commit that " +
				"changes more than the target file"}, nil
	}
	want, state, err := planned(freshFile, rr)
	switch {
	case err != nil:
		return &renderRefusal{reason: ReasonPatchRefused,
			message: "The typed patcher refused the target file on the target branch: " + patchReason(err)}, nil
	case state != fileHoldsPrevious:
		return &renderRefusal{reason: ReasonTargetFileDiverged,
			message: "The target file on the target branch no longer holds the value the change was made from"}, nil
	case !bytes.Equal(want, data):
		return &renderRefusal{reason: ReasonBranchDiverged,
			message: "The target file on the branch is not exactly the planned change of the target file on " +
				"the target branch"}, nil
	}
	return nil, nil
}

// refuseOpened ends a request whose pull request is open and carries its marker (Shadewright opened it) but was
// not recorded, and whose head did not pass a check before recording, whatever the check (#524). It records the
// pull request by the number the open or the look-up returned, closes it by that number, never by a new look-up
// of the marker (which a person may have removed since), and stops in NeedsIntervention with the refusal's
// reason in one status write. The pull request is recorded as closed only when the Git host confirmed the
// close (CloseChangeRequest returns only then); a person's merge before it, seen in a look-up, ends
// MergedWithForeignChange, a person's close ChangeRequestClosed. A close that fails for a reason worth a retry
// (a host failure, the deadline, paused writes, a conflict the look-up does not explain) changes nothing and
// is returned for a retry. Only a final refusal of the host stops the request with the pull request recorded as
// open, so it holds the lock on its target until a person closes it (checkHeld). It never stops with an open
// pull request unrecorded.
func (r *RemediationRequestReconciler) refuseOpened(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest, ref *renderRefusal) error {
	before, rv := rr.Status.DeepCopy(), rr.ResourceVersion
	recordNumber(rr, remediationv1alpha1.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA})
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionPROpened,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonPullRequestOpened,
		Message:            fmt.Sprintf("Pull request #%d is open", cr.Number),
		ObservedGeneration: rr.Generation,
	})
	// retry hands err back with the status as it was, except what changeOf wrote meanwhile (restoreStatus):
	// nothing is decided, and the next reconcile finds the pull request again by its branch and checks it again.
	retry := func(err error) error {
		restoreStatus(rr, before, rv)
		return err
	}
	cleanCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	defer cancel()
	ended := func(found providers.ChangeRequest) (bool, error) {
		switch found.State {
		case providers.ChangeRequestMerged:
			return true, r.mergedForeign(ctx, rr, ref)
		case providers.ChangeRequestClosed:
			markClosed(rr, ReasonChangeRequestClosed, "was closed without a merge")
			return true, r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonChangeRequestClosed,
				"The pull request was closed without a merge; automation stopped")
		}
		return false, nil
	}
	// A person may have merged or closed it since the open: that is looked at first, because closing a closed
	// pull request succeeds and would hide who closed it. A look-up that fails, or finds another pull request,
	// decides nothing; the close by number below still runs.
	read := changeQuery{number: cr.Number}
	switch v := r.changeOf(cleanCtx, git, rr, read); v.rule().refuseOpened {
	case refuseEnded:
		if ok, err := ended(v.pr); ok {
			return err
		}
	case refuseMoved:
		// A person moved it to another base since the open (#892): nothing is closed there.
		return r.stopOpenElsewhere(ctx, rr, v.pr)
	case refuseMergedElsewhere:
		// Moved to another base and merged there: nothing to close (the close to the target branch would be refused),
		// and the request stops with the pull request named, as the watch stops it.
		return r.stopMergedElsewhere(ctx, rr, v.pr)
	case refuseClose:
	}
	err := git.CloseChangeRequest(cleanCtx, cr.Number)
	err = boundErr(ctx, cleanCtx, "close the pull request", err)
	outcome := "; the pull request was closed"
	switch {
	case err == nil:
		closedByUs(rr, "the branch does not hold exactly the planned change ("+ref.reason+")")
	case providers.IsConflict(err):
		// A merge between the look-up and the close is a conflict, but so is any other 409 of the host: only a
		// look-up that shows the pull request merged (or closed) decides; anything else is retried.
		v := r.changeOf(cleanCtx, git, rr, read)
		if v.rule().refuseOpened == refuseEnded {
			if ok, err := ended(v.pr); ok {
				return err
			}
		}
		return retry(errors.Join(freshness.WrapProvider("close the pull request", err), v.err))
	default:
		if _, _, final := refusal(err); !final {
			// A host failure, the deadline or paused writes: retried (a pause waits for the kill switch).
			return retry(freshness.WrapProvider("close the pull request", err))
		}
		logf.FromContext(ctx).Info("Could not close the pull request of RemediationRequest", "remediationRequest",
			client.ObjectKeyFromObject(rr), "pullRequest", cr.Number, "error", err.Error())
		outcome = "; the Git host refused to close the pull request, so it is still open: close it by hand"
	}
	return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ref.reason, ref.message+outcome)
}

// adoptChangeRequest takes over a pull request found for the branch: one this request opened before its
// status write failed, or one someone else opened on its branch name.
func (r *RemediationRequestReconciler) adoptChangeRequest(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest, freshFile []byte) error {
	if !hasMarker(cr.Body, rr) {
		return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonForeignChangeRequest,
			"A pull request for the branch exists without this request's marker; it was not touched")
	}
	switch cr.State {
	case providers.ChangeRequestMerged:
		return r.endMerged(ctx, git, rr, cr)
	case providers.ChangeRequestClosed:
		return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonChangeRequestClosed,
			"The pull request was closed without merging; it is not reopened")
	}
	// The status lost the head Shadewright pushed, so the head is accepted only when the file on it holds the
	// planned value. The pull request carries the marker and is open: a head that fails any check below is
	// recorded and closed, never left open unrecorded (#524). A read that fails decides nothing and is retried.
	file, err := git.ReadFile(ctx, cr.HeadSHA, string(rr.Status.Source.Path), providers.MaxFileBytes)
	if err != nil {
		return freshness.WrapProvider("read the file of the pull request", err)
	}
	if _, state, err := planned(file.Data, rr); err != nil || state != fileHoldsDesired {
		return r.refuseOpened(ctx, git, rr, cr, &renderRefusal{reason: ReasonHeadMoved,
			message: "The pull request's head does not hold the planned change"})
	}
	// The head is recorded only as checked here; a later push to the pull request is watchPullRequest's
	// HeadMoved.
	switch ref, err := r.checkPlanned(ctx, git, rr, freshFile, cr.HeadSHA, file.Data); {
	case err != nil:
		return err
	case ref != nil:
		return r.refuseOpened(ctx, git, rr, cr, ref)
	}
	return r.recordPullRequest(ctx, rr, cr)
}

// branchState asks the Git host what the branch head carries over the target branch (#524). A branch handed
// over in a supersede chain carries one commit per request of the chain (chainDepth, ADR-0028 section 4), each
// of which may change only the target file. A failure to find out is returned for a retry; it never counts as
// an answer.
func branchState(ctx context.Context, git providers.GitProvider, rr *remediationv1alpha1.RemediationRequest,
	head string) (providers.BranchState, error) {
	return branchStateOf(ctx, git, string(rr.Status.Source.Path), head, chainDepth(rr))
}

// branchStateOf is branchState for a chain of commits commits at path.
func branchStateOf(ctx context.Context, git providers.GitProvider, path, head string, commits int) (
	providers.BranchState, error) {
	var state providers.BranchState
	var err error
	if commits == 1 {
		state, err = git.CheckBranch(ctx, head, path)
	} else {
		state, err = git.CheckChain(ctx, head, path, commits)
	}
	if err != nil {
		return 0, freshness.WrapProvider("check what the branch holds", err)
	}
	return state, nil
}

// requireOnTarget stops the request in NeedsIntervention (BranchDiverged) unless head, the branch head before
// the commit, is on the target branch, so the branch carries nothing of its own yet. Any other answer, an
// unknown one included, stops it. A failure to find out is returned for a retry.
func (r *RemediationRequestReconciler) requireOnTarget(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, head string) error {
	state, err := branchState(ctx, git, rr, head)
	if err != nil {
		return err
	}
	if state == providers.BranchAtTarget {
		return nil
	}
	return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonBranchDiverged,
		"The branch holds commits that are not on the target branch; nothing was committed")
}

// endMerged ends a request whose pull request, carrying its marker but never recorded (its open's answer or
// status write was lost), a person merged (#524). The pull request is recorded either way. Merged only when its
// head held exactly the planned change (checkMerged), with its merge commit recorded as watchStep does
// (recordMerge, #561), so delivery is checked against that commit; otherwise MergedWithForeignChange without a
// merge commit, since the change that landed is not the planned one. A read that fails decides nothing and is
// returned for a retry, with the status unchanged, except a check at the merge that did not answer, which
// records the pull request first (mergeUnchecked). A failed read of checkMerged is one of the merge wait about cr,
// so the caller's retry records that wait and it ends at its end, whatever bound the caller has (#889).
func (r *RemediationRequestReconciler) endMerged(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest) error {
	// Without a full merge commit ID it waits for the host to report one, a bounded time (awaitMergeCommit), and
	// is retried after what is left of the wait; nothing is recorded meanwhile.
	switch left, err := r.awaitMergeCommit(ctx, rr, cr); {
	case err != nil:
		return err
	case left > 0:
		return &freshness.ProviderError{Op: "wait for the merge commit", Class: providers.ErrRetryable, RetryAfter: left}
	}
	ref, err := r.checkMerged(ctx, git, rr, cr)
	if err != nil {
		return err
	}
	recordNumber(rr, remediationv1alpha1.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA})
	// A request cancelled because of its Incident needs no check at the merge: its plan was stale before the merge,
	// and it gets the mark with the reason of the cancel (markCancelledMerge), so the outcome is capped as a stale
	// plan's is. The mark goes on a merge with another change too: the end rewrites PullRequestOpened and Approved,
	// and PlanFresh is then the one place that keeps why it was cancelled.
	reason, cancelled := cancelledForIncident(rr)
	if cancelled {
		markCancelledMerge(rr, reason)
	}
	if ref != nil {
		return r.mergedForeign(ctx, rr, ref)
	}
	// The reduced freshness check at the merge (ADR-0041): a stale plan is marked in the write below. A check that
	// did not answer records the pull request like an open one (mergeUnchecked) and is returned for a retry.
	if !cancelled {
		if err := r.checkAtMerge(ctx, rr, cr); err != nil {
			return r.mergeUnchecked(ctx, rr, cr, err)
		}
	}
	markClosed(rr, ReasonChangeRequestMerged, "was merged")
	recordMerge(ctx, rr, cr)
	return r.stop(ctx, rr, remediationv1alpha1.PhaseMerged, ReasonChangeRequestMerged, "The pull request was merged")
}

// mergeUnchecked records the pull request cr, found merged and checked by checkMerged, whose check at the merge
// did not answer (checkErr): PRCreated with the pull request, as recordHandoff does, and returns checkErr for a
// retry. The merge is not recorded. From then on recheck looks at the recorded pull request first
// (watchPullRequest), which checks the plan at the merge again before it records the merge, within the wait that
// checkAtMerge started; the full freshness check never runs on the merged change, which makes the plan look stale
// (TargetFileChanged) and would end a fresh plan capped. A failed write is joined to checkErr: nothing is
// recorded, and the retry finds the pull request through the branch again.
//
// A request that no longer progresses (it ended Expired, or stopped, and its own pull request was found merged by
// the read after the stop) keeps its phase and its deadline: such a request leaves its phase only for the record of
// the merge (sixth decision of 2026-11-18 on #771, #808), never back to PRCreated. The stop's condition moves from
// PullRequestOpened to Approved, where stop puts it once a pull request is recorded, so the read after the stop
// still sees why the request ended (expiredUncleaned) and records the merge when the check answers or its wait ends.
func (r *RemediationRequestReconciler) mergeUnchecked(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest, checkErr error) error {
	if progresses(rr) {
		r.setPhase(rr, remediationv1alpha1.PhasePRCreated)
		rr.Status.Deadline = nil
	} else if c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPROpened); c != nil &&
		c.Status == metav1.ConditionFalse {
		// The stop was written before a pull request was recorded (endMerged has just recorded it in memory).
		moved := *c
		moved.Type = remediationv1alpha1.ConditionApproved
		meta.SetStatusCondition(&rr.Status.Conditions, moved)
	}
	recordNumber(rr, remediationv1alpha1.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA})
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:   remediationv1alpha1.ConditionPROpened,
		Status: metav1.ConditionTrue,
		Reason: ReasonPullRequestOpened,
		Message: fmt.Sprintf("Pull request #%d was found merged; the merge is recorded once the plan is checked "+
			"at the merge", cr.Number),
		ObservedGeneration: rr.Generation,
	})
	if err := r.Status().Update(ctx, rr); err != nil {
		return errors.Join(checkErr, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace,
			rr.Name, err))
	}
	logf.FromContext(ctx).Error(checkErr, "Recorded the merged pull request of RemediationRequest without its "+
		"merge: the plan could not be checked at the merge", "remediationRequest", client.ObjectKeyFromObject(rr),
		"pullRequest", cr.Number)
	return checkErr
}

// checkMerged checks the head of a merged pull request (#524) against its own parent, the commit of the target
// branch it was made on, not against the target branch now, which the merge moved: the head is one commit that
// changes only the target file on top of a commit of the target branch from before the merge (CheckMerged), and
// the target file there is byte for byte the planned change of the target file at that parent. A merge commit
// that took the head into the target branch so does not change the answer. A head that fails is returned as a
// [*renderRefusal]; a read that fails as an error of the merge wait about cr (readOfMergeCheck), whatever the Git
// host answers, so every caller's retry records that wait and the request ends at its end, from the entry, as a
// merge whose content could not be checked (endMergeWait, #889), also on a path whose own bound has no end.
func (r *RemediationRequestReconciler) checkMerged(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest) (*renderRefusal, error) {
	ref, err := checkMergedHead(ctx, git, rr, cr)
	return ref, readOfMergeCheck(cr, err)
}

// checkMergedHead is checkMerged's reads and verdict, with a failed read returned as it failed.
func checkMergedHead(ctx context.Context, git providers.GitProvider, rr *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest) (*renderRefusal, error) {
	path := string(rr.Status.Source.Path)
	file, err := git.ReadFile(ctx, cr.HeadSHA, path, providers.MaxFileBytes)
	if err != nil {
		return nil, freshness.WrapProvider("read the file of the pull request", err)
	}
	if _, state, err := planned(file.Data, rr); err != nil || state != fileHoldsDesired {
		return &renderRefusal{reason: ReasonHeadMoved,
			message: "The pull request's head does not hold the planned change"}, nil
	}
	diverged := &renderRefusal{reason: ReasonBranchDiverged,
		message: "The pull request's head is not one commit that changes only the target file on top of the " +
			"target branch as it was before the merge"}
	if !shaPattern.MatchString(cr.MergeCommit) {
		// After the wait for it (endMerged): what the merge brought cannot be checked without the merge commit.
		return &renderRefusal{reason: ReasonBranchDiverged, message: "The Git host reported no full merge commit " +
			"ID for the merged pull request, so what the merge brought cannot be checked"}, nil
	}
	state, parent, err := git.CheckMerged(ctx, cr.HeadSHA, cr.MergeCommit, path)
	switch {
	case err != nil:
		return nil, freshness.WrapProvider("check what the merged pull request held", err)
	case state != providers.BranchOneChange || parent == "":
		return diverged, nil
	}
	from, err := git.ReadFile(ctx, parent, path, providers.MaxFileBytes)
	if err != nil {
		return nil, freshness.WrapProvider("read the target file the pull request was made on", err)
	}
	if want, state, err := planned(from.Data, rr); err != nil || state != fileHoldsPrevious || !bytes.Equal(want, file.Data) {
		return &renderRefusal{reason: ReasonBranchDiverged,
			message: "The target file of the merged pull request is not exactly the planned change"}, nil
	}
	return nil, nil
}

// mergedUnrecorded ends a request whose stale plan's cleanup found its change request merged (closing a merged
// one is a conflict) before any status recorded it: the merge of its own pull request is what made the plan
// stale, or whose close at the deadline was refused the same way. It reads the pull request again by the number
// the close tried (closeConflictError; changeOf with the marker; ADR-0030 section 1: a newer pull request on the
// same branch never stands in for it), and ends the request through endMerged when it is merged. Unknown (a failed
// read, a 404 the branch does not confirm, an answer about another pull request) is returned joined with closeErr
// for a retry. Open, closed or gone (a 404 the branch confirms, another branch or base, no marker), or a closeErr
// without a number, returns closeErr.
func (r *RemediationRequestReconciler) mergedUnrecorded(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	closeErr error) error {
	tried, ok := errors.AsType[*closeConflictError](closeErr)
	if !ok {
		return closeErr
	}
	git, err := r.openPlanRepository(ctx, rr)
	if err != nil {
		return errors.Join(closeErr, err)
	}
	// openPlanRepository returned, so the source is recorded and its target branch is the repository's.
	switch v := r.changeOf(ctx, git, rr, changeQuery{number: tried.number, marker: true}); v.rule().mergedUnrecorded {
	case mergedUnrecordedMerged:
		return r.endMerged(ctx, git, rr, v.pr)
	case mergedUnrecordedRetry:
		return errors.Join(closeErr, v.err)
	case mergedUnrecordedNothing:
	}
	return closeErr
}

// mergedForeign ends a request whose recorded pull request a person merged although its head failed a check
// before Shadewright could close it (#524): NeedsIntervention with MergedWithForeignChange and a message that
// names the pull request, which is recorded as merged (PullRequestOpened ChangeRequestMerged), so the lock on the
// target is released in the write that ends the request (stop, settleRecorded) and the merged change is reviewed by a
// person. Shadewright cannot stop a human merge; it reports it.
func (r *RemediationRequestReconciler) mergedForeign(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	ref *renderRefusal) error {
	markClosed(rr, ReasonChangeRequestMerged, "was merged although it failed a check")
	return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonMergedWithForeignChange,
		fmt.Sprintf("Pull request #%d was merged although it failed a check (%s): "+
			"%s. Review the merged change", recordedNumber(rr), ref.reason, ref.message))
}

// recordPullRequest writes PRCreated with the pull request in one status write, which also ends the Git waits that
// no longer apply (the request deadline's).
func (r *RemediationRequestReconciler) recordPullRequest(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest) error {
	r.setPhase(rr, remediationv1alpha1.PhasePRCreated)
	recordNumber(rr, remediationv1alpha1.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA})
	rr.Status.Deadline = nil
	// The deadline's wait ends with the deadline, in this write: a write applied but reported failed must not leave
	// an entry that no longer applies standing past its end until the next reconcile removes it.
	dropEnded(rr, r.now())
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionPROpened,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonPullRequestOpened,
		Message:            fmt.Sprintf("Pull request #%d is open", cr.Number),
		ObservedGeneration: rr.Generation,
	})
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	logf.FromContext(ctx).Info("Opened pull request for RemediationRequest", "remediationRequest",
		client.ObjectKeyFromObject(rr), "pullRequest", cr.Number)
	return nil
}

// recordNumber records pr as the request's pull request, in the status write the caller makes.
func recordNumber(rr *remediationv1alpha1.RemediationRequest, pr remediationv1alpha1.PullRequestStatus) {
	rr.Status.PullRequest = &pr
}

// markClosed rewrites PullRequestOpened of a request whose recorded pull request is closed without a merge (or,
// found by checkHeld after the request stopped, merged), so the condition no longer says it is open: the
// reason, the number and a fixed text that says why. The lock on the target reads these reasons (prMayBeOpen).
// The caller writes it in the status write that ends the request. A request without a recorded pull request
// is left alone: its PullRequestOpened never said one was open. The phase that ends the request reads the reason
// to decline its Pending reviewer hints with (declineOnEnd).
func markClosed(rr *remediationv1alpha1.RemediationRequest, reason, text string) {
	pr := rr.Status.PullRequest
	if pr == nil {
		return
	}
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionPROpened,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            fmt.Sprintf("Pull request #%d %s", pr.Number, text),
		ObservedGeneration: rr.Generation,
	})
}

// closedByUs is markClosed for a pull request Shadewright has just closed, with why it did. A close of a pull
// request that is already closed succeeds too, so the text does not claim who closed it first.
func closedByUs(rr *remediationv1alpha1.RemediationRequest, why string) {
	markClosed(rr, ReasonPullRequestClosed, "is closed: "+why)
}

// renderRefusal is a render check that did not pass: merging the change would not build exactly the planned change.
// It carries a stable reason and fixed text, never file content; the caller says what happened to the commit
// or the pull request.
type renderRefusal struct {
	reason, message string
}

func (e *renderRefusal) Error() string { return e.reason + ": " + e.message }

// checkRender proves what merging the change into the target branch would build (design 6 and 8.2, steps 4
// and 5). It is the one render for both windows in which the target branch can move under a change: between
// cutting the branch and the commit, and while the pull request waits for review. The caller has checked that
// the merge result is the tree at head with changed in place of the target file; checkRender renders that tree
// with and without the change, and the one difference must be the planned field of the target. A refusal is
// returned as a [*renderRefusal]; Git that cannot be read, and a render process that ended without an answer
// fewer than maxRenderEnds times in a row (renderEnds), are returned for a retry.
//
// The render is marked in status before the render process starts and unmarked when the check returns (#714,
// markRender and unmarkRender), so a render that takes the executor down with it is counted across restarts: at
// MaxUnfinishedRenders unfinished renders of the plan in a row it is refused with RenderNeverFinished and no
// render starts. passed, when not nil, records a pass in rr's status in the write that removes the mark, and
// returns false when rr has no place for it.
func (r *RemediationRequestReconciler) checkRender(ctx context.Context, git providers.GitProvider, head string,
	changed []byte, rr *remediationv1alpha1.RemediationRequest, passed func(*remediationv1alpha1.RemediationRequest) bool) error {
	src := rr.Status.Source
	// Defence in depth: the freshness check already supersedes a plan without a build directory, because the
	// mapping now always returns one.
	if src.BuildPath == "" {
		return &renderRefusal{reason: ReasonPlanIncomplete,
			message: "The plan does not record the directory Flux builds; the request has to be decided again"}
	}
	// Defence in depth too: the decision records the digest. A render without it could not be marked (markRender).
	if rr.Status.PlanDigest == "" {
		return &renderRefusal{reason: ReasonPlanIncomplete,
			message: "The plan does not record its digest; the request has to be decided again"}
	}
	files, err := semdiff.Load(ctx, git, head, string(src.BuildPath))
	if err == nil {
		err = r.markRender(ctx, rr)
		if err != nil {
			return err
		}
		check := semdiff.Check
		if r.renderCheck != nil {
			check = r.renderCheck
		}
		_, err = check(ctx, semdiff.Input{
			Files: files, Path: string(src.BuildPath), File: string(src.Path), Patched: changed,
			Target: rr.Spec.TargetRef, Action: rr.Spec.Action,
		})
		// The executor is still here, so the render finished, whatever its outcome. A failed write of that is
		// retried before the outcome is used or counted, so the mark never stays on a render that answered.
		onPass := passed
		if err != nil {
			onPass = nil
		}
		if uerr := r.unmarkRender(ctx, rr, onPass); uerr != nil {
			return uerr
		}
		// A render process that keeps ending without an answer is refused after a few ends in a row (ADR-0035).
		err = r.renderEnds.note(rr, err)
	}
	if rf, ok := errors.AsType[*renderRefusal](err); ok {
		return rf
	}
	if se, ok := errors.AsType[*semdiff.Error](err); ok {
		return &renderRefusal{reason: string(se.Reason),
			message: "The rendered change is not exactly the planned one (" + string(se.Reason) + ")"}
	}
	return err
}

// prOutcome is what watchPullRequest found.
type prOutcome int

const (
	// prOpen: open with Shadewright's head; the freshness check decides next.
	prOpen prOutcome = iota
	// prUnchecked: it could not be checked. recheck waits out a delay the host asked for first; otherwise it goes
	// on as for prOpen, always with the full recheck, whose error handling covers paused writes, changed settings
	// and the error backoff.
	prUnchecked
	// prStopped: the request ended (Merged, gone, head moved).
	prStopped
	// prClosed: closed without a merge. That is a rejection only if the plan is still fresh; a stale plan's
	// own close that was cut short looks the same and ends Superseded.
	prClosed
	// prConflicted: open with Shadewright's head, and the Git host reports a merge conflict. With a fresh plan
	// that is recorded, and ends the request (ChangeRequestConflict) only when seen again after a wait
	// (conflicted); a stale plan is superseded as usual.
	prConflicted
	// prMergeCommitPending: merged, and the executor waits for the Git host to report the merge commit
	// (awaitMergeCommit). Nothing else is done meanwhile: a merged change makes the plan look stale.
	prMergeCommitPending
	// prMergeUnchecked: merged, and the check of the plan at the merge (checkAtMerge) could not read what it
	// needs. Nothing is recorded and nothing else is done: the error is retried, and the full freshness check is
	// not run on a merged change, which makes the plan look stale.
	prMergeUnchecked
	// prBranchUnread: the target branch of the plan's repository changed, and the pull request could not be read
	// on the target branch the settings name now (#771) or on the recorded one (#754), or a provider for either
	// could not be opened (watchBranchChanged). Nothing else is done: the error is retried, since the full
	// freshness check would end the request without knowing whether it was merged.
	prBranchUnread
	// prOpenElsewhere: spec.cancel is set, and the request's own pull request, read by its number, is open on a base
	// other than the target branch (and, after a change of the target branch, other than the one the settings name
	// now), where a person moved it (changeElsewhere, watchStep, watchBranchChanged, #892). Only the cancel acts on it
	// (cancelOpen): the request ends Cancelled saying so, and nothing marks the pull request closed.
	prOpenElsewhere
)

// watchPullRequest looks at the recorded pull request: a merge ends the request (Merged), a pull request
// that is gone or a moved head too (NeedsIntervention); open with the head Shadewright pushed moves PRCreated
// to AwaitingApproval. On an error the caller goes on to the freshness check, which owns the handling of
// paused writes, changed settings and provider failures; the error is returned with prUnchecked, so a delay the
// host asked for is kept. Delivery after the merge is a later step (#94, #95). Unless the request ended or its
// pull request is closed, the reviewer hints are handled next, within deadlines of their own (pollHints):
// the comments only when the look found the pull request waiting for review, the answers of Pending hints and
// their timeout always. Then, on a pull request found open with the recorded head, the replies to the hints
// (replyHints) and the check run shadewright/review (reviewCheck), whose gate decides only when the look at the
// comments caught up.
func (r *RemediationRequestReconciler) watchPullRequest(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) (prOutcome, error) {
	stepCtx, cancel := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	look, err := r.watchStep(stepCtx, rr)
	mergeSeen, timedOut := errors.Is(err, errMergeCheck), stepCtx.Err() != nil
	branchUnread := errors.Is(err, errBranchChangedUnread)
	// A read of a wait of its own (the content of a retargeted merge, the check at the merge) that ran out of the
	// step's time is still that read, so its wait starts and ends as for any other failure of it.
	err = keepGitRead(ctx, stepCtx, "pull request check", err)
	cancel()
	if branchUnread {
		// boundErr may replace the error, as for a merge seen: the pull request is still unread.
		logf.FromContext(ctx).Info("Could not read the pull request of RemediationRequest after its target "+
			"branch changed", "remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error())
		return prBranchUnread, err
	}
	if mergeSeen {
		// boundErr replaces the error when the step's deadline or the reconcile's cancellation ended it; the merge
		// is still seen and not recorded, which the caller must know so it never runs the full check on it.
		return prMergeUnchecked, r.mergeSeenErr(ctx, rr, err, timedOut)
	}
	switch {
	case errors.Is(err, errStopped):
		return prStopped, nil
	case errors.Is(err, errClosed):
		return prClosed, nil
	case errors.Is(err, errOpenElsewhere):
		return prOpenElsewhere, nil
	case errors.Is(err, errConflicted):
		return prConflicted, nil
	case errors.Is(err, errMergeCommitPending):
		return prMergeCommitPending, nil
	}
	comments := commentsNone
	if ctx.Err() == nil {
		// Deadlines of their own (pollHints): a slow check of the pull request does not leave them its remainder.
		comments = r.watchHints(ctx, look, rr)
	}
	if err != nil {
		logf.FromContext(ctx).V(1).Info("Could not check the pull request of RemediationRequest",
			"remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error())
		return prUnchecked, err
	}
	if staleMark(rr) != nil || cancelRequested(rr) {
		// A stale plan whose close is pending or refused (#199): nothing but the close and the failed check run of
		// cleanUpStale (#713) is written to its pull request, so no reply and no merge gate either. The same holds
		// for a cancelled request (#289), whose close follows this look.
		return prOpen, nil
	}
	if ctx.Err() == nil {
		// The replies to the hints this look or an earlier one answered (hintreply.go), on a pull request found
		// waiting for review; within a deadline of their own.
		r.watchReplies(ctx, look, rr)
	}
	if ctx.Err() == nil {
		// Open with the recorded head: the check run on it, and the merge gate only after the comments of this
		// look are stored (reviewcheck.go). A failed Git read of it is the look's error: open, read again.
		return prOpen, r.watchReviewCheck(ctx, rr, comments)
	}
	return prOpen, nil
}

// mergeSeenErr returns err, the error of a look that saw the pull request merged and did not record the merge,
// so that it matches errMergeCheck. When the step's own deadline ended the look (timedOut) and the reconcile goes
// on, it records the start of the wait for the merge first (startMergeWait, within a deadline of its own), so a
// check that always runs out of time is bounded by the wait like any other failure (checkAtMerge). A failed write
// is joined to the error; the next look tries again.
func (r *RemediationRequestReconciler) mergeSeenErr(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	err error, timedOut bool) error {
	if !errors.Is(err, errMergeCheck) {
		err = fmt.Errorf("%w: %w", errMergeCheck, err)
	}
	pr := rr.Status.PullRequest
	if !timedOut || gitWait(rr, waitMergeCheck) != nil || ctx.Err() != nil || pr == nil || rr.Status.Source == nil {
		return err
	}
	waitCtx, cancel := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	defer cancel()
	// The look ran out of time before it returned what it read: the wait is about the recorded pull request, merged
	// into the recorded target branch, with no merge commit seen (endMergeWait records it without a receipt), unless
	// the read that ran out of time was one of the merge wait, which names the pull request it read (a moved head,
	// #889).
	cr := providers.ChangeRequest{Number: pr.Number, URL: pr.URL, HeadSHA: pr.HeadSHA, State: providers.ChangeRequestMerged,
		BaseBranch: string(rr.Status.Source.TargetBranch)}
	if e, ok := errors.AsType[*kindedReadError](err); ok && e.wait.Kind == remediationv1alpha1.GitWaitMergeCheck &&
		e.wait.PullRequest != nil {
		p := e.wait.PullRequest
		cr = providers.ChangeRequest{Number: p.Number, URL: p.URL, HeadSHA: p.HeadSHA, State: providers.ChangeRequestMerged,
			BaseBranch: string(p.BaseBranch), MergeCommit: p.MergeCommitSHA}
	}
	return errors.Join(err, r.startMergeWait(waitCtx, rr, cr, "check the plan at the merge"))
}

// rejected ends a fresh request whose pull request a person closed without a merge. It may also be
// Shadewright's own close after a render refusal whose status write was lost, so the text does not say who.
func (r *RemediationRequestReconciler) rejected(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	markClosed(rr, ReasonChangeRequestClosed, "was closed without a merge")
	return reconcile.Result{}, r.ignoreStopped(r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention,
		ReasonChangeRequestClosed, "The pull request was closed without a merge; automation stopped"))
}

// confirmWait is how long, on the executor's clock, an answer of the Git host that ends something must last
// before it is believed: a merge conflict before it ends the request (GitHub computes mergeability lazily: after
// the target branch moves, and moves back, it may answer "dirty" for a pull request that merges cleanly, until it
// computes the answer again), and a pull request missing by its number and from the listing of its branch before
// it is gone (goneLooks, #688), since both reads may come from a replica that lags.
const confirmWait = 2 * time.Minute

// confirmWaitRemaining is what is left of confirmWait since the answer was first seen, never negative.
// ok is false when the recorded time lies more than mergeCommitClockSlack after now: the clock went back and the
// wait can no longer be measured, so the caller starts it again rather than end the request on it. Inside the
// slack the wait is at most confirmWait plus the slack.
func confirmWaitRemaining(since, now time.Time) (left time.Duration, ok bool) {
	if since.After(now.Add(mergeCommitClockSlack)) {
		return 0, false
	}
	return max(since.Add(confirmWait).Sub(now), 0), true
}

// conflicted handles a fresh plan whose open pull request the Git host reports with a merge conflict (#680). A
// first report is recorded in status.pullRequest.conflictSeen, with the time on the executor's clock, the
// recorded head and the target branch head the freshness check read, and the request waits. It ends only on a
// conflicted read at least confirmWait later on the same recorded head; a clock that went back records
// the report anew. The target branch head is recorded as what the first report was about and decides nothing:
// a branch that moves more often than the wait would otherwise start the wait again without end. A failed status write is returned for a retry and decides
// nothing.
//
// At the end the pull request is left open: resolving the conflict pushes to its branch, which only a person may
// do, and a close by a person later is seen by the lock on the target (checkHeld). PullRequestOpened keeps saying
// it is open, so the request holds that lock until then.
func (r *RemediationRequestReconciler) conflicted(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	fresh freshness.Result) (reconcile.Result, error) {
	pr := rr.Status.PullRequest
	if seen := pr.ConflictSeen; seen != nil && seen.HeadSHA == pr.HeadSHA {
		left, ok := confirmWaitRemaining(seen.At.Time, r.now())
		if ok && left == 0 {
			return reconcile.Result{}, r.ignoreStopped(r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention,
				ReasonChangeRequestConflict, "The Git host reports that the pull request conflicts with the target "+
					"branch; it is left open for a person to resolve or close, and automation stopped"))
		}
		if ok {
			return reconcile.Result{RequeueAfter: max(min(left, r.pullRequestPollInterval()), stepDelay)}, nil
		}
	}
	base := rr.DeepCopy()
	pr.ConflictSeen = &remediationv1alpha1.ConflictSeen{
		At: metav1.NewTime(r.now().Truncate(time.Second)), HeadSHA: pr.HeadSHA, TargetHead: fresh.Head,
	}
	if err := r.Status().Patch(ctx, rr, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return reconcile.Result{}, fmt.Errorf("record the merge conflict of RemediationRequest %s/%s: %w",
			rr.Namespace, rr.Name, err)
	}
	logf.FromContext(ctx).Info("The Git host reports a merge conflict; waiting to see it again",
		"remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", pr.Number, "wait", confirmWait)
	return reconcile.Result{RequeueAfter: max(min(confirmWait, r.pullRequestPollInterval()), stepDelay)}, nil
}

// endLooked handles a fresh plan on what the look at its pull request found: closed without a merge (rejected)
// or open with a merge conflict (conflicted).
func (r *RemediationRequestReconciler) endLooked(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	outcome prOutcome, fresh freshness.Result) (reconcile.Result, error) {
	if outcome == prConflicted {
		return r.conflicted(ctx, rr, fresh)
	}
	return r.rejected(ctx, rr)
}

// clearConflictSeen removes status.pullRequest.conflictSeen after a look that read the pull request open with
// the recorded head and without a merge conflict (prOpen): the earlier report was not confirmed. Any other
// outcome, a failed look included, leaves it. A failed write is returned for a retry.
func (r *RemediationRequestReconciler) clearConflictSeen(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, outcome prOutcome) error {
	if outcome != prOpen || rr.Status.PullRequest == nil || rr.Status.PullRequest.ConflictSeen == nil {
		return nil
	}
	base := rr.DeepCopy()
	rr.Status.PullRequest.ConflictSeen = nil
	if err := r.Status().Patch(ctx, rr, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("clear the merge conflict of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	return nil
}

// watchStep is the look of watchPullRequest. It returns the provider to read the reviewer hints with when it
// found the pull request open with the head Shadewright pushed, after the request had reached
// AwaitingApproval; otherwise the zero hintLook.
func (r *RemediationRequestReconciler) watchStep(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	hintLook, error) {
	repo, reinstalled, err := r.planEntry(ctx, rr)
	switch {
	case errors.Is(err, errTargetBranchChanged):
		// A merge is recorded from the target branch the settings name now, when it brought the planned change
		// alone (#771), or from the recorded branch, the stale plan's (#754); anything else is as before.
		return hintLook{}, r.watchBranchChanged(ctx, rr, repo)
	case err != nil:
		return hintLook{}, err
	}
	git, host, err := r.openPlanEntry(ctx, rr, repo, reinstalled)
	if err != nil {
		return hintLook{}, err
	}
	// By its number, not its branch: the branch may carry a newer pull request (ADR-0030 section 1).
	// Unknown is returned for a retry and ends nothing; gone is confirmed (changeOf). Through a new installation
	// (#290) only a pull request with this request's marker is taken for the recorded one.
	v := r.changeOf(ctx, git, rr, changeQuery{marker: reinstalled})
	switch v.rule().watch {
	case watchRetry:
		return hintLook{}, v.err
	case watchGone:
		return hintLook{}, r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonChangeRequestGone,
			"The recorded pull request cannot be found any more ("+v.gone+"); automation stopped")
	case watchMoved:
		return hintLook{}, r.stopOpenElsewhere(ctx, rr, v.pr)
	case watchMergedElsewhere:
		return hintLook{}, r.stopMergedElsewhere(ctx, rr, v.pr)
	case watchGoOn:
	}
	// changeOf read the recorded number and answers about it only, so cr is that pull request.
	cr := v.pr
	if cr.HeadSHA != rr.Status.PullRequest.HeadSHA {
		if intent(rr) != nil {
			// The replacement's commit is expected while the intent stands (handoff.go): the replacement records
			// what happens to the pull request from here, and this request waits for it.
			switch ok, err := r.expectedHandoffHead(ctx, git, rr, cr.HeadSHA); {
			case err != nil:
				return hintLook{}, err
			case ok:
				return hintLook{}, nil
			}
		}
		return hintLook{}, r.headMoved(ctx, git, rr, cr)
	}
	switch cr.State {
	case providers.ChangeRequestMerged:
		// Every failure from here on is a merge seen and not recorded (errMergeCheck): the caller retries the look
		// and never runs the full freshness check on a merged change.
		return hintLook{}, r.recordMergeSeen(ctx, rr, cr)
	case providers.ChangeRequestClosed:
		return hintLook{}, errClosed
	}
	if !r.ReviewerHints && tookOver(rr) {
		// A head a hint steered, with hints off: never AwaitingApproval, and no hints read (afterHandoff stops
		// such a request first; this holds for any caller).
		return hintLook{}, r.hintsOffTakenOver(ctx, rr)
	}
	if cr.Conflicted {
		// The freshness check decides first: a stale plan is superseded, a fresh one waits to see the conflict
		// again (conflicted).
		return hintLook{}, errConflicted
	}
	if rr.Status.Phase != remediationv1alpha1.PhasePRCreated {
		if intent(rr) != nil {
			// Handed over: new comments are the replacement's to read, from the cursor it carries over.
			return hintLook{}, nil
		}
		// Open, with the head Shadewright pushed: reviewer hints are read next (AwaitingApproval only).
		return hintLook{git: git, host: host}, nil
	}
	r.setPhase(rr, remediationv1alpha1.PhaseAwaitingApproval)
	setWaitingForReview(rr)
	if err := r.Status().Update(ctx, rr); err != nil {
		return hintLook{}, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	return hintLook{}, nil
}

// stopOpenElsewhere is the watch's answer to the request's own pull request, cr, read by its number open on another
// base than the target branch, where a person moved it (#892). A cancelled request ends Cancelled saying so
// (errOpenElsewhere, prOpenElsewhere, cancelOpen), whether or not its target branch changed. Any other request stops
// in NeedsIntervention with ChangeRequestOnOtherBase. Nothing is written to the pull request, and the lock stays
// while it is open (lookHeld, TargetHeld). In the same step the review check of its head is failed (failStoppedCheck,
// #925): moved back to the target branch, the pull request cannot merge on a check written before the stop. A failed
// write of the check does not hold the stop: the stopped holder's next look tries it again.
func (r *RemediationRequestReconciler) stopOpenElsewhere(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest) error {
	if cancelRequested(rr) {
		return errOpenElsewhere
	}
	if err := r.failStoppedCheck(ctx, rr, ReasonChangeRequestOnOtherBase); ctx.Err() != nil {
		return err
	}
	return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonChangeRequestOnOtherBase,
		"The recorded "+openElsewhereText(cr)+"; automation stopped"+messageOpenElsewhere)
}

// stopMergedElsewhere is the watch's and the adoption's answer to the request's own pull request, cr, merged into a
// base a person moved it to, not the target branch (#892): the change did not reach the target branch, so nothing is
// recorded as merged and nothing can be verified. The request stops in NeedsIntervention with
// ChangeRequestOnOtherBase and a message that names the pull request, for a person to decide. Nothing is written to
// the pull request, and it keeps no lock: it cannot merge again.
func (r *RemediationRequestReconciler) stopMergedElsewhere(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest) error {
	return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonChangeRequestOnOtherBase,
		"The "+mergedElsewhereText(cr)+", so its change did not reach the target branch; automation stopped and "+
			"Shadewright writes nothing to it")
}

// recordMergeSeen records the merge of the recorded pull request, cr, seen merged with the head Shadewright
// pushed: without a full merge commit ID it waits for the host to report one, a bounded time (awaitMergeCommit,
// errMergeCommitPending); then the plan is checked at the merge (checkAtMerge), so a plan that became stale since
// the last full recheck is marked in the write that sets Merged. It returns errStopped once that write went
// through, and any failure wrapped in errMergeCheck with nothing recorded.
func (r *RemediationRequestReconciler) recordMergeSeen(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest) error {
	switch left, err := r.awaitMergeCommit(ctx, rr, cr); {
	case err != nil:
		return fmt.Errorf("%w: %w", errMergeCheck, err)
	case left > 0:
		return errMergeCommitPending
	}
	if err := r.checkAtMerge(ctx, rr, cr); err != nil {
		return err
	}
	recordMerge(ctx, rr, cr)
	err := r.stop(ctx, rr, remediationv1alpha1.PhaseMerged, ReasonChangeRequestMerged, "The pull request was merged")
	if err != nil && !errors.Is(err, errStopped) {
		return fmt.Errorf("%w: %w", errMergeCheck, err)
	}
	return err
}

// setWaitingForReview sets condition Approved False with reason WaitingForReview: the pull request waits for a
// human review. Only a merge sets Approved True.
func setWaitingForReview(rr *remediationv1alpha1.RemediationRequest) {
	message := "The pull request waits for a human review"
	if rr.Status.EffectiveApprovalRequired != nil && !*rr.Status.EffectiveApprovalRequired {
		// Auto-merge is not built (#20, #98): a request policy would let merge by itself waits for a person too.
		message = "The pull request waits for a human review; auto-merge is not available yet"
	}
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionApproved,
		Status:             metav1.ConditionFalse,
		Reason:             ReasonWaitingForReview,
		Message:            message,
		ObservedGeneration: rr.Generation,
	})
}

// commentForeign is the one comment Shadewright posts on its pull request when someone else pushed to it.
const commentForeign = "Someone else pushed to this branch, and it no longer holds only Shadewright's planned change. " +
	"Shadewright has stopped: this pull request is now a person's to review or close."

// commentMarker is the hidden line that ties commentForeign to the request, so a retry after a lost status
// write finds the comment instead of posting it again.
func commentMarker(rr *remediationv1alpha1.RemediationRequest) string {
	return fmt.Sprintf("<!-- shadewright comment=branch-diverged request-uid=%s -->", rr.UID)
}

// headMoved ends a request whose recorded pull request no longer has the head Shadewright pushed (#524, ADR-0028).
// When it is the same pull request and the new head may carry more than the planned change (CheckBranch is not
// one change, or the target file differs from the recorded head's), it is foreign: merged already, the request
// ends MergedWithForeignChange; open, Shadewright posts one comment that hands the pull request to a person and
// stops with BranchDiverged, leaving it open, since a person may be working on it; for a stale plan without the
// comment (errStalePlanWrite), which ends the retried close of its pull request too. Anything else, a clean move
// to the same planned change included, stops with HeadMoved as before. A read that fails is returned for a
// retry (on a merged pull request wrapped in errMergeCheck, so nothing after the look runs on the merged change,
// #879), as is a comment the kill switch held back; another failure to comment is logged and the request
// stops all the same. A comment already there (its marker) is not posted again. A failed read of the check of a
// merged head belongs to the merge wait about that head (mergeCheckFailed), so it is bounded like the check at the
// merge and ends from the wait's entry (#889).
func (r *RemediationRequestReconciler) headMoved(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest) error {
	moved := func() error {
		return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonHeadMoved,
			"The pull request's head is not the commit Shadewright pushed; automation stopped")
	}
	if cr.Number != rr.Status.PullRequest.Number {
		return moved()
	}
	switch cr.State {
	case providers.ChangeRequestMerged:
		ref, err := r.checkMerged(ctx, git, rr, cr)
		switch {
		case err != nil && gitReadFailure(err):
			// A merge seen and not recorded (#879): the look is retried, and the full freshness check never runs on
			// the merged change, which makes the plan look stale. The read is one of the merge wait about cr, the
			// merged head (#889), as a failed check at the merge is (mergeCheckFailed): at its end the merge is
			// recorded from the entry, as one whose content could not be checked (endMergeWait).
			return r.mergeCheckFailed(ctx, rr, cr, err)
		case err != nil:
			return fmt.Errorf("%w: %w", errMergeCheck, err)
		case ref != nil:
			return r.mergedForeign(ctx, rr, ref)
		}
		return moved()
	case providers.ChangeRequestClosed:
		return moved()
	}
	switch foreign, err := r.foreignHead(ctx, git, rr, cr.HeadSHA); {
	case err != nil:
		return err
	case !foreign:
		return moved()
	}
	outcome := "; a comment on the pull request hands it to a person"
	mark := commentMarker(rr)
	if err := git.CommentChangeRequest(ctx, cr.Number, mark, commentForeign+"\n\n"+mark); err != nil {
		switch {
		case errors.Is(err, errStalePlanWrite):
			// A stale plan (#199): its pull request gets no write but the close, so no comment either. The
			// request stops all the same; the pull request now holds a person's commits and is theirs to close.
			return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonBranchDiverged,
				"Someone else pushed to the pull request of a stale plan, and its branch no longer holds only the "+
					"planned change; it is left open for a person to review or close, without a comment")
		case errors.Is(err, freshness.ErrPaused) || errors.Is(err, freshness.ErrWriteBlocked):
			// The kill switch held the comment back: it is posted once writes go on.
			return err
		}
		logf.FromContext(ctx).Info("Could not comment on the pull request of RemediationRequest", "remediationRequest",
			client.ObjectKeyFromObject(rr), "pullRequest", cr.Number, "error", err.Error())
		outcome = "; commenting on the pull request failed"
	}
	return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonBranchDiverged,
		"Someone else pushed to the pull request, and its branch no longer holds only the planned change; it is "+
			"left open for a person to review or close"+outcome)
}

// foreignHead reports whether head, the new head of the recorded pull request, may carry more than the planned
// change: it is not one commit that changes only the target file on top of the target branch, or the target
// file on it is not byte for byte the file on the head Shadewright pushed and checked.
func (r *RemediationRequestReconciler) foreignHead(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, head string) (bool, error) {
	state, err := branchState(ctx, git, rr, head)
	if err != nil {
		return false, err
	}
	if state != providers.BranchOneChange {
		return true, nil
	}
	path := string(rr.Status.Source.Path)
	now, err := git.ReadFile(ctx, head, path, providers.MaxFileBytes)
	if err != nil {
		return false, freshness.WrapProvider("read the file of the pull request", err)
	}
	was, err := git.ReadFile(ctx, rr.Status.PullRequest.HeadSHA, path, providers.MaxFileBytes)
	if err != nil {
		return false, freshness.WrapProvider("read the file Shadewright pushed", err)
	}
	return !bytes.Equal(now.Data, was.Data), nil
}

// stop ends the request in phase with a condition that says why (PullRequestOpened, or Approved once a pull
// request is recorded), and returns errStopped.
func (r *RemediationRequestReconciler) stop(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	phase remediationv1alpha1.RemediationPhase, reason, message string) error {
	// The stale mark (PlanFresh False with a stale reason) stays on a Merged request: a pull request merged after
	// Shadewright knew its plan was stale, before its close or after the Git host refused it (#199), is still a
	// stale plan merged, and the outcome after the merge is capped (capStaleSuccess). Only the request ending as a
	// closed stale plan (endFreshness) replaces it. The pull request is merged or closed now, or no longer this
	// request's to close, so PullRequestNotClosed goes.
	clearNotClosed(rr)
	if rr.Status.Phase == remediationv1alpha1.PhaseExpired && rr.DeletionTimestamp.IsZero() {
		// The close of an expired request's cleanup (expiredCloseFailed) is over once it ends again or its merge is
		// recorded.
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionCleanupFailing)
	}
	r.setPhase(rr, phase)
	if phase == remediationv1alpha1.PhaseMerged {
		// The Incident's count at the merge, in the write that sets Merged with the receipt (ADR-0030 section
		// 4). A failed read leaves it absent and holds nothing back.
		r.recordCountAtMerge(ctx, rr)
	}
	// A final phase waits for nothing, also not for the kill switch; the request does not go on, so no
	// WritesResumed Event is recorded.
	resume(rr)
	status := metav1.ConditionFalse
	if phase == remediationv1alpha1.PhaseMerged {
		status = metav1.ConditionTrue
	}
	// Before a pull request is recorded the stop explains why none was opened; after, PullRequestOpened stays
	// true and the outcome of the review is the Approved condition.
	conditionType := remediationv1alpha1.ConditionPROpened
	if rr.Status.PullRequest != nil {
		conditionType = remediationv1alpha1.ConditionApproved
	}
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: rr.Generation,
	})
	r.settleRecorded(rr)
	// A wait the stop ends (the deadline's, a merge wait once the merge is recorded) goes in the same write.
	dropEnded(rr, r.now())
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	logf.FromContext(ctx).Info("Stopped RemediationRequest", "remediationRequest", client.ObjectKeyFromObject(rr),
		"phase", phase, "reason", reason)
	return errStopped
}

// expire ends a request whose pull request was not opened before its deadline: Expired with reason
// PullRequestDeadline, in one status write and with no read of the Git host (fifth decision of 2026-11-18 on #771,
// points 3 to 5 as the sixth kept them): the deadline has passed, so a Git host that fails does not hold the end back.
// A pause of the kill switch does (sixth decision of 2026-11-18 on #771): while writes are paused the request keeps
// its phase and waits (waitWhilePaused); the time paused counts against the deadline, so the first reconcile after
// the pause ends it.
//
// The cleanup is a step of its own after the stop (cleanUpExpired, through checkHeld): a pull request of its own
// opened while the write that would record it was lost is closed, and one found merged is recorded as the request's
// merge (endMerged). The target lock stays until the read of the branch after the stop finds none open that may
// deliver the change (#808). Only a failure of the Kubernetes API is returned, for the error backoff; the result asks
// for that step a second later.
func (r *RemediationRequestReconciler) expire(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	switch paused, err := r.writesPaused(ctx); {
	case err != nil:
		return reconcile.Result{}, err
	case paused:
		return r.waitWhilePaused(ctx, rr)
	}
	err := r.ignoreStopped(r.stop(ctx, rr, remediationv1alpha1.PhaseExpired, ReasonPullRequestDeadline,
		messageDeadlinePassed))
	if err != nil || !ptr.Deref(rr.Status.HoldsTarget, false) {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: stepDelay}, nil
}

// messageDeadlinePassed is the message of an expired request whose cleanup has not finished (expiredUncleaned).
const messageDeadlinePassed = "The pull request was not opened before the deadline; Shadewright looks for a pull " +
	"request it may have opened on the branch and closes it, and the target stays locked until none is found open"

// expiredUncleaned reports whether rr ended Expired at its deadline (reason PullRequestDeadline, on the condition stop
// writes: PullRequestOpened, or Approved once a pull request is recorded), so a pull request of its own may be left
// open or merged (cleanUpExpired). A refused cleanup changes the reason (PullRequestDeadlineCleanupRefused) and is
// not tried again.
func expiredUncleaned(rr *remediationv1alpha1.RemediationRequest) bool {
	if rr.Status.Phase != remediationv1alpha1.PhaseExpired {
		return false
	}
	conditionType := remediationv1alpha1.ConditionPROpened
	if rr.Status.PullRequest != nil {
		conditionType = remediationv1alpha1.ConditionApproved
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, conditionType)
	return c != nil && c.Reason == ReasonPullRequestDeadline
}

// cleanUpExpired is the cleanup of an expired request (expiredUncleaned) whose own pull request the read of the
// branch after the stop found open (v), or of one whose failing close passed its cutoff (checkHeld, one more close):
// it is closed, within CleanupTimeout, and a close that works removes condition CleanupFailing. A close the kill switch holds waits for
// the pause (waitWhilePaused), the lock and the pull request kept; a failed read is retried visibly at the pace of a
// stopped holder's looks (heldUnsettled); a close that fails for a reason worth a retry is shown and bounded
// (expiredCloseFailed); a pull request found merged meanwhile is recorded (endMerged); a
// close the Git host refuses, or one that cannot be made on this entry (the target branch changed, the repository
// is unusable), ends the cleanup with reason PullRequestDeadlineCleanupRefused and a message that asks a person to
// close it. The next look settles the lock.
func (r *RemediationRequestReconciler) cleanUpExpired(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	cleanCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	err := r.closeChangeRequest(cleanCtx, rr)
	err = keepGitRead(ctx, cleanCtx, "cleanup of the change request", err)
	cancel()
	if done, derr := r.endUnrecordedMerge(ctx, rr, err); done {
		if derr == nil || errors.Is(derr, errStopped) {
			return reconcile.Result{RequeueAfter: stepDelay}, nil
		}
		err = derr
	}
	what := ""
	switch reason, _, refused := refusal(err); {
	case err == nil:
		if cleanupFailing(rr) {
			// The close works again: the cleanup no longer fails, and the next look settles the lock.
			closeNoLongerFailing(rr)
			if uerr := r.Status().Update(ctx, rr); uerr != nil {
				return reconcile.Result{}, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace,
					rr.Name, uerr)
			}
		}
		logf.FromContext(ctx).Info("Closed the pull request of an expired RemediationRequest",
			"remediationRequest", client.ObjectKeyFromObject(rr))
		return reconcile.Result{RequeueAfter: stepDelay}, nil
	case ctx.Err() != nil || isClusterError(err):
		return reconcile.Result{}, err
	case errors.Is(err, freshness.ErrPaused):
		return r.waitWhilePaused(ctx, rr)
	case refused && isUnknownChange(err):
		what = "the Git host refused (" + reason + ") to read the pull requests of the branch, so none was closed"
	case refused:
		what = "the Git host refused (" + reason + ") to close a pull request Shadewright may have opened on the branch"
	case errors.Is(err, errTargetBranchChanged):
		what = "the repository of the plan now targets another branch, so the pull request on the recorded branch " +
			"was not closed"
	case errors.Is(err, errOpenElsewhere):
		pr := openElsewhereOf(err)
		what = "its " + openElsewhereText(pr) + ", and Shadewright does not close it there"
	case errors.Is(err, freshness.ErrRepositoryUnusable), errors.Is(err, errRepositoryGone),
		errors.Is(err, errChangeRequestNotOurs):
		what = "the repository of the plan cannot be used to close it"
	case gitReadFailure(err) && !expiredCleanupOver(rr, r.now()):
		// A failed read of the cleanup: a visible retry at the pace of a stopped holder's looks. Past the cutoff it is
		// the one more close that failed, which ends the cleanup (expiredCloseFailed).
		return r.heldUnsettled(ctx, rr, unknownChange(err), err, false)
	default:
		// A close the Git host failed to answer (a 5xx, a rate limit, a conflict, a token it does not give): retried
		// visibly and within a bound (expiredCloseFailed).
		return r.expiredCloseFailed(ctx, rr, err)
	}
	// Decided on for good: nothing is closed by the executor; the lock stays until none is found open.
	return reconcile.Result{}, r.ignoreStopped(r.stop(ctx, rr, remediationv1alpha1.PhaseExpired,
		ReasonPullRequestDeadlineCleanupRefused, "The pull request was not opened before the deadline, and "+what+
			"; if one is open, close it by hand. The target stays locked until none is found open"))
}

// messageDeadlineCloseFailing is the message of an expired request whose close of its own pull request failed and is
// tried again (expiredCloseFailed).
const messageDeadlineCloseFailing = "The pull request was not opened before the deadline; closing a pull request " +
	"Shadewright opened on the branch failed and is tried again every 10 minutes, for at most 24 hours after the " +
	"first failure (condition CleanupFailing), and the target stays locked until none is found open"

// expiredCloseFailed handles err, a close of an expired request's own pull request (cleanUpExpired) that failed in a
// way worth a retry. It is bounded as the cleanup of a deleted request is (cleanupFailingBound, on the executor's
// clock):
//   - the first failure sets condition CleanupFailing (reason CleanupFailed, its lastTransitionTime the start of the
//     bound), with one Warning Event, and rewrites the message of the deadline's condition so it says the close is
//     failing (messageDeadlineCloseFailing), in one status write; a failed write is returned for the error backoff;
//   - until the bound passes the close is tried again at the pace of a stopped holder's looks (heldRecheckInterval,
//     or a longer Retry-After), never later than the end of the bound, and never as an error for the rate limiter;
//   - once it has passed the cleanup ends with reason PullRequestDeadlineCleanupFailed and a message that asks a
//     person to close the pull request; the lock stays until the read of the branch finds none open.
//
// The condition goes when the cleanup ends or the merge is recorded (stop), or when the read after the stop finds no
// pull request of its own open (checkHeld, closeNoLongerFailing). A deletion that follows counts its own bound from the same condition.
func (r *RemediationRequestReconciler) expiredCloseFailed(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	err error) (reconcile.Result, error) {
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr))
	now := r.now()
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionCleanupFailing)
	if c == nil || c.Status != metav1.ConditionTrue {
		failing := metav1.Condition{Type: remediationv1alpha1.ConditionCleanupFailing, Status: metav1.ConditionTrue,
			Reason: ReasonCleanupFailed, Message: "The close of a pull request Shadewright opened on the branch " +
				"after the deadline failed; it is tried again, and 24 hours after the first failure the cleanup ends " +
				"and asks a person to close it", ObservedGeneration: rr.Generation}
		r.setRefused(rr, failing)
		setDeadlineMessage(rr, messageDeadlineCloseFailing)
		if uerr := r.Status().Update(ctx, rr); uerr != nil {
			return reconcile.Result{}, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace,
				rr.Name, uerr)
		}
		r.refusedEvent(rr, EventCleanupFailing, failing)
		log.Info("Could not close the pull request of an expired RemediationRequest; tried again", "error", err.Error())
		c = meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionCleanupFailing)
	}
	left := expiredCleanupLeft(c, now)
	if left <= 0 {
		log.Info("Ended the cleanup of an expired RemediationRequest: its close kept failing", "error", err.Error())
		return r.endExpiredCleanupFailed(ctx, rr)
	}
	return reconcile.Result{RequeueAfter: min(max(retryAfterOf(err), heldRecheckInterval), left)}, nil
}

// expiredCleanupOver reports whether the cleanup of an expired request (expiredUncleaned) has a close that failed
// (condition CleanupFailing True) for cleanupFailingBound, on the executor's clock (expiredCleanupLeft): decided from
// recorded state alone, so checkHeld checks it before any read of the Git host, and failing reads cannot keep the
// cleanup from ending (seventh decision of 2026-11-18 on #771, point 4). Past it checkHeld tries one more close, not
// during a pause, and the cleanup ends when that fails (ninth decision of 2026-11-18 on #771, point 2).
func expiredCleanupOver(rr *remediationv1alpha1.RemediationRequest, now time.Time) bool {
	if !cleanupFailing(rr) {
		return false
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionCleanupFailing)
	return expiredCleanupLeft(c, now) <= 0
}

// cleanupFailing reports whether rr is an expired request whose cleanup has a close that failed (expiredUncleaned,
// condition CleanupFailing True).
func cleanupFailing(rr *remediationv1alpha1.RemediationRequest) bool {
	if !expiredUncleaned(rr) {
		return false
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionCleanupFailing)
	return c != nil && c.Status == metav1.ConditionTrue
}

// expiredCleanupLeft is what is left at now of the bound of an expired request's failing close, c its condition
// CleanupFailing: cleanupFailingBound from c's lastTransitionTime (boundLeft).
func expiredCleanupLeft(c *metav1.Condition, now time.Time) time.Duration {
	return boundLeft(c, cleanupFailingBound, now)
}

// endExpiredCleanupFailed ends the cleanup of an expired request whose close kept failing for cleanupFailingBound:
// reason PullRequestDeadlineCleanupFailed, with a message that asks a person to close the pull request, in one status
// write and with no read of the Git host. The lock stays until the read of the branch finds none open; the next look
// comes a second later (lookAgainWhileHeld). A failed write is returned for the error backoff.
func (r *RemediationRequestReconciler) endExpiredCleanupFailed(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) (reconcile.Result, error) {
	return reconcile.Result{}, r.ignoreStopped(r.stop(ctx, rr, remediationv1alpha1.PhaseExpired,
		ReasonPullRequestDeadlineCleanupFailed, "The pull request was not opened before the deadline, and "+
			"closing a pull request Shadewright opened on the branch kept failing for 24 hours; if one is open, "+
			"close it by hand. The target stays locked until none is found open"))
}

// setDeadlineMessage sets message on the condition that holds reason PullRequestDeadline of an expired request
// (expiredUncleaned), in memory.
func setDeadlineMessage(rr *remediationv1alpha1.RemediationRequest, message string) {
	for _, t := range []string{remediationv1alpha1.ConditionPROpened, remediationv1alpha1.ConditionApproved} {
		if c := meta.FindStatusCondition(rr.Status.Conditions, t); c != nil && c.Reason == ReasonPullRequestDeadline {
			c.Message = message
		}
	}
}

// closeNoLongerFailing removes condition CleanupFailing of an expired request whose read after the stop found no
// pull request of its own open, and puts back the deadline's message (messageDeadlinePassed), in memory.
func closeNoLongerFailing(rr *remediationv1alpha1.RemediationRequest) {
	if rr.Status.Phase != remediationv1alpha1.PhaseExpired ||
		meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionCleanupFailing) == nil {
		return
	}
	meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionCleanupFailing)
	setDeadlineMessage(rr, messageDeadlinePassed)
}

// retry maps a step error to a requeue: paused writes wait, a host that asked for a delay gets it, the rest
// goes to the error backoff.
func (r *RemediationRequestReconciler) retry(ctx context.Context, rr *remediationv1alpha1.RemediationRequest, err error) (
	reconcile.Result, error) {
	switch {
	case errors.Is(err, freshness.ErrPaused):
		// The kill switch stopped the write, at the gate right before it or earlier in the step.
		return r.waitWhilePaused(ctx, rr)
	case errors.Is(err, freshness.ErrRepositoryUnusable):
		return reconcile.Result{}, r.ignoreStopped(r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention,
			reasonRepositoryUnusable, messageRepositoryUnusable))
	case errors.Is(err, errTargetBranchChanged):
		return reconcile.Result{}, r.ignoreStopped(r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention,
			reasonTargetBranchChanged, "The repository of the plan now targets another branch"))
	case errors.Is(err, errRepositoryGone):
		return reconcile.Result{}, r.ignoreStopped(r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention,
			reasonRepositoryUnavailable, "The repository of the plan is not configured any more"))
	}
	return r.refuseOrWait(ctx, rr, err)
}

// refuseOrWait handles a step error of recheck and retry. A final refusal of the Git host (refusal) stops
// Shadewright's writes at once instead of retrying until the deadline: a request without a recorded pull
// request ends in NeedsIntervention, while one whose pull request waits for review (PRCreated,
// AwaitingApproval) keeps its phase, says why on PlanFresh (Unknown) and keeps watching the pull request, so
// a later merge is still recorded (a close without a merge is decided once the freshness check runs again).
// Any other error goes to waitOrFail, and so does a failed read of a pull request (isUnknownChange), whatever
// its class: a read refused with 403 or 422 is retried, never taken for a refused write.
func (r *RemediationRequestReconciler) refuseOrWait(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	err error) (reconcile.Result, error) {
	if errors.Is(err, errStaleNotClosed) || errors.Is(err, errStaleCheckPending) || errors.Is(err, errStaleLockPending) {
		// The close of the stale plan's pull request is refused (#199), or it went through while its failed check
		// is still pending (#713) or its target lock is not settled through a new installation (#290): looked at,
		// and the close, the check or the look tried, again after the poll interval.
		return reconcile.Result{RequeueAfter: r.pullRequestPollInterval()}, nil
	}
	if isUnknownChange(err) {
		// A read of a pull request that failed, whatever its class (a 403 included), is never a refusal of a
		// write: it is retried, until the request's own deadline (expire) or the handoff's (retryRecovery).
		return r.waitOrFail(ctx, rr, err, requestBound(rr))
	}
	reason, message, ok := refusal(err)
	if !ok {
		return r.waitOrFail(ctx, rr, err, requestBound(rr))
	}
	if rr.Status.PullRequest == nil {
		// The refusal ends the request: nothing is retried.
		return reconcile.Result{}, r.ignoreStopped(r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, reason, message))
	}
	// The recorded pull request is looked at first on every recheck; nothing else is written to Git while
	// the refusal lasts, because the plan's freshness cannot be confirmed.
	changed := meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionPlanFresh,
		Status:             metav1.ConditionUnknown,
		Reason:             reason,
		Message:            refusedWhileWatching(reason),
		ObservedGeneration: rr.Generation,
	})
	if changed {
		if err := r.Status().Update(ctx, rr); err != nil {
			return reconcile.Result{}, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
		}
		logf.FromContext(ctx).Info("Paused writes of RemediationRequest after a refusal of the Git host",
			"remediationRequest", client.ObjectKeyFromObject(rr), "reason", reason)
	}
	// The refusal is asked again at the next full recheck; the looks until then read only the pull request.
	waited := r.waited(rr)
	if gitReadFailure(err) {
		// A refused read is a Git read retried like any other: GitReadRetry says so, besides PlanFresh.
		res, err := r.retryGitRead(ctx, rr, err, unboundedRead(noBoundRefusedWatch))
		if err != nil {
			return reconcile.Result{}, err
		}
		if res.RequeueAfter, err = r.withinWaits(ctx, rr, max(res.RequeueAfter, waited.RequeueAfter)); err != nil {
			return reconcile.Result{}, err
		}
		return res, nil
	}
	return waited, nil
}

// refusedWhileWatching is the fixed PlanFresh message of a refusal while a recorded pull request waits.
func refusedWhileWatching(reason string) string {
	return "The Git host refused the freshness check (" + reason + "); Shadewright writes nothing and keeps " +
		"watching the pull request"
}

// clearWaits removes, once the freshness check passes again, the PlanFresh condition refuseOrWait or
// reconcilingTooLong wrote and the WritesPaused condition of the kill switch (the check has just read the
// settings without a pause), in one status write.
func (r *RemediationRequestReconciler) clearWaits(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	changed := false
	if c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPlanFresh); c != nil &&
		c.Status == metav1.ConditionUnknown && (c.Reason == ReasonGitForbidden || c.Reason == ReasonGitRejected ||
		c.Reason == string(sourcemap.ReasonKustomizationReconciling)) {
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
		changed = true
	}
	wasPaused := resume(rr)
	if !changed && !wasPaused {
		return nil
	}
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	if wasPaused {
		r.resumed(ctx, rr)
	}
	return nil
}

// refusal returns the reason and fixed message of a final refusal of the Git host (providers.ErrForbidden or
// ErrRejected), so the request stops at once instead of retrying until its deadline. A failure worth retrying
// (a rate limit included) is never a refusal, and neither is the caller's cancellation or the step's own
// deadline, which boundErr has replaced by then. The message names no provider text, only the class.
func refusal(err error) (reason, message string, ok bool) {
	switch {
	case err == nil || providers.IsRetryable(err):
		return "", "", false
	case providers.IsForbidden(err):
		return ReasonGitForbidden, "The Git host refused Shadewright's request: the App lacks a permission or a rule " +
			"of the repository forbids it; automation stopped", true
	case providers.IsRejected(err):
		return ReasonGitRejected, "The Git host refused Shadewright's request as invalid; automation stopped", true
	}
	return "", "", false
}

// retryAfterOf returns the delay a host asked for (Retry-After), from a provider error or a transient error
// of the source mapping or the render check's reads; zero when there is none.
func retryAfterOf(err error) time.Duration {
	if pe, ok := errors.AsType[*freshness.ProviderError](err); ok {
		return pe.RetryAfter
	}
	if te, ok := errors.AsType[*sourcemap.TransientError](err); ok {
		return te.RetryAfter
	}
	return 0
}

// waitOrFail sends failed Git reads to retryGitRead with the caller's bound. Other failures honour the host delay
// or return to the error backoff, which does not read Retry-After; that delay ends at the bound too (at least
// stepDelay after now), so a request in PolicyChecked (requestBound) expires on time; expiring makes no branch,
// commit or pull request. A request whose cleanup of a stale plan has started (staleMark) never expires, so it
// waits as long as the host asked.
func (r *RemediationRequestReconciler) waitOrFail(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	err error, bound waitBound) (reconcile.Result, error) {
	if gitReadFailure(err) {
		return r.retryGitRead(ctx, rr, err, bound)
	}
	d := retryAfterOf(err)
	if d <= 0 {
		return reconcile.Result{}, err
	}
	now := r.now()
	if by, ok := bound.lookBy(rr, now); ok {
		// No Git wait is recorded for this failure, so the step's own end runs at its next look: a bound
		// already passed (during a long step) still gets a positive requeue, never a loop.
		d = min(d, max(by.Sub(now), stepDelay))
	}
	logf.FromContext(ctx).Info("Waiting before retrying RemediationRequest", "remediationRequest",
		client.ObjectKeyFromObject(rr), "error", err.Error(), "retryAfter", d)
	return reconcile.Result{RequeueAfter: d}, nil
}

func (r *RemediationRequestReconciler) ignoreStopped(err error) error {
	if errors.Is(err, errStopped) {
		return nil
	}
	return err
}

// finalize closes the request's pull request when the request is deleted, then removes the finalizer. It
// never touches the cluster. Settings that cannot reach the plan's repository any more do not block the
// deletion: there is nothing this executor can close there. A request that stopped (NeedsIntervention,
// Expired) may still have an open pull request of its own, so it is closed too, and so is the one of any
// request that holds the lock on its target (status.holdsTarget), whatever its phase, except a merged request
// that holds it only while its outcome is verified (heldForVerification, #719); a merged one refuses the
// close with a conflict, and one without this request's marker is left alone. With a pull request recorded, every
// open pull request of the branch that carries this request's marker is closed too, whatever its number. A 404 on
// the recorded number that the branch does not confirm, an answer about another pull request or any failed read
// keeps the finalizer for a retry; a recorded number that is not this request's while the branch lists one with
// its marker does not (closeAllOwn). While writes are paused (RemediationSettings spec.paused) the deletion waits,
// because closing is a write, and the pause starts no bound. Every other failure of the cleanup sets CleanupFailing
// and is tried again until cleanupFailingBound after the first failure; a step the Git host refuses for good
// (refusedCleanup: the token, the read, the listing or the close) also sets CleanupRefused and is tried again until
// closeRefusedGrace after the first refusal. At the end of either bound the finalizer goes, with a Warning Event and
// a log line that name the pull request (#288, cleanupFailedOnDelete).
func (r *RemediationRequestReconciler) finalize(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(rr, FinalizerName) {
		return reconcile.Result{}, nil
	}
	waits := len(rr.Status.GitWaits)
	// The waits for a read do not apply to a request being deleted (waitKind.applies, #878): removed in the same write,
	// so the status does not say it waits for a merge while the cleanup is retried.
	dropEnded(rr, r.now())
	if marked := markDeleting(rr); marked || len(rr.Status.GitWaits) != waits {
		// Before the close below: the limits read this request, and count it against the cap, until the
		// finalizer is gone (breakercounts.go).
		if err := r.Status().Update(ctx, rr); err != nil {
			return reconcile.Result{}, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
		}
	}
	closes := r.OpenRepository != nil && rr.Status.PlanRepository != nil &&
		(closeOnDelete(rr.Status.Phase) ||
			(ptr.Deref(rr.Status.HoldsTarget, false) && !heldForVerification(rr, r.VerifyOutcomes)))
	if closes {
		// A pull request handed over to a replacement is the replacement's: never closed here (ADR-0028 section
		// 4 item 8). While the replacement may still take it over, the deletion waits for it.
		keep, wait, err := r.handoffHolds(ctx, rr)
		switch {
		case err != nil:
			return reconcile.Result{}, err
		case wait:
			return reconcile.Result{RequeueAfter: stepDelay}, nil
		case keep:
			closes = false
		}
	}
	if closes && ended(rr.Status.Phase) {
		// A request that ended was not active until it was deleted, so another may have been let through
		// meanwhile: the close waits for a slot under the cap (waitForCapOnDelete).
		freeSlot, res, wait, err := r.waitForCapOnDelete(ctx, rr)
		if wait || err != nil {
			return res, err
		}
		defer freeSlot()
	}
	var left *cleanupRelease
	unreachable := false
	if closes {
		cleanCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
		err := r.closeOnDeleteOf(cleanCtx, rr)
		err = boundErr(ctx, cleanCtx, "cleanup of the change request", err)
		cancel()
		_, isRefused := refusedCleanup(err)
		switch {
		case isRefused, isUnknownChange(err):
			// A step the Git host refuses for good (#288), or a read that failed, which is never an answer, whatever
			// its class (ErrConflict included): retried within the bounds of the deletion (cleanupFailedOnDelete).
			res, release, err := r.cleanupFailedOnDelete(ctx, rr, err)
			if release == nil {
				return res, err
			}
			left = release
		case cleanupDone(err):
			// Closed, nothing reachable to close (said by leftUnreachable below, #925), the open ones closed and a
			// merged one read as merged, or not ours to close. A close refused as a conflict is not a merge until a
			// read says so: it is retried.
			unreachable = errors.Is(err, errRepositoryGone) || errors.Is(err, freshness.ErrRepositoryUnusable)
			if err := r.clearCleanupConditions(ctx, rr); err != nil {
				return reconcile.Result{}, err
			}
		case errors.Is(err, freshness.ErrPaused):
			// A pause is no failure: it holds the deletion and starts neither bound.
			return r.waitWhilePaused(ctx, rr)
		default:
			// Any other failure, whatever its class (a 5xx, a token the host does not give, a signer that cannot
			// sign): retried within the bounds of the deletion.
			res, release, err := r.cleanupFailedOnDelete(ctx, rr, err)
			if release == nil {
				return res, err
			}
			left = release
		}
	}
	// The outcomes this request's success cleared are read again before the finalizer goes; until they all are,
	// the finalizer stays and the deletion is retried.
	if err := r.restoreCleared(ctx, rr); err != nil {
		if ctx.Err() == nil && permanentClearError(err) {
			return r.restoreRefused(ctx, rr, err), nil
		}
		return reconcile.Result{}, err
	}
	// Before the patch, so a patch that finds the request gone, or a restart right after it, loses neither the Event
	// nor the log line. A failed patch is retried and reports it again: at least once, maybe twice.
	r.reportLetGo(ctx, rr, left, unreachable)
	base := rr.DeepCopy()
	controllerutil.RemoveFinalizer(rr, FinalizerName)
	if err := r.Patch(ctx, rr, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		// NotFound: the request is gone already, nothing more to say.
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	return reconcile.Result{}, nil
}

// reportLetGo reports a deleted request the finalizer lets go without a finished cleanup: left, when a bound of the
// cleanup released it (reportRelease), or unreachable, when its repository has no usable entry in the settings
// (leftUnreachable). It does nothing for a cleanup that finished.
func (r *RemediationRequestReconciler) reportLetGo(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	left *cleanupRelease, unreachable bool) {
	switch {
	case left != nil:
		r.reportRelease(ctx, rr, *left)
	case unreachable:
		r.leftUnreachable(ctx, rr)
	}
}

// cleanupDone reports whether err, from the finalizer's close (closeOnDeleteOf), lets the finalizer go: nil, or an
// answer that leaves nothing to close or nothing reachable (the plan's repository gone from the settings or unusable,
// a merge read as merged, a pull request not the request's).
func cleanupDone(err error) bool {
	return err == nil || errors.Is(err, errRepositoryGone) || errors.Is(err, freshness.ErrRepositoryUnusable) ||
		isMergedUnrecorded(err) || errors.Is(err, errChangeRequestNotOurs)
}

// closeOnDelete reports whether deleting a request in phase p closes its pull request: before merge, and
// after a stop that may have left one open.
func closeOnDelete(p remediationv1alpha1.RemediationPhase) bool {
	return beforeMerge(p) || p == remediationv1alpha1.PhaseNeedsIntervention || p == remediationv1alpha1.PhaseExpired
}

// fileState is what the file on the branch holds.
type fileState int

const (
	fileHoldsOther fileState = iota
	fileHoldsPrevious
	fileHoldsDesired
)

// planned applies the request's typed change to data. It reports fileHoldsPrevious with the changed content
// when data holds the previous value, fileHoldsDesired when data already holds the planned value, and
// fileHoldsOther when it holds neither. An error is a refusal of the file itself (it does not parse, the
// object is not in it).
func planned(data []byte, rr *remediationv1alpha1.RemediationRequest) ([]byte, fileState, error) {
	changed, err := applyAction(data, rr.Spec.TargetRef, rr.Spec.Action, false)
	if err == nil {
		return changed, fileHoldsPrevious, nil
	}
	if !errors.Is(err, patch.ErrPreviousValueDiffers) {
		return nil, fileHoldsOther, err
	}
	// Patching the planned value onto itself succeeds only when the file already holds it, and changes nothing.
	same, err := applyAction(data, rr.Spec.TargetRef, rr.Spec.Action, true)
	if err == nil && bytes.Equal(same, data) {
		return nil, fileHoldsDesired, nil
	}
	return nil, fileHoldsOther, nil
}

// notCheckable is the refusal of a merge whose file before it the typed patcher refuses (patchErr, the error of
// planned): reason MergeNotCheckable, never MergedWithForeignChange, since nothing shows the content foreign. what
// names the file; the message names the patcher's reason (a stable value, never file content).
func notCheckable(what string, patchErr error) *renderRefusal {
	reason := "the file could not be patched"
	if pe, ok := errors.AsType[*patch.Error](patchErr); ok {
		reason = "the patcher refused it (" + string(pe.Reason) + ")"
	}
	return &renderRefusal{reason: ReasonMergeNotCheckable, message: what + " cannot be checked against the plan: " +
		reason}
}

// uncountedMergeReasons are the reasons of a merge that was seen but not shown to bring the planned change alone:
// it ends NeedsIntervention with no merge receipt, is recorded as the request's merge (mergeRecorded) and, on a
// stale plan, counts for nothing (mergedForeignStale).
var uncountedMergeReasons = map[string]bool{
	ReasonMergedWithForeignChange: true, ReasonMergeContentUnreadable: true, ReasonMergeNotCheckable: true,
}

// applyAction runs the typed patcher of the action. With onto true it expects the desired value instead of the
// previous one, to tell whether the change is already there.
func applyAction(data []byte, target remediationv1alpha1.ObjectRef, a remediationv1alpha1.Action, onto bool) ([]byte, error) {
	switch {
	case a.Type == remediationv1alpha1.ActionSetContainerMemoryLimit && a.SetContainerMemoryLimit != nil:
		p := *a.SetContainerMemoryLimit
		if onto {
			p.Previous = p.Desired
		}
		return patch.SetContainerMemoryLimit(data, target, p)
	case a.Type == remediationv1alpha1.ActionAdjustKedaMaxReplicas && a.AdjustKedaMaxReplicas != nil:
		p := *a.AdjustKedaMaxReplicas
		if onto {
			p.PreviousValue = p.DesiredValue
		}
		return patch.AdjustKedaMaxReplicas(data, target, p)
	}
	return nil, patch.ErrInvalidInput
}

// patchReason returns the stable reason of a patch refusal, never text from the document.
func patchReason(err error) string {
	if pe, ok := errors.AsType[*patch.Error](err); ok {
		return string(pe.Reason)
	}
	return string(patch.ReasonInvalidInput)
}

// marker is the line in the pull request body that ties it to one request and one plan.
func marker(rr *remediationv1alpha1.RemediationRequest) string {
	return fmt.Sprintf("<!-- shadewright request-uid=%s plan-digest=%s -->", rr.UID, rr.Status.PlanDigest)
}

// hasMarker reports whether body carries this request's marker on a line of its own. A replacement that took
// the pull request over (ADR-0028 section 4) also accepts its predecessor's marker, by the predecessor's UID,
// until the Git host has rewritten the body with its own (handoff step 6); a refused rewrite leaves it there.
// The marker of a request further up the chain, left by more than one refused rewrite, needs reads of the
// chain: ownsMarker.
func hasMarker(body string, rr *remediationv1alpha1.RemediationRequest) bool {
	var from types.UID
	if inheritsMarker(rr) {
		from = types.UID(rr.Status.Handoff.From.UID)
	}
	return carriesMarker(body, marker(rr), from)
}

// inheritsMarker reports whether rr took its pull request over and the Git host has not rewritten the body with
// rr's marker (handoff step 6 pending or refused), so the body may still carry an earlier request's marker.
func inheritsMarker(rr *remediationv1alpha1.RemediationRequest) bool {
	ho := rr.Status.Handoff
	return ho != nil && ho.From != nil && ho.From.Phase == remediationv1alpha1.HandoffDone &&
		ho.From.PullRequestUpdate != remediationv1alpha1.HandoffStepDone
}

// carriesMarker reports whether body has the marker want on a line of its own, or the marker of a request in
// uids with any plan digest.
func carriesMarker(body, want string, uids ...types.UID) bool {
	for line := range strings.Lines(body) {
		line = strings.TrimRight(line, "\r\n")
		if line == want {
			return true
		}
		for _, uid := range uids {
			if uid == "" {
				continue
			}
			from := fmt.Sprintf("<!-- shadewright request-uid=%s plan-digest=", uid)
			if strings.HasPrefix(line, from) && strings.HasSuffix(line, " -->") &&
				!strings.ContainsAny(line[len(from):len(line)-len(" -->")], " <>") {
				return true
			}
		}
	}
	return false
}

// ownsMarker is hasMarker for the look-ups of a recorded pull request (the close, the lock's look-up). Beyond
// hasMarker, a replacement whose body was not rewritten also accepts the marker of an earlier request of its
// own supersede chain that the body may still carry after more than one refused rewrite (#591): it walks the
// chain from rr through status.handoff.from, each link read by name and accepted only with the UID it names,
// the same chain root as rr and an outgoing handoff to the request below it, and stops at the first request
// whose own rewrite was done (the body holds its marker, or a later one) or that has no incoming handoff (it
// opened the pull request). A request of another chain, or one the chain does not reach, is never accepted, so
// the pull request of another chain or Incident is never claimed. A link that no longer exists ends the walk,
// failing closed; a read that fails is returned for a retry, as a *clusterError.
func (r *RemediationRequestReconciler) ownsMarker(ctx context.Context, body string,
	rr *remediationv1alpha1.RemediationRequest) (bool, error) {
	if ok := hasMarker(body, rr); ok || !inheritsMarker(rr) {
		return ok, nil
	}
	root := chainRootOf(rr)
	var uids []types.UID
	cur := rr
	for range providers.MaxChainCommits {
		if !inheritsMarker(cur) {
			break
		}
		from := cur.Status.Handoff.From
		pred := &remediationv1alpha1.RemediationRequest{}
		err := r.Get(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: string(from.Name)}, pred)
		if apierrors.IsNotFound(err) {
			logf.FromContext(ctx).V(1).Info("Stopped at a missing request of the supersede chain",
				"remediationRequest", client.ObjectKeyFromObject(rr), "missing", from.Name)
			break
		}
		if err != nil {
			// A read of the Kubernetes API, not of the Git host: a cleanup through a new installation retries it
			// without saying it in PullRequestNotClosed (#812).
			return false, &clusterError{err: fmt.Errorf("get RemediationRequest %s of the supersede chain of %s: %w",
				from.Name, rr.Name, err)}
		}
		ho := pred.Status.Handoff
		if pred.UID != types.UID(from.UID) || chainRootOf(pred) != root || ho == nil || ho.To == nil ||
			ho.To.UID != remediationv1alpha1.UID(cur.UID) {
			logf.FromContext(ctx).V(1).Info("Stopped at a request that does not link to the supersede chain",
				"remediationRequest", client.ObjectKeyFromObject(rr), "link", from.Name)
			break
		}
		uids = append(uids, pred.UID)
		cur = pred
	}
	return carriesMarker(body, marker(rr), uids...), nil
}

// change describes the action in words: the field, the previous and the planned value.
func change(rr *remediationv1alpha1.RemediationRequest) (what, previous, desired string) {
	a := rr.Spec.Action
	switch {
	case a.SetContainerMemoryLimit != nil:
		return "the memory limit of container " + a.SetContainerMemoryLimit.Container,
			a.SetContainerMemoryLimit.Previous.String(), a.SetContainerMemoryLimit.Desired.String()
	case a.AdjustKedaMaxReplicas != nil:
		return "the maximum replica count", fmt.Sprint(a.AdjustKedaMaxReplicas.PreviousValue),
			fmt.Sprint(a.AdjustKedaMaxReplicas.DesiredValue)
	}
	return string(a.Type), "", ""
}

// stepSize says how large the planned step is relative to the previous value, from the request's validated
// numbers only: "an increase of 33% of the previous value". It returns "" when there is no previous value to
// compare with.
func stepSize(rr *remediationv1alpha1.RemediationRequest) string {
	var prev, desired int64
	a := rr.Spec.Action
	switch {
	case a.SetContainerMemoryLimit != nil:
		p, d := a.SetContainerMemoryLimit.Previous.DeepCopy(), a.SetContainerMemoryLimit.Desired.DeepCopy()
		prev, desired = p.Value(), d.Value()
	case a.AdjustKedaMaxReplicas != nil:
		prev, desired = int64(a.AdjustKedaMaxReplicas.PreviousValue), int64(a.AdjustKedaMaxReplicas.DesiredValue)
	}
	if prev <= 0 || desired == prev {
		return ""
	}
	kind, delta := "an increase", desired-prev
	if delta < 0 {
		kind, delta = "a decrease", -delta
	}
	// Rounded up, so a step is never shown smaller than it is.
	return fmt.Sprintf("%s of %d%% of the previous value", kind, (delta*100+prev-1)/prev)
}

// commitTitle is the commit message and pull request title. It follows Conventional Commits, as the GitOps
// repository's title check requires: lower case, no full stop. Every part is a validated name or value. A
// revert (ADR-0030 section 8) says so with the type "revert" and "back to", which both the operator's and the
// fleet's title checks accept. Neither the branch nor the marker derives from the title, so a pull request
// opened with an older title is still found (invariant 5).
func commitTitle(rr *remediationv1alpha1.RemediationRequest) string {
	what, _, desired := change(rr)
	t := rr.Spec.TargetRef
	if remediationv1alpha1.IsRevert(rr) {
		return redact.String(fmt.Sprintf("revert: set %s of %s/%s back to %s", what, t.Namespace, t.Name, desired))
	}
	return redact.String(fmt.Sprintf("fix: set %s of %s/%s to %s", what, t.Namespace, t.Name, desired))
}

// pullRequestText returns the title and the body of rr's pull request. For a revert it first reads the request rr
// reverts from the API server (revertedRequest); a failed read is returned for a retry, and no pull request is
// opened or rewritten with a body that leaves out what it reverts.
func (r *RemediationRequestReconciler) pullRequestText(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) (title, body string, err error) {
	failed, err := r.revertedRequest(ctx, rr)
	if err != nil {
		return "", "", err
	}
	return commitTitle(rr), pullRequestBody(rr, r.verifiedIncidentName(ctx, rr), failed), nil
}

// revertedRequest returns the request a revert names in spec.reverts, read from the API server (readReverted), or
// nil when rr is no revert, the request is gone or its name now belongs to another UID: the body then names it
// from spec.reverts alone. Any other failed read is an error, never taken for a missing request. The read has a
// deadline of its own, as the Incident read of verifiedIncidentName (incidentReadTimeout), so a stalled API
// server fails it with time left in the step instead of using the step's deadline up; the error is then retried
// like any other, and no pull request is opened or rewritten without the paragraph on what it reverts.
func (r *RemediationRequestReconciler) revertedRequest(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) (*remediationv1alpha1.RemediationRequest, error) {
	ref := rr.Spec.Reverts
	if ref == nil {
		return nil, nil
	}
	readCtx, cancel := context.WithTimeout(ctx, incidentReadTimeout(ctx, r.IncidentReadTimeout))
	defer cancel()
	failed, err := r.readReverted(readCtx, rr)
	switch {
	case err != nil:
		return nil, fmt.Errorf("write the pull request text of a revert: %w", err)
	case failed == nil || failed.UID != types.UID(ref.UID):
		return nil, nil
	}
	return failed, nil
}

// pullRequestBody is the evidence, scope, expected effect, risk and rollback plan, and the marker. Every value
// comes from validated fields of the request and its status; the whole body is redacted once more, so nothing
// that looks like a secret reaches the Git host. The exceptions are incident, the Incident name as
// verifiedIncidentName returned it, and the "namespace/name" of the request and of the request it reverts as
// keptRequestName returns them: when not empty, their whole occurrences are kept (redact.StringKeeping), because
// the names the collector and the analyzer derive are long hex runs the redactor takes for a token.
//
// For a revert, failed is the request spec.reverts names as revertedRequest read it (nil when it is gone or has
// another UID). The body then says what it reverts: that request, its pull request and the reason it failed, all
// from that request's status as the executor wrote it, and no model text.
func pullRequestBody(rr *remediationv1alpha1.RemediationRequest, incident string,
	failed *remediationv1alpha1.RemediationRequest) string {
	what, previous, desired := change(rr)
	t, src := rr.Spec.TargetRef, rr.Status.Source
	revert := remediationv1alpha1.IsRevert(rr)
	approval := "yes"
	if rr.Status.EffectiveApprovalRequired != nil && !*rr.Status.EffectiveApprovalRequired {
		approval = "no (policy allows automation; auto-merge is not available yet, so a person merges)"
	}
	var b strings.Builder
	if revert {
		fmt.Fprintf(&b, "Shadewright proposes to revert a failed change for incident `%s`.\n\n", rr.Spec.IncidentRef.Name)
		writeReverts(&b, rr, failed)
	} else {
		fmt.Fprintf(&b, "Shadewright proposes one typed change for incident `%s`.\n\n", rr.Spec.IncidentRef.Name)
	}
	fmt.Fprintf(&b, "**Change:** %s (`%s`) of %s `%s/%s`, from `%s` to `%s`.\n\n", what, rr.Spec.Action.Type,
		t.Kind, t.Namespace, t.Name, previous, desired)
	fmt.Fprintf(&b, "**Scope:** one value in `%s` on `%s`. Nothing else in this pull request is part of the plan.\n\n",
		src.Path, src.TargetBranch)
	fmt.Fprintf(&b, "**Evidence:** RemediationRequest `%s/%s` (UID `%s`), incident `%s`, policy `%s` "+
		"(decision `WithinApprovedBounds`), plan digest `%s`.\n\n",
		rr.Namespace, rr.Name, rr.UID, rr.Spec.IncidentRef.Name, rr.Status.PolicyRevision, rr.Status.PlanDigest)
	switch size := stepSize(rr); {
	case size == "":
	case revert:
		fmt.Fprintf(&b, "**Size:** %s, within policy `%s`: the step of the failed request, undone. No model chose "+
			"it.\n\n", size, rr.Spec.PolicyRef)
	default:
		fmt.Fprintf(&b, "**Size:** %s, within policy `%s`. The policy bounds the step; it does not say the step is "+
			"the right size. The diagnosis, with the model's unverified reason for the size, is in the analysis record "+
			"and the analyzer's log for this request's name, not in this pull request (ADR-0026): check it before "+
			"approving.\n\n", size, rr.Spec.PolicyRef)
	}
	fmt.Fprintf(&b, "**Expected effect:** once merged and delivered, %s is `%s`.\n\n", what, desired)
	b.WriteString("**Risk:** the change is bounded by the policy's limits and touches one field. " +
		"Any other push to this branch stops the automation.\n\n")
	if revert {
		fmt.Fprintf(&b, "**Rollback:** do not revert this pull request: that applies `%s` again, the value of the "+
			"failed request. To keep the current value, close this pull request without merging it.\n\n", previous)
	} else {
		b.WriteString("**Rollback:** revert this pull request; the GitOps controller applies the previous value.\n\n")
	}
	fmt.Fprintf(&b, "**Approval required:** %s.\n\n", approval)
	keep := []string{incident, keptRequestName(rr)}
	if failed != nil {
		keep = append(keep, keptRequestName(failed))
	}
	out := redact.StringKeeping(b.String(), keep...)
	// The marker is added after redaction: it holds only the UID and the digest, and must stay exact.
	return out + marker(rr) + "\n"
}

// writeReverts writes the paragraph of a revert's body that names the request it reverts: from failed, the
// request as read back with the UID in spec.reverts, its pull request and the reason of its Verified condition;
// without it, only the name and UID in spec.reverts and that the rest is not shown.
func writeReverts(b *strings.Builder, rr, failed *remediationv1alpha1.RemediationRequest) {
	ref := rr.Spec.Reverts
	fmt.Fprintf(b, "**Reverts:** RemediationRequest `%s/%s` (UID `%s`)", rr.Namespace, ref.Name, ref.UID)
	if failed == nil {
		b.WriteString(". It is no longer there with that UID, so its pull request and the reason it failed are not " +
			"shown.\n\n")
		return
	}
	if pr := failed.Status.PullRequest; pr != nil && pr.Number > 0 {
		// The number only: the code host links it. status.pullRequest.url is status text that is checked against
		// nothing here, so it does not go into a body a reviewer reads.
		fmt.Fprintf(b, ", pull request #%d", pr.Number)
	} else {
		b.WriteString(", no pull request recorded")
	}
	verified := meta.FindStatusCondition(failed.Status.Conditions, remediationv1alpha1.ConditionVerified)
	if failed.Status.Phase == remediationv1alpha1.PhaseFailed && verified != nil &&
		verified.Status == metav1.ConditionFalse && verified.Reason != "" {
		fmt.Fprintf(b, ". It ended `Failed` with the reason `%s`", verified.Reason)
	} else {
		b.WriteString(". Its status no longer shows why it failed")
	}
	b.WriteString(". This pull request sets the value it changed back.\n\n")
}

// keptRequestName returns the request's "namespace/name", as the body shows it, when the body may show it
// unredacted, and "" otherwise. metadata.name is chosen by whoever created the request, so its shape proves
// nothing: a hex secret could be written as "remediation-<secret>" or "revert-<secret>". The name is kept only
// when it is the name the analyzer derives from the request's own spec as the API server stored it (the spec is
// immutable): remediationv1alpha1.RevertName of spec.reverts.uid for a revert, else
// remediationv1alpha1.RequestName of spec.incidentRef.uid and the plan. Either is a hash nobody can steer to a
// chosen value. The namespace is kept only when the redactor leaves it alone, so keeping the pair never lets a
// token-like namespace through. Anything else gives "" and so a redacted name.
func keptRequestName(rr *remediationv1alpha1.RemediationRequest) string {
	if redact.String(rr.Namespace) != rr.Namespace {
		return ""
	}
	if ref := rr.Spec.Reverts; ref != nil {
		if ref.UID == "" || string(remediationv1alpha1.RevertName(ref.UID)) != rr.Name {
			return ""
		}
		return rr.Namespace + "/" + rr.Name
	}
	if rr.Spec.IncidentRef.UID == "" {
		return ""
	}
	if name, err := remediationv1alpha1.RequestName(rr.Spec); err != nil || string(name) != rr.Name {
		return ""
	}
	return rr.Namespace + "/" + rr.Name
}

// verifiedIncidentName returns the name of the request's Incident when the body may show it unredacted, and ""
// otherwise. spec.incidentRef is written by whoever created the request, so the name alone proves nothing: a
// hex secret could be written as "incident-<secret>". The name is kept only when the executor reads an
// Incident of that name in the request's namespace from the API server (never the cache, so get is enough),
// its UID is the one in incidentRef, and the name is the one the analyzer derives from the Incident's
// fingerprint and occurrence number (remediationv1alpha1.IncidentName: "incident-<64 hex>", with "-<n>" for
// occurrence n >= 2; any other suffix is refused before the read). Any failure to read it, including a missing permission or a read slower than
// IncidentReadTimeout, gives "" and so a redacted name: the body is only less readable, never blocked. The
// read has a deadline of its own, shorter than the step's, so a stalled API server leaves the step the time to
// open the pull request; a cancelled step still cancels the read, and the open then fails as it would have.
func (r *RemediationRequestReconciler) verifiedIncidentName(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) string {
	ref := rr.Spec.IncidentRef
	if _, _, ok := remediationv1alpha1.ParseIncidentName(ref.Name); !ok {
		return ""
	}
	readCtx, cancel := context.WithTimeout(ctx, incidentReadTimeout(ctx, r.IncidentReadTimeout))
	defer cancel()
	var inc remediationv1alpha1.Incident
	if err := r.reader().Get(readCtx, client.ObjectKey{Namespace: rr.Namespace, Name: ref.Name}, &inc); err != nil {
		if !apierrors.IsNotFound(err) {
			reason := apierrors.ReasonForError(err)
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				// The read's own deadline, not the step's: the error itself carries no API reason. Any other
				// error, even one that came as the deadline passed, keeps its own.
				reason = metav1.StatusReasonTimeout
			}
			// Logged as an error with the reason only: the API error repeats the name, which may be a secret
			// written as "incident-<secret>". The name stays redacted (fail closed).
			logf.FromContext(ctx).Error(nil, "Failed to read the Incident of RemediationRequest, so its name was redacted",
				"remediationRequest", client.ObjectKeyFromObject(rr), "reason", reason)
		}
		return ""
	}
	if ref.UID == "" || types.UID(ref.UID) != inc.UID || !inc.HasDerivedName() {
		return ""
	}
	return inc.Name
}

// incidentReadTimeout is the deadline of the Incident read: configured (zero means [DefaultIncidentReadTimeout]),
// and never more than half the time the step has left, so even a short WriteTimeout leaves the open its time.
func incidentReadTimeout(step context.Context, configured time.Duration) time.Duration {
	d := configured
	if d <= 0 {
		d = DefaultIncidentReadTimeout
	}
	if deadline, ok := step.Deadline(); ok {
		d = min(d, time.Until(deadline)/2)
	}
	return d
}
