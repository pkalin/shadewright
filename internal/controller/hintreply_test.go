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
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// opCommentChangeRequest is the fake Git's name of CommentChangeRequest, for SetFail.
const opCommentChangeRequest = "CommentChangeRequest"

// reviewerB is the author of the refused commands in these tests.
const reviewerB = "reviewer-b"

// replyWorld is gateWorld's predecessor waiting for review with the branch's head recorded and hints as its
// only hints. It returns the reconciler, the fake Git and the request as stored.
func replyWorld(t *testing.T, hints []api.Hint, refused []api.RefusedHint) (*RemediationRequestReconciler,
	*fakegit.Git, *api.RemediationRequest) {
	t.Helper()
	r, g, p, _, _, head := gateWorld(t)
	plainWaiting(t, r, p, head)
	p.Status.Hints, p.Status.RefusedHints = hints, refused
	if err := r.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return r, g, p
}

func declinedHint(id int64, reason, text string) api.Hint {
	at := metav1.NewTime(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))
	return api.Hint{Source: api.HintSourceGitHub, CommentID: id, Digest: "sha256:" + strings.Repeat("c", 64),
		Author: api.HintAuthor{ID: 2002, Login: "octo-cat"}, Text: text, UpdatedAt: at, AcceptedAt: at,
		State: api.HintDeclined, Reason: reason}
}

func stored(t *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest) *api.RemediationRequest {
	t.Helper()
	out := &api.RemediationRequest{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(rr), out); err != nil {
		t.Fatal(err)
	}
	return out
}

// replies returns the comments of change request n that carry a reply marker.
func replies(g *fakegit.Git, n int64) []string {
	var out []string
	for _, c := range g.Comments(n) {
		if strings.Contains(c, "<!-- shadewright hint-reply ") {
			out = append(out, c)
		}
	}
	return out
}

func setPaused(t *testing.T, r *RemediationRequestReconciler, paused bool) {
	t.Helper()
	s := &api.RemediationSettings{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: api.SettingsName}, s); err != nil {
		t.Fatal(err)
	}
	s.Spec.Paused = &paused
	if err := r.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

func TestReplyToADeclinedHintOnceAcrossLooksAndRestarts(t *testing.T) {
	ctx := context.Background()
	r, g, p := replyWorld(t, []api.Hint{declinedHint(41, "BudgetSpent", "the leak is in the @sidecar `cache`")}, nil)
	n := p.Status.PullRequest.Number
	for range 2 {
		if _, err := r.watchPullRequest(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	got := replies(g, n)
	if len(got) != 1 {
		t.Fatalf("replies after two looks: %q", got)
	}
	for _, want := range []string{"@octo-cat", "comment 41", "`` the leak is in the @sidecar `cache` ``",
		"Not applied: the analysis budget is spent", "`BudgetSpent`", "keeps its plan",
		"<!-- shadewright hint-reply source=GitHub comment=41 -->"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("reply lacks %q:\n%s", want, got[0])
		}
	}
	if rec := stored(t, r, p).Status.Hints[0].Reply; rec != api.HintReplyPosted {
		t.Fatalf("reply recorded %q, want Posted", rec)
	}

	// A restart whose status write was lost: a new process with the reply unrecorded finds the marker.
	lost := stored(t, r, p)
	lost.Status.Hints[0].Reply = ""
	if err := r.Status().Update(ctx, lost); err != nil {
		t.Fatal(err)
	}
	restarted := &RemediationRequestReconciler{Client: r.Client, APIReader: r.APIReader, ReviewerHints: true,
		OpenRepository: r.OpenRepository}
	if _, err := restarted.watchPullRequest(ctx, lost); err != nil {
		t.Fatal(err)
	}
	if got := replies(g, n); len(got) != 1 {
		t.Fatalf("replies after a restart: %d", len(got))
	}
	if rec := stored(t, r, p).Status.Hints[0].Reply; rec != api.HintReplyPosted {
		t.Fatalf("reply recorded %q after the restart, want Posted", rec)
	}
}

// replyCorpus builds token-shaped values at run time from obviously synthetic parts, as the redactor's own
// corpus does, so no realistic secret is committed.
func replyCorpus() map[string]string {
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return map[string]string{
		"github token": "gh" + "p_" + strings.Repeat("Synth3tic", 4),
		"aws key id":   "AK" + "IA" + strings.Repeat("EXAMPLE0", 2),
		"jwt": b64(`{"alg":"HS256","typ":"JWT"}`) + "." + b64(`{"sub":"synthetic-user","iat":0}`) + "." +
			b64("not-a-real-signature-at-all"),
		"connection string": "postgres://orders:" + "Synthetic-Pa55-" + strings.Repeat("x", 6) + "@db.internal:5432/orders",
		"email":             "synthetic.person" + "@" + "example.com",
	}
}

func TestReplyHoldingATokenIsRefused(t *testing.T) {
	ctx := context.Background()
	for name, secret := range replyCorpus() {
		t.Run(name, func(t *testing.T) {
			// A hint stored with a secret in it (a redactor that missed it at intake, or a rule added since).
			r, g, p := replyWorld(t, []api.Hint{declinedHint(42, "NoProposal", "try "+secret+" instead")}, nil)
			if _, err := r.watchPullRequest(ctx, p); err != nil {
				t.Fatal(err)
			}
			for _, c := range g.Comments(p.Status.PullRequest.Number) {
				if strings.Contains(c, secret) || strings.Contains(c, "hint-reply") {
					t.Fatalf("a reply was posted:\n%s", c)
				}
			}
			if rec := stored(t, r, p).Status.Hints[0].Reply; rec != api.HintReplyNotRedactable {
				t.Fatalf("reply recorded %q, want NotRedactable", rec)
			}
		})
	}
}

func TestNoReplyWhilePaused(t *testing.T) {
	ctx := context.Background()
	r, g, p := replyWorld(t, []api.Hint{declinedHint(43, "CoolingDown", "look at the cache")}, nil)
	setPaused(t, r, true)
	_, _ = r.watchPullRequest(ctx, p)
	if got := replies(g, p.Status.PullRequest.Number); len(got) != 0 {
		t.Fatalf("replied while paused: %q", got)
	}
	if rec := stored(t, r, p).Status.Hints[0].Reply; rec != "" {
		t.Fatalf("recorded %q while paused", rec)
	}
	setPaused(t, r, false)
	p = stored(t, r, p)
	if _, err := r.watchPullRequest(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got := replies(g, p.Status.PullRequest.Number); len(got) != 1 {
		t.Fatalf("replies after the pause: %d", len(got))
	}
}

func TestReplyToARefusedCommand(t *testing.T) {
	ctx := context.Background()
	refused := api.RefusedHint{Source: api.HintSourceGitHub, CommentID: 44,
		Author: api.HintAuthor{ID: 2003, Login: reviewerB}, Reason: api.HintRefusalCapReached,
		RefusedAt: metav1.NewTime(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))}
	r, g, p := replyWorld(t, nil, []api.RefusedHint{refused})
	if _, err := r.watchPullRequest(ctx, p); err != nil {
		t.Fatal(err)
	}
	got := replies(g, p.Status.PullRequest.Number)
	if len(got) != 1 || !strings.Contains(got[0], "@reviewer-b") || !strings.Contains(got[0], "No hint was stored") ||
		!strings.Contains(got[0], "`CapReached`") {
		t.Fatalf("replies: %q", got)
	}
	if rec := stored(t, r, p).Status.RefusedHints[0].Reply; rec != api.HintReplyPosted {
		t.Fatalf("reply recorded %q, want Posted", rec)
	}
}

func TestNoReplyToPendingOrReservedHints(t *testing.T) {
	ctx := context.Background()
	pending := declinedHint(45, "", "pending")
	pending.State = api.HintPending
	r, g, p := replyWorld(t, []api.Hint{pending}, nil)
	if _, err := r.watchPullRequest(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got := replies(g, p.Status.PullRequest.Number); len(got) != 0 {
		t.Fatalf("replied to a Pending hint: %q", got)
	}
}

func TestReplyAfterTheHandoffNamesTheNewPlan(t *testing.T) {
	ctx := context.Background()
	r, g, p, rr, cr, head := gateWorld(t)
	h := p.Status.Hints[0]
	h.Text, h.State, h.Reply = "raise it more", api.HintConsumed, ""
	older := declinedHint(40, "", "earlier")
	older.State = api.HintConsumed
	rr.Status.Phase = api.PhaseAwaitingApproval
	rr.Status.PullRequest = &api.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: head}
	rr.Status.Handoff.From.Phase = api.HandoffDone
	rr.Status.Hints = []api.Hint{older, h}
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if err := r.replyHints(ctx, hintLook{git: mustOpen(t, r, rr), host: api.GitProviderGitHub}, rr); err != nil {
		t.Fatal(err)
	}
	got := replies(g, cr.Number)
	if len(got) != 2 {
		t.Fatalf("replies: %q", got)
	}
	answered := slices.IndexFunc(got, func(c string) bool { return strings.Contains(c, "comment 7") })
	if answered < 0 || !strings.Contains(got[answered], "Applied: this pull request now holds a new plan") ||
		!strings.Contains(got[answered], "`512Mi`") || !strings.Contains(got[answered], head) {
		t.Fatalf("reply to the answered hint: %q", got)
	}
	if !strings.Contains(got[1-answered], "Applied earlier") {
		t.Fatalf("reply to the earlier hint: %q", got[1-answered])
	}
	for _, h := range stored(t, r, rr).Status.Hints {
		if h.Reply != api.HintReplyPosted {
			t.Fatalf("hint %d recorded %q", h.CommentID, h.Reply)
		}
	}
}

// mustOpen opens rr's repository through the reconciler, so the provider passes the kill switch.
func mustOpen(t *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest) providers.GitProvider {
	t.Helper()
	git, err := r.openPlanRepository(context.Background(), rr)
	if err != nil {
		t.Fatal(err)
	}
	return git
}

func TestReplyRetriedThenGivenUp(t *testing.T) {
	ctx := context.Background()
	r, g, p := replyWorld(t, []api.Hint{declinedHint(46, "NoAnswer", "x")}, nil)
	now := time.Now()
	r.Now = func() time.Time { return now }
	tries := 0
	g.SetFail(func(op string) error {
		if op == opCommentChangeRequest {
			tries++
			return &providers.RetryableError{Err: errors.New("status 502")}
		}
		return nil
	})
	look := hintLook{git: mustOpen(t, r, p), host: api.GitProviderGitHub}
	for i := range maxReplyAttempts {
		if err := r.replyHints(ctx, look, p); err != nil {
			t.Fatal(err)
		}
		rec := stored(t, r, p).Status.Hints[0].Reply
		if i < maxReplyAttempts-1 && rec != "" {
			t.Fatalf("recorded %q after %d tries", rec, i+1)
		}
		now = now.Add(replyRetryWait) // each failure makes the replies wait
	}
	if rec := stored(t, r, p).Status.Hints[0].Reply; rec != api.HintReplyGaveUp || tries != maxReplyAttempts {
		t.Fatalf("recorded %q after %d tries, want GaveUp after %d", rec, tries, maxReplyAttempts)
	}
	if err := r.replyHints(ctx, look, stored(t, r, p)); err != nil || tries != maxReplyAttempts {
		t.Fatalf("asked again after giving up: %d tries, %v", tries, err)
	}
}

func TestReplyRefusedForGoodAndAfterALostAnswer(t *testing.T) {
	ctx := context.Background()
	r, g, p := replyWorld(t, []api.Hint{declinedHint(47, "NoAnswer", "x"), declinedHint(48, "NoAnswer", "y")}, nil)
	g.SetFail(func(op string) error {
		if op == opCommentChangeRequest {
			return fmt.Errorf("status 403: %w", providers.ErrForbidden)
		}
		return nil
	})
	look := hintLook{git: mustOpen(t, r, p), host: api.GitProviderGitHub}
	if err := r.replyHints(ctx, look, p); err != nil {
		t.Fatal(err)
	}
	for _, h := range stored(t, r, p).Status.Hints {
		if h.Reply != api.HintReplyRefused {
			t.Fatalf("hint %d recorded %q, want Refused", h.CommentID, h.Reply)
		}
	}

	// A post whose answer was lost is retried, and the marker keeps it from being posted twice.
	r, g, p = replyWorld(t, []api.Hint{declinedHint(49, "NoAnswer", "z")}, nil)
	now := time.Now()
	r.Now = func() time.Time { return now }
	lost := true
	g.SetAfter(func(op string) error {
		if op == opCommentChangeRequest && lost {
			lost = false
			return &providers.RetryableError{Err: errors.New("connection reset")}
		}
		return nil
	})
	look = hintLook{git: mustOpen(t, r, p), host: api.GitProviderGitHub}
	for range 2 {
		if err := r.replyHints(ctx, look, p); err != nil {
			t.Fatal(err)
		}
		now = now.Add(replyRetryWait)
	}
	if got := replies(g, p.Status.PullRequest.Number); len(got) != 1 {
		t.Fatalf("replies after a lost answer: %d", len(got))
	}
	if rec := stored(t, r, p).Status.Hints[0].Reply; rec != api.HintReplyPosted {
		t.Fatalf("recorded %q, want Posted", rec)
	}
}

func TestReplyQuote(t *testing.T) {
	long := strings.Repeat("é", maxReplyQuoteBytes)
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"plain", "` plain `"},
		{"a `b` c", "`` a `b` c ``"},
		{"has <!-- a marker", ""},
		{"closes -->", ""},
		{"tab\there", ""},
		// Format characters (bidi overrides, zero width) become spaces, as in cluster text before redaction.
		{"evil\u202egnp.exe", "` evil gnp.exe `"},
		{"zero\u200bwidth\u2066", "` zero width  `"},
		{long, "` " + strings.Repeat("é", maxReplyQuoteBytes/2) + "... `"},
	} {
		if got := replyQuote(c.in); got != c.want {
			t.Errorf("replyQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEveryReasonHasItsOwnWords(t *testing.T) {
	for _, reason := range api.HintDeclineReasons {
		if declineText(reason) == declineText("Unknown") {
			t.Errorf("reason %s has no words of its own", reason)
		}
	}
	for _, reason := range []api.HintRefusal{api.HintRefusalCapReached, api.HintRefusalTooLong,
		api.HintRefusalNotRedactable, api.HintRefusalEmpty, api.HintRefusalEditedAfterSeen} {
		if refusalText(reason) == refusalText("Unknown") {
			t.Errorf("refusal %s has no words of its own", reason)
		}
	}
}

func refusedCommand(id int64, rec api.HintReply) api.RefusedHint {
	return api.RefusedHint{Source: api.HintSourceGitHub, CommentID: id,
		Author: api.HintAuthor{ID: 2003, Login: reviewerB}, Reason: api.HintRefusalEmpty,
		RefusedAt: metav1.NewTime(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)), Reply: rec}
}

func TestGaveUpSurvivesALostStatusWrite(t *testing.T) {
	ctx := context.Background()
	r, g, p := replyWorld(t, []api.Hint{declinedHint(50, "NoAnswer", "x")}, nil)
	now := time.Now()
	r.Now = func() time.Time { return now }
	tries := 0
	g.SetFail(func(op string) error {
		if op != opCommentChangeRequest {
			return nil
		}
		tries++
		if tries == maxReplyAttempts {
			// The status write that would record GaveUp is held back.
			setPaused(t, r, true)
		}
		return &providers.RetryableError{Err: errors.New("status 502")}
	})
	look := hintLook{git: mustOpen(t, r, p), host: api.GitProviderGitHub}
	for range maxReplyAttempts {
		_ = r.replyHints(ctx, look, stored(t, r, p))
		now = now.Add(replyRetryWait)
	}
	if rec := stored(t, r, p).Status.Hints[0].Reply; rec != "" {
		t.Fatalf("recorded %q while writes were paused", rec)
	}
	setPaused(t, r, false)
	if err := r.replyHints(ctx, look, stored(t, r, p)); err != nil {
		t.Fatal(err)
	}
	if rec := stored(t, r, p).Status.Hints[0].Reply; rec != api.HintReplyGaveUp || tries != maxReplyAttempts {
		t.Fatalf("recorded %q after %d tries, want GaveUp after %d", rec, tries, maxReplyAttempts)
	}
}

func TestReplyStopsAtAClosedPullRequest(t *testing.T) {
	ctx := context.Background()
	r, g, p := replyWorld(t, []api.Hint{declinedHint(51, "NoAnswer", "x"), declinedHint(52, "NoAnswer", "y")}, nil)
	tries := 0
	g.SetFail(func(op string) error {
		if op == opCommentChangeRequest {
			tries++
			return fmt.Errorf("closed: %w", providers.ErrConflict)
		}
		return nil
	})
	if err := r.replyHints(ctx, hintLook{git: mustOpen(t, r, p), host: api.GitProviderGitHub}, p); err != nil {
		t.Fatal(err)
	}
	if tries != 1 {
		t.Fatalf("asked %d times after the pull request was closed, want once", tries)
	}
}

func TestReplyNotPostedWhenTheMarkerCannotBeChecked(t *testing.T) {
	ctx := context.Background()
	r, g, p := replyWorld(t, []api.Hint{declinedHint(53, "NoAnswer", "x")}, nil)
	g.SetFail(func(op string) error {
		if op == opCommentChangeRequest {
			return fmt.Errorf("busy: %w", providers.ErrTooManyComments)
		}
		return nil
	})
	if err := r.replyHints(ctx, hintLook{git: mustOpen(t, r, p), host: api.GitProviderGitHub}, p); err != nil {
		t.Fatal(err)
	}
	if rec := stored(t, r, p).Status.Hints[0].Reply; rec != api.HintReplyTooManyComments {
		t.Fatalf("recorded %q, want TooManyComments", rec)
	}
}

func TestHandoffCarriesRefusedCommands(t *testing.T) {
	ctx := context.Background()
	r, _, p, rr, cr, head := gateWorld(t)
	p.Status.RefusedHints = []api.RefusedHint{refusedCommand(60, ""), refusedCommand(61, api.HintReplyPosted)}
	if err := r.recordHandoff(ctx, rr, p, cr, head); err != nil {
		t.Fatal(err)
	}
	got := stored(t, r, rr).Status.RefusedHints
	if len(got) != 2 || got[0].CommentID != 60 || got[0].Reply != "" || got[1].CommentID != 61 ||
		got[1].Reply != api.HintReplyPosted {
		t.Fatalf("refused commands after the handoff: %+v", got)
	}
}

func TestRefusedHintsKeepEntriesThatWaitForAReply(t *testing.T) {
	st := &api.RemediationRequestStatus{}
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	fb := func(id int64) providers.Feedback {
		return providers.Feedback{ID: id, Author: providers.FeedbackAuthor{ID: 2003, Login: reviewerB}}
	}
	for id := range int64(api.MaxRefusedHints) {
		refuseHint(st, api.HintSourceGitHub, fb(id+1), api.HintRefusalEmpty, now)
	}
	if refuseHint(st, api.HintSourceGitHub, fb(100), api.HintRefusalEmpty, now) ||
		!findRefused(st.RefusedHints, api.HintSourceGitHub, 1) || len(st.RefusedHints) != api.MaxRefusedHints {
		t.Fatalf("an entry that waits for its reply was dropped: %+v", st.RefusedHints)
	}
	st.RefusedHints[3].Reply = api.HintReplyPosted
	if !refuseHint(st, api.HintSourceGitHub, fb(101), api.HintRefusalEmpty, now) ||
		findRefused(st.RefusedHints, api.HintSourceGitHub, 4) || !findRefused(st.RefusedHints, api.HintSourceGitHub, 1) ||
		len(st.RefusedHints) != api.MaxRefusedHints {
		t.Fatalf("the replied entry was not the one dropped: %+v", st.RefusedHints)
	}
}

// A rate limit is no try: a secondary limit across more looks than maxReplyAttempts never records GaveUp, and
// no look asks again before the delay the host asked for has passed.
func TestReplyRateLimitIsNoTry(t *testing.T) {
	ctx := context.Background()
	r, g, p := replyWorld(t, []api.Hint{declinedHint(54, "NoAnswer", "x")}, nil)
	now := time.Now()
	r.Now = func() time.Time { return now }
	tries := 0
	limited := true
	g.SetFail(func(op string) error {
		if op == opCommentChangeRequest && limited {
			tries++
			return &providers.RetryableError{RetryAfter: 2 * time.Minute, Err: errors.New("rate limit")}
		}
		return nil
	})
	look := hintLook{git: mustOpen(t, r, p), host: api.GitProviderGitHub}
	for i := range 2 * maxReplyAttempts {
		if err := r.replyHints(ctx, look, stored(t, r, p)); err != nil {
			t.Fatal(err)
		}
		if rec := stored(t, r, p).Status.Hints[0].Reply; rec != "" {
			t.Fatalf("recorded %q after %d rate limits", rec, i+1)
		}
		if d := r.replyWait(p); d != 2*time.Minute {
			t.Fatalf("wait = %v, want the 2m the host asked for", d)
		}
		// A look within the delay does not ask.
		if err := r.replyHints(ctx, look, stored(t, r, p)); err != nil || tries != i+1 {
			t.Fatalf("asked within the delay: %d tries, %v", tries, err)
		}
		now = now.Add(2 * time.Minute)
	}
	limited = false
	if err := r.replyHints(ctx, look, stored(t, r, p)); err != nil {
		t.Fatal(err)
	}
	if rec := stored(t, r, p).Status.Hints[0].Reply; rec != api.HintReplyPosted ||
		len(replies(g, p.Status.PullRequest.Number)) != 1 {
		t.Fatalf("recorded %q, want Posted once the limit passed", rec)
	}
}

// After a post that may have gone through, a look within replyRetryWait neither looks for the marker nor
// posts: a list of comments that lags behind could hide the reply just made.
func TestReplyWaitsAfterAnAmbiguousFailure(t *testing.T) {
	ctx := context.Background()
	r, g, p := replyWorld(t, []api.Hint{declinedHint(55, "NoAnswer", "x"), declinedHint(56, "NoAnswer", "y")}, nil)
	now := time.Now()
	r.Now = func() time.Time { return now }
	tries := 0
	g.SetFail(func(op string) error {
		if op == opCommentChangeRequest {
			tries++
			if tries == 1 {
				return &providers.RetryableError{Err: errors.New("status 502")}
			}
		}
		return nil
	})
	look := hintLook{git: mustOpen(t, r, p), host: api.GitProviderGitHub}
	if err := r.replyHints(ctx, look, stored(t, r, p)); err != nil {
		t.Fatal(err)
	}
	if tries != 1 {
		t.Fatalf("asked %d times in the look of the failure, want once", tries)
	}
	now = now.Add(replyRetryWait - time.Second)
	if err := r.replyHints(ctx, look, stored(t, r, p)); err != nil || tries != 1 {
		t.Fatalf("asked again within the wait: %d tries, %v", tries, err)
	}
	now = now.Add(time.Second)
	if err := r.replyHints(ctx, look, stored(t, r, p)); err != nil {
		t.Fatal(err)
	}
	if got := stored(t, r, p).Status.Hints; got[0].Reply != api.HintReplyPosted || got[1].Reply != api.HintReplyPosted {
		t.Fatalf("recorded %q and %q, want both Posted after the wait", got[0].Reply, got[1].Reply)
	}
}
