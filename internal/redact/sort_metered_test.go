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
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/redact/meter"
)

// unmeteredSorts are the functions that may call a sort of package slices or sort directly: sortFunc, which counts
// each comparison, and newRuneSet, which sorts the ranges of an expression's syntax when the redactor is made,
// never a text.
var unmeteredSorts = map[string]bool{"sortFunc": true, "newRuneSet": true}

// A sort of the spans of a text costs comparisons in proportion to n log n of them, work the budget must see like
// any other (#873, decision of 2026-11-20): every sort of this package and of the reading of the release before
// goes through sortFunc, which counts them.
func TestEverySortCountsItsComparisons(t *testing.T) {
	var files []string
	for _, dir := range []string{".", "released"} {
		matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, matches...)
	}
	checked := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || unmeteredSorts[fn.Name.Name] {
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
				if pkg, ok := sel.X.(*ast.Ident); ok &&
					(pkg.Name == "sort" || pkg.Name == "slices" && strings.HasPrefix(sel.Sel.Name, "Sort")) {
					t.Errorf("%s: %s calls %s.%s, whose comparisons no budget counts; use sortFunc", file,
						fn.Name.Name, pkg.Name, sel.Sel.Name)
				}
				return true
			})
			checked++
		}
	}
	if checked < 300 {
		t.Fatalf("%d functions checked: the files of the package were not found", checked)
	}
}

// shuffledSpans returns n disjoint spans in a random order.
func shuffledSpans(n int) []span {
	rng := rand.New(rand.NewPCG(873, 3)) // #nosec G404 -- reproducible order
	spans := make([]span, n)
	for i, j := range rng.Perm(n) {
		spans[i] = span{start: 2 * j, end: 2*j + 1, kind: KindCredential}
	}
	return spans
}

// A sort with no budget left stops: 65,536 shuffled spans take some n log n comparisons, far more than the
// four steps per span given here, which the joining after the sort alone would not use up.
func TestSortWithNoBudgetLeftStops(t *testing.T) {
	const n = 65536
	for name, merge := range map[string]func(*meter.Meter, []span) []span{
		"mergeSpans":  mergeSpans,
		"mergeRanges": mergeRanges,
	} {
		wm := meter.New(4 * n)
		over := false
		func() {
			defer meter.Catch(&over)
			merge(wm, shuffledSpans(n))
		}()
		if !over {
			t.Errorf("%s of %d shuffled spans spent %d steps and did not stop on a budget of %d", name, n, wm.Spent(),
				4*n)
		}
		// With the budget it needs, the comparisons are counted: more than the four steps per span.
		wm = meter.New(1 << 40)
		if got := merge(wm, shuffledSpans(n)); len(got) != n || wm.Spent() <= 4*n {
			t.Errorf("%s of %d shuffled spans: %d spans, %d steps", name, n, len(got), wm.Spent())
		}
	}
}
