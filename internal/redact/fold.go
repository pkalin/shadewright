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
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pkalin/shadewright/internal/redact/meter"
)

// The folded view of a text (#845, second to fifth rounds): the text a reader sees, where the rules read the
// text as written. Four things are folded, and nothing else:
//
//   - A run of invisible characters (invisibleRanges: Unicode's default ignorable code points and category Cf,
//     such as the zero-width space and joiners, the soft hyphen, the combining grapheme joiner, the Hangul
//     fillers and the variation selectors) after a byte of a key name is dropped when a key byte, a colon, an
//     equals sign, a quote, a colon look-alike or the end of the line follows it ("pass​word",
//     "api_key​: V"): a reader does not see it, and it split the key from itself or from its colon for
//     every rule.
//   - Each character of foldColons becomes ":", each of foldEquals "=", and each of foldArrows "->" or "=>".
//   - An explicit YAML key, "? key" with ":" at the same column on the next line, becomes "key:" on one line;
//     so does a key on its own line below "?" or below "? |" (a block scalar: one line of key, more indented
//     than the "?", blank lines around it), with ":" at the column of the "?" after it.
//   - The separator after a secret key, read on the view the three folds above give (sepRunAt): a run of
//     blanks of any length and at most maxSepRun controls, invisible characters, colons, equals signs and the
//     arrows "->" and "=>", with at least one colon, equals sign or arrow in it, becomes ": ", which every rule
//     for a key and its value reads. A run the rules have always read (one ":", "=" or ":=" between spaces and
//     tabs) stays as it is, unless an "=" follows a quoted key, and a run of blanks alone is no separator
//     ("password reset failed" is prose). A secret key and separator right after the separator of another
//     secret key starts a line of its own ("auth: password: V"), so the first key does not take it as its
//     value.
//
// The rules read the folded view as well as the text ([Redactor.spans]); each of its matches counts on the
// bytes of the text it was folded from, so a redacted range still starts and ends on the original bytes and
// covers the characters folded away inside it. A text with nothing to fold is read once. seqValues reads each
// string of a sequence in its folded view too.

// foldColons are the characters that Unicode's confusables list maps to a colon, or to two (U+2237 PROPORTION
// and U+2E2C SQUARED FOUR DOT PUNCTUATION read "::"), each folded to one ":": every line of confusables.txt
// version 18.0.0 (UTS #39, https://www.unicode.org/Public/18.0.0/security/confusables.txt) whose target is
// one or more U+003A COLON, in the file's order. testdata/confusables-colon.txt holds those lines, and
// TestFoldColonsAreTheConfusablesOfColon checks this table and the version named here against it.
var foldColons = []rune{
	0x0903, 0x0A83, 0x11002, 0x11082, 0x11182, 0x115BE, 0x116AC, 0x11838, 0xFF1A, 0xFE55, 0xFE13, 0x0589, 0x0703,
	0x0704, 0x0831, 0x1361, 0x16EC, 0xFE30, 0x1803, 0x1809, 0x205A, 0x05C3, 0x10AF5, 0x1123A, 0x02F8, 0x1393,
	0xA789, 0x2236, 0x1D108, 0x02D0, 0x1015B, 0x11DD9, 0xA4FD, 0x2E2C, 0x2237,
}

// foldEquals are the characters that Unicode's confusables list maps to an equals sign, or to two or three
// (U+2A75 and U+2A76), each folded to one "=": every line of confusables.txt version 18.0.0 whose target is
// one or more U+003D EQUALS SIGN, in the file's order. foldArrows are U+2192 RIGHTWARDS ARROW and U+21D2
// RIGHTWARDS DOUBLE ARROW, folded to "->" and "=>", and the characters the same list maps to either of them
// (U+FFEB HALFWIDTH RIGHTWARDS ARROW). testdata/confusables-equals-arrows.txt holds those lines, and
// TestFoldMarksAreTheConfusablesOfEqualsAndArrows checks both tables against it (#845, decision of 2026-11-18).
var (
	foldEquals = []rune{0x1400, 0x2E40, 0x30A0, 0xA4FF, 0xA78A, 0xFF1D, 0xFE66, 0x10190, 0x11053, 0xA60C, 0x2A75, 0x2A76}
	foldArrows = map[rune]string{0x2192: "->", 0x21D2: "=>", 0xFFEB: "->"}
)

// foldMark returns what the character r folds to when it is one of foldEquals or foldArrows.
func foldMark(_ *meter.Meter, r rune) (string, bool) {
	if r < 0x1400 {
		return "", false
	}
	if a, ok := foldArrows[r]; ok {
		return a, true
	}
	if slices.Contains(foldEquals, r) {
		return "=", true
	}
	return "", false
}

// foldMarkAt returns what the character at s[i] folds to when it is one of foldEquals or foldArrows.
func foldMarkAt(wm *meter.Meter, s string, i int) (string, bool) {
	if s[i] < utf8.RuneSelf {
		return "", false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return foldMark(wm, r)
}

// foldedView is the folded view of a text: its text, and for each of its bytes the first byte of the original
// it stands for (from) and the end of the last (to). What each byte stands for is kept as segments (foldSeg), not
// as two offsets a byte: a text copied as it is is one segment, and so is a folded piece. Two offsets a byte held
// sixteen bytes for each byte of the text, and both folds held them at once (#1087).
type foldedView struct {
	text string
	segs []foldSeg
	// cur is the segment the last lookup found, so that lookups along the text take constant time.
	cur *int
}

// foldSeg is a piece of a folded view from its byte at on, up to the next segment's at or the end of the text:
// copied (to is negative), byte at+k stands for the byte from+k of the original; folded, every byte stands for
// [from, to). Twelve bytes a segment: a text with a fold every few bytes holds a few segments for each.
type foldSeg struct {
	at, from, to int32
}

// copied reports whether s is a copy of the original.
func (s foldSeg) copied() bool { return s.to < 0 }

// seg returns the segment that holds byte i of the view: the one found last or the next, otherwise by a binary
// search. It counts no step: a lookup stands for the read of an offset the view held for each byte, which counted
// none either, and its search halves the segments at most 32 times.
func (f foldedView) seg(_ *meter.Meter, i int) foldSeg {
	k := *f.cur
	if k < len(f.segs) && int(f.segs[k].at) <= i && (k+1 == len(f.segs) || int(f.segs[k+1].at) > i) {
		return f.segs[k]
	}
	if k+1 < len(f.segs) && int(f.segs[k+1].at) <= i && (k+2 >= len(f.segs) || int(f.segs[k+2].at) > i) {
		*f.cur = k + 1
		return f.segs[k+1]
	}
	lo, hi := 0, len(f.segs) // the last segment at or before i is in [lo, hi)
	for hi-lo > 1 {
		if m := int(uint(lo+hi) >> 1); int(f.segs[m].at) <= i {
			lo = m
		} else {
			hi = m
		}
	}
	*f.cur = lo
	return f.segs[lo]
}

// from returns the first byte of the original that byte i of the view stands for.
func (f foldedView) from(wm *meter.Meter, i int) int {
	s := f.seg(wm, i)
	if s.copied() {
		return int(s.from) + i - int(s.at)
	}
	return int(s.from)
}

// to returns the end of the last byte of the original that byte i of the view stands for.
func (f foldedView) to(wm *meter.Meter, i int) int {
	s := f.seg(wm, i)
	if s.copied() {
		return int(s.from) + i - int(s.at) + 1
	}
	return int(s.to)
}

// isKeyByte reports whether c is a byte of a key name.
func isKeyByte(_ *meter.Meter, c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-'
}

// foldView returns the folded view of s, and false when it is s itself: the glyph folds (foldWalk), then the
// separator fold (sepWalk) on their result. A first pass of each only looks for something to fold, so a text
// with nothing to fold costs one read of each and no allocation.
func foldView(wm *meter.Meter, s string) (foldedView, bool) {
	var g foldedView
	glyphs := foldWalk(wm, s, nil)
	if glyphs {
		g = newFoldBuilder(wm, len(s)).build(wm, func(emit func(text string, from, to int, copied bool)) {
			foldWalk(wm, s, func(text string, from, to int) { emit(text, from, to, to-from == len(text)) })
		})
	}
	t := s
	if glyphs {
		t = g.text
	}
	if !sepWalk(wm, t, nil) {
		return g, glyphs
	}
	from := func(i int) int { return i }
	to := func(i int) int { return i + 1 }
	if glyphs {
		from = func(i int) int { return g.from(wm, i) }
		to = func(i int) int { return g.to(wm, i) }
	}
	return newFoldBuilder(wm, len(t)).build(wm, func(emit func(text string, from, to int, copied bool)) {
		sepWalk(wm, t, func(text string, a, b int, copied bool) {
			if copied {
				// Each byte stands for what its byte of the glyph fold stood for: a look-alike colon folded to
				// ":" stands for all of its bytes, so a range that ends on it ends on a character boundary
				// (#845, third decision of 2026-11-18).
				for i := a; i < b; i++ {
					tick(wm, 1)
					emit(t[i:i+1], from(i), to(i), false)
				}
				return
			}
			emit(text, from(a), to(b-1), false)
		})
	}), true
}

// foldBuilder writes a folded view piece by piece.
type foldBuilder struct {
	b strings.Builder
	f foldedView
}

// newFoldBuilder returns a builder for a view of about n bytes.
func newFoldBuilder(_ *meter.Meter, n int) *foldBuilder {
	fb := &foldBuilder{f: foldedView{cur: new(int)}}
	fb.b.Grow(n)
	return fb
}

// build calls walk with an emit function and returns the view of what it emitted: a piece copied from the
// original (copied) maps each byte to its own, a folded piece maps each byte to all of from to to. A piece that
// goes on with the segment before it (a copy after a copy of the bytes before, or one byte that stands for itself)
// adds no segment.
func (fb *foldBuilder) build(wm *meter.Meter, walk func(emit func(text string, from, to int, copied bool))) foldedView {
	walk(func(text string, from, to int, copied bool) {
		tick(wm, 1+len(text))
		if text == "" {
			return
		}
		at := fb.b.Len()
		fb.b.WriteString(text)
		if !copied && len(text) == 1 && to == from+1 {
			copied = true // one byte that stands for one byte is a copy of it
		}
		if n := len(fb.f.segs); n > 0 {
			l := fb.f.segs[n-1]
			if copied && l.copied() && int(l.from)+at-int(l.at) == from ||
				!copied && !l.copied() && int(l.from) == from && int(l.to) == to {
				return
			}
		}
		if copied {
			to = -1
		}
		fb.f.segs = append(fb.f.segs, foldSeg{at: int32(at), from: int32(from), to: int32(to)}) // #nosec G115 -- an entry is at most MaxEntry bytes
	})
	fb.f.text = fb.b.String()
	return fb.f
}

// foldWalk reads s line by line and calls emit for each piece of the folded view: its text and the range of s
// it stands for. With emit nil it only reports, as soon as it finds one, whether anything folds.
func foldWalk(wm *meter.Meter, s string, emit func(text string, from, to int)) bool {
	changed := false
	copyText := func(from, to int) {
		if emit != nil && to > from {
			emit(s[from:to], from, to)
		}
	}
	skipIndent := false
	for pos := 0; pos < len(s); {
		tick(wm, 1)
		l := readLine(wm, s, pos)
		pos = l.next
		at := l.text
		if !skipIndent {
			copyText(l.start, l.text)
		}
		skipIndent = false
		at += dashes(wm, s[l.text:l.end])
		copyText(l.text, at)
		if at < l.end && s[at] == '?' && (at+1 == l.end || indentLen(wm, s, at+1) > 0) && l.next < len(s) {
			q := colOf(wm, s, l.start, at)
			n := readLine(wm, s, l.next)
			if !n.blank(wm) && s[n.text] == ':' && n.ind == q {
				// An explicit key: "? key" and ":" below the "?" read as "key:".
				changed = true
				if emit == nil {
					return true
				}
				key := at + 1
				for key < l.end && indentLen(wm, s, key) > 0 {
					tick(wm, 1)
					key += indentLen(wm, s, key)
				}
				end := l.end
				for end > key && lastIndentLen(wm, s[:end], end) > 0 {
					tick(wm, 1)
					end -= lastIndentLen(wm, s[:end], end)
				}
				if foldChars(wm, s, key, end, emit) {
					changed = true
				}
				// The line break and the indentation before ":" fold away, onto the last byte of the key.
				skipIndent = true
				continue
			}
			if k, c, ok := explicitKeyBelow(wm, s, at, l, q); ok {
				// A key below "?" or "? |": the header, the line breaks, the blank lines and the indentation
				// before the key and before ":" fold away.
				changed = true
				if emit == nil {
					return true
				}
				end := k.end
				for end > k.text && lastIndentLen(wm, s[:end], end) > 0 {
					tick(wm, 1)
					end -= lastIndentLen(wm, s[:end], end)
				}
				foldChars(wm, s, k.text, end, emit)
				pos, skipIndent = c.start, true
				continue
			}
		}
		if foldChars(wm, s, at, l.end, emit) {
			changed = true
			if emit == nil {
				return true
			}
		}
		copyText(l.end, l.next)
	}
	return changed
}

// blockKeyHeader matches what may follow "? " when the key stands on the line below: nothing, or a block
// scalar header with its chomping and indentation indicators and a comment.
var blockKeyHeader = regexp.MustCompile(`^(?:[|>][1-9+-]{0,2}(?:` + keySpaceExpr + `+#.*)?)?$`)

// explicitKeyBelow reports whether the line l, whose "?" stands at s[at] in column q, opens an explicit key
// written on the line below (k): "?" alone or with a block scalar header, then blank lines, one line of key
// more indented than the "?", blank lines, and the line c that starts with ":" at column q. It reads only the
// lines up to c.
func explicitKeyBelow(wm *meter.Meter, s string, at int, l viewLine, q col) (k, c viewLine, ok bool) {
	h := at + 1
	for h < l.end && indentLen(wm, s, h) > 0 {
		tick(wm, 1)
		h += indentLen(wm, s, h)
	}
	if !blockKeyHeader.MatchString(counted(wm, s[h:l.end])) {
		return k, c, false
	}
	next := func(pos int) (viewLine, bool) {
		for pos < len(s) {
			tick(wm, 1)
			n := readLine(wm, s, pos)
			if !n.blank(wm) {
				return n, true
			}
			pos = n.next
		}
		return viewLine{}, false
	}
	k, found := next(l.next)
	if !found || !k.ind.deeper(wm, q) || s[k.text] == ':' || s[k.text] == '#' {
		return k, c, false
	}
	c, found = next(k.next)
	if !found || s[c.text] != ':' || c.ind != q {
		return k, c, false
	}
	return k, c, true
}

// foldChars emits s[from:to] with the invisible characters inside key names dropped and the colon look-alikes
// folded to ":", and reports whether it folded anything. With emit nil it stops at the first fold.
func foldChars(wm *meter.Meter, s string, from, to int, emit func(text string, from, to int)) bool {
	changed := false
	plain := from
	flush := func(i int) {
		if emit != nil && i > plain {
			emit(s[plain:i], plain, i)
		}
	}
	for i := from; i < to; {
		tick(wm, 1)
		if s[i] < utf8.RuneSelf {
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case isFoldColon(wm, r):
			changed = true
			if emit == nil {
				return true
			}
			flush(i)
			emit(":", i, i+n)
			plain = i + n
		case foldMarkOf(wm, r) != "":
			changed = true
			if emit == nil {
				return true
			}
			flush(i)
			emit(foldMarkOf(wm, r), i, i+n)
			plain = i + n
		case isInvisible(wm, r) && i > from && isKeyByte(wm, s[i-1]):
			// A run of invisible characters after a byte of a key name, before another, a separator, a quote or
			// the end of the line.
			j := i + n
			for j < to {
				tick(wm, 1)
				r2, n2 := utf8.DecodeRuneInString(s[j:])
				if !isInvisible(wm, r2) {
					break
				}
				j += n2
			}
			if j == to || isKeyByte(wm, s[j]) || strings.IndexByte(`:="'`, s[j]) >= 0 || isFoldColonAt(wm, s, j) || isFoldMarkAt(wm, s, j) {
				changed = true
				if emit == nil {
					return true
				}
				flush(i)
				plain = j
			}
			n = j - i
		}
		i += n
	}
	flush(to)
	return changed
}

// foldMarkOf returns what r folds to when it is one of foldEquals or foldArrows, or "".
func foldMarkOf(wm *meter.Meter, r rune) string {
	m, _ := foldMark(wm, r)
	return m
}

// isFoldMarkAt reports whether the character at s[i] is one of foldEquals or foldArrows.
func isFoldMarkAt(wm *meter.Meter, s string, i int) bool {
	_, ok := foldMarkAt(wm, s, i)
	return ok
}

// isFoldColon reports whether r is one of foldColons.
func isFoldColon(_ *meter.Meter, r rune) bool { return r >= 0x2D0 && slices.Contains(foldColons, r) }

// isFoldColonAt reports whether the character at s[i] is one of foldColons.
func isFoldColonAt(wm *meter.Meter, s string, i int) bool {
	if s[i] < utf8.RuneSelf {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return isFoldColon(wm, r)
}

// invisibleRanges are the invisible characters of the folded view, as sorted, merged ranges: every code point
// that Unicode 18.0.0 marks Default_Ignorable_Code_Point (DerivedCoreProperties.txt) or puts in category Cf
// (DerivedGeneralCategory.txt). testdata/invisible.txt holds those lines of both files, and
// TestInvisibleIsDefaultIgnorableOrFormat checks this table and the version named here against it.
var invisibleRanges = [][2]rune{
	{0x00AD, 0x00AD}, {0x034F, 0x034F}, {0x0600, 0x0605}, {0x061C, 0x061C}, {0x06DD, 0x06DD}, {0x070F, 0x070F},
	{0x0890, 0x0891}, {0x08E2, 0x08E2}, {0x115F, 0x1160}, {0x17B4, 0x17B5}, {0x180B, 0x180F}, {0x200B, 0x200F},
	{0x202A, 0x202E}, {0x2060, 0x206F}, {0x3164, 0x3164}, {0xFE00, 0xFE0F}, {0xFEFF, 0xFEFF}, {0xFFA0, 0xFFA0},
	{0xFFF0, 0xFFFB}, {0x110BD, 0x110BD}, {0x110CD, 0x110CD}, {0x13430, 0x1343F}, {0x1BCA0, 0x1BCA3},
	{0x1D173, 0x1D17A}, {0xE0000, 0xE0FFF},
}

// isInvisible reports whether r is in invisibleRanges.
func isInvisible(_ *meter.Meter, r rune) bool {
	if r < invisibleRanges[0][0] {
		return false
	}
	_, found := slices.BinarySearchFunc(invisibleRanges, r, func(rg [2]rune, r rune) int {
		switch {
		case rg[1] < r:
			return -1
		case rg[0] > r:
			return 1
		}
		return 0
	})
	return found
}

// invisibleLen returns the length of the invisible character at s[i], or 0.
func invisibleLen(wm *meter.Meter, s string, i int) int {
	if s[i] < utf8.RuneSelf {
		return 0
	}
	r, n := utf8.DecodeRuneInString(s[i:])
	if isInvisible(wm, r) {
		return n
	}
	return 0
}

// sepKey matches a secret key and the closing quote after it, plain or escaped, if there is one: where the
// separator fold looks for a separator.
var sepKey = regexp.MustCompile(`(?i)` + bareKeyExpr + `(?:\\*["'])?`)

// sepKeyWindows is sepKey read by windows (windows.go): a key repeated in a long text is read once.
var sepKeyWindows = newWindowed(nil, sepKey)

// maxSepRun bounds the marks, controls and invisible characters of a separator run, so that a long run of
// punctuation after a key is no separator (a heading underlined with "=" after "Password"). A key followed by a
// longer run keeps its value. Blanks do not count (#845, decision of 2026-11-18): a column-aligned key
// ("password::", fifteen spaces, the value) is read like one with a single space, and each blank is still read
// once, since the run of one key ends where the next key starts.
const maxSepRun = 16

// sepRun is a run of separator characters (sepRunAt): where it ends, and what it holds.
type sepRun struct {
	end   int
	marks string // its colons, equals signs and arrows, in order
	plain bool   // nothing but marks, spaces and tabs
}

// sepBlankLen returns the length of the blank at s[i], or 0: a space, a tab, or another Unicode space that is
// no line break.
func sepBlankLen(_ *meter.Meter, s string, i int) int {
	if c := s[i]; c == ' ' || c == '\t' {
		return 1
	} else if c < utf8.RuneSelf {
		return 0
	}
	r, n := utf8.DecodeRuneInString(s[i:])
	if unicode.Is(unicode.Zs, r) {
		return n
	}
	return 0
}

// sepRunAt reads the run of separator characters at s[i]: blanks and other indentation characters but line
// breaks (indentLen), invisible characters, ":" and its look-alikes, "=" and its look-alikes, "->" and "=>"
// and the arrow characters (foldArrows), with at most maxSepRun of them that are no blanks (sepBlankLen). ok is
// false when there is no mark in it, or when the run goes on past maxSepRun characters that are no blanks.
func sepRunAt(wm *meter.Meter, s string, i int) (r sepRun, ok bool) {
	r.plain = true
	j := i
	for n := 0; j < len(s); {
		tick(wm, 1)
		k := 0
		switch c := s[j]; {
		case c == ':' || c == '=':
			k = 1
			if c == '=' && j+1 < len(s) && s[j+1] == '>' {
				k, r.plain = 2, false
			}
			r.marks += s[j : j+k]
		case c == '-' && j+1 < len(s) && s[j+1] == '>':
			k, r.plain = 2, false
			r.marks += "->"
		case c == ' ' || c == '\t':
			j++
			continue
		case isFoldColonAt(wm, s, j):
			// A colon look-alike, read where the glyph folds have not run (a string of a sequence as written).
			k, r.plain = charLen(wm, s, j), false
			r.marks += ":"
		default:
			if m, ok := foldMarkAt(wm, s, j); ok {
				// A look-alike of "=" or an arrow character, read where the glyph folds have not run.
				k, r.plain = charLen(wm, s, j), false
				r.marks += m
				break
			}
			if b := sepBlankLen(wm, s, j); b > 0 {
				j += b
				r.plain = false
				continue
			}
			if k = indentLen(wm, s, j); k == 0 {
				k = invisibleLen(wm, s, j)
			}
			if k == 0 {
				r.end = j
				return r, r.marks != ""
			}
			r.plain = false
		}
		if n == maxSepRun {
			return r, false
		}
		n++
		j += k
	}
	r.end = j
	return r, r.marks != ""
}

// foldsAsSeparator reports whether the separator fold rewrites the run r: it holds a mark, and it is not one
// ":", "=" or ":=" between spaces and tabs.
func (r sepRun) foldsAsSeparator(_ *meter.Meter) bool {
	return !r.plain || r.marks != ":" && r.marks != "=" && r.marks != ":="
}

// sepWalk calls emit for each piece of the separator fold of s: the text between separators, copied, and each
// separator run after a secret key (sepKey) that folds (foldsAsSeparator), folded. A secret key with a
// separator right after the separator of another secret key ("auth: password: V", "token = apiKey -> V")
// starts a line of its own in the view: a line break takes the place of the blanks before it, so the key before
// takes no value on that line and this key takes its own (#845, decision of 2026-11-18: the key before took
// "password" as its value, and V was shown). With emit nil it only reports, as soon as it finds one, whether
// anything folds.
func sepWalk(wm *meter.Meter, s string, emit func(text string, from, to int, copied bool)) bool {
	changed, pos, prevRunEnd := false, 0, -1
	found := false // with emit nil, something folds: the matches after it are read, as before, but not looked at
	sepKeyWindows.each(wm, s, func(m []int) bool {
		if found {
			return true
		}
		tick(wm, 1)
		at := m[1]
		if s[at-1] == '-' && at < len(s) && s[at] == '>' {
			// The key took the dash of an arrow.
			at--
		}
		if at < pos {
			return true
		}
		r, ok := sepRunAt(wm, s, at)
		if !ok {
			prevRunEnd = -1
			return true
		}
		// A plain run the rules read stays, unless an "=" follows a quoted key ("\"password\" = V"), which no
		// rule reads.
		fold := r.foldsAsSeparator(wm) || r.marks != ":" && (s[at-1] == '"' || s[at-1] == '\'')
		keyAt := m[0]
		if keyAt > 0 && (s[keyAt-1] == '"' || s[keyAt-1] == '\'') {
			keyAt--
		}
		nested := keyAt == prevRunEnd && keyAt >= pos
		prevRunEnd = r.end
		if !fold && !nested {
			return true
		}
		changed = true
		if emit == nil {
			found = true
			return true
		}
		if nested {
			blanksAt := keyAt
			for blanksAt > pos && (s[blanksAt-1] == ' ' || s[blanksAt-1] == '\t') {
				tick(wm, 1)
				blanksAt--
			}
			if blanksAt > pos {
				emit(s[pos:blanksAt], pos, blanksAt, true)
			}
			emit("\n", blanksAt, keyAt, false)
			pos = keyAt
		}
		if !fold {
			return true
		}
		if at > pos {
			emit(s[pos:at], pos, at, true)
		}
		emit(": ", at, r.end, false)
		pos = r.end
		return true
	})
	if found {
		return true
	}
	if changed && pos < len(s) {
		emit(s[pos:], pos, len(s), true)
	}
	return changed
}
