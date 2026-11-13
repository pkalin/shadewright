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
	// ReasonPatchRefused: the typed patcher refused the file on the branch, or the target file at the head of
	// the target branch while the pull request waits.
	ReasonPatchRefused = "PatchRefused"
	// ReasonPullRequestDeadline: the pull request was not opened before status.deadline.
	ReasonPullRequestDeadline = "PullRequestDeadline"
	// ReasonPullRequestDeadlineCleanupRefused: the pull request was not opened before status.deadline, and the
	// Git host refused to find or close one Shadewright may have opened on the branch (its status write lost).
	// The request keeps the lock on its target until a look-up finds none of its own open (targetlock.go).
	ReasonPullRequestDeadlineCleanupRefused = "PullRequestDeadlineCleanupRefused"
	// ReasonChangeRequestGone: the recorded pull request cannot be found any more.
	ReasonChangeRequestGone = "ChangeRequestGone"
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

// openPlanRepositoryOf is openPlanRepository that also returns the type of the provider that hosts it.
func (r *RemediationRequestReconciler) openPlanRepositoryOf(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) (providers.GitProvider, remediationv1alpha1.GitProviderType, error) {
	settings := &remediationv1alpha1.RemediationSettings{}
	switch err := r.getLive(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, settings); {
	case apierrors.IsNotFound(err):
		return nil, "", errRepositoryGone
	case err != nil:
		return nil, "", fmt.Errorf("get RemediationSettings %s: %w", remediationv1alpha1.SettingsName, err)
	}
	if freshness.Paused(settings) {
		return nil, "", freshness.ErrPaused
	}
	repo, ok := freshness.FindRepository(settings, rr.Status.PlanRepository)
	if !ok {
		return nil, "", errRepositoryGone
	}
	if src := rr.Status.Source; src == nil || repo.TargetBranch != src.TargetBranch {
		return nil, "", errTargetBranchChanged
	}
	git, err := r.openRepository(ctx, repo)
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
// cleanup (staleMark) nor WritesPaused, which resumeIfAllowed removes with the WritesResumed Event at the first
// reconcile after the pause lifts. A PlanFresh Unknown of a refusal is left to the next full recheck, as before
// the looks: asking a host that refused again every look would only cost calls.
func lookOnly(rr *remediationv1alpha1.RemediationRequest) bool {
	if p := rr.Status.Phase; p != remediationv1alpha1.PhasePRCreated && p != remediationv1alpha1.PhaseAwaitingApproval {
		return false
	}
	return staleMark(rr) == nil &&
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
	if err := r.checkRender(ctx, git, fresh.Head, file.Data, rr); err != nil {
		return err
	}
	rr.Status.PullRequest.RenderedTargetHead = fresh.Head
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	return nil
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
	err = boundErr(ctx, cleanCtx, "cleanup of the change request", err)
	cancel()
	outcome := "; the pull request was closed"
	switch {
	case err == nil:
		closedByUs(rr, "the change no longer renders as planned ("+ref.reason+")")
	case errors.Is(err, freshness.ErrRepositoryUnusable), errors.Is(err, errTargetBranchChanged),
		errors.Is(err, errRepositoryGone):
		outcome = "; the settings no longer reach its repository, so the pull request was not closed"
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
	// of its own is adopted, else a merged one, else a closed one. With none of its own, a pull request of the
	// branch without its marker stops the request; with none at all the branch steps go on. Unknown is retried.
	switch v := r.changeOf(ctx, git, rr, changeQuery{}); adoptRule[v.state] {
	case adoptRetry:
		return reconcile.Result{}, v.err
	case adoptIt:
		return reconcile.Result{}, r.adoptChangeRequest(ctx, git, rr, v.pr, fresh.File)
	case adoptNone:
		if v.foreign {
			return reconcile.Result{}, r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention,
				ReasonForeignChangeRequest, "A pull request for the branch exists without this request's marker; "+
					"it was not touched")
		}
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
		if err := r.checkRender(ctx, git, fresh.Head, changed, rr); err != nil {
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

	cr, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{
		HeadBranch: branch,
		BaseBranch: string(src.TargetBranch),
		Title:      commitTitle(rr),
		Body:       pullRequestBody(rr, r.verifiedIncidentName(ctx, rr)),
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

// adoptOutcome is what pullRequestStep does per verdict on the pull requests of the branch.
type adoptOutcome int

const (
	adoptRetry adoptOutcome = iota
	adoptIt
	adoptNone
)

// adoptRule is pullRequestStep's rule per verdict: open, merged and closed adopt the pull request (record it, end
// Merged, or stop as closed), gone goes on to the branch steps, unknown is retried.
var adoptRule = map[changeState]adoptOutcome{
	changeOpen: adoptIt, changeMerged: adoptIt, changeClosed: adoptIt, changeGone: adoptNone, changeUnknown: adoptRetry,
}

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
	// recordNumber removed a mark in memory only: changeOf reads the stored one (markRemoved).
	read := changeQuery{number: cr.Number, markRemoved: before.GoneSeen != nil}
	if v := r.changeOf(cleanCtx, git, rr, read); v.state == changeMerged || v.state == changeClosed {
		if ok, err := ended(v.pr); ok {
			return err
		}
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
		if v.state == changeMerged || v.state == changeClosed {
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
// returned for a retry, with the status unchanged.
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
	if ref != nil {
		return r.mergedForeign(ctx, rr, ref)
	}
	markClosed(rr, ReasonChangeRequestMerged, "was merged")
	recordMerge(ctx, rr, cr)
	return r.stop(ctx, rr, remediationv1alpha1.PhaseMerged, ReasonChangeRequestMerged, "The pull request was merged")
}

// checkMerged checks the head of a merged pull request (#524) against its own parent, the commit of the target
// branch it was made on, not against the target branch now, which the merge moved: the head is one commit that
// changes only the target file on top of a commit of the target branch from before the merge (CheckMerged), and
// the target file there is byte for byte the planned change of the target file at that parent. A merge commit
// that took the head into the target branch so does not change the answer. A head that fails is returned as a
// [*renderRefusal]; a read that fails as an error.
func (r *RemediationRequestReconciler) checkMerged(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest) (*renderRefusal, error) {
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
	switch v := r.changeOf(ctx, git, rr, changeQuery{number: tried.number, marker: true}); v.state {
	case changeMerged:
		return r.endMerged(ctx, git, rr, v.pr)
	case changeUnknown:
		return errors.Join(closeErr, v.err)
	}
	return closeErr
}

// mergedForeign ends a request whose recorded pull request a person merged although its head failed a check
// before Shadewright could close it (#524): NeedsIntervention with MergedWithForeignChange and a message that
// names the pull request, which is recorded as merged, so the lock on the target is released and the merged
// change is reviewed by a person. Shadewright cannot stop a human merge; it reports it.
func (r *RemediationRequestReconciler) mergedForeign(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	ref *renderRefusal) error {
	markClosed(rr, ReasonChangeRequestMerged, "was merged although it failed a check")
	return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonMergedWithForeignChange,
		fmt.Sprintf("Pull request #%d was merged although it failed a check (%s): "+
			"%s. Review the merged change", rr.Status.PullRequest.Number, ref.reason, ref.message))
}

// recordPullRequest writes PRCreated with the pull request in one status write.
func (r *RemediationRequestReconciler) recordPullRequest(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest) error {
	r.setPhase(rr, remediationv1alpha1.PhasePRCreated)
	recordNumber(rr, remediationv1alpha1.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA})
	rr.Status.Deadline = nil
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

// recordNumber records pr as the request's pull request, in the status write the caller makes, and removes
// status.goneSeen in the same write (#688): a pull request of this request was just found, so an older look that
// saw a number missing confirms nothing any more, and a later 404 waits again.
func recordNumber(rr *remediationv1alpha1.RemediationRequest, pr remediationv1alpha1.PullRequestStatus) {
	rr.Status.PullRequest = &pr
	rr.Status.GoneSeen = nil
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
// returned as a [*renderRefusal]; Git that cannot be read is returned for a retry.
func (r *RemediationRequestReconciler) checkRender(ctx context.Context, git providers.GitProvider, head string,
	changed []byte, rr *remediationv1alpha1.RemediationRequest) error {
	src := rr.Status.Source
	// Defence in depth: the freshness check already supersedes a plan without a build directory, because the
	// mapping now always returns one.
	if src.BuildPath == "" {
		return &renderRefusal{reason: ReasonPlanIncomplete,
			message: "The plan does not record the directory Flux builds; the request has to be decided again"}
	}
	files, err := semdiff.Load(ctx, git, head, string(src.BuildPath))
	if err == nil {
		_, err = semdiff.Check(ctx, semdiff.Input{
			Files: files, Path: string(src.BuildPath), File: string(src.Path), Patched: changed,
			Target: rr.Spec.TargetRef, Action: rr.Spec.Action,
		})
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
	err = boundErr(ctx, stepCtx, "pull request check", err)
	cancel()
	switch {
	case errors.Is(err, errStopped):
		return prStopped, nil
	case errors.Is(err, errClosed):
		return prClosed, nil
	case errors.Is(err, errConflicted):
		return prConflicted, nil
	case errors.Is(err, errMergeCommitPending):
		return prMergeCommitPending, nil
	}
	caughtUp := false
	if ctx.Err() == nil {
		// Deadlines of their own (pollHints): a slow check of the pull request does not leave them its remainder.
		caughtUp = r.watchHints(ctx, look, rr)
	}
	if err != nil {
		logf.FromContext(ctx).V(1).Info("Could not check the pull request of RemediationRequest",
			"remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error())
		return prUnchecked, err
	}
	if ctx.Err() == nil {
		// The replies to the hints this look or an earlier one answered (hintreply.go), on a pull request found
		// waiting for review; within a deadline of their own.
		r.watchReplies(ctx, look, rr)
	}
	if ctx.Err() == nil {
		// Open with the recorded head: the check run on it, and the merge gate only after the comments of this
		// look are stored (reviewcheck.go).
		r.watchReviewCheck(ctx, rr, caughtUp)
	}
	return prOpen, nil
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
// it is gone (status.goneSeen, #688), since both reads may come from a replica that lags.
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
	git, host, err := r.openPlanRepositoryOf(ctx, rr)
	if err != nil {
		return hintLook{}, err
	}
	// By its number, not its branch: the branch may carry a newer pull request (ADR-0030 section 1).
	// Unknown is returned for a retry and ends nothing; gone is confirmed (changeOf).
	v := r.changeOf(ctx, git, rr, changeQuery{})
	switch v.state {
	case changeUnknown:
		return hintLook{}, v.err
	case changeGone:
		return hintLook{}, r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonChangeRequestGone,
			"The recorded pull request cannot be found any more ("+v.gone+"); automation stopped")
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
		// Without a full merge commit ID it waits for the host to report one, a bounded time (awaitMergeCommit).
		switch left, err := r.awaitMergeCommit(ctx, rr, cr); {
		case err != nil:
			return hintLook{}, err
		case left > 0:
			return hintLook{}, errMergeCommitPending
		}
		recordMerge(ctx, rr, cr)
		return hintLook{}, r.stop(ctx, rr, remediationv1alpha1.PhaseMerged, ReasonChangeRequestMerged,
			"The pull request was merged")
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
// stops with BranchDiverged, leaving it open, since a person may be working on it. Anything else, a clean move
// to the same planned change included, stops with HeadMoved as before. A read that fails is returned for a
// retry, as is a comment the kill switch held back; another failure to comment is logged and the request
// stops all the same. A comment already there (its marker) is not posted again.
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
		case err != nil:
			return err
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
		if errors.Is(err, freshness.ErrPaused) || errors.Is(err, freshness.ErrWriteBlocked) {
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
	if phase == remediationv1alpha1.PhaseMerged && staleMark(rr) != nil {
		// The cleanup of a stale plan started, but its pull request merged before the close: the change
		// landed, so the plan's freshness no longer says anything and the mark is not left behind.
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
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
	settleLock(rr)
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	logf.FromContext(ctx).Info("Stopped RemediationRequest", "remediationRequest", client.ObjectKeyFromObject(rr),
		"phase", phase, "reason", reason)
	return errStopped
}

// expire ends a request whose pull request was not opened before its deadline. A pull request opened in the
// meantime, whose status write failed, is closed first.
func (r *RemediationRequestReconciler) expire(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	cleanCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	err := r.closeChangeRequest(cleanCtx, rr)
	err = boundErr(ctx, cleanCtx, "cleanup of the change request", err)
	cancel()
	if done, err := r.endUnrecordedMerge(ctx, rr, err); done {
		// Its own pull request was merged before any status recorded it: it ends Merged (or
		// MergedWithForeignChange), never Expired, as for a stale plan's cleanup (cleanUpStale).
		if err != nil && !errors.Is(err, errStopped) {
			return r.retry(ctx, rr, err)
		}
		return reconcile.Result{}, nil
	}
	if reason, _, ok := refusal(err); ok {
		// The deadline has passed either way; waiting for a permission or a rule to change would only keep
		// the request open longer. The message says that a pull request of its own may be left open, and
		// whether the host refused the read of the pull requests (an unknown verdict, isUnknownChange) or the
		// close. The lock on the target stays: the status rule keeps it (prMayBeOpen), until checkHeld's read of
		// the branch finds none open that may deliver the change.
		what := "to close a pull request Shadewright may have opened on the branch"
		if isUnknownChange(err) {
			what = "to read the pull requests of the branch, so none was closed"
		}
		return reconcile.Result{}, r.ignoreStopped(r.stop(ctx, rr, remediationv1alpha1.PhaseExpired,
			ReasonPullRequestDeadlineCleanupRefused, "The pull request was not opened before the deadline, and the "+
				"Git host refused ("+reason+") "+what+"; if one is open, close it by hand. The target stays "+
				"locked until none is found open"))
	}
	if err != nil && !errors.Is(err, errRepositoryGone) && !errors.Is(err, errTargetBranchChanged) &&
		!errors.Is(err, freshness.ErrRepositoryUnusable) && !errors.Is(err, errChangeRequestNotOurs) {
		return r.retry(ctx, rr, err)
	}
	// The lock is not released here, even when the cleanup closed every pull request of the branch with this
	// request's marker: one without the marker may be open on the branch and still deliver the change. The read
	// of the branch after the stop (checkHeld, through openHolding) releases it. A repository that left the
	// settings allows no such read: condition PullRequestUnreadable says so in the same status write, and the
	// lock is kept until it is configured again or the request is deleted.
	var gone, was *metav1.Condition
	if errors.Is(err, errRepositoryGone) {
		gone = repositoryGoneRead(rr)
		if c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPullRequestUnreadable); c != nil {
			was = c.DeepCopy()
		}
		putUnreadable(&rr.Status.Conditions, gone)
	}
	err = r.stop(ctx, rr, remediationv1alpha1.PhaseExpired, ReasonPullRequestDeadline,
		"The pull request was not opened before the deadline")
	if gone != nil && errors.Is(err, errStopped) {
		r.unreadableEvent(rr, was, gone)
	}
	return reconcile.Result{}, r.ignoreStopped(err)
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
	if isUnknownChange(err) {
		// A read of a pull request that failed, whatever its class (a 403 included), is never a refusal of a
		// write: it is retried, until the request's own deadline (expire) or the handoff's (retryRecovery).
		return r.waitOrFail(ctx, rr, err)
	}
	reason, message, ok := refusal(err)
	if !ok {
		return r.waitOrFail(ctx, rr, err)
	}
	if rr.Status.PullRequest == nil {
		return reconcile.Result{}, r.ignoreStopped(r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention,
			reason, message))
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
	return r.waited(rr), nil
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

// waitOrFail requeues after the delay a host asked for, with a log line saying why, or returns err to the
// error backoff, which does not read Retry-After. For a request in PolicyChecked the delay ends at
// status.deadline at the latest, so the request expires on time; expiring makes no branch, commit or pull
// request. A request whose cleanup of a stale plan has started (staleMark) never expires, so it waits as long
// as the host asked.
func (r *RemediationRequestReconciler) waitOrFail(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	err error) (reconcile.Result, error) {
	d := retryAfterOf(err)
	if d <= 0 {
		return reconcile.Result{}, err
	}
	if rr.Status.Phase == remediationv1alpha1.PhasePolicyChecked && rr.Status.Deadline != nil && staleMark(rr) == nil {
		// A deadline already passed (during a long step) still needs a positive requeue to reach expire.
		d = min(d, max(rr.Status.Deadline.Sub(r.now()), stepDelay))
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
// request that holds the lock on its target (status.holdsTarget), whatever its phase; a merged one refuses the
// close with a conflict, and one without this request's marker is left alone. With a pull request recorded, every
// open pull request of the branch that carries this request's marker is closed too, whatever its number. A 404 on
// the recorded number that the branch does not confirm, an answer about another pull request or any failed read
// keeps the finalizer for a retry; a recorded number that is not this request's while the branch lists one with
// its marker does not (closeAllOwn). While writes are paused the deletion waits, because closing is a write.
func (r *RemediationRequestReconciler) finalize(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(rr, FinalizerName) {
		return reconcile.Result{}, nil
	}
	if markDeleting(rr) {
		// Before the close below: the limits read this request, and count it against the cap, until the
		// finalizer is gone (breakercounts.go).
		if err := r.Status().Update(ctx, rr); err != nil {
			return reconcile.Result{}, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
		}
	}
	closes := r.OpenRepository != nil && rr.Status.PlanRepository != nil &&
		(closeOnDelete(rr.Status.Phase) || ptr.Deref(rr.Status.HoldsTarget, false))
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
	if closes {
		cleanCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
		err := r.closeOnDeleteOf(cleanCtx, rr)
		err = boundErr(ctx, cleanCtx, "cleanup of the change request", err)
		cancel()
		switch {
		case isUnknownChange(err):
			// A read that failed is never an answer, whatever its class (ErrConflict included): retried.
			return r.waitOrFail(ctx, rr, err)
		case err == nil, errors.Is(err, errRepositoryGone), errors.Is(err, errTargetBranchChanged),
			errors.Is(err, freshness.ErrRepositoryUnusable), isMergedUnrecorded(err),
			errors.Is(err, errChangeRequestNotOurs):
			// Closed, nothing reachable to close, the open ones closed and a merged one read as merged, or not
			// ours to close. A close refused as a conflict is not a merge until a read says so: it is retried.
		case errors.Is(err, freshness.ErrPaused):
			return r.waitWhilePaused(ctx, rr)
		default:
			return r.waitOrFail(ctx, rr, err)
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
	base := rr.DeepCopy()
	controllerutil.RemoveFinalizer(rr, FinalizerName)
	if err := r.Patch(ctx, rr, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	return reconcile.Result{}, nil
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
// failing closed; a read that fails is returned for a retry.
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
			return false, fmt.Errorf("get RemediationRequest %s of the supersede chain of %s: %w", from.Name, rr.Name, err)
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
// repository's title check requires: lower case, no full stop. Every part is a validated name or value.
func commitTitle(rr *remediationv1alpha1.RemediationRequest) string {
	what, _, desired := change(rr)
	t := rr.Spec.TargetRef
	return redact.String(fmt.Sprintf("fix: set %s of %s/%s to %s", what, t.Namespace, t.Name, desired))
}

// pullRequestBody is the evidence, scope, expected effect, risk and rollback plan, and the marker. Every value
// comes from validated fields of the request and its status; the whole body is redacted once more, so nothing
// that looks like a secret reaches the Git host. The exceptions are incident, the Incident name as
// verifiedIncidentName returned it, and the request's own "namespace/name" as keptRequestName returns it: when
// not empty, their whole occurrences are kept (redact.StringKeeping), because the names the collector and the
// analyzer derive are long hex runs the redactor takes for a token.
func pullRequestBody(rr *remediationv1alpha1.RemediationRequest, incident string) string {
	what, previous, desired := change(rr)
	t, src := rr.Spec.TargetRef, rr.Status.Source
	approval := "yes"
	if rr.Status.EffectiveApprovalRequired != nil && !*rr.Status.EffectiveApprovalRequired {
		approval = "no (policy allows automation; auto-merge is not available yet, so a person merges)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Shadewright proposes one typed change for incident `%s`.\n\n", rr.Spec.IncidentRef.Name)
	fmt.Fprintf(&b, "**Change:** %s (`%s`) of %s `%s/%s`, from `%s` to `%s`.\n\n", what, rr.Spec.Action.Type,
		t.Kind, t.Namespace, t.Name, previous, desired)
	fmt.Fprintf(&b, "**Scope:** one value in `%s` on `%s`. Nothing else in this pull request is part of the plan.\n\n",
		src.Path, src.TargetBranch)
	fmt.Fprintf(&b, "**Evidence:** RemediationRequest `%s/%s` (UID `%s`), incident `%s`, policy `%s` "+
		"(decision `WithinApprovedBounds`), plan digest `%s`.\n\n",
		rr.Namespace, rr.Name, rr.UID, rr.Spec.IncidentRef.Name, rr.Status.PolicyRevision, rr.Status.PlanDigest)
	if size := stepSize(rr); size != "" {
		fmt.Fprintf(&b, "**Size:** %s, within policy `%s`. The policy bounds the step; it does not say the step is "+
			"the right size. The diagnosis, with the model's unverified reason for the size, is in the analysis record "+
			"and the analyzer's log for this request's name, not in this pull request (ADR-0026): check it before "+
			"approving.\n\n", size, rr.Spec.PolicyRef)
	}
	fmt.Fprintf(&b, "**Expected effect:** once merged and delivered, %s is `%s`.\n\n", what, desired)
	b.WriteString("**Risk:** the change is bounded by the policy's limits and touches one field. " +
		"Any other push to this branch stops the automation.\n\n")
	b.WriteString("**Rollback:** revert this pull request; the GitOps controller applies the previous value.\n\n")
	fmt.Fprintf(&b, "**Approval required:** %s.\n\n", approval)
	out := redact.StringKeeping(b.String(), incident, keptRequestName(rr))
	// The marker is added after redaction: it holds only the UID and the digest, and must stay exact.
	return out + marker(rr) + "\n"
}

// keptRequestName returns the request's "namespace/name", as the body shows it, when the body may show it
// unredacted, and "" otherwise. metadata.name is chosen by whoever created the request, so its shape proves
// nothing: a hex secret could be written as "remediation-<secret>". The name is kept only when it equals
// remediationv1alpha1.RequestName of the request's own spec as the API server stored it (spec.incidentRef.uid
// and the plan; the spec is immutable), the name the analyzer derives: a hash nobody can steer to a chosen
// value. The namespace is kept only when the redactor leaves it alone, so keeping the pair never lets a
// token-like namespace through. Anything else gives "" and so a redacted name.
func keptRequestName(rr *remediationv1alpha1.RemediationRequest) string {
	if rr.Spec.IncidentRef.UID == "" || redact.String(rr.Namespace) != rr.Namespace {
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
