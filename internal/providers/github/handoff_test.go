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
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

// shaMid is the apps tree of shaOld, the first commit of a two-commit chain.
const shaMid = "3333333333333333333333333333333333333333"

// commitOf is a Git commit of the REST API: sha with parents.
func commitOf(sha string, parents ...string) string {
	return strings.Replace(commitBody(parents...), `"sha":"`+shaCommit, `"sha":"`+sha, 1)
}

// diverged is the compare step that answers that commit is not on the target branch.
func diverged(commit string) step {
	s := get("/compare/"+shaBase+"..."+commit, "compare_diverged.json")
	s.query = perPageOne
	return s
}

// chainTrees gives the trees of a chain shaCommit -> shaOld -> shaBase where each commit changes only
// apps/deploy.yaml: shaCommit's apps tree is shaNewTree, shaOld's is shaMid.
func chainTrees(t *testing.T) (root, apps, newRoot, newApps, midRoot, midApps string) {
	root, apps = mustRead(t, "tree_root.json"), mustRead(t, "tree_apps.json")
	newRoot = strings.ReplaceAll(root, shaApps, shaNewTree)
	newApps = strings.ReplaceAll(apps, shaDeploy, shaNewBlob)
	midRoot = strings.ReplaceAll(root, shaApps, shaMid)
	midApps = strings.ReplaceAll(apps, shaDeploy, shaOld)
	return
}

func TestCheckChain(t *testing.T) {
	root, apps, newRoot, newApps, midRoot, midApps := chainTrees(t)
	// top is shaCommit off the target, its parent shaOld and the trees of the two.
	top := func(middleApps string) []step {
		return []step{get(mainRef, "ref_main.json"), diverged(shaCommit), raw("/git/commits/"+shaCommit, commitOf(shaCommit, shaOld)),
			raw("/git/trees/"+shaCommit, newRoot), raw("/git/trees/"+shaOld, midRoot),
			raw("/git/trees/"+shaNewTree, newApps), raw("/git/trees/"+shaMid, middleApps),
			get(mainRef, "ref_main.json"), diverged(shaOld)}
	}
	// below is shaOld with parent and its trees against shaBase.
	below := func(parent string) []step {
		return []step{raw("/git/commits/"+shaOld, commitOf(shaOld, parent)),
			raw("/git/trees/"+shaOld, midRoot), raw("/git/trees/"+parent, root),
			raw("/git/trees/"+shaMid, midApps), raw("/git/trees/"+shaApps, apps)}
	}
	for name, c := range map[string]struct {
		commits int
		steps   []step
		want    providers.BranchState
	}{
		"two commits on the target": {2, slices.Concat(top(midApps), below(shaBase), []step{get(mainRef, "ref_main.json")}),
			providers.BranchOneChange},
		"a chain longer than allowed": {2, slices.Concat(top(midApps), below(shaApps), []step{get(mainRef, "ref_main.json"),
			diverged(shaApps)}), providers.BranchForeign},
		"another file in the top commit": {2, top(strings.ReplaceAll(midApps, shaRun, shaOld))[:7], providers.BranchForeign},
		"a merge below": {2, append(top(midApps), raw("/git/commits/"+shaOld, commitOf(shaOld, shaBase, shaApps))),
			providers.BranchForeign},
		"the head on the target": {3, []step{get(mainRef, "ref_main.json"), behindStep(shaOld)}, providers.BranchAtTarget},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, c.steps...)
			head := shaCommit
			if c.want == providers.BranchAtTarget {
				head = shaOld
			}
			got, err := h.p.CheckChain(ctx, head, deployPath, c.commits)
			if err != nil || got != c.want {
				t.Fatalf("CheckChain() = %v, %v, want %v", got, err, c.want)
			}
		})
	}
}

func behindStep(commit string) step {
	s := get("/compare/"+shaBase+"..."+commit, "compare_behind.json")
	s.query = perPageOne
	return s
}

func TestCheckChainOneCommitIsCheckBranch(t *testing.T) {
	h := newHarness(t, get(mainRef, "ref_main.json"))
	if got, err := h.p.CheckChain(ctx, shaBase, deployPath, 1); err != nil || got != providers.BranchAtTarget {
		t.Fatalf("CheckChain() = %v, %v", got, err)
	}
}

func TestCheckChainFailsClosed(t *testing.T) {
	h := newHarness(t, get(mainRef, "ref_main.json"), diverged(shaCommit),
		status(http.MethodGet, "/git/commits/"+shaCommit, http.StatusNotFound, ""))
	got, err := h.p.CheckChain(ctx, shaCommit, deployPath, 2)
	if !providers.IsNotFound(err) || got != 0 {
		t.Fatalf("CheckChain() = %v, %v, want an error and no answer", got, err)
	}
	assertSafe(t, err)
}

func TestCheckChainRefusesBadInput(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct {
		commit, path string
		commits      int
	}{
		{shaCommit, deployPath, 0}, {shaCommit, deployPath, providers.MaxChainCommits + 1},
		{"1234", deployPath, 2}, {shaCommit, "/apps/x", 2},
	} {
		if _, err := h.p.CheckChain(ctx, c.commit, c.path, c.commits); err == nil {
			t.Errorf("CheckChain(%q, %q, %d) was allowed", c.commit, c.path, c.commits)
		}
	}
}

func TestParents(t *testing.T) {
	h := newHarness(t, raw("/git/commits/"+shaCommit, commitOf(shaCommit, shaOld, shaBase)))
	got, err := h.p.Parents(ctx, shaCommit)
	if err != nil || !slices.Equal(got, []string{shaOld, shaBase}) {
		t.Fatalf("Parents() = %v, %v", got, err)
	}
	if _, err := h.p.Parents(ctx, "12345"); err == nil {
		t.Error("a short SHA was allowed")
	}
}

func TestParentsRefusesAnotherCommit(t *testing.T) {
	h := newHarness(t, raw("/git/commits/"+shaCommit, commitOf(shaOld, shaBase)))
	if _, err := h.p.Parents(ctx, shaCommit); err == nil {
		t.Fatal("the parents of another commit were returned")
	}
}

// appendSteps is what AppendCommit sends when the branch is at shaOld, a commit of its own: the head, the
// target check, the file read at shaOld, the blob, tree and commit on top of shaOld and the compare-and-swap.
func appendSteps(t *testing.T) []step {
	_, _, _, _, midRoot, midApps := chainTrees(t)
	at := raw(branchRef, strings.ReplaceAll(mustRead(t, "ref_branch.json"), shaBase, shaOld))
	blob := status(http.MethodPost, "/git/blobs", http.StatusCreated, "blob_created.json")
	tree := status(http.MethodPost, "/git/trees", http.StatusCreated, "tree_created.json")
	tree.check = func(t *testing.T, b map[string]any) {
		if b["base_tree"] != shaTree {
			t.Errorf("tree body = %v", b)
		}
	}
	commit := status(http.MethodPost, "/git/commits", http.StatusCreated, "commit_created.json")
	commit.check = func(t *testing.T, b map[string]any) {
		want := `{"message":"fix: raise replicas","parents":["` + shaOld + `"],"tree":"` + shaNewTree + `"}`
		if got, _ := json.Marshal(b); string(got) != want {
			t.Errorf("commit body = %s", got)
		}
	}
	update := status(http.MethodPost, graphQLPath, http.StatusOK, "graphql_update_refs.json")
	update.check = func(t *testing.T, b map[string]any) {
		vars, _ := b["variables"].(map[string]any)
		got, _ := json.Marshal(vars)
		want := `{"repo":"R_kgDOAAAAAQ","updates":[{"afterOid":"` + shaCommit + `","beforeOid":"` + shaOld +
			`","force":false,"name":"refs/heads/shadewright/req-1"}]}`
		if string(got) != want {
			t.Errorf("updateRefs variables = %s, want %s", got, want)
		}
	}
	return []step{at, get(mainRef, "ref_main.json"), diverged(shaOld),
		raw("/git/trees/"+shaOld, midRoot), raw("/git/trees/"+shaMid, midApps),
		blob, tree, commit, get("", "repo.json"), update}
}

func TestAppendCommit(t *testing.T) {
	h := newHarness(t, appendSteps(t)...)
	sha, err := h.p.AppendCommit(ctx, branch, shaOld, change)
	if err != nil || sha != shaCommit {
		t.Fatalf("AppendCommit() = %q, %v", sha, err)
	}
}

func TestAppendCommitHeadMovedIsConflict(t *testing.T) {
	h := newHarness(t, get(branchRef, "ref_branch_moved.json")) // nothing is written
	_, err := h.p.AppendCommit(ctx, branch, shaOld, change)
	if !providers.IsConflict(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
}

func TestAppendCommitRefusesAHeadOnTheTarget(t *testing.T) {
	// The branch carries no commit of its own: the first commit is CommitFile's, never an append.
	h := newHarness(t, get(branchRef, "ref_branch.json"), get(mainRef, "ref_main.json"))
	_, err := h.p.AppendCommit(ctx, branch, shaBase, change)
	if err == nil || providers.IsConflict(err) || providers.IsRetryable(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
}

func TestAppendCommitRaceIsConflict(t *testing.T) {
	steps := appendSteps(t)
	steps[len(steps)-1].file = "graphql_update_refs_stale.json"
	h := newHarness(t, append(steps, get(branchRef, "ref_branch_moved.json"))...)
	_, err := h.p.AppendCommit(ctx, branch, shaOld, change)
	if !providers.IsConflict(err) {
		t.Fatalf("got %v", err)
	}
}

func TestAppendCommitRefusesBadInput(t *testing.T) {
	h := newHarness(t)
	for name, c := range map[string]struct {
		branch, head string
		change       providers.FileChange
	}{
		"target branch":  {target, shaOld, change},
		"head not a SHA": {branch, "1234", change},
		"invalid path":   {branch, shaOld, providers.FileChange{Path: "apps/../x", Content: []byte("x"), Message: "m"}},
		"no message":     {branch, shaOld, providers.FileChange{Path: deployPath, Content: []byte("x"), Message: " "}},
		"too large": {branch, shaOld, providers.FileChange{Path: deployPath,
			Content: make([]byte, providers.MaxFileBytes+1), Message: "m"}},
	} {
		if _, err := h.p.AppendCommit(ctx, c.branch, c.head, c.change); err == nil {
			t.Errorf("%s was allowed", name)
		}
	}
}

const (
	newTitle = "fix(apps): raise replicas to 4"
	newBody  = "Raise replicas to 4."
)

func TestUpdateChangeRequest(t *testing.T) {
	patch := status(http.MethodPatch, "/pulls/12", http.StatusOK, "pull_open.json")
	patch.check = func(t *testing.T, b map[string]any) {
		if b["title"] != newTitle || b["body"] != newBody || len(b) != 2 {
			t.Errorf("update body = %v", b)
		}
	}
	h := newHarness(t, get("/pulls/12", "pull_open.json"), patch)
	if err := h.p.UpdateChangeRequest(ctx, 12, newTitle, newBody); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateChangeRequestUnchangedSendsNothing(t *testing.T) {
	h := newHarness(t, get("/pulls/12", "pull_open.json")) // no PATCH
	err := h.p.UpdateChangeRequest(ctx, 12, "fix(apps): raise replicas",
		"Raise replicas.\n\n<!-- shadewright: uid=req-1 digest=sha256:abc -->")
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpdateChangeRequestNotOpenIsConflict(t *testing.T) {
	for _, file := range []string{"pull_closed.json", "pull_merged.json"} {
		h := newHarness(t, get("/pulls/12", file))
		if err := h.p.UpdateChangeRequest(ctx, 12, newTitle, newBody); !providers.IsConflict(err) {
			t.Fatalf("%s: got %v", file, err)
		}
	}
}

func TestUpdateChangeRequestMergedMeanwhile(t *testing.T) {
	h := newHarness(t, get("/pulls/12", "pull_open.json"),
		status(http.MethodPatch, "/pulls/12", http.StatusUnprocessableEntity, fxForbidden),
		get("/pulls/12", "pull_merged.json"))
	if err := h.p.UpdateChangeRequest(ctx, 12, newTitle, newBody); !providers.IsConflict(err) {
		t.Fatalf("got %v", err)
	}
}

func TestUpdateChangeRequestRefuses(t *testing.T) {
	h := newHarness(t, get("/pulls/13", "pull_foreign.json"))
	if err := h.p.UpdateChangeRequest(ctx, 13, newTitle, newBody); err == nil {
		t.Fatal("a pull request Shadewright did not open was updated")
	}
	for name, c := range map[string]struct {
		number      int64
		title, body string
	}{
		"number 0":     {0, newTitle, newBody},
		"blank title":  {12, " ", newBody},
		"invalid body": {12, newTitle, "\xff"},
	} {
		if err := h.p.UpdateChangeRequest(ctx, c.number, c.title, c.body); err == nil {
			t.Errorf("%s was allowed", name)
		}
	}
}

var run = providers.CheckRun{Name: providers.CheckRunName, Status: providers.CheckRunInProgress,
	Title: "Waiting for approval", Summary: "The plan changed: approve it again."}

func TestCreateCheckRun(t *testing.T) {
	post := status(http.MethodPost, "/check-runs", http.StatusCreated, "")
	post.raw = `{"id":1}`
	post.check = func(t *testing.T, b map[string]any) {
		want := `{"head_sha":"` + shaCommit + `","name":"shadewright/review","output":{"summary":"` + run.Summary +
			`","title":"` + run.Title + `"},"status":"in_progress"}`
		if got, _ := json.Marshal(b); string(got) != want {
			t.Errorf("check run body = %s", got)
		}
	}
	h := newHarness(t, post)
	if err := h.p.CreateCheckRun(ctx, shaCommit, run); err != nil {
		t.Fatal(err)
	}
}

// A completed check run is created with its conclusion: the merge gate's success (ADR-0028 section 5).
func TestCreateCheckRunCompleted(t *testing.T) {
	done := providers.CheckRun{Name: providers.CheckRunName, Status: providers.CheckRunSuccess,
		Title: "Approved", Summary: "Someone who wrote no hint approved the head."}
	post := status(http.MethodPost, "/check-runs", http.StatusCreated, "")
	post.raw = `{"id":2}`
	post.check = func(t *testing.T, b map[string]any) {
		want := `{"conclusion":"success","head_sha":"` + shaCommit + `","name":"shadewright/review","output":{"summary":"` +
			done.Summary + `","title":"` + done.Title + `"},"status":"completed"}`
		if got, _ := json.Marshal(b); string(got) != want {
			t.Errorf("check run body = %s", got)
		}
	}
	h := newHarness(t, post)
	if err := h.p.CreateCheckRun(ctx, shaCommit, done); err != nil {
		t.Fatal(err)
	}
}

func TestCreateCheckRunFailures(t *testing.T) {
	h := newHarness(t, status(http.MethodPost, "/check-runs", http.StatusForbidden, fxForbidden))
	err := h.p.CreateCheckRun(ctx, shaCommit, run)
	if !providers.IsForbidden(err) {
		t.Fatalf("got %v", err)
	}
	assertSafe(t, err)
}

func TestCreateCheckRunRefusesBadInput(t *testing.T) {
	h := newHarness(t)
	other := run
	other.Name = "ci/test"
	done := run
	done.Status = "Completed"
	empty := run
	empty.Title = " "
	long := run
	long.Summary = strings.Repeat("x", providers.MaxCheckRunTextBytes+1)
	for name, c := range map[string]struct {
		head string
		run  providers.CheckRun
	}{
		"head not a SHA": {"1234", run}, "another check": {shaCommit, other}, "another status": {shaCommit, done},
		"blank title": {shaCommit, empty}, "long summary": {shaCommit, long},
	} {
		if err := h.p.CreateCheckRun(ctx, c.head, c.run); err == nil {
			t.Errorf("%s was allowed", name)
		}
	}
}
