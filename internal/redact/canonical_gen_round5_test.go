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
	"strings"
)

// The forms of the fifth round of #845 (second decision of 2026-11-18), each drawn from a class, not from the
// inputs a review found: a separator with blanks of any length around its marks, a value that is itself a
// secret key with a separator, the look-alikes of "=" and the arrows, and a URL in angle brackets whose
// password is digits, letters or punctuation. Like canonical_gen_test.go, this file uses only String and the
// redactor's spans, so it runs unchanged in a checkout of main. Its cases come after every earlier form.

const (
	// canonFormSepBlanks is a secret key, blanks of any length, a separator and blanks again, then the value.
	canonFormSepBlanks = "separator between blanks"
	// canonFormKeyValue is a key whose value is a secret key with a separator and a value ("auth: password: V").
	canonFormKeyValue = "secret key as a value"
	// canonFormEqualsArrow is a secret key followed by a look-alike of "=" or an arrow character.
	canonFormEqualsArrow = "equals or arrow look-alike"
)

// canonEqualsLookalikes are the characters that Unicode's confusables list (confusables.txt 18.0.0) maps to
// "=" or to a run of them, written here from the data, not from the rule.
var canonEqualsLookalikes = []string{
	"\u1400", "\u2e40", "\u30a0", "\ua4ff", "\ua78a", "\uff1d", "\ufe66", "\U00010190", "\U00011053", "\ua60c",
	"\u2a75", "\u2a76",
}

// canonArrows are the arrow characters: U+2192 RIGHTWARDS ARROW, U+21D2 RIGHTWARDS DOUBLE ARROW and what the
// confusables list maps to them (U+FFEB HALFWIDTH RIGHTWARDS ARROW).
var canonArrows = []string{"\u2192", "\u21d2", "\uffeb"}

// canonPadLengths are the numbers of blanks before and after a separator's marks: none, a few, and around and
// past the bound of the separator run (16).
var canonPadLengths = []int{0, 1, 3, 8, 14, 15, 16, 17, 19, 24, 40}

// canonPadMarks are the marks of a padded separator: plain, doubled, arrows, and runs with a control or an
// invisible character, which count toward the bound.
var canonPadMarks = []string{":", "=", "::", ":=", "->", "=>", "==", "\x00:", "\u200b:", ":\u200b", "\uff1a", "=:"}

// foldRoundFiveCases writes the forms of the fifth round.
func (g *canonGen) foldRoundFiveCases() {
	g.sepBlanksCases()
	g.keyValueCases()
	g.equalsArrowCases()
	g.autolinkPasswordCases()
}

// pad returns n blanks, drawn from canonSepBlanks.
func (g *canonGen) pad(n int) string {
	var b strings.Builder
	for range n {
		if g.rng.IntN(4) == 0 {
			b.WriteString(canonPick(g.rng, canonSepBlanks))
			continue
		}
		b.WriteByte(' ')
	}
	return b.String()
}

// sepBlanksCases writes a key, blanks, marks, blanks and a value, with each pad length before and after.
func (g *canonGen) sepBlanksCases() {
	for mi, mark := range canonPadMarks {
		for bi, before := range canonPadLengths {
			for ai, after := range canonPadLengths {
				if (bi+ai+mi)%2 == 1 && before > 3 && after > 3 {
					// Every other long pair: the pad lengths are what the form varies, not their product.
					continue
				}
				key := canonSepKeys[(mi+bi+ai)%len(canonSepKeys)]
				nl := canonNLs[(mi+bi+ai)%len(canonNLs)]
				pre, post := g.pad(before), g.pad(after)
				g.add(canonFormSepBlanks, fmt.Sprintf("key %s, %d blanks, separator %q, %d blanks, line end %q", key, before, mark, after, nl), func(w *canonWriter) {
					w.kept(key)
					w.text(pre + mark + post)
					w.secret()
					w.next(nl)
				})
			}
		}
	}
}

// canonOuterKeys are the keys in front of a secret key that stands as their value: bare short keys, other keys
// and secret keys.
var canonOuterKeys = []string{"auth", "pass", "config", "spec", "msg", passwordKey, canonTokenKey, "env"}

// canonInnerSeps are the separators between the inner secret key and its value, and canonOuterSeps those after
// the outer key.
var (
	canonInnerSeps = []string{": ", "=", canonSpacedEquals, " -> ", "->", " => ", " :: ", ":: ", " := "}
	canonOuterSeps = []string{": ", "=", " -> ", " => ", canonSpacedEquals}
)

// canonSpacedEquals is an equals sign between blanks.
const canonSpacedEquals = " = "

// keyValueCases writes an outer key, a separator, a secret key, a separator and a value.
func (g *canonGen) keyValueCases() {
	for oi, outer := range canonOuterKeys {
		for si, osep := range canonOuterSeps {
			for ii, isep := range canonInnerSeps {
				inner := canonGlyphKeys[(oi+si+ii)%len(canonGlyphKeys)]
				nl := canonNLs[(oi+si+ii)%len(canonNLs)]
				g.add(canonFormKeyValue, fmt.Sprintf("%s%q%s%q, line end %q", outer, osep, inner, isep, nl), func(w *canonWriter) {
					w.kept(outer)
					w.text(osep)
					w.kept(inner)
					w.text(isep)
					w.secret()
					w.next(nl)
				})
			}
		}
	}
}

// equalsArrowCases writes a secret key, a look-alike of "=" or an arrow, and a value: glued, between blanks,
// and in JSON.
func (g *canonGen) equalsArrowCases() {
	for ci, c := range append(append([]string{}, canonEqualsLookalikes...), canonArrows...) {
		for v := range 4 {
			key := canonSepKeys[(ci+v)%len(canonSepKeys)]
			nl := canonNLs[(ci+v)%len(canonNLs)]
			g.add(canonFormEqualsArrow, fmt.Sprintf("key %s, separator %q, shape %d, line end %q", key, c, v, nl), func(w *canonWriter) {
				switch v {
				case 0:
					w.kept(key)
					w.text(c)
					w.secret()
				case 1:
					w.kept(key)
					w.text(" " + c + " ")
					w.secret()
				case 2:
					w.text("- ")
					w.kept(key)
					w.text(c + " ")
					w.secret()
				case 3:
					w.text(`{"`)
					w.kept(key)
					w.text(`"` + c + ` "`)
					w.secret()
					w.text(`"}`)
				}
				w.next(nl)
			})
		}
	}
}

// secretOf writes a secret of the given alphabet, made unique by the counter, and records its range: digits
// only, letters only, or letters, digits and the punctuation a URL's user info may hold unencoded, 11 bytes as
// every generated secret.
func (w *canonWriter) secretOf(alphabet int) {
	*w.n++
	start := w.b.Len()
	switch alphabet {
	case 0:
		fmt.Fprintf(&w.b, "%011d", 70000000000+*w.n)
	case 1:
		n := *w.n
		w.b.WriteString("Qz")
		for range 9 {
			w.b.WriteByte(byte('a' + n%26))
			n /= 26
		}
	default:
		fmt.Fprintf(&w.b, "Q!%05d$x*w", 20000+*w.n)
	}
	w.c.secrets = append(w.c.secrets, [2]int{start, w.b.Len()})
}

// autolinkPasswordCases writes "<scheme://user:password>" closed by ">,<" or ">;<" before a second link with an
// "@", the password drawn from digits (where a port would stand), letters, or letters, digits and punctuation.
func (g *canonGen) autolinkPasswordCases() {
	schemes := []string{"postgres", "mysql", "redis", "https", "amqp"}
	seconds := []string{"x@db/app>", "mailto:ops@db>", "u@h>"}
	for si, scheme := range schemes {
		for ci, closer := range []string{">,<", ">;<"} {
			for vi, second := range seconds {
				for alphabet := range 3 {
					nl := canonNLs[(si+ci+vi+alphabet)%len(canonNLs)]
					g.add(canonFormAutolink, fmt.Sprintf("%s, %q, %q, alphabet %d, line end %q", scheme, closer, second, alphabet, nl), func(w *canonWriter) {
						w.text("<" + scheme + "://alice:")
						w.secretOf(alphabet)
						w.text(closer + second)
						w.next(nl)
					})
				}
			}
		}
	}
}
