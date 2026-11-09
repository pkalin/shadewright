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
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// Test values: an outcome of the analysis that declines a hint, and the fake's permission question.
const (
	outcomeNoProposal = "NoProposal"
	opCanWrite        = "CanWrite"
)

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
	got, evicted := mergeSeen(old, []remediationv1alpha1.SeenComment{seen(2, at(20)), seen(3, at(15))}, since)
	if len(evicted) != 0 {
		t.Fatalf("evicted %+v; want none within the bound", evicted)
	}
	if len(got) != 2 || got[0].CommentID != 2 || !got[0].UpdatedAt.Time.Equal(at(20)) || got[1].CommentID != 3 {
		t.Fatalf("got %+v; want 2 at its newest time and 3, and not 1 (before since)", got)
	}
	many := make([]remediationv1alpha1.SeenComment, 0, remediationv1alpha1.MaxSeenComments+5)
	for i := range remediationv1alpha1.MaxSeenComments + 5 {
		many = append(many, seen(int64(i+1), at(11+i%40)))
	}
	got, evicted = mergeSeen(nil, many, since)
	if len(got) != remediationv1alpha1.MaxSeenComments || len(evicted) != 5 {
		t.Fatalf("%d seen, %d evicted; want the bound %d and 5", len(got), len(evicted),
			remediationv1alpha1.MaxSeenComments)
	}
	oldest := got[0].UpdatedAt
	for _, k := range got {
		if k.UpdatedAt.Before(&oldest) {
			oldest = k.UpdatedAt
		}
	}
	for _, e := range evicted {
		if e.UpdatedAt.After(oldest.Time) {
			t.Fatalf("evicted %+v is newer than a kept one (%v)", e, oldest)
		}
	}
}

func seen(id int64, at time.Time) remediationv1alpha1.SeenComment {
	return remediationv1alpha1.SeenComment{CommentID: id, UpdatedAt: metav1.NewTime(at)}
}

// answerFixture is a request with one Pending hint, accepted at at, and a reader that holds its Incident with
// the Analyzed outcome given (none when outcome is empty).
func answerFixture(t *testing.T, at time.Time, outcome string) (*RemediationRequestReconciler,
	*remediationv1alpha1.RemediationRequest) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	const ns, uid = "shadewright-system", "33333333-3333-4333-8333-333333333333"
	h := remediationv1alpha1.Hint{Source: remediationv1alpha1.HintSourceGitHub, CommentID: 7,
		Digest: "sha256:" + strings.Repeat("a", 64), State: remediationv1alpha1.HintPending,
		AcceptedAt: metav1.NewTime(at)}
	inc := &remediationv1alpha1.Incident{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "inc", UID: uid}}
	if outcome != "" {
		inc.Status.Phase = remediationv1alpha1.IncidentProposed
		inc.Status.HandledHint = &remediationv1alpha1.HintRef{Source: h.Source, CommentID: h.CommentID, Digest: h.Digest}
		meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{
			Type: conditionIncidentAnalyzed, Status: metav1.ConditionTrue, Reason: outcome})
	}
	rr := &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "rr"}}
	rr.Spec.IncidentRef = remediationv1alpha1.IncidentRef{Name: "inc", UID: uid}
	rr.Status.Hints = []remediationv1alpha1.Hint{h}
	reader := clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(inc).Build()
	return &RemediationRequestReconciler{APIReader: reader}, rr
}

// An outcome the API does not list is declined as NoAnswer, so the reason enum never refuses the status write;
// a listed one with its own reason, and a replacement's outcome is stored on the hint.
func TestAnswerHintsMapsTheIncidentsOutcome(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	for outcome, want := range map[string]remediationv1alpha1.Hint{
		"SomethingNew":    {State: remediationv1alpha1.HintDeclined, Reason: ReasonHintNoAnswer},
		outcomeNoProposal: {State: remediationv1alpha1.HintDeclined, Reason: outcomeNoProposal},
		outcomeProposed:   {State: remediationv1alpha1.HintPending, Outcome: outcomeProposed},
		"RunbookMatched":  {State: remediationv1alpha1.HintPending, Outcome: outcomeRunbookMatched},
	} {
		r, rr := answerFixture(t, now, outcome)
		r.answerHints(context.Background(), rr, now)
		if h := rr.Status.Hints[0]; h.State != want.State || h.Reason != want.Reason || h.Outcome != want.Outcome {
			t.Errorf("outcome %s: got %s %q %q; want %s %q %q", outcome, h.State, h.Reason, h.Outcome,
				want.State, want.Reason, want.Outcome)
		}
	}
}

// A command refused at the cap and pushed out of the seen list by more than MaxSeenComments others is still
// judged when the next sweep returns it unchanged: a slot freed later does not turn it into a hint.
func TestAnEvictedRefusedCommentIsNotAcceptedLater(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	g, number := overlapRepository(t, &now)
	rr := &remediationv1alpha1.RemediationRequest{}
	// Two consumed hints and one reservation fill the cap; declining the reservation is permitted.
	for id := int64(101); id <= 103; id++ {
		state := remediationv1alpha1.HintConsumed
		if id == 103 {
			state = remediationv1alpha1.HintReserved
		}
		rr.Status.Hints = append(rr.Status.Hints, remediationv1alpha1.Hint{
			Source: remediationv1alpha1.HintSourceGitHub, CommentID: id,
			State: state,
		})
	}
	for id := int64(1); id <= remediationv1alpha1.MaxSeenComments+1; id++ {
		g.AddComment(number, providers.Feedback{ID: id, Author: overlapWriter,
			Body: hintCommand + " check the sidecar", CreatedAt: now})
	}
	overlapSweep(t, g, number, rr, now)
	if len(rr.Status.Hints) != 3 {
		t.Fatal("first sweep should refuse every command at the cap")
	}
	rr.Status.Hints[2].State = remediationv1alpha1.HintDeclined
	rr.Status.Hints[2].Reason = outcomeNoProposal
	now = now.Add(time.Minute)
	overlapSweep(t, g, number, rr, now)
	if h := findHint(rr.Status.Hints, remediationv1alpha1.HintSourceGitHub, 1); h != nil {
		t.Fatalf("unchanged comment refused in the first sweep became a new %s hint after seen overflow", h.State)
	}
}

// An evicted comment edited after its judgement is judged again with its new text.
func TestAnEvictedCommentEditedLaterIsJudgedAgain(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	g, number := overlapRepository(t, &now)
	rr := &remediationv1alpha1.RemediationRequest{}
	for id := int64(1); id <= remediationv1alpha1.MaxSeenComments+1; id++ {
		g.AddComment(number, providers.Feedback{ID: id, Author: overlapWriter, Body: hintCommand, CreatedAt: now})
	}
	overlapSweep(t, g, number, rr, now)
	if c := rr.Status.CommentCursor; c == nil || c.EvictedAt == nil || c.EvictedID != 1 {
		t.Fatalf("cursor %+v; want the evicted comment 1 in its watermark", c)
	}
	_, before := g.FeedbackCalls()
	now = now.Add(time.Minute)
	if err := g.EditComment(number, 1, hintCommand+" check the sidecar", now); err != nil {
		t.Fatal(err)
	}
	overlapSweep(t, g, number, rr, now)
	if _, after := g.FeedbackCalls(); after != before+1 {
		t.Fatalf("permission questions %d -> %d; want the edited comment judged again", before, after)
	}
	if findHint(rr.Status.Hints, remediationv1alpha1.HintSourceGitHub, 1) == nil {
		t.Fatalf("the edited comment was not judged again: hints %+v", rr.Status.Hints)
	}
}

// infoLog returns a context whose logger keeps the Info lines (verbosity 0 only) in the returned builder.
func infoLog() (context.Context, *strings.Builder) {
	var out strings.Builder
	var mu sync.Mutex
	return logr.NewContext(context.Background(), funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		out.WriteString(args + "\n")
	}, funcr.Options{})), &out
}

// A failed read of the Incident and an outcome the API does not list are logged at Info, with the cause.
func TestAnswerHintsLogsWhatItCannotAnswer(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	r, rr := answerFixture(t, now, "SomethingNew")
	ctx, out := infoLog()
	r.answerHints(ctx, rr, now)
	if !strings.Contains(out.String(), `"outcome"="SomethingNew"`) {
		t.Errorf("the unlisted outcome was not logged at Info: %q", out.String())
	}

	r, rr = answerFixture(t, now, "")
	r.APIReader = clientfake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("etcd is down")
		},
	}).Build()
	ctx, out = infoLog()
	r.answerHints(ctx, rr, now)
	if !strings.Contains(out.String(), "etcd is down") {
		t.Errorf("the failed Incident read was not logged at Info: %q", out.String())
	}
	if rr.Status.Hints[0].State != remediationv1alpha1.HintPending {
		t.Errorf("a failed read answered the hint: %+v", rr.Status.Hints[0])
	}
}
