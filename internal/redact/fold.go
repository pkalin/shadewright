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
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The folded view of a text (#845, second round): the text a reader sees, where the rules read the text as
// written. Three things are folded, and nothing else:
//
//   - A format character (Unicode category Cf: the zero-width space and joiners, the word joiner, the byte
//     order mark, the soft hyphen, the Mongolian vowel separator) between two characters of a key name
//     ("pass​word") is dropped: a reader does not see it, and it split the key for every rule.
//   - A character Unicode's confusables list maps to ":" (foldColons) becomes ":".
//   - An explicit YAML key, "? key" with ":" at the same column on the next line, becomes "key:" on one line.
//
// The rules read the folded view as well as the text ([Redactor.spans]); each of its matches counts on the
// bytes of the text it was folded from, so a redacted range still starts and ends on the original bytes and
// covers the characters folded away inside it. A text with nothing to fold is read once.

// foldColons are the characters that look like a colon and are folded to one: the fullwidth colon of CJK text,
// the small and the vertical presentation forms, the ratio sign and the modifier letter colon.
var foldColons = []rune{'：', '﹕', '︓', '∶', '꞉'}

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
			n := readLine(s, l.next)
			if !n.blank() && s[n.text] == ':' && n.ind == colOf(s, l.start, at) {
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
			// A run of format characters between two bytes of a key name.
			j := i + n
			for j < to {
				tick(1)
				r2, n2 := utf8.DecodeRuneInString(s[j:])
				if !unicode.Is(unicode.Cf, r2) {
					break
				}
				j += n2
			}
			if j < to && isKeyByte(s[j]) {
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
func isFoldColon(r rune) bool { return slices.Contains(foldColons, r) }
