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

package released

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The files of the release before are kept byte for byte: the sums are those of redact.go, extent.go and work.go
// of package redact at origin/main 0701cd3a with "package released" for "package redact", and the change of #873,
// which made the work linear without changing the output (lines.go is new with it), and the change of #1087, which
// moved the reading of base64 windows into windows.go and keeps no empty part between slashes there, so that its
// heap is bounded without changing the output. What the code computes is
// checked against that release's results by TestReleasedReproducesTheReleaseBefore (package redact), and against
// the code before #873 (testdata/v052) by the tests of kept_test.go and TestReleasedSpansMatchKeptCopy.
//
// The work budget of #873 (decision of 2026-11-20) threads a meter through the same files, which changes their
// bytes: the sums are of the files with it taken out (unmetered), printed by go/printer.
func TestReleasedIsTheReleaseBefore(t *testing.T) {
	for name, want := range map[string]string{
		"redact.go":  "26267c19f828be0455e464dfeca53f98d09be516068394489d76aa4cec8c31b8",
		"extent.go":  "83c7cd9d2f8b1de7af41019e7631c88eeb23962bfa3952a5f2edc6f54d0ba0aa",
		"work.go":    "02ba296592fb1f3ac8f8d5cda941abe1ee782c89e7d1f7901844d6ae7211e3f5",
		"lines.go":   "7ba72fa9847d6bce30faecc36b3385f67d85fe2e12ea512746a33452469b6a0c",
		"windows.go": "62c688d6e48020c015f0ff59bf4d9e8bf3e7d28b2c9031e84d51bbff39a3c24e",
	} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(unmetered(t, name, b)))
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s changed: sha256 %s, want %s; the kept reading must not be edited", name, got, want)
		}
	}
}

// unmetered returns src, a file of this package, as Go source with the work budget of #873 (package meter, decision
// of 2026-11-20) taken out: the parameter wm of every function and function type, the first argument (wm or nil)
// of every call of a function or rule field that takes it, the metered sort (sortFunc, read back as
// slices.SortFunc), the charged search (rule.findAll, read back as FindAllStringSubmatchIndex on counted) and the
// import of package meter. The budget was added
// to the kept files by that change alone, so what is left is the release's code, printed by go/printer.
// isWM reports whether p is the meter parameter: wm, or _ where the function does not use it.
func isWM(p *ast.Field) bool {
	if len(p.Names) != 1 {
		return false
	}
	star, ok := p.Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Meter" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "meter" && (p.Names[0].Name == "wm" || p.Names[0].Name == "_")
}

// releasedFindAll turns the charged search, ru.findAll(s) once the meter is taken out (rule.findAll in find.go,
// #873), back into the release's ru.re.FindAllStringSubmatchIndex(counted(s), -1).
func releasedFindAll(x *ast.CallExpr) {
	sel, ok := x.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "findAll" || len(x.Args) != 1 {
		return
	}
	x.Fun = &ast.SelectorExpr{
		X:   &ast.SelectorExpr{X: sel.X, Sel: ast.NewIdent("re")},
		Sel: ast.NewIdent("FindAllStringSubmatchIndex"),
	}
	x.Args = []ast.Expr{
		&ast.CallExpr{Fun: ast.NewIdent("counted"), Args: x.Args},
		&ast.UnaryExpr{Op: token.SUB, X: &ast.BasicLit{Kind: token.INT, Value: "1"}},
	}
}

func unmetered(t *testing.T, name string, src []byte) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	takes := map[string]bool{"span": true, "spans": true, "spanLines": true, "spansLines": true}
	dropWM := func(ft *ast.FuncType) bool {
		if ft.Params == nil || len(ft.Params.List) == 0 {
			return false
		}
		if !isWM(ft.Params.List[0]) {
			return false
		}
		ft.Params.List = ft.Params.List[1:]
		return true
	}
	// The functions of the package that take wm, declared in any of its files.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		pf, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range pf.Decls {
			if d, ok := d.(*ast.FuncDecl); ok && d.Type.Params != nil && len(d.Type.Params.List) > 0 {
				if isWM(d.Type.Params.List[0]) {
					takes[d.Name.Name] = true
				}
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncType:
			dropWM(x)
		case *ast.CallExpr:
			var callee string
			switch fn := x.Fun.(type) {
			case *ast.Ident:
				callee = fn.Name
			case *ast.SelectorExpr:
				callee = fn.Sel.Name
			}
			if takes[callee] && len(x.Args) > 0 {
				if id, ok := x.Args[0].(*ast.Ident); ok && (id.Name == "wm" || id.Name == "nil") {
					x.Args = x.Args[1:]
				}
			}
			// The metered sort (sortFunc, #873) is the release's slices.SortFunc with the budget taken out.
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "sortFunc" {
				x.Fun = &ast.SelectorExpr{X: ast.NewIdent("slices"), Sel: ast.NewIdent("SortFunc")}
			}
			releasedFindAll(x)
		}
		return true
	})
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
			g.Specs = slices.DeleteFunc(g.Specs, func(s ast.Spec) bool {
				return s.(*ast.ImportSpec).Path.Value == `"github.com/pkalin/shadewright/internal/redact/meter"`
			})
			if len(g.Specs) == 1 {
				g.Lparen, g.Rparen = token.NoPos, token.NoPos
			}
		}
	}
	f.Imports = slices.DeleteFunc(f.Imports, func(s *ast.ImportSpec) bool {
		return s.Path.Value == `"github.com/pkalin/shadewright/internal/redact/meter"`
	})
	var b bytes.Buffer
	if err := format.Node(&b, fset, f); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
