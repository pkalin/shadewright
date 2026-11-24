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

package analysis

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// The controls gate of #852 through the analysis loop (decision of 2026-11-22: a new reading may only add to what
// origin/main hides). Each input of redacttest.GateCases is model text: the summary of a result, or an item of
// its missing data, the two free-text fields an analysis stores. The four-byte parts of the probe the stored text
// shows (redacttest.VisibleGrams, which reads a text with and without its controls) must be among those
// origin/main stored. modelGateMainTable holds origin/main's answer, written at a commit of main with
// MODEL_GATE_MAIN_TABLE set to the output path. This file compiles on origin/main as it is.

// modelGateMainTable is the table of origin/main's answers.
const modelGateMainTable = "testdata/controls-gate-model-main.tsv"

// modelGateBatch is the number of inputs one analysis stores: one summary and ten items of missing data.
const modelGateBatch = 11

// modelGateRows runs the inputs through the loop, modelGateBatch at a time, each as the summary in one batch and
// as an item of missing data in the next, and returns per input what the stored summary and the stored item
// showed of its probe, in hex.
func modelGateRows(t *testing.T, cases []redacttest.GateCase) [][2]string {
	t.Helper()
	rows := make([][2]string, len(cases))
	quote := func(s string) string {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for from := 0; from < len(cases); from += modelGateBatch - 1 {
		batch := cases[from:min(from+modelGateBatch, len(cases))]
		missing := make([]string, 0, len(batch)-1)
		for _, c := range batch[1:] {
			missing = append(missing, quote(c.Text()))
		}
		f := newFixture()
		f.triage.steps = []step{reply(`{"summary":` + quote(batch[0].Text()) + `,"hypotheses":[],"evidence":[],"missingData":[` +
			strings.Join(missing, ",") + `],"uncertainty":"","proposal":null}`)}
		out := f.analyze(t, testLimits())
		if len(out.MissingData) != len(batch)-1 {
			t.Fatalf("inputs %d..: %d items of missing data stored, want %d (reason %s)", from, len(out.MissingData), len(batch)-1, out.Reason)
		}
		rows[from][0] = strconv.FormatUint(redacttest.VisibleGrams(out.Summary, batch[0].Probe), 16)
		for i, c := range batch[1:] {
			rows[from+1+i][1] = strconv.FormatUint(redacttest.VisibleGrams(out.MissingData[i], c.Probe), 16)
		}
	}
	return rows
}

// TestWriteModelGateMainTable writes modelGateMainTable when MODEL_GATE_MAIN_TABLE names the output path and
// MODEL_GATE_MAIN_COMMIT the commit. It refuses in a tree that holds internal/redact/controls.go, the added
// reading of #852: the table is origin/main's answer, never the branch's own.
func TestWriteModelGateMainTable(t *testing.T) {
	path := os.Getenv("MODEL_GATE_MAIN_TABLE")
	if path == "" {
		t.Skip("MODEL_GATE_MAIN_TABLE is not set")
	}
	if _, err := os.Stat("../redact/controls.go"); err == nil {
		t.Fatal("internal/redact/controls.go exists in this tree: the main table is written from a checkout of origin/main")
	}
	commit := os.Getenv("MODEL_GATE_MAIN_COMMIT")
	if commit == "" {
		t.Fatal("MODEL_GATE_MAIN_COMMIT must name the commit of the checkout")
	}
	cases := redacttest.GateCases()
	rows := modelGateRows(t, cases)
	var b strings.Builder
	fmt.Fprintf(&b, "# What the stored summary and missing data showed of the probe of each input of the controls gate of #852, at origin/main %s.\n", commit)
	for i, c := range cases {
		fmt.Fprintf(&b, "%s\t%s\t%s\n", c.Name, rows[i][0], rows[i][1])
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil { // #nosec G703 -- a path the person running the test names
		t.Fatal(err)
	}
}

// No stored summary or item of missing data shows a four-byte part of a probe that origin/main did not store
// (decision of 2026-11-22 on #852).
func TestModelGateHidesWhatMainHid(t *testing.T) {
	f, err := os.Open(modelGateMainTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var want [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" && !strings.HasPrefix(line, "#") {
			want = append(want, strings.Split(line, "\t"))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	cases := redacttest.GateCases()
	if len(want) != len(cases) {
		t.Fatalf("%s has %d rows, the gate %d inputs", modelGateMainTable, len(want), len(cases))
	}
	got := modelGateRows(t, cases)
	shows, hidOnMain := 0, 0
	for i, c := range cases {
		if len(want[i]) != 3 || want[i][0] != c.Name {
			t.Fatalf("%s row %d does not match the input %q", modelGateMainTable, i+1, c.Name)
		}
		for k, field := range []string{"the summary", "the missing data"} {
			if got[i][k] == "" && want[i][k+1] == "" {
				continue
			}
			m, errM := strconv.ParseUint(want[i][k+1], 16, 64)
			b, errB := strconv.ParseUint(got[i][k], 16, 64)
			if errM != nil || errB != nil {
				t.Fatalf("row %d: %v %v", i+1, errM, errB)
			}
			if m != uint64(1)<<(len(c.Probe)-3)-1 {
				hidOnMain++
			}
			if b&^m != 0 {
				if shows++; shows <= 3 {
					t.Errorf("%s stored shows more of %q than origin/main on %s: main %s, now %s", field, c.Probe, c.Name, want[i][k+1], got[i][k])
				}
			}
		}
	}
	if shows > 0 {
		t.Errorf("%d inputs show more than origin/main", shows)
	}
	if hidOnMain == 0 {
		t.Fatal("origin/main hid no part of any probe: the table is not read")
	}
	t.Logf("%d inputs, %d where origin/main hid a part of the probe", len(cases), hidOnMain)
}
