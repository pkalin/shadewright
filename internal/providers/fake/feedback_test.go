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
	"context"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
)

func TestFeedbackSweeps(t *testing.T) {
	ctx := context.Background()
	g := NewGit("main", map[string][]byte{"a.yaml": []byte("a: 1\n")})
	if err := g.CreateBranch(ctx, "shadewright/x", g.branches["main"]); err != nil {
		t.Fatal(err)
	}
	cr, err := g.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: "shadewright/x", BaseBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.ListComments(ctx, cr.Number+1, providers.FeedbackCursor{}); err == nil {
		t.Fatal("a change request the fake did not open was read")
	}
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	g.SetFeedbackClock(func() time.Time { return now })
	g.SetFeedbackPerLook(2)
	person := providers.FeedbackAuthor{ID: 1, Login: "p", Kind: providers.AuthorPerson}
	for id := range int64(3) {
		g.AddComment(cr.Number, providers.Feedback{ID: id + 1, Author: person, CreatedAt: now.Add(-time.Hour)})
	}

	first, err := g.ListComments(ctx, cr.Number, providers.FeedbackCursor{})
	if err != nil || first.Complete || len(first.Items) != 2 || len(first.Resume) != 2 {
		t.Fatalf("first look: %+v, %v", first, err)
	}
	resumed, err := g.ListComments(ctx, cr.Number, first.Resume[0])
	if err != nil || len(resumed.Items) != 2 || resumed.Items[0].ID != 2 {
		t.Fatalf("a look after the first item returns the items after it: %+v, %v", resumed, err)
	}
	second, err := g.ListComments(ctx, cr.Number, first.Next)
	if err != nil || !second.Complete || len(second.Items) != 1 || second.Items[0].ID != 3 {
		t.Fatalf("second look: %+v, %v", second, err)
	}
	if want := now.Add(-time.Second); !second.Next.Since.Equal(want) {
		t.Fatalf("since %v, want %v", second.Next.Since, want)
	}
	quiet, err := g.ListComments(ctx, cr.Number, second.Next)
	if err != nil || !quiet.Complete || len(quiet.Items) != 0 || quiet.Next != second.Next {
		t.Fatalf("a quiet sweep must keep the cursor: %+v, %v", quiet, err)
	}
	if err := g.EditComment(cr.Number, 2, "edited", now); err != nil {
		t.Fatal(err)
	}
	edited, err := g.ListComments(ctx, cr.Number, quiet.Next)
	if err != nil || len(edited.Items) != 1 || edited.Items[0].Body != "edited" {
		t.Fatalf("an edit is returned again: %+v, %v", edited, err)
	}
	if lists, _ := g.FeedbackCalls(); lists != 5 {
		t.Fatalf("lists %d, want 5", lists)
	}
}

func TestFeedbackCanWrite(t *testing.T) {
	ctx := context.Background()
	g := NewGit("main", nil)
	g.SetWriter(1, true)
	for _, tc := range []struct {
		a    providers.FeedbackAuthor
		want bool
	}{
		{providers.FeedbackAuthor{ID: 1, Login: "w", Kind: providers.AuthorPerson}, true},
		{providers.FeedbackAuthor{ID: 2, Login: "o", Kind: providers.AuthorPerson}, false},
		{providers.FeedbackAuthor{ID: 1, Login: "w", Kind: providers.AuthorBot}, false},
		{providers.FeedbackAuthor{ID: 1, Login: "w", Kind: providers.AuthorApp}, false},
	} {
		if got, err := g.CanWrite(ctx, tc.a); err != nil || got != tc.want {
			t.Errorf("CanWrite(%+v) = %v, %v; want %v", tc.a, got, err, tc.want)
		}
	}
	if _, asks := g.FeedbackCalls(); asks != 2 {
		t.Errorf("asks %d, want 2: a bot or App is never asked about", asks)
	}
}
