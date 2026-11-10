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
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
)

// Reviewer hints, the executor's side (ADR-0028 sections 2 and 3, #538). On each look at a pull request that
// waits for review (AwaitingApproval, open, with the head Shadewright pushed), the executor reads the
// conversation comments of that pull request from its durable cursor (status.commentCursor) through the
// provider's FeedbackSource, one bounded look per poll, and handles each comment once:
//
//   - Only a person's comment counts: bots and Apps (the executor's own comments among them) are ignored.
//   - The command is the first line only, starting exactly with hintCommand; its text is the rest of that
//     line. Anything else is not a command and is ignored.
//   - A comment already stored as a hint is never handled again; the first edit seen (another updated time)
//     sets the hint's editedAfterAccept, once, and the stored hint stays as it is. A command the cursor lists
//     as seen with the same updated time, or under the watermark of the ones pushed out of seen by its bound,
//     was judged already with this text and is not judged again, so the overlap of two sweeps costs nothing and
//     a verdict that would differ now (CapReached) is not reached again; status.refusedHints is only a record,
//     never the key.
//   - When status.hints holds MaxStoredHints entries, a further command is not stored and not refused: it
//     stays unjudged, the cursor passes it, and a log line counts it.
//   - Then the code host is asked whether the author may write, at most maxPermissionAsks authors per look.
//     A refusal or an unknown author ends it there: such commands are counted in a log line, never recorded,
//     so a person without write access cannot push records out of status.refusedHints. A failed question, or
//     one beyond the bound, stops the look at that comment: the comments before it are judged and stored, and
//     the cursor moves to just after the last of them (FeedbackPage.Resume), so every look makes progress.
//   - A writer's command is refused when it has no text (Empty) or was cut by the host (TooLong), when the
//     comment was created at or before the cursor's since, so it was read by an earlier look and edited later to
//     carry the command (EditedAfterSeen), and when MaxHints of the request's hints are Consumed or Reserved
//     (CapReached): the hints of the whole supersede chain are carried into it (#539); Pending and Declined
//     hints do not count. The text is redacted (internal/redact) before anything is stored, and refused when
//     the redacted text is not clean (NotRedactable) or longer than MaxHintTextBytes (TooLong).
//   - An accepted hint is pinned (comment ID, digest of the stored text, the comment's updated time) in
//     status.hints, Pending.
//
// The same look answers Pending hints from the Incident the request names (answerHints). Whatever ends the
// request (a merge, a close, a moved head, a stale plan) declines its Pending hints in the status write that
// records the end: setPhase calls declineOnEnd. Everything else goes into
// the request's status in one update, with the resourceVersion read, and only after the kill switch allowed
// it: a lost or refused write stores nothing, and the next look reads the same comments again and makes the
// same decisions (invariant 5). Nothing here writes to Git.

// hintCommand starts the first line of a comment that gives a hint.
const hintCommand = "/shadewright reanalyze:"

// HintAnswerTimeout is how long a Pending hint may wait for an answer of the analysis, from its acceptance:
// with neither an outcome in the Incident nor a replacement by then, it is Declined with ReasonHintNoAnswer
// (ADR-0028 section 3). An analysis takes at most the analysis timeout (10 minutes by default), a lost one is
// run again after the cooldown (15 minutes), and hints are answered one at a time, so an hour leaves room for
// one lost run and the hint before it.
const HintAnswerTimeout = time.Hour

// ReasonHintNoAnswer: a Pending hint got no answer of the analysis within HintAnswerTimeout.
const ReasonHintNoAnswer = "NoAnswer"

// ReasonHintPullRequestMerged and ReasonHintPullRequestClosed: the pull request of a request with a Pending
// hint was merged, or closed without a merge, so no replacement can take it over (ADR-0028 section 4).
const (
	ReasonHintPullRequestMerged = "PullRequestMerged"
	ReasonHintPullRequestClosed = "PullRequestClosed"
	// ReasonHintRequestEnded: the request ended for another reason (a moved head, a stale plan, a cancel), so
	// no replacement can take its pull request over.
	ReasonHintRequestEnded = "RequestEnded"
)

// ReasonHintPermissionUnknown is the reason of the Warning Event about a request whose look at the comments
// stops at a comment because the code host did not answer whether its author may write.
const ReasonHintPermissionUnknown = "HintPermissionUnknown"

// maxPermissionAsks is how many authors one look asks the code host about. Anyone who can comment on the
// pull request can post the command, so a look full of them must not turn into a request per comment.
const maxPermissionAsks = 10

// The Incident's Analyzed condition and the outcome reasons with which the analysis creates a replacement
// request (internal/incidents and internal/analysis, which the executor does not import; a test on each
// side pins the same values).
const (
	conditionIncidentAnalyzed = "Analyzed"
	outcomeProposed           = "Proposed"
	outcomeRunbookMatched     = "RunbookMatched"
)

// hintLoginPattern is the pattern of HintAuthor.login.
var hintLoginPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// hintClean checks redacted hint text. The redactor's own output is clean, so it refuses only when the
// redactor misbehaves; tests replace it to prove the refusal.
var hintClean = func(s string) bool { return redact.Clean(s) }

// maxCursorPage is the largest page status.commentCursor holds (its schema's maximum).
const maxCursorPage = 100000

// hintSource is the HintSource of the comments of a provider's change requests.
func hintSource(host remediationv1alpha1.GitProviderType) (remediationv1alpha1.HintSource, bool) {
	if host == remediationv1alpha1.GitProviderGitHub {
		return remediationv1alpha1.HintSourceGitHub, true
	}
	return "", false
}

// feedbackOf returns the FeedbackSource of a provider the reconciler opened, when it reads feedback.
func feedbackOf(git providers.GitProvider) (providers.FeedbackSource, bool) {
	if g, ok := git.(*gatedGit); ok {
		return g.feedback()
	}
	return nil, false
}

// hintLook is what watchStep found for the reviewer hints: the provider to read the comments with, set only
// when the pull request was just seen open with Shadewright's head and the request waits for review.
type hintLook struct {
	git  providers.GitProvider
	host remediationv1alpha1.GitProviderType
}

// watchHints runs pollHints after each look at the pull request of a request that may still be reviewed, with
// the reconcile's context (pollHints bounds its steps itself). A failure is logged and changes nothing: the
// watch of the pull request goes on, and the next look tries again from the stored cursor. It returns pollHints'
// caughtUp, false on a failure.
func (r *RemediationRequestReconciler) watchHints(ctx context.Context, look hintLook,
	rr *remediationv1alpha1.RemediationRequest) bool {
	caughtUp, err := r.pollHints(ctx, look, rr)
	if err != nil {
		logf.FromContext(ctx).Info("Could not handle the reviewer hints of RemediationRequest",
			"remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error())
		return false
	}
	return caughtUp
}

// pollHints is one look at the comments of rr's pull request, when look has a provider and reviewer hints are
// on, and the answers of its Pending hints (with their timeout) in any case, written in one status update. rr
// is updated in place only when that write succeeded. ctx is the reconcile's context: the look and the rest
// (the answers, the kill switch read and the status update) each get a deadline of their own
// (WriteTimeout), so a look at the comments that keeps using up its deadline never keeps an overdue hint
// Pending. caughtUp is true only when the look read the comments to the end, judged every one and the status
// holds what it judged: the merge gate decides only then (reviewCheck).
func (r *RemediationRequestReconciler) pollHints(ctx context.Context, look hintLook,
	rr *remediationv1alpha1.RemediationRequest) (caughtUp bool, err error) {
	now := r.now()
	upd := rr.DeepCopy()
	var lookErr error
	looked := false
	if source, fs, ok := r.feedbackFor(look, rr); ok {
		lookCtx, cancel := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
		cur := cursorOf(upd.Status.CommentCursor)
		page, err := fs.ListComments(lookCtx, upd.Status.PullRequest.Number, cur)
		if err != nil {
			lookErr = fmt.Errorf("list the comments of the pull request: %w", err)
		} else {
			looked, lookErr = r.lookComments(lookCtx, fs, upd, source, cur, page, now)
		}
		cancel()
	}
	storeCtx, cancel := bound(ctx, r.WriteTimeout, DefaultWriteTimeout)
	defer cancel()
	r.answerHints(storeCtx, upd, now)
	r.settleHandoff(storeCtx, upd)
	looked = looked && lookErr == nil
	if equality.Semantic.DeepEqual(rr.Status, upd.Status) {
		return looked, lookErr
	}
	// The kill switch covers this status write too (ADR-0028 section 4): while writes are paused, nothing is
	// stored, and the next look after the pause makes the same decisions.
	if err := r.writeAllowed(storeCtx); err != nil {
		return false, errors.Join(lookErr, fmt.Errorf("store reviewer hints: %w", err))
	}
	if err := r.Status().Update(storeCtx, upd); err != nil {
		return false, errors.Join(lookErr, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace,
			rr.Name, err))
	}
	upd.DeepCopyInto(rr)
	return looked, lookErr
}

// feedbackFor returns the source and the FeedbackSource to read rr's comments with: only when reviewer hints
// are on, look has a provider that reads feedback, and rr waits for review with a pull request.
func (r *RemediationRequestReconciler) feedbackFor(look hintLook, rr *remediationv1alpha1.RemediationRequest) (
	remediationv1alpha1.HintSource, providers.FeedbackSource, bool) {
	if !r.ReviewerHints || look.git == nil || rr.Status.Phase != remediationv1alpha1.PhaseAwaitingApproval ||
		rr.Status.PullRequest == nil {
		return "", nil, false
	}
	source, ok := hintSource(look.host)
	if !ok {
		return "", nil, false
	}
	fs, ok := feedbackOf(look.git)
	if !ok {
		return "", nil, false // a provider that reads no feedback has no hints
	}
	return source, fs, true
}

// lookComments judges the comments of one look (takeComments) into upd's status and moves the cursor past the
// comments it judged, in upd's status too: to page.Next when it judged them all, or just after the last one it
// judged when it stopped (page.Resume), so every look makes progress, whatever the record of refusals keeps.
// caughtUp is true when it judged every comment of a page that reached the end of what the host had.
func (r *RemediationRequestReconciler) lookComments(ctx context.Context, fs providers.FeedbackSource,
	upd *remediationv1alpha1.RemediationRequest, source remediationv1alpha1.HintSource,
	cur providers.FeedbackCursor, page providers.FeedbackPage, now time.Time) (caughtUp bool, err error) {
	before := seenOf(upd.Status.CommentCursor)
	var judged []remediationv1alpha1.SeenComment
	n, err := r.takeComments(ctx, fs, upd, source, cur, page.Items, before, &judged, now)
	var next *remediationv1alpha1.CommentCursor
	switch {
	case n == len(page.Items):
		next = cursorStatus(page.Next)
	case n > 0 && len(page.Resume) == len(page.Items):
		next = cursorStatus(page.Resume[n-1])
	default:
		return false, err
	}
	var since time.Time
	if next.Since != nil {
		since = next.Since.Time
	}
	kept, evicted := mergeSeen(before.seen, judged, since)
	next.Seen = kept
	at, id := before.evictedAt, before.evictedID
	for _, e := range evicted {
		if e.UpdatedAt.After(at) {
			at = e.UpdatedAt.Time
		}
		id = max(id, e.CommentID)
	}
	if id > 0 && !at.Before(since) {
		// Kept while a sweep from since may return an evicted command again; it only grows until then.
		next.EvictedAt, next.EvictedID = new(metav1.NewTime(at)), id
	}
	upd.Status.CommentCursor = next
	return err == nil && n == len(page.Items) && page.Complete, err
}

// seenSet is what a look knows was judged already: the seen comments of the cursor, and the watermark of the
// commands pushed out of them by the bound (evictedAt, evictedID).
type seenSet struct {
	seen      []remediationv1alpha1.SeenComment
	evictedAt time.Time
	evictedID int64
}

// seenOf reads the seen comments and the watermark of a stored cursor.
func seenOf(c *remediationv1alpha1.CommentCursor) seenSet {
	if c == nil {
		return seenSet{}
	}
	out := seenSet{seen: c.Seen}
	if c.EvictedAt != nil && c.EvictedID > 0 {
		out.evictedAt, out.evictedID = c.EvictedAt.Time, c.EvictedID
	}
	return out
}

// has reports whether c was judged already with this very text: it is in seen with the same update time, or it
// is under the watermark of the evicted ones (an ID up to evictedID, an update time up to evictedAt).
func (s seenSet) has(c providers.Feedback) bool {
	at := c.UpdatedAt.Truncate(time.Second)
	if s.evictedID > 0 && c.ID <= s.evictedID && !at.After(s.evictedAt) {
		return true
	}
	for _, e := range s.seen {
		if e.CommentID == c.ID && e.UpdatedAt.Time.Equal(at) {
			return true
		}
	}
	return false
}

// mergeSeen returns the seen comments a later look may return again unchanged: those of old and judged updated at
// or after since (an older one comes back only edited, with another update time), one per comment at its newest
// update time, at most MaxSeenComments of the newest, in comment ID order. The ones that do not fit are returned
// as evicted, for the watermark that keeps them judged.
func mergeSeen(old, judged []remediationv1alpha1.SeenComment, since time.Time) (
	kept, evicted []remediationv1alpha1.SeenComment) {
	newest := map[int64]metav1.Time{}
	for _, s := range slices.Concat(old, judged) {
		if s.UpdatedAt.Time.Before(since) {
			continue
		}
		if t, ok := newest[s.CommentID]; !ok || s.UpdatedAt.After(t.Time) {
			newest[s.CommentID] = s.UpdatedAt
		}
	}
	out := make([]remediationv1alpha1.SeenComment, 0, len(newest))
	for id, at := range newest {
		out = append(out, remediationv1alpha1.SeenComment{CommentID: id, UpdatedAt: at})
	}
	// Newest first (then the higher ID) to cut to the bound, then in ID order for a stable status.
	slices.SortFunc(out, func(a, b remediationv1alpha1.SeenComment) int {
		if c := b.UpdatedAt.Compare(a.UpdatedAt.Time); c != 0 {
			return c
		}
		return cmp.Compare(b.CommentID, a.CommentID)
	})
	cut := min(len(out), remediationv1alpha1.MaxSeenComments)
	kept, evicted = out[:cut], out[cut:]
	slices.SortFunc(kept, func(a, b remediationv1alpha1.SeenComment) int { return cmp.Compare(a.CommentID, b.CommentID) })
	if len(kept) == 0 {
		kept = nil
	}
	if len(evicted) == 0 {
		evicted = nil
	}
	return kept, evicted
}

// takeComments judges the comments of one look in order, into upd's status (see the top of this file), and
// returns how many of them it judged: all of them, or fewer when it stopped at a comment whose author would be
// one more than maxPermissionAsks to ask about (no error: a later look judges it) or at a permission question
// that failed (its error). The comments before that one are judged. A command seenBefore has is not judged
// again; each command it reaches a verdict on is appended to judged.
func (r *RemediationRequestReconciler) takeComments(ctx context.Context, fs providers.FeedbackSource,
	upd *remediationv1alpha1.RemediationRequest, source remediationv1alpha1.HintSource,
	cur providers.FeedbackCursor, items []providers.Feedback, seenBefore seenSet,
	judged *[]remediationv1alpha1.SeenComment, now time.Time) (int, error) {
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(upd))
	st := &upd.Status
	asked := map[int64]bool{}
	unstored, outsiders := 0, 0
	defer func() {
		if unstored > 0 {
			log.Info("Did not store reviewer hints of RemediationRequest: its list of hints is full",
				"comments", unstored, "maxStoredHints", remediationv1alpha1.MaxStoredHints)
		}
		if outsiders > 0 {
			log.Info("Ignored reviewer hint commands of authors without write access for RemediationRequest",
				"comments", outsiders)
		}
	}()
	for i, c := range items {
		if c.Kind != providers.FeedbackComment || c.Author.Kind != providers.AuthorPerson || c.ID < 1 ||
			c.Author.ID < 1 || !hintLoginPattern.MatchString(c.Author.Login) {
			continue
		}
		refuse := func(why remediationv1alpha1.HintRefusal) {
			switch {
			case refuseHint(st, source, c, why, now):
				log.Info("Refused a reviewer hint for RemediationRequest", "comment", c.ID,
					"author", c.Author.ID, "reason", string(why))
			case !findRefused(st.RefusedHints, source, c.ID):
				log.Info("Refused a reviewer hint for RemediationRequest without a record: every recorded "+
					"refusal still waits for its reply", "comment", c.ID, "author", c.Author.ID,
					"reason", string(why), "max", remediationv1alpha1.MaxRefusedHints)
			}
		}
		if h := findHint(st.Hints, source, c.ID); h != nil {
			if !h.EditedAfterAccept && !c.UpdatedAt.Truncate(time.Second).Equal(h.UpdatedAt.Time) {
				// Recorded once, on the hint itself: further edits change nothing and take no record.
				h.EditedAfterAccept = true
				log.Info("Saw the comment of a stored reviewer hint edited for RemediationRequest; the hint stays",
					"comment", c.ID)
			}
			continue
		}
		text, isCommand, why := parseHintCommand(c.Body, c.Truncated)
		if !isCommand {
			continue
		}
		if seenBefore.has(c) {
			// Judged already with this very text: the overlap of two sweeps returned it again.
			continue
		}
		// mark records the comment as judged with this text, once the look has a verdict on it.
		mark := func() {
			*judged = append(*judged, remediationv1alpha1.SeenComment{CommentID: c.ID,
				UpdatedAt: metav1.NewTime(c.UpdatedAt.UTC().Truncate(time.Second))})
		}
		if len(st.Hints) >= remediationv1alpha1.MaxStoredHints {
			// Nothing more can be stored; the comment stays unjudged and the cursor passes it (no record, so no reply).
			unstored++
			mark()
			continue
		}
		// The author first: only a writer's command is judged further and recorded, so a person without write
		// access cannot push records out of status.refusedHints; theirs are only counted.
		may, ok := asked[c.Author.ID]
		if !ok {
			if len(asked) >= maxPermissionAsks {
				// The bound limits the questions of one look, not which comments are judged: this one and the
				// ones after it stay unjudged, the cursor stops before it, and the next look asks.
				return i, nil
			}
			var err error
			if may, err = fs.CanWrite(ctx, c.Author); err != nil {
				if ctx.Err() == nil {
					r.warnPermissionUnknown(upd, c.ID)
				}
				return i, fmt.Errorf("ask whether the author of comment %d may write: %w", c.ID, err)
			}
			asked[c.Author.ID] = may
		}
		mark()
		if !may {
			outsiders++
			continue
		}
		switch {
		case why != "":
			refuse(why)
			continue
		case !cur.Since.IsZero() && !c.CreatedAt.After(cur.Since):
			// Created at or before since, so an earlier look returned it: it was edited to carry the command.
			refuse(remediationv1alpha1.HintRefusalEditedAfterSeen)
			continue
		case chainHints(st.Hints) >= remediationv1alpha1.MaxHints:
			refuse(remediationv1alpha1.HintRefusalCapReached)
			continue
		}
		stored := redact.String(text)
		switch {
		case !hintClean(stored):
			refuse(remediationv1alpha1.HintRefusalNotRedactable)
			continue
		case len(stored) > remediationv1alpha1.MaxHintTextBytes:
			refuse(remediationv1alpha1.HintRefusalTooLong)
			continue
		}
		sum := sha256.Sum256([]byte(stored))
		st.Hints = append(st.Hints, remediationv1alpha1.Hint{
			Source: source, CommentID: c.ID,
			Author: remediationv1alpha1.HintAuthor{ID: c.Author.ID, Login: c.Author.Login},
			Text:   stored, Digest: "sha256:" + hex.EncodeToString(sum[:]),
			UpdatedAt:  metav1.NewTime(c.UpdatedAt.UTC().Truncate(time.Second)),
			AcceptedAt: metav1.NewTime(now.UTC().Truncate(time.Second)),
			State:      remediationv1alpha1.HintPending,
		})
		log.Info("Accepted a reviewer hint for RemediationRequest", "comment", c.ID, "author", c.Author.ID)
	}
	return len(items), nil
}

// permissionWarning is the comment a Warning Event of ReasonHintPermissionUnknown was recorded for, per request.
type permissionWarning struct {
	uid     types.UID
	comment int64
}

// warnPermissionUnknown records a Warning Event on rr when the look stops at comment because the code host did
// not answer the permission question: stopping there is safe, but a question that keeps failing would hold
// every later comment back without a trace. One Event per comment and request in this process; nothing is
// stored, so a restart may warn once more.
func (r *RemediationRequestReconciler) warnPermissionUnknown(rr *remediationv1alpha1.RemediationRequest, comment int64) {
	if r.Recorder == nil {
		return
	}
	w := permissionWarning{uid: rr.UID, comment: comment}
	if last, ok := r.permissionWarned.Swap(client.ObjectKeyFromObject(rr), w); ok && last == w {
		return
	}
	r.Recorder.Eventf(rr, corev1.EventTypeWarning, ReasonHintPermissionUnknown,
		"Reviewer hints wait at comment %d: the code host did not answer whether its author may write", comment)
}

// chainHints counts the hints that count toward the cap of a supersede chain: Consumed and Reserved ones.
func chainHints(hints []remediationv1alpha1.Hint) int {
	n := 0
	for _, h := range hints {
		if h.State == remediationv1alpha1.HintConsumed || h.State == remediationv1alpha1.HintReserved {
			n++
		}
	}
	return n
}

// declineOnEnd declines the Pending hints of rr when phase ends the request, before setPhase records it, so
// the status write that ends the request declines them too: no replacement can take its pull request over.
// The reason says why: ReasonHintPullRequestMerged for a merge (the phase, or a close condition markClosed
// wrote for one), ReasonHintPullRequestClosed for a close, ReasonHintRequestEnded for anything else. Only the
// phases a request goes through while its pull request may still be reviewed leave them Pending.
func declineOnEnd(rr *remediationv1alpha1.RemediationRequest, phase remediationv1alpha1.RemediationPhase) {
	switch phase {
	case remediationv1alpha1.PhasePending, remediationv1alpha1.PhasePolicyChecked, remediationv1alpha1.PhasePRCreated,
		remediationv1alpha1.PhaseAwaitingApproval, remediationv1alpha1.PhaseAutoMergeQueued:
		return
	}
	reason := ReasonHintRequestEnded
	switch closed := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPROpened); {
	case phase == remediationv1alpha1.PhaseMerged || phase == remediationv1alpha1.PhaseWaitingForDelivery ||
		phase == remediationv1alpha1.PhaseVerifying || phase == remediationv1alpha1.PhaseSucceeded:
		reason = ReasonHintPullRequestMerged
	case closed == nil:
	case closed.Reason == ReasonChangeRequestMerged:
		reason = ReasonHintPullRequestMerged
	case closed.Reason == ReasonChangeRequestClosed || closed.Reason == ReasonPullRequestClosed:
		reason = ReasonHintPullRequestClosed
	}
	for i := range rr.Status.Hints {
		if h := &rr.Status.Hints[i]; h.State == remediationv1alpha1.HintPending {
			h.State, h.Reason = remediationv1alpha1.HintDeclined, reason
		}
	}
	// A handoff whose intent still stands when the request ends for another reason did not happen: the
	// request that ends here keeps what it ends with, and its replacement learns of it from this status
	// (handoff.go). A handoff that happened set its intent Done before the end it caused.
	abortIntent(rr, reason)
	// Likewise on the replacement's side: one that ends with its handoff still Reserved did not take the pull
	// request over, whatever ended it (endHandoff, a retry that stopped, the grace of the recovery).
	if ho := rr.Status.Handoff; ho != nil && ho.From != nil && ho.From.Phase == remediationv1alpha1.HandoffReserved {
		ho.From.Phase = remediationv1alpha1.HandoffAborted
	}
}

// parseHintCommand reads the command from the first line of body. isCommand is false when the first line does
// not start with hintCommand. why is set when it does but the text cannot be a hint: there is none (Empty), or
// the line does not end within the body the host returned (TooLong).
func parseHintCommand(body string, truncated bool) (text string, isCommand bool, why remediationv1alpha1.HintRefusal) {
	line, _, found := strings.Cut(body, "\n")
	line = strings.TrimSuffix(line, "\r")
	rest, ok := strings.CutPrefix(line, hintCommand)
	if !ok {
		return "", false, ""
	}
	if !found && truncated {
		return "", true, remediationv1alpha1.HintRefusalTooLong
	}
	text = strings.TrimSpace(rest)
	if text == "" {
		return "", true, remediationv1alpha1.HintRefusalEmpty
	}
	return text, true, ""
}

// refuseHint records the refusal of comment c, unless one is recorded (see addRefused). It reports whether it
// recorded one.
func refuseHint(st *remediationv1alpha1.RemediationRequestStatus, source remediationv1alpha1.HintSource,
	c providers.Feedback, why remediationv1alpha1.HintRefusal, now time.Time) bool {
	if findRefused(st.RefusedHints, source, c.ID) {
		return false
	}
	return addRefused(st, remediationv1alpha1.RefusedHint{
		Source: source, CommentID: c.ID,
		Author:    remediationv1alpha1.HintAuthor{ID: c.Author.ID, Login: c.Author.Login},
		Reason:    why,
		RefusedAt: metav1.NewTime(now.UTC().Truncate(time.Second)),
	})
}

// addRefused appends f to the refusals of st, which must not hold its comment. At MaxRefusedHints it first
// drops the oldest refusal that has its reply recorded; a refusal still waiting for its reply is never dropped,
// so when all of them wait, f is not recorded and addRefused reports false.
func addRefused(st *remediationv1alpha1.RemediationRequestStatus, f remediationv1alpha1.RefusedHint) bool {
	if len(st.RefusedHints) >= remediationv1alpha1.MaxRefusedHints {
		i := slices.IndexFunc(st.RefusedHints, func(r remediationv1alpha1.RefusedHint) bool { return r.Reply != "" })
		if i < 0 {
			return false
		}
		st.RefusedHints = slices.Delete(slices.Clone(st.RefusedHints), i, i+1)
	}
	st.RefusedHints = append(st.RefusedHints, f)
	return true
}

func findHint(hints []remediationv1alpha1.Hint, source remediationv1alpha1.HintSource, id int64) *remediationv1alpha1.Hint {
	for i := range hints {
		if hints[i].Source == source && hints[i].CommentID == id {
			return &hints[i]
		}
	}
	return nil
}

func findRefused(refused []remediationv1alpha1.RefusedHint, source remediationv1alpha1.HintSource, id int64) bool {
	for _, r := range refused {
		if r.Source == source && r.CommentID == id {
			return true
		}
	}
	return false
}

// answerHints answers the Pending hints of upd (ADR-0028 section 3). It reads the Incident the request names
// from the API server, once, and only when a hint is Pending and not answered yet. A hint the Incident's
// status.handledHint names, outside phase Analyzing, with the Analyzed condition True, is answered: an outcome
// that created a replacement (Proposed, RunbookMatched) is stored on the hint (outcome), which stays Pending for
// the handoff (#539) and is judged from that record from then on, whatever the Incident handles later; any
// other declines it with that reason. A hint with no such answer HintAnswerTimeout after its acceptance is
// declined with ReasonHintNoAnswer. An Incident that cannot be read, or has another UID, answers nothing; the
// timeout still applies.
func (r *RemediationRequestReconciler) answerHints(ctx context.Context, upd *remediationv1alpha1.RemediationRequest,
	now time.Time) {
	open := func(h *remediationv1alpha1.Hint) bool {
		return h.State == remediationv1alpha1.HintPending && h.Outcome == ""
	}
	if !slices.ContainsFunc(upd.Status.Hints, func(h remediationv1alpha1.Hint) bool { return open(&h) }) {
		return
	}
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(upd))
	inc := r.hintIncident(ctx, upd)
	for i := range upd.Status.Hints {
		h := &upd.Status.Hints[i]
		if !open(h) {
			continue
		}
		if outcome, answered := incidentAnswer(inc, h); answered {
			switch {
			case outcome == outcomeProposed || outcome == outcomeRunbookMatched:
				h.Outcome = outcome
			case slices.Contains(remediationv1alpha1.HintDeclineReasons, outcome):
				h.State, h.Reason = remediationv1alpha1.HintDeclined, outcome
			default:
				// An outcome the API does not list (a newer analyzer): declined all the same, as no answer it
				// can name, and logged with the outcome so it stays told apart from a real NoAnswer.
				log.Info("Declined a reviewer hint of RemediationRequest: the Incident's outcome is not a hint reason",
					"comment", h.CommentID, "outcome", outcome, "reason", ReasonHintNoAnswer)
				h.State, h.Reason = remediationv1alpha1.HintDeclined, ReasonHintNoAnswer
			}
			continue
		}
		if !now.Before(h.AcceptedAt.Add(HintAnswerTimeout)) {
			h.State, h.Reason = remediationv1alpha1.HintDeclined, ReasonHintNoAnswer
		}
	}
}

// hintIncident reads the Incident upd names, within the Incident read's own deadline; nil when it cannot be
// read or is not the one named (another UID).
func (r *RemediationRequestReconciler) hintIncident(ctx context.Context,
	upd *remediationv1alpha1.RemediationRequest) *remediationv1alpha1.Incident {
	ref := upd.Spec.IncidentRef
	if ref.Name == "" || ref.UID == "" {
		return nil
	}
	readCtx, cancel := context.WithTimeout(ctx, incidentReadTimeout(ctx, r.IncidentReadTimeout))
	defer cancel()
	inc := &remediationv1alpha1.Incident{}
	if err := r.reader().Get(readCtx, client.ObjectKey{Namespace: upd.Namespace, Name: ref.Name}, inc); err != nil {
		logf.FromContext(ctx).Info("Could not read the Incident of RemediationRequest to answer its hints",
			"remediationRequest", client.ObjectKeyFromObject(upd), "incident", ref.Name, "error", err.Error())
		return nil
	}
	if inc.UID != types.UID(ref.UID) {
		return nil
	}
	return inc
}

// incidentAnswer reports whether inc holds the analysis's answer to h, and that answer: the reason of its
// Analyzed condition.
func incidentAnswer(inc *remediationv1alpha1.Incident, h *remediationv1alpha1.Hint) (string, bool) {
	if inc == nil || inc.Status.Phase == remediationv1alpha1.IncidentAnalyzing {
		return "", false
	}
	handled := inc.Status.HandledHint
	if handled == nil || *handled != (remediationv1alpha1.HintRef{Source: h.Source, CommentID: h.CommentID, Digest: h.Digest}) {
		return "", false
	}
	cond := meta.FindStatusCondition(inc.Status.Conditions, conditionIncidentAnalyzed)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return "", false
	}
	return cond.Reason, true
}

// cursorOf converts the stored cursor; nil is the start.
func cursorOf(c *remediationv1alpha1.CommentCursor) providers.FeedbackCursor {
	if c == nil {
		return providers.FeedbackCursor{}
	}
	out := providers.FeedbackCursor{Page: int(c.Page), AfterID: c.AfterID, ETag: c.ETag}
	if c.Since != nil {
		out.Since = c.Since.Time
	}
	if c.SweepStart != nil {
		out.SweepStart = c.SweepStart.Time
	}
	return out
}

// cursorStatus converts a cursor for status. Times are kept to the second, as the API stores them; a time cut
// so is earlier, so the next look reads more, never less. A page beyond the schema's maximum, or an entity tag
// the schema refuses, starts the sweep again from its since (it reads more, never less).
func cursorStatus(c providers.FeedbackCursor) *remediationv1alpha1.CommentCursor {
	out := &remediationv1alpha1.CommentCursor{AfterID: max(c.AfterID, 0)}
	if c.Page > 0 && c.Page <= maxCursorPage {
		out.Page = int32(c.Page) // #nosec G115 -- bounded to maxCursorPage just above
		if !c.SweepStart.IsZero() {
			out.SweepStart = new(metav1.NewTime(c.SweepStart.UTC().Truncate(time.Second)))
		}
	} else {
		out.AfterID = 0
	}
	if !c.Since.IsZero() {
		out.Since = new(metav1.NewTime(c.Since.UTC().Truncate(time.Second)))
	}
	if providers.ValidETag(c.ETag) {
		out.ETag = c.ETag
	}
	return out
}
