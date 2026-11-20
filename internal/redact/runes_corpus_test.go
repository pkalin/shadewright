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
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// Spans on character boundaries (#845, P4). A fuzz input found a value the extent rule cut at its byte bound
// (maxValueExtent) inside a zero-width space: the marker took its first byte and the output kept the other two,
// which is not valid UTF-8. The release before has the same bound, so its reading did the same. Every span a
// reading returns starts and ends on a character boundary of the text when the text is valid UTF-8; this file
// checks that over the generated corpus and over a corpus of multi-byte characters put directly before, inside
// and after keys, quotes and values, and across the bound.

// runeChars are the multi-byte characters of the corpus: invisible ones the folded view drops, look-alikes it
// folds, and visible ones of two, three and four bytes.
var runeChars = []string{
	"\u200b", "\ufeff", "\u2060", "\u00a0", "\u2028", "\uff1a", "\u00e9", "\u4e2d", "\U0001f600", "\U00010348",
}

// runeInvisible are the characters of runeChars the folded view drops inside a key.
var runeInvisible = []string{"\u200b", "\ufeff", "\u2060"}

// runeKeys are the keys of the corpus, as a key with a colon, an environment name with "=" and a flag.
var runeKeys = []struct{ key, sep string }{
	{passwordKey, ": "}, {clientSecretKey, ": "}, {"TOKEN", "="}, {dbPasswordKey, "="}, {canonFlagKeys[0], " "},
	{"--api-key", "="}, {`"token"`, ": "},
}

// runeBoundChars are the characters the bound falls inside: one of two, three and four bytes, and a look-alike.
var runeBoundChars = []string{"\u00e9", "\u200b", "\U0001f600", "\uff1a"}

// runeBoundChar is 1 when the corpus runs c across the bound, 0 otherwise.
func runeBoundChar(c string) int {
	if slices.Contains(runeBoundChars, c) {
		return 1
	}
	return 0
}

// runeBoundLeads are the text between a key's separator and a value that runs past the bound.
var runeBoundLeads = []string{`"`, ``, runeShellLead, `'`, `[`}

// runeShellLead is the lead of the fuzz input: a quoted string glued to a word, one shell word.
const runeShellLead = `"a",b,`

// runeNeighbourSeps are the separators between a value and a second key after it.
var runeNeighbourSeps = []string{",", ";", " ", "&", "\t", `",`, `" `}

// runeCorpus returns the inputs of the multi-byte corpus, each with its secrets and kept text by construction.
func runeCorpus() []canonCase {
	g := &canonGen{n: 9000}
	const form = "multi-byte character"
	for ki, k := range runeKeys {
		for ci, c := range runeChars {
			name := fmt.Sprintf("key %s, char %q", k.key, c)
			g.add(form, name+", before the key", func(w *canonWriter) {
				w.text("x" + c)
				w.kept(k.key)
				w.text(k.sep)
				w.secret()
				w.text("\n")
				w.kept("visible")
			})
			g.add(form, name+", glued before the value", func(w *canonWriter) {
				w.kept(k.key)
				w.text(k.sep + c)
				w.secret()
				w.text("\n")
				w.kept("visible")
			})
			g.add(form, name+", inside the value", func(w *canonWriter) {
				w.kept(k.key)
				w.text(k.sep)
				w.secret()
				w.text(c)
				w.secret()
				w.text("\n")
				w.kept("visible")
			})
			g.add(form, name+", after the value", func(w *canonWriter) {
				w.kept(k.key)
				w.text(k.sep)
				w.secret()
				w.text(c + " \n")
				w.kept("visible")
			})
			g.add(form, name+", in double quotes", func(w *canonWriter) {
				w.kept(k.key)
				w.text(k.sep + `"` + c)
				w.secret()
				w.text(c + `" `)
				w.kept("after")
			})
			g.add(form, name+", in single quotes", func(w *canonWriter) {
				w.kept(k.key)
				w.text(k.sep + "'" + c)
				w.secret()
				w.text(c + "' ")
				w.kept("after")
			})
			g.add(form, name+", in JSON", func(w *canonWriter) {
				w.text(`{`)
				w.kept(`"password"`)
				w.text(`:"` + c)
				w.secret()
				w.text(c + `",`)
				w.kept(`"user"`)
				w.text(`:"`)
				w.kept("bob")
				w.text(`"}`)
			})
			sep := runeNeighbourSeps[(ki+ci)%len(runeNeighbourSeps)]
			g.add(form, fmt.Sprintf("%s, a second key after %q", name, sep), func(w *canonWriter) {
				w.kept(k.key)
				w.text(k.sep)
				w.secret()
				w.text(c + sep + "token=")
				w.secret()
				w.text("\n")
				w.kept("visible")
			})
			// The bound: a value that does not end within maxValueExtent bytes (an open quote, a word, a quoted
			// string glued to a word as in the fuzz input), padded so that the bound falls on each byte of the
			// character in turn.
			for off := range len(c) * len(runeBoundLeads) * runeBoundChar(c) {
				lead := runeBoundLeads[off/len(c)]
				if lead == runeShellLead && k.sep != "=" {
					// Only a shell word glues a quoted string to the text after it; after a colon "a" is the value.
					continue
				}
				g.add(form, fmt.Sprintf("%s, lead %q, the bound at byte %d of the character", name, lead, off%len(c)), func(w *canonWriter) {
					w.kept(k.key)
					w.text(k.sep + lead)
					start := w.b.Len()
					w.secret()
					pad := maxValueExtent - (w.b.Len() - start) - off
					for pad%len(c) != 0 {
						w.text("a")
						pad--
					}
					w.text(strings.Repeat(c, pad/len(c)+64))
				})
			}
		}
		for _, c := range runeInvisible {
			if k.key[0] == '"' || k.key[0] == '-' {
				continue
			}
			for at := 1; at < len(k.key); at += 3 {
				g.add(form, fmt.Sprintf("key %s, char %q inside the key at %d", k.key, c, at), func(w *canonWriter) {
					w.kept(k.key[:at])
					w.text(c)
					w.kept(k.key[at:])
					w.text(k.sep)
					w.secret()
					w.text(c + "\n")
					w.kept("visible")
				})
			}
			g.add(form, fmt.Sprintf("key %s, char %q before the separator", k.key, c), func(w *canonWriter) {
				w.kept(k.key)
				w.text(c + k.sep)
				w.secret()
				w.text("\n")
				w.kept("visible")
			})
		}
	}
	return g.out
}

// offBoundaries returns the spans of sp that start or end inside a character of s.
func offBoundaries(s string, sp []span) []span {
	var out []span
	for _, x := range sp {
		if (x.start < len(s) && !utf8.RuneStart(s[x.start])) || (x.end < len(s) && !utf8.RuneStart(s[x.end])) {
			out = append(out, x)
		}
	}
	return out
}

// runeReadings are the readings whose spans the entry points join, by name.
var runeReadings = []struct {
	name string
	of   func(s string) []span
}{
	{"rules", func(s string) []span { return defaultRedactor.ruleMatches(nil, s) }},
	{"release, once", func(s string) []span { return defaultRedactor.releasedString(nil, s, false, nil) }},
	// Twice and cut holds what twice hides, and String is settle of the first two: neither is read again.
	{"release, twice and cut", func(s string) []span {
		return defaultRedactor.releasedString(nil, s, true, func(t string) int { return len(t) / 3 })
	}},
	{"release, excerpt", func(s string) []span {
		return defaultRedactor.releasedExcerpt(nil, s, len(s)/4, func(t string) int { return len(t) / 5 })
	}},
	{"release, pieces", func(s string) []span {
		h := len(s) / 2
		for h > 0 && !utf8.RuneStart(s[h]) {
			h--
		}
		return defaultRedactor.releasedPieces(nil, []Piece{{Text: s[:h], Data: true, Form: Line}, {Text: s[h:], Data: true, Form: JSON}})
	}},
}

// runeEntryOutputs are the outputs of the entry points, by name, for the UTF-8 check.
var runeEntryOutputs = []struct {
	name string
	of   func(s string) []string
}{
	{"String", func(s string) []string { return []string{String(s)} }},
	{"StringTwice", func(s string) []string { return []string{StringTwice(s, func(t string) int { return len(t) / 3 })} }},
	{"Strings", func(s string) []string { return Strings(strings.Split(s, "\n")) }},
	{"ReplaceFrom", func(s string) []string {
		out, _ := defaultRedactor.ReplaceFrom(s, 0, func(t string) int { return len(t) / 5 },
			func(kind, text string) string { return Marker(kind, len(text)) },
			func(t string) string { return t })
		return []string{out}
	}},
	{"Pieces", func(s string) []string { return defaultRedactor.Pieces([]Piece{{Text: s, Data: true, Form: JSON}}) }},
}

// checkRunes checks the spans of every reading and, with outputs, the output of every entry point on in, when in
// is valid UTF-8 (bytes that are not valid UTF-8 may pass through, as the release before let them).
func checkRunes(t *testing.T, name, in string, outputs bool) {
	t.Helper()
	if !utf8.ValidString(in) {
		return
	}
	for _, r := range runeReadings {
		if bad := offBoundaries(in, r.of(in)); len(bad) > 0 {
			x := bad[0]
			t.Errorf("%s: %s returns span %d-%d inside a character: %q", name, r.name, x.start, x.end,
				in[max(0, x.start-8):min(len(in), x.end+8)])
		}
	}
	if bad := offBoundaries(in, defaultRedactor.spans(nil, in)); len(bad) > 0 {
		t.Errorf("%s: String hides %d-%d, inside a character", name, bad[0].start, bad[0].end)
	}
	for _, e := range runeEntryOutputs {
		if !outputs {
			break
		}
		for _, out := range e.of(in) {
			if !utf8.ValidString(out) {
				t.Errorf("%s: %s is not valid UTF-8", name, e.name)
			}
		}
	}
}

// runeOutputsDivisor is the share of the canonical corpus whose entry point outputs are checked too: one input
// in that many (every input's spans are checked, and the outputs are written from them).
const runeOutputsDivisor = 10

// Every span of every reading starts and ends on a character boundary on the generated canonical corpus, and
// the entry points write valid UTF-8 on a share of it.
func TestSpansOnRuneBoundariesCanonCorpus(t *testing.T) {
	// A corpus test that counts no work: it runs beside the others (#873).
	t.Parallel()
	cases, _ := canonSample(canonCorpus())
	for i, c := range cases {
		checkRunes(t, c.name, c.in, i%runeOutputsDivisor == 0)
	}
}

// The multi-byte corpus: spans on character boundaries, valid UTF-8 out of every entry point, each secret hidden
// by String and StringTwice, each kept piece kept by String, and each byte the release's String hides still
// hidden by String.
func TestSpansOnRuneBoundariesMultiByteCorpus(t *testing.T) {
	// A corpus test that counts no work: it runs beside the others (#873).
	t.Parallel()
	cases, sampled := canonSample(runeCorpus())
	bound := 0
	for _, c := range cases {
		if strings.Contains(c.name, "the bound") {
			bound++
		}
		if !utf8.ValidString(c.in) {
			t.Fatalf("%s: the generator wrote invalid UTF-8", c.name)
		}
		checkRunes(t, c.name, c.in, true)
		now := defaultRedactor.spans(nil, c.in)
		red := make([]bool, len(c.in))
		for _, sp := range now {
			for i := sp.start; i < sp.end; i++ {
				red[i] = true
			}
		}
		for _, r := range c.secrets {
			for i := r[0]; i < r[1]; i++ {
				if !red[i] {
					t.Errorf("%s: secret %q keeps byte %d: %q", c.name, c.in[r[0]:r[1]], i, String(c.in))
					break
				}
			}
			if twice := StringTwice(c.in, nil); strings.Contains(twice, c.in[r[0]:r[1]]) {
				t.Errorf("%s: StringTwice shows secret %q", c.name, c.in[r[0]:r[1]])
			}
		}
		for _, r := range c.keep {
			for i := r[0]; i < r[1]; i++ {
				if red[i] {
					t.Errorf("%s: kept %q loses byte %d: %q", c.name, c.in[r[0]:r[1]], i, String(c.in))
					break
				}
			}
		}
		for _, f := range defaultRedactor.before.Spans(c.in) {
			for i := f.Start; i < f.End; i++ {
				if !red[i] {
					t.Errorf("%s: the release hid byte %d, String keeps it", c.name, i)
					break
				}
			}
		}
	}
	if !sampled && (len(cases) < 500 || bound < 100) {
		t.Fatalf("corpus too small: %d inputs, %d across the bound", len(cases), bound)
	}
	t.Logf("%d inputs, %d across the bound", len(cases), bound)
}

// The widening only grows a span, to the character it cuts, and leaves spans on boundaries and invalid bytes as
// they are.
func TestOnRunesWidensToTheCharacter(t *testing.T) {
	s := "a\u200bb\xe2\x80c\U0001f600"
	cases := []struct{ in, want [2]int }{
		{[2]int{0, 2}, [2]int{0, 4}},   // ends inside the zero-width space
		{[2]int{2, 4}, [2]int{1, 4}},   // starts inside it
		{[2]int{1, 4}, [2]int{1, 4}},   // on boundaries
		{[2]int{5, 6}, [2]int{5, 6}},   // inside invalid bytes: nothing to take
		{[2]int{9, 11}, [2]int{8, 12}}, // both ends inside the emoji
		{[2]int{0, 12}, [2]int{0, 12}},
	}
	for _, c := range cases {
		got := onRunes(nil, s, []span{{start: c.in[0], end: c.in[1]}})[0]
		if got.start != c.want[0] || got.end != c.want[1] {
			t.Errorf("onRunes(%v) = %d-%d, want %v", c.in, got.start, got.end, c.want)
		}
	}
}
