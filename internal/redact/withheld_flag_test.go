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

package redact

import (
	"slices"
	"strings"
	"testing"
)

// flagged is an entry point that says with a flag whether it withheld its entry, its output joined.
type flagged struct {
	name string
	run  func(in string) (out []string, ok bool)
}

var flaggedEntries = []flagged{
	{"StringChecked", func(in string) ([]string, bool) { out, ok := StringChecked(in); return []string{out}, ok }},
	{"StringTwiceChecked", func(in string) ([]string, bool) {
		out, ok := StringTwiceChecked(in, nil)
		return []string{out}, ok
	}},
	{"ReplaceChecked", func(in string) ([]string, bool) {
		out, ok := ReplaceChecked(in, budgetRepl, func(s string) string { return s })
		return []string{out}, ok
	}},
	{"(*Redactor).ReplaceFrom", func(in string) ([]string, bool) {
		out, ok := defaultRedactor.ReplaceFrom(in, budgetFrom(in), nil, budgetRepl, func(s string) string { return s })
		return []string{out}, ok
	}},
	{"StringsChecked", func(in string) ([]string, bool) { return StringsChecked(strings.Split(in, "\n")) }},
	{"PiecesWithin", func(in string) ([]string, bool) {
		return defaultRedactor.PiecesWithin([]Piece{{Text: "v: "}, {Text: in, Data: true, Form: Line, Key: "v"}}, -1)
	}},
	{"PiecesFitting", func(in string) ([]string, bool) {
		return defaultRedactor.PiecesFitting([]Piece{{Text: in, Data: true, Form: Line, Key: "v"}, {Text: "\n"}},
			func([]string) bool { return true })
	}},
}

// Withheld is told by a flag, never by the text (#873, decision of 2026-11-20): an entry that spells the marker,
// alone or as the first value of an answer with nothing after it, is decided (ok true) and shows the marker as
// text; an entry over the work budget is withheld (ok false) and is Withheld with nothing else.
func TestWithheldIsAFlagNotAText(t *testing.T) {
	over := strings.Repeat("--token,}", 4096)
	for _, e := range flaggedEntries {
		for _, in := range []string{Withheld, Withheld + "\n"} {
			out, ok := e.run(in)
			if !ok || !strings.Contains(strings.Join(out, ""), Withheld) {
				t.Errorf("%s(%q) = %q, ok %v; want the marker as text, ok", e.name, in, out, ok)
			}
		}
		out, ok := e.run(over)
		if ok || !slices.Equal(out, withheldStrings(len(out))) {
			t.Errorf("%s on %d bytes over the budget: ok %v, %d strings, first %q", e.name, len(over), ok, len(out),
				out[0][:min(len(out[0]), 40)])
		}
	}
}
