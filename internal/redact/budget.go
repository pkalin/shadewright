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
	// withheld from the size where it passes the budget. No shape of the corpus is known to pass it since #962
	// brought the last four that did under the budget ("--token,}" and "-password," repeated, glued quoted
	// pairs, TOKEN="a" repeated before a long blank line). The work tests check that work grows linearly with the text on every shape of the corpus
	// (TestCorpusWorkGrowsLinearly).
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
	// 128 Mi. TestHeapOfOneEntryIsBounded measures it at the largest entry of every entry point, on the worst shapes
	// (#1087: runs of slashes, folds and text dense in secrets among them), with no exception. The redactions
	// running at once share a limit of half the Go memory limit (heaplimit.go).
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
	// FreeMatches and MatchBytes gate the reading of the release before on a text dense in matches (#1087): that
	// reading (package released, kept byte for byte) holds what it finds for every match of every rule until the
	// end of each pass, about 140 bytes a match, so a text with more matches of this redactor's rules than
	// FreeMatches plus one for every MatchBytes bytes is withheld whole before that reading starts (ReasonHeap).
	// The matches of this redactor's rules are counted as they are found, before that reading, and the rules of
	// the release before are a subset of the same expressions, so the count stands for its matches too. A text
	// people write has far fewer: lines of a key and a secret, 17 bytes each ("password=hunter2"), are two matches a
	// line, so they are withheld from about 230 KiB of them on.
	FreeMatches = 16 << 10
	MatchBytes  = 10
)

// heapStop is the value heapGate panics with; meteredOn recovers it and withholds the entry with ReasonHeap.
type heapStop struct{}

// heapGate stops the redaction of an entry of size bytes, before the reading of the release before, when the
// matches of this redactor's rules found on it are more than its bytes allow (FreeMatches, MatchBytes).
func heapGate(matches, size int) {
	if matches > FreeMatches+size/MatchBytes {
		panic(heapStop{})
	}
}

// catchHeap is deferred by meteredOn: it recovers the stop of heapGate and sets *stopped, and lets every other
// panic go on.
func catchHeap(stopped *bool) {
	if r := recover(); r != nil {
		if _, ok := r.(heapStop); !ok {
			panic(r)
		}
		*stopped = true
	}
}

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
	// any work, or holds more matches than its bytes allow (FreeMatches), withheld before the reading of the
	// release before, so that its redaction holds no more heap than the bound (HeapPerByte).
	ReasonHeap = "heap"
)

// withheldTotal counts the entries withheld, by reason (work, size, heap); all three series start at zero.
var withheldTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_redaction_withheld_total",
	Help: "Entries redaction withheld whole, by reason (work: the work budget ran out; size: longer than the bound; " +
		"heap: more pieces, strings or matches than its bytes allow).",
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

// budgetOf returns the budget of an entry of size bytes for r: the package's (budgetOf), or the lower one a test
// set with WithWorkBudget.
func (r *Redactor) budgetOf(size int) int64 {
	b := budgetOf(size)
	if r.lowered {
		b = min(b, r.workBudget)
	}
	return b
}

// WithWorkBudget returns a copy of r on which every entry has at most steps steps of work, whatever its size, and
// withholds an entry that needs more (ReasonWork). It only lowers the budget: an entry keeps its own when that is
// lower. It is for tests of what a caller does with an entry withheld for its work (#962, decision of
// 2026-11-22): since #962 no input of the corpora passes the package's budget, so a test that needs one withheld
// lowers it on its own redactor, through redacttest.LowBudget. Nothing else may call it: the budget of a product
// binary is the package's constant (ADR-0015), and TestLoweredBudgetOnlyInTests fails if code outside _test.go
// files and package redacttest uses it.
func (r *Redactor) WithWorkBudget(steps int64) *Redactor {
	c := *r
	c.lowered, c.workBudget = true, max(steps, 0)
	if r.lowered {
		c.workBudget = min(c.workBudget, r.workBudget)
	}
	return &c
}

// metered runs f, the redaction of an entry of size bytes, on r's budget of that entry, and returns its result
// and true; or the zero value and false when the entry is withheld: longer than MaxEntry (f is not run) or its
// budget ran out (f stopped).
func metered[T any](r *Redactor, size int, f func(wm *meter.Meter) T) (out T, ok bool) {
	return meteredOn(size, r.budgetOf(size), f)
}

// meteredOn is metered with the budget given.
func meteredOn[T any](size int, steps int64, f func(wm *meter.Meter) T) (out T, ok bool) {
	if size > MaxEntry {
		withheldTotal.WithLabelValues(ReasonSize).Inc()
		return out, false
	}
	defer redactionHeap.acquire(size)() // the heap of the redactions running at once (heaplimit.go)
	over, heap := false, false
	func() {
		defer meter.Catch(&over)
		defer catchHeap(&heap)
		out = f(meter.New(steps))
	}()
	if heap {
		var zero T
		withheldTotal.WithLabelValues(ReasonHeap).Inc()
		return zero, false
	}
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
func meteredText(r *Redactor, size int, f func(wm *meter.Meter) string) (string, bool) {
	if out, ok := metered(r, size, f); ok {
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
