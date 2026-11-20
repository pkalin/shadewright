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
func newRuneSet(pairs []rune) runeSet {
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
func (set *runeSet) in(r rune) bool {
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
}

// newWindowed returns re with its alphabet and start characters.
func newWindowed(re *regexp.Regexp) *windowed {
	w := &windowed{re: re, whole: true}
	tree, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		return w
	}
	tree = tree.Simplify()
	var all, later []rune
	if minLen(tree) == 0 || !alphabet(tree, &all) || !laterAlphabet(tree, &later) {
		return w
	}
	w.whole = false
	w.alpha, w.later, w.begins = newRuneSet(all), newRuneSet(later), asserts(tree, syntax.OpBeginText, syntax.OpBeginLine)
	return w
}

// asserts reports whether re holds one of the assertions ops.
func asserts(re *syntax.Regexp, ops ...syntax.Op) bool {
	if slices.Contains(ops, re.Op) {
		return true
	}
	for _, sub := range re.Sub {
		if asserts(sub, ops...) {
			return true
		}
	}
	return false
}

// minLen returns the fewest characters a match of re consumes.
func minLen(re *syntax.Regexp) int {
	switch re.Op {
	case syntax.OpLiteral:
		return len(re.Rune)
	case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return 1
	case syntax.OpCapture, syntax.OpPlus:
		return minLen(re.Sub[0])
	case syntax.OpRepeat:
		return re.Min * minLen(re.Sub[0])
	case syntax.OpConcat:
		n := 0
		for _, sub := range re.Sub {
			n += minLen(sub)
		}
		return n
	case syntax.OpAlternate:
		n := -1
		for _, sub := range re.Sub {
			if m := minLen(sub); n < 0 || m < n {
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
func literalRunes(rs []rune, fold bool, pairs *[]rune) {
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
func alphabet(re *syntax.Regexp, pairs *[]rune) bool {
	switch re.Op {
	case syntax.OpLiteral:
		literalRunes(re.Rune, re.Flags&syntax.FoldCase != 0, pairs)
		return true
	case syntax.OpCharClass:
		*pairs = append(*pairs, re.Rune...)
		return true
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return false
	default:
		for _, sub := range re.Sub {
			if !alphabet(sub, pairs) {
				return false
			}
		}
		return true
	}
}

// laterAlphabet appends to pairs the ranges of the characters a match of re can hold at another place than its
// first (more of them than that, never fewer), and reports false when that is every character.
func laterAlphabet(re *syntax.Regexp, pairs *[]rune) bool {
	switch re.Op {
	case syntax.OpLiteral:
		if len(re.Rune) > 1 {
			literalRunes(re.Rune[1:], re.Flags&syntax.FoldCase != 0, pairs)
		}
		return true
	case syntax.OpStar, syntax.OpPlus:
		// A repeated part can follow itself: all of its characters may stand later.
		return alphabet(re.Sub[0], pairs)
	case syntax.OpRepeat:
		if re.Max == 1 {
			return laterAlphabet(re.Sub[0], pairs)
		}
		return alphabet(re.Sub[0], pairs)
	case syntax.OpConcat:
		consumed := false
		for _, sub := range re.Sub {
			if !laterAlphabet(sub, pairs) {
				return false
			}
			if consumed && !alphabet(sub, pairs) {
				return false
			}
			var own []rune
			if alphabet(sub, &own); len(own) > 0 {
				consumed = true
			}
		}
		return true
	default: // a class or any character (one character, the first of itself), a capture, an alternation, a quest
		for _, sub := range re.Sub {
			if !laterAlphabet(sub, pairs) {
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
func chunkCut(s string, i int) bool {
	var x uint64
	for _, c := range []byte(s[max(0, i-chunkHashBytes):i]) {
		x = x<<8 | uint64(c)
	}
	return (x*0x9e3779b97f4a7c15)>>60 == 0
}

// longRuns returns, for each of the expressions ws (at most 64), the long windows and the chunks of shorter windows
// of s, in order: one pass over s for all of them. An expression read whole gets none.
func longRuns(s string, ws []*windowed) [][]longRun {
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
	// chunks are the chunks being gathered (start, prev and cut of their first window; end, next and last of their
	// last), open where chunked has the expression's bit; first is the start of the first window of each
	// expression after the last long run it got, -1 when none stands there yet.
	chunks := make([]longRun, len(ws))
	var chunked uint64
	first := make([]int, len(ws))
	for k := range first {
		first[k] = -1
	}
	emit := func(k int, r longRun) {
		r.follows = first[k] == r.start
		first[k] = -1
		out[k] = append(out[k], r)
	}
	var cur uint64
	prev := 0 // the start of the character before i
	for i := 0; i <= len(s); {
		tick(1)
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
						if all&(1<<k) != 0 && w.alpha.in(r) {
							m.alpha |= 1 << k
							if !w.later.in(r) {
								m.start |= 1 << k
							}
						}
					}
					other[r] = m
				}
			}
		}
		if closing := (cur &^ m.alpha) | (cur & m.start); closing != 0 || m.alpha&^cur != 0 {
			tick(1)
			cutHere := 0 // whether a chunk may end at i (chunkCut): 0 not read yet, 1 yes, 2 no
			for ; closing != 0; closing &= closing - 1 {
				k := bits.TrailingZeros64(closing)
				win := longRun{prev: prevs[k], start: starts[k], end: i, next: i + size, last: prev, cut: cuts[k]}
				if i-starts[k] >= windowMinRun {
					// A long window is read on its own, as before chunks: the chunk before it ends at the window before.
					if chunked&(1<<k) != 0 && chunks[k].end-chunks[k].start >= windowMinRun {
						emit(k, chunks[k])
					}
					chunked &^= 1 << k
					emit(k, win)
					continue
				}
				if chunked&(1<<k) == 0 {
					chunks[k], chunked = win, chunked|1<<k
				} else {
					chunks[k].end, chunks[k].next, chunks[k].last = win.end, win.next, win.last
				}
				if i-chunks[k].start < windowMinRun {
					continue
				}
				if cutHere == 0 {
					tick(chunkHashBytes)
					cutHere = 2
					if chunkCut(s, i) {
						cutHere = 1
					}
				}
				if cutHere == 1 || i-chunks[k].start >= chunkMaxRun {
					emit(k, chunks[k])
					chunked &^= 1 << k
				}
			}
			for opening := m.alpha &^ (cur &^ m.start); opening != 0; opening &= opening - 1 {
				k := bits.TrailingZeros64(opening)
				starts[k], prevs[k], cuts[k] = i, prev, cur&(1<<k) != 0
				if chunked&(1<<k) != 0 && i-chunks[k].end >= windowMinRun {
					emit(k, chunks[k])
					chunked &^= 1 << k
				}
				if first[k] < 0 {
					first[k] = i
				}
			}
			cur = m.alpha
		}
		if i == len(s) {
			break
		}
		prev = i
		i += size
	}
	for ; chunked != 0; chunked &= chunked - 1 {
		if k := bits.TrailingZeros64(chunked); chunks[k].end-chunks[k].start >= windowMinRun || len(s)-chunks[k].end >= windowMinRun {
			emit(k, chunks[k])
		}
	}
	for k := range out {
		if n := len(out[k]); n > 0 && first[k] < 0 {
			out[k][n-1].final = true
		}
	}
	return out
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
	found map[windowFound][][]int
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
func (ids *windowIDs) newText() { clear(ids.at) }

// of returns the number of the long window r of s, read with lead before it.
func (ids *windowIDs) of(s string, r longRun, lead string) int {
	at := windowAt{r, lead}
	if id, ok := ids.at[at]; ok {
		return id
	}
	if ids.at == nil {
		ids.at, ids.byText = map[windowAt]int{}, map[windowKey]int{}
	}
	key := windowKey{lead: lead, text: s[r.start:r.next]}
	tick(len(key.lead) + len(key.text)) // the lookup hashes the text
	id, ok := ids.byText[key]
	if !ok {
		id = len(ids.byText)
		ids.byText[key] = id
	}
	ids.at[at] = id
	return id
}

// findAll is w.re.FindAllStringSubmatchIndex(s, -1), with each distinct long window of s read once.
func (w *windowed) findAll(s string) [][]int {
	if w.whole || len(s) < windowMinText {
		return w.readWhole(s)
	}
	return w.findAllIn(s, longRuns(s, []*windowed{w})[0], &windowIDs{})
}

// findAllWith is findAll on one of several texts read with ids, which keeps the windows read in the texts before:
// the lines of a text, read one by one.
func (w *windowed) findAllWith(s string, ids *windowIDs) [][]int {
	if w.whole || len(s) < windowMinText {
		return w.readWhole(s)
	}
	ids.newText()
	return w.findAllIn(s, longRuns(s, []*windowed{w})[0], ids)
}

// leadClass is the character the assertions read the same as the character r before a window that begins with a
// start character: a line break, a word character or neither.
func leadClass(r rune) string {
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
func (w *windowed) findAllIn(s string, runs []longRun, ids *windowIDs) [][]int {
	if len(runs) == 0 {
		return w.readWhole(s)
	}
	var out [][]int
	// keep appends the matches ms of a text read from byte at of s that start in [from, to) of s.
	keep := func(ms [][]int, at, from, to int) {
		for _, m := range ms {
			tick(1)
			if m[0]+at >= from && m[0]+at < to {
				out = append(out, shifted(m, at))
			}
		}
	}
	read := func(t string) [][]int {
		if w.onRead != nil {
			w.onRead(len(t))
		}
		return w.re.FindAllStringSubmatchIndex(counted(t), -1)
	}
	// part reads the bytes [from, to) of s with the character before from (starting at lo) and the one at to.
	part := func(lo, from, to int) {
		hi := to
		if to < len(s) {
			_, size := utf8.DecodeRuneInString(s[to:])
			hi += size
		}
		keep(read(s[lo:hi]), lo, from, to)
	}
	if ids.found == nil {
		ids.found = map[windowFound][][]int{}
	}
	from, lo := 0, 0
	for _, r := range runs {
		tick(1)
		if r.start > from && !r.follows {
			part(lo, from, r.start)
		}
		lead := s[r.prev:r.start]
		if r.cut {
			c, _ := utf8.DecodeRuneInString(lead)
			lead = leadClass(c)
		}
		if c, _ := utf8.DecodeRuneInString(lead); lead != "" && !w.begins && (c >= utf8.RuneSelf || !syntax.IsWordChar(c)) {
			// Without ^ or \A, a character before the window that is no word character reads as the start of a
			// text: \b and \B see no word character in both. It is outside the alphabet or the window begins with
			// a start character, so no match is lost with it. The window is then read as it is read at the start
			// of a value alone.
			lead = ""
		}
		id := ids.of(s, r, lead)
		found, ok := ids.found[windowFound{w, id}]
		if !ok {
			n := len(lead)
			for _, m := range read(lead + s[r.start:r.next]) {
				tick(1)
				if m[0] >= n && m[0] < n+r.end-r.start {
					found = append(found, shifted(m, -n))
				}
			}
			ids.found[windowFound{w, id}] = found
		}
		keep(found, r.start, r.start, r.end)
		from, lo = r.end, r.last
	}
	if from < len(s) && !runs[len(runs)-1].final {
		part(lo, from, len(s))
	}
	return out
}

// readWhole is w.re.FindAllStringSubmatchIndex(s, -1), told to onRead like the parts of a windowed reading.
func (w *windowed) readWhole(s string) [][]int {
	if w.onRead != nil {
		w.onRead(len(s))
	}
	return w.re.FindAllStringSubmatchIndex(counted(s), -1)
}

// shifted returns the match m, found in a text that starts at byte by of another, as a match of the other.
func shifted(m []int, by int) []int {
	out := make([]int, len(m))
	for k, v := range m {
		out[k] = v
		if v >= 0 {
			out[k] = v + by
		}
	}
	return out
}
