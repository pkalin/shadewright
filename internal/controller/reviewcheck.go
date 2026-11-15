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
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
)

// The check run shadewright/review and the merge gate (ADR-0028 section 5), with reviewer hints on. Every head of
// a Shadewright pull request gets the check, whether the executor opened the pull request, adopted it after a
// restart or pushed it in a handoff, so a ruleset that requires it never blocks a pull request no hint touched:
//
//   - status.pullRequest.reviewCheck records the state of the check on one head, and always names
//     status.pullRequest.headSHA. Every write that records a new head writes a new pullRequest without it, so a
//     state never outlives its head.
//   - Each look at the pull request that finds it open with the recorded head (watchPullRequest) creates the
//     check in progress on a head that has none (InProgress, or Refused when the Git host refuses it for good).
//   - The gate decides only after the look at the comments of the same look read them to the end and stored
//     what it accepted (pollHints), so an approver whose hint is already on the pull request is never counted.
//     It completes the check with success once the latest review of someone who wrote no hint in the chain
//     (every hint in status.hints, whatever became of it, compared by user ID) approves the recorded head.
//   - The check follows what the Git host shows, not what was recorded (#748). On each caught-up look while the
//     request waits for approval the gate decides what the check should show from the reviews (success while an
//     approval it counts stands, in progress otherwise, failure on a list too long to read), reads the App's own
//     latest run on the recorded head, writes the run when the host shows something else, and then records the
//     state. The record is never a reason to skip that comparison.
//   - A list of reviews too long to read in one gate (maxReviewLooks) completes the check with failure and is
//     recorded as Failed, with condition Approved False and a Warning Event, both with reason
//     ReviewListTooLong: the gate fails closed on that head, never passes on a list it did not read whole, and
//     does not read the list again on that head. Only the stale failure below may follow it there.
//   - A final refusal of the Git host is recorded as Refused, with the count of refusals in a row and the time of
//     the latest on the executor's clock, condition Approved False and a Warning Event per refusal, both with
//     reason ReviewCheckRefused and a message that says what the host shows. The host is asked again after a
//     backoff from that record (refusalBackoff), and a look that finds the host already showing what the
//     reviews call for records that state without a write.
//   - Recording InProgress or Passed sets condition Approved back to WaitingForReview when it still carries one
//     of those two reasons. Only a merge sets Approved True.
//   - A stale plan's cleanup completes the check with failure and the text of a stale plan, and records Stale
//     (failStaleCheck, stalecheck.go, #713), also on a head recorded Failed; from then on this file never writes
//     the check of that request again. Recording Stale sets condition Approved back to WaitingForReview as above.
//   - A failed read of the reviews or of the check run changes nothing and is retried on a later look; looks that
//     fail reviewCheckStuckLooks times in a row on one head record one Warning Event (watchReviewCheck).
//
// A restart, or a failed status write, between a write to the Git host and the status write leaves the record
// behind the host in either direction; the next look compares the host with the reviews again and writes only
// what differs. Reconciles of one request do not overlap, and a status write that lost a race to another writer
// is a conflict retried on the next look. The check never passes more than the gate decided.

// The text of the check run while it waits.
const (
	checkRunTitle   = "Waiting for an approval of this head"
	checkRunSummary = "Shadewright opened or changed this pull request. The check completes once someone who wrote " +
		"no reviewer hint in this pull request approves its latest head; an approval of an earlier head does not " +
		"count."
)

const (
	// maxReviewLooks bounds the looks at the reviews of one pull request per reconcile. Every gate reads the
	// list from its start, so a list longer than that would never be read whole: the gate fails the check on
	// that head (ReasonReviewListTooLong) and a person decides the merge.
	maxReviewLooks = 4
	// reviewApproved is the state of an approving review.
	reviewApproved = "APPROVED"
	// reviewCommented is the state of a review that only comments, such as a reply in a review thread.
	reviewCommented = "COMMENTED"
	// checkRunPassedTitle and checkRunPassedSummary are the text of the completed check.
	checkRunPassedTitle   = "Approved"
	checkRunPassedSummary = "Someone who wrote no hint in this pull request approved its latest head."
	// checkRunFailedTitle and checkRunFailedSummary are the text of the check failed on a list of reviews too
	// long to read.
	checkRunFailedTitle   = "Too many reviews to check"
	checkRunFailedSummary = "This pull request has more reviews than Shadewright reads, so it cannot tell whether " +
		"someone who wrote no reviewer hint approved its latest head. The check fails on this head; a person " +
		"decides the merge."
	// hintApprovalComment is posted once per approval of the head by a hint author.
	hintApprovalComment = "This approval does not satisfy the check shadewright/review: its author wrote a " +
		"reviewer hint in this pull request, and the person who steered a change through a hint cannot also be " +
		"its only approver. Someone who wrote no hint here has to approve the latest head."
)

// ReasonReviewCheckRefused is the reason of condition Approved (False) and of the Warning Event when the Git host
// refused for good to create or complete the check run shadewright/review on the recorded head: a ruleset that
// requires the check blocks the merge until a person acts.
const ReasonReviewCheckRefused = "ReviewCheckRefused"

// reviewCheckStuckLooks is how many looks in a row whose review check failed raise the one Warning Event with
// reason ReviewCheckStuck.
const reviewCheckStuckLooks = 10

// ReasonReviewCheckStuck is the reason of the Warning Event recorded once when the review check of a head failed
// on reviewCheckStuckLooks looks in a row: a read or write of the Git host, or a status write, that keeps failing
// leaves the check as it was, which may be a pass that no longer rests on an approval.
const ReasonReviewCheckStuck = "ReviewCheckStuck"

// stuckLooks counts, for one request and head, the looks in a row whose review check failed.
type stuckLooks struct {
	uid   types.UID
	head  string
	looks int
}

// watchReviewCheck runs reviewCheck after a look at the pull request found it open with the recorded head;
// caughtUp says whether that look read the comments to the end and stored them. A failure is logged and
// changes nothing: the next look tries again. A cancelled reconcile is not logged. After reviewCheckStuckLooks
// failed looks in a row on one head, not counting looks while writes are paused, it records one Warning Event
// with reason ReviewCheckStuck; a look that goes through starts the count again. The count is in memory only:
// after a restart it starts again, so the Event may come once more.
func (r *RemediationRequestReconciler) watchReviewCheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	caughtUp bool) {
	err := r.reviewCheck(ctx, rr, caughtUp)
	if ctx.Err() != nil {
		return
	}
	key := client.ObjectKeyFromObject(rr)
	if err == nil || errors.Is(err, freshness.ErrPaused) || rr.Status.PullRequest == nil {
		if err == nil {
			r.reviewCheckFailures.Delete(key)
		}
		return
	}
	logf.FromContext(ctx).Info("Could not update the review check of RemediationRequest",
		"remediationRequest", key, "error", err.Error())
	head := rr.Status.PullRequest.HeadSHA
	n := stuckLooks{uid: rr.UID, head: head, looks: 1}
	if v, ok := r.reviewCheckFailures.Load(key); ok {
		if old, ok := v.(stuckLooks); ok && old.uid == rr.UID && old.head == head {
			n.looks = old.looks + 1
		}
	}
	r.reviewCheckFailures.Store(key, n)
	if n.looks == reviewCheckStuckLooks && r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeWarning, ReasonReviewCheckStuck, fmt.Sprintf("The review check "+
			"shadewright/review on the pull request's head could not be brought in line with the reviews on %d "+
			"looks in a row; it keeps what the Git host shows, which may be a pass that no longer rests on an "+
			"approval. The executor's log says why", reviewCheckStuckLooks))
	}
}

// reviewCheck makes the check run on the recorded head show the state it should have, and records that state.
// On a head with no state for it, the state is InProgress. While the request waits for review and the look caught
// up (caughtUp), the merge gate decides it from the reviews (reviewGate) on every look, whatever is recorded,
// unless the head's check Failed, which is final. A request whose handoff intent stands leaves both to its
// replacement. A request that records the check Stale (failStaleCheck) never writes it again, on any head. rr is updated in place by each status write that went through.
func (r *RemediationRequestReconciler) reviewCheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	caughtUp bool) error {
	pr := rr.Status.PullRequest
	if !r.ReviewerHints || pr == nil || intent(rr) != nil || !progresses(rr) ||
		(rr.Status.Phase != remediationv1alpha1.PhasePRCreated && rr.Status.Phase != remediationv1alpha1.PhaseAwaitingApproval) {
		return nil
	}
	rc := pr.ReviewCheck
	if rc != nil && rc.State == remediationv1alpha1.ReviewCheckStale {
		// The plan was found stale and its check failed (failStaleCheck, #713): this request never writes it again.
		return nil
	}
	current := rc != nil && rc.Head == pr.HeadSHA
	gate := caughtUp && rr.Status.Phase == remediationv1alpha1.PhaseAwaitingApproval
	if current && (!gate || rc.State == remediationv1alpha1.ReviewCheckFailed) {
		return nil
	}
	stepCtx, cancel := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	defer cancel()
	git, err := r.openPlanRepository(stepCtx, rr)
	if err != nil {
		return err
	}
	if !gate {
		return r.syncReviewCheck(stepCtx, git, rr, providers.CheckRunInProgress)
	}
	return r.reviewGate(stepCtx, git, rr)
}

// recordReviewCheck records state for the recorded head in rr's status, through the kill switch, without a
// refusal; Failed also sets condition Approved False with reason ReviewListTooLong and, once written, records a
// Warning Event; InProgress and Passed set it back to WaitingForReview when it still carries a reason of a check
// that did not pass (staleReviewCheckWarning). rr is changed only when the write went through.
func (r *RemediationRequestReconciler) recordReviewCheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	state remediationv1alpha1.ReviewCheckState) error {
	if err := r.writeAllowed(ctx); err != nil {
		return err
	}
	upd := rr.DeepCopy()
	head := upd.Status.PullRequest.HeadSHA
	upd.Status.PullRequest.ReviewCheck = &remediationv1alpha1.ReviewCheck{Head: head, State: state}
	reason, message := reviewCheckWarning(state)
	switch {
	case reason != "":
		setApprovedFalse(upd, reason, message)
	case staleReviewCheckWarning(upd):
		// The reason was about an earlier head or a refusal that the Git host's answer now ends (#606, #748).
		setWaitingForReview(upd)
	}
	if err := r.Status().Update(ctx, upd); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	upd.DeepCopyInto(rr)
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr))
	if reason != "" {
		log.Info("Recorded the review check of RemediationRequest as not passing", "pullRequest",
			rr.Status.PullRequest.Number, "head", head, "state", state, "reason", reason)
		if r.Recorder != nil {
			r.Recorder.Event(rr, corev1.EventTypeWarning, reason, message)
		}
		return nil
	}
	log.Info("Recorded the review check of RemediationRequest", "pullRequest", rr.Status.PullRequest.Number,
		"head", head, "state", state)
	return nil
}

// setApprovedFalse sets condition Approved of rr False with reason and message.
func setApprovedFalse(rr *remediationv1alpha1.RemediationRequest, reason, message string) {
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionApproved,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: rr.Generation,
	})
}

// reviewGate is the merge gate of a request waiting for review. It reads every review of the pull request and
// decides the state the check run on the recorded head should have: success when the latest review that is not a
// plain comment of a person who wrote no hint in the chain (compared by user ID) approves the recorded head, in
// progress otherwise, and failure when it cannot read the list whole (fail closed). A comment review is skipped
// as GitHub's own count of approvals skips it, so a reply in a review thread never takes an approval back. An
// approval of an earlier head never counts. An approval of the head by a hint author does not count either, and
// gets a comment saying why, once per review (by its marker); this process asks for it once per review, not on
// every look, as each ask reads the comments for the marker, and not again after a final refusal. The comments get
// half of the time left, so one that runs out of time never keeps a passing gate from writing the check; the
// rest are asked on a later look. syncReviewCheck then makes the Git host show that state and records it. A failed
// read returns an error and changes nothing; the next look reads again.
func (r *RemediationRequestReconciler) reviewGate(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest) error {
	fs, ok := feedbackOf(git)
	if !ok {
		return nil
	}
	pr := rr.Status.PullRequest
	latest := map[int64]providers.Feedback{}
	var order []int64
	cur := providers.FeedbackCursor{}
	complete := false
	for range maxReviewLooks {
		page, err := fs.ListReviews(ctx, pr.Number, cur)
		if err != nil {
			return freshness.WrapProvider("list the reviews of the pull request", err)
		}
		for _, it := range page.Items {
			if it.Kind != providers.FeedbackReview || it.Author.Kind != providers.AuthorPerson ||
				it.ReviewState == reviewCommented {
				continue
			}
			if _, seen := latest[it.Author.ID]; !seen {
				order = append(order, it.Author.ID)
			}
			if prev, seen := latest[it.Author.ID]; !seen || !it.CreatedAt.Before(prev.CreatedAt) {
				latest[it.Author.ID] = it
			}
		}
		if complete = page.Complete; complete {
			break
		}
		cur = page.Next
	}
	if !complete {
		return r.syncReviewCheck(ctx, git, rr, providers.CheckRunFailure)
	}
	authors := map[int64]bool{}
	for _, h := range rr.Status.Hints {
		authors[h.Author.ID] = true
	}
	passed := false
	var steered []int64
	for _, id := range order {
		rv := latest[id]
		if rv.ReviewState != reviewApproved || rv.CommitID != pr.HeadSHA {
			continue
		}
		if authors[id] {
			steered = append(steered, rv.ID)
			continue
		}
		passed = true
	}
	if err := r.commentSteered(ctx, git, rr, steered); err != nil {
		return err
	}
	if passed {
		return r.syncReviewCheck(ctx, git, rr, providers.CheckRunSuccess)
	}
	return r.syncReviewCheck(ctx, git, rr, providers.CheckRunInProgress)
}

// syncReviewCheck makes the Git host show want for the check run on the recorded head and records the matching
// state (#748). It reads the App's own run there first. When the host already shows want, it only records the
// state, so a status write lost after an earlier host write is made good without another one. Otherwise it writes
// the host and then records the state: the order on a restart between the two does not matter, as the next look
// compares again. A Refused check whose backoff has not run out is not written again (refusalBackoff); a final
// refusal is recorded Refused (recordRefusal). A failed read or write changes nothing and is returned for a retry;
// a pull request closed or merged meanwhile (a conflict) records nothing. The provider passes the kill switch
// right before the host write, so nothing is written to the host while writes are not allowed.
func (r *RemediationRequestReconciler) syncReviewCheck(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, want providers.CheckRunStatus) error {
	pr := rr.Status.PullRequest
	state := reviewCheckStateOf(want)
	shows, found, err := git.ReviewCheckRun(ctx, pr.HeadSHA)
	if err != nil {
		return freshness.WrapProvider("read the check run", err)
	}
	if found && shows == want {
		return r.mirrorReviewCheck(ctx, rr, state)
	}
	rc := pr.ReviewCheck
	if rc != nil && rc.Head == pr.HeadSHA && rc.State == remediationv1alpha1.ReviewCheckRefused {
		if wait := refusalBackoff(rc, r.now()); wait > 0 {
			return nil
		}
	}
	err = git.CreateCheckRun(ctx, pr.HeadSHA, checkRunFor(want))
	_, _, final := refusal(err)
	switch {
	case err == nil:
		return r.recordReviewCheck(ctx, rr, state)
	case providers.IsConflict(err):
		// Closed or merged meanwhile: the next look at the pull request decides.
		return nil
	case final:
		return r.recordRefusal(ctx, rr, want, shows, found)
	}
	return freshness.WrapProvider("write the check run", err)
}

// mirrorReviewCheck records state when rr does not record it for the recorded head yet, or records it Refused.
func (r *RemediationRequestReconciler) mirrorReviewCheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	state remediationv1alpha1.ReviewCheckState) error {
	pr := rr.Status.PullRequest
	if rc := pr.ReviewCheck; rc != nil && rc.Head == pr.HeadSHA && rc.State == state {
		return nil
	}
	return r.recordReviewCheck(ctx, rr, state)
}

// reviewCheckStateOf is the state recorded for a check run the Git host shows with status s.
func reviewCheckStateOf(s providers.CheckRunStatus) remediationv1alpha1.ReviewCheckState {
	switch s {
	case providers.CheckRunSuccess:
		return remediationv1alpha1.ReviewCheckPassed
	case providers.CheckRunFailure:
		return remediationv1alpha1.ReviewCheckFailed
	}
	return remediationv1alpha1.ReviewCheckInProgress
}

// checkRunFor is the check run with status s and its fixed text.
func checkRunFor(s providers.CheckRunStatus) providers.CheckRun {
	switch s {
	case providers.CheckRunSuccess:
		return providers.CheckRun{Name: providers.CheckRunName, Status: s, Title: checkRunPassedTitle,
			Summary: checkRunPassedSummary}
	case providers.CheckRunFailure:
		return providers.CheckRun{Name: providers.CheckRunName, Status: s, Title: checkRunFailedTitle,
			Summary: checkRunFailedSummary}
	}
	return providers.CheckRun{Name: providers.CheckRunName, Status: providers.CheckRunInProgress,
		Title: checkRunTitle, Summary: checkRunSummary}
}

const (
	// refusalRetryBase is the wait after the first final refusal of the check run on a head before the executor
	// asks the Git host again; each further refusal in a row doubles it, up to refusalRetryMax.
	refusalRetryBase = time.Minute
	refusalRetryMax  = time.Hour
	// maxRecordedRefusals bounds ReviewCheck.Refusals (its schema maximum).
	maxRecordedRefusals = 1000
)

// refusalBackoff returns how long a Refused check run rc still waits before the Git host is asked again, from
// the recorded refusals and refusedAt, both written by the executor on its own clock, and now on that clock. A
// check without them (recorded before they existed) or with a refusedAt after now (a clock that went back, a
// status written by hand) waits no more.
func refusalBackoff(rc *remediationv1alpha1.ReviewCheck, now time.Time) time.Duration {
	if rc.RefusedAt == nil || rc.Refusals < 1 || rc.RefusedAt.After(now) {
		return 0
	}
	wait := refusalRetryMax
	if n := rc.Refusals - 1; n < 6 {
		wait = min(refusalRetryBase<<n, refusalRetryMax)
	}
	return max(rc.RefusedAt.Add(wait).Sub(now), 0)
}

// recordRefusal records that the Git host refused for good to make the check run on the recorded head show want:
// state Refused with one more refusal and refusedAt now, condition Approved False with reason ReviewCheckRefused
// whose message says what the host shows, and, once written, one Warning Event per refusal. The backoff of
// refusalBackoff runs from it. rr is changed only when the write went through.
func (r *RemediationRequestReconciler) recordRefusal(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	want, shows providers.CheckRunStatus, found bool) error {
	if err := r.writeAllowed(ctx); err != nil {
		return err
	}
	upd := rr.DeepCopy()
	pr := upd.Status.PullRequest
	refusals := int32(1)
	if rc := pr.ReviewCheck; rc != nil && rc.Head == pr.HeadSHA && rc.State == remediationv1alpha1.ReviewCheckRefused {
		refusals = min(rc.Refusals+1, maxRecordedRefusals)
	}
	pr.ReviewCheck = &remediationv1alpha1.ReviewCheck{Head: pr.HeadSHA, State: remediationv1alpha1.ReviewCheckRefused,
		Refusals: refusals, RefusedAt: &metav1.Time{Time: r.now()}}
	message := refusalMessage(want, shows, found)
	setApprovedFalse(upd, ReasonReviewCheckRefused, message)
	if err := r.Status().Update(ctx, upd); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	upd.DeepCopyInto(rr)
	logf.FromContext(ctx).Info("Recorded the review check of RemediationRequest as refused by the Git host",
		"remediationRequest", client.ObjectKeyFromObject(rr), "pullRequest", pr.Number, "head", pr.HeadSHA,
		"want", want, "refusals", refusals, "reason", ReasonReviewCheckRefused)
	if r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeWarning, ReasonReviewCheckRefused, message)
	}
	return nil
}

// refusalMessage is the fixed text of a refusal to make the check run show want while the Git host shows shows
// (found false: no run of the App).
func refusalMessage(want, shows providers.CheckRunStatus, found bool) string {
	words := map[providers.CheckRunStatus]string{providers.CheckRunInProgress: "in progress",
		providers.CheckRunSuccess: "success", providers.CheckRunFailure: "failure"}
	now := "no check run shadewright/review from Shadewright"
	if found {
		now = "the check run shadewright/review as " + words[shows]
	}
	m := "The Git host refused for good to set the check run shadewright/review on the pull request's head to " +
		words[want] + "; it shows " + now + " there. "
	if found && shows == providers.CheckRunSuccess {
		m += "It no longer rests on an approval the merge gate counts, so a person must not merge on it. "
	}
	return m + "Shadewright asks again after a backoff; check the App's checks permission and the rules"
}

// commentSteered posts the comment saying why an approval does not count on each review in steered that this
// process has not answered yet, within half of the time left in ctx. A comment posted, or refused for good, is
// remembered (answerSteered); a failure worth retrying is logged and asked again on a later look, and so are the
// comments left when their time ran out. Only the caller's cancellation is returned.
func (r *RemediationRequestReconciler) commentSteered(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, steered []int64) error {
	commentCtx, cancel := ctx, context.CancelFunc(func() {})
	if dl, ok := ctx.Deadline(); ok {
		commentCtx, cancel = context.WithDeadline(ctx, time.Now().Add(time.Until(dl)/2))
	}
	defer cancel()
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr))
	for _, id := range steered {
		if r.steeredAnswered(rr, id) {
			continue
		}
		mark := fmt.Sprintf("<!-- shadewright review-gate review=%d -->", id)
		err := git.CommentChangeRequest(commentCtx, rr.Status.PullRequest.Number, mark, hintApprovalComment+"\n\n"+mark)
		reason, _, final := refusal(err)
		switch {
		case err == nil:
			r.answerSteered(rr, id)
		case ctx.Err() != nil:
			return fmt.Errorf("comment on an approval that does not count stopped: %w", ctx.Err())
		case commentCtx.Err() != nil:
			log.Info("Ran out of time to say why approvals do not count for RemediationRequest; the rest are "+
				"asked on a later look", "review", id)
			return nil
		case final:
			r.answerSteered(rr, id)
			log.Info("The Git host refused the comment saying why an approval does not count for RemediationRequest; "+
				"it is not asked again", "review", id, "reason", reason)
		case errors.Is(err, providers.ErrTooManyComments):
			// Fail closed: an earlier comment cannot be ruled out, so none is posted, and it is not asked again.
			r.answerSteered(rr, id)
			log.Info("Did not say why an approval does not count for RemediationRequest: the pull request has more "+
				"comments than are read to find an earlier one", "review", id)
		default:
			log.Info("Could not say why an approval does not count for RemediationRequest", "review", id,
				"error", err.Error())
		}
	}
	return nil
}

// ReasonReviewListTooLong is the reason of condition Approved (False) and of the Warning Event when the reviews
// of a pull request were too many for the merge gate to read: it failed the check run on the head, so a ruleset
// that requires it blocks the merge until a person acts.
const ReasonReviewListTooLong = "ReviewListTooLong"

// reviewCheckWarning returns the reason and fixed message of condition Approved (False) and of the Warning Event
// for a recorded state of the check run that does not pass, or "" for one that needs neither. Refused has its
// own, which says what the Git host shows (recordRefusal).
func reviewCheckWarning(state remediationv1alpha1.ReviewCheckState) (reason, message string) {
	if state == remediationv1alpha1.ReviewCheckFailed {
		return ReasonReviewListTooLong, "The pull request has more reviews than the merge gate reads, so the " +
			"check run shadewright/review failed on this head; a person decides the merge"
	}
	return "", ""
}

// staleReviewCheckWarning reports whether condition Approved of rr carries ReasonReviewCheckRefused or
// ReasonReviewListTooLong. Called when a state that needs neither (InProgress, Passed) is recorded: the reason was
// set on an earlier head (Failed changes only to Stale on its head), or by a refusal on this head that the Git host's answer
// now ends (#748). Either way it no longer says why this head is blocked.
func staleReviewCheckWarning(rr *remediationv1alpha1.RemediationRequest) bool {
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionApproved)
	return c != nil && (c.Reason == ReasonReviewCheckRefused || c.Reason == ReasonReviewListTooLong)
}

// steeredReviews is what steeredCommented holds per request: the reviews of hint authors whose comment was
// posted or found by this process.
type steeredReviews struct {
	uid types.UID
	ids []int64
}

// steeredAnswered reports whether this process already posted, or found, the comment on review id of rr.
func (r *RemediationRequestReconciler) steeredAnswered(rr *remediationv1alpha1.RemediationRequest, id int64) bool {
	v, _ := r.steeredCommented.Load(client.ObjectKeyFromObject(rr))
	s, ok := v.(steeredReviews)
	return ok && s.uid == rr.UID && slices.Contains(s.ids, id)
}

// answerSteered remembers that the comment on review id of rr is on the pull request. Reconciles of one request
// never overlap, so the read and the store below do not race.
func (r *RemediationRequestReconciler) answerSteered(rr *remediationv1alpha1.RemediationRequest, id int64) {
	key := client.ObjectKeyFromObject(rr)
	s := steeredReviews{uid: rr.UID}
	v, _ := r.steeredCommented.Load(key)
	if old, ok := v.(steeredReviews); ok && old.uid == rr.UID {
		s.ids = slices.Clone(old.ids)
	}
	s.ids = append(s.ids, id)
	r.steeredCommented.Store(key, s)
}
