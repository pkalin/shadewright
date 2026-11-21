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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
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

// failedCheckRun builds the failed check run shadewright/review that failCheck writes, with the end of its summary
// for where failCheckStep's read found the pull request (checkWhere).
type failedCheckRun func(where checkWhere) providers.CheckRun

// checkWhere is where failCheckStep's read found the pull request, for the summary of the failed check: on the target
// branch, where the cleanup closes it (closeSummary); on another base, where nothing is closed (otherBaseCheckSummary);
// or gone, where nothing can be closed (goneCheckSummary). No summary may promise a close that does not happen.
type checkWhere int

const (
	checkOnTarget checkWhere = iota
	checkOnOtherBase
	checkGone
)

// closeSummary is the end of the summary of a stale plan's or a cancel's failed check on the target branch, which the
// cleanup closes.
const closeSummary = "Shadewright closes the pull request; if the Git host refuses the close, a person closes it. " +
	"This check stays failed."

// otherBaseCheckSummary is the end of the summary of a failed check written while the pull request is on another
// base than the target branch (decision of 2026-11-20 on #925, point 4): nothing is closed there. It is fixed text:
// no base, title or other text of the Git host goes into a summary.
const otherBaseCheckSummary = "This pull request is on another base than the target branch the request was made " +
	"for: Shadewright closes nothing on that base, and a person decides what becomes of it. This check stays failed."

// goneCheckSummary is the end of the summary of a failed check written on the recorded head of a pull request read
// gone (ADR-0045 case 5): the Git host no longer shows that pull request, so nothing is closed, and the check keeps
// the commit from merging through another pull request from the branch. Fixed text, as otherBaseCheckSummary.
const goneCheckSummary = "The pull request this check was written for cannot be found any more: Shadewright closes " +
	"nothing, and another pull request with this commit must not be merged on an earlier check. This check stays failed."

// failedSummaryEnd is the end of a failed check's summary for where (checkWhere).
func failedSummaryEnd(where checkWhere) string {
	switch where {
	case checkOnOtherBase:
		return otherBaseCheckSummary
	case checkGone:
		return goneCheckSummary
	}
	return closeSummary
}

// staleCheckRun is the failed check run shadewright/review of a plan found stale with reason, one of staleReasons;
// where as for failedCheckRun.
func staleCheckRun(reason string, where checkWhere) providers.CheckRun {
	return providers.CheckRun{Name: providers.CheckRunName, Status: providers.CheckRunFailure, Title: staleCheckTitle,
		Summary: "Shadewright found the plan of this pull request stale (" + reason + "): what it was decided on " +
			"has changed since, so it must not be merged. " + failedSummaryEnd(where)}
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
	return r.failCheck(ctx, rr, func(where checkWhere) providers.CheckRun { return staleCheckRun(reason, where) })
}

// failCheck writes run, a failed check run shadewright/review, on the recorded head of rr's pull request and records
// state Stale, which no later write changes: for a stale plan (failStaleCheck) and a cancelled request
// (failCancelCheck, #289). It does nothing and returns nil when the check is not pending (staleCheckPending). A
// failure is logged and returned, but for a cancelled reconcile and a pause, which are only returned. The step has a
// deadline of its own, so it never eats the close's.
func (r *RemediationRequestReconciler) failCheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	run failedCheckRun) error {
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
		"title", run(checkOnTarget).Title, "error", err.Error())
	return err
}

// failCheckStep is failCheck's work: it reads the recorded pull request by its number, writes the failed check, then
// records Stale. What the read allows is the verdict table's (verdictRow.failCheck): nothing is written when that
// read fails (returned for a retry) or finds the pull request merged into a base a person moved it to. Through a new
// installation of the plan's repository (#290) the read also asks for this request's marker, and nothing is written
// either when it finds the pull request gone (not this request's). One open or closed on a base a person moved it to
// gets the failed check too (#925, ADR-0045): the check run belongs to the head commit, and moved back to the target
// branch, or reopened, the pull request must not merge on a check written before. So does one read gone without a
// new installation (decision of 2026-11-20 on #925 replacing point 2): the check is on the request's own recorded
// commit, and without it an earlier pass there stays while another pull request from the branch is open. A pull
// request not read open on the target branch gets a summary that promises no close (otherBaseCheckSummary, or
// goneCheckSummary for one read gone).
func (r *RemediationRequestReconciler) failCheckStep(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, run failedCheckRun) error {
	git, _, reinstalled, err := r.openPlanRepositoryFor(ctx, rr)
	if err != nil {
		return err
	}
	v := r.changeOf(ctx, git, rr, changeQuery{marker: reinstalled})
	where := checkOnTarget
	switch v.rule().failCheck {
	case failCheckRetry:
		return v.err
	case failCheckNotOurs:
		// Merged into another base: nothing is written for it.
		return fmt.Errorf("%w: %s", errChangeRequestNotOurs, v.gone)
	case failCheckNotOursReinstalled:
		if reinstalled {
			return fmt.Errorf("%w: %s", errChangeRequestNotOurs, v.gone)
		}
		where = checkGone
	case failCheckWriteOtherBase:
		where = checkOnOtherBase
	case failCheckWrite:
	}
	if err := git.CreateCheckRun(ctx, rr.Status.PullRequest.HeadSHA, run(where)); err != nil {
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

// stoppedCheckRefusal is the gate's decision (writeStoppedCheck) on live, the request as read from the API server
// just before the write, for the write on rr's recorded head: the cause of a refusal, or "" when the write may go.
// The zero value of live refuses (no UID). The order only picks which cause the log names.
func (r *RemediationRequestReconciler) stoppedCheckRefusal(rr, live *remediationv1alpha1.RemediationRequest) string {
	switch {
	case live.UID != rr.UID:
		return "the request was deleted and created again"
	case !live.DeletionTimestamp.IsZero():
		return "the request is being deleted"
	case !samePullRequest(live.Status.PullRequest, rr.Status.PullRequest):
		return "the request records another pull request or head"
	case mergeRecorded(live):
		return "the request records the merge of its pull request"
	case !ptr.Deref(live.Status.HoldsTarget, false):
		return "the request no longer holds the target lock"
	case !r.ownsReviewCheck(live):
		return "the request owns no review check: reviewer hints off or a handoff"
	}
	return ""
}

// writeStoppedCheck is the gate of the failed review check of a stopped request (failStoppedCheck), and the only
// function that writes it (decision of 2026-11-21 on #1008, ADR-0046): it decides on live, the request as its caller
// just read it from the API server (afterLook's read), or reads it itself when live is nil, and writes the failed run
// on rr's recorded head only when that request still exists with rr's UID, is not being deleted, records the same pull
// request and head, records no merge, holds the target lock and owns the review check (stoppedCheckRefusal). A step
// that decides the check may be written (a stop exception's "may write", the stop of a pull request open elsewhere or
// gone) is the first condition; this gate is the second, and both must hold. A refusal writes nothing, is logged at
// Info with its cause, and returns false with no error: no look owes the check in any of those cases. A failed read of
// the request is a failure of the Kubernetes API, returned for the error backoff with nothing written. A change of the
// request between the gate's read and the write is not seen (the one limit left, ADR-0046). ctx is the reconcile's,
// stepCtx the step's bounded one, for naming a failed write (boundErr).
func (r *RemediationRequestReconciler) writeStoppedCheck(ctx, stepCtx context.Context, git providers.GitProvider,
	rr, live *remediationv1alpha1.RemediationRequest, reason string) (bool, error) {
	pr := rr.Status.PullRequest
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest",
		pr.Number, "head", pr.HeadSHA)
	cause := ""
	if live == nil {
		live = &remediationv1alpha1.RemediationRequest{}
		switch err := r.reader().Get(ctx, client.ObjectKeyFromObject(rr), live); {
		case apierrors.IsNotFound(err):
			cause = "the request is gone"
		case err != nil:
			return false, &clusterError{err: fmt.Errorf("read RemediationRequest %s/%s before the failed review "+
				"check: %w", rr.Namespace, rr.Name, err)}
		}
	}
	if cause == "" {
		cause = r.stoppedCheckRefusal(rr, live)
	}
	if cause != "" {
		log.Info("Did not write the failed review check of a stopped RemediationRequest", "cause", cause)
		return false, nil
	}
	if err := git.CreateCheckRun(stepCtx, pr.HeadSHA, stoppedCheckRun(reason)); err != nil {
		return false, boundErr(ctx, stepCtx, "write the check run", err)
	}
	return true, nil
}

// EventReviewCheckNotFailed is the Warning Event recorded when condition ReviewCheckNotFailed is set or its reason
// changes: a stopped request could not fail the review check of its open pull request (failStoppedCheck).
const EventReviewCheckNotFailed = "ReviewCheckNotFailed"

// failStoppedCheck fails the review check shadewright/review on the recorded head of a stopped request, and records
// state Stale in rr's status in memory, for the caller's status write: no open pull request from the request's branch
// can then merge that commit on a check written before the stop (#925, ADR-0045). Its callers: the step that stops
// the request for its pull request open on another base (stopOpenElsewhere), the watch's stop for one read gone
// (ChangeRequestGone), and every look of a stopped holder that leaves the lock held (failCheckAfterLook, the one rule
// of the decision of 2026-11-21 on #925), so a stop for any other reason gets it at its first look, and a failed
// write is tried again at the next. A repository reached through a new installation of the App (#290) gets the same
// write, whichever pull request keeps the lock (ADR-0046), but a stopped holder reaches one only while the stale mark
// or staleCleanupStopped holds (cleanupRepository): it names the recorded head, by its full object ID from status (or
// from the listing of the request's own branch when the same look adopted its lost pull request), and can only block
// that commit. The write only ever names the recorded head: the gate of the provider opened through a new installation
// (gatedGit.CreateCheckRun) lets a failed run of that name through on any head, so only its two callers keep to it,
// this function's gate (writeStoppedCheck) and failCheck (a stale plan's failed check, #713). A request that owns no review check (reviewer hints
// off, a handoff intent,
// the pull request handed over) writes nothing, and condition ReviewCheckNotFailed is removed with a line at Info when
// it was set (endCheckNoteLogged), since no look tries the check again. Once Stale is recorded on the head, such a look
// reads the latest review check of the head (stoppedCheckShown) and writes the failed check again only when the Git
// host shows another state last or none (decision of 2026-11-20 on #925): one read and at most one write per look,
// bounded by those looks (heldRecheckInterval while the lock is held), with no polling of its own. Only a failed
// status write after the check write on a head without Stale recorded is faster (ADR-0046 limit (g)): Stale is not
// recorded then, so each retry at the error backoff writes the same failed run again without a read. Those looks run
// while the request keeps its lock, which an open pull request keeps. It reads no pull request itself, so it never
// changes what a look concludes (a first look at a missing number, goneLooks). A lost status write leaves the check
// pending, and the next look writes the same failed run again.
//
// The stop goes on whatever it returns, since a final phase waits for nothing, so a check that is not written is said
// in status, in the same write: condition ReviewCheckNotFailed, with the reason of the failure (checkNotFailed), and a
// Warning Event when it is set or its reason changes, not at each look. The write that records Stale removes it, so
// does a look that sees the failed check last, and so does every write that ends the looks (endCheckNote); a look on
// a head recorded Stale that cannot read the check leaves it as it is and records the Warning Event
// StoppedCheckUnread once (stoppedCheckUnread). A repository entry that names another target branch writes nothing,
// since a branch change permits reads only (#771): that is said the same way, until the entry names the branch again. A cancelled reconcile is only returned, and so is a
// failure of the Kubernetes API (reading the settings), for the error backoff.
//
// The write itself goes through one gate, writeStoppedCheck, the only caller of the Git host's check write for this
// purpose (TestStoppedCheckWrittenOnlyByItsGate). live is the request as its caller just read it from the API server
// (afterLook's read), or nil, and the gate reads it then.
func (r *RemediationRequestReconciler) failStoppedCheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	live *remediationv1alpha1.RemediationRequest, reason string) error {
	if !r.ownsReviewCheck(rr) {
		endCheckNoteLogged(ctx, rr, "the request owns no review check: reviewer hints off or a handoff")
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
		var wrote bool
		// A write the Git host accepted is written, also when the step's time ran out as it returned: only a failed
		// write is named for the deadline or the cancel.
		if wrote, err = r.writeStoppedCheck(ctx, stepCtx, git, rr, live, reason); err == nil && !wrote {
			// Refused by the gate, which logged why: nothing is recorded, deliberately. The request is going away,
			// records a merge, no longer holds the lock or no longer owns the check, and in each case no look owes
			// the check any more; condition ReviewCheckNotFailed is left as it is for the write that ends the looks.
			return nil
		}
	}
	switch {
	case err == nil:
		pr.ReviewCheck = &remediationv1alpha1.ReviewCheck{Head: pr.HeadSHA, State: remediationv1alpha1.ReviewCheckStale}
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionReviewCheckNotFailed)
		r.stoppedCheckLooks.Delete(client.ObjectKeyFromObject(rr))
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
	r.noteCheckNotFailed(rr, why, fmt.Sprintf("The review check shadewright/review of pull request #%d could not "+
		"be failed after the request stopped: %s. A check written before the stop may still show on the pull "+
		"request; do not merge it on that check. The write is tried again at the next look of the request",
		pr.Number, text))
	return err
}

// noteCheckNotFailed sets condition ReviewCheckNotFailed of rr in memory with reason why and message, for the
// caller's status write, and records the Warning Event ReviewCheckNotFailed when the condition is set or its reason
// changes, not at each look.
func (r *RemediationRequestReconciler) noteCheckNotFailed(rr *remediationv1alpha1.RemediationRequest, why,
	message string) {
	before := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionReviewCheckNotFailed)
	changed := before == nil || before.Reason != why
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type: remediationv1alpha1.ConditionReviewCheckNotFailed, Status: metav1.ConditionTrue, Reason: why,
		Message: message, ObservedGeneration: rr.Generation,
	})
	if changed && r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeWarning, EventReviewCheckNotFailed, message)
	}
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

// EventStoppedCheckUnread is the Warning Event recorded when a look of a stopped holder whose failed review check is
// recorded Stale cannot tell whether that check is still the latest on the head (stoppedCheckUnread): once when such
// looks start failing on a head, not at each look.
const EventStoppedCheckUnread = "StoppedCheckUnread"

// stoppedCheckUnread ends failStoppedCheck's look at a check recorded Stale that wrote nothing (stoppedCheckShown):
// with no error the host shows the failed check last, and condition ReviewCheckNotFailed is removed; a cancelled
// reconcile and a failure of the Kubernetes API are returned. Any other failure (the settings permit no read or write
// of the repository of the plan, a pause, or the Git host failed the read of the check) means the look cannot tell
// whether the failed check is still the latest on the head. It is logged at Info and, once when such looks start on
// a head, recorded as the Warning Event StoppedCheckUnread (kept in memory, stoppedCheckLooks of the reconciler, and
// started again by a look that reads the check or writes it), and nil is returned, so the look keeps its pace
// (heldRecheckInterval) and is tried again at the next one. Condition ReviewCheckNotFailed is left as it is: it says
// the failed check could not be written, which such a look does not know.
func (r *RemediationRequestReconciler) stoppedCheckUnread(ctx, stepCtx context.Context,
	rr *remediationv1alpha1.RemediationRequest, err error) error {
	pr := rr.Status.PullRequest
	key := client.ObjectKeyFromObject(rr)
	log := logf.FromContext(ctx).WithValues("remediationRequest", key, "pullRequest", pr.Number, "head", pr.HeadSHA)
	switch {
	case err == nil:
		r.stoppedCheckLooks.Delete(key)
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionReviewCheckNotFailed)
		return nil
	case ctx.Err() != nil:
		return err
	case isClusterError(err):
		log.Error(err, "Could not read the settings to look at the review check of a stopped RemediationRequest")
		return err
	}
	err = boundErr(ctx, stepCtx, "read the check run", err)
	why, text := checkNotFailed(err)
	if why == ReasonGitFailed {
		text = "the Git host did not answer the read of the latest check on its head"
	}
	log.Info("Could not look at the failed review check of a stopped RemediationRequest; looked at again at its "+
		"next look", "reason", why, "error", err.Error())
	if countLook(&r.stoppedCheckLooks, key, rr.UID, pr.HeadSHA) == 1 {
		r.warn(rr, EventStoppedCheckUnread, fmt.Sprintf("The review check shadewright/review of pull request #%d "+
			"could not be looked at after the request stopped: %s. The failed check written at the stop may no "+
			"longer be the latest on its head; do not merge the pull request on another check. Each look of the "+
			"request tries again", pr.Number, text))
	}
	return nil
}

// checkMayStay reports whether a stopped holder's recorded head is not known to show the stop's failed review check:
// rr owns its review check (ownsReviewCheck), no longer progresses, holds the target lock, has no merge recorded, and
// records no state Stale on that head. A deletion of such a request writes no check (ADR-0046 limit (a), #984), so a
// check written before the stop may stay the latest there.
func (r *RemediationRequestReconciler) checkMayStay(rr *remediationv1alpha1.RemediationRequest) bool {
	return r.ownsReviewCheck(rr) && !progresses(rr) && ptr.Deref(rr.Status.HoldsTarget, false) &&
		!mergeRecorded(rr) && !staleOnHead(rr)
}

// endCheckNote removes condition ReviewCheckNotFailed from rr's status in memory, for the write that ends the looks
// of a stopped request: the condition says the failed check is tried again at the next look, so it is True only
// while the request's own pull request, or another open pull request from its branch that keeps the lock, is open
// and its failed check is not written (#925). Its callers: a look that reads the pull request not open while no other
// open pull request keeps the lock (failCheckAfterLook), a look of a request that owns no review check any more
// (failStoppedCheck), a write that records the merge (settleRecorded), every release of the lock
// (lockverdict.Release, which removes it itself) and the first write of a deletion (beginFinalize). Each of them says
// the end in the log at Info when the condition was set (logCheckNoteEnd); the deletion says it also when the
// condition was not set but the recorded head is not known to show the failed check (checkMayStay).
func endCheckNote(rr *remediationv1alpha1.RemediationRequest) bool {
	return meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionReviewCheckNotFailed)
}

// checkNotFailed is the reason of condition ReviewCheckNotFailed for err, the failure to write a stopped request's
// failed review check, and what it says: the settings that permit no write to the repository of the plan, a pause of
// the kill switch, a refusal of the Git host that holds until a person fixes it (GitForbidden, GitRejected, as
// refusal classes them; a token the installation refuses is forbidden), credentials the Git host rejects (a 401,
// GitUnauthorized: a wrong private key or a deleted App, which holds until the App or its key is fixed), a 404
// (GitNotFound: the installation or the repository is not reachable through it), or any other failure of the Git
// host (GitFailed, which passes by itself), never with the host's answer.
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
	switch reason, _, final := refusal(err); {
	case final && reason == ReasonGitForbidden:
		return reason, "the Git host refused the App (a permission it lacks, its installation or a rule of the " +
			"repository), which holds until that is fixed"
	case final:
		return reason, "the Git host refused the request as invalid, which holds until its cause is fixed"
	case providers.IsUnauthorized(err):
		return ReasonGitUnauthorized, "the Git host rejected the App's credentials (401): its private key is wrong " +
			"or the App was deleted, which holds until the App or its key is fixed"
	case providers.IsNotFound(err):
		return ReasonGitNotFound, "the Git host answered 404: the App's installation or the repository is not " +
			"reachable through it"
	}
	return ReasonGitFailed, "the Git host did not take the write"
}
