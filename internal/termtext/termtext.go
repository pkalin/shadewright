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

// Package termtext turns untrusted text into what a terminal shows of it. The kubectl plugin prints every
// string from the cluster or the model through [Clean]. The OpenAI-compatible provider's credential scan
// uses [EscapeEnd], the same grammar of escape sequences, so a sequence a terminal hides cannot hide a key
// from the scan. It imports only the standard library.
package termtext

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Clean makes a string safe to print on a terminal: it removes whole ANSI escape sequences (CSI, OSC, DCS,
// SOS, PM, APC and two-character ESC sequences, in their 7-bit and 8-bit forms), then every other control
// character (Unicode Cc), format character (Cf, which holds the bidirectional overrides and zero-width
// characters) and line or paragraph separator. Tabs and line breaks become one space, invalid UTF-8 becomes
// U+FFFD. A maxRunes of zero or more cuts the result to that many runes, ending in "..." only when visible
// text was cut off (not when only escapes or invisible characters follow); a negative maxRunes keeps it
// whole. The payload of a string sequence (an OSC title or hyperlink target) is dropped with it, so a name
// cannot set the window title, write a hidden link, move the cursor or rewrite earlier lines.
func Clean(s string, maxRunes int) string {
	out, i := clean(s, maxRunes, false)
	if maxRunes >= 0 && i < len(s) && maxRunes > 3 && Clean(s[i:], 1) != "" {
		out = string([]rune(out)[:maxRunes-3]) + "..."
	}
	return out
}

// Compact returns the runes Clean(s, -1) shows, without whitespace (unicode.IsSpace), in one pass.
func Compact(s string) string {
	out, _ := clean(s, -1, true)
	return out
}

// EscapeEnd reports whether an escape sequence starts at s[i] (ESC, or the UTF-8 form of an 8-bit CSI, DCS,
// SOS, OSC, PM or APC introducer), and returns the index after it as [Clean] skips it.
func EscapeEnd(s string, i int) (int, bool) {
	r, size := utf8.DecodeRuneInString(s[i:])
	switch r {
	case 0x1b:
		return skipEscape(s, i+size), true
	case 0x9b:
		return skipCSI(s, i+size), true
	case 0x90, 0x98, 0x9d, 0x9e, 0x9f:
		return skipString(s, i+size), true
	}
	return i, false
}

// clean writes what a terminal shows of s, up to maxRunes runes when maxRunes is not negative, and returns
// it with the index of s where it stopped. With compact set, whitespace is left out.
func clean(s string, maxRunes int, compact bool) (string, int) {
	var b strings.Builder
	n, i := 0, 0
	for i < len(s) && (maxRunes < 0 || n < maxRunes) {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			b.WriteRune(utf8.RuneError)
			n++
			i += max(size, 1)
			continue
		case r == 0x1b:
			i = skipEscape(s, i+size)
			continue
		case r == 0x9b: // 8-bit CSI
			i = skipCSI(s, i+size)
			continue
		case r == 0x90 || r == 0x98 || r == 0x9d || r == 0x9e || r == 0x9f: // 8-bit DCS, SOS, OSC, PM, APC
			i = skipString(s, i+size)
			continue
		case r == '\t' || r == '\n' || r == '\r':
			if !compact {
				b.WriteByte(' ')
				n++
			}
		case unicode.Is(unicode.Cc, r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			// dropped
		case compact && unicode.IsSpace(r):
			// left out of the compact form
		default:
			b.WriteRune(r)
			n++
		}
		i += size
	}
	return b.String(), i
}

// skipEscape returns the index after the escape sequence whose ESC ended just before i.
func skipEscape(s string, i int) int {
	if i >= len(s) {
		return i
	}
	switch s[i] {
	case '[':
		return skipCSI(s, i+1)
	case ']', 'P', 'X', '^', '_':
		return skipString(s, i+1)
	}
	// A two-character sequence (or ESC with intermediates): skip intermediates, then one final character.
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
		i++
	}
	if i < len(s) && s[i] >= 0x30 && s[i] <= 0x7e {
		i++
	}
	return i
}

// skipCSI returns the index after a control sequence whose introducer ended just before i: parameter and
// intermediate bytes, then one final byte. A sequence cut short by a byte outside those ranges ends there.
func skipCSI(s string, i int) int {
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x3f {
		i++
	}
	if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
		i++
	}
	return i
}

// skipString returns the index after a string sequence (OSC, DCS, SOS, PM, APC) whose introducer ended just
// before i: up to and including BEL, ESC \ or the 8-bit ST, up to another ESC, or to the end of s.
func skipString(s string, i int) int {
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case 0x07, 0x9c:
			return i + size
		case 0x1b:
			if i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
			return i // an ESC that is not ST ends the string and starts a sequence of its own
		}
		i += size
	}
	return i
}
