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
	"slices"
	"strconv"
	"strings"
	"testing"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/redact/redacttest"
)

// The tests of the third decision of 2026-11-16 on #796: the fit holds at every summary bound the settings
// allow, and a cut is always visible.

// largestTally is the tally with the largest counts, and largestBlock its lines as the summary writes them.
var (
	largestTally = []remediationv1alpha1.AbsorbedTally{
		absorbed(remediationv1alpha1.AbsorbedBackOff, math.MaxInt64, testNow),
		absorbed(remediationv1alpha1.AbsorbedUnhealthy, math.MaxInt64, testNow),
	}
	largestBlock = absorbedHeader + "\n" +
		"  BackOff: 9223372036854775807 occurrences, last seen 2026-11-05T12:00:00Z\n" +
		"  Unhealthy: 9223372036854775807 occurrences, last seen 2026-11-05T12:00:00Z\n"
)

// schemaMax returns a cause Incident whose every field the summary shows is at the schema's limit: the kind,
// namespace and name in the ASCII their patterns require, and the reason, reporting controller and symptom
// in unit, a character of one to four bytes, at their maximum length in characters. The symptom is cut to
// the longest value a line shows whole (maxValueBytes; a longer one is replaced by a short note). The phase
// is the longest one and the count the largest.
func schemaMax(unit string, tally ...remediationv1alpha1.AbsorbedTally) *remediationv1alpha1.Incident {
	inc := cause(tally...)
	inc.Spec.Subject.Kind = "K" + strings.Repeat("k", 62)
	inc.Spec.Subject.Namespace = strings.Repeat("n", 63)
	inc.Spec.Subject.Name = strings.Repeat("w", 253)
	inc.Spec.Reason = strings.Repeat(unit, 128)
	inc.Spec.ReportingController = strings.Repeat(unit, 253)
	inc.Spec.Symptom = strings.Repeat(unit, min(1024, maxValueBytes/len(unit)))
	inc.Status.Phase = remediationv1alpha1.IncidentEscalated
	inc.Status.Count = math.MaxInt64
	return inc
}

// withSummary returns a copy of b whose summary bound is n.
func withSummary(b *Builder, n int) *Builder {
	c := *b
	c.limits.Summary = n
	return &c
}

// Generated, the answer known by construction: at every summary bound from the smallest the settings allow
// to the default, an Incident at the schema's limits, in ASCII text that spells words and in two- and
// four-byte text, with no tally and with the largest one, keeps every line of its summary, the phase
// included, ends with the whole tally, and stays within the bound. A value is shortened only after every value
// before it in the fixed order (symptom, reporting controller, reason, subject name). Every fourth bound is
// checked without redaction, which is the slow part under the race detector, and every 128th with it.
func TestSummaryFitsAtEveryBoundGenerated(t *testing.T) {
	t.Parallel()
	words := cause()
	words.Spec.Subject.Kind = "K" + strings.Repeat("k", 62)
	words.Spec.Subject.Namespace = strings.Repeat("n", 63)
	words.Spec.Subject.Name = strings.Repeat("web.", 63) + "x"
	words.Spec.Reason = strings.Repeat("Back off ", 14) + "Bac"
	words.Spec.ReportingController = strings.Repeat("kube let ", 28) + "k"
	words.Spec.Symptom = strings.Repeat("the pod failed. ", 64)
	words.Status.Phase = remediationv1alpha1.IncidentEscalated
	cases := map[string]*remediationv1alpha1.Incident{
		"words": words, "two-byte": schemaMax("é"), "four-byte": schemaMax("𝄞"),
	}
	cuts := 0
	for _, step := range []int{4, 128} {
		var r Redactor = noRedaction{}
		if step > 4 {
			r = nil
		}
		b := newBuilder(t, reader(t), sources{}, func(c *Config) {
			if r != nil {
				c.Redactor = r
			}
		})
		ub := unbounded(t, r)
		anyReasonShowsTally(b, ub)
		for name, inc := range cases {
			full := wantSummary(t, ub, inc)
			for _, tc := range []struct {
				tally []remediationv1alpha1.AbsorbedTally
				block string
			}{{nil, ""}, {largestTally, largestBlock}} {
				inc := inc.DeepCopy()
				inc.Status.Absorbed = tc.tally
				for n := MinSectionBytes; n <= DefaultSummaryBytes; n += step {
					with := withSummary(b, n).summaryBlock(inc)
					id := name + "/" + strconv.Itoa(len(tc.tally)) + "/" + strconv.Itoa(n) + "/" + strconv.Itoa(step)
					if !strings.Contains(with.Text, "\nphase: Escalated\n") {
						t.Errorf("%s: no phase line:\n%s", id, with.Text)
					}
					cut := checkTallyShown(t, id, full, with, tc.block, n)
					cuts += len(cut)
					for i, label := range shortenable {
						if slices.Contains(cut, label) {
							for _, before := range shortenable[:i] {
								if !slices.Contains(cut, before) {
									t.Errorf("%s: %q was shortened, %q before it was not:\n%s", id, label, before, with.Text)
								}
							}
						}
					}
				}
			}
		}
	}
	if cuts == 0 {
		t.Fatal("no value was shortened: the sweep does not reach the bound")
	}
}

// The Incident proven on 2026-11-16 (#796): at the smallest summary bound with a tally, its reason,
// occurrences, seen times and phase lines were removed, and at 1200 its phase. Each line now stays.
func TestSummaryAtTheSmallestBoundKeepsEveryLine(t *testing.T) {
	b, ub := newBuilder(t, reader(t), sources{}, nil), unbounded(t, nil)
	anyReasonShowsTally(b, ub)
	inc := cause(bothTally()...)
	inc.Spec.Subject.Kind = "K" + strings.Repeat("k", 62)
	inc.Spec.Subject.Namespace = strings.Repeat("n", 63)
	inc.Spec.Subject.Name = strings.Repeat("w", 253)
	inc.Spec.Reason = strings.Repeat("𝄞", 128)
	inc.Spec.Symptom = strings.Repeat("𝄞", 256)
	full := wantSummary(t, ub, inc)
	for _, n := range []int{MinSectionBytes, 1200, 1400} {
		for _, tc := range []struct {
			tally []remediationv1alpha1.AbsorbedTally
			block string
		}{{nil, ""}, {bothTally(), bothBlock}} {
			inc := inc.DeepCopy()
			inc.Status.Absorbed = tc.tally
			with := withSummary(b, n).summaryBlock(inc)
			for _, l := range []string{"\nreason: ", "\noccurrences: 3\n", "\nfirst seen: ", "\nlast seen: ", "\nphase: Open\n"} {
				if !strings.Contains(with.Text, l) {
					t.Errorf("bound %d, tally %d: no line %q:\n%s", n, len(tc.tally), l, with.Text)
				}
			}
			checkTallyShown(t, strconv.Itoa(n), full, with, tc.block, n)
		}
	}
}

// sweepCut places text, which holds secret, at the end of a symptom and moves the summary bound one byte at a
// time from where the cut lands in the filler before the text to where nothing is cut, so the planned cut
// lands before the text, at every byte of it (inside a key, right after it, inside the marker of the secret)
// and after it. At each bound the summary keeps every line, the symptom's cut note is intact and its number
// right, whatever the redaction of the whole text does next to it, and no part of the secret is in the text.
// The bounds are below the smallest the settings allow: the fit does not depend on the bound, and a short
// summary keeps the sweep fast under the race detector.
func sweepCut(t *testing.T, b, ub *Builder, name, text, secret string, tally []remediationv1alpha1.AbsorbedTally, block string) {
	t.Helper()
	room := 0
	if tally != nil {
		room = longestTally
	}
	const margin = 24
	inc := cause(tally...)
	inc.Spec.Symptom = strings.Repeat("é", margin) + "\n" + text
	symptom := func(summary string) string {
		for l := range strings.SplitSeq(summary, "\n") {
			if v, ok := strings.CutPrefix(l, "symptom: "); ok {
				return v
			}
		}
		t.Fatalf("%s: no symptom line:\n%s", name, summary)
		return ""
	}
	want := wantSummary(t, ub, inc)
	full := want.text
	filler := 2*margin + 1
	region := len(want.plain["symptom: "]) - len("symptom: ") - filler // the text after the filler, redacted
	first, uncut := true, false
	for n := len(full) + room - region - margin; n <= len(full)+room; n++ {
		with := withSummary(b, n).summaryBlock(inc)
		id := name + "/" + strconv.Itoa(n)
		checkTallyShown(t, id, want, with, block, n)
		for i := 0; i+8 <= len(secret) || i == 0; i++ {
			part := secret[i:min(i+8, len(secret))]
			if strings.Contains(with.Text, part) {
				t.Errorf("%s: part %q of the secret is in the summary:\n%s", id, part, with.Text)
				break
			}
		}
		v := symptom(with.Text)
		if first {
			if m := cutNoteIn.FindStringSubmatch(v); m == nil || len(m[1]) >= filler {
				t.Errorf("%s: the sweep does not start with a cut before the text: %q", id, v)
			}
			first = false
		}
		uncut = v == symptom(full)
	}
	if !uncut {
		t.Errorf("%s: the sweep does not end where nothing is cut", name)
	}
}

// Generated from the redaction rules: for each built-in rule of package redact, its sample secret (package
// redacttest, one per rule, checked there) is placed at the end of the symptom and the cut swept across it,
// with and without a tally. The cut never lands inside a redaction marker, and when the redaction of the whole
// text would take the note, or text before it, for the value of a secret ("token= *** bytes cut)"), the cut
// backs off until the note stands intact in the final text.
func TestSummaryCutAroundEverySecretGenerated(t *testing.T) {
	t.Parallel()
	for _, s := range redacttest.Samples() {
		t.Run(strconv.Itoa(s.Rule), func(t *testing.T) {
			t.Parallel()
			b, ub := newBuilder(t, reader(t), sources{}, nil), unbounded(t, nil)
			sweepCut(t, b, ub, "no tally", s.Text, s.Secret, nil, "")
			sweepCut(t, b, ub, "tally", s.Text, s.Secret, bothTally(), bothBlock)
		})
	}
}

// The two cuts proven on 2026-11-16 (#796): a cut right after "token=", whose note the redaction of the whole
// text took for the token ("token= *** bytes cut)"), and a cut inside the marker of an Authorization header,
// which the redaction took with its note ("Authorization: [credential]").
func TestSummaryCutNextToASecret(t *testing.T) {
	b, ub := newBuilder(t, reader(t), sources{}, nil), unbounded(t, nil)
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N"
	for name, c := range map[string]struct{ text, secret string }{
		"token":         {"é token=" + strings.Repeat("A", 40), strings.Repeat("A", 40)},
		"authorization": {"é Authorization: Bearer " + jwt, jwt},
	} {
		sweepCut(t, b, ub, name, c.text, c.secret, bothTally(), bothBlock)
		sweepCut(t, b, ub, name+"/no tally", c.text, c.secret, nil, "")
	}
}

// Cluster text that ends in something shaped like a cut note is shown as it is: it cannot hide a real cut,
// since a real cut appends its own note after it.
func TestSummaryForgedCutNote(t *testing.T) {
	b := newBuilder(t, reader(t), sources{}, nil)
	inc := cause(bothTally()...)
	inc.Spec.Symptom = "container app failed (5 bytes cut)"
	if sum := b.summaryBlock(inc); !strings.Contains(sum.Text, "\nsymptom: container app failed (5 bytes cut)\n") ||
		hasLabel(sum, LabelSectionCut) {
		t.Errorf("a symptom shaped like a cut was changed:\n%s", sum.Text)
	}
	inc.Spec.Symptom = strings.Repeat("é", 500) + " (5 bytes cut)"
	inc.Spec.ReportingController = strings.Repeat("é", 253)
	sum := withSummary(b, MinSectionBytes).summaryBlock(inc)
	if cut := checkTallyShown(t, "forged", wantSummary(t, unbounded(t, nil), inc), sum, bothBlock, MinSectionBytes); !slices.Contains(cut, "symptom: ") {
		t.Errorf("the symptom ending in a forged note was not cut with a note of its own:\n%s", sum.Text)
	}
}
