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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
)

// The canonical view corpus of #845: the forms where a step changes the text before or between redaction
// passes, so that a block value under a secret key survives. The generator writes every secret and every piece
// of text that must survive at a known offset, without reading any rule, so the answer is known by
// construction. The redaction tests retain their original fixture tables in package redact.

// The forms of the corpus.
const (
	// canonFormIndent is a block indented with characters other than plain spaces: no-break and other Unicode
	// spaces, zero-width characters, NUL and other controls, invalid UTF-8.
	canonFormIndent = "indentation of other characters"
	// canonFormKeys is two or three keys on one line claiming one block.
	canonFormKeys = "keys on one line"
	// canonFormBrackets is a flow value with a bracket inside the secret.
	canonFormBrackets = "bracket inside the secret"
	// canonFormSequence is a key in one string of a sequence and its value in the next string, or a flag and
	// its value in one string.
	canonFormSequence = "key and value in two strings"
	// canonFormEnv is the name of an environment variable in one field and its value in the next.
	canonFormEnv = "env name and value in two fields"
	// canonFormExcerpt is an indented block in a log, whose key line a cut of the log excerpt drops before the
	// text reaches redaction.
	canonFormExcerpt = "block cut from its key"
	// canonFormMixed is the forms above combined, and next to a second secret key.
	canonFormMixed = "forms combined"
	// canonFormGlyph is a secret key written with a format character inside it (a zero-width space or joiner, a
	// soft hyphen, a byte order mark) or with a character that looks like a colon after it.
	canonFormGlyph = "key with format characters or a colon look-alike"
	// canonFormExplicit is a secret key written as an explicit YAML key: "? password" and its value on the
	// next line after ":".
	canonFormExplicit = "explicit key"
	// canonFormComment is a comment line at column 0 between the lines of a block value.
	canonFormComment = "comment inside the block"
	// canonFormLabels is a metric series whose labels are secret keys and values, as query_metrics shows them.
	canonFormLabels = "metric labels"
)

// canonForms lists the forms in the order the corpus writes them.
var canonForms = []string{
	canonFormIndent, canonFormKeys, canonFormBrackets, canonFormSequence, canonFormEnv, canonFormExcerpt,
	canonFormMixed, canonFormGlyph, canonFormExplicit, canonFormComment, canonFormLabels, canonFormSeparator,
	canonFormAutolink, canonFormSepBlanks, canonFormKeyValue, canonFormEqualsArrow,
}

// CanonSource is one input of the corpus. in is the text as a caller reads it. args, for a sequence, holds its
// strings as a container's arguments hold them; in is then one rendering of that sequence. labels, for a
// metric series, holds its labels in the order query_metrics shows them (sorted by name); in is then the
// series as it shows them. secrets and keep are byte ranges of in: each secret range is a piece of a secret
// that must not appear in any output, each keep range a piece of text (a key, a flag, a word around the
// secret) that must survive.
type CanonSource struct {
	Form    string
	Name    string
	In      string
	Args    []string
	Labels  [][2]string
	Secrets [][2]int
	Keep    [][2]int
}

// CanonWriter builds one CanonSource.
type CanonWriter struct {
	// B holds the text written so far.
	B strings.Builder
	// C holds the recorded byte ranges.
	C CanonSource
	// N points to the shared synthetic secret counter.
	N *int
}

// Text appends literal text without recording a range.
func (w *CanonWriter) Text(s string) { w.B.WriteString(s) }

// canonPartialMin is the length of a piece's prefix and suffix that counts as a leak of the piece at an entry
// point: a generated secret is "Qz", five digits of a counter and "xK9w", so its first and last 8 bytes each
// hold the counter's digits that tell it from the other pieces of its input. A piece shorter than that (a word
// of a phrase) counts only whole.
const canonPartialMin = 8

// Secret writes the next generated secret, which looks like a secret to the bare short keys too, and records
// its range.
func (w *CanonWriter) Secret() string {
	*w.N++
	start := w.B.Len()
	fmt.Fprintf(&w.B, "Qz%05dxK9w", 20000+*w.N)
	w.C.Secrets = append(w.C.Secrets, [2]int{start, w.B.Len()})
	return w.B.String()[start:]
}

// canonPhraseWords are the words of a phrase secret: a value of words that does not look like a secret, so a
// bare short key refuses it, while a secret key takes it.
var canonPhraseWords = []string{
	"amber", "basil", "cobalt", "dune", "ember", "fjord", "garnet", "heron", "indigo", "juniper", "kelp", "lilac",
	"maple", "nectar", "onyx", "pebble",
}

// phrase writes the next phrase secret, three words, and records each word as a piece of secret.
func (w *CanonWriter) phrase() string {
	*w.N++
	start := w.B.Len()
	// Three different words, chosen by the counter from the words not taken yet.
	k := *w.N
	left := slices.Clone(canonPhraseWords)
	for i := range 3 {
		if i > 0 {
			w.Text(" ")
		}
		ws := w.B.Len()
		j := k % len(left)
		k /= len(left)
		w.Text(left[j])
		left = slices.Delete(left, j, j+1)
		w.C.Secrets = append(w.C.Secrets, [2]int{ws, w.B.Len()})
	}
	return w.B.String()[start:]
}

// canonBareKey reports whether key is a bare short key.
func canonBareKey(key string) bool { return slices.Contains(canonBareKeys, key) }

// canonBareKeys are the bare short keys.
var canonBareKeys = strings.Fields("pass auth sig")

// bracketSecret writes a secret of pieces with the brackets of glue between them; each piece is recorded as a
// secret, the brackets are not (a caller may replace them).
func (w *CanonWriter) bracketSecret(glue []string) {
	w.Secret()
	for _, g := range glue {
		w.Text(g)
		w.Secret()
	}
}

// Kept writes s and records that it must survive.
func (w *CanonWriter) Kept(s string) {
	start := w.B.Len()
	w.B.WriteString(s)
	w.C.Keep = append(w.C.Keep, [2]int{start, w.B.Len()})
}

// next writes a line break and a line at column 0 whose key and value must survive.
func (w *CanonWriter) next(nl string) {
	w.Text(nl)
	w.Kept("next")
	w.Text(": ")
	w.Kept("--port=8080")
}

// canonKeys are secret keys of each form the rules know, the bare short keys among them, whose values count
// only when they look like a secret, as every generated secret does.
var canonKeys = strings.Fields(`password DB_PASSWORD client_secret token apiKey credentials "password" 'token' pass
	auth sig`)

// canonFlagKeys are flags whose value is the next word, or the next string of a sequence.
var canonFlagKeys = []string{passwordFlag, "--token", "--client-secret", "--api-key", "-password", "--db-password"}

// canonFirstKeys are the keys before a secret key on one line: bare short keys that refuse a block that does
// not look like a secret, innocent keys and secret keys.
var canonFirstKeys = strings.Fields("pass auth sig msg description password token")

// canonIndicators are block scalar headers with chomping and indentation indicators, in either order.
var canonIndicators = []string{"|", ">", "|-", ">+", "|2", ">-", "|+", ">2-", "|-2"}

// canonHeaders are canonIndicators and the same headers with a comment after the indicator. A header with a
// comment ends its line, so it stands only where nothing follows on the line.
var canonHeaders = append(slices.Clone(canonIndicators), "| # note", ">- #x", "|2+ # folded below")

// canonIndentChars are characters other than a plain space that count as indentation in the canonical view:
// Unicode spaces, zero-width characters and joiners, the byte order mark, the line and paragraph separators,
// NUL and other C0 and C1 controls, the tab and other ASCII blanks, and bytes that are not valid UTF-8 (a lone
// continuation byte, a cut sequence, an encoded surrogate).
var canonIndentChars = []string{
	"\u00a0", "\u200b", "\x00", "\u2007", "\u202f", "\u3000", "\u2002", "\u2009", "\u205f", "\u2060", "\ufeff",
	"\u1680", "\u0085", "\u2028", "\u2029", "\u200c", "\u200d", "\u180e", "\t", "\v", "\f", "\x01", "\x1f", "\x7f",
	"\xff", "\x80", "\xc3", "\xed\xa0\x80",
}

// canonBrackets are the brackets placed inside a secret.
var canonBrackets = []string{"]", "[", "}", "{", ">", "<", ")", "(", "]]", "][", "}{", "><", "]}", "[{"}

// canonFlowContexts are the places a flow value under a key stands in: open, close, and whether the value is a
// block indicator's.
var canonFlowContexts = []struct{ open, close string }{
	{"[", "]"}, {"{", "}"}, {"- {", "}"}, {canonArgsFlow, "]"}, {"", ""}, {"[{", "}]"}, {`msg="[`, `]"`},
}

// canonNeighbour is the glue before a second secret key after a first secret value, the key, and the glue
// between that key and its value.
type canonNeighbour struct{ before, key, after string }

// canonTokenKey is the second secret key most neighbours use.
const canonTokenKey = "token"

// canonNeighbours are the second secret keys after a first secret value: on the next line, after a separator on
// the same line, after a quote.
var canonNeighbours = []canonNeighbour{
	{"\n", apiKeyName, "="}, {"\n", canonTokenKey, ": "}, {nlIndent, canonTokenKey, ": "}, {" ", canonTokenKey, "="}, {", ", canonTokenKey, ": "},
	{"; ", apiKeyName, "="}, {`" `, canonTokenKey, `="`}, {"' ", canonCheckSecret, "='"}, {"\t", passwordFlag, " "},
	{`","`, canonTokenKey, `":"`}, {"\r\n", apiKeyName, "="}, {"\r", canonTokenKey, ": "},
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
func canonCorpus() []CanonSource {
	g := &canonGen{rng: rand.New(rand.NewPCG(845, 17))} //nolint:gosec // a fixed seed keeps the corpus the same on every run
	g.indentCases()
	g.keysCases()
	g.bracketCases()
	g.sequenceCases()
	g.envCases()
	g.excerptCases()
	g.mixedCases()
	// The forms of the second round of #845 come last, so the inputs above keep their text and their hashes.
	g.splitSequenceCases()
	g.glyphCases()
	g.explicitKeyCases()
	g.commentCases()
	g.envValueFirstBlockCases()
	g.labelCases()
	// The forms of the third round come after those of the second, for the same reason.
	g.foldRoundThreeCases()
	// The forms of the fourth round come after those of the third.
	g.foldRoundFourCases()
	// The forms of the fifth round come after those of the fourth.
	g.foldRoundFiveCases()
	return g.out
}

// canonGen builds the corpus: the seeded choices, the secret counter and the inputs so far.
type canonGen struct {
	rng *rand.Rand
	n   int
	out []CanonSource
}

// add writes one input of form with f.
func (g *canonGen) add(form, name string, f func(w *CanonWriter)) {
	w := &CanonWriter{N: &g.n}
	f(w)
	w.C.Form, w.C.Name, w.C.In = form, form+": "+name, w.B.String()
	g.out = append(g.out, w.C)
}

// canonNLs are the line ends of the corpus: a line feed, CRLF and a lone carriage return.
var canonNLs = []string{"\n", "\r\n", "\r"}

// indentCases writes a block indented with other characters, in seven shapes.
func (g *canonGen) indentCases() {
	rng, add, nls := g.rng, g.add, canonNLs
	for _, key := range canonKeys {
		for ci, c := range canonIndentChars {
			for v := range 7 {
				ind := canonPick(rng, canonHeaders)
				nl := nls[(ci+v)%len(nls)]
				add(canonFormIndent, fmt.Sprintf("key %s, indicator %q, indent %q, line end %q, shape %d", key, ind, c, nl, v), func(w *CanonWriter) {
					switch v {
					case 0:
						w.Kept(key)
						w.Text(": " + ind + nl + c + c)
						w.Secret()
					case 1:
						w.Kept("warning")
						w.Text(": ")
						w.Kept(key)
						w.Text(": " + ind + nl + c)
						w.Secret()
					case 2:
						w.Kept(key)
						w.Text(": " + ind + nl + c + c)
						w.Secret()
						w.Text(nl + c + c)
						w.Secret()
					case 3:
						w.Text("  ")
						w.Kept(key)
						w.Text(": " + ind + nl + "  " + c)
						w.Secret()
					case 4:
						w.Kept(key)
						w.Text(": " + ind + nl + " " + c + " ")
						w.Secret()
					case 5:
						w.Kept("spec")
						w.Text(":" + nl + "  ")
						w.Kept(key)
						w.Text(": " + ind + nl + c + c + c + c)
						w.Secret()
					case 6:
						// Bytes that are not valid UTF-8 next to the value, after it and before the next line's.
						w.Kept(key)
						w.Text(": " + ind + nl + "  " + c)
						w.Secret()
						w.Text("\xff" + nl + "  \xc0")
						w.Secret()
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
					// Only the last header on the line may carry a comment: it would hide the keys after it.
					i1, i2 := canonPick(rng, canonIndicators), canonPick(rng, canonHeaders)
					nl := nls[(si+v)%len(nls)]
					add(canonFormKeys, fmt.Sprintf("%s %q %s, %q %q, line end %q, shape %d", first, sep, key, i1, i2, nl, v), func(w *CanonWriter) {
						// Shapes 3 to 5 are 0 to 2 with a phrase for a value, which a bare short key first on the
						// line refuses.
						value := w.Secret
						if v >= 3 {
							value = w.phrase
						}
						switch v % 3 {
						case 0:
							w.Kept(first)
							w.Text(": " + i1 + sep)
							w.Kept(key)
							w.Text(": " + i2 + nl + "  ")
							value()
						case 1:
							w.Kept("level=warn")
							w.Text(" ")
							w.Kept(first)
							w.Text(": " + i1 + sep)
							w.Kept(key)
							// Flattened onto the line: no header comment, it would hold the value.
							w.Text(": " + i1 + "   ")
							value()
						case 2:
							w.Kept(first)
							w.Text(": " + i1 + sep)
							w.Kept(canonBareKeys[1])
							w.Text(": " + i1 + sep)
							w.Kept(key)
							w.Text(": " + i2 + nl + "    ")
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
			add(canonFormKeys, fmt.Sprintf("%s then %s, %q", key, second, ind), func(w *CanonWriter) {
				w.Kept(key)
				w.Text(": " + ind + " ")
				w.Kept(second)
				w.Text(": " + ind + nlIndent)
				w.Secret()
				w.next("\n")
			})
		}
	}
	// A flag and its value after an equals sign on a command line, next to a flag that must stay.
	for _, flag := range canonFlagKeys {
		for ni, nl := range canonNLs {
			add(canonFormKeys, fmt.Sprintf("command line %s=, line end %q", flag, nl), func(w *CanonWriter) {
				w.Kept("exec /bin/app")
				w.Text(" ")
				w.Kept(flag + "=")
				w.Secret()
				w.Text(" ")
				w.Kept("--port=8080")
				if ni > 0 {
					w.Text(" ")
					w.Kept(flag + "=")
					w.Text(`"`)
					w.Secret()
					w.Text(`"`)
				}
				w.next(nl)
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
				nl := canonNLs[(ci+bi)%len(canonNLs)]
				add(canonFormBrackets, fmt.Sprintf("context %q, key %s, %q, bracket %q, three pieces %v, line end %q", ctx.open, key, ind, br, two, nl), func(w *CanonWriter) {
					w.Kept("retry")
					w.Text(" " + ctx.open)
					w.Kept(key)
					w.Text(": " + ind)
					glue := []string{br}
					if two {
						glue = append(glue, canonBrackets[(bi+5)%len(canonBrackets)])
					}
					w.bracketSecret(glue)
					w.Text(ctx.close)
					w.next(nl)
				})
			}
		}
	}
}

// sequenceCases writes a key in one string of a sequence and its value in the next, in each rendering of the
// sequence; and a flag and its value in one string.
func (g *canonGen) sequenceCases() {
	rng, add := g.rng, g.add
	type seqItem struct{ key, pad string }
	var items []seqItem
	for _, f := range canonFlagKeys {
		items = append(items, seqItem{f, ""}, seqItem{f + "=", ""})
	}
	for _, key := range canonKeys {
		if strings.ContainsAny(key, `"'`) {
			continue
		}
		items = append(items, seqItem{key + ": " + canonPick(rng, canonHeaders), "  "}, seqItem{key + ":", "  "})
	}
	for _, it := range items {
		for r := range canonSeqRenderings {
			for _, pad := range []string{it.pad, "\u00a0\u00a0", "\t"} {
				add(canonFormSequence, fmt.Sprintf("%q then %q, rendering %d", it.key, pad, r), func(w *CanonWriter) {
					w.sequence(r, []string{canonServe, it.key}, func(w *CanonWriter) {
						w.Text(pad)
						w.Secret()
					}, "--port=8080")
				})
			}
		}
	}
	for _, f := range canonFlagKeys {
		for r := range canonSeqRenderings {
			add(canonFormSequence, fmt.Sprintf("%q and its value in one string, rendering %d", f+"=", r), func(w *CanonWriter) {
				w.sequence(r, []string{canonServe}, func(w *CanonWriter) {
					w.Kept(f + "=")
					w.Secret()
				}, "--port=8080")
			})
		}
	}
}

// canonEnvNames are names of environment variables that hold a secret.
var canonEnvNames = strings.Fields("PASSWORD DB_PASSWORD API_TOKEN CLIENT_SECRET AWS_SECRET_ACCESS_KEY GITHUB_TOKEN")

// canonEnvRenderings write an environment variable: its name in the field name and its value in the field
// value, in the text of a manifest, of JSON and of JSON escaped inside a string, and with the value first.
var canonEnvRenderings = []func(w *CanonWriter, name, nl string){
	func(w *CanonWriter, name, _ string) {
		w.Text("{")
		w.Kept("name")
		w.Text(": ")
		w.Kept(name)
		w.Text(", ")
		w.Kept("value")
		w.Text(": ")
		w.Secret()
		w.Text("}")
	},
	func(w *CanonWriter, name, nl string) {
		w.Text("- ")
		w.Kept("name")
		w.Text(": ")
		w.Kept(name)
		w.Text(nl + "  ")
		w.Kept("value")
		w.Text(": ")
		w.Secret()
	},
	func(w *CanonWriter, name, nl string) {
		w.Kept("env")
		w.Text(":" + nl + "- ")
		w.Kept("name")
		w.Text(": ")
		w.Kept(name)
		w.Text(nl + "  ")
		w.Kept("value")
		w.Text(`: "`)
		w.Secret()
		w.Text(`"`)
	},
	func(w *CanonWriter, name, nl string) {
		w.Text("- ")
		w.Kept("name")
		w.Text(": ")
		w.Kept(name)
		w.Text(nl + "  ")
		w.Kept("value")
		w.Text(": |" + nl + "    ")
		w.Secret()
	},
	func(w *CanonWriter, name, _ string) {
		w.Text(`{"`)
		w.Kept("name")
		w.Text(`":"`)
		w.Kept(name)
		w.Text(`","`)
		w.Kept("value")
		w.Text(`":"`)
		w.Secret()
		w.Text(`"}`)
	},
	func(w *CanonWriter, name, nl string) {
		w.Text("{" + nl + `  "`)
		w.Kept("name")
		w.Text(`": "`)
		w.Kept(name)
		w.Text(`",` + nl + `  "`)
		w.Kept("value")
		w.Text(`": "`)
		w.Secret()
		w.Text(`"` + nl + "}")
	},
	func(w *CanonWriter, name, _ string) {
		w.Text(`{"`)
		w.Kept("env")
		w.Text(`":"[{\"`)
		w.Kept("name")
		w.Text(`\":\"`)
		w.Kept(name)
		w.Text(`\",\"`)
		w.Kept("value")
		w.Text(`\":\"`)
		w.Secret()
		w.Text(`\"}]"}`)
	},
	func(w *CanonWriter, name, _ string) {
		w.Text("{")
		w.Kept("value")
		w.Text(": ")
		w.Secret()
		w.Text(", ")
		w.Kept("name")
		w.Text(": ")
		w.Kept(name)
		w.Text("}")
	},
}

// envCases writes an environment variable's name and value in neighbouring fields.
func (g *canonGen) envCases() {
	for _, name := range canonEnvNames {
		for r, render := range canonEnvRenderings {
			for _, nl := range canonNLs {
				g.add(canonFormEnv, fmt.Sprintf("%s, rendering %d, line end %q", name, r, nl), func(w *CanonWriter) {
					render(w, name, nl)
					w.next(nl)
				})
			}
		}
	}
}

// excerptCases writes a block under a secret key in a log: lines before it, the key line, one to three value
// lines, and a line after it. A cut of the log excerpt between the key line and the value lines leaves the
// values without their key.
func (g *canonGen) excerptCases() {
	rng := g.rng
	for _, key := range canonKeys {
		for li, lead := range []string{"  ", "    ", "\t", " \u00a0"} {
			for lines := 1; lines <= 3; lines++ {
				nl := []string{"\n", "\r\n"}[(li+lines)%2]
				ind := canonPick(rng, canonHeaders)
				g.add(canonFormExcerpt, fmt.Sprintf("key %s, indicator %q, lead %q, %d lines, line end %q", key, ind, lead, lines, nl), func(w *CanonWriter) {
					w.Kept("level=info msg=starting")
					w.Text(nl)
					w.Kept("config")
					w.Text(":" + nl + " ")
					w.Kept(key)
					w.Text(": " + ind)
					for range lines {
						w.Text(nl + lead)
						w.Secret()
					}
					w.next(nl)
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
			ind := canonPick(rng, canonHeaders)
			nb := canonNeighbours[(ki+ci)%len(canonNeighbours)]
			br := canonBrackets[(ki+ci)%len(canonBrackets)]
			nl := canonNLs[(ki+ci)%len(canonNLs)]
			add(canonFormMixed, fmt.Sprintf("keys on one line, indent %q, key %s, line end %q", c, key, nl), func(w *CanonWriter) {
				w.Kept(passKey)
				w.Text(": " + canonIndicators[ci%len(canonIndicators)] + " ")
				w.Kept(key)
				w.Text(": " + ind + nl + c + c)
				w.Secret()
				w.next(nl)
			})
			add(canonFormMixed, fmt.Sprintf("indent %q, bracket %q, key %s, line end %q", c, br, key, nl), func(w *CanonWriter) {
				w.Kept(key)
				w.Text(": " + ind + nl + c + c)
				w.bracketSecret([]string{br})
				w.next(nl)
			})
			add(canonFormMixed, fmt.Sprintf("indent %q, then %q, key %s, line end %q", c, nb, key, nl), func(w *CanonWriter) {
				w.Kept(key)
				w.Text(": " + ind + nl + c + c)
				w.Secret()
				w.Text(nb.before)
				w.Kept(nb.key)
				w.Text(nb.after)
				w.Secret()
				w.next(nl)
			})
			add(canonFormMixed, fmt.Sprintf("flow bracket %q, then %q, key %s, line end %q", br, nb, key, nl), func(w *CanonWriter) {
				w.Kept("retry")
				w.Text(" {")
				w.Kept(key)
				w.Text(": > ")
				w.bracketSecret([]string{br})
				w.Text(nb.before)
				w.Kept(nb.key)
				w.Text(nb.after)
				w.Secret()
				w.Text("}")
				w.next(nl)
			})
			add(canonFormMixed, fmt.Sprintf("keys on one line, flow bracket %q, key %s, line end %q", br, key, nl), func(w *CanonWriter) {
				w.Text("[")
				w.Kept(passKey)
				w.Text(": " + canonIndicators[ci%len(canonIndicators)] + " ")
				w.Kept(key)
				w.Text(": > ")
				w.bracketSecret([]string{br})
				w.Text("]")
				w.next(nl)
			})
		}
	}
	for fi, flag := range canonFlagKeys {
		for ci, c := range canonIndentChars {
			r := (fi + ci) % len(canonSeqRenderings)
			br := canonBrackets[(fi+ci)%len(canonBrackets)]
			key := []string{"password: |", flag, flag + "="}[ci%3]
			add(canonFormMixed, fmt.Sprintf("sequence %q, indent %q, rendering %d", key, c, r), func(w *CanonWriter) {
				w.sequence(r, []string{canonServe, key}, func(w *CanonWriter) {
					w.Text(c + c)
					w.Secret()
				}, "--port=8080")
			})
			add(canonFormMixed, fmt.Sprintf("sequence %q, bracket %q, then a flag, rendering %d", flag, br, r), func(w *CanonWriter) {
				w.sequence(r, []string{canonServe, flag}, func(w *CanonWriter) {
					w.bracketSecret([]string{br})
				}, "--token")
				// The value of the second flag is the last string: a sequence ends with it.
				w.C.Args = append(w.C.Args, w.secretArg(r))
			})
		}
	}
}

// canonSeqRenderings are the ways a text shows a sequence of strings: JSON on one line, a YAML flow sequence
// after a key, a YAML block sequence of quoted items, pretty-printed JSON, JSON escaped inside a string, and the
// block sequence and pretty-printed JSON with CRLF line ends.
var canonSeqRenderings = []struct{ open, sep, close, quote string }{
	{`[`, `, `, `]`, `"`},
	{canonArgsFlow, `, `, `]`, `"`},
	{"args:" + nlDash, nlDash, "\n", `"`},
	{"[\n  ", ",\n  ", "\n]", `"`},
	{`{"args":"[`, `, `, `]"}`, `\"`},
	{"args:\r\n- ", "\r\n- ", "\r\n", `"`},
	{"[\r\n  ", ",\r\n  ", "\r\n]", `"`},
}

// sequence writes the strings before, then the value string val writes, then after, in rendering r, and
// records the strings in args. Every string but the value's is text that must survive.
func (w *CanonWriter) sequence(r int, before []string, val func(w *CanonWriter), after string) {
	rd := canonSeqRenderings[r]
	w.Text(rd.open)
	for i, s := range before {
		if i > 0 {
			w.Text(rd.sep)
		}
		w.Text(rd.quote)
		w.Kept(s)
		w.Text(rd.quote)
		w.C.Args = append(w.C.Args, s)
	}
	w.Text(rd.sep + rd.quote)
	start := w.B.Len()
	val(w)
	w.C.Args = append(w.C.Args, w.B.String()[start:])
	w.Text(rd.quote + rd.sep + rd.quote)
	w.Kept(after)
	w.Text(rd.quote)
	w.C.Args = append(w.C.Args, after)
	w.Text(rd.close)
}

// secretArg writes one more string, a secret, at the end of the sequence just written in rendering r (before
// its close) and returns it. The text is rewritten: the close moves after the new string.
func (w *CanonWriter) secretArg(r int) string {
	rd := canonSeqRenderings[r]
	s := w.B.String()
	s = strings.TrimSuffix(s, rd.close)
	w.B.Reset()
	w.B.WriteString(s)
	w.Text(rd.sep + rd.quote)
	v := w.Secret()
	w.Text(rd.quote + rd.close)
	return v
}

// canonHash is the hash of an input the tables of the corpus hold: the first 8 bytes of the SHA-256 of the
// input and of its strings, in hex.
func canonHash(c CanonSource) string {
	h := sha256.New()
	h.Write([]byte(c.In))
	for _, a := range c.Args {
		h.Write([]byte{0})
		h.Write([]byte(a))
	}
	for _, l := range c.Labels {
		h.Write([]byte{1})
		h.Write([]byte(l[0]))
		h.Write([]byte{2})
		h.Write([]byte(l[1]))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}
