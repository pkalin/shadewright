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

package termtext

import (
	"math/rand/v2"
	"strings"
	"testing"
	"unicode"
)

// A negative bound keeps the whole text, as a bound above its length does, and Compact is that text
// without whitespace.
func TestUnboundedAndCompact(t *testing.T) {
	pieces := []string{"a", "Z", " ", "\t", "\n", "\x1b[0m", "\x1b]0;t\x07", "\u009b1m", "\u009dx\u009c", "\x1b(B",
		"\x1b", "\u200b", "\u00a0", "\u2028", "\xff", "é", "\x00", "\x1bPq\x1b\\", "["}
	r := rand.New(rand.NewPCG(1, 2)) // #nosec G404 -- reproducible corpus
	for range 5000 {
		var b strings.Builder
		for range r.IntN(12) {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		s := b.String()
		whole := Clean(s, -1)
		if got := Clean(s, len(s)+1); got != whole {
			t.Fatalf("Clean(%q, -1) = %q, with a bound above its length %q", s, whole, got)
		}
		want := strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, whole)
		if got := Compact(s); got != want {
			t.Fatalf("Compact(%q) = %q, want %q", s, got, want)
		}
	}
	if got := Compact("sk-\x1b[0mabc\t d"); got != "sk-abcd" {
		t.Fatalf("Compact = %q", got)
	}
}

// EscapeEnd skips exactly what Clean skips: each whole sequence, and nothing where none starts.
func TestEscapeEnd(t *testing.T) {
	for _, seq := range []string{"\x1b[0m", "\x1b[38;5;196m", "\x1b]0;t\x07", "\x1b]8;;https://e.test/\x1b\\", "\u009b0m", "\u009d0;t\u009c", "\x1bPq#0\x1b\\", "\x1b(B", "\x1b7"} {
		end, ok := EscapeEnd(seq+"b", 0)
		if !ok || end != len(seq) {
			t.Errorf("EscapeEnd(%q) = %d, %v", seq, end, ok)
		}
		if Clean("a"+seq+"b", -1) != "ab" {
			t.Errorf("Clean does not skip %q", seq)
		}
	}
	for _, s := range []string{"a", "[0m", "é", " "} {
		if end, ok := EscapeEnd(s, 0); ok || end != 0 {
			t.Errorf("EscapeEnd(%q) = %d, %v", s, end, ok)
		}
	}
}
