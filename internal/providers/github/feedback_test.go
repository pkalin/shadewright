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
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
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
	permPath     = "/collaborators/reviewer-one/permission"
	prNumber     = 12
)

var (
	since    = time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	reviewer = providers.FeedbackAuthor{ID: 1234567, Login: "reviewer-one", Kind: providers.AuthorPerson}
)

func pageQuery(page int) string {
	return "page=" + strconv.Itoa(page) + "&per_page=" + strconv.Itoa(feedbackPerPage)
}

func commentsQuery(page int) string {
	return pageQuery(page) + "&since=2026-11-08T10%3A00%3A00Z"
}

// recordedComment returns the first comment of comments_mixed.json as a map, to build pages from.
func recordedComment(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "comments_mixed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var all []map[string]any
	if err := json.Unmarshal(data, &all); err != nil {
		t.Fatal(err)
	}
	return all[0]
}

// commentPage is n copies of the recorded comment, one minute apart from first, with IDs from id and body.
func commentPage(t *testing.T, n int, id int64, first time.Time, body string) string {
	t.Helper()
	base := recordedComment(t)
	page := make([]map[string]any, n)
	for i := range page {
		c := maps.Clone(base)
		cid := id + int64(i)
		at := first.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		c["id"], c["created_at"], c["updated_at"], c["body"] = cid, at, at, body
		c["html_url"] = "https://github.com/acme/fleet/pull/12#issuecomment-" + strconv.FormatInt(cid, 10)
		page[i] = c
	}
	data, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func commentsStep(page int, raw string) step {
	return step{method: http.MethodGet, path: commentsPath, query: commentsQuery(page), status: http.StatusOK, raw: raw}
}

func TestListCommentsFlagsBotsAndApps(t *testing.T) {
	h := newHarness(t, step{method: http.MethodGet, path: commentsPath, query: commentsQuery(1),
		status: http.StatusOK, file: "comments_mixed.json"})
	got, err := h.p.ListComments(ctx, prNumber, since)
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
	// A short page is the last one; Resume is the latest update seen.
	if !got.Complete || !got.Resume.Equal(time.Date(2026, 11, 8, 10, 20, 0, 0, time.UTC)) {
		t.Errorf("complete %v, resume %v", got.Complete, got.Resume)
	}
}

func TestListCommentsPagingStopsAtTheBound(t *testing.T) {
	steps := make([]step, 0, providers.MaxFeedbackPages)
	for page := 1; page <= providers.MaxFeedbackPages; page++ {
		first := since.Add(time.Duration(page) * time.Hour)
		steps = append(steps, commentsStep(page, commentPage(t, feedbackPerPage, int64(page*1000), first, "ok")))
	}
	h := newHarness(t, steps...) // a sixth request fails the script
	got, err := h.p.ListComments(ctx, prNumber, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != providers.MaxFeedbackPages*feedbackPerPage || got.Complete {
		t.Fatalf("got %d items, complete %v", len(got.Items), got.Complete)
	}
	last := got.Items[len(got.Items)-1]
	if !got.Resume.Equal(last.CreatedAt) {
		t.Errorf("resume %v, want the creation of the last item %v", got.Resume, last.CreatedAt)
	}
}

func TestListCommentsResumeNeverPassesAnUnreadComment(t *testing.T) {
	// The first comment of the last page was edited late: an incomplete look must not resume after it.
	steps := make([]step, 0, providers.MaxFeedbackPages)
	for page := 1; page <= providers.MaxFeedbackPages; page++ {
		raw := commentPage(t, feedbackPerPage, int64(page*1000), since.Add(time.Duration(page)*time.Hour), "ok")
		if page == 1 {
			raw = strings.Replace(raw, `"updated_at":"2026-11-08T11:00:00Z"`, `"updated_at":"2026-11-09T00:00:00Z"`, 1)
		}
		steps = append(steps, commentsStep(page, raw))
	}
	h := newHarness(t, steps...)
	got, err := h.p.ListComments(ctx, prNumber, since)
	if err != nil {
		t.Fatal(err)
	}
	if want := got.Items[len(got.Items)-1].CreatedAt; !got.Resume.Equal(want) {
		t.Errorf("resume %v, want %v", got.Resume, want)
	}
}

func TestListCommentsCutsALongBody(t *testing.T) {
	// A three-byte rune straddles the bound, so a cut by bytes alone would split it.
	long := strings.Repeat("a", providers.MaxFeedbackBodyBytes-1) + "€" + strings.Repeat("b", 100)
	h := newHarness(t, commentsStep(1, commentPage(t, 1, 1, since, long)))
	got, err := h.p.ListComments(ctx, prNumber, since)
	if err != nil {
		t.Fatal(err)
	}
	it := got.Items[0]
	if !it.Truncated || len(it.Body) > providers.MaxFeedbackBodyBytes || !utf8.ValidString(it.Body) ||
		!strings.HasPrefix(long, it.Body) || len(it.Body) != providers.MaxFeedbackBodyBytes-1 {
		t.Errorf("body of %d bytes, truncated %v", len(it.Body), it.Truncated)
	}
}

func TestListCommentsStopsAtTheByteBound(t *testing.T) {
	body := strings.Repeat("x", providers.MaxFeedbackBodyBytes)
	keep := providers.MaxFeedbackBytes / providers.MaxFeedbackBodyBytes
	// Two full pages hold more than the byte bound: the look ends inside the second, without a third request.
	if keep <= feedbackPerPage || keep >= 2*feedbackPerPage {
		t.Fatalf("test needs the byte bound inside the second page")
	}
	h := newHarness(t,
		commentsStep(1, commentPage(t, feedbackPerPage, 1, since, body)),
		commentsStep(2, commentPage(t, feedbackPerPage, 1000, since.Add(time.Hour), body)))
	got, err := h.p.ListComments(ctx, prNumber, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != keep || got.Complete {
		t.Fatalf("got %d items, complete %v; want %d, incomplete", len(got.Items), got.Complete, keep)
	}
	if want := got.Items[keep-1].CreatedAt; !got.Resume.Equal(want) {
		t.Errorf("resume %v, want %v", got.Resume, want)
	}
}

func TestListCommentsRefusesAnotherRepository(t *testing.T) {
	for name, file := range map[string]string{
		"other repository": "comments_foreign.json",
		"issue, not pull":  "comments_on_issue.json",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, step{method: http.MethodGet, path: commentsPath, query: commentsQuery(1),
				status: http.StatusOK, file: file})
			got, err := h.p.ListComments(ctx, prNumber, since)
			assertSafe(t, err)
			if len(got.Items) != 0 {
				t.Errorf("returned %d items with the error", len(got.Items))
			}
		})
	}
}

func TestListCommentsWithoutSince(t *testing.T) {
	h := newHarness(t, step{method: http.MethodGet, path: commentsPath, query: pageQuery(1),
		status: http.StatusOK, raw: "[]"})
	got, err := h.p.ListComments(ctx, prNumber, time.Time{})
	if err != nil || !got.Complete || len(got.Items) != 0 || !got.Resume.IsZero() {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestListCommentsRefusesAnInvalidAuthor(t *testing.T) {
	raw := `[{"id":5,"html_url":"https://github.com/acme/fleet/pull/12#issuecomment-5",` +
		`"issue_url":"https://api.github.com/repos/acme/fleet/issues/12","user":null,` +
		`"created_at":"2026-11-08T10:12:03Z","updated_at":"2026-11-08T10:12:03Z","body":"x"}]`
	h := newHarness(t, commentsStep(1, raw))
	_, err := h.p.ListComments(ctx, prNumber, since)
	assertSafe(t, err)
}

func TestListCommentsServerErrors(t *testing.T) {
	t.Run("retried", func(t *testing.T) {
		h := newHarness(t,
			step{method: http.MethodGet, path: commentsPath, status: http.StatusBadGateway},
			commentsStep(1, "[]"))
		if _, err := h.p.ListComments(ctx, prNumber, since); err != nil {
			t.Fatal(err)
		}
		if len(h.sleeps) != 1 {
			t.Errorf("slept %d times", len(h.sleeps))
		}
	})
	t.Run("final", func(t *testing.T) {
		steps := make([]step, 0, DefaultMaxAttempts)
		for range DefaultMaxAttempts {
			steps = append(steps, step{method: http.MethodGet, path: commentsPath, status: http.StatusInternalServerError})
		}
		h := newHarness(t, steps...)
		_, err := h.p.ListComments(ctx, prNumber, since)
		if !providers.IsRetryable(err) {
			t.Fatalf("got %v", err)
		}
		assertSafe(t, err)
	})
}

func TestListCommentsRateLimit(t *testing.T) {
	t.Run("short delay waited for", func(t *testing.T) {
		h := newHarness(t,
			step{method: http.MethodGet, path: commentsPath, status: http.StatusTooManyRequests,
				header: map[string]string{hRetry: "3"}},
			commentsStep(1, "[]"))
		if _, err := h.p.ListComments(ctx, prNumber, since); err != nil {
			t.Fatal(err)
		}
		if len(h.sleeps) != 1 || h.sleeps[0] != 3*time.Second {
			t.Errorf("sleeps = %v", h.sleeps)
		}
	})
	t.Run("long delay returned", func(t *testing.T) {
		reset := strconv.FormatInt(t0.Add(10*time.Minute).Unix(), 10)
		h := newHarness(t, step{method: http.MethodGet, path: commentsPath, status: http.StatusForbidden,
			header: map[string]string{hdrRemaining: "0", hdrReset: reset}})
		_, err := h.p.ListComments(ctx, prNumber, since)
		if d, ok := providers.RetryAfter(err); !ok || d != 10*time.Minute {
			t.Fatalf("got %v", err)
		}
		assertSafe(t, err)
	})
	t.Run("forbidden without rate limit is final", func(t *testing.T) {
		h := newHarness(t, status(http.MethodGet, commentsPath, http.StatusForbidden, fxForbidden))
		_, err := h.p.ListComments(ctx, prNumber, since)
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
	h := newHarness(t, first) // the second page is never asked for
	got, err := h.p.ListComments(cctx, prNumber, since)
	if !errors.Is(err, context.Canceled) || providers.IsRetryable(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
	if len(got.Items) != 0 {
		t.Errorf("returned %d items with the error", len(got.Items))
	}
}

func TestListCommentsValidatesNumber(t *testing.T) {
	h := newHarness(t)
	if _, err := h.p.ListComments(ctx, 0, since); err == nil {
		t.Error("accepted number 0")
	}
	if _, err := h.p.ListReviews(ctx, -1, since); err == nil {
		t.Error("accepted number -1")
	}
}

func TestListReviews(t *testing.T) {
	h := newHarness(t, step{method: http.MethodGet, path: reviewsPath, query: pageQuery(1),
		status: http.StatusOK, file: "reviews.json"})
	got, err := h.p.ListReviews(ctx, prNumber, since)
	if err != nil {
		t.Fatal(err)
	}
	// 80001 was submitted before since, 80003 is a draft.
	if len(got.Items) != 2 || !got.Complete {
		t.Fatalf("got %+v", got)
	}
	approved, bot := got.Items[0], got.Items[1]
	if approved.ID != 80002 || approved.Kind != providers.FeedbackReview || approved.ReviewState != "APPROVED" ||
		approved.CommitID != shaCommit || approved.Author.Kind != providers.AuthorPerson {
		t.Errorf("approved = %+v", approved)
	}
	if bot.ID != 80004 || bot.Author.Kind != providers.AuthorBot || bot.CommitID != "" {
		t.Errorf("bot review = %+v", bot)
	}
	if want := time.Date(2026, 11, 8, 11, 30, 0, 0, time.UTC); !got.Resume.Equal(want) {
		t.Errorf("resume %v, want %v", got.Resume, want)
	}
}

func TestListReviewsRefusesAnotherRepository(t *testing.T) {
	h := newHarness(t, step{method: http.MethodGet, path: reviewsPath, query: pageQuery(1),
		status: http.StatusOK, file: "reviews_foreign.json"})
	_, err := h.p.ListReviews(ctx, prNumber, since)
	assertSafe(t, err)
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
