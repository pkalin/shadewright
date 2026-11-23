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
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"
)

// The shown view writes each run of invalid bytes as one "?" and leaves the controls out, as the text is shown, and
// a match that starts at a "?" starts at its run, one that ends with it ends after its run.
func TestShownViewWritesARunAsOneQuestionMark(t *testing.T) {
	s := "a\xff\xfeb\x00c\xed\xa0\x80"
	v, ok := shownViewOf(nil, s)
	if !ok || v.text != "a?bc?" {
		t.Fatalf("shownViewOf(%q) = %q, %v; want %q", s, v.text, ok, "a?bc?")
	}
	for _, c := range []struct{ start, end, origStart, origEnd int }{
		{0, 1, 0, 1}, {1, 2, 1, 3}, {0, 2, 0, 3}, {2, 3, 3, 4}, {1, 4, 1, 6}, {4, 5, 6, 9}, {0, 5, 0, 9},
	} {
		if got, gotEnd := v.orig(nil, c.start), v.origEnd(nil, c.end); got != c.origStart || gotEnd != c.origEnd {
			t.Errorf("view [%d, %d) is [%d, %d) of the text, want [%d, %d)", c.start, c.end, got, gotEnd, c.origStart, c.origEnd)
		}
	}
	if _, ok := shownViewOf(nil, "valid é text\x00"); ok {
		t.Error("valid UTF-8 has a shown view")
	}
}

// Every range of the shown view of generated text, invalid bytes, controls and multi-byte characters mixed, is
// carried back to a range of the text that starts and ends at a character or an invalid byte, never inside a
// multi-byte sequence, holds the bytes the view's range was read from and stays inside the text.
func TestShownViewRangesLandOnCharacters(t *testing.T) {
	units := []string{"a", "=", "\xff", "\x80", "\xc0", "\xe2\x82", "\xed\xa0\x80", "\x00", "\x1b", "\u0085", "é", "€", "\U0001f600", "\n"}
	rng := rand.New(rand.NewPCG(1100, 23))
	boundary := func(s string, i int) bool {
		return i == 0 || i == len(s) || utf8.RuneStart(s[i]) || invalidAt(s, i)
	}
	for range 300 {
		var b strings.Builder
		for range 1 + rng.IntN(12) {
			b.WriteString(units[rng.IntN(len(units))])
		}
		b.WriteString("\xff") // at least one invalid byte
		s := b.String()
		v, ok := shownViewOf(nil, s)
		if !ok {
			t.Fatalf("%q has no shown view", s)
		}
		for i := 0; i < len(v.text); i++ {
			for j := i + 1; j <= len(v.text); j++ {
				if !utf8.RuneStart(v.text[i]) || j < len(v.text) && !utf8.RuneStart(v.text[j]) {
					continue
				}
				start, end := v.orig(nil, i), v.origEnd(nil, j)
				if start < 0 || end > len(s) || start >= end || !boundary(s, start) || !boundary(s, end) {
					t.Fatalf("%q: view [%d, %d) of %q is [%d, %d) of the text", s, i, j, v.text, start, end)
				}
				if got := WithoutControls(strings.ToValidUTF8(s[start:end], "?")); !strings.Contains(got, strings.Trim(v.text[i:j], "?")) {
					t.Fatalf("%q: view [%d, %d) %q is carried back to %q", s, i, j, v.text[i:j], s[start:end])
				}
			}
		}
	}
}

// The shown view only adds: on generated text with invalid bytes around secrets, every byte the readings of the
// text as it is hide is hidden with the views added, and no match of theirs is shortened.
func TestShownViewOnlyAdds(t *testing.T) {
	units := []string{"password", "=", ": ", "token", "api_key", "\n", " ", "Hunter2Secret9x", "\xff", "\x80\xee", "\xed\xa0\x80", "\x00", "ghp_"}
	rng := rand.New(rand.NewPCG(1100, 24))
	r := defaultRedactor
	for range 400 {
		var b strings.Builder
		for range 2 + rng.IntN(10) {
			b.WriteString(units[rng.IntN(len(units))])
		}
		s := b.String()
		before := settle(nil, s, r.mainMatches(nil, s).spans(nil))
		after := r.spans(nil, s)
		hidden := func(sp []span) []bool {
			h := make([]bool, len(s))
			for _, m := range sp {
				for k := m.start; k < m.end; k++ {
					h[k] = true
				}
			}
			return h
		}
		hb, ha := hidden(before), hidden(after)
		for k := range hb {
			if hb[k] && !ha[k] {
				t.Fatalf("%q: byte %d is hidden without the views and shown with them", s, k)
			}
		}
	}
}
