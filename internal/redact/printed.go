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

// The printed text of a shown short entry (ReplaceCheckedShortShown). The text an Incident stores is built first,
// and the rules read that text and nothing else: what is matched is what is printed, and no match is moved from
// one text to another.
//
// The printed text is the entry as a terminal prints it, on one line:
//
//   - invalid UTF-8 bytes are dropped;
//   - each complete escape sequence is removed whole: a CSI (ESC "[" or U+009B, parameter bytes 0x30 to 0x3F,
//     intermediate bytes 0x21 to 0x2F, one final byte 0x40 to 0x7E), an OSC (ESC "]" or U+009D, up to BEL or the
//     string terminator ST, ESC "\" or U+009C), a DCS, SOS, PM or APC (ESC "P", "X", "^", "_" or U+0090, U+0098,
//     U+009E, U+009F, up to ST), an escape with intermediate bytes (ESC, bytes 0x21 to 0x2F, one final byte 0x30
//     to 0x7E) and a two-byte escape (ESC and one byte 0x30 to 0x7E);
//   - a run of whitespace of any kind (Unicode White_Space: blanks, tabs, CR, LF, NEL, the no-break and wide
//     spaces, the line and paragraph separators) becomes one ASCII space; what is removed in a run does not end it;
//   - every other character that is not printable is removed: the C0 and C1 controls, the format characters
//     (category Cf: zero-width, bidirectional, tag, soft hyphen), the default-ignorable ones (variation selectors,
//     fillers; isInvisible) and code points that are not assigned or are private.
//
// An entry is withheld whole when it cannot be printed that way with certainty: an escape sequence or string that
// is not terminated or holds a byte its kind does not allow (a space among them: "--password" ESC " value" would
// take the space and the first byte of the value), or a backspace or DEL, which a terminal uses to overstrike what
// it printed.
//
// Withheld on divergence. The same entry is also read as squeezed texts, in which every separator the printed text
// removed or collapsed is deleted outright: a run of whitespace is deleted unless it is one ASCII space (a plain
// single space of the input stays), and every removed character is deleted. The first squeezed text removes each
// escape sequence whole, as the printed text does; the second keeps every byte of an escape sequence after its
// introducer (the bytes a sequence may have taken from the text after it: "pass" ESC "word", "pass" ESC "[word").
// The entry as it was read (the readings of origin/main on it) is the third. When a rule finds in any of them a
// secret whose printed bytes the matches of the printed text do not all cover, the entry is withheld whole. It is
// withheld too when a match of the printed text, other than an address, ends at a run of whitespace other than one
// ASCII space that more text follows: the run may split the value ("--password abc" CR "def" prints as
// "--password abc def", and the match holds "abc" alone). So a credential a control, an escape sequence, an
// invisible character, an invalid byte, a line end or a blank other than one ASCII space splits is either hidden
// in the printed text or not shown at all. Not in scope: a credential split by plain ASCII spaces alone
// ("ghp_abc def"), which the printed and the squeezed texts both show as it is.
//
// Cost: each byte is read once to build the texts, which hold at most the bytes of the entry each, and one 32-bit
// offset for each of their bytes; the rules read each text once, so the work stays linear in the entry, within the
// budget of its size, and an entry over it is withheld. A text without anything to remove or collapse is read once,
// as before.

// shownTexts are the printed text of an entry and the texts it is checked against.
type shownTexts struct {
	// printed is the printed text.
	printed string
	// squeezed and squeezedBodies are the squeezed texts, without and with the bytes of escape sequences after
	// their introducers; squeezedAt and bodiesAt give for each of their bytes the byte of printed it is, or -1.
	squeezed, squeezedBodies string
	squeezedAt, bodiesAt     []int32
	// readAt gives for each byte of the entry the byte of printed it is, or -1 (removed, or a run of whitespace).
	readAt []int32
	// collapsed holds, in order, the bytes of printed that stand for a run of whitespace other than one ASCII space.
	collapsed []int32
	// changed is false when printed is the entry, byte for byte.
	changed bool
}

// printedBuilder builds the shown texts of one entry.
type printedBuilder struct {
	p, q, f          strings.Builder
	qAt, fAt, readAt []int32
	runs, runSpaces  int // the code points of the open run of whitespace, and its ASCII spaces
	runOpen          bool
	// collapsed holds the bytes of the printed text that are a run of whitespace other than one ASCII space.
	collapsed []int32
}

// printedTextsOf returns the shown texts of s, and false when s is withheld whole (an escape that is not complete,
// a backspace or DEL). The work is linear in the length of s.
func printedTextsOf(wm *meter.Meter, s string) (shownTexts, bool) {
	b := printedBuilder{readAt: make([]int32, len(s))}
	b.p.Grow(len(s))
	b.q.Grow(len(s))
	b.f.Grow(len(s))
	b.qAt = make([]int32, 0, len(s))
	b.fAt = make([]int32, 0, len(s))
	for i := range b.readAt {
		b.readAt[i] = -1
	}
	for i := 0; i < len(s); {
		tick(wm, 1)
		if n, body, ok := escapeAt(wm, s, i); n > 0 || !ok {
			if !ok {
				return shownTexts{}, false
			}
			if body >= 0 {
				b.f.WriteString(s[body : i+n])
				for range i + n - body {
					b.fAt = append(b.fAt, -1)
				}
			}
			i += n
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			// An invalid byte is dropped.
		case r == '\b' || r == 0x7f:
			return shownTexts{}, false
		case unicode.IsSpace(r):
			if !b.runOpen {
				b.runOpen, b.runs, b.runSpaces = true, 0, 0
			}
			b.runs++
			if r == ' ' {
				b.runSpaces++
			}
		case !printable(wm, r):
			// A control, format or invisible character is removed.
		default:
			b.closeRun()
			at := int32(b.p.Len()) // #nosec G115 -- an entry is at most MaxEntry bytes
			for k := range n {
				b.readAt[i+k] = at + int32(k) // #nosec G115 -- n is at most 4
				b.qAt = append(b.qAt, at+int32(k))
				b.fAt = append(b.fAt, at+int32(k))
			}
			b.p.WriteString(s[i : i+n])
			b.q.WriteString(s[i : i+n])
			b.f.WriteString(s[i : i+n])
		}
		i += n
	}
	b.closeRun()
	p := b.p.String()
	return shownTexts{printed: p, squeezed: b.q.String(), squeezedBodies: b.f.String(), squeezedAt: b.qAt,
		bodiesAt: b.fAt, readAt: b.readAt, collapsed: b.collapsed, changed: p != s}, true
}

// closeRun writes the open run of whitespace: one space in the printed text, and one in the squeezed texts only
// when the run is one ASCII space; any other run is deleted from them outright. A space holds no part of a secret:
// it maps to no byte of the printed text, so a match over it needs no cover there.
func (b *printedBuilder) closeRun() {
	if !b.runOpen {
		return
	}
	b.runOpen = false
	if b.runs == 1 && b.runSpaces == 1 {
		b.q.WriteByte(' ')
		b.qAt = append(b.qAt, -1)
		b.f.WriteByte(' ')
		b.fAt = append(b.fAt, -1)
	} else {
		b.collapsed = append(b.collapsed, int32(b.p.Len())) // #nosec G115 -- an entry is at most MaxEntry bytes
	}
	b.p.WriteByte(' ')
}

// printable reports whether r is kept in the printed text: a graphic character (unicode.IsGraphic) that is not
// a format character, not default-ignorable (isInvisible) and not a space (the caller reads spaces first).
func printable(wm *meter.Meter, r rune) bool {
	if r < utf8.RuneSelf {
		return r >= 0x21 && r < 0x7f
	}
	return unicode.IsGraphic(r) && !unicode.Is(unicode.Cf, r) && !isInvisible(wm, r)
}

// escapeAt reads the escape sequence that starts at s[i]: its length n (0 when none starts there), the byte of
// s right after its introducer (ESC, ESC "[" or a C1 introducer; -1 for an escape string, whose end is a
// terminator), and false when one starts there but is not complete or holds a byte its kind does not allow.
func escapeAt(wm *meter.Meter, s string, i int) (n, body int, ok bool) {
	switch {
	case s[i] == 0x1b:
		return escapeAfterESC(wm, s, i)
	case s[i] == 0xc2 && i+1 < len(s):
		switch s[i+1] {
		case 0x9b:
			return csiAt(wm, s, i, i+2)
		case 0x9d:
			return escapeStringAt(wm, s, i, i+2, true)
		case 0x90, 0x98, 0x9e, 0x9f:
			return escapeStringAt(wm, s, i, i+2, false)
		}
	}
	return 0, -1, true
}

// escapeAfterESC is escapeAt for the ESC at s[i].
func escapeAfterESC(wm *meter.Meter, s string, i int) (n, body int, ok bool) {
	if i+1 >= len(s) {
		return 0, -1, false
	}
	switch c := s[i+1]; {
	case c == '[':
		return csiAt(wm, s, i, i+2)
	case c == ']':
		return escapeStringAt(wm, s, i, i+2, true)
	case c == 'P' || c == 'X' || c == '^' || c == '_':
		return escapeStringAt(wm, s, i, i+2, false)
	case c >= 0x21 && c <= 0x2f:
		// An escape with intermediate bytes, then one final byte.
		j := i + 1
		for j < len(s) && s[j] >= 0x21 && s[j] <= 0x2f {
			tick(wm, 1)
			j++
		}
		if j < len(s) && s[j] >= 0x30 && s[j] <= 0x7e {
			return j + 1 - i, i + 1, true
		}
	case c >= 0x30 && c <= 0x7e:
		return 2, i + 1, true
	}
	return 0, -1, false
}

// csiAt reads the CSI that starts at s[i], whose parameter bytes start at s[j].
func csiAt(wm *meter.Meter, s string, i, j int) (n, body int, ok bool) {
	start := j
	for j < len(s) && s[j] >= 0x30 && s[j] <= 0x3f {
		tick(wm, 1)
		j++
	}
	for j < len(s) && s[j] >= 0x21 && s[j] <= 0x2f {
		tick(wm, 1)
		j++
	}
	if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7e {
		return j + 1 - i, start, true
	}
	return 0, -1, false
}

// escapeStringAt reads the escape string that starts at s[i], whose payload starts at s[j], up to its terminator:
// ST (ESC "\" or U+009C), or BEL for an OSC (osc). An escape inside it must be ST.
func escapeStringAt(wm *meter.Meter, s string, i, j int, osc bool) (n, body int, ok bool) {
	for ; j < len(s); j++ {
		tick(wm, 1)
		switch {
		case s[j] == 0x07 && osc:
			return j + 1 - i, -1, true
		case s[j] == 0x1b:
			if j+1 < len(s) && s[j+1] == '\\' {
				return j + 2 - i, -1, true
			}
			return 0, -1, false
		case s[j] == 0xc2 && j+1 < len(s) && s[j+1] == 0x9c:
			return j + 2 - i, -1, true
		}
	}
	return 0, -1, false
}

// shownSpans returns the printed text of s and the spans of it to replace, and false when s is withheld whole:
// it cannot be printed with certainty, or a rule finds in a squeezed text or in s as it was read a secret whose
// printed bytes the spans of the printed text do not all cover.
func (r *Redactor) shownSpans(wm *meter.Meter, s string) (string, []span, bool) {
	t, ok := printedTextsOf(wm, s)
	if !ok {
		return "", nil, false
	}
	spans := r.spansAsMain(wm, t.printed)
	if !t.changed {
		return t.printed, spans, true
	}
	if continuesPastARun(wm, t, spans) {
		return "", nil, false
	}
	covered := make([]bool, len(t.printed))
	for _, sp := range spans {
		for k := sp.start; k < sp.end; k++ {
			tick(wm, 1)
			covered[k] = true
		}
	}
	shows := func(text string, at []int32) bool {
		for _, sp := range r.spansAsMain(wm, text) {
			for k := sp.start; k < sp.end; k++ {
				tick(wm, 1)
				if p := at[k]; p >= 0 && !covered[p] {
					return true
				}
			}
		}
		return false
	}
	if shows(t.squeezed, t.squeezedAt) || (t.squeezedBodies != t.squeezed && shows(t.squeezedBodies, t.bodiesAt)) ||
		shows(s, t.readAt) {
		return "", nil, false
	}
	return t.printed, spans, true
}

// continuesPastARun reports whether a match of the printed text other than an address ends right before a run of
// whitespace other than one ASCII space that more text follows: the run may split the secret ("password=abc" CR
// "def" prints as "password=abc def", and the match holds "abc" alone), so the entry is withheld. spans and
// t.collapsed are both in order, so each is read once.
func continuesPastARun(wm *meter.Meter, t shownTexts, spans []span) bool {
	k := 0
	for _, sp := range spans {
		tick(wm, 1)
		for k < len(t.collapsed) && int(t.collapsed[k]) < sp.end {
			tick(wm, 1)
			k++
		}
		if k < len(t.collapsed) && int(t.collapsed[k]) == sp.end && sp.end+1 < len(t.printed) && sp.kind != KindIP {
			return true
		}
	}
	return false
}
