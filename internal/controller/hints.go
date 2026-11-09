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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

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
//   - A comment already stored as a hint is never handled again; one edited since (another updated time) is
//     recorded as refused (EditedAfterAccept) and the stored hint stays as it is. A comment already refused is
//     not handled again. A comment created before the cursor's since was read by an earlier look, so if it
//     carries the command now it was edited to: refused (EditedAfterSeen).
//   - The text is redacted (internal/redact) before anything is stored, refused when the redacted text is not
//     clean (NotRedactable) or longer than MaxHintTextBytes (TooLong), and refused when the request holds
//     MaxHints already (CapReached): the hints of the whole supersede chain are carried into it (#539).
//   - Only then is the code host asked whether the author may write, at most maxPermissionAsks authors per
//     look (PermissionNotChecked beyond); a refusal or an unknown author is NoWriteAccess. A failed question
//     stops the look there: what was handled before it is stored, the cursor is not, and the next look reads
//     the rest again.
//   - An accepted hint is pinned (comment ID, digest of the stored text, the comment's updated time) in
//     status.hints, Pending.
//
// The same look answers Pending hints from the Incident the request names (answerHints). Everything goes into
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

// maxPermissionAsks is how many authors one look asks the code host about. Anyone who can comment on the
// pull request can post the command, so a look full of them must not turn into a request per comment.
const maxPermissionAsks = 10

// The Incident's Analyzed condition and the outcome reasons with which the analysis creates a replacement
// request (internal/incidents and internal/analysis, which the executor does not import; a test keeps them
// equal).
const (
	conditionIncidentAnalyzed = "Analyzed"
	outcomeProposed           = "Proposed"
	outcomeRunbookMatched     = "RunbookMatched"
)

// hintReasonPattern is the pattern of a Hint's reason; an outcome reason that does not match is not copied.
var hintReasonPattern = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,63}$`)

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

// watchHints runs pollHints for a request whose pull request was just seen open with Shadewright's head. A
// failure is logged and changes nothing: the watch of the pull request goes on, and the next look tries again
// from the stored cursor.
func (r *RemediationRequestReconciler) watchHints(ctx context.Context, git providers.GitProvider,
	host remediationv1alpha1.GitProviderType, rr *remediationv1alpha1.RemediationRequest) {
	if err := r.pollHints(ctx, git, host, rr); err != nil {
		logf.FromContext(ctx).Info("Could not handle the reviewer hints of RemediationRequest",
			"remediationRequest", client.ObjectKeyFromObject(rr), "error", err.Error())
	}
}

// pollHints is one look at the comments of rr's pull request, and the answers of its Pending hints, written in
// one status update. rr is updated in place only when that write succeeded.
func (r *RemediationRequestReconciler) pollHints(ctx context.Context, git providers.GitProvider,
	host remediationv1alpha1.GitProviderType, rr *remediationv1alpha1.RemediationRequest) error {
	if !r.ReviewerHints || rr.Status.Phase != remediationv1alpha1.PhaseAwaitingApproval ||
		rr.Status.PullRequest == nil {
		return nil
	}
	source, ok := hintSource(host)
	if !ok {
		return nil
	}
	fs, ok := feedbackOf(git)
	if !ok {
		return nil // a provider that reads no feedback has no hints
	}
	now := r.now()
	upd := rr.DeepCopy()
	cur := cursorOf(upd.Status.CommentCursor)
	var lookErr error
	page, err := fs.ListComments(ctx, upd.Status.PullRequest.Number, cur)
	if err != nil {
		lookErr = fmt.Errorf("list the comments of the pull request: %w", err)
	} else if lookErr = r.takeComments(ctx, fs, upd, source, cur, page.Items, now); lookErr == nil {
		// Every comment of the look was handled: the cursor moves on in the same write.
		upd.Status.CommentCursor = cursorStatus(page.Next)
	}
	r.answerHints(ctx, upd, now)
	if equality.Semantic.DeepEqual(rr.Status, upd.Status) {
		return lookErr
	}
	// The kill switch covers this status write too (ADR-0028 section 4): while writes are paused, nothing is
	// stored, and the next look after the pause makes the same decisions.
	if err := r.writeAllowed(ctx); err != nil {
		return errors.Join(lookErr, fmt.Errorf("store reviewer hints: %w", err))
	}
	if err := r.Status().Update(ctx, upd); err != nil {
		return errors.Join(lookErr, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace,
			rr.Name, err))
	}
	upd.DeepCopyInto(rr)
	return lookErr
}

// takeComments handles the comments of one look in order, into upd's status (see the top of this file). It
// returns the error of a permission question that failed; the comments before it are handled.
func (r *RemediationRequestReconciler) takeComments(ctx context.Context, fs providers.FeedbackSource,
	upd *remediationv1alpha1.RemediationRequest, source remediationv1alpha1.HintSource,
	cur providers.FeedbackCursor, items []providers.Feedback, now time.Time) error {
	log := logf.FromContext(ctx).WithValues("remediationRequest", client.ObjectKeyFromObject(upd))
	st := &upd.Status
	asked := map[int64]bool{}
	for _, c := range items {
		if c.Kind != providers.FeedbackComment || c.Author.Kind != providers.AuthorPerson || c.ID < 1 ||
			c.Author.ID < 1 || !hintLoginPattern.MatchString(c.Author.Login) {
			continue
		}
		refuse := func(why remediationv1alpha1.HintRefusal) {
			if refuseHint(st, source, c, why, now) {
				log.Info("Refused a reviewer hint for RemediationRequest", "comment", c.ID,
					"author", c.Author.ID, "reason", string(why))
			}
		}
		if h := findHint(st.Hints, source, c.ID); h != nil {
			if !c.UpdatedAt.Truncate(time.Second).Equal(h.UpdatedAt.Time) {
				refuse(remediationv1alpha1.HintRefusalEditedAfterAccept)
			}
			continue
		}
		if findRefused(st.RefusedHints, source, c.ID) {
			continue
		}
		text, isCommand, why := parseHintCommand(c.Body, c.Truncated)
		switch {
		case !isCommand:
			continue
		case why != "":
			refuse(why)
			continue
		case !cur.Since.IsZero() && c.CreatedAt.Before(cur.Since):
			refuse(remediationv1alpha1.HintRefusalEditedAfterSeen)
			continue
		case len(st.Hints) >= remediationv1alpha1.MaxHints:
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
		may, ok := asked[c.Author.ID]
		if !ok {
			if len(asked) >= maxPermissionAsks {
				refuse(remediationv1alpha1.HintRefusalPermissionNotChecked)
				continue
			}
			var err error
			if may, err = fs.CanWrite(ctx, c.Author); err != nil {
				return fmt.Errorf("ask whether the author of comment %d may write: %w", c.ID, err)
			}
			asked[c.Author.ID] = may
		}
		if !may {
			refuse(remediationv1alpha1.HintRefusalNoWriteAccess)
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
	return nil
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

// refuseHint records the refusal of comment c, unless one is recorded, dropping the oldest beyond
// MaxRefusedHints. It reports whether it recorded one.
func refuseHint(st *remediationv1alpha1.RemediationRequestStatus, source remediationv1alpha1.HintSource,
	c providers.Feedback, why remediationv1alpha1.HintRefusal, now time.Time) bool {
	if findRefused(st.RefusedHints, source, c.ID) {
		return false
	}
	st.RefusedHints = append(st.RefusedHints, remediationv1alpha1.RefusedHint{
		Source: source, CommentID: c.ID,
		Author:    remediationv1alpha1.HintAuthor{ID: c.Author.ID, Login: c.Author.Login},
		Reason:    why,
		RefusedAt: metav1.NewTime(now.UTC().Truncate(time.Second)),
	})
	if n := len(st.RefusedHints); n > remediationv1alpha1.MaxRefusedHints {
		st.RefusedHints = append([]remediationv1alpha1.RefusedHint(nil),
			st.RefusedHints[n-remediationv1alpha1.MaxRefusedHints:]...)
	}
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

// answerHints declines the Pending hints of upd that have an answer (ADR-0028 section 3). It reads the
// Incident the request names from the API server, once, and only when a hint is Pending. A hint the Incident's
// status.handledHint names, outside phase Analyzing, with the Analyzed condition True, is answered: an outcome
// that created a replacement (Proposed, RunbookMatched) leaves it Pending for the handoff (#539); any other
// declines it with that reason. A hint with no such answer HintAnswerTimeout after its acceptance is declined
// with ReasonHintNoAnswer. An Incident that cannot be read, or has another UID, answers nothing; the timeout
// still applies.
func (r *RemediationRequestReconciler) answerHints(ctx context.Context, upd *remediationv1alpha1.RemediationRequest,
	now time.Time) {
	pending := false
	for i := range upd.Status.Hints {
		pending = pending || upd.Status.Hints[i].State == remediationv1alpha1.HintPending
	}
	if !pending {
		return
	}
	inc := r.hintIncident(ctx, upd)
	for i := range upd.Status.Hints {
		h := &upd.Status.Hints[i]
		if h.State != remediationv1alpha1.HintPending {
			continue
		}
		if reason, answered := incidentAnswer(inc, h); answered {
			if reason != "" {
				h.State, h.Reason = remediationv1alpha1.HintDeclined, reason
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
		logf.FromContext(ctx).V(1).Info("Could not read the Incident of RemediationRequest to answer its hints",
			"remediationRequest", client.ObjectKeyFromObject(upd), "error", err.Error())
		return nil
	}
	if inc.UID != types.UID(ref.UID) {
		return nil
	}
	return inc
}

// incidentAnswer reports whether inc holds the analysis's answer to h, and the reason to decline h with: empty
// for an outcome that created a replacement.
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
	switch {
	case cond.Reason == outcomeProposed || cond.Reason == outcomeRunbookMatched:
		return "", true
	case hintReasonPattern.MatchString(cond.Reason):
		return cond.Reason, true
	}
	return ReasonHintNoAnswer, true
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
