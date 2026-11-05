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

package fake_test

import (
	"context"
	"errors"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

var errPaused = errors.New("paused")

func refusing(ctx context.Context) context.Context {
	return providers.WithWriteCheckpoint(ctx, func(context.Context) error { return errPaused }, errPaused)
}

// TestGitWritesHonourTheCheckpoint: every write of the fake asks the write checkpoint and changes nothing when
// it refuses.
func TestGitWritesHonourTheCheckpoint(t *testing.T) {
	ctx := context.Background()
	g, head := newRepo(t)
	_, cr := mustOpen(t, g, head)
	before := g.ChangeRequests()
	c := refusing(ctx)
	writes := map[string]error{
		"CreateBranch": g.CreateBranch(c, "shadewright/other", head),
		"OpenChangeRequest": func() error {
			_, err := g.OpenChangeRequest(c, providers.NewChangeRequest{HeadBranch: "shadewright/other", BaseBranch: base, Title: "t"})
			return err
		}(),
		"EnableAutoMerge":    g.EnableAutoMerge(c, cr.Number, cr.HeadSHA),
		"CloseChangeRequest": g.CloseChangeRequest(c, cr.Number),
	}
	tip, err := g.BranchHead(ctx, branch)
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.CommitFile(c, branch, tip, providers.FileChange{Path: path, Content: []byte(oldVal)})
	writes["CommitFile"] = err
	for op, err := range writes {
		if !errors.Is(err, errPaused) {
			t.Errorf("%s returned %v, want the checkpoint's refusal", op, err)
		}
	}
	if _, err := g.BranchHead(ctx, "shadewright/other"); !providers.IsNotFound(err) {
		t.Error("the refused CreateBranch created the branch")
	}
	if now, _ := g.BranchHead(ctx, branch); now != tip {
		t.Error("the refused CommitFile moved the branch")
	}
	after := g.ChangeRequests()
	if len(after) != len(before) || after[0].State != providers.ChangeRequestOpen || g.AutoMergeEnabled(cr.Number) {
		t.Errorf("a refused write changed the change requests: %+v", after)
	}
}

// TestCommitFileStopsBetweenItsWrites: a refusal after the commit object is written leaves the branch where it
// was, and the same commit made again later succeeds once.
func TestCommitFileStopsBetweenItsWrites(t *testing.T) {
	ctx := context.Background()
	g, head := newRepo(t)
	if err := g.CreateBranch(ctx, branch, head); err != nil {
		t.Fatal(err)
	}
	paused := false
	g.SetBetween(func(op string) {
		if op == "CommitFile" {
			paused = true
		}
	})
	c := providers.WithWriteCheckpoint(ctx, func(context.Context) error {
		if paused {
			return errPaused
		}
		return nil
	}, errPaused)
	change := providers.FileChange{Path: path, Content: []byte(newVal)}
	if _, err := g.CommitFile(c, branch, head, change); !errors.Is(err, errPaused) {
		t.Fatalf("CommitFile returned %v, want the refusal between its writes", err)
	}
	if now, _ := g.BranchHead(ctx, branch); now != head {
		t.Fatal("the branch moved although the update was refused")
	}
	g.SetBetween(nil)
	paused = false
	sha, err := g.CommitFile(c, branch, head, change)
	if err != nil {
		t.Fatal(err)
	}
	if now, _ := g.BranchHead(ctx, branch); now != sha {
		t.Fatal("the commit made again did not move the branch")
	}
}
