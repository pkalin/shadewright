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

// The block-indicator forms of #840 (decision of 2026-11-17): a secret key and a YAML block scalar indicator
// in the places the first corpus (blockvalue_corpus_test.go) did not reach. The key inside a JSON or logfmt
// string value, after an unbalanced "[" or "{" in prose, with no blank between the colon and the indicator,
// with a no-break or other Unicode space around the colon, with one blank before a value flattened onto the
// line, with a block less indented than the key's line, after a "---" line, with the head of a value a cut
// note follows, and inside the block of a bare short key that refuses its own value. The generator places
// every secret and every word that must survive at a known offset, without reading any rule, so the answer
// is known by construction. This file uses only String and the redactor's spans, so it runs unchanged
// against an older commit to count what that commit leaks.

// FormCase is one input of the forms corpus with the byte ranges of its secrets and of the text that must
// survive.
type FormCase struct {
	Name    string
	In      string
	Secrets [][2]int
	Keep    [][2]int
}

// FormWriter builds one FormCase.
type FormWriter struct {
	// B holds the text written so far.
	B strings.Builder
	// C holds the recorded byte ranges.
	C FormCase
	// N points to the shared synthetic secret counter.
	N *int
}

// Text appends literal text without recording a range.
func (w *FormWriter) Text(s string) { w.B.WriteString(s) }

// Secret writes the next generated secret and records its range.
func (w *FormWriter) Secret() {
	*w.N++
	start := w.B.Len()
	fmt.Fprintf(&w.B, "Qz%dxK9w", 10000+*w.N)
	w.C.Secrets = append(w.C.Secrets, [2]int{start, w.B.Len()})
}

// secretHead writes the first six bytes of the next generated secret, as a cut leaves it, and records them.
func (w *FormWriter) secretHead() {
	*w.N++
	start := w.B.Len()
	fmt.Fprintf(&w.B, "%.6s", fmt.Sprintf("Qz%dxK9w", 10000+*w.N))
	w.C.Secrets = append(w.C.Secrets, [2]int{start, w.B.Len()})
}

// Kept writes s and records that it must survive.
func (w *FormWriter) Kept(s string) {
	start := w.B.Len()
	w.B.WriteString(s)
	w.C.Keep = append(w.C.Keep, [2]int{start, w.B.Len()})
}

// formKeys are secret keys of each form the rules know, the bare short keys among them (passKey, "auth",
// "sig"), whose values count only when they look like a secret, as every generated secret does.
var formKeys = []string{
	passwordKey, dbPasswordKey, clientSecretKey, tokenKeyName, apiKeyName, credentialsKey, quotedPasswordKey, "'token'", passKey,
	authKey, sigKey, "basic_auth",
}

// formPlainKeys are formKeys without quotes, for places a quote would end the enclosing string.
var formPlainKeys = func() []string {
	var out []string
	for _, k := range formKeys {
		if !strings.ContainsAny(k, `"'`) {
			out = append(out, k)
		}
	}
	return out
}()

// formLongKeys are formKeys without the bare short keys.
var formLongKeys = func() []string {
	var out []string
	for _, k := range formKeys {
		if k != passKey && k != authKey && k != sigKey {
			out = append(out, k)
		}
	}
	return out
}()

// authKey, passKey and sigKey are the bare short key names, apiKeyName and tokenKeyName secret key names, and
// threeBlanks the blanks a flattened line break and indentation leave.
const (
	authKey      = "auth"
	passKey      = "pass"
	sigKey       = "sig"
	apiKeyName   = "apiKey"
	tokenKeyName = "token"
	threeBlanks  = "   "
)

// formBareKeys are the bare short keys the forms place a block under.
var formBareKeys = []string{authKey, passKey, sigKey}

// formIndicators are block scalar headers with chomping and indentation indicators.
var formIndicators = []string{"|", ">", "|-", ">+", "|2", ">-", "|2-", "|+"}

// formUnicodeSpaces are spaces other than ASCII ones that a message may carry around a colon: no-break,
// figure, narrow no-break, ideographic, en, thin and medium mathematical spaces, and the zero-width space,
// word joiner and byte order mark.
var formUnicodeSpaces = []string{
	"\u00a0", "\u2007", "\u202f", "\u3000", "\u2002", "\u2009", "\u205f", "\u200b", "\u2060", "\ufeff",
}

// formLeads are texts before a key on its line; the word in each must survive.
var formLeads = []struct{ word, glue string }{
	{"", ""}, {"warning", ": "}, {"config error", " "}, {"level=warn msg", "="},
}

// lead writes a lead: its word kept, then its glue.
func (w *FormWriter) lead(l struct{ word, glue string }) {
	if l.word != "" {
		w.Kept(l.word)
	}
	w.Text(l.glue)
}

// formForm is one form of the corpus: name, and the function writing an input for a key, an indicator
// and a variant index.
type formForm struct {
	name     string
	keys     []string
	variants int
	write    func(w *FormWriter, key, ind string, v int)
}

// formForms returns the forms of the corpus.
func formForms() []formForm {
	nls := []string{"\n", "\r\n"}
	return []formForm{
		{name: "key inside a JSON string", keys: formPlainKeys, variants: 8, write: func(w *FormWriter, key, ind string, v int) {
			sep := []string{" ", threeBlanks, `\n  `, `\n`, `\t`, " \\n    ", "  ", " "}[v]
			w.Text(`{"level":"warn","msg":"`)
			w.Kept("bad")
			w.Text(" config: " + key + ": " + ind + sep)
			w.Secret()
			// The text after the string is not checked: main's extent rule already takes it after an
			// escaped line break ("\n"), and every byte main redacted stays redacted.
			w.Text(`","ts":"t0","apiKey":"`)
			w.Secret()
			w.Text(`"}`)
		}},
		{name: "key inside a logfmt string", keys: formPlainKeys, variants: 3, write: func(w *FormWriter, key, ind string, v int) {
			w.Text("level=warn msg=\"")
			w.Kept("bad")
			w.Text(" config " + key + ": " + ind + []string{" ", threeBlanks, `\n  `}[v])
			w.Secret()
			w.Text("\" ")
			w.Kept("ts")
			w.Text("=1 token=")
			w.Secret()
		}},
		{name: "after an unbalanced bracket", keys: formKeys, variants: 5, write: func(w *FormWriter, key, ind string, v int) {
			w.Kept("retry")
			w.Text([]string{" [3/5 ", " {cfg ", " [[", " [3/5] [", " ({"}[v] + key + ": " + ind + threeBlanks)
			w.Secret()
		}},
		{name: "after a balanced bracket", keys: formKeys, variants: 2, write: func(w *FormWriter, key, ind string, v int) {
			w.Kept("retry")
			w.Text([]string{" [3/5] ", " {a: b} "}[v] + key + ": " + ind + threeBlanks)
			w.Secret()
		}},
		{name: "no blank before the indicator", keys: formKeys, variants: 4, write: func(w *FormWriter, key, ind string, v int) {
			w.lead(formLeads[v])
			w.Text(key + ":" + ind + "  ")
			w.Secret()
			w.Text("\n")
			w.Kept("Back-off")
		}},
		{name: "no blank before the indicator, block below", keys: formKeys, variants: 4, write: func(w *FormWriter, key, ind string, v int) {
			w.lead(formLeads[v])
			w.Text(key + ":" + ind + nls[v%2] + "  ")
			w.Secret()
			w.Text(nls[v%2])
			w.Kept("Back-off")
		}},
		{name: "one blank after the indicator", keys: formKeys, variants: 4, write: func(w *FormWriter, key, ind string, v int) {
			w.lead(formLeads[v])
			w.Text(key + ": " + ind + " ")
			w.Secret()
			w.Text("\n")
			w.Kept("Back-off")
		}},
		{name: "comment then a flattened value", keys: formKeys, variants: 4, write: func(w *FormWriter, key, ind string, v int) {
			w.lead(formLeads[v])
			w.Text(key + ": " + ind + " # rotated by CI   ")
			w.Secret()
		}},
		{name: "block less indented than the key's line", keys: formKeys, variants: 12, write: func(w *FormWriter, key, ind string, v int) {
			nl := nls[v%2]
			col := 2 + v%3*2
			w.Text(strings.Repeat(" ", col))
			if v/6 == 1 {
				w.Kept("msg")
				w.Text(": ")
			}
			w.Text(key + ": " + ind + nl + strings.Repeat(" ", 1+v/2%(col-1)))
			w.Secret()
			w.Text(nl)
			w.Kept("Back-off")
			w.Text(" restarting")
		}},
		{name: "document marker after the header", keys: formKeys, variants: 4, write: func(w *FormWriter, key, ind string, v int) {
			nl := nls[v%2]
			w.lead(formLeads[v])
			w.Text(key + ": " + ind + nl + "---" + nl + "  ")
			w.Secret()
			w.Text(nl)
			w.Kept("next")
			w.Text(": x")
		}},
		{name: "document marker flattened", keys: formKeys, variants: 4, write: func(w *FormWriter, key, ind string, v int) {
			w.lead(formLeads[v])
			w.Text(key + ": " + ind + " --- ")
			w.Secret()
		}},
		{name: "unicode space before the colon", keys: formKeys, variants: len(formUnicodeSpaces), write: func(w *FormWriter, key, ind string, v int) {
			w.Kept("x")
			w.Text(" " + key + formUnicodeSpaces[v] + ": " + ind + "   ")
			w.Secret()
		}},
		{name: "unicode spaces after the colon", keys: formKeys, variants: len(formUnicodeSpaces), write: func(w *FormWriter, key, ind string, v int) {
			w.Kept("x")
			u := formUnicodeSpaces[v]
			w.Text(" " + key + ":" + u + ind + u)
			w.Secret()
		}},
		{name: "unicode space before the colon, block below", keys: formKeys, variants: len(formUnicodeSpaces), write: func(w *FormWriter, key, ind string, v int) {
			w.Text(key + formUnicodeSpaces[v] + ": " + ind + "\n  ")
			w.Secret()
			w.Text("\n")
			w.Kept("next")
			w.Text(": x")
		}},
		// Not under a bare short key: the head of a value is too short to look like a secret, which such a key
		// asks for.
		{name: "head of a cut value", keys: formLongKeys, variants: 4, write: func(w *FormWriter, key, ind string, v int) {
			w.lead(formLeads[v])
			w.Text(key + ": " + ind + " ")
			w.secretHead()
			w.Text(" (37 bytes cut)")
		}},
		{name: "inside a refused bare block", keys: formKeys, variants: 12, write: func(w *FormWriter, key, ind string, v int) {
			nl := nls[v%2]
			bare := formBareKeys[v/2%3]
			if v/6 == 1 {
				w.Kept("x")
				w.Text(" ")
			}
			w.Text(bare + ":" + nl + "  ")
			w.Kept([]string{"mode", "note", "type"}[v%3])
			w.Text(": basic " + key + ": " + ind + nl + "    ")
			w.Secret()
			w.Text(nl)
			w.Kept("next")
			w.Text(": x")
		}},
		{name: "flattened inside a refused bare block", keys: formKeys, variants: 6, write: func(w *FormWriter, key, ind string, v int) {
			nl := nls[v%2]
			w.Text(formBareKeys[v/2] + ":" + nl + "  ")
			w.Kept("mode")
			w.Text(": basic " + key + ": " + ind + threeBlanks)
			w.Secret()
			w.Text(nl)
			w.Kept("next")
			w.Text(": x")
		}},
	}
}

// formCorpus returns every input of the forms corpus.
func formCorpus() []FormCase {
	var out []FormCase
	n := 0
	for _, f := range formForms() {
		for ki, key := range f.keys {
			for ii, ind := range formIndicators {
				// Each key meets every variant over the indicators, so the corpus stays small.
				v := (ki + ii) % f.variants
				for range 2 {
					w := &FormWriter{N: &n}
					f.write(w, key, ind, v)
					w.C.In = w.B.String()
					w.C.Name = fmt.Sprintf("%s, key %s, indicator %q, variant %d", f.name, key, ind, v)
					out = append(out, w.C)
					v = (v + f.variants/2 + 1) % f.variants
				}
			}
		}
	}
	return out
}
