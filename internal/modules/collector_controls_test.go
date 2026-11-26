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

package modules

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/incidents/fingerprint"
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// The collector's readings decide as origin/main (#852, decision of 2026-11-22): fingerprint.Normalize, Symptom,
// Value, BoundValue and the fingerprint do not read the controls view of package redact. Their callers redact,
// change the text and redact again, and the added reading there showed a key's bytes main hid
// ("DB_PASS" NUL "WORD=" and a value gave "DB_PASS WORD=<redacted>" where main gave "DB_PASS <redacted>"), and a
// fingerprint must not change with this release (ADR-0039). collectorMainTable holds origin/main's outputs for
// every input of redacttest.GateCases and a few more, written at a commit of main with COLLECTOR_MAIN_TABLE set to
// the output path. This file and redacttest/controls.go compile on origin/main as they are.
//
// The shown columns (Symptom, Value, BoundValue) were written again when the text an Incident stores and a
// module's evidence value became the printed text, redacted as printed and withheld where its squeezed forms
// disagree (redact.ReplaceCheckedShortShownAndRead), because main showed a secret a control, an escape sequence or
// a line end splits there. Normalize and the fingerprint were not touched: every one of the 3,845 rows holds them as at
// the commit the table names. The table is checked by construction too: a row of a form the shown paths hide
// (redacttest.ShownInScope) whose shown column shows its probe fails (redacttest.ShowsProbe), whatever the table
// says.

// collectorMainTable is the table of origin/main's outputs.
const collectorMainTable = "testdata/controls-collector-main.tsv"

// collectorInputs are the gate's inputs, and the key a control splits with each line end the finding named.
func collectorInputs() (names, texts []string) {
	for _, c := range redacttest.GateCases() {
		names, texts = append(names, c.Name), append(texts, c.Text())
	}
	for _, sep := range []string{"\x00", "\r", "\v", "\r\n", "\x1b"} {
		names = append(names, "env key split by "+strconv.Quote(sep))
		texts = append(texts, "DB_PASS"+sep+"WORD="+redacttest.GateProbeHunter+" ok")
	}
	return names, texts
}

// collectorRegardingKind is the kind of the object the inputs are about.
const collectorRegardingKind = "Pod"

// collectorRow is what the collector's readings make of one input, each output quoted, tab separated.
func collectorRow(text string) string {
	o := fingerprint.Observation{ClusterID: "c", Namespace: "ns", OwnerUID: "uid-1", Reason: "BackOff", Type: "Warning",
		ReportingController: "kubelet", Note: text, RegardingKind: collectorRegardingKind, RegardingName: "web-abc-1"}
	return strings.Join([]string{
		strconv.Quote(fingerprint.Normalize(o)), strconv.Quote(fingerprint.Symptom(o)),
		strconv.Quote(fingerprint.Value(text)), strconv.Quote(BoundValue(text)), fingerprint.Fingerprint(o),
	}, "\t")
}

// TestWriteCollectorMainTable writes collectorMainTable when COLLECTOR_MAIN_TABLE names the output path and
// COLLECTOR_MAIN_COMMIT the commit. It refuses in a tree that holds internal/redact/controls.go: the table is
// origin/main's answer, never the branch's own.
func TestWriteCollectorMainTable(t *testing.T) {
	path := os.Getenv("COLLECTOR_MAIN_TABLE")
	if path == "" {
		t.Skip("COLLECTOR_MAIN_TABLE is not set")
	}
	if _, err := os.Stat("../redact/controls.go"); err == nil {
		t.Fatal("internal/redact/controls.go exists in this tree: the main table is written from a checkout of origin/main")
	}
	commit := os.Getenv("COLLECTOR_MAIN_COMMIT")
	if commit == "" {
		t.Fatal("COLLECTOR_MAIN_COMMIT must name the commit of the checkout")
	}
	names, texts := collectorInputs()
	var b strings.Builder
	fmt.Fprintf(&b, "# Normalize, Symptom, Value, BoundValue and the fingerprint of each input, at origin/main %s.\n", commit)
	for i := range texts {
		fmt.Fprintf(&b, "%s\t%s\n", strings.ReplaceAll(names[i], "\t", " "), collectorRow(texts[i]))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil { // #nosec G703 -- a path the person running the test names
		t.Fatal(err)
	}
}

// Every collector reading of every input equals the table: Normalize and the fingerprint as on origin/main, the
// shown columns as since the shown short entry point.
func TestCollectorReadingsDecideAsMain(t *testing.T) {
	f, err := os.Open(collectorMainTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var want []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		if line := sc.Text(); line != "" && !strings.HasPrefix(line, "#") {
			want = append(want, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	names, texts := collectorInputs()
	if len(want) != len(texts) {
		t.Fatalf("%s has %d rows, the test %d inputs", collectorMainTable, len(want), len(texts))
	}
	cases := redacttest.GateCases()
	differ, shown := 0, 0
	for i := range texts {
		name, row, _ := strings.Cut(want[i], "\t")
		if name != strings.ReplaceAll(names[i], "\t", " ") {
			t.Fatalf("%s row %d is %q, the input %q", collectorMainTable, i+1, name, names[i])
		}
		probe, inScope := redacttest.GateProbeHunter, true // the keys a control splits that the finding named
		if i < len(cases) {
			probe, inScope = cases[i].Probe, cases[i].ShownInScope()
		}
		if inScope {
			cols := strings.Split(row, "\t")
			for _, col := range cols[1:4] {
				if v, err := strconv.Unquote(col); err != nil || redacttest.ShowsProbe(v, probe) {
					if shown++; shown <= 5 {
						t.Errorf("%s row %d (%q) shows its probe in a shown column: %s", collectorMainTable, i+1,
							texts[i], col)
					}
				}
			}
		}
		if got := collectorRow(texts[i]); got != row {
			if differ++; differ <= 5 {
				t.Errorf("%s (%q):\n got %s\nmain %s", names[i], texts[i], got, row)
			}
		}
	}
	if differ > 0 {
		t.Errorf("%d of %d inputs differ from the table", differ, len(texts))
	}
	if shown > 0 {
		t.Errorf("%d shown columns of the table show their probe", shown)
	}
}
