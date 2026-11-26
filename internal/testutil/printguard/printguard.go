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

// Package printguard checks that a value holding a secret shows none of it when it is printed, logged or
// encoded, alone or inside another value. fmt and slog walk a struct without calling the methods of its
// unexported fields, and for a verb that does not fit a pointer (%s, %q, %x, %d) fmt prints the target of
// the pointer in its bad-verb form, so a type's own print methods are not enough. It is for tests only.
package printguard

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
)

// Verbs are the fmt verbs every form is printed with. They are applied through a format string built at run
// time, so vet does not refuse the verbs that do not fit.
var Verbs = []string{"v", "+v", "#v", "s", "q", "x", "X", "d", "T"}

// holder keeps v as an unexported field, the case where fmt and slog never call v's methods.
type holder[T any] struct {
	name string
	v    T
}

// exported keeps v as an exported field, the case where fmt calls v's methods.
type exported[T any] struct {
	Name string
	V    T
}

// Form is one printed, logged or encoded form of a value and how it was made.
type Form struct {
	How  string
	Text string
}

// Forms returns every form of v this package checks: v itself, a pointer to it, a struct holding it as an
// unexported and as an exported field, a pointer to that struct, a slice and a map holding it, each printed
// with every verb of [Verbs], logged with the slog text and JSON handlers and encoded as JSON.
func Forms[T any](t testing.TB, v T) []Form {
	t.Helper()
	h := holder[T]{name: "x", v: v}
	args := []struct {
		name string
		arg  any
	}{
		{"value", v},
		{"pointer", &v},
		{"struct", h},
		{"pointer to struct", &h},
		{"struct with exported field", exported[T]{Name: "x", V: v}},
		{"slice", []T{v}},
		{"slice of structs", []holder[T]{h}},
		{"map", map[string]T{"x": v}},
		{"map of structs", map[string]holder[T]{"x": h}},
	}
	var out []Form
	for _, a := range args {
		for _, verb := range Verbs {
			out = append(out, Form{How: "%" + verb + " of " + a.name, Text: fmt.Sprintf("%"+verb, a.arg)})
		}
		var text, js bytes.Buffer
		slog.New(slog.NewTextHandler(&text, nil)).Info("m", "k", a.arg)
		slog.New(slog.NewJSONHandler(&js, nil)).Info("m", "k", a.arg)
		out = append(out, Form{How: "slog text of " + a.name, Text: text.String()},
			Form{How: "slog JSON of " + a.name, Text: js.String()})
		if b, err := json.Marshal(a.arg); err == nil {
			out = append(out, Form{How: "JSON of " + a.name, Text: string(b)})
		}
	}
	return out
}

// Leaks returns the forms of v that show canary as text, in hexadecimal or as decimal bytes.
func Leaks[T any](t testing.TB, v T, canary string) []Form {
	t.Helper()
	if canary == "" {
		t.Fatal("printguard: empty canary")
	}
	needles := []string{canary, hex.EncodeToString([]byte(canary)), strings.ToUpper(hex.EncodeToString([]byte(canary))),
		decimalBytes(canary)}
	var out []Form
	for _, f := range Forms(t, v) {
		for _, n := range needles {
			if strings.Contains(f.Text, n) {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// Check fails t for each form of v that shows canary.
func Check[T any](t testing.TB, v T, canary string) {
	t.Helper()
	for _, f := range Leaks(t, v, canary) {
		t.Errorf("%s shows the secret: %s", f.How, f.Text)
	}
}

// decimalBytes returns the bytes of s as fmt prints a byte slice, without the brackets: "104 105" for "hi".
func decimalBytes(s string) string {
	parts := make([]string, len(s))
	for i := range len(s) {
		parts[i] = strconv.Itoa(int(s[i]))
	}
	return strings.Join(parts, " ")
}
