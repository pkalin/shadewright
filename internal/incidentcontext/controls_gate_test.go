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

package incidentcontext

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/shadewright-labs/shadewright/internal/incidents/fingerprint"
	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/providers/fake"
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// The controls gate of #852 through the context builder (decision of 2026-11-22: a new reading may only add to
// what origin/main hides). Each input of redacttest.GateCases is the message of the crashing container (a status
// message), its previous log, the value and the name of a metric label, and the note the Incident's symptom was
// made from (fingerprint.Symptom); the four-byte parts of the probe the context shows (redacttest.VisibleGrams)
// must be among those origin/main's context showed, and the symptom line must be origin/main's, byte for byte
// (the context's value of the symptom decides as main). buildGateMainTable holds origin/main's answer, written at a commit of main with BUILD_GATE_MAIN_TABLE set to the output path. This
// file compiles on origin/main as it is.

// buildGateMainTable is the table of origin/main's answers.
const buildGateMainTable = "testdata/controls-gate-build-main.tsv"

// buildGateText builds the context of one input and returns its text.
func buildGateText(t testing.TB, c redacttest.GateCase) string {
	s := webSources()
	key := fake.LogKey{Namespace: testNS, Pod: crashPod, Container: appName, Previous: true}
	s.logs.Set(key, "starting\n"+c.Text()+"\n")
	s.logs.SetInstance(key, providers.LogInstance{PodUID: "uid-web-abc-1", RestartCount: 4}, false)
	labels := map[string]string{labelPod: crashPod, labelContainer: appName, "note": c.Text()}
	if c.Key != "" {
		labels[c.Key] = c.Value
	}
	for _, p := range metricPlans {
		s.metrics.Set(p.template, []providers.Series{{Labels: labels, Samples: []providers.Sample{{Time: testNow, Value: 1}}}})
	}
	objs := webObjects()
	crashing(objs[2].(*corev1.Pod), c.Text())
	b := newBuilder(t, reader(t, objs...), s, nil)
	inc := incident(deploymentRef())
	inc.Spec.Symptom = fingerprint.Symptom(fingerprint.Observation{Note: c.Text()})
	return contextText(build(t, b, inc))
}

// symptomLine returns the line of text that shows the symptom, quoted.
func symptomLine(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, "symptom: ") {
			return strconv.Quote(line)
		}
	}
	return `""`
}

// buildGateRows builds the inputs keep accepts, in parallel, and returns what each context showed of its probe in
// hex and its symptom line, quoted, separated by a tab ("" for an input not run).
func buildGateRows(t testing.TB, cases []redacttest.GateCase, keep func(i int) bool) []string {
	rows := make([]string, len(cases))
	var wg sync.WaitGroup
	next := make(chan int)
	for range 8 {
		wg.Go(func() {
			for i := range next {
				text := buildGateText(t, cases[i])
				rows[i] = strconv.FormatUint(redacttest.VisibleGrams(text, cases[i].Probe), 16) + "\t" + symptomLine(text)
			}
		})
	}
	for i := range cases {
		if keep(i) {
			next <- i
		}
	}
	close(next)
	wg.Wait()
	return rows
}

// TestWriteBuildGateMainTable writes buildGateMainTable when BUILD_GATE_MAIN_TABLE names the output path and
// BUILD_GATE_MAIN_COMMIT the commit. It refuses in a tree that holds internal/redact/controls.go, the added
// reading of #852: the table is origin/main's answer, never the branch's own.
func TestWriteBuildGateMainTable(t *testing.T) {
	path := os.Getenv("BUILD_GATE_MAIN_TABLE")
	if path == "" {
		t.Skip("BUILD_GATE_MAIN_TABLE is not set")
	}
	if raceDetector {
		t.Fatal("the main table needs every input: run without -race")
	}
	if _, err := os.Stat("../redact/controls.go"); err == nil {
		t.Fatal("internal/redact/controls.go exists in this tree: the main table is written from a checkout of origin/main")
	}
	commit := os.Getenv("BUILD_GATE_MAIN_COMMIT")
	if commit == "" {
		t.Fatal("BUILD_GATE_MAIN_COMMIT must name the commit of the checkout")
	}
	cases := redacttest.GateCases()
	rows := buildGateRows(t, cases, func(int) bool { return true })
	var b strings.Builder
	fmt.Fprintf(&b, "# What the context showed of the probe of each input of the controls gate of #852, and its symptom line, at origin/main %s.\n", commit)
	for i, c := range cases {
		fmt.Fprintf(&b, "%s\t%s\n", strings.ReplaceAll(c.Name, "\t", " "), rows[i])
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil { // #nosec G703 -- a path the person running the test names
		t.Fatal(err)
	}
}

// No context shows a four-byte part of a probe that origin/main's context did not show (decision of 2026-11-22 on
// #852). Under -race it checks a seeded sample (racesample_test.go).
func TestBuildGateHidesWhatMainHid(t *testing.T) {
	f, err := os.Open(buildGateMainTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var want [][3]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" && !strings.HasPrefix(line, "#") {
			name, rest, _ := strings.Cut(line, "\t")
			bits, symptom, _ := strings.Cut(rest, "\t")
			want = append(want, [3]string{name, bits, symptom})
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	cases := redacttest.GateCases()
	if len(want) != len(cases) {
		t.Fatalf("%s has %d rows, the gate %d inputs", buildGateMainTable, len(want), len(cases))
	}
	built := func(i int) bool { return i%buildGateEvery == 0 && raceKeeps(t.Name(), i) }
	got := buildGateRows(t, cases, built)
	shows, ran, hidOnMain, symptoms := 0, 0, 0, 0
	for i, c := range cases {
		if want[i][0] != c.Name {
			t.Fatalf("%s row %d is %q, the input %q", buildGateMainTable, i+1, want[i][0], c.Name)
		}
		if got[i] == "" {
			continue
		}
		ran++
		gotBits, gotSymptom, _ := strings.Cut(got[i], "\t")
		if gotSymptom != want[i][2] {
			if symptoms++; symptoms <= 3 {
				t.Errorf("the symptom line differs from origin/main's on %s:\n got %s\nmain %s", c.Name, gotSymptom, want[i][2])
			}
		}
		m, errM := strconv.ParseUint(want[i][1], 16, 64)
		b, errB := strconv.ParseUint(gotBits, 16, 64)
		if errM != nil || errB != nil {
			t.Fatalf("row %d: %v %v", i+1, errM, errB)
		}
		if m != uint64(1)<<(len(c.Probe)-3)-1 {
			hidOnMain++
		}
		if b&^m != 0 {
			if shows++; shows <= 3 {
				t.Errorf("the context shows more of %q than origin/main on %s: main %s, now %s", c.Probe, c.Name, want[i][1], got[i])
			}
		}
	}
	if shows > 0 {
		t.Errorf("%d inputs show more than origin/main", shows)
	}
	if symptoms > 0 {
		t.Errorf("%d symptom lines differ from origin/main's", symptoms)
	}
	if !raceDetector {
		for _, missing := range redacttest.GateUncovered(cases, built) {
			t.Errorf("the gate builds no input with %s", missing)
		}
	}
	if hidOnMain == 0 {
		t.Fatal("origin/main hid no part of any probe: the table is not read")
	}
	t.Logf("%d inputs, %d built, %d where origin/main hid a part of the probe", len(cases), ran, hidOnMain)
}

// buildGateEvery is the share of the inputs the test builds: one in buildGateEvery, so the test stays near five
// seconds (a build reads every source); the table holds every input. It is coprime to the inputs of one form and
// boundary (redacttest.GateCases: 48), so the sample takes every filler, with and without the "log: " prefix, at
// many forms and boundaries; the test fails when one is never built (redacttest.GateUncovered).
const buildGateEvery = 7
