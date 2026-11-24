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

// Package v052 is the reading of the release before (package released) as it was before #873 made its work
// linear: redact.go, extent.go and work.go are copies of internal/redact/released at commit 194211ee with only
// the package name changed, and this file is added. The differential tests of package released check that the
// faster code finds exactly the spans this copy finds, so the change of #873 changes the work and nothing else.
// It lives under testdata so that the go tool, the linter and the coverage report leave it out of ./..., and it
// is never imported outside tests. Do not change it.
package v052

// tick counts nothing: the copy is the reference of the output, not of the work.
func tick(int) {}

// Span is a byte range the copy redacts and the kind of the rule that found it.
type Span struct {
	Start, End int
	Kind       string
}

// Spans returns the spans the copy redacts in s, sorted and merged.
func Spans(s string) []Span {
	var out []Span
	for _, x := range defaultRedactor.spans(s) {
		out = append(out, Span{Start: x.start, End: x.end, Kind: x.kind})
	}
	return out
}
