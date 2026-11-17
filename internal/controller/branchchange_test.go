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

package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// Names of the fake's Git calls a test fails on purpose.
const (
	opParents      = "Parents"
	opCountAhead   = "CountAhead"
	opCheckBetween = "CheckBetween"
)

// mergeMethod is how a person merged the retargeted pull request on the fake.
type mergeMethod int

const (
	viaSquash mergeMethod = iota
	viaRebase
	viaMergeCommit
)

func (m mergeMethod) String() string {
	return [...]string{"squash", "rebase", "merge commit"}[m]
}

func (m mergeMethod) merge(g *fakegit.Git, n int64) (string, error) {
	switch m {
	case viaRebase:
		return g.RebaseMerge(n)
	case viaMergeCommit:
		return g.MergeWithCommit(n)
	}
	return g.Merge(n)
}

// retargetedWorld is a repository whose recorded branch main and configured branch release share their first
// commit, which holds the request's target file with the previous value.
type retargetedWorld struct {
	t   *testing.T
	git *fakegit.Git
	rr  *remediationv1alpha1.RemediationRequest
	// previous is the target file as the first commit holds it.
	previous []byte
}

func newRetargetedWorld(t *testing.T) *retargetedWorld {
	t.Helper()
	_, rr := plannedWorld(t)
	previous := deploymentManifest(appName, fx.TargetNamespace, workerContainer, "256Mi")
	g := fakegit.NewGit(mainBranch, map[string][]byte{sourcePath: previous, readmePath: []byte("a: 1\n")})
	w := &retargetedWorld{t: t, git: g, rr: rr, previous: previous}
	if err := g.CreateBranch(context.Background(), freshOtherBranch, w.head(mainBranch)); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *retargetedWorld) head(branch string) string {
	w.t.Helper()
	sha, err := w.git.BranchHead(context.Background(), branch)
	if err != nil {
		w.t.Fatal(err)
	}
	return sha
}

func (w *retargetedWorld) push(branch, path string, data []byte) string {
	w.t.Helper()
	sha, err := w.git.Push(branch, path, data)
	if err != nil {
		w.t.Fatal(err)
	}
	return sha
}

// file is the target file at commit.
func (w *retargetedWorld) file(commit string) []byte {
	w.t.Helper()
	f, err := w.git.ReadFile(context.Background(), commit, sourcePath, providers.MaxFileBytes)
	if err != nil {
		w.t.Fatal(err)
	}
	return f.Data
}

// planned is the planned change of data, which must hold the previous value.
func (w *retargetedWorld) planned(data []byte) []byte {
	w.t.Helper()
	out, state, err := planned(data, w.rr)
	if err != nil || state != fileHoldsPrevious {
		w.t.Fatalf("planned() = %v, %v", state, err)
	}
	return out
}

// withLimit is data, which holds the previous memory limit, with the limit set to to.
func withLimit(data []byte, to string) []byte {
	return bytes.Replace(data, []byte("memory: 256Mi"), []byte("memory: "+to), 1)
}

// withReplicas is data with a foreign change to another field of the same object.
func withReplicas(data []byte) []byte {
	return bytes.Replace(data, []byte("spec:\n  template"), []byte("spec:\n  replicas: 7\n  template"), 1)
}

// branchOff makes the request's branch at commit from, opens its pull request on main and moves it to release.
func (w *retargetedWorld) branchOff(from string) int64 {
	w.t.Helper()
	ctx := context.Background()
	if err := w.git.CreateBranch(ctx, "shadewright/rr", from); err != nil {
		w.t.Fatal(err)
	}
	cr, err := w.git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: "shadewright/rr", BaseBranch: mainBranch})
	if err != nil {
		w.t.Fatal(err)
	}
	return cr.Number
}

// mergeInto moves pull request n to release and merges it with m, and returns it as the Git host reports it then.
func (w *retargetedWorld) mergeInto(n int64, m mergeMethod) providers.ChangeRequest {
	w.t.Helper()
	if err := w.git.SetBase(n, freshOtherBranch); err != nil {
		w.t.Fatal(err)
	}
	head := w.head("shadewright/rr")
	if _, err := m.merge(w.git, n); err != nil {
		w.t.Fatal(err)
	}
	for _, cr := range w.git.ChangeRequests() {
		if cr.Number == n {
			cr.HeadSHA = head
			return cr
		}
	}
	w.t.Fatalf("no change request %d", n)
	return providers.ChangeRequest{}
}

func (w *retargetedWorld) content(cr providers.ChangeRequest) (*renderRefusal, error) {
	return retargetedContent(context.Background(), w.git, w.rr, cr, freshOtherBranch)
}

// #771: what a merge into the configured branch brought is compared as a whole with the branch's head before the
// merge, for every merge method. A head built on commits of the recorded branch that the configured branch lacks
// never counts, also when such a commit changes the plan's own file and the merge replays it (a rebase merge).
func TestRetargetedContentComparesWholeMerge(t *testing.T) {
	methods := []mergeMethod{viaSquash, viaRebase, viaMergeCommit}
	for _, m := range methods {
		t.Run("planned change alone counts, "+m.String(), func(t *testing.T) {
			w := newRetargetedWorld(t)
			w.push(freshOtherBranch, "NOTES.yaml", []byte("n: 1\n")) // release moved on meanwhile
			n := w.branchOff(w.head(mainBranch))
			w.push("shadewright/rr", sourcePath, w.planned(w.previous))
			if ref, err := w.content(w.mergeInto(n, m)); err != nil || ref != nil {
				t.Fatalf("retargetedContent() = %v, %v; want the merge counted", ref, err)
			}
		})
		t.Run("a foreign commit to the plan's file is refused, "+m.String(), func(t *testing.T) {
			w := newRetargetedWorld(t)
			// C1 on the recorded branch only: another field of the same file. The head carries C1 and the change.
			c1 := w.push(mainBranch, sourcePath, withReplicas(w.previous))
			n := w.branchOff(c1)
			w.push("shadewright/rr", sourcePath, w.planned(w.file(c1)))
			ref, err := w.content(w.mergeInto(n, m))
			if err != nil || ref == nil || ref.reason != ReasonMergedWithForeignChange {
				t.Fatalf("retargetedContent() = %v, %v; want MergedWithForeignChange", ref, err)
			}
		})
		t.Run("a head whose file is more than the planned change is refused, "+m.String(), func(t *testing.T) {
			w := newRetargetedWorld(t)
			n := w.branchOff(w.head(mainBranch))
			w.push("shadewright/rr", sourcePath, withReplicas(w.planned(w.previous)))
			ref, err := w.content(w.mergeInto(n, m))
			if err != nil || ref == nil || ref.reason != ReasonMergedWithForeignChange {
				t.Fatalf("retargetedContent() = %v, %v; want MergedWithForeignChange", ref, err)
			}
		})
		t.Run("a foreign commit to another file is refused, "+m.String(), func(t *testing.T) {
			w := newRetargetedWorld(t)
			c1 := w.push(mainBranch, readmePath, []byte("a: 2\n"))
			n := w.branchOff(c1)
			w.push("shadewright/rr", sourcePath, w.planned(w.previous))
			ref, err := w.content(w.mergeInto(n, m))
			if err != nil || ref == nil || ref.reason != ReasonMergedWithForeignChange {
				t.Fatalf("retargetedContent() = %v, %v; want MergedWithForeignChange", ref, err)
			}
		})
	}
	// A chain of Shadewright's own commits (ADR-0028 supersede chain) replayed by a rebase: the whole of it is
	// compared, not only its last commit, which alone does not hold the previous value at its parent.
	for _, m := range []mergeMethod{viaRebase, viaMergeCommit} {
		t.Run("a chain of two own commits counts, "+m.String(), func(t *testing.T) {
			w := newRetargetedWorld(t)
			n := w.branchOff(w.head(mainBranch))
			w.push("shadewright/rr", sourcePath, withLimit(w.previous, "300Mi"))
			w.push("shadewright/rr", sourcePath, w.planned(w.previous))
			if ref, err := w.content(w.mergeInto(n, m)); err != nil || ref != nil {
				t.Fatalf("retargetedContent() = %v, %v; want the merge counted", ref, err)
			}
		})
	}
	t.Run("more commits than a Shadewright branch carries are refused", func(t *testing.T) {
		w := newRetargetedWorld(t)
		n := w.branchOff(w.head(mainBranch))
		for i := range providers.MaxChainCommits {
			w.push("shadewright/rr", sourcePath, withLimit(w.previous, fmt.Sprintf("%dMi", 300+i)))
		}
		w.push("shadewright/rr", sourcePath, w.planned(w.previous))
		ref, err := w.content(w.mergeInto(n, viaRebase))
		if err != nil || ref == nil {
			t.Fatalf("retargetedContent() = %v, %v; want a refusal", ref, err)
		}
	})
	t.Run("a merge commit that does not take in the head is refused", func(t *testing.T) {
		w := newRetargetedWorld(t)
		n := w.branchOff(w.head(mainBranch))
		other := w.push("shadewright/rr", sourcePath, w.planned(w.previous))
		cr := w.mergeInto(n, viaMergeCommit)
		// The same content under another commit: the head Shadewright pushed is not what was merged.
		if err := w.git.CreateBranch(context.Background(), "elsewhere", w.head(mainBranch)); err != nil {
			t.Fatal(err)
		}
		cr.HeadSHA = w.push("elsewhere", sourcePath, w.planned(w.previous))
		if cr.HeadSHA == other {
			t.Fatal("the two heads are the same commit")
		}
		ref, err := w.content(cr)
		if err != nil || ref == nil {
			t.Fatalf("retargetedContent() = %v, %v; want a refusal", ref, err)
		}
	})
	t.Run("a head taken in as it is is refused", func(t *testing.T) {
		w := newRetargetedWorld(t)
		n := w.branchOff(w.head(mainBranch))
		w.push("shadewright/rr", sourcePath, w.planned(w.previous))
		cr := w.mergeInto(n, viaSquash)
		cr.MergeCommit = cr.HeadSHA
		if ref, err := w.content(cr); err != nil || ref == nil {
			t.Fatalf("retargetedContent() = %v, %v; want a refusal", ref, err)
		}
	})
	t.Run("a walk that meets a merge commit is refused", func(t *testing.T) {
		w := newRetargetedWorld(t)
		// release ends in a merge commit; a squash of a two-commit head then walks onto it.
		side := w.branchOff(w.head(mainBranch))
		w.push("shadewright/rr", "NOTES.yaml", []byte("n: 1\n"))
		w.mergeInto(side, viaMergeCommit)
		ctx := context.Background()
		if err := w.git.CreateBranch(ctx, "shadewright/two", w.head(mainBranch)); err != nil {
			t.Fatal(err)
		}
		cr, err := w.git.OpenChangeRequest(ctx, providers.NewChangeRequest{HeadBranch: "shadewright/two",
			BaseBranch: freshOtherBranch})
		if err != nil {
			t.Fatal(err)
		}
		w.push("shadewright/two", sourcePath, withLimit(w.previous, "300Mi"))
		head := w.push("shadewright/two", sourcePath, w.planned(w.previous))
		merge, err := w.git.Merge(cr.Number)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := w.content(providers.ChangeRequest{Number: cr.Number, HeadSHA: head, MergeCommit: merge,
			State: providers.ChangeRequestMerged})
		if err != nil || ref == nil {
			t.Fatalf("retargetedContent() = %v, %v; want a refusal", ref, err)
		}
	})
}

// #771: every read of the content check that fails is an error for a retry, never an answer that counts or
// refuses the merge.
func TestRetargetedContentFailedReadsAreErrors(t *testing.T) {
	for _, m := range []mergeMethod{viaSquash, viaRebase, viaMergeCommit} {
		for _, op := range []string{opParents, opCountAhead, opCheckBetween, opReadFile} {
			t.Run(m.String()+" "+op, func(t *testing.T) {
				w := newRetargetedWorld(t)
				n := w.branchOff(w.head(mainBranch))
				if m != viaSquash {
					// Two commits, so the rebase walks one parent more.
					w.push("shadewright/rr", sourcePath, withLimit(w.previous, "300Mi"))
				}
				w.push("shadewright/rr", sourcePath, w.planned(w.previous))
				cr := w.mergeInto(n, m)
				failed := errors.New("the Git host failed")
				calls := 0
				w.git.Fail = func(name string) error {
					if name == op {
						calls++
						return failed
					}
					return nil
				}
				ref, err := w.content(cr)
				if m == viaMergeCommit && op == opCountAhead {
					// A merge commit needs no count.
					if err != nil || ref != nil || calls != 0 {
						t.Fatalf("retargetedContent() = %v, %v after %d calls; want the merge counted", ref, err, calls)
					}
					return
				}
				if err == nil || ref != nil || calls == 0 {
					t.Fatalf("retargetedContent() = %v, %v; want the failure and no answer", ref, err)
				}
			})
		}
	}
}

// #827: the configured branch changes the README and reverts it, and the pull request carries the recorded
// branch's README change and the planned change. Against the commit the merge was made on, the merge brought the
// README change as well, whatever the merge method; against the earlier commit that held the same README it would
// look like the planned change alone.
func TestRetargetedContentComparesWithTheRealParent(t *testing.T) {
	for _, m := range []mergeMethod{viaSquash, viaRebase, viaMergeCommit} {
		t.Run(m.String(), func(t *testing.T) {
			w := newRetargetedWorld(t)
			w.push(freshOtherBranch, readmePath, []byte("a: 2\n"))
			w.push(freshOtherBranch, readmePath, []byte("a: 1\n"))
			c1 := w.push(mainBranch, readmePath, []byte("a: 2\n"))
			n := w.branchOff(c1)
			w.push("shadewright/rr", sourcePath, w.planned(w.previous))
			ref, err := w.content(w.mergeInto(n, m))
			if err != nil || ref == nil || ref.reason != ReasonMergedWithForeignChange {
				t.Fatalf("retargetedContent() = %v, %v; want MergedWithForeignChange", ref, err)
			}
		})
	}
}
