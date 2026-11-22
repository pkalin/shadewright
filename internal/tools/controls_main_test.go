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

package tools

import (
	"bufio"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
)

// The entries corpus (controls_main_corpus_test.go) with origin/main as the oracle (decision of 2026-11-22 on
// #852): every case origin/main's answer hid is hidden in the answer now. Under -race a seeded eighth of the
// cases runs.
func TestEntriesHideWhatMainHid(t *testing.T) {
	f, err := os.Open(entriesMainTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rows := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" && !strings.HasPrefix(line, "#") {
			cut := strings.LastIndexByte(line, '\t')
			rows[line[:cut]] = line[cut+1:]
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	set, src := diffSet(t)
	layouts := entriesLayouts()
	cases := entriesCases()
	if raceDetector {
		rng := rand.New(rand.NewPCG(852, 2)) // #nosec G404 -- a reproducible sample, not a secret
		rng.Shuffle(len(cases), func(i, j int) { cases[i], cases[j] = cases[j], cases[i] })
		cases = cases[:len(cases)/8]
	}
	hidden := map[string]int{}
	for _, c := range cases {
		row := rows[entriesKey(c)]
		at := slices.Index(entriesControls(layouts[c.layout].all), c.ctl)
		if at < 0 || len(row) != len(entriesControls(layouts[c.layout].all)) {
			t.Fatalf("the table has no answer for %s, control %U: write it again on origin/main", entriesKey(c), c.ctl)
		}
		if row[at] != 'h' {
			continue
		}
		hidden[layouts[c.layout].name]++
		if r := layouts[c.layout].run(set, src, c.a, c.b); entriesShown(r) {
			t.Errorf("%s, form %d, place %d, control %U: origin/main hid the secret, the answer shows it: %q",
				layouts[c.layout].name, c.form, c.place, c.ctl, r.Text)
		}
	}
	t.Logf("%d cases; hidden on origin/main, by layout: %v", len(cases), hidden)
	for _, l := range layouts {
		if hidden[l.name] == 0 {
			t.Errorf("no case of %s was hidden on origin/main: the layout proves nothing", l.name)
		}
	}
}

// The label values of query_metrics that origin/main read as one answer: a NUL between a connection string and
// "---password" in one label left "--password" a flag whose value is the next label, and origin/main answered
// {arg0=[REDACTED:connection-string], ***=[credential]} (run at a9b4120f). With the NUL left out of the answer,
// "db---password" read as no flag, and the next label's value was shown, until the answer is also read as
// origin/main wrote it (#852).
func TestQueryMetricsHidesAValueAControlSeparatedFromItsFlag(t *testing.T) {
	set, src := diffSet(t)
	l := entriesLayouts()[0]
	for _, c := range entriesControls(true) {
		r := l.run(set, src, "postgres://u:p@h/db"+string(c)+"---password", entriesSecret)
		if entriesShown(r) || heldCorpusControl(r.Text) {
			t.Errorf("control %U: answer %q, want the value hidden and no control", c, r.Text)
		}
	}
}

// heldCorpusControl reports whether s holds a control character an answer is shown without.
func heldCorpusControl(s string) bool { return strings.ContainsFunc(s, corpusControl) }
