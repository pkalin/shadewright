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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/tools"
)

// canonSequenceEntry is the read tool get_resource on a pod whose container arguments are the strings of a
// sequence input: a key in one string and its value in the next (#845). The other entry points, and
// get_resource for every input, take the text of the input, as entryPoints gives them.
const canonSequenceEntry = tools.NameGetResource + " of the strings"

// argsReader answers get_resource with a pod whose container arguments are args.
type argsReader struct {
	client.Reader
	args []string
}

func (r *argsReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("unexpected kind %T", obj)
	}
	*p = corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: toolContainer, Args: r.args}}},
	}
	return nil
}

// argsTools is one tool set over an argsReader; calls are serialized, since each sets the strings first.
var argsTools = sync.OnceValue(func() *struct {
	sync.Mutex
	set    *tools.Set
	reader *argsReader
} {
	reader := &argsReader{}
	set, err := tools.New(tools.Config{
		Pods: reader, Events: reader, Objects: reader, Logs: &fake.Logs{},
		Redactor: defaultRedactor(), Namespaces: []string{toolNS},
	})
	if err != nil {
		panic(err)
	}
	return &struct {
		sync.Mutex
		set    *tools.Set
		reader *argsReader
	}{set: set, reader: reader}
})

// getResourceOfArgs returns the answer of get_resource on a pod whose container arguments are args.
func getResourceOfArgs(args []string) string {
	at := argsTools()
	at.Lock()
	defer at.Unlock()
	at.reader.args = args
	raw, err := json.Marshal(map[string]any{argKind: toolKind, argNamespace: toolNS, argName: toolPod})
	if err != nil {
		panic(err)
	}
	r, err := at.set.Call(context.Background(), providers.ToolCall{ID: "c1", Name: tools.NameGetResource, Arguments: raw})
	if err != nil {
		panic(err)
	}
	return r.Text
}

// canonPieceIn reports whether out holds the secret piece s, whole or in part: its first or its last
// redact.CanonPartialMin bytes, so that a prefix or a suffix of that length or longer counts. A shorter piece (a
// word of a phrase) counts only whole.
func canonPieceIn(out, s string) bool {
	n := min(len(s), redact.CanonPartialMin)
	return strings.Contains(out, s[:n]) || strings.Contains(out, s[len(s)-n:])
}

// canonPieceIn finds a piece whole, by a prefix or a suffix of redact.CanonPartialMin bytes or more, and a
// short piece only whole; canonKeptLost finds a kept text an output holds fewer times than the input.
func TestCanonPieceAndKeptChecks(t *testing.T) {
	const s, word = "Qz20001xK9w", "dune"
	for _, tc := range []struct {
		out, piece string
		want       bool
	}{
		{"a " + s + " b", s, true},
		{"[REDACTED]" + s[:8], s, true},
		{s[3:] + "]", s, true},
		{s[:7] + "[REDACTED]" + s[4:], s, false},
		{"Qz20002xK9w Qz20001", s, false},
		{word, word, true},
		{word[:3], word, false},
	} {
		if got := canonPieceIn(tc.out, tc.piece); got != tc.want {
			t.Errorf("canonPieceIn(%q, %q) = %v, want %v", tc.out, tc.piece, got, tc.want)
		}
	}
	masked := []string{"pass: | password: |\n  \x00\x00\x00\nnext: --port=8080"}
	keep := []string{"pass", "password", "next", "--port=8080", `"token"`}
	for _, tc := range []struct {
		out  string
		want []int
	}{
		{"pass: | password: [REDACTED]\nnext: --port=8080", nil},
		{"[REDACTED] password: [REDACTED]\nnext: --port=8080", []int{0}},
		{"pass: | [REDACTED]", []int{0, 1, 2, 3}},
		{`{"a":"pass: | password: [REDACTED]\\nnext: --port=8080"}`, nil},
	} {
		if got := canonKeptLost(tc.out, masked, keep); !slices.Equal(got, tc.want) {
			t.Errorf("canonKeptLost(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

// canonEscapes are the ways an entry point writes a kept text: as it is, and escaped as a JSON string with and
// without the HTML escapes.
var canonEscapes = []func(s string) string{
	func(s string) string { return s },
	func(s string) string {
		b, _ := json.Marshal(s)
		return string(b[1 : len(b)-1])
	},
	func(s string) string {
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(s)
		out := strings.TrimSuffix(b.String(), "\n")
		return out[1 : len(out)-1]
	},
}

// canonKeptLost returns the indexes of the kept pieces that out lost: a kept text that out, as written or
// escaped as JSON, holds fewer times than the input does outside its secret pieces (masked). Other text of the
// output (the fields of a tool's answer) can only add occurrences, so the check is a lower bound.
func canonKeptLost(out string, masked []string, keep []string) []int {
	var lost []int
	for i, k := range keep {
		need := 0
		for _, m := range masked {
			need += strings.Count(m, k)
		}
		have := 0
		for _, esc := range canonEscapes {
			have = max(have, strings.Count(out, esc(k)))
		}
		if have < need {
			lost = append(lost, i)
		}
	}
	return lost
}

// canonKeptEntries are the entry points whose output must hold every kept piece of the input byte for byte (or
// escaped as JSON): the context builder's value, the read tools, the log excerpt of the whole read. The
// others change kept text by design: fingerprint.Normalize, fingerprint.Symptom, fingerprint.Value and
// modules.BoundValue replace variable parts (numbers, durations) by placeholders and collapse whitespace, and
// the context builder's value of the symptom is fingerprint.Symptom's text; the log excerpt cut after a line
// drops the lines before the cut, so its kept pieces are checked on the excerpt of the whole read only.
var canonKeptEntries = map[string]bool{
	canonEntryValue: true, tools.NameGetResource: true, tools.NameListEvents: true,
	tools.NameGetPodLogs: true, canonEntryExcerpt: true, canonSequenceEntry: true,
}

// canonEntryValue and canonEntryExcerpt are the names of two entry points in entryPoints.
const (
	canonEntryValue   = "incidentcontext.Value"
	canonEntryExcerpt = "kube.Excerpt"
)

// canonEntryLeaks runs every input of cases through every entry point (entryPoints, and get_resource of the
// strings for a sequence) and returns, for each, the pieces of each input whose secret is in an output, whole
// or in part (canonPieceIn), and the kept pieces an output lost (canonKeptLost, at canonKeptEntries), with the
// first such output of each entry point for the message. The entry points run side by side, one goroutine
// each; the read tools share one tool set and take turns.
func canonEntryLeaks(cases []redact.CanonCase) (leaks []redact.CanonLeak, firstOut map[string]string) {
	type result struct {
		leaks []redact.CanonLeak
		first string
	}
	// pieces returns the items of c that the outputs leak: secret pieces in any output, kept pieces lost by
	// the first.
	pieces := func(name string, c redact.CanonCase, outs []string, masked []string) []string {
		var items []string
		for i, s := range c.Secrets {
			for _, out := range outs {
				if canonPieceIn(out, s) {
					items = append(items, redact.CanonSecretItem(i))
					break
				}
			}
		}
		if canonKeptEntries[name] && len(outs) > 0 {
			for _, i := range canonKeptLost(outs[0], masked, c.Keep) {
				items = append(items, redact.CanonKeptItem(i))
			}
		}
		return items
	}
	checks := map[string]func(c redact.CanonCase) ([]string, string){}
	for name, f := range entryPoints {
		checks[name] = func(c redact.CanonCase) ([]string, string) {
			outs := f(c.In)
			items := pieces(name, c, outs, []string{c.Masked})
			if len(items) == 0 {
				return nil, ""
			}
			return items, outs[0]
		}
	}
	checks[canonSequenceEntry] = func(c redact.CanonCase) ([]string, string) {
		if c.Args == nil {
			return nil, ""
		}
		out := getResourceOfArgs(c.Args)
		return pieces(canonSequenceEntry, c, []string{out}, c.MaskedArgs), out
	}
	run := func(name string, f func(c redact.CanonCase) ([]string, string)) result {
		var r result
		for _, c := range cases {
			items, out := f(c)
			for _, it := range items {
				r.leaks = append(r.leaks, redact.CanonLeak{Check: name, Hash: c.Hash, Name: c.Name, Item: it})
			}
			if len(items) > 0 && r.first == "" {
				r.first = fmt.Sprintf("%s, pieces %v:\n  in: %q\n out: %q", c.Name, items, c.In, out)
			}
		}
		return r
	}
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	firstOut = map[string]string{}
	for name, f := range checks {
		wg.Go(func() {
			r := run(name, f)
			mu.Lock()
			defer mu.Unlock()
			leaks = append(leaks, r.leaks...)
			if r.first != "" {
				firstOut[name] = r.first
			}
		})
	}
	wg.Wait()
	return leaks, firstOut
}

// canonCorpusLeaks is the corpus (or its sample) and its leaks at the entry points, computed once for the
// tests below.
var canonCorpusLeaks = sync.OnceValues(func() (struct {
	cases   []redact.CanonCase
	sampled bool
}, struct {
	leaks []redact.CanonLeak
	first map[string]string
}) {
	cases, sampled := redact.CanonCorpus()
	leaks, first := canonEntryLeaks(cases)
	return struct {
		cases   []redact.CanonCase
		sampled bool
	}{cases, sampled}, struct {
		leaks []redact.CanonLeak
		first map[string]string
	}{leaks, first}
})

// Every input of the canonical view corpus goes through every entry point, the read tools included, and no
// piece of secret survives in any output, except the leaks listed in redact.CanonEntryLeaks. A listed leak
// that stops leaking fails too, so the table shrinks with the fix of #845.
func TestCanonCorpusThroughEveryEntryPoint(t *testing.T) {
	corpus, found := canonCorpusLeaks()
	cases, sampled, got, first := corpus.cases, corpus.sampled, found.leaks, found.first
	if redact.CanonWriteTables(t, sampled) {
		redact.WriteCanonTable(t, redact.CanonEntryLeaks, []string{
			"# Expected leaks of the entry points on the canonical view corpus (#845), piece by piece, written by CANON_LEAKS_WRITE=1.",
		}, got)
		return
	}
	checked := map[string]string{}
	for _, c := range cases {
		checked[c.Hash] = c.Name
	}
	want, _ := redact.ReadCanonTable(t, redact.CanonEntryLeaks)
	redact.CheckCanonLeaks(t, redact.CanonEntryLeaks, redact.CanonIndex(), checked, want, got)
	names := make([]string, 0, len(first))
	for n := range first {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		t.Logf("first leak at %s: %s", n, first[n])
	}
}

// No piece of secret of the corpus leaks at an entry point where it did not leak on main
// (redact.CanonMainEntryLeaks, written at the commit of the main table with every entry point main has). A row
// of that table that names no piece of the corpus fails.
func TestCanonEntryPointsLeakNothingMainHid(t *testing.T) {
	_, found := canonCorpusLeaks()
	main, comments := redact.ReadCanonTable(t, redact.CanonMainEntryLeaks)
	var onMain []string
	for _, c := range comments {
		if list, ok := strings.CutPrefix(c, "# entry points: "); ok {
			onMain = strings.Split(list, ",")
		}
	}
	if len(onMain) == 0 {
		t.Fatalf("%s names no entry points", redact.CanonMainEntryLeaks)
	}
	for i, e := range redact.CanonTableErrors(main, redact.CanonIndex()) {
		if i < 20 {
			t.Errorf("%s: %s", redact.CanonMainEntryLeaks, e)
		}
	}
	key := func(r redact.CanonLeak) string { return r.Check + "\t" + r.Hash + "\t" + r.Item }
	mainLeak := map[string]bool{}
	for _, r := range main {
		mainLeak[key(r)] = true
	}
	lost := 0
	for _, r := range found.leaks {
		if !strings.HasPrefix(r.Item, "s") || !slices.Contains(onMain, r.Check) || mainLeak[key(r)] {
			continue
		}
		lost++
		if lost <= 20 {
			t.Errorf("%s: piece %s leaks at %s, did not on main", r.Name, r.Item, r.Check)
		}
	}
	if lost > 0 {
		t.Errorf("%d secret pieces leak at an entry point where main hid them", lost)
	}
}

// TestWriteCanonMainEntryTable writes redact.CanonMainEntryLeaks from the entry points it runs with, when
// CANON_MAIN_ENTRY_TABLE names the output path (and CANON_MAIN_COMMIT the commit, for its first line); it is
// run once in a checkout of the commit of the main table, with entryPoints reduced to the entry points that
// commit has.
func TestWriteCanonMainEntryTable(t *testing.T) {
	path := os.Getenv("CANON_MAIN_ENTRY_TABLE")
	if path == "" {
		t.Skip("CANON_MAIN_ENTRY_TABLE is not set")
	}
	corpus, found := canonCorpusLeaks()
	if corpus.sampled {
		t.Fatal("the main table needs the full corpus: run without -race and -short")
	}
	got := found.leaks
	names := make([]string, 0, len(entryPoints)+1)
	for n := range entryPoints {
		names = append(names, n)
	}
	names = append(names, canonSequenceEntry)
	slices.Sort(names)
	redact.WriteCanonTable(t, path, []string{
		"# Leaks of the entry points on the canonical view corpus (#845), piece by piece, at main " + os.Getenv("CANON_MAIN_COMMIT") + ".",
		"# entry points: " + strings.Join(names, ","),
	}, got)
}
