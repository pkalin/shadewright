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

// Package importguard holds the checks behind the import guard tests that enforce ADR-0013: product SDKs are
// imported only in their adapter package. It is for tests only.
package importguard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Module is the import path prefix of this module.
const Module = "github.com/pkalin/shadewright/"

// VendorSDKs are import path prefixes of product SDKs, allowed only in their adapter package.
var VendorSDKs = []string{
	"github.com/Azure/",
	"github.com/AzureAD/",
	"github.com/aws/",
	"cloud.google.com/",
	"google.golang.org/api/",
	"github.com/google/go-github/",
	"github.com/hashicorp/vault/",
	"github.com/slack-go/",
}

// Adapters is the import path prefix of the adapter packages in this module. Only their fakes may reach the
// core.
const Adapters = Module + "internal/providers/"

// adapterExceptions are packages under Adapters that hold interfaces or fakes, not adapters.
var adapterExceptions = []string{Module + "internal/providers/fake"}

// T is the part of testing.TB the checks use, so a test can record what a check reports.
type T interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// FileImports returns the import paths of one Go file, whatever its build tags.
func FileImports(t T, path string) []string {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := make([]string, 0, len(parsed.Imports))
	for _, imp := range parsed.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatalf("%s: import %s: %v", path, imp.Path.Value, err)
		}
		out = append(out, p)
	}
	return out
}

// IsStandardOrModule reports whether an import path is in the standard library (no dot in its first element)
// or in this module.
func IsStandardOrModule(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".") || strings.HasPrefix(path, Module)
}

// DirectImportsOnlyStandardOrModule fails the test for any Go file in dirs, test files included, that imports
// a package outside the standard library and this module.
func DirectImportsOnlyStandardOrModule(t T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		if len(files) == 0 {
			t.Fatalf("no Go files in %s", dir)
		}
		for _, f := range files {
			for _, p := range FileImports(t, f) {
				if !IsStandardOrModule(p) {
					t.Errorf("%s imports %q: only the standard library and %s* are allowed here", f, p, Module)
				}
			}
		}
	}
}

// Reasons [Forbidden] gives.
const (
	ReasonSDK     = "a product SDK"
	ReasonAdapter = "an adapter package"
)

// Forbidden reports why a package must not be among the dependencies of the core, or "" when it may be.
func Forbidden(importPath string) string {
	for _, prefix := range VendorSDKs {
		if strings.HasPrefix(importPath, prefix) {
			return ReasonSDK
		}
	}
	if !strings.HasPrefix(importPath, Adapters) {
		return ""
	}
	for _, ok := range adapterExceptions {
		if importPath == ok || strings.HasPrefix(importPath, ok+"/") {
			return ""
		}
	}
	return ReasonAdapter
}

// Package is the part of a package in `go list -json` output the checks read.
type Package struct {
	// ImportPath is the package's import path.
	ImportPath string
	// Standard is true for a package of the standard library.
	Standard bool
	// Imports are the packages it imports directly, test files excluded.
	Imports []string
}

// Deps returns every package the packages matching patterns depend on, themselves included, as `go list
// -deps` reports them for the current build. dir is where `go list` runs.
func Deps(t T, dir string, patterns ...string) []Package {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go command not found: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, goBin, append([]string{"list", "-deps", "-json"}, patterns...)...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go list -deps %v: %v: %s", patterns, err, stderr.String())
	}
	var out []Package
	dec := json.NewDecoder(&stdout)
	for {
		var p Package
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		t.Fatalf("go list -deps %v listed no package", patterns)
	}
	return out
}

// NoForbiddenDeps fails the test when a package matching patterns depends, directly or through any other
// package, on a product SDK or an adapter package (see [Forbidden]). It names the package that imports it, so
// the path is easy to follow.
func NoForbiddenDeps(t T, dir string, patterns ...string) {
	t.Helper()
	for _, p := range Deps(t, dir, patterns...) {
		if p.Standard {
			continue
		}
		for _, imp := range p.Imports {
			if why := Forbidden(imp); why != "" {
				t.Errorf("%s imports %q, %s, so it reaches %v", p.ImportPath, imp, why, patterns)
			}
		}
	}
}

// Testutil is the import path prefix of the test-only helper packages of this module, this one included.
const Testutil = Module + "internal/testutil/"

// NoTestutilInProductCode fails the test for any Go file under root that is not a _test.go file and imports a
// package under [Testutil]. The helpers are for tests only; the Go toolchain cannot enforce that, since they
// are ordinary importable packages. Files inside a directory named testutil are the helpers themselves and are
// skipped. It returns the number of files checked.
func NoTestutilInProductCode(t T, root string) int {
	t.Helper()
	checked := 0
	WalkGoFiles(t, root, func(path string) {
		if strings.HasSuffix(path, "_test.go") || slices.Contains(strings.Split(filepath.ToSlash(path), "/"), "testutil") {
			return
		}
		checked++
		for _, p := range FileImports(t, path) {
			if strings.HasPrefix(p, Testutil) {
				t.Errorf("%s imports %q: internal/testutil is for _test.go files only", path, p)
			}
		}
	})
	return checked
}

// ModuleRoot returns the directory of this module's go.mod, searched upwards from the working directory.
func ModuleRoot(t T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found")
		}
		dir = parent
	}
}

// WalkGoFiles calls fn for every Go file of the module under root, skipping hidden directories, bin, vendor,
// testdata and node_modules. It returns the number of files visited.
func WalkGoFiles(t T, root string, fn func(path string)) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "bin" || name == "vendor" ||
				name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			n++
			fn(path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return n
}
