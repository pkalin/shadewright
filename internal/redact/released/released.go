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

// This package is the redactor of the release before #845 (origin/main at 0701cd3a): redact.go, extent.go and
// work.go are that release's files of package redact, byte for byte apart from the package clause, and must not
// be edited (TestReleasedIsTheReleaseBefore pins them). Package redact applies it as one reading of each text, at
// the places and in the order of passes the tools of that release applied it (#845, thirteenth decision of
// 2026-11-18), so that what the release before hid is hidden because its own code says so. Only this file and
// the tick files are new. The package is retired, with its reading, by its own issue (pkalin/shadewright#922)
// once a release has run with both readings.

// Span is a byte range of a text that the redactor of the release before hides, with the kind its marker names.
type Span struct {
	Start, End int
	Kind       string
}

// Spans returns the ranges of s that String hides, sorted and disjoint, as the release before computed them.
func (r *Redactor) Spans(s string) []Span {
	sp := r.spans(s)
	out := make([]Span, len(sp))
	for i, x := range sp {
		out[i] = Span{Start: x.start, End: x.end, Kind: x.kind}
	}
	return out
}

// Spans is the package's Spans with the built-in rules.
func Spans(s string) []Span { return defaultRedactor.Spans(s) }
