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

package main

import (
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestClean(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"plain":                {"web-app", "web-app"},
		"SGR colour":           {"\x1b[31mred\x1b[0m", "red"},
		"clear screen":         {"a\x1b[2J\x1b[Hb", "ab"},
		"OSC title with BEL":   {"\x1b]0;pwned\x07name", "name"},
		"OSC 8 hyperlink":      {"\x1b]8;;https://evil.example\x1b\\click\x1b]8;;\x1b\\", "click"},
		"OSC cut by ESC":       {"\x1b]0;pwned\x1b[31mx", "x"},
		"unterminated OSC":     {"ok\x1b]0;pwned", "ok"},
		"DCS":                  {"\x1bPq#0;2;0;0;0\x1b\\x", "x"},
		"8-bit CSI":            {"a\u009b31mb", "ab"},
		"8-bit OSC":            {"a\u009d0;pwned\u009cb", "ab"},
		"two-character ESC":    {"a\x1bcb", "ab"},
		"ESC at end":           {"a\x1b", "a"},
		"bidi override":        {"abc\u202edcba", "abcdcba"},
		"zero-width":           {"a\u200bb", "ab"},
		"line breaks and tabs": {"a\nb\r\tc", "a b  c"},
		"line separator":       {"a\u2028b", "ab"},
		"backspace and BEL":    {"ab\bc\x07", "abc"},
		"invalid UTF-8":        {"a\xffb", "a\ufffdb"},
		"non-ASCII kept":       {"zażółć", "zażółć"},
	}
	for name, tc := range cases {
		if got := clean(tc.in, 100); got != tc.want {
			t.Errorf("%s: clean(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}

func TestCleanCuts(t *testing.T) {
	if got := clean("abcdefghij", 6); got != "abc..." {
		t.Errorf("clean cut = %q, want %q", got, "abc...")
	}
	if got := clean("abcdef", 6); got != "abcdef" {
		t.Errorf("clean at the bound = %q, want it whole", got)
	}
	if got := clean("ab\x1b[31mcdef", 6); got != "abcdef" {
		t.Errorf("clean with an escape at the bound = %q, want %q", got, "abcdef")
	}
}

// FuzzClean checks that no input yields a control, format or separator character, invalid UTF-8 or more runes
// than the bound.
func FuzzClean(f *testing.F) {
	for _, s := range []string{"\x1b[31mred", "\x1b]8;;u\x1b\\t", "\u009b1m", "\u202e", "a\xff", "\x1bP\x1b\\"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := clean(s, 64)
		if !utf8.ValidString(out) {
			t.Fatalf("clean(%q) = %q is not valid UTF-8", s, out)
		}
		if n := utf8.RuneCountInString(out); n > 64 {
			t.Fatalf("clean(%q) has %d runes", s, n)
		}
		for _, r := range out {
			if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
				t.Fatalf("clean(%q) = %q holds %U", s, out, r)
			}
		}
	})
}
