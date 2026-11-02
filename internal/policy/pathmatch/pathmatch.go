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

// Package pathmatch decides whether a repository-relative file path matches a policy's path pattern, with the
// semantics documented on [v1alpha1.GitPathPattern]:
//
//   - Pattern and path are split on "/" and compared segment by segment, case-sensitively, byte for byte.
//   - A segment that is exactly "**" matches zero or more whole path segments.
//   - In any other segment, "*" matches any run of characters within that one segment (so a run of stars
//     inside a segment, such as "a**b", acts like a single "*").
//   - No wildcard matches a path segment that starts with a dot.
//
// Match fails closed: a pattern or path that the API schema would refuse (an absolute path, an empty
// segment, a segment starting with a dot or a dash, "..", a wildcard in the first pattern segment or in the
// path, a path that is not a .yaml or .yml file) never matches. The package does not use [path/filepath.Match],
// whose semantics differ and depend on the operating system.
package pathmatch

import (
	"regexp"
	"slices"
	"strings"

	"github.com/pkalin/shadewright/api/v1alpha1"
)

// maxLen is the MaxLength of both GitFilePath and GitPathPattern in the API schema.
const maxLen = 512

// These mirror the kubebuilder validation markers on the API types, so that Match refuses the same inputs
// the API server refuses even when it is called with values that did not pass through it.
var (
	filePathRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)*\.ya?ml$`)
	patternRE  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_*][A-Za-z0-9_.*-]*)*$`)
)

// Match reports whether path matches pattern. It returns false for any pattern or path that is not valid
// according to its API type.
func Match(pattern v1alpha1.GitPathPattern, path v1alpha1.GitFilePath) bool {
	p, s := string(pattern), string(path)
	if !validPattern(p) || !validPath(s) {
		return false
	}
	return matchSegments(strings.Split(p, "/"), strings.Split(s, "/"))
}

func validPattern(p string) bool {
	return len(p) <= maxLen && patternRE.MatchString(p) && !strings.Contains(p, "..")
}

func validPath(s string) bool {
	return len(s) <= maxLen && filePathRE.MatchString(s)
}

// matchSegments matches pattern segments against path segments. It uses dynamic programming over
// (pattern index, path index) instead of recursion, so patterns with many "**" segments cannot cause
// exponential work.
func matchSegments(pat, segs []string) bool {
	// next[j] reports whether pat[i+1:] matches segs[j:]; cur is the same for pat[i:].
	next := make([]bool, len(segs)+1)
	cur := make([]bool, len(segs)+1)
	next[len(segs)] = true
	for _, ps := range slices.Backward(pat) {
		if ps == "**" {
			// "**" consumes zero segments, or one non-hidden segment and stays.
			cur[len(segs)] = next[len(segs)]
			for j := len(segs) - 1; j >= 0; j-- {
				cur[j] = next[j] || (!hidden(segs[j]) && cur[j+1])
			}
		} else {
			cur[len(segs)] = false
			for j := len(segs) - 1; j >= 0; j-- {
				cur[j] = next[j+1] && matchSegment(ps, segs[j])
			}
		}
		cur, next = next, cur
	}
	return next[0]
}

func hidden(seg string) bool {
	return strings.HasPrefix(seg, ".")
}

// matchSegment matches one pattern segment, where "*" matches any run of characters, against one path
// segment. A wildcard never matches a segment that starts with a dot. The loop keeps a single backtrack
// point, so it runs in O(len(pat) * len(seg)) time.
func matchSegment(pat, seg string) bool {
	if hidden(seg) && strings.Contains(pat, "*") {
		return false
	}
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(seg) {
		switch {
		case pi < len(pat) && pat[pi] == '*':
			star, mark = pi, si
			pi++
		case pi < len(pat) && pat[pi] == seg[si]:
			pi++
			si++
		case star >= 0:
			mark++
			si = mark
			pi = star + 1
		default:
			return false
		}
	}
	for pi < len(pat) && pat[pi] == '*' {
		pi++
	}
	return pi == len(pat)
}
