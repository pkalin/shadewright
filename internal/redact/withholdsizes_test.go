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

// The units of the known expensive shapes that more than one test builds.
const (
	passwordFlagUnit = "-password,"
	gluedAuthUnit    = `auth="a"`
	gluedTokenUnit   = `token="a"`
)

// withholdShape is one of the known shapes whose work per byte passes WorkPerByte from some size on (decision of
// 2026-11-21 on #873): before, then unit repeated, then after.
type withholdShape struct {
	unit, before, after string
}

// text returns the shape with k units.
func (s withholdShape) text(k int) string {
	return s.before + strings.Repeat(s.unit, k) + s.after
}

// units returns the count of units of the shape of size bytes, and false when no count gives that size.
func (s withholdShape) units(size int) (int, bool) {
	n := size - len(s.before) - len(s.after)
	if n < 0 || n%len(s.unit) != 0 {
		return 0, false
	}
	return n / len(s.unit), true
}

// withholdShapes are the known expensive shapes, by the name withholdSizes gives them: the two flags of #962
// repeated, whose reading of the release before grows with the square of the text (growthExcused); TOKEN="a"
// repeated before a blank line of 4 KiB; and the glued quoted pairs (gluedQuotedPairs), with and without a tail.
var withholdShapes = map[string]withholdShape{
	"--token,}":        {unit: tokenEndRun + "}"},
	passwordFlagUnit:   {unit: passwordFlagUnit},
	"TOKEN-blank-line": {unit: `TOKEN="a"`, after: "\"\n" + strings.Repeat(" ", 4096) + "\"："},
	"glued-auth":       {unit: gluedAuthUnit},
	"glued-auth-tail":  {unit: gluedAuthUnit, after: "：x"},
	"glued-token":      {unit: gluedTokenUnit},
	"glued-token-tail": {unit: gluedTokenUnit, after: "：x"},
}

// withholdSizes are, for each known expensive shape and entry point, the largest size of the text in bytes that is
// decided inside its budget and the smallest that is withheld (one unit more), as TestBudgetWithholdSizes finds
// them. They are the only source of these figures: the pages (ADR-0015, design 10.3, troubleshooting, the metrics
// reference) and the comments of budget.go cite this test. Every shape at String, StringTwice (StringTwiceChecked)
// and Strings, whose lines are the strings of the sequence; "--token,}" at every entry point. One text, so that the
// names of budgetEntries stay literals there alone.
const withholdSizes = `
--token,}         String        6039   6048
--token,}         StringKeeping 6030   6039
--token,}         Clean         6039   6048
--token,}         StringTwice   6039   6048
--token,}         ReplaceFrom   6039   6048
--token,}         Replace       6039   6048
--token,}         ReleasedText  6300   6309
--token,}         Strings       2709   2718
--token,}         PiecesWithin  5625   5634
--token,}         PiecesFitting 5625   5634
-password,        String        9600   9610
-password,        StringTwice   9600   9610
-password,        Strings       4440   4450
TOKEN-blank-line  String        30562  30571
TOKEN-blank-line  StringTwice   25342  25351
TOKEN-blank-line  Strings       11572  11581
glued-auth        String        34808  34816
glued-auth        StringTwice   31448  31456
glued-auth        Strings       9752   9760
glued-auth-tail   String        29292  29300
glued-auth-tail   StringTwice   26908  26916
glued-auth-tail   Strings       9292   9300
glued-token       String        99711  99720
glued-token       StringTwice   72108  72117
glued-token       Strings       10647  10656
glued-token-tail  String        61672  61681
glued-token-tail  StringTwice   49954  49963
glued-token-tail  Strings       10003  10012
`

// withholdRow is one line of withholdSizes.
type withholdRow struct {
	shape, entry      string
	decided, withheld int
}

func withholdRows(t *testing.T) []withholdRow {
	t.Helper()
	var rows []withholdRow
	for line := range strings.Lines(withholdSizes) {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 4 {
			t.Fatalf("withholdSizes: %q is not shape, entry, decided, withheld", line)
		}
		decided, err1 := strconv.Atoi(f[2])
		withheld, err2 := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil {
			t.Fatalf("withholdSizes: %q: %v %v", line, err1, err2)
		}
		rows = append(rows, withholdRow{f[0], f[1], decided, withheld})
	}
	return rows
}

// withholdBoundary finds the count of units of s at which e stops deciding: the largest count decided, whose next
// is withheld. It starts at the count from: when from is decided and from+1 is withheld, that is the boundary;
// otherwise it searches from one unit up, doubling, then halving the gap.
func withholdBoundary(e budgetEntry, s withholdShape, from int) int {
	decided := func(k int) bool {
		in := s.text(k)
		_, ok, _ := meteredSpent(e, in, budgetOf(e.size(in)))
		return ok
	}
	if from > 0 && decided(from) && !decided(from+1) {
		return from
	}
	lo, hi := 0, 1
	for decided(hi) {
		lo, hi = hi, 2*hi
	}
	for hi-lo > 1 {
		if m := (lo + hi) / 2; decided(m) {
			lo = m
		} else {
			hi = m
		}
	}
	return lo
}

// The sizes from which each known expensive shape is withheld, at each entry point, are those of withholdSizes:
// the test finds the boundary itself (withholdBoundary) and compares it with the table, and checks that the public
// entry point returns the entry withheld whole at the first size withheld. A change to the budget, to the rules or
// to the reading of the release before that moves a boundary fails here, and the pages that cite this test must
// change with it.
func TestBudgetWithholdSizes(t *testing.T) {
	if raceDetector {
		t.Skip("it spends the whole budget of texts of up to 100 KiB, 10 to 20 times slower under -race; make test " +
			"runs this package again without it")
	}
	entries := map[string]budgetEntry{}
	for _, e := range budgetEntries {
		entries[e.name] = e
	}
	for _, row := range withholdRows(t) {
		t.Run(row.shape+"/"+row.entry, func(t *testing.T) {
			t.Parallel()
			s, ok := withholdShapes[row.shape]
			e, eok := entries[row.entry]
			if !ok || !eok {
				t.Fatalf("withholdSizes names %s at %s: no such shape or entry point", row.shape, row.entry)
			}
			from, exact := s.units(row.decided)
			if !exact {
				from = 0
			}
			k := withholdBoundary(e, s, from)
			decided, withheld := len(s.text(k)), len(s.text(k+1))
			if decided != row.decided || withheld != row.withheld {
				t.Errorf("%s on %s: decided up to %d bytes and withheld from %d, the table says %d and %d", e.name,
					row.shape, decided, withheld, row.decided, row.withheld)
			}
			if got := e.public(s.text(k + 1)); !withheldOut(got) {
				t.Errorf("%s on %s of %d bytes returned %q, not withheld", e.name, row.shape, withheld,
					redacttest.Clip(got))
			}
		})
	}
}
