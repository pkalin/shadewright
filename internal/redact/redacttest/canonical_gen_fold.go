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
)

// The forms of the third round of #845 (review of 0299401d): the folded view as the strings of a sequence show
// it (a key with a format character inside it, or a colon look-alike after it or in a string of its own), a
// format character between a key and its colon, the colon look-alikes of Unicode's confusables list the second
// round did not write, and an explicit YAML key written as a block scalar. The generator
// is independent of the redactor. Its cases come
// after every earlier form, so the inputs before them keep their text and their hashes.

// canonMoreColons are the characters Unicode's confusables list (version 18.0.0) maps to one colon or to two,
// besides canonColonLookalikes: visargas of Indic scripts, the Armenian full stop, Syriac, Samaritan, Hebrew,
// Ethiopic, Runic, Mongolian and Manichaean punctuation, the modifier letters, the musical repeat dots, the
// Lisu tone letter, the two dot punctuation and leader, and the proportion and squared four dot signs ("::").
// The list is written here from the data, not from the rule.
var canonMoreColons = []string{
	"\u0903", "\u0a83", "\U00011002", "\U00011082", "\U00011182", "\U000115be", "\U000116ac", "\U00011838",
	"\u0589", "\u0703", "\u0704", "\u0831", "\u1361", "\u16ec", "\ufe30", "\u1803", "\u1809", "\u205a", "\u05c3",
	"\U00010af5", "\U0001123a", "\u02f8", "\u1393", "\U0001d108", "\u02d0", "\U0001015b", "\U00011dd9",
	"\ua4fd", "\u2e2c", "\u2237",
}

// canonAllColons are the colon look-alikes of both rounds.
var canonAllColons = append(append([]string{}, canonColonLookalikes...), canonMoreColons...)

// foldRoundThreeCases writes the forms of the third round.
func (g *canonGen) foldRoundThreeCases() {
	g.seqGlyphCases()
	g.keyGapCases()
	g.moreColonCases()
	g.explicitBlockKeyCases()
}

// seqGlyphCases writes a sequence whose key or flag has a format character inside it, a format character
// before its separator, or a colon look-alike after it or in a string of its own, followed by the value
// string. The rendering turns with the case.
func (g *canonGen) seqGlyphCases() {
	k := func(s string) canonSeqPart { return canonSeqPart{kept: s} }
	x := func(s string) canonSeqPart { return canonSeqPart{text: s} }
	value := func(w *CanonWriter) { w.Secret() }
	n := 0
	emit := func(items [][]canonSeqPart) {
		r := n % len(canonSeqRenderings)
		n++
		g.add(canonFormGlyph, fmt.Sprintf("sequence %q, rendering %d", canonSeqText(items), r), func(w *CanonWriter) {
			w.sequenceParts(r, items, []func(*CanonWriter){value}, "--port=8080")
		})
	}
	for fi, f := range canonFlagKeys {
		for ci, fc := range canonFormatChars {
			// Inside the name, after the dashes.
			at := 3 + (fi+ci)%(len(f)-3)
			emit([][]canonSeqPart{{k(f[:at]), x(fc), k(f[at:])}})
		}
	}
	for ki, key := range canonGlyphKeys {
		for ci, fc := range canonFormatChars {
			at := 1 + (ki+ci)%(len(key)-1)
			emit([][]canonSeqPart{{k(key[:at]), x(fc), k(key[at:]), x("=")}})
			emit([][]canonSeqPart{{k(key[:at]), x(fc), k(key[at:])}, {k(":")}})
			emit([][]canonSeqPart{{k(key), x(fc + ":")}})
			emit([][]canonSeqPart{{k(key), x(fc + "=")}})
		}
		for _, c := range canonAllColons {
			emit([][]canonSeqPart{{k(key), x(c)}})
			emit([][]canonSeqPart{{k(key)}, {x(c)}})
		}
	}
}

// keyGapCases writes a secret key with a run of format characters between it and its colon, its equals sign,
// the closing quote of a JSON key or a colon look-alike.
func (g *canonGen) keyGapCases() {
	for ki, key := range canonGlyphKeys {
		for fi, fc := range canonFormatChars {
			run := fc
			if fi%2 == 1 {
				run += canonFormatChars[(fi+1)%len(canonFormatChars)]
			}
			for v := range 5 {
				nl := canonNLs[(ki+fi+v)%len(canonNLs)]
				g.add(canonFormGlyph, fmt.Sprintf("key %s, %q before the colon, shape %d, line end %q", key, run, v, nl), func(w *CanonWriter) {
					switch v {
					case 0:
						w.Kept(key)
						w.Text(run + ": ")
						w.Secret()
					case 1:
						w.Kept(key)
						w.Text(run + ": " + canonIndicators[(ki+fi)%len(canonIndicators)] + nl + "  ")
						w.Secret()
					case 2:
						w.Kept(key)
						w.Text(run + "=")
						w.Secret()
					case 3:
						w.Text(`{"`)
						w.Kept(key)
						w.Text(run + `": "`)
						w.Secret()
						w.Text(`"}`)
					case 4:
						w.Kept(key)
						w.Text(run + canonAllColons[(ki+fi)%len(canonAllColons)] + " ")
						w.Secret()
					}
					w.next(nl)
				})
			}
		}
	}
}

// moreColonCases writes a secret key followed by each colon look-alike of canonMoreColons, in the shapes of
// glyphCases, with two keys for each.
func (g *canonGen) moreColonCases() {
	for ci, c := range canonMoreColons {
		for j := range 2 {
			ki := (ci + j*3) % len(canonGlyphKeys)
			key := canonGlyphKeys[ki]
			for v := range 4 {
				nl := canonNLs[(ki+ci+v)%len(canonNLs)]
				g.add(canonFormGlyph, fmt.Sprintf("key %s, colon %q, shape %d, line end %q", key, c, v, nl), func(w *CanonWriter) {
					w.Kept(key)
					switch v {
					case 0:
						w.Text(c + " ")
					case 1:
						w.Text(c)
					case 2:
						w.Text(" " + c + " ")
					case 3:
						w.Text(c + " " + canonIndicators[(ki+ci)%len(canonIndicators)] + nl + "  ")
					}
					w.Secret()
					w.next(nl)
				})
			}
		}
	}
}

// explicitBlockKeyCases writes a secret key as an explicit YAML key whose key is a block scalar ("? |" and the
// key indented on the line below) or a plain scalar on the line below a bare "?", and its value after ":" at
// the column of the "?": inline, as a block, indented, in a sequence entry, with a comment after the header and
// with a blank line before the key.
func (g *canonGen) explicitBlockKeyCases() {
	for ki, key := range canonGlyphKeys {
		for v := range 7 {
			nl := canonNLs[(ki+v)%len(canonNLs)]
			ind := canonIndicators[(ki+v)%len(canonIndicators)]
			g.add(canonFormExplicit, fmt.Sprintf("block key %s, %q, shape %d, line end %q", key, ind, v, nl), func(w *CanonWriter) {
				switch v {
				case 0:
					w.Text("? " + ind + nl + "  ")
					w.Kept(key)
					w.Text(nl + ": ")
					w.Secret()
				case 1:
					w.Text("? " + ind + nl + "  ")
					w.Kept(key)
					w.Text(nl + ": " + canonIndicators[(ki+v+1)%len(canonIndicators)] + nl + "  ")
					w.Secret()
				case 2:
					w.Kept("spec")
					w.Text(":" + nl + "  ? " + ind + nl + "    ")
					w.Kept(key)
					w.Text(nl + "  : ")
					w.Secret()
				case 3:
					w.Text("- ? " + ind + nl + "    ")
					w.Kept(key)
					w.Text(nl + "  : ")
					w.Secret()
				case 4:
					w.Text("? " + ind + " # ")
					w.Kept("note")
					w.Text(nl + "  ")
					w.Kept(key)
					w.Text(nl + ": ")
					w.Secret()
				case 5:
					w.Text("? " + ind + nl + nl + "  ")
					w.Kept(key)
					w.Text(nl + ": ")
					w.Secret()
				case 6:
					w.Text("?" + nl + "  ")
					w.Kept(key)
					w.Text(nl + ": ")
					w.Secret()
				}
				w.next(nl)
			})
		}
	}
}
