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

package pathmatch_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/pathmatch"
)

//nolint:goconst // literal paths keep each test case readable
func TestMatch(t *testing.T) {
	tests := []struct {
		pattern string
		match   []string
		noMatch []string
	}{
		// "*" stays within one segment.
		{
			pattern: "apps/*/scaledobject.yaml",
			match:   []string{"apps/orders/scaledobject.yaml", "apps/x-1.y_z/scaledobject.yaml"},
			noMatch: []string{"apps/a/b/scaledobject.yaml", "apps/scaledobject.yaml", "apps/orders/deployment.yaml"},
		},
		{
			pattern: "apps/*.yaml",
			match:   []string{"apps/x.yaml", "apps/a.b.yaml"},
			noMatch: []string{"apps/x.yml", "apps/a/x.yaml", "apps/x.YAML"},
		},
		{
			pattern: "apps/orders-*.yaml",
			match:   []string{"apps/orders-api.yaml", "apps/orders-.yaml"},
			noMatch: []string{"apps/billing-api.yaml", "apps/orders.yaml"},
		},
		{
			pattern: "apps/*-*.yaml",
			match:   []string{"apps/a-b.yaml", "apps/a-b-c.yaml"},
			noMatch: []string{"apps/ab.yaml"},
		},
		{
			// A run of stars inside a segment acts like a single star.
			pattern: "apps/a**.yaml",
			match:   []string{"apps/abc.yaml", "apps/a.yaml"},
			noMatch: []string{"apps/a/b.yaml", "apps/b.yaml"},
		},
		{
			pattern: "apps/*",
			match:   []string{"apps/x.yaml"},
			noMatch: []string{"apps/a/x.yaml", "apps/*.yaml", "apps/x.json", `apps\x.yaml`},
		},
		{
			pattern: "apps/*/X.yaml",
			match:   []string{"apps/orders/X.yaml"},
			noMatch: []string{"apps/orders/x.yaml"},
		},

		// "**" spans zero or more whole segments.
		{
			pattern: "apps/**/*.yaml",
			match:   []string{"apps/x.yaml", "apps/a/x.yaml", "apps/a/b/x.yaml"},
			noMatch: []string{"infra/a/x.yaml", "apps/a/x.yml"},
		},
		{
			pattern: "apps/**",
			match:   []string{"apps/x.yaml", "apps/a/b/x.yml"},
			noMatch: []string{"apps.yaml", "Apps/x.yaml", "/apps/x.yaml", "apps/../x.yaml", "apps//x.yaml", ""},
		},
		{
			pattern: "apps/**/base/kustomization.yaml",
			match:   []string{"apps/base/kustomization.yaml", "apps/x/y/base/kustomization.yaml"},
			noMatch: []string{"apps/x/kustomization.yaml", "apps/base/x/kustomization.yaml"},
		},
		{
			pattern: "apps/**/x/**/y.yaml",
			match:   []string{"apps/x/y.yaml", "apps/a/x/b/c/y.yaml"},
			noMatch: []string{"apps/a/b/y.yaml", "apps/x/y/z.yaml"},
		},

		// A pattern without wildcards matches only itself, case-sensitively.
		{
			pattern: "apps/orders/scaledobject.yaml",
			match:   []string{"apps/orders/scaledobject.yaml"},
			noMatch: []string{"Apps/orders/scaledobject.yaml", "apps/orders/ScaledObject.yaml", "apps/orders/scaledobject.yml",
				"apps/orders/scaledobject.yaml/x.yaml", "apps/orders/deployment.yaml", "apps/scaledobject.yaml"},
		},
		{pattern: "apps/orders", noMatch: []string{"apps/orders/scaledobject.yaml"}},
		{pattern: "x.yaml", match: []string{"x.yaml"}, noMatch: []string{"y.yaml", "a/x.yaml"}},

		// Patterns the API refuses never match.
		{pattern: "", noMatch: []string{"x.yaml"}},
		{pattern: "*/x.yaml", noMatch: []string{"apps/x.yaml"}},
		{pattern: "**", noMatch: []string{"apps/x.yaml"}},
		{pattern: "/apps/x.yaml", noMatch: []string{"/apps/x.yaml"}},
		{pattern: "apps/../**", noMatch: []string{"apps/x.yaml"}},
		{pattern: "apps/-x/*.yaml", noMatch: []string{"apps/-x/y.yaml"}},
		{pattern: "apps//x.yaml", noMatch: []string{"apps/x.yaml"}},
		{pattern: "apps/", noMatch: []string{"apps/x.yaml"}},
		{pattern: "apps/?.yaml", noMatch: []string{"apps/x.yaml"}},
		{pattern: "apps/[a-z].yaml", noMatch: []string{"apps/x.yaml"}},
		{pattern: "apps/" + strings.Repeat("a", 512) + "/**", noMatch: []string{"apps/" + strings.Repeat("a", 512) + "/x.yaml"}},
		{pattern: "apps/.hidden/x.yaml", noMatch: []string{"apps/.hidden/x.yaml"}},
		{pattern: "apps/**", noMatch: []string{"apps/" + strings.Repeat("a", 512) + ".yaml"}},
	}
	for _, tt := range tests {
		for _, path := range tt.match {
			if !pathmatch.Match(v1alpha1.GitPathPattern(tt.pattern), v1alpha1.GitFilePath(path)) {
				t.Errorf("Match(%q, %q) = false, want true", tt.pattern, path)
			}
		}
		for _, path := range tt.noMatch {
			if pathmatch.Match(v1alpha1.GitPathPattern(tt.pattern), v1alpha1.GitFilePath(path)) {
				t.Errorf("Match(%q, %q) = true, want false", tt.pattern, path)
			}
		}
	}
}

// TestNoPatternMatchesHidden checks that no pattern, wildcard or literal, matches a path with a segment that
// starts with a dot.
func TestNoPatternMatchesHidden(t *testing.T) {
	patterns := []string{
		"apps/*/x.yaml", "apps/**/x.yaml", "apps/**/*.yaml", "apps/**", "apps/*", "apps/*x.yaml",
		"apps/.hidden/x.yaml", "apps/*/*", "apps/**/*",
	}
	paths := []string{
		"apps/.hidden/x.yaml", "apps/a/.hidden/b/x.yaml", "apps/.x.yaml", ".github/workflows/x.yaml",
	}
	for _, p := range patterns {
		for _, path := range paths {
			if pathmatch.Match(v1alpha1.GitPathPattern(p), v1alpha1.GitFilePath(path)) {
				t.Errorf("Match(%q, %q) = true, want false", p, path)
			}
		}
	}
}

// TestMatchManyGlobstars checks that a pattern made of many "**" segments finishes quickly instead of
// backtracking exponentially.
func TestMatchManyGlobstars(t *testing.T) {
	pattern := "apps" + strings.Repeat("/**", 120) + "/z.yaml"
	path := "apps" + strings.Repeat("/a", 120) + "/b.yaml"
	if pathmatch.Match(v1alpha1.GitPathPattern(pattern), v1alpha1.GitFilePath(path)) {
		t.Error("expected no match")
	}
	if !pathmatch.Match(v1alpha1.GitPathPattern(pattern), v1alpha1.GitFilePath(path[:len(path)-len("b.yaml")]+"z.yaml")) {
		t.Error("expected a match")
	}
}

// TestRefusesLookalikes covers inputs that a looser regex or input normalisation could let through.
func TestRefusesLookalikes(t *testing.T) {
	cases := [][2]string{
		{"apps/**", "apps/x.yaml\n"},
		{"apps/**", "apps/x.yaml\x00"},
		{"apps/**", "apps/x\x00.yaml"},
		{"apps/**", "apps/x\uff0eyaml"},
		{"apps/**", "apps\u2215x.yaml"},
		{"apps/**", "apps/x.YAML"},
		{"apps/*.yaml", "apps/x.YAML"},
		{"apps/**", "apps/\uff0egithub/x.yaml"},
		{"apps/**", "Apps/x.yaml"},
		{"apps/**\n", "apps/x.yaml"},
	}
	for _, c := range cases {
		if pathmatch.Match(v1alpha1.GitPathPattern(c[0]), v1alpha1.GitFilePath(c[1])) {
			t.Errorf("Match(%q, %q) = true, want false", c[0], c[1])
		}
	}
}

// refMatch is a deliberately simple recursive reference for short inputs: "**" matches zero or more
// non-hidden segments, other segments are path.Match-style globs where "*" never crosses "/" and never
// matches a segment starting with a dot.
func refMatch(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		if refMatch(pat[1:], segs) {
			return true
		}
		return len(segs) > 0 && !strings.HasPrefix(segs[0], ".") && refMatch(pat, segs[1:])
	}
	if len(segs) == 0 {
		return false
	}
	if strings.Contains(pat[0], "*") && strings.HasPrefix(segs[0], ".") {
		return false
	}
	return refGlob(pat[0], segs[0]) && refMatch(pat[1:], segs[1:])
}

func refGlob(p, s string) bool {
	if p == "" {
		return s == ""
	}
	if p[0] == '*' {
		for i := 0; i <= len(s); i++ {
			if refGlob(p[1:], s[i:]) {
				return true
			}
		}
		return false
	}
	return s != "" && p[0] == s[0] && refGlob(p[1:], s[1:])
}

var pathRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)*\.ya?ml$`)

func FuzzMatch(f *testing.F) {
	seeds := [][2]string{
		{"apps/**", "apps/x.yaml\n"},
		{"apps/**", "apps/\uff0egithub/x.yaml"},
		{"apps/*.yaml", "apps/x.YAML"},
		{"apps/**", "apps\u2215x.yaml"},
		{"apps/*/scaledobject.yaml", "apps/orders/scaledobject.yaml"},
		{"apps/**/*.yaml", "apps/a/b/x.yaml"},
		{"apps/**", "apps/.hidden/x.yaml"},
		{"apps/a**b/**/*-*.yml", "apps/axxb/c/d-e.yml"},
		{"apps/../**", "apps/x.yaml"},
		{"", ""},
		{"a/**/**/**/**/*", "a/b/c/d/e/f/g.yaml"},
	}
	for _, s := range seeds {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, pattern, path string) {
		got := pathmatch.Match(v1alpha1.GitPathPattern(pattern), v1alpha1.GitFilePath(path))
		if !got {
			return
		}
		// A match implies a path without hidden, empty, parent or absolute segments.
		for seg := range strings.SplitSeq(path, "/") {
			if seg == "" || strings.HasPrefix(seg, ".") || strings.HasPrefix(seg, "-") {
				t.Fatalf("Match(%q, %q) matched an invalid path", pattern, path)
			}
		}
		// A match implies a path the API server would accept.
		if len(path) > 512 || !pathRE.MatchString(path) || strings.Contains(path, "..") {
			t.Fatalf("Match(%q, %q) matched a path the schema refuses", pattern, path)
		}
		// The reference matcher agrees (short inputs only, it is exponential).
		if len(pattern) <= 64 && len(path) <= 64 && strings.Count(pattern, "*") <= 6 {
			// "**" inside a longer segment counts as "*" in both implementations.
			pat := strings.Split(pattern, "/")
			for i, p := range pat {
				if p != "**" {
					for strings.Contains(p, "**") {
						p = strings.ReplaceAll(p, "**", "*")
					}
					pat[i] = p
				}
			}
			if !refMatch(pat, strings.Split(path, "/")) {
				t.Fatalf("Match(%q, %q) = true but the reference matcher says false", pattern, path)
			}
		}
		// A wildcard-free pattern matches only itself.
		if !strings.Contains(pattern, "*") && pattern != path {
			t.Fatalf("literal pattern %q matched other path %q", pattern, path)
		}
	})
}
