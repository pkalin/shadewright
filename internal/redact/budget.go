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
	// WorkPerByte is the steps the budget gives each byte of an entry: the largest cost per byte measured on the
	// corpus tables (TestBudgetFactorCoversTheCorpus), with headroom.
	WorkPerByte = 1024
	// WorkFixed is the steps the budget gives every entry besides WorkPerByte per byte: the fixed cost of a call
	// on a short entry.
	WorkFixed = 64 << 10
	// MaxEntry is the longest entry, in bytes, that redaction reads: above what the API server accepts for one
	// object (a request body of 3 MiB), so no object that can exist is withheld by it.
	MaxEntry = 4 << 20
)

// Withheld is what an entry point returns in place of an entry it withholds: a fixed marker, with nothing of the
// entry in it. A caller compares with it (or uses [IsWithheld]) and shows the entry as it shows a source that
// failed.
const Withheld = "[REDACTED:withheld]"

// Reasons an entry is withheld, the values of the label "reason" of shadewright_redaction_withheld_total.
const (
	// ReasonWork: the redaction used up its budget.
	ReasonWork = "work"
	// ReasonSize: the entry is longer than MaxEntry, withheld before any work.
	ReasonSize = "size"
)

// withheldTotal counts the entries withheld, by reason; both series start at zero.
var withheldTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_redaction_withheld_total",
	Help: "Entries redaction withheld whole, by reason (work: the work budget ran out; size: longer than the bound).",
}, []string{"reason"})

func init() {
	metrics.Registry.MustRegister(withheldTotal)
	withheldTotal.WithLabelValues(ReasonWork)
	withheldTotal.WithLabelValues(ReasonSize)
}

// IsWithheld reports whether s is what an entry point returns for an entry it withheld.
func IsWithheld(s string) bool { return s == Withheld }

// PiecesWithheld reports whether out, what Pieces, PiecesWithin, PiecesFitting or Strings returned, is an entry
// withheld: Withheld first and every other string empty. A decided answer that only looks so (a value that is the
// marker and nothing else around it) is taken for one too, which only ever shows less.
func PiecesWithheld(out []string) bool {
	if len(out) == 0 || out[0] != Withheld {
		return false
	}
	for _, s := range out[1:] {
		if s != "" {
			return false
		}
	}
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

// meteredString is metered for an entry point that returns a text: Withheld in place of an entry withheld.
func meteredString(size int, f func(wm *meter.Meter) string) string {
	if out, ok := metered(size, f); ok {
		return out
	}
	return Withheld
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
