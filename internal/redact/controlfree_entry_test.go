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
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/pkalin/shadewright/internal/redact"
)

// The condition of the cost of #852 (decision of 2026-11-22, ADR-0015 amendment of that date): text without
// control characters is decided at every entry point exactly as on origin/main, so the kept pieces newly hidden
// fall only on text that holds a control. controlFreeMainTable holds, for each input of redact.ControlFreeCorpus
// in order, the hash of what each entry point of origin/main returned for it; TestControlFreeTextDecidedAsOnMain
// compares the branch with it. This file and export_controlfree_test.go compile on origin/main as they are; the
// table was written there with CONTROLFREE_MAIN_TABLE set to the output path.

// controlFreeMainTable is the table of origin/main's outputs on the control-free corpus.
const controlFreeMainTable = "testdata/controlfree-main-entry.tsv"

// controlFreeEntries are the entry points the condition covers: every entry point of entryPoints (the context
// builder's value, the read tools, the log excerpt, the fingerprint and the module bound), get_resource of a
// sequence and query_metrics of a series, and the decisions of package redact that the product calls on text
// from the cluster, each with its withheld flag. A decision that takes several strings gets the lines of the
// input; SecretKey and SecretCollection decide each line as a key or a name.
func controlFreeEntries() map[string]func(c redact.ControlFreeCase) []string {
	r := defaultRedactor()
	flag := func(out string, ok bool) []string { return []string{out, strconv.FormatBool(ok)} }
	m := map[string]func(c redact.ControlFreeCase) []string{
		"redact.StringChecked": func(c redact.ControlFreeCase) []string { return flag(r.StringChecked(c.In)) },
		"redact.StringTwiceChecked": func(c redact.ControlFreeCase) []string {
			return flag(r.StringTwiceChecked(c.In, nil))
		},
		"redact.StringKeepingChecked": func(c redact.ControlFreeCase) []string {
			return flag(r.StringKeepingChecked(c.In, "hunter"))
		},
		"redact.StringsChecked": func(c redact.ControlFreeCase) []string {
			out, ok := r.StringsChecked(strings.Split(c.In, "\n"))
			return append(out, strconv.FormatBool(ok))
		},
		"redact.SecretKey": func(c redact.ControlFreeCase) []string {
			var out []string
			for l := range strings.SplitSeq(c.In, "\n") {
				out = append(out, strconv.FormatBool(redact.SecretKey(l)))
			}
			return out
		},
		"redact.SecretCollection": func(c redact.ControlFreeCase) []string {
			var out []string
			for l := range strings.SplitSeq(c.In, "\n") {
				out = append(out, strconv.FormatBool(redact.SecretCollection(l)))
			}
			return out
		},
		canonSequenceEntry: func(c redact.ControlFreeCase) []string {
			if c.Args == nil {
				return nil
			}
			return []string{getResourceOfArgs(c.Args)}
		},
		canonLabelsEntry: func(c redact.ControlFreeCase) []string {
			if c.Labels == nil {
				return nil
			}
			return []string{queryMetricsOfLabels(c.Labels)}
		},
	}
	for name, f := range entryPoints {
		m[name] = func(c redact.ControlFreeCase) []string { return f(c.In) }
	}
	return m
}

// controlFreeNames returns the names of controlFreeEntries in order.
func controlFreeNames() []string {
	entries := controlFreeEntries()
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// controlFreeHash is the hash of the outputs of one entry point for one input, as the table holds it: the first
// 3 bytes of the SHA-256 of the outputs, each with its length, in hex (a different output goes unseen once in 16
// million checks); "-" when the entry point does not take the input (no sequence, no series).
func controlFreeHash(outs []string) string {
	if outs == nil {
		return "-"
	}
	h := sha256.New()
	for _, o := range outs {
		_, _ = fmt.Fprintf(h, "%d:%s", len(o), o)
	}
	return hex.EncodeToString(h.Sum(nil)[:3])
}

// controlFreeInputHash is the hash of an input, its strings and labels included: the first 8 bytes of its
// SHA-256, in hex.
func controlFreeInputHash(c redact.ControlFreeCase) string {
	h := sha256.New()
	for _, s := range append([]string{c.In}, c.Args...) {
		_, _ = fmt.Fprintf(h, "%d:%s", len(s), s)
	}
	for _, l := range c.Labels {
		_, _ = fmt.Fprintf(h, "%d:%s%d:%s", len(l[0]), l[0], len(l[1]), l[1])
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// controlFreeCall returns the outputs of f for c. An entry point helper panics where the tool does not answer as
// the corpus tests expect (check_missing_reference answers "unknown" for a text that is no key); that answer is an
// output too, compared as the panic's text.
// The time get_pod_logs read its excerpt at is the clock of the run, so it is left out of the comparison.
func controlFreeCall(f func(c redact.ControlFreeCase) []string, c redact.ControlFreeCase) (outs []string) {
	defer func() {
		if p := recover(); p != nil {
			outs = []string{fmt.Sprint("panic: ", p)}
		}
	}()
	outs = f(c)
	for i, o := range outs {
		outs[i] = controlFreeReadAt.ReplaceAllString(o, "read at T")
	}
	return outs
}

// controlFreeReadAt is the time in the header of a log excerpt.
var controlFreeReadAt = regexp.MustCompile(`read at \d{4}-\d\d-\d\dT[0-9:.]+Z`)

// controlFreeRows runs every input whose index keep accepts through every entry point (one goroutine per entry
// point) and returns, per input, the hashes in the order of names ("" for an input not run).
func controlFreeRows(cases []redact.ControlFreeCase, names []string, keep func(name string, i int) bool) [][]string {
	entries := controlFreeEntries()
	rows := make([][]string, len(cases))
	for i := range rows {
		rows[i] = make([]string, len(names))
	}
	var wg sync.WaitGroup
	for j, name := range names {
		f := entries[name]
		wg.Go(func() {
			for i, c := range cases {
				if keep(name, i) {
					rows[i][j] = controlFreeHash(controlFreeCall(f, c))
				}
			}
		})
	}
	wg.Wait()
	return rows
}

// TestWriteControlFreeMainTable writes controlFreeMainTable from the entry points it runs with, when
// CONTROLFREE_MAIN_TABLE names the output path (and CONTROLFREE_MAIN_COMMIT the commit, for its first line); it
// is run once in a checkout of origin/main with this file and export_controlfree_test.go copied in.
func TestWriteControlFreeMainTable(t *testing.T) {
	path := os.Getenv("CONTROLFREE_MAIN_TABLE")
	if path == "" {
		t.Skip("CONTROLFREE_MAIN_TABLE is not set")
	}
	if redact.RaceDetector {
		t.Fatal("the main table needs every input: run without -race")
	}
	cases, names := redact.ControlFreeCorpus(), controlFreeNames()
	rows := controlFreeRows(cases, names, func(string, int) bool { return true })
	f, err := os.Create(path) //nolint:gosec // a path the person running the test names
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	_, _ = fmt.Fprintf(w, "# Outputs of the entry points on the control-free corpus of #852, by input, at origin/main %s.\n",
		os.Getenv("CONTROLFREE_MAIN_COMMIT"))
	_, _ = fmt.Fprintf(w, "# entry points: %s\n", strings.Join(names, ","))
	for i, c := range cases {
		_, _ = fmt.Fprintf(w, "%s\t%s\n", controlFreeInputHash(c), strings.Join(rows[i], "\t"))
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// Text without control characters is decided at every entry point exactly as on origin/main (the condition of
// the decision of 2026-11-22 on #852): every output of every entry point for every input of the control-free
// corpus hashes as main's did. Under -race each entry point checks a seeded sample (redact.RaceKeeps); make test
// runs every input without it.
func TestControlFreeTextDecidedAsOnMain(t *testing.T) {
	redact.ParallelUnderRace(t)
	b, err := os.ReadFile(controlFreeMainTable)
	if err != nil {
		t.Fatal(err)
	}
	cases, names := redact.ControlFreeCorpus(), controlFreeNames()
	var mainNames []string
	var rows [][]string
	for line := range strings.SplitSeq(strings.TrimSuffix(string(b), "\n"), "\n") {
		if list, ok := strings.CutPrefix(line, "# entry points: "); ok {
			mainNames = strings.Split(list, ",")
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		rows = append(rows, strings.Split(line, "\t"))
	}
	if !slices.Equal(mainNames, names) {
		t.Fatalf("%s has the entry points %v, the test %v: write it again at origin/main", controlFreeMainTable, mainNames, names)
	}
	if len(rows) != len(cases) {
		t.Fatalf("%s has %d rows, the corpus %d inputs", controlFreeMainTable, len(rows), len(cases))
	}
	for i, c := range cases {
		if len(rows[i]) != len(names)+1 || rows[i][0] != controlFreeInputHash(c) {
			t.Fatalf("%s row %d does not match input %d of the corpus (%s)", controlFreeMainTable, i+1, i, c.Name)
		}
	}
	got := controlFreeRows(cases, names, redact.RaceKeeps)
	entries := controlFreeEntries()
	differ, ran := map[string]int{}, 0
	for i, c := range cases {
		for j, name := range names {
			if got[i][j] == "" {
				continue
			}
			ran++
			if got[i][j] == rows[i][j+1] {
				continue
			}
			if differ[name]++; differ[name] <= 3 {
				t.Errorf("%s decides %s (input %d) unlike origin/main:\n  in:  %q\n  args: %q\n  labels: %q\n  out: %q",
					name, c.Name, i, c.In, c.Args, c.Labels, controlFreeCall(entries[name], c))
			}
		}
	}
	for _, name := range names {
		if differ[name] > 0 {
			t.Errorf("%s: %d inputs decided unlike origin/main", name, differ[name])
		}
	}
	if ran < len(cases)*len(names)/(2*redact.RaceSampleEvery) {
		t.Fatalf("only %d of %d checks ran", ran, len(cases)*len(names))
	}
	t.Logf("%d inputs, %d entry points, %d checks", len(cases), len(names), ran)
}
