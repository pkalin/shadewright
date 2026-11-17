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
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
)

// The canonical view corpus of #845: the forms where a step changes the text before or between redaction
// passes, so that a block value under a secret key survives. The generator writes every secret and every word
// that must survive at a known offset, without reading any rule, so the answer is known by construction. This
// file uses only String and the redactor's spans, so it runs unchanged in a checkout of main to write the
// table of what main redacts (canonMainTable).

// The forms of the corpus.
const (
	// canonFormIndent is a block indented with characters other than plain spaces: no-break and other Unicode
	// spaces, zero-width characters, NUL and other controls.
	canonFormIndent = "indentation of other characters"
	// canonFormKeys is two or three keys on one line claiming one block.
	canonFormKeys = "keys on one line"
	// canonFormBrackets is a flow value with a bracket inside the secret.
	canonFormBrackets = "bracket inside the secret"
	// canonFormSequence is a key in one string of a sequence and its value in the next string.
	canonFormSequence = "key and value in two strings"
	// canonFormMixed is the forms above combined, and next to a second secret key.
	canonFormMixed = "forms combined"
)

// canonForms lists the forms in the order the corpus writes them.
var canonForms = []string{canonFormIndent, canonFormKeys, canonFormBrackets, canonFormSequence, canonFormMixed}

// canonCase is one input of the corpus. in is the text as a caller reads it. args, for a sequence, holds its
// strings as a container's arguments hold them; in is then one rendering of that sequence. secrets and keep
// are byte ranges of in: each secret range is a piece of a secret that must not appear in any output, each
// keep range a word that String must leave alone.
type canonCase struct {
	form    string
	name    string
	in      string
	args    []string
	secrets [][2]int
	keep    [][2]int
}

// canonWriter builds one canonCase.
type canonWriter struct {
	b strings.Builder
	c canonCase
	n *int
}

func (w *canonWriter) text(s string) { w.b.WriteString(s) }

// secret writes the next generated secret, which looks like a secret to the bare short keys too, and records
// its range.
func (w *canonWriter) secret() string {
	*w.n++
	start := w.b.Len()
	fmt.Fprintf(&w.b, "Qz%dxK9w", 20000+*w.n)
	w.c.secrets = append(w.c.secrets, [2]int{start, w.b.Len()})
	return w.b.String()[start:]
}

// canonPhraseWords are the words of a phrase secret: a value of words that does not look like a secret, so a
// bare short key refuses it, while a secret key takes it.
var canonPhraseWords = []string{
	"amber", "basil", "cobalt", "dune", "ember", "fjord", "garnet", "heron", "indigo", "juniper", "kelp", "lilac",
	"maple", "nectar", "onyx", "pebble",
}

// phrase writes the next phrase secret, three words, and records each word as a piece of secret.
func (w *canonWriter) phrase() string {
	*w.n++
	start := w.b.Len()
	// Three different words, chosen by the counter from the words not taken yet.
	k := *w.n
	left := slices.Clone(canonPhraseWords)
	for i := range 3 {
		if i > 0 {
			w.text(" ")
		}
		ws := w.b.Len()
		j := k % len(left)
		k /= len(left)
		w.text(left[j])
		left = slices.Delete(left, j, j+1)
		w.c.secrets = append(w.c.secrets, [2]int{ws, w.b.Len()})
	}
	return w.b.String()[start:]
}

// canonBareKey reports whether key is a bare short key.
func canonBareKey(key string) bool { return slices.Contains(canonBareKeys, key) }

// canonBareKeys are the bare short keys.
var canonBareKeys = strings.Fields("pass auth sig")

// bracketSecret writes a secret of pieces with the brackets of glue between them; each piece is recorded as a
// secret, the brackets are not (a caller may replace them).
func (w *canonWriter) bracketSecret(glue []string) {
	w.secret()
	for _, g := range glue {
		w.text(g)
		w.secret()
	}
}

// kept writes s and records that it must survive.
func (w *canonWriter) kept(s string) {
	start := w.b.Len()
	w.b.WriteString(s)
	w.c.keep = append(w.c.keep, [2]int{start, w.b.Len()})
}

// next writes a line break and a line at column 0 whose word must survive.
func (w *canonWriter) next(nl string) {
	w.text(nl)
	w.kept("next")
	w.text(": x")
}

// canonKeys are secret keys of each form the rules know, the bare short keys among them, whose values count
// only when they look like a secret, as every generated secret does.
var canonKeys = strings.Fields(`password DB_PASSWORD client_secret token apiKey credentials "password" 'token' pass
	auth sig`)

// canonFlagKeys are flags whose value is the next word, or the next string of a sequence.
var canonFlagKeys = []string{"--password", "--token", "--client-secret", "--api-key", "-password", "--db-password"}

// canonFirstKeys are the keys before a secret key on one line: bare short keys that refuse a block that does
// not look like a secret, innocent keys and secret keys.
var canonFirstKeys = strings.Fields("pass auth sig msg description password token")

// canonIndicators are block scalar headers with chomping and indentation indicators.
var canonIndicators = []string{"|", ">", "|-", ">+", "|2", ">-"}

// canonIndentChars are characters other than a plain space that count as indentation in the canonical view:
// Unicode spaces, zero-width characters, the byte order mark, NUL and other C0 and C1 controls, and the
// tab and other ASCII blanks.
var canonIndentChars = []string{
	"\u00a0", "\u200b", "\x00", "\u2007", "\u202f", "\u3000", "\u2002", "\u2009", "\u205f", "\u2060", "\ufeff",
	"\u1680", "\u0085", "\u2028", "\t", "\v", "\f", "\x01", "\x1f", "\x7f",
}

// canonBrackets are the brackets placed inside a secret.
var canonBrackets = []string{"]", "[", "}", "{", ">", "<", ")", "(", "]]", "][", "}{", "><", "]}", "[{"}

// canonFlowContexts are the places a flow value under a key stands in: open, close, and whether the value is a
// block indicator's.
var canonFlowContexts = []struct{ open, close string }{
	{"[", "]"}, {"{", "}"}, {"- {", "}"}, {canonArgsFlow, "]"}, {"", ""}, {"[{", "}]"}, {`msg="[`, `]"`},
}

// canonNeighbours are the glue and second secret key after a first secret value: on the next line, after a
// separator on the same line, after a quote.
var canonNeighbours = []string{
	"\napiKey=", "\ntoken: ", "\n  token: ", " token=", ", token: ", "; apiKey=", `" token="`, "' secret='", "\t--password ",
	`","token":"`,
}

// canonServe is the first string of a sequence, which must survive, and canonArgsFlow the start of a YAML flow
// sequence under the key args.
const (
	canonServe    = "serve"
	canonArgsFlow = "args: ["
)

// canonPick returns one of xs.
func canonPick(rng *rand.Rand, xs []string) string { return xs[rng.IntN(len(xs))] }

// canonCorpus returns every input of the corpus. The choices that do not multiply the forms (indicators,
// leads, line ends) come from a fixed seed.
func canonCorpus() []canonCase {
	g := &canonGen{rng: rand.New(rand.NewPCG(845, 17))} //nolint:gosec // a fixed seed keeps the corpus the same on every run
	g.indentCases()
	g.keysCases()
	g.bracketCases()
	g.sequenceCases()
	g.mixedCases()
	return g.out
}

// canonGen builds the corpus: the seeded choices, the secret counter and the inputs so far.
type canonGen struct {
	rng *rand.Rand
	n   int
	out []canonCase
}

// add writes one input of form with f.
func (g *canonGen) add(form, name string, f func(w *canonWriter)) {
	w := &canonWriter{n: &g.n}
	f(w)
	w.c.form, w.c.name, w.c.in = form, form+": "+name, w.b.String()
	g.out = append(g.out, w.c)
}

// canonNLs are the line ends of the corpus.
var canonNLs = []string{"\n", "\r\n"}

// indentCases writes a block indented with other characters, in six shapes.
func (g *canonGen) indentCases() {
	rng, add, nls := g.rng, g.add, canonNLs
	for _, key := range canonKeys {
		for ci, c := range canonIndentChars {
			for v := range 6 {
				ind := canonPick(rng, canonIndicators)
				nl := nls[(ci+v)%2]
				add(canonFormIndent, fmt.Sprintf("key %s, indicator %q, indent %q, shape %d", key, ind, c, v), func(w *canonWriter) {
					switch v {
					case 0:
						w.text(key + ": " + ind + nl + c + c)
						w.secret()
					case 1:
						w.kept("warning")
						w.text(": " + key + ": " + ind + nl + c)
						w.secret()
					case 2:
						w.text(key + ": " + ind + nl + c + c)
						w.secret()
						w.text(nl + c + c)
						w.secret()
					case 3:
						w.text("  " + key + ": " + ind + nl + "  " + c)
						w.secret()
					case 4:
						w.text(key + ": " + ind + nl + " " + c + " ")
						w.secret()
					case 5:
						w.kept("spec")
						w.text(":" + nl + "  " + key + ": " + ind + nl + c + c + c + c)
						w.secret()
					}
					w.next(nl)
				})
			}
		}
	}

}

// keysCases writes keys on one line: two, or three, the block under the line or flattened onto it; and a secret
// key first with an innocent key after it, claiming the same block.
func (g *canonGen) keysCases() {
	rng, add, nls := g.rng, g.add, canonNLs
	seps := []string{" ", "\t", ", ", "; ", "  "}
	for _, first := range canonFirstKeys {
		for _, key := range canonKeys {
			for si, sep := range seps {
				for v := range 6 {
					if v >= 3 && canonBareKey(key) {
						// A bare short key takes only a value that looks like a secret, and a phrase does not.
						continue
					}
					i1, i2 := canonPick(rng, canonIndicators), canonPick(rng, canonIndicators)
					nl := nls[(si+v)%2]
					add(canonFormKeys, fmt.Sprintf("%s %q %s, %q %q, shape %d", first, sep, key, i1, i2, v), func(w *canonWriter) {
						// Shapes 3 to 5 are 0 to 2 with a phrase for a value, which a bare short key first on the
						// line refuses.
						value := w.secret
						if v >= 3 {
							value = w.phrase
						}
						switch v % 3 {
						case 0:
							w.text(first + ": " + i1 + sep + key + ": " + i2 + nl + "  ")
							value()
						case 1:
							w.kept("level")
							w.text("=warn " + first + ": " + i1 + sep + key + ": " + i2 + "   ")
							value()
						case 2:
							w.text(first + ": " + i1 + sep + canonBareKeys[1] + ": " + i1 + sep + key + ": " + i2 + nl + "    ")
							value()
						}
						w.next(nl)
					})
				}
			}
		}
	}
	// A secret key first and an innocent key after it, claiming the same block.
	for _, key := range canonKeys {
		for second := range strings.FieldsSeq("msg description pass x") {
			ind := canonPick(rng, canonIndicators)
			add(canonFormKeys, fmt.Sprintf("%s then %s, %q", key, second, ind), func(w *canonWriter) {
				w.text(key + ": " + ind + " " + second + ": " + ind + "\n  ")
				w.secret()
				w.next("\n")
			})
		}
	}

}

// bracketCases writes a flow value with brackets inside the secret, with or without a block indicator.
func (g *canonGen) bracketCases() {
	rng, add := g.rng, g.add
	for _, key := range canonKeys {
		for ci, ctx := range canonFlowContexts {
			for bi, br := range canonBrackets {
				ind := []string{"", "> ", "| ", canonPick(rng, canonIndicators) + " "}[(ci+bi)%4]
				two := (ci+bi)%3 == 0
				add(canonFormBrackets, fmt.Sprintf("context %q, key %s, %q, bracket %q, three pieces %v", ctx.open, key, ind, br, two), func(w *canonWriter) {
					w.kept("retry")
					w.text(" " + ctx.open + key + ": " + ind)
					glue := []string{br}
					if two {
						glue = append(glue, canonBrackets[(bi+5)%len(canonBrackets)])
					}
					w.bracketSecret(glue)
					w.text(ctx.close)
					w.next("\n")
				})
			}
		}
	}

}

// sequenceCases writes a key in one string of a sequence and its value in the next, in each rendering of the
// sequence.
func (g *canonGen) sequenceCases() {
	rng, add := g.rng, g.add
	type seqItem struct{ key, pad string }
	var items []seqItem
	for _, f := range canonFlagKeys {
		items = append(items, seqItem{f, ""})
	}
	for _, key := range canonKeys {
		if strings.ContainsAny(key, `"'`) {
			continue
		}
		items = append(items, seqItem{key + ": " + canonPick(rng, canonIndicators), "  "}, seqItem{key + ":", "  "})
	}
	for _, it := range items {
		for r := range canonSeqRenderings {
			for _, pad := range []string{it.pad, "\u00a0\u00a0", "\t"} {
				add(canonFormSequence, fmt.Sprintf("%q then %q, rendering %d", it.key, pad, r), func(w *canonWriter) {
					w.sequence(r, []string{canonServe, it.key}, func(w *canonWriter) {
						w.text(pad)
						w.secret()
					}, "--port=8080")
				})
			}
		}
	}

}

// mixedCases writes the forms combined, and next to a second secret key.
func (g *canonGen) mixedCases() {
	rng, add := g.rng, g.add
	for ki, key := range canonKeys {
		for ci, c := range canonIndentChars {
			ind := canonPick(rng, canonIndicators)
			nb := canonNeighbours[(ki+ci)%len(canonNeighbours)]
			br := canonBrackets[(ki+ci)%len(canonBrackets)]
			add(canonFormMixed, fmt.Sprintf("keys on one line, indent %q, key %s", c, key), func(w *canonWriter) {
				w.text("pass: " + ind + " " + key + ": " + ind + "\n" + c + c)
				w.secret()
				w.next("\n")
			})
			add(canonFormMixed, fmt.Sprintf("indent %q, bracket %q, key %s", c, br, key), func(w *canonWriter) {
				w.text(key + ": " + ind + "\n" + c + c)
				w.bracketSecret([]string{br})
				w.next("\n")
			})
			add(canonFormMixed, fmt.Sprintf("indent %q, then %q, key %s", c, nb, key), func(w *canonWriter) {
				w.text(key + ": " + ind + "\n" + c + c)
				w.secret()
				w.text(nb)
				w.secret()
				w.next("\n")
			})
			add(canonFormMixed, fmt.Sprintf("flow bracket %q, then %q, key %s", br, nb, key), func(w *canonWriter) {
				w.kept("retry")
				w.text(" {" + key + ": > ")
				w.bracketSecret([]string{br})
				w.text(nb)
				w.secret()
				w.text("}")
				w.next("\n")
			})
			add(canonFormMixed, fmt.Sprintf("keys on one line, flow bracket %q, key %s", br, key), func(w *canonWriter) {
				w.text("[pass: " + ind + " " + key + ": > ")
				w.bracketSecret([]string{br})
				w.text("]")
				w.next("\n")
			})
		}
	}
	for fi, flag := range canonFlagKeys {
		for ci, c := range canonIndentChars {
			r := (fi + ci) % len(canonSeqRenderings)
			br := canonBrackets[(fi+ci)%len(canonBrackets)]
			key := []string{"password: |", flag}[ci%2]
			add(canonFormMixed, fmt.Sprintf("sequence %q, indent %q, rendering %d", key, c, r), func(w *canonWriter) {
				w.sequence(r, []string{canonServe, key}, func(w *canonWriter) {
					w.text(c + c)
					w.secret()
				}, "--port=8080")
			})
			add(canonFormMixed, fmt.Sprintf("sequence %q, bracket %q, then a flag, rendering %d", flag, br, r), func(w *canonWriter) {
				w.sequence(r, []string{canonServe, flag}, func(w *canonWriter) {
					w.bracketSecret([]string{br})
				}, "--token")
				// The value of the second flag is the last string: a sequence ends with it.
				w.c.args = append(w.c.args, w.secretArg(r))
			})
		}
	}
}

// canonSeqRenderings are the ways a text shows a sequence of strings: JSON on one line, a YAML flow sequence
// after a key, a YAML block sequence of quoted items, pretty-printed JSON, and JSON escaped inside a string.
var canonSeqRenderings = []struct{ open, sep, close, quote string }{
	{`[`, `, `, `]`, `"`},
	{canonArgsFlow, `, `, `]`, `"`},
	{"args:" + nlDash, nlDash, "\n", `"`},
	{"[\n  ", ",\n  ", "\n]", `"`},
	{`{"args":"[`, `, `, `]"}`, `\"`},
}

// sequence writes the strings before, then the value string val writes, then after, in rendering r, and
// records the strings in args.
func (w *canonWriter) sequence(r int, before []string, val func(w *canonWriter), after string) {
	rd := canonSeqRenderings[r]
	w.text(rd.open)
	for i, s := range before {
		if i > 0 {
			w.text(rd.sep)
		}
		w.text(rd.quote)
		if s == canonServe {
			w.kept(s)
		} else {
			w.text(s)
		}
		w.text(rd.quote)
		w.c.args = append(w.c.args, s)
	}
	w.text(rd.sep + rd.quote)
	start := w.b.Len()
	val(w)
	w.c.args = append(w.c.args, w.b.String()[start:])
	w.text(rd.quote + rd.sep + rd.quote + after + rd.quote)
	w.c.args = append(w.c.args, after)
	w.text(rd.close)
}

// secretArg writes one more string, a secret, at the end of the sequence just written in rendering r (before
// its close) and returns it. The text is rewritten: the close moves after the new string.
func (w *canonWriter) secretArg(r int) string {
	rd := canonSeqRenderings[r]
	s := w.b.String()
	s = strings.TrimSuffix(s, rd.close)
	w.b.Reset()
	w.b.WriteString(s)
	w.text(rd.sep + rd.quote)
	v := w.secret()
	w.text(rd.quote + rd.close)
	return v
}

// canonHash is the hash of an input the tables of the corpus hold: the first 8 bytes of the SHA-256 of the
// input and of its strings, in hex.
func canonHash(c canonCase) string {
	h := sha256.New()
	h.Write([]byte(c.in))
	for _, a := range c.args {
		h.Write([]byte{0})
		h.Write([]byte(a))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// canonMainTable holds, for each input of the corpus in order, its hash and the byte ranges main redacted
// (commit 26a6108e). It was written once by running TestWriteCanonMainTable in a checkout of that commit with
// CANON_MAIN_TABLE set to the output path; regenerate it only from that commit.
const canonMainTable = "testdata/canonical-main.jsonl"

// canonMainRow is one line of canonMainTable.
type canonMainRow struct {
	Hash  string   `json:"h"`
	Spans [][2]int `json:"s,omitempty"`
}

// TestWriteCanonMainTable writes canonMainTable from the redactor it runs with, when CANON_MAIN_TABLE names
// the output path; otherwise it is skipped.
func TestWriteCanonMainTable(t *testing.T) {
	path := os.Getenv("CANON_MAIN_TABLE")
	if path == "" {
		t.Skip("CANON_MAIN_TABLE is not set")
	}
	f, err := os.Create(path) //nolint:gosec // a path the person running the test names
	if err != nil {
		t.Fatal(err)
	}
	bw := bufio.NewWriter(f)
	enc := json.NewEncoder(bw)
	for _, c := range canonCorpus() {
		row := canonMainRow{Hash: canonHash(c)}
		for _, sp := range defaultRedactor.spans(c.in) {
			row.Spans = append(row.Spans, [2]int{sp.start, sp.end})
		}
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
