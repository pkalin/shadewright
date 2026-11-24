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
	"strings"
	"testing"
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
// countWorkOfReleased) and the reading of the release before are each held to the bound on every shape, with no
// exception (#962 removed the two units excused before). Whether String withholds a shape is not checked here: the budget withholds a linear shape whose cost per byte is above WorkPerByte too (decision of
// 2026-11-21 on #873), and growth is what tells a superlinear shape.

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
// fourth shape of #873, on which each flag read its value to the end of the run (tokenEnd) and up to
// maxValueExtent bytes of the run again (plainWordEnd), so the work grew with the square of the text. This package
// reads the run once with the memories of #845 and #873 (tokenMemo, plainRun), and the reading of the release
// before does the same since #962 (released.TestFlagRunsAndTokenPairsWorkIsLinear).
const tokenEndRun = "--" + canonTokenKey + ","

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
// alone, which the windows are part of and the reading of the release before is not; the doubling across it on
// "args: [\"--password\", " + ":-" needs that step.
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
// linear bound, as the count of units before it and the work on either side, and false when none does. windowed
// says whether the work is read by windows (this package's own work), the only work the doubling across
// windowMinText may cost complexityWindowStep more on: the reading of the release before reads no window.
func doublingOver(work, size [4]int64, windowed bool) (units int, before, after int64, over bool) {
	for i := 1; i < len(work); i++ {
		bound := 2 * complexitySlack
		if windowed && (size[i-1] < windowMinText) != (size[i] < windowMinText) {
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
// whatever the reading of the release does on it. That reading is held to it too, on every shape (#962).
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
			for i := range own {
				in := open + strings.Repeat(unit, complexityN<<i)
				total, release := decisionWork(t, in)
				own[i], rel[i], size[i] = total-release, release, int64(len(in))
			}
			if n, before, after, over := doublingOver(own, size, true); over {
				fail("%q + %q x %d: own work %d, at twice as many units %d (own work %v)", open, unit, n, before,
					after, own)
			}
			if _, _, _, releaseOver := doublingOver(rel, size, false); releaseOver {
				fail("%q + %q: the release's reading is superlinear (release work %v)", open, unit, rel)
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
