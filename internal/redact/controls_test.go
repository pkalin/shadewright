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
	"unicode"
)

// controlParts are the parts the generated texts of the controls view are made of: every code point up to U+009F
// (each of Control, the two kept controls, ASCII), invalid bytes (the lead byte of C1 alone among them; no part is
// a continuation byte of C1, so no two parts join into a control), and characters of one to four bytes.
func controlParts() []string {
	var parts []string
	for r := rune(0); r <= 0x9f; r++ {
		parts = append(parts, string(r))
	}
	return append(parts, "\xc2", "\xbf", "\xc3", "\xa9", "\xff", "é", "€", "\U0001F600", "a", " ", ":")
}

// Control is Unicode category Cc but newline and tab, and the controls view of a generated text, whose answer is
// known by construction (its parts with each code point of Control left out), is that text: each of its bytes
// stands for the byte of the input it was copied from, and WithoutControls returns the same text. An invalid byte
// is kept, so "\xc2", a control and "\xbf" stay two invalid bytes.
func TestControlsViewIsTheTextWithoutControls(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if want := unicode.Is(unicode.Cc, r) && r != '\n' && r != '\t'; Control(r) != want {
			t.Fatalf("Control(%U) = %v, want %v", r, !want, want)
		}
	}
	parts := controlParts()
	rng := rand.New(rand.NewPCG(852, 2)) // #nosec G404 -- a reproducible test corpus
	for range 5000 {
		var in, want strings.Builder
		for range rng.IntN(12) {
			p := parts[rng.IntN(len(parts))]
			in.WriteString(p)
			if r := []rune(p); len(p) == 1 && p[0] < 0x80 && Control(r[0]) || len(p) == 2 && r[0] >= 0x80 && r[0] <= 0x9f {
				continue
			}
			want.WriteString(p)
		}
		s := in.String()
		if got := WithoutControls(s); got != want.String() {
			t.Fatalf("WithoutControls(%q) = %q, want %q", s, got, want.String())
		}
		v, ok := controlsViewOf(nil, s)
		if ok != (s != want.String()) {
			t.Fatalf("controlsViewOf(%q) reports %v", s, ok)
		}
		if !ok {
			continue
		}
		if v.text != want.String() {
			t.Fatalf("controlsViewOf(%q) = %q, want %q", s, v.text, want.String())
		}
		for i := range len(v.text) {
			if o := v.orig(nil, i); s[o] != v.text[i] || i > 0 && o <= v.orig(nil, i-1) {
				t.Fatalf("controlsViewOf(%q): byte %d stands for byte %d", s, i, o)
			}
		}
	}
}

// A match in the controls view counts on the bytes of the text it was read from, the controls inside it included:
// a token with a NUL in its body is hidden whole by StringShownChecked and StringTwiceShownChecked, and nothing around it.
func TestControlsViewMatchesCountOnTheText(t *testing.T) {
	body := strings.Repeat("Hd4kQ", 7) + "w"
	in := "before ghp_" + body[:10] + "\x00" + body[10:] + " after"
	shown, _ := StringShownChecked(in)
	twice, _ := defaultRedactor.StringTwiceShownChecked(in, nil)
	for name, got := range map[string]string{
		"StringShownChecked":      shown,
		"StringTwiceShownChecked": twice,
	} {
		if strings.Contains(WithoutControls(got), body[:10]) || strings.Contains(got, body[10:]) {
			t.Errorf("%s(%q) = %q, shows the token", name, in, got)
		}
		if !strings.HasPrefix(got, "before ") || !strings.HasSuffix(got, " after") {
			t.Errorf("%s(%q) = %q, lost the text around the token", name, in, got)
		}
	}
}
