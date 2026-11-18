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

package redact

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The cost tests count work with tick ([Work]), so a loop that does not count its turns is work they cannot
// see: a loop over the later keys of a line, run for each key, made String quadratic in a line of many keys
// without any cost test noticing (#845, decision of 2026-11-18). Every loop of the package counts its turns
// with a tick call in its own body (not in a nested loop or function), or is listed in boundedLoops with the
// constant that bounds its turns.

// boundedLoops are the loops that do not count their turns, by function and by the loop's place among the
// loops of that function (0 for the first, in source order), each with the constant that bounds it.
var boundedLoops = map[string]string{
	"(*bracketCursor).at#1": "maxStripClosers brackets",
	"viewEnvLine#0":         matchPairs,
	"viewEnvValueFirst#1":   matchPairs,
	"viewKeyLine#1":         matchPairs,
}

// matchPairs is the bound of a loop over the index pairs of one regular expression match.
const matchPairs = "the index pairs of one regular expression match, fixed by the expression"

// loopName returns the name a loop has in boundedLoops: the function it is in, and its place there.
func loopName(fn *ast.FuncDecl, n int) string {
	name := fn.Name.Name
	if fn.Recv != nil && len(fn.Recv.List) == 1 {
		var b strings.Builder
		switch t := fn.Recv.List[0].Type.(type) {
		case *ast.StarExpr:
			fmt.Fprintf(&b, "(*%s)", t.X.(*ast.Ident).Name)
		case *ast.Ident:
			b.WriteString(t.Name)
		}
		name = b.String() + "." + name
	}
	return fmt.Sprintf("%s#%d", name, n)
}

// ticksInBody reports whether body calls tick as one of its own statements, so that every turn counts: a tick
// inside an if, a switch, a nested loop or a function literal does not.
func ticksInBody(body *ast.BlockStmt) bool {
	for _, st := range body.List {
		if e, ok := st.(*ast.ExprStmt); ok {
			if c, ok := e.X.(*ast.CallExpr); ok {
				if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "tick" {
					return true
				}
			}
		}
	}
	return false
}

// packageLoops returns, for each non-test file of the package, the loops of its functions by name, and for
// each whether it ticks.
func packageLoops(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	loops := map[string]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			n := 0
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				var body *ast.BlockStmt
				switch l := node.(type) {
				case *ast.ForStmt:
					body = l.Body
				case *ast.RangeStmt:
					body = l.Body
				default:
					return true
				}
				name := loopName(fn, n)
				if _, dup := loops[name]; dup {
					t.Fatalf("two functions named %s", name)
				}
				loops[name] = ticksInBody(body)
				n++
				return true
			})
		}
	}
	return loops
}

// Every loop of the package counts its turns or is listed with its bound, and every listed loop exists and
// does not count (a listing that outlives its loop would excuse the next loop given that place).
func TestEveryLoopCountsItsWork(t *testing.T) {
	loops := packageLoops(t)
	names := make([]string, 0, len(loops))
	for n := range loops {
		names = append(names, n)
	}
	slices.Sort(names)
	counted, listed := 0, 0
	for _, n := range names {
		_, ok := boundedLoops[n]
		switch {
		case loops[n] && ok:
			t.Errorf("loop %s counts its turns and is listed in boundedLoops too: remove the listing", n)
		case loops[n]:
			counted++
		case ok:
			listed++
		default:
			t.Errorf("loop %s neither calls tick in its body nor is listed in boundedLoops with its bound", n)
		}
	}
	for n := range boundedLoops {
		if _, ok := loops[n]; !ok {
			t.Errorf("boundedLoops lists %s, which is no loop of the package", n)
		}
	}
	t.Logf("%d loops: %d count their turns, %d are listed with their bound", len(loops), counted, listed)
}
