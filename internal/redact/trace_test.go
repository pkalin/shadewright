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
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/redact/meter"
)

// oldTrace is the trace as it was before its segments (#873, decision of 2026-11-21 on the heap): two offsets for
// each byte. The test builds both from the same steps and compares what every byte stands for.
type oldTrace struct {
	text   string
	lo, hi []int32
}

func oldTraceOf(s string, at int) oldTrace {
	t := oldTrace{text: s, lo: make([]int32, len(s)), hi: make([]int32, len(s))}
	for i := range len(s) {
		t.lo[i], t.hi[i] = int32(at+i), int32(at+i+1) // #nosec G115 -- a test text
	}
	return t
}

type oldTraceBuilder struct {
	b      strings.Builder
	lo, hi []int32
}

func (tb *oldTraceBuilder) add(s string, lo, hi int32) {
	tb.b.WriteString(s)
	for range len(s) {
		tb.lo, tb.hi = append(tb.lo, lo), append(tb.hi, hi)
	}
}

func (tb *oldTraceBuilder) copy(t oldTrace, from, to int) {
	tb.b.WriteString(t.text[from:to])
	tb.lo, tb.hi = append(tb.lo, t.lo[from:to]...), append(tb.hi, t.hi[from:to]...)
}

func (tb *oldTraceBuilder) trace() oldTrace {
	return oldTrace{text: tb.b.String(), lo: tb.lo, hi: tb.hi}
}

// The trace kept as segments gives every byte the bytes the trace of two offsets a byte gave, through the steps
// the reading of the release before takes: a text traced as itself, units of every length added for one
// character or for a run (escapes, "?", a marker), parts copied from anywhere (inside a unit, across segments),
// and a trace cut from its end. It counts the same steps.
func TestTraceMatchesBefore(t *testing.T) {
	rng := rand.New(rand.NewPCG(873, 47)) //nolint:gosec // a fixed seed keeps the inputs the same on every run
	units := []string{"\\u0001", "\\n", "?", " ", "a", "€", "[REDACTED:credential]", "(12 bytes left out)"}
	for round := range 3000 {
		src := unitValue(rng, 1+rng.IntN(3*unitStep))
		at := rng.IntN(100)
		nt, ot := traceOf(nil, src, at), oldTraceOf(src, at)
		for step := range 1 + rng.IntN(5) {
			var nb traceBuilder
			var ob oldTraceBuilder
			nm, om := meter.New(math.MaxInt64), meter.New(math.MaxInt64)
			pos := 0
			for pos < len(ot.text) {
				switch rng.IntN(3) {
				case 0: // a unit standing for the next bytes
					n := 1 + rng.IntN(min(6, len(ot.text)-pos))
					u := units[rng.IntN(len(units))]
					if rng.IntN(2) == 0 { // a run of units of one length, each for bytes of one length
						for k := 0; k < 8 && pos+n <= len(ot.text); k++ {
							nb.add(nm, u, nt.lo(pos), nt.hi(pos+n-1))
							ob.add(u, ot.lo[pos], ot.hi[pos+n-1])
							om.Spend(len(u))
							pos += n
						}
						continue
					}
					nb.add(nm, u, nt.lo(pos), nt.hi(pos+n-1))
					ob.add(u, ot.lo[pos], ot.hi[pos+n-1])
					om.Spend(len(u))
					pos += n
				default: // a part copied as it is
					n := 1 + rng.IntN(len(ot.text)-pos)
					nb.copy(nm, nt, pos, pos+n)
					ob.copy(ot, pos, pos+n)
					om.Spend(n)
					pos += n
				}
			}
			nt, ot = nb.trace(nm), ob.trace()
			if nm.Spent() != om.Spent() {
				t.Fatalf("round %d, step %d: %d steps, before %d", round, step, nm.Spent(), om.Spent())
			}
			if rng.IntN(3) == 0 && len(ot.text) > 1 {
				from := rng.IntN(len(ot.text))
				nt, ot = nt.from(from), oldTrace{text: ot.text[from:], lo: ot.lo[from:], hi: ot.hi[from:]}
			}
			if nt.text != ot.text {
				t.Fatalf("round %d, step %d: text %q, before %q", round, step, nt.text, ot.text)
			}
			// Every byte, in order and then in an order drawn at random, as the passes look them up.
			for i := range len(ot.text) {
				if lo, hi := nt.lo(i), nt.hi(i); lo != ot.lo[i] || hi != ot.hi[i] {
					t.Fatalf("round %d, step %d, byte %d of %q: [%d, %d), before [%d, %d)", round, step, i, ot.text, lo, hi,
						ot.lo[i], ot.hi[i])
				}
			}
			for range len(ot.text) {
				i := rng.IntN(len(ot.text))
				if lo, hi := nt.lo(i), nt.hi(i); lo != ot.lo[i] || hi != ot.hi[i] {
					t.Fatalf("round %d, step %d, byte %d drawn: [%d, %d), before [%d, %d)", round, step, i, lo, hi, ot.lo[i], ot.hi[i])
				}
			}
			if len(ot.text) == 0 {
				break
			}
		}
	}
}
