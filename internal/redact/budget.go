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
	"math"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/pkalin/shadewright/internal/redact/meter"
)

// The work budget of one redaction (#873, decision of 2026-11-20). Every entry point of this package takes one
// decision on one entry (a text, the strings of a sequence, the pieces of a tool's answer) and counts its steps
// where the linearity tests count them (tick). The budget is linear in the size of the entry: WorkPerByte steps
// per byte and WorkFixed for the fixed cost of a call. An entry that uses it up, or one longer than MaxEntry
// before any work is done, is withheld whole: the caller gets Withheld and nothing of the entry, and the metric
// shadewright_redaction_withheld_total counts it by reason. What the rules hide on an entry inside its budget does
// not change: the budget only ever shows less.

const (
	// WorkPerByte is the steps the budget gives each byte of an entry. It is a bound on work, not a promise that
	// every linear shape fits (decision of 2026-11-21 on #873): a shape whose cost is linear but above it is
	// withheld from the size where it passes the budget. The known ones are glued quoted pairs (auth="a"
	// repeated), TOKEN="a" repeated before a long blank line, and "--token,}" and "-password," repeated, whose
	// reading of the release before grows with the square of the text until #962; TestBudgetWithholdSizes holds
	// the size from which each is withheld at each entry point. The work tests check that work grows linearly
	// with the text on every other shape of the corpus (TestCorpusWorkGrowsLinearly).
	WorkPerByte = 1024
	// WorkFixed is the steps the budget gives every entry besides WorkPerByte per byte: the fixed cost of a call
	// on a short entry.
	WorkFixed = 64 << 10
	// MaxEntry is the longest entry, in bytes, that redaction reads: above what the API server accepts in one
	// request body (3 MiB), so no object that can exist is withheld by it; an object stored is smaller still
	// (about 1.5 MiB with etcd's default request limit).
	MaxEntry = 4 << 20
	// HeapPerByte and HeapFixed bound the heap one redaction may hold (#873, decision of 2026-11-21 on the heap): at
	// most HeapPerByte bytes for each byte of the entry plus HeapFixed, 46 MiB for the largest entry a caller passes
	// (an object of about 1.5 MiB), inside the 48 MiB the decision allows and well inside the analyzer's limit of
	// 128 Mi. TestHeapOfOneEntryIsBounded measures it at the largest entry of every entry point, on the worst shapes.
	HeapPerByte = 28
	HeapFixed   = 4 << 20
	// FreePieces is how many pieces an answer (Pieces), or strings a sequence (Strings), may hold whatever its
	// bytes: past it, an answer of more than one piece for every PieceBytes bytes, or a sequence of more than one
	// string for every ItemBytes bytes, is withheld before any work (ReasonHeap). What a piece or a string holds
	// besides its bytes (its place in the answer, the spans found in it, its result) would take it over HeapPerByte.
	// An answer a read tool writes for an object has each value with its key and the text around it, so it stays
	// under one piece for every PieceBytes bytes.
	FreePieces = 4096
	PieceBytes = 2
	ItemBytes  = 16
)

// Withheld is what an entry point returns in place of an entry it withholds: a fixed marker, with nothing of the
// entry in it. A caller never tells a withheld entry by this text, since a value from the cluster may spell it: an
// entry point that returns a flag ([StringChecked], [Redactor.PiecesWithin] and the others) says so, and a caller
// that shows the entry as a source that failed reads that flag (#873, decision of 2026-11-20). A caller that keeps
// the text form (String) shows the marker in place of the value, as text.
const Withheld = "[REDACTED:withheld]"

// Reasons an entry is withheld, the values of the label "reason" of shadewright_redaction_withheld_total.
const (
	// ReasonWork: the redaction used up its budget.
	ReasonWork = "work"
	// ReasonSize: the entry is longer than MaxEntry, withheld before any work.
	ReasonSize = "size"
	// ReasonHeap: the entry is made of more pieces or strings than its bytes allow (FreePieces), withheld before
	// any work, so that its redaction holds no more heap than the bound (HeapPerByte).
	ReasonHeap = "heap"
)

// withheldTotal counts the entries withheld, by reason; both series start at zero.
var withheldTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_redaction_withheld_total",
	Help: "Entries redaction withheld whole, by reason (work: the work budget ran out; size: longer than the bound; " +
		"heap: more pieces or strings than its bytes allow).",
}, []string{"reason"})

func init() {
	metrics.Registry.MustRegister(withheldTotal)
	withheldTotal.WithLabelValues(ReasonWork)
	withheldTotal.WithLabelValues(ReasonSize)
	withheldTotal.WithLabelValues(ReasonHeap)
}

// tooManyParts reports whether an entry of size bytes made of parts pieces (or strings), each standing for
// perPart bytes, is withheld before any work for the heap they would hold (ReasonHeap), and counts it if so.
func tooManyParts(parts, size, perPart int) bool {
	if parts <= FreePieces+size/perPart {
		return false
	}
	withheldTotal.WithLabelValues(ReasonHeap).Inc()
	return true
}

// budgetOf returns the budget of an entry of size bytes.
func budgetOf(size int) int64 { return WorkFixed + WorkPerByte*int64(size) }

// metered runs f, the redaction of an entry of size bytes, on the budget of that entry, and returns its result
// and true; or the zero value and false when the entry is withheld: longer than MaxEntry (f is not run) or its
// budget ran out (f stopped).
func metered[T any](size int, f func(wm *meter.Meter) T) (out T, ok bool) {
	return meteredOn(size, budgetOf(size), f)
}

// meteredOn is metered with the budget given.
func meteredOn[T any](size int, steps int64, f func(wm *meter.Meter) T) (out T, ok bool) {
	if size > MaxEntry {
		withheldTotal.WithLabelValues(ReasonSize).Inc()
		return out, false
	}
	over := false
	func() {
		defer meter.Catch(&over)
		out = f(meter.New(steps))
	}()
	if over {
		var zero T
		withheldTotal.WithLabelValues(ReasonWork).Inc()
		return zero, false
	}
	return out, true
}

// unstopped returns a meter for the replacement of an entry decided inside its budget (Replace, ReplaceFrom): one
// step per span, so linear, counted and never stopped, since repl and between, the caller's functions, run in it
// and a stop there would leave them half done.
func unstopped() *meter.Meter { return meter.New(math.MaxInt64) }

// meteredText is metered for an entry point that returns a text: Withheld and false in place of an entry
// withheld.
func meteredText(size int, f func(wm *meter.Meter) string) (string, bool) {
	if out, ok := metered(size, f); ok {
		return out, true
	}
	return Withheld, false
}

// withheldStrings returns n strings for an entry of n parts that is withheld: the first is Withheld, the others
// empty, so the parts joined are Withheld.
func withheldStrings(n int) []string {
	out := make([]string, n)
	if n > 0 {
		out[0] = Withheld
	}
	return out
}
