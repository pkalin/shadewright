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
	"maps"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
)

// Replies to reviewer hints (ADR-0028 section 7, #542). While a request waits for review with its pull request
// open and the head Shadewright pushed, each look at the pull request replies, in that pull request and to the
// author, to every hint of status.hints that is Declined or Consumed and to every command of
// status.refusedHints, once:
//
//   - The reply is a fixed template filled with typed fields only: the author's login, the comment ID, the
//     reason (an enum), and for a hint this request answers the new plan's typed values and head. The one free
//     text is the stored hint's text (already redacted when it was accepted), quoted in a code span, cut to
//     maxReplyQuoteBytes, with format characters (bidi controls, zero width) turned into spaces, and left out
//     when it holds a control character or a comment opener. No model text goes into a reply.
//   - Fail closed: the whole reply, marker included, must be text the redactor would not change
//     (redact.Clean, keeping only the recorded head, a 40 hex digit commit ID). Otherwise nothing is posted,
//     and the record says why (NotRedactable).
//   - One reply per comment: the reply carries a marker naming the comment, and the provider posts nothing when
//     a comment of its own with that marker is on the pull request already, so a retry after a lost answer or a
//     lost status write, a restart, or a replacement that carries the hint (or the refused command) does not
//     post it twice. When the provider cannot read every comment to look for it, nothing is posted and the
//     record says so (TooManyComments). The record in status (reply) is the durable key: an entry with a reply
//     is never answered again.
//   - The write passes the kill switch (gatedGit): while writes are paused nothing is posted and nothing is
//     recorded, and the next look after the pause posts it. The status write that records the replies passes
//     it too.
//   - A post that fails for a reason worth retrying is tried again on later looks, at most maxReplyAttempts
//     times per process (memory only, so a restart allows that many again), then recorded GaveUp; the count is
//     dropped only once the record is written, so a lost status write does not allow more tries. A final
//     refusal of the Git host is recorded Refused at once. A pull request closed or merged meanwhile records
//     nothing and ends the look: the next look ends the request.
//   - A reply never changes a hint's state, the plan or the pull request: it only reports what happened.

// maxReplyAttempts is how many times one process tries to post one reply before it records GaveUp.
const maxReplyAttempts = 5

// maxRepliesPerLook bounds the replies one look posts: each asks the Git host a few times (the pull request,
// the pages it reads for the marker, the post), and the rest wait for the next look.
const maxRepliesPerLook = 5

// maxReplyQuoteBytes is the most bytes of the hint's text a reply quotes.
const maxReplyQuoteBytes = 200

// replyHeadPattern is the shape of a recorded head the redaction check keeps.
var replyHeadPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// replyTries is what replyAttempts holds per request: the failed posts of this process, by reply marker.
type replyTries struct {
	uid   types.UID
	tries map[string]int
}

// replyItem is one entry of status that waits for a reply: a hint or a refused command, never both.
type replyItem struct {
	hint    *remediationv1alpha1.Hint
	refused *remediationv1alpha1.RefusedHint
}

func (it replyItem) source() remediationv1alpha1.HintSource {
	if it.hint != nil {
		return it.hint.Source
	}
	return it.refused.Source
}

func (it replyItem) commentID() int64 {
	if it.hint != nil {
		return it.hint.CommentID
	}
	return it.refused.CommentID
}

func (it replyItem) author() remediationv1alpha1.HintAuthor {
	if it.hint != nil {
		return it.hint.Author
	}
	return it.refused.Author
}

func (it replyItem) reply() remediationv1alpha1.HintReply {
	if it.hint != nil {
		return it.hint.Reply
	}
	return it.refused.Reply
}

func (it replyItem) set(rec remediationv1alpha1.HintReply) {
	if it.hint != nil {
		it.hint.Reply = rec
		return
	}
	it.refused.Reply = rec
}

// replyItems returns the entries of st that wait for a reply: Declined and Consumed hints, then refused
// commands, each without a reply recorded.
func replyItems(st *remediationv1alpha1.RemediationRequestStatus) []replyItem {
	var out []replyItem
	for i := range st.Hints {
		h := &st.Hints[i]
		if h.Reply == "" && (h.State == remediationv1alpha1.HintDeclined || h.State == remediationv1alpha1.HintConsumed) {
			out = append(out, replyItem{hint: h})
		}
	}
	for i := range st.RefusedHints {
		if f := &st.RefusedHints[i]; f.Reply == "" {
			out = append(out, replyItem{refused: f})
		}
	}
	return out
}

// watchReplies runs replyHints after a look at the pull request. A failure is logged and changes nothing: the
// next look tries again. A cancelled reconcile is not logged.
func (r *RemediationRequestReconciler) watchReplies(ctx context.Context, look hintLook,
	rr *remediationv1alpha1.RemediationRequest) {
	if err := r.replyHints(ctx, look, rr); err != nil && ctx.Err() == nil {
		logf.FromContext(ctx).Info("Could not reply to the reviewer hints of RemediationRequest",
			"remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error())
	}
}

// replyHints posts the replies rr owes (see the top of this file), when reviewer hints are on, look found the
// pull request open with the head Shadewright pushed while rr waits for review, and no handoff intent of rr
// stands (the predecessor writes nothing to Git then). It records what became of each in one status update,
// through the kill switch, within a deadline of its own; rr is updated in place only when that write went
// through. A paused write stops it and records nothing: a reply posted before the pause is found by its marker on
// a later look and recorded then.
func (r *RemediationRequestReconciler) replyHints(ctx context.Context, look hintLook,
	rr *remediationv1alpha1.RemediationRequest) error {
	if !r.ReviewerHints || look.git == nil || rr.Status.Phase != remediationv1alpha1.PhaseAwaitingApproval ||
		rr.Status.PullRequest == nil || intent(rr) != nil {
		return nil
	}
	source, ok := hintSource(look.host)
	if !ok {
		return nil
	}
	upd := rr.DeepCopy()
	items := replyItems(&upd.Status)
	if len(items) == 0 {
		return nil
	}
	stepCtx, cancel := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	defer cancel()
	var stopErr error
	var recorded []replyItem
	tried := 0
	for _, it := range items {
		if it.source() != source {
			continue // a comment of another code host is not this provider's to answer
		}
		if tried >= maxRepliesPerLook {
			break
		}
		tried++
		rec, err := r.replyOne(stepCtx, look.git, upd, it)
		if errors.Is(err, errReplyPullRequestGone) {
			break // the next look ends the request; asking for the other replies would fail the same way
		}
		if err != nil {
			stopErr = err
			break
		}
		if rec != "" {
			it.set(rec)
			recorded = append(recorded, it)
		}
	}
	if len(recorded) == 0 {
		return stopErr
	}
	// A deadline of its own: posts that used up the step's time are still recorded, so their markers are not
	// the only record.
	storeCtx, cancelStore := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	defer cancelStore()
	if err := r.writeAllowed(storeCtx); err != nil {
		return errors.Join(stopErr, fmt.Errorf("record the replies to reviewer hints: %w", err))
	}
	if err := r.Status().Update(storeCtx, upd); err != nil {
		return errors.Join(stopErr, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace,
			rr.Name, err))
	}
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr))
	for _, it := range recorded {
		// The failed posts are forgotten only with the record: after a lost write the count still stands.
		r.forgetReplyTries(rr, replyMarker(it.source(), it.commentID()))
		log.Info("Recorded the reply to a reviewer hint of RemediationRequest", "comment", it.commentID(),
			"reply", string(it.reply()))
	}
	upd.DeepCopyInto(rr)
	return stopErr
}

// errReplyPullRequestGone ends a look's replies: the pull request was closed or merged meanwhile.
var errReplyPullRequestGone = errors.New("the pull request is no longer open")

// replyOne builds and posts the reply to it, and returns what to record: "" when nothing is to be recorded yet
// (a failure worth retrying, within maxReplyAttempts). The error is set only when the look has to stop: the
// kill switch held the write back, the time ran out, or the pull request was closed or merged meanwhile
// (errReplyPullRequestGone). The failed posts it counts are dropped by the caller once the record is written.
func (r *RemediationRequestReconciler) replyOne(ctx context.Context, git providers.GitProvider,
	rr *remediationv1alpha1.RemediationRequest, it replyItem) (remediationv1alpha1.HintReply, error) {
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(rr),
		"comment", it.commentID())
	mark := replyMarker(it.source(), it.commentID())
	if r.replyTries(rr, mark) >= maxReplyAttempts {
		// Given up before, and the record was not written: record it now, without another post.
		return remediationv1alpha1.HintReplyGaveUp, nil
	}
	body, rec := replyBody(rr, it, mark)
	if rec != "" {
		// Never sent: the log says why, without the text.
		log.Info("Did not post the reply to a reviewer hint of RemediationRequest", "reply", string(rec))
		return rec, nil
	}
	err := git.CommentChangeRequest(ctx, rr.Status.PullRequest.Number, mark, body)
	reason, _, final := refusal(err)
	switch {
	case err == nil:
		return remediationv1alpha1.HintReplyPosted, nil
	case errors.Is(err, freshness.ErrPaused) || errors.Is(err, freshness.ErrWriteBlocked):
		// Not a failure: it is posted once writes go on.
		return "", err
	case ctx.Err() != nil:
		return "", fmt.Errorf("reply to a reviewer hint stopped: %w", ctx.Err())
	case providers.IsConflict(err):
		// Closed or merged meanwhile: the next look at the pull request ends the request.
		return "", errReplyPullRequestGone
	case errors.Is(err, providers.ErrTooManyComments):
		log.Info("Did not post the reply to a reviewer hint of RemediationRequest: the pull request has more "+
			"comments than are read to find an earlier reply", "reply", string(remediationv1alpha1.HintReplyTooManyComments))
		return remediationv1alpha1.HintReplyTooManyComments, nil
	case final:
		log.Info("The Git host refused the reply to a reviewer hint of RemediationRequest; it is not asked again",
			"reason", reason)
		return remediationv1alpha1.HintReplyRefused, nil
	}
	if n := r.countReplyTry(rr, mark); n < maxReplyAttempts {
		log.Info("Could not post the reply to a reviewer hint of RemediationRequest; it is tried again",
			"attempt", n, "error", err.Error())
		return "", nil
	}
	log.Info("Gave up on the reply to a reviewer hint of RemediationRequest", "attempts", maxReplyAttempts,
		"error", err.Error())
	return remediationv1alpha1.HintReplyGaveUp, nil
}

// replyMarker is the hidden line that ties a reply to the comment it answers, the same for every request of a
// supersede chain, so the pull request gets one reply per comment.
func replyMarker(source remediationv1alpha1.HintSource, comment int64) string {
	return fmt.Sprintf("<!-- shadewright hint-reply source=%s comment=%d -->", source, comment)
}

// replyBody builds the reply to it with mark as its last line, and checks it: a reply the redactor would
// change gives NotRedactable, and no body. The only kept string is rr's recorded head, when it is a commit ID.
// Its parts are bounded (a login, IDs, a quote of at most maxReplyQuoteBytes, the target's names), so it stays
// far below the provider's limit for a comment.
func replyBody(rr *remediationv1alpha1.RemediationRequest, it replyItem, mark string) (
	string, remediationv1alpha1.HintReply) {
	var b strings.Builder
	fmt.Fprintf(&b, "@%s, about your reviewer hint in comment %d", it.author().Login, it.commentID())
	if it.hint != nil {
		if q := replyQuote(it.hint.Text); q != "" {
			b.WriteString(", " + q)
		}
	}
	b.WriteString(":\n\n")
	b.WriteString(replyOutcome(rr, it))
	b.WriteString("\n\n" + mark + "\n")
	body := b.String()
	var keep []string
	if head := rr.Status.PullRequest.HeadSHA; replyHeadPattern.MatchString(head) {
		keep = append(keep, head)
	}
	if !redact.Clean(body, keep...) {
		return "", remediationv1alpha1.HintReplyNotRedactable
	}
	return body, ""
}

// replyQuote returns the hint's text as a Markdown code span, so it neither mentions anyone nor links
// anything, cut to maxReplyQuoteBytes on a character boundary; "" when there is none, or when it holds a
// control character or a comment opener or closer, which could hide or forge a marker. Format characters
// (bidi controls, zero width, Unicode category Cf) become spaces, as in cluster text before redaction, so the
// quote cannot reorder or hide what the reply around it shows.
func replyQuote(text string) string {
	if text == "" || !utf8.ValidString(text) || strings.ContainsFunc(text, unicode.IsControl) ||
		strings.Contains(text, "<!--") || strings.Contains(text, "-->") {
		return ""
	}
	q := strings.Map(func(c rune) rune {
		if unicode.Is(unicode.Cf, c) {
			return ' '
		}
		return c
	}, text)
	if len(q) > maxReplyQuoteBytes {
		cut := maxReplyQuoteBytes
		for cut > 0 && !utf8.RuneStart(q[cut]) {
			cut--
		}
		q = q[:cut] + "..."
	}
	// A run of backticks longer than any in the text opens and closes the span; the spaces keep a backtick at
	// either end of the text inside it.
	longest, run := 0, 0
	for _, c := range q {
		if c == '`' {
			run++
			longest = max(longest, run)
			continue
		}
		run = 0
	}
	fence := strings.Repeat("`", longest+1)
	return fence + " " + q + " " + fence
}

// replyOutcome is the sentence of the reply that says what happened, from typed fields only.
func replyOutcome(rr *remediationv1alpha1.RemediationRequest, it replyItem) string {
	if it.refused != nil {
		return fmt.Sprintf("Not stored: %s (reason `%s`). No hint was stored, and this pull request keeps its "+
			"plan.", refusalText(it.refused.Reason), it.refused.Reason)
	}
	h := it.hint
	if h.State == remediationv1alpha1.HintConsumed {
		rep := rr.Spec.Replaces
		if rep == nil || rep.Hint.Source != h.Source || rep.Hint.CommentID != h.CommentID {
			return "Applied earlier: a new plan replaced the one of this pull request after your hint, and a " +
				"later hint has replaced that plan since. The check shadewright/review needs an approval of the " +
				"latest head by someone who wrote no hint in this pull request."
		}
		what, _, desired := change(rr)
		t := rr.Spec.TargetRef
		return fmt.Sprintf("Applied: this pull request now holds a new plan, which sets %s of %s `%s/%s` to "+
			"`%s`, at head `%s`. The check shadewright/review needs a new approval of that head by someone who "+
			"wrote no hint in this pull request.", what, t.Kind, t.Namespace, t.Name, desired,
			rr.Status.PullRequest.HeadSHA)
	}
	return fmt.Sprintf("Not applied: %s (reason `%s`). This pull request keeps its plan.", declineText(h.Reason),
		h.Reason)
}

// declineText says why a hint was declined, from its reason.
func declineText(reason string) string {
	switch reason {
	case "NoModelConfigured":
		return "no model is configured for the analysis"
	case "BudgetSpent", "BudgetUnavailable":
		return "the analysis budget is spent or could not be read"
	case "CoolingDown":
		return "the Incident was analyzed shortly before, and the cooldown still runs"
	case "NoProposal", "MissingData", "NoEvidence", "TargetNotSupported", "PreviousValueUnknown",
		"EscalationNotConfigured":
		return "the new analysis made no proposal"
	case "NoChange", "DirectionReversed", "PlanUnchanged":
		return "the new analysis proposed no different plan"
	case "NoPolicyForAction", "ActionOutOfBounds", "ReplacementDenied":
		return "policy refused the new proposal"
	case "InvalidOutput", "StepLimit", "TokenLimit", "DeadlineExceeded", "ModelUnavailable", "ModelNotFound",
		"ModelForbidden", "ModelError", "UnexpectedModel", "EndpointNotAllowed":
		return "the analysis failed or hit a limit"
	case "HintNotPending", "RequestNotCreated":
		return "the analysis could not create a replacement"
	case ReasonHintNoAnswer:
		return "the analysis gave no answer in time"
	case "HandoffAborted":
		return "the new plan could not take this pull request over"
	case ReasonHintPullRequestMerged, ReasonHintPullRequestClosed, ReasonHintRequestEnded:
		return "the request of this pull request ended first"
	}
	return "it was not applied"
}

// refusalText says why no hint was stored for a command, from its reason.
func refusalText(reason remediationv1alpha1.HintRefusal) string {
	switch reason {
	case remediationv1alpha1.HintRefusalCapReached:
		return fmt.Sprintf("this pull request already used the %d hints it may consume", remediationv1alpha1.MaxHints)
	case remediationv1alpha1.HintRefusalTooLong:
		return fmt.Sprintf("the hint is longer than %d bytes", remediationv1alpha1.MaxHintTextBytes)
	case remediationv1alpha1.HintRefusalNotRedactable:
		return "the hint could not be stored safely after redaction"
	case remediationv1alpha1.HintRefusalEmpty:
		return "the command has no text"
	case remediationv1alpha1.HintRefusalEditedAfterSeen:
		return "the command was added to a comment by an edit; post it as a new comment"
	}
	return "it could not be stored"
}

// countReplyTry counts one more failed post of the reply with marker mark for rr, and returns the count.
// Reconciles of one request never overlap, so the read and the store below do not race.
func (r *RemediationRequestReconciler) countReplyTry(rr *remediationv1alpha1.RemediationRequest, mark string) int {
	key := client.ObjectKeyFromObject(rr)
	t := replyTries{uid: rr.UID, tries: map[string]int{}}
	if v, ok := r.replyAttempts.Load(key); ok {
		if old, ok := v.(replyTries); ok && old.uid == rr.UID {
			maps.Copy(t.tries, old.tries)
		}
	}
	t.tries[mark]++
	r.replyAttempts.Store(key, t)
	return t.tries[mark]
}

// replyTries returns the failed posts this process counted for the reply with marker mark of rr.
func (r *RemediationRequestReconciler) replyTries(rr *remediationv1alpha1.RemediationRequest, mark string) int {
	if v, ok := r.replyAttempts.Load(client.ObjectKeyFromObject(rr)); ok {
		if t, ok := v.(replyTries); ok && t.uid == rr.UID {
			return t.tries[mark]
		}
	}
	return 0
}

// forgetReplyTries drops the failed posts counted for the reply with marker mark of rr.
func (r *RemediationRequestReconciler) forgetReplyTries(rr *remediationv1alpha1.RemediationRequest, mark string) {
	key := client.ObjectKeyFromObject(rr)
	v, ok := r.replyAttempts.Load(key)
	if !ok {
		return
	}
	old, ok := v.(replyTries)
	if !ok || old.uid != rr.UID {
		return
	}
	if _, counted := old.tries[mark]; !counted {
		return
	}
	t := replyTries{uid: old.uid, tries: maps.Clone(old.tries)}
	delete(t.tries, mark)
	r.replyAttempts.Store(key, t)
}
