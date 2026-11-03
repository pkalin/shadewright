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
package repopath_test

import (
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/gitops/repopath"
)

const orders = "apps/orders"

func TestJoin(t *testing.T) {
	for _, tc := range []struct {
		dir, ref, want string
		ok             bool
	}{
		{".", "./apps/orders", orders, true},
		{orders, "../base", "apps/base", true},
		{orders, "so.yaml", "apps/orders/so.yaml", true},
		{"apps", "app.v2", "apps/app.v2", true},
		{".", "", "", false},
		{".", "/etc", "", false},
		{"apps", "../../x", "", false},
		{".", "a..b", "", false},
		{".", "github.com/org/repo", "", false},
		{".", "GitHub.com/org/repo", "", false},
		{".", "https://example.com/x", "", false},
		{".", "git@github.com:org/repo", "", false},
		{".", "x.yaml?ref=main", "", false},
		{".", ".hidden", "", false},
		{".", strings.Repeat("a", repopath.MaxLen+1), "", false},
	} {
		got, ok := repopath.Join(tc.dir, tc.ref)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Join(%q, %q) = %q, %v; want %q, %v", tc.dir, tc.ref, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNames(t *testing.T) {
	if !repopath.IsManifest("a/b.yml") || repopath.IsManifest("a/b.json") {
		t.Error("IsManifest")
	}
	if !repopath.IsKustomizationFile("a/Kustomization") || repopath.IsKustomizationFile("a/so.yaml") {
		t.Error("IsKustomizationFile")
	}
	if !repopath.SafeKey("patches") || repopath.SafeKey("a b") {
		t.Error("SafeKey")
	}
}
