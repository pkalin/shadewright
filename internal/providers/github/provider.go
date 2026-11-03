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

package github

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/github/ratelimit"
)

// Fixed labels for errors. They name the operation, never its arguments.
const (
	opBranchHead   = "GitHub branch head"
	opReadFile     = "GitHub read file"
	opCreateBranch = "GitHub create branch"
	opCommitFile   = "GitHub commit file"
	opOpenCR       = "GitHub open pull request"
	opFindCR       = "GitHub find pull request"
	opCloseCR      = "GitHub close pull request"
	opAutoMerge    = "GitHub auto-merge"
)

const (
	modeFile       = "100644"
	modeExecutable = "100755"
	modeTree       = "040000"
	stateOpen      = "open"
	stateClosed    = "closed"
)

func (p *Provider) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, p.timeout)
}

// Capabilities reports what this adapter supports. AutoMerge is false until auto-merge is implemented.
func (p *Provider) Capabilities() providers.GitCapabilities {
	return providers.GitCapabilities{AutoMerge: false, RequiredReviews: true}
}

// BranchHead returns the commit at the head of the target branch or of a shadewright/ branch. Any other
// branch is refused without a request.
func (p *Provider) BranchHead(ctx context.Context, branch string) (string, error) {
	if branch != p.target && !isHeadBranch(branch) {
		return "", errors.New(opBranchHead + ": branch is neither the target branch nor a Shadewright branch")
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	return p.ref(ctx, opBranchHead, branch)
}

type refJSON struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

// ref reads a branch with the exact-match endpoint (git/refs/... may answer with every ref sharing a prefix).
func (p *Provider) ref(ctx context.Context, op, branch string) (string, error) {
	r, err := p.do(ctx, call{op: op, method: http.MethodGet, path: "/git/ref/" + refPath(branch)})
	if err != nil {
		return "", err
	}
	if !r.ok() {
		return "", statusError(op, r.status)
	}
	return parseRef(op, r, branch)
}

func parseRef(op string, r response, branch string) (string, error) {
	var v refJSON
	if err := decode(op, r, &v); err != nil {
		return "", err
	}
	if v.Ref != "refs/heads/"+branch || v.Object.Type != "commit" || !validSHA(v.Object.SHA) {
		return "", errors.New(op + ": GitHub returned an unexpected reference")
	}
	return v.Object.SHA, nil
}

type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

type treeJSON struct {
	SHA       string      `json:"sha"`
	Tree      []treeEntry `json:"tree"`
	Truncated bool        `json:"truncated"`
}

func (p *Provider) tree(ctx context.Context, op, sha string) (treeJSON, error) {
	r, err := p.do(ctx, call{op: op, method: http.MethodGet, path: "/git/trees/" + sha})
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			return treeJSON{}, fmt.Errorf("%s: directory listing is too large", op)
		}
		return treeJSON{}, err
	}
	if r.status == http.StatusUnprocessableEntity {
		// GitHub answers 422 for an object that is not a tree or commit.
		return treeJSON{}, fmt.Errorf("%s: GitHub returned status %d: %w", op, r.status, providers.ErrNotFound)
	}
	if !r.ok() {
		return treeJSON{}, statusError(op, r.status)
	}
	var t treeJSON
	if err := decode(op, r, &t); err != nil {
		return treeJSON{}, err
	}
	if !validSHA(t.SHA) {
		return treeJSON{}, errors.New(op + ": GitHub returned an unexpected tree")
	}
	return t, nil
}

// lookup resolves path at commit through one tree per directory and returns the root tree and the file's
// entry. ErrNotFound when the commit or path does not exist, or the path is not a regular file.
func (p *Provider) lookup(ctx context.Context, op, commit, path string) (string, treeEntry, error) {
	segs := strings.Split(path, "/")
	t, err := p.tree(ctx, op, commit)
	if err != nil {
		return "", treeEntry{}, err
	}
	root := t.SHA
	for i, seg := range segs {
		idx := slices.IndexFunc(t.Tree, func(e treeEntry) bool { return e.Path == seg })
		if idx < 0 {
			if t.Truncated {
				return "", treeEntry{}, errors.New(op + ": directory listing is truncated")
			}
			return "", treeEntry{}, fmt.Errorf("%s: file %w", op, providers.ErrNotFound)
		}
		e := t.Tree[idx]
		if !validSHA(e.SHA) {
			return "", treeEntry{}, errors.New(op + ": GitHub returned an unexpected tree entry")
		}
		if i == len(segs)-1 {
			// Only regular files: never a directory, a submodule (type commit) or a symbolic link (120000).
			if e.Type != "blob" || (e.Mode != modeFile && e.Mode != modeExecutable) {
				return "", treeEntry{}, fmt.Errorf("%s: not a regular file: %w", op, providers.ErrNotFound)
			}
			return root, e, nil
		}
		if e.Type != "tree" || e.Mode != modeTree {
			return "", treeEntry{}, fmt.Errorf("%s: file %w", op, providers.ErrNotFound)
		}
		if t, err = p.tree(ctx, op, e.SHA); err != nil {
			return "", treeEntry{}, err
		}
	}
	return "", treeEntry{}, fmt.Errorf("%s: file %w", op, providers.ErrNotFound)
}

// ReadFile returns a regular file at a commit. The size GitHub reports is checked before the content is
// downloaded, at most MaxFileBytes+1 bytes are read, and the content must match its Git object ID.
func (p *Provider) ReadFile(ctx context.Context, commit, path string) (providers.File, error) {
	if !validSHA(commit) {
		return providers.File{}, errors.New(opReadFile + ": commit is not a full SHA")
	}
	if !validPath(path) {
		return providers.File{}, errors.New(opReadFile + ": path is invalid")
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	_, e, err := p.lookup(ctx, opReadFile, commit, path)
	if err != nil {
		return providers.File{}, err
	}
	if e.Size > providers.MaxFileBytes {
		return providers.File{}, fmt.Errorf("%s: %w", opReadFile, providers.ErrTooLarge)
	}
	r, err := p.do(ctx, call{
		op: opReadFile, method: http.MethodGet, path: "/git/blobs/" + e.SHA,
		accept: "application/vnd.github.raw+json", limit: providers.MaxFileBytes,
	})
	if errors.Is(err, errBodyTooLarge) {
		return providers.File{}, fmt.Errorf("%s: %w", opReadFile, providers.ErrTooLarge)
	}
	if err != nil {
		return providers.File{}, err
	}
	if !r.ok() {
		return providers.File{}, statusError(opReadFile, r.status)
	}
	if blobSHA(r.body) != e.SHA {
		return providers.File{}, errors.New(opReadFile + ": content does not match its Git object ID")
	}
	return providers.File{Path: path, Commit: commit, Data: r.body}, nil
}

type compareJSON struct {
	Status string `json:"status"`
}

// onTarget checks that commit is the head of the target branch or an ancestor of it.
func (p *Provider) onTarget(ctx context.Context, op, commit string) error {
	head, err := p.ref(ctx, op, p.target)
	if err != nil {
		return err
	}
	if head == commit {
		return nil
	}
	r, err := p.do(ctx, call{
		op: op, method: http.MethodGet, path: "/compare/" + head + "..." + commit,
		query: url.Values{"per_page": {"1"}},
	})
	if errors.Is(err, errBodyTooLarge) {
		// Only a commit that is not behind the target has commits and files to list.
		return errors.New(op + ": base commit is not on the target branch")
	}
	if err != nil {
		return err
	}
	if !r.ok() {
		return statusError(op, r.status)
	}
	var c compareJSON
	if err := decode(op, r, &c); err != nil {
		return err
	}
	if c.Status != "behind" && c.Status != "identical" {
		return errors.New(op + ": base commit is not on the target branch")
	}
	return nil
}

// CreateBranch creates shadewright/<name> at a commit of the target branch. A branch that already exists
// at that commit is success; at another commit, ErrConflict.
func (p *Provider) CreateBranch(ctx context.Context, branch, fromCommit string) error {
	if !isHeadBranch(branch) {
		return errors.New(opCreateBranch + ": branch is not a Shadewright branch")
	}
	if !validSHA(fromCommit) {
		return errors.New(opCreateBranch + ": commit is not a full SHA")
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	if err := p.onTarget(ctx, opCreateBranch, fromCommit); err != nil {
		return err
	}
	r, err := p.do(ctx, call{
		op: opCreateBranch, method: http.MethodPost, path: "/git/refs",
		body: map[string]string{"ref": "refs/heads/" + branch, "sha": fromCommit},
	})
	if err != nil {
		return err
	}
	switch r.status {
	case http.StatusCreated:
		sha, err := parseRef(opCreateBranch, r, branch)
		if err != nil {
			return err
		}
		if sha != fromCommit {
			return fmt.Errorf("%s: branch is at another commit: %w", opCreateBranch, providers.ErrConflict)
		}
		return nil
	case http.StatusUnprocessableEntity:
		// Usually "Reference already exists", for example after a retry or a restart: compare its head.
		sha, err := p.ref(ctx, opCreateBranch, branch)
		if err != nil {
			if providers.IsNotFound(err) {
				// Not an existing branch: a name or rule GitHub refuses. The same request fails the same way.
				return statusError(opCreateBranch, r.status)
			}
			return err
		}
		if sha != fromCommit {
			return fmt.Errorf("%s: branch exists at another commit: %w", opCreateBranch, providers.ErrConflict)
		}
		return nil
	}
	return statusError(opCreateBranch, r.status)
}

type shaJSON struct {
	SHA string `json:"sha"`
}

// post sends a create request that must answer 201 with an object ID.
func (p *Provider) post(ctx context.Context, path string, body any) (string, error) {
	r, err := p.do(ctx, call{op: opCommitFile, method: http.MethodPost, path: path, body: body})
	if err != nil {
		return "", err
	}
	if r.status != http.StatusCreated {
		return "", statusError(opCommitFile, r.status)
	}
	var v shaJSON
	if err := decode(opCommitFile, r, &v); err != nil {
		return "", err
	}
	if !validSHA(v.SHA) {
		return "", errors.New(opCommitFile + ": GitHub returned an unexpected object ID")
	}
	return v.SHA, nil
}

// CommitFile commits one change to an existing regular file on a shadewright/ branch whose head must be
// expectedHead, and returns the new commit. The file keeps its mode. The branch moves with an atomic
// compare-and-swap on expectedHead (GraphQL updateRefs, no force); a head that moved in any direction is
// ErrConflict and is never overwritten.
func (p *Provider) CommitFile(
	ctx context.Context, branch, expectedHead string, change providers.FileChange,
) (string, error) {
	switch {
	case !isHeadBranch(branch):
		return "", errors.New(opCommitFile + ": branch is not a Shadewright branch")
	case !validSHA(expectedHead):
		return "", errors.New(opCommitFile + ": expected head is not a full SHA")
	case !validPath(change.Path):
		return "", errors.New(opCommitFile + ": path is invalid")
	case len(change.Content) > providers.MaxFileBytes:
		return "", fmt.Errorf("%s: %w", opCommitFile, providers.ErrTooLarge)
	case strings.TrimSpace(change.Message) == "" || len(change.Message) > maxMessageBytes ||
		!utf8.ValidString(change.Message):
		return "", errors.New(opCommitFile + ": commit message is empty, too long or not UTF-8")
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

	head, err := p.ref(ctx, opCommitFile, branch)
	if err != nil {
		return "", err
	}
	if head != expectedHead {
		return "", fmt.Errorf("%s: branch head moved: %w", opCommitFile, providers.ErrConflict)
	}
	root, entry, err := p.lookup(ctx, opCommitFile, expectedHead, change.Path)
	if err != nil {
		return "", err
	}
	blob, err := p.post(ctx, "/git/blobs", map[string]string{
		"content": base64.StdEncoding.EncodeToString(change.Content), "encoding": "base64",
	})
	if err != nil {
		return "", err
	}
	if blob != blobSHA(change.Content) {
		return "", errors.New(opCommitFile + ": GitHub stored different content")
	}
	type treeItem struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
	}
	tree, err := p.post(ctx, "/git/trees", struct {
		BaseTree string     `json:"base_tree"`
		Tree     []treeItem `json:"tree"`
	}{root, []treeItem{{Path: change.Path, Mode: entry.Mode, Type: "blob", SHA: blob}}})
	if err != nil {
		return "", err
	}
	commit, err := p.post(ctx, "/git/commits", struct {
		Message string   `json:"message"`
		Tree    string   `json:"tree"`
		Parents []string `json:"parents"`
	}{change.Message, tree, []string{expectedHead}})
	if err != nil {
		return "", err
	}
	if err := p.updateRef(ctx, branch, expectedHead, commit); err != nil {
		return "", err
	}
	return commit, nil
}

type repoJSON struct {
	NodeID   string `json:"node_id"`
	FullName string `json:"full_name"`
}

// repositoryID returns the GraphQL node ID of the repository.
func (p *Provider) repositoryID(ctx context.Context) (string, error) {
	r, err := p.do(ctx, call{op: opCommitFile, method: http.MethodGet})
	if err != nil {
		return "", err
	}
	if !r.ok() {
		return "", statusError(opCommitFile, r.status)
	}
	var v repoJSON
	if err := decode(opCommitFile, r, &v); err != nil {
		return "", err
	}
	if !strings.EqualFold(v.FullName, p.repo.String()) || !nodeIDPattern.MatchString(v.NodeID) {
		return "", errors.New(opCommitFile + ": GitHub returned an unexpected repository")
	}
	return v.NodeID, nil
}

const updateRefsMutation = `mutation($repo: ID!, $updates: [RefUpdate!]!) {
  updateRefs(input: {repositoryId: $repo, refUpdates: $updates}) { clientMutationId }
}`

type graphQLResponse struct {
	Data *struct {
		UpdateRefs *struct{} `json:"updateRefs"`
	} `json:"data"`
	Errors []struct {
		Type string `json:"type"`
	} `json:"errors"`
}

// updateRef moves branch from expectedHead to commit with GraphQL updateRefs: an atomic compare-and-swap on
// beforeOid, without force. A branch that moved in any direction since it was read, including a rewind to
// an ancestor, is refused, so removed commits are never restored and pushed commits never overwritten.
// When GitHub refuses, the branch is read again: already at commit (a repeated request after a lost
// response) is success, anywhere else ErrConflict. The text of GraphQL errors is never read.
func (p *Provider) updateRef(ctx context.Context, branch, expectedHead, commit string) error {
	repoID, err := p.repositoryID(ctx)
	if err != nil {
		return err
	}
	type refUpdate struct {
		Name      string `json:"name"`
		BeforeOid string `json:"beforeOid"`
		AfterOid  string `json:"afterOid"`
		Force     bool   `json:"force"`
	}
	r, err := p.do(ctx, call{op: opCommitFile, method: http.MethodPost, graphQL: true, body: map[string]any{
		"query": updateRefsMutation,
		"variables": map[string]any{
			"repo": repoID,
			"updates": []refUpdate{{
				Name: "refs/heads/" + branch, BeforeOid: expectedHead, AfterOid: commit, Force: false,
			}},
		},
	}})
	if err != nil {
		return err
	}
	if !r.ok() {
		return statusError(opCommitFile, r.status)
	}
	var v graphQLResponse
	if err := decode(opCommitFile, r, &v); err != nil {
		return err
	}
	if len(v.Errors) == 0 && v.Data != nil && v.Data.UpdateRefs != nil {
		return nil
	}
	for _, e := range v.Errors {
		if e.Type == "RATE_LIMITED" {
			return &providers.RetryableError{
				RetryAfter: ratelimit.Delay(r.header, p.now()), Err: errors.New(opCommitFile + ": GitHub rate limit"),
			}
		}
	}
	head, err := p.ref(ctx, opCommitFile, branch)
	switch {
	case err != nil:
		return err
	case head == commit:
		return nil
	case head != expectedHead:
		return fmt.Errorf("%s: branch head moved: %w", opCommitFile, providers.ErrConflict)
	}
	return errors.New(opCommitFile + ": GitHub refused the branch update")
}

type pullRepoJSON struct {
	FullName string `json:"full_name"`
}

type pullJSON struct {
	Number         int64   `json:"number"`
	State          string  `json:"state"`
	Title          string  `json:"title"`
	Body           *string `json:"body"`
	MergedAt       *string `json:"merged_at"`
	MergeCommitSHA *string `json:"merge_commit_sha"`
	Head           struct {
		Ref  string        `json:"ref"`
		SHA  string        `json:"sha"`
		Repo *pullRepoJSON `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string        `json:"ref"`
		Repo *pullRepoJSON `json:"repo"`
	} `json:"base"`
}

func (p *Provider) ownRepo(r *pullRepoJSON) bool {
	return r != nil && strings.EqualFold(r.FullName, p.repo.String())
}

// ours reports whether a pull request comes from a shadewright/ branch of this repository and targets the
// target branch of this repository. A fork may use the same branch name; its pull requests are not ours.
func (p *Provider) ours(pr pullJSON) bool {
	return pr.Number > 0 && isHeadBranch(pr.Head.Ref) && p.ownRepo(pr.Head.Repo) &&
		pr.Base.Ref == p.target && p.ownRepo(pr.Base.Repo)
}

func (p *Provider) toChangeRequest(op string, pr pullJSON) (providers.ChangeRequest, error) {
	if !validSHA(pr.Head.SHA) {
		return providers.ChangeRequest{}, errors.New(op + ": GitHub returned an unexpected pull request")
	}
	cr := providers.ChangeRequest{
		Number:     pr.Number,
		URL:        "https://github.com/" + p.repo.String() + "/pull/" + strconv.FormatInt(pr.Number, 10),
		HeadBranch: pr.Head.Ref,
		BaseBranch: pr.Base.Ref,
		HeadSHA:    pr.Head.SHA,
		Title:      pr.Title,
	}
	if pr.Body != nil {
		cr.Body = *pr.Body
	}
	switch {
	case pr.MergedAt != nil:
		cr.State = providers.ChangeRequestMerged
		if pr.MergeCommitSHA == nil || !validSHA(*pr.MergeCommitSHA) {
			return providers.ChangeRequest{}, errors.New(op + ": GitHub returned a merged pull request without its commit")
		}
		cr.MergeCommit = *pr.MergeCommitSHA
	case pr.State == stateOpen:
		cr.State = providers.ChangeRequestOpen
	case pr.State == stateClosed:
		cr.State = providers.ChangeRequestClosed
	default:
		return providers.ChangeRequest{}, errors.New(op + ": GitHub returned an unexpected pull request state")
	}
	return cr, nil
}

// pulls lists the pull requests of a head branch in a state ("open" or "all"), newest first, keeping only
// those from this repository. A branch of Shadewright has one or a few; the first page of 100 is enough.
func (p *Provider) pulls(ctx context.Context, op, branch, state string) ([]pullJSON, error) {
	r, err := p.do(ctx, call{op: op, method: http.MethodGet, path: "/pulls", query: url.Values{
		"head": {p.repo.Owner + ":" + branch}, "state": {state},
		"sort": {"created"}, "direction": {"desc"}, "per_page": {"100"},
	}})
	if err != nil {
		return nil, err
	}
	if !r.ok() {
		return nil, statusError(op, r.status)
	}
	var all []pullJSON
	if err := decode(op, r, &all); err != nil {
		return nil, err
	}
	out := slices.DeleteFunc(all, func(pr pullJSON) bool {
		return pr.Head.Ref != branch || !p.ownRepo(pr.Head.Repo)
	})
	slices.SortFunc(out, func(a, b pullJSON) int { return cmp.Compare(b.Number, a.Number) })
	return out, nil
}

// openFor returns the open pull request of a head branch, if any. One that targets another branch is
// ErrConflict: GitHub allows one open pull request per head and base, and this one is not ours to reuse.
func (p *Provider) openFor(ctx context.Context, op, branch string) (providers.ChangeRequest, bool, error) {
	open, err := p.pulls(ctx, op, branch, stateOpen)
	if err != nil {
		return providers.ChangeRequest{}, false, err
	}
	// Check every open one before choosing: the refusal must not depend on which was created first.
	var match *pullJSON
	for i, pr := range open {
		if pr.State != stateOpen {
			continue
		}
		if !p.ours(pr) {
			return providers.ChangeRequest{}, false, fmt.Errorf(
				"%s: an open pull request of this branch targets another branch: %w", op, providers.ErrConflict)
		}
		if match == nil {
			match = &open[i]
		}
	}
	if match == nil {
		return providers.ChangeRequest{}, false, nil
	}
	cr, err := p.toChangeRequest(op, *match)
	return cr, err == nil, err
}

// OpenChangeRequest opens a pull request from a shadewright/ branch to the target branch, or returns the
// open one for that branch unchanged.
func (p *Provider) OpenChangeRequest(
	ctx context.Context, req providers.NewChangeRequest,
) (providers.ChangeRequest, error) {
	switch {
	case !isHeadBranch(req.HeadBranch):
		return providers.ChangeRequest{}, errors.New(opOpenCR + ": head branch is not a Shadewright branch")
	case req.BaseBranch != p.target:
		return providers.ChangeRequest{}, errors.New(opOpenCR + ": base branch is not the target branch")
	case strings.TrimSpace(req.Title) == "" || !utf8.ValidString(req.Title) ||
		utf8.RuneCountInString(req.Title) > maxTitleRunes:
		return providers.ChangeRequest{}, errors.New(opOpenCR + ": title is empty, too long or not UTF-8")
	case !utf8.ValidString(req.Body) || utf8.RuneCountInString(req.Body) > maxBodyRunes:
		return providers.ChangeRequest{}, errors.New(opOpenCR + ": body is too long or not UTF-8")
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

	if cr, ok, err := p.openFor(ctx, opOpenCR, req.HeadBranch); err != nil || ok {
		return cr, err
	}
	r, err := p.do(ctx, call{op: opOpenCR, method: http.MethodPost, path: "/pulls", body: struct {
		Title               string `json:"title"`
		Head                string `json:"head"`
		Base                string `json:"base"`
		Body                string `json:"body"`
		MaintainerCanModify bool   `json:"maintainer_can_modify"`
	}{req.Title, req.HeadBranch, p.target, req.Body, false}})
	if err != nil {
		return providers.ChangeRequest{}, err
	}
	switch r.status {
	case http.StatusCreated:
		var pr pullJSON
		if err := decode(opOpenCR, r, &pr); err != nil {
			return providers.ChangeRequest{}, err
		}
		if !p.ours(pr) || pr.Head.Ref != req.HeadBranch {
			return providers.ChangeRequest{}, errors.New(opOpenCR + ": GitHub returned an unexpected pull request")
		}
		return p.toChangeRequest(opOpenCR, pr)
	case http.StatusUnprocessableEntity:
		// Usually "A pull request already exists", after a lost response or a race: return that one.
		if cr, ok, err := p.openFor(ctx, opOpenCR, req.HeadBranch); err != nil || ok {
			return cr, err
		}
		if _, err := p.ref(ctx, opOpenCR, req.HeadBranch); err != nil {
			return providers.ChangeRequest{}, err
		}
		// No open pull request explains the refusal, and the branch exists: final.
		return providers.ChangeRequest{}, statusError(opOpenCR, r.status)
	}
	return providers.ChangeRequest{}, statusError(opOpenCR, r.status)
}

// FindChangeRequest returns the most recent pull request, in any state, from a shadewright/ branch of this
// repository to the target branch. ErrNotFound when there is none.
func (p *Provider) FindChangeRequest(ctx context.Context, headBranch string) (providers.ChangeRequest, error) {
	if !isHeadBranch(headBranch) {
		return providers.ChangeRequest{}, errors.New(opFindCR + ": head branch is not a Shadewright branch")
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	all, err := p.pulls(ctx, opFindCR, headBranch, "all")
	if err != nil {
		return providers.ChangeRequest{}, err
	}
	for _, pr := range all {
		if p.ours(pr) {
			return p.toChangeRequest(opFindCR, pr)
		}
	}
	return providers.ChangeRequest{}, fmt.Errorf("%s: %w", opFindCR, providers.ErrNotFound)
}

func (p *Provider) pull(ctx context.Context, number int64) (pullJSON, error) {
	r, err := p.do(ctx, call{op: opCloseCR, method: http.MethodGet, path: "/pulls/" + strconv.FormatInt(number, 10)})
	if err != nil {
		return pullJSON{}, err
	}
	if !r.ok() {
		return pullJSON{}, statusError(opCloseCR, r.status)
	}
	var pr pullJSON
	if err := decode(opCloseCR, r, &pr); err != nil {
		return pullJSON{}, err
	}
	if pr.Number != number {
		return pullJSON{}, errors.New(opCloseCR + ": GitHub returned an unexpected pull request")
	}
	if !p.ours(pr) {
		return pullJSON{}, errors.New(opCloseCR + ": not a Shadewright pull request to the target branch")
	}
	return pr, nil
}

// closeState is nil for a closed pull request, ErrConflict for a merged one, and false for an open one.
func closeState(pr pullJSON) (bool, error) {
	switch {
	case pr.MergedAt != nil:
		return true, fmt.Errorf("%s: pull request is merged: %w", opCloseCR, providers.ErrConflict)
	case pr.State == stateClosed:
		return true, nil
	case pr.State == stateOpen:
		return false, nil
	}
	return true, errors.New(opCloseCR + ": GitHub returned an unexpected pull request state")
}

// CloseChangeRequest closes a pull request from a shadewright/ branch to the target branch without merging
// it. Closing a closed one succeeds; a merged one is ErrConflict. Other pull requests are refused.
func (p *Provider) CloseChangeRequest(ctx context.Context, number int64) error {
	if number < 1 {
		return errors.New(opCloseCR + ": number must be positive")
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	pr, err := p.pull(ctx, number)
	if err != nil {
		return err
	}
	if done, err := closeState(pr); done {
		return err
	}
	r, err := p.do(ctx, call{
		op: opCloseCR, method: http.MethodPatch, path: "/pulls/" + strconv.FormatInt(number, 10),
		body: map[string]string{"state": stateClosed},
	})
	if err != nil {
		return err
	}
	switch r.status {
	case http.StatusOK:
		var closed pullJSON
		if err := decode(opCloseCR, r, &closed); err != nil {
			return err
		}
		if closed.Number != number {
			return errors.New(opCloseCR + ": GitHub returned an unexpected pull request")
		}
		pr = closed
	case http.StatusUnprocessableEntity:
		// It may have been merged or closed in the meantime: read it again.
		if pr, err = p.pull(ctx, number); err != nil {
			return err
		}
	default:
		return statusError(opCloseCR, r.status)
	}
	if done, err := closeState(pr); done {
		return err
	}
	return errors.New(opCloseCR + ": pull request is still open")
}

// EnableAutoMerge is not supported by this adapter yet (Capabilities reports AutoMerge false).
func (p *Provider) EnableAutoMerge(context.Context, int64, string) error {
	return errors.New(opAutoMerge + ": not supported by this adapter yet")
}
