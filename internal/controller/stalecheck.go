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

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// The failed check run of a stale plan (#713, ADR-0028 amendment of 2026-11-15). With reviewer hints on, the check
// run shadewright/review is the merge check a ruleset may require. A check that went green before the plan went
// stale would stay green while the Git host refuses the close (#199), so a person could merge the stale plan on it.
// cleanUpStale therefore completes the check on the recorded head as failed, before each try of the close:
//
//   - once per request: state Stale on the recorded head (status.pullRequest.reviewCheck) says it is done, and a
//     request that records it never writes the check again (reviewCheck), so it never goes green again. Until Stale
//     is recorded each try writes the failed run again, without reading what the Git host shows first: a second
//     failed run after a lost status write is harmless, and no read can keep the write from happening;
//   - whether or not the close then goes through: a closed pull request with a failed check is harmless, but one
//     whose check is still green could be reopened and merged on it, so the request does not end Superseded while
//     the check is pending (staleCheckPending): it is tried again every poll interval (errStaleCheckPending), with
//     no other bound, as the refused close is;
//   - a failed or refused write, or a lost status write, changes nothing else: it is logged, the close goes on,
//     and the next try of the close tries the check again, with no backoff of its own and no Event;
//   - the provider opened for the stale plan lets any failed check run shadewright/review through, and no other
//     write but the close (gatedGit.CreateCheckRun).

// staleCheckTitle is the title of the failed check run of a stale plan.
const staleCheckTitle = "The plan is stale"

// errStaleCheckPending is the answer of a stale plan's cleanup whose pull request is closed while the failed check
// is not recorded yet (staleCheckPending): the request keeps its phase and the mark, and the check and the end are
// tried again after the poll interval (refuseOrWait, endStaleClosed). It is not a final phase.
var errStaleCheckPending = errors.New("the review check of the stale plan's closed pull request is not failed yet")

// staleCheckRun is the failed check run shadewright/review of a plan found stale with reason, one of staleReasons.
func staleCheckRun(reason string) providers.CheckRun {
	return providers.CheckRun{Name: providers.CheckRunName, Status: providers.CheckRunFailure, Title: staleCheckTitle,
		Summary: "Shadewright found the plan of this pull request stale (" + reason + "): what it was decided on " +
			"has changed since, so it must not be merged. Shadewright closes the pull request; if the Git host " +
			"refuses the close, a person closes it. This check stays failed."}
}

// staleCheckPending reports whether a stale plan's check run still has to be failed: reviewer hints on, a recorded
// pull request this request still owns (not handed over, no handoff intent), and no state Stale on its head.
func (r *RemediationRequestReconciler) staleCheckPending(rr *remediationv1alpha1.RemediationRequest) bool {
	pr := rr.Status.PullRequest
	if !r.ReviewerHints || pr == nil || handedOff(rr) || intent(rr) != nil {
		return false
	}
	rc := pr.ReviewCheck
	return rc == nil || rc.Head != pr.HeadSHA || rc.State != remediationv1alpha1.ReviewCheckStale
}

// failStaleCheck completes the check run shadewright/review on the recorded head of a stale plan's pull request as
// failed and records state Stale (see above), with failCheck.
func (r *RemediationRequestReconciler) failStaleCheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	reason string) error {
	return r.failCheck(ctx, rr, staleCheckRun(reason))
}

// failCheck writes run, a failed check run shadewright/review, on the recorded head of rr's pull request and records
// state Stale, which no later write changes: for a stale plan (failStaleCheck) and a cancelled request
// (failCancelCheck, #289). It does nothing and returns nil when the check is not pending (staleCheckPending). A
// failure is logged and returned, but for a cancelled reconcile and a pause, which are only returned. The step has a
// deadline of its own, so it never eats the close's.
func (r *RemediationRequestReconciler) failCheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	run providers.CheckRun) error {
	if !r.staleCheckPending(rr) {
		return nil
	}
	pr := rr.Status.PullRequest
	stepCtx, cancel := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	defer cancel()
	err := r.failCheckStep(stepCtx, rr, run)
	if err == nil || ctx.Err() != nil || errors.Is(err, freshness.ErrPaused) {
		// A pause is the close's to report (waitWhilePaused).
		return err
	}
	logf.FromContext(ctx).Info("Could not fail the review check of RemediationRequest; tried again with the close",
		"remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", pr.Number, "head", pr.HeadSHA,
		"title", run.Title, "error", err.Error())
	return err
}

// failCheckStep is failCheck's work: it reads the recorded pull request by its number, writes the failed check, then
// records Stale. It writes nothing when that read fails (returned for a retry) or finds the pull request merged into a
// base a person moved it to. One open on a base a person moved it to gets the failed check too (#925): the check run
// belongs to the head commit, and moved back to the target branch the pull request must not merge on a check written
// before. Through a new installation of the plan's repository (#290) the read also asks for this request's
// marker, and nothing is written either when the pull request is not this request's.
func (r *RemediationRequestReconciler) failCheckStep(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, run providers.CheckRun) error {
	git, _, reinstalled, err := r.openPlanRepositoryFor(ctx, rr)
	if err != nil {
		return err
	}
	v := r.changeOf(ctx, git, rr, changeQuery{marker: reinstalled})
	switch v.rule().failCheck {
	case failCheckRetry:
		return v.err
	case failCheckNotOurs:
		// Merged into another base: nothing is written to it there, as for gone.
		return fmt.Errorf("%w: %s", errChangeRequestNotOurs, v.gone)
	case failCheckNotOursReinstalled:
		if reinstalled {
			return fmt.Errorf("%w: %s", errChangeRequestNotOurs, v.gone)
		}
	case failCheckWrite:
	}
	if err := git.CreateCheckRun(ctx, rr.Status.PullRequest.HeadSHA, run); err != nil {
		return freshness.WrapProvider("write the check run", err)
	}
	return r.recordStaleCheck(ctx, rr)
}

// recordStaleCheck records state Stale for the recorded head in rr's status, through the kill switch. A reason of
// condition Approved about the check (ReviewCheckRefused, ReviewListTooLong) no longer applies once the check is
// failed for the stale plan, so it gives way to WaitingForReview, as in recordReviewCheck; PlanFresh says why the
// request will not be merged. rr is changed only when the write went through.
func (r *RemediationRequestReconciler) recordStaleCheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	if err := r.writeAllowed(ctx); err != nil {
		return err
	}
	upd := rr.DeepCopy()
	pr := upd.Status.PullRequest
	pr.ReviewCheck = &remediationv1alpha1.ReviewCheck{Head: pr.HeadSHA, State: remediationv1alpha1.ReviewCheckStale}
	if staleReviewCheckWarning(upd) {
		setWaitingForReview(upd)
	}
	if err := r.Status().Update(ctx, upd); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	upd.DeepCopyInto(rr)
	logf.FromContext(ctx).Info("Failed the review check of RemediationRequest", "remediationRequest",
		client.ObjectKeyFromObject(rr), "pullRequest", pr.Number, "head", pr.HeadSHA)
	return nil
}

// stoppedCheckTitle is the title of the failed check run of a request that stopped while its pull request is open.
const stoppedCheckTitle = "The request stopped"

// stoppedCheckRun is the failed check run shadewright/review of a request that stopped with reason while its pull
// request is open (failStoppedCheck).
func stoppedCheckRun(reason string) providers.CheckRun {
	return providers.CheckRun{Name: providers.CheckRunName, Status: providers.CheckRunFailure, Title: stoppedCheckTitle,
		Summary: "Shadewright stopped the request of this pull request (" + reason + ") and no longer checks its " +
			"plan, so it must not be merged on an earlier check. This check stays failed."}
}

// EventReviewCheckNotFailed is the Warning Event recorded when a stopped request could not fail the review check of
// its open pull request (failStoppedCheck). The next look of the stopped holder tries again.
const EventReviewCheckNotFailed = "ReviewCheckNotFailed"

// failStoppedCheck fails the review check shadewright/review of a request that stopped while its own pull request is
// open, on the target branch or on a base a person moved it to (#925, ADR-0043 limit (e)), and records state Stale in
// rr's status in memory, for the caller's status write: moved back to the target branch, or left there, the pull
// request cannot merge on a check written before the stop. Its callers have just read the pull request open
// (stoppedCheck): the step that stops the request for a pull request open on another base (stopOpenElsewhere), and
// every look of a stopped holder (checkHeld) while the check is pending (staleCheckPending, reviewer hints on), so a
// stop for any other reason gets it at its first look, and a failed write is tried again at the next. Those looks run
// while the request keeps its lock, which an open pull request keeps. It reads no pull request itself, so it never
// changes what a look concludes (a first look at a missing number, goneLooks). A lost status write leaves the check
// pending, and the next look writes the same failed run again. A failure is logged with a Warning Event
// ReviewCheckNotFailed and returned; the stop goes on whatever it returns, since a final phase waits for nothing. A
// repository entry that names another target branch, or none, writes nothing and records no Event: a branch change
// permits reads only (#771).
func (r *RemediationRequestReconciler) failStoppedCheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	reason string) error {
	if !r.staleCheckPending(rr) {
		return nil
	}
	pr := rr.Status.PullRequest
	stepCtx, cancel := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	defer cancel()
	git, _, _, err := r.openPlanRepositoryFor(stepCtx, rr)
	if err == nil {
		err = git.CreateCheckRun(stepCtx, pr.HeadSHA, stoppedCheckRun(reason))
		err = boundErr(ctx, stepCtx, "write the check run", err)
	}
	switch {
	case err == nil:
		pr.ReviewCheck = &remediationv1alpha1.ReviewCheck{Head: pr.HeadSHA, State: remediationv1alpha1.ReviewCheckStale}
		logf.FromContext(ctx).Info("Failed the review check of a stopped RemediationRequest", "remediationRequest",
			client.ObjectKeyFromObject(rr), "pullRequest", pr.Number, "head", pr.HeadSHA)
		return nil
	case ctx.Err() != nil || errors.Is(err, freshness.ErrPaused):
		return err
	case errors.Is(err, errTargetBranchChanged), errors.Is(err, errRepositoryGone),
		errors.Is(err, freshness.ErrRepositoryUnusable):
		logf.FromContext(ctx).Info("Did not fail the review check of a stopped RemediationRequest: its repository "+
			"entry does not allow it", "remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", pr.Number,
			"error", err.Error())
		return err
	}
	logf.FromContext(ctx).Info("Could not fail the review check of a stopped RemediationRequest; tried again at its "+
		"next look", "remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", pr.Number, "head", pr.HeadSHA,
		"error", err.Error())
	if r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeWarning, EventReviewCheckNotFailed, fmt.Sprintf("The review check of "+
			"pull request #%d could not be failed after the request stopped (%s); it is tried again at the next look",
			pr.Number, reason))
	}
	return err
}
