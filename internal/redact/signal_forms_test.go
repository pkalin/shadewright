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

import "fmt"

// The generated forms of a text hidden by one rule that carries the signal of another rule (#845, ninth decision
// of 2026-11-18). The file depends on nothing else in the package's tests, so that it compiles in a checkout of
// main next to readings_main_test.go, which writes what main did with them.

// The separators and the names of signalWords that other tests of the package write too.
const (
	signalColon       = ": "
	signalQuotedColon = `": "`
	signalAPIKey      = "api_key"
	signalHeader      = "X-Api-Key"
	signalPassword    = "password"
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
	{"key word password", signalPassword, []string{signalColon, "="}},
	{"key name api_key", signalAPIKey, []string{signalColon, "="}},
	{"flag --password", "--" + signalPassword, []string{" ", "="}},
	{"flag --token", "--token", []string{" ", "="}},
	{"header name", signalHeader, []string{signalColon}},
}

// signalGlues join the carrier to the signal.
var signalGlues = []string{"-", "", " ", "_"}

// signalLineBreaks are the line breaks placed around the signal.
var signalLineBreaks = []string{"\n", "\r\n", "\r"}

// signalForm is one generated carrier with a signal: prefix is the text up to and including the signal (and the
// separator, sep, written apart for a map key and a list item), and the secret follows. split is set when the
// line break stands between the signal and the secret (after the signal, or after its separator).
type signalForm struct {
	name, prefix, sep string
	split             bool
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
							signalForm{name: name + ", line break after the signal", prefix: c.text + g + s.text + lb, sep: sep, split: true},
							signalForm{name: name + ", line break after the separator", prefix: c.text + g + s.text, sep: sep + lb, split: true},
						)
					}
				}
			}
		}
	}
	return out
}
