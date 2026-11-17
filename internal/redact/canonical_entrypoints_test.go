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

// canonEntryLeaks runs every input of cases through every entry point (entryPoints, and get_resource of the
// strings for a sequence) and returns, for each, the inputs whose output holds a piece of secret, with the
// first such output of each entry point for the message. The entry points run side by side, one goroutine
// each; the read tools share one tool set and take turns.
func canonEntryLeaks(cases []redact.CanonCase) (leaks []redact.CanonLeak, firstOut map[string]string) {
	type result struct {
		leaks []redact.CanonLeak
		first string
	}
	run := func(name string, f func(c redact.CanonCase) (string, bool)) result {
		var r result
		for _, c := range cases {
			if out, ok := f(c); ok {
				r.leaks = append(r.leaks, redact.CanonLeak{Check: name, Hash: c.Hash, Name: c.Name})
				if r.first == "" {
					r.first = fmt.Sprintf("%s:\n  in: %q\n out: %q", c.Name, c.In, out)
				}
			}
		}
		return r
	}
	checks := map[string]func(c redact.CanonCase) (string, bool){}
	for name, f := range entryPoints {
		checks[name] = func(c redact.CanonCase) (string, bool) {
			for _, out := range f(c.In) {
				if leakedSecret(out, c.Secrets) != "" {
					return out, true
				}
			}
			return "", false
		}
	}
	checks[canonSequenceEntry] = func(c redact.CanonCase) (string, bool) {
		if c.Args == nil {
			return "", false
		}
		out := getResourceOfArgs(c.Args)
		return out, leakedSecret(out, c.Secrets) != ""
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
			"# Expected leaks of the entry points on the canonical view corpus (#845), written by CANON_LEAKS_WRITE=1.",
		}, got)
		return
	}
	checked := map[string]string{}
	for _, c := range cases {
		checked[c.Hash] = c.Name
	}
	want, _ := redact.ReadCanonTable(t, redact.CanonEntryLeaks)
	redact.CheckCanonLeaks(t, redact.CanonEntryLeaks, checked, want, got)
	names := make([]string, 0, len(first))
	for n := range first {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		t.Logf("first leak at %s: %s", n, first[n])
	}
}

// No input of the corpus leaks at an entry point where it did not leak on main (redact.CanonMainEntryLeaks,
// written at the commit of the main table with every entry point main has).
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
	mainLeak := map[string]bool{}
	for _, r := range main {
		mainLeak[r.Check+"\t"+r.Hash] = true
	}
	lost := 0
	for _, r := range found.leaks {
		if !slices.Contains(onMain, r.Check) || mainLeak[r.Check+"\t"+r.Hash] {
			continue
		}
		lost++
		if lost <= 20 {
			t.Errorf("%s: leaks at %s, did not on main", r.Name, r.Check)
		}
	}
	if lost > 0 {
		t.Errorf("%d inputs leak at an entry point where main hid them", lost)
	}
}

// TestWriteCanonMainEntryTable writes redact.CanonMainEntryLeaks from the entry points it runs with, when
// CANON_MAIN_ENTRY_TABLE names the output path; it is run once in a checkout of the commit of the main table,
// with entryPoints reduced to the entry points that commit has.
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
		"# Leaks of the entry points on the canonical view corpus (#845) at main 26a6108e.",
		"# entry points: " + strings.Join(names, ","),
	}, got)
}
