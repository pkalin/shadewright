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
	"math/bits"
	"regexp"
	"regexp/syntax"
	"slices"
	"unicode"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/redact/meter"
)

// The regular expressions of the rules cost far more per byte than the count of their work says on some texts:
// Go's matcher steps every live thread of the program at every character, and a long run of key words
// ("aws_secret_access_key" repeated, the label of FuzzDiffBuild input 81e201e983440dcc) keeps the threads of
// every key word alive (#873, second round). An answer that repeats such a run (a label in the series of eight
// metric templates) paid that price once per copy. A windowed expression reads each distinct long run once.
//
// What it relies on: a match of an expression that cannot match the empty text holds only characters the
// expression can consume (its alphabet, read from its syntax), and a character that the expression consumes only
// as the first of a match (a start character: "{" and "," for the flow rule) stands nowhere else in one. So no
// match crosses a character outside the alphabet or the start of a start character, and the matches of
// FindAllStringSubmatchIndex on the whole text are the matches that start in each window (a maximal run of
// characters of the alphabet, cut before each start character), read with the character before and after it, in
// order. Those two characters are all that the assertions of Go's syntax look at (^ and $ with or without (?m),
// \A, \z, \b, \B: one character on each side). A match found on them is dropped: one that starts on the
// character after a window belongs to the next window, and one that starts on the character before a window cannot
// reach into it (it would hold the window's first character, outside the alphabet or a start character, at
// another place than its first). Before a window that begins with a start character, the character before only
// tells the assertions whether it is a line break, a word character or neither, so it is read as "\n", "a" or " ".
// A window read with the same characters around it gives the same matches wherever it stands, so a long run
// repeated in a text is read once. The parts of a text between long windows are read as they are, with the
// character on each side, with the same rule.

// windowMinText is the length from which a text is read by windows; a shorter one is read whole, as before.
const windowMinText = 4 << 10

// windowMinRun is the length from which a window is read on its own and remembered; shorter ones are read
// with the parts of the text between long windows.
const windowMinRun = 512

// runeSet is a set of characters.
type runeSet struct {
	ascii [utf8.RuneSelf]bool
	// ranges are the sorted, disjoint ranges of the set from utf8.RuneSelf on, as pairs of their first and last.
	ranges []rune
}

// newRuneSet returns the set of the ranges in pairs (pairs of a first and a last character).
func newRuneSet(_ *meter.Meter, pairs []rune) runeSet {
	var set runeSet
	type rng struct{ lo, hi rune }
	rs := make([]rng, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		rs = append(rs, rng{pairs[i], pairs[i+1]})
	}
	slices.SortFunc(rs, func(a, b rng) int { return int(a.lo - b.lo) })
	for _, r := range rs {
		for c := max(r.lo, 0); c <= r.hi && c < utf8.RuneSelf; c++ {
			set.ascii[c] = true
		}
		lo := max(r.lo, utf8.RuneSelf)
		if lo > r.hi {
			continue
		}
		if n := len(set.ranges); n > 0 && lo <= set.ranges[n-1]+1 {
			set.ranges[n-1] = max(set.ranges[n-1], r.hi)
			continue
		}
		set.ranges = append(set.ranges, lo, r.hi)
	}
	return set
}

// in reports whether r is in the set.
func (set *runeSet) in(_ *meter.Meter, r rune) bool {
	if r < utf8.RuneSelf {
		return r >= 0 && set.ascii[r]
	}
	i, found := slices.BinarySearch(set.ranges, r)
	// r is in a range when it is one of its ends, or falls between the first (even index) and last of a pair.
	return found || i%2 == 1
}

// windowed is a regular expression with its alphabet and its start characters.
type windowed struct {
	re *regexp.Regexp
	// whole is true when the expression is read whole on every text: it can match the empty text, or its
	// alphabet has every character.
	whole bool
	// begins is true when the expression asserts the start of the text or of a line (\A, ^).
	begins bool
	// alpha is the alphabet; later the characters a match can hold at another place than its first.
	alpha, later runeSet
	// idx is the expression's place in the redactor's wins.
	idx int
	// onRead, when not nil, is told the length of each part of a text the expression reads (tests).
	onRead func(n int)
	// expr is re with the bytes its engine reads charged attempt by attempt (#873).
	expr *meter.Expr
}

// newWindowed returns re with its alphabet and start characters.
func newWindowed(_ *meter.Meter, re *regexp.Regexp) *windowed {
	w := &windowed{re: re, whole: true, expr: meter.NewExpr(re)}
	tree, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		return w
	}
	tree = tree.Simplify()
	var all, later []rune
	if minLen(nil, tree) == 0 || !alphabet(nil, tree, &all) || !laterAlphabet(nil, tree, &later) {
		return w
	}
	w.whole = false
	w.alpha, w.later, w.begins = newRuneSet(nil, all), newRuneSet(nil, later), asserts(nil, tree, syntax.OpBeginText, syntax.OpBeginLine)
	return w
}

// asserts reports whether re holds one of the assertions ops.
func asserts(_ *meter.Meter, re *syntax.Regexp, ops ...syntax.Op) bool {
	if slices.Contains(ops, re.Op) {
		return true
	}
	for _, sub := range re.Sub {
		if asserts(nil, sub, ops...) {
			return true
		}
	}
	return false
}

// minLen returns the fewest characters a match of re consumes.
func minLen(_ *meter.Meter, re *syntax.Regexp) int {
	switch re.Op {
	case syntax.OpLiteral:
		return len(re.Rune)
	case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return 1
	case syntax.OpCapture, syntax.OpPlus:
		return minLen(nil, re.Sub[0])
	case syntax.OpRepeat:
		return re.Min * minLen(nil, re.Sub[0])
	case syntax.OpConcat:
		n := 0
		for _, sub := range re.Sub {
			n += minLen(nil, sub)
		}
		return n
	case syntax.OpAlternate:
		n := -1
		for _, sub := range re.Sub {
			if m := minLen(nil, sub); n < 0 || m < n {
				n = m
			}
		}
		return max(n, 0)
	default: // empty matches, assertions, stars and quests
		return 0
	}
}

// literalRunes appends to pairs the characters a literal matches: each of its runes, with its case orbit
// (unicode.SimpleFold) when it folds case, as the matcher takes them.
func literalRunes(_ *meter.Meter, rs []rune, fold bool, pairs *[]rune) {
	for _, r := range rs {
		*pairs = append(*pairs, r, r)
		if fold {
			for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
				*pairs = append(*pairs, f, f)
			}
		}
	}
}

// alphabet appends to pairs the ranges of the characters a match of re can consume, and reports false when that
// is every character.
func alphabet(wm *meter.Meter, re *syntax.Regexp, pairs *[]rune) bool {
	switch re.Op {
	case syntax.OpLiteral:
		literalRunes(wm, re.Rune, re.Flags&syntax.FoldCase != 0, pairs)
		return true
	case syntax.OpCharClass:
		*pairs = append(*pairs, re.Rune...)
		return true
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return false
	default:
		for _, sub := range re.Sub {
			if !alphabet(wm, sub, pairs) {
				return false
			}
		}
		return true
	}
}

// laterAlphabet appends to pairs the ranges of the characters a match of re can hold at another place than its
// first (more of them than that, never fewer), and reports false when that is every character.
func laterAlphabet(wm *meter.Meter, re *syntax.Regexp, pairs *[]rune) bool {
	switch re.Op {
	case syntax.OpLiteral:
		if len(re.Rune) > 1 {
			literalRunes(wm, re.Rune[1:], re.Flags&syntax.FoldCase != 0, pairs)
		}
		return true
	case syntax.OpStar, syntax.OpPlus:
		// A repeated part can follow itself: all of its characters may stand later.
		return alphabet(wm, re.Sub[0], pairs)
	case syntax.OpRepeat:
		if re.Max == 1 {
			return laterAlphabet(wm, re.Sub[0], pairs)
		}
		return alphabet(wm, re.Sub[0], pairs)
	case syntax.OpConcat:
		consumed := false
		for _, sub := range re.Sub {
			if !laterAlphabet(wm, sub, pairs) {
				return false
			}
			if consumed && !alphabet(wm, sub, pairs) {
				return false
			}
			var own []rune
			if alphabet(wm, sub, &own); len(own) > 0 {
				consumed = true
			}
		}
		return true
	default: // a class or any character (one character, the first of itself), a capture, an alternation, a quest
		for _, sub := range re.Sub {
			if !laterAlphabet(wm, sub, pairs) {
				return false
			}
		}
		return true
	}
}

// longRun is a window of at least windowMinRun bytes, or a chunk of shorter windows that follow each other: the
// characters [start, end) of a text, with prev the start of the character before it (start when there is none),
// next the end of the character after it (end when there is none) and last the start of its last character. cut
// is true when it begins with a start character after a character of the alphabet. follows is true when no
// window of the expression stands between the long run before it (or the start of the text) and this one: the
// characters between hold none of the alphabet, so no match starts there and they need no reading. final is true
// on the last long run when no window stands after it.
type longRun struct {
	prev, start, end, next, last int
	cut, follows, final          bool
}

// A chunk is read like one window (#873, third round): the windows of a text that are shorter than windowMinRun (a
// label of many short key words, separators and values, "TOKEN=\"a\",b,DB_PASSWORD: |+\n  ..." in FuzzDiffBuild
// input 22905eb52eae1385) were read with the parts of the text between long windows, so an answer that repeats such
// a label in each of its lines read every copy. A chunk is a run of consecutive windows, from the start of one to
// the end of another, of at least windowMinRun bytes, or shorter where windowMinRun bytes or more without a window
// follow it (those bytes are then not read: no match starts in them). No match crosses the end of a window, so the
// matches that start in a chunk read with the character on each side are the matches the whole text has there, as
// for one window. Where a chunk ends is decided by the text alone: at the end of a window where the bytes before it
// hash to a chosen value (chunkCut), once the chunk is long enough, or at the first end of a window past
// chunkMaxRun. Two copies of a text then end their chunks at the same places once they have ended one at the same
// place, and a chunk repeated is read once.

// chunkHashBytes is how many bytes before the end of a window decide whether a chunk ends there.
const chunkHashBytes = 8

// chunkMaxRun is the length from which a chunk ends at the end of any window. A text of one short unit repeated
// ("aws_secret_access_key", 21 bytes) ends its windows at a few places of the unit, where the hash may never
// choose one: its chunks then end at this length, so they begin at one of those few places of the unit and a
// copy of the text holds the same few chunks.
const chunkMaxRun = 4 * windowMinRun

// chunkCut reports whether a chunk may end at byte i of s: the chunkHashBytes bytes before i hash to one of 16
// values, so chunks of short windows end about every 16th window once they are long enough.
func chunkCut(_ *meter.Meter, s string, i int) bool {
	var x uint64
	for _, c := range []byte(s[max(0, i-chunkHashBytes):i]) {
		x = x<<8 | uint64(c)
	}
	return (x*0x9e3779b97f4a7c15)>>60 == 0
}

// longRuns returns, for each of the expressions ws (at most 64), the long windows and the chunks of shorter windows
// of s, in order: one pass over s for all of them. An expression read whole gets none.
func longRuns(wm *meter.Meter, s string, ws []*windowed) [][]longRun {
	out := make([][]longRun, len(ws))
	var alphaASCII, startASCII [utf8.RuneSelf]uint64
	var all uint64
	for k, w := range ws {
		if w.whole || k >= 64 {
			continue
		}
		all |= 1 << k
		for c := range alphaASCII {
			if w.alpha.ascii[c] {
				alphaASCII[c] |= 1 << k
				if !w.later.ascii[c] {
					startASCII[c] |= 1 << k
				}
			}
		}
	}
	if all == 0 {
		return out
	}
	type masks struct{ alpha, start uint64 }
	other := map[rune]masks{}
	starts := make([]int, len(ws))
	prevs := make([]int, len(ws))
	cuts := make([]bool, len(ws))
	ch := newChunker(wm, s, out)
	var cur uint64
	prev := 0 // the start of the character before i
	for i := 0; i <= len(s); {
		tick(wm, 1)
		var m masks
		size := 0
		if i < len(s) {
			if c := s[i]; c < utf8.RuneSelf {
				m, size = masks{alphaASCII[c], startASCII[c]}, 1
			} else {
				var r rune
				r, size = utf8.DecodeRuneInString(s[i:])
				var ok bool
				if m, ok = other[r]; !ok {
					for k, w := range ws {
						if all&(1<<k) != 0 && w.alpha.in(wm, r) {
							m.alpha |= 1 << k
							if !w.later.in(wm, r) {
								m.start |= 1 << k
							}
						}
					}
					other[r] = m
				}
			}
		}
		if closing := (cur &^ m.alpha) | (cur & m.start); closing != 0 || m.alpha&^cur != 0 {
			tick(wm, 1)
			ch.cutHere = 0
			for ; closing != 0; closing &= closing - 1 {
				k := bits.TrailingZeros64(closing)
				ch.closeWindow(wm, k, longRun{prev: prevs[k], start: starts[k], end: i, next: i + size, last: prev, cut: cuts[k]})
			}
			for opening := m.alpha &^ (cur &^ m.start); opening != 0; opening &= opening - 1 {
				k := bits.TrailingZeros64(opening)
				starts[k], prevs[k], cuts[k] = i, prev, cur&(1<<k) != 0
				ch.openWindow(wm, k, i)
			}
			cur = m.alpha
		}
		if i == len(s) {
			break
		}
		prev = i
		i += size
	}
	ch.finish(wm)
	return out
}

// chunker gathers the long windows and the chunks of short windows of a text for each expression of one pass of
// longRuns (a bit each).
type chunker struct {
	s   string
	out [][]longRun
	// chunks are the chunks being gathered (start, prev and cut of their first window; end, next and last of their
	// last), open where chunked has the expression's bit.
	chunks  []longRun
	chunked uint64
	// first is the start of the first window of each expression after the last long run it got, -1 when none
	// stands there yet.
	first []int
	// cutHere is whether a chunk may end where windows end now (chunkCut): 0 not read yet, 1 yes, 2 no.
	cutHere int
}

// newChunker returns a chunker of s that appends to out.
func newChunker(wm *meter.Meter, s string, out [][]longRun) *chunker {
	c := &chunker{s: s, out: out, chunks: make([]longRun, len(out)), first: make([]int, len(out))}
	for k := range c.first {
		tick(wm, 1)
		c.first[k] = -1
	}
	return c
}

// emit gives expression k the long run r.
func (c *chunker) emit(_ *meter.Meter, k int, r longRun) {
	r.follows = c.first[k] == r.start
	c.first[k] = -1
	c.out[k] = append(c.out[k], r)
}

// closeWindow takes the window win of expression k, which ends here: a long window on its own (the chunk before it
// ends at the window before), a short one into the chunk being gathered, which ends here when the text decides so.
func (c *chunker) closeWindow(wm *meter.Meter, k int, win longRun) {
	bit := uint64(1) << k
	if win.end-win.start >= windowMinRun {
		if c.chunked&bit != 0 && c.chunks[k].end-c.chunks[k].start >= windowMinRun {
			c.emit(wm, k, c.chunks[k])
		}
		c.chunked &^= bit
		c.emit(wm, k, win)
		return
	}
	if c.chunked&bit == 0 {
		c.chunks[k], c.chunked = win, c.chunked|bit
	} else {
		c.chunks[k].end, c.chunks[k].next, c.chunks[k].last = win.end, win.next, win.last
	}
	if win.end-c.chunks[k].start < windowMinRun {
		return
	}
	if c.cutHere == 0 {
		tick(wm, chunkHashBytes)
		c.cutHere = 2
		if chunkCut(wm, c.s, win.end) {
			c.cutHere = 1
		}
	}
	if c.cutHere == 1 || win.end-c.chunks[k].start >= chunkMaxRun {
		c.emit(wm, k, c.chunks[k])
		c.chunked &^= bit
	}
}

// openWindow notes a window of expression k that begins at i: the chunk being gathered ends before it when
// windowMinRun bytes or more without a window stand between them.
func (c *chunker) openWindow(wm *meter.Meter, k, i int) {
	if c.chunked&(1<<k) != 0 && i-c.chunks[k].end >= windowMinRun {
		c.emit(wm, k, c.chunks[k])
		c.chunked &^= 1 << k
	}
	if c.first[k] < 0 {
		c.first[k] = i
	}
}

// finish gives each expression the chunk it was gathering at the end of the text, when it is long enough or
// windowMinRun bytes without a window follow it, and marks its last long run final when no window follows that.
func (c *chunker) finish(wm *meter.Meter) {
	for ; c.chunked != 0; c.chunked &= c.chunked - 1 {
		k := bits.TrailingZeros64(c.chunked)
		if c.chunks[k].end-c.chunks[k].start >= windowMinRun || len(c.s)-c.chunks[k].end >= windowMinRun {
			c.emit(wm, k, c.chunks[k])
		}
	}
	for k := range c.out {
		tick(wm, 1)
		if n := len(c.out[k]); n > 0 && c.first[k] < 0 {
			c.out[k][n-1].final = true
		}
	}
}

// windowKey is what a long window was read as: the character before it, as read, and the window with the
// character after it.
type windowKey struct{ lead, text string }

// windowIDs names the distinct long windows of one text, for all the expressions read on it: windows at the same
// place are looked up once, and windows of the same text (a label repeated) get the same number, so each
// expression reads one of them and takes its matches for the others.
type windowIDs struct {
	at     map[windowAt]int
	byText map[windowKey]int
	// found holds the matches of each expression in each long window it read, relative to the window's start.
	found map[windowFound]windowMatches
}

// windowMatches are the matches of an expression in a long window, each of stride indexes, one after the other in
// 32 bits each (#1087: a slice for each match held about four times as much, for every match of a text dense in
// them).
type windowMatches struct {
	stride int
	at     []int32
}

// add appends m, shifted by by (shifted).
func (wms *windowMatches) add(_ *meter.Meter, m []int, by int) {
	wms.stride = len(m)
	for _, v := range m {
		if v >= 0 {
			v += by
		}
		wms.at = append(wms.at, int32(v)) // #nosec G115 -- a window is part of an entry of at most MaxEntry bytes
	}
}

// len returns the number of matches.
func (wms windowMatches) len() int {
	if wms.stride == 0 {
		return 0
	}
	return len(wms.at) / wms.stride
}

// match returns match k, shifted by by, in a slice of its own.
func (wms windowMatches) match(_ *meter.Meter, k, by int) []int {
	out := make([]int, wms.stride)
	for j, v := range wms.at[k*wms.stride : (k+1)*wms.stride] {
		out[j] = int(v)
		if v >= 0 {
			out[j] += by
		}
	}
	return out
}

// windowAt is a long window of the text read now, with the character read before it.
type windowAt struct {
	r    longRun
	lead string
}

// windowFound is an expression and the number of a long window.
type windowFound struct {
	w  *windowed
	id int
}

// newText starts the numbering of the windows of another text: the windows already read are kept.
func (ids *windowIDs) newText(_ *meter.Meter) { clear(ids.at) }

// of returns the number of the long window r of s, read with lead before it.
func (ids *windowIDs) of(wm *meter.Meter, s string, r longRun, lead string) int {
	at := windowAt{r, lead}
	if id, ok := ids.at[at]; ok {
		return id
	}
	if ids.at == nil {
		ids.at, ids.byText = map[windowAt]int{}, map[windowKey]int{}
	}
	key := windowKey{lead: lead, text: s[r.start:r.next]}
	tick(wm, len(key.lead)+len(key.text)) // the lookup hashes the text
	id, ok := ids.byText[key]
	if !ok {
		id = len(ids.byText)
		ids.byText[key] = id
	}
	ids.at[at] = id
	return id
}

// each calls f with each match of w.re.FindAllStringSubmatchIndex(s, -1), in order, as it is found, until f returns false.
func (w *windowed) each(wm *meter.Meter, s string, f func(m []int) bool) {
	if w.whole || len(s) < windowMinText {
		w.eachWhole(wm, s, f)
		return
	}
	w.eachIn(wm, s, longRuns(wm, s, []*windowed{w})[0], &windowIDs{}, f)
}

// findAllWith is w.re.FindAllStringSubmatchIndex(s, -1) on one of several texts read with ids, which keeps the
// windows read in the texts before: the lines of a text, read one by one.
func (w *windowed) findAllWith(wm *meter.Meter, s string, ids *windowIDs) [][]int {
	if w.whole || len(s) < windowMinText {
		return w.readWhole(wm, s)
	}
	ids.newText(wm)
	return w.findAllIn(wm, s, longRuns(wm, s, []*windowed{w})[0], ids)
}

// leadClass is the character the assertions read the same as the character r before a window that begins with a
// start character: a line break, a word character or neither.
func leadClass(_ *meter.Meter, r rune) string {
	switch {
	case r == '\n':
		return "\n"
	case r < utf8.RuneSelf && syntax.IsWordChar(r):
		return "a"
	default:
		return " "
	}
}

// findAllIn is w.re.FindAllStringSubmatchIndex(s, -1), where runs are the long windows of s for w (longRuns) and
// ids names the long windows of s (shared by the expressions read on s).
func (w *windowed) findAllIn(wm *meter.Meter, s string, runs []longRun, ids *windowIDs) [][]int {
	var out [][]int
	w.eachIn(wm, s, runs, ids, func(m []int) bool { out = append(out, m); return true })
	return out
}

// eachIn calls f with each match findAllIn returns, in order, as it is found, until f returns false: the matches
// of the whole text are not held at once, only those of each long window, which the expressions share (ids).
func (w *windowed) eachIn(wm *meter.Meter, s string, runs []longRun, ids *windowIDs, f func(m []int) bool) {
	if len(runs) == 0 {
		w.eachWhole(wm, s, f)
		return
	}
	stopped := false
	// keep calls f with the matches ms of a text read from byte at of s that start in [from, to) of s.
	keep := func(ms windowMatches, at, from, to int) {
		for k := range ms.len() {
			tick(wm, 1)
			if start := int(ms.at[k*ms.stride]) + at; start >= from && start < to && !f(ms.match(wm, k, at)) {
				stopped = true
				return
			}
		}
	}
	// part reads the bytes [from, to) of s with the character before from (starting at lo) and the one at to.
	part := func(lo, from, to int) {
		hi := to
		if to < len(s) {
			_, size := utf8.DecodeRuneInString(s[to:])
			hi += size
		}
		if w.onRead != nil {
			w.onRead(hi - lo)
		}
		eachCharged(wm, w.expr, s[lo:hi], func(m []int) bool {
			tick(wm, 1)
			if m[0]+lo >= from && m[0]+lo < to && !f(shifted(wm, m, lo)) {
				stopped = true
			}
			return !stopped
		})
	}
	if ids.found == nil {
		ids.found = map[windowFound]windowMatches{}
	}
	from, lo := 0, 0
	for _, r := range runs {
		tick(wm, 1)
		if r.start > from && !r.follows {
			if part(lo, from, r.start); stopped {
				return
			}
		}
		lead := s[r.prev:r.start]
		if r.cut {
			c, _ := utf8.DecodeRuneInString(lead)
			lead = leadClass(wm, c)
		}
		if c, _ := utf8.DecodeRuneInString(lead); lead != "" && !w.begins && (c >= utf8.RuneSelf || !syntax.IsWordChar(c)) {
			// Without ^ or \A, a character before the window that is no word character reads as the start of a
			// text: \b and \B see no word character in both. It is outside the alphabet or the window begins with
			// a start character, so no match is lost with it. The window is then read as it is read at the start
			// of a value alone.
			lead = ""
		}
		id := ids.of(wm, s, r, lead)
		found, ok := ids.found[windowFound{w, id}]
		if !ok {
			// The matches are kept up to the end of the text read, not to r.end: the same bytes can end another
			// long run at another place (a chunk before a start character that opens a long window, and the same
			// chunk at the end of a text, which takes that character), and keep applies each run's end.
			n := len(lead)
			t := lead + s[r.start:r.next]
			if w.onRead != nil {
				w.onRead(len(t))
			}
			eachCharged(wm, w.expr, t, func(m []int) bool {
				tick(wm, 1)
				if m[0] >= n {
					found.add(wm, m, -n)
				}
				return true
			})
			ids.found[windowFound{w, id}] = found
		}
		if keep(found, r.start, r.start, r.end); stopped {
			return
		}
		from, lo = r.end, r.last
	}
	if from < len(s) && !runs[len(runs)-1].final {
		part(lo, from, len(s))
	}
}

// eachWhole is readWhole with each match given to f as it is found, until f returns false.
func (w *windowed) eachWhole(wm *meter.Meter, s string, f func(m []int) bool) {
	if w.onRead != nil {
		w.onRead(len(s))
	}
	eachCharged(wm, w.expr, s, f)
}

// readWhole is w.re.FindAllStringSubmatchIndex(s, -1), told to onRead like the parts of a windowed reading.
func (w *windowed) readWhole(wm *meter.Meter, s string) [][]int {
	if w.onRead != nil {
		w.onRead(len(s))
	}
	return findAllCharged(wm, w.expr, s)
}

// findAllCharged returns e.Regexp().FindAllStringSubmatchIndex(s, -1), with each byte the engine reads charged to
// wm as the work of an expression, attempt by attempt (meter.Expr): an expression that reads the rest of the text
// on every match attempt (a.*z|a on a run of a) costs the square of the text, and the budget stops it there
// instead of after the call (#873, decision of 2026-11-20).
func findAllCharged(wm *meter.Meter, e *meter.Expr, s string) [][]int {
	return e.FindAll(s, func(n int) { tickExpression(wm, n) })
}

// eachCharged is findAllCharged with each match given to f as it is found, until f returns false (meter.Expr.Each).
func eachCharged(wm *meter.Meter, e *meter.Expr, s string, f func(m []int) bool) {
	e.Each(s, func(n int) { tickExpression(wm, n) }, f)
}

// shifted returns the match m, found in a text that starts at byte by of another, as a match of the other.
func shifted(_ *meter.Meter, m []int, by int) []int {
	out := make([]int, len(m))
	for k, v := range m {
		out[k] = v
		if v >= 0 {
			out[k] = v + by
		}
	}
	return out
}
