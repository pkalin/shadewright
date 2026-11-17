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
)

// The forms of the second round of #845 (decision of 2026-11-17): a key and its value split over more than two
// strings of a sequence, a key written with format characters or a colon look-alike, an explicit YAML key, a
// comment at column 0 inside a block, an environment entry in block form with its value first, and the labels
// of a metric series. Like canonical_gen_test.go, this file uses only String and the redactor's spans, so it
// runs unchanged in a checkout of main.

// canonSeqPart is one piece of a string of a sequence: text that must survive, text that may go (a blank, a
// line break), or the secret.
type canonSeqPart struct {
	kept, text string
}

// canonSplitKeys are keys and flags whose value is split from them over more strings: each item list is the
// strings before the value. A blank string, a line break at the end of the key's string, a flag after other
// words in one string, and the colon or equals sign in a string of its own.
var canonSplitKeys = func() [][][]canonSeqPart {
	k := func(s string) canonSeqPart { return canonSeqPart{kept: s} }
	x := func(s string) canonSeqPart { return canonSeqPart{text: s} }
	var out [][][]canonSeqPart
	for _, f := range canonFlagKeys {
		out = append(out,
			[][]canonSeqPart{{k(f)}, {}},
			[][]canonSeqPart{{k(f)}, {x(" ")}},
			[][]canonSeqPart{{k(f)}, {x("\n")}},
			[][]canonSeqPart{{k(f), x("\n")}},
			[][]canonSeqPart{{k(f), x(" ")}},
			[][]canonSeqPart{{k("app"), x(" "), k(f)}},
			[][]canonSeqPart{{k("app"), x(" "), k(f)}, {}},
		)
	}
	for _, key := range canonKeys {
		if strings.ContainsAny(key, `"'`) || canonBareKey(key) {
			continue
		}
		out = append(out,
			[][]canonSeqPart{{k(key)}, {k(":")}},
			[][]canonSeqPart{{k(key)}, {k("=")}},
			[][]canonSeqPart{{k(key)}, {k(":")}, {}},
			[][]canonSeqPart{{k(key), x(" ")}, {k(":")}},
			[][]canonSeqPart{{k(key + " :")}},
			[][]canonSeqPart{{k(key + ":"), x("\n")}},
			[][]canonSeqPart{{k(key + ": |")}, {}},
			[][]canonSeqPart{{k(key)}, {k(": |")}},
		)
	}
	return out
}()

// sequenceParts writes "serve", the strings of items, then the value strings (each written by one of vals),
// then after, in rendering r, and records the strings in args.
func (w *canonWriter) sequenceParts(r int, items [][]canonSeqPart, vals []func(w *canonWriter), after string) {
	rd := canonSeqRenderings[r]
	w.text(rd.open + rd.quote)
	w.kept(canonServe)
	w.text(rd.quote)
	w.c.args = append(w.c.args, canonServe)
	item := func(f func()) {
		w.text(rd.sep + rd.quote)
		start := w.b.Len()
		f()
		w.c.args = append(w.c.args, w.b.String()[start:])
		w.text(rd.quote)
	}
	for _, it := range items {
		item(func() {
			for _, p := range it {
				if p.kept != "" {
					w.kept(p.kept)
				}
				w.text(p.text)
			}
		})
	}
	for _, v := range vals {
		item(func() { v(w) })
	}
	item(func() { w.kept(after) })
	w.text(rd.close)
}

// splitSequenceCases writes a key and its value split over more than two strings of a sequence, in each
// rendering, and a block value over two strings under a key with a block indicator.
func (g *canonGen) splitSequenceCases() {
	value := func(w *canonWriter) { w.secret() }
	indented := func(w *canonWriter) {
		w.text("  ")
		w.secret()
	}
	for ki, items := range canonSplitKeys {
		for r := range canonSeqRenderings {
			if r == 4 && slices.ContainsFunc(items, func(it []canonSeqPart) bool {
				return slices.ContainsFunc(it, func(p canonSeqPart) bool { return strings.Contains(p.text, "\n") })
			}) {
				// JSON escaped inside a string holds no raw line break in a string.
				continue
			}
			g.add(canonFormSequence, fmt.Sprintf("split key %d %q, rendering %d", ki, canonSeqText(items), r), func(w *canonWriter) {
				w.sequenceParts(r, items, []func(*canonWriter){value}, "--port=8080")
			})
		}
	}
	for _, key := range []string{passwordKey, clientSecretKey, canonTokenKey} {
		for r := range canonSeqRenderings {
			g.add(canonFormSequence, fmt.Sprintf("%s block over two strings, rendering %d", key, r), func(w *canonWriter) {
				w.sequenceParts(r, [][]canonSeqPart{{{kept: key + ": |"}}}, []func(*canonWriter){indented, indented}, "--port=8080")
			})
		}
	}
}

// canonSeqText returns the strings of items as one text, for a case's name.
func canonSeqText(items [][]canonSeqPart) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		var b strings.Builder
		for _, p := range it {
			b.WriteString(p.kept + p.text)
		}
		out = append(out, b.String())
	}
	return out
}

// canonFormatChars are format characters (Unicode category Cf) and the soft hyphen, which a reader does not
// see inside a word.
var canonFormatChars = []string{"\u200b", "\u200c", "\u200d", "\u2060", "\ufeff", "\u00ad", "\u180e"}

// canonColonLookalikes are the characters Unicode's confusables list maps to ":" that a key and value can be
// written with: the fullwidth colon of CJK text, the small and the vertical presentation forms, the ratio sign
// and the modifier letter colon.
var canonColonLookalikes = []string{"\uff1a", "\ufe55", "\ufe13", "\u2236", "\ua789"}

// canonGlyphKeys are the keys of the glyph form, plain identifiers so that a character can go inside them.
var canonGlyphKeys = strings.Fields("password DB_PASSWORD client_secret token apiKey credentials")

// glyphCases writes a secret key with a format character inside it, and a key followed by a colon look-alike,
// inline, in a block, after "=" and in JSON.
func (g *canonGen) glyphCases() {
	for ki, key := range canonGlyphKeys {
		for fi, fc := range canonFormatChars {
			at := 1 + (ki+fi)%(len(key)-1)
			written := key[:at] + fc + key[at:]
			for v := range 4 {
				nl := canonNLs[(ki+fi+v)%len(canonNLs)]
				g.add(canonFormGlyph, fmt.Sprintf("key %q, shape %d, line end %q", written, v, nl), func(w *canonWriter) {
					switch v {
					case 0:
						w.kept(key[:at])
						w.text(fc)
						w.kept(key[at:])
						w.text(": ")
						w.secret()
					case 1:
						w.kept(key[:at])
						w.text(fc)
						w.kept(key[at:])
						w.text(": " + canonIndicators[(ki+fi)%len(canonIndicators)] + nl + "  ")
						w.secret()
					case 2:
						w.kept(key[:at])
						w.text(fc)
						w.kept(key[at:])
						w.text("=")
						w.secret()
					case 3:
						w.text(`{"`)
						w.kept(key[:at])
						w.text(fc)
						w.kept(key[at:])
						w.text(`": "`)
						w.secret()
						w.text(`"}`)
					}
					w.next(nl)
				})
			}
		}
		for ci, c := range canonColonLookalikes {
			for v := range 4 {
				nl := canonNLs[(ki+ci+v)%len(canonNLs)]
				g.add(canonFormGlyph, fmt.Sprintf("key %s, colon %q, shape %d, line end %q", key, c, v, nl), func(w *canonWriter) {
					w.kept(key)
					switch v {
					case 0:
						w.text(c + " ")
					case 1:
						w.text(c)
					case 2:
						w.text(" " + c + " ")
					case 3:
						w.text(c + " " + canonIndicators[(ki+ci)%len(canonIndicators)] + nl + "  ")
					}
					w.secret()
					w.next(nl)
				})
			}
		}
	}
}

// explicitKeyCases writes a secret key as an explicit YAML key ("? password") and its value on the next line,
// inline or as a block, at column 0, indented, in a sequence entry and quoted.
func (g *canonGen) explicitKeyCases() {
	for ki, key := range canonKeys {
		if canonBareKey(key) {
			continue
		}
		for v := range 5 {
			nl := canonNLs[(ki+v)%len(canonNLs)]
			ind := canonIndicators[(ki+v)%len(canonIndicators)]
			g.add(canonFormExplicit, fmt.Sprintf("key %s, shape %d, line end %q", key, v, nl), func(w *canonWriter) {
				switch v {
				case 0:
					w.text("? ")
					w.kept(key)
					w.text(nl + ": ")
					w.secret()
				case 1:
					w.text("? ")
					w.kept(key)
					w.text(nl + ": " + ind + nl + "  ")
					w.secret()
				case 2:
					w.kept("spec")
					w.text(":" + nl + "  ? ")
					w.kept(key)
					w.text(nl + "  : ")
					w.secret()
				case 3:
					w.text("- ? ")
					w.kept(key)
					w.text(nl + "  : ")
					w.secret()
				case 4:
					w.text("?   ")
					w.kept(key)
					w.text(nl + ":   " + ind + nl + "    ")
					w.secret()
				}
				w.next(nl)
			})
		}
	}
}

// commentCases writes a block under a secret key with a comment line at column 0 between its lines.
func (g *canonGen) commentCases() {
	for ki, key := range canonKeys {
		for v := range 4 {
			nl := canonNLs[(ki+v)%len(canonNLs)]
			ind := canonIndicators[(ki+v)%len(canonIndicators)]
			g.add(canonFormComment, fmt.Sprintf("key %s, %q, shape %d, line end %q", key, ind, v, nl), func(w *canonWriter) {
				lead := "  "
				if v == 2 {
					w.kept("spec")
					w.text(":" + nl + lead)
					lead += lead
				}
				w.kept(key)
				w.text(": " + ind + nl + lead)
				w.secret()
				w.text(nl)
				switch v {
				case 1:
					w.text(nl)
				case 3:
					w.text("#" + nl)
				}
				if v != 3 {
					w.text("# ")
					w.kept("rotated")
					w.text(nl)
				}
				w.text(lead)
				w.secret()
				w.next(nl)
			})
		}
	}
}

// envValueFirstBlockCases writes a Kubernetes environment entry in block form whose value comes before its name.
func (g *canonGen) envValueFirstBlockCases() {
	for ni, name := range canonEnvNames {
		for v := range 6 {
			nl := canonNLs[(ni+v)%len(canonNLs)]
			g.add(canonFormEnv, fmt.Sprintf("%s, value first in block form, shape %d, line end %q", name, v, nl), func(w *canonWriter) {
				lead := ""
				if v == 3 {
					w.kept("env")
					w.text(":" + nl)
					lead = "  "
				}
				w.text(lead + "- ")
				w.kept("value")
				switch v {
				case 1:
					w.text(`: "`)
					w.secret()
					w.text(`"`)
				case 2:
					w.text(": |" + nl + lead + strings.Repeat(" ", 4))
					w.secret()
				default:
					w.text(": ")
					w.secret()
				}
				if v == 4 {
					w.text(nl + lead + "  ")
					w.kept("description")
					w.text(": ")
					w.kept("db")
				}
				w.text(nl + lead + "  ")
				w.kept("name")
				w.text(": ")
				if v == 5 {
					w.text(`"`)
					w.kept(name)
					w.text(`"`)
				} else {
					w.kept(name)
				}
				w.next(nl)
			})
		}
	}
}

// canonLabelKeys are secret keys as metric label names.
var canonLabelKeys = strings.Fields("password api_token DB_PASSWORD client_secret token apiKey secret auth")

// labelCases writes the labels of a metric series as query_metrics shows them: "{name=value, ...}", sorted by
// name, with a secret key whose value is a secret, next to a second secret label and next to labels that must
// stay.
func (g *canonGen) labelCases() {
	for ki, key := range canonLabelKeys {
		second := canonLabelKeys[(ki+1)%len(canonLabelKeys)]
		for v := range 4 {
			g.add(canonFormLabels, fmt.Sprintf("label %s, shape %d", key, v), func(w *canonWriter) {
				type label struct {
					name   string
					secret bool
					value  string
				}
				var ls []label
				switch v {
				case 0:
					ls = []label{{name: key, secret: true}}
				case 1:
					ls = []label{{name: key, secret: true}, {name: second, secret: true}}
				case 2:
					ls = []label{{name: key, secret: true}, {name: "namespace", value: "shop"}, {name: "pod", value: "web-abc-1"}}
				case 3:
					ls = []label{{name: key, secret: true}, {name: "msg", value: "retry"}, {name: second, secret: true}}
				}
				slices.SortFunc(ls, func(a, b label) int { return strings.Compare(a.name, b.name) })
				w.text("{")
				for i, l := range ls {
					if i > 0 {
						w.text(", ")
					}
					w.kept(l.name)
					w.text("=")
					if l.secret {
						l.value = w.secret()
					} else {
						w.kept(l.value)
					}
					w.c.labels = append(w.c.labels, [2]string{l.name, l.value})
				}
				w.text("}")
			})
		}
	}
}
