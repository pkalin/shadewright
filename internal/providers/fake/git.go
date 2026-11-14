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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/pkalin/shadewright/internal/providers"
)

var _ providers.GitProvider = (*Git)(nil)

type commit struct {
	parent string
	// merged is the second parent of a merge commit (MergeWithCommit), empty otherwise.
	merged string
	files  map[string][]byte
}

// Git is an in-memory repository with change requests.
type Git struct {
	Hook

	mu       sync.Mutex
	caps     providers.GitCapabilities
	url      string
	target   string
	commits  map[string]commit
	branches map[string]string
	crs      []*providers.ChangeRequest
	auto     map[int64]bool
	comments map[int64][]string
	// checkRuns are the check runs created per commit, guarded by mu.
	checkRuns map[string][]providers.CheckRun
	// fb is the feedback of the change requests (feedback.go), guarded by mu.
	fb feedbackStore
}

// NewGit returns a repository whose branch base, its target branch, has one commit holding files.
func NewGit(base string, files map[string][]byte) *Git {
	g := &Git{
		caps:     providers.GitCapabilities{AutoMerge: true, RequiredReviews: true},
		url:      "https://git.example.test/owner/repo",
		target:   base,
		commits:  map[string]commit{},
		branches: map[string]string{},
		auto:     map[int64]bool{},
		comments: map[int64][]string{},
	}
	own := make(map[string][]byte, len(files))
	for p, data := range files {
		own[p] = slices.Clone(data)
	}
	sha := g.store("", own)
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

// ReadFile returns a file at a commit, or ErrTooLarge when it is larger than limit (clamped by
// providers.ReadLimit).
func (g *Git) ReadFile(ctx context.Context, sha, path string, limit int) (providers.File, error) {
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
		return providers.File{}, fmt.Errorf("file %q at %s: %w", path, sha, providers.ErrPathNotInTree)
	}
	if len(data) > providers.ReadLimit(limit) {
		return providers.File{}, fmt.Errorf("file %q at %s: %w", path, sha, providers.ErrTooLarge)
	}
	return providers.File{Path: path, Commit: sha, Data: slices.Clone(data)}, nil
}

// ListDir returns the files and directories directly in dir at a commit, sorted by name. The fake holds only
// regular files, so a directory exists when some file is below it. ErrTooLarge when it holds more than
// providers.MaxDirEntries entries.
func (g *Git) ListDir(ctx context.Context, sha, dir string) ([]providers.DirEntry, error) {
	if err := g.before("ListDir"); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.commits[sha]
	if !ok {
		return nil, fmt.Errorf("commit %q: %w", sha, providers.ErrNotFound)
	}
	prefix := ""
	if dir != "." {
		prefix = dir + "/"
	}
	types := map[string]providers.DirEntryType{}
	for p := range c.files {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok {
			continue
		}
		if name, _, nested := strings.Cut(rest, "/"); nested {
			types[name] = providers.DirEntryDir
		} else {
			types[name] = providers.DirEntryFile
		}
	}
	if len(types) == 0 && dir != "." {
		return nil, fmt.Errorf("directory %q at %s: %w", dir, sha, providers.ErrPathNotInTree)
	}
	if len(types) > providers.MaxDirEntries {
		return nil, fmt.Errorf("directory %q at %s: %w", dir, sha, providers.ErrTooLarge)
	}
	out := make([]providers.DirEntry, 0, len(types))
	for _, name := range slices.Sorted(maps.Keys(types)) {
		out = append(out, providers.DirEntry{Name: name, Type: types[name]})
	}
	return out, nil
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
	if err := providers.CheckWrite(ctx); err != nil {
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

// CommitFile commits one file change on a branch whose head must be expectedHead. Like a host's API it makes
// two writes, each after the write checkpoint of ctx: the commit object, then the branch update. A refusal
// between them leaves the commit unreferenced and the branch where it was; Between is called there.
func (g *Git) CommitFile(ctx context.Context, branch, expectedHead string, ch providers.FileChange) (string, error) {
	if err := g.before("CommitFile"); err != nil {
		return "", err
	}
	if err := providers.CheckWrite(ctx); err != nil {
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
	if !g.onTarget(cur) {
		g.mu.Unlock()
		return "", fmt.Errorf("commit %s is not on the target branch", cur)
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

	g.between("CommitFile")
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
	return sha, g.after("CommitFile")
}

// onTarget reports whether sha is the head of the target branch or an ancestor of it. g.mu must be held.
func (g *Git) onTarget(sha string) bool { return g.ancestor(g.branches[g.target], sha) }

// ancestor reports whether sha is of or an ancestor of it, through both parents of a merge commit. g.mu must
// be held.
func (g *Git) ancestor(of, sha string) bool {
	todo, seen := []string{of}, map[string]bool{}
	for len(todo) > 0 {
		at := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if at == "" || seen[at] {
			continue
		}
		if at == sha {
			return true
		}
		seen[at] = true
		todo = append(todo, g.commits[at].parent, g.commits[at].merged)
	}
	return false
}

// onlyFile reports whether commit c changes at most the file at path against the commit parent, which holds
// that file too. g.mu must be held.
func (g *Git) onlyFile(c commit, parent, path string) bool {
	old := g.commits[parent].files
	if _, ok := c.files[path]; !ok || len(old) != len(c.files) {
		return false
	}
	for p, data := range c.files {
		o, ok := old[p]
		if !ok || (p != path && !bytes.Equal(o, data)) {
			return false
		}
	}
	return true
}

// CheckCommit reports what commit carries over its own parent (providers.GitProvider.CheckCommit).
func (g *Git) CheckCommit(ctx context.Context, sha, path string) (providers.BranchState, string, error) {
	if err := g.before("CheckCommit"); err != nil {
		return 0, "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.commits[sha]
	if !ok {
		return 0, "", fmt.Errorf("commit %q: %w", sha, providers.ErrNotFound)
	}
	if c.parent == "" || c.merged != "" || !g.onlyFile(c, c.parent, path) {
		return providers.BranchForeign, "", nil
	}
	return providers.BranchOneChange, c.parent, nil
}

// CheckMerged reports what head carried over the target branch before mergeCommit merged it
// (providers.GitProvider.CheckMerged).
func (g *Git) CheckMerged(ctx context.Context, head, mergeCommit, path string) (providers.BranchState, string, error) {
	if err := g.before("CheckMerged"); err != nil {
		return 0, "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.commits[head]
	m, okM := g.commits[mergeCommit]
	if !ok || !okM {
		return 0, "", fmt.Errorf("commit: %w", providers.ErrNotFound)
	}
	if mergeCommit == head || !g.onTarget(mergeCommit) || c.merged != "" || c.parent == "" ||
		!g.ancestor(m.parent, c.parent) || !g.onlyFile(c, c.parent, path) {
		return providers.BranchForeign, "", nil
	}
	return providers.BranchOneChange, c.parent, nil
}

// MaxComments is how many comments of a change request CommentChangeRequest reads to look for its marker, as
// many as the GitHub adapter reads (50 pages of 20); past them it posts nothing (ErrTooManyComments).
const MaxComments = 1000

// CommentChangeRequest adds a comment to an open change request, unless a comment it posted (the App's own,
// kept apart from the people's comments of AddComment) holds marker as a whole line already, as the
// interface says. A marker in a person's comment does not count. When the change request holds more than
// MaxComments comments, the App's and the people's together, it posts nothing and returns ErrTooManyComments.
func (g *Git) CommentChangeRequest(ctx context.Context, n int64, marker, body string) error {
	if err := g.before("CommentChangeRequest"); err != nil {
		return err
	}
	if strings.TrimSpace(marker) == "" || strings.ContainsAny(marker, "\r\n") ||
		len(marker) > providers.MaxCommentMarkerBytes || !markerLine(body, marker) {
		return errors.New("comment marker is invalid or not a line of the body")
	}
	g.mu.Lock()
	if len(g.comments[n])+len(g.fb.comments[n]) > MaxComments {
		g.mu.Unlock()
		return fmt.Errorf("change request %d: %w", n, providers.ErrTooManyComments)
	}
	for _, c := range g.comments[n] {
		if markerLine(c, marker) {
			g.mu.Unlock()
			return nil
		}
	}
	g.mu.Unlock()
	if err := providers.CheckWrite(ctx); err != nil {
		return err
	}
	g.mu.Lock()
	cr, err := g.get(n)
	if err == nil && cr.State != providers.ChangeRequestOpen {
		err = fmt.Errorf("change request %d is %s: %w", n, cr.State, providers.ErrConflict)
	}
	if err == nil {
		g.comments[n] = append(g.comments[n], body)
	}
	g.mu.Unlock()
	if err != nil {
		return err
	}
	return g.after("CommentChangeRequest")
}

// markerLine reports whether a line of body, without surrounding white space, is marker.
func markerLine(body, marker string) bool {
	marker = strings.TrimSpace(marker)
	for line := range strings.SplitSeq(body, "\n") {
		if strings.TrimSpace(line) == marker {
			return true
		}
	}
	return false
}

// Comments returns the comments CommentChangeRequest posted on change request n (the App's own), oldest first;
// the people's comments of AddComment are not among them.
func (g *Git) Comments(n int64) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.comments[n])
}

// ForcePush points branch at sha as a force push by someone else would, and moves the head of its open
// change requests. Tests use it to replace a branch's commits.
func (g *Git) ForcePush(branch, sha string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.commits[sha]; !ok {
		return fmt.Errorf("commit %q: %w", sha, providers.ErrNotFound)
	}
	g.branches[branch] = sha
	g.syncHeads(branch, sha)
	return nil
}

// CheckBranch reports what a commit carries over the target branch (providers.GitProvider.CheckBranch).
func (g *Git) CheckBranch(ctx context.Context, sha, path string) (providers.BranchState, error) {
	if err := g.before("CheckBranch"); err != nil {
		return 0, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.commits[sha]
	if !ok {
		return 0, fmt.Errorf("commit %q: %w", sha, providers.ErrNotFound)
	}
	if g.onTarget(sha) {
		return providers.BranchAtTarget, nil
	}
	if c.parent == "" || c.merged != "" || !g.onTarget(c.parent) || !g.onlyFile(c, c.parent, path) {
		return providers.BranchForeign, nil
	}
	return providers.BranchOneChange, nil
}

// Push moves a branch as someone else would, by committing a file change outside Shadewright. It returns
// the new head. Tests use it to simulate a base branch that moved or a pushed review branch.
func (g *Git) Push(branch, path string, content []byte) (string, error) {
	return g.PushFiles(branch, map[string][]byte{path: content})
}

// PushFiles is Push with one commit that changes or adds every file in files.
func (g *Git) PushFiles(branch string, files map[string][]byte) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cur, ok := g.branches[branch]
	if !ok {
		return "", fmt.Errorf("branch %q: %w", branch, providers.ErrNotFound)
	}
	next := maps.Clone(g.commits[cur].files)
	for path, content := range files {
		next[path] = slices.Clone(content)
	}
	sha := g.store(cur, next)
	g.branches[branch] = sha
	g.syncHeads(branch, sha)
	return sha, nil
}

// SetBody replaces the description of change request n, as a person editing it would. Tests use it to remove
// a marker.
func (g *Git) SetBody(n int64, body string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	cr, err := g.get(n)
	if err != nil {
		return err
	}
	cr.Body = body
	return nil
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
	if err := providers.CheckWrite(ctx); err != nil {
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
			found := *cr
			// A listing does not carry the conflict, as on GitHub: only GetChangeRequest reports it.
			found.Conflicted = false
			return found, nil
		}
	}
	return providers.ChangeRequest{}, fmt.Errorf("change request for %q: %w", headBranch, providers.ErrNotFound)
}

// ListChangeRequests returns the change requests from headBranch to the target branch, newest first: the open
// ones when openOnly is true, all of them otherwise. More than providers.MaxBranchChangeRequests is ErrTooLarge.
func (g *Git) ListChangeRequests(ctx context.Context, headBranch string, openOnly bool) ([]providers.ChangeRequest, error) {
	if err := g.before("ListChangeRequests"); err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []providers.ChangeRequest
	for _, cr := range slices.Backward(g.crs) {
		if cr.HeadBranch != headBranch || cr.BaseBranch != g.target || (openOnly && cr.State != providers.ChangeRequestOpen) {
			continue
		}
		if out = append(out, *cr); len(out) > providers.MaxBranchChangeRequests {
			return nil, fmt.Errorf("change requests for %q: %w", headBranch, providers.ErrTooLarge)
		}
	}
	return out, nil
}

// Reopen opens a closed change request again, as a person can on the host. It fails with ErrConflict when the
// change request is merged, or when another change request of its head branch is open (one open per head).
func (g *Git) Reopen(n int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	cr, err := g.get(n)
	if err != nil {
		return err
	}
	if cr.State == providers.ChangeRequestMerged {
		return fmt.Errorf("change request %d is merged: %w", n, providers.ErrConflict)
	}
	for _, o := range g.crs {
		if o != cr && o.HeadBranch == cr.HeadBranch && o.State == providers.ChangeRequestOpen {
			return fmt.Errorf("change request %d is open on the branch: %w", o.Number, providers.ErrConflict)
		}
	}
	cr.State = providers.ChangeRequestOpen
	cr.HeadSHA = g.branches[cr.HeadBranch]
	return nil
}

// GetChangeRequest returns the change request with number n, ErrNotFound when there is none or when it is not
// from a shadewright/ branch to the target branch.
func (g *Git) GetChangeRequest(ctx context.Context, n int64) (providers.ChangeRequest, error) {
	if err := g.before("GetChangeRequest"); err != nil {
		return providers.ChangeRequest{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cr, err := g.get(n)
	if err != nil {
		return providers.ChangeRequest{}, err
	}
	if !strings.HasPrefix(cr.HeadBranch, "shadewright/") || cr.BaseBranch != g.target {
		return providers.ChangeRequest{}, fmt.Errorf("change request %d is not a Shadewright one: %w", n, providers.ErrNotFound)
	}
	found := *cr
	found.Conflicted = cr.Conflicted && cr.State == providers.ChangeRequestOpen
	return found, nil
}

// SetConflicted sets whether the host reports change request n in conflict with its base, as GitHub's
// mergeable_state "dirty" does. GetChangeRequest reports it only while the change request is open.
func (g *Git) SetConflicted(n int64, conflicted bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	cr, err := g.get(n)
	if err != nil {
		return err
	}
	cr.Conflicted = conflicted
	return nil
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
	if err := providers.CheckWrite(ctx); err != nil {
		return err
	}
	if err := g.enableAutoMerge(n, expectedHeadSHA); err != nil {
		return err
	}
	return g.after("EnableAutoMerge")
}

func (g *Git) enableAutoMerge(n int64, expectedHeadSHA string) error {
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
	if err := providers.CheckWrite(ctx); err != nil {
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
func (g *Git) Merge(n int64) (string, error) { return g.merge(n, false) }

// MergeWithCommit merges change request n as Merge does, with a merge commit whose second parent is the
// head, so the head becomes an ancestor of the target branch as a regular merge on a host makes it.
func (g *Git) MergeWithCommit(n int64) (string, error) { return g.merge(n, true) }

func (g *Git) merge(n int64, withCommit bool) (string, error) {
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
		fork, inFork := forkFiles[p]
		if inFork && bytes.Equal(fork, data) {
			continue // unchanged on the head branch
		}
		cur, inBase := baseFiles[p]
		switch {
		case inBase && bytes.Equal(cur, data):
			continue // the base already has the same change
		case inBase != inFork || (inBase && !bytes.Equal(cur, fork)):
			return "", fmt.Errorf("file %q changed on both sides: %w", p, providers.ErrConflict)
		}
		next[p] = data
	}
	sha := g.store(baseSHA, next)
	if withCommit {
		c := g.commits[sha]
		c.merged = g.branches[cr.HeadBranch]
		g.commits[sha] = c
	}
	g.branches[cr.BaseBranch] = sha
	g.syncHeads(cr.BaseBranch, sha)
	cr.State = providers.ChangeRequestMerged
	cr.MergeCommit = sha
	return sha, nil
}

// SetMergeCommit replaces the merge commit the fake reports for the merged change request n, as a host that
// reports none (empty) or a value that is not a full commit ID would.
func (g *Git) SetMergeCommit(n int64, sha string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	cr, err := g.get(n)
	if err != nil {
		return err
	}
	if cr.State != providers.ChangeRequestMerged {
		return fmt.Errorf("change request %d is %s: %w", n, cr.State, providers.ErrConflict)
	}
	cr.MergeCommit = sha
	return nil
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

var _ providers.AncestryChecker = (*Git)(nil)

// IsAncestor reports whether ancestor is commit or one of its ancestors. ErrNotFound when either commit is
// unknown.
func (g *Git) IsAncestor(ctx context.Context, ancestor, commit string) (bool, error) {
	if err := g.before("IsAncestor"); err != nil {
		return false, err
	}
	if !providers.IsObjectID(ancestor) || !providers.IsObjectID(commit) {
		return false, fmt.Errorf("commit is not a full object ID: %w", providers.ErrInvalidCommit)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range []string{ancestor, commit} {
		if _, ok := g.commits[c]; !ok {
			return false, fmt.Errorf("commit %q: %w", c, providers.ErrNotFound)
		}
	}
	for c := commit; c != ""; c = g.commits[c].parent {
		if c == ancestor {
			return true, nil
		}
	}
	return false, nil
}
