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
	"strconv"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/redact/redacttest"
)

// Cost is linear on inputs built from the tokens of the rules themselves (#845, third decision of 2026-11-18,
// property 3). A loop that counts its work can still be quadratic: "{password: marigold" followed by n closing
// braces read the rest of the line again at each brace (152,137, 565,577 and 2,178,889 steps for n = 512, 1024
// and 2048 on 38cf1a37). Each shape is an opening (nothing, or a key in one of its forms) followed by a unit of
// one or two tokens (keys, separators, brackets, quotes, block markers, line ends, invisible characters)
// repeated n, 2n, 4n and 8n times; the work of the decision of String may at most double when the input doubles,
// with complexitySlack for the constant part.
//
// The work is that of the decision without a budget (render on spans with a nil meter, what String returns
// inside its budget): the budget (budget.go) stops an entry at a work linear in its size, so the work of String
// itself grows linearly on any shape and could not show one that is not. That a superlinear shape costs an
// entry its text shows instead in the check that String withholds no shape (#873).

// complexityOpenings are what a shape starts with.
var complexityOpenings = []string{
	"", "{" + passwordKey + ": marigold", passwordKey + ": |\n", "[--" + canonTokenKey + " ", "\"" + passwordKey + "\": \"",
	passwordKey + "=", "{\"a\": \"",
	"? password\n", "- name: DB_PASSWORD\n", "args: [\"--password\", ",
}

// complexityTokens are the tokens a unit is made of; complexityCore are those paired with each other.
var (
	complexityTokens = []string{
		"}", "]", "{", "[", "\"", "'", ":", "=", "|", ">", "-", "#", "\n", "\r", " ", "\t", ",", "?", "\\",
		passwordKey + ": ", "--" + canonTokenKey + " ", passwordKey, passKey + ": |", "：", "\u200b", "->", "x", "\"password\": ",
		tokenEndRun, tokenEndRun + "}",
	}
	complexityCore = []string{"}", "]", "{", "[", "\"", ":", "|", "\n", " ", passwordKey + ": ", "--" + canonTokenKey + " ", "-"}
)

// tokenEndRun is a flag followed by a comma with no value: repeated, alone or with a closing brace, it is the
// fourth shape of #873, on which the reading of the release before reads each value to the end of the run
// (released.tokenEnd) and its work grows with the square of the text. That reading is the release's, kept byte
// for byte (TestReleasedIsTheReleaseBefore), and not fixed: the budget withholds such an entry instead.
const tokenEndRun = "--" + canonTokenKey + ","

// complexityKnownSuperlinear are the units whose shapes, after every opening, are known to cost more than
// twice as much at twice the size, each with the reason. TestTokenShapesWorkIsLinear checks that each of them
// still is (so an entry here cannot hide a shape that became linear, or one that grows for another reason
// unseen) and that String withholds it at the largest size.
var complexityKnownSuperlinear = map[string]string{
	tokenEndRun:       "released.tokenEnd reads each value to the end of the run (#873, the fourth shape)",
	tokenEndRun + "}": "released.tokenEnd reads each value to the end of the run (#873, the fourth shape)",
}

// complexityUnits returns every unit: each token, and each pair of core tokens.
func complexityUnits() []string {
	units := append([]string(nil), complexityTokens...)
	for _, a := range complexityCore {
		for _, b := range complexityCore {
			units = append(units, a+b)
		}
	}
	return units
}

// complexityN is the smallest count of units; complexitySlack is how much more than twice the work a doubling
// may cost, for the part of the work that does not grow with n. complexityWindowStep is how much more again a
// doubling across windowMinText may cost: texts shorter than it are read whole and longer ones by windows
// (windows.go), whose fixed cost per byte is higher, a one-time step (the largest measured is 2.92 times the
// work for "{\"a\": \"" + ":-" from 2,055 to 4,103 bytes; the doublings on either side cost at most 2.2 times).
const (
	complexityN          = 256
	complexitySlack      = 1.1
	complexityWindowStep = 1.4
)

// decisionWork returns the work of the decision String takes on in, with no budget to stop it.
func decisionWork(t *testing.T, in string) int64 {
	return countWork(t, func() { render(nil, in, defaultRedactor.spans(nil, in)) })
}

func TestTokenShapesWorkIsLinear(t *testing.T) {
	shapes, failed := 0, 0
	fail := func(format string, args ...any) {
		if failed++; failed <= 10 {
			t.Errorf(format, args...)
		}
	}
	for _, open := range complexityOpenings {
		for _, unit := range complexityUnits() {
			shapes++
			var work, size [4]int
			var in string
			for i := range work {
				in = open + strings.Repeat(unit, complexityN<<i)
				work[i], size[i] = int(decisionWork(t, in)), len(in)
			}
			superlinear := false
			for i := 1; i < len(work); i++ {
				bound := 2 * complexitySlack
				if (size[i-1] < windowMinText) != (size[i] < windowMinText) {
					bound *= complexityWindowStep
				}
				if float64(work[i]) > bound*float64(work[i-1]) {
					superlinear = true
					if _, known := complexityKnownSuperlinear[unit]; !known {
						fail("%q + %q x %d: work %d, at twice as many units %d", open, unit, complexityN<<(i-1),
							work[i-1], work[i])
						break
					}
				}
			}
			withheld := IsWithheld(String(in))
			if reason, known := complexityKnownSuperlinear[unit]; known {
				if !superlinear || !withheld {
					fail("%q + %q, known superlinear (%s): superlinear %v (work %v), withheld at %d bytes %v",
						open, unit, reason, superlinear, work, len(in), withheld)
				}
				continue
			}
			if withheld {
				fail("%q + %q x %d: String withheld %d bytes", open, unit, complexityN<<3, len(in))
			}
		}
	}
	if failed > 0 {
		t.Errorf("%d checks failed on %d shapes", failed, shapes)
	}
	t.Logf("%d shapes at %d, %d, %d and %d units", shapes, complexityN, 2*complexityN, 4*complexityN, 8*complexityN)
	for _, n := range []int{512, 1024, 2048} {
		open := "{" + passwordKey + ": marigold"
		in := open + strings.Repeat("}", n)
		t.Logf("%q and %d closing braces: %d steps", open, n, decisionWork(t, in))
	}
}

// The fourth shape of #873 ("--token,}" repeated, complexityKnownSuperlinear) is withheld from a size that
// depends on the entry point, as each reads the text a number of times of its own (the ADR-0015 amendment of
// 2026-11-20 states them): at each, the largest size of the doublings from 1,152 bytes that is still decided,
// and the next, withheld. The inputs are one line, so Strings and PiecesWithin have one string or data piece.
func TestTokenEndRunWithheldFrom(t *testing.T) {
	// The largest size decided, by entry point, as one text so that the names of budgetEntries stay literals
	// there alone.
	decided := map[string]int{}
	for field := range strings.FieldsSeq(`String=2304 StringKeeping=2304 Clean=2304 StringTwice=2304
		ReplaceFrom=2304 ReleasedText=4608 Strings=1152 PiecesWithin=2304`) {
		name, size, _ := strings.Cut(field, "=")
		n, err := strconv.Atoi(size)
		if err != nil {
			t.Fatal(err)
		}
		decided[name] = n
	}
	for _, e := range budgetEntries {
		want, ok := decided[e.name]
		if !ok {
			t.Errorf("%s: no size stated", e.name)
			continue
		}
		for _, n := range []int{want, 2 * want} {
			in := strings.Repeat(tokenEndRun+"}", n/len(tokenEndRun+"}"))
			_, decidedIn, _ := meteredSpent(e, in, budgetOf(e.size(in)))
			if decidedIn != (n <= want) {
				t.Errorf("%s on \"--token,}\" repeated to %d bytes: decided %v, want %v", e.name, len(in), decidedIn,
					n <= want)
			}
			if got := e.public(in); n > want && !withheldOut(got) {
				t.Errorf("%s on \"--token,}\" repeated to %d bytes returned %q, not withheld", e.name, len(in),
					redacttest.Clip(got))
			}
		}
	}
}
