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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/tools"
)

// A type that writes its own JSON as a string is a place like any other (#845, seventh decision of 2026-11-18,
// point 1). The field walk of fields_entry_test.go places text in string fields, lists of strings and maps, so a
// string in an IntOrString (a probe's port written as "--password V") was never placed: get_resource showed it
// and main hid it. Here the walk finds every type of the objects get_resource reads that implements
// json.Marshaler, and places a secret in each through the value of that type: as its text (IntOrString, the raw
// content of FieldsV1 and RawExtension), as digits under every secret key of a map of quantities, and so on. A
// type the walk finds that marshaledKinds does not name fails, so a new one is not missed. The rows follow those
// of fieldRelationResults in testdata/fields-main-entry.tsv and are written from main like them.

// marshaledKind says how a value of a type that writes its own JSON carries a secret v: as text (text), as
// digits (digits, a quantity), or not at all (neither: a time, written in a fixed format).
type marshaledKind struct {
	text   func(v string) reflect.Value
	digits func(d string) reflect.Value
	// object, for a type that writes raw JSON, carries v as the value of a secret key in a JSON object.
	object func(v string) reflect.Value
}

// rawJSON returns the JSON of v, for the raw content of FieldsV1 and RawExtension.
func rawJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// marshaledKinds are the types that write their own JSON in the objects the read tools show.
var marshaledKinds = map[reflect.Type]marshaledKind{
	reflect.TypeFor[intstr.IntOrString](): {text: func(v string) reflect.Value { return reflect.ValueOf(intstr.FromString(v)) }},
	reflect.TypeFor[metav1.FieldsV1](): {
		text: func(v string) reflect.Value { return reflect.ValueOf(*metav1.NewFieldsV1(string(rawJSON(v)))) },
		object: func(v string) reflect.Value {
			return reflect.ValueOf(*metav1.NewFieldsV1(string(rawJSON(map[string]string{fieldsKey: v}))))
		},
	},
	reflect.TypeFor[runtime.RawExtension](): {
		text: func(v string) reflect.Value { return reflect.ValueOf(runtime.RawExtension{Raw: rawJSON(v)}) },
		object: func(v string) reflect.Value {
			return reflect.ValueOf(runtime.RawExtension{Raw: rawJSON(map[string]string{fieldsKey: v})})
		},
	},
	reflect.TypeFor[resource.Quantity](): {digits: func(d string) reflect.Value { return reflect.ValueOf(resource.MustParse(d)) }},
	reflect.TypeFor[metav1.Time]():       {},
	reflect.TypeFor[metav1.MicroTime]():  {},
	reflect.TypeFor[metav1.Duration]():   {},
}

// marshaledPlace is a place of a type that writes its own JSON: a field or the first element of a list (inMap
// false), or the values of a map with string keys (inMap).
type marshaledPlace struct {
	fieldPlace
	typ   reflect.Type
	inMap bool
}

// writesItself reports whether values of t write their own JSON.
func writesItself(t reflect.Type) bool {
	m := reflect.TypeFor[json.Marshaler]()
	return t.Kind() == reflect.Struct && (t.Implements(m) || reflect.PointerTo(t).Implements(m))
}

// marshaledPlaces walks t and returns every place of a type that writes its own JSON, in the order of its
// fields, as fieldPlaces walks it.
func marshaledPlaces(t reflect.Type) []marshaledPlace {
	var out []marshaledPlace
	var walk func(t reflect.Type, path []fieldStep, seen []reflect.Type)
	walk = func(t reflect.Type, path []fieldStep, seen []reflect.Type) {
		if writesItself(t) {
			out = append(out, marshaledPlace{fieldPlace: fieldPlace{path: path}, typ: t})
			return
		}
		if slices.Contains(seen, t) {
			return
		}
		seen = append(seen, t)
		at := func(s fieldStep) []fieldStep { return append(slices.Clone(path), s) }
		switch t.Kind() {
		case reflect.Pointer:
			walk(t.Elem(), at(fieldStep{field: -1}), seen)
		case reflect.Slice:
			walk(t.Elem(), at(fieldStep{field: -2}), seen)
		case reflect.Map:
			if t.Key().Kind() == reflect.String && writesItself(t.Elem()) {
				out = append(out, marshaledPlace{fieldPlace: fieldPlace{path: path, kind: reflect.Map, elem: t.Elem()}, typ: t.Elem(), inMap: true})
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

// marshaledForm is one way of placing a secret v in a marshaled place: the value, and for a map its key.
type marshaledForm struct {
	name  string
	key   string
	value func(v string) reflect.Value
}

// marshaledForms returns the forms of the place p of type kind k, and the secret each one places (text, or the
// digits of a quantity, made from n).
func (p marshaledPlace) forms(k marshaledKind) []marshaledForm {
	var out []marshaledForm
	switch {
	case p.inMap && k.text != nil:
		for _, key := range fieldSecretKeys() {
			out = append(out, marshaledForm{name: "marshaled every key " + key, key: key, value: k.text})
		}
	case p.inMap && k.digits != nil:
		for _, key := range fieldSecretKeys() {
			out = append(out, marshaledForm{name: "marshaled digits, every key " + key, key: key, value: k.digits})
		}
	case !p.inMap && k.text != nil:
		for _, f := range fieldStringForms {
			out = append(out, marshaledForm{name: "marshaled " + f.name, value: func(v string) reflect.Value { return k.text(f.text(v)) }})
		}
		out = append(out, marshaledForm{name: "marshaled flag and value", value: func(v string) reflect.Value { return k.text("--" + fieldsKey + " " + v) }})
		for _, sh := range fieldShapes {
			out = append(out, marshaledForm{name: "marshaled password colon, " + sh.name, value: func(v string) reflect.Value { return k.text(fieldsKey + ": " + sh.shape(v)) }})
		}
		if k.object != nil {
			out = append(out, marshaledForm{name: "marshaled object with a secret key", value: k.object})
		}
	}
	return out
}

// set writes the value of the form with the secret v into the place of root.
func (p marshaledPlace) set(root reflect.Value, f marshaledForm, v string) {
	cur := p.at(root)
	val := f.value(v)
	if p.inMap {
		m := reflect.MakeMap(cur.Type())
		m.SetMapIndex(reflect.ValueOf(f.key).Convert(cur.Type().Key()), val)
		cur.Set(m)
		return
	}
	cur.Set(val)
}

// fieldMarshaledResults calls get_resource with a secret in every marshaled place of the types it reads, in every
// form of that place, and returns the results and the number of places by type. With sample set, only the rows
// redact.RaceKeeps keeps are called under -race.
func fieldMarshaledResults(t *testing.T, sample bool) ([]fieldResult, map[string]int) {
	t.Helper()
	var out []fieldResult
	byType := map[string]int{}
	n := 0
	for _, ft := range fieldTools {
		if ft.name != tools.NameGetResource {
			continue
		}
		e := newFieldEnv(t)
		for _, typ := range ft.types {
			for _, p := range marshaledPlaces(typ) {
				k, ok := marshaledKinds[p.typ]
				if !ok {
					t.Errorf("%s %s: type %s writes its own JSON and marshaledKinds does not say how it carries text", ft.name, p.name(typ), p.typ)
					continue
				}
				byType[p.typ.String()]++
				for _, f := range p.forms(k) {
					n++
					v := fmt.Sprintf("Vm%06dxK9w", n)
					if strings.Contains(f.name, "digits") {
						// A quantity's digits, none a trailing zero, so the quantity is written as it is.
						v = fmt.Sprintf("97%06d13", n)
					}
					place := p.name(typ) + " (" + p.typ.String() + ")"
					if sample && !redact.RaceKeeps("fields marshaled", n) {
						out = append(out, fieldResult{tool: ft.name, place: place, form: f.name, skipped: true})
						continue
					}
					shown := ft.call(t, e, typ, v, func(obj reflect.Value) { p.set(obj, f, v) })
					out = append(out, fieldResult{tool: ft.name, place: place, form: f.name, hidden: !shown})
				}
			}
		}
	}
	return out, byType
}

// The digits of fieldMarshaledResults are written by a quantity as they are, so a shown quantity is found.
func TestMarshaledDigitsAreWrittenAsTheyAre(t *testing.T) {
	for _, n := range []int{1, 999999} {
		d := fmt.Sprintf("97%06d13", n)
		q := resource.MustParse(d)
		if q.String() != d {
			t.Errorf("quantity %s is written %s", d, q.String())
		}
	}
}
