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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/github/auth"
)

const (
	testRepoURL = "https://github.com/acme/fleet"
	testToken   = "ghs_SECRETINSTALLATIONTOKEN0123456789"
	repoPath    = "/repos/acme/fleet"
	branch      = "shadewright/req-1"
	branchRef   = "/git/ref/heads/shadewright/req-1"
	shaBase     = "1111111111111111111111111111111111111111"
	shaOld      = "2222222222222222222222222222222222222222"
	shaTree     = "4444444444444444444444444444444444444444"
	shaApps     = "5555555555555555555555555555555555555555"
	shaNewTree  = "6666666666666666666666666666666666666666"
	shaCommit   = "7777777777777777777777777777777777777777"
	shaDeploy   = "eb5c26739ea8767a00de3d6e1932c340ce323472"
	shaNewBlob  = "71ffd9f273f08d3bc7dde7ec46a5a103ed52feb6"
	shaRun      = "1a2485251c33a70432394c93fb89330ef214bfc9"
	// bodyMarker is in error fixtures; it must never reach an error.
	bodyMarker = "LEAKED-BODY-MARKER"
	target     = "main"
	owner      = "acme"
	other      = "release"
	mainRef    = "/git/ref/heads/main"
	hRetry     = "Retry-After"
	// graphQLPath stands for the GraphQL endpoint in steps.
	graphQLPath = "/graphql"
	pullsPath   = "/pulls"
	// Error fixtures in testdata.
	fxForbidden = "error_forbidden.json"
	fxSecondary = "error_secondary.json"
)

var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// step is one recorded exchange: the request expected and the response to give.
type step struct {
	method string
	path   string // below /repos/acme/fleet, without the query
	query  string // exact raw query when not empty
	check  func(t *testing.T, body map[string]any)
	status int
	header map[string]string
	file   string // fixture in testdata
	raw    string // literal body, when file is empty
	err    error  // a transport error instead of a response
	// ifNoneMatch is the If-None-Match header the request must carry; empty means none.
	ifNoneMatch string
}

// script is a RoundTripper that plays recorded exchanges in order. It fails the test on any request to
// another host or repository, on any request not in the script, and when steps are left over.
type script struct {
	t     *testing.T
	mu    sync.Mutex
	steps []step
	n     int
	// sent records each request that went out to the scripted host, as "METHOD path".
	sent []string
}

func (s *script) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.t
	t.Helper()
	if req.URL.Scheme != "https" || req.URL.Host != "api.github.com" {
		t.Errorf("request to %s://%s", req.URL.Scheme, req.URL.Host)
		return nil, errors.New("unexpected host")
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Errorf("Authorization header is %q", got)
	}
	path, ok := strings.CutPrefix(req.URL.EscapedPath(), repoPath)
	switch {
	case req.URL.EscapedPath() == graphQLPath:
		path = graphQLPath
	case !ok || (path != "" && !strings.HasPrefix(path, "/")):
		t.Errorf("request outside the repository: %s %s", req.Method, req.URL.EscapedPath())
		return nil, errors.New("unexpected repository")
	}
	s.sent = append(s.sent, req.Method+" "+path)
	if s.n >= len(s.steps) {
		t.Errorf("unexpected request %s %s", req.Method, path)
		return nil, errors.New("unexpected request")
	}
	st := s.steps[s.n]
	s.n++
	if req.Method != st.method || path != st.path {
		t.Errorf("request %d: got %s %s, want %s %s", s.n, req.Method, path, st.method, st.path)
	}
	if st.query != "" && req.URL.RawQuery != st.query {
		t.Errorf("request %d: query %q, want %q", s.n, req.URL.RawQuery, st.query)
	}
	if got := req.Header.Get("If-None-Match"); got != st.ifNoneMatch {
		t.Errorf("request %d: If-None-Match %q, want %q", s.n, got, st.ifNoneMatch)
	}
	if st.check != nil {
		var body map[string]any
		if req.Body != nil {
			data, _ := io.ReadAll(req.Body)
			if err := json.Unmarshal(data, &body); err != nil {
				t.Errorf("request %d: body is not a JSON object", s.n)
			}
		}
		st.check(t, body)
	}
	if st.err != nil {
		return nil, st.err
	}
	data := []byte(st.raw)
	if st.file != "" {
		var err error
		if data, err = os.ReadFile(filepath.Join("testdata", st.file)); err != nil {
			t.Fatal(err)
		}
	}
	h := http.Header{}
	for k, v := range st.header {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: st.status, Header: h, Body: io.NopCloser(bytes.NewReader(data)),
		ContentLength: -1, Request: req,
	}, nil
}

type harness struct {
	p      *Provider
	s      *script
	sleeps []time.Duration
}

func newHarness(t *testing.T, steps ...step) *harness {
	t.Helper()
	h := &harness{s: &script{t: t, steps: steps}}
	p, err := newProvider(Config{RepositoryURL: testRepoURL, TargetBranch: target}, testHooks{
		transport: h.s,
		token:     func(context.Context) (string, error) { return testToken, nil },
		sleep: func(_ context.Context, d time.Duration) error {
			h.sleeps = append(h.sleeps, d)
			return nil
		},
		jitter: func(d time.Duration) time.Duration { return d },
		now:    func() time.Time { return t0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	h.p = p
	t.Cleanup(func() {
		if h.s.n != len(h.s.steps) {
			t.Errorf("%d of %d recorded requests were made", h.s.n, len(h.s.steps))
		}
	})
	return h
}

func get(path, file string) step {
	return step{method: http.MethodGet, path: path, status: http.StatusOK, file: file}
}

func status(method, path string, code int, file string) step {
	return step{method: method, path: path, status: code, file: file}
}

// assertSafe checks that no error in the chain names the token, the repository, a branch, the host or
// response text, and that no HTTP client error is reachable.
func assertSafe(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		for _, s := range []string{testToken, owner, "fleet", "req-", "/git/", "api.github.com", bodyMarker, "https://"} {
			for _, text := range []string{e.Error(), fmt.Sprintf("%+v", e), fmt.Sprintf("%#v", e)} {
				if strings.Contains(text, s) {
					t.Errorf("error leaks %q: %s", s, text)
				}
			}
		}
	}
	if _, ok := errors.AsType[*url.Error](err); ok {
		t.Error("an HTTP client error is reachable")
	}
	var ne net.Error
	if errors.As(err, &ne) && !errors.Is(err, context.DeadlineExceeded) {
		t.Error("a network error is reachable")
	}
}

var ctx = context.Background()

func TestNewValidates(t *testing.T) {
	token := func(context.Context) (string, error) { return testToken, nil }
	for name, cfg := range map[string]Config{
		"bad URL":            {RepositoryURL: "https://user:ghp_SECRET@github.com/acme/fleet", TargetBranch: target},
		"other host":         {RepositoryURL: "https://gitlab.com/acme/fleet", TargetBranch: target},
		"empty target":       {RepositoryURL: testRepoURL},
		"target is ours":     {RepositoryURL: testRepoURL, TargetBranch: "shadewright/x"},
		"target with dotdot": {RepositoryURL: testRepoURL, TargetBranch: "a..b"},
		"target lock":        {RepositoryURL: testRepoURL, TargetBranch: "main.lock"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newProvider(cfg, testHooks{token: token})
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "acme") {
				t.Errorf("error repeats the input: %v", err)
			}
		})
	}
	if _, err := New(Config{RepositoryURL: testRepoURL, TargetBranch: target}); err == nil {
		t.Error("New accepted a nil token source")
	}
}

type tokenSource struct{ repo auth.Repository }

func (s tokenSource) Token(context.Context) (auth.Token, error) { return auth.Token{}, nil }
func (s tokenSource) Repository() auth.Repository               { return s.repo }

func TestNewBindsTheTokenSourceRepository(t *testing.T) {
	cfg := Config{RepositoryURL: testRepoURL, TargetBranch: target}
	cfg.Tokens = tokenSource{auth.Repository{Owner: owner, Name: "other"}}
	if _, err := New(cfg); err == nil {
		t.Error("a token source for another repository was accepted")
	}
	cfg.Tokens = tokenSource{auth.Repository{Owner: "Acme", Name: "Fleet"}}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The zero token is refused before any request is sent (the default transport is never used).
	if _, err := p.BranchHead(ctx, target); err == nil {
		t.Error("an empty token was used")
	}
}

func TestCapabilities(t *testing.T) {
	h := newHarness(t)
	if got := h.p.Capabilities(); got.AutoMerge || !got.RequiredReviews {
		t.Errorf("Capabilities() = %+v", got)
	}
	if err := h.p.EnableAutoMerge(ctx, 12, shaCommit); err == nil {
		t.Error("EnableAutoMerge succeeded without support")
	}
}

func TestBranchHead(t *testing.T) {
	h := newHarness(t, get(mainRef, "ref_main.json"), get(branchRef, "ref_branch.json"),
		status(http.MethodGet, "/git/ref/heads/shadewright/req-2", http.StatusNotFound, fxForbidden))
	if sha, err := h.p.BranchHead(ctx, target); err != nil || sha != shaBase {
		t.Fatalf("BranchHead(main) = %q, %v", sha, err)
	}
	if sha, err := h.p.BranchHead(ctx, branch); err != nil || sha != shaBase {
		t.Fatalf("BranchHead(branch) = %q, %v", sha, err)
	}
	_, err := h.p.BranchHead(ctx, "shadewright/req-2")
	if !providers.IsNotFound(err) {
		t.Fatalf("missing branch: %v", err)
	}
	assertSafe(t, err)
}

func TestBranchHeadRefusesOtherBranches(t *testing.T) {
	h := newHarness(t) // no request may be made
	for _, b := range []string{other, "shadewright/a/b", "shadewright/..x", "shadewright/", "main/../x", ""} {
		if _, err := h.p.BranchHead(ctx, b); err == nil {
			t.Errorf("BranchHead(%q) was allowed", b)
		}
	}
}

func TestBranchHeadRefusesUnexpectedReference(t *testing.T) {
	// The exact-match endpoint must answer for the branch asked; anything else is refused.
	h := newHarness(t, get(mainRef, "ref_branch.json"))
	if _, err := h.p.BranchHead(ctx, target); err == nil {
		t.Fatal("a reference for another branch was accepted")
	}
}

func readSteps() []step {
	return []step{get("/git/trees/"+shaBase, "tree_root.json"), get("/git/trees/"+shaApps, "tree_apps.json")}
}

func TestReadFile(t *testing.T) {
	blob := get("/git/blobs/"+shaDeploy, "")
	blob.raw = "replicas: 2\n"
	h := newHarness(t, append(readSteps(), blob)...)
	f, err := h.p.ReadFile(ctx, shaBase, "apps/deploy.yaml", providers.MaxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	if f.Path != "apps/deploy.yaml" || f.Commit != shaBase || string(f.Data) != "replicas: 2\n" {
		t.Errorf("ReadFile() = %+v", f)
	}
}

func TestReadFileNotRegular(t *testing.T) {
	for _, path := range []string{"link.yaml", "vendor", "apps", "missing.yaml", "README.md/x", "vendor/x"} {
		t.Run(path, func(t *testing.T) {
			h := newHarness(t, get("/git/trees/"+shaBase, "tree_root.json"))
			_, err := h.p.ReadFile(ctx, shaBase, path, providers.MaxFileBytes)
			// Read from the commit's tree, so the answer holds for good: a cache may keep it.
			if !providers.IsPathNotInTree(err) || !providers.IsNotFound(err) {
				t.Fatalf("ReadFile(%s) = %v, want a path not in the tree", path, err)
			}
			assertSafe(t, err)
		})
	}
}

func TestReadFileMissingCommit(t *testing.T) {
	h := newHarness(t, status(http.MethodGet, "/git/trees/"+shaOld, http.StatusNotFound, fxForbidden))
	_, err := h.p.ReadFile(ctx, shaOld, "apps/deploy.yaml", providers.MaxFileBytes)
	if !providers.IsNotFound(err) || providers.IsPathNotInTree(err) {
		t.Fatalf("got %v, want a plain not found", err)
	}
	assertSafe(t, err)
}

// A 404 or 422 from GitHub on a tree or blob read is a not found, but not a path known to be absent from the
// tree: the host may have failed, or not have the commit yet, so a cache must not keep it.
func TestReadFileHostNotFoundIsNotAPathNotInTree(t *testing.T) {
	blob := status(http.MethodGet, "/git/blobs/"+shaDeploy, http.StatusNotFound, fxForbidden)
	for name, steps := range map[string][]step{
		"root tree 422": {status(http.MethodGet, "/git/trees/"+shaBase, http.StatusUnprocessableEntity, fxForbidden)},
		"subtree 404": {get("/git/trees/"+shaBase, "tree_root.json"),
			status(http.MethodGet, "/git/trees/"+shaApps, http.StatusNotFound, fxForbidden)},
		"subtree 410": {get("/git/trees/"+shaBase, "tree_root.json"),
			status(http.MethodGet, "/git/trees/"+shaApps, http.StatusGone, fxForbidden)},
		"blob 404": append(readSteps(), blob),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, steps...)
			_, err := h.p.ReadFile(ctx, shaBase, "apps/deploy.yaml", providers.MaxFileBytes)
			if !providers.IsNotFound(err) || providers.IsPathNotInTree(err) {
				t.Fatalf("ReadFile = %v, want a plain not found", err)
			}
			assertSafe(t, err)
		})
	}
}

func TestReadFileTooLargeBeforeDownload(t *testing.T) {
	h := newHarness(t, readSteps()...) // no blob request is recorded: the size check must come first
	_, err := h.p.ReadFile(ctx, shaBase, "apps/big.yaml", providers.MaxFileBytes)
	if !providers.IsTooLarge(err) {
		t.Fatalf("got %v", err)
	}
}

// A caller's limit below MaxFileBytes is checked against the tree entry's size before the blob is requested,
// and a limit out of range is clamped, never widened (#298).
func TestReadFileLimitBeforeDownload(t *testing.T) {
	// apps/deploy.yaml is 12 bytes in the tree; apps/big.yaml is over MaxFileBytes.
	for name, tc := range map[string]struct {
		path  string
		limit int
	}{
		"one byte under the size": {deployPath, 11},
		"zero":                    {deployPath, 0},
		"negative":                {deployPath, -1},
		"over MaxFileBytes":       {"apps/big.yaml", 4 * providers.MaxFileBytes},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, readSteps()...) // no blob request is recorded
			if _, err := h.p.ReadFile(ctx, shaBase, tc.path, tc.limit); !providers.IsTooLarge(err) {
				t.Fatalf("ReadFile(limit %d) = %v, want ErrTooLarge", tc.limit, err)
			}
		})
	}
	blob := get("/git/blobs/"+shaDeploy, "")
	blob.raw = deployYAML
	h := newHarness(t, append(readSteps(), blob)...)
	if f, err := h.p.ReadFile(ctx, shaBase, deployPath, 12); err != nil || string(f.Data) != deployYAML {
		t.Fatalf("ReadFile(limit = size) = %q, %v; want the file", f.Data, err)
	}
}

// A stream larger than the caller's limit is cut after about limit+1 bytes, not MaxFileBytes+1 (#298).
func TestReadFileStopsReadingStreamAtLimit(t *testing.T) {
	const limit = 100
	// The tree claims 12 bytes, the stream is larger than the limit: it is not the blob the tree names.
	big := &countingReader{left: 4 * providers.MaxFileBytes}
	h := newHarness(t, readSteps()...)
	h.s.steps = append(h.s.steps, get("/git/blobs/"+shaDeploy, ""))
	h.p.client.Transport = bodyOverride{h.s, big}
	_, err := h.p.ReadFile(ctx, shaBase, deployPath, limit)
	if err == nil || providers.IsTooLarge(err) || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("got %v, want an integrity error: the tree entry said the file fits", err)
	}
	if big.read > limit+1+32<<10 {
		t.Errorf("read %d bytes, want at most about limit+1", big.read)
	}
}

func TestReadFileStopsReadingLargeStream(t *testing.T) {
	// The tree claims 12 bytes, the stream is larger than MaxFileBytes.
	big := &countingReader{left: 4 * providers.MaxFileBytes}
	h := newHarness(t, readSteps()...)
	h.s.steps = append(h.s.steps, get("/git/blobs/"+shaDeploy, ""))
	h.p.client.Transport = bodyOverride{h.s, big}
	_, err := h.p.ReadFile(ctx, shaBase, "apps/deploy.yaml", providers.MaxFileBytes)
	if err == nil || providers.IsTooLarge(err) || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("got %v, want an integrity error: the tree entry said the file fits", err)
	}
	if big.read > providers.MaxFileBytes+1+32<<10 {
		t.Errorf("read %d bytes, want at most about MaxFileBytes+1", big.read)
	}
}

type countingReader struct{ left, read int }

func (c *countingReader) Read(p []byte) (int, error) {
	if c.left == 0 {
		return 0, io.EOF
	}
	n := min(len(p), c.left)
	for i := range p[:n] {
		p[i] = 'x'
	}
	c.left -= n
	c.read += n
	return n, nil
}

// bodyOverride replaces the body of blob responses.
type bodyOverride struct {
	rt   http.RoundTripper
	body io.Reader
}

func (b bodyOverride) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := b.rt.RoundTrip(req)
	if err == nil && strings.Contains(req.URL.Path, "/git/blobs/") {
		resp.Body = io.NopCloser(b.body)
	}
	return resp, err
}

func TestReadFileChecksContentHash(t *testing.T) {
	blob := get("/git/blobs/"+shaDeploy, "")
	blob.raw = "replicas: 9\n"
	h := newHarness(t, append(readSteps(), blob)...)
	if _, err := h.p.ReadFile(ctx, shaBase, "apps/deploy.yaml", providers.MaxFileBytes); err == nil {
		t.Fatal("content that does not match its object ID was accepted")
	}
}

// The root holds a symbolic link (link.yaml) and a submodule (vendor): both are skipped, never followed (#122).
func TestListDir(t *testing.T) {
	h := newHarness(t, get("/git/trees/"+shaBase, "tree_root.json"))
	got, err := h.p.ListDir(ctx, shaBase, ".")
	want := []providers.DirEntry{{Name: "README.md", Type: providers.DirEntryFile}, {Name: "apps", Type: providers.DirEntryDir}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("ListDir(.) = %v, %v, want %v", got, err, want)
	}

	h = newHarness(t, readSteps()...)
	got, err = h.p.ListDir(ctx, shaBase, "apps")
	want = []providers.DirEntry{{Name: "big.yaml", Type: providers.DirEntryFile},
		{Name: "deploy.yaml", Type: providers.DirEntryFile}, {Name: "run.sh", Type: providers.DirEntryFile}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("ListDir(apps) = %v, %v, want %v", got, err, want)
	}
}

// A path that is not a directory in the tree is ErrPathNotInTree, as for ReadFile; a missing commit or a
// failing host is a plain not found, which a cache must not keep.
func TestListDirNotADirectory(t *testing.T) {
	for _, path := range []string{"README.md", "link.yaml", "vendor", "missing", deployPath, "vendor/x"} {
		t.Run(path, func(t *testing.T) {
			steps := []step{get("/git/trees/"+shaBase, "tree_root.json")}
			if strings.HasPrefix(path, "apps/") {
				steps = readSteps()
			}
			h := newHarness(t, steps...)
			_, err := h.p.ListDir(ctx, shaBase, path)
			if !providers.IsPathNotInTree(err) {
				t.Fatalf("ListDir(%s) = %v, want a path not in the tree", path, err)
			}
			assertSafe(t, err)
		})
	}
	h := newHarness(t, status(http.MethodGet, "/git/trees/"+shaOld, http.StatusNotFound, fxForbidden))
	if _, err := h.p.ListDir(ctx, shaOld, "apps"); !providers.IsNotFound(err) || providers.IsPathNotInTree(err) {
		t.Fatalf("missing commit: %v, want a plain not found", err)
	}
}

// A walk lists each directory with one request: a listing below one already read starts from its tree, not
// from the root again, and a later read of a file in it reads only that tree and the blob.
func TestListDirWalkCostsOneRequestPerDirectory(t *testing.T) {
	blob := get("/git/blobs/"+shaDeploy, "")
	blob.raw = deployYAML
	apps := get("/git/trees/"+shaApps, "tree_apps.json")
	h := newHarness(t, get("/git/trees/"+shaBase, "tree_root.json"), apps, apps, blob)
	if _, err := h.p.ListDir(ctx, shaBase, "."); err != nil {
		t.Fatal(err)
	}
	if _, err := h.p.ListDir(ctx, shaBase, "apps"); err != nil {
		t.Fatal(err)
	}
	if f, err := h.p.ReadFile(ctx, shaBase, deployPath, providers.MaxFileBytes); err != nil || string(f.Data) != deployYAML {
		t.Fatalf("ReadFile = %q, %v", f.Data, err)
	}
	if len(h.s.sent) != 4 {
		t.Errorf("sent %d requests, want 4: %v", len(h.s.sent), h.s.sent)
	}
}

// Names are returned as Git stores them, a hidden one included; an entry no tree can hold, or of an unknown
// type, fails the listing with ErrInvalidEntry, which callers do not retry.
func TestListDirNames(t *testing.T) {
	tree := func(body string) step { s := get("/git/trees/"+shaApps, ""); s.raw = body; return s }
	root := get("/git/trees/"+shaBase, "tree_root.json")
	h := newHarness(t, root, tree(treeBody(blobEntry(".hidden.yaml"), blobEntry("a:b"))))
	got, err := h.p.ListDir(ctx, shaBase, "apps")
	if err != nil || len(got) != 2 || got[0].Name != ".hidden.yaml" {
		t.Fatalf("ListDir = %v, %v, want both names", got, err)
	}
	for name, e := range map[string]string{
		"slash":        blobEntry("a/b.yaml"),
		"dot dot":      blobEntry(".."),
		"empty":        blobEntry(""),
		"unknown type": `{"path":"x","mode":"100644","type":"tag","sha":"` + shaDeploy + `"}`,
	} {
		h = newHarness(t, root, tree(treeBody(e)))
		if _, err := h.p.ListDir(ctx, shaBase, "apps"); !providers.IsInvalidEntry(err) {
			t.Errorf("%s: ListDir = %v, want ErrInvalidEntry", name, err)
		}
	}
}

// treeBody returns a tree answer holding the given entries.
func treeBody(entries ...string) string {
	return `{"sha":"` + shaApps + `","tree":[` + strings.Join(entries, ",") + `],"truncated":false}`
}

func blobEntry(name string) string {
	return `{"path":"` + name + `","mode":"100644","type":"blob","sha":"` + shaDeploy + `","size":1}`
}

func TestListDirBounds(t *testing.T) {
	wide := make([]string, providers.MaxDirEntries+1)
	for i := range wide {
		wide[i] = blobEntry(fmt.Sprintf("f%d.yaml", i))
	}
	tree := func(body string) step { s := get("/git/trees/"+shaApps, ""); s.raw = body; return s }
	root := get("/git/trees/"+shaBase, "tree_root.json")

	h := newHarness(t, root, tree(treeBody(wide...)))
	if _, err := h.p.ListDir(ctx, shaBase, "apps"); !providers.IsTooLarge(err) {
		t.Errorf("ListDir of %d entries = %v, want ErrTooLarge", len(wide), err)
	}
	h = newHarness(t, root, tree(treeBody(wide[:providers.MaxDirEntries]...)))
	if got, err := h.p.ListDir(ctx, shaBase, "apps"); err != nil || len(got) != providers.MaxDirEntries {
		t.Errorf("ListDir of MaxDirEntries entries = %d entries, %v", len(got), err)
	}

	truncated := `{"sha":"` + shaApps + `","tree":[` + blobEntry("a.yaml") + `],"truncated":true}`
	h = newHarness(t, root, tree(truncated))
	if _, err := h.p.ListDir(ctx, shaBase, "apps"); err == nil || providers.IsNotFound(err) || providers.IsTooLarge(err) {
		t.Errorf("truncated listing = %v, want an error that is neither not found nor too large", err)
	}

	for name, e := range map[string]string{
		"slash":        blobEntry("a/b.yaml"),
		"dot dot":      blobEntry(".."),
		"unknown type": `{"path":"x","mode":"100644","type":"tag","sha":"` + shaDeploy + `"}`,
		"tree mode":    `{"path":"x","mode":"100644","type":"tree","sha":"` + shaDeploy + `"}`,
	} {
		h = newHarness(t, root, tree(treeBody(blobEntry("a.yaml"), e)))
		if _, err := h.p.ListDir(ctx, shaBase, "apps"); err == nil {
			t.Errorf("%s: an unexpected tree entry was listed", name)
		}
	}

	// An answer over the JSON bound is refused without reading it all.
	big := &countingReader{left: 4 * maxJSONBody}
	h = newHarness(t, root, get("/git/trees/"+shaApps, ""))
	h.p.client.Transport = treeOverride{h.s, big}
	if _, err := h.p.ListDir(ctx, shaBase, "apps"); err == nil {
		t.Error("an answer over the JSON bound was accepted")
	}
	if big.read > maxJSONBody+1+32<<10 {
		t.Errorf("read %d bytes, want at most about the JSON bound", big.read)
	}
}

// treeOverride replaces the body of the apps tree answer.
type treeOverride struct {
	rt   http.RoundTripper
	body io.Reader
}

func (o treeOverride) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := o.rt.RoundTrip(req)
	if err == nil && strings.HasSuffix(req.URL.Path, "/git/trees/"+shaApps) {
		resp.Body = io.NopCloser(o.body)
	}
	return resp, err
}

func TestListDirRefusesBadInput(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"", "/apps", "apps/", "apps//x", "../apps", "./apps", ".git", "a\x00b"} {
		if _, err := h.p.ListDir(ctx, shaBase, path); err == nil {
			t.Errorf("ListDir(%q) was allowed", path)
		}
	}
	for _, sha := range []string{"", target, "HEAD", shaBase[:39]} {
		if _, err := h.p.ListDir(ctx, sha, "."); err == nil {
			t.Errorf("ListDir at %q was allowed", sha)
		}
	}
}

func TestReadFileRefusesBadInput(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"", "/apps/x", "apps/", "apps//x", "../x", "apps/../x", "./x", ".git/config",
		"a/.GIT/x", "a\x00b", "a\nb", "a\\b", strings.Repeat("a/", 40) + "x"} {
		if _, err := h.p.ReadFile(ctx, shaBase, path, providers.MaxFileBytes); err == nil {
			t.Errorf("ReadFile(%q) was allowed", path)
		}
	}
	for _, sha := range []string{"", target, "HEAD", strings.ToUpper(shaDeploy), shaBase[:39], "../" + shaBase[3:]} {
		if _, err := h.p.ReadFile(ctx, sha, "apps/deploy.yaml", providers.MaxFileBytes); err == nil {
			t.Errorf("ReadFile at %q was allowed", sha)
		}
	}
}

func refBody(want string) func(*testing.T, map[string]any) {
	return func(t *testing.T, b map[string]any) {
		if b["ref"] != "refs/heads/"+branch || b["sha"] != want {
			t.Errorf("create ref body = %v", b)
		}
	}
}

func TestCreateBranch(t *testing.T) {
	create := status(http.MethodPost, "/git/refs", http.StatusCreated, "ref_branch.json")
	create.check = refBody(shaBase)
	h := newHarness(t, get(mainRef, "ref_main.json"), create)
	if err := h.p.CreateBranch(ctx, branch, shaBase); err != nil {
		t.Fatal(err)
	}
}

func TestCreateBranchFromAncestor(t *testing.T) {
	cmp := get("/compare/"+shaBase+"..."+shaOld, "compare_behind.json")
	cmp.query = "per_page=1"
	create := status(http.MethodPost, "/git/refs", http.StatusCreated, "")
	create.raw = strings.ReplaceAll(mustRead(t, "ref_branch.json"), shaBase, shaOld)
	h := newHarness(t, get(mainRef, "ref_main.json"), cmp, create)
	if err := h.p.CreateBranch(ctx, branch, shaOld); err != nil {
		t.Fatal(err)
	}
}

func TestCreateBranchExistingIsSuccess(t *testing.T) {
	h := newHarness(t, get(mainRef, "ref_main.json"),
		status(http.MethodPost, "/git/refs", http.StatusUnprocessableEntity, "error_ref_exists.json"),
		get(branchRef, "ref_branch.json"))
	if err := h.p.CreateBranch(ctx, branch, shaBase); err != nil {
		t.Fatal(err)
	}
}

func TestCreateBranchExistingElsewhereIsConflict(t *testing.T) {
	h := newHarness(t, get(mainRef, "ref_main.json"),
		status(http.MethodPost, "/git/refs", http.StatusUnprocessableEntity, "error_ref_exists.json"),
		get(branchRef, "ref_branch_moved.json"))
	err := h.p.CreateBranch(ctx, branch, shaBase)
	if !providers.IsConflict(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
}

func TestCreateBranchRefusesCommitOffTarget(t *testing.T) {
	// A commit of a fork or another branch is reachable in the repository network; it must not become a base.
	h := newHarness(t, get(mainRef, "ref_main.json"),
		get("/compare/"+shaBase+"..."+shaOld, "compare_diverged.json"))
	err := h.p.CreateBranch(ctx, branch, shaOld)
	if err == nil || providers.IsConflict(err) || providers.IsRetryable(err) {
		t.Fatalf("got %v", err)
	}
}

func TestCreateBranchRefusesOtherBranches(t *testing.T) {
	h := newHarness(t)
	for _, b := range []string{target, other, "shadewright", "shadewright/a/b", "feature/x", "refs/heads/x"} {
		if err := h.p.CreateBranch(ctx, b, shaBase); err == nil {
			t.Errorf("CreateBranch(%q) was allowed", b)
		}
	}
}

// commitHead is what CommitFile reads before the file: the branch at shaBase, and the target branch at
// shaBase, so the head the commit is made on is on the target branch.
func commitHead() []step {
	return []step{get(branchRef, "ref_branch.json"), get(mainRef, "ref_main.json")}
}

func commitSteps() []step {
	blob := status(http.MethodPost, "/git/blobs", http.StatusCreated, "blob_created.json")
	blob.check = func(t *testing.T, b map[string]any) {
		if b["encoding"] != "base64" || b["content"] != "cmVwbGljYXM6IDMK" {
			t.Errorf("blob body = %v", b)
		}
	}
	tree := status(http.MethodPost, "/git/trees", http.StatusCreated, "tree_created.json")
	tree.check = func(t *testing.T, b map[string]any) {
		want := `{"base_tree":"` + shaTree + `","tree":[{"mode":"100644","path":"apps/deploy.yaml","sha":"` +
			shaNewBlob + `","type":"blob"}]}`
		if got, _ := json.Marshal(b); string(got) != want {
			t.Errorf("tree body = %s", got)
		}
	}
	commit := status(http.MethodPost, "/git/commits", http.StatusCreated, "commit_created.json")
	commit.check = func(t *testing.T, b map[string]any) {
		want := `{"message":"fix: raise replicas","parents":["` + shaBase + `"],"tree":"` + shaNewTree + `"}`
		if got, _ := json.Marshal(b); string(got) != want {
			t.Errorf("commit body = %s", got)
		}
	}
	return append(append(commitHead(), readSteps()...), blob, tree, commit)
}

var change = providers.FileChange{Path: "apps/deploy.yaml", Content: []byte("replicas: 3\n"), Message: "fix: raise replicas"}

// casUpdate checks the GraphQL updateRefs request: a compare-and-swap from shaBase to the new commit,
// without force, on this repository.
func casUpdate(t *testing.T, b map[string]any) {
	vars, _ := b["variables"].(map[string]any)
	got, _ := json.Marshal(vars)
	want := `{"repo":"R_kgDOAAAAAQ","updates":[{"afterOid":"` + shaCommit + `","beforeOid":"` + shaBase +
		`","force":false,"name":"refs/heads/shadewright/req-1"}]}`
	if string(got) != want {
		t.Errorf("updateRefs variables = %s, want %s", got, want)
	}
	if q, _ := b["query"].(string); !strings.Contains(q, "updateRefs(") {
		t.Errorf("query = %q", q)
	}
}

func updateSteps(file string) []step {
	update := status(http.MethodPost, graphQLPath, http.StatusOK, file)
	update.check = casUpdate
	return []step{get("", "repo.json"), update}
}

func TestCommitFile(t *testing.T) {
	h := newHarness(t, append(commitSteps(), updateSteps("graphql_update_refs.json")...)...)
	sha, err := h.p.CommitFile(ctx, branch, shaBase, change)
	if err != nil || sha != shaCommit {
		t.Fatalf("CommitFile() = %q, %v", sha, err)
	}
}

func TestCommitFileKeepsMode(t *testing.T) {
	h := newHarness(t, append(commitHead(), readSteps()...)...)
	h.s.steps = append(h.s.steps, status(http.MethodPost, "/git/blobs", http.StatusCreated, ""))
	h.s.steps[len(h.s.steps)-1].raw = `{"sha":"` + blobSHA([]byte("#!/bin/sh\nexit 1\n")) + `"}`
	tree := status(http.MethodPost, "/git/trees", http.StatusInternalServerError, "")
	tree.check = func(t *testing.T, b map[string]any) {
		if items := b["tree"].([]any); items[0].(map[string]any)["mode"] != modeExecutable {
			t.Errorf("mode = %v", items[0])
		}
	}
	h.s.steps = append(h.s.steps, tree)
	h.p.attempts = 1
	_, err := h.p.CommitFile(ctx, branch, shaBase,
		providers.FileChange{Path: "apps/run.sh", Content: []byte("#!/bin/sh\nexit 1\n"), Message: "m"})
	if !providers.IsRetryable(err) {
		t.Fatalf("got %v", err)
	}
}

func TestCommitFileHeadMovedIsConflict(t *testing.T) {
	h := newHarness(t, get(branchRef, "ref_branch_moved.json")) // nothing is written
	_, err := h.p.CommitFile(ctx, branch, shaBase, change)
	if !providers.IsConflict(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
}

func TestCommitFileRaceIsConflictNeverForce(t *testing.T) {
	// Someone pushes on top of the head between the check and the update: the swap is refused.
	steps := append(commitSteps(), updateSteps("graphql_update_refs_stale.json")...)
	h := newHarness(t, append(steps, get(branchRef, "ref_branch_moved.json"))...)
	_, err := h.p.CommitFile(ctx, branch, shaBase, change)
	if !providers.IsConflict(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
}

func TestCommitFileRewindIsConflict(t *testing.T) {
	// The branch is rewound to an ancestor of the expected head after it was read. A fast-forward-only
	// update would succeed and bring back the removed commits; the compare-and-swap on beforeOid is refused.
	rewound := strings.ReplaceAll(mustRead(t, "ref_branch.json"), shaBase, shaOld)
	steps := append(commitSteps(), updateSteps("graphql_update_refs_stale.json")...)
	h := newHarness(t, append(steps, step{method: http.MethodGet, path: branchRef, status: http.StatusOK, raw: rewound})...)
	_, err := h.p.CommitFile(ctx, branch, shaBase, change)
	if !providers.IsConflict(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
}

func TestCommitFileRepeatedUpdateIsSuccess(t *testing.T) {
	// The swap was applied but its response was lost; the repeat is refused, and the branch is at our commit.
	steps := append(commitSteps(), updateSteps("graphql_update_refs_stale.json")...)
	h := newHarness(t, append(steps, get(branchRef, "ref_branch_committed.json"))...)
	sha, err := h.p.CommitFile(ctx, branch, shaBase, change)
	if err != nil || sha != shaCommit {
		t.Fatalf("CommitFile() = %q, %v", sha, err)
	}
}

func TestCommitFileRefusedUpdateAtExpectedHead(t *testing.T) {
	// Refused, yet the branch has not moved: neither success nor a conflict.
	steps := append(commitSteps(), updateSteps("graphql_update_refs_stale.json")...)
	h := newHarness(t, append(steps, get(branchRef, "ref_branch.json"))...)
	_, err := h.p.CommitFile(ctx, branch, shaBase, change)
	if err == nil || providers.IsConflict(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
}

func TestCommitFileGraphQLRateLimit(t *testing.T) {
	h := newHarness(t, append(commitSteps(), updateSteps("graphql_rate_limited.json")...)...)
	_, err := h.p.CommitFile(ctx, branch, shaBase, change)
	if d, ok := providers.RetryAfter(err); !ok || d != time.Minute {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
}

const (
	hdrRemaining = "X-RateLimit-Remaining"
	hdrReset     = "X-RateLimit-Reset"
)

func TestCommitFileGraphQLRateLimitUsesHeaders(t *testing.T) {
	cases := map[string]struct {
		header map[string]string
		want   time.Duration
	}{
		"retry-after": {map[string]string{"Retry-After": "30"}, 30 * time.Second},
		"quota reset": {map[string]string{
			hdrRemaining: "0", hdrReset: strconv.FormatInt(t0.Add(90*time.Second).Unix(), 10),
		}, 90 * time.Second},
		"reset in the past": {map[string]string{
			hdrRemaining: "0", hdrReset: strconv.FormatInt(t0.Add(-time.Minute).Unix(), 10),
		}, time.Minute},
		"nonzero quota ignores reset": {map[string]string{
			hdrRemaining: "10", hdrReset: strconv.FormatInt(t0.Add(90*time.Second).Unix(), 10),
		}, time.Minute},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			steps := updateSteps("graphql_rate_limited.json")
			steps[1].header = c.header
			h := newHarness(t, append(commitSteps(), steps...)...)
			_, err := h.p.CommitFile(ctx, branch, shaBase, change)
			if d, ok := providers.RetryAfter(err); !ok || d != c.want {
				t.Fatalf("got %v, want a delay of %v", err, c.want)
			}
			assertSafe(t, err)
		})
	}
}

func TestCommitFileRefusesOtherRepository(t *testing.T) {
	other := step{method: http.MethodGet, path: "", status: http.StatusOK,
		raw: strings.ReplaceAll(mustRead(t, "repo.json"), "acme/fleet", "acme/other")}
	h := newHarness(t, append(commitSteps(), other)...)
	if _, err := h.p.CommitFile(ctx, branch, shaBase, change); err == nil {
		t.Fatal("a node ID of another repository was used")
	}
}

func TestCommitFileMissingFile(t *testing.T) {
	h := newHarness(t, append(commitHead(), readSteps()...)...)
	_, err := h.p.CommitFile(ctx, branch, shaBase, providers.FileChange{Path: "apps/new.yaml", Content: []byte("x"), Message: "m"})
	if !providers.IsNotFound(err) {
		t.Fatalf("got %v", err)
	}
}

func TestCommitFileRefusesHeadOffTarget(t *testing.T) {
	// The branch carries a commit someone else pushed (#524): nothing is written on top of it.
	branchAtOld := step{method: http.MethodGet, path: branchRef, status: http.StatusOK,
		raw: strings.ReplaceAll(mustRead(t, "ref_branch.json"), shaBase, shaOld)}
	cmp := get("/compare/"+shaBase+"..."+shaOld, "compare_diverged.json")
	cmp.query = perPageOne
	h := newHarness(t, branchAtOld, get(mainRef, "ref_main.json"), cmp)
	_, err := h.p.CommitFile(ctx, branch, shaOld, change)
	if err == nil || providers.IsConflict(err) || providers.IsRetryable(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
}

// perPageOne is the query of the compare request that checks a commit is on the target branch.
const perPageOne = "per_page=1"

// raw is a step that answers GET path with 200 and body.
func raw(path, body string) step {
	return step{method: http.MethodGet, path: path, status: http.StatusOK, raw: body}
}

// commitBody is a Git commit of the REST API: shaCommit with parents.
func commitBody(parents ...string) string {
	ps := make([]string, 0, len(parents))
	for _, p := range parents {
		ps = append(ps, `{"sha":"`+p+`"}`)
	}
	return `{"sha":"` + shaCommit + `","tree":{"sha":"` + shaNewTree + `"},"parents":[` + strings.Join(ps, ",") + `]}`
}

func TestCheckBranch(t *testing.T) {
	root, apps := mustRead(t, "tree_root.json"), mustRead(t, "tree_apps.json")
	// The commit changes apps/deploy.yaml only: a new apps tree and a new blob.
	newRoot := strings.ReplaceAll(root, shaApps, shaNewTree)
	newApps := strings.ReplaceAll(apps, shaDeploy, shaNewBlob)
	ahead := get("/compare/"+shaBase+"..."+shaCommit, "")
	ahead.raw, ahead.query = `{"status":"ahead","ahead_by":1,"behind_by":0,"total_commits":1,"commits":[],"files":[]}`, perPageOne
	// head reads shaCommit, which is not on the target branch, and its one parent, the target head.
	head := func(commit string) []step {
		return []step{get(mainRef, "ref_main.json"), ahead, raw("/git/commits/"+shaCommit, commit), get(mainRef, "ref_main.json")}
	}
	trees := func(rootA, appsA string) []step {
		return []step{raw("/git/trees/"+shaCommit, rootA), raw("/git/trees/"+shaBase, root),
			raw("/git/trees/"+shaNewTree, appsA), raw("/git/trees/"+shaApps, apps)}
	}
	behind := get("/compare/"+shaBase+"..."+shaOld, "compare_behind.json")
	behind.query = perPageOne
	diverged := get("/compare/"+shaBase+"..."+shaOld, "compare_diverged.json")
	diverged.query = perPageOne
	for name, c := range map[string]struct {
		commit string
		steps  []step
		want   providers.BranchState
	}{
		"the target head":         {shaBase, []step{get(mainRef, "ref_main.json")}, providers.BranchAtTarget},
		"an ancestor of the head": {shaOld, []step{get(mainRef, "ref_main.json"), behind}, providers.BranchAtTarget},
		"the one change":          {shaCommit, slices.Concat(head(commitBody(shaBase)), trees(newRoot, newApps)), providers.BranchOneChange},
		"another file beside it": {shaCommit, slices.Concat(head(commitBody(shaBase)),
			trees(newRoot, strings.ReplaceAll(newApps, shaRun, shaOld))), providers.BranchForeign},
		"a file added beside it": {shaCommit, slices.Concat(head(commitBody(shaBase)), trees(newRoot, strings.Replace(newApps,
			`"tree":[`, `"tree":[{"path":"new.yaml","mode":"100644","type":"blob","sha":"`+shaOld+`"},`, 1))),
			providers.BranchForeign},
		"another file at the root": {shaCommit, slices.Concat(head(commitBody(shaBase)),
			trees(strings.ReplaceAll(newRoot, strings.Repeat("9", 40), shaOld), newApps)[:2]), providers.BranchForeign},
		"the mode of the file": {shaCommit, slices.Concat(head(commitBody(shaBase)), trees(newRoot, strings.ReplaceAll(newApps,
			`"mode":"100644","type":"blob","sha":"`+shaNewBlob, `"mode":"100755","type":"blob","sha":"`+shaNewBlob))),
			providers.BranchForeign},
		"a merge":                 {shaCommit, head(commitBody(shaBase, shaOld))[:3], providers.BranchForeign},
		"a commit without parent": {shaCommit, head(commitBody())[:3], providers.BranchForeign},
		"a parent off the target": {shaCommit, append(head(commitBody(shaOld)), diverged), providers.BranchForeign},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, c.steps...)
			got, err := h.p.CheckBranch(ctx, c.commit, deployPath)
			if err != nil || got != c.want {
				t.Fatalf("CheckBranch() = %v, %v, want %v", got, err, c.want)
			}
		})
	}
}

func TestCheckBranchFailsClosed(t *testing.T) {
	root := mustRead(t, "tree_root.json")
	newRoot := strings.ReplaceAll(root, shaApps, shaNewTree)
	ahead := get("/compare/"+shaBase+"..."+shaCommit, "")
	ahead.raw, ahead.query = `{"status":"ahead"}`, perPageOne
	head := []step{get(mainRef, "ref_main.json"), ahead}
	parent := []step{raw("/git/commits/"+shaCommit, commitBody(shaBase)), get(mainRef, "ref_main.json")}
	for name, c := range map[string]struct {
		steps []step
		class func(error) bool
	}{
		"commit not found": {append(head, status(http.MethodGet, "/git/commits/"+shaCommit, http.StatusNotFound, "")),
			providers.IsNotFound},
		"another commit returned": {append(head, raw("/git/commits/"+shaCommit, strings.ReplaceAll(commitBody(shaBase),
			`"sha":"`+shaCommit, `"sha":"`+shaOld))), nil},
		"truncated tree": {slices.Concat(head, parent, []step{raw("/git/trees/"+shaCommit,
			strings.ReplaceAll(newRoot, `"truncated":false`, `"truncated":true`)), raw("/git/trees/"+shaBase, root)}), nil},
		"name listed twice": {slices.Concat(head, parent, []step{raw("/git/trees/"+shaCommit, newRoot),
			raw("/git/trees/"+shaBase, strings.Replace(root, `"tree":[`,
				`"tree":[{"path":"README.md","mode":"100644","type":"blob","sha":"`+shaOld+`"},`, 1))}), nil},
		"forbidden": {[]step{status(http.MethodGet, mainRef, http.StatusForbidden, fxForbidden)}, providers.IsForbidden},
		"apps tree missing": {slices.Concat(head, parent, []step{raw("/git/trees/"+shaCommit, newRoot),
			raw("/git/trees/"+shaBase, root), status(http.MethodGet, "/git/trees/"+shaNewTree, http.StatusNotFound, ""),
		}), providers.IsNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, c.steps...)
			got, err := h.p.CheckBranch(ctx, shaCommit, deployPath)
			if err == nil || got != 0 {
				t.Fatalf("CheckBranch() = %v, %v, want an error and no answer", got, err)
			}
			if c.class != nil && !c.class(err) {
				t.Fatalf("got %v", err)
			}
			assertSafe(t, err)
		})
	}
}

func TestCheckBranchRefusesBadInput(t *testing.T) {
	h := newHarness(t)
	for name, c := range map[string]struct{ commit, path string }{
		"short commit": {"1111", deployPath},
		"branch name":  {branch, deployPath},
		"bad path":     {shaCommit, "a/../b"},
		"empty path":   {shaCommit, ""},
	} {
		if _, err := h.p.CheckBranch(ctx, c.commit, c.path); err == nil {
			t.Errorf("%s: allowed", name)
		}
	}
}

func TestCheckMerged(t *testing.T) {
	root, apps := mustRead(t, "tree_root.json"), mustRead(t, "tree_apps.json")
	newRoot := strings.ReplaceAll(root, shaApps, shaNewTree)
	newApps := strings.ReplaceAll(apps, shaDeploy, shaNewBlob)
	// shaBase, the target head, merged shaCommit; its first parent is shaOld.
	merge := func(parents ...string) step {
		ps := make([]string, 0, len(parents))
		for _, p := range parents {
			ps = append(ps, `{"sha":"`+p+`"}`)
		}
		return raw("/git/commits/"+shaBase, `{"sha":"`+shaBase+`","parents":[`+strings.Join(ps, ",")+`]}`)
	}
	// trees compares shaCommit with its parent shaOld.
	trees := func(appsA string) []step {
		return []step{raw("/git/trees/"+shaCommit, newRoot), raw("/git/trees/"+shaOld, root),
			raw("/git/trees/"+shaNewTree, appsA), raw("/git/trees/"+shaApps, apps)}
	}
	const elsewhere = "8888888888888888888888888888888888888888"
	diverged := func(base, head string) step {
		s := get("/compare/"+base+"..."+head, "compare_diverged.json")
		s.query = perPageOne
		return s
	}
	for name, c := range map[string]struct {
		mergeCommit string
		steps       []step
		want        providers.BranchState
		parent      string
	}{
		// A merge commit takes head into the target branch, so head is now an ancestor of it; judged against its
		// own parent it is still the one change.
		"merged with a merge commit": {shaBase, slices.Concat([]step{get(mainRef, "ref_main.json"),
			merge(shaOld, shaCommit), raw("/git/commits/"+shaCommit, commitBody(shaOld))}, trees(newApps)),
			providers.BranchOneChange, shaOld},
		"squashed": {shaBase, slices.Concat([]step{get(mainRef, "ref_main.json"), merge(shaOld),
			raw("/git/commits/"+shaCommit, commitBody(shaOld))}, trees(newApps)), providers.BranchOneChange, shaOld},
		"another file beside it": {shaBase, slices.Concat([]step{get(mainRef, "ref_main.json"), merge(shaOld, shaCommit),
			raw("/git/commits/"+shaCommit, commitBody(shaOld))}, trees(strings.ReplaceAll(newApps, shaRun, elsewhere))),
			providers.BranchForeign, ""},
		"head is the merge commit": {shaCommit, nil, providers.BranchForeign, ""},
		"merge commit off the target": {shaOld, []step{get(mainRef, "ref_main.json"), diverged(shaBase, shaOld)},
			providers.BranchForeign, ""},
		"head with two parents": {shaBase, []step{get(mainRef, "ref_main.json"), merge(shaOld, shaCommit),
			raw("/git/commits/"+shaCommit, commitBody(shaOld, elsewhere))}, providers.BranchForeign, ""},
		"parent after the merge base": {shaBase, []step{get(mainRef, "ref_main.json"), merge(shaOld, shaCommit),
			raw("/git/commits/"+shaCommit, commitBody(elsewhere)), diverged(shaOld, elsewhere)}, providers.BranchForeign, ""},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, c.steps...)
			got, parent, err := h.p.CheckMerged(ctx, shaCommit, c.mergeCommit, deployPath)
			if err != nil || got != c.want || parent != c.parent {
				t.Fatalf("CheckMerged() = %v, %q, %v, want %v, %q", got, parent, err, c.want, c.parent)
			}
		})
	}
}

func TestCheckMergedFailsClosed(t *testing.T) {
	for name, steps := range map[string][]step{
		"forbidden": {status(http.MethodGet, mainRef, http.StatusForbidden, fxForbidden)},
		"merge commit not found": {get(mainRef, "ref_main.json"),
			status(http.MethodGet, "/git/commits/"+shaBase, http.StatusNotFound, "")},
		"bad parent": {get(mainRef, "ref_main.json"),
			raw("/git/commits/"+shaBase, `{"sha":"`+shaBase+`","parents":[{"sha":"main"}]}`)},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, steps...)
			got, parent, err := h.p.CheckMerged(ctx, shaCommit, shaBase, deployPath)
			if err == nil || got != 0 || parent != "" {
				t.Fatalf("CheckMerged() = %v, %q, %v, want an error and no answer", got, parent, err)
			}
			assertSafe(t, err)
		})
	}
	h := newHarness(t)
	for name, c := range map[string]struct{ head, merge, path string }{
		"short head":   {"1111", shaBase, deployPath},
		"short merge":  {shaCommit, "", deployPath},
		"dot dot path": {shaCommit, shaBase, "a/../b"},
	} {
		if _, _, err := h.p.CheckMerged(ctx, c.head, c.merge, c.path); err == nil {
			t.Errorf("%s: allowed", name)
		}
	}
}

func TestCommitFileRefusesBadInput(t *testing.T) {
	h := newHarness(t)
	for name, c := range map[string]struct {
		branch, head string
		change       providers.FileChange
	}{
		"target branch": {target, shaBase, change},
		"other branch":  {other, shaBase, change},
		"short head":    {branch, "1111", change},
		"bad path":      {branch, shaBase, providers.FileChange{Path: "../x", Content: []byte("x"), Message: "m"}},
		"empty message": {branch, shaBase, providers.FileChange{Path: "a", Content: []byte("x"), Message: " "}},
		"content > max": {branch, shaBase, providers.FileChange{Path: "a", Content: make([]byte, providers.MaxFileBytes+1), Message: "m"}},
	} {
		if _, err := h.p.CommitFile(ctx, c.branch, c.head, c.change); err == nil {
			t.Errorf("%s: allowed", name)
		}
	}
}

func TestCommitFileChecksStoredBlob(t *testing.T) {
	h := newHarness(t, append(commitHead(), readSteps()...)...)
	h.s.steps = append(h.s.steps, status(http.MethodPost, "/git/blobs", http.StatusCreated, ""))
	h.s.steps[len(h.s.steps)-1].raw = `{"sha":"` + shaDeploy + `"}`
	if _, err := h.p.CommitFile(ctx, branch, shaBase, change); err == nil {
		t.Fatal("a blob with other content was accepted")
	}
}

const pullsQuery = "direction=desc&head=acme%3Ashadewright%2Freq-1&per_page=100&sort=created&state="

func listPulls(state, file string) step {
	s := get(pullsPath, file)
	s.query = pullsQuery + state
	return s
}

var newCR = providers.NewChangeRequest{
	HeadBranch: branch, BaseBranch: target, Title: "fix(apps): raise replicas", Body: "marker",
}

func TestOpenChangeRequestExistingIsReturned(t *testing.T) {
	h := newHarness(t, listPulls("open", "pulls_open.json")) // no POST
	cr, err := h.p.OpenChangeRequest(ctx, newCR)
	if err != nil {
		t.Fatal(err)
	}
	want := providers.ChangeRequest{
		Number: 12, URL: "https://github.com/acme/fleet/pull/12", HeadBranch: branch, BaseBranch: target,
		HeadSHA: shaCommit, State: providers.ChangeRequestOpen, Title: "fix(apps): raise replicas",
		Body: "Raise replicas.\n\n<!-- shadewright: uid=req-1 digest=sha256:abc -->",
	}
	if cr != want {
		t.Errorf("got %+v\nwant %+v", cr, want)
	}
}

func TestOpenChangeRequestCreates(t *testing.T) {
	create := status(http.MethodPost, pullsPath, http.StatusCreated, "pull_open.json")
	create.check = func(t *testing.T, b map[string]any) {
		want := `{"base":"main","body":"marker","head":"shadewright/req-1","maintainer_can_modify":false,` +
			`"title":"fix(apps): raise replicas"}`
		if got, _ := json.Marshal(b); string(got) != want {
			t.Errorf("create body = %s", got)
		}
	}
	h := newHarness(t, listPulls("open", "pulls_empty.json"), create)
	cr, err := h.p.OpenChangeRequest(ctx, newCR)
	if err != nil || cr.Number != 12 || cr.State != providers.ChangeRequestOpen {
		t.Fatalf("got %+v, %v", cr, err)
	}
}

func TestOpenChangeRequestAlreadyExistsAfterRace(t *testing.T) {
	h := newHarness(t, listPulls("open", "pulls_empty.json"),
		status(http.MethodPost, pullsPath, http.StatusUnprocessableEntity, "error_pr_exists.json"),
		listPulls("open", "pulls_open.json"))
	cr, err := h.p.OpenChangeRequest(ctx, newCR)
	if err != nil || cr.Number != 12 {
		t.Fatalf("got %+v, %v", cr, err)
	}
}

func TestOpenChangeRequestIgnoresForks(t *testing.T) {
	// A fork's pull request from a branch of the same name is not ours: open our own.
	h := newHarness(t, listPulls("open", "pulls_open_fork.json"),
		status(http.MethodPost, pullsPath, http.StatusCreated, "pull_open.json"))
	cr, err := h.p.OpenChangeRequest(ctx, newCR)
	if err != nil || cr.Number != 12 {
		t.Fatalf("got %+v, %v", cr, err)
	}
}

func TestOpenChangeRequestOtherBaseIsConflict(t *testing.T) {
	h := newHarness(t, listPulls("open", "pulls_open_other_base.json"))
	_, err := h.p.OpenChangeRequest(ctx, newCR)
	if !providers.IsConflict(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
}

func TestOpenChangeRequestMixedBasesIsConflict(t *testing.T) {
	// One head branch with open pull requests to the target and to another base: a conflict whichever was
	// opened first.
	ours := mustRead(t, "pull_open.json")
	otherBase := strings.ReplaceAll(strings.ReplaceAll(mustRead(t, "pull_open.json"), `"ref":"main"`, `"ref":"release"`),
		`"number":12`, `"number":14`)
	for name, list := range map[string]string{
		"target newest": "[" + strings.Replace(ours, `"number":12`, `"number":15`, 1) + "," + otherBase + "]",
		"target oldest": "[" + otherBase + "," + ours + "]",
	} {
		t.Run(name, func(t *testing.T) {
			s := listPulls("open", "")
			s.raw = list
			h := newHarness(t, s)
			_, err := h.p.OpenChangeRequest(ctx, newCR)
			if !providers.IsConflict(err) {
				t.Fatalf("got %v", err)
			}
			assertSafe(t, err)
		})
	}
}

func TestOpenChangeRequestMissingBranch(t *testing.T) {
	h := newHarness(t, listPulls("open", "pulls_empty.json"),
		status(http.MethodPost, pullsPath, http.StatusUnprocessableEntity, "error_pr_exists.json"),
		listPulls("open", "pulls_empty.json"),
		status(http.MethodGet, branchRef, http.StatusNotFound, fxForbidden))
	_, err := h.p.OpenChangeRequest(ctx, newCR)
	if !providers.IsNotFound(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
}

func TestOpenChangeRequestRefusesBadInput(t *testing.T) {
	h := newHarness(t)
	for name, mut := range map[string]func(*providers.NewChangeRequest){
		"other base":  func(r *providers.NewChangeRequest) { r.BaseBranch = other },
		"head main":   func(r *providers.NewChangeRequest) { r.HeadBranch = target },
		"head other":  func(r *providers.NewChangeRequest) { r.HeadBranch = "feature/x" },
		"empty title": func(r *providers.NewChangeRequest) { r.Title = "" },
		"long title":  func(r *providers.NewChangeRequest) { r.Title = strings.Repeat("t", 257) },
		"long body":   func(r *providers.NewChangeRequest) { r.Body = strings.Repeat("b", 65537) },
		"bad UTF-8":   func(r *providers.NewChangeRequest) { r.Body = "\xff" },
	} {
		req := newCR
		mut(&req)
		if _, err := h.p.OpenChangeRequest(ctx, req); err == nil {
			t.Errorf("%s: allowed", name)
		}
	}
}

func TestFindChangeRequest(t *testing.T) {
	// The fork's pull request is newest, but not ours; the merged one is the most recent of ours.
	h := newHarness(t, listPulls("all", "pulls_all.json"))
	cr, err := h.p.FindChangeRequest(ctx, branch)
	if err != nil {
		t.Fatal(err)
	}
	if cr.Number != 12 || cr.State != providers.ChangeRequestMerged ||
		cr.MergeCommit != "dddddddddddddddddddddddddddddddddddddddd" {
		t.Errorf("got %+v", cr)
	}
}

func TestFindChangeRequestNone(t *testing.T) {
	h := newHarness(t, listPulls("all", "pulls_open_fork.json"))
	_, err := h.p.FindChangeRequest(ctx, branch)
	if !providers.IsNotFound(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
	if _, err := h.p.FindChangeRequest(ctx, "feature/x"); err == nil {
		t.Error("a branch outside shadewright/ was allowed")
	}
}

func TestCloseChangeRequest(t *testing.T) {
	patch := status(http.MethodPatch, "/pulls/12", http.StatusOK, "pull_closed.json")
	patch.check = func(t *testing.T, b map[string]any) {
		if b["state"] != "closed" || len(b) != 1 {
			t.Errorf("close body = %v", b)
		}
	}
	h := newHarness(t, get("/pulls/12", "pull_open.json"), patch)
	if err := h.p.CloseChangeRequest(ctx, 12); err != nil {
		t.Fatal(err)
	}
}

func TestCloseChangeRequestIdempotent(t *testing.T) {
	h := newHarness(t, get("/pulls/12", "pull_closed.json")) // no PATCH
	if err := h.p.CloseChangeRequest(ctx, 12); err != nil {
		t.Fatal(err)
	}
}

func TestCloseChangeRequestMergedIsConflict(t *testing.T) {
	h := newHarness(t, get("/pulls/12", "pull_merged.json"))
	if err := h.p.CloseChangeRequest(ctx, 12); !providers.IsConflict(err) {
		t.Fatalf("got %v", err)
	}
}

func TestCloseChangeRequestMergedMeanwhile(t *testing.T) {
	h := newHarness(t, get("/pulls/12", "pull_open.json"),
		status(http.MethodPatch, "/pulls/12", http.StatusUnprocessableEntity, fxForbidden),
		get("/pulls/12", "pull_merged.json"))
	if err := h.p.CloseChangeRequest(ctx, 12); !providers.IsConflict(err) {
		t.Fatalf("got %v", err)
	}
}

func TestCloseChangeRequestRefusesOthers(t *testing.T) {
	// A pull request that Shadewright did not open (another head branch) is never closed.
	h := newHarness(t, get("/pulls/13", "pull_foreign.json"))
	err := h.p.CloseChangeRequest(ctx, 13)
	if err == nil || providers.IsNotFound(err) || providers.IsConflict(err) {
		t.Fatalf("got %v", err)
	}
	if err := h.p.CloseChangeRequest(ctx, 0); err == nil {
		t.Error("number 0 was allowed")
	}
}

// commentMarker is the marker of the comments in the tests of CommentChangeRequest.
const commentMarker = "<!-- shadewright comment -->"

// comments is the step that answers page of the comments of pull request 12 with bodies.
func comments(page int, bodies ...string) step {
	cs := make([]string, 0, len(bodies))
	for _, b := range bodies {
		cs = append(cs, `{"body":"`+b+`"}`)
	}
	st := raw("/issues/12/comments", "["+strings.Join(cs, ",")+"]")
	st.query = "page=" + strconv.Itoa(page) + "&per_page=20"
	return st
}

func TestCommentChangeRequest(t *testing.T) {
	post := status(http.MethodPost, "/issues/12/comments", http.StatusCreated, "")
	post.check = func(t *testing.T, b map[string]any) {
		if b["body"] != "note "+commentMarker || len(b) != 1 {
			t.Errorf("comment body = %v", b)
		}
	}
	full := make([]string, 20)
	for i := range full {
		full[i] = "other"
	}
	for name, steps := range map[string][]step{
		"no comments":    {get("/pulls/12", "pull_open.json"), comments(1), post},
		"two pages":      {get("/pulls/12", "pull_open.json"), comments(1, full...), comments(2, "other"), post},
		"marker present": {get("/pulls/12", "pull_open.json"), comments(1, "other", "x "+commentMarker)},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, steps...)
			if err := h.p.CommentChangeRequest(ctx, 12, commentMarker, "note "+commentMarker); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCommentChangeRequestRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		number int64
		steps  []step
		class  func(error) bool
	}{
		"closed":  {12, []step{get("/pulls/12", "pull_closed.json")}, providers.IsConflict},
		"merged":  {12, []step{get("/pulls/12", "pull_merged.json")}, providers.IsConflict},
		"foreign": {13, []step{get("/pulls/13", "pull_foreign.json")}, nil},
		"comments unreadable": {12, []step{get("/pulls/12", "pull_open.json"),
			status(http.MethodGet, "/issues/12/comments", http.StatusForbidden, fxForbidden)}, providers.IsForbidden},
		"failed": {12, []step{get("/pulls/12", "pull_open.json"), comments(1),
			status(http.MethodPost, "/issues/12/comments", http.StatusForbidden, fxForbidden)}, providers.IsForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, c.steps...)
			err := h.p.CommentChangeRequest(ctx, c.number, commentMarker, "note "+commentMarker)
			if err == nil || (c.class != nil && !c.class(err)) {
				t.Fatalf("got %v", err)
			}
			assertSafe(t, err)
		})
	}
	h := newHarness(t)
	for name, c := range map[string]struct {
		number       int64
		marker, body string
	}{
		"number 0":            {0, commentMarker, commentMarker},
		"empty":               {12, commentMarker, " "},
		"too long":            {12, commentMarker, commentMarker + strings.Repeat("x", 70000)},
		"not UTF-8":           {12, commentMarker, commentMarker + "\xff"},
		"no marker":           {12, "", "note"},
		"marker missing":      {12, commentMarker, "note"},
		"marker on two lines": {12, "x\ny", "x\ny"},
	} {
		if err := h.p.CommentChangeRequest(ctx, c.number, c.marker, c.body); err == nil {
			t.Errorf("%s: allowed", name)
		}
	}
}

func TestCloseChangeRequestNumberMismatch(t *testing.T) {
	h := newHarness(t, get("/pulls/99", "pull_open.json"))
	if err := h.p.CloseChangeRequest(ctx, 99); err == nil {
		t.Fatal("a response for another number was accepted")
	}
}

func TestRetriesServerErrorsWithBackoff(t *testing.T) {
	h := newHarness(t,
		status(http.MethodGet, mainRef, http.StatusBadGateway, fxForbidden),
		step{method: http.MethodGet, path: mainRef, err: errors.New("dial tcp: " + bodyMarker)},
		status(http.MethodGet, mainRef, http.StatusServiceUnavailable, ""),
		get(mainRef, "ref_main.json"))
	h.p.maxDelay = 1500 * time.Millisecond
	if _, err := h.p.BranchHead(ctx, target); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 1500 * time.Millisecond}
	if fmt.Sprint(h.sleeps) != fmt.Sprint(want) {
		t.Errorf("sleeps = %v, want %v (doubling, capped)", h.sleeps, want)
	}
}

func TestRetryAttemptsAreBounded(t *testing.T) {
	steps := make([]step, 0, DefaultMaxAttempts)
	for range DefaultMaxAttempts {
		steps = append(steps, status(http.MethodGet, mainRef, http.StatusInternalServerError, ""))
	}
	h := newHarness(t, steps...)
	_, err := h.p.BranchHead(ctx, target)
	if !providers.IsRetryable(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
	if len(h.sleeps) != DefaultMaxAttempts-1 {
		t.Errorf("slept %d times", len(h.sleeps))
	}
}

func TestJitterSpreadsDelays(t *testing.T) {
	p, err := newProvider(Config{RepositoryURL: testRepoURL, TargetBranch: target},
		testHooks{token: func(context.Context) (string, error) { return testToken, nil }})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[time.Duration]bool{}
	for range 50 {
		d := p.jitter(time.Second)
		if d < 500*time.Millisecond || d > time.Second {
			t.Fatalf("jitter(1s) = %v", d)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Error("jitter is constant")
	}
}

func TestRetryAfterIsHonoured(t *testing.T) {
	reset := strconv.FormatInt(t0.Add(7*time.Second).Unix(), 10)
	h := newHarness(t,
		step{method: http.MethodGet, path: mainRef, status: http.StatusTooManyRequests,
			header: map[string]string{hRetry: "3"}},
		step{method: http.MethodGet, path: mainRef, status: http.StatusForbidden,
			header: map[string]string{hdrRemaining: "0", "X-RateLimit-Reset": reset}, file: fxForbidden},
		step{method: http.MethodGet, path: mainRef, status: http.StatusServiceUnavailable,
			header: map[string]string{hRetry: t0.Add(2 * time.Second).Format(http.TimeFormat)}},
		get(mainRef, "ref_main.json"))
	if _, err := h.p.BranchHead(ctx, target); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{3 * time.Second, 7 * time.Second, 2 * time.Second}
	if fmt.Sprint(h.sleeps) != fmt.Sprint(want) {
		t.Errorf("sleeps = %v, want %v", h.sleeps, want)
	}
}

func TestLongRetryAfterIsReturned(t *testing.T) {
	h := newHarness(t, step{method: http.MethodPost, path: pullsPath, status: http.StatusForbidden,
		header: map[string]string{hRetry: "120"}, file: fxSecondary})
	h.s.steps = append([]step{listPulls("open", "pulls_empty.json")}, h.s.steps...)
	_, err := h.p.OpenChangeRequest(ctx, newCR)
	if d, ok := providers.RetryAfter(err); !ok || d != 2*time.Minute {
		t.Fatalf("RetryAfter = %v, %v (%v)", d, ok, err)
	}
	if len(h.sleeps) != 0 {
		t.Errorf("slept %v; a delay over MaxDelay must go back to the caller", h.sleeps)
	}
	assertSafe(t, err)
}

func TestSecondaryRateLimitWithoutHeaders(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			h := newHarness(t, status(http.MethodGet, mainRef, code, fxSecondary))
			_, err := h.p.BranchHead(ctx, target)
			if d, ok := providers.RetryAfter(err); !ok || d != time.Minute {
				t.Fatalf("RetryAfter = %v, %v (%v)", d, ok, err)
			}
			assertSafe(t, err)
		})
	}
}

func TestForbiddenIsNotRetried(t *testing.T) {
	h := newHarness(t, status(http.MethodGet, mainRef, http.StatusForbidden, fxForbidden))
	_, err := h.p.BranchHead(ctx, target)
	if err == nil || providers.IsRetryable(err) || providers.IsNotFound(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
}

// finalCase is one refusal GitHub gives for good: the steps up to it and the call that meets it.
type finalCase struct {
	steps []step
	call  func(p *Provider) error
	want  func(error) bool
}

// assertFinal checks that a refusal is classed, final, sent once and carries nothing of the response.
func assertFinal(t *testing.T, c finalCase) {
	t.Helper()
	h := newHarness(t, c.steps...)
	err := c.call(h.p)
	if !c.want(err) || providers.IsRetryable(err) || providers.IsNotFound(err) || providers.IsConflict(err) {
		t.Fatalf("got %v", err)
	}
	if _, ok := providers.RetryAfter(err); ok {
		t.Errorf("a final refusal carries a delay: %v", err)
	}
	if len(h.sleeps) != 0 {
		t.Errorf("slept %v before a final refusal", h.sleeps)
	}
	assertSafe(t, err)
}

func createBranch(p *Provider) error { return p.CreateBranch(ctx, branch, shaBase) }

func openCR(p *Provider) error {
	_, err := p.OpenChangeRequest(ctx, newCR)
	return err
}

func commitFile(p *Provider) error {
	_, err := p.CommitFile(ctx, branch, shaBase, change)
	return err
}

// A 403 without rate limit signals is a permission the App lacks or a rule of the repository: final, on a
// read and on every kind of write.
func TestForbiddenIsFinal(t *testing.T) {
	forbidden := func(method, path string) step {
		return status(method, path, http.StatusForbidden, fxForbidden)
	}
	for name, c := range map[string]finalCase{
		"read the branch head": {[]step{forbidden(http.MethodGet, mainRef)}, func(p *Provider) error {
			_, err := p.BranchHead(ctx, target)
			return err
		}, providers.IsForbidden},
		"create the branch": {[]step{get(mainRef, "ref_main.json"), forbidden(http.MethodPost, "/git/refs")},
			createBranch, providers.IsForbidden},
		"store the blob": {append(append(commitHead(), readSteps()...),
			forbidden(http.MethodPost, "/git/blobs")), commitFile, providers.IsForbidden},
		"move the branch": {append(commitSteps(), forbidden(http.MethodGet, "")), commitFile, providers.IsForbidden},
		"list pull requests": {[]step{{method: http.MethodGet, path: pullsPath, query: pullsQuery + "open",
			status: http.StatusForbidden, file: fxForbidden}}, openCR, providers.IsForbidden},
		"open the pull request": {[]step{listPulls("open", "pulls_empty.json"), forbidden(http.MethodPost, pullsPath)},
			openCR, providers.IsForbidden},
		"close the pull request": {[]step{get("/pulls/12", "pull_open.json"), forbidden(http.MethodPatch, "/pulls/12")},
			func(p *Provider) error { return p.CloseChangeRequest(ctx, 12) }, providers.IsForbidden},
	} {
		t.Run(name, func(t *testing.T) { assertFinal(t, c) })
	}
}

// A 422 the adapter cannot resolve by reading the state is final: a refused branch name or rule, a pull
// request GitHub will not open while no open one of the branch explains it, an object it does not accept.
func TestUnresolvedValidationIsFinal(t *testing.T) {
	refused := func(method, path, file string) step {
		return status(method, path, http.StatusUnprocessableEntity, file)
	}
	assertFinal(t, finalCase{append(append(commitHead(), readSteps()...),
		status(http.MethodPost, "/git/blobs", http.StatusCreated, "blob_created.json"),
		status(http.MethodPost, "/git/trees", http.StatusCreated, "tree_created.json"),
		refused(http.MethodPost, "/git/commits", "error_validation.json")), commitFile, providers.IsRejected})

	// A create whose 422 the state does not explain is asked again, with backoff, before it is final: a read
	// may come from a replica that does not show the branch or pull request yet.
	repeat := func(n int, steps ...step) []step {
		out := make([]step, 0, n*len(steps))
		for range n {
			out = append(out, steps...)
		}
		return out
	}
	for name, c := range map[string]finalCase{
		"create the branch, which does not exist": {append([]step{get(mainRef, "ref_main.json")},
			repeat(DefaultMaxAttempts, refused(http.MethodPost, "/git/refs", "error_rule_violation.json"),
				status(http.MethodGet, branchRef, http.StatusNotFound, fxForbidden))...),
			createBranch, providers.IsRejected},
		"open the pull request, with none open and the branch there": {append([]step{listPulls("open", "pulls_empty.json")},
			repeat(DefaultMaxAttempts, refused(http.MethodPost, pullsPath, "error_validation.json"),
				listPulls("open", "pulls_empty.json"), get(branchRef, "ref_branch.json"))...), openCR, providers.IsRejected},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, c.steps...)
			err := c.call(h.p)
			if !c.want(err) || providers.IsRetryable(err) {
				t.Fatalf("got %v", err)
			}
			want := []time.Duration{DefaultBaseDelay, 2 * DefaultBaseDelay, 4 * DefaultBaseDelay}
			if !slices.Equal(h.sleeps, want) {
				t.Errorf("slept %v, want %v", h.sleeps, want)
			}
			assertSafe(t, err)
		})
	}
}

// A replica that lags the write: the branch of a lost create response is not readable at once, the pull
// request of a lost create response is not listed at once, or the commit just pushed is not seen ("No commits
// between"). The adapter asks again instead of calling the 422 final.
func TestUnexplainedValidationWaitsForReplicas(t *testing.T) {
	t.Run("branch not readable yet", func(t *testing.T) {
		h := newHarness(t, get(mainRef, "ref_main.json"),
			status(http.MethodPost, "/git/refs", http.StatusUnprocessableEntity, "error_ref_exists.json"),
			status(http.MethodGet, branchRef, http.StatusNotFound, fxForbidden),
			status(http.MethodPost, "/git/refs", http.StatusUnprocessableEntity, "error_ref_exists.json"),
			get(branchRef, "ref_branch.json"))
		if err := createBranch(h.p); err != nil {
			t.Fatalf("CreateBranch: %v", err)
		}
	})
	t.Run("pull request not listed yet", func(t *testing.T) {
		h := newHarness(t, listPulls("open", "pulls_empty.json"),
			status(http.MethodPost, pullsPath, http.StatusUnprocessableEntity, "error_pr_exists.json"),
			listPulls("open", "pulls_empty.json"), get(branchRef, "ref_branch.json"),
			status(http.MethodPost, pullsPath, http.StatusUnprocessableEntity, "error_pr_exists.json"),
			listPulls("open", "pulls_open.json"))
		cr, err := h.p.OpenChangeRequest(ctx, newCR)
		if err != nil || cr.Number != 12 {
			t.Fatalf("OpenChangeRequest = %+v, %v", cr, err)
		}
	})
	t.Run("commit not seen yet", func(t *testing.T) {
		h := newHarness(t, listPulls("open", "pulls_empty.json"),
			status(http.MethodPost, pullsPath, http.StatusUnprocessableEntity, "error_validation.json"),
			listPulls("open", "pulls_empty.json"), get(branchRef, "ref_branch.json"),
			status(http.MethodPost, pullsPath, http.StatusCreated, "pull_open.json"))
		if _, err := h.p.OpenChangeRequest(ctx, newCR); err != nil {
			t.Fatalf("OpenChangeRequest: %v", err)
		}
	})
	t.Run("no time left for another attempt", func(t *testing.T) {
		h := newHarness(t, listPulls("open", "pulls_empty.json"),
			status(http.MethodPost, pullsPath, http.StatusUnprocessableEntity, "error_validation.json"),
			listPulls("open", "pulls_empty.json"), get(branchRef, "ref_branch.json"))
		h.p.now = time.Now
		short, cancel := context.WithTimeout(ctx, DefaultBaseDelay/5)
		defer cancel()
		_, err := h.p.OpenChangeRequest(short, newCR)
		if !providers.IsRetryable(err) || providers.IsRejected(err) {
			t.Fatalf("got %v, want retryable", err)
		}
	})
}

// A 422 that the state explains is resolved by the adapter, never returned as a refusal: an existing branch at
// the commit, or an open pull request of the branch (the controller then checks its marker).
func TestValidationResolvedByTheStateIsNotRejected(t *testing.T) {
	h := newHarness(t, get(mainRef, "ref_main.json"),
		status(http.MethodPost, "/git/refs", http.StatusUnprocessableEntity, "error_ref_exists.json"),
		get(branchRef, "ref_branch.json"),
		listPulls("open", "pulls_empty.json"),
		status(http.MethodPost, pullsPath, http.StatusUnprocessableEntity, "error_pr_exists.json"),
		listPulls("open", "pulls_open.json"))
	if err := h.p.CreateBranch(ctx, branch, shaBase); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	cr, err := h.p.OpenChangeRequest(ctx, newCR)
	if err != nil || cr.Number != 12 || !strings.Contains(cr.Body, "shadewright: uid=req-1") {
		t.Fatalf("OpenChangeRequest = %+v, %v", cr, err)
	}
}

// A 403 or 429 with GitHub's rate limit signals (Retry-After, an exhausted quota, or the secondary rate limit
// message) stays retryable with the delay GitHub asked for; it is never a final refusal. A delay longer than
// the adapter waits in place goes back to the caller, which caps it at the request's deadline.
func TestRateLimitIsNotForbidden(t *testing.T) {
	reset := strconv.FormatInt(t0.Add(10*time.Minute).Unix(), 10)
	for name, c := range map[string]struct {
		code   int
		header map[string]string
		file   string
		want   time.Duration
	}{
		"403 with Retry-After":        {http.StatusForbidden, map[string]string{hRetry: "120"}, fxSecondary, 2 * time.Minute},
		"429 with Retry-After":        {http.StatusTooManyRequests, map[string]string{hRetry: "90"}, "", 90 * time.Second},
		"403 with an exhausted quota": {http.StatusForbidden, map[string]string{hdrRemaining: "0", hdrReset: reset}, fxForbidden, 10 * time.Minute},
		"429 with an exhausted quota": {http.StatusTooManyRequests, map[string]string{hdrRemaining: "0", hdrReset: reset}, "", 10 * time.Minute},
		"403 secondary message only":  {http.StatusForbidden, nil, fxSecondary, time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, listPulls("open", "pulls_empty.json"),
				step{method: http.MethodPost, path: pullsPath, status: c.code, header: c.header, file: c.file})
			h.p.attempts = 1
			err := openCR(h.p)
			if providers.IsForbidden(err) || providers.IsRejected(err) {
				t.Fatalf("a rate limit is classed as final: %v", err)
			}
			if d, ok := providers.RetryAfter(err); !ok || d != c.want || !providers.IsRetryable(err) {
				t.Fatalf("RetryAfter = %v, %v (%v), want %v", d, ok, err, c.want)
			}
			assertSafe(t, err)
		})
	}
}

func TestRetryStopsBeforeTheDeadline(t *testing.T) {
	h := newHarness(t, step{method: http.MethodGet, path: mainRef, status: http.StatusServiceUnavailable,
		header: map[string]string{hRetry: "10"}})
	h.p.now = time.Now
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := h.p.BranchHead(c, target)
	if d, ok := providers.RetryAfter(err); !ok || d != 10*time.Second || len(h.sleeps) != 0 {
		t.Fatalf("RetryAfter = %v, %v, sleeps %v", d, ok, h.sleeps)
	}
}

func TestTransportErrorsAreReplaced(t *testing.T) {
	inner := fmt.Errorf("Get \"https://api.github.com/repos/acme/fleet/git/ref/heads/main?token=%s\": %s", testToken, bodyMarker)
	h := newHarness(t, step{method: http.MethodGet, path: mainRef, err: inner})
	h.p.attempts = 1
	_, err := h.p.BranchHead(ctx, target)
	if !providers.IsRetryable(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
	if errors.Is(err, inner) {
		t.Error("the HTTP client error is reachable")
	}
}

func TestCanceledContext(t *testing.T) {
	h := newHarness(t)
	c, cancel := context.WithCancel(ctx)
	cancel()
	_, err := h.p.BranchHead(c, target)
	if !errors.Is(err, context.Canceled) || providers.IsRetryable(err) {
		t.Fatalf("got %v", err)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	h := newHarness(t, step{method: http.MethodGet, path: mainRef, status: http.StatusFound,
		header: map[string]string{"Location": "https://evil.example/" + bodyMarker}})
	_, err := h.p.BranchHead(ctx, target)
	if err == nil {
		t.Fatal("a redirect was accepted")
	}
	assertSafe(t, err)
}

func TestResponseBodyIsBounded(t *testing.T) {
	h := newHarness(t, step{method: http.MethodGet, path: "/git/trees/" + shaBase, status: http.StatusOK,
		raw: `{"sha":"` + shaTree + `","tree":[` + strings.Repeat(" ", maxJSONBody) + `]}`})
	_, err := h.p.ReadFile(ctx, shaBase, "apps/deploy.yaml", providers.MaxFileBytes)
	if err == nil || providers.IsTooLarge(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
}

type leakyTokens struct{ err error }

func (s leakyTokens) Token(context.Context) (auth.Token, error) { return auth.Token{}, s.err }
func (s leakyTokens) Repository() auth.Repository {
	return auth.Repository{Owner: owner, Name: "fleet"}
}

func TestTokenSourceErrorsAreReplaced(t *testing.T) {
	leak := fmt.Errorf("POST https://api.github.com/app/installations/1/access_tokens?jwt=%s: %s", testToken, bodyMarker)
	for name, c := range map[string]struct {
		err   error
		check func(error) bool
	}{
		"plain":     {leak, func(err error) bool { return !providers.IsRetryable(err) && !providers.IsNotFound(err) }},
		"not found": {fmt.Errorf("%w: %w", leak, providers.ErrNotFound), providers.IsNotFound},
		// A token it cannot get says nothing about a tree, so a cache must never keep the answer.
		"path not in tree": {fmt.Errorf("%w: %w", leak, providers.ErrPathNotInTree), func(err error) bool {
			return providers.IsNotFound(err) && !providers.IsPathNotInTree(err)
		}},
		"retryable": {&providers.RetryableError{RetryAfter: 5 * time.Minute, Err: leak}, func(err error) bool {
			d, ok := providers.RetryAfter(err)
			return ok && d == 5*time.Minute
		}},
		"deadline": {fmt.Errorf("%w: %w", leak, context.DeadlineExceeded), func(err error) bool {
			return providers.IsRetryable(err) && errors.Is(err, context.DeadlineExceeded)
		}},
		"canceled": {fmt.Errorf("%w: %w", leak, context.Canceled), func(err error) bool {
			return !providers.IsRetryable(err) && errors.Is(err, context.Canceled)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			p, err := New(Config{RepositoryURL: testRepoURL, TargetBranch: target, Tokens: leakyTokens{c.err}, MaxAttempts: 1})
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.BranchHead(ctx, target)
			if !c.check(err) {
				t.Fatalf("class lost: %v", err)
			}
			assertSafe(t, err)
			if errors.Is(err, leak) {
				t.Error("the token source error is reachable")
			}
		})
	}
}

func TestTokenErrorsKeepTheirClass(t *testing.T) {
	h := newHarness(t)
	h.p.token = func(context.Context) (string, error) {
		return "", &providers.RetryableError{RetryAfter: time.Hour, Err: errors.New("installation token: status 503")}
	}
	_, err := h.p.BranchHead(ctx, target)
	if d, ok := providers.RetryAfter(err); !ok || d != time.Hour {
		t.Fatalf("got %v", err)
	}
}

// A token the installation refuses (a permission the App lacks) keeps ErrForbidden through tokenError, so the
// caller stops instead of retrying until its deadline; no text of the token source stays reachable.
func TestTokenRefusalIsForbidden(t *testing.T) {
	h := newHarness(t)
	leak := errors.New("installation token: status 403 for https://api.github.com ghs_leaked")
	h.p.token = func(context.Context) (string, error) {
		return "", fmt.Errorf("%w: %w", leak, providers.ErrForbidden)
	}
	_, err := h.p.BranchHead(ctx, target)
	if !providers.IsForbidden(err) || providers.IsRetryable(err) || errors.Is(err, leak) {
		t.Fatalf("got %v, want ErrForbidden only", err)
	}
	if strings.Contains(err.Error(), "ghs_leaked") {
		t.Fatalf("token source text leaked: %v", err)
	}
}

func mustRead(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
