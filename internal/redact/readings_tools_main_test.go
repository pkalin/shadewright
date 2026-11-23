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
	"maps"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/tools"
)

// The generated list items of the readings corpus at every read tool that can carry a list (#845, twelfth
// decision of 2026-11-18): the comparison with main is made on the text the tools return. The file depends only
// on signal_forms_test.go, readings_main_test.go and what main exports, so that it compiles in a checkout of
// main, where TestWriteReadingsToolsMainTable writes what main's tools did with them.

// readingsToolsTable holds, for each list tool and each generated form, whether main's tool hid the secret:
// "row<TAB>hidden or shown", the row readingsRow(tool, form).
const readingsToolsTable = "testdata/readings-tools-main.tsv"

// The arguments and objects of the list tools (the template is in readings_main_test.go).
const (
	readingsNS        = "shop"
	readingsLabel     = "message"
	readingsContainer = "app"
	readingsArgLimit  = "limit"
	readingsArgTempl  = "template"
	readingsArgPod    = "pod"
	readingsArgCont   = "container"
)

// readingsLists is the source of the list tools: a pod whose container arguments are the items, one Event of the
// pod per item (the first the newest), a log whose lines are the items, and metric series.
type readingsLists struct {
	client.Reader
	items []string
}

func (r *readingsLists) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("unexpected kind %T", obj)
	}
	*p = corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: readingsContainer, Args: r.items}}},
	}
	return nil
}

func (r *readingsLists) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	l, ok := list.(*eventsv1.EventList)
	if !ok {
		return fmt.Errorf("unexpected kind %T", list)
	}
	at, err := time.Parse(time.RFC3339, readingsEventTime)
	if err != nil {
		return err
	}
	l.Items = nil
	for i, note := range r.items {
		l.Items = append(l.Items, eventsv1.Event{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("e%d", i+1), Namespace: readingsNS},
			EventTime:  metav1.NewMicroTime(at),
			Regarding:  corev1.ObjectReference{Kind: readingsKind, Name: readingsPod, Namespace: readingsNS},
			Reason:     readingsReason, Type: readingsEventType, Note: note,
		})
	}
	return nil
}

// readingsToolSet is one tool set over a readingsLists, a fake log and a fake metrics source, with the redactor
// red; calls are serialized, since each sets the items first.
type readingsToolSet struct {
	sync.Mutex
	set     *tools.Set
	src     *readingsLists
	logs    *fake.Logs
	metrics *fake.Metrics
}

func newReadingsToolSet(red tools.Redactor) *readingsToolSet {
	src, logs, metrics := &readingsLists{}, &fake.Logs{}, &fake.Metrics{}
	set, err := tools.New(tools.Config{
		Pods: src, Events: src, Objects: src, Logs: logs, Metrics: metrics,
		Redactor: red, Namespaces: []string{readingsNS},
	})
	if err != nil {
		panic(err)
	}
	return &readingsToolSet{set: set, src: src, logs: logs, metrics: metrics}
}

// readingsRedactor is the redactor of the tool sets of the list tools.
var readingsRedactor = sync.OnceValue(func() *redact.Redactor {
	r, err := redact.New()
	if err != nil {
		panic(err)
	}
	return r
})

// call sets the items in every source and returns the answer of the tool name with args.
func (ts *readingsToolSet) call(name string, args map[string]any, items []string, series []providers.Series) string {
	ts.Lock()
	defer ts.Unlock()
	ts.src.items = items
	ts.logs.Set(fake.LogKey{Namespace: readingsNS, Pod: readingsPod, Container: readingsContainer}, strings.Join(items, "\n")+"\n")
	ts.metrics.Set(readingsTemplate, series)
	raw, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	r, err := ts.set.Call(context.Background(), providers.ToolCall{ID: "c1", Name: name, Arguments: raw})
	if err != nil {
		panic(err)
	}
	if r.Status != tools.StatusOK {
		panic(fmt.Sprintf("%s answered %s: %s", name, r.Status, r.Text))
	}
	return r.Text
}

// readingsListTool is a read tool that can carry a list of the items: it answers with them, in order.
type readingsListTool struct {
	name string
	call func(ts *readingsToolSet, items []string) string
}

// readingsSeries is one series of the template with the labels.
func readingsSeries(labels map[string]string) providers.Series {
	return providers.Series{Labels: labels, Samples: []providers.Sample{{Value: 1}}}
}

// readingsListTools are the read tools that can carry a list, each with the way it carries one: get_resource
// writes the container arguments as a JSON array, one item per line; list_events writes one event per line;
// get_pod_logs one log line per item; query_metrics the labels of one series on one line ("name=value, ..."),
// and one series per line. get_delivery_status (a revision and a message) and check_missing_reference (the
// names the kubelet reported) carry no list of source text.
var readingsListTools = []readingsListTool{
	{tools.NameGetResource + " of the container arguments", func(ts *readingsToolSet, items []string) string {
		return ts.call(tools.NameGetResource, map[string]any{argKind: readingsKind, argNamespace: readingsNS, argName: readingsPod}, items, nil)
	}},
	{tools.NameListEvents + " of the notes", func(ts *readingsToolSet, items []string) string {
		return ts.call(tools.NameListEvents, map[string]any{argNamespace: readingsNS, argKind: readingsKind, argName: readingsPod, readingsArgLimit: len(items)}, items, nil)
	}},
	{tools.NameGetPodLogs + " of the lines", func(ts *readingsToolSet, items []string) string {
		return ts.call(tools.NameGetPodLogs, map[string]any{argNamespace: readingsNS, readingsArgPod: readingsPod, readingsArgCont: readingsContainer}, items, nil)
	}},
	{tools.NameQueryMetrics + " of the labels of a series", func(ts *readingsToolSet, items []string) string {
		labels := map[string]string{}
		for i, it := range items {
			labels[fmt.Sprintf("arg%d", i)] = it
		}
		return ts.call(tools.NameQueryMetrics, map[string]any{argNamespace: readingsNS, readingsArgPod: readingsPod, readingsArgTempl: readingsTemplate},
			items, []providers.Series{readingsSeries(labels)})
	}},
	{tools.NameQueryMetrics + " of the series", func(ts *readingsToolSet, items []string) string {
		series := make([]providers.Series, 0, len(items))
		for _, it := range items {
			series = append(series, readingsSeries(map[string]string{readingsLabel: it}))
		}
		return ts.call(tools.NameQueryMetrics, map[string]any{argNamespace: readingsNS, readingsArgPod: readingsPod, readingsArgTempl: readingsTemplate},
			items, series)
	}},
}

// readingsToolsHidden runs the list items of every generated form through every list tool, one goroutine and one
// tool set per tool, and returns whether each tool hid the secret, by row readingsRow(tool, form); keep says
// which forms (by index) run.
func readingsToolsHidden(keep func(tool string, i int) bool) map[string]bool {
	forms := signalForms()
	out := map[string]bool{}
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, lt := range readingsListTools {
		wg.Go(func() {
			ts := newReadingsToolSet(readingsRedactor())
			got := map[string]bool{}
			for i, f := range forms {
				if !keep(lt.name, i) {
					continue
				}
				secret := readingsSecret(i)
				got[readingsRow(lt.name, f.name)] = !strings.Contains(lt.call(ts, readingsListItems(f, secret)), secret)
			}
			mu.Lock()
			defer mu.Unlock()
			maps.Copy(out, got)
		})
	}
	wg.Wait()
	return out
}

// TestWriteReadingsToolsMainTable writes readingsToolsTable from the tools it runs with, when
// READINGS_TOOLS_MAIN_TABLE names the output path (and READINGS_MAIN_COMMIT the commit); it is run once in a
// checkout of main with this file, readings_main_test.go and signal_forms_test.go.
func TestWriteReadingsToolsMainTable(t *testing.T) {
	path := os.Getenv("READINGS_TOOLS_MAIN_TABLE")
	if path == "" {
		t.Skip("READINGS_TOOLS_MAIN_TABLE is not set")
	}
	hidden := readingsToolsHidden(func(string, int) bool { return true })
	var b strings.Builder
	b.WriteString("# Whether main's read tools hid the secret of each generated list (TestReadingsListsAtTheToolsAsMain), at main " +
		os.Getenv("READINGS_MAIN_COMMIT") + " (#845).\n# row (the first 16 hex digits of the SHA-256 of \"tool<TAB>form\")\thidden or shown\n")
	for _, lt := range readingsListTools {
		for _, f := range signalForms() {
			v := readingsShown
			if hidden[readingsRow(lt.name, f.name)] {
				v = readingsHidden
			}
			b.WriteString(readingsRow(lt.name, f.name) + "\t" + v + "\n")
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil { // #nosec G703 -- a path the person running the test names
		t.Fatal(err)
	}
}
