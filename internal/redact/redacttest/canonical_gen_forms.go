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
	"slices"
	"strings"
)

// The forms of the second round of #845 (decision of 2026-11-17): a key and its value split over more than two
// strings of a sequence, a key written with format characters or a colon look-alike, an explicit YAML key, a
// comment at column 0 inside a block, an environment entry in block form with its value first, and the labels
// of a metric series. The generator is independent of the redactor.

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
func (w *CanonWriter) sequenceParts(r int, items [][]canonSeqPart, vals []func(w *CanonWriter), after string) { //nolint:unparam // every generated sequence ends with the same flag, text that must stay
	rd := canonSeqRenderings[r]
	w.Text(rd.open + rd.quote)
	w.Kept(canonServe)
	w.Text(rd.quote)
	w.C.Args = append(w.C.Args, canonServe)
	item := func(f func()) {
		w.Text(rd.sep + rd.quote)
		start := w.B.Len()
		f()
		w.C.Args = append(w.C.Args, w.B.String()[start:])
		w.Text(rd.quote)
	}
	for _, it := range items {
		item(func() {
			for _, p := range it {
				if p.kept != "" {
					w.Kept(p.kept)
				}
				w.Text(p.text)
			}
		})
	}
	for _, v := range vals {
		item(func() { v(w) })
	}
	item(func() { w.Kept(after) })
	w.Text(rd.close)
}

// splitSequenceCases writes a key and its value split over more than two strings of a sequence, in each
// rendering, and a block value over two strings under a key with a block indicator.
func (g *canonGen) splitSequenceCases() {
	value := func(w *CanonWriter) { w.Secret() }
	indented := func(w *CanonWriter) {
		w.Text("  ")
		w.Secret()
	}
	for ki, items := range canonSplitKeys {
		for r := range canonSeqRenderings {
			if r == 4 && slices.ContainsFunc(items, func(it []canonSeqPart) bool {
				return slices.ContainsFunc(it, func(p canonSeqPart) bool { return strings.Contains(p.text, "\n") })
			}) {
				// JSON escaped inside a string holds no raw line break in a string.
				continue
			}
			g.add(canonFormSequence, fmt.Sprintf("split key %d %q, rendering %d", ki, canonSeqText(items), r), func(w *CanonWriter) {
				w.sequenceParts(r, items, []func(*CanonWriter){value}, "--port=8080")
			})
		}
	}
	for _, key := range []string{passwordKey, clientSecretKey, canonTokenKey} {
		for r := range canonSeqRenderings {
			g.add(canonFormSequence, fmt.Sprintf("%s block over two strings, rendering %d", key, r), func(w *CanonWriter) {
				w.sequenceParts(r, [][]canonSeqPart{{{kept: key + ": |"}}}, []func(*CanonWriter){indented, indented}, "--port=8080")
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
				g.add(canonFormGlyph, fmt.Sprintf("key %q, shape %d, line end %q", written, v, nl), func(w *CanonWriter) {
					switch v {
					case 0:
						w.Kept(key[:at])
						w.Text(fc)
						w.Kept(key[at:])
						w.Text(": ")
						w.Secret()
					case 1:
						w.Kept(key[:at])
						w.Text(fc)
						w.Kept(key[at:])
						w.Text(": " + canonIndicators[(ki+fi)%len(canonIndicators)] + nl + "  ")
						w.Secret()
					case 2:
						w.Kept(key[:at])
						w.Text(fc)
						w.Kept(key[at:])
						w.Text("=")
						w.Secret()
					case 3:
						w.Text(`{"`)
						w.Kept(key[:at])
						w.Text(fc)
						w.Kept(key[at:])
						w.Text(`": "`)
						w.Secret()
						w.Text(`"}`)
					}
					w.next(nl)
				})
			}
		}
		for ci, c := range canonColonLookalikes {
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
			g.add(canonFormExplicit, fmt.Sprintf("key %s, shape %d, line end %q", key, v, nl), func(w *CanonWriter) {
				switch v {
				case 0:
					w.Text("? ")
					w.Kept(key)
					w.Text(nl + ": ")
					w.Secret()
				case 1:
					w.Text("? ")
					w.Kept(key)
					w.Text(nl + ": " + ind + nl + "  ")
					w.Secret()
				case 2:
					w.Kept("spec")
					w.Text(":" + nl + "  ? ")
					w.Kept(key)
					w.Text(nl + "  : ")
					w.Secret()
				case 3:
					w.Text("- ? ")
					w.Kept(key)
					w.Text(nl + "  : ")
					w.Secret()
				case 4:
					w.Text("?   ")
					w.Kept(key)
					w.Text(nl + ":   " + ind + nl + "    ")
					w.Secret()
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
			g.add(canonFormComment, fmt.Sprintf("key %s, %q, shape %d, line end %q", key, ind, v, nl), func(w *CanonWriter) {
				lead := "  "
				if v == 2 {
					w.Kept("spec")
					w.Text(":" + nl + lead)
					lead += lead
				}
				w.Kept(key)
				w.Text(": " + ind + nl + lead)
				w.Secret()
				w.Text(nl)
				switch v {
				case 1:
					w.Text(nl)
				case 3:
					w.Text("#" + nl)
				}
				if v != 3 {
					w.Text("# ")
					w.Kept("rotated")
					w.Text(nl)
				}
				w.Text(lead)
				w.Secret()
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
			g.add(canonFormEnv, fmt.Sprintf("%s, value first in block form, shape %d, line end %q", name, v, nl), func(w *CanonWriter) {
				lead := ""
				if v == 3 {
					w.Kept("env")
					w.Text(":" + nl)
					lead = "  "
				}
				w.Text(lead + "- ")
				w.Kept("value")
				switch v {
				case 1:
					w.Text(`: "`)
					w.Secret()
					w.Text(`"`)
				case 2:
					w.Text(": |" + nl + lead + strings.Repeat(" ", 4))
					w.Secret()
				default:
					w.Text(": ")
					w.Secret()
				}
				if v == 4 {
					w.Text(nl + lead + "  ")
					w.Kept("description")
					w.Text(": ")
					w.Kept("db")
				}
				w.Text(nl + lead + "  ")
				w.Kept("name")
				w.Text(": ")
				if v == 5 {
					w.Text(`"`)
					w.Kept(name)
					w.Text(`"`)
				} else {
					w.Kept(name)
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
			g.add(canonFormLabels, fmt.Sprintf("label %s, shape %d", key, v), func(w *CanonWriter) {
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
				w.Text("{")
				for i, l := range ls {
					if i > 0 {
						w.Text(", ")
					}
					w.Kept(l.name)
					w.Text("=")
					if l.secret {
						l.value = w.Secret()
					} else {
						w.Kept(l.value)
					}
					w.C.Labels = append(w.C.Labels, [2]string{l.name, l.value})
				}
				w.Text("}")
			})
		}
	}
}
