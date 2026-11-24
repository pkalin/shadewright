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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/shadewright-labs/shadewright/internal/incidentcontext"
	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/redact"
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
	"github.com/shadewright-labs/shadewright/internal/tools"
)

// The controls gate of #852 (decision of 2026-11-22): a new reading may only add to what origin/main hides. Every
// input of redacttest.GateCases, and the four inputs the counts of that day found, goes through every entry point
// of cluster text in this package's reach (the decisions of package redact, the forms the log source uses, the
// answer of pieces, the context builder's value and log excerpt, and the read tools; the short entry points of the
// collector do not read the controls view and are checked against main byte for byte in internal/modules); for
// each, the four-byte parts of the probe the output shows (redacttest.VisibleGrams) must be among those
// origin/main's output showed. gateMainTable holds origin/main's answer; it was written at a commit of main with
// CONTROLS_GATE_MAIN_TABLE set to the output path. This file compiles on origin/main as it is.

// gateMainTable is the table of origin/main's answers.
const gateMainTable = "testdata/controls-gate-main.tsv"

// gateEntryString is the entry point redact.String in the gate's tables.
const gateEntryString = "redact.String"

// gateInput is one input of the gate: a generated case, or one of the four known inputs with its own pieces.
type gateInput struct {
	redacttest.GateCase
	// pieces is an answer of pieces the known input was found in; the pieces entry points read it, and the
	// text entry points read the case's text when it has parts.
	pieces []redact.Piece
}

// gateSeeds are the four inputs of the counts of 2026-11-22 on #852 (the text, the pieces or the key each was
// found in), each with its probe.
func gateSeeds() []gateInput {
	zw, value, hunter := "\u200b", redacttest.GateProbeValue, redacttest.GateProbeHunter
	seed := func(name string, probe string, parts []string, ps ...redact.Piece) gateInput {
		return gateInput{GateCase: redacttest.GateCase{Name: "seed " + name, Parts: parts, Probe: probe, Value: strings.Join(parts, "")}, pieces: ps}
	}
	kv := seed("key password ZW CR blank", value, []string{"password" + zw + "\r ", value})
	kv.Key, kv.Value = "password"+zw+"\r ", value
	data := func(text string, form redact.Form, key string) redact.Piece {
		return redact.Piece{Text: text, Data: true, Form: form, Key: key}
	}
	tool := func(text string) redact.Piece { return redact.Piece{Text: text} }
	out := make([]gateInput, 0, 7)
	out = append(out,
		seed("CR before Bearer", "Qz7secretValue1", []string{"x\rBearer Qz7secretValue1"}),
		seed("flag split by ESC as Line", value, nil, tool("args: "), data("--pass"+zw+"word\x1b"+value, redact.Line, "")),
		seed("flag split by CR as JSON", value, nil, tool(`{"a":"`), data("'pass"+zw+"word':|\r"+value, redact.JSON, ""), tool(`"}`)),
		kv,
	)
	for _, f := range []redact.Form{redact.AsIs, redact.Line, redact.JSON} {
		out = append(out, seed("three pieces key with NUL form "+strconv.Itoa(int(f)), hunter, nil,
			tool("pass"+zw+"word: \""), data("open ", f, "pass\x00word"), data(hunter+"\"", f, "other")))
	}
	return out
}

// gateInputs returns the seeds, then the generated cases.
func gateInputs() []gateInput {
	out := gateSeeds()
	for _, c := range redacttest.GateCases() {
		out = append(out, gateInput{GateCase: c})
	}
	return out
}

// gatePieces returns the answers of pieces a generated case is written as: its parts as values side by side as
// they are, on one line, and as the strings of a JSON array; and its key and value as a member of a JSON object
// and as a label line.
func gatePieces(c gateInput) map[string][]redact.Piece {
	if c.pieces != nil {
		return map[string][]redact.Piece{"given": c.pieces}
	}
	m := map[string][]redact.Piece{}
	asIs, line, arr := []redact.Piece{}, []redact.Piece{}, []redact.Piece{{Text: `["`}}
	for i, p := range c.Parts {
		asIs = append(asIs, redact.Piece{Text: p, Data: true})
		line = append(line, redact.Piece{Text: p, Data: true, Form: redact.Line})
		if i > 0 {
			arr = append(arr, redact.Piece{Text: `","`})
		}
		arr = append(arr, redact.Piece{Text: p, Data: true, Form: redact.JSON})
	}
	m["as is"], m["line"], m["array"] = asIs, line, append(arr, redact.Piece{Text: `"]`})
	if c.Key != "" {
		m["member"] = []redact.Piece{{Text: `{"`}, {Text: c.Key, Data: true, Form: redact.JSON, MapKey: true},
			{Text: `":"`}, {Text: c.Value, Data: true, Form: redact.JSON, Key: c.Key}, {Text: `"}`}}
		m["label"] = []redact.Piece{{Text: c.Key, Data: true, Form: redact.Line, MapKey: true},
			{Text: "="}, {Text: c.Value, Data: true, Form: redact.Line, Key: c.Key}}
	}
	return m
}

// gateEntries are the entry points of the gate, by name. Each returns its outputs for an input, nil when it does
// not take it (a text entry point for an input of pieces only, a key entry point for an input without a key).
func gateEntries() map[string]func(c gateInput) []string {
	r := defaultRedactor()
	text := func(f func(s string) []string) func(c gateInput) []string {
		return func(c gateInput) []string {
			if len(c.Parts) == 0 {
				return nil
			}
			return f(c.Text())
		}
	}
	m := map[string]func(c gateInput) []string{
		gateEntryString:         text(func(s string) []string { return []string{r.String(s)} }),
		"redact.StringTwice":    text(func(s string) []string { return []string{r.StringTwice(s, nil)} }),
		"redact.StringKeeping":  text(func(s string) []string { return []string{r.StringKeeping(s, "hunter")} }),
		"redact.ReplaceChecked": text(func(s string) []string { out, _ := r.ReplaceChecked(s, gateMark, gateSame); return []string{out} }),
		"redact.Strings": func(c gateInput) []string {
			if len(c.Parts) == 0 {
				return nil
			}
			return r.Strings(c.Parts)
		},
		"incidentcontext.RenderLog": text(func(s string) []string {
			ex := providers.LogExcerpt{Text: s, CollectedAt: time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)}
			out := make([]string, 0, 2)
			for _, refit := range []bool{false, true} {
				out = append(out, incidentcontext.RenderLog(r, refit, "log", ex, nil, 4096, 8192).Text)
			}
			return out
		}),
		"redact.Pieces": func(c gateInput) []string {
			out := make([]string, 0, 16)
			for _, ps := range gateLayouts(c) {
				out = append(out, r.Pieces(ps)...)
			}
			return out
		},
		"redact.PiecesWithin": func(c gateInput) []string {
			var out []string
			for _, ps := range gateLayouts(c) {
				got, _ := r.PiecesWithin(ps, len(c.Text())/2)
				out = append(out, got...)
			}
			return out
		},
		"redact.PiecesFitting": func(c gateInput) []string {
			var out []string
			for _, ps := range gateLayouts(c) {
				got, _ := r.PiecesFitting(ps, func(o []string) bool { return len(strings.Join(o, "")) <= len(c.Text()) })
				out = append(out, got...)
			}
			return out
		},
		tools.NameGetResource + " of a label": func(c gateInput) []string {
			if c.Key == "" {
				return nil
			}
			return []string{getResourceOfLabel(c.Key, c.Value)}
		},
		tools.NameQueryMetrics + " of a label": func(c gateInput) []string {
			if c.Key == "" {
				return nil
			}
			return []string{queryMetricsOfLabels([][2]string{{c.Key, c.Value}})}
		},
	}
	for name, f := range entryPoints {
		m[name] = text(f)
	}
	return m
}

// gateLayouts returns the answers of pieces of c in the order of their names.
func gateLayouts(c gateInput) [][]redact.Piece {
	m := gatePieces(c)
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	slices.Sort(names)
	out := make([][]redact.Piece, 0, len(names))
	for _, n := range names {
		out = append(out, m[n])
	}
	return out
}

// gateMark is the replacement of redact.ReplaceChecked in the gate: the kind only.
func gateMark(kind, _ string) string { return "<" + kind + ">" }

// gateNames returns the names of gateEntries in order.
func gateNames() []string {
	entries := gateEntries()
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// gateBits is what the outputs show of the probe: the union of redacttest.VisibleGrams over them, in hex; "-"
// when the entry point does not take the input. A panic of a helper (a tool that does not answer as expected) is
// an output too, compared as its text.
func gateBits(f func(c gateInput) []string, c gateInput) (bits string) {
	defer func() {
		if p := recover(); p != nil {
			bits = "panic"
		}
	}()
	outs := f(c)
	if outs == nil {
		return "-"
	}
	var b uint64
	for _, o := range outs {
		b |= redacttest.VisibleGrams(o, c.Probe)
	}
	return strconv.FormatUint(b, 16)
}

// gateInputHash is the hash of an input: the first 8 bytes of the SHA-256 of its parts, key, value and pieces.
func gateInputHash(c gateInput) string {
	h := sha256.New()
	for _, s := range append(append([]string{}, c.Parts...), c.Key, c.Sep, c.Value, c.Probe) {
		_, _ = fmt.Fprintf(h, "%d:%s", len(s), s)
	}
	for _, p := range c.pieces {
		_, _ = fmt.Fprintf(h, "%d:%s%t%d%s", len(p.Text), p.Text, p.Data, p.Form, p.Key)
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// gateRows runs the inputs keep accepts through every entry point, one goroutine per entry point, and returns
// per input the bits in the order of names ("" for an input not run).
func gateRows(cases []gateInput, names []string, keep func(name string, i int) bool) [][]string {
	entries := gateEntries()
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
					rows[i][j] = gateBits(f, c)
				}
			}
		})
	}
	wg.Wait()
	return rows
}

// TestWriteControlsGateMainTable writes gateMainTable when CONTROLS_GATE_MAIN_TABLE names the output path and
// CONTROLS_GATE_MAIN_COMMIT the commit of the checkout. It refuses in a tree that holds controls.go, the added
// reading of #852: the table is origin/main's answer, never the branch's own.
func TestWriteControlsGateMainTable(t *testing.T) {
	path := os.Getenv("CONTROLS_GATE_MAIN_TABLE")
	if path == "" {
		t.Skip("CONTROLS_GATE_MAIN_TABLE is not set")
	}
	if redact.RaceDetector {
		t.Fatal("the main table needs every input: run without -race")
	}
	if _, err := os.Stat("controls.go"); err == nil {
		t.Fatal("controls.go exists in this tree: the main table is written from a checkout of origin/main")
	}
	commit := os.Getenv("CONTROLS_GATE_MAIN_COMMIT")
	if commit == "" {
		t.Fatal("CONTROLS_GATE_MAIN_COMMIT must name the commit of the checkout")
	}
	cases, names := gateInputs(), gateNames()
	rows := gateRows(cases, names, func(string, int) bool { return true })
	f, err := os.Create(path) //nolint:gosec // a path the person running the test names
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	_, _ = fmt.Fprintf(w, "# What each entry point showed of the probe of each input of the controls gate of #852, at origin/main %s.\n", commit)
	_, _ = fmt.Fprintf(w, "# entry points: %s\n", strings.Join(names, ","))
	for i, c := range cases {
		_, _ = fmt.Fprintf(w, "%s\t%s\n", gateInputHash(c), strings.Join(rows[i], "\t"))
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// readGateTable reads a gate table: the entry points of its header and its rows, each the input's hash and then
// one column per entry point.
func readGateTable(t *testing.T, path string) (names []string, rows [][]string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(b), "\n"), "\n") {
		if list, ok := strings.CutPrefix(line, "# entry points: "); ok {
			names = strings.Split(list, ",")
			continue
		}
		if !strings.HasPrefix(line, "#") {
			rows = append(rows, strings.Split(line, "\t"))
		}
	}
	return names, rows
}

// No entry point shows a four-byte part of a probe that origin/main's did not show, on any input of the gate
// (decision of 2026-11-22 on #852). Under -race each entry point checks a seeded sample (redact.RaceKeeps).
func TestControlsGateHidesWhatMainHid(t *testing.T) {
	redact.ParallelUnderRace(t)
	cases, names := gateInputs(), gateNames()
	mainNames, want := readGateTable(t, gateMainTable)
	if !slices.Equal(mainNames, names) {
		t.Fatalf("%s has the entry points %v, the test %v: write it again at origin/main", gateMainTable, mainNames, names)
	}
	if len(want) != len(cases) {
		t.Fatalf("%s has %d rows, the gate %d inputs", gateMainTable, len(want), len(cases))
	}
	for i, c := range cases {
		if len(want[i]) != len(names)+1 || want[i][0] != gateInputHash(c) {
			t.Fatalf("%s row %d does not match input %d (%s)", gateMainTable, i+1, i, c.Name)
		}
	}
	seeds := len(gateSeeds())
	got := gateRows(cases, names, func(name string, i int) bool { return i < seeds || redact.RaceKeeps(name, i) })
	entries := gateEntries()
	shows, ran, hidOnMain := map[string]int{}, 0, 0
	for i, c := range cases {
		full := uint64(1)<<(len(c.Probe)-3) - 1
		for j, name := range names {
			if got[i][j] == "" || got[i][j] == want[i][j+1] && (got[i][j] == "-" || got[i][j] == "panic") {
				continue
			}
			ran++
			m, errM := strconv.ParseUint(want[i][j+1], 16, 64)
			b, errB := strconv.ParseUint(got[i][j], 16, 64)
			if errM == nil && m != full {
				hidOnMain++
			}
			if errM == nil && errB == nil && b&^m == 0 {
				continue
			}
			if shows[name]++; shows[name] <= 3 {
				t.Errorf("%s shows more of %q than origin/main on %s (input %d): main %s, now %s\n  text: %q\n  key: %q value: %q\n  out: %q",
					name, c.Probe, c.Name, i, want[i][j+1], got[i][j], c.Text(), c.Key, c.Value, entries[name](c))
			}
		}
	}
	for _, name := range names {
		if shows[name] > 0 {
			t.Errorf("%s: %d inputs show more than origin/main", name, shows[name])
		}
	}
	if hidOnMain == 0 {
		t.Fatal("origin/main hid no part of any probe: the table is not read")
	}
	t.Logf("%d inputs, %d entry points, %d checks, %d where origin/main hid a part of the probe", len(cases), len(names), ran, hidOnMain)
}

// labelReader answers get_resource with a pod whose one label is key and value; calls are serialized.
type labelReader struct {
	client.Reader
	sync.Mutex
	key, value string
}

func (r *labelReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("unexpected kind %T", obj)
	}
	*p = corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{r.key: r.value}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: toolContainer}}}}
	return nil
}

// labelTools is the tool set over a labelReader.
var labelTools = sync.OnceValues(func() (*tools.Set, *labelReader) {
	reader := &labelReader{}
	set, err := tools.New(tools.Config{Pods: reader, Events: &textReader{}, Objects: reader, Redactor: defaultRedactor(),
		Namespaces: []string{toolNS}})
	if err != nil {
		panic(err)
	}
	return set, reader
})

// getResourceOfLabel returns the answer of get_resource for a pod whose one label is key and value.
func getResourceOfLabel(key, value string) string {
	set, reader := labelTools()
	reader.Lock()
	defer reader.Unlock()
	reader.key, reader.value = key, value
	return callTool(set, tools.NameGetResource, map[string]any{argKind: toolKind, argNamespace: toolNS, argName: toolPod})
}

// gateSame leaves the text between the replacements of redact.ReplaceChecked as it is.
func gateSame(s string) string { return s }
