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
	"os"
	"strings"
	"testing"
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

// formCase is one input of the forms corpus with the byte ranges of its secrets and of the text that must
// survive.
type formCase struct {
	name    string
	in      string
	secrets [][2]int
	keep    [][2]int
}

// formWriter builds one formCase.
type formWriter struct {
	b strings.Builder
	c formCase
	n *int
}

func (w *formWriter) text(s string) { w.b.WriteString(s) }

// secret writes the next generated secret and records its range.
func (w *formWriter) secret() {
	*w.n++
	start := w.b.Len()
	fmt.Fprintf(&w.b, "Qz%dxK9w", 10000+*w.n)
	w.c.secrets = append(w.c.secrets, [2]int{start, w.b.Len()})
}

// secretHead writes the first six bytes of the next generated secret, as a cut leaves it, and records them.
func (w *formWriter) secretHead() {
	*w.n++
	start := w.b.Len()
	fmt.Fprintf(&w.b, "%.6s", fmt.Sprintf("Qz%dxK9w", 10000+*w.n))
	w.c.secrets = append(w.c.secrets, [2]int{start, w.b.Len()})
}

// kept writes s and records that it must survive.
func (w *formWriter) kept(s string) {
	start := w.b.Len()
	w.b.WriteString(s)
	w.c.keep = append(w.c.keep, [2]int{start, w.b.Len()})
}

// formKeys are secret keys of each form the rules know, the bare short keys among them ("pass", "auth",
// "sig"), whose values count only when they look like a secret, as every generated secret does.
var formKeys = []string{
	passwordKey, dbPasswordKey, clientSecretKey, tokenKeyName, apiKeyName, "credentials", `"password"`, "'token'", passKey,
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
func (w *formWriter) lead(l struct{ word, glue string }) {
	if l.word != "" {
		w.kept(l.word)
	}
	w.text(l.glue)
}

// formForm is one form of the corpus: name, and the function writing an input for a key, an indicator
// and a variant index.
type formForm struct {
	name     string
	keys     []string
	variants int
	write    func(w *formWriter, key, ind string, v int)
}

// formForms returns the forms of the corpus.
func formForms() []formForm {
	nls := []string{"\n", "\r\n"}
	return []formForm{
		{name: "key inside a JSON string", keys: formPlainKeys, variants: 8, write: func(w *formWriter, key, ind string, v int) {
			sep := []string{" ", threeBlanks, `\n  `, `\n`, `\t`, " \\n    ", "  ", " "}[v]
			w.text(`{"level":"warn","msg":"`)
			w.kept("bad")
			w.text(" config: " + key + ": " + ind + sep)
			w.secret()
			// The text after the string is not checked: main's extent rule already takes it after an
			// escaped line break ("\n"), and every byte main redacted stays redacted.
			w.text(`","ts":"t0","apiKey":"`)
			w.secret()
			w.text(`"}`)
		}},
		{name: "key inside a logfmt string", keys: formPlainKeys, variants: 3, write: func(w *formWriter, key, ind string, v int) {
			w.text("level=warn msg=\"")
			w.kept("bad")
			w.text(" config " + key + ": " + ind + []string{" ", threeBlanks, `\n  `}[v])
			w.secret()
			w.text("\" ")
			w.kept("ts")
			w.text("=1 token=")
			w.secret()
		}},
		{name: "after an unbalanced bracket", keys: formKeys, variants: 5, write: func(w *formWriter, key, ind string, v int) {
			w.kept("retry")
			w.text([]string{" [3/5 ", " {cfg ", " [[", " [3/5] [", " ({"}[v] + key + ": " + ind + threeBlanks)
			w.secret()
		}},
		{name: "after a balanced bracket", keys: formKeys, variants: 2, write: func(w *formWriter, key, ind string, v int) {
			w.kept("retry")
			w.text([]string{" [3/5] ", " {a: b} "}[v] + key + ": " + ind + threeBlanks)
			w.secret()
		}},
		{name: "no blank before the indicator", keys: formKeys, variants: 4, write: func(w *formWriter, key, ind string, v int) {
			w.lead(formLeads[v])
			w.text(key + ":" + ind + "  ")
			w.secret()
			w.text("\n")
			w.kept("Back-off")
		}},
		{name: "no blank before the indicator, block below", keys: formKeys, variants: 4, write: func(w *formWriter, key, ind string, v int) {
			w.lead(formLeads[v])
			w.text(key + ":" + ind + nls[v%2] + "  ")
			w.secret()
			w.text(nls[v%2])
			w.kept("Back-off")
		}},
		{name: "one blank after the indicator", keys: formKeys, variants: 4, write: func(w *formWriter, key, ind string, v int) {
			w.lead(formLeads[v])
			w.text(key + ": " + ind + " ")
			w.secret()
			w.text("\n")
			w.kept("Back-off")
		}},
		{name: "comment then a flattened value", keys: formKeys, variants: 4, write: func(w *formWriter, key, ind string, v int) {
			w.lead(formLeads[v])
			w.text(key + ": " + ind + " # rotated by CI   ")
			w.secret()
		}},
		{name: "block less indented than the key's line", keys: formKeys, variants: 12, write: func(w *formWriter, key, ind string, v int) {
			nl := nls[v%2]
			col := 2 + v%3*2
			w.text(strings.Repeat(" ", col))
			if v/6 == 1 {
				w.kept("msg")
				w.text(": ")
			}
			w.text(key + ": " + ind + nl + strings.Repeat(" ", 1+v/2%(col-1)))
			w.secret()
			w.text(nl)
			w.kept("Back-off")
			w.text(" restarting")
		}},
		{name: "document marker after the header", keys: formKeys, variants: 4, write: func(w *formWriter, key, ind string, v int) {
			nl := nls[v%2]
			w.lead(formLeads[v])
			w.text(key + ": " + ind + nl + "---" + nl + "  ")
			w.secret()
			w.text(nl)
			w.kept("next")
			w.text(": x")
		}},
		{name: "document marker flattened", keys: formKeys, variants: 4, write: func(w *formWriter, key, ind string, v int) {
			w.lead(formLeads[v])
			w.text(key + ": " + ind + " --- ")
			w.secret()
		}},
		{name: "unicode space before the colon", keys: formKeys, variants: len(formUnicodeSpaces), write: func(w *formWriter, key, ind string, v int) {
			w.kept("x")
			w.text(" " + key + formUnicodeSpaces[v] + ": " + ind + "   ")
			w.secret()
		}},
		{name: "unicode spaces after the colon", keys: formKeys, variants: len(formUnicodeSpaces), write: func(w *formWriter, key, ind string, v int) {
			w.kept("x")
			u := formUnicodeSpaces[v]
			w.text(" " + key + ":" + u + ind + u)
			w.secret()
		}},
		{name: "unicode space before the colon, block below", keys: formKeys, variants: len(formUnicodeSpaces), write: func(w *formWriter, key, ind string, v int) {
			w.text(key + formUnicodeSpaces[v] + ": " + ind + "\n  ")
			w.secret()
			w.text("\n")
			w.kept("next")
			w.text(": x")
		}},
		// Not under a bare short key: the head of a value is too short to look like a secret, which such a key
		// asks for.
		{name: "head of a cut value", keys: formLongKeys, variants: 4, write: func(w *formWriter, key, ind string, v int) {
			w.lead(formLeads[v])
			w.text(key + ": " + ind + " ")
			w.secretHead()
			w.text(" (37 bytes cut)")
		}},
		{name: "inside a refused bare block", keys: formKeys, variants: 12, write: func(w *formWriter, key, ind string, v int) {
			nl := nls[v%2]
			bare := formBareKeys[v/2%3]
			if v/6 == 1 {
				w.kept("x")
				w.text(" ")
			}
			w.text(bare + ":" + nl + "  ")
			w.kept([]string{"mode", "note", "type"}[v%3])
			w.text(": basic " + key + ": " + ind + nl + "    ")
			w.secret()
			w.text(nl)
			w.kept("next")
			w.text(": x")
		}},
		{name: "flattened inside a refused bare block", keys: formKeys, variants: 6, write: func(w *formWriter, key, ind string, v int) {
			nl := nls[v%2]
			w.text(formBareKeys[v/2] + ":" + nl + "  ")
			w.kept("mode")
			w.text(": basic " + key + ": " + ind + threeBlanks)
			w.secret()
			w.text(nl)
			w.kept("next")
			w.text(": x")
		}},
	}
}

// formCorpus returns every input of the forms corpus.
func formCorpus() []formCase {
	var out []formCase
	n := 0
	for _, f := range formForms() {
		for ki, key := range f.keys {
			for ii, ind := range formIndicators {
				// Each key meets every variant over the indicators, so the corpus stays small.
				v := (ki + ii) % f.variants
				for range 2 {
					w := &formWriter{n: &n}
					f.write(w, key, ind, v)
					w.c.in = w.b.String()
					w.c.name = fmt.Sprintf("%s, key %s, indicator %q, variant %d", f.name, key, ind, v)
					out = append(out, w.c)
					v = (v + f.variants/2 + 1) % f.variants
				}
			}
		}
	}
	return out
}

// formLeaks returns the secrets of c that keep a byte, and the kept ranges that lose one, under spans.
func formLeaks(c formCase, spans []span) (leaked, lost []string) {
	red := formRedacted(c.in, spans)
	for _, r := range c.secrets {
		for i := r[0]; i < r[1]; i++ {
			if !red[i] {
				leaked = append(leaked, c.in[r[0]:r[1]])
				break
			}
		}
	}
	for _, r := range c.keep {
		for i := r[0]; i < r[1]; i++ {
			if red[i] {
				lost = append(lost, c.in[r[0]:r[1]])
				break
			}
		}
	}
	return leaked, lost
}

// Every secret of the forms corpus is redacted and the text around it survives. At b2d4d3eb (the first
// build of #840) 1,900 of the 3,152 inputs leaked a secret or lost kept text; on main (0dcfec7a) 2,736.
func TestBlockFormsCorpusRedactsEverySecret(t *testing.T) {
	// A corpus test that counts no work: it runs beside the others (#873).
	t.Parallel()
	cases := formCorpus()
	failed := 0
	byForm := map[string]int{}
	for i, c := range cases {
		if !raceKeeps(t.Name(), i) {
			continue // a seeded sample under -race; all of them without it
		}
		leaked, lost := formLeaks(c, defaultRedactor.spans(nil, c.in))
		if len(leaked)+len(lost) == 0 {
			continue
		}
		failed++
		form, _, _ := strings.Cut(c.name, ", key")
		byForm[form]++
		if failed <= 20 {
			t.Errorf("%s:\n  in: %q\n out: %q\n leaked %q, lost %q", c.name, c.in, String(c.in), leaked, lost)
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d forms corpus inputs leak a secret or lose kept text, by form: %v", failed, len(cases), byForm)
	}
}

// formMainTable holds, for each input of the forms corpus in order, its hash and the byte ranges main
// redacted (commit 0dcfec7a). It was written once by running TestWriteFormMainTable in a checkout of that
// commit with FORM_MAIN_TABLE set to the output path; regenerate it only from that commit.
const formMainTable = "testdata/blockforms-main.jsonl"

// formMainRow is one line of formMainTable.
type formMainRow struct {
	Hash  string   `json:"h"`
	Spans [][2]int `json:"s,omitempty"`
}

// TestWriteFormMainTable writes formMainTable from the redactor it runs with, when FORM_MAIN_TABLE names
// the output path; otherwise it is skipped.
func TestWriteFormMainTable(t *testing.T) {
	path := os.Getenv("FORM_MAIN_TABLE")
	if path == "" {
		t.Skip("FORM_MAIN_TABLE is not set")
	}
	corpus := formCorpus()
	rows := make([]any, 0, len(corpus))
	for _, c := range corpus {
		row := formMainRow{Hash: formHash(c.in)}
		for _, sp := range defaultRedactor.spans(nil, c.in) {
			row.Spans = append(row.Spans, [2]int{sp.start, sp.end})
		}
		rows = append(rows, row)
	}
	writeJSONLines(t, path, rows)
}

// writeJSONLines writes rows to path, one JSON value per line.
func writeJSONLines(t *testing.T, path string, rows []any) {
	t.Helper()
	f, err := os.Create(path) //nolint:gosec // a path the person running the test names
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// readJSONLines reads path, one JSON value per line, calling row for each with its line number.
func readJSONLines(t *testing.T, path string, row func(n int, line []byte)) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	n := 0
	for sc.Scan() {
		row(n, sc.Bytes())
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

// formHash is the hash of an input that the main tables hold: the first 8 bytes of its SHA-256, in hex.
func formHash(in string) string {
	sum := sha256.Sum256([]byte(in))
	return hex.EncodeToString(sum[:8])
}

// formRedacted returns, for each byte of s, whether one of spans covers it.
func formRedacted(s string, spans []span) []bool {
	out := make([]bool, len(s))
	for _, sp := range spans {
		for i := sp.start; i < sp.end; i++ {
			out[i] = true
		}
	}
	return out
}

// Each byte main redacted in the forms corpus is still redacted.
func TestBlockFormsStillRedactWhatMainRedacted(t *testing.T) {
	// A corpus test that counts no work: it runs beside the others (#873).
	t.Parallel()
	gen := formCorpus()
	failed := 0
	n := readJSONLines(t, formMainTable, func(i int, line []byte) {
		var row formMainRow
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		if i >= len(gen) || formHash(gen[i].in) != row.Hash {
			t.Fatalf("%s line %d does not match its generator: regenerate it from the commit it names", formMainTable, i+1)
		}
		if !raceKeeps(t.Name(), i) {
			return // a seeded sample under -race; all of them without it
		}
		in := gen[i].in
		red := formRedacted(in, defaultRedactor.spans(nil, in))
		for _, want := range row.Spans {
			for j := want[0]; j < want[1]; j++ {
				if !red[j] {
					failed++
					if failed <= 20 {
						t.Errorf("main redacted %q, now kept:\n in: %q\n now: %q", in[want[0]:want[1]], in, String(in))
					}
					break
				}
			}
		}
	})
	if n != len(gen) {
		t.Fatalf("%s has %d lines, the corpus %d inputs", formMainTable, n, len(gen))
	}
	if failed > 0 {
		t.Errorf("%d ranges main redacted are kept now", failed)
	}
}
