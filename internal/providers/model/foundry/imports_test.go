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

package foundry

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/testutil/importguard"
)

// TestStandardLibraryOnly keeps the adapter free of SDKs (ADR-0013): its code imports the standard library
// and this module only. Test files may use test helpers.
func TestStandardLibraryOnly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		checked++
		for _, p := range importguard.FileImports(t, f) {
			if !importguard.IsStandardOrModule(p) {
				t.Errorf("%s imports %q: only the standard library and this module are allowed", f, p)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no Go files checked")
	}
}
