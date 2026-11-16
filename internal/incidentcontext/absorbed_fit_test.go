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

package incidentcontext

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// The tally of [bothTally], as the summary writes it.
var bothBlock = absorbedHeader + "\n" +
	"  BackOff: 12 occurrences, last seen 2026-11-05T11:59:00Z\n" +
	"  Unhealthy: 3 occurrences, last seen 2026-11-05T11:58:00Z\n"

func bothTally() []remediationv1alpha1.AbsorbedTally {
	return []remediationv1alpha1.AbsorbedTally{
		absorbed(remediationv1alpha1.AbsorbedUnhealthy, 3, testNow.Add(-2*time.Minute)),
		absorbed(remediationv1alpha1.AbsorbedBackOff, 12, testNow.Add(-time.Minute)),
	}
}

// wideCause is a cause Incident whose subject, reason and symptom are at the schema's limits, the symptom in
// two-byte text, and whose reporting controller is rc.
func wideCause(rc string, tally ...remediationv1alpha1.AbsorbedTally) *remediationv1alpha1.Incident {
	inc := cause(tally...)
	inc.Spec.Subject.Namespace = strings.Repeat("n", 63)
	inc.Spec.Subject.Name = strings.Repeat("w", 253)
	inc.Spec.Reason = strings.Repeat("R", 128)
	inc.Spec.Symptom = strings.Repeat("é", 512)
	inc.Spec.ReportingController = rc
	return inc
}

// cutNote is the note a shortened value ends with.
var cutNote = regexp.MustCompile(` \(([0-9]+) bytes cut\)$`)

// noRedaction leaves text as it is. The generated sweeps use it: they check the arithmetic of the bound over
// thousands of summaries, and the redactor, which only ever shortens text, is the slow part under the race
// detector. TestAbsorbedTallyFitUsesTheRedactedText and the cases near the bound run with redaction.
type noRedaction struct{}

func (noRedaction) String(s string) string { return s }

// unbounded returns a builder whose summary and total bounds are the largest allowed, redacting with r.
func unbounded(t *testing.T, r Redactor) *Builder {
	t.Helper()
	return newBuilder(t, reader(t), sources{}, func(c *Config) {
		c.Limits = DefaultLimits()
		c.Limits.Summary, c.Limits.Total = MaxLimitBytes, MaxLimitBytes
		if r != nil {
			c.Redactor = r
		}
	})
}

// fullSummary returns the summary of inc without its tally and without any cut, built by b from
// [unbounded]: what the lines are before the section bound.
func fullSummary(t *testing.T, b *Builder, inc *remediationv1alpha1.Incident) string {
	t.Helper()
	inc = inc.DeepCopy()
	inc.Status.Absorbed = nil
	sum := b.summaryBlock(inc)
	if hasLabel(sum, LabelSectionCut) {
		t.Fatalf("the summary without a bound was cut:\n%s", sum.Text)
	}
	return sum.Text
}

// checkTallyShown checks the decision of 2026-11-16 (#796, "the tally is always shown, with room kept for
// it") on one summary: full is the summary of the same Incident without its tally and without any cut, with
// its summary with the tally whose block is block, n the section bound. The summary ends with the whole
// block and stays within n; it has every line of full, in its order, each either unchanged or, for the
// symptom and the reporting controller, cut to a head of the line followed by a note of the bytes cut; and
// when full fits next to the longest tally, nothing is cut. A cut value's head stays quotable, its note does
// not, and no part of the tally is quotable.
func checkTallyShown(t *testing.T, name, full string, with Block, block string, n int) {
	t.Helper()
	if len(with.Text) > n {
		t.Errorf("%s: %d bytes, over the bound %d", name, len(with.Text), n)
	}
	head, ok := strings.CutSuffix(with.Text, block)
	if !ok {
		t.Errorf("%s: the summary does not end with the whole tally\n%q\nsummary:\n%s", name, block, with.Text)
		return
	}
	if len(full)+longestTally <= n && head != full {
		t.Errorf("%s: there was room, yet the lines changed:\n%s\nwant\n%s", name, head, full)
	}
	got, want := strings.Split(head, "\n"), strings.Split(full, "\n")
	if len(got) != len(want) {
		t.Errorf("%s: %d lines, want the %d of the summary without the tally:\n%s\nwant\n%s", name, len(got),
			len(want), head, full)
		return
	}
	for i, w := range want {
		g := got[i]
		if g == w {
			continue
		}
		m := cutNote.FindStringSubmatchIndex(g)
		shortenable := strings.HasPrefix(w, "symptom: ") || strings.HasPrefix(w, "reported by: ")
		if m == nil || !shortenable || !strings.HasPrefix(w, g[:m[0]]) ||
			strconv.Itoa(len(w)-m[0]) != g[m[2]:m[3]] {
			t.Errorf("%s: line %q is not line %q or a head of it with the note of its cut", name, g, w)
			continue
		}
		if !hasLabel(with, LabelSectionCut) {
			t.Errorf("%s: a value was cut without the section cut label", name)
		}
	}
	for _, part := range with.Data() {
		if strings.Contains(part, "bytes cut)") || strings.Contains(part, absorbedHeader) ||
			strings.Contains(part, " occurrences, last seen ") {
			t.Errorf("%s: %q is quotable", name, part)
		}
	}
}

// The Incidents of the defect found on 2026-11-16 (#796), where the cut left the tally's heading with no line
// under it, or one reason without the other, and pushed the phase line out, and one with every field at the
// schema's limit in multi-byte text: each shows the whole tally and keeps its phase line.
func TestAbsorbedTallyNearTheSummaryBound(t *testing.T) {
	b, ub := newBuilder(t, reader(t), sources{}, nil), unbounded(t, nil)
	extreme := wideCause(strings.Repeat("𝄞", 253), bothTally()...)
	extreme.Spec.Subject.Kind = strings.Repeat("é", 63)
	extreme.Spec.Reason = strings.Repeat("日", 128)
	extreme.Spec.Symptom = strings.Repeat("𝄞", 256)
	for name, inc := range map[string]*remediationv1alpha1.Incident{
		"heading alone":  wideCause(strings.Repeat("r", 140)+strings.Repeat("é", 113), bothTally()...),
		"one reason":     wideCause(strings.Repeat("r", 140)+strings.Repeat("é", 53), bothTally()...),
		"schema extreme": extreme,
	} {
		with := one(t, build(t, b, inc), SectionSummary)
		if !strings.Contains(with.Text, "\nphase: Open\n") {
			t.Errorf("%s: no phase line:\n%s", name, with.Text)
		}
		checkTallyShown(t, name, fullSummary(t, ub, inc), with, bothBlock, DefaultSummaryBytes)
	}
}

// Generated sweep around the section bound, the answer known by construction: the reporting controller grows
// one character at a time, in one-, two-, three- and four-byte text, so the lines end at every distance from
// the bound, and each tally (one reason, the other, both, both with the largest counts) is shown whole after
// every line of the summary without it, which the cut only shortens. Where the lines are cut does not move
// with the counts: room is kept for the longest tally.
//
// Redaction: the tally is redacted before it is measured, and the other lines are measured before the
// redaction of the whole text, which never makes text longer ([Redactor]); so redaction can only leave more
// room than was measured, never break the bound or the tally (TestAbsorbedTallyFitUsesTheRedactedText).
func TestAbsorbedTallyFitGenerated(t *testing.T) {
	t.Parallel()
	b := newBuilder(t, reader(t), sources{}, func(c *Config) { c.Redactor = noRedaction{} })
	ub := unbounded(t, noRedaction{})
	largest := []remediationv1alpha1.AbsorbedTally{
		absorbed(remediationv1alpha1.AbsorbedBackOff, math.MaxInt64, testNow),
		absorbed(remediationv1alpha1.AbsorbedUnhealthy, math.MaxInt64, testNow),
	}
	tallies := []struct {
		name  string
		tally []remediationv1alpha1.AbsorbedTally
		block string
	}{
		{"BackOff", []remediationv1alpha1.AbsorbedTally{absorbed(remediationv1alpha1.AbsorbedBackOff, 7, testNow)},
			absorbedHeader + "\n  BackOff: 7 occurrences, last seen 2026-11-05T12:00:00Z\n"},
		{"Unhealthy", []remediationv1alpha1.AbsorbedTally{absorbed(remediationv1alpha1.AbsorbedUnhealthy, 1_000_000, testNow)},
			absorbedHeader + "\n  Unhealthy: 1000000 occurrences, last seen 2026-11-05T12:00:00Z\n"},
		{"both", bothTally(), bothBlock},
		{"largest", largest, absorbedHeader + "\n  BackOff: 9223372036854775807 occurrences, last seen 2026-11-05T12:00:00Z\n" +
			"  Unhealthy: 9223372036854775807 occurrences, last seen 2026-11-05T12:00:00Z\n"},
	}
	shortened := 0
	for _, unit := range []string{"r", "é", "日", "𝄞"} {
		for k := 0; k*len(unit) <= 400; k++ {
			rc := strings.Repeat("r", 140) + strings.Repeat(unit, k)
			full := fullSummary(t, ub, wideCause(rc))
			heads := make([]string, 0, len(tallies))
			for _, tc := range tallies {
				with := b.summaryBlock(wideCause(rc, tc.tally...))
				name := unit + "/" + strconv.Itoa(k) + "/" + tc.name
				checkTallyShown(t, name, full, with, tc.block, DefaultSummaryBytes)
				if strings.Contains(with.Text, "bytes cut)") {
					shortened++
				}
				heads = append(heads, strings.TrimSuffix(with.Text, tc.block))
			}
			for i := range heads {
				if heads[i] != heads[0] {
					t.Errorf("%s/%d: the lines before tally %s differ from those before tally %s:\n%s\nwant\n%s",
						unit, k, tallies[i].name, tallies[0].name, heads[i], heads[0])
				}
			}
		}
	}
	if shortened == 0 {
		t.Fatal("no case shortened a value: the sweep does not reach the bound")
	}
}

// shrinkOccurrences is a redactor that shortens the tally's word "occurrences", as a redaction rule could.
type shrinkOccurrences struct{ Redactor }

func (r shrinkOccurrences) String(s string) string {
	return strings.ReplaceAll(r.Redactor.String(s), " occurrences,", ",")
}

// The tally is redacted before it is placed: a redactor that changes it gives the redacted tally, whole and
// last, and the summary stays within its bound whatever the lines' length.
func TestAbsorbedTallyFitUsesTheRedactedText(t *testing.T) {
	b := newBuilder(t, reader(t), sources{}, func(c *Config) { c.Redactor = shrinkOccurrences{c.Redactor} })
	block := strings.ReplaceAll(bothBlock, " occurrences,", ",")
	for k := 0; k <= 700; k += 20 {
		with := b.summaryBlock(wideCause(strings.Repeat("r", 140+k), bothTally()...))
		if !strings.HasSuffix(with.Text, block) || len(with.Text) > DefaultSummaryBytes ||
			!strings.Contains(with.Text, "\nphase: Open\n") {
			t.Fatalf("k %d: summary of %d bytes, want it to end with\n%s\nsummary:\n%s", k, len(with.Text), block, with.Text)
		}
	}
}

// The total bound cuts the summary last, and only when bounds that add up to more than the total are
// configured; when it cuts into the tally, the tally goes whole: the summary keeps a whole-line prefix of
// the text without the tally, or that text followed by the whole tally.
func TestAbsorbedTallyTotalCutGenerated(t *testing.T) {
	t.Parallel()
	lim := Limits{Total: MinTotalBytes, Summary: 8 << 10, Status: MinSectionBytes, Logs: MinSectionBytes,
		Metrics: MinSectionBytes, Git: MinSectionBytes, Policy: MinSectionBytes}
	b := newBuilder(t, reader(t), sources{}, func(c *Config) { c.Limits, c.Redactor = lim, noRedaction{} })
	// Values a fake client lets in beyond the schema's limits, so the summary alone outgrows the total.
	wide := func(k int, tally ...remediationv1alpha1.AbsorbedTally) *remediationv1alpha1.Incident {
		inc := wideCause(strings.Repeat("r", 1000), tally...)
		inc.Spec.Subject.Kind = strings.Repeat("K", 1000)
		inc.Spec.Type = strings.Repeat("T", 1000)
		inc.Spec.Symptom = strings.Repeat("s", k)
		return inc
	}
	cuts := 0
	for k := 0; k <= 1024; k++ {
		// The total cut of the summary alone: the cut of the other sections comes first and is tested elsewhere.
		c := &Context{Blocks: []Block{b.summaryBlock(wide(k))}}
		b.fitTotal(c)
		plain := c.Blocks[0].Text
		with := &Context{Blocks: []Block{b.summaryBlock(wide(k, bothTally()...))}}
		b.fitTotal(with)
		got := with.Blocks[0]
		if !hasLabel(got, LabelContextCut) {
			continue
		}
		cuts++
		text := got.Text
		if i := strings.Index(text, absorbedHeader); i >= 0 {
			if text[i:] != bothBlock {
				t.Fatalf("k %d: the total cut left part of the tally:\n%q", k, text[i:])
			}
			text = text[:i]
		}
		if !strings.HasPrefix(plain, text) {
			t.Fatalf("k %d: the summary\n%q\nis not a prefix of the summary without the tally\n%q", k, text, plain)
		}
		if text != "" && !strings.HasSuffix(text, "\n") {
			t.Fatalf("k %d: the summary ends inside a line: %q", k, text)
		}
		if with.Size() > lim.Total {
			t.Fatalf("k %d: context of %d bytes over the total %d", k, with.Size(), lim.Total)
		}
	}
	if cuts == 0 {
		t.Fatal("the total bound never cut the summary")
	}
}
