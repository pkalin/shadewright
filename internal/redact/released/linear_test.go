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

package released

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/redact/meter"
)

// #962: the reading was not linear on three shapes. "--token,}" and "-password," repeated read each flag's value
// to the end of the run (tokenEnd), and "--token,}" read up to maxValueExtent bytes of the run again for each flag
// (plainWordEnd), so their work grew with the square of the text, 3.5 to 4 times per doubling. TOKEN="a" repeated
// before a quote, a line break and a long blank line read the shell word of every key up to maxValueExtent bytes,
// although the string the key sits in closes at its value and the extent is cut back to it (keyValueExtent), so its
// work per byte grew with the run up to that bound. Each shape is built with its repeated part doubling from
// 2 to 32 KiB; the work of the reading may at most double, with linearSlack for the part that does not grow.

// linearSlack is how much more than twice the work a doubling may cost.
const linearSlack = 1.1

// The units of the two flag shapes of #962.
const (
	tokenFlagUnit    = "--token,}"
	passwordFlagUnit = "-password,"
)

// linearShapes are the three shapes of #962, each a unit repeated and a fixed tail.
var linearShapes = []struct {
	name, unit, tail string
}{
	{"--token,} repeated", tokenFlagUnit, ""},
	{"-password, repeated", passwordFlagUnit, ""},
	{`TOKEN="a" before a blank line`, `TOKEN="a"`, "\"\n" + strings.Repeat(" ", 4096) + "\"："},
}

// workOf returns the steps of the reading on s, with no budget to stop it.
func workOf(s string) int64 {
	wm := meter.New(math.MaxInt64)
	defaultRedactor.SpansWithin(wm, s)
	return wm.Spent()
}

func TestFlagRunsAndTokenPairsWorkIsLinear(t *testing.T) {
	for _, sh := range linearShapes {
		var prev int64
		for i, size := range []int{2 << 10, 4 << 10, 8 << 10, 16 << 10, 32 << 10} {
			s := strings.Repeat(sh.unit, size/len(sh.unit)) + sh.tail
			w := workOf(s)
			t.Logf("%s, %d bytes: %d steps, %.0f per byte", sh.name, len(s), w, float64(w)/float64(len(s)))
			if i > 0 && float64(w) > 2*linearSlack*float64(prev) {
				t.Errorf("%s: %d steps at %d bytes of units, %d at half as many: %.2f times for twice the run",
					sh.name, w, size, prev, float64(w)/float64(prev))
			}
			prev = w
		}
	}
}

// Parts of the generated runs of flags and glued key pairs: flags and key names with their separators, values,
// the bytes that end a value or open a group, and the tails that close or leave open a quote after a blank line.
var (
	runUnits = []string{
		"--token,", tokenFlagUnit, passwordFlagUnit, "--password", "-token", "--client-secret,", "TOKEN=\"a\"",
		"TOKEN='a'", "password=\"a\"", "auth=\"a\"", "TOKEN=\"a\",b,", "TOKEN=a", "--token=", "-password=\"a\"",
	}
	runGlue = []string{
		"", "", "", ",", "}", "]", " ", "\t", "\n", "\"", "'", "\\", "\\\"", "\\ ", "$(", ")", "`", "=", ":", "a",
		"：", " ", ";", "[", "{",
	}
	runTails = []string{
		"", "\"", "\"\n" + strings.Repeat(" ", 300) + "\"：", "\"\n" + strings.Repeat(" ", 5000) + "\"：",
		"'\n  x'", " --region x", "\n", "\\\"", "`", "$(echo a",
	}
)

// runText returns a generated run of n units, each followed now and then by a glue piece, and a tail.
func runText(r *rand.Rand, n int) string {
	var b strings.Builder
	unit := pick(r, runUnits)
	for range n {
		if r.IntN(8) == 0 {
			unit = pick(r, runUnits)
		}
		b.WriteString(unit)
		if r.IntN(4) == 0 {
			b.WriteString(pick(r, runGlue))
		}
	}
	b.WriteString(pick(r, runTails))
	return b.String()
}

// The reading finds the spans of the release before on generated runs of flags and glued key pairs, short ones and
// ones past maxValueExtent, where the memories of #962 (tokenMemo, plainRun) and the cut of an extent at the
// string the key sits in (keyValueExtent) take effect.
func TestFlagRunsMatchKeptCopy(t *testing.T) {
	r := rand.New(rand.NewPCG(962, 1)) // #nosec G404 -- reproducible generated texts
	inputs := make([]string, 0, 3100)
	for range sampled(3000) {
		inputs = append(inputs, runText(r, 1+r.IntN(40)))
	}
	for range sampled(40) {
		inputs = append(inputs, runText(r, 300+r.IntN(600)))
	}
	for _, sh := range linearShapes {
		for _, n := range []int{100, 455, 456, 500, 1000} {
			inputs = append(inputs, strings.Repeat(sh.unit, n)+sh.tail)
		}
	}
	checkSameAsKept(t, "flag runs", inputs)
}

// The plain run plainWordEnd keeps for one pass over a text (#962) does not change the end it returns: on generated
// texts, reads at any start, limit, key and width, in any order, find the end found without it. A "$" just before a
// read's limit (plain to that read, a group to one with a larger limit) is among them.
func TestPlainWordEndMemoMatches(t *testing.T) {
	bytes := []string{"a", "-", ",", "}", "$", "$(", ")", "(", "`", "\\", "\\ ", " ", "\"", "'", "\n", ";", "--token,"}
	r := rand.New(rand.NewPCG(962, 2)) // #nosec G404 -- reproducible generated texts
	calls, jumped := 0, 0
	for range sampled(20000) {
		var b strings.Builder
		for range 1 + r.IntN(40) {
			b.WriteString(pick(r, bytes))
		}
		s := b.String()
		pr := &plainRun{}
		extent := 1 + r.IntN(len(s))
		pos := 0
		for range 30 {
			if r.IntN(3) == 0 {
				pos = r.IntN(len(s))
			} else {
				pos = min(len(s)-1, pos+r.IntN(4))
			}
			limit := pos + 1 + r.IntN(len(s)-pos)
			if r.IntN(2) == 0 {
				limit = min(len(s), pos+extent)
			}
			keyPos := noKey
			if r.IntN(2) == 0 {
				keyPos = r.IntN(pos + 1)
			}
			wide := r.IntN(3) > 0
			want := plainWordEnd(nil, s, pos, limit, keyPos, wide, nil)
			if pr.from <= pos && pos < pr.to {
				jumped++
			}
			calls++
			if got := plainWordEnd(nil, s, pos, limit, keyPos, wide, pr); got != want {
				t.Fatalf("plainWordEnd(%q, %d, %d, %d, %v) with the plain run = %d, without = %d", s, pos, limit,
					keyPos, wide, got, want)
			}
		}
	}
	if jumped < calls/10 {
		t.Fatalf("%d of %d reads started inside the plain run: the generator no longer reaches it", jumped, calls)
	}
}

// The memory of the last value end (tokenMemo, #962) changes no result: on generated texts, every tokenEnd through
// one memory, at positions in any order, gives what tokenEnd gives alone.
func TestTokenMemoMatches(t *testing.T) {
	pieces := []string{"\\\\\"", "\\\\\\\"", "\\\\x", "\\'", "\\", passwordFlagUnit, tokenFlagUnit, "a", " ", "\t", "\n",
		"\"", "'", ",", "}"}
	r := rand.New(rand.NewPCG(962, 3)) // #nosec G404 -- reproducible generated texts
	calls, hits := 0, 0
	for range sampled(20000) {
		var b strings.Builder
		for range 1 + r.IntN(30) {
			b.WriteString(pick(r, pieces))
		}
		s := b.String()
		tm := &tokenMemo{}
		pos := 0
		for range 20 {
			if r.IntN(3) == 0 {
				pos = r.IntN(len(s) + 1)
			} else {
				pos = min(len(s), pos+r.IntN(4))
			}
			if tm.ok && tm.from <= pos && pos <= tm.end {
				hits++
			}
			calls++
			if got, want := tm.tokenEnd(nil, s, pos), tokenEnd(nil, s, pos); got != want {
				t.Fatalf("tokenEnd(%q, %d) through the memory = %d, alone %d", s, pos, got, want)
			}
		}
	}
	if hits < calls/4 {
		t.Fatalf("only %d of %d reads answered from the memory", hits, calls)
	}
}

// keyValueExtent skips the extent when the string the key sits in closes at the value's start, as the extent
// would be cut back to that start whatever it is. That holds because extendedValueSpan never returns a start or
// an end before the position it reads from: checked on generated texts at every position, with each boundary,
// width and key position.
func TestExtendedValueSpanEndsAtOrAfterItsStart(t *testing.T) {
	r := rand.New(rand.NewPCG(962, 4)) // #nosec G404 -- reproducible generated texts
	pieces := append(append([]string{"/*a*/", "// a\n", "[a]", "{a: b}", "\\\"a\\\"", "\\\\\\\"a\\\\\\\"", "  x"},
		runUnits...), runGlue...)
	boundaries := []int{boundaryWhitespace, boundaryFlow, boundaryFlowWord, boundaryLine}
	checked := 0
	for range sampled(3000) {
		var b strings.Builder
		for range 1 + r.IntN(20) {
			b.WriteString(pick(r, pieces))
		}
		s := b.String()
		lines := newLineIndex(nil, s)
		for pos := range len(s) + 1 {
			keyPos := r.IntN(pos + 1)
			if keyPos == len(s) {
				keyPos = max(0, len(s)-1)
			}
			bd := boundaries[r.IntN(len(boundaries))]
			xs, xe := extendedValueSpan(nil, s, pos, bd, r.IntN(4), r.IntN(3), keyPos, r.IntN(2) == 0, lines)
			checked++
			if xs < pos || xe < pos {
				t.Fatalf("extendedValueSpan(%q, %d, boundary %d) = %d, %d: before its start", s, pos, bd, xs, xe)
			}
		}
	}
	t.Logf("%d positions", checked)
}
