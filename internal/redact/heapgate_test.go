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
	"strings"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
)

// An answer of more pieces than its bytes allow (FreePieces, one for every PieceBytes bytes past it), or a
// sequence of more strings (one for every ItemBytes), is withheld whole before any work, with the flag and the
// reason heap; one piece or string fewer is decided (#873, decision of 2026-11-21 on the heap).
func TestHeapGateWithholdsTooManyParts(t *testing.T) {
	heap := func() float64 { return promtest.ToFloat64(withheldTotal.WithLabelValues(ReasonHeap)) }
	// Pieces of one byte each (an empty value under a one-letter key, the secret in the first): size = pieces.
	pieces := func(n int) []Piece {
		ps := make([]Piece, n)
		for i := range ps {
			ps[i] = Piece{Data: true, Form: JSON, Key: "k"}
		}
		ps[0].Text, ps[0].Key = "password=hunter2", ""
		return ps
	}
	// The most pieces of one byte the gate lets through: n <= FreePieces + size/PieceBytes, with size = n + 16 - 1.
	most := 0
	for n := FreePieces; n <= 4*FreePieces; n++ {
		if n <= FreePieces+(n+15)/PieceBytes {
			most = n
		}
	}
	for _, c := range []struct {
		name string
		run  func(ps []Piece) ([]string, bool)
	}{
		{epPiecesWithin, func(ps []Piece) ([]string, bool) { return defaultRedactor.PiecesWithin(ps, -1) }},
		{epPiecesFitting, func(ps []Piece) ([]string, bool) {
			return defaultRedactor.PiecesFitting(ps, func([]string) bool { return true })
		}},
	} {
		before := heap()
		out, ok := c.run(pieces(most))
		if !ok || strings.Contains(out[0], "hunter2") {
			t.Errorf("%s of %d pieces: ok %v, %q; want decided and redacted", c.name, most, ok, out[0])
		}
		out, ok = c.run(pieces(most + 1))
		if ok || out[0] != Withheld || len(out) != most+1 {
			t.Errorf("%s of %d pieces: ok %v, first %q, %d strings; want withheld", c.name, most+1, ok, out[0], len(out))
		}
		if got := heap() - before; got != 1 {
			t.Errorf("%s: %v entries counted with reason heap, want 1", c.name, got)
		}
	}
	// Strings of one byte each: size = 2 per string.
	mostItems := 0
	for n := FreePieces; n <= 4*FreePieces; n++ {
		if n <= FreePieces+2*n/ItemBytes {
			mostItems = n
		}
	}
	items := func(n int) []string { return strings.Split(strings.Repeat("x\n", n-1)+"x", "\n") }
	before := heap()
	if _, ok := defaultRedactor.StringsChecked(items(mostItems)); !ok {
		t.Errorf("StringsChecked of %d strings withheld, want decided", mostItems)
	}
	if out, ok := defaultRedactor.StringsChecked(items(mostItems + 1)); ok || out[0] != Withheld {
		t.Errorf("StringsChecked of %d strings: ok %v, first %q; want withheld", mostItems+1, ok, out[0])
	}
	if got := heap() - before; got != 1 {
		t.Errorf("StringsChecked: %v entries counted with reason heap, want 1", got)
	}
}
