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
//   - A list of reviews too long to read in one gate (maxReviewLooks) completes the check with failure and is
//     recorded as Failed, with condition Approved False and a Warning Event, both with reason
//     ReviewListTooLong: the gate fails closed on that head, never passes on a list it did not read whole, and
//     does not read the list again on that head.
//   - A final refusal (InProgress, Passed or Failed) is recorded as Refused, with condition Approved False and a
//     Warning Event, both with reason ReviewCheckRefused, so a pull request the required check blocks says why.
//
// A restart between a write to the Git host and the status write creates the check again on the same head: a
// check created in progress after one that passed makes the head wait again until the gate passes it again,
// which it does on the same look. The check never passes more than the gate decided.

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

// watchReviewCheck runs reviewCheck after a look at the pull request found it open with the recorded head;
// caughtUp says whether that look read the comments to the end and stored them. A failure is logged and
// changes nothing: the next look tries again. A cancelled reconcile is not logged.
func (r *RemediationRequestReconciler) watchReviewCheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	caughtUp bool) {
	if err := r.reviewCheck(ctx, rr, caughtUp); err != nil && ctx.Err() == nil {
		logf.FromContext(ctx).Info("Could not update the review check of RemediationRequest",
			"remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error())
	}
}

// reviewCheck creates the check run in progress on a recorded head that has no state for it, and runs the merge
// gate on one in progress while the request waits for review, only when caughtUp. A request whose handoff
// intent stands leaves both to its replacement. rr is updated in place by each status write that went through.
func (r *RemediationRequestReconciler) reviewCheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	caughtUp bool) error {
	pr := rr.Status.PullRequest
	if !r.ReviewerHints || pr == nil || intent(rr) != nil || !progresses(rr) ||
		(rr.Status.Phase != remediationv1alpha1.PhasePRCreated && rr.Status.Phase != remediationv1alpha1.PhaseAwaitingApproval) {
		return nil
	}
	rc := pr.ReviewCheck
	current := rc != nil && rc.Head == pr.HeadSHA
	gate := caughtUp && rr.Status.Phase == remediationv1alpha1.PhaseAwaitingApproval
	if current && (rc.State != remediationv1alpha1.ReviewCheckInProgress || !gate) {
		return nil
	}
	stepCtx, cancel := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	defer cancel()
	git, err := r.openPlanRepository(stepCtx, rr)
	if err != nil {
		return err
	}
	if !current {
		err := git.CreateCheckRun(stepCtx, pr.HeadSHA, providers.CheckRun{Name: providers.CheckRunName,
			Status: providers.CheckRunInProgress, Title: checkRunTitle, Summary: checkRunSummary})
		step, err := stepOutcome(logf.FromContext(ctx), err, "create the check run")
		switch {
		case err != nil:
			return freshness.WrapProvider("create the check run", err)
		case step == "":
			// Closed or merged meanwhile: the next look at the pull request decides.
			return nil
		}
		state := remediationv1alpha1.ReviewCheckInProgress
		if step == remediationv1alpha1.HandoffStepRefused {
			state = remediationv1alpha1.ReviewCheckRefused
		}
		if err := r.recordReviewCheck(stepCtx, rr, state); err != nil {
			return err
		}
		if state != remediationv1alpha1.ReviewCheckInProgress || !gate {
			return nil
		}
	}
	return r.reviewGate(stepCtx, git, rr)
}

// recordReviewCheck records state for the recorded head in rr's status, through the kill switch; Refused and
// Failed also set condition Approved False with their reason (reviewCheckWarning) and, once written, record a
// Warning Event. rr is changed only when the write went through.
func (r *RemediationRequestReconciler) recordReviewCheck(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	state remediationv1alpha1.ReviewCheckState) error {
	if err := r.writeAllowed(ctx); err != nil {
		return err
	}
	upd := rr.DeepCopy()
	head := upd.Status.PullRequest.HeadSHA
	upd.Status.PullRequest.ReviewCheck = &remediationv1alpha1.ReviewCheck{Head: head, State: state}
	reason, message := reviewCheckWarning(state)
	if reason != "" {
		meta.SetStatusCondition(&upd.Status.Conditions, metav1.Condition{
			Type:               remediationv1alpha1.ConditionApproved,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: upd.Generation,
		})
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

// reviewGate is the merge gate of a request waiting for review whose check run on the recorded head is in
// progress: it reads every review of the pull request and, when the latest review of a person who wrote no hint
// in the chain (compared by user ID) approves the recorded head, completes the check run with success and
// records it Passed. An approval of an earlier head never counts. An approval of the head by a hint author does
// not count either, and gets a comment saying why, once per review (by its marker); this process asks for it
// once per review, not on every look, as each ask reads the comments for the marker, and not again after a
// final refusal. The comments get half of the time left, so one that runs out of time never keeps a passing
// gate from completing the check; the rest are asked on a later look. A list it cannot read whole completes
// the check with failure and records it Failed (fail closed), so that head is not read again.
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
			if it.Kind != providers.FeedbackReview || it.Author.Kind != providers.AuthorPerson {
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
		return r.completeReviewCheck(ctx, git, rr, remediationv1alpha1.ReviewCheckFailed)
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
	if !passed {
		return nil
	}
	return r.completeReviewCheck(ctx, git, rr, remediationv1alpha1.ReviewCheckPassed)
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
		default:
			log.Info("Could not say why an approval does not count for RemediationRequest", "review", id,
				"error", err.Error())
		}
	}
	return nil
}

// completeReviewCheck completes the check run on the recorded head as state (Passed with success, Failed with
// failure) and records it; a final refusal is recorded Refused, and a pull request closed or merged meanwhile
// (a conflict) records nothing.
func (r *RemediationRequestReconciler) completeReviewCheck(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, state remediationv1alpha1.ReviewCheckState) error {
	run := providers.CheckRun{Name: providers.CheckRunName, Status: providers.CheckRunSuccess,
		Title: checkRunPassedTitle, Summary: checkRunPassedSummary}
	if state == remediationv1alpha1.ReviewCheckFailed {
		run = providers.CheckRun{Name: providers.CheckRunName, Status: providers.CheckRunFailure,
			Title: checkRunFailedTitle, Summary: checkRunFailedSummary}
	}
	err := git.CreateCheckRun(ctx, rr.Status.PullRequest.HeadSHA, run)
	step, err := stepOutcome(logf.FromContext(ctx), err, "complete the check run")
	switch {
	case err != nil:
		return freshness.WrapProvider("complete the check run", err)
	case step == "":
		return nil
	case step == remediationv1alpha1.HandoffStepRefused:
		state = remediationv1alpha1.ReviewCheckRefused
	}
	return r.recordReviewCheck(ctx, rr, state)
}

// ReasonReviewListTooLong is the reason of condition Approved (False) and of the Warning Event when the reviews
// of a pull request were too many for the merge gate to read: it failed the check run on the head, so a ruleset
// that requires it blocks the merge until a person acts.
const ReasonReviewListTooLong = "ReviewListTooLong"

// reviewCheckWarning returns the reason and fixed message of condition Approved (False) and of the Warning Event
// for a state of the check run that does not pass, or "" for one that needs neither.
func reviewCheckWarning(state remediationv1alpha1.ReviewCheckState) (reason, message string) {
	switch state {
	case remediationv1alpha1.ReviewCheckRefused:
		return ReasonReviewCheckRefused, "The Git host refused the check run shadewright/review on the pull " +
			"request's head for good; a ruleset that requires it blocks the merge until a person acts (the App's " +
			"checks permission, a rule)"
	case remediationv1alpha1.ReviewCheckFailed:
		return ReasonReviewListTooLong, "The pull request has more reviews than the merge gate reads, so the " +
			"check run shadewright/review failed on this head; a person decides the merge"
	}
	return "", ""
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
