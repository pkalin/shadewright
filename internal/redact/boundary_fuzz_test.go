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
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"
)

// A replacement begins and ends on a character boundary of the original: valid UTF-8 in gives valid UTF-8 out
// (#845, third decision of 2026-11-18, property 4). The folded view maps each of its bytes back to the bytes of
// the original it stands for; a span that ended on a look-alike colon folded to ":" ended inside the colon's
// three bytes, because the separator fold copied that byte as if it were its own original
// ("password：\u200b<<：" and "--token=\u00ad\r=：" gave invalid UTF-8). The fuzz target checks String, Strings
// and Pieces on any input; the test below runs its check on a seeded sample of inputs built from the tokens of
// the rules, so the normal test run covers it.

// boundaryTokens are the pieces the sample builds inputs from: secret keys and flags, separators and their
// look-alikes, invisible characters, brackets, quotes, block markers, line ends and characters of two to four
// bytes.
var boundaryTokens = []string{
	passwordKey, "--" + canonTokenKey, "api_key", "secret", "auth", "X-Api-Key", passKey,
	":", "=", "::", "：", "꞉", "∶", "∷", "＝", "->", "=>", "→", "⇒", "ː",
	"\u200b", "\u00ad", "\u2060", "\ufeff", "\u180e", "\u034f", "\U000e0001",
	" ", "\t", "\u00a0", "\u3000", "\r", "\n", "\r\n", "\u0085", "\u2028",
	"{", "}", "[", "]", "\"", "'", "\\", "|", ">", "-", "#", "?", ",", "<<", "@", "/",
	"x", "V9q", "é", "日本", "😀", "Ω",
}

// boundaryInput returns a generated input of up to n tokens.
func boundaryInput(r *rand.Rand, n int) string {
	var b strings.Builder
	for range 1 + r.IntN(n) {
		b.WriteString(boundaryTokens[r.IntN(len(boundaryTokens))])
	}
	return b.String()
}

// checkBoundaries returns what breaks property 4 for in, or "".
func checkBoundaries(in string) string {
	if !utf8.ValidString(in) {
		return ""
	}
	if out := String(in); !utf8.ValidString(out) {
		return "String gave invalid UTF-8 " + strqq(out)
	}
	items := strings.Split(in, "\n")
	for _, out := range Strings(items) {
		if !utf8.ValidString(out) {
			return "Strings gave invalid UTF-8 " + strqq(out)
		}
	}
	ps := make([]Piece, 0, 2*len(items))
	for _, it := range items {
		ps = append(ps, Piece{Text: "- name: "}, Piece{Text: it, Data: true}, Piece{Text: "\n"})
	}
	for _, out := range Pieces(ps) {
		if !utf8.ValidString(out) {
			return "Pieces gave invalid UTF-8 " + strqq(out)
		}
	}
	// Values that keep their line breaks (#845, sixth decision of 2026-11-18, point 6): the input cut at each
	// comma, each part one value written in each form, under a key and after one, as a tool writes them. In JSON
	// the answer must stay a valid JSON string: a replacement takes an escape whole or not at all.
	parts := strings.Split(in, ",")
	for _, f := range []Form{AsIs, Line, JSON} {
		ps := make([]Piece, 0, 3*len(parts))
		for i, it := range parts {
			ps = append(ps, Piece{Text: `"`}, Piece{Text: it, Data: true, Form: f, Key: []string{"", "note", passwordKey}[i%3]}, Piece{Text: "\"\n"})
		}
		out := Pieces(ps)
		for i, o := range out {
			if !utf8.ValidString(o) {
				return fmt.Sprintf("Pieces in form %d gave invalid UTF-8 %s", f, strqq(o))
			}
			if f == JSON && ps[i].Data && !json.Valid([]byte(`"`+o+`"`)) {
				return fmt.Sprintf("Pieces in JSON gave no JSON string %s", strqq(o))
			}
		}
	}
	return ""
}

func strqq(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "\n", `\n`), "\r", `\r`) }

// boundarySeeds are the inputs found to break property 4 on 38cf1a37, a few of their neighbours, and values
// with line breaks, quotes and backslashes that JSON escapes.
var boundarySeeds = []string{
	"password：\u200b<<：", "--token=\u00ad\r=：", "password：：", "token=\u2060：x", "auth: ∷\u200bV9q", "：：",
	"token: expired\nretrying in 5s", "password: \"V9q\\\"x\"\r\n  host", "|\n  block,other: keepme", "token=\tV9q\u2028x",
}

func FuzzReplacementBoundaries(f *testing.F) {
	for _, s := range boundarySeeds {
		f.Add(s)
	}
	r := rand.New(rand.NewPCG(845, 4)) // #nosec G404 -- seeds from a fixed seed, not a secret
	for range 64 {
		f.Add(boundaryInput(r, 12))
	}
	f.Fuzz(func(t *testing.T, in string) {
		if msg := checkBoundaries(in); msg != "" {
			t.Fatalf("%q: %s", in, msg)
		}
	})
}

// boundarySample is the size of the seeded sample TestReplacementsEndOnCharacterBoundaries checks.
const boundarySample = 20000

func TestReplacementsEndOnCharacterBoundaries(t *testing.T) {
	// A corpus test that counts no work: it runs beside the others (#873).
	t.Parallel()
	r := rand.New(rand.NewPCG(845, 3)) // #nosec G404 -- test data from a fixed seed, not a secret
	inputs := append([]string(nil), boundarySeeds...)
	for range boundarySample {
		inputs = append(inputs, boundaryInput(r, 12))
	}
	failed := 0
	for i, in := range inputs {
		if !raceKeeps(t.Name(), i) {
			continue
		}
		if msg := checkBoundaries(in); msg != "" {
			if failed++; failed <= 10 {
				t.Errorf("%q: %s", in, msg)
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d generated inputs give invalid UTF-8", failed, len(inputs))
	}
}

// A look-alike colon folds to ":" for keys and separators only: two of them are no IPv6 address, which is read
// as written (#845, third decision of 2026-11-18; "：：" alone became "[ip]").
func TestFoldedColonsAreNoAddress(t *testing.T) {
	for in, want := range map[string]string{
		"：：": "：：", "a ：： b": "a ：： b", "fe80：：1": "fe80：：1", "∷": "∷",
		"::": "**", "x ::1 y": "x *** y", "fe80::1": "[ip]",
	} {
		if got := String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
}
