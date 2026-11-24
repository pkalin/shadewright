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
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// redacttestPath is the import path of the package that lowers the budget for tests.
const redacttestPath = "github.com/shadewright-labs/shadewright/internal/redact/redacttest"

// moduleRoot returns the directory of go.mod above the package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package")
		}
		dir = parent
	}
}

// loweredBudgetUses returns, for every Go file of the module that is not a _test.go file and not in package
// redacttest, the places that lower a redactor's budget: a call of WithWorkBudget or LowBudget, or an import of
// redacttest. The budget of a product binary is the package's constant (ADR-0015, decision of 2026-11-22 on #962).
func loweredBudgetUses(t *testing.T, root string) []string {
	t.Helper()
	var uses []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "testdata", "vendor", "node_modules", "site", ".venv":
				return filepath.SkipDir
			}
			if path == filepath.Join(root, "internal", "redact", "redacttest") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, im := range f.Imports {
			if p, _ := strconv.Unquote(im.Path.Value); p == redacttestPath {
				uses = append(uses, rel+": imports redacttest")
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "WithWorkBudget" || sel.Sel.Name == "LowBudget") {
				uses = append(uses, rel+":"+strconv.Itoa(fset.Position(sel.Pos()).Line)+": "+sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return uses
}

// No product code lowers the work budget: WithWorkBudget and redacttest.LowBudget are reached only from _test.go
// files and package redacttest (decision of 2026-11-22 on #962).
func TestLoweredBudgetOnlyInTests(t *testing.T) {
	root := moduleRoot(t)
	if uses := loweredBudgetUses(t, root); len(uses) != 0 {
		t.Errorf("code outside tests lowers the work budget of redaction:\n%s", strings.Join(uses, "\n"))
	}
}

// The check of TestLoweredBudgetOnlyInTests finds a use: a product file of a module that calls WithWorkBudget,
// one that imports redacttest, and passes over the same in a _test.go file.
func TestLoweredBudgetCheckFindsAUse(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example\n")
	write("a/a.go", "package a\n\nfunc f(r interface{ WithWorkBudget(int64) any }) { r.WithWorkBudget(1) }\n")
	write("b/b.go", "package b\n\nimport _ \""+redacttestPath+"\"\n")
	write("c/c_test.go", "package c\n\nimport _ \""+redacttestPath+"\"\n\nfunc f(r interface{ WithWorkBudget(int64) any }) { r.WithWorkBudget(1) }\n")
	uses := loweredBudgetUses(t, root)
	if len(uses) != 2 || !strings.Contains(uses[0], "WithWorkBudget") || !strings.Contains(uses[1], "imports redacttest") {
		t.Errorf("uses found: %q; want the call in a/a.go and the import in b/b.go", uses)
	}
}

// A redactor of redacttest.LowBudget withholds OverLowBudget at every entry point for its work (ReasonWork),
// where the package's budget decides it; a short entry it decides as the package's redactor does; and it only
// ever lowers the budget.
func TestLowBudgetWithholdsForWork(t *testing.T) {
	parallelUnderRace(t)
	low := redacttest.LowBudget(defaultRedactor)
	if low.budgetOf(1) != budgetOf(1) || low.budgetOf(MaxEntry) != redacttest.LowWorkBudget {
		t.Errorf("budget of 1 byte %d, of MaxEntry %d; want %d and %d", low.budgetOf(1), low.budgetOf(MaxEntry),
			budgetOf(1), redacttest.LowWorkBudget)
	}
	if again := low.WithWorkBudget(1 << 40); again.budgetOf(MaxEntry) != redacttest.LowWorkBudget {
		t.Errorf("a second WithWorkBudget raised the budget to %d", again.budgetOf(MaxEntry))
	}
	if defaultRedactor.lowered {
		t.Fatal("LowBudget changed the package's redactor")
	}
	over := redacttest.OverLowBudget
	if _, ok := defaultRedactor.StringChecked(over); !ok {
		t.Fatal("the package's budget withholds OverLowBudget")
	}
	work := withheldTotal.WithLabelValues(ReasonWork)
	for _, e := range flaggedEntries {
		before := testutil.ToFloat64(work)
		out, ok := e.on(low, over)
		if ok || out[0] != Withheld || strings.Contains(strings.Join(out, ""), "--token,}--token") {
			t.Errorf("%s on the low budget: ok %v, %.80q; want withheld", e.name, ok, out)
		}
		if got := testutil.ToFloat64(work) - before; got < 1 {
			t.Errorf("%s on the low budget: reason work counted %v times", e.name, got)
		}
		const short = "password: Qz9SecretValue in a short line"
		got, ok := e.on(low, short)
		want, _ := e.on(defaultRedactor, short)
		if !ok || strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%s on the low budget decided a short entry %v, %q; want %q", e.name, ok, got, want)
		}
	}
}
