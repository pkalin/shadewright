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
	"unicode"
	"unicode/utf8"

	"github.com/shadewright-labs/shadewright/internal/redact/meter"
)

// flowLine answers, for the keys of one line, where a value in a flow collection ends (viewValueEnd): the
// first comma that starts a plain key and the last closing bracket of the collection. The line is read once
// when the first key asks, and each key's answer then takes the logarithm of the number of brackets and commas
// on the line (a binary search in each list). Read for each key from its value to the end of the line,
// as before, a line of n bare short keys in flow collections ("X{auth: v}\treason", repeated) cost n times the
// line (#845, P3: FuzzDiffReplaceFrom found 915 steps per byte at 40 KB).
//
// A closing bracket that ends a value is the one flowCloseAt of the tests (flowline_old_test.go) finds by
// reading from the value on: the last bracket before the end that closes the collection the value stands in,
// followed after its run of closing brackets and quotes by the end, a blank, a comma, a semicolon or a comment.
// Reading from the value, a bracket closes that collection when it is of the collection's kind and does not
// close a bracket opened inside the value. The brackets of the line are paired once from its start with the
// same rule (a closing bracket pairs with the innermost open bracket when it is of that kind, and is passed
// over otherwise); the brackets opened inside the value are then the open brackets of that pairing after the
// value's start, so a closing bracket of the collection's kind ends the value when it pairs with nothing or
// with a bracket opened before the value starts. Those are the brackets open where the value starts, whose
// closing brackets lie further out the shallower they are, and the closing brackets that pair with nothing.
type flowLine struct {
	s      string
	lo, hi int
	built  bool
	// mate holds, for a bracket at lo+i, the offset of the bracket it pairs with, or -1.
	mate []int32
	// good holds whether the closing bracket at lo+i ends a value whatever the end of the value: its run of
	// closing brackets and quotes ends before the line does, at a blank, a comma, a semicolon or a comment.
	good []bool
	// runStart holds, by end offset e (0 to hi-lo), where the run of closing brackets and quotes that holds
	// lo+e-1 starts, or e when lo+e-1 is no such character.
	runStart []int32
	// For each kind ('[' then '{'), in order: the offsets of the closing brackets of that kind, of those that
	// pair with nothing, and of those that pair with nothing and are good.
	closes, lone, goodLone [2][]int32
	// commas are the offsets of the commas, in order.
	commas []int32
	// keyCommas are the commas that start a plain key (plainKeyAfterComma), with the end of that match, in order.
	keyCommas [][2]int
	// The cursor pairs brackets from lo up to pos: stack holds the offsets of the open brackets, and goodOpen,
	// by kind, the indexes in stack of the open brackets whose closing bracket is good, outermost first.
	pos      int
	stack    []int
	goodOpen [2][]int
}

// flowKind returns 0 for '[' or ']', 1 for '{' or '}'.
func flowKind(_ *meter.Meter, c byte) int {
	if c == '[' || c == ']' {
		return 0
	}
	return 1
}

// newFlowLine returns the line from lo to hi of s, read when a key first asks.
func newFlowLine(_ *meter.Meter, s string, lo, hi int) *flowLine {
	return &flowLine{s: s, lo: lo, hi: hi}
}

// isRunChar reports whether c is a closing bracket or a quote, the characters of a run after a closing bracket.
func isRunChar(_ *meter.Meter, c byte) bool { return strings.IndexByte(`]})"'`, c) >= 0 }

// build reads the line once: the pairs of brackets, the runs of closing characters and the commas. It keeps
// about nine bytes for each byte of the line and four for each bracket and comma.
func (f *flowLine) build(wm *meter.Meter) {
	if f.built {
		return
	}
	f.built = true
	s, lo, n := f.s, f.lo, f.hi-f.lo
	f.mate, f.good, f.runStart = make([]int32, n), make([]bool, n), make([]int32, n+1)
	var open []int32
	for i := range n {
		tick(wm, 1)
		f.mate[i] = -1
		switch c := s[lo+i]; c {
		case '[', '{':
			open = append(open, int32(i))
		case ']', '}':
			if k := len(open); k > 0 && s[lo+int(open[k-1])] == c-2 {
				f.mate[i], f.mate[open[k-1]] = open[k-1], int32(i)
				open = open[:k-1]
			}
		case ',':
			f.commas = append(f.commas, int32(i))
		}
		f.runStart[i+1] = int32(i + 1)
		if isRunChar(wm, s[lo+i]) {
			f.runStart[i+1] = int32(i)
			if i > 0 && isRunChar(wm, s[lo+i-1]) {
				f.runStart[i+1] = f.runStart[i]
			}
		}
	}
	// A closing bracket is good when the run from it ends before the line does at a blank, a comma, a semicolon
	// or a comment; read back to front, with the end of the current run.
	runEnd := n
	for i := n - 1; i >= 0; i-- {
		tick(wm, 1)
		c := s[lo+i]
		if !isRunChar(wm, c) {
			runEnd = i
			continue
		}
		if (c == ']' || c == '}') && runEnd < n {
			f.good[i] = indentLen(wm, s, lo+runEnd) > 0 || strings.IndexByte(",;#", s[lo+runEnd]) >= 0
		}
	}
	for i := range n {
		tick(wm, 1)
		switch c := s[lo+i]; c {
		case ']', '}':
			t := flowKind(wm, c)
			f.closes[t] = append(f.closes[t], int32(i))
			if f.mate[i] < 0 {
				f.lone[t] = append(f.lone[t], int32(i))
				if f.good[i] {
					f.goodLone[t] = append(f.goodLone[t], int32(i))
				}
			}
		case ',':
			if e := plainKeyCommaEnd(wm, s, lo+i, f.hi); e >= 0 {
				f.keyCommas = append(f.keyCommas, [2]int{lo + i, e})
			}
		}
	}
}

// firstFrom returns the index of the first offset in xs (in order) at or after x, or len(xs).
func firstFrom(wm *meter.Meter, xs []int32, x int) int {
	lo, hi := 0, len(xs)
	for lo < hi {
		tick(wm, 1)
		mid := (lo + hi) / 2
		if int(xs[mid]) >= x {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// lastBefore returns the last offset in xs (in order) before e, or -1.
func lastBefore(wm *meter.Meter, xs []int32, e int) int {
	if i := firstFrom(wm, xs, e); i > 0 {
		return int(xs[i-1])
	}
	return -1
}

// nextComma returns the first comma at or after start, or hi.
func (f *flowLine) nextComma(wm *meter.Meter, start int) int {
	f.build(wm)
	if i := firstFrom(wm, f.commas, start-f.lo); i < len(f.commas) {
		return f.lo + int(f.commas[i])
	}
	return f.hi
}

// advance moves the cursor to offset x, pairing the brackets before it as build did.
func (f *flowLine) advance(wm *meter.Meter, x int) {
	if x < f.pos {
		// The values of a line's keys start in order (viewKeyLine), so this does not happen there; read again.
		f.pos, f.stack, f.goodOpen = 0, f.stack[:0], [2][]int{f.goodOpen[0][:0], f.goodOpen[1][:0]}
	}
	for ; f.pos < x; f.pos++ {
		tick(wm, 1)
		i := f.pos
		switch c := f.s[f.lo+i]; c {
		case '[', '{':
			if j := f.mate[i]; j >= 0 && f.good[j] {
				t := flowKind(wm, c)
				f.goodOpen[t] = append(f.goodOpen[t], len(f.stack))
			}
			f.stack = append(f.stack, i)
		case ']', '}':
			if f.mate[i] < 0 {
				continue
			}
			k := len(f.stack) - 1
			t := flowKind(wm, c)
			if g := len(f.goodOpen[t]); g > 0 && f.goodOpen[t][g-1] == k {
				f.goodOpen[t] = f.goodOpen[t][:g-1]
			}
			f.stack = f.stack[:k]
		}
	}
}

// closeAt returns where a value from start to end in a flow collection opened by opener ends: the last closing
// bracket of that collection that flowCloseAt finds, or -1.
func (f *flowLine) closeAt(wm *meter.Meter, start, end int, opener byte) int {
	f.build(wm)
	x, e := start-f.lo, end-f.lo
	if x >= e {
		return -1
	}
	t := flowKind(wm, opener)
	f.advance(wm, x)
	best := -1
	// A closing bracket that pairs with nothing and ends a value whatever its end.
	if u := lastBefore(wm, f.goodLone[t], e); u >= x {
		best = u
	}
	// The good closing bracket of the outermost bracket of the kind open at start that closes before the end:
	// their closing brackets lie further out the shallower they are.
	g := f.goodOpen[t]
	loIdx, hiIdx := 0, len(g)
	for loIdx < hiIdx {
		tick(wm, 1)
		mid := (loIdx + hiIdx) / 2
		if int(f.mate[f.stack[g[mid]]]) < e {
			hiIdx = mid
		} else {
			loIdx = mid + 1
		}
	}
	if loIdx < len(g) {
		best = max(best, int(f.mate[f.stack[g[loIdx]]]))
	}
	// A closing bracket in the run of closing brackets and quotes that reaches the end ends the value whatever
	// follows. The last of the kind there is the outermost; when it closes a bracket opened inside the value, the
	// brackets before it in the run lie inside that pair, and only one that pairs with nothing ends the value.
	if from := max(x, int(f.runStart[e])); from < e {
		if c := lastBefore(wm, f.closes[t], e); c >= from {
			if p := int(f.mate[c]); p < x {
				best = max(best, c)
			} else if u := lastBefore(wm, f.lone[t], c); u >= from {
				best = max(best, u)
			}
		}
	}
	if best < 0 {
		return -1
	}
	return f.lo + best
}

// keyCommaAt returns the first comma from start on that starts a plain key ending at or before end
// (plainKeyAfterComma in s[start:end]), or -1.
func (f *flowLine) keyCommaAt(wm *meter.Meter, start, end int) int {
	f.build(wm)
	cs := f.keyCommas
	loIdx, hiIdx := 0, len(cs)
	for loIdx < hiIdx {
		tick(wm, 1)
		mid := (loIdx + hiIdx) / 2
		if cs[mid][0] >= start {
			hiIdx = mid
		} else {
			loIdx = mid + 1
		}
	}
	// A match is one comma, a quoted or plain key and a colon, and a key that holds a comma holds it in quotes,
	// so the matches that hold end are few: each starts before end and the next starts inside its quotes.
	for i := loIdx; i < len(cs) && cs[i][0] < end; i++ {
		tick(wm, 1)
		if cs[i][1] <= end {
			return cs[i][0]
		}
	}
	return -1
}

// isKeySpace reports whether r is a character of keySpaceExpr.
func isKeySpace(_ *meter.Meter, r rune) bool {
	return r == ' ' || r == '\t' || r == 0x200b || r == 0x2060 || r == 0xfeff || unicode.Is(unicode.Zs, r)
}

// keySpaceRun returns where the run of keySpaceExpr characters from i ends, at most at hi.
func keySpaceRun(wm *meter.Meter, s string, i, hi int) int {
	for i < hi {
		tick(wm, 1)
		r, n := utf8.DecodeRuneInString(s[i:hi])
		if !isKeySpace(wm, r) || r == utf8.RuneError && n <= 1 {
			break
		}
		i += n
	}
	return i
}

// plainKeyCommaEnd returns the end of the match of plainKeyAfterComma that starts at the comma s[c], read up to
// hi, or -1. The match is unique when there is one: the key is quoted or a run of name characters, which a
// shorter key could not end, and the blanks around it are runs.
func plainKeyCommaEnd(wm *meter.Meter, s string, c, hi int) int {
	i := keySpaceRun(wm, s, c+1, hi)
	if i >= hi {
		return -1
	}
	switch q := s[i]; {
	case q == '"' || q == '\'':
		j := i + 1
		for j < hi && s[j] != q && s[j] != '\r' && s[j] != '\n' {
			tick(wm, 1)
			j++
		}
		if j >= hi || s[j] != q {
			return -1
		}
		i = j + 1
	case q == '_' || q >= 'A' && q <= 'Z' || q >= 'a' && q <= 'z':
		i++
		for i < hi {
			tick(wm, 1)
			b := s[i]
			if b != '_' && b != '.' && b != '-' && (b < 'A' || b > 'Z') && (b < 'a' || b > 'z') && (b < '0' || b > '9') {
				break
			}
			i++
		}
	default:
		return -1
	}
	i = keySpaceRun(wm, s, i, hi)
	if i < hi && s[i] == ':' {
		return i + 1
	}
	return -1
}
