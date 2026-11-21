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
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/redact/meter"
	"github.com/pkalin/shadewright/internal/redact/redacttest"
)

// The work budget of an entry (#873, decision of 2026-11-20; budget.go). The meter counts the steps of one
// redaction in every build, so these tests need no build tag: what an entry spent is what its meter counted.
//
//	B1: every entry of the corpus tables, at every entry point, fits its budget with headroom (budgetHeadroom):
//	    the budget withholds nothing the rules decide today, so nothing they hide or show changes.
//	B2: the work stays inside the budget for every input: an entry stops within one counting step of it.
//	B3: an entry is either decided whole, as without a budget, or withheld whole (Withheld, nothing of it),
//	    wherever its budget runs out; and an entry longer than MaxEntry is withheld before any work.

// budgetHeadroom is how many times its work the budget of every entry of the corpus tables must be at least: the
// margin of a half WorkPerByte is set with (decision of 2026-11-21 on #873).
const budgetHeadroom = 1.5

// budgetEntry is one entry point as the budget sees it: run decides on in with the meter wm (nil: none) and
// returns the output joined, size is the size of the entry its budget is set from, and public is the exported
// entry point.
type budgetEntry struct {
	name   string
	size   func(in string) int
	run    func(wm *meter.Meter, in string) string
	public func(in string) string
}

// budgetLines are the lines of in, the strings of a sequence.
func budgetLines(in string) []string { return strings.Split(in, "\n") }

// budgetPieces writes the lines of in as the data pieces of an answer, each under a key and on its own line, as
// the read tools write the values of an object.
func budgetPieces(in string) []Piece {
	lines := budgetLines(in)
	ps := make([]Piece, 0, 3*len(lines))
	for i, l := range lines {
		ps = append(ps, Piece{Text: fmt.Sprintf("k%d: ", i)}, Piece{Text: l, Data: true, Form: Line, Key: "k"},
			Piece{Text: "\n"})
	}
	return ps
}

func piecesSize(ps []Piece) int {
	n := 0
	for _, p := range ps {
		n += len(p.Text) + len(p.Key)
	}
	return n
}

func stringsSize(items []string) int {
	n := len(items)
	for _, s := range items {
		n += len(s)
	}
	return n
}

// budgetRepl writes the kind of a match in its place (a caller's function, which counts no steps).
func budgetRepl(kind, _ string) string { return "[" + kind + "]" }

// budgetFrom is where the excerpt of a log keeps its bytes: its second half, cut at a line.
func budgetFrom(in string) int { return excerptFrom([]byte(in), len(in)/2+1) }

var budgetEntries = []budgetEntry{
	{"String", func(in string) int { return len(in) },
		func(wm *meter.Meter, in string) string { return render(wm, in, defaultRedactor.spans(wm, in)) }, String},
	{"StringKeeping", func(in string) int { return len(in) },
		func(wm *meter.Meter, in string) string { return defaultRedactor.stringKeeping(wm, in, []string{"api"}) },
		func(in string) string { return StringKeeping(in, "api") }},
	{"Clean", func(in string) int { return len(in) },
		func(wm *meter.Meter, in string) string { return fmt.Sprint(defaultRedactor.clean(wm, in, nil)) },
		func(in string) string { return fmt.Sprint(Clean(in)) }},
	{"StringTwice", func(in string) int { return len(in) },
		func(wm *meter.Meter, in string) string { return defaultRedactor.stringTwice(wm, in, nil) },
		func(in string) string { return StringTwice(in, nil) }},
	{"ReplaceFrom", func(in string) int { return len(in) },
		func(wm *meter.Meter, in string) string {
			r := defaultRedactor
			from := budgetFrom(in)
			spans := settle(wm, in, append(r.ruleMatches(wm, in), r.releasedExcerpt(wm, in, from, nil)...))
			return r.replaceSpans(wm, in, spans, budgetRepl, func(s string) string { return s })
		},
		func(in string) string {
			out, _ := defaultRedactor.ReplaceFrom(in, budgetFrom(in), nil, budgetRepl, func(s string) string { return s })
			return out
		}},
	{"Replace", func(in string) int { return len(in) },
		func(wm *meter.Meter, in string) string {
			return defaultRedactor.replaceSpans(wm, in, defaultRedactor.spans(wm, in), budgetRepl,
				func(s string) string { return s })
		},
		func(in string) string { return Replace(in, budgetRepl, func(s string) string { return s }) }},
	{"ReleasedText", func(in string) int { return len(in) },
		func(wm *meter.Meter, in string) string {
			b := defaultRedactor.before
			return b.StringWithin(wm, strings.ToValidUTF8(b.StringWithin(wm, in), "?"))
		},
		defaultRedactor.ReleasedText},
	{"Strings", func(in string) int { return stringsSize(budgetLines(in)) },
		func(wm *meter.Meter, in string) string {
			return strings.Join(defaultRedactor.strings(wm, budgetLines(in)), "\n")
		},
		func(in string) string { return strings.Join(Strings(budgetLines(in)), "\n") }},
	{"PiecesWithin", func(in string) int { return piecesSize(budgetPieces(in)) },
		func(wm *meter.Meter, in string) string {
			return strings.Join(defaultRedactor.piecesWithin(wm, budgetPieces(in), -1, false, nil), "")
		},
		func(in string) string {
			out, _ := defaultRedactor.PiecesWithin(budgetPieces(in), -1)
			return strings.Join(out, "")
		}},
	{"PiecesFitting", func(in string) int { return piecesSize(budgetPieces(in)) },
		func(wm *meter.Meter, in string) string {
			return strings.Join(defaultRedactor.piecesWithin(wm, budgetPieces(in), -1, false, budgetFits(in)), "")
		},
		func(in string) string {
			out, _ := defaultRedactor.PiecesFitting(budgetPieces(in), budgetFits(in))
			return strings.Join(out, "")
		}},
}

// budgetFits is the check of PiecesFitting for the pieces of in: the answer fits when it is no longer than in, so
// that the longer forms of a marker are tried and refused before a shorter one fits (a caller's function, which
// counts no steps).
func budgetFits(in string) func(out []string) bool {
	return func(out []string) bool {
		n := 0
		for _, s := range out {
			n += len(s)
		}
		return n <= len(in)
	}
}

// spentOn returns the steps e takes on in, with no budget to stop it.
func spentOn(e budgetEntry, in string) int64 {
	wm := meter.New(math.MaxInt64)
	e.run(wm, in)
	return wm.Spent()
}

// budgetCost is the largest work of one entry point on the inputs measured: per byte on the inputs of 16 KiB or
// more, where the fixed cost of a call is small beside it, and above WorkPerByte per byte on any input, the part
// WorkFixed covers.
type budgetCost struct {
	perByte float64
	at      string
	fixed   int64
	fixedAt string
}

func (c *budgetCost) add(name string, spent int64, size int) {
	if f := spent - WorkPerByte*int64(size); f > c.fixed {
		c.fixed, c.fixedAt = f, name
	}
	if size >= 16<<10 {
		if pb := float64(spent) / float64(size); pb > c.perByte {
			c.perByte, c.at = pb, name
		}
	}
}

// checkBudget checks B1 for one input at one entry point.
func checkBudget(t *testing.T, e budgetEntry, what, in string, cost *budgetCost) {
	t.Helper()
	spent, size := spentOn(e, in), e.size(in)
	cost.add(what, spent, size)
	if budgetHeadroom*float64(spent) > float64(budgetOf(size)) {
		t.Errorf("B1: %s on %s: %d steps on %d bytes, more than 1/%.1f of its budget %d", e.name, what, spent, size,
			budgetHeadroom, budgetOf(size))
	}
}

// budgetMeasures are the inputs of the cost tests (canonWorkMeasures) at their smaller size: every form of the
// canonical corpus, the staircases at 64 KiB and the lines of repeated quoted keys. Their work per byte is the
// same at the larger sizes (TestCanonCorpusWorkIsLinear, TestCanonStairWorkIsLinear).
func budgetMeasures() map[string]string {
	cases := canonCorpus()
	sizes := map[string]int{}
	for _, c := range cases {
		sizes[c.form] += len(c.in) + 1
	}
	out := map[string]string{}
	for _, form := range canonForms {
		out["form "+form] = canonFormText(cases, form, max(16<<10, sizes[form]))
	}
	for _, line := range canonStairLines {
		out[fmt.Sprintf("stair %q", line)] = canonStair(line, 64<<10)
	}
	for _, n := range []int{512, 2048} {
		out[fmt.Sprintf("repeated quoted keys %d", n)] = repeatedQuotedKeys(n)
	}
	maps.Copy(out, gluedQuotedPairs(64<<10))
	return out
}

// gluedQuotedPairs are lines of quoted pairs glued with no separator (auth="a" repeated), with and without a
// tail, of about n bytes each: the release before reads each pair's value again for every key before it on the
// line, up to a window, so the cost per byte rises to about 1,200 steps of that reading at 64 KiB and stays
// there (1,201 at 1 MiB), and Strings, which takes that reading twice, to about 2,500 (decision of 2026-11-21 on
// #873; found by FuzzDiffGetPodLogs).
func gluedQuotedPairs(n int) map[string]string {
	out := map[string]string{}
	for _, unit := range []string{`auth="a"`, `token="a"`} {
		for _, tail := range []string{"", "\uff1ax"} {
			out[fmt.Sprintf("glued %s tail %q", unit, tail)] = strings.Repeat(unit, n/len(unit)) + tail
		}
	}
	return out
}

// B1: every input of the corpus tables (releasedInputs) and of the cost tests fits the budget of every entry
// point with budgetHeadroom to spare. The largest cost per byte is logged: the factor of the budget is set from it
// (docs/adr/0015, amendment of 2026-11-20).
func TestBudgetFactorCoversTheCorpus(t *testing.T) {
	inputs := releasedInputs(t)
	measures := budgetMeasures()
	if raceDetector {
		// Under -race each entry point checks its own seeded sample of the corpus and none of the measures, which
		// are 16 to 140 KiB each; make test runs this package again without -race on all of them (decision of
		// 2026-11-21 on #873).
		measures = nil
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for n := range measures {
			if !yield(n) {
				return
			}
		}
	})
	for _, e := range budgetEntries {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			cost := &budgetCost{}
			for i, in := range inputs {
				if !raceKeeps(t.Name(), i) {
					continue
				}
				checkBudget(t, e, fmt.Sprintf("corpus input %d", i), in, cost)
			}
			for _, n := range names {
				checkBudget(t, e, n, measures[n], cost)
			}
			t.Logf("%s: at most %.1f steps per byte on 16 KiB or more (%s); at most %d steps above %d per byte (%s)",
				e.name, cost.perByte, cost.at, cost.fixed, WorkPerByte, cost.fixedAt)
		})
	}
}

// budgetShapes are the four inputs on which the work grew faster than the text before the budget (#873,
// decision of 2026-11-20), each at the size given: ever deeper indentation, a long run of one repeated text, a
// label made of short windows, and "--token,}" repeated (released.tokenEnd reads each value to the end of the
// run).
func budgetShapes(n int) map[string]string {
	return map[string]string{
		"staircase":          canonStair("pass\u200b: |", n),
		"repeated text":      strings.Repeat("TOKEN=\"a\",b,DB_PASSWORD: |+\n  SECRETPROBE0001", n/45+1),
		"short windows":      strings.Repeat("a=\"b\",c: |+ d,", n/14+1),
		"--token,} repeated": strings.Repeat("--token,}", n/9+1),
	}
}

// meteredSpent runs e on in on a meter of steps steps and returns what it returned, whether it was decided
// (meteredOn) and the steps it spent, those of the step that ran out included.
func meteredSpent(e budgetEntry, in string, steps int64) (out string, ok bool, spent int64) {
	out, ok = meteredOn(e.size(in), steps, func(wm *meter.Meter) string {
		defer func() { spent = wm.Spent() }()
		return e.run(wm, in)
	})
	return out, ok, spent
}

// budgetOverrun bounds the steps an entry of size bytes may spend past its budget: the one counting step that
// runs out, at most the bytes of a text handed to an index function or an expression whole (a folded view is
// at most four times its text).
func budgetOverrun(size int) int64 { return 4*int64(size) + 64 }

// B2: on the four shapes, at sizes where the work outgrew the text, every entry point stops within one counting
// step of its budget, and returns Withheld, or the decision it takes without a budget.
func TestBudgetBoundsTheFourShapes(t *testing.T) {
	sizes := []int{16 << 10, 64 << 10}
	if raceDetector {
		// One size, 8 KiB: the race detector makes each redaction 10 to 20 times slower, and make test runs both
		// sizes without it.
		sizes = []int{8 << 10}
	}
	shapeNames := slices.Sorted(maps.Keys(budgetShapes(0)))
	for ei, e := range budgetEntries {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			for _, n := range sizes {
				for name, in := range budgetShapes(n) {
					if raceDetector && name != shapeNames[ei%len(shapeNames)] {
						// Under -race each entry point checks one of the four shapes, the entry points in turn
						// all four; make test runs this package again without -race on every pair.
						continue
					}
					size := e.size(in)
					out, ok, spent := meteredSpent(e, in, budgetOf(size))
					if spent > budgetOf(size)+budgetOverrun(size) {
						t.Errorf("B2: %s on %s of %d bytes spent %d steps, budget %d", e.name, name, size, spent,
							budgetOf(size))
					}
					switch {
					case !ok:
						if got := e.public(in); !withheldOut(got) {
							t.Errorf("B3: %s on %s of %d bytes ran out of its budget and returned %q", e.name, name,
								size, redacttest.Clip(got))
						}
					case out != e.run(nil, in):
						t.Errorf("B3: %s on %s of %d bytes inside its budget differs from the decision without one",
							e.name, name, size)
					}
					t.Logf("%s on %s of %d bytes: %d steps (%.0f per byte), decided %v", e.name, name, size, spent,
						float64(spent)/float64(size), ok)
				}
			}
		})
	}
}

// budgetKnown builds an input whose right decision is known by construction: text before, a secret value after
// a secret key, text after, each part n bytes of ordinary words, and returns it with the secret and the text
// that must stay.
func budgetKnown(n, i int) (in, secret string, kept []string) {
	words := []string{"pods", "ready", "restarts", "image", "pulled", "node", "scheduled", "volume"}
	filler := func(seed int) string {
		var b strings.Builder
		for j := 0; b.Len() < n; j++ {
			b.WriteString(words[(seed+j*7)%len(words)])
			b.WriteString(" ")
		}
		return b.String()
	}
	before, after := "before "+filler(i), "after "+filler(i+3)
	secret = fmt.Sprintf("Qz9%05dSecretValue", i)
	return before + "\npassword: " + secret + "\n" + after, secret, []string{strings.TrimSpace(before), strings.TrimSpace(after)}
}

// B3: wherever the budget runs out, an entry is decided whole or withheld whole. On inputs whose decision is
// known by construction (budgetKnown), each entry point runs on budgets from none to all the steps it needs:
// below them the result is withheld and holds nothing of the entry; at them it is the decision without a budget,
// which hides the secret and keeps the text around it.
func TestBudgetDecidesWholeOrWithholdsWhole(t *testing.T) {
	for _, e := range budgetEntries {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			sizes := []int{8, 200, 3000}
			if raceDetector {
				// The race detector makes each decision 10 to 20 times slower; make test runs this package again
				// without it, on every size.
				sizes = sizes[:2]
			}
			for i, n := range sizes {
				in, secret, kept := budgetKnown(n, i)
				want := e.run(nil, in)
				if e.name != "Clean" {
					if strings.Contains(want, secret) {
						t.Fatalf("%s shows the secret of the known input without a budget: %q", e.name, redacttest.Clip(want))
					}
					for _, k := range kept {
						if !strings.Contains(strings.ReplaceAll(want, "\n", ""), strings.ReplaceAll(k, "\n", "")) {
							t.Fatalf("%s lost the text around the secret: %q", e.name, redacttest.Clip(want))
						}
					}
				}
				need := spentOn(e, in)
				for _, steps := range budgetSweep(need) {
					out, ok, _ := meteredSpent(e, in, steps)
					switch {
					case ok != (steps >= need):
						t.Errorf("%s on %d bytes with %d of %d steps: decided %v", e.name, len(in), steps, need, ok)
					case ok && out != want:
						t.Errorf("%s on %d bytes with %d steps differs from the decision without a budget", e.name,
							len(in), steps)
					case !ok && out != "":
						t.Errorf("%s on %d bytes with %d steps ran out and returned %q", e.name, len(in), steps,
							redacttest.Clip(out))
					}
				}
			}
		})
	}
}

// budgetSweep returns budgets from none to need steps: every step up to 64, then about 200 spread over the rest,
// need-1 and need. Under -race it is every step up to 16 and about 40 spread over the rest, need-1 and need: each
// budget is one redaction, 10 to 20 times slower there, and make test runs the whole sweep without -race.
func budgetSweep(need int64) []int64 {
	first, spread := int64(64), int64(200)
	if raceDetector {
		first, spread = 16, 40
	}
	var out []int64
	for s := range min(need, first) {
		out = append(out, s)
	}
	for k := int64(1); k <= spread; k++ {
		out = append(out, need*k/(spread+1))
	}
	return append(out, max(need-1, 0), need)
}

// withheldOut reports whether got, an entry point's output joined, is that of an entry withheld: Withheld (the
// parts after it empty), or false from Clean.
func withheldOut(got string) bool { return strings.TrimRight(got, "\n") == Withheld || got == "false" }

// B3: an entry longer than MaxEntry is withheld at every entry point before any work.
func TestBudgetWithholdsAnEntryOverTheBound(t *testing.T) {
	in := strings.Repeat("a", MaxEntry+1)
	for _, e := range budgetEntries {
		if got := e.public(in); !withheldOut(got) {
			t.Errorf("%s on %d bytes returned %q", e.name, len(in), redacttest.Clip(got))
		}
		if _, ok, spent := meteredSpent(e, in, budgetOf(len(in))); ok || spent != 0 {
			t.Errorf("%s on %d bytes: decided %v after %d steps", e.name, len(in), ok, spent)
		}
	}
}
