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

// stepDelay is how soon the next step runs after a single write of the step.
const stepDelay = time.Second

// Reasons of the pull request step.
const (
	// ReasonPullRequestOpened: the pull request is open and its head is the commit Shadewright made.
	ReasonPullRequestOpened = "PullRequestOpened"
	// ReasonWaitingForReview: the pull request waits for a human review.
	ReasonWaitingForReview = "WaitingForReview"
	// ReasonHeadMoved: someone else pushed to the branch. Automation stops; the plan no longer describes the
	// pull request.
	ReasonHeadMoved = "HeadMoved"
	// ReasonBranchDiverged: the branch holds neither the previous nor the planned value of the file.
	ReasonBranchDiverged = "BranchDiverged"
	// ReasonForeignChangeRequest: a pull request for the branch exists without this request's marker.
	ReasonForeignChangeRequest = "ForeignChangeRequest"
	// ReasonChangeRequestClosed: the pull request was closed without merging before the request recorded it.
	ReasonChangeRequestClosed = "ChangeRequestClosed"
	// ReasonChangeRequestMerged: the pull request was merged.
	ReasonChangeRequestMerged = "ChangeRequestMerged"
	// ReasonPatchRefused: the typed patcher refused the file on the branch, or the target file at the head of
	// the target branch while the pull request waits.
	ReasonPatchRefused = "PatchRefused"
	// ReasonPullRequestDeadline: the pull request was not opened before status.deadline.
	ReasonPullRequestDeadline = "PullRequestDeadline"
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

	reasonTargetBranchChanged   = "TargetBranchChanged"
	reasonRepositoryUnavailable = "RepositoryUnavailable"
)

// errStopped means the step ended the request in a final phase; the caller returns without requeue.
var errStopped = errors.New("the pull request step stopped the request")

// errClosed means the recorded pull request is closed without a merge; see prClosed.
var errClosed = errors.New("the pull request is closed")

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

// openPlanRepository opens the repository the plan was made for, and nothing else. It returns the provider
// and the settings entry, or the same errors as closeChangeRequest.
func (r *RemediationRequestReconciler) openPlanRepository(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	providers.GitProvider, remediationv1alpha1.GitRepository, error) {
	settings := &remediationv1alpha1.RemediationSettings{}
	switch err := r.getLive(ctx, types.NamespacedName{Name: settingsName}, settings); {
	case apierrors.IsNotFound(err):
		return nil, remediationv1alpha1.GitRepository{}, errRepositoryGone
	case err != nil:
		return nil, remediationv1alpha1.GitRepository{}, fmt.Errorf("get RemediationSettings %s: %w", settingsName, err)
	}
	if freshness.Paused(settings) {
		return nil, remediationv1alpha1.GitRepository{}, freshness.ErrPaused
	}
	repo, ok := freshness.FindRepository(settings, rr.Status.PlanRepository)
	if !ok {
		return nil, remediationv1alpha1.GitRepository{}, errRepositoryGone
	}
	if src := rr.Status.Source; src == nil || repo.TargetBranch != src.TargetBranch {
		return nil, remediationv1alpha1.GitRepository{}, errTargetBranchChanged
	}
	git, err := r.OpenRepository(ctx, repo)
	if providers.IsInvalidConfig(err) {
		return nil, remediationv1alpha1.GitRepository{}, freshness.ErrRepositoryUnusable
	}
	if err != nil {
		return nil, remediationv1alpha1.GitRepository{}, freshness.WrapProvider("open the repository", err)
	}
	return git, repo, nil
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
		return reconcile.Result{RequeueAfter: freshRecheckInterval}, nil
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
	return reconcile.Result{RequeueAfter: freshRecheckInterval}, nil
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
	file, err := git.ReadFile(ctx, rr.Status.PullRequest.HeadSHA, string(src.Path))
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
	case errors.Is(err, freshness.ErrRepositoryUnusable), errors.Is(err, errTargetBranchChanged),
		errors.Is(err, errRepositoryGone):
		outcome = "; the settings no longer reach its repository, so the pull request was not closed"
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
	if err := r.Status().Update(ctx, rr); err != nil {
		return reconcile.Result{}, fmt.Errorf("set the deadline of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
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
	res, err := r.pullRequestStep(stepCtx, rr, fresh)
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

	// The pull request may exist although the status write that records it failed (design section 6).
	switch cr, err := git.FindChangeRequest(ctx, branch); {
	case err == nil:
		return reconcile.Result{}, r.adoptChangeRequest(ctx, git, rr, cr)
	case !errors.Is(err, providers.ErrNotFound):
		return reconcile.Result{}, freshness.WrapProvider("find the change request", err)
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

	file, err := git.ReadFile(ctx, head, string(src.Path))
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
		// The commit is there (a restart after the commit, or a concurrent reconcile); open the pull request.
	default:
		return reconcile.Result{}, r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonBranchDiverged,
			"The branch holds neither the previous nor the planned value of the file")
	}

	cr, err := git.OpenChangeRequest(ctx, providers.NewChangeRequest{
		HeadBranch: branch,
		BaseBranch: string(src.TargetBranch),
		Title:      commitTitle(rr),
		Body:       pullRequestBody(rr),
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
		// The branch moved between the read and the open: someone else pushed.
		return reconcile.Result{}, r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonHeadMoved,
			"The branch moved while the pull request was opened")
	}
	return reconcile.Result{}, r.recordPullRequest(ctx, rr, cr)
}

// adoptChangeRequest takes over a pull request found for the branch: one this request opened before its
// status write failed, or one someone else opened on its branch name.
func (r *RemediationRequestReconciler) adoptChangeRequest(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest) error {
	if !hasMarker(cr.Body, rr) {
		return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonForeignChangeRequest,
			"A pull request for the branch exists without this request's marker; it was not touched")
	}
	switch cr.State {
	case providers.ChangeRequestMerged:
		return r.stop(ctx, rr, remediationv1alpha1.PhaseMerged, ReasonChangeRequestMerged, "The pull request was merged")
	case providers.ChangeRequestClosed:
		return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonChangeRequestClosed,
			"The pull request was closed without merging; it is not reopened")
	}
	// The status lost the head Shadewright pushed, so the head is accepted only when the file on it holds the
	// planned value.
	file, err := git.ReadFile(ctx, cr.HeadSHA, string(rr.Status.Source.Path))
	if err != nil {
		return freshness.WrapProvider("read the file of the pull request", err)
	}
	if _, state, err := planned(file.Data, rr); err != nil || state != fileHoldsDesired {
		return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonHeadMoved,
			"The pull request's head does not hold the planned change")
	}
	return r.recordPullRequest(ctx, rr, cr)
}

// recordPullRequest writes PRCreated with the pull request in one status write.
func (r *RemediationRequestReconciler) recordPullRequest(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	cr providers.ChangeRequest) error {
	rr.Status.Phase = remediationv1alpha1.PhasePRCreated
	rr.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA}
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
	// prOpen: open with Shadewright's head, or it could not be checked; the freshness check decides next.
	prOpen prOutcome = iota
	// prStopped: the request ended (Merged, gone, head moved).
	prStopped
	// prClosed: closed without a merge. That is a rejection only if the plan is still fresh; a stale plan's
	// own close that was cut short looks the same and ends Superseded.
	prClosed
)

// watchPullRequest looks at the recorded pull request: a merge ends the request (Merged), a pull request
// that is gone or a moved head too (NeedsIntervention); open with the head Shadewright pushed moves PRCreated
// to AwaitingApproval. On an error the caller goes on to the freshness check, which owns the handling of
// paused writes, changed settings and provider failures. Delivery after the merge is a later step (#94, #95).
func (r *RemediationRequestReconciler) watchPullRequest(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) prOutcome {
	stepCtx, cancel := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	defer cancel()
	err := r.watchStep(stepCtx, rr)
	err = boundErr(ctx, stepCtx, "pull request check", err)
	switch {
	case errors.Is(err, errStopped):
		return prStopped
	case errors.Is(err, errClosed):
		return prClosed
	case err != nil:
		logf.FromContext(ctx).V(1).Info("Could not check the pull request of RemediationRequest",
			"remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error())
	}
	return prOpen
}

// rejected ends a fresh request whose pull request a person closed without a merge.
func (r *RemediationRequestReconciler) rejected(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	return reconcile.Result{}, r.ignoreStopped(r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention,
		ReasonChangeRequestClosed, "The pull request was closed without a merge; automation stopped"))
}

func (r *RemediationRequestReconciler) watchStep(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	git, _, err := r.openPlanRepository(ctx, rr)
	if err != nil {
		return err
	}
	cr, err := git.FindChangeRequest(ctx, freshness.BranchName(rr))
	switch {
	case errors.Is(err, providers.ErrNotFound):
		return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonChangeRequestGone,
			"The recorded pull request cannot be found any more; automation stopped")
	case err != nil:
		return freshness.WrapProvider("find the change request", err)
	}
	if cr.Number != rr.Status.PullRequest.Number || cr.HeadSHA != rr.Status.PullRequest.HeadSHA {
		return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonHeadMoved,
			"The pull request's head is not the commit Shadewright pushed; automation stopped")
	}
	switch cr.State {
	case providers.ChangeRequestMerged:
		return r.stop(ctx, rr, remediationv1alpha1.PhaseMerged, ReasonChangeRequestMerged,
			"The pull request was merged")
	case providers.ChangeRequestClosed:
		return errClosed
	}
	if rr.Status.Phase != remediationv1alpha1.PhasePRCreated {
		return nil
	}
	rr.Status.Phase = remediationv1alpha1.PhaseAwaitingApproval
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
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	return nil
}

// stop ends the request in phase with a condition that says why (PullRequestOpened, or Approved once a pull
// request is recorded), and returns errStopped.
func (r *RemediationRequestReconciler) stop(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	phase remediationv1alpha1.RemediationPhase, reason, message string) error {
	rr.Status.Phase = phase
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
	if reason, _, ok := refusal(err); ok {
		// The deadline has passed either way; waiting for a permission or a rule to change would only keep
		// the request open longer. The message says that a pull request of its own may be left open.
		return reconcile.Result{}, r.ignoreStopped(r.stop(ctx, rr, remediationv1alpha1.PhaseExpired,
			ReasonPullRequestDeadline, "The pull request was not opened before the deadline, and the Git host "+
				"refused ("+reason+") to find or close a pull request Shadewright may have opened on the branch; "+
				"if one is open, close it by hand"))
	}
	if err != nil && !errors.Is(err, errRepositoryGone) && !errors.Is(err, errTargetBranchChanged) &&
		!errors.Is(err, freshness.ErrRepositoryUnusable) {
		return r.retry(ctx, rr, err)
	}
	return reconcile.Result{}, r.ignoreStopped(r.stop(ctx, rr, remediationv1alpha1.PhaseExpired,
		ReasonPullRequestDeadline, "The pull request was not opened before the deadline"))
}

// retry maps a step error to a requeue: paused writes wait, a host that asked for a delay gets it, the rest
// goes to the error backoff.
func (r *RemediationRequestReconciler) retry(ctx context.Context, rr *remediationv1alpha1.RemediationRequest, err error) (
	reconcile.Result, error) {
	switch {
	case errors.Is(err, freshness.ErrPaused):
		return reconcile.Result{RequeueAfter: freshRecheckDelay}, nil
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
// Any other error goes to waitOrFail.
func (r *RemediationRequestReconciler) refuseOrWait(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	err error) (reconcile.Result, error) {
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
	return reconcile.Result{RequeueAfter: freshRecheckInterval}, nil
}

// refusedWhileWatching is the fixed PlanFresh message of a refusal while a recorded pull request waits.
func refusedWhileWatching(reason string) string {
	return "The Git host refused the freshness check (" + reason + "); Shadewright writes nothing and keeps " +
		"watching the pull request"
}

// clearRefusal removes the PlanFresh condition refuseOrWait wrote, once the freshness check passes again.
func (r *RemediationRequestReconciler) clearRefusal(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
	if c == nil || c.Status != metav1.ConditionUnknown || (c.Reason != ReasonGitForbidden && c.Reason != ReasonGitRejected) {
		return nil
	}
	meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
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
// request.
func (r *RemediationRequestReconciler) waitOrFail(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	err error) (reconcile.Result, error) {
	d := retryAfterOf(err)
	if d <= 0 {
		return reconcile.Result{}, err
	}
	if rr.Status.Phase == remediationv1alpha1.PhasePolicyChecked && rr.Status.Deadline != nil {
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
// Expired) may still have an open pull request of its own, so it is closed too; a merged one refuses the
// close with a conflict, and one without this request's marker is left alone. While writes are paused the
// deletion waits, because closing is a write.
func (r *RemediationRequestReconciler) finalize(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	if !controllerutil.ContainsFinalizer(rr, FinalizerName) {
		return reconcile.Result{}, nil
	}
	if r.OpenRepository != nil && rr.Status.PlanRepository != nil && closeOnDelete(rr.Status.Phase) {
		cleanCtx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
		err := r.closeChangeRequest(cleanCtx, rr)
		err = boundErr(ctx, cleanCtx, "cleanup of the change request", err)
		cancel()
		switch {
		case err == nil, errors.Is(err, errRepositoryGone), errors.Is(err, errTargetBranchChanged),
			errors.Is(err, freshness.ErrRepositoryUnusable), errors.Is(err, providers.ErrConflict):
			// Closed, nothing reachable to close, or already merged.
		case errors.Is(err, freshness.ErrPaused):
			return reconcile.Result{RequeueAfter: freshRecheckDelay}, nil
		default:
			return r.waitOrFail(ctx, rr, err)
		}
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

// hasMarker reports whether body carries this request's marker on a line of its own.
func hasMarker(body string, rr *remediationv1alpha1.RemediationRequest) bool {
	want := marker(rr)
	for line := range strings.Lines(body) {
		if strings.TrimRight(line, "\r\n") == want {
			return true
		}
	}
	return false
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

// commitTitle is the commit message and pull request title. It follows Conventional Commits, as the GitOps
// repository's title check requires: lower case, no full stop. Every part is a validated name or value.
func commitTitle(rr *remediationv1alpha1.RemediationRequest) string {
	what, _, desired := change(rr)
	t := rr.Spec.TargetRef
	return redact.String(fmt.Sprintf("fix: set %s of %s/%s to %s", what, t.Namespace, t.Name, desired))
}

// pullRequestBody is the evidence, scope, expected effect, risk and rollback plan, and the marker. Every value
// comes from validated fields of the request and its status; the whole body is redacted once more, so nothing
// that looks like a secret reaches the Git host.
func pullRequestBody(rr *remediationv1alpha1.RemediationRequest) string {
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
	fmt.Fprintf(&b, "**Expected effect:** once merged and delivered, %s is `%s`.\n\n", what, desired)
	b.WriteString("**Risk:** the change is bounded by the policy's limits and touches one field. " +
		"Any other push to this branch stops the automation.\n\n")
	b.WriteString("**Rollback:** revert this pull request; the GitOps controller applies the previous value.\n\n")
	fmt.Fprintf(&b, "**Approval required:** %s.\n\n", approval)
	out := redact.String(b.String())
	// The marker is added after redaction: it holds only the UID and the digest, and must stay exact.
	return out + marker(rr) + "\n"
}
