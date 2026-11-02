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
	"sync"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
)

const (
	base   = "main"
	branch = "shadewright/0f8e2c1a"
	path   = "apps/demo/web.yaml"
)

func newRepo(t *testing.T) (*fake.Git, string) {
	t.Helper()
	g := fake.NewGit(base, map[string][]byte{path: []byte("memory: 128Mi\n")})
	head, err := g.BranchHead(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	return g, head
}

func mustOpen(t *testing.T, g *fake.Git, head string) (string, providers.ChangeRequest) {
	t.Helper()
	ctx := context.Background()
	if err := g.CreateBranch(ctx, branch, head); err != nil {
		t.Fatal(err)
	}
	c, err := g.CommitFile(ctx, branch, head, providers.FileChange{Path: path, Content: []byte("memory: 256Mi\n")})
	if err != nil {
		t.Fatal(err)
	}
	cr, err := g.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: branch, BaseBranch: base, Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return c, cr
}

func TestGitReadFile(t *testing.T) {
	g, head := newRepo(t)
	ctx := context.Background()
	f, err := g.ReadFile(ctx, head, path)
	if err != nil || string(f.Data) != "memory: 128Mi\n" || f.Commit != head {
		t.Fatalf("ReadFile = %+v, %v", f, err)
	}
	if _, err := g.ReadFile(ctx, head, "nope.yaml"); !providers.IsNotFound(err) {
		t.Errorf("missing file: %v", err)
	}
	if _, err := g.ReadFile(ctx, "deadbeef", path); !providers.IsNotFound(err) {
		t.Errorf("missing commit: %v", err)
	}
	if _, err := g.BranchHead(ctx, "nope"); !providers.IsNotFound(err) {
		t.Errorf("missing branch: %v", err)
	}
}

func TestGitCreateBranchIdempotent(t *testing.T) {
	g, head := newRepo(t)
	ctx := context.Background()
	if err := g.CreateBranch(ctx, branch, head); err != nil {
		t.Fatal(err)
	}
	if err := g.CreateBranch(ctx, branch, head); err != nil {
		t.Errorf("second create at the same commit: %v", err)
	}
	moved, _ := g.Push(base, path, []byte("memory: 192Mi\n"))
	if err := g.CreateBranch(ctx, branch, moved); !providers.IsConflict(err) {
		t.Errorf("create at another commit: %v", err)
	}
}

func TestGitCommitNeedsExpectedHead(t *testing.T) {
	g, head := newRepo(t)
	ctx := context.Background()
	if err := g.CreateBranch(ctx, branch, head); err != nil {
		t.Fatal(err)
	}
	ch := providers.FileChange{Path: path, Content: []byte("memory: 256Mi\n")}
	c1, err := g.CommitFile(ctx, branch, head, ch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.CommitFile(ctx, branch, head, ch); !providers.IsConflict(err) {
		t.Errorf("commit on a stale head: %v", err)
	}
	if _, err := g.CommitFile(ctx, branch, c1, providers.FileChange{Path: "new.yaml"}); !providers.IsNotFound(err) {
		t.Errorf("commit to a missing file: %v", err)
	}
	f, _ := g.ReadFile(ctx, c1, path)
	if string(f.Data) != "memory: 256Mi\n" {
		t.Errorf("content after commit = %q", f.Data)
	}
	old, _ := g.ReadFile(ctx, head, path)
	if string(old.Data) != "memory: 128Mi\n" {
		t.Errorf("old commit changed: %q", old.Data)
	}
}

func TestGitOpenChangeRequestIdempotent(t *testing.T) {
	g, head := newRepo(t)
	ctx := context.Background()
	c, first := mustOpen(t, g, head)
	if first.HeadSHA != c || first.State != providers.ChangeRequestOpen {
		t.Fatalf("first = %+v", first)
	}
	again, err := g.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: branch, BaseBranch: base, Title: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if again.Number != first.Number || again.Title != first.Title {
		t.Errorf("second open returned %+v, want the existing %+v", again, first)
	}
	if n := len(g.ChangeRequests()); n != 1 {
		t.Errorf("%d change requests, want 1", n)
	}
	found, err := g.FindChangeRequest(ctx, branch)
	if err != nil || found.Number != first.Number {
		t.Errorf("find = %+v, %v", found, err)
	}
	if _, err := g.FindChangeRequest(ctx, "other"); !providers.IsNotFound(err) {
		t.Errorf("find missing: %v", err)
	}
}

func TestGitConcurrentOpenMakesOne(t *testing.T) {
	g, head := newRepo(t)
	ctx := context.Background()
	if err := g.CreateBranch(ctx, branch, head); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_, _ = g.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: branch, BaseBranch: base})
		})
	}
	wg.Wait()
	if n := len(g.ChangeRequests()); n != 1 {
		t.Errorf("%d change requests, want 1", n)
	}
}

func TestGitCrashAfterOpenFindsExisting(t *testing.T) {
	g, head := newRepo(t)
	ctx := context.Background()
	if err := g.CreateBranch(ctx, branch, head); err != nil {
		t.Fatal(err)
	}
	lost := errors.New("connection reset")
	g.SetAfter(func(op string) error {
		if op == "OpenChangeRequest" {
			return lost
		}
		return nil
	})
	if _, err := g.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: branch, BaseBranch: base}); !errors.Is(err, lost) {
		t.Fatalf("want the injected error, got %v", err)
	}
	g.SetAfter(nil)
	cr, err := g.FindChangeRequest(ctx, branch)
	if err != nil || cr.State != providers.ChangeRequestOpen {
		t.Fatalf("change request made before the crash not found: %+v, %v", cr, err)
	}
}

func TestGitFailHook(t *testing.T) {
	g, head := newRepo(t)
	g.SetFail(func(op string) error {
		if op == "CreateBranch" {
			return &providers.RetryableError{Err: errors.New("503")}
		}
		return nil
	})
	if err := g.CreateBranch(context.Background(), branch, head); !providers.IsRetryable(err) {
		t.Errorf("want retryable, got %v", err)
	}
	if _, err := g.BranchHead(context.Background(), branch); !providers.IsNotFound(err) {
		t.Errorf("branch created despite the failure: %v", err)
	}
}

func TestGitHeadFollowsPushes(t *testing.T) {
	g, head := newRepo(t)
	ctx := context.Background()
	c, cr := mustOpen(t, g, head)
	pushed, _ := g.Push(branch, path, []byte("memory: 1Gi\n"))
	got, _ := g.FindChangeRequest(ctx, branch)
	if got.HeadSHA != pushed || got.HeadSHA == c {
		t.Errorf("head = %s, want the pushed %s", got.HeadSHA, pushed)
	}
	if err := g.EnableAutoMerge(ctx, cr.Number, c); !providers.IsConflict(err) {
		t.Errorf("auto-merge on a moved head: %v", err)
	}
	if err := g.EnableAutoMerge(ctx, cr.Number, pushed); err != nil || !g.AutoMergeEnabled(cr.Number) {
		t.Errorf("auto-merge on the current head: %v", err)
	}
}

func TestGitAutoMergeCapability(t *testing.T) {
	g, head := newRepo(t)
	_, cr := mustOpen(t, g, head)
	g.SetCapabilities(providers.GitCapabilities{})
	if g.Capabilities().AutoMerge {
		t.Fatal("capability not applied")
	}
	if err := g.EnableAutoMerge(context.Background(), cr.Number, cr.HeadSHA); err == nil {
		t.Error("auto-merge allowed without the capability")
	}
}

func TestGitCloseAndMerge(t *testing.T) {
	g, head := newRepo(t)
	ctx := context.Background()
	_, cr := mustOpen(t, g, head)
	if err := g.CloseChangeRequest(ctx, cr.Number); err != nil {
		t.Fatal(err)
	}
	if err := g.CloseChangeRequest(ctx, cr.Number); err != nil {
		t.Errorf("second close: %v", err)
	}
	if err := g.CloseChangeRequest(ctx, 99); !providers.IsNotFound(err) {
		t.Errorf("close missing: %v", err)
	}
	reopened, _ := g.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: branch, BaseBranch: base})
	if reopened.Number == cr.Number {
		t.Fatal("a closed change request was returned as open")
	}
	mc, err := g.Merge(reopened.Number)
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := g.BranchHead(ctx, base); h != mc {
		t.Errorf("base head %s, want merge commit %s", h, mc)
	}
	f, _ := g.ReadFile(ctx, mc, path)
	if string(f.Data) != "memory: 256Mi\n" {
		t.Errorf("merged content %q", f.Data)
	}
	if err := g.CloseChangeRequest(ctx, reopened.Number); !providers.IsConflict(err) {
		t.Errorf("close merged: %v", err)
	}
	if got, _ := g.FindChangeRequest(ctx, branch); got.State != providers.ChangeRequestMerged || got.MergeCommit != mc {
		t.Errorf("find after merge = %+v", got)
	}
}

func TestGitMergeConflict(t *testing.T) {
	g, head := newRepo(t)
	_, cr := mustOpen(t, g, head)
	if _, err := g.Push(base, path, []byte("memory: 512Mi\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Merge(cr.Number); !providers.IsConflict(err) {
		t.Errorf("merge with the file changed on both sides: %v", err)
	}
}
