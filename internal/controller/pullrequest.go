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
// opening a pull request for a branch that has one returns it.
//
// The finalizer that closes the pull request on deletion is added before the first write, so no branch or
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

// stepDelay is how soon the next step runs after a write that changed only Git, not the request.
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
	// ReasonPatchRefused: the typed patcher refused the file on the branch.
	ReasonPatchRefused = "PatchRefused"
	// ReasonPullRequestDeadline: the pull request was not opened before status.deadline.
	ReasonPullRequestDeadline = "PullRequestDeadline"
	// ReasonChangeRequestGone: the recorded pull request cannot be found any more.
	ReasonChangeRequestGone = "ChangeRequestGone"
	// ReasonPlanIncomplete: the request's status.source has no buildPath, so the change cannot be rendered.
	ReasonPlanIncomplete = "PlanIncomplete"

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
// is called by recheck after EnsureFresh returned nil.
func (r *RemediationRequestReconciler) advance(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	switch rr.Status.Phase {
	case remediationv1alpha1.PhasePolicyChecked:
		return r.openPullRequest(ctx, rr)
	case remediationv1alpha1.PhasePRCreated, remediationv1alpha1.PhaseAwaitingApproval:
		// recheck looked at the pull request before the freshness check; the next look is the periodic one.
		return reconcile.Result{RequeueAfter: freshRecheckInterval}, nil
	}
	return reconcile.Result{RequeueAfter: freshRecheckInterval}, nil
}

// openPullRequest does the next step towards an open pull request: set the deadline, add the finalizer,
// create the branch, commit the change or open the pull request, one of them per call.
func (r *RemediationRequestReconciler) openPullRequest(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	if rr.Status.Deadline == nil {
		rr.Status.Deadline = &metav1.Time{Time: r.now().Add(r.pullRequestTimeout())}
		if err := r.Status().Update(ctx, rr); err != nil {
			return reconcile.Result{}, fmt.Errorf("set the deadline of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
		}
		return reconcile.Result{RequeueAfter: stepDelay}, nil
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
	res, err := r.pullRequestStep(stepCtx, rr)
	err = boundErr(ctx, stepCtx, "pull request step", err)
	switch {
	case errors.Is(err, errStopped):
		return reconcile.Result{}, nil
	case err != nil:
		return r.retry(ctx, rr, err)
	}
	return res, nil
}

// pullRequestStep rebuilds the state from Git and makes at most one write.
func (r *RemediationRequestReconciler) pullRequestStep(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	reconcile.Result, error) {
	git, repo, err := r.openPlanRepository(ctx, rr)
	if err != nil {
		return reconcile.Result{}, err
	}
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
		// Step 2: the branch starts at the target branch's head as it is now. The freshness check has just
		// confirmed that the file there still holds the previous value.
		base, err := git.BranchHead(ctx, string(repo.TargetBranch))
		if err != nil {
			return reconcile.Result{}, freshness.WrapProvider("read the head of the target branch", err)
		}
		if err := git.CreateBranch(ctx, branch, base); err != nil {
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
		// Steps 3 to 6: one commit that changes the one value, on top of the head just read.
		if err := r.checkRender(ctx, git, head, changed, rr); err != nil {
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
		BaseBranch: string(repo.TargetBranch),
		Title:      commitTitle(rr),
		Body:       pullRequestBody(rr),
	})
	if err != nil {
		return reconcile.Result{}, freshness.WrapProvider("open the pull request", err)
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

// checkRender renders the Kustomization's directory at head with and without the change and stops the
// request unless the one difference is the planned field of the target (design 8.2, steps 4 and 5). A
// refusal is written with the check's stable reason and fixed text, never with file content; Git that
// cannot be read is retried.
func (r *RemediationRequestReconciler) checkRender(ctx context.Context, git providers.GitProvider, head string,
	changed []byte, rr *remediationv1alpha1.RemediationRequest) error {
	src := rr.Status.Source
	if src.BuildPath == "" {
		return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, ReasonPlanIncomplete,
			"The plan does not record the directory Flux builds; the request has to be decided again")
	}
	files, err := semdiff.Load(ctx, git, head, string(src.BuildPath))
	if err == nil {
		_, err = semdiff.Check(ctx, semdiff.Input{
			Files: files, Path: string(src.BuildPath), File: string(src.Path), Patched: changed,
			Target: rr.Spec.TargetRef, Action: rr.Spec.Action,
		})
	}
	if se, ok := errors.AsType[*semdiff.Error](err); ok {
		return r.stop(ctx, rr, remediationv1alpha1.PhaseNeedsIntervention, string(se.Reason),
			"The rendered change is not exactly the planned one ("+string(se.Reason)+"); nothing was committed")
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
	if te, ok := errors.AsType[*sourcemap.TransientError](err); ok && te.RetryAfter > 0 {
		return reconcile.Result{RequeueAfter: te.RetryAfter}, nil
	}
	if pe, ok := errors.AsType[*freshness.ProviderError](err); ok && pe.RetryAfter > 0 {
		logf.FromContext(ctx).Info("Waiting before retrying RemediationRequest", "remediationRequest",
			client.ObjectKeyFromObject(rr), "error", err.Error(), "retryAfter", pe.RetryAfter)
		return reconcile.Result{RequeueAfter: pe.RetryAfter}, nil
	}
	return reconcile.Result{}, err
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
			if pe, ok := errors.AsType[*freshness.ProviderError](err); ok && pe.RetryAfter > 0 {
				return reconcile.Result{RequeueAfter: pe.RetryAfter}, nil
			}
			return reconcile.Result{}, err
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
