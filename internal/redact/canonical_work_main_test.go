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
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The cost of main's code on the inputs of the cost tests, so that each figure the docs give for main is
// printed by a test, next to the figure of this code (#845, second decision of 2026-11-18). The table is
// written by TestWriteCanonMainWork in a checkout of main with the tag redactwork; this file and
// canonical_work_test.go use only String and the corpus, so they run unchanged there.

// canonMainWork holds main's work on each measure: "name<TAB>steps<TAB>bytes".
const canonMainWork = "testdata/canonical-main-work.tsv"

// canonWorkMeasures returns the measures of the cost tests by name, each a function that returns its work
// and its size.
func canonWorkMeasures(t *testing.T) (names []string, measures map[string]func() (int64, int)) {
	cases := canonCorpus()
	measures = map[string]func() (int64, int){}
	add := func(name string, f func() (int64, int)) {
		names = append(names, name)
		measures[name] = f
	}
	add("corpus", func() (int64, int) {
		total, size := int64(0), 0
		for _, c := range cases {
			total += countWork(t, func() { String(c.in) })
			size += len(c.in)
		}
		return total, size
	})
	sizes := map[string]int{}
	for _, c := range cases {
		sizes[c.form] += len(c.in) + 1
	}
	for _, form := range canonForms {
		n := max(16<<10, sizes[form])
		add("form "+form+" once", func() (int64, int) { return canonWork(t, canonFormText(cases, form, n)) })
		add("form "+form+" eight times", func() (int64, int) { return canonWork(t, canonFormText(cases, form, 8*n)) })
	}
	for _, n := range []int{64 << 10, 256 << 10, 1 << 20} {
		add(fmt.Sprintf("stair %q %d", canonStairLines[0], n), func() (int64, int) {
			return canonWork(t, canonStair(canonStairLines[0], n))
		})
	}
	for _, n := range []int{512, 1024, 2048} {
		add(fmt.Sprintf("repeated quoted keys %d", n), func() (int64, int) { return canonWork(t, repeatedQuotedKeys(n)) })
	}
	return names, measures
}

// mainWork is main's work on one measure.
type mainWork struct {
	steps int64
	bytes int
}

// perByte is the work per byte.
func (w mainWork) perByte() float64 { return float64(w.steps) / float64(w.bytes) }

// readCanonMainWork reads canonMainWork: main's work by measure, and the commit its first line names.
func readCanonMainWork(t *testing.T) (map[string]mainWork, string) {
	t.Helper()
	f, err := os.Open(canonMainWork)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]mainWork{}
	commit := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if c, ok := strings.CutPrefix(line, "# commit "); ok {
			commit = c
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			t.Fatalf("%s: malformed line %q", canonMainWork, line)
		}
		steps, err1 := strconv.ParseInt(fields[1], 10, 64)
		size, err2 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || size == 0 {
			t.Fatalf("%s: malformed line %q", canonMainWork, line)
		}
		out[fields[0]] = mainWork{steps: steps, bytes: size}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out, commit
}

// logMainWork logs main's work on the measure name next to this code's, and fails when the table has no row
// for it or one for a text of another size (the corpus changed since the table was written).
func logMainWork(t *testing.T, name string, steps int64, size int) {
	t.Helper()
	main, commit := readCanonMainWork(t)
	w, ok := main[name]
	switch {
	case !ok:
		t.Errorf("%s has no row %q: write it again in a checkout of main", canonMainWork, name)
	case w.bytes != size:
		t.Errorf("%s: %q measured %d bytes on main, %d now: write it again in a checkout of main", canonMainWork, name, w.bytes, size)
	default:
		t.Logf("%s: %.1f steps per byte, %.1f on main %s (%.2f times)", name, float64(steps)/float64(size), w.perByte(),
			commit, float64(steps)/float64(size)/w.perByte())
	}
}

// Every measure has a row of main's work, and every row is a measure.
func TestCanonMainWorkTableIsWhole(t *testing.T) {
	names, _ := canonWorkMeasures(t)
	main, commit := readCanonMainWork(t)
	if commit == "" {
		t.Errorf("%s names no commit", canonMainWork)
	}
	for _, n := range names {
		if _, ok := main[n]; !ok {
			t.Errorf("%s has no row %q", canonMainWork, n)
		}
	}
	if len(main) != len(names) {
		t.Errorf("%s has %d rows, the cost tests have %d measures", canonMainWork, len(main), len(names))
	}
}

// TestWriteCanonMainWork writes canonMainWork from the code it runs with, when CANON_MAIN_WORK names the output
// path (and CANON_MAIN_COMMIT the commit); it is run once with the tag redactwork in a checkout of main.
func TestWriteCanonMainWork(t *testing.T) {
	path := os.Getenv("CANON_MAIN_WORK")
	if path == "" {
		t.Skip("CANON_MAIN_WORK is not set")
	}
	names, measures := canonWorkMeasures(t)
	var b strings.Builder
	b.WriteString("# The work (steps, -tags redactwork) of String on the inputs of the cost tests (#845), on main.\n")
	b.WriteString("# commit " + os.Getenv("CANON_MAIN_COMMIT") + "\n")
	for _, n := range names {
		steps, size := measures[n]()
		if steps == 0 {
			t.Fatal("no work counted: run with -tags redactwork")
		}
		fmt.Fprintf(&b, "%s\t%d\t%d\n", n, steps, size)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil { // #nosec G703 -- a path the person running the test names
		t.Fatal(err)
	}
}
