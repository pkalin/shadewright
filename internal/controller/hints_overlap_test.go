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
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// overlapSweep is one whole look with a new reconciler and the status cursor, as after a restart.
func overlapSweep(t *testing.T, g *fakegit.Git, number int64, rr *remediationv1alpha1.RemediationRequest, now time.Time) {
	t.Helper()
	cur := cursorOf(rr.Status.CommentCursor)
	page, err := g.ListComments(context.Background(), number, cur)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Complete {
		t.Fatal("the look did not reach the end")
	}
	if err := (&RemediationRequestReconciler{}).lookComments(context.Background(), g, rr,
		remediationv1alpha1.HintSourceGitHub, cur, page, now); err != nil {
		t.Fatal(err)
	}
}

func overlapRepository(t *testing.T, now *time.Time) (*fakegit.Git, int64) {
	t.Helper()
	g := fakegit.NewGit(mainBranch, nil)
	head, err := g.BranchHead(context.Background(), mainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.CreateBranch(context.Background(), "shadewright/overlap", head); err != nil {
		t.Fatal(err)
	}
	cr, err := g.OpenChangeRequest(context.Background(), providers.NewChangeRequest{
		HeadBranch: "shadewright/overlap", BaseBranch: mainBranch,
	})
	if err != nil {
		t.Fatal(err)
	}
	g.SetFeedbackClock(func() time.Time { return *now })
	g.SetWriter(1, true)
	return g, cr.Number
}

var overlapWriter = providers.FeedbackAuthor{ID: 1, Login: "writer", Kind: providers.AuthorPerson}

// More refusals than the ring keeps are not judged again when the next sweep returns them unchanged.
func TestRefusedCommentsAreNotJudgedAgainOnTheOverlap(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	g, number := overlapRepository(t, &now)
	for id := int64(1); id <= remediationv1alpha1.MaxRefusedHints+1; id++ {
		g.AddComment(number, providers.Feedback{ID: id, Author: overlapWriter, Body: hintCommand, CreatedAt: now})
	}
	rr := &remediationv1alpha1.RemediationRequest{}
	overlapSweep(t, g, number, rr, now)
	_, before := g.FeedbackCalls()
	refusals := rr.Status.RefusedHints
	now = now.Add(time.Minute)
	overlapSweep(t, g, number, rr, now)
	if _, after := g.FeedbackCalls(); after != before {
		t.Fatalf("unchanged refused comments were judged again: permission questions %d -> %d", before, after)
	}
	if len(rr.Status.RefusedHints) != len(refusals) || rr.Status.RefusedHints[0] != refusals[0] {
		t.Fatalf("the record changed: %+v -> %+v", refusals, rr.Status.RefusedHints)
	}
}

// A comment read as plain text, created at the cursor's since, then edited into the command after a restart,
// is refused as EditedAfterSeen, not stored as a new hint.
func TestASeenPlainCommentAtSinceCannotBecomeAHint(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	g, number := overlapRepository(t, &now)
	g.AddComment(number, providers.Feedback{ID: 1, Author: overlapWriter, Body: "looks fine",
		CreatedAt: now.Add(-time.Second)})
	rr := &remediationv1alpha1.RemediationRequest{}
	overlapSweep(t, g, number, rr, now)
	if since := rr.Status.CommentCursor.Since; since == nil || !since.Time.Equal(now.Add(-time.Second)) {
		t.Fatalf("since %v; the comment must sit exactly on it", since)
	}
	now = now.Add(time.Minute)
	if err := g.EditComment(number, 1, hintCommand+" check the sidecar", now); err != nil {
		t.Fatal(err)
	}
	overlapSweep(t, g, number, rr, now)
	if len(rr.Status.Hints) != 0 || !findRefused(rr.Status.RefusedHints, remediationv1alpha1.HintSourceGitHub, 1) {
		t.Fatalf("a comment read as plain text was accepted after an edit: hints %+v, refusals %+v",
			rr.Status.Hints, rr.Status.RefusedHints)
	}
}

// mergeSeen keeps what the next sweep may return again unchanged, newest first within its bound.
func TestMergeSeen(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 11, 9, 12, 0, s, 0, time.UTC) }
	since := at(10)
	old := []remediationv1alpha1.SeenComment{seen(1, at(5)), seen(2, at(12))}
	got := mergeSeen(old, []remediationv1alpha1.SeenComment{seen(2, at(20)), seen(3, at(15))}, since)
	if len(got) != 2 || got[0].CommentID != 2 || !got[0].UpdatedAt.Time.Equal(at(20)) || got[1].CommentID != 3 {
		t.Fatalf("got %+v; want 2 at its newest time and 3, and not 1 (before since)", got)
	}
	many := make([]remediationv1alpha1.SeenComment, 0, remediationv1alpha1.MaxSeenComments+5)
	for i := range remediationv1alpha1.MaxSeenComments + 5 {
		many = append(many, seen(int64(i+1), at(11+i%40)))
	}
	if got := mergeSeen(nil, many, since); len(got) != remediationv1alpha1.MaxSeenComments {
		t.Fatalf("%d seen; want the bound %d", len(got), remediationv1alpha1.MaxSeenComments)
	}
}

func seen(id int64, at time.Time) remediationv1alpha1.SeenComment {
	return remediationv1alpha1.SeenComment{CommentID: id, UpdatedAt: metav1.NewTime(at)}
}

// An outcome the API does not list is declined as NoAnswer, so the reason enum never refuses the status write.
func TestIncidentAnswerMapsAnUnlistedOutcomeToNoAnswer(t *testing.T) {
	h := &remediationv1alpha1.Hint{Source: remediationv1alpha1.HintSourceGitHub, CommentID: 7,
		Digest: "sha256:" + strings.Repeat("a", 64)}
	for outcome, want := range map[string]string{"SomethingNew": ReasonHintNoAnswer, "NoProposal": "NoProposal"} {
		inc := &remediationv1alpha1.Incident{}
		inc.Status.HandledHint = &remediationv1alpha1.HintRef{Source: h.Source, CommentID: h.CommentID, Digest: h.Digest}
		meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{
			Type: conditionIncidentAnalyzed, Status: metav1.ConditionTrue, Reason: outcome})
		if got, ok := incidentAnswer(inc, h); !ok || got != want {
			t.Errorf("outcome %s: got %q, %v; want %q", outcome, got, ok, want)
		}
	}
}
