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
// repeated n, 2n, 4n and 8n times; the work may at most double when the input doubles, with complexitySlack for
// the constant part.
//
// The work is that of the decision without a budget (render on spans with a nil meter, what String returns
// inside its budget): the budget (budget.go) stops an entry at a work linear in its size, so the work of String
// itself grows linearly on any shape and could not show one that is not. It is split in two (decision of
// 2026-11-20 on #873): the work of this package (the total less the reading of the release before,
// countWorkOfReleased) is held to the bound on every shape, with no exception; the reading of the release before
// is held to it too, except on the units named in complexityKnownSuperlinear. That a superlinear shape costs an
// entry its text shows in the check that String withholds no other shape.

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
// (released.tokenEnd) and its work grows with the square of the text. That reading is the release's code (with the
// linearity change of #873 that left its output unchanged, TestReleasedIsTheReleaseBefore and kept_test.go), and
// not fixed for this shape (#962): the budget withholds such an entry instead. The work
// of this package on it is linear: the plain run of plainWordEnd reads the run of flags once, where each flag
// read up to maxValueExtent bytes of it before (377,785, 1,326,458, 3,571,606 and 8,061,008 steps for 2,304 to
// 18,432 bytes of "--token,}").
const tokenEndRun = "--" + canonTokenKey + ","

// complexityKnownSuperlinear are the units on which the reading of the release before (and only it) is known to
// cost more than twice as much at twice the size, each with the reason. TestTokenShapesWorkIsLinear checks that it
// still does (so an entry here cannot hide a shape that became linear) and that String withholds it at the
// largest size; the work of this package on them is held to the linear bound as on every other shape.
var complexityKnownSuperlinear = map[string]string{
	tokenEndRun:       "released.tokenEnd reads each value to the end of the run (#873, the fourth shape; #962)",
	tokenEndRun + "}": "released.tokenEnd reads each value to the end of the run (#873, the fourth shape; #962)",
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
// (windows.go), whose fixed cost per byte is higher, a one-time step. It is measured on the work of this package
// alone, which the windows are part of and the reading of the release before is not: the largest is 3.61 times
// the work for "args: [\"--password\", " + ":-" from 2,070 to 4,118 bytes, where the doublings on either side
// cost at most 2.2 times.
const (
	complexityN          = 256
	complexitySlack      = 1.1
	complexityWindowStep = 1.7
)

// decisionWork returns the work of the decision String takes on in, with no budget to stop it, and the part of it
// the reading of the release before (package released) took.
func decisionWork(t *testing.T, in string) (total, release int64) {
	return countWorkOfReleased(t, func() { render(nil, in, defaultRedactor.spans(nil, in)) })
}

// doublingOver returns the first doubling of work, measured at the sizes of size, that costs more than the
// linear bound, as the count of units before it and the work on either side, and false when none does.
func doublingOver(work, size [4]int64) (units int, before, after int64, over bool) {
	for i := 1; i < len(work); i++ {
		bound := 2 * complexitySlack
		if (size[i-1] < windowMinText) != (size[i] < windowMinText) {
			bound *= complexityWindowStep
		}
		if float64(work[i]) > bound*float64(work[i-1]) {
			return complexityN << (i - 1), work[i-1], work[i], true
		}
	}
	return 0, 0, 0, false
}

// The work of this package (the total less the reading of the release before) is held to the linear bound on
// every shape, with no shape excused: a change to the kept code that makes a shape superlinear fails here
// whatever the reading of the release does on it. Only that reading may be excused, by the name of its unit in
// complexityKnownSuperlinear.
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
			var own, rel, size [4]int64
			var in string
			for i := range own {
				in = open + strings.Repeat(unit, complexityN<<i)
				total, release := decisionWork(t, in)
				own[i], rel[i], size[i] = total-release, release, int64(len(in))
			}
			if n, before, after, over := doublingOver(own, size); over {
				fail("%q + %q x %d: own work %d, at twice as many units %d (own work %v)", open, unit, n, before,
					after, own)
			}
			_, _, _, releaseOver := doublingOver(rel, size)
			_, decided := StringChecked(in)
			withheld := !decided
			if reason, known := complexityKnownSuperlinear[unit]; known {
				if !releaseOver || !withheld {
					fail("%q + %q, known superlinear in the release's reading (%s): superlinear %v (release work %v), "+
						"withheld at %d bytes %v", open, unit, reason, releaseOver, rel, len(in), withheld)
				}
				continue
			}
			if releaseOver {
				fail("%q + %q: the release's reading is superlinear (release work %v)", open, unit, rel)
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
		total, release := decisionWork(t, in)
		t.Logf("%q and %d closing braces: %d steps, %d of them the release's", open, n, total, release)
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
	for field := range strings.FieldsSeq(`String=4608 StringKeeping=4608 Clean=4608 StringTwice=4608
		ReplaceFrom=4608 Replace=4608 ReleasedText=4608 Strings=2304 PiecesWithin=4608 PiecesFitting=4608`) {
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
