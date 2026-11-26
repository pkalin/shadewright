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
		// An OSC and a DCS are removed whole, up to their terminators.
		{"pass\x1b]0;title\x07word=" + secret + " ok", passwordHidden},
		{"pass\x1bPq#0\x1b\\word=" + secret + " ok", passwordHidden},
		{"pass\u009d0;t\u009cword=" + secret + " ok", passwordHidden},
		// Invisible characters and invalid bytes are removed; a run of whitespace is one space.
		{"pass\u200b\u202e\U000E0041\ufe0f\u00adword=" + secret + " ok", passwordHidden},
		{token[:20] + "\xff" + token[20:] + " ok", "[REDACTED:token] ok"},
		{token[:20] + "\x1bA" + token[20:], "[REDACTED:token]"}, // a two-byte escape takes "A": still a token
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
