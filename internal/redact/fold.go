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
)

// The folded view of a text (#845, second and third rounds): the text a reader sees, where the rules read the
// text as written. Three things are folded, and nothing else:
//
//   - A run of format characters (Unicode category Cf: the zero-width space and joiners, the word joiner, the
//     byte order mark, the soft hyphen, the Mongolian vowel separator) after a byte of a key name is dropped
//     when a key byte, a colon, an equals sign, a quote, a colon look-alike or the end of the line follows it
//     ("pass\u200bword", "api_key\u200b: V"): a reader does not see it, and it split the key from itself or
//     from its colon for every rule.
//   - Each character of foldColons becomes ":".
//   - An explicit YAML key, "? key" with ":" at the same column on the next line, becomes "key:" on one line;
//     so does a key on its own line below "?" or below "? |" (a block scalar: one line of key, more indented
//     than the "?", blank lines around it), with ":" at the column of the "?" after it.
//
// The rules read the folded view as well as the text ([Redactor.spans]); each of its matches counts on the
// bytes of the text it was folded from, so a redacted range still starts and ends on the original bytes and
// covers the characters folded away inside it. A text with nothing to fold is read once. [Redactor.Strings]
// reads each string of a sequence in its folded view too (seqValues).

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

// foldedView is the folded view of a text: its text, and for each of its bytes the first byte of the original
// it stands for (from) and the end of the last (to).
type foldedView struct {
	text     string
	from, to []int
}

// isKeyByte reports whether c is a byte of a key name.
func isKeyByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-'
}

// foldView returns the folded view of s, and false when it is s itself. A first pass only looks for something to
// fold, so a text with nothing to fold costs one read and no allocation.
func foldView(s string) (foldedView, bool) {
	if !foldWalk(s, nil) {
		return foldedView{}, false
	}
	var b strings.Builder
	b.Grow(len(s))
	f := foldedView{from: make([]int, 0, len(s)), to: make([]int, 0, len(s))}
	foldWalk(s, func(text string, from, to int) {
		b.WriteString(text)
		for i := range len(text) {
			if to-from == len(text) {
				f.from, f.to = append(f.from, from+i), append(f.to, from+i+1)
				continue
			}
			f.from, f.to = append(f.from, from), append(f.to, to)
		}
	})
	f.text = b.String()
	return f, true
}

// foldWalk reads s line by line and calls emit for each piece of the folded view: its text and the range of s
// it stands for. With emit nil it only reports, as soon as it finds one, whether anything folds.
func foldWalk(s string, emit func(text string, from, to int)) bool {
	changed := false
	copyText := func(from, to int) {
		if emit != nil && to > from {
			emit(s[from:to], from, to)
		}
	}
	skipIndent := false
	for pos := 0; pos < len(s); {
		tick(1)
		l := readLine(s, pos)
		pos = l.next
		at := l.text
		if !skipIndent {
			copyText(l.start, l.text)
		}
		skipIndent = false
		at += dashes(s[l.text:l.end])
		copyText(l.text, at)
		if at < l.end && s[at] == '?' && (at+1 == l.end || indentLen(s, at+1) > 0) && l.next < len(s) {
			q := colOf(s, l.start, at)
			n := readLine(s, l.next)
			if !n.blank() && s[n.text] == ':' && n.ind == q {
				// An explicit key: "? key" and ":" below the "?" read as "key:".
				changed = true
				if emit == nil {
					return true
				}
				key := at + 1
				for key < l.end && indentLen(s, key) > 0 {
					tick(1)
					key += indentLen(s, key)
				}
				end := l.end
				for end > key && lastIndentLen(s[:end], end) > 0 {
					tick(1)
					end -= lastIndentLen(s[:end], end)
				}
				if foldChars(s, key, end, emit) {
					changed = true
				}
				// The line break and the indentation before ":" fold away, onto the last byte of the key.
				skipIndent = true
				continue
			}
			if k, c, ok := explicitKeyBelow(s, at, l, q); ok {
				// A key below "?" or "? |": the header, the line breaks, the blank lines and the indentation
				// before the key and before ":" fold away.
				changed = true
				if emit == nil {
					return true
				}
				end := k.end
				for end > k.text && lastIndentLen(s[:end], end) > 0 {
					tick(1)
					end -= lastIndentLen(s[:end], end)
				}
				foldChars(s, k.text, end, emit)
				pos, skipIndent = c.start, true
				continue
			}
		}
		if foldChars(s, at, l.end, emit) {
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
func explicitKeyBelow(s string, at int, l viewLine, q col) (k, c viewLine, ok bool) {
	h := at + 1
	for h < l.end && indentLen(s, h) > 0 {
		tick(1)
		h += indentLen(s, h)
	}
	if !blockKeyHeader.MatchString(counted(s[h:l.end])) {
		return k, c, false
	}
	next := func(pos int) (viewLine, bool) {
		for pos < len(s) {
			tick(1)
			n := readLine(s, pos)
			if !n.blank() {
				return n, true
			}
			pos = n.next
		}
		return viewLine{}, false
	}
	k, found := next(l.next)
	if !found || !k.ind.deeper(q) || s[k.text] == ':' || s[k.text] == '#' {
		return k, c, false
	}
	c, found = next(k.next)
	if !found || s[c.text] != ':' || c.ind != q {
		return k, c, false
	}
	return k, c, true
}

// foldChars emits s[from:to] with the format characters inside key names dropped and the colon look-alikes
// folded to ":", and reports whether it folded anything. With emit nil it stops at the first fold.
func foldChars(s string, from, to int, emit func(text string, from, to int)) bool {
	changed := false
	plain := from
	flush := func(i int) {
		if emit != nil && i > plain {
			emit(s[plain:i], plain, i)
		}
	}
	for i := from; i < to; {
		tick(1)
		if s[i] < utf8.RuneSelf {
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case isFoldColon(r):
			changed = true
			if emit == nil {
				return true
			}
			flush(i)
			emit(":", i, i+n)
			plain = i + n
		case unicode.Is(unicode.Cf, r) && i > from && isKeyByte(s[i-1]):
			// A run of format characters after a byte of a key name, before another, a separator, a quote or the
			// end of the line.
			j := i + n
			for j < to {
				tick(1)
				r2, n2 := utf8.DecodeRuneInString(s[j:])
				if !unicode.Is(unicode.Cf, r2) {
					break
				}
				j += n2
			}
			if j == to || isKeyByte(s[j]) || strings.IndexByte(`:="'`, s[j]) >= 0 || isFoldColonAt(s, j) {
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

// isFoldColon reports whether r is one of foldColons.
func isFoldColon(r rune) bool { return r >= 0x2D0 && slices.Contains(foldColons, r) }

// isFoldColonAt reports whether the character at s[i] is one of foldColons.
func isFoldColonAt(s string, i int) bool {
	if s[i] < utf8.RuneSelf {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return isFoldColon(r)
}
