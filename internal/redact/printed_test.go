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
	"testing"
)

// passwordHidden is what the shown short entry point makes of "password=" and a value on the rest of its line.
const passwordHidden = "password=[REDACTED]"

// The shown short entry point prints the text first and redacts what it printed: the output is the printed text
// with its matches replaced, so nothing of an escape sequence is left and nothing after a match is lost.
func TestShownTextIsThePrintedText(t *testing.T) {
	const secret = "mysecretvalue" // #nosec G101 -- a made-up value the test looks for
	token := "ghp_" + strings.Repeat("A", 36)
	for _, c := range []struct{ in, want string }{
		// No part of the sequence is kept. The rule of a password takes the rest of its line as the value, as on
		// main, so " ok" goes with it.
		{"\x1b[31mpassword=" + secret + " ok", passwordHidden},
		{"\x1b[31mtoken " + token + "\x1b[0m ok", "token [REDACTED:token] ok"},
		{"pass\x1b[31mword=" + secret + "\x1b[0m ok", passwordHidden},
		{"pass\u009b31mword=" + secret + "\u009b0m ok", passwordHidden}, // the C1 CSI
		// Invisible characters and invalid bytes other than 0x80 to 0x9F are removed; a run of whitespace is one
		// space.
		{"pass\u200b\u202e\U000E0041\ufe0f\u00adword=" + secret + " ok", passwordHidden},
		{token[:20] + "\xff" + token[20:] + " ok", "[REDACTED:token] ok"},
		{"line one\r\n\tline\u00a0two\u2028end", "line one line two end"},
		// Text without anything to remove is decided as it is.
		{"Back-off restarting failed container app", "Back-off restarting failed container app"},
	} {
		got, ok := ReplaceCheckedShortShown(c.in, keep, same)
		if !ok || got != c.want {
			t.Errorf("ReplaceCheckedShortShown(%q) = %q, %t; want %q", c.in, got, ok, c.want)
		}
	}
}

// An entry is withheld whole when it cannot be printed with certainty (an escape string without its terminator, a
// broken escape, a backspace or DEL), and when a squeezed text or the text as read finds a secret the printed text
// does not hide: a token an NBSP or a CRLF splits, a key an escape that takes a byte splits, a value a line end splits.
func TestShownTextIsWithheldWhenItCouldShowASecret(t *testing.T) {
	const secret = "mysecretvalue" // #nosec G101 -- a made-up value the test looks for
	token := "ghp_" + strings.Repeat("A", 36)
	for _, in := range []string{
		"pass\x1b]0;title word=" + secret,           // an OSC without its terminator
		"pass\x1bPq word=" + secret,                 // a DCS without its terminator
		"pass\x1b[3" + "\x00word=" + secret,         // a CSI broken by a control
		"\x1b",                                      // an escape at the end
		"--password\x1b " + secret,                  // an escape with a space as its intermediate byte
		"password=" + secret[:5] + "\b\b\b" + "xyz", // overstrike
		"password=" + secret + "\x7f",
		token[:20] + "\u00a0" + token[20:],                     // the printed text holds a space where the squeezed one does not
		token[:20] + "\r\n" + token[20:],                       // the same, a line end
		"pass\x1bword=" + secret,                               // and here "w" of the key
		"pass\x1b[word=" + secret,                              // a CSI takes "w"
		"--password " + secret[:6] + "\r" + secret[6:] + " ok", // the match of the printed text ends at a line end
	} {
		if got, ok := ReplaceCheckedShortShown(in, keep, same); ok {
			t.Errorf("ReplaceCheckedShortShown(%q) = %q, want it withheld", in, got)
		}
	}
}

// Terminal strings are not interpreted: a complete CSI is the only escape the printed text removes. Every other
// escape and every C1 control withholds the entry whole, whatever its encoding (ESC and a byte, the C1 control as
// UTF-8, the C1 control as one raw byte), terminated or not, nested or not.
func TestShownTextWithholdsEveryEscapeButCSI(t *testing.T) {
	var in []string
	// The three inputs of the finding: a raw OSC byte, its payload printed; the same unterminated; nested strings.
	in = append(in, "\x9dtitle\x07ready", "\x9dtitle", "x\u009done\u009dtwo\u009crest\u009cz")
	// Each string introducer and ST in its three encodings, terminated by BEL, by ST, or not at all.
	type enc struct{ esc, c1, raw string }
	strs := []enc{
		{"\x1b]", "\u009d", "\x9d"}, // OSC
		{"\x1bP", "\u0090", "\x90"}, // DCS
		{"\x1b_", "\u009f", "\x9f"}, // APC
		{"\x1b^", "\u009e", "\x9e"}, // PM
		{"\x1bX", "\u0098", "\x98"}, // SOS
	}
	st := enc{"\x1b\\", "\u009c", "\x9c"}
	for _, s := range strs {
		for _, intro := range []string{s.esc, s.c1, s.raw} {
			for _, end := range []string{"\x07", st.esc, st.c1, st.raw, ""} {
				in = append(in, "ready "+intro+"payload"+end+" ok")
				// Nested: the inner string ends first, the outer one later.
				in = append(in, "x"+intro+"one"+intro+"two"+end+"rest"+end+"z")
			}
		}
	}
	// ST alone, every other C1 control in both encodings, and NEL as a raw byte.
	for _, end := range []string{st.esc, st.c1, st.raw} {
		in = append(in, "ready"+end+"ok")
	}
	for c := rune(0x80); c <= 0x9f; c++ {
		if c == 0x9b {
			continue // the C1 CSI, complete, is removed (TestShownTextIsThePrintedText)
		}
		in = append(in, "ready "+string([]byte{byte(c)})+" ok")
		if c != 0x85 {
			in = append(in, "ready "+string(c)+" ok") // NEL as UTF-8 is a line end (TestNELIsALineEnd)
		}
	}
	// The raw CSI byte, and escapes other than CSI: two-byte escapes, an escape with an intermediate byte.
	in = append(in, "ready \x9b31m ok", "ready \x1b7 ok", "ready \x1b= ok", "ready \x1b(B ok", "ready \x1bA ok")
	for _, s := range in {
		if got, ok := ReplaceCheckedShortShown(s, keep, same); ok {
			t.Errorf("ReplaceCheckedShortShown(%q) = %q, want it withheld", s, got)
		}
	}
}

// NEL (U+0085) encoded as UTF-8 is a line end, Unicode White_Space like CR, LF and U+2028: a run of whitespace
// that holds it is one space, and a secret it splits is still hidden or the entry withheld. The raw byte 0x85 is
// a C1 control in its 8-bit form and withholds the entry (TestShownTextWithholdsEveryEscapeButCSI).
func TestNELIsALineEnd(t *testing.T) {
	const secret = "mysecretvalue" // #nosec G101 -- a made-up value the test looks for
	token := "ghp_" + strings.Repeat("A", 36)
	for _, c := range []struct{ in, want string }{
		{"line one\u0085line two", "line one line two"},
		{"line one\r\u0085 \tline two", "line one line two"},
		{"Back-off\u0085restarting failed container", "Back-off restarting failed container"},
	} {
		if got, ok := ReplaceCheckedShortShown(c.in, keep, same); !ok || got != c.want {
			t.Errorf("ReplaceCheckedShortShown(%q) = %q, %t; want %q", c.in, got, ok, c.want)
		}
	}
	for _, in := range []string{
		token[:20] + "\u0085" + token[20:] + " ok",
		"pass\u0085word=" + secret + " ok",
		"password=" + secret[:6] + "\u0085" + secret[6:] + " ok",
		"--password " + secret[:6] + "\u0085" + secret[6:] + " ok",
	} {
		got, ok := ReplaceCheckedShortShown(in, keep, same)
		if ok && (strings.Contains(got, secret[:4]) || strings.Contains(got, secret[6:10]) ||
			strings.Contains(got, token[:20]) || strings.Contains(got, token[20:])) {
			t.Errorf("ReplaceCheckedShortShown(%q) = %q shows the secret", in, got)
		}
	}
	if got, ok := ReplaceCheckedShortShown("line one\x85line two", keep, same); ok {
		t.Errorf("ReplaceCheckedShortShown(raw 0x85) = %q, want it withheld", got)
	}
}
