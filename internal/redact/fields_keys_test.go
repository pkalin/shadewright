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
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/tools"
)

// The key places and the non-string tokens of the field walk (#845, eighth decision of 2026-11-18). A key of a
// free-form map (annotations, volumeAttributes, a nodeSelector, labels, the names of a ResourceList) holds the
// secret after each value shape (fieldShapes): main redacted no key alone, only its answer, so a key is hidden as
// far as main hid it there (fieldKeyResults). A number or a boolean is placed in every such field of the objects
// get_resource reads: inside a collection main hid whole, it is hidden too, and the answer stays JSON
// (fieldLiteralResults). The rows follow those of fieldMarshaledResults in testdata/fields-main-entry.tsv and are
// written from main like them; the file uses only the read tools' API, so it runs unchanged in a checkout of main.

// fieldKeyForms are the ways of writing the secret v into a map key after a value shape: the value of a key and
// equals sign, and the text after a connection string, which main's answer ran on to the closing quote.
var fieldKeyForms = []struct {
	name string
	key  func(shaped string) string
}{
	{name: "key password equals", key: func(s string) string { return fieldsKey + "=" + s }},
	{name: "key connection string", key: func(s string) string { return "postgres" + "://u:" + "Pw9q" + "@h/db" + s }},
}

// fieldKeyResults calls every tool with the secret in the key of every map with string keys of the types it reads,
// in every key form and value shape, and returns the results and the number of map places per tool. With sample
// set, only the rows redact.RaceKeeps keeps are called under -race.
func fieldKeyResults(t *testing.T, sample bool) ([]fieldResult, map[string]int) {
	t.Helper()
	var out []fieldResult
	maps := map[string]int{}
	n := 0
	for _, ft := range fieldTools {
		e := newFieldEnv(t)
		for _, typ := range ft.types {
			for _, p := range fieldPlaces(typ) {
				if p.kind != reflect.Map {
					continue
				}
				maps[ft.name]++
				for _, kf := range fieldKeyForms {
					for _, sh := range fieldShapes {
						n++
						v := fmt.Sprintf("Vk%06dxK9w", n)
						form := "in the key, " + kf.name + ", " + sh.name
						if sample && !redact.RaceKeeps("fields keys", n) {
							out = append(out, fieldResult{tool: ft.name, place: p.name(typ), form: form, skipped: true})
							continue
						}
						f := fieldForm{key: func(v string) string { return kf.key(sh.shape(v)) }, value: func(string) string { return "x" }}
						shown := ft.call(t, e, typ, v, func(obj reflect.Value) { p.set(obj, f, v) })
						out = append(out, fieldResult{tool: ft.name, place: p.name(typ), form: form, hidden: !shown})
					}
				}
			}
		}
	}
	return out, maps
}

// literalPlace is a field of a type that holds a number or a boolean, with its JSON name.
type literalPlace struct {
	fieldPlace
	kind reflect.Kind
	json string
}

// literalPlaces walks t as fieldPlaces does and returns every place of a number or a boolean, not inside a type that
// writes its own JSON or a map.
func literalPlaces(t reflect.Type) []literalPlace {
	var out []literalPlace
	var walk func(t reflect.Type, path []fieldStep, name string, seen []reflect.Type)
	walk = func(t reflect.Type, path []fieldStep, name string, seen []reflect.Type) {
		if writesItself(t) || slices.Contains(seen, t) {
			return
		}
		at := func(s fieldStep) []fieldStep { return append(slices.Clone(path), s) }
		switch k := t.Kind(); k {
		case reflect.Bool, reflect.Int, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
			out = append(out, literalPlace{fieldPlace: fieldPlace{path: path}, kind: k, json: name})
		case reflect.Pointer:
			walk(t.Elem(), at(fieldStep{field: -1}), name, seen)
		case reflect.Slice:
			walk(t.Elem(), at(fieldStep{field: -2}), name, seen)
		case reflect.Struct:
			seen = append(seen, t)
			for i := range t.NumField() {
				f := t.Field(i)
				if !f.IsExported() {
					continue
				}
				tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
				if tag == "-" {
					continue
				}
				if tag == "" {
					tag = f.Name
				}
				walk(f.Type, at(fieldStep{field: i, name: f.Name}), tag, seen)
			}
		}
	}
	walk(t, nil, "", nil)
	return out
}

// fieldLiteralResults calls get_resource with a number (digits made from n) or true in every literal place of the
// types it reads, and returns the results and the number of places by tool. A number is shown when its digits
// are in the answer, true when the field's name is followed by it. With checkJSON set, an answer that is no JSON
// fails (main's was none where it hid a collection whole). With sample set, only the rows redact.RaceKeeps keeps
// are called under -race.
func fieldLiteralResults(t *testing.T, sample, checkJSON bool) ([]fieldResult, map[string]int) {
	t.Helper()
	var out []fieldResult
	places := map[string]int{}
	n := 0
	for _, ft := range fieldTools {
		if ft.name != tools.NameGetResource {
			continue
		}
		e := newFieldEnv(t)
		for _, typ := range ft.types {
			for _, p := range literalPlaces(typ) {
				places[typ.Name()]++
				n++
				form := "a number"
				if p.kind == reflect.Bool {
					form = "true"
				}
				if sample && !redact.RaceKeeps("fields literals", n) {
					out = append(out, fieldResult{tool: ft.name, place: p.name(typ), form: form, skipped: true})
					continue
				}
				num := 61000000 + n
				obj := reflect.New(typ)
				switch cur := p.at(obj); {
				case p.kind == reflect.Bool:
					cur.SetBool(true)
				case cur.CanInt():
					cur.SetInt(int64(num))
				case cur.CanUint():
					cur.SetUint(uint64(num))
				default:
					cur.SetFloat(float64(num))
				}
				if m, ok := reflect.TypeAssert[metav1.Object](obj); ok && m.GetName() == "" {
					m.SetName(fieldsPod)
					m.SetNamespace(fieldsNS)
				}
				e.reader.obj, e.reader.events = nil, nil
				ft.put(e, obj)
				text := callFieldTool(t, e.set, ft.name, map[string]any{fieldsArgKind: typ.Name(), fieldsArgNS: fieldsNS, fieldsArgName: fieldsPod})
				shown := strings.Contains(text, strconv.Itoa(num))
				if p.kind == reflect.Bool {
					shown = strings.Contains(text, strconv.Quote(p.json)+": true")
				}
				if checkJSON && !json.Valid([]byte(text)) {
					t.Errorf("%s %s, %s: the answer is no JSON:\n%.2000s", ft.name, p.name(typ), form, text)
				}
				out = append(out, fieldResult{tool: ft.name, place: p.name(typ), form: form, hidden: !shown})
			}
		}
	}
	return out, places
}

// fieldRowGroup names the group of a row for the counts of TestFieldsOfEveryToolHideWhatMainHid: a key place, a
// number or a boolean, a hidden text with a signal, or any other.
func fieldRowGroup(r fieldResult) string {
	switch {
	case strings.HasPrefix(r.form, "signal "):
		return "signals"
	case strings.HasPrefix(r.form, "in the key, "):
		return "keys"
	case r.form == "a number" || r.form == "true":
		return "literals"
	}
	return "values"
}
