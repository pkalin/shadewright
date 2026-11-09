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
	"errors"
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

// feedbackPerPage is the page size of a feedback look. GitHub allows 100, but a body may be 65,536 characters,
// up to about 384 KiB once JSON-escaped, and a page must always fit in feedbackPageLimit: otherwise anyone who
// can comment could make every look fail with a page that is too large.
const (
	feedbackPerPage   = 30
	feedbackPageLimit = feedbackPerPage * (400 << 10)
)

// loginPattern bounds a login before it goes into a path. GitHub logins are shorter and use fewer characters;
// this is the pattern of the API's HintAuthor.login.
var loginPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

var _ providers.FeedbackSource = (*Provider)(nil)

type userJSON struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

type commentJSON struct {
	ID        int64     `json:"id"`
	HTMLURL   string    `json:"html_url"`
	IssueURL  string    `json:"issue_url"`
	User      *userJSON `json:"user"`
	Body      *string   `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// PerformedViaGitHubApp is set when an App posted the comment, also for a user.
	PerformedViaGitHubApp *struct{} `json:"performed_via_github_app"`
}

type reviewJSON struct {
	ID             int64      `json:"id"`
	HTMLURL        string     `json:"html_url"`
	PullRequestURL string     `json:"pull_request_url"`
	User           *userJSON  `json:"user"`
	Body           *string    `json:"body"`
	State          string     `json:"state"`
	CommitID       string     `json:"commit_id"`
	SubmittedAt    *time.Time `json:"submitted_at"`
}

// reviewStates are the states of a submitted review. PENDING is a draft only its author sees, and is skipped.
// A review of a commit that is gone may have no commit_id.
var reviewStates = map[string]bool{"APPROVED": true, "CHANGES_REQUESTED": true, "COMMENTED": true, "DISMISSED": true}

// author converts the user of an item. A missing user (a deleted account) or a malformed one is refused, so
// an item is never attributed to nobody.
func author(op string, u *userJSON, viaApp bool) (providers.FeedbackAuthor, error) {
	if u == nil || u.ID < 1 || !loginPattern.MatchString(strings.TrimSuffix(u.Login, "[bot]")) {
		return providers.FeedbackAuthor{}, errors.New(op + ": GitHub returned an item without a valid author")
	}
	a := providers.FeedbackAuthor{ID: u.ID, Login: u.Login, Kind: providers.AuthorPerson}
	switch {
	case u.Type != "User" || strings.HasSuffix(u.Login, "[bot]"):
		a.Kind = providers.AuthorBot
	case viaApp:
		a.Kind = providers.AuthorApp
	}
	return a, nil
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

// look collects the items of one look within its bounds.
type look struct {
	since time.Time
	page  providers.FeedbackPage
	bytes int
}

// add keeps item unless the byte bound is reached, which ends the look as incomplete.
func (l *look) add(item providers.Feedback) bool {
	if l.bytes+len(item.Body) > providers.MaxFeedbackBytes {
		return false
	}
	l.bytes += len(item.Body)
	l.page.Items = append(l.page.Items, item)
	return true
}

// finish sets Complete and Resume. An incomplete look resumes no later than the creation of its last item:
// an item past the bound was created after it, and updated no earlier than it was created.
func (l *look) finish(complete bool) providers.FeedbackPage {
	l.page.Complete = complete
	l.page.Resume = l.since
	for _, it := range l.page.Items {
		if it.UpdatedAt.After(l.page.Resume) {
			l.page.Resume = it.UpdatedAt
		}
	}
	if n := len(l.page.Items); !complete && n > 0 {
		if last := l.page.Items[n-1].CreatedAt; last.Before(l.page.Resume) {
			l.page.Resume = last
		}
	}
	return l.page
}

// listPages reads up to providers.MaxFeedbackPages pages of path, handing each decoded page to each. each
// returns false to stop the look as incomplete. full reports whether a page held feedbackPerPage items.
func (p *Provider) listPages(ctx context.Context, op, path string, query url.Values,
	each func(r response) (cont, full bool, err error)) (bool, error) {
	for page := 1; page <= providers.MaxFeedbackPages; page++ {
		q := url.Values{paramPerPage: {strconv.Itoa(feedbackPerPage)}, "page": {strconv.Itoa(page)}}
		maps.Copy(q, query)
		r, err := p.do(ctx, call{op: op, method: http.MethodGet, path: path, query: q, limit: feedbackPageLimit})
		if err != nil {
			return false, err
		}
		if !r.ok() {
			return false, statusError(op, r.status)
		}
		cont, full, err := each(r)
		switch {
		case err != nil:
			return false, err
		case !cont:
			return false, nil
		case !full:
			return true, nil
		}
	}
	return false, nil
}

// ListComments returns the conversation comments of pull request number updated at or after since, oldest
// first, within the bounds of one look. A comment of another repository, or of another issue or pull request,
// fails the whole look.
func (p *Provider) ListComments(ctx context.Context, number int64, since time.Time) (providers.FeedbackPage, error) {
	if number < 1 {
		return providers.FeedbackPage{}, errors.New(opListComments + ": number must be positive")
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	num := strconv.FormatInt(number, 10)
	wantIssue := apiBase + "/repos/" + p.repo.String() + "/issues/" + num
	wantHTML := "https://github.com/" + p.repo.String() + "/pull/" + num + "#"
	var query url.Values
	if !since.IsZero() {
		query = url.Values{"since": {since.UTC().Format(time.RFC3339)}}
	}
	l := look{since: since}
	complete, err := p.listPages(ctx, opListComments, "/issues/"+num+"/comments", query,
		func(r response) (bool, bool, error) {
			var page []commentJSON
			if err := decode(opListComments, r, &page); err != nil {
				return false, false, err
			}
			for _, c := range page {
				// The issue URL names the repository and the number; the HTML URL proves a pull request.
				if !strings.EqualFold(c.IssueURL, wantIssue) || !hasPrefixFold(c.HTMLURL, wantHTML) {
					return false, false, errors.New(opListComments + ": GitHub returned a comment of another repository or pull request")
				}
				if c.ID < 1 || c.CreatedAt.IsZero() || c.UpdatedAt.Before(c.CreatedAt) {
					return false, false, errors.New(opListComments + ": GitHub returned an unexpected comment")
				}
				a, err := author(opListComments, c.User, c.PerformedViaGitHubApp != nil)
				if err != nil {
					return false, false, err
				}
				item := providers.Feedback{Kind: providers.FeedbackComment, ID: c.ID, Author: a,
					CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
				if c.Body != nil {
					item.Body, item.Truncated = cutBody(*c.Body)
				}
				if !l.add(item) {
					return false, false, nil
				}
			}
			return true, len(page) >= feedbackPerPage, nil
		})
	if err != nil {
		return providers.FeedbackPage{}, err
	}
	return l.finish(complete), nil
}

// ListReviews returns the bodies of the submitted reviews of pull request number submitted at or after since,
// oldest first, within the bounds of one look. GitHub has no since for reviews, so pages are read from the
// first one and older reviews are skipped here; reviews past the page bound are reported as incomplete.
func (p *Provider) ListReviews(ctx context.Context, number int64, since time.Time) (providers.FeedbackPage, error) {
	if number < 1 {
		return providers.FeedbackPage{}, errors.New(opListReviews + ": number must be positive")
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	num := strconv.FormatInt(number, 10)
	wantPull := apiBase + "/repos/" + p.repo.String() + "/pulls/" + num
	wantHTML := "https://github.com/" + p.repo.String() + "/pull/" + num + "#"
	l := look{since: since}
	complete, err := p.listPages(ctx, opListReviews, "/pulls/"+num+"/reviews", nil,
		func(r response) (bool, bool, error) {
			var page []reviewJSON
			if err := decode(opListReviews, r, &page); err != nil {
				return false, false, err
			}
			for _, rv := range page {
				if !strings.EqualFold(rv.PullRequestURL, wantPull) || !hasPrefixFold(rv.HTMLURL, wantHTML) {
					return false, false, errors.New(opListReviews + ": GitHub returned a review of another repository or pull request")
				}
				if rv.State == "PENDING" {
					continue
				}
				if rv.ID < 1 || !reviewStates[rv.State] || rv.SubmittedAt == nil ||
					(rv.CommitID != "" && !validSHA(rv.CommitID)) {
					return false, false, errors.New(opListReviews + ": GitHub returned an unexpected review")
				}
				if rv.SubmittedAt.Before(since) {
					continue
				}
				a, err := author(opListReviews, rv.User, false)
				if err != nil {
					return false, false, err
				}
				item := providers.Feedback{Kind: providers.FeedbackReview, ID: rv.ID, Author: a,
					CreatedAt: *rv.SubmittedAt, UpdatedAt: *rv.SubmittedAt, ReviewState: rv.State, CommitID: rv.CommitID}
				if rv.Body != nil {
					item.Body, item.Truncated = cutBody(*rv.Body)
				}
				if !l.add(item) {
					return false, false, nil
				}
			}
			return true, len(page) >= feedbackPerPage, nil
		})
	if err != nil {
		return providers.FeedbackPage{}, err
	}
	return l.finish(complete), nil
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
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
