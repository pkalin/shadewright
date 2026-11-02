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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/pkalin/shadewright/internal/providers"
)

var _ providers.GitProvider = (*Git)(nil)

type commit struct {
	parent string
	files  map[string][]byte
}

// Git is an in-memory repository with change requests.
type Git struct {
	Hook

	mu       sync.Mutex
	caps     providers.GitCapabilities
	url      string
	commits  map[string]commit
	branches map[string]string
	crs      []*providers.ChangeRequest
	auto     map[int64]bool
}

// NewGit returns a repository whose branch base has one commit holding files.
func NewGit(base string, files map[string][]byte) *Git {
	g := &Git{
		caps:     providers.GitCapabilities{AutoMerge: true, RequiredReviews: true},
		url:      "https://git.example.test/owner/repo",
		commits:  map[string]commit{},
		branches: map[string]string{},
		auto:     map[int64]bool{},
	}
	sha := g.store("", maps.Clone(files))
	g.branches[base] = sha
	return g
}

// SetCapabilities overrides what the fake reports as supported.
func (g *Git) SetCapabilities(c providers.GitCapabilities) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.caps = c
}

func (g *Git) store(parent string, files map[string][]byte) string {
	h := sha256.New()
	// The sequence number keeps commits with the same parent and content distinct, as timestamps do on a
	// real host.
	_, _ = fmt.Fprintf(h, "%d\x00%s", len(g.commits), parent)
	for _, p := range slices.Sorted(maps.Keys(files)) {
		_, _ = fmt.Fprintf(h, "\x00%s\x00%d\x00", p, len(files[p]))
		h.Write(files[p])
	}
	sha := hex.EncodeToString(h.Sum(nil))[:40]
	g.commits[sha] = commit{parent: parent, files: files}
	return sha
}

// Capabilities reports what the fake supports.
func (g *Git) Capabilities() providers.GitCapabilities {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.caps
}

// ReadFile returns a file at a commit.
func (g *Git) ReadFile(ctx context.Context, sha, path string) (providers.File, error) {
	if err := g.before("ReadFile"); err != nil {
		return providers.File{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.commits[sha]
	if !ok {
		return providers.File{}, fmt.Errorf("commit %q: %w", sha, providers.ErrNotFound)
	}
	data, ok := c.files[path]
	if !ok {
		return providers.File{}, fmt.Errorf("file %q at %s: %w", path, sha, providers.ErrNotFound)
	}
	return providers.File{Path: path, Commit: sha, Data: slices.Clone(data)}, nil
}

// BranchHead returns the head of a branch.
func (g *Git) BranchHead(ctx context.Context, branch string) (string, error) {
	if err := g.before("BranchHead"); err != nil {
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	sha, ok := g.branches[branch]
	if !ok {
		return "", fmt.Errorf("branch %q: %w", branch, providers.ErrNotFound)
	}
	return sha, nil
}

// CreateBranch creates a branch, idempotently.
func (g *Git) CreateBranch(ctx context.Context, branch, from string) error {
	if err := g.before("CreateBranch"); err != nil {
		return err
	}
	g.mu.Lock()
	if _, ok := g.commits[from]; !ok {
		g.mu.Unlock()
		return fmt.Errorf("commit %q: %w", from, providers.ErrNotFound)
	}
	if cur, ok := g.branches[branch]; ok {
		g.mu.Unlock()
		if cur == from {
			return nil
		}
		return fmt.Errorf("branch %q exists at %s, not %s: %w", branch, cur, from, providers.ErrConflict)
	}
	g.branches[branch] = from
	g.mu.Unlock()
	return g.after("CreateBranch")
}

// CommitFile commits one file change on a branch whose head must be expectedHead.
func (g *Git) CommitFile(ctx context.Context, branch, expectedHead string, ch providers.FileChange) (string, error) {
	if err := g.before("CommitFile"); err != nil {
		return "", err
	}
	g.mu.Lock()
	cur, ok := g.branches[branch]
	if !ok {
		g.mu.Unlock()
		return "", fmt.Errorf("branch %q: %w", branch, providers.ErrNotFound)
	}
	if cur != expectedHead {
		g.mu.Unlock()
		return "", fmt.Errorf("branch %q is at %s, expected %s: %w", branch, cur, expectedHead, providers.ErrConflict)
	}
	files := g.commits[cur].files
	if _, ok := files[ch.Path]; !ok {
		g.mu.Unlock()
		return "", fmt.Errorf("file %q at %s: %w", ch.Path, cur, providers.ErrNotFound)
	}
	next := maps.Clone(files)
	next[ch.Path] = slices.Clone(ch.Content)
	sha := g.store(cur, next)
	g.branches[branch] = sha
	g.syncHeads(branch, sha)
	g.mu.Unlock()
	return sha, g.after("CommitFile")
}

// Push moves a branch as someone else would, by committing a file change outside Shadewright. It returns
// the new head. Tests use it to simulate a base branch that moved or a pushed review branch.
func (g *Git) Push(branch, path string, content []byte) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cur, ok := g.branches[branch]
	if !ok {
		return "", fmt.Errorf("branch %q: %w", branch, providers.ErrNotFound)
	}
	next := maps.Clone(g.commits[cur].files)
	next[path] = slices.Clone(content)
	sha := g.store(cur, next)
	g.branches[branch] = sha
	g.syncHeads(branch, sha)
	return sha, nil
}

func (g *Git) syncHeads(branch, sha string) {
	for _, cr := range g.crs {
		if cr.HeadBranch == branch && cr.State == providers.ChangeRequestOpen {
			cr.HeadSHA = sha
		}
	}
}

// OpenChangeRequest opens a change request, or returns the open one for the same head branch.
func (g *Git) OpenChangeRequest(ctx context.Context, req providers.NewChangeRequest) (providers.ChangeRequest, error) {
	if err := g.before("OpenChangeRequest"); err != nil {
		return providers.ChangeRequest{}, err
	}
	g.mu.Lock()
	for _, cr := range g.crs {
		if cr.HeadBranch == req.HeadBranch && cr.State == providers.ChangeRequestOpen {
			out := *cr
			g.mu.Unlock()
			return out, nil
		}
	}
	head, ok := g.branches[req.HeadBranch]
	if !ok {
		g.mu.Unlock()
		return providers.ChangeRequest{}, fmt.Errorf("branch %q: %w", req.HeadBranch, providers.ErrNotFound)
	}
	if _, ok := g.branches[req.BaseBranch]; !ok {
		g.mu.Unlock()
		return providers.ChangeRequest{}, fmt.Errorf("branch %q: %w", req.BaseBranch, providers.ErrNotFound)
	}
	n := int64(len(g.crs) + 1)
	cr := &providers.ChangeRequest{
		Number: n, URL: fmt.Sprintf("%s/pull/%d", g.url, n),
		HeadBranch: req.HeadBranch, BaseBranch: req.BaseBranch, HeadSHA: head,
		State: providers.ChangeRequestOpen, Title: req.Title, Body: req.Body,
	}
	g.crs = append(g.crs, cr)
	out := *cr
	g.mu.Unlock()
	return out, g.after("OpenChangeRequest")
}

// FindChangeRequest returns the most recent change request for a head branch.
func (g *Git) FindChangeRequest(ctx context.Context, headBranch string) (providers.ChangeRequest, error) {
	if err := g.before("FindChangeRequest"); err != nil {
		return providers.ChangeRequest{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, cr := range slices.Backward(g.crs) {
		if cr.HeadBranch == headBranch {
			return *cr, nil
		}
	}
	return providers.ChangeRequest{}, fmt.Errorf("change request for %q: %w", headBranch, providers.ErrNotFound)
}

func (g *Git) get(n int64) (*providers.ChangeRequest, error) {
	if n < 1 || n > int64(len(g.crs)) {
		return nil, fmt.Errorf("change request %d: %w", n, providers.ErrNotFound)
	}
	return g.crs[n-1], nil
}

// EnableAutoMerge records that auto-merge is on, if the head is still expectedHeadSHA.
func (g *Git) EnableAutoMerge(ctx context.Context, n int64, expectedHeadSHA string) error {
	if err := g.before("EnableAutoMerge"); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.caps.AutoMerge {
		return errors.New("auto-merge is not supported by this host")
	}
	cr, err := g.get(n)
	if err != nil {
		return err
	}
	if cr.State != providers.ChangeRequestOpen {
		return fmt.Errorf("change request %d is %s: %w", n, cr.State, providers.ErrConflict)
	}
	if cr.HeadSHA != expectedHeadSHA {
		return fmt.Errorf("change request %d head is %s, expected %s: %w", n, cr.HeadSHA, expectedHeadSHA, providers.ErrConflict)
	}
	g.auto[n] = true
	return nil
}

// AutoMergeEnabled reports whether auto-merge was enabled on a change request.
func (g *Git) AutoMergeEnabled(n int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.auto[n]
}

// CloseChangeRequest closes a change request, idempotently.
func (g *Git) CloseChangeRequest(ctx context.Context, n int64) error {
	if err := g.before("CloseChangeRequest"); err != nil {
		return err
	}
	g.mu.Lock()
	cr, err := g.get(n)
	if err != nil {
		g.mu.Unlock()
		return err
	}
	switch cr.State {
	case providers.ChangeRequestMerged:
		g.mu.Unlock()
		return fmt.Errorf("change request %d is merged: %w", n, providers.ErrConflict)
	case providers.ChangeRequestClosed:
		g.mu.Unlock()
		return nil
	}
	cr.State = providers.ChangeRequestClosed
	delete(g.auto, n)
	g.mu.Unlock()
	return g.after("CloseChangeRequest")
}

// Merge squash-merges an open change request into its base, as a reviewer would, and returns the merge
// commit. It fails with ErrConflict when the file changes cannot be applied to the current base.
func (g *Git) Merge(n int64) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cr, err := g.get(n)
	if err != nil {
		return "", err
	}
	if cr.State != providers.ChangeRequestOpen {
		return "", fmt.Errorf("change request %d is %s: %w", n, cr.State, providers.ErrConflict)
	}
	headFiles := g.commits[g.branches[cr.HeadBranch]].files
	baseSHA := g.branches[cr.BaseBranch]
	baseFiles := g.commits[baseSHA].files
	forkFiles := g.commits[g.mergeBase(baseSHA, g.branches[cr.HeadBranch])].files
	next := maps.Clone(baseFiles)
	for p, data := range headFiles {
		if string(forkFiles[p]) == string(data) {
			continue
		}
		if string(baseFiles[p]) != string(forkFiles[p]) {
			return "", fmt.Errorf("file %q changed on both sides: %w", p, providers.ErrConflict)
		}
		next[p] = data
	}
	sha := g.store(baseSHA, next)
	g.branches[cr.BaseBranch] = sha
	cr.State = providers.ChangeRequestMerged
	cr.MergeCommit = sha
	return sha, nil
}

func (g *Git) mergeBase(a, b string) string {
	seen := map[string]bool{}
	for c := a; c != ""; c = g.commits[c].parent {
		seen[c] = true
	}
	for c := b; c != ""; c = g.commits[c].parent {
		if seen[c] {
			return c
		}
	}
	return ""
}

// ChangeRequests returns a copy of every change request, oldest first.
func (g *Git) ChangeRequests() []providers.ChangeRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]providers.ChangeRequest, 0, len(g.crs))
	for _, cr := range g.crs {
		out = append(out, *cr)
	}
	return out
}
