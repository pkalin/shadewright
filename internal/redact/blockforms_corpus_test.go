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
	"os"
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// formCase is one input of the forms corpus with the byte ranges of its secrets and of the text that must
// survive.
type formCase struct {
	name    string
	in      string
	secrets [][2]int
	keep    [][2]int
}

// formCorpus forwards to the shared generator for the existing redaction tests.
func formCorpus() []formCase {
	cases := redacttest.FormCorpus()
	out := make([]formCase, 0, len(cases))
	for _, c := range cases {
		out = append(out, formCase{
			name:    c.Name,
			in:      c.In,
			secrets: c.Secrets,
			keep:    c.Keep})
	}
	return out
}

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

var formKeys = redacttest.FormKeys()
var formIndicators = redacttest.FormIndicators()

// formWriter delegates the small builders used by the flow marker tests.
type formWriter struct {
	b      *strings.Builder
	c      formCase
	n      *int
	shared *redacttest.FormWriter
}

func (w *formWriter) init() {
	if w.shared == nil {
		w.shared = &redacttest.FormWriter{N: w.n}
		w.b = &w.shared.B
	}
}
func (w *formWriter) syncRanges()   { w.c.secrets = w.shared.C.Secrets; w.c.keep = w.shared.C.Keep }
func (w *formWriter) text(s string) { w.init(); w.shared.Text(s) }
func (w *formWriter) secret()       { w.init(); w.shared.Secret(); w.syncRanges() }
func (w *formWriter) kept(s string) { w.init(); w.shared.Kept(s); w.syncRanges() }
