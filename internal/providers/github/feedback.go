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
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/providers"
)

const (
	opListComments = "GitHub list pull request comments"
	opListReviews  = "GitHub list pull request reviews"
	opPermission   = "GitHub author permission"
)

// feedbackPerPage is the page size of a feedback look, and feedbackPageLimit the most bytes read of one page.
// A body may be 65,536 characters, up to about 384 KiB once JSON-escaped, so a page of feedbackPerPage items
// always fits: otherwise anyone who can comment could make every look fail with a page that is too large.
// A page is decoded one item at a time and each body is cut as it is decoded, so a look holds at most
// MaxFeedbackBytes of bodies plus one item, however many bytes it reads.
const (
	feedbackPerPage   = 30
	feedbackPageLimit = feedbackPerPage * (400 << 10)
)

// clockMargin is subtracted from the host's time when a sweep of comments began, to make the next sweep's
// since: GitHub's times are whole seconds.
const clockMargin = time.Second

// loginPattern bounds a login before it goes into a path. GitHub logins are shorter and use fewer characters;
// this is the pattern of the API's HintAuthor.login.
var loginPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

var _ providers.FeedbackSource = (*Provider)(nil)

type userJSON struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

// Times are strings, parsed per item, so a malformed time makes that item malformed instead of the page.
type commentJSON struct {
	ID        int64     `json:"id"`
	HTMLURL   string    `json:"html_url"`
	IssueURL  string    `json:"issue_url"`
	User      *userJSON `json:"user"`
	Body      *string   `json:"body"`
	CreatedAt string    `json:"created_at"`
	UpdatedAt string    `json:"updated_at"`
	// PerformedViaGitHubApp is set when an App posted the comment, also for a user.
	PerformedViaGitHubApp *struct{} `json:"performed_via_github_app"`
}

type reviewJSON struct {
	ID             int64     `json:"id"`
	HTMLURL        string    `json:"html_url"`
	PullRequestURL string    `json:"pull_request_url"`
	User           *userJSON `json:"user"`
	Body           *string   `json:"body"`
	State          string    `json:"state"`
	CommitID       *string   `json:"commit_id"`
	SubmittedAt    *string   `json:"submitted_at"`
}

// reviewStates are the states of a submitted review. PENDING is a draft only its author sees, and is ignored.
// A review of a commit that is gone may have no commit_id.
var reviewStates = map[string]bool{"APPROVED": true, "CHANGES_REQUESTED": true, "COMMENTED": true, "DISMISSED": true}

// errForeignItem refuses a whole look: an item names another repository, pull request or an issue.
var errForeignItem = fmt.Errorf("an item of another repository or pull request: %w", errStreamRefused)

// item is one decoded element of a page.
type item struct {
	id int64
	// malformed items are counted as skipped; ignored ones (a pending review) are not counted.
	malformed, ignored bool
	fb                 providers.Feedback
}

// author converts the user of an item. A missing or malformed user is an error, so an item is never
// attributed to nobody.
func author(u *userJSON, viaApp bool) (providers.FeedbackAuthor, bool) {
	if u == nil || u.ID < 1 || !loginPattern.MatchString(strings.TrimSuffix(u.Login, "[bot]")) {
		return providers.FeedbackAuthor{}, false
	}
	a := providers.FeedbackAuthor{ID: u.ID, Login: u.Login, Kind: providers.AuthorPerson}
	switch {
	case u.Type != "User" || strings.HasSuffix(u.Login, "[bot]"):
		a.Kind = providers.AuthorBot
	case viaApp:
		a.Kind = providers.AuthorApp
	}
	return a, true
}

// cutBody bounds a body to providers.MaxFeedbackBodyBytes without splitting a rune, and reports a cut. Invalid
// UTF-8 is replaced first, so the result is always valid.
func cutBody(s string) (string, bool) {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= providers.MaxFeedbackBodyBytes {
		return s, false
	}
	cut := providers.MaxFeedbackBodyBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil && !t.IsZero()
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// pageResult is what decodePage kept of one page.
type pageResult struct {
	items []item
	// n is how many elements the page held, or had been decoded when the byte budget stopped it.
	n int
	// stopped is true when the byte budget ended the page before its last element.
	stopped bool
}

// decodePage reads a JSON array one element at a time. An element that is not a T is a malformed item; one
// that conv reports foreign refuses the page. Elements with an ID up to afterID are dropped, and keeping stops
// before the bodies kept would pass budget, without reading further.
func decodePage[T any](r io.Reader, budget int, afterID int64, conv func(T) (item, bool)) (pageResult, error) {
	var res pageResult
	dec := json.NewDecoder(r)
	if tok, err := dec.Token(); err != nil {
		return res, err
	} else if d, ok := tok.(json.Delim); !ok || d != '[' {
		return res, &json.SyntaxError{}
	}
	used := 0
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return res, err
		}
		res.n++
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			res.items = append(res.items, item{malformed: true})
			continue
		}
		it, own := conv(v)
		switch {
		case !own:
			return res, errForeignItem
		case it.id >= 1 && it.id <= afterID:
			continue
		case !it.malformed && !it.ignored:
			if used+len(it.fb.Body) > budget {
				res.stopped = true
				return res, nil
			}
			used += len(it.fb.Body)
		}
		res.items = append(res.items, it)
	}
	if _, err := dec.Token(); err != nil {
		return res, err
	}
	return res, nil
}

// lookResult is one look over a list.
type lookResult struct {
	page providers.FeedbackPage
	// ended is true when the look read a page that was not full: nothing more is there now.
	ended    bool
	lastPage int
	afterID  int64
	// started is the host's time of the look's first response; zero when it sent none.
	started time.Time
}

// look reads up to providers.MaxFeedbackPages pages of path from cursor's page, each within its own deadline,
// so a slow page does not take the time of the pages before it. It returns the items after cursor.AfterID.
func (p *Provider) look(ctx context.Context, op, path string, query url.Values, cur providers.FeedbackCursor,
	decode func(r io.Reader, budget int, afterID int64) (pageResult, error)) (lookResult, error) {
	first := max(cur.Page, 1)
	res := lookResult{lastPage: first, afterID: cur.AfterID}
	budget := providers.MaxFeedbackBytes
	for page := first; page < first+providers.MaxFeedbackPages; page++ {
		q := url.Values{paramPerPage: {strconv.Itoa(feedbackPerPage)}, "page": {strconv.Itoa(page)}}
		maps.Copy(q, query)
		var pr pageResult
		pctx, cancel := p.withTimeout(ctx)
		r, err := p.do(pctx, call{op: op, method: http.MethodGet, path: path, query: q, limit: feedbackPageLimit,
			stream: func(body io.Reader) error {
				var err error
				pr, err = decode(body, budget, res.afterID)
				if errors.Is(err, errForeignItem) {
					return fmt.Errorf("%s: GitHub returned %w", op, err)
				}
				return err
			}})
		cancel()
		if err != nil {
			return lookResult{}, err
		}
		if !r.ok() {
			return lookResult{}, statusError(op, r.status)
		}
		if page == first {
			if t, err := http.ParseTime(r.header.Get("Date")); err == nil {
				res.started = t
			}
		}
		res.lastPage = page
		for _, it := range pr.items {
			res.afterID = max(res.afterID, it.id)
			switch {
			case it.malformed:
				res.page.Skipped++
			case !it.ignored:
				budget -= len(it.fb.Body)
				res.page.Items = append(res.page.Items, it.fb)
			}
		}
		if pr.stopped {
			return res, nil
		}
		if pr.n < feedbackPerPage {
			res.ended = true
			return res, nil
		}
	}
	return res, nil
}

// ownPull refuses a pull request that is not a Shadewright one to the target branch of this repository,
// within its own deadline.
func (p *Provider) ownPull(ctx context.Context, op string, number int64) error {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	_, err := p.pullFor(ctx, op, number)
	return err
}

func validCursor(c providers.FeedbackCursor) bool {
	return c.Page >= 0 && c.AfterID >= 0
}

// ListComments returns the conversation comments of pull request number, oldest first, within the bounds of
// one look. A sweep asks for the comments updated since cursor.Since and pages through them across looks;
// when it reaches the end, the next sweep asks for those updated since this one began (by GitHub's clock), so
// an edit made during a sweep is read by the next one, and Since never decreases. A pull request that is not
// Shadewright's is refused, and so is a look holding a comment of another repository, issue or pull request.
func (p *Provider) ListComments(ctx context.Context, number int64, cur providers.FeedbackCursor) (providers.FeedbackPage, error) {
	if number < 1 || !validCursor(cur) {
		return providers.FeedbackPage{}, errors.New(opListComments + ": number or cursor is invalid")
	}
	if err := p.ownPull(ctx, opListComments, number); err != nil {
		return providers.FeedbackPage{}, err
	}
	num := strconv.FormatInt(number, 10)
	wantIssue := apiBase + "/repos/" + p.repo.String() + "/issues/" + num
	wantHTML := "https://github.com/" + p.repo.String() + "/pull/" + num + "#"
	conv := func(c commentJSON) (item, bool) {
		// The issue URL names the repository and the number; the HTML URL proves a pull request.
		if !strings.EqualFold(c.IssueURL, wantIssue) || !hasPrefixFold(c.HTMLURL, wantHTML) {
			return item{}, false
		}
		it := item{id: c.ID, malformed: true}
		created, okC := parseTime(c.CreatedAt)
		updated, okU := parseTime(c.UpdatedAt)
		a, okA := author(c.User, c.PerformedViaGitHubApp != nil)
		if c.ID < 1 || !okC || !okU || updated.Before(created) || !okA {
			return it, true
		}
		it.malformed = false
		it.fb = providers.Feedback{Kind: providers.FeedbackComment, ID: c.ID, Author: a,
			CreatedAt: created, UpdatedAt: updated}
		if c.Body != nil {
			it.fb.Body, it.fb.Truncated = cutBody(*c.Body)
		}
		return it, true
	}
	var query url.Values
	if !cur.Since.IsZero() {
		query = url.Values{"since": {cur.Since.UTC().Format(time.RFC3339)}}
	}
	res, err := p.look(ctx, opListComments, "/issues/"+num+"/comments", query, cur,
		func(r io.Reader, budget int, afterID int64) (pageResult, error) {
			return decodePage(r, budget, afterID, conv)
		})
	if err != nil {
		return providers.FeedbackPage{}, err
	}
	// Without a Date header the sweep has no start, and the next sweep keeps the same since: it reads again,
	// never skips.
	start := cur.SweepStart
	if cur.Page == 0 {
		start = res.started
	}
	out := res.page
	if res.ended {
		out.Complete = true
		out.Next = providers.FeedbackCursor{Since: cur.Since}
		if s := start.Add(-clockMargin); !start.IsZero() && s.After(cur.Since) {
			out.Next.Since = s
		}
		return out, nil
	}
	out.Next = providers.FeedbackCursor{Since: cur.Since, Page: res.lastPage, AfterID: res.afterID, SweepStart: start}
	return out, nil
}

// ListReviews returns the bodies of the submitted reviews of pull request number after cursor, oldest first,
// within the bounds of one look. GitHub lists reviews in the order they were created and has no since, so the
// cursor is a page and the last review ID returned: each look goes on from there, and a look that reached the
// end starts the next on that same page, to see the reviews added to it. A pull request that is not
// Shadewright's is refused, and so is a look holding a review of another repository or pull request.
func (p *Provider) ListReviews(ctx context.Context, number int64, cur providers.FeedbackCursor) (providers.FeedbackPage, error) {
	if number < 1 || !validCursor(cur) {
		return providers.FeedbackPage{}, errors.New(opListReviews + ": number or cursor is invalid")
	}
	if err := p.ownPull(ctx, opListReviews, number); err != nil {
		return providers.FeedbackPage{}, err
	}
	num := strconv.FormatInt(number, 10)
	wantPull := apiBase + "/repos/" + p.repo.String() + "/pulls/" + num
	wantHTML := "https://github.com/" + p.repo.String() + "/pull/" + num + "#"
	conv := func(rv reviewJSON) (item, bool) {
		if !strings.EqualFold(rv.PullRequestURL, wantPull) || !hasPrefixFold(rv.HTMLURL, wantHTML) {
			return item{}, false
		}
		it := item{id: rv.ID, malformed: true}
		if rv.State == "PENDING" {
			// A draft gets its ID when it is started, so a later submit could fall behind the cursor; only the
			// App's own drafts are visible to it, and it writes none.
			it.malformed, it.ignored = false, true
			return it, true
		}
		var submitted time.Time
		ok := rv.SubmittedAt != nil
		if ok {
			submitted, ok = parseTime(*rv.SubmittedAt)
		}
		commit := ""
		if rv.CommitID != nil {
			commit = *rv.CommitID
		}
		a, okA := author(rv.User, false)
		if rv.ID < 1 || !reviewStates[rv.State] || !ok || (commit != "" && !validSHA(commit)) || !okA {
			return it, true
		}
		it.malformed = false
		it.fb = providers.Feedback{Kind: providers.FeedbackReview, ID: rv.ID, Author: a,
			CreatedAt: submitted, UpdatedAt: submitted, ReviewState: rv.State, CommitID: commit}
		if rv.Body != nil {
			it.fb.Body, it.fb.Truncated = cutBody(*rv.Body)
		}
		return it, true
	}
	res, err := p.look(ctx, opListReviews, "/pulls/"+num+"/reviews", nil, cur,
		func(r io.Reader, budget int, afterID int64) (pageResult, error) {
			return decodePage(r, budget, afterID, conv)
		})
	if err != nil {
		return providers.FeedbackPage{}, err
	}
	out := res.page
	out.Complete = res.ended
	out.Next = providers.FeedbackCursor{Page: res.lastPage, AfterID: res.afterID}
	return out, nil
}

// writePermissions are the values of the base permission field that may write. GitHub reports maintain and
// custom roles that include write as write; maintain is accepted in case it is reported as itself.
var writePermissions = map[string]bool{"admin": true, "maintain": true, "write": true}

// CanWrite asks GitHub for the author's permission on the repository (GET collaborators/{login}/permission)
// and accepts admin, maintain and write. triage, read, none, anything else and a 404 are refused, as is an
// answer for another account than author.ID (a login renamed and taken by someone else). A Bot or App is
// refused without a request.
func (p *Provider) CanWrite(ctx context.Context, a providers.FeedbackAuthor) (bool, error) {
	if a.Kind != providers.AuthorPerson {
		return false, nil
	}
	if a.ID < 1 || !loginPattern.MatchString(a.Login) {
		return false, errors.New(opPermission + ": author is invalid")
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	r, err := p.do(ctx, call{op: opPermission, method: http.MethodGet,
		path: "/collaborators/" + url.PathEscape(a.Login) + "/permission"})
	if err != nil {
		return false, err
	}
	if r.status == http.StatusNotFound {
		return false, nil
	}
	if !r.ok() {
		return false, statusError(opPermission, r.status)
	}
	var v struct {
		Permission string    `json:"permission"`
		User       *userJSON `json:"user"`
	}
	if err := decode(opPermission, r, &v); err != nil {
		return false, err
	}
	if v.User == nil || v.User.ID != a.ID || v.User.Type != "User" {
		return false, nil
	}
	return writePermissions[v.Permission], nil
}
