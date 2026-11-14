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
package semdiff_test

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/gitops/semdiff"
	"github.com/pkalin/shadewright/internal/testutil/importguard"
)

// TestCheckInProcessAnswersAsTheRenderProcess checks the render of the tests (#717): with
// RenderInProcessForTests on (TestMain), Check starts no render process and gives the result or the refusal a
// render process gives for the same input.
func TestCheckInProcessAnswersAsTheRenderProcess(t *testing.T) {
	nonYAML := ordersRepo()
	nonYAML[soKust] = kust("scaledobject.yaml", "deploy.json")
	nonYAML["apps/orders/deploy.json"] = []byte("{}")
	missing := ordersRepo()
	missing[soKust] = kust("scaledobject.yaml", "missing.yaml")
	for name, in := range map[string]semdiff.Input{
		"accepted":           soInput(t, ordersRepo(), "apps/orders", 15),
		"refused":            soInput(t, nonYAML, "apps/orders", 15),
		"render failed":      soInput(t, missing, "apps/orders", 15),
		"ordinary chain":     soInput(t, chainedRepo(16, 100), "apps/orders", 15),
		"overridden overlay": soInput(t, overlayPatch(), "clusters/lab", 15),
	} {
		t.Run(name, func(t *testing.T) {
			started := semdiff.CountRenderProcesses(t)
			here, hereErr := semdiff.Check(context.Background(), in)
			if started() != 0 {
				t.Fatalf("Check started %d render processes, want none in a test binary that renders in process", started())
			}
			semdiff.UseRenderProcess(t)
			there, thereErr := semdiff.Check(context.Background(), in)
			if started() != 1 {
				t.Fatalf("Check started %d render processes, want 1 with the render process on", started())
			}
			if !reflect.DeepEqual(here, there) || fmt.Sprint(hereErr) != fmt.Sprint(thereErr) {
				t.Fatalf("in process: %+v, %v; render process: %+v, %v", here, hereErr, there, thereErr)
			}
		})
	}
}

// TestRenderInProcessOnlyFromTests checks that no file outside tests calls RenderInProcessForTests, so every
// binary but a test binary renders in a render process, with its limits (#717). The function also refuses in
// a binary that go test did not build.
func TestRenderInProcessOnlyFromTests(t *testing.T) {
	const name = "RenderInProcessForTests"
	n := importguard.WalkGoFiles(t, importguard.ModuleRoot(t), func(path string) {
		if strings.HasSuffix(path, "_test.go") {
			return
		}
		if filepath.Base(path) == "inprocess.go" && filepath.Base(filepath.Dir(path)) == "semdiff" {
			return // the definition
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == name {
				t.Errorf("%s names %s: only tests may render without the render process", path, name)
			}
			return true
		})
	})
	if n == 0 {
		t.Fatal("no Go files checked")
	}
}
