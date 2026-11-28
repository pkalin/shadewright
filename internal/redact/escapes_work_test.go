//go:build redactwork

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
	"strings"
	"testing"
)

// escapesPiecesShapes are the answers the work of the values views of the layout (#1223) is measured on: one value
// written on a line with keys in colours, the same as JSON, and many short values each with a colour sequence, a
// tool's text between them.
var escapesPiecesShapes = []struct {
	name   string
	pieces func(n int) []Piece
}{
	{"one line value", func(n int) []Piece {
		return []Piece{{Text: repeatTo("pass\x1b[1mword=a\x1b[0m ", n), Data: true, Form: Line}}
	}},
	{"one JSON value", func(n int) []Piece {
		return []Piece{{Text: repeatTo("\x1b]0;t\x07pwd\x1b[0m=a\n", n), Data: true, Form: JSON}}
	}},
	{"many values", func(n int) []Piece {
		const unit = "\x1b[36mk\x1b[0m"
		ps := make([]Piece, 0, 2*n/(len(unit)+1))
		for range n / (len(unit) + 1) {
			ps = append(ps, Piece{Text: unit, Data: true, Form: Line}, Piece{Text: " "})
		}
		return ps
	}},
}

// The values views of the layout are linear in the length of the answer (#1223): on each shape, the work of
// PiecesWithin per byte at 512 KiB is at most 1.3 times its work per byte at 32 KiB, and the answer is decided,
// never withheld.
func TestValuesEscapesViewWorkIsLinear(t *testing.T) {
	if raceDetector {
		t.Skip("it counts work on answers up to 512 KiB")
	}
	r := defaultRedactor
	for _, sh := range escapesPiecesShapes {
		per := map[int]float64{}
		var line strings.Builder
		for _, n := range []int{32 << 10, 1 << 19} {
			ps := sh.pieces(n)
			var ok bool
			work := Work(func() { _, ok = r.PiecesWithin(ps, -1) })
			per[n] = float64(work) / float64(PiecesSize(ps))
			fmt.Fprintf(&line, " %d KiB: %.0f (decided %v);", n>>10, per[n], ok)
			if !ok {
				t.Errorf("%s: PiecesWithin withheld an answer of %d bytes", sh.name, n)
			}
		}
		t.Logf("%s, steps per byte:%s", sh.name, line.String())
		if small, large := per[32<<10], per[1<<19]; large > 1.3*small+1 {
			t.Errorf("%s: %.0f steps per byte at 512 KiB, %.0f at 32 KiB: not linear", sh.name, large, small)
		}
	}
}
