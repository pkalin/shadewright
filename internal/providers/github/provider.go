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
	"sync"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/github/ratelimit"
)

// Fixed labels for errors. They name the operation, never its arguments.
const (
	opBranchHead   = "GitHub branch head"
	opReadFile     = "GitHub read file"
	opListDir      = "GitHub list directory"
	opCreateBranch = "GitHub create branch"
	opCommitFile   = "GitHub commit file"
	opCheckBranch  = "GitHub check branch"
	opOpenCR       = "GitHub open pull request"
	opFindCR       = "GitHub find pull request"
	opCloseCR      = "GitHub close pull request"
	opCommentCR    = "GitHub comment on pull request"
	opAutoMerge    = "GitHub auto-merge"
)

const (
	modeFile       = "100644"
	modeExecutable = "100755"
	modeTree       = "040000"
	modeSymlink    = "120000"
	typeBlob       = "blob"
	typeTree       = "tree"
	typeCommit     = "commit"
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
// entry. ErrPathNotInTree when a tree it read has no such entry, or the path is not a regular file; any other
// failure to read a tree (including a 404 or 422 for the commit) is returned as it is.
func (p *Provider) lookup(ctx context.Context, op, commit, path string) (string, treeEntry, error) {
	segs := strings.Split(path, "/")
	root, t, err := p.descend(ctx, op, commit, segs[:len(segs)-1])
	if err != nil {
		return "", treeEntry{}, err
	}
	e, err := entry(op, t, segs[len(segs)-1])
	if err != nil {
		return "", treeEntry{}, err
	}
	// Only regular files: never a directory, a submodule (type commit) or a symbolic link (120000).
	if !isRegularFile(e) {
		return "", treeEntry{}, fmt.Errorf("%s: not a regular file: %w", op, providers.ErrPathNotInTree)
	}
	return root, e, nil
}

// descend reads the tree of the directory dirs at commit and returns the root tree's SHA and that tree. It
// starts from the deepest directory on the way whose tree SHA an earlier call recorded (p.trees), else from the
// root tree at commit, and reads one tree per remaining segment, so a walk down a directory tree costs about
// one request per directory. ErrPathNotInTree when a segment is missing or not a directory; any other failure
// to read a tree is returned as it is.
func (p *Provider) descend(ctx context.Context, op, commit string, dirs []string) (string, treeJSON, error) {
	root, sha, from := p.trees.start(commit, dirs)
	var t treeJSON
	var err error
	if root == "" {
		if t, err = p.tree(ctx, op, commit); err != nil {
			return "", treeJSON{}, err
		}
		root = t.SHA
		p.trees.keep(commit, nil, root, root, t)
	} else if t, err = p.tree(ctx, op, sha); err != nil {
		return "", treeJSON{}, err
	}
	for i := from; i < len(dirs); i++ {
		e, err := entry(op, t, dirs[i])
		if err != nil {
			return "", treeJSON{}, err
		}
		if e.Type != typeTree || e.Mode != modeTree {
			return "", treeJSON{}, fmt.Errorf("%s: file %w", op, providers.ErrPathNotInTree)
		}
		if t, err = p.tree(ctx, op, e.SHA); err != nil {
			return "", treeJSON{}, err
		}
		p.trees.keep(commit, dirs[:i+1], root, e.SHA, t)
	}
	return root, t, nil
}

// maxTreeIndex bounds how many directory trees a Provider remembers; past it, it starts over.
const maxTreeIndex = 4096

// treeIndex maps a directory at a commit to the SHA of its tree, and a commit to its root tree. It is bounded
// and safe for concurrent use. Only trees GitHub returned are recorded, and a tree at a commit never changes,
// so an entry never goes stale.
type treeIndex struct {
	mu   sync.Mutex
	shas map[treeKey]string
}

type treeKey struct{ commit, dir string }

// start returns where descend can begin for dirs at commit: the root tree's SHA (empty when unknown), the SHA
// of the deepest directory on the way that is known (the root's when none) and how many segments of dirs it
// covers.
func (x *treeIndex) start(commit string, dirs []string) (root, sha string, from int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	root, ok := x.shas[treeKey{commit, ""}]
	if !ok {
		return "", "", 0
	}
	for i := len(dirs); i > 0; i-- {
		if s, ok := x.shas[treeKey{commit, strings.Join(dirs[:i], "/")}]; ok {
			return root, s, i
		}
	}
	return root, root, 0
}

// keep records the root tree of commit, the tree sha of dirs and the subtrees t lists.
func (x *treeIndex) keep(commit string, dirs []string, root, sha string, t treeJSON) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.shas == nil || len(x.shas)+len(t.Tree)+2 > maxTreeIndex {
		x.shas = map[treeKey]string{}
	}
	dir := strings.Join(dirs, "/")
	x.shas[treeKey{commit, ""}] = root
	if dir != "" {
		x.shas[treeKey{commit, dir}] = sha
	}
	for _, e := range t.Tree {
		if e.Type != typeTree || e.Mode != modeTree || !validSHA(e.SHA) || !validEntryName(e.Path) {
			continue
		}
		child := e.Path
		if dir != "" {
			child = dir + "/" + e.Path
		}
		x.shas[treeKey{commit, child}] = e.SHA
	}
}

// validEntryName reports whether name can be one entry of a Git tree: not empty, "." or "..", no slash or NUL
// byte, valid UTF-8.
func validEntryName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00") && utf8.ValidString(name)
}

// entry returns the entry of t named seg: ErrPathNotInTree when there is none, an error when t is truncated
// and so may hold it, or when the entry's SHA is not valid.
func entry(op string, t treeJSON, seg string) (treeEntry, error) {
	idx := slices.IndexFunc(t.Tree, func(e treeEntry) bool { return e.Path == seg })
	if idx < 0 {
		if t.Truncated {
			return treeEntry{}, errors.New(op + ": directory listing is truncated")
		}
		return treeEntry{}, fmt.Errorf("%s: file %w", op, providers.ErrPathNotInTree)
	}
	e := t.Tree[idx]
	if !validSHA(e.SHA) {
		return treeEntry{}, errors.New(op + ": GitHub returned an unexpected tree entry")
	}
	return e, nil
}

// isRegularFile reports whether e is a regular file, executable or not.
func isRegularFile(e treeEntry) bool {
	return e.Type == typeBlob && (e.Mode == modeFile || e.Mode == modeExecutable)
}

// ListDir lists one directory at a commit through the Git Trees API, not recursively. It starts from the
// deepest directory on the way whose tree an earlier call read, so listing every directory of a walk costs
// about one request per directory. Each answer is bounded by the JSON body limit, and a directory of more than
// providers.MaxDirEntries entries is refused with ErrTooLarge. Symbolic links (mode 120000) and submodules
// (type commit) are skipped. Names are returned as Git stores them; an entry no tree can hold (see
// validEntryName) or of another type fails the listing with providers.ErrInvalidEntry.
func (p *Provider) ListDir(ctx context.Context, commit, path string) ([]providers.DirEntry, error) {
	if !validSHA(commit) {
		return nil, errors.New(opListDir + ": commit is not a full SHA")
	}
	var dirs []string
	if path != "." {
		if !validPath(path) {
			return nil, errors.New(opListDir + ": path is invalid")
		}
		dirs = strings.Split(path, "/")
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	_, t, err := p.descend(ctx, opListDir, commit, dirs)
	if err != nil {
		return nil, err
	}
	if len(t.Tree) > providers.MaxDirEntries {
		return nil, fmt.Errorf("%s: %w", opListDir, providers.ErrTooLarge)
	}
	if t.Truncated {
		return nil, errors.New(opListDir + ": directory listing is truncated")
	}
	out := make([]providers.DirEntry, 0, len(t.Tree))
	for _, e := range t.Tree {
		var typ providers.DirEntryType
		switch {
		case e.Type == typeCommit || (e.Type == typeBlob && e.Mode == modeSymlink):
			continue
		case isRegularFile(e):
			typ = providers.DirEntryFile
		case e.Type == typeTree && e.Mode == modeTree:
			typ = providers.DirEntryDir
		default:
			return nil, fmt.Errorf("%s: %w", opListDir, providers.ErrInvalidEntry)
		}
		if !validEntryName(e.Path) {
			return nil, fmt.Errorf("%s: %w", opListDir, providers.ErrInvalidEntry)
		}
		out = append(out, providers.DirEntry{Name: e.Path, Type: typ})
	}
	slices.SortFunc(out, func(a, b providers.DirEntry) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// ReadFile returns a regular file at a commit. The size GitHub reports in the tree entry is checked against
// the limit (clamped by providers.ReadLimit) before the content is downloaded, at most limit+1 bytes are read
// (two for a limit of 0), and the content must match its Git object ID: a body longer than the limit the tree
// entry fits is refused as content that does not match, not as ErrTooLarge.
func (p *Provider) ReadFile(ctx context.Context, commit, path string, limit int) (providers.File, error) {
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
	limit = providers.ReadLimit(limit)
	if e.Size > int64(limit) {
		return providers.File{}, fmt.Errorf("%s: %w", opReadFile, providers.ErrTooLarge)
	}
	r, err := p.do(ctx, call{
		op: opReadFile, method: http.MethodGet, path: "/git/blobs/" + e.SHA,
		// A call limit of 0 means the default JSON bound, so an empty file is read with a limit of 1 and its
		// length checked below.
		accept: "application/vnd.github.raw+json", limit: int64(max(limit, 1)),
	})
	// The tree entry said the file fits the limit, so a longer body is not the blob it names.
	if errors.Is(err, errBodyTooLarge) || (err == nil && r.ok() && len(r.body) > limit) {
		return providers.File{}, errors.New(opReadFile + ": content does not match its Git object ID")
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
	ok, err := p.isOnTarget(ctx, op, commit)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New(op + ": base commit is not on the target branch")
	}
	return nil
}

// isOnTarget reports whether commit is the head of the target branch or an ancestor of it. An error means it
// could not be found out.
func (p *Provider) isOnTarget(ctx context.Context, op, commit string) (bool, error) {
	head, err := p.ref(ctx, op, p.target)
	if err != nil {
		return false, err
	}
	return p.isAncestor(ctx, op, head, commit)
}

// isAncestor reports whether commit is head or an ancestor of it. An error means it could not be found out.
func (p *Provider) isAncestor(ctx context.Context, op, head, commit string) (bool, error) {
	if head == commit {
		return true, nil
	}
	r, err := p.do(ctx, call{
		op: op, method: http.MethodGet, path: "/compare/" + head + "..." + commit,
		query: url.Values{paramPerPage: {"1"}},
	})
	if errors.Is(err, errBodyTooLarge) {
		// Only a commit that is not behind the target has commits and files to list.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !r.ok() {
		return false, statusError(op, r.status)
	}
	var c compareJSON
	if err := decode(op, r, &c); err != nil {
		return false, err
	}
	return c.Status == "behind" || c.Status == "identical", nil
}

type commitJSON struct {
	SHA     string    `json:"sha"`
	Parents []shaJSON `json:"parents"`
}

// CheckBranch reports what commit carries over the target branch (providers.GitProvider.CheckBranch): it reads
// whether the commit is on the target branch, else its parents, whether its one parent is, and then the trees of
// the two along path, one tree each per directory. Every entry beside the one on the way to the file must be
// equal in name, mode, type and object ID, and the file must be a regular file of the same mode in both. A
// truncated or oversized tree is an error, never a pass.
func (p *Provider) CheckBranch(ctx context.Context, commit, path string) (providers.BranchState, error) {
	switch {
	case !validSHA(commit):
		return 0, errors.New(opCheckBranch + ": commit is not a full SHA")
	case !validPath(path):
		return 0, errors.New(opCheckBranch + ": path is invalid")
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	switch on, err := p.isOnTarget(ctx, opCheckBranch, commit); {
	case err != nil:
		return 0, err
	case on:
		return providers.BranchAtTarget, nil
	}
	parents, err := p.parents(ctx, commit)
	if err != nil {
		return 0, err
	}
	if len(parents) != 1 {
		return providers.BranchForeign, nil
	}
	parent := parents[0]
	switch on, err := p.isOnTarget(ctx, opCheckBranch, parent); {
	case err != nil:
		return 0, err
	case !on:
		return providers.BranchForeign, nil
	}
	return p.changedOnly(ctx, commit, parent, strings.Split(path, "/"))
}

// parents reads the parents of commit, each a full SHA.
func (p *Provider) parents(ctx context.Context, commit string) ([]string, error) {
	r, err := p.do(ctx, call{op: opCheckBranch, method: http.MethodGet, path: "/git/commits/" + commit})
	if err != nil {
		return nil, err
	}
	if !r.ok() {
		return nil, statusError(opCheckBranch, r.status)
	}
	var c commitJSON
	if err := decode(opCheckBranch, r, &c); err != nil {
		return nil, err
	}
	if c.SHA != commit {
		return nil, errors.New(opCheckBranch + ": GitHub returned an unexpected commit")
	}
	out := make([]string, 0, len(c.Parents))
	for _, pc := range c.Parents {
		if !validSHA(pc.SHA) {
			return nil, errors.New(opCheckBranch + ": GitHub returned an unexpected commit")
		}
		out = append(out, pc.SHA)
	}
	return out, nil
}

// CheckMerged reports what head carried over the target branch before mergeCommit merged it
// (providers.GitProvider.CheckMerged): mergeCommit must be on the target branch and not head itself, head must
// have one parent that is the first parent of mergeCommit or an ancestor of it, and the trees of head and that
// parent are compared as CheckBranch compares them.
func (p *Provider) CheckMerged(ctx context.Context, head, mergeCommit, path string) (providers.BranchState, string, error) {
	switch {
	case !validSHA(head) || !validSHA(mergeCommit):
		return 0, "", errors.New(opCheckBranch + ": commit is not a full SHA")
	case !validPath(path):
		return 0, "", errors.New(opCheckBranch + ": path is invalid")
	case head == mergeCommit:
		// A head taken into the target branch as it is says nothing about what was on it before.
		return providers.BranchForeign, "", nil
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	switch on, err := p.isOnTarget(ctx, opCheckBranch, mergeCommit); {
	case err != nil:
		return 0, "", err
	case !on:
		return providers.BranchForeign, "", nil
	}
	merged, err := p.parents(ctx, mergeCommit)
	if err != nil {
		return 0, "", err
	}
	parents, err := p.parents(ctx, head)
	if err != nil {
		return 0, "", err
	}
	if len(merged) == 0 || len(parents) != 1 {
		return providers.BranchForeign, "", nil
	}
	parent := parents[0]
	switch before, err := p.isAncestor(ctx, opCheckBranch, merged[0], parent); {
	case err != nil:
		return 0, "", err
	case !before:
		return providers.BranchForeign, "", nil
	}
	state, err := p.changedOnly(ctx, head, parent, strings.Split(path, "/"))
	if err != nil || state != providers.BranchOneChange {
		return state, "", err
	}
	return state, parent, nil
}

// changedOnly compares the trees of commit and parent along the path segs, from the root trees down.
func (p *Provider) changedOnly(ctx context.Context, commit, parent string, segs []string) (providers.BranchState, error) {
	shaA, shaB := commit, parent
	for i, seg := range segs {
		a, err := p.tree(ctx, opCheckBranch, shaA)
		if err != nil {
			return 0, err
		}
		b, err := p.tree(ctx, opCheckBranch, shaB)
		if err != nil {
			return 0, err
		}
		ea, eb, same, err := sameBeside(a, b, seg)
		switch {
		case err != nil:
			return 0, err
		case !same:
			return providers.BranchForeign, nil
		}
		if i == len(segs)-1 {
			if !isRegularFile(ea) || !isRegularFile(eb) || ea.Mode != eb.Mode {
				return providers.BranchForeign, nil
			}
			return providers.BranchOneChange, nil
		}
		if ea.Type != typeTree || ea.Mode != modeTree || eb.Type != typeTree || eb.Mode != modeTree {
			return providers.BranchForeign, nil
		}
		if ea.SHA == eb.SHA {
			// The same directory below: the file is unchanged too.
			return providers.BranchOneChange, nil
		}
		shaA, shaB = ea.SHA, eb.SHA
	}
	return providers.BranchForeign, nil
}

// sameBeside reports whether trees a and b hold the same entries beside the one named seg, which both must
// hold, and returns those two entries. A truncated tree, an entry with an invalid object ID or a name listed
// twice is an error.
func sameBeside(a, b treeJSON, seg string) (treeEntry, treeEntry, bool, error) {
	if a.Truncated || b.Truncated {
		return treeEntry{}, treeEntry{}, false, errors.New(opCheckBranch + ": directory listing is truncated")
	}
	index := func(t treeJSON) (map[string]treeEntry, error) {
		m := make(map[string]treeEntry, len(t.Tree))
		for _, e := range t.Tree {
			if _, dup := m[e.Path]; dup || !validSHA(e.SHA) {
				return nil, errors.New(opCheckBranch + ": GitHub returned an unexpected tree entry")
			}
			m[e.Path] = e
		}
		return m, nil
	}
	ma, err := index(a)
	if err != nil {
		return treeEntry{}, treeEntry{}, false, err
	}
	mb, err := index(b)
	if err != nil {
		return treeEntry{}, treeEntry{}, false, err
	}
	ea, okA := ma[seg]
	eb, okB := mb[seg]
	if !okA || !okB || len(ma) != len(mb) {
		return treeEntry{}, treeEntry{}, false, nil
	}
	for name, e := range ma {
		if name == seg {
			continue
		}
		o, ok := mb[name]
		if !ok || o.Mode != e.Mode || o.Type != e.Type || o.SHA != e.SHA {
			return treeEntry{}, treeEntry{}, false, nil
		}
	}
	return ea, eb, true, nil
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
	for attempt := 0; ; attempt++ {
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
			if err != nil && !providers.IsNotFound(err) {
				return err
			}
			if err == nil {
				if sha != fromCommit {
					return fmt.Errorf("%s: branch exists at another commit: %w", opCreateBranch, providers.ErrConflict)
				}
				return nil
			}
			// No branch to explain the refusal: a name or rule GitHub refuses, or a read from a replica that
			// does not see the branch yet. Ask again before calling it final.
			if err := p.awaitUnexplained(ctx, opCreateBranch, attempt); err != nil {
				return err
			}
			continue
		}
		return statusError(opCreateBranch, r.status)
	}
}

// awaitUnexplained waits before the next attempt of a create that GitHub answered with a 422 the state does
// not explain (yet). GitHub reads may come from a replica that lags the write: the branch of a lost create
// response, its pull request, or the commit just pushed may not be visible for a moment, so the refusal
// becomes final (ErrRejected) only after p.attempts attempts all saw the same. When the call's deadline
// leaves no time for the next attempt it returns a retryable error instead, never an early final one. The
// 422 body is not read.
func (p *Provider) awaitUnexplained(ctx context.Context, op string, attempt int) error {
	if attempt+1 >= p.attempts {
		return statusError(op, http.StatusUnprocessableEntity)
	}
	d := p.jitter(min(p.base<<attempt, p.maxDelay))
	if dl, ok := ctx.Deadline(); ok && p.now().Add(d).After(dl) {
		return &providers.RetryableError{Err: fmt.Errorf("%s: GitHub returned status 422, not explained yet", op)}
	}
	if err := p.sleep(ctx, d); err != nil {
		return ctxError(op, ctx)
	}
	return nil
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
// expectedHead, and returns the new commit. expectedHead must be the head of the target branch or an ancestor
// of it, so the commit is the branch's only one of its own. The file keeps its mode. The branch moves with an atomic
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
	// The branch carries no commit of its own yet: anything on it beside the target branch is someone else's,
	// and a commit on top would put it in the pull request unchecked (#524).
	if err := p.onTarget(ctx, opCommitFile, expectedHead); err != nil {
		return "", err
	}
	return p.commitOnto(ctx, branch, expectedHead, change)
}

// commitOnto makes one commit on top of expectedHead with the content of the existing regular file at
// change.Path replaced (same mode), and moves branch to it with updateRef. The caller has validated the input
// and checked the branch head.
func (p *Provider) commitOnto(ctx context.Context, branch, expectedHead string, change providers.FileChange) (string, error) {
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
	}{root, []treeItem{{Path: change.Path, Mode: entry.Mode, Type: typeBlob, SHA: blob}}})
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
		"sort": {"created"}, "direction": {"desc"}, paramPerPage: {"100"},
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
	for attempt := 0; ; attempt++ {
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
			// No open pull request explains the refusal, and the branch exists. A replica may not show the pull
			// request of a lost create response yet, or the commit just pushed ("No commits between"): ask again
			// before calling it final.
			if err := p.awaitUnexplained(ctx, opOpenCR, attempt); err != nil {
				return providers.ChangeRequest{}, err
			}
			continue
		}
		return providers.ChangeRequest{}, statusError(opOpenCR, r.status)
	}
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
	return p.pullFor(ctx, opCloseCR, number)
}

// pullFor reads pull request number for op and refuses one that is not ours.
func (p *Provider) pullFor(ctx context.Context, op string, number int64) (pullJSON, error) {
	r, err := p.do(ctx, call{op: op, method: http.MethodGet, path: "/pulls/" + strconv.FormatInt(number, 10)})
	if err != nil {
		return pullJSON{}, err
	}
	if !r.ok() {
		return pullJSON{}, statusError(op, r.status)
	}
	var pr pullJSON
	if err := decode(op, r, &pr); err != nil {
		return pullJSON{}, err
	}
	if pr.Number != number {
		return pullJSON{}, errors.New(op + ": GitHub returned an unexpected pull request")
	}
	if !p.ours(pr) {
		return pullJSON{}, errors.New(op + ": not a Shadewright pull request to the target branch")
	}
	return pr, nil
}

// paramPerPage is the REST query parameter that sets the page size.
const paramPerPage = "per_page"

// Comment pages CommentChangeRequest reads for its marker: commentsPerPage comments of at most 64 KiB each fit
// in maxJSONBody, and past maxCommentPages pages the comment is posted without looking further.
const (
	commentsPerPage = 20
	maxCommentPages = 10
)

// CommentChangeRequest adds one comment to an open pull request from a shadewright/ branch to the target
// branch, unless one of its first maxCommentPages pages of comments holds marker already. A closed or merged
// one is ErrConflict; other pull requests are refused.
func (p *Provider) CommentChangeRequest(ctx context.Context, number int64, marker, body string) error {
	switch {
	case number < 1:
		return errors.New(opCommentCR + ": number must be positive")
	case strings.TrimSpace(body) == "" || len(body) > maxMessageBytes || !utf8.ValidString(body):
		return errors.New(opCommentCR + ": comment is empty, too long or not UTF-8")
	case strings.TrimSpace(marker) == "" || strings.ContainsAny(marker, "\r\n") ||
		len(marker) > providers.MaxCommentMarkerBytes || !strings.Contains(body, marker):
		return errors.New(opCommentCR + ": marker is invalid or not in the comment")
	}
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()
	pr, err := p.pullFor(ctx, opCommentCR, number)
	if err != nil {
		return err
	}
	if pr.MergedAt != nil || pr.State != stateOpen {
		return fmt.Errorf("%s: pull request is not open: %w", opCommentCR, providers.ErrConflict)
	}
	switch found, err := p.hasComment(ctx, number, marker); {
	case err != nil:
		return err
	case found:
		return nil
	}
	r, err := p.do(ctx, call{
		op: opCommentCR, method: http.MethodPost, path: "/issues/" + strconv.FormatInt(number, 10) + "/comments",
		body: map[string]string{"body": body},
	})
	if err != nil {
		return err
	}
	if r.status != http.StatusCreated {
		return statusError(opCommentCR, r.status)
	}
	return nil
}

// hasComment reports whether a comment of pull request number holds marker, reading at most maxCommentPages
// pages.
func (p *Provider) hasComment(ctx context.Context, number int64, marker string) (bool, error) {
	for page := 1; page <= maxCommentPages; page++ {
		r, err := p.do(ctx, call{op: opCommentCR, method: http.MethodGet,
			path:  "/issues/" + strconv.FormatInt(number, 10) + "/comments",
			query: url.Values{paramPerPage: {strconv.Itoa(commentsPerPage)}, "page": {strconv.Itoa(page)}}})
		if err != nil {
			return false, err
		}
		if !r.ok() {
			return false, statusError(opCommentCR, r.status)
		}
		var comments []struct {
			Body string `json:"body"`
		}
		if err := decode(opCommentCR, r, &comments); err != nil {
			return false, err
		}
		for _, c := range comments {
			if strings.Contains(c.Body, marker) {
				return true, nil
			}
		}
		if len(comments) < commentsPerPage {
			return false, nil
		}
	}
	return false, nil
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
