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

// TestPullRequestReadsGoThroughChangeOf fails when a function of this package outside pullRequestReads calls one
// of its methods.
func TestPullRequestReadsGoThroughChangeOf(t *testing.T) {
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
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				allowed, read := pullRequestReads[sel.Sel.Name]
				if read && !allowed[funcName(fn)] {
					t.Errorf("%s: %s calls %s; read pull requests through changeOf", fset.Position(call.Pos()),
						funcName(fn), sel.Sel.Name)
				}
				return true
			})
		}
	}
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
