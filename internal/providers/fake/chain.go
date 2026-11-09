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
	"fmt"
	"maps"
	"slices"

	"github.com/pkalin/shadewright/internal/providers"
)

// The supersede handoff of ADR-0028 section 4: a branch that carries a chain of commits, the commit appended
// to it, the rewrite of the change request and the check run on its new head.

// CheckChain reports what a commit carries over the target branch when the branch may carry up to commits
// commits of its own (providers.GitProvider.CheckChain).
func (g *Git) CheckChain(ctx context.Context, sha, path string, commits int) (providers.BranchState, error) {
	if commits < 1 || commits > providers.MaxChainCommits {
		return 0, fmt.Errorf("a chain of %d commits is out of range", commits)
	}
	if err := g.before("CheckChain"); err != nil {
		return 0, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.commits[sha]; !ok {
		return 0, fmt.Errorf("commit %q: %w", sha, providers.ErrNotFound)
	}
	if g.onTarget(sha) {
		return providers.BranchAtTarget, nil
	}
	at := sha
	for range commits {
		c := g.commits[at]
		if c.parent == "" || c.merged != "" || !g.onlyFile(c, c.parent, path) {
			return providers.BranchForeign, nil
		}
		if g.onTarget(c.parent) {
			return providers.BranchOneChange, nil
		}
		at = c.parent
	}
	return providers.BranchForeign, nil
}

// Parents returns the parents of a commit: none for the first commit, two for a merge commit.
func (g *Git) Parents(ctx context.Context, sha string) ([]string, error) {
	if err := g.before("Parents"); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.commits[sha]
	if !ok {
		return nil, fmt.Errorf("commit %q: %w", sha, providers.ErrNotFound)
	}
	var out []string
	if c.parent != "" {
		out = append(out, c.parent)
	}
	if c.merged != "" {
		out = append(out, c.merged)
	}
	return out, nil
}

// AppendCommit commits one file change on a branch whose head must be expectedHead, a commit that is not on
// the target branch. Like CommitFile it makes two writes, each after the write checkpoint of ctx, and calls
// Between between them.
func (g *Git) AppendCommit(ctx context.Context, branch, expectedHead string, ch providers.FileChange) (string, error) {
	if err := g.before("AppendCommit"); err != nil {
		return "", err
	}
	if err := providers.CheckWrite(ctx); err != nil {
		return "", err
	}
	g.mu.Lock()
	cur, ok := g.branches[branch]
	switch {
	case !ok:
		g.mu.Unlock()
		return "", fmt.Errorf("branch %q: %w", branch, providers.ErrNotFound)
	case cur != expectedHead:
		g.mu.Unlock()
		return "", fmt.Errorf("branch %q is at %s, expected %s: %w", branch, cur, expectedHead, providers.ErrConflict)
	case g.onTarget(cur):
		g.mu.Unlock()
		return "", fmt.Errorf("commit %s is on the target branch", cur)
	}
	files := g.commits[cur].files
	if _, ok := files[ch.Path]; !ok {
		g.mu.Unlock()
		return "", fmt.Errorf("file %q at %s: %w", ch.Path, cur, providers.ErrPathNotInTree)
	}
	next := maps.Clone(files)
	next[ch.Path] = slices.Clone(ch.Content)
	sha := g.store(cur, next)
	g.mu.Unlock()

	g.between("AppendCommit")
	if err := providers.CheckWrite(ctx); err != nil {
		return "", err
	}
	g.mu.Lock()
	if now := g.branches[branch]; now != expectedHead {
		g.mu.Unlock()
		return "", fmt.Errorf("branch %q is at %s, expected %s: %w", branch, now, expectedHead, providers.ErrConflict)
	}
	g.branches[branch] = sha
	g.syncHeads(branch, sha)
	g.mu.Unlock()
	return sha, g.after("AppendCommit")
}

// UpdateChangeRequest replaces the title and body of an open change request.
func (g *Git) UpdateChangeRequest(ctx context.Context, n int64, title, body string) error {
	if err := g.before("UpdateChangeRequest"); err != nil {
		return err
	}
	if err := providers.CheckWrite(ctx); err != nil {
		return err
	}
	g.mu.Lock()
	cr, err := g.get(n)
	if err != nil {
		g.mu.Unlock()
		return err
	}
	if cr.State != providers.ChangeRequestOpen {
		g.mu.Unlock()
		return fmt.Errorf("change request %d is %s: %w", n, cr.State, providers.ErrConflict)
	}
	cr.Title, cr.Body = title, body
	g.mu.Unlock()
	return g.after("UpdateChangeRequest")
}

// CreateCheckRun records a check run on a commit.
func (g *Git) CreateCheckRun(ctx context.Context, head string, run providers.CheckRun) error {
	if err := g.before("CreateCheckRun"); err != nil {
		return err
	}
	if !providers.IsObjectID(head) {
		return fmt.Errorf("commit is not a full object ID: %w", providers.ErrInvalidCommit)
	}
	if err := providers.CheckWrite(ctx); err != nil {
		return err
	}
	g.mu.Lock()
	if _, ok := g.commits[head]; !ok {
		g.mu.Unlock()
		return fmt.Errorf("commit %q: %w", head, providers.ErrNotFound)
	}
	if g.checkRuns == nil {
		g.checkRuns = map[string][]providers.CheckRun{}
	}
	g.checkRuns[head] = append(g.checkRuns[head], run)
	g.mu.Unlock()
	return g.after("CreateCheckRun")
}

// CheckRuns returns a copy of the check runs created on a commit, oldest first.
func (g *Git) CheckRuns(head string) []providers.CheckRun {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.checkRuns[head])
}
