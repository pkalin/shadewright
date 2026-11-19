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
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// flowLineUnits are lines of keys in flow collections whose values were each read to the end of the line: a
// bare short key takes no value of its own, so the value of the one before it is not ended there (#845, P3).
// The first is the unit of the FuzzDiffReplaceFrom input testdata/fuzz/FuzzDiffReplaceFrom/999b1875153ad13b,
// with its look-alike colon.
var flowLineUnits = []string{
	"SECRETPROBE0003{auth\u2237SECRETPROBE0004}\treason",
	"X{auth: v}\treason",
	"X[auth: v]\treason",
	"X{auth: | v}\treason",
	"X{auth: [v]}, {auth: v}]]\t",
	"{auth: v, pass: w}}'\t",
}

// The work of String on a line of n, 2n and 4n such units is linear, and within the bound per byte of what the
// rule adds to the release's reading (before flowLine: 303, 542, 1,020 and 1,977 steps per byte at 250, 500,
// 1,000 and 2,000 units of the first).
func TestFlowLineWorkIsLinear(t *testing.T) {
	for _, unit := range flowLineUnits {
		samples := make([]workSample, 0, 3)
		for _, n := range []int{250, 500, 1000} {
			samples = append(samples, sampleWorkAlone(t, strings.Repeat(unit, n)))
		}
		checkWork(t, fmt.Sprintf("flow line %q", unit), samples)
	}
}

// flowLinePieces are the characters the brackets, runs and commas of a line are read from.
var flowLinePieces = []string{
	"[", "]", "{", "}", "\"", "'", ",", ";", "#", " ", "\t", ":", "a", "b_1", "k.e-y", "\u2237", "\u00a0",
	"\u200b", "\ufeff", "\xff", "\r", "\n", authKey, ", 'k':", ",\"a,'b':\":", ", x :", ",'x,y:':",
}

// flowLine gives what the old reading gives (flowline_old_test.go), on generated lines, for values asked in
// order of their start as viewKeyLine asks them and in any order: the last closing bracket of a collection,
// the first comma that starts a plain key, and the first comma.
func TestFlowLineMatchesTheOldReading(t *testing.T) {
	rng := rand.New(rand.NewPCG(845, 919)) // #nosec G404 -- reproducible generated lines
	for range 20000 {
		var b strings.Builder
		for range 1 + rng.IntN(40) {
			b.WriteString(flowLinePieces[rng.IntN(len(flowLinePieces))])
		}
		s := b.String()
		lo := rng.IntN(len(s) + 1)
		hi := lo + rng.IntN(len(s)-lo+1)
		fl := newFlowLine(s, lo, hi)
		starts := make([]int, 1+rng.IntN(8))
		for i := range starts {
			starts[i] = lo + rng.IntN(hi-lo+1)
		}
		if rng.IntN(2) == 0 {
			for i := 1; i < len(starts); i++ {
				starts[i] = max(starts[i], starts[i-1])
			}
		}
		for _, start := range starts {
			end := start + rng.IntN(hi-start+1)
			for _, opener := range []byte{'[', '{'} {
				if got, want := fl.closeAt(start, end, opener), flowCloseAt(s, start, end, opener); got != want {
					t.Fatalf("closeAt(%q, %d, %d, %d, %q) = %d, the old reading %d", s, lo, start, end, opener, got, want)
				}
			}
			want := -1
			if c := plainKeyAfterComma.FindStringIndex(s[start:end]); c != nil {
				want = start + c[0]
			}
			if got := fl.keyCommaAt(start, end); got != want {
				t.Fatalf("keyCommaAt(%q, %d, %d, %d) = %d, the regular expression %d", s, lo, start, end, got, want)
			}
			if start < hi {
				want = -1
				if c := strings.IndexByte(s[start:hi], ','); c >= 0 {
					want = start + c
				}
				if got := fl.nextComma(start); (want < 0 && got != hi) || (want >= 0 && got != want) {
					t.Fatalf("nextComma(%q, %d) = %d, want %d", s, start, got, want)
				}
			}
		}
	}
}

// A value repeated in the pieces of one answer (a label in the series lines of several metric templates, the
// FuzzDiffBuild input testdata/fuzz/FuzzDiffBuild/81e201e983440dcc of the context builder) is read alone once:
// eight copies cost eight times one copy less at least six readings of the value alone.
func TestPiecesReadARepeatedValueOnce(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	value := strings.Repeat("aws_secret_access_key", 256) + "\ufe55\ufeff\xbf\u200b"
	alone := countWork(t, func() { r.ownSpans(value) })
	one := countWork(t, func() { r.PiecesWithin(seriesLinesOf(value, 1), -1) })
	eight := countWork(t, func() { r.PiecesWithin(seriesLinesOf(value, 8), -1) })
	if limit := 8*one - 6*alone; eight > limit {
		t.Errorf("eight copies of a %d byte value cost %d steps, over %d (one copy %d, the value read alone %d): "+
			"each copy is read alone again", len(value), eight, limit, one, alone)
	}
}

// seriesLinesOf returns n series lines of a metric answer, each with value as its one label name.
func seriesLinesOf(value string, n int) []Piece {
	ps := make([]Piece, 0, 3*n)
	for i := range n {
		ps = append(ps, Piece{Text: fmt.Sprintf("template_%d{", i)}, Piece{Text: value, Data: true, Form: Line},
			Piece{Text: "=\"\"} 1\n"})
	}
	return ps
}
