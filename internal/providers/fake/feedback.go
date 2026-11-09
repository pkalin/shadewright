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

package fake

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
)

var _ providers.FeedbackSource = (*Git)(nil)

// DefaultFeedbackPerLook is how many comments one look of the fake returns at most, as many as the GitHub
// adapter's page bound holds (5 pages of 30).
const DefaultFeedbackPerLook = 150

// feedbackStore holds the conversation comments of the fake's change requests and who may write.
type feedbackStore struct {
	comments map[int64][]providers.Feedback
	writers  map[int64]bool
	now      func() time.Time
	perLook  int
	lists    int
	asks     int
}

// The fake reads comments in sweeps as the GitHub adapter does: a sweep asks for the comments updated at or after
// the cursor's Since, at most perLook per look, in ID order, and goes on after the last ID; once it reaches the
// end, the next sweep asks from the fake's clock when this one began, less a second, so comments updated within
// that second are returned twice. A sweep that found nothing keeps the cursor as it is (the adapter's quiet
// sweep), so an unchanged list changes no cursor. Every comment created before Since was returned already.

// AddComment adds a conversation comment to change request n. Only Kind, ID, Author, Body, CreatedAt and
// UpdatedAt matter; a zero UpdatedAt is CreatedAt.
func (g *Git) AddComment(n int64, c providers.Feedback) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = c.CreatedAt
	}
	if c.Kind == "" {
		c.Kind = providers.FeedbackComment
	}
	if g.fb.comments == nil {
		g.fb.comments = map[int64][]providers.Feedback{}
	}
	g.fb.comments[n] = append(g.fb.comments[n], c)
}

// EditComment replaces the body of comment id on change request n and sets its update time to at.
func (g *Git) EditComment(n, id int64, body string, at time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.fb.comments[n] {
		if c := &g.fb.comments[n][i]; c.ID == id {
			c.Body, c.UpdatedAt = body, at
			return nil
		}
	}
	return fmt.Errorf("comment %d: %w", id, providers.ErrNotFound)
}

// SetWriter sets whether the author with this ID may write to the repository; an unknown author may not.
func (g *Git) SetWriter(id int64, may bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fb.writers == nil {
		g.fb.writers = map[int64]bool{}
	}
	g.fb.writers[id] = may
}

// SetFeedbackClock sets the clock that starts a sweep; nil means time.Now.
func (g *Git) SetFeedbackClock(now func() time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fb.now = now
}

// SetFeedbackPerLook sets how many comments one look returns at most; 0 means DefaultFeedbackPerLook.
func (g *Git) SetFeedbackPerLook(n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fb.perLook = n
}

// FeedbackCalls returns how many looks at comments and how many permission questions to the host were made.
func (g *Git) FeedbackCalls() (lists, asks int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fb.lists, g.fb.asks
}

// ListComments returns the comments of change request n as one look of a sweep (see feedbackStore). A change
// request the fake did not open is refused, as the adapter refuses a pull request that is not Shadewright's.
func (g *Git) ListComments(ctx context.Context, n int64, cur providers.FeedbackCursor) (providers.FeedbackPage, error) {
	if err := g.before("ListComments"); err != nil {
		return providers.FeedbackPage{}, err
	}
	if cur.Page < 0 || cur.AfterID < 0 || (cur.ETag != "" && !providers.ValidETag(cur.ETag)) {
		return providers.FeedbackPage{}, errors.New("cursor is invalid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.get(n); err != nil {
		return providers.FeedbackPage{}, fmt.Errorf("not a change request Shadewright opened: %w", err)
	}
	g.fb.lists++
	now := time.Now
	if g.fb.now != nil {
		now = g.fb.now
	}
	start := cur.SweepStart
	if cur.Page == 0 {
		start = now()
	}
	var all []providers.Feedback
	for _, c := range g.fb.comments[n] {
		if !c.UpdatedAt.Before(cur.Since) && (cur.Page == 0 || c.ID > cur.AfterID) {
			all = append(all, c)
		}
	}
	slices.SortFunc(all, func(a, b providers.Feedback) int { return cmp.Compare(a.ID, b.ID) })
	limit := cmp.Or(g.fb.perLook, DefaultFeedbackPerLook)
	if len(all) > limit {
		items := slices.Clone(all[:limit])
		return providers.FeedbackPage{Items: items, Next: providers.FeedbackCursor{
			Since: cur.Since, Page: 1, AfterID: items[len(items)-1].ID, SweepStart: start}}, nil
	}
	out := providers.FeedbackPage{Items: all, Complete: true, Next: cur}
	if cur.Page != 0 || len(all) != 0 {
		out.Next = providers.FeedbackCursor{Since: cur.Since}
		if s := start.Add(-time.Second); s.After(cur.Since) {
			out.Next.Since = s
		}
	}
	return out, nil
}

// ListReviews returns no reviews: the fake keeps none.
func (g *Git) ListReviews(ctx context.Context, n int64, cur providers.FeedbackCursor) (providers.FeedbackPage, error) {
	if err := g.before("ListReviews"); err != nil {
		return providers.FeedbackPage{}, err
	}
	return providers.FeedbackPage{Complete: true, Next: cur}, nil
}

// CanWrite reports what SetWriter set for the author's ID. A Bot or App is refused without asking.
func (g *Git) CanWrite(ctx context.Context, a providers.FeedbackAuthor) (bool, error) {
	if a.Kind != providers.AuthorPerson {
		return false, nil
	}
	g.mu.Lock()
	g.fb.asks++
	g.mu.Unlock()
	if err := g.before("CanWrite"); err != nil {
		return false, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fb.writers[a.ID], nil
}
