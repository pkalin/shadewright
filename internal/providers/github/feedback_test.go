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

package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/providers"
)

const (
	commentsPath = "/issues/12/comments"
	reviewsPath  = "/pulls/12/reviews"
	pullPath     = "/pulls/12"
	permPath     = "/collaborators/reviewer-one/permission"
	prNumber     = 12
	hDate        = "Date"
)

var (
	since    = time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	reviewer = providers.FeedbackAuthor{ID: 1234567, Login: "reviewer-one", Kind: providers.AuthorPerson}
	// hostNow is GitHub's clock in the Date header of the first page of a sweep.
	hostNow = time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	start   = providers.FeedbackCursor{Since: since}
)

// ownPR is the read that proves the pull request is a Shadewright one; every look starts with it.
func ownPR() step { return get(pullPath, "pull_open.json") }

func pageQuery(page int) string {
	return "page=" + strconv.Itoa(page) + "&per_page=" + strconv.Itoa(feedbackPerPage)
}

func commentsQuery(page int) string {
	return pageQuery(page) + "&since=2026-11-08T10%3A00%3A00Z"
}

// recorded returns the first element of a recorded list, to build pages from.
func recorded(t *testing.T, file string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	var all []map[string]any
	if err := json.Unmarshal(data, &all); err != nil {
		t.Fatal(err)
	}
	return all[0]
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// commentPage is n copies of the recorded comment, one minute apart from first, with IDs from id and body.
func commentPage(t *testing.T, n int, id int64, first time.Time, body string) string {
	t.Helper()
	base := recorded(t, "comments_mixed.json")
	page := make([]map[string]any, n)
	for i := range page {
		c := maps.Clone(base)
		cid := id + int64(i)
		at := first.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		c["id"], c["created_at"], c["updated_at"], c["body"] = cid, at, at, body
		c["html_url"] = "https://github.com/acme/fleet/pull/12#issuecomment-" + strconv.FormatInt(cid, 10)
		page[i] = c
	}
	return marshal(t, page)
}

// reviewPage is n copies of the recorded review, with IDs from id.
func reviewPage(t *testing.T, n int, id int64) string {
	t.Helper()
	base := recorded(t, "reviews.json")
	page := make([]map[string]any, n)
	for i := range page {
		r := maps.Clone(base)
		rid := id + int64(i)
		r["id"] = rid
		r["html_url"] = "https://github.com/acme/fleet/pull/12#pullrequestreview-" + strconv.FormatInt(rid, 10)
		page[i] = r
	}
	return marshal(t, page)
}

func commentsStep(page int, raw string) step {
	return step{method: http.MethodGet, path: commentsPath, query: commentsQuery(page), status: http.StatusOK, raw: raw}
}

func reviewsStep(page int, raw string) step {
	return step{method: http.MethodGet, path: reviewsPath, query: pageQuery(page), status: http.StatusOK, raw: raw}
}

// dated sets GitHub's Date header on a step.
func dated(s step) step {
	s.header = map[string]string{hDate: hostNow.Format(http.TimeFormat)}
	return s
}

func ids(items []providers.Feedback) []int64 {
	out := make([]int64, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

func TestListCommentsFlagsBotsAndApps(t *testing.T) {
	h := newHarness(t, ownPR(), dated(step{method: http.MethodGet, path: commentsPath, query: commentsQuery(1),
		status: http.StatusOK, file: "comments_mixed.json"}))
	got, err := h.p.ListComments(ctx, prNumber, start)
	if err != nil {
		t.Fatal(err)
	}
	want := []providers.FeedbackAuthor{
		reviewer,
		{ID: 9900001, Login: "shadewright-app[bot]", Kind: providers.AuthorBot},
		{ID: 7654321, Login: "reviewer-two", Kind: providers.AuthorApp},
		{ID: 5550001, Login: "helper-org", Kind: providers.AuthorBot},
	}
	if len(got.Items) != len(want) {
		t.Fatalf("got %d items, want %d", len(got.Items), len(want))
	}
	for i, it := range got.Items {
		if it.Author != want[i] || it.Kind != providers.FeedbackComment {
			t.Errorf("item %d: author %+v kind %s, want %+v", i, it.Author, it.Kind, want[i])
		}
	}
	first := got.Items[0]
	if first.ID != 3201234567 || !strings.HasPrefix(first.Body, "/shadewright reanalyze: the leak") || first.Truncated {
		t.Errorf("first item = %+v", first)
	}
	// A short page ends the sweep; the next asks for what changed since GitHub's clock at its start.
	wantNext := providers.FeedbackCursor{Since: hostNow.Add(-clockMargin)}
	if !got.Complete || got.Next != wantNext {
		t.Errorf("complete %v, next %+v", got.Complete, got.Next)
	}
}

func TestListCommentsPagingStopsAtTheBoundAndGoesOn(t *testing.T) {
	steps := []step{ownPR()}
	for page := 1; page <= providers.MaxFeedbackPages; page++ {
		s := commentsStep(page, commentPage(t, feedbackPerPage, int64(page*1000), since.Add(time.Duration(page)*time.Hour), "ok"))
		if page == 1 {
			s = dated(s)
		}
		steps = append(steps, s)
	}
	h := newHarness(t, steps...) // a sixth page fails the script
	got, err := h.p.ListComments(ctx, prNumber, start)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != providers.MaxFeedbackPages*feedbackPerPage || got.Complete {
		t.Fatalf("got %d items, complete %v", len(got.Items), got.Complete)
	}
	lastID := got.Items[len(got.Items)-1].ID
	want := providers.FeedbackCursor{Since: since, Page: providers.MaxFeedbackPages, AfterID: lastID, SweepStart: hostNow}
	if got.Next != want {
		t.Fatalf("next %+v, want %+v", got.Next, want)
	}

	// The next look reads the last page again (nothing new on it) and goes on to the end.
	last := providers.MaxFeedbackPages
	h = newHarness(t, ownPR(),
		commentsStep(last, commentPage(t, feedbackPerPage, int64(last*1000), since, "ok")),
		commentsStep(last+1, commentPage(t, 2, 9000, since.Add(9*time.Hour), "new")))
	got, err = h.p.ListComments(ctx, prNumber, want)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(got.Items), []int64{9000, 9001}) || !got.Complete {
		t.Fatalf("items %v, complete %v", ids(got.Items), got.Complete)
	}
	if want := (providers.FeedbackCursor{Since: hostNow.Add(-clockMargin)}); got.Next != want {
		t.Errorf("next %+v, want %+v (the sweep's start, not this look's)", got.Next, want)
	}
}

func TestListCommentsOldEditedCommentsDoNotHoldTheCursorBack(t *testing.T) {
	// Five full pages of comments created long before since and edited after it: the cursor moves past them.
	steps := []step{ownPR()}
	old := since.Add(-30 * 24 * time.Hour)
	for page := 1; page <= providers.MaxFeedbackPages; page++ {
		var page0 []map[string]any
		if err := json.Unmarshal([]byte(commentPage(t, feedbackPerPage, int64(page*1000), old, "ok")), &page0); err != nil {
			t.Fatal(err)
		}
		for _, c := range page0 {
			c["updated_at"] = "2026-11-08T11:00:00Z"
		}
		steps = append(steps, commentsStep(page, marshal(t, page0)))
	}
	h := newHarness(t, steps...)
	got, err := h.p.ListComments(ctx, prNumber, start)
	if err != nil {
		t.Fatal(err)
	}
	if got.Next.Since.Before(since) || got.Next.Page != providers.MaxFeedbackPages || got.Next.AfterID != 5029 {
		t.Errorf("next %+v: went back or did not move on", got.Next)
	}
}

func TestListCommentsSinceNeverDecreases(t *testing.T) {
	// GitHub's clock says the sweep began before the cursor's since; and without a Date header there is no start.
	early := dated(commentsStep(1, "[]"))
	early.header[hDate] = since.Add(-time.Hour).Format(http.TimeFormat)
	for name, s := range map[string]step{"clock behind": early, "no Date": commentsStep(1, "[]")} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, ownPR(), s)
			got, err := h.p.ListComments(ctx, prNumber, start)
			if err != nil {
				t.Fatal(err)
			}
			if got.Next != start || !got.Complete {
				t.Errorf("next %+v, complete %v; want since kept", got.Next, got.Complete)
			}
		})
	}
}

func TestListCommentsCutsALongBody(t *testing.T) {
	// A three-byte rune straddles the bound, so a cut by bytes alone would split it.
	long := strings.Repeat("a", providers.MaxFeedbackBodyBytes-1) + "€" + strings.Repeat("b", 100)
	h := newHarness(t, ownPR(), commentsStep(1, commentPage(t, 1, 1, since, long)))
	got, err := h.p.ListComments(ctx, prNumber, start)
	if err != nil {
		t.Fatal(err)
	}
	it := got.Items[0]
	if !it.Truncated || len(it.Body) > providers.MaxFeedbackBodyBytes || !utf8.ValidString(it.Body) ||
		!strings.HasPrefix(long, it.Body) || len(it.Body) != providers.MaxFeedbackBodyBytes-1 {
		t.Errorf("body of %d bytes, truncated %v", len(it.Body), it.Truncated)
	}
}

func TestListCommentsStopsAtTheByteBoundAndGoesOn(t *testing.T) {
	body := strings.Repeat("x", providers.MaxFeedbackBodyBytes)
	keep := providers.MaxFeedbackBytes / providers.MaxFeedbackBodyBytes
	if keep <= feedbackPerPage || keep >= 2*feedbackPerPage {
		t.Fatalf("test needs the byte bound inside the second page")
	}
	page2 := commentPage(t, feedbackPerPage, 1000, since.Add(time.Hour), body)
	// Two full pages hold more than the byte bound: the look ends inside the second, without a third request.
	h := newHarness(t, ownPR(), dated(commentsStep(1, commentPage(t, feedbackPerPage, 1, since, body))),
		commentsStep(2, page2))
	got, err := h.p.ListComments(ctx, prNumber, start)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != keep || got.Complete {
		t.Fatalf("got %d items, complete %v; want %d, incomplete", len(got.Items), got.Complete, keep)
	}
	lastKept := got.Items[keep-1].ID
	want := providers.FeedbackCursor{Since: since, Page: 2, AfterID: lastKept, SweepStart: hostNow}
	if got.Next != want {
		t.Fatalf("next %+v, want %+v", got.Next, want)
	}
	// The next look starts on the second page again and returns the rest of it, none twice.
	h = newHarness(t, ownPR(), commentsStep(2, page2), commentsStep(3, "[]"))
	got, err = h.p.ListComments(ctx, prNumber, want)
	if err != nil {
		t.Fatal(err)
	}
	if n := feedbackPerPage*2 - keep; len(got.Items) != n || got.Items[0].ID != lastKept+1 || !got.Complete {
		t.Errorf("got %d items from %d, complete %v; want %d from %d", len(got.Items), got.Items[0].ID,
			got.Complete, n, lastKept+1)
	}
}

// byteCounter counts the bytes read through it.
type byteCounter struct {
	r io.Reader
	n int
}

func (c *byteCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestDecodePageStopsReadingAtTheByteBound(t *testing.T) {
	// A page far larger than the byte bound: decoding stops reading once the bound is reached.
	body := strings.Repeat("x", providers.MaxFeedbackBodyBytes)
	raw := commentPage(t, 200, 1, since, body)
	cr := &byteCounter{r: strings.NewReader(raw)}
	conv := func(c commentJSON) (item, bool) {
		b, cut := cutBody(*c.Body)
		return item{id: c.ID, fb: providers.Feedback{ID: c.ID, Body: b, Truncated: cut}}, true
	}
	res, err := decodePage(cr, providers.MaxFeedbackBytes, 0, conv)
	if err != nil {
		t.Fatal(err)
	}
	if !res.stopped || len(res.items) != providers.MaxFeedbackBytes/providers.MaxFeedbackBodyBytes {
		t.Fatalf("stopped %v with %d items", res.stopped, len(res.items))
	}
	if cr.n > len(raw)/2 {
		t.Errorf("read %d of %d bytes to keep %d", cr.n, len(raw), providers.MaxFeedbackBytes)
	}
}

func TestListCommentsSkipsMalformedItems(t *testing.T) {
	good := recorded(t, "comments_mixed.json")
	edited := maps.Clone(good)
	edited["id"], edited["updated_at"] = 11, "2026-11-08T09:00:00Z" // updated before created
	badTime := maps.Clone(good)
	badTime["id"], badTime["created_at"] = 12, "yesterday"
	badType := maps.Clone(good)
	badType["id"] = "13"
	noUser := maps.Clone(good)
	noUser["id"], noUser["user"] = 14, nil
	last := maps.Clone(good)
	last["id"] = 15
	h := newHarness(t, ownPR(), commentsStep(1, marshal(t, []any{edited, badTime, badType, noUser, last})))
	got, err := h.p.ListComments(ctx, prNumber, start)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(got.Items), []int64{15}) || got.Skipped != 4 || !got.Complete {
		t.Errorf("items %v, skipped %d, complete %v", ids(got.Items), got.Skipped, got.Complete)
	}
}

func TestListCommentsRefusesAnotherRepository(t *testing.T) {
	for name, file := range map[string]string{
		"other repository": "comments_foreign.json",
		"issue, not pull":  "comments_on_issue.json",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, ownPR(), step{method: http.MethodGet, path: commentsPath, query: commentsQuery(1),
				status: http.StatusOK, file: file})
			got, err := h.p.ListComments(ctx, prNumber, start)
			assertSafe(t, err)
			if providers.IsRetryable(err) || len(got.Items) != 0 {
				t.Errorf("retryable %v, %d items with the error", providers.IsRetryable(err), len(got.Items))
			}
			if len(h.sleeps) != 0 {
				t.Error("a refusal was retried")
			}
		})
	}
}

func TestListFeedbackRefusesAPullRequestNotOurs(t *testing.T) {
	for name, list := range map[string]func(*Provider) (providers.FeedbackPage, error){
		"comments": func(p *Provider) (providers.FeedbackPage, error) { return p.ListComments(ctx, 13, start) },
		"reviews":  func(p *Provider) (providers.FeedbackPage, error) { return p.ListReviews(ctx, 13, start) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, get("/pulls/13", "pull_foreign.json")) // no list request follows
			_, err := list(h.p)
			assertSafe(t, err)
		})
	}
}

func TestListCommentsWithoutSince(t *testing.T) {
	h := newHarness(t, ownPR(), step{method: http.MethodGet, path: commentsPath, query: pageQuery(1),
		status: http.StatusOK, raw: "[]"})
	got, err := h.p.ListComments(ctx, prNumber, providers.FeedbackCursor{})
	if err != nil || !got.Complete || len(got.Items) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestListCommentsEachPageHasItsOwnDeadline(t *testing.T) {
	// Four pages of 80ms each under a 200ms timeout: a deadline shared by the look would end it on the fourth.
	slow := func(s step) step {
		s.check = func(*testing.T, map[string]any) { time.Sleep(80 * time.Millisecond) }
		return s
	}
	steps := []step{ownPR()}
	for page := 1; page <= 3; page++ {
		steps = append(steps, slow(commentsStep(page, commentPage(t, feedbackPerPage, int64(page*1000), since, "ok"))))
	}
	steps = append(steps, slow(commentsStep(4, "[]")))
	h := newHarness(t, steps...)
	h.p.timeout = 200 * time.Millisecond
	got, err := h.p.ListComments(ctx, prNumber, start)
	if err != nil || len(got.Items) != 3*feedbackPerPage || !got.Complete {
		t.Fatalf("got %d items, complete %v, err %v", len(got.Items), got.Complete, err)
	}
}

func TestListCommentsServerErrors(t *testing.T) {
	t.Run("retried", func(t *testing.T) {
		h := newHarness(t, ownPR(),
			step{method: http.MethodGet, path: commentsPath, status: http.StatusBadGateway},
			commentsStep(1, "[]"))
		if _, err := h.p.ListComments(ctx, prNumber, start); err != nil {
			t.Fatal(err)
		}
		if len(h.sleeps) != 1 {
			t.Errorf("slept %d times", len(h.sleeps))
		}
	})
	t.Run("final", func(t *testing.T) {
		steps := append(make([]step, 0, 1+DefaultMaxAttempts), ownPR())
		for range DefaultMaxAttempts {
			steps = append(steps, step{method: http.MethodGet, path: commentsPath, status: http.StatusInternalServerError})
		}
		h := newHarness(t, steps...)
		_, err := h.p.ListComments(ctx, prNumber, start)
		if !providers.IsRetryable(err) {
			t.Fatalf("got %v", err)
		}
		assertSafe(t, err)
	})
	t.Run("cut off mid body is retried", func(t *testing.T) {
		h := newHarness(t, ownPR(), commentsStep(1, `[{"id":1`), commentsStep(1, "[]"))
		if _, err := h.p.ListComments(ctx, prNumber, start); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("not JSON is final", func(t *testing.T) {
		h := newHarness(t, ownPR(), commentsStep(1, `{"message":"`+bodyMarker+`"}`))
		_, err := h.p.ListComments(ctx, prNumber, start)
		assertSafe(t, err)
		if providers.IsRetryable(err) {
			t.Errorf("got %v", err)
		}
	})
}

func TestListCommentsRateLimit(t *testing.T) {
	t.Run("short delay waited for", func(t *testing.T) {
		h := newHarness(t, ownPR(),
			step{method: http.MethodGet, path: commentsPath, status: http.StatusTooManyRequests,
				header: map[string]string{hRetry: "3"}},
			commentsStep(1, "[]"))
		if _, err := h.p.ListComments(ctx, prNumber, start); err != nil {
			t.Fatal(err)
		}
		if len(h.sleeps) != 1 || h.sleeps[0] != 3*time.Second {
			t.Errorf("sleeps = %v", h.sleeps)
		}
	})
	t.Run("long delay returned", func(t *testing.T) {
		reset := strconv.FormatInt(t0.Add(10*time.Minute).Unix(), 10)
		h := newHarness(t, ownPR(), step{method: http.MethodGet, path: commentsPath, status: http.StatusForbidden,
			header: map[string]string{hdrRemaining: "0", hdrReset: reset}})
		_, err := h.p.ListComments(ctx, prNumber, start)
		if d, ok := providers.RetryAfter(err); !ok || d != 10*time.Minute {
			t.Fatalf("got %v", err)
		}
		assertSafe(t, err)
	})
	t.Run("forbidden without rate limit is final", func(t *testing.T) {
		h := newHarness(t, ownPR(), status(http.MethodGet, commentsPath, http.StatusForbidden, fxForbidden))
		_, err := h.p.ListComments(ctx, prNumber, start)
		if !providers.IsForbidden(err) || providers.IsRetryable(err) {
			t.Fatalf("got %v", err)
		}
		assertSafe(t, err)
	})
}

func TestListCommentsCancelledWhilePaging(t *testing.T) {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	first := commentsStep(1, commentPage(t, feedbackPerPage, 1, since, "ok"))
	first.check = func(*testing.T, map[string]any) { cancel() }
	h := newHarness(t, ownPR(), first) // the second page is never asked for
	got, err := h.p.ListComments(cctx, prNumber, start)
	if !errors.Is(err, context.Canceled) || providers.IsRetryable(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
	if len(got.Items) != 0 {
		t.Errorf("returned %d items with the error", len(got.Items))
	}
}

func TestListFeedbackValidatesInput(t *testing.T) {
	h := newHarness(t)
	if _, err := h.p.ListComments(ctx, 0, start); err == nil {
		t.Error("accepted number 0")
	}
	if _, err := h.p.ListReviews(ctx, -1, start); err == nil {
		t.Error("accepted number -1")
	}
	if _, err := h.p.ListReviews(ctx, prNumber, providers.FeedbackCursor{Page: -1}); err == nil {
		t.Error("accepted a negative page")
	}
}

func TestListReviews(t *testing.T) {
	h := newHarness(t, ownPR(), dated(step{method: http.MethodGet, path: reviewsPath, query: pageQuery(1),
		status: http.StatusOK, file: "reviews.json"}))
	got, err := h.p.ListReviews(ctx, prNumber, providers.FeedbackCursor{})
	if err != nil {
		t.Fatal(err)
	}
	// 80003 is a draft.
	if !slices.Equal(ids(got.Items), []int64{80001, 80002, 80004}) || !got.Complete || got.Skipped != 0 {
		t.Fatalf("items %v, complete %v, skipped %d", ids(got.Items), got.Complete, got.Skipped)
	}
	approved, bot := got.Items[1], got.Items[2]
	if approved.Kind != providers.FeedbackReview || approved.ReviewState != "APPROVED" ||
		approved.CommitID != shaCommit || approved.Author.Kind != providers.AuthorPerson {
		t.Errorf("approved = %+v", approved)
	}
	if bot.Author.Kind != providers.AuthorBot || bot.CommitID != "" {
		t.Errorf("bot review = %+v", bot)
	}
	// The next sweep keeps the reviews submitted since this one began.
	if want := (providers.FeedbackCursor{Since: hostNow.Add(-clockMargin)}); got.Next != want {
		t.Errorf("next %+v, want %+v", got.Next, want)
	}
}

func TestListReviewsCursorMovesPastTheBound(t *testing.T) {
	steps := []step{ownPR()}
	for page := 1; page <= providers.MaxFeedbackPages; page++ {
		s := reviewsStep(page, reviewPage(t, feedbackPerPage, int64(page*1000)))
		if page == 1 {
			s = dated(s)
		}
		steps = append(steps, s)
	}
	h := newHarness(t, steps...)
	got, err := h.p.ListReviews(ctx, prNumber, providers.FeedbackCursor{})
	if err != nil {
		t.Fatal(err)
	}
	last := providers.MaxFeedbackPages
	want := providers.FeedbackCursor{Page: last, AfterID: int64(last*1000 + feedbackPerPage - 1), SweepStart: hostNow}
	if got.Complete || got.Next != want {
		t.Fatalf("complete %v, next %+v, want %+v", got.Complete, got.Next, want)
	}
	// Reviews past the bound are read by the next look; the page it starts on is not returned twice.
	h = newHarness(t, ownPR(), reviewsStep(last, reviewPage(t, feedbackPerPage, int64(last*1000))),
		reviewsStep(last+1, reviewPage(t, 3, 9000)))
	got, err = h.p.ListReviews(ctx, prNumber, want)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(got.Items), []int64{9000, 9001, 9002}) || !got.Complete {
		t.Fatalf("items %v, complete %v", ids(got.Items), got.Complete)
	}
	// At the end of the sweep, the next one starts on page 1 from the sweep's start.
	if want := (providers.FeedbackCursor{Since: hostNow.Add(-clockMargin)}); got.Next != want {
		t.Errorf("next %+v, want %+v", got.Next, want)
	}
}

func TestListReviewsSkipsMalformedItems(t *testing.T) {
	good := recorded(t, "reviews.json")
	unknown := maps.Clone(good)
	unknown["id"], unknown["state"] = 1, "SOMETHING_NEW"
	noTime := maps.Clone(good)
	noTime["id"] = 2
	delete(noTime, "submitted_at")
	badCommit := maps.Clone(good)
	badCommit["id"], badCommit["commit_id"] = 3, "not-a-sha"
	last := maps.Clone(good)
	last["id"] = 4
	h := newHarness(t, ownPR(), reviewsStep(1, marshal(t, []any{unknown, noTime, badCommit, last})))
	got, err := h.p.ListReviews(ctx, prNumber, providers.FeedbackCursor{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(got.Items), []int64{4}) || got.Skipped != 3 || !got.Complete {
		t.Errorf("items %v, skipped %d, next %+v", ids(got.Items), got.Skipped, got.Next)
	}
}

func TestListReviewsRefusesAnotherRepository(t *testing.T) {
	h := newHarness(t, ownPR(), step{method: http.MethodGet, path: reviewsPath, query: pageQuery(1),
		status: http.StatusOK, file: "reviews_foreign.json"})
	_, err := h.p.ListReviews(ctx, prNumber, providers.FeedbackCursor{})
	assertSafe(t, err)
}

// Regressions found by a second review: an unread comment moved to an earlier page by deletions, and a draft
// review submitted after a later review.

func TestListCommentsContinuationStepsBackAfterDeletions(t *testing.T) {
	// 151 comments exist before the sweep; the first look reads IDs 1 to 150 and resumes on page 5.
	steps := []step{ownPR()}
	for page := 1; page <= providers.MaxFeedbackPages; page++ {
		s := commentsStep(page, commentPage(t, feedbackPerPage, int64((page-1)*feedbackPerPage+1), since, "hint"))
		if page == 1 {
			s = dated(s)
		}
		steps = append(steps, s)
	}
	h := newHarness(t, steps...)
	first, err := h.p.ListComments(ctx, prNumber, start)
	if err != nil {
		t.Fatal(err)
	}
	// IDs 1 to 31 are deleted: 120 remain, so page 5 is empty and ID 151 is the last item of page 4.
	h = newHarness(t, ownPR(), commentsStep(5, "[]"),
		commentsStep(4, commentPage(t, feedbackPerPage, 122, since, "hint")),
		commentsStep(5, "[]"))
	second, err := h.p.ListComments(ctx, prNumber, first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(second.Items), []int64{151}) || !second.Complete {
		t.Fatalf("continuation items %v, complete %v; want [151], complete", ids(second.Items), second.Complete)
	}
}

func TestListCommentsStepBackSpansLooks(t *testing.T) {
	// The position moved back further than one look can step: the cursor walks back across looks, and never
	// moves its since or its last ID.
	cur := providers.FeedbackCursor{Since: since, Page: 9, AfterID: 270, SweepStart: hostNow}
	steps := []step{ownPR()}
	for page := 9; page > 9-providers.MaxFeedbackPages; page-- {
		steps = append(steps, commentsStep(page, "[]"))
	}
	h := newHarness(t, steps...)
	got, err := h.p.ListComments(ctx, prNumber, cur)
	if err != nil {
		t.Fatal(err)
	}
	want := providers.FeedbackCursor{Since: since, Page: 9 - providers.MaxFeedbackPages, AfterID: 270, SweepStart: hostNow}
	if len(got.Items) != 0 || got.Complete || got.Next != want {
		t.Fatalf("items %v, complete %v, next %+v; want %+v", ids(got.Items), got.Complete, got.Next, want)
	}
}

func TestListReviewsDraftSubmittedLateIsRead(t *testing.T) {
	// Reviewer A starts draft 1, reviewer B submits review 2, which is visible.
	two := recorded(t, "reviews.json")
	two["id"] = 2
	two["html_url"] = "https://github.com/acme/fleet/pull/12#pullrequestreview-2"
	h := newHarness(t, ownPR(), dated(reviewsStep(1, marshal(t, []any{two}))))
	first, err := h.p.ListReviews(ctx, prNumber, providers.FeedbackCursor{})
	if err != nil {
		t.Fatal(err)
	}
	// A submits draft 1 after the first look: it keeps ID 1, before review 2.
	one := maps.Clone(two)
	one["id"], one["submitted_at"] = 1, hostNow.Add(time.Minute).Format(time.RFC3339)
	one["html_url"] = "https://github.com/acme/fleet/pull/12#pullrequestreview-1"
	h = newHarness(t, ownPR(), reviewsStep(1, marshal(t, []any{one, two})))
	second, err := h.p.ListReviews(ctx, prNumber, first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(second.Items), []int64{1}) {
		t.Fatalf("items %v, cursor %+v; want [1]", ids(second.Items), second.Next)
	}
}

func TestReadStreamRefusesATooLargePage(t *testing.T) {
	p := newHarness(t).p
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(make([]byte, 100))),
		ContentLength: -1, Header: http.Header{}}
	_, err := p.readStream(ctx, call{op: opListComments, stream: func(r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		return err
	}}, resp, 10)
	if !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestCanWrite(t *testing.T) {
	for perm, want := range map[string]bool{
		"admin": true, "maintain": true, "write": true,
		"triage": false, "read": false, "none": false,
		"other_user": false, // the login now belongs to another account
	} {
		t.Run(perm, func(t *testing.T) {
			h := newHarness(t, get(permPath, "permission_"+perm+".json"))
			got, err := h.p.CanWrite(ctx, reviewer)
			if err != nil || got != want {
				t.Fatalf("got %v, %v; want %v", got, err, want)
			}
		})
	}
}

func TestCanWriteRefusals(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		h := newHarness(t, status(http.MethodGet, permPath, http.StatusNotFound, ""))
		if got, err := h.p.CanWrite(ctx, reviewer); err != nil || got {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	for _, kind := range []providers.AuthorKind{providers.AuthorBot, providers.AuthorApp, ""} {
		t.Run("kind "+string(kind), func(t *testing.T) {
			h := newHarness(t) // no request
			a := reviewer
			a.Kind = kind
			if got, err := h.p.CanWrite(ctx, a); err != nil || got {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
	for name, a := range map[string]providers.FeedbackAuthor{
		"path in login": {ID: 1, Login: "../../x", Kind: providers.AuthorPerson},
		"no ID":         {Login: "reviewer-one", Kind: providers.AuthorPerson},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			if got, err := h.p.CanWrite(ctx, a); err == nil || got {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
	t.Run("server error is not a refusal", func(t *testing.T) {
		steps := make([]step, 0, DefaultMaxAttempts)
		for range DefaultMaxAttempts {
			steps = append(steps, status(http.MethodGet, permPath, http.StatusServiceUnavailable, ""))
		}
		h := newHarness(t, steps...)
		got, err := h.p.CanWrite(ctx, reviewer)
		if got || !providers.IsRetryable(err) {
			t.Fatalf("got %v, %v", got, err)
		}
		assertSafe(t, err)
	})
}

// etagged is s with GitHub's Date header and the entity tag tag.
func etagged(s step, tag string) step {
	s = dated(s)
	s.header["ETag"] = tag
	return s
}

func TestListCommentsAsksConditionallyWhileNothingChanges(t *testing.T) {
	const tag = `W/"5f1c0a"`
	notModified := commentsStep(1, "")
	notModified.status, notModified.ifNoneMatch = http.StatusNotModified, tag
	changed := etagged(commentsStep(1, commentPage(t, 1, 7, since.Add(time.Hour), "hello")), `"other"`)
	changed.ifNoneMatch = tag
	after := commentsStep(1, "[]")
	after.query = "" // since moved on
	h := newHarness(t, ownPR(), etagged(commentsStep(1, "[]"), tag), ownPR(), notModified, ownPR(), changed,
		ownPR(), after)

	quiet, err := h.p.ListComments(ctx, prNumber, start)
	if err != nil {
		t.Fatal(err)
	}
	want := providers.FeedbackCursor{Since: since, ETag: tag}
	if !quiet.Complete || len(quiet.Items) != 0 || quiet.Next != want {
		t.Fatalf("empty sweep: next %+v, complete %v; want since kept with the tag", quiet.Next, quiet.Complete)
	}
	same, err := h.p.ListComments(ctx, prNumber, quiet.Next)
	if err != nil {
		t.Fatal(err)
	}
	if !same.Complete || len(same.Items) != 0 || same.Next != want {
		t.Fatalf("304: next %+v, complete %v, items %d; want the cursor unchanged", same.Next, same.Complete,
			len(same.Items))
	}
	news, err := h.p.ListComments(ctx, prNumber, same.Next)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(news.Items), []int64{7}) || !news.Complete {
		t.Fatalf("changed answer: items %v, complete %v", ids(news.Items), news.Complete)
	}
	if want := (providers.FeedbackCursor{Since: hostNow.Add(-clockMargin)}); news.Next != want {
		t.Fatalf("after items: next %+v, want %+v (since moved on, no tag: it was the tag of another query)",
			news.Next, want)
	}
	// The next sweep asks for another since without a condition (the step expects no If-None-Match).
	if _, err := h.p.ListComments(ctx, prNumber, news.Next); err != nil {
		t.Fatal(err)
	}
}

func TestListCommentsKeepsNoMalformedOrForeignTag(t *testing.T) {
	for name, tag := range map[string]string{
		"unquoted":  "abc",
		"too long":  `"` + strings.Repeat("a", providers.MaxETagBytes) + `"`,
		"blank":     `"a b"`,
		"quote":     `"a"b"`,
		"no header": "",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, ownPR(), etagged(commentsStep(1, "[]"), tag))
			got, err := h.p.ListComments(ctx, prNumber, start)
			if err != nil {
				t.Fatal(err)
			}
			if got.Next != start {
				t.Errorf("next %+v, want %+v", got.Next, start)
			}
		})
	}
}

func TestListCommentsKeepsNoTagOfASweepWithItemsOrSkips(t *testing.T) {
	malformed := commentPage(t, 1, 3, since, "x")
	malformed = strings.Replace(malformed, `"created_at":"2026-11-08T10:00:00Z"`, `"created_at":"never"`, 1)
	for name, raw := range map[string]string{
		"items":     commentPage(t, 1, 3, since, "x"),
		"malformed": malformed,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, ownPR(), etagged(commentsStep(1, raw), `"t"`))
			got, err := h.p.ListComments(ctx, prNumber, start)
			if err != nil {
				t.Fatal(err)
			}
			if got.Next.ETag != "" || !got.Next.Since.After(since) {
				t.Errorf("next %+v; want since moved on and no tag", got.Next)
			}
		})
	}
}

func TestListFeedbackSendsNoConditionOutsideTheFirstPageOfACommentSweep(t *testing.T) {
	// A continuation of a sweep, and the reviews, never ask conditionally, whatever the cursor holds.
	cont := providers.FeedbackCursor{Since: since, Page: 2, AfterID: 41, SweepStart: hostNow, ETag: `"t"`}
	h := newHarness(t, ownPR(), commentsStep(2, commentPage(t, 2, 41, since, "x")),
		ownPR(), reviewsStep(1, reviewPage(t, 1, 1)))
	if _, err := h.p.ListComments(ctx, prNumber, cont); err != nil {
		t.Fatal(err)
	}
	if _, err := h.p.ListReviews(ctx, prNumber, providers.FeedbackCursor{ETag: `"t"`}); err != nil {
		t.Fatal(err)
	}
}

func TestListCommentsRefusesACursorWithAMalformedTag(t *testing.T) {
	h := newHarness(t)
	if _, err := h.p.ListComments(ctx, prNumber, providers.FeedbackCursor{ETag: "abc\r\nX-Evil: 1"}); err == nil {
		t.Fatal("a malformed tag was accepted")
	}
}
