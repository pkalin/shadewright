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

package redact

import (
	"slices"
	"strings"

	"github.com/shadewright-labs/shadewright/internal/redact/meter"
)

// The work of redaction is counted, not timed, by the tests that show it stays linear in the size of the
// text (#825, #840): a busy machine slows a clock, never a count. The scanning loops and the library calls
// below report their work to tick. In every build tick spends it on the meter of the entry's work budget (#873,
// budget.go), which belongs to that one call; the product (no build tag) holds no other counter and no shared
// state for it (tick.go). Built with the tag redactwork, as make test runs the linearity tests, tick also adds
// to the count of Work (work_counted.go).

// Tick reports n steps of work done for a decision of this package outside it: the read tools count the bytes
// of an answer they walk to make its pieces, so that the linearity tests see that work with the redactor's. It
// is outside every entry point, so it spends on no budget: it does nothing in the product build and adds to the
// count of Work in a build with the tag redactwork.
func Tick(n int) { tick(nil, n) }

// counted returns t, counting its bytes as the work of the regular expression it is handed to.
func counted(wm *meter.Meter, t string) string {
	tickExpression(wm, len(t))
	return t
}

// The index functions of package strings, counting the bytes they scan: up to and including what they
// find, or the whole of t.

func indexByte(wm *meter.Meter, t string, c byte) int {
	i := strings.IndexByte(t, c)
	tick(wm, found(wm, i, 1, len(t)))
	return i
}

//nolint:unparam // the counted strings.LastIndexByte, with its signature like the other counted functions here
func lastIndexByte(wm *meter.Meter, t string, c byte) int {
	i := strings.LastIndexByte(t, c)
	tick(wm, len(t)-i)
	return i
}

func indexAny(wm *meter.Meter, t, chars string) int {
	i := strings.IndexAny(t, chars)
	tick(wm, found(wm, i, 1, len(t)))
	return i
}

func index(wm *meter.Meter, t, sub string) int {
	i := strings.Index(t, sub)
	tick(wm, found(wm, i, len(sub), len(t)))
	return i
}

func containsAny(wm *meter.Meter, t, chars string) bool { return indexAny(wm, t, chars) >= 0 }

// found is the bytes an index function scanned that returned i for a match of n bytes in t of size bytes.
func found(_ *meter.Meter, i, n, size int) int {
	if i < 0 {
		return size
	}
	return i + n
}

// sortFunc is slices.SortFunc that counts each comparison as one step on wm (#873, decision of 2026-11-20): a
// sort of the spans of a text costs comparisons in proportion to n log n of them, which the budget sees like any
// other work and stops when it runs out.
func sortFunc[S ~[]E, E any](wm *meter.Meter, x S, cmp func(a, b E) int) {
	slices.SortFunc(x, func(a, b E) int {
		tick(wm, 1)
		return cmp(a, b)
	})
}
