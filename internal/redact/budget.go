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
	"context"
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
	// LargestEntry is the largest entry the heap of one redaction was measured for (TestHeapPerByteOfEveryShape,
	// heapbound_test.go): the largest object, about 1.5 MiB with etcd's default request limit. MaxEntry stays the
	// last check for every caller; a read tool never passes an entry larger than LargestEntry (package tools, #1106,
	// decision of 2026-11-22): list_events leaves notes out unread until its answer fits, and any other answer over
	// it is withheld whole (OverLargestEntry).
	LargestEntry = 3 << 19
	// HeapPerByte and HeapFixed bound the heap one redaction may hold (#873, decision of 2026-11-21 on the heap): at
	// most HeapPerByte bytes for each byte of the entry plus HeapFixed, 46 MiB for the largest entry a caller passes
	// (an object of about 1.5 MiB), inside the 48 MiB the decision allows. It is the bound of the shapes the heap
	// tests list as within it (heapShape.overAt), not of every shape: text dense in matches or in control characters
	// holds more, at worst 71.7 bytes per byte measured (#1087, TestHeapPerByteOfEveryShapePieces). The redactions
	// running at once are weighed for that worst case instead (HeapWeightPerByte, heaplimit.go).
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
	// ReasonSize: the entry is longer than MaxEntry, or a read tool's answer is longer than LargestEntry
	// (OverLargestEntry), withheld before any work.
	ReasonSize = "size"
	// ReasonHeap: the entry is made of more pieces or strings than its bytes allow (FreePieces), withheld before
	// any work, so that its redaction holds no more heap than the bound (HeapPerByte).
	ReasonHeap = "heap"
	// ReasonWait: the wait for the limit on the heap of the redactions running at once ended before the entry
	// could run (its context ended, or maxHeapWait passed), withheld before any work (heaplimit.go).
	ReasonWait = "wait"
)

// withheldTotal counts the entries withheld, by reason (work, size, heap, wait); every series starts at zero.
var withheldTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_redaction_withheld_total",
	Help: "Entries redaction withheld whole, by reason (work: the work budget ran out; size: longer than the bound; " +
		"heap: more pieces or strings than its bytes allow; wait: the wait for the heap limit of redactions at once ended).",
}, []string{"reason"})

func init() {
	metrics.Registry.MustRegister(withheldTotal)
	withheldTotal.WithLabelValues(ReasonWork)
	withheldTotal.WithLabelValues(ReasonSize)
	withheldTotal.WithLabelValues(ReasonHeap)
	withheldTotal.WithLabelValues(ReasonWait)
}

// PiecesSize is the size of the entry the pieces make, as the budget counts it: the bytes of each piece's text
// and of its key, as the caller passes them, before anything is written in its Form or made valid UTF-8.
func PiecesSize(ps []Piece) int {
	size := 0
	for _, p := range ps {
		size += len(p.Text) + len(p.Key)
	}
	return size
}

// OverLargestEntry reports whether an entry of size bytes is larger than LargestEntry, and if so counts it as
// withheld with ReasonSize: a caller that bounds its entries to LargestEntry withholds such an entry whole before
// any work, as an entry over MaxEntry is withheld here (#1106).
func OverLargestEntry(size int) bool {
	if size <= LargestEntry {
		return false
	}
	withheldTotal.WithLabelValues(ReasonSize).Inc()
	return true
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

// WithContext returns a copy of r whose entries wait for the heap limit of the redactions running at once
// (heaplimit.go) at most until ctx ends, and are withheld (ReasonWait) if it ends first. It changes nothing else: an
// entry that runs is decided as r decides it, and a running decision is not stopped by ctx. A redactor without a
// context waits at most maxHeapWait.
func (r *Redactor) WithContext(ctx context.Context) *Redactor {
	c := *r
	c.ctx = ctx
	return &c
}

// metered runs f, the redaction of an entry of size bytes, on r's budget of that entry, and returns its result
// and true; or the zero value and false when the entry is withheld: longer than MaxEntry or its wait for the heap
// limit ended (f is not run), or its budget ran out (f stopped).
func metered[T any](r *Redactor, size int, f func(wm *meter.Meter) T) (out T, ok bool) {
	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background() // the wait ends after maxHeapWait
	}
	return meteredOn(ctx, size, r.budgetOf(size), f)
}

// meteredOn is metered with the budget given, its wait for the heap limit ending with ctx or at maxHeapWait.
func meteredOn[T any](ctx context.Context, size int, steps int64, f func(wm *meter.Meter) T) (out T, ok bool) {
	out, why := meteredWhy(ctx, size, steps, f)
	return out, why == ""
}

// meteredWhy is meteredOn with the reason the entry was withheld (ReasonSize, ReasonWait or ReasonWork), or ""
// when it was decided.
func meteredWhy[T any](ctx context.Context, size int, steps int64, f func(wm *meter.Meter) T) (out T, why string) {
	if size > MaxEntry {
		withheldTotal.WithLabelValues(ReasonSize).Inc()
		return out, ReasonSize
	}
	// The heap of the redactions running at once (heaplimit.go): given back on every return and on a panic.
	release, ok := redactionHeap.acquire(ctx, size)
	if !ok {
		withheldTotal.WithLabelValues(ReasonWait).Inc()
		return out, ReasonWait
	}
	defer release()
	if out, ok = onBudget(steps, f); !ok {
		return out, ReasonWork
	}
	return out, ""
}

// onBudget runs f on a meter of steps steps and returns its result and true, or the zero value and false, counted
// as ReasonWork, when the meter ran out.
func onBudget[T any](steps int64, f func(wm *meter.Meter) T) (out T, ok bool) {
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
