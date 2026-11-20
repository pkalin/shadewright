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
	"encoding/json"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
)

// commaFieldsMainTable is the differential table of the comma handling of #769 (fieldAfterComma): inputs of
// commaFieldsCorpus, two key=value fields joined by a comma, with the byte ranges main redacted (commit
// 28a8212c, before #769). It was written once by running this file in a checkout of that commit with
// COMMA_FIELDS_MAIN_TABLE set to the output path; regenerate it only from that commit.
const commaFieldsMainTable = "testdata/comma-fields-main.jsonl"

// commaFirstKeys, commaSeps, commaFirstValues, commaSecondKeys, commaSecondSeps, commaSecondValues and
// commaTails are the pieces of a commaFieldsCorpus input: a first key, its separator and value, a comma, blanks
// or none, a second key (secret-named or plain), its separator and value, and the text after it.
// clientSecretKey, plainABC and nextLineTail are pieces the corpus uses more than once.
const (
	clientSecretKey = "client_secret"
	plainABC        = "abc"
	nextLineTail    = "\nnext: x"
)

var (
	commaFirstKeys   = []string{"password", "TOKEN", "API_TOKEN", "DB_PASSWORD", clientSecretKey, "user", spacedPasswordFlag, "export TOKEN"}
	commaSeps        = []string{"=", ": ", "= ", ":"}
	commaFirstValues = []string{`"abc"`, `'abc'`, `"a b"`, `'a b'`, `"a"x`, plainABC, `"abc"def`, `""`}
	commaSecondKeys  = []string{
		"DB_PASSWORD", "password", "token", "apiKey", clientSecretKey, "AWS_SECRET_ACCESS_KEY", "pass", "AUTH",
		"db.password", "x-api-key", "user", "next", "region", plainABC, "id",
	}
	commaBlanks       = []string{"", "", " ", "  ", "\t"}
	commaSecondSeps   = []string{"=", "= ", " = ", ": ", ":", "=  ", " =", ":= "}
	commaSecondValues = []string{
		`"hidden word"`, `'hidden word'`, "hidden", `"hidden"`, "hidden word", "Hidden9word", `"Hid den9"`,
		"hidden,more", `"hidden",more`,
	}
	commaTails = []string{"", nextX, ",user=bob", `"`, regionWest, nextLineTail, `", "x": 1}`}
)

// commaFieldsCorpus returns n inputs built from the pieces above with a fixed seed.
func commaFieldsCorpus(n int) []string {
	rng := rand.New(rand.NewPCG(769, 4)) //nolint:gosec // a fixed seed keeps the corpus the same on every run
	one := func(xs []string) string { return xs[rng.IntN(len(xs))] }
	out := make([]string, 0, n)
	for range n {
		var b strings.Builder
		first := one(commaFirstKeys)
		b.WriteString(first)
		if !strings.HasSuffix(first, " ") {
			b.WriteString(one(commaSeps))
		}
		b.WriteString(one(commaFirstValues))
		b.WriteString(",")
		b.WriteString(one(commaBlanks))
		b.WriteString(one(commaSecondKeys))
		b.WriteString(one(commaSecondSeps))
		b.WriteString(one(commaSecondValues))
		b.WriteString(one(commaTails))
		out = append(out, b.String())
	}
	return out
}

// TestWriteCommaFieldsMainTable writes commaFieldsMainTable from the redactor it runs with, when
// COMMA_FIELDS_MAIN_TABLE names the output path; otherwise it is skipped. Run it only in a checkout of the
// commit that commaFieldsMainTable names.
func TestWriteCommaFieldsMainTable(t *testing.T) {
	path := os.Getenv("COMMA_FIELDS_MAIN_TABLE")
	if path == "" {
		t.Skip("COMMA_FIELDS_MAIN_TABLE is not set")
	}
	f, err := os.Create(path) //nolint:gosec // a path the person running the test names
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, in := range commaFieldsCorpus(3000) {
		c := mainRedaction{In: in, Out: String(in)}
		for _, sp := range defaultRedactor.spans(in) {
			c.Spans = append(c.Spans, [2]int{sp.start, sp.end})
		}
		if err := enc.Encode(c); err != nil {
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

// Nothing main redacted is lost by the comma handling of #769: on every input of two key=value fields joined
// by a comma, each byte main redacted is still redacted, so a secret-named key after the comma keeps its
// value redacted at least as well as on main.
func TestCommaFieldsStillRedactWhatMainRedacted(t *testing.T) {
	f, err := os.Open(commaFieldsMainTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gen := commaFieldsCorpus(3000)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	cases, redacted, failed := 0, 0, 0
	for sc.Scan() {
		var c mainRedaction
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatalf("line %d: %v", cases+1, err)
		}
		cases++
		checkTableInput(t, commaFieldsMainTable, cases, c.In, gen)
		if len(c.Spans) > 0 {
			redacted++
		}
		now := defaultRedactor.spans(c.In)
	spans:
		for _, want := range c.Spans {
			for i := want[0]; i < want[1]; i++ {
				if !coveredBy([2]int{i, i + 1}, now) {
					failed++
					if failed <= 20 {
						t.Errorf("main redacted byte %d, now kept:\n in: %q\nmain: %q\n now: %q",
							i, c.In, c.Out, String(c.In))
					}
					break spans
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if cases != len(gen) {
		t.Fatalf("%s has %d lines, its corpus generator %d inputs", commaFieldsMainTable, cases, len(gen))
	}
	if failed > 0 {
		t.Errorf("%d of %d inputs lost bytes main redacted", failed, cases)
	}
	if cases < 3000 || redacted < 2000 {
		t.Fatalf("table too small: %d cases, %d redacted on main", cases, redacted)
	}
}
