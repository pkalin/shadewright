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

import (
	"slices"

	"github.com/shadewright-labs/shadewright/internal/redact/meter"
)

// This package is the redactor of the release before #845 (origin/main at 0701cd3a): redact.go, extent.go and
// work.go are that release's files of package redact, apart from the package clause and the changes of #873 and
// #962, and must not be edited otherwise (TestReleasedIsTheReleaseBefore pins them). #873 made its work linear on the
// shapes where it was not (lines.go, the line index of a text, and the reach of the comma key expression in
// extent.go) without changing what it hides: the code before that change is kept in testdata/v052, and the
// tests check that both find the same spans on every input of the corpora and on generated ones. Package redact applies it as one reading of each text, at
// the places and in the order of passes the tools of that release applied it (#845, thirteenth decision of
// 2026-11-18), so that what the release before hid is hidden because its own code says so. Only this file, lines.go
// and the tick files are new. The work budget of #873 (decision of 2026-11-20, package meter) is threaded
// through every function of the kept files as a first parameter wm and spent where they count their work and
// on each comparison of their sorts (sortFunc);
// TestReleasedIsTheReleaseBefore pins the kept files with it taken out. The package is retired, with its reading, by its own issue (shadewright-labs/shadewright#922)
// once a release has run with both readings.
//
// The package comment of the kept redact.go says that matching takes time linear in the input. That holds of one
// pass of the engine, not of a text: a rule is tried again after each match and an attempt may read the rest of
// the text (rule.findAll), and the work on "--token,}" and "-password," repeated grew with the square of the text
// until #962 gave the flag rules a memory of the values they read in one pass (tokenMemo, plainRun) and cut the
// extent of a key glued to the quote before it without reading it (keyValueExtent), again without changing what
// it hides (TestFlagRunsMatchKeptCopy). Only the entry points that take a meter stop at a work budget:
// [Redactor.SpansWithin] and [Redactor.StringWithin], which package redact calls with the budget of its own
// entry. [String], [Spans], [Replace], [StringKeeping], [Clean] and their methods on [Redactor] pass no meter
// and run with no budget.

// Span is a byte range of a text that the redactor of the release before hides, with the kind its marker names.
type Span struct {
	Start, End int
	Kind       string
}

// Spans returns the ranges of s that String hides, sorted and disjoint, as the release before computed them.
func (r *Redactor) Spans(s string) []Span { return r.SpansWithin(nil, s) }

// SpansWithin is Spans that counts its work on wm, the budget of the redaction it is part of (package meter,
// #873): when the steps run out it stops by the panic of [meter.Meter.Spend], which the entry point of that
// redaction recovers.
func (r *Redactor) SpansWithin(wm *meter.Meter, s string) []Span {
	sp := r.spans(wm, s)
	out := make([]Span, len(sp))
	for i, x := range sp {
		out[i] = Span{Start: x.start, End: x.end, Kind: x.kind}
	}
	return out
}

// Spans is the package's Spans with the built-in rules.
func Spans(s string) []Span { return defaultRedactor.Spans(s) }

// StringWithin is String that counts its work on wm, as SpansWithin does.
func (r *Redactor) StringWithin(wm *meter.Meter, s string) string {
	return render(wm, s, r.spans(wm, s))
}

// MarkerWithin is Marker that counts its work on wm, as SpansWithin does.
func MarkerWithin(wm *meter.Meter, kind string, n int) string { return marker(wm, kind, n) }

// sortFunc is slices.SortFunc that counts each comparison as one step on wm (#873, decision of 2026-11-20), as
// in package redact. The kept files call it where the release called slices.SortFunc;
// TestReleasedIsTheReleaseBefore reads the call back as slices.SortFunc when it takes the budget out.
func sortFunc[S ~[]E, E any](wm *meter.Meter, x S, cmp func(a, b E) int) {
	slices.SortFunc(x, func(a, b E) int {
		tick(wm, 1)
		return cmp(a, b)
	})
}
