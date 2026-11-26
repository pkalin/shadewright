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

// The printed text of a shown short entry (ReplaceCheckedShortShown). The printed text is built first, and the
// rules read that text and nothing else: what is matched is what is printed, and no match is moved from one text
// to another. The callers then normalise and cut the redacted text (package fingerprint), so what an Incident
// stores is that text after their normalisation, not the printed text itself.
//
// The printed text is the entry as a terminal prints it, on one line:
//
//   - invalid UTF-8 bytes other than 0x80 to 0x9F are dropped;
//   - each complete CSI (ESC "[" or U+009B, parameter bytes 0x30 to 0x3F, intermediate bytes 0x21 to 0x2F, one
//     final byte 0x40 to 0x7E: colours, cursor moves, erasing) is removed whole. Cursor movement is not simulated:
//     "state" ESC "[2DOK" prints as "staOK" on a terminal, and as "stateOK" here;
//   - a run of whitespace of any kind (Unicode White_Space: blanks, tabs, CR, LF, the no-break and wide
//     spaces, the line and paragraph separators) becomes one ASCII space; what is removed in a run does not end it;
//   - every other character that is not printable is removed: the C0 controls but backspace, the format characters
//     (category Cf: zero-width, bidirectional, tag, soft hyphen), the default-ignorable ones (variation selectors,
//     fillers; isInvisible) and code points that are not assigned or are private.
//
// An entry is withheld whole when it cannot be printed that way with certainty. Terminal strings are not
// interpreted, so every escape but a complete CSI withholds it: a CSI that is not complete or holds a byte its kind
// does not allow, ESC followed by anything but "[" (an OSC, DCS, SOS, PM or APC string, ST, a two-byte escape, an
// escape with intermediate bytes, ESC at the end), any other C1 control (U+0080 to U+009F as UTF-8, NEL among them)
// and a raw byte 0x80 to 0x9F (a C1 control in its 8-bit form), terminated or not, nested or not. A backspace or
// DEL, which a terminal uses to overstrike what it printed, withholds it too.
//
// Withheld on divergence. The same entry is also read as squeezed texts, in which every separator the printed text
// removed or collapsed is deleted outright: a run of whitespace is deleted unless it is one ASCII space (a plain
// single space of the input stays), and every removed character is deleted. The first squeezed text removes each
// CSI whole, as the printed text does; the second keeps every byte of a CSI after its introducer (the bytes a
// sequence may have taken from the text after it: "pass" ESC "[1mword").
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
		tick(wm, 1)
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
					tick(wm, 1)
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
				tick(wm, 1)
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
// s right after its introducer (ESC "[" or U+009B), and false when the entry is withheld there. Only a complete
// CSI is read; terminal strings are not interpreted. Any other escape (ESC and any byte but "[", ESC at the end)
// and any other C1 control (U+0080 to U+009F encoded as UTF-8, or one raw byte 0x80 to 0x9F, which is never the
// start of a character) withholds the entry: an OSC, DCS, SOS, PM or APC string and its terminator ST in every
// encoding among them, terminated or not, nested or not.
func escapeAt(wm *meter.Meter, s string, i int) (n, body int, ok bool) {
	switch c := s[i]; {
	case c == 0x1b:
		if i+1 < len(s) && s[i+1] == '[' {
			return csiAt(wm, s, i, i+2)
		}
		return 0, -1, false
	case c == 0xc2 && i+1 < len(s) && s[i+1] >= 0x80 && s[i+1] <= 0x9f:
		if s[i+1] == 0x9b {
			return csiAt(wm, s, i, i+2)
		}
		return 0, -1, false
	case c >= 0x80 && c <= 0x9f:
		return 0, -1, false
	}
	return 0, -1, true
}

// csiAt reads the CSI that starts at s[i], whose parameter bytes start at s[j]: parameter bytes 0x30 to 0x3F,
// intermediate bytes 0x21 to 0x2F, one final byte 0x40 to 0x7E. Any other byte before the final one, or the end
// of s, withholds the entry.
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
		tick(wm, 1)
		for k := sp.start; k < sp.end; k++ {
			tick(wm, 1)
			covered[k] = true
		}
	}
	shows := func(text string, at []int32) bool {
		for _, sp := range r.spansAsMain(wm, text) {
			tick(wm, 1)
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
// whitespace other than one ASCII space that more text follows: the run may split the secret ("--password abc" CR
// "def" prints as "--password abc def", and the match holds "abc" alone), so the entry is withheld. spans and
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
