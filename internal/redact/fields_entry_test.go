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
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp/syntax"
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
	"github.com/pkalin/shadewright/internal/redact"
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
// hid that is shown now fails. The file uses only the read tools' API (and the two key expressions of
// export_keys_test.go, which main has too), so it runs unchanged in that checkout with that file copied.

// fieldsMainTable holds, for each place and form, whether main hid the value.
const fieldsMainTable = "testdata/fields-main-entry.tsv"

// rowHidden and rowShown are the last column of a table written from main.
const (
	rowHidden = "hidden"
	rowShown  = "shown"
)

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
	// word: the secret is a word of letters (fieldWord), not shaped like a credential, so only the key can
	// hide it.
	word bool
}

// fieldWord is the secret of call n as a word of letters only: no digit and no symbol, so the rule for a bare
// short key in prose ("auth: failed"), which wants a value shaped like a credential, does not take it.
func fieldWord(n int) string {
	var b strings.Builder
	b.WriteString("Vfw")
	for _, d := range fmt.Sprintf("%06d", n) {
		b.WriteRune('a' + d - '0')
	}
	b.WriteString("xkqw")
	return b.String()
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

// fieldListForms place a secret flag and its value as two elements of one list of strings: the string after a
// secret flag is its value whatever it looks like (#845, third decision of 2026-11-18).
var fieldListForms = []fieldListForm{
	{name: "flag, dashed value", items: func(v string) []string { return []string{"--" + fieldsKey, "--" + v} }},
	{name: "token flag, short dashed value", items: func(v string) []string { return []string{"--token", "-" + v} }},
	{name: "flag, short flag with value", items: func(v string) []string { return []string{"--" + fieldsKey, "-p=" + v} }},
	{name: "client-secret flag, flag with value", items: func(v string) []string { return []string{"--client-secret", "--x=" + v} }},
	{name: "flag, plain value", items: func(v string) []string { return []string{"--" + fieldsKey, v} }},
}

// listForms returns fieldListForms and, for the value shapes of turn (shapesAt), a secret flag and the value in
// that shape.
func listForms(turn int) []fieldListForm {
	out := slices.Clone(fieldListForms)
	for _, sh := range shapesAt(turn, fieldListShapes) {
		out = append(out, fieldListForm{name: "flag, value " + sh.name, items: func(v string) []string { return []string{"--" + fieldsKey, sh.shape(v)} }})
	}
	return out
}

// fieldListForm is one way of placing a secret key and its value in two elements of a list of strings.
type fieldListForm struct {
	name  string
	items func(v string) []string
}

// fieldPairForms place a secret key name in one place and the secret in another (#845, third decision of
// 2026-11-18): a sibling of the same object, or the place that follows in document order. An HTTP header's
// name and value, a flag at the end of a command and its value in the arguments, a selector's key and values.
var fieldPairForms = []fieldPairForm{
	{name: "flag, value", key: "--" + fieldsKey},
	{name: "key and colon, value", key: fieldsKey + ":"},
	{name: "header name, value", key: "X-Api-Key"},
	{name: "key name, value", key: fieldsKey},
}

// fieldPairForm is one way of placing a key in one place and its value in another: the key is the whole text of
// the first place (or of the one element of its list), the value the whole text of the second.
type fieldPairForm struct {
	name, key string
}

// fieldPairs returns the pairs of places of a type that a pair form fills: two string places or lists of strings
// that are siblings (fields of the same object), the first before the second, and each such place with the one
// after it in document order.
func fieldPairs(places []fieldPlace) [][2]fieldPlace {
	var text []fieldPlace
	for _, p := range places {
		if p.kind != reflect.Map {
			text = append(text, p)
		}
	}
	parent := func(p fieldPlace) string {
		var b strings.Builder
		for _, s := range p.path[:len(p.path)-1] {
			fmt.Fprintf(&b, "%d.", s.field)
		}
		return b.String()
	}
	var out [][2]fieldPlace
	seen := map[[2]int]bool{}
	for i, p := range text {
		for j, q := range text {
			if i == j || len(p.path) == 0 || len(q.path) == 0 {
				continue
			}
			if (i < j && parent(p) == parent(q) && len(p.path) == len(q.path)) || j == i+1 {
				if !seen[[2]int{i, j}] {
					seen[[2]int{i, j}] = true
					out = append(out, [2]fieldPlace{p, q})
				}
			}
		}
	}
	return out
}

// setText writes text into the string place p of root, or as the one element of the list p.
func (p fieldPlace) setText(root reflect.Value, text string) {
	p.set(root, fieldForm{text: func(string) string { return text }}, "")
}

// forms returns the forms of the place of number turn: the fixed forms, then value shapes (fieldShapes) with a
// form of the place, and in a map of strings every secret key (fieldSecretKeys) with a plain value and with a
// word (fieldWord) (#845, fifth decision of 2026-11-18). A string or a list of strings gets fieldStringShapes of
// the shapes, the next ones at each place (shapesAt), so every place gets values after a line break, blanks or a
// marker and each shape reaches every tool's places; a map gets every shape, the keys taken in turn. The shapes
// take the forms of their place in turn.
func (p fieldPlace) forms(turn int) []fieldForm {
	switch {
	case p.kind != reflect.Map:
		out := slices.Clone(fieldStringForms)
		for i, sh := range shapesAt(turn, fieldStringShapes) {
			f := fieldStringForms[i%len(fieldStringForms)]
			out = append(out, fieldForm{name: f.name + ", " + sh.name, text: func(v string) string { return f.text(sh.shape(v)) }})
		}
		return out
	case p.elem.Kind() == reflect.String:
		out := slices.Clone(fieldMapForms)
		keys := fieldSecretKeys()
		for _, k := range keys {
			out = append(out, fieldForm{name: "every key " + k, key: func(string) string { return k }, value: func(v string) string { return v }})
			out = append(out, fieldForm{name: "every key " + k + ", a word", key: func(string) string { return k }, value: func(v string) string { return v }, word: true})
		}
		for i, sh := range fieldShapes {
			k := keys[i*7%len(keys)]
			out = append(out, fieldForm{
				name: "every key " + k + ", " + sh.name, key: func(string) string { return k }, value: sh.shape,
			})
		}
		return out
	}
	return fieldMapForms[len(fieldMapForms)-1:]
}

// How many value shapes a string place, a list of strings and a pair of places get (shapesAt): all of them at
// every place would be some 170,000 calls, more than the package's test run allows.
const (
	fieldStringShapes = 4
	fieldListShapes   = 3
	fieldPairShapes   = 2
)

// shapesAt returns n of fieldShapes for the place or pair of number turn, the next n after those of turn-1, so
// consecutive places go through all the shapes.
func shapesAt(turn, n int) []fieldShape {
	out := make([]fieldShape, n)
	for i := range n {
		out[i] = fieldShapes[(turn*n+i)%len(fieldShapes)]
	}
	return out
}

// fieldShape is a way of writing a secret value v: after a line break, a line of text, blanks or a marker
// that the laid-out text reads as its own (#845, fifth decision of 2026-11-18). main encoded the answer as
// JSON before its second pass, so a line break in a value was "\n" there and the value stayed one.
type fieldShape struct {
	name  string
	shape func(v string) string
}

var fieldShapes = []fieldShape{
	{name: "after a line break", shape: func(v string) string { return "\n" + v }},
	{name: "after CRLF", shape: func(v string) string { return "\r\n" + v }},
	{name: "after two line breaks", shape: func(v string) string { return "\n\n" + v }},
	{name: "on the second line", shape: func(v string) string { return "line1\n" + v }},
	{name: "indented on the second line", shape: func(v string) string { return "x\n  " + v }},
	{name: "after the end of a list", shape: func(v string) string { return "(end of list)\n" + v }},
	{name: "after a key line", shape: func(v string) string { return "a: b\n" + v }},
	{name: "after a literal block marker", shape: func(v string) string { return "|\n" + v }},
	{name: "after a folded block marker", shape: func(v string) string { return ">\n " + v }},
	{name: "after leading blanks", shape: func(v string) string { return "   " + v }},
	{name: "after a list dash", shape: func(v string) string { return "- " + v }},
	{name: "after a comment line", shape: func(v string) string { return "# note\n" + v }},
	{name: "after a lone CR", shape: func(v string) string { return "x\r" + v }},
}

// fieldSecretKeys returns every key the secret key list names, by construction from its expressions: each
// string the secret words match (redact.SecretWordExpr), each short key name (redact.ShortKeyNamesExpr), and
// the short names as the last part of a longer key. Every one is a key main hid the value of, quoted.
func fieldSecretKeys() []string {
	re, err := syntax.Parse(redact.SecretWordExpr, syntax.Perl)
	if err != nil {
		panic(err)
	}
	var words func(re *syntax.Regexp) []string
	words = func(re *syntax.Regexp) []string {
		switch re.Op {
		case syntax.OpLiteral:
			return []string{string(re.Rune)}
		case syntax.OpCharClass:
			var out []string
			for i := 0; i < len(re.Rune); i += 2 {
				for r := re.Rune[i]; r <= re.Rune[i+1]; r++ {
					out = append(out, string(r))
				}
			}
			return out
		case syntax.OpQuest:
			return append([]string{""}, words(re.Sub[0])...)
		case syntax.OpCapture:
			return words(re.Sub[0])
		case syntax.OpAlternate:
			var out []string
			for _, s := range re.Sub {
				out = append(out, words(s)...)
			}
			return out
		case syntax.OpConcat:
			out := []string{""}
			for _, s := range re.Sub {
				var next []string
				for _, a := range out {
					for _, b := range words(s) {
						next = append(next, a+b)
					}
				}
				out = next
			}
			return out
		case syntax.OpEmptyMatch:
			return []string{""}
		}
		panic(fmt.Sprintf("secret word expression: unexpected %v", re.Op))
	}
	out := words(re)
	for short := range strings.SplitSeq(redact.ShortKeyNamesExpr, "|") {
		out = append(out, short, "DB_"+strings.ToUpper(short), "proxy-"+short)
	}
	out = append(out, "storageSas")
	slices.Sort(out)
	return slices.Compact(out)
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
	last     map[string]any // the arguments of the last call (fieldTool.call)
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
	// skipped: not called in this run (a relation row outside the sample under -race).
	skipped bool
}

// key is the result's row key: its tool, place and form.
func (r fieldResult) key() string { return r.tool + "\t" + r.place + "\t" + r.form }

// id is the result's row in the table: the first 16 hex digits of the SHA-256 of its key, which keeps the table
// of some 170,000 rows small. A failure names the key, not the id.
func (r fieldResult) id() string {
	sum := sha256.Sum256([]byte(r.key()))
	return hex.EncodeToString(sum[:8])
}

// fieldResults calls every tool with a secret in every place of the types it reads, in every form of that
// place, and returns the results and the number of places per tool.
// With sample set, only the rows redact.RaceKeeps keeps are called under -race, the others are returned skipped.
func fieldResults(t *testing.T, sample bool) ([]fieldResult, map[string]int) {
	t.Helper()
	var out []fieldResult
	places := map[string]int{}
	n := 0
	for _, ft := range fieldTools {
		e := newFieldEnv(t)
		for _, typ := range ft.types {
			for turn, p := range fieldPlaces(typ) {
				places[ft.name]++
				for _, f := range p.forms(turn) {
					n++
					v := fmt.Sprintf("Vf%06dxK9w", n)
					if f.word {
						v = fieldWord(n)
					}
					if sample && !redact.RaceKeeps("fields", n) {
						out = append(out, fieldResult{tool: ft.name, place: p.name(typ), form: f.name, skipped: true})
						continue
					}
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

// readFieldsMainTable reads fieldsMainTable: whether main hid each row's secret, by row id.
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
		rows[line[:i]] = line[i+1:] == rowHidden
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestFieldsOfEveryToolHideWhatMainHid(t *testing.T) {
	results, places := fieldResults(t, true)
	relations, calls := fieldRelationResults(t, true)
	marshaled, byType := fieldMarshaledResults(t, true)
	keys, maps := fieldKeyResults(t, true)
	literals, literalsByType := fieldLiteralResults(t, true, true)
	results = append(append(append(append(results, relations...), marshaled...), keys...), literals...)
	t.Logf("relation calls per tool %v; places of types that write their own JSON %v; maps per tool %v; literal places per type %v",
		calls, byType, maps, literalsByType)
	main := readFieldsMainTable(t)
	lost, hidden, mainHidden := 0, 0, 0
	rows, lostBy := map[string]int{}, map[string]int{}
	seen := map[string]bool{}
	for _, r := range results {
		seen[r.id()] = true
		if r.skipped {
			continue
		}
		was, ok := main[r.id()]
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
		group := fieldRowGroup(r)
		rows[group]++
		if was && !r.hidden {
			lostBy[group]++
			if lost++; lost <= 20 {
				t.Errorf("%s %s, %s: the secret shows, main hid it", r.tool, r.place, r.form)
			}
		}
	}
	for k := range main {
		if !seen[k] {
			t.Errorf("row %s of %s: the walk no longer makes it", k, fieldsMainTable)
		}
	}
	if lost > 0 {
		t.Errorf("%d secrets show that main hid", lost)
	}
	t.Logf("%d calls, places per tool %v; hidden %d, by main %d; rows called %v, shown that main hid %v",
		len(results), places, hidden, mainHidden, rows, lostBy)
}

// TestWriteFieldsMainTable writes fieldsMainTable from the code it runs with, when FIELDS_MAIN_TABLE names the
// output path (and FIELDS_MAIN_COMMIT the commit, for its first line); it is run once in a checkout of main.
func TestWriteFieldsMainTable(t *testing.T) {
	path := os.Getenv("FIELDS_MAIN_TABLE")
	if path == "" {
		t.Skip("FIELDS_MAIN_TABLE is not set")
	}
	results, _ := fieldResults(t, false)
	relations, _ := fieldRelationResults(t, false)
	marshaled, _ := fieldMarshaledResults(t, false)
	keys, _ := fieldKeyResults(t, false)
	literals, _ := fieldLiteralResults(t, false, false)
	results = append(append(append(append(results, relations...), marshaled...), keys...), literals...)
	var b strings.Builder
	b.WriteString("# Whether each read tool hid a secret placed in each field of the objects it reads, at main " +
		os.Getenv("FIELDS_MAIN_COMMIT") + " (#845).\n# row (the first 16 hex digits of the SHA-256 of \"tool<TAB>place<TAB>form\")\thidden or shown\n")
	for _, r := range results {
		b.WriteString(r.id() + "\t" + map[bool]string{true: rowHidden, false: rowShown}[r.hidden] + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil { // #nosec G703 -- a path the person running the test names
		t.Fatal(err)
	}
}

// call fills a new object of typ with fill, gives it to the tool and reports whether v shows in the answer.
func (ft fieldTool) call(t *testing.T, e *fieldEnv, typ reflect.Type, v string, fill func(obj reflect.Value)) bool {
	t.Helper()
	obj := reflect.New(typ)
	fill(obj)
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
	e.last = args
	text := callFieldTool(t, e.set, ft.name, args)
	return strings.Contains(text, v[:8]) || strings.Contains(text, v[len(v)-8:])
}

// answer calls the tool again with the arguments of its last call and returns the answer.
func (ft fieldTool) answer(t *testing.T, e *fieldEnv) string {
	t.Helper()
	return callFieldTool(t, e.set, ft.name, e.last)
}

// at returns the value at the place p of root, creating the pointers and the first elements of slices on the
// way.
func (p fieldPlace) at(root reflect.Value) reflect.Value {
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
	return cur
}

// fieldRelationResults calls every tool with a secret key in one element of a list and its value in the next
// (fieldListForms), and with a key in one place and its value in another (fieldPairs, fieldPairForms), and
// returns the results and the number of calls per tool. Its rows follow those of fieldResults in the table.
// With sample set, only the rows redact.RaceKeeps keeps are called under -race, the others are returned skipped.
func fieldRelationResults(t *testing.T, sample bool) ([]fieldResult, map[string]int) {
	t.Helper()
	var out []fieldResult
	calls := map[string]int{}
	n := 0
	for _, ft := range fieldTools {
		e := newFieldEnv(t)
		for _, typ := range ft.types {
			places := fieldPlaces(typ)
			for _, p := range places {
				if p.kind == reflect.Map {
					continue
				}
				// The value alone, with no key: what main hid by the place alone (a value in a
				// collection under a secret-named field).
				n++
				v := fmt.Sprintf("Vl%06dxK9w", n)
				if sample && !redact.RaceKeeps("fields relations", n) {
					out = append(out, fieldResult{tool: ft.name, place: p.name(typ), form: "plain value", skipped: true})
				} else {
					calls[ft.name]++
					shown := ft.call(t, e, typ, v, func(obj reflect.Value) { p.setText(obj, v) })
					out = append(out, fieldResult{tool: ft.name, place: p.name(typ), form: "plain value", hidden: !shown})
				}
			}
			for turn, p := range places {
				if p.kind != reflect.Slice {
					continue
				}
				for _, f := range listForms(turn) {
					n++
					v := fmt.Sprintf("Vl%06dxK9w", n)
					if sample && !redact.RaceKeeps("fields relations", n) {
						out = append(out, fieldResult{tool: ft.name, place: p.name(typ), form: "list " + f.name, skipped: true})
						continue
					}
					calls[ft.name]++
					shown := ft.call(t, e, typ, v, func(obj reflect.Value) {
						cur := p.at(obj)
						items := f.items(v)
						l := reflect.MakeSlice(cur.Type(), len(items), len(items))
						for i, it := range items {
							l.Index(i).SetString(it)
						}
						cur.Set(l)
					})
					out = append(out, fieldResult{tool: ft.name, place: p.name(typ), form: "list " + f.name, hidden: !shown})
				}
			}
			for turn, pq := range fieldPairs(places) {
				p, q := pq[0], pq[1]
				for _, f := range fieldPairForms {
					n++
					v := fmt.Sprintf("Vp%06dxK9w", n)
					if sample && !redact.RaceKeeps("fields relations", n) {
						out = append(out, fieldResult{tool: ft.name, place: p.name(typ) + " > " + q.name(typ), form: "pair " + f.name, skipped: true})
						continue
					}
					calls[ft.name]++
					shown := ft.call(t, e, typ, v, func(obj reflect.Value) {
						p.setText(obj, f.key)
						q.setText(obj, v)
					})
					out = append(out, fieldResult{tool: ft.name, place: p.name(typ) + " > " + q.name(typ), form: "pair " + f.name, hidden: !shown})
				}
				// The value in the shapes of this pair (shapesAt), the pair forms taken in turn.
				for i, sh := range shapesAt(turn, fieldPairShapes) {
					f := fieldPairForms[(turn+i)%len(fieldPairForms)]
					form := "pair " + f.name + ", " + sh.name
					n++
					v := fmt.Sprintf("Vp%06dxK9w", n)
					if sample && !redact.RaceKeeps("fields relations", n) {
						out = append(out, fieldResult{tool: ft.name, place: p.name(typ) + " > " + q.name(typ), form: form, skipped: true})
						continue
					}
					calls[ft.name]++
					shown := ft.call(t, e, typ, v, func(obj reflect.Value) {
						p.setText(obj, f.key)
						q.setText(obj, sh.shape(v))
					})
					out = append(out, fieldResult{tool: ft.name, place: p.name(typ) + " > " + q.name(typ), form: form, hidden: !shown})
				}
			}
		}
	}
	return out, calls
}

// Two map keys that redact alike stay two entries (#845, fifth decision of 2026-11-18): a value only partly
// hidden never merges entries, so a map keeps as many entries as the object has, as main kept them. Each map of
// strings that a tool shows gets three keys that redact to the same marker (tokens of one length) with values
// that are no secret, and each value must show.
func TestMapsKeepEveryEntry(t *testing.T) {
	values := []string{"Ent1qzk", "Ent2qzk", "Ent3qzk"}
	maps := 0
	for _, ft := range fieldTools {
		e := newFieldEnv(t)
		for _, typ := range ft.types {
			for _, p := range fieldPlaces(typ) {
				if p.kind != reflect.Map || p.elem.Kind() != reflect.String {
					continue
				}
				// A map the tool does not show is no place to lose an entry.
				if !ft.call(t, e, typ, "Ent0zzkq", func(obj reflect.Value) {
					cur := p.at(obj)
					m := reflect.MakeMap(cur.Type())
					m.SetMapIndex(reflect.ValueOf("k").Convert(cur.Type().Key()), reflect.ValueOf("Ent0zzkq").Convert(cur.Type().Elem()))
					cur.Set(m)
				}) {
					continue
				}
				maps++
				var lost []string
				ft.call(t, e, typ, "unused-value", func(obj reflect.Value) {
					cur := p.at(obj)
					m := reflect.MakeMap(cur.Type())
					for i, v := range values {
						key := "ghp_" + strings.Repeat(string(rune('A'+i)), 36)
						m.SetMapIndex(reflect.ValueOf(key).Convert(cur.Type().Key()), reflect.ValueOf(v).Convert(cur.Type().Elem()))
					}
					cur.Set(m)
				})
				// call reports only one value; read the answer again for all three.
				text := ft.answer(t, e)
				for _, v := range values {
					if !strings.Contains(text, v) {
						lost = append(lost, v)
					}
				}
				if len(lost) > 0 {
					t.Errorf("%s %s: entries %v are gone; the map had %d", ft.name, p.name(typ), lost, len(values))
				}
			}
		}
	}
	if maps == 0 {
		t.Fatal("no tool shows a map of strings")
	}
	t.Logf("%d maps of strings shown", maps)
}
