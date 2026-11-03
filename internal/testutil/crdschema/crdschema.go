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

// Package crdschema reads field schemas from the generated CRDs in config/crd/bases, so a test can check that a
// rule in Go and the schema the API server enforces agree. It is for tests only.
package crdschema

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Field returns the OpenAPI schema of one field of the CRD in config/crd/bases/<file>, which must serve exactly
// one version. path is a dotted path from openAPIV3Schema ("spec.modules.platforms.items"): each element names a
// property, except "items", which steps into the schema of a list's items. It fails the test when the file or
// the field is missing.
func Field(t testing.TB, file, path string) map[string]any {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot find the crdschema source file")
	}
	full := filepath.Join(filepath.Dir(self), "..", "..", "..", "config", "crd", "bases", file)
	raw, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read CRD: %v", err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	if len(crd.Spec.Versions) != 1 {
		t.Fatalf("%s serves %d versions, want 1", file, len(crd.Spec.Versions))
	}
	node := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	steps := strings.Split(path, ".")
	for i, step := range steps {
		var next any
		if step == "items" {
			next = node["items"]
		} else if props, ok := node["properties"].(map[string]any); ok {
			next = props[step]
		}
		m, ok := next.(map[string]any)
		if !ok {
			t.Fatalf("%s has no field %s", file, strings.Join(steps[:i+1], "."))
		}
		node = m
	}
	return node
}

// Pattern returns the pattern of a string field (see [Field] for path), failing the test when it has none.
func Pattern(t testing.TB, file, path string) string {
	t.Helper()
	p, ok := Field(t, file, path)["pattern"].(string)
	if !ok {
		t.Fatalf("%s field %s has no pattern", file, path)
	}
	return p
}

// MaxItems returns the maxItems of a list field (see [Field] for path), failing the test when it has none.
func MaxItems(t testing.TB, file, path string) int {
	t.Helper()
	n, ok := Field(t, file, path)["maxItems"].(float64)
	if !ok {
		t.Fatalf("%s field %s has no maxItems", file, path)
	}
	return int(n)
}
