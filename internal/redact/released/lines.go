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

package released

import (
	"math"
	"strings"

	"github.com/shadewright-labs/shadewright/internal/redact/meter"
)

// lineIndex answers indentedLines and blockLooksLikeSecret for one text in logarithmic work per call (#873).
// Read line by line, both cost the lines of the block after a header: the block scalar rule and the value rule
// ask them for every header, and a header inside a block they refuse (a bare short key whose lines look like
// no secret) asks again for the lines after it, so a staircase of headers, each one column deeper than the one
// before, read the rest of the text once per header (505 steps per byte at 64 KiB, 1,952 at 1 MiB). The index
// reads the text once, on first use, and gives the same answers.
//
// One index belongs to one call of spans and to the text that call reads; it is not safe for concurrent use.
type lineIndex struct {
	s     string
	built bool
	// starts are the offsets where the lines of s start: 0, and the byte after each "\n".
	starts []int32
	// tree is a segment tree, over the lines, of the column of each line's text, or blankKey for a line of
	// blanks only; size is the number of its leaves (a power of two, at least 2), line i is leaf size+i.
	tree []int32
	size int
	// looks are the counts of lines, from line 0, whose text looks like a secret (lineLooksLikeSecret):
	// looks[i] for the lines before line i, computed as far as a call needed.
	looks []int32
}

// blankKey is the key of a line of blanks only, which ends no block.
const blankKey = math.MaxInt32

// newLineIndex returns the index of s, built on first use.
func newLineIndex(_ *meter.Meter, s string) *lineIndex { return &lineIndex{s: s} }

// usable reports whether x indexes s: it does when it was made for s and s fits the offsets of the index.
// The callers hand on the text of the one call of spans they run in, so a nil index or another text only
// happens for callers outside that call, which read the lines one by one as before.
func (x *lineIndex) usable(_ *meter.Meter, s string) bool {
	return x != nil && len(x.s) == len(s) && len(s) < blankKey
}

// build reads the lines of the text once.
func (x *lineIndex) build(wm *meter.Meter) {
	if x.built {
		return
	}
	x.built = true
	s := x.s
	x.starts = append(x.starts, 0)
	for from := 0; ; {
		i := indexByte(wm, s[from:], '\n')
		if i < 0 {
			break
		}
		from += i + 1
		x.starts = append(x.starts, int32(from)) // #nosec G115 -- usable bounds len(s) below MaxInt32
	}
	n := len(x.starts)
	x.size = 2
	for x.size < n {
		x.size *= 2
	}
	x.tree = make([]int32, 2*x.size)
	for i := range x.tree {
		tick(wm, 1)
		x.tree[i] = blankKey
	}
	for i := range n {
		tick(wm, 1)
		start, end := x.textBounds(wm, i)
		col := int32(0)
		for p := start; p < end && (s[p] == ' ' || s[p] == '\t'); p++ {
			tick(wm, 1)
			col++
		}
		if int(col) < end-start {
			x.tree[x.size+i] = col
		}
	}
	for i := x.size - 1; i > 0; i-- {
		tick(wm, 1)
		x.tree[i] = min(x.tree[2*i], x.tree[2*i+1])
	}
}

// textBounds returns where line i starts and where it ends: before its "\n", and before one "\r" ahead of it
// or at the end of the text (lineEnd).
func (x *lineIndex) textBounds(_ *meter.Meter, i int) (int, int) {
	start := int(x.starts[i])
	end := len(x.s)
	if i+1 < len(x.starts) {
		end = int(x.starts[i+1]) - 1
	}
	if end > start && x.s[end-1] == '\r' {
		end--
	}
	return start, end
}

// firstAtMost returns the first line from line from on whose key is at most c, or the number of lines.
func (x *lineIndex) firstAtMost(wm *meter.Meter, from, c int) int {
	n := len(x.starts)
	if from >= n {
		return n
	}
	i := x.size + from
	for {
		tick(wm, 1)
		if int(x.tree[i]) <= c {
			for i < x.size {
				tick(wm, 1)
				if int(x.tree[2*i]) <= c {
					i = 2 * i
				} else {
					i = 2*i + 1
				}
			}
			return i - x.size
		}
		for i > 1 && i&1 == 1 {
			tick(wm, 1)
			i >>= 1
		}
		if i == 1 {
			return n
		}
		i++
	}
}

// lastAtMost returns the last line before line before whose key is at most c, or -1.
func (x *lineIndex) lastAtMost(wm *meter.Meter, before, c int) int {
	if before <= 0 {
		return -1
	}
	i := x.size + before - 1
	for {
		tick(wm, 1)
		if int(x.tree[i]) <= c {
			for i < x.size {
				tick(wm, 1)
				if int(x.tree[2*i+1]) <= c {
					i = 2*i + 1
				} else {
					i = 2 * i
				}
			}
			return i - x.size
		}
		for i > 1 && i&1 == 0 {
			tick(wm, 1)
			i >>= 1
		}
		if i == 1 {
			return -1
		}
		i--
	}
}

// lineAfter returns the line that starts at pos+1, where pos is at a "\n".
func (x *lineIndex) lineAfter(wm *meter.Meter, pos int) int {
	lo, hi := 0, len(x.starts)
	for lo < hi {
		tick(wm, 1)
		mid := int(uint(lo+hi) >> 1)
		if int(x.starts[mid]) <= pos {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// indentedLines is indentedLines(s, pos, keyCol) on the indexed text, and the lines of the block it found:
// first and last, or -1, -1 with no lines.
func (x *lineIndex) indentedLines(wm *meter.Meter, pos, keyCol int) (start, end, first, last int) {
	x.build(wm)
	s := x.s
	// As nextBlockLine: carriage returns are skipped, and the block starts after a line feed.
	for pos < len(s) && s[pos] == '\r' {
		tick(wm, 1)
		pos++
	}
	if pos >= len(s) || s[pos] != '\n' {
		return -1, -1, -1, -1
	}
	from := x.lineAfter(wm, pos)
	stop := x.firstAtMost(wm, from, min(keyCol, blankKey-1))
	first = x.firstAtMost(wm, from, blankKey-1)
	if first >= stop {
		return -1, -1, -1, -1
	}
	last = x.lastAtMost(wm, stop, blankKey-1)
	start, _ = x.textBounds(wm, first)
	start += int(x.tree[x.size+first])
	_, end = x.textBounds(wm, last)
	return start, end, first, last
}

// anyLooksLikeSecret is blockLooksLikeSecret on the block of lines first to last, as indentedLines found it:
// whether the text of one of them, trimmed, looks like a secret. Each line is judged once per text.
func (x *lineIndex) anyLooksLikeSecret(wm *meter.Meter, first, last int) bool {
	if len(x.looks) == 0 {
		x.looks = append(x.looks, 0)
	}
	for k := len(x.looks) - 1; k <= last; k++ {
		tick(wm, 1)
		start := int(x.starts[k])
		end := len(x.s)
		if k+1 < len(x.starts) {
			end = int(x.starts[k+1]) - 1
		}
		n := x.looks[k]
		if lineLooksLikeSecret(wm, strings.Trim(counted(wm, x.s[start:end]), " \t\r")) {
			n++
		}
		x.looks = append(x.looks, n)
	}
	return x.looks[last+1] > x.looks[first]
}

// indented returns indentedLines(s, pos, keyCol), from x when it indexes s.
func (x *lineIndex) indented(wm *meter.Meter, s string, pos, keyCol int) (int, int) {
	if !x.usable(wm, s) {
		return indentedLines(wm, s, pos, keyCol)
	}
	start, end, _, _ := x.indentedLines(wm, pos, keyCol)
	return start, end
}
