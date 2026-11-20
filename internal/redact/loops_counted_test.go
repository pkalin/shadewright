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
	"maps"
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
	// The syntax of an expression is read once, when the redactor is made (newWindowed): never the text.
	"alphabet#0":      exprSyntax,
	"asserts#0":       exprSyntax,
	"laterAlphabet#0": exprSyntax,
	"laterAlphabet#1": exprSyntax,
	"literalRunes#0":  exprSyntax,
	"literalRunes#1":  exprSyntax,
	"minLen#0":        exprSyntax,
	"minLen#1":        exprSyntax,
	"newRuneSet#0":    exprSyntax,
	"newRuneSet#1":    exprSyntax,
	"newRuneSet#2":    exprSyntax,
	// The expressions read by windows, at most 64 (a bit each), and the ASCII characters.
	"longRuns#0":          windowExprCount,
	"longRuns#1":          "utf8.RuneSelf ASCII characters per expression",
	"longRuns#3":          windowExprCount + ", once per distinct character of the text",
	"longRuns#4":          windowExprCount + " per character, in the counted loop over the text",
	"longRuns#5":          windowExprCount + " per character, in the counted loop over the text",
	"(*chunker).finish#0": windowExprCount,
	"chunkCut#0":          "chunkHashBytes bytes, counted by its caller",
	"shifted#0":           matchPairs,
	// The size of an entry, read once before its decision to set its budget (budget.go, #873): no meter exists yet,
	// and a step counted there would be one no budget holds.
	"(*Redactor).StringsChecked#0": entrySize,
	"(*Redactor).meteredPieces#0":  entrySize,
}

// entrySize is the bound of a loop that adds up the lengths of the strings or pieces of one entry.
const entrySize = "the strings or pieces of the entry, once, to size its budget before any work"

// exprSyntax is the bound of a loop over the syntax tree of one expression of the package or of an extra pattern.
const exprSyntax = "the syntax of one expression, read once when the redactor is made"

// windowExprCount is the bound of a loop over the expressions one pass of longRuns reads by windows.
const windowExprCount = "the expressions read by windows, at most 64"

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

// ticksInBody reports whether body calls tick as one of its own statements before any statement that can leave
// the turn early, so that every turn counts: a tick inside an if, a switch, a nested loop or a function literal
// does not, nor one after a statement holding a continue, a break, a goto or a return (#845, third decision of
// 2026-11-18: a tick placed after an early continue does not count the turns that continue).
func ticksInBody(body *ast.BlockStmt) bool {
	for _, st := range body.List {
		if isTick(st) {
			return true
		}
		if leavesEarly(st) {
			return false
		}
	}
	return false
}

// isTick reports whether st is a call of tick.
func isTick(st ast.Stmt) bool {
	if e, ok := st.(*ast.ExprStmt); ok {
		if c, ok := e.X.(*ast.CallExpr); ok {
			if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "tick" {
				return true
			}
		}
	}
	return false
}

// leavesEarly reports whether st holds a branch (continue, break, goto) or a return outside a function literal.
func leavesEarly(st ast.Stmt) bool {
	found := false
	ast.Inspect(st, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.BranchStmt, *ast.ReturnStmt:
			found = true
		}
		return !found
	})
	return found
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

// The loop check also covers what turns without a for statement (#845, third decision of 2026-11-18): a
// function that calls itself, directly or through other functions of the package, and a goto that jumps back
// to a label. Each turn of such a cycle counts with a tick as the first statements of the function's body (or
// of the labeled statement), before anything that can leave it early, or the cycle is listed in boundedCycles
// with the constant that bounds it.

// boundedCycles are the recursions and backward gotos that do not count their turns, each with its bound.
var boundedCycles = map[string]string{
	"alphabet#-1":      exprSyntax,
	"asserts#-1":       exprSyntax,
	"laterAlphabet#-1": exprSyntax,
	"minLen#-1":        exprSyntax,
}

// parsePackage parses the non-test files of the package.
func parsePackage(t *testing.T) []*ast.File {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []*ast.File
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, file)
	}
	return out
}

// recvType returns the name of the type of a method's receiver, or "" for a function.
func recvType(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return ""
	}
	switch t := fn.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// funcKey is the name a function's calls use: the function's name, or "Type.name" for a method.
func funcKey(fn *ast.FuncDecl) string {
	if r := recvType(fn); r != "" {
		return r + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// recvName returns the name of a method's receiver variable, or "".
func recvName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 || len(fn.Recv.List[0].Names) != 1 {
		return ""
	}
	return fn.Recv.List[0].Names[0].Name
}

// cycles returns, for each function of files that is part of a cycle of calls (itself included), whether it
// counts its turns (ticksInBody of its body), by loopName with -1 for the place. A call is a call of a function
// of the package by its name, or of a method on the calling method's own receiver.
func cycles(files []*ast.File) map[string]bool {
	decls := map[string][]*ast.FuncDecl{}
	for _, f := range files {
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
				decls[funcKey(fn)] = append(decls[funcKey(fn)], fn)
			}
		}
	}
	calls := map[string]map[string]bool{}
	for key, fns := range decls {
		calls[key] = map[string]bool{}
		for _, fn := range fns {
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch f := c.Fun.(type) {
				case *ast.Ident:
					if _, ok := decls[f.Name]; ok {
						calls[key][f.Name] = true
					}
				case *ast.SelectorExpr:
					// A method called on the method's own receiver; calls on other values (a builder's String)
					// are no calls of the package's methods.
					if x, ok := f.X.(*ast.Ident); ok && x.Name != "" && x.Name == recvName(fn) {
						if m := recvType(fn) + "." + f.Sel.Name; len(decls[m]) > 0 {
							calls[key][m] = true
						}
					}
				}
				return true
			})
		}
	}
	// reaches reports whether from calls to, through any chain of calls.
	reaches := func(from, to string) bool {
		seen := map[string]bool{}
		stack := []string{from}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for next := range calls[cur] {
				if next == to {
					return true
				}
				if !seen[next] {
					seen[next] = true
					stack = append(stack, next)
				}
			}
		}
		return false
	}
	out := map[string]bool{}
	for key, fns := range decls {
		if !reaches(key, key) {
			continue
		}
		for _, fn := range fns {
			out[loopName(fn, -1)] = ticksInBody(fn.Body)
		}
	}
	return out
}

// backwardGotos returns, for each goto of files that jumps back to a label before it, whether the labeled
// statement is a tick, by the function's loopName and the label.
func backwardGotos(files []*ast.File) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			labels := map[string]*ast.LabeledStmt{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch s := n.(type) {
				case *ast.LabeledStmt:
					labels[s.Label.Name] = s
				case *ast.BranchStmt:
					if s.Tok != token.GOTO || s.Label == nil {
						break
					}
					if l, ok := labels[s.Label.Name]; ok && l.Pos() < s.Pos() {
						out[loopName(fn, -1)+":"+s.Label.Name] = isTick(l.Stmt)
					}
				}
				return true
			})
		}
	}
	return out
}

func TestEveryCycleCountsItsWork(t *testing.T) {
	files := parsePackage(t)
	found := map[string]bool{}
	for _, m := range []map[string]bool{cycles(files), backwardGotos(files)} {
		for name, counts := range m {
			found[name] = true
			_, listed := boundedCycles[name]
			switch {
			case counts && listed:
				t.Errorf("%s counts its turns and is listed in boundedCycles too: remove the listing", name)
			case !counts && !listed:
				t.Errorf("%s turns without a tick before it can leave a turn, and is not listed in boundedCycles", name)
			}
		}
	}
	for name := range boundedCycles {
		if !found[name] {
			t.Errorf("boundedCycles lists %s, which is no cycle of the package", name)
		}
	}
	t.Logf("%d recursions and backward gotos", len(found))
}

// The checks see what they are for: a tick after an early continue, a recursion through another function
// without a tick, and a goto back to a label that is no tick; and they take the counted forms.
func TestLoopChecksSeeEveryForm(t *testing.T) {
	const src = `package p
func tick(int) {}
func early(xs []int) {
	for _, x := range xs {
		if x > 0 {
			continue
		}
		tick(1)
	}
}
func good(xs []int) {
	for range xs {
		tick(1)
		if len(xs) > 0 {
			continue
		}
	}
}
func a(n int) int { if n == 0 { return 0 }; return b(n - 1) }
func b(n int) int { return a(n) }
func c(n int) int { tick(1); if n == 0 { return 0 }; return c(n - 1) }
type T struct{}
func (t *T) m(n int) { if n > 0 { t.m(n - 1) } }
func (t *T) String() string { var b T; return b.String() }
func g(n int) {
again:
	n--
	if n > 0 {
		goto again
	}
}
func h(n int) {
again:
	tick(1)
	n--
	if n > 0 {
		goto again
	}
	goto done
done:
}
`
	file, err := parser.ParseFile(token.NewFileSet(), "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	loops := map[string]bool{}
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if l, ok := n.(*ast.RangeStmt); ok {
				loops[fn.Name.Name] = ticksInBody(l.Body)
			}
			return true
		})
	}
	want := map[string]bool{"early": false, "good": true}
	for name, w := range want {
		if loops[name] != w {
			t.Errorf("loop of %s counts its turns: %v, want %v", name, loops[name], w)
		}
	}
	files := []*ast.File{file}
	if got, want := cycles(files), map[string]bool{"a#-1": false, "b#-1": false, "c#-1": true, "(*T).m#-1": false}; !maps.Equal(got, want) {
		t.Errorf("cycles = %v, want %v", got, want)
	}
	if got, want := backwardGotos(files), map[string]bool{"g#-1:again": false, "h#-1:again": true}; !maps.Equal(got, want) {
		t.Errorf("backward gotos = %v, want %v", got, want)
	}
}
