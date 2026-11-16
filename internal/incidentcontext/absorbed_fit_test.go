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

// checkTallyFit checks the decision of 2026-11-16 (#796) on one summary: plain is the summary of the same
// Incident with an empty tally, with its summary with the tally whose block is block, n the section bound.
// The tally is the last thing in the summary and is cut whole: the summary is plain followed by the whole
// block, or plain alone, marked cut, when the block does not fit after it or plain was already cut (nothing
// is left of the section). It never removes a line plain has.
func checkTallyFit(t *testing.T, name string, plainBlk, with Block, block string, n int) {
	t.Helper()
	plain := plainBlk.Text
	switch with.Text {
	case plain + block:
		if len(with.Text) > n {
			t.Errorf("%s: %d bytes, over the bound %d", name, len(with.Text), n)
		}
	case plain:
		if len(plain)+len(block) <= n && !hasLabel(plainBlk, LabelSectionCut) {
			t.Errorf("%s: the tally (%d bytes) fits after the summary (%d bytes) in %d but was left out", name,
				len(block), len(plain), n)
		}
		if !hasLabel(with, LabelSectionCut) {
			t.Errorf("%s: the tally was left out without the section cut label", name)
		}
	default:
		t.Errorf("%s: summary\n%q\nis neither the summary without the tally (%q) nor it followed by the whole tally\n%q",
			name, with.Text, plain, block)
	}
}

// The two Incidents of the defect found on 2026-11-16 (#796): near the bound, the cut left the tally's heading
// with no line under it, or one reason without the other, and pushed the phase line out.
func TestAbsorbedTallyNearTheSummaryBound(t *testing.T) {
	b := newBuilder(t, reader(t), sources{}, nil)
	for name, rc := range map[string]string{
		"heading alone": strings.Repeat("r", 140) + strings.Repeat("é", 113),
		"one reason":    strings.Repeat("r", 140) + strings.Repeat("é", 53),
	} {
		plain := one(t, build(t, b, wideCause(rc)), SectionSummary)
		if !strings.HasSuffix(plain.Text, "phase: Open\n") {
			t.Fatalf("%s: the summary without a tally lost its phase line:\n%s", name, plain.Text)
		}
		with := one(t, build(t, b, wideCause(rc, bothTally()...)), SectionSummary)
		if !strings.Contains(with.Text, "phase: Open\n") {
			t.Errorf("%s: the tally pushed the phase line out:\n%s", name, with.Text)
		}
		checkTallyFit(t, name, plain, with, bothBlock, DefaultSummaryBytes)
	}
}

// Generated sweep around the section bound, the answer known by construction: the reporting controller grows
// one character at a time, in one-, two-, three- and four-byte text, so the summary without the tally ends
// at every distance from the bound, and each tally (one reason, the other, both) is either written whole
// after every other line or left out. The fit is decided on the redacted text (the redactor never makes text
// longer, and the tally is redacted before it is measured): see TestAbsorbedTallyFitUsesTheRedactedText.
func TestAbsorbedTallyFitGenerated(t *testing.T) {
	b := newBuilder(t, reader(t), sources{}, nil)
	tallies := map[string]struct {
		tally []remediationv1alpha1.AbsorbedTally
		block string
	}{
		"BackOff": {[]remediationv1alpha1.AbsorbedTally{absorbed(remediationv1alpha1.AbsorbedBackOff, 7, testNow)},
			absorbedHeader + "\n  BackOff: 7 occurrences, last seen 2026-11-05T12:00:00Z\n"},
		"Unhealthy": {[]remediationv1alpha1.AbsorbedTally{absorbed(remediationv1alpha1.AbsorbedUnhealthy, 1_000_000, testNow)},
			absorbedHeader + "\n  Unhealthy: 1000000 occurrences, last seen 2026-11-05T12:00:00Z\n"},
		"both": {bothTally(), bothBlock},
	}
	for _, unit := range []string{"r", "é", "日", "𝄞"} {
		for k := 0; k*len(unit) <= 400; k++ {
			rc := strings.Repeat("r", 140) + strings.Repeat(unit, k)
			plain := one(t, build(t, b, wideCause(rc)), SectionSummary)
			for tn, tc := range tallies {
				with := one(t, build(t, b, wideCause(rc, tc.tally...)), SectionSummary)
				checkTallyFit(t, unit+"/"+strconv.Itoa(k)+"/"+tn, plain, with, tc.block, DefaultSummaryBytes)
			}
		}
	}
}

// shrinkOccurrences is a redactor that shortens the tally's word "occurrences", as a redaction rule could:
// the tally fits only if the builder measures it after redaction.
type shrinkOccurrences struct{ Redactor }

func (r shrinkOccurrences) String(s string) string {
	return strings.ReplaceAll(r.Redactor.String(s), " occurrences,", ",")
}

// The fit of the tally is decided on its redacted text: with a redactor that shortens it, a tally that fits
// only once redacted is written, and the summary stays within its bound.
func TestAbsorbedTallyFitUsesTheRedactedText(t *testing.T) {
	b := newBuilder(t, reader(t), sources{}, func(c *Config) { c.Redactor = shrinkOccurrences{c.Redactor} })
	raw, redacted := len(bothBlock), len(strings.ReplaceAll(bothBlock, " occurrences,", ","))
	found := false
	for k := 0; k <= 700; k++ {
		rc := strings.Repeat("r", 140+k)
		plain := one(t, build(t, b, wideCause(rc)), SectionSummary).Text
		if len(plain)+redacted > DefaultSummaryBytes || len(plain)+raw <= DefaultSummaryBytes {
			continue
		}
		found = true
		with := one(t, build(t, b, wideCause(rc, bothTally()...)), SectionSummary).Text
		if !strings.HasPrefix(with, plain) || !strings.Contains(with, "  BackOff: 12, last seen") ||
			!strings.Contains(with, "  Unhealthy: 3, last seen") || len(with) > DefaultSummaryBytes {
			t.Fatalf("k %d: the tally fits once redacted but the summary is\n%s", k, with)
		}
	}
	if !found {
		t.Fatal("no reporting controller length puts the summary between the two sizes of the tally")
	}
}

// The total bound cuts the summary last, and only when bounds that add up to more than the total are
// configured; when it cuts into the tally, the tally goes whole: the summary keeps a whole-line prefix of
// the text without the tally, or that text followed by the whole tally.
func TestAbsorbedTallyTotalCutGenerated(t *testing.T) {
	lim := Limits{Total: MinTotalBytes, Summary: 8 << 10, Status: MinSectionBytes, Logs: MinSectionBytes,
		Metrics: MinSectionBytes, Git: MinSectionBytes, Policy: MinSectionBytes}
	b := newBuilder(t, reader(t), sources{}, func(c *Config) { c.Limits = lim })
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
		c := build(t, b, wide(k))
		plain := one(t, c, SectionSummary).Text
		with := build(t, b, wide(k, bothTally()...))
		got := one(t, with, SectionSummary)
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
