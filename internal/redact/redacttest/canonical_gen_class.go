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

package redacttest

import (
	"fmt"
	"strings"
)

// The forms of the fourth round of #845 (decision of 2026-11-18): separators and invisible characters drawn
// from their classes, not from the forms found so far. A separator after a secret key is a short run of
// characters that are not letters or digits (blanks, control and invisible characters, punctuation) holding a
// colon or an equals sign, plain or a look-alike, or an arrow; an invisible character is one Unicode marks as
// default ignorable, or one of category Cf. The generator is independent of the redactor. Its cases come after every earlier form, so
// the inputs before them keep their text and their hashes.

// canonFormSeparator is a secret key followed by a separator of other characters than one colon or equals
// sign between blanks: doubled or mixed separators, an arrow, controls and invisible characters in the run.
const canonFormSeparator = "separator of other characters"

// canonFormAutolink is a URL with a user and a password in angle brackets, closed before the "@" of a second
// link after it: the brackets of a list of links, around a URL whose password stands where a port would.
const canonFormAutolink = "user info in angle brackets"

// canonInvisibles are characters of the invisible class: the first and the last assigned code point of each
// range of Default_Ignorable_Code_Point (DerivedCoreProperties.txt) and of category Cf
// (DerivedGeneralCategory.txt), Unicode 18.0.0. Written here from the data, not from the rule.
var canonInvisibles = []string{
	"\u00ad", "\u034f", "\u0600", "\u0605", "\u061c", "\u06dd", "\u070f", "\u0890", "\u0891", "\u08e2", "\u115f",
	"\u1160", "\u17b4", "\u17b5", "\u180b", "\u180d", "\u180e", "\u180f", "\u200b", "\u200f", "\u202a", "\u202e",
	"\u2060", "\u2064", "\u2066", "\u206f", "\u3164", "\ufe00", "\ufe0f", "\ufeff", "\uffa0", "\ufff9", "\ufffb",
	"\U000110bd", "\U000110cd", "\U00013430", "\U0001343f", "\U0001bca0", "\U0001bca3", "\U0001d173",
	"\U0001d17a", "\U000e0001", "\U000e0020", "\U000e007f", "\U000e0100", "\U000e01ef",
}

// canonSepBlanks are blanks a separator run may hold: ASCII and Unicode spaces.
var canonSepBlanks = []string{" ", "\t", "\u00a0", "\u3000", "\u2009", "\u202f"}

// canonSepControls are control characters a separator run may hold.
var canonSepControls = []string{"\x00", "\v", "\f", "\x1b", "\x7f", "\x01"}

// canonSepMarks are the characters that make a run a separator: a colon, an equals sign, an arrow, and the
// colon look-alikes of both earlier rounds.
var canonSepMarks = append([]string{":", "=", "->", "=>", ":=", "::"}, canonAllColons...)

// canonSepFound are the separators reviews found leaking on c5daa6f8, kept so that each stays in the corpus
// whatever the seeded runs draw.
var canonSepFound = []string{
	"::", ":::", "==", "=>", "->", "::=", "\uff1a\uff1a", "\u2237\u2237", "\u02d0\u02d0", "\x00:", "\v:",
	"\u00a0:", "\u3000:", "\u200b :", ":\u200b", ":\x00", ":\u3000", ":\u00a0", "\u200b\t:", " \u200b:",
}

// canonSepKeys are the keys of the separator form: the glyph keys and the LDIF attribute.
var canonSepKeys = append([]string{"userPassword"}, canonGlyphKeys...)

// sepRun draws one separator run from the classes: one to five pieces, at least one of them a mark, at most
// one of them a blank at its end (a blank after the run is written by the shape).
func (g *canonGen) sepRun() string {
	var b strings.Builder
	n := 1 + g.rng.IntN(5)
	mark := g.rng.IntN(n)
	for i := range n {
		switch c := g.rng.IntN(4); {
		case i == mark || c == 0:
			b.WriteString(canonPick(g.rng, canonSepMarks))
		case c == 1:
			b.WriteString(canonPick(g.rng, canonSepBlanks))
		case c == 2:
			b.WriteString(canonPick(g.rng, canonSepControls))
		default:
			b.WriteString(canonPick(g.rng, canonInvisibles))
		}
	}
	return b.String()
}

// sepRunPlain reports whether run is one ":", "=" or ":=" with ASCII blanks around it: the separator every
// rule has always read, which the class must not need.
func sepRunPlain(run string) bool {
	t := strings.Trim(run, " \t")
	return t == ":" || t == "=" || t == ":="
}

// foldRoundFourCases writes the forms of the fourth round.
func (g *canonGen) foldRoundFourCases() {
	g.separatorCases()
	g.invisibleKeyCases()
	g.sepSequenceCases()
	g.autolinkCases()
}

// autolinkCases writes "<scheme://user:secret>", closed by ">,<" or ">;<", and a second link with an "@" after
// it, at the start of a line and inside prose.
func (g *canonGen) autolinkCases() {
	schemes := []string{"postgres", "postgresql", "mysql", "mongodb+srv", "redis", "amqps", "https", "sftp"}
	// The third is an address with a domain, built from parts so that no line of the source holds one.
	seconds := []string{"x@db/app>", "mailto:ops@db>", "ops" + "@" + "db.example>", "u@h>"}
	for si, scheme := range schemes {
		for ci, closer := range []string{">,<", ">;<"} {
			for vi, second := range seconds {
				nl := canonNLs[(si+ci+vi)%len(canonNLs)]
				user := []string{"alice", "app"}[(si+vi)%2]
				for v := range 2 {
					g.add(canonFormAutolink, fmt.Sprintf("%s, %q, %q, shape %d, line end %q", scheme, closer, second, v, nl), func(w *CanonWriter) {
						if v == 1 {
							w.Kept("see")
							w.Text(" ")
						}
						w.Text("<" + scheme + "://" + user + ":")
						w.Secret()
						w.Text(closer + second)
						if v == 1 {
							w.Text(" ")
							w.Kept("and retry")
						}
						w.next(nl)
					})
				}
			}
		}
	}
}

// separatorCases writes a secret key, a separator run (canonSepFound, then runs drawn from the classes) and a
// secret: after a blank, glued, quoted, in JSON with the key quoted, and as the value of a list entry.
func (g *canonGen) separatorCases() {
	runs := append([]string{}, canonSepFound...)
	for len(runs) < 160 {
		if r := g.sepRun(); !sepRunPlain(r) {
			runs = append(runs, r)
		}
	}
	for ri, run := range runs {
		key := canonSepKeys[ri%len(canonSepKeys)]
		for v := range 5 {
			nl := canonNLs[(ri+v)%len(canonNLs)]
			g.add(canonFormSeparator, fmt.Sprintf("key %s, separator %q, shape %d, line end %q", key, run, v, nl), func(w *CanonWriter) {
				switch v {
				case 0:
					w.Kept(key)
					w.Text(run + " ")
					w.Secret()
				case 1:
					w.Kept(key)
					w.Text(run)
					w.Secret()
				case 2:
					w.Kept(key)
					w.Text(run + ` "`)
					w.Secret()
					w.Text(`"`)
				case 3:
					w.Text(`{"`)
					w.Kept(key)
					w.Text(`"` + run + ` "`)
					w.Secret()
					w.Text(`"}`)
				case 4:
					w.Text("- ")
					w.Kept(key)
					w.Text(run + " ")
					w.Secret()
				}
				w.next(nl)
			})
		}
	}
}

// invisibleKeyCases writes a secret key with an invisible character inside it, or between it and a blank
// before its colon, or after its closing quote.
func (g *canonGen) invisibleKeyCases() {
	for ki, key := range canonGlyphKeys {
		for ci, x := range canonInvisibles {
			at := 1 + (ki+ci)%(len(key)-1)
			for v := range 5 {
				nl := canonNLs[(ki+ci+v)%len(canonNLs)]
				g.add(canonFormGlyph, fmt.Sprintf("key %s, invisible %q, shape %d, line end %q", key, x, v, nl), func(w *CanonWriter) {
					switch v {
					case 0:
						w.Kept(key[:at])
						w.Text(x)
						w.Kept(key[at:])
						w.Text(": ")
					case 1:
						w.Kept(key[:at])
						w.Text(x)
						w.Kept(key[at:])
						w.Text("=")
					case 2:
						w.Kept(key)
						w.Text(x + " : ")
					case 3:
						w.Kept(key)
						w.Text(" " + x + ": ")
					case 4:
						w.Text(`{"`)
						w.Kept(key)
						w.Text(`"` + x + `: "`)
						w.Secret()
						w.Text(`"}`)
						w.next(nl)
						return
					}
					w.Secret()
					w.next(nl)
				})
			}
		}
	}
}

// sepSequenceCases writes a key in one string of a sequence, a separator run of the class in the next and the
// value in the one after, and the forms reviews found split over strings: an explicit key, an env entry's
// name and value, a quoted key, and a flag after other text in its string.
func (g *canonGen) sepSequenceCases() {
	k := func(s string) canonSeqPart { return canonSeqPart{kept: s} }
	x := func(s string) canonSeqPart { return canonSeqPart{text: s} }
	value := func(w *CanonWriter) { w.Secret() }
	indented := func(w *CanonWriter) {
		w.Text("  ")
		w.Secret()
	}
	n := 0
	emit := func(items [][]canonSeqPart, val func(*CanonWriter)) {
		r := n % len(canonSeqRenderings)
		n++
		g.add(canonFormSequence, fmt.Sprintf("separator sequence %q, rendering %d", canonSeqText(items), r), func(w *CanonWriter) {
			w.sequenceParts(r, items, []func(*CanonWriter){val}, "--port=8080")
		})
	}
	seps := []string{"::", "=:", ": :", ":=", "==", "=>", "->", "\uff1a\uff1a", "\u00a0:", ":\u200b", "\x00="}
	for ki, key := range canonGlyphKeys {
		for _, sep := range seps {
			emit([][]canonSeqPart{{k(key)}, {x(sep)}}, value)
		}
		emit([][]canonSeqPart{{k(key + "::")}}, value)
		emit([][]canonSeqPart{{k(key), x("\u200b")}, {k(":")}}, value)
		emit([][]canonSeqPart{{k("? "), k(key)}}, func(w *CanonWriter) {
			w.Kept(":")
			w.Text(" ")
			w.Secret()
		})
		emit([][]canonSeqPart{{k(`'`), k(key), k(`':`)}}, indented)
		flag := canonFlagKeys[ki%len(canonFlagKeys)]
		emit([][]canonSeqPart{{k("--args="), k(flag)}}, value)
		emit([][]canonSeqPart{{k(":"), k(flag)}}, value)
		emit([][]canonSeqPart{{k("|")}, {k("="), k(flag)}}, value)
	}
	for _, name := range canonEnvNames {
		emit([][]canonSeqPart{{k("- name: "), k(name)}}, func(w *CanonWriter) {
			w.Text("  ")
			w.Kept("value:")
			w.Text(" ")
			w.Secret()
		})
	}
}
