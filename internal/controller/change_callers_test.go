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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// pullRequestReads are the GitProvider methods that read pull requests, or a branch (BranchHead), with the
// functions of this package allowed to call them. Every other function asks changeOf (#688), so one rule per
// verdict holds for all of them. gatedGit only passes the calls on (killswitch.go).
var pullRequestReads = map[string]map[string]bool{
	opGet:  {"changeByNumber": true, "gatedGit.GetChangeRequest": true},
	opList: {"changeOf": true, "changeByNumber": true, "changeOnBranch": true, "gatedGit.ListChangeRequests": true},
	opFind: {"gatedGit.FindChangeRequest": true},
	// BranchHead reads a head, never whether a pull request exists: the step reads it to cut the branch (a 404
	// creates it, and CreateBranch refuses one that exists at another commit), the handoff to update it, and the
	// recovery decides a 404 through changeOf.
	opBranchHead: {"pullRequestStep": true, "takeOver": true, "recoverStep": true, "gatedGit.BranchHead": true},
}

// TestPullRequestReadsGoThroughChangeOf fails when code of this package outside the functions pullRequestReads
// allows refers to one of its methods: a call, a method value (f := git.GetChangeRequest) or a method expression,
// in a function or in a package-level initializer, which no function owns and so is never allowed.
func TestPullRequestReadsGoThroughChangeOf(t *testing.T) {
	forEachSource(t, func(fset *token.FileSet, owner string, n ast.Node) {
		if read, ok := forbiddenRead(owner, n); ok {
			t.Errorf("%s: %q refers to %s; read pull requests through changeOf", fset.Position(n.Pos()), owner, read)
		}
	})
}

// forbiddenRead reports whether n, in the function owner ("" for a package-level declaration), refers to a
// method of pullRequestReads that owner may not use, and which.
func forbiddenRead(owner string, n ast.Node) (string, bool) {
	sel, ok := n.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	allowed, read := pullRequestReads[sel.Sel.Name]
	return sel.Sel.Name, read && !allowed[owner]
}

// forEachSource calls visit for every node of the non-test files of this package, with the name of the function
// it is in (funcName), or "" for a package-level declaration.
func forEachSource(t *testing.T, visit func(fset *token.FileSet, owner string, n ast.Node)) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name) // #nosec G304 -- a file of this package, listed by Glob.
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		forEachDecl(f, func(owner string, n ast.Node) { visit(fset, owner, n) })
	}
}

// forEachDecl calls visit for every node of f, with the name of the function it is in, or "".
func forEachDecl(f *ast.File, visit func(owner string, n ast.Node)) {
	for _, decl := range f.Decls {
		owner := ""
		if fn, ok := decl.(*ast.FuncDecl); ok {
			owner = funcName(fn)
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			if n != nil {
				visit(owner, n)
			}
			return true
		})
	}
}

// TestPullRequestReadGuardCatchesEveryReference checks the guard above on code it must refuse: a method value
// and a package-level initializer, besides a call.
func TestPullRequestReadGuardCatchesEveryReference(t *testing.T) {
	const src = `package controller
var get = providers.GitProvider.GetChangeRequest
func other(git providers.GitProvider) {
	f := git.ListChangeRequests
	_ = f
	_, _ = git.GetChangeRequest(nil, 1)
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	forEachDecl(f, func(owner string, n ast.Node) {
		if read, ok := forbiddenRead(owner, n); ok {
			found = append(found, owner+":"+read)
		}
	})
	want := []string{":GetChangeRequest", "other:ListChangeRequests", "other:GetChangeRequest"}
	if !slices.Equal(found, want) {
		t.Errorf("found %v, want %v", found, want)
	}
}

// lockWriters are the functions that may assign status.holdsTarget, and releasers those that may call release.
// release is the one place that writes it false; the release on a pull request's account goes through the lock
// rule only (settleLock, checkHeld with openHolding). The others release a lock that no pull request of the
// request holds: a refusal or a queued duplicate never took it, and the two ends of a handoff (endHandoff,
// supersedePredecessor) leave the pull request to the replacement, which holds it.
var (
	lockWriters = map[string]bool{"release": true, "reconcile": true, "backfill": true}
	releasers   = map[string]bool{
		"settleLock": true, "checkHeld": true, "reconcile": true, "supersedeDuplicate": true,
		"endHandoff": true, "supersedePredecessor": true,
	}
)

// TestLockClearedInOnePlace fails when a function outside lockWriters assigns status.holdsTarget, a function
// outside releasers calls release, or reconcile and backfill write it false (#688): a caller such as the
// deadline's cleanup, which closes only the pull requests with this request's marker, never releases the lock on
// its own.
func TestLockClearedInOnePlace(t *testing.T) {
	forEachSource(t, func(fset *token.FileSet, owner string, n ast.Node) {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "HoldsTarget" {
					continue
				}
				if !lockWriters[owner] {
					t.Errorf("%s: %q writes status.holdsTarget; release the lock through release", fset.Position(sel.Pos()), owner)
				}
				if owner != "release" && i < len(n.Rhs) && writesFalse(n.Rhs[i]) {
					t.Errorf("%s: %q writes status.holdsTarget false; use release", fset.Position(sel.Pos()), owner)
				}
			}
		case *ast.KeyValueExpr:
			if id, ok := n.Key.(*ast.Ident); ok && id.Name == "HoldsTarget" {
				t.Errorf("%s: %q sets status.holdsTarget in a literal", fset.Position(id.Pos()), owner)
			}
		case *ast.CallExpr:
			if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "release" && !releasers[owner] {
				t.Errorf("%s: %q releases the lock; only the lock rule does (settleLock, checkHeld)",
					fset.Position(id.Pos()), owner)
			}
		}
	})
}

// writesFalse reports whether e is a literal false or new(false), ptr.To(false) and the like.
func writesFalse(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name == "false"
	case *ast.CallExpr:
		return len(e.Args) == 1 && writesFalse(e.Args[0])
	}
	return false
}

// funcName is the name of fn, with the receiver's type for a method of gatedGit.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok && id.Name == "gatedGit" {
		return "gatedGit." + fn.Name.Name
	}
	return fn.Name.Name
}

// TestEveryRuleCoversEveryVerdict checks that each caller's rule table has an entry for every verdict, and that
// unknown never ends, releases or removes anything: it keeps the lock, retries the adoption, the finalizer's close
// and the recovery.
func TestEveryRuleCoversEveryVerdict(t *testing.T) {
	check := func(name string, n int, unknownOK bool) {
		t.Helper()
		if n != len(changeStates) {
			t.Errorf("%s has %d of %d verdicts", name, n, len(changeStates))
		}
		if !unknownOK {
			t.Errorf("%s does not retry or keep on unknown", name)
		}
	}
	check("lockRule", len(lockRule), lockRule[changeUnknown] == lockKeep)
	check("adoptRule", len(adoptRule), adoptRule[changeUnknown] == adoptRetry)
	check("finalizeRule", len(finalizeRule), finalizeRule[changeUnknown] == finalizeRetry)
	check("recoverRule", len(recoverRule), recoverRule[changeUnknown] == recoverRetry)
	// The zero value of each outcome is the safe one, so a verdict missing from a table keeps and retries.
	if lockKeep != 0 || adoptRetry != 0 || finalizeRetry != 0 || recoverRetry != 0 {
		t.Error("the zero outcome of a rule must keep and retry")
	}
}
