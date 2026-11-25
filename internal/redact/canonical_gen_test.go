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

	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// canonCase is one input of the corpus. in is the text as a caller reads it. args, for a sequence, holds its
// strings as a container's arguments hold them; in is then one rendering of that sequence. labels, for a
// metric series, holds its labels in the order query_metrics shows them (sorted by name); in is then the
// series as it shows them. secrets and keep are byte ranges of in: each secret range is a piece of a secret
// that must not appear in any output, each keep range a piece of text (a key, a flag, a word around the
// secret) that must survive.
type canonCase struct {
	form    string
	name    string
	in      string
	args    []string
	labels  [][2]string
	secrets [][2]int
	keep    [][2]int
}

type canonWriter struct {
	b      *strings.Builder
	shared *redacttest.CanonWriter
}

func (w *canonWriter) text(s string) { w.shared.Text(s) }
func (w *canonWriter) secret()       { w.shared.Secret() }
func (w *canonWriter) kept(s string) { w.shared.Kept(s) }

type canonGen struct {
	rng *rand.Rand
	n   int
	out []canonCase
}

func (g *canonGen) add(form, name string, f func(w *canonWriter)) { //nolint:unparam // retains the builder signature used by the rune corpus
	shared := &redacttest.CanonWriter{N: &g.n}
	w := &canonWriter{b: &shared.B, shared: shared}
	f(w)
	shared.C.Form, shared.C.Name, shared.C.In = form, form+": "+name, shared.B.String()
	g.out = append(g.out, canonCaseFrom(shared.C))
}
func (g *canonGen) sepRun() string { return redacttest.CanonSeparatorRun(g.rng) }
func canonCaseFrom(c redacttest.CanonSource) canonCase {
	return canonCase{
		form:    c.Form,
		name:    c.Name,
		in:      c.In,
		args:    c.Args,
		labels:  c.Labels,
		secrets: c.Secrets,
		keep:    c.Keep}
}
func canonSourceFrom(c canonCase) redacttest.CanonSource {
	return redacttest.CanonSource{
		Form:    c.form,
		Name:    c.name,
		In:      c.in,
		Args:    c.args,
		Labels:  c.labels,
		Secrets: c.secrets,
		Keep:    c.keep}
}
func canonCorpus() []canonCase {
	cases := redacttest.FullCanonCorpus()
	out := make([]canonCase, 0, len(cases))
	for _, c := range cases {
		out = append(out, canonCaseFrom(c))
	}
	return out
}
func canonHash(c canonCase) string { return redacttest.CanonHash(canonSourceFrom(c)) }

var canonForms = redacttest.CanonForms()
var canonPhraseWords = redacttest.CanonPhraseWords()
var canonFlagKeys = redacttest.CanonFlagKeys()

const canonPartialMin = redacttest.CanonPartialMin
const canonTokenKey = "token"
const canonServe = "serve"

func canonPick(rng *rand.Rand, xs []string) string { return redacttest.CanonPick(rng, xs) }

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

// canonMainTable holds, for each input of the corpus in order, its hash and the byte ranges main redacted
// (commit 7e5cedb1). It was written once by running TestWriteCanonMainTable in a checkout of that commit with
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
		for _, sp := range defaultRedactor.spans(nil, c.in) {
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

const canonArgsFlow = "args: ["
