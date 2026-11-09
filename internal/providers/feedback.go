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

package providers

import (
	"context"
	"time"
)

// Bounds of one look at the feedback on a change request (ADR-0028 section 3). Each list (comments, reviews)
// is one look: it reads at most MaxFeedbackPages pages and keeps at most MaxFeedbackBytes of bodies, and stops
// at whichever bound comes first, returning a cursor where the next look goes on.
const (
	// MaxFeedbackPages is the most pages one look reads. A look that does not reach the end starts the next
	// one on the last page it read, so each look moves on by at least MaxFeedbackPages-1 pages.
	MaxFeedbackPages = 5
	// MaxFeedbackBodyBytes is the most bytes of one body an item carries. A longer body is cut on a UTF-8
	// boundary and marked Truncated. It is well above a hint's bound (2 KiB of text after the command), so a
	// cut body never holds a hint the command parser could accept in full.
	MaxFeedbackBodyBytes = 8 << 10
	// MaxFeedbackBytes is the most bytes of bodies, after the cut above, one look returns. It holds at least
	// MaxFeedbackBytes/MaxFeedbackBodyBytes items, so every look that has items left returns some.
	MaxFeedbackBytes = 256 << 10
)

// AuthorKind says who wrote an item, as the host's API reported it, never as the text claims.
type AuthorKind string

// Author kinds. Only a Person can give a hint (ADR-0028 section 2); Bot and App are ignored.
const (
	// AuthorPerson is a user account acting by itself.
	AuthorPerson AuthorKind = "Person"
	// AuthorBot is a bot or App account, or any account type other than a user.
	AuthorBot AuthorKind = "Bot"
	// AuthorApp is a user account whose item an App posted on its behalf.
	AuthorApp AuthorKind = "App"
)

// FeedbackAuthor is the author of an item as the host's API reported it.
type FeedbackAuthor struct {
	// ID is the author's numeric account ID; it survives a rename of the login.
	ID int64
	// Login is the author's login when the item was read.
	Login string
	// Kind says whether a person, a bot or an App wrote the item.
	Kind AuthorKind
}

// FeedbackKind is the kind of an item.
type FeedbackKind string

// Feedback kinds.
const (
	// FeedbackComment is a comment in the change request's conversation (not an inline review comment).
	FeedbackComment FeedbackKind = "Comment"
	// FeedbackReview is the body of a submitted review.
	FeedbackReview FeedbackKind = "Review"
)

// Feedback is one comment or review body on a change request. Body is untrusted text typed by a person or
// quoted by them from anywhere (invariant 4): it is neither redacted nor parsed here.
type Feedback struct {
	Kind FeedbackKind
	// ID is the item's ID in the host, unique per kind.
	ID     int64
	Author FeedbackAuthor
	// Body is at most MaxFeedbackBodyBytes of the item's text, valid UTF-8.
	Body string
	// Truncated is true when Body was cut to MaxFeedbackBodyBytes.
	Truncated bool
	// CreatedAt is when the item was created (a review: when it was submitted).
	CreatedAt time.Time
	// UpdatedAt is when the item was last edited; for a review, which has no such time, CreatedAt.
	UpdatedAt time.Time
	// ReviewState is the state of a review (for example APPROVED), empty for a comment.
	ReviewState string
	// CommitID is the commit a review was submitted on, empty for a comment.
	CommitID string
}

// FeedbackCursor is where a look starts; the zero value starts from the beginning. The caller stores the Next
// cursor of a look durably once it has handled every item of that look, and passes it to the next look of the
// same list; its fields mean something only to the adapter that returned it. A cursor only moves forward:
// Since never decreases, and while items are left each look moves on by at least one.
type FeedbackCursor struct {
	// Since is the oldest update time the current sweep of the list asks for; zero asks for everything.
	Since time.Time
	// Page is the page the next look starts on; zero starts a new sweep on the first page.
	Page int
	// AfterID: the items of the current sweep with an ID up to this one were returned already.
	AfterID int64
	// SweepStart is the host's time when the current sweep began.
	SweepStart time.Time
}

// FeedbackPage is what one look returned.
type FeedbackPage struct {
	// Items are in the host's order, oldest first. An item edited later may be returned again by a later
	// sweep, so callers deduplicate by ID.
	Items []Feedback
	// Skipped counts items left out because the host returned them malformed (an unknown review state, an
	// impossible time, no valid author). They do not stop the look.
	Skipped int
	// Complete is true when the look reached the end of what the host had.
	Complete bool
	// Next is the cursor of the next look.
	Next FeedbackCursor
}

// FeedbackSource reads the feedback on a change request the executor opened, and asks the host whether an
// author may write to the repository (ADR-0028 sections 2 and 3). Implementations read only the repository
// they are bound to and the change requests Shadewright opened there, and refuse a response that names
// another repository or change request.
type FeedbackSource interface {
	// ListComments returns the conversation comments of change request number that are new, or edited since
	// the sweep cursor belongs to began, bounded as one look.
	ListComments(ctx context.Context, number int64, cursor FeedbackCursor) (FeedbackPage, error)
	// ListReviews returns the bodies of the submitted reviews of change request number after cursor, bounded
	// as one look. Reviews without a body are returned too: their state counts.
	ListReviews(ctx context.Context, number int64, cursor FeedbackCursor) (FeedbackPage, error)
	// CanWrite reports whether author may write to the repository, as the host says now. Only a Person can;
	// a Bot or App is refused without asking the host. An author the host does not know, or one whose ID
	// differs from the one the host reports for the login, is refused (false, nil).
	CanWrite(ctx context.Context, author FeedbackAuthor) (bool, error)
}
