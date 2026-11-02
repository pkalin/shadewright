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
}

// script is a RoundTripper that plays recorded exchanges in order. It fails the test on any request to
// another host or repository, on any request not in the script, and when steps are left over.
type script struct {
	t     *testing.T
	mu    sync.Mutex
	steps []step
	n     int
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
		status(http.MethodGet, "/git/ref/heads/shadewright/req-2", http.StatusNotFound, "error_forbidden.json"))
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
	f, err := h.p.ReadFile(ctx, shaBase, "apps/deploy.yaml")
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
			_, err := h.p.ReadFile(ctx, shaBase, path)
			if !providers.IsNotFound(err) {
				t.Fatalf("ReadFile(%s) = %v, want not found", path, err)
			}
			assertSafe(t, err)
		})
	}
}

func TestReadFileMissingCommit(t *testing.T) {
	h := newHarness(t, status(http.MethodGet, "/git/trees/"+shaOld, http.StatusNotFound, "error_forbidden.json"))
	_, err := h.p.ReadFile(ctx, shaOld, "apps/deploy.yaml")
	if !providers.IsNotFound(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
}

func TestReadFileTooLargeBeforeDownload(t *testing.T) {
	h := newHarness(t, readSteps()...) // no blob request is recorded: the size check must come first
	_, err := h.p.ReadFile(ctx, shaBase, "apps/big.yaml")
	if !providers.IsTooLarge(err) {
		t.Fatalf("got %v", err)
	}
}

func TestReadFileStopsReadingLargeStream(t *testing.T) {
	// The tree claims 12 bytes, the stream is larger than MaxFileBytes.
	big := &countingReader{left: 4 * providers.MaxFileBytes}
	h := newHarness(t, readSteps()...)
	h.s.steps = append(h.s.steps, get("/git/blobs/"+shaDeploy, ""))
	h.p.client.Transport = bodyOverride{h.s, big}
	_, err := h.p.ReadFile(ctx, shaBase, "apps/deploy.yaml")
	if !providers.IsTooLarge(err) {
		t.Fatalf("got %v", err)
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
	if _, err := h.p.ReadFile(ctx, shaBase, "apps/deploy.yaml"); err == nil {
		t.Fatal("content that does not match its object ID was accepted")
	}
}

func TestReadFileRefusesBadInput(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"", "/apps/x", "apps/", "apps//x", "../x", "apps/../x", "./x", ".git/config",
		"a/.GIT/x", "a\x00b", "a\nb", "a\\b", strings.Repeat("a/", 40) + "x"} {
		if _, err := h.p.ReadFile(ctx, shaBase, path); err == nil {
			t.Errorf("ReadFile(%q) was allowed", path)
		}
	}
	for _, sha := range []string{"", target, "HEAD", strings.ToUpper(shaDeploy), shaBase[:39], "../" + shaBase[3:]} {
		if _, err := h.p.ReadFile(ctx, sha, "apps/deploy.yaml"); err == nil {
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
	return append(append([]step{get(branchRef, "ref_branch.json")}, readSteps()...), blob, tree, commit)
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
	h := newHarness(t, append([]step{get(branchRef, "ref_branch.json")}, readSteps()...)...)
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
	h := newHarness(t, append([]step{get(branchRef, "ref_branch.json")}, readSteps()...)...)
	_, err := h.p.CommitFile(ctx, branch, shaBase, providers.FileChange{Path: "apps/new.yaml", Content: []byte("x"), Message: "m"})
	if !providers.IsNotFound(err) {
		t.Fatalf("got %v", err)
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
	h := newHarness(t, append([]step{get(branchRef, "ref_branch.json")}, readSteps()...)...)
	h.s.steps = append(h.s.steps, status(http.MethodPost, "/git/blobs", http.StatusCreated, ""))
	h.s.steps[len(h.s.steps)-1].raw = `{"sha":"` + shaDeploy + `"}`
	if _, err := h.p.CommitFile(ctx, branch, shaBase, change); err == nil {
		t.Fatal("a blob with other content was accepted")
	}
}

const pullsQuery = "direction=desc&head=acme%3Ashadewright%2Freq-1&per_page=100&sort=created&state="

func listPulls(state, file string) step {
	s := get("/pulls", file)
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
	create := status(http.MethodPost, "/pulls", http.StatusCreated, "pull_open.json")
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
		status(http.MethodPost, "/pulls", http.StatusUnprocessableEntity, "error_pr_exists.json"),
		listPulls("open", "pulls_open.json"))
	cr, err := h.p.OpenChangeRequest(ctx, newCR)
	if err != nil || cr.Number != 12 {
		t.Fatalf("got %+v, %v", cr, err)
	}
}

func TestOpenChangeRequestIgnoresForks(t *testing.T) {
	// A fork's pull request from a branch of the same name is not ours: open our own.
	h := newHarness(t, listPulls("open", "pulls_open_fork.json"),
		status(http.MethodPost, "/pulls", http.StatusCreated, "pull_open.json"))
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
		status(http.MethodPost, "/pulls", http.StatusUnprocessableEntity, "error_pr_exists.json"),
		listPulls("open", "pulls_empty.json"),
		status(http.MethodGet, branchRef, http.StatusNotFound, "error_forbidden.json"))
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
		status(http.MethodPatch, "/pulls/12", http.StatusUnprocessableEntity, "error_forbidden.json"),
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

func TestCloseChangeRequestNumberMismatch(t *testing.T) {
	h := newHarness(t, get("/pulls/99", "pull_open.json"))
	if err := h.p.CloseChangeRequest(ctx, 99); err == nil {
		t.Fatal("a response for another number was accepted")
	}
}

func TestRetriesServerErrorsWithBackoff(t *testing.T) {
	h := newHarness(t,
		status(http.MethodGet, mainRef, http.StatusBadGateway, "error_forbidden.json"),
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
			header: map[string]string{hdrRemaining: "0", "X-RateLimit-Reset": reset}, file: "error_forbidden.json"},
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
	h := newHarness(t, step{method: http.MethodPost, path: "/pulls", status: http.StatusForbidden,
		header: map[string]string{hRetry: "120"}, file: "error_secondary.json"})
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
			h := newHarness(t, status(http.MethodGet, mainRef, code, "error_secondary.json"))
			_, err := h.p.BranchHead(ctx, target)
			if d, ok := providers.RetryAfter(err); !ok || d != time.Minute {
				t.Fatalf("RetryAfter = %v, %v (%v)", d, ok, err)
			}
			assertSafe(t, err)
		})
	}
}

func TestForbiddenIsNotRetried(t *testing.T) {
	h := newHarness(t, status(http.MethodGet, mainRef, http.StatusForbidden, "error_forbidden.json"))
	_, err := h.p.BranchHead(ctx, target)
	if err == nil || providers.IsRetryable(err) || providers.IsNotFound(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
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
	_, err := h.p.ReadFile(ctx, shaBase, "apps/deploy.yaml")
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

func mustRead(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
