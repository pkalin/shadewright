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
	"slices"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/redact/meter"
)

// unitAlphabet holds what the forms write apart: plain bytes, blanks, line breaks of every kind, the characters
// JSON escapes in two and in six bytes, characters of two to four bytes, U+FFFD itself, and invalid bytes alone,
// in runs and as the start of a sequence cut short.
var unitAlphabet = []string{
	"a", "Z", "0", " ", "\t", "\n", "\r", "\v", "\f", "\b", "\x00", "\x01", "\x1f", "\x7f", `"`, `\`, "?", "u",
	"\u0085", " ", " ", "é", "€", "😀", "�", "\x80", "\xff", "\xe2\x80", "\xf0\x9f\x98", "\xc3",
}

// unitValue draws a value of up to n units of unitAlphabet; runs of one unit long enough to pass several
// checkpoints come often.
func unitValue(rng *rand.Rand, n int) string {
	var b strings.Builder
	for b.Len() < n {
		u := unitAlphabet[rng.IntN(len(unitAlphabet))]
		k := 1
		if rng.IntN(4) == 0 {
			k = rng.IntN(3 * unitStep)
		}
		b.WriteString(strings.Repeat(u, k))
	}
	return b.String()
}

// The map of units kept as checkpoints gives, for every byte of the value and of the text, the unit the map before
// gave (an offset for each byte), in every form, and counts the same steps; a span of the text counts the same
// units at a shorter marker. The values are drawn so that checkpoints fall inside escapes, characters and runs of
// invalid bytes.
func TestWrittenUnitsMatchBefore(t *testing.T) {
	rng := rand.New(rand.NewPCG(873, 21)) //nolint:gosec // a fixed seed keeps the inputs the same on every run
	for round := range 3000 {
		v := unitValue(rng, rng.IntN(5*unitStep))
		if round%4 == 0 { // a plain value, or one byte from it
			v = strings.Repeat("ab ?u0", rng.IntN(60))
			if v != "" && rng.IntN(2) == 0 {
				i := rng.IntN(len(v))
				v = v[:i] + unitAlphabet[rng.IntN(len(unitAlphabet))] + v[i+1:]
			}
		}
		for _, f := range []Form{AsIs, Line, JSON} {
			p := Piece{Text: v, Data: true, Form: f}
			nm, om := meter.New(math.MaxInt64), meter.New(math.MaxInt64)
			w, o := write(nm, p), oldWrite(om, p)
			if writtenLen(p) != len(o.text) {
				t.Fatalf("round %d, form %d, value %q: length %d, before %d", round, f, v, writtenLen(p), len(o.text))
			}
			if w.text != o.text {
				t.Fatalf("round %d, form %d, value %q: text %q, before %q", round, f, v, w.text, o.text)
			}
			if nm.Spent() != om.Spent() {
				t.Fatalf("round %d, form %d, value %q: %d steps, before %d", round, f, v, nm.Spent(), om.Spent())
			}
			if !w.mapped() {
				// No map: the value is written as itself, one byte a unit, as the map before said.
				if o.start != nil && (w.text != v || !plain(f, v)) {
					t.Fatalf("round %d, form %d, value %q: no map for a value that is not plain", round, f, v)
				}
				for j := range len(o.start) {
					if o.start[j] != j || o.end[j] != j+1 || o.unitStart[j] != j || o.unitEnd[j] != j+1 {
						t.Fatalf("round %d, form %d, value %q: byte %d is no unit of its own before", round, f, v, j)
					}
				}
				continue
			}
			for j := range len(v) {
				if s, e := w.unitOfValue(j); s != o.start[j] || e != o.end[j] {
					t.Fatalf("round %d, form %d, value %q, byte %d: unit [%d, %d), before [%d, %d)", round, f, v, j, s, e,
						o.start[j], o.end[j])
				}
			}
			for y := range len(w.text) {
				if s, e := w.unitOfText(y); s != o.unitStart[y] || e != o.unitEnd[y] {
					t.Fatalf("round %d, form %d, text %q, byte %d: unit [%d, %d), before [%d, %d)", round, f, w.text, y, s, e,
						o.unitStart[y], o.unitEnd[y])
				}
			}
			var units [][4]int
			w.eachUnit(func(vs, ve, us, ue int) { units = append(units, [4]int{vs, ve, us, ue}) })
			for _, u := range units {
				for j := u[0]; j < u[1]; j++ {
					if o.start[j] != u[2] || o.end[j] != u[3] {
						t.Fatalf("round %d, form %d, value %q: unit %v, byte %d before [%d, %d)", round, f, v, u, j, o.start[j], o.end[j])
					}
				}
			}
			if len(w.text) > 0 {
				a := rng.IntN(len(w.text))
				b := a + 1 + rng.IntN(len(w.text)-a)
				sp := w.widen(nil, span{start: a, end: b})
				if got, want := w.size(nil, sp, 1), oldSize(o, sp); got != want {
					t.Fatalf("round %d, form %d, text %q, span %v: %d units, before %d", round, f, w.text, sp, got, want)
				}
			}
		}
	}
}

// oldSize is written.size at a shorter marker as it was.
func oldSize(o oldWritten, sp span) int {
	n := 0
	for x := sp.start; x < sp.end; x = o.unitEnd[x] {
		n++
	}
	return n
}

// endAtValueLines with its kinds held two bits a byte ends every match where it ended with a kind and two offsets
// a byte, and counts the same steps, with or without a match. The answers are drawn from pieces in every form, so
// that blocks of the kinds fall anywhere.
func TestEndAtValueLinesMatchesBefore(t *testing.T) {
	rng := rand.New(rand.NewPCG(873, 440)) //nolint:gosec // a fixed seed keeps the inputs the same on every run
	for round := range 2000 {
		var ps []Piece
		for range 1 + rng.IntN(6) {
			if rng.IntN(3) == 0 {
				ps = append(ps, Piece{Text: unitValue(rng, rng.IntN(40))})
				continue
			}
			ps = append(ps, Piece{Text: unitValue(rng, rng.IntN(4*unitStep)), Data: true, Form: Form(rng.IntN(3))})
		}
		ws, os := make([]written, len(ps)), make([]oldWritten, len(ps))
		var answer strings.Builder
		for i, p := range ps {
			ws[i], os[i] = write(nil, p), oldWrite(nil, p)
			answer.WriteString(ws[i].text)
		}
		a := answer.String()
		var matches []span
		for range rng.IntN(8) {
			if len(a) == 0 {
				break
			}
			s := rng.IntN(len(a))
			matches = append(matches, span{start: s, end: s + 1 + rng.IntN(len(a)-s)})
		}
		nm, om := meter.New(math.MaxInt64), meter.New(math.MaxInt64)
		got := endAtValueLines(nm, ps, func(i int) written { return ws[i] }, a, slices.Clone(matches))
		want := oldEndAtValueLines(om, ps, os, a, slices.Clone(matches))
		if !slices.Equal(got, want) {
			t.Fatalf("round %d, answer %q, matches %v: %v, before %v", round, a, matches, got, want)
		}
		if nm.Spent() != om.Spent() {
			t.Fatalf("round %d, answer %q: %d steps, before %d", round, a, nm.Spent(), om.Spent())
		}
	}
}
