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
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/redact/meter"
)

// The layout's map of units and its line kinds as they were before they were kept as checkpoints (#873, decision
// of 2026-11-21 on the heap): an offset for each byte of the value and two for each byte of the text, and a kind
// and two offsets for each byte of the answer. The tests compare the code with them.

// oldWritten is written as it was: start and end for each byte of the value, unitStart and unitEnd for each
// byte of text, nil when text is the value.
type oldWritten struct {
	text                           string
	start, end, unitStart, unitEnd []int
}

// oldWrite writes the piece p as the answer writes it.
func oldWrite(wm *meter.Meter, p Piece) oldWritten {
	if !p.Data || p.Form == AsIs {
		return oldWritten{text: p.Text}
	}
	n := len(p.Text)
	w := oldWritten{start: make([]int, n), end: make([]int, n)}
	var b strings.Builder
	b.Grow(n)
	for i := 0; i < n; {
		tick(wm, 1)
		c, size := utf8.DecodeRuneInString(p.Text[i:])
		from := b.Len()
		if p.Form == JSON && c == utf8.RuneError && size == 1 {
			// A run of invalid bytes is one unit, written as one "?", as the release before made each value valid
			// UTF-8 before it was encoded (strings.ToValidUTF8 with "?"). A unit is never longer than the bytes it
			// stands for, so an answer of many runs is not longer than the value read: one escape of U+FFFD per
			// byte, then per run (six bytes for one), made an answer of a few KiB of them too long for its bound
			// (#845, fifteenth decision, P4).
			for i+size < n {
				tick(wm, 1)
				if r, sz := utf8.DecodeRuneInString(p.Text[i+size:]); r != utf8.RuneError || sz != 1 {
					break
				}
				size++
			}
			b.WriteByte('?')
		} else {
			writeUnit(wm, &b, p.Form, p.Text[i:i+size], c, size)
		}
		to := b.Len()
		for j := i; j < i+size; j++ {
			tick(wm, 1)
			w.start[j], w.end[j] = from, to
		}
		for range to - from {
			tick(wm, 1)
			w.unitStart, w.unitEnd = append(w.unitStart, from), append(w.unitEnd, to)
		}
		i += size
	}
	w.text = b.String()
	return w
}

// oldEndAtValueLines is endAtValueLines as it was.
func oldEndAtValueLines(wm *meter.Meter, ps []Piece, ws []oldWritten, answer string, matches []span) []span {
	n := len(answer)
	kind := make([]byte, n)
	off := 0
	for i, p := range ps {
		tick(wm, 1)
		w := ws[i]
		if !p.Data {
			for j := range len(w.text) {
				tick(wm, 1)
				if c := w.text[j]; c == ' ' || c == '\t' || c == '\n' || c == '\r' {
					kind[off+j] = byteBlank
				}
			}
			off += len(w.text)
			continue
		}
		for j := 0; j < len(p.Text); {
			tick(wm, 1)
			c, size := utf8.DecodeRuneInString(p.Text[j:])
			s, e := j, j+size
			if w.start != nil {
				s, e = w.start[j], w.end[j]
			}
			k := byte(byteSolid)
			switch {
			case c == utf8.RuneError && size == 1:
			case isLineBreak(wm, c):
				k = byteLineBreak
			case c == ' ' || c == '\t':
				k = byteBlank
			}
			for x := s; x < e; x++ {
				tick(wm, 1)
				kind[off+x] = k
			}
			j += size
		}
		off += len(w.text)
	}
	// nextSolid[x] and nextBreak[x] are the first byte at or after x of that kind, n for none.
	nextSolid, nextBreak := make([]int, n+1), make([]int, n+1)
	nextSolid[n], nextBreak[n] = n, n
	for x := n - 1; x >= 0; x-- {
		tick(wm, 1)
		nextSolid[x], nextBreak[x] = nextSolid[x+1], nextBreak[x+1]
		switch kind[x] {
		case byteSolid:
			nextSolid[x] = x
		case byteLineBreak:
			nextBreak[x] = x
		}
	}
	for i := range matches {
		tick(wm, 1)
		if q := nextSolid[min(matches[i].start, n)]; q < matches[i].end {
			matches[i].end = min(matches[i].end, nextBreak[q])
		}
	}
	return matches
}
