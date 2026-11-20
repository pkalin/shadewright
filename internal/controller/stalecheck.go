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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
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
	return r.ownsReviewCheck(rr) && !staleOnHead(rr)
}

// ownsReviewCheck reports whether rr writes the review check of its recorded pull request at all: reviewer hints on,
// a recorded pull request this request still owns (not handed over, no handoff intent).
func (r *RemediationRequestReconciler) ownsReviewCheck(rr *remediationv1alpha1.RemediationRequest) bool {
	return r.ReviewerHints && rr.Status.PullRequest != nil && !handedOff(rr) && intent(rr) == nil
}

// staleOnHead reports whether rr records state Stale for the review check of its recorded pull request's head.
func staleOnHead(rr *remediationv1alpha1.RemediationRequest) bool {
	pr := rr.Status.PullRequest
	rc := pr.ReviewCheck
	return rc != nil && rc.Head == pr.HeadSHA && rc.State == remediationv1alpha1.ReviewCheckStale
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

// EventReviewCheckNotFailed is the Warning Event recorded when condition ReviewCheckNotFailed is set or its reason
// changes: a stopped request could not fail the review check of its open pull request (failStoppedCheck).
const EventReviewCheckNotFailed = "ReviewCheckNotFailed"

// failStoppedCheck fails the review check shadewright/review of a request that stopped while its own pull request is
// open, on the target branch or on a base a person moved it to (#925, ADR-0043 limit (e)), and records state Stale in
// rr's status in memory, for the caller's status write: moved back to the target branch, or left there, the pull
// request cannot merge on a check written before the stop. Its callers have just read the pull request open
// (stoppedCheck): the step that stops the request for a pull request open on another base (stopOpenElsewhere), and
// every look of a stopped holder that reads it open (failCheckAfterLook), so a stop for any other reason gets it at
// its first look, and a failed write is tried again at the next. Once Stale is recorded on the head, such a look
// reads the latest review check of the head (stoppedCheckShown) and writes the failed check again only when the Git
// host shows another state last or none (decision of 2026-11-20 on #925): one read and at most one write per look,
// bounded by those looks (heldRecheckInterval while the lock is held), with no polling of its own. Those looks run
// while the request keeps its lock, which an open pull request keeps. It reads no pull request itself, so it never
// changes what a look concludes (a first look at a missing number, goneLooks). A lost status write leaves the check
// pending, and the next look writes the same failed run again.
//
// The stop goes on whatever it returns, since a final phase waits for nothing, so a check that is not written is said
// in status, in the same write: condition ReviewCheckNotFailed, with the reason of the failure (checkNotFailed), and a
// Warning Event when it is set or its reason changes, not at each look. The write that records Stale removes it, so
// does a look that sees the failed check last, and so does every write that ends the looks (endCheckNote); a failed
// read of the check on a head recorded Stale leaves it as it is and is logged (stoppedCheckUnread). A repository
// entry that names another target branch writes nothing, since a branch change permits reads only (#771): that is
// said the same way, until the entry names the branch again. A cancelled reconcile is only returned, and so is a
// failure of the Kubernetes API (reading the settings), for the error backoff.
func (r *RemediationRequestReconciler) failStoppedCheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	reason string) error {
	if !r.ownsReviewCheck(rr) {
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionReviewCheckNotFailed)
		return nil
	}
	recorded := staleOnHead(rr)
	pr := rr.Status.PullRequest
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest",
		pr.Number, "head", pr.HeadSHA)
	stepCtx, cancel := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	defer cancel()
	git, _, _, err := r.openPlanRepositoryFor(stepCtx, rr)
	if recorded {
		var again bool
		if again, err = stoppedCheckShown(stepCtx, git, pr.HeadSHA, err); !again {
			return r.stoppedCheckUnread(ctx, stepCtx, rr, err)
		}
		log.Info("The failed review check of a stopped RemediationRequest is no longer the latest on its head; " +
			"writing it again")
	}
	if err == nil {
		err = git.CreateCheckRun(stepCtx, pr.HeadSHA, stoppedCheckRun(reason))
		err = boundErr(ctx, stepCtx, "write the check run", err)
	}
	switch {
	case err == nil:
		pr.ReviewCheck = &remediationv1alpha1.ReviewCheck{Head: pr.HeadSHA, State: remediationv1alpha1.ReviewCheckStale}
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionReviewCheckNotFailed)
		log.Info("Failed the review check of a stopped RemediationRequest")
		return nil
	case ctx.Err() != nil:
		return err
	case isClusterError(err):
		log.Error(err, "Could not read the settings to fail the review check of a stopped RemediationRequest")
		return err
	}
	why, text := checkNotFailed(err)
	log.Info("Could not fail the review check of a stopped RemediationRequest; tried again at its next look",
		"reason", why, "error", err.Error())
	before := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionReviewCheckNotFailed)
	changed := before == nil || before.Reason != why
	message := fmt.Sprintf("The review check shadewright/review of pull request #%d could not be failed after the "+
		"request stopped: %s. A check written before the stop may still show on the pull request; do not merge it "+
		"on that check. The write is tried again at the next look of the request", pr.Number, text)
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type: remediationv1alpha1.ConditionReviewCheckNotFailed, Status: metav1.ConditionTrue, Reason: why,
		Message: message, ObservedGeneration: rr.Generation,
	})
	if changed && r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeWarning, EventReviewCheckNotFailed, message)
	}
	return err
}

// stoppedCheckShown is the look of a stopped holder whose failed check is recorded Stale on the head (failStoppedCheck,
// decision of 2026-11-20 on #925): it reads the latest review check of head and reports whether the failed check has
// to be written again, because the Git host shows another state last or none. A failure to open the repository
// (opened) or to read is returned with again false: nothing is written in that look.
func stoppedCheckShown(ctx context.Context, git providers.GitProvider, head string, opened error) (again bool,
	err error) {
	if opened != nil {
		return false, opened
	}
	shows, found, err := git.ReviewCheckRun(ctx, head)
	if err != nil {
		return false, freshness.WrapProvider("read the check run", err)
	}
	return !found || shows != providers.CheckRunFailure, nil
}

// stoppedCheckUnread ends failStoppedCheck's look at a check recorded Stale that wrote nothing (stoppedCheckShown):
// with no error the host shows the failed check last, and condition ReviewCheckNotFailed is removed; a cancelled
// reconcile and a failure of the Kubernetes API are returned, and any other failure is logged and tried at the next
// look. A failed read leaves the condition as it is: set by an earlier look whose write failed, it stays until a
// look sees the failed check last; not set, it is not set, as the failed check was written and nothing shows it was
// written over.
func (r *RemediationRequestReconciler) stoppedCheckUnread(ctx, stepCtx context.Context,
	rr *remediationv1alpha1.RemediationRequest, err error) error {
	pr := rr.Status.PullRequest
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest",
		pr.Number, "head", pr.HeadSHA)
	switch {
	case err == nil:
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionReviewCheckNotFailed)
		return nil
	case ctx.Err() != nil:
		return err
	case isClusterError(err):
		log.Error(err, "Could not read the settings to look at the review check of a stopped RemediationRequest")
		return err
	}
	err = boundErr(ctx, stepCtx, "read the check run", err)
	log.Info("Could not look at the failed review check of a stopped RemediationRequest; looked at again at its "+
		"next look", "error", err.Error())
	return nil
}

// endCheckNote removes condition ReviewCheckNotFailed from rr's status in memory, for the write that ends the looks
// of a stopped request: the condition says the failed check is tried again at the next look, so it is True only
// while the request's own pull request is open and its failed check is not written (#925). Its callers: a look that
// reads the pull request not open (failCheckAfterLook), a write that records the merge (settleRecorded), every
// release of the lock (lockverdict.Release, which removes it itself) and the first write of a deletion (finalize).
func endCheckNote(rr *remediationv1alpha1.RemediationRequest) bool {
	return meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionReviewCheckNotFailed)
}

// checkNotFailed is the reason of condition ReviewCheckNotFailed for err, the failure to write a stopped request's
// failed review check, and what it says: the settings that permit no write to the repository of the plan, a pause of
// the kill switch, or any other failure of the Git host (GitFailed), never with the host's answer.
func checkNotFailed(err error) (reason, text string) {
	switch {
	case errors.Is(err, freshness.ErrPaused):
		return ReasonWritesPaused, "RemediationSettings spec.paused holds Git writes"
	case errors.Is(err, errNoSettings):
		return ReasonSettingsNotFound, "RemediationSettings " + remediationv1alpha1.SettingsName + " does not exist"
	case errors.Is(err, errRepositoryGone):
		return reasonRepositoryUnavailable, "the repository of the plan is not in RemediationSettings any more"
	case errors.Is(err, errTargetBranchChanged):
		return reasonTargetBranchChanged, "the repository of the plan names another target branch in " +
			"RemediationSettings now, which permits reads only"
	case errors.Is(err, freshness.ErrRepositoryUnusable):
		return string(sourcemap.ReasonRepositoryUnusable), "the entry of the repository of the plan in " +
			"RemediationSettings cannot be used"
	}
	return ReasonGitFailed, "the Git host did not take the write"
}
