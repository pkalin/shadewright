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
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// parseDir parses the Go files of dir, tests left out.
func parseDir(t *testing.T, dir string) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatalf("no Go files in %s", dir)
	}
	return files
}

// stringConsts returns the exported string constants declared in files, by name. When typ is not empty, only
// those declared with that type.
func stringConsts(t *testing.T, files []*ast.File, typ string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); typ != "" && (!ok || id.Name != typ) {
					continue
				}
				for i, name := range vs.Names {
					if !name.IsExported() || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					out[name.Name] = v
				}
			}
		}
	}
	return out
}

// Every reason that can end a request Denied is reported by name, not as Other: the reasons of package policy
// (the decision's Reason) and of package sourcemap (a Denied result), and the reasons this package passes to
// denied. The reasons that only come with an allowed outcome are left out.
func TestDenialReasonsCoverEveryReason(t *testing.T) {
	allowedOnly := map[string]bool{"WithinApprovedBounds": true, "SourceResolved": true}
	for _, dir := range []string{filepath.Join("..", "policy"), filepath.Join("..", "gitops", "sourcemap")} {
		reasons := stringConsts(t, parseDir(t, dir), "Reason")
		if len(reasons) < 10 {
			t.Fatalf("found only %d Reason constants in %s: the walk is broken", len(reasons), dir)
		}
		for name, v := range reasons {
			if _, ok := denialReasons[v]; !ok && !allowedOnly[v] {
				t.Errorf("%s.%s (%q) is missing from denialReasons", filepath.Base(dir), name, v)
			}
		}
	}

	files := parseDir(t, ".")
	consts := stringConsts(t, files, "")
	var named int
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "denied" {
				return true
			}
			id, ok := call.Args[0].(*ast.Ident)
			if !ok {
				// string(res.Reason) and the like: the reasons of package sourcemap, checked above.
				return true
			}
			named++
			v, ok := consts[id.Name]
			if !ok {
				t.Errorf("denied(%s, ...): %s is not an exported string constant of this package", id.Name, id.Name)
				return true
			}
			if _, ok := denialReasons[v]; !ok {
				t.Errorf("denied(%s, ...) (%q) is missing from denialReasons", id.Name, v)
			}
			return true
		})
	}
	if named < 4 {
		t.Fatalf("found only %d calls of denied with a named reason: the walk is broken", named)
	}
}
