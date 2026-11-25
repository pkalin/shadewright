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

	"github.com/shadewright-labs/shadewright/internal/redact/meter"
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// The work budget of an entry (#873, decision of 2026-11-20; budget.go). The meter counts the steps of one
// redaction in every build, so these tests need no build tag: what an entry spent is what its meter counted.
//
//	B1: on every shape of the corpus (corpusShapes), at every entry point, the work counted with no budget grows
//	    at most maxGrowth times when the input doubles from 32 to 64 KiB, with no shape excused (#962 removed the
//	    two excused by the decision of 2026-11-21 on #873). The budget, not a factor that covers every shape, is
//	    what bounds the work: a linear shape above WorkPerByte per byte is withheld, and a shape whose work grows
//	    faster than the text fails here.
//	B2: the work stays inside the budget for every input: an entry stops within one counting step of it.
//	B3: an entry is either decided whole, as without a budget, or withheld whole (Withheld, nothing of it),
//	    wherever its budget runs out; and an entry longer than MaxEntry is withheld before any work.

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

// corpusShapes are the shapes the work tests build, each to about n bytes: every form of the canonical corpus
// (repeated), every staircase, the line of repeated quoted keys, the glued quoted pairs, the four shapes of the
// budget (budgetShapes), "-password," repeated and TOKEN="a" repeated before a long blank line (the last two found
// by the fuzz targets of #845).
func corpusShapes(n int) map[string]string {
	cases := canonCorpus()
	out := map[string]string{}
	for _, form := range canonForms {
		out["form "+form] = canonFormText(cases, form, n)
	}
	for _, line := range canonStairLines {
		out[fmt.Sprintf("stair %q", line)] = canonStair(line, n)
	}
	out["repeated quoted keys"] = repeatedQuotedKeys(n / len(repeatedQuotedKeys(1)))
	maps.Copy(out, gluedQuotedPairs(n))
	maps.Copy(out, budgetShapes(n))
	maps.Copy(out, shortFlagShapes(n))
	out[passwordFlagRun] = strings.Repeat(passwordFlagUnit, n/len(passwordFlagUnit))
	out[`TOKEN="a" before a blank line`] = strings.Repeat(`TOKEN="a"`, n/len(`TOKEN="a"`)) + "\"\n" +
		strings.Repeat(" ", 4096) + "\"："
	return out
}

// passwordFlagRun names "-password," repeated in corpusShapes.
const passwordFlagRun = "-password, repeated"

// The units of the shapes that cost the most per byte before #962, which more than one test builds.
const (
	passwordFlagUnit = "-password,"
	gluedAuthUnit    = `auth="a"`
	gluedTokenUnit   = `token="a"`
)

// gluedQuotedPairs are lines of quoted pairs glued with no separator (auth="a" repeated), with and without a
// tail, of about n bytes each: the release before reads each pair's value again for every key before it on the
// line, up to a window: a shape whose work grew linearly but passed WorkPerByte per byte and was withheld from
// about 30 KiB (decision of 2026-11-21 on #873; found by FuzzDiffGetPodLogs), until #962 stopped the release before
// reading the value of a key glued to the quote of the pair before it (released.keyValueExtent).
func gluedQuotedPairs(n int) map[string]string {
	out := map[string]string{}
	for _, unit := range []string{gluedAuthUnit, gluedTokenUnit} {
		for _, tail := range []string{"", "：x"} {
			out[fmt.Sprintf("glued %s tail %q", unit, tail)] = strings.Repeat(unit, n/len(unit)) + tail
		}
	}
	return out
}

// maxGrowth is how many times its work at 32 KiB an entry may take at 64 KiB (decision of 2026-11-21 on #873):
// twice, and a margin for the part of the work that is not proportional to the text (TOKEN="a" before a blank
// line has a fixed blank line of 4 KiB).
const maxGrowth = 2.3

// growthOf returns how many times its work on small e takes on large, an input about twice as long: the work is
// counted with no budget, and its ratio is scaled to an exact doubling, as the texts built are not exactly 32 and
// 64 KiB.
func growthOf(e budgetEntry, small, large string) (growth float64, wSmall, wLarge int64) {
	wSmall, wLarge = spentOn(e, small), spentOn(e, large)
	return float64(wLarge) / float64(wSmall) * 2 * float64(e.size(small)) / float64(e.size(large)), wSmall, wLarge
}

// growthFails returns the growth of e on the shape name, built at small and at twice its size at large, and its
// B1 failure or nothing.
func growthFails(e budgetEntry, name, small, large string) (growth float64, fail string) {
	g, ws, wl := growthOf(e, small, large)
	if g > maxGrowth {
		return g, fmt.Sprintf("B1: %s on %s: %d steps on %d bytes, %d on %d: %.2f times for twice the size, over %.1f",
			e.name, name, ws, e.size(small), wl, e.size(large), g, maxGrowth)
	}
	return g, ""
}

// B1: the work on every shape of the corpus grows linearly at every entry point, from 32 to 64 KiB. Both sizes are read by windows (windowMinText is 4 KiB), so the one-time step from
// reading a text whole to reading it by windows is not in the doubling.
func TestCorpusWorkGrowsLinearly(t *testing.T) {
	if raceDetector {
		t.Skip("it counts work on texts of 32 and 64 KiB, 10 to 20 times slower under -race; make test runs this " +
			"package again without it")
	}
	small, large := corpusShapes(32<<10), corpusShapes(64<<10)
	names := slices.Sorted(maps.Keys(small))
	for _, e := range budgetEntries {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			worst, worstAt := 0.0, ""
			for _, name := range names {
				g, fail := growthFails(e, name, small[name], large[name])
				if fail != "" {
					t.Error(fail)
				}
				if g > worst {
					worst, worstAt = g, name
				}
			}
			t.Logf("%s: at most %.2f times from 32 to 64 KiB (%s)", e.name, worst, worstAt)
		})
	}
}

// B1 can fail: a reading that reads the rest of the text again at every line, as the staircases once did (#845),
// grows more than maxGrowth times for twice the size and fails the check.
func TestCorpusGrowthCheckFindsAQuadraticReading(t *testing.T) {
	quadratic := budgetEntry{name: "a quadratic reading", size: func(in string) int { return len(in) },
		run: func(wm *meter.Meter, in string) string {
			for i := range len(in) {
				if in[i] == '\n' {
					wm.Spend(len(in) - i)
				}
			}
			return in
		}}
	small, large := canonStair("pass: |", 4<<10), canonStair("pass: |", 8<<10)
	g, fail := growthFails(quadratic, "stair", small, large)
	if fail == "" {
		t.Errorf("the growth check passed a quadratic reading: %.2f times for twice the size", g)
	}
	t.Logf("a quadratic reading on the staircase: %.2f times for twice the size", g)
}

// budgetShapes are the four inputs on which the work grew faster than the text before the budget (#873,
// decision of 2026-11-20), each at the size given: ever deeper indentation, a long run of one repeated text, a
// label made of short windows, and "--token,}" repeated (the release before read each value to the end of the
// run until #962).
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
	out, ok = meteredOn(defaultRedactor, e.size(in), steps, func(wm *meter.Meter) string {
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
