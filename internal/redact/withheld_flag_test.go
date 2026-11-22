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

	"github.com/pkalin/shadewright/internal/redact/redacttest"
)

// flagged is an entry point that says with a flag whether it withheld its entry, its output joined. run is the
// package's function, on the package's redactor; on is the same entry point as a method of the redactor given.
// parts is the number of strings it returns for in, withheld or not: one for a text, one per line for Strings, one
// per piece for the pieces.
type flagged struct {
	name  string
	run   func(in string) (out []string, ok bool)
	on    func(r *Redactor, in string) (out []string, ok bool)
	parts func(in string) int
}

// onePart is parts of an entry point that returns one text.
func onePart(string) int { return 1 }

// oneText is the output of an entry point that returns one text, as flagged returns it.
func oneText(out string, ok bool) ([]string, bool) { return []string{out}, ok }

// flagPieces and fitPieces are the answers PiecesWithin and PiecesFitting are run on: in as a value with text
// around it.
func flagPieces(in string) []Piece {
	return []Piece{{Text: "v: "}, {Text: in, Data: true, Form: Line, Key: "v"}}
}
func fitPieces(in string) []Piece {
	return []Piece{{Text: in, Data: true, Form: Line, Key: "v"}, {Text: "\n"}}
}

// keepAll is the check of PiecesFitting in these tests: every answer fits.
func keepAll([]string) bool { return true }

// identity is the between function of Replace in these tests.
func identity(s string) string { return s }

var flaggedEntries = []flagged{
	{"StringChecked", func(in string) ([]string, bool) { return oneText(StringChecked(in)) },
		func(r *Redactor, in string) ([]string, bool) { return oneText(r.StringChecked(in)) }, onePart},
	{"StringKeepingChecked", func(in string) ([]string, bool) { return oneText(StringKeepingChecked(in, "api")) },
		func(r *Redactor, in string) ([]string, bool) { return oneText(r.StringKeepingChecked(in, "api")) }, onePart},
	{"StringTwiceChecked", func(in string) ([]string, bool) { return oneText(StringTwiceChecked(in, nil)) },
		func(r *Redactor, in string) ([]string, bool) { return oneText(r.StringTwiceChecked(in, nil)) }, onePart},
	{"ReplaceChecked", func(in string) ([]string, bool) { return oneText(ReplaceChecked(in, budgetRepl, identity)) },
		func(r *Redactor, in string) ([]string, bool) {
			return oneText(r.ReplaceChecked(in, budgetRepl, identity))
		},
		onePart},
	{"(*Redactor).ReplaceFrom", func(in string) ([]string, bool) {
		return oneText(defaultRedactor.ReplaceFrom(in, budgetFrom(in), nil, budgetRepl, identity))
	}, func(r *Redactor, in string) ([]string, bool) {
		return oneText(r.ReplaceFrom(in, budgetFrom(in), nil, budgetRepl, identity))
	}, onePart},
	{"StringsChecked", func(in string) ([]string, bool) { return StringsChecked(strings.Split(in, "\n")) },
		func(r *Redactor, in string) ([]string, bool) { return r.StringsChecked(strings.Split(in, "\n")) },
		func(in string) int { return strings.Count(in, "\n") + 1 }},
	{"PiecesWithin", func(in string) ([]string, bool) { return defaultRedactor.PiecesWithin(flagPieces(in), -1) },
		func(r *Redactor, in string) ([]string, bool) { return r.PiecesWithin(flagPieces(in), -1) },
		func(string) int { return 2 }},
	{"PiecesFitting", func(in string) ([]string, bool) { return defaultRedactor.PiecesFitting(fitPieces(in), keepAll) },
		func(r *Redactor, in string) ([]string, bool) { return r.PiecesFitting(fitPieces(in), keepAll) },
		func(string) int { return 2 }},
}

// Withheld is told by a flag, never by the text (#873, decision of 2026-11-20): an entry that spells the marker,
// alone or as the first value of an answer with nothing after it, is decided (ok true) and shows the marker as
// text; an entry withheld (ok false) is Withheld with nothing else, whether withheld for its size (one byte longer
// than MaxEntry, on the package's redactor) or for its work (OverLowBudget on a redactor of redacttest.LowBudget:
// no shape of the corpora is known to pass the package's work budget since #962).
func TestWithheldIsAFlagNotAText(t *testing.T) {
	parallelUnderRace(t)
	low := redacttest.LowBudget(defaultRedactor)
	overs := []struct {
		reason string
		in     string
		run    func(e flagged, in string) ([]string, bool)
	}{
		{ReasonSize, strings.Repeat("x", MaxEntry+1), func(e flagged, in string) ([]string, bool) { return e.run(in) }},
		{ReasonWork, redacttest.OverLowBudget, func(e flagged, in string) ([]string, bool) { return e.on(low, in) }},
	}
	for _, e := range flaggedEntries {
		for _, in := range []string{Withheld, Withheld + "\n"} {
			for name, run := range map[string]func(string) ([]string, bool){
				"package": e.run, "low budget": func(in string) ([]string, bool) { return e.on(low, in) },
			} {
				out, ok := run(in)
				if !ok || len(out) != e.parts(in) || !strings.Contains(strings.Join(out, ""), Withheld) {
					t.Errorf("%s(%q), %s = %q, ok %v; want the marker as text in %d strings, ok", e.name, in, name, out,
						ok, e.parts(in))
				}
			}
		}
		for _, over := range overs {
			out, ok := over.run(e, over.in)
			// The marker in the first string and nothing in the others, written here, not taken from the package.
			want := make([]string, e.parts(over.in))
			want[0] = Withheld
			if ok || !slices.Equal(out, want) {
				t.Errorf("%s on %d bytes withheld for its %s: ok %v, %d strings %.80q; want ok false and %q", e.name,
					len(over.in), over.reason, ok, len(out), out, want)
			}
		}
	}
}
