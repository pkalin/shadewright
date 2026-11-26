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

package redact_test

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/shadewright-labs/shadewright/internal/redact"
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// The unchanged table of the shown view (#1100, decision of 2026-11-23): text that is valid UTF-8 is decided
// exactly as before by every entry point, and the collector's short entry points decide text with invalid bytes
// exactly as before too. invalidSameTable holds, for each input, a hash of each entry point's outputs at the
// commit before the shown view; it was written with INVALID_SAME_TABLE set to the output path. This file
// compiles on that commit as it is. The columns of the collector's shown text (fingerprint.Symptom,
// fingerprint.Value, modules.BoundValue and incidentcontext.Value of the symptom) were written again when that text
// became the printed text, redacted as printed and withheld where its squeezed forms disagree
// (redact.ReplaceCheckedShortShownAndRead); every other column, fingerprint.Normalize included, is as at that commit.
// TestStoredSymptomOmitsSecretsSplitByControls (package incidents) checks the shown text on a generated corpus.

// invalidSameTable is the table of the outputs before the shown view.
const invalidSameTable = "testdata/invalid-utf8-same.tsv"

// invalidSameInput is one input of the table, and whether it is read by the collector's entry points only.
type invalidSameInput struct {
	text      string
	collector bool
}

// invalidSameInputs returns the inputs of the table: valid UTF-8 (each sample, each case of the invalid bytes
// corpus made valid with "?" and with U+FFFD, and each case of the controls gate that is valid UTF-8) for every
// entry point, and each case of the corpus as it is for the collector's.
func invalidSameInputs() []invalidSameInput {
	var out []invalidSameInput
	seen := map[string]bool{}
	add := func(s string, collector bool) {
		k := fmt.Sprint(collector) + s
		if !seen[k] {
			seen[k] = true
			out = append(out, invalidSameInput{text: s, collector: collector})
		}
	}
	for _, s := range redacttest.Samples() {
		add(s.Text, false)
	}
	cases := invalidCases()
	for _, c := range cases {
		add(strings.ToValidUTF8(c.text, "?"), false)
		add(strings.ToValidUTF8(c.text, "�"), false)
	}
	for _, c := range redacttest.GateCases() {
		if t := c.Text(); utf8.ValidString(t) {
			add(t, false)
		}
	}
	for _, c := range cases {
		add(c.text, true)
	}
	return out
}

// invalidSameCollector are the collector's short entry points, which never read the shown view.
var invalidSameCollector = func() []string {
	out := []string{"redact.StringShort", "redact.ReplaceCheckedShort"}
	for name := range entryPoints {
		if strings.HasPrefix(name, "fingerprint.") || strings.HasPrefix(name, "modules.") || strings.HasSuffix(name, " of the symptom") {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}()

// invalidSameEntries are the entry points of the table, by name: every entry point of the corpus (the pieces
// ones each on every answer of pieces, none left out), the decisions of package redact on one text, and the
// collector's.
func invalidSameEntries() map[string]func(s string) []string {
	r := defaultRedactor()
	m := maps.Clone(entryPoints)
	cut := func(s string, k int) int {
		at := len(s) * k / 3
		for at > 0 && at < len(s) && !utf8.RuneStart(s[at]) {
			at--
		}
		return at
	}
	layouts := func(s string) [][]redact.Piece {
		forms := []redact.Form{redact.AsIs, redact.Line, redact.JSON}
		out := make([][]redact.Piece, 0, 3*len(forms))
		for _, form := range forms {
			out = append(out,
				[]redact.Piece{{Text: s, Data: true, Form: form}},
				[]redact.Piece{{Text: s[:cut(s, 1)], Data: true, Form: form}, {Text: s[cut(s, 1):], Data: true, Form: form}},
				[]redact.Piece{{Text: s[:cut(s, 1)], Data: true, Form: form}, {Text: s[cut(s, 1):cut(s, 2)], Data: true, Form: form},
					{Text: s[cut(s, 2):], Data: true, Form: form}})
		}
		return out
	}
	m["redact.Pieces"] = func(s string) []string {
		out := make([]string, 0, 18)
		for _, ps := range layouts(s) {
			out = append(out, r.Pieces(ps)...)
		}
		return out
	}
	m["redact.PiecesWithin"] = func(s string) []string {
		var out []string
		for _, ps := range layouts(s) {
			got, ok := r.PiecesWithin(ps, len(s)/2)
			out = append(append(out, got...), fmt.Sprint(ok))
		}
		return out
	}
	m["redact.PiecesFitting"] = func(s string) []string {
		var out []string
		for _, ps := range layouts(s) {
			got, ok := r.PiecesFitting(ps, func(o []string) bool { return len(strings.Join(o, "")) <= len(s) })
			out = append(append(out, got...), fmt.Sprint(ok))
		}
		return out
	}
	for _, name := range []string{entryRenderLog, entryModelText} {
		f := invalidEntries()[name]
		m[name] = func(s string) []string { return f(invalidCase{text: s}) }
	}
	m[gateEntryString] = func(s string) []string { return []string{r.String(s)} }
	m["redact.StringTwice"] = func(s string) []string { return []string{r.StringTwice(s, nil)} }
	m["redact.StringKeeping"] = func(s string) []string { return []string{r.StringKeeping(s, "hunter")} }
	m["redact.ReplaceChecked"] = func(s string) []string {
		out, ok := r.ReplaceChecked(s, gateMark, gateSame)
		return []string{out, fmt.Sprint(ok)}
	}
	m["redact.Strings"] = func(s string) []string { return r.Strings([]string{s[:cut(s, 1)], s[cut(s, 1):]}) }
	m["redact.StringShort"] = func(s string) []string { return []string{r.StringShort(s)} }
	m["redact.ReplaceCheckedShort"] = func(s string) []string {
		out, ok := r.ReplaceCheckedShort(s, gateMark, gateSame)
		return []string{out, fmt.Sprint(ok)}
	}
	return m
}

// invalidSameNames returns the names of invalidSameEntries in order.
func invalidSameNames() []string {
	m := invalidSameEntries()
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// invalidSameReadAt is the time get_pod_logs says it read the log at, which the hash leaves out.
var invalidSameReadAt = regexp.MustCompile(`read at [0-9T:Z-]+`)

// invalidSameHash is the first 4 bytes of the SHA-256 of the strings, in hex, without the time of a read.
func invalidSameHash(ss ...string) string {
	h := sha256.New()
	for _, s := range ss {
		s = invalidSameReadAt.ReplaceAllString(s, "read at T")
		_, _ = fmt.Fprintf(h, "%d:%s", len(s), s)
	}
	return hex.EncodeToString(h.Sum(nil)[:4])
}

// invalidSameRows runs the inputs keep accepts through the entry points that read them, one goroutine per entry
// point: per input, the hash of each entry point's outputs in the order of names, "-" for an entry point that
// does not read it and "" for one not run.
func invalidSameRows(in []invalidSameInput, names []string, keep func(name string, i int) bool) [][]string {
	entries := invalidSameEntries()
	rows := make([][]string, len(in))
	for i := range rows {
		rows[i] = make([]string, len(names))
	}
	var wg sync.WaitGroup
	for j, name := range names {
		f, collector := entries[name], slices.Contains(invalidSameCollector, name)
		wg.Go(func() {
			for i, c := range in {
				switch {
				case c.collector && !collector:
					rows[i][j] = "-"
				case keep(name, i):
					rows[i][j] = invalidSameHash(f(c.text)...)
				}
			}
		})
	}
	wg.Wait()
	return rows
}

// TestWriteInvalidSameTable writes invalidSameTable when INVALID_SAME_TABLE names the output path and
// INVALID_SAME_COMMIT the commit of the checkout. It refuses in a tree that reads the shown view.
func TestWriteInvalidSameTable(t *testing.T) {
	path := os.Getenv("INVALID_SAME_TABLE")
	if path == "" {
		t.Skip("INVALID_SAME_TABLE is not set")
	}
	if redact.RaceDetector {
		t.Fatal("the table needs every input: run without -race")
	}
	if b, err := os.ReadFile("controls.go"); err != nil || strings.Contains(string(b), "shownViewOf") {
		t.Fatal("this tree reads the shown view: the table is written at the commit before it")
	}
	commit := os.Getenv("INVALID_SAME_COMMIT")
	if commit == "" {
		t.Fatal("INVALID_SAME_COMMIT must name the commit of the checkout")
	}
	in, names := invalidSameInputs(), invalidSameNames()
	rows := invalidSameRows(in, names, func(string, int) bool { return true })
	f, err := os.Create(path) //nolint:gosec // a path the person running the test names
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	_, _ = fmt.Fprintf(w, "# A hash of each entry point's outputs for each input of the shown view's unchanged table, at %s.\n", commit)
	_, _ = fmt.Fprintf(w, "# entry points: %s\n", strings.Join(names, ","))
	for i, c := range in {
		_, _ = fmt.Fprintf(w, "%s\t%s\n", invalidSameHash(fmt.Sprint(c.collector), c.text), strings.Join(rows[i], "\t"))
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// Text that is valid UTF-8 is decided exactly as before the shown view by every entry point, and text with
// invalid bytes by the collector's short entry points (decision of 2026-11-23): each output is byte for byte the
// one of the table, whose columns of the collector's shown text are as since that text is the printed text. Under
// -race each entry point checks a seeded sample (redact.RaceKeeps).
func TestShownViewLeavesValidTextAndTheCollectorAsBefore(t *testing.T) {
	redact.ParallelUnderRace(t)
	in, names := invalidSameInputs(), invalidSameNames()
	tableNames, want := readGateTable(t, invalidSameTable)
	if !slices.Equal(tableNames, names) {
		t.Fatalf("%s has the entry points %v, the test %v: write it again", invalidSameTable, tableNames, names)
	}
	if len(want) != len(in) {
		t.Fatalf("%s has %d rows, the test %d inputs", invalidSameTable, len(want), len(in))
	}
	for i, c := range in {
		if len(want[i]) != len(names)+1 || want[i][0] != invalidSameHash(fmt.Sprint(c.collector), c.text) {
			t.Fatalf("%s row %d does not match input %d", invalidSameTable, i+1, i)
		}
	}
	got := invalidSameRows(in, names, redact.RaceKeeps)
	changed, ran, collector := map[string]int{}, 0, 0
	for i, c := range in {
		for j, name := range names {
			if got[i][j] == "" || got[i][j] == "-" {
				continue
			}
			ran++
			if c.collector {
				collector++
			}
			if got[i][j] == want[i][j+1] {
				continue
			}
			if changed[name]++; changed[name] <= 3 {
				t.Errorf("%s decides %q otherwise than before (collector input %t)", name, c.text, c.collector)
			}
		}
	}
	for _, name := range names {
		if changed[name] > 0 {
			t.Errorf("%s: %d inputs decided otherwise than before", name, changed[name])
		}
	}
	if collector == 0 {
		t.Fatal("no input with invalid bytes ran through the collector's entry points")
	}
	t.Logf("%d inputs, %d entry points, %d checks, %d of them the collector's on invalid bytes", len(in), len(names), ran, collector)
}
