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
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/redact"
)

// A key or a value hidden by one rule that carries the signal of another rule, at every read tool (#845, ninth
// decision of 2026-11-18). A map key that is a connection string, a line break and "-authorization" was hidden as
// a piece before the answer was read, which took the word away, and get_resource showed the value under it that
// main hid. Every map of strings gets such a key with the secret as its value, every string the text with a
// separator and the secret after it, and every list of strings the text and the secret as the next item, in the
// forms of signalForms (the carriers, the signals, the glues and the line breaks taken by construction). The rows
// follow those of fieldLiteralResults in testdata/fields-main-entry.tsv and are written from main like them; the
// file uses only the read tools' API, so it runs unchanged in a checkout of main.

// signalFieldPerPlace is the least number of forms a place gets. A tool with few places gives each place more, so every form reaches every tool.
const signalFieldPerPlace = 3

// signalFieldResults calls every tool with a hidden text that carries a signal in every place of the types it
// reads, and returns the results and the number of calls per tool. With sample set, only the rows
// redact.RaceKeeps keeps are called under -race.
func signalFieldResults(t *testing.T, sample bool) ([]fieldResult, map[string]int) {
	t.Helper()
	forms := signalFieldForms()
	var out []fieldResult
	calls := map[string]int{}
	n := 0
	for _, ft := range fieldTools {
		e := newFieldEnv(t)
		var places []struct {
			typ reflect.Type
			p   fieldPlace
		}
		for _, typ := range ft.types {
			for _, p := range fieldPlaces(typ) {
				if p.kind == reflect.Map && p.elem.Kind() != reflect.String {
					continue
				}
				places = append(places, struct {
					typ reflect.Type
					p   fieldPlace
				}{typ, p})
			}
		}
		if len(places) == 0 {
			continue
		}
		per := max(signalFieldPerPlace, (len(forms)+len(places)-1)/len(places))
		next := 0
		for _, pl := range places {
			typ, p := pl.typ, pl.p
			for range per {
				f := forms[next%len(forms)]
				next++
				n++
				v := fmt.Sprintf("Vs%06dxK9w", n)
				form := "signal " + f.name
				if sample && !redact.RaceKeeps("fields signals", n) {
					out = append(out, fieldResult{tool: ft.name, place: p.name(typ), form: form, skipped: true})
					continue
				}
				calls[ft.name]++
				shown := ft.call(t, e, typ, v, func(obj reflect.Value) {
					switch p.kind {
					case reflect.Map:
						p.set(obj, fieldForm{key: func(string) string { return f.prefix }, value: func(v string) string { return v }}, v)
					case reflect.Slice:
						cur := p.at(obj)
						l := reflect.MakeSlice(cur.Type(), 2, 2)
						l.Index(0).SetString(f.prefix + strings.TrimRight(f.sep, " \r\n"))
						l.Index(1).SetString(v)
						cur.Set(l)
					default:
						p.setText(obj, f.prefix+f.sep+v)
					}
				})
				out = append(out, fieldResult{tool: ft.name, place: p.name(typ), form: form, hidden: !shown})
			}
		}
	}
	return out, calls
}

// signalFieldForms are the forms of the field rows: every carrier with every signal and line break, the glues
// and separators of signalForms taken in turn so that the rows stay some 20,000.
func signalFieldForms() []signalForm {
	all := signalForms()
	var out []signalForm
	for i, f := range all {
		// Keep the first glue of each form and every fourth form of the others.
		if strings.Contains(f.name, `glue "-"`) || i%4 == 0 {
			out = append(out, f)
		}
	}
	return out
}

// The separators and the names of signalWords that other tests of the package write too.
const (
	signalColon       = ": "
	signalQuotedColon = `": "`
	signalAPIKey      = "api_key"
	signalHeader      = "X-Api-Key"
)

// signalCarriers are texts a rule hides by themselves: a connection string, a key and its value, a bearer token.
var signalCarriers = []struct{ name, text string }{
	{"connection string", "postgres" + "://u:p@h/db"},
	{"key and value", "token=" + "Hq7wZp2Lx9"},
	{"bearer token", "Bearer " + "Hq7wZp2Lx9AbcD"},
}

// signalWords are the signals of other rules, each with the separators that join it to a value.
var signalWords = []struct {
	name, text string
	seps       []string
}{
	{"key word authorization", "authorization", []string{signalColon, "=", signalQuotedColon}},
	{"key word password", fieldsKey, []string{signalColon, "="}},
	{"key name api_key", signalAPIKey, []string{signalColon, "="}},
	{"flag --password", "--" + fieldsKey, []string{" ", "="}},
	{"flag --token", "--token", []string{" ", "="}},
	{"header name", signalHeader, []string{signalColon}},
}

// signalGlues join the carrier to the signal.
var signalGlues = []string{"-", "", " ", "_"}

// signalLineBreaks are the line breaks placed around the signal.
var signalLineBreaks = []string{"\n", "\r\n", "\r"}

// signalForm is one generated carrier with a signal: prefix is the text up to and including the signal (and the
// separator, sep, written apart for a map key and a list item), and the secret follows.
type signalForm struct {
	name, prefix, sep string
}

// signalForms returns every carrier with every signal, glue and line break, the line break before the signal
// (between the carrier and the signal), inside the carrier, and after the signal and its separator.
func signalForms() []signalForm {
	var out []signalForm
	for _, c := range signalCarriers {
		mid := len(c.text) / 2
		for _, s := range signalWords {
			for _, g := range signalGlues {
				for _, lb := range signalLineBreaks {
					for _, sep := range s.seps {
						name := fmt.Sprintf("%s, %s, glue %q, line break %q, separator %q", c.name, s.name, g, lb, sep)
						out = append(out,
							signalForm{name: name + ", line break before the signal", prefix: c.text + lb + g + s.text, sep: sep},
							signalForm{name: name + ", line break inside the carrier", prefix: c.text[:mid] + lb + c.text[mid:] + g + s.text, sep: sep},
							signalForm{name: name + ", line break after the signal", prefix: c.text + g + s.text + lb, sep: sep},
							signalForm{name: name + ", line break after the separator", prefix: c.text + g + s.text, sep: sep + lb},
						)
					}
				}
			}
		}
	}
	return out
}
