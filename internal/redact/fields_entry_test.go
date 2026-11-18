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
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
	"github.com/pkalin/shadewright/internal/tools"
)

// Nothing main hides is shown, at every read tool, for a secret in any field of the objects the tool reads
// (#845, second decision of 2026-11-18). The corpus put text only in a container's arguments and an Event's
// note, and get_resource lost the second pass over its whole answer unseen: a map value under a secret key
// (a CSI volume's volumeAttributes, a FlexVolume's options, a nodeSelector, labels) was redacted without its
// key and shown. Here each tool reads objects of the types it reads, and the walk of those types (not a list
// of fields) places a secret key and a secret value in every string field, every list of strings and every map
// with string keys, one place at a time. The answers are compared with what main hid
// (testdata/fields-main-entry.tsv, written by TestWriteFieldsMainTable in a checkout of main): a value main
// hid that is shown now fails. The file uses only the read tools' API, so it runs unchanged in that checkout.

// fieldsMainTable holds, for each place and form, whether main hid the value.
const fieldsMainTable = "testdata/fields-main-entry.tsv"

// The arguments of the field walk's calls, and the secret key of its forms.
const (
	fieldsKey       = "password"
	fieldsArgKind   = "kind"
	fieldsArgNS     = "namespace"
	fieldsArgName   = "name"
	fieldsArgPod    = "pod"
	fieldsArgLimit  = "limit"
	fieldsKustomize = "Kustomization"
	fieldsNS        = "shop"
	fieldsPod       = "web-abc-1"
	fieldsContainer = "app"
	fieldsTemplate  = "container_restarts_total"
	fieldsUnit      = "apps"
)

// fieldStep is one step of a path from the root of an object to a place: a field (its index), the element of
// a pointer, or the first element of a slice.
type fieldStep struct {
	field int // -1 for a pointer, -2 for a slice
	name  string
}

// fieldPlace is a place in a type that can hold text: a string, a list of strings, or a map with string keys.
type fieldPlace struct {
	path []fieldStep
	kind reflect.Kind // reflect.String, reflect.Slice (of strings) or reflect.Map
	elem reflect.Type // the map's element type
}

// name is the place's path, as the table names it.
func (p fieldPlace) name(root reflect.Type) string {
	var b strings.Builder
	b.WriteString(root.Name())
	for _, s := range p.path {
		switch s.field {
		case -1:
		case -2:
			b.WriteString("[]")
		default:
			b.WriteString("." + s.name)
		}
	}
	return b.String()
}

// fieldPlaces walks t and returns every place in it, in the order of its fields; a type inside itself (a
// recursive type) is not walked again.
func fieldPlaces(t reflect.Type) []fieldPlace {
	var out []fieldPlace
	var walk func(t reflect.Type, path []fieldStep, seen []reflect.Type)
	walk = func(t reflect.Type, path []fieldStep, seen []reflect.Type) {
		if slices.Contains(seen, t) {
			return
		}
		seen = append(seen, t)
		at := func(s fieldStep) []fieldStep { return append(slices.Clone(path), s) }
		switch t.Kind() {
		case reflect.String:
			out = append(out, fieldPlace{path: path, kind: reflect.String})
		case reflect.Pointer:
			walk(t.Elem(), at(fieldStep{field: -1}), seen)
		case reflect.Slice:
			if t.Elem().Kind() == reflect.String {
				out = append(out, fieldPlace{path: path, kind: reflect.Slice})
				return
			}
			walk(t.Elem(), at(fieldStep{field: -2}), seen)
		case reflect.Map:
			if t.Key().Kind() == reflect.String {
				out = append(out, fieldPlace{path: path, kind: reflect.Map, elem: t.Elem()})
			}
		case reflect.Struct:
			for i := range t.NumField() {
				if f := t.Field(i); f.IsExported() {
					walk(f.Type, at(fieldStep{field: i, name: f.Name}), seen)
				}
			}
		}
	}
	walk(t, nil, nil)
	return out
}

// fieldForm is one way of placing a secret key and value in a place: the text of a string or of the one
// string of a list, or the key and value of the one entry of a map (the value left zero when the map's values
// hold no text, the secret then in the key).
type fieldForm struct {
	name       string
	text       func(v string) string
	key, value func(v string) string
}

var (
	fieldStringForms = []fieldForm{
		{name: "password colon", text: func(v string) string { return fieldsKey + ": " + v }},
		{name: "api-token equals", text: func(v string) string { return "api-token=" + v }},
	}
	fieldMapForms = []fieldForm{
		{name: "key password", key: func(string) string { return fieldsKey }, value: func(v string) string { return v }},
		{name: "key api-token", key: func(string) string { return "api-token" }, value: func(v string) string { return v }},
		{name: "key secret", key: func(string) string { return "secret" }, value: func(v string) string { return v }},
		{name: "secret in the key", key: func(v string) string { return fieldsKey + "=" + v }, value: func(string) string { return "x" }},
	}
)

// forms returns the forms of a place.
func (p fieldPlace) forms() []fieldForm {
	switch {
	case p.kind != reflect.Map:
		return fieldStringForms
	case p.elem.Kind() == reflect.String:
		return fieldMapForms
	}
	return fieldMapForms[len(fieldMapForms)-1:]
}

// set writes the form with the secret v into the place of root (a pointer to a zero object), creating the
// pointers, slices and maps on the way.
func (p fieldPlace) set(root reflect.Value, f fieldForm, v string) {
	cur := root.Elem()
	for _, s := range p.path {
		switch s.field {
		case -1:
			if cur.IsNil() {
				cur.Set(reflect.New(cur.Type().Elem()))
			}
			cur = cur.Elem()
		case -2:
			if cur.Len() == 0 {
				cur.Set(reflect.MakeSlice(cur.Type(), 1, 1))
			}
			cur = cur.Index(0)
		default:
			cur = cur.Field(s.field)
		}
	}
	switch p.kind {
	case reflect.String:
		cur.SetString(f.text(v))
	case reflect.Slice:
		s := reflect.MakeSlice(cur.Type(), 1, 1)
		s.Index(0).SetString(f.text(v))
		cur.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(cur.Type())
		val := reflect.New(p.elem).Elem()
		if p.elem.Kind() == reflect.String {
			val.SetString(f.value(v))
		}
		m.SetMapIndex(reflect.ValueOf(f.key(v)).Convert(cur.Type().Key()), val)
		cur.Set(m)
	}
}

// fieldReader answers every read with obj (when it is of the type asked for, otherwise a pod with a container)
// and with events.
type fieldReader struct {
	client.Reader
	obj    client.Object
	events []eventsv1.Event
}

func (r *fieldReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if r.obj != nil && reflect.TypeOf(r.obj) == reflect.TypeOf(obj) {
		reflect.ValueOf(obj).Elem().Set(reflect.ValueOf(r.obj).Elem())
		return nil
	}
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("unexpected kind %T", obj)
	}
	*p = corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: fieldsContainer}}},
	}
	return nil
}

func (r *fieldReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	l, ok := list.(*eventsv1.EventList)
	if !ok {
		return fmt.Errorf("unexpected kind %T", list)
	}
	*l = eventsv1.EventList{Items: slices.Clone(r.events)}
	return nil
}

// fieldTool is one read tool with the types it reads and how a value of each reaches it.
type fieldTool struct {
	name  string
	args  map[string]any
	types []reflect.Type
	// put gives the tool set the object (a pointer to a value of one of types).
	put func(e *fieldEnv, obj reflect.Value)
}

// fieldEnv is a tool set over a fieldReader and fake sources.
type fieldEnv struct {
	set      *tools.Set
	reader   *fieldReader
	logs     *fake.Logs
	metrics  *fake.Metrics
	delivery *fake.Delivery
}

func newFieldEnv(t *testing.T) *fieldEnv {
	t.Helper()
	e := &fieldEnv{reader: &fieldReader{}, logs: &fake.Logs{}, metrics: &fake.Metrics{}, delivery: &fake.Delivery{}}
	set, err := tools.New(tools.Config{
		Pods: e.reader, Events: e.reader, Objects: e.reader, Logs: e.logs, Metrics: e.metrics, Delivery: e.delivery,
		Redactor: defaultRedactor(), Namespaces: []string{fieldsNS}, DeliveryNamespaces: []string{fieldsNS},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.set = set
	return e
}

// putObject makes obj the object every Get answers with.
func putObject(e *fieldEnv, obj reflect.Value) { e.reader.obj = obj.Interface().(client.Object) }

// putEvent makes obj, an Event, the one Event every List answers with, regarding the pod of the calls.
func putEvent(e *fieldEnv, obj reflect.Value) {
	ev := obj.Elem().Interface().(eventsv1.Event)
	e.reader.events = []eventsv1.Event{ev}
}

// fieldTools are the read tools that show text from a source, each with the types of what it reads.
var fieldTools = []fieldTool{
	{
		name: tools.NameGetResource,
		types: []reflect.Type{
			reflect.TypeFor[corev1.Pod](), reflect.TypeFor[appsv1.Deployment](),
			reflect.TypeFor[appsv1.StatefulSet](), reflect.TypeFor[appsv1.ReplicaSet](),
		},
		put: putObject,
	},
	{
		name: tools.NameListEvents, args: map[string]any{fieldsArgNS: fieldsNS, fieldsArgLimit: 5},
		types: []reflect.Type{reflect.TypeFor[eventsv1.Event]()}, put: putEvent,
	},
	{
		name:  tools.NameCheckMissingReference,
		args:  map[string]any{fieldsArgNS: fieldsNS, fieldsArgPod: fieldsPod, fieldsArgKind: "Secret", fieldsArgName: "db"},
		types: []reflect.Type{reflect.TypeFor[corev1.Pod](), reflect.TypeFor[eventsv1.Event]()},
		put: func(e *fieldEnv, obj reflect.Value) {
			if _, ok := reflect.TypeAssert[*eventsv1.Event](obj); ok {
				putEvent(e, obj)
				return
			}
			putObject(e, obj)
		},
	},
	{
		name: tools.NameQueryMetrics, args: map[string]any{fieldsArgNS: fieldsNS, fieldsArgPod: fieldsPod, "template": fieldsTemplate},
		types: []reflect.Type{reflect.TypeFor[providers.Series]()},
		put: func(e *fieldEnv, obj reflect.Value) {
			e.metrics.Set(fieldsTemplate, []providers.Series{obj.Elem().Interface().(providers.Series)})
		},
	},
	{
		name: tools.NameGetDeliveryStatus, args: map[string]any{fieldsArgKind: fieldsKustomize, fieldsArgNS: fieldsNS, fieldsArgName: fieldsUnit},
		types: []reflect.Type{reflect.TypeFor[providers.DeliveryStatus]()},
		put: func(e *fieldEnv, obj reflect.Value) {
			e.delivery.Set(providers.DeliveryUnit{Kind: fieldsKustomize, Namespace: fieldsNS, Name: fieldsUnit},
				obj.Elem().Interface().(providers.DeliveryStatus))
		},
	},
	{
		// A log is one text, a string of its own.
		name: tools.NameGetPodLogs, args: map[string]any{fieldsArgNS: fieldsNS, fieldsArgPod: fieldsPod, "container": fieldsContainer},
		types: []reflect.Type{reflect.TypeFor[fieldLog]()},
		put: func(e *fieldEnv, obj reflect.Value) {
			e.logs.Set(fake.LogKey{Namespace: fieldsNS, Pod: fieldsPod, Container: fieldsContainer}, obj.Elem().Interface().(fieldLog).Text)
		},
	},
}

// fieldLog is the text of a container's log.
type fieldLog struct{ Text string }

// fieldResult is what one tool showed of one form in one place: whether the secret was hidden.
type fieldResult struct {
	tool, place, form string
	hidden            bool
}

// key is the result's row key in the table.
func (r fieldResult) key() string { return r.tool + "\t" + r.place + "\t" + r.form }

// fieldResults calls every tool with a secret in every place of the types it reads, in every form of that
// place, and returns the results and the number of places per tool.
func fieldResults(t *testing.T) ([]fieldResult, map[string]int) {
	t.Helper()
	var out []fieldResult
	places := map[string]int{}
	n := 0
	for _, ft := range fieldTools {
		e := newFieldEnv(t)
		for _, typ := range ft.types {
			for _, p := range fieldPlaces(typ) {
				places[ft.name]++
				for _, f := range p.forms() {
					n++
					v := fmt.Sprintf("Vf%06dxK9w", n)
					obj := reflect.New(typ)
					p.set(obj, f, v)
					if m, ok := reflect.TypeAssert[metav1.Object](obj); ok && m.GetName() == "" {
						m.SetName(fieldsPod)
						m.SetNamespace(fieldsNS)
					}
					e.reader.obj, e.reader.events = nil, nil
					ft.put(e, obj)
					args := ft.args
					if ft.name == tools.NameGetResource {
						args = map[string]any{fieldsArgKind: typ.Name(), fieldsArgNS: fieldsNS, fieldsArgName: fieldsPod}
					}
					text := callFieldTool(t, e.set, ft.name, args)
					shown := strings.Contains(text, v[:8]) || strings.Contains(text, v[len(v)-8:])
					out = append(out, fieldResult{tool: ft.name, place: p.name(typ), form: f.name, hidden: !shown})
				}
			}
		}
	}
	return out, places
}

// callFieldTool calls one read tool and returns its answer, which must be no refusal.
func callFieldTool(t *testing.T, set *tools.Set, name string, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	r, err := set.Call(context.Background(), providers.ToolCall{ID: "c1", Name: name, Arguments: raw})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != tools.StatusOK {
		t.Fatalf("%s answered %s: %s", name, r.Status, r.Text)
	}
	return r.Text
}

// readFieldsMainTable reads fieldsMainTable: whether main hid each row's secret, by row key.
func readFieldsMainTable(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(fieldsMainTable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	rows := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, '\t')
		if i < 0 {
			t.Fatalf("%s: malformed line %q", fieldsMainTable, line)
		}
		rows[line[:i]] = line[i+1:] == "hidden"
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestFieldsOfEveryToolHideWhatMainHid(t *testing.T) {
	results, places := fieldResults(t)
	main := readFieldsMainTable(t)
	lost, hidden, mainHidden := 0, 0, 0
	seen := map[string]bool{}
	for _, r := range results {
		seen[r.key()] = true
		was, ok := main[r.key()]
		if !ok {
			t.Errorf("%s: no row in %s (write it again from main)", r.key(), fieldsMainTable)
			continue
		}
		if r.hidden {
			hidden++
		}
		if was {
			mainHidden++
		}
		if was && !r.hidden {
			if lost++; lost <= 20 {
				t.Errorf("%s %s, %s: the secret shows, main hid it", r.tool, r.place, r.form)
			}
		}
	}
	for k := range main {
		if !seen[k] {
			t.Errorf("%s: row of %s that the walk no longer makes", k, fieldsMainTable)
		}
	}
	if lost > 0 {
		t.Errorf("%d secrets show that main hid", lost)
	}
	t.Logf("%d calls, places per tool %v; hidden %d, by main %d", len(results), places, hidden, mainHidden)
}

// TestWriteFieldsMainTable writes fieldsMainTable from the code it runs with, when FIELDS_MAIN_TABLE names the
// output path (and FIELDS_MAIN_COMMIT the commit, for its first line); it is run once in a checkout of main.
func TestWriteFieldsMainTable(t *testing.T) {
	path := os.Getenv("FIELDS_MAIN_TABLE")
	if path == "" {
		t.Skip("FIELDS_MAIN_TABLE is not set")
	}
	results, _ := fieldResults(t)
	var b strings.Builder
	b.WriteString("# Whether each read tool hid a secret placed in each field of the objects it reads, at main " +
		os.Getenv("FIELDS_MAIN_COMMIT") + " (#845).\n# tool\tplace\tform\thidden or shown\n")
	for _, r := range results {
		b.WriteString(r.key() + "\t" + map[bool]string{true: "hidden", false: "shown"}[r.hidden] + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil { // #nosec G703 -- a path the person running the test names
		t.Fatal(err)
	}
}
