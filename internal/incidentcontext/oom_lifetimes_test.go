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
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/incidents/causereason"
	"github.com/shadewright-labs/shadewright/internal/redact"
)

func termRec(n int, started, finished time.Time) remediationv1alpha1.OccurrenceRecord {
	r := remediationv1alpha1.OccurrenceRecord{
		ID:    fmt.Sprintf("termination:%032x", n),
		Total: 1,
	}
	if !finished.IsZero() {
		r.Began = ptrTime(finished)
	}
	if !started.IsZero() {
		r.Started = ptrTime(started)
	}
	return r
}

func oomIncident(occs ...remediationv1alpha1.OccurrenceRecord) *remediationv1alpha1.Incident {
	inc := cause()
	inc.Status.Count = max(int64(len(occs)), 1)
	inc.Status.Occurrences = occs
	return inc
}

// TestOOMLifetimesGolden pins the exact summary wording for issue #1005 (decision of 2026-11-21, ADR-0023
// amendments of 2026-11-21 and 2026-11-26): same short age each time, growing ages, more than 5 instances,
// one instance only, one instance with earlier kills lacking a start time, none with a start time (nothing
// claimed, and never computed from the gap between two kills), same-second kill rendered as <1s, negative
// durations and missing times counted as without, and unsorted records ordered oldest first and labelled as
// mixed across pods and revisions.
func TestOOMLifetimesGolden(t *testing.T) {
	b := newBuilder(t, reader(t, webObjects()...), sources{}, nil)
	base := testNow.Add(-time.Hour)

	cases := []struct {
		name     string
		occs     []remediationv1alpha1.OccurrenceRecord
		wantLine string // empty means no age-at-kill line is added
	}{
		{
			name: "same short age each time",
			occs: []remediationv1alpha1.OccurrenceRecord{
				termRec(3, base.Add(6*time.Minute), base.Add(6*time.Minute+13*time.Second)),
				termRec(2, base.Add(3*time.Minute), base.Add(3*time.Minute+14*time.Second)),
				termRec(1, base, base.Add(12*time.Second)),
			},
			wantLine: "age at kill (mixed across pods and revisions, oldest first): 12s, 14s, 13s",
		},
		{
			name: "growing ages",
			occs: []remediationv1alpha1.OccurrenceRecord{
				termRec(4, base.Add(30*time.Minute), base.Add(50*time.Minute)),
				termRec(3, base.Add(15*time.Minute), base.Add(25*time.Minute)),
				termRec(2, base.Add(5*time.Minute), base.Add(10*time.Minute)),
				termRec(1, base, base.Add(2*time.Minute)),
			},
			wantLine: "age at kill (mixed across pods and revisions, oldest first): 2m0s, 5m0s, 10m0s, 20m0s",
		},
		{
			name: "more than five instances keeps last five oldest first",
			occs: []remediationv1alpha1.OccurrenceRecord{
				termRec(7, base.Add(40*time.Minute), base.Add(56*time.Minute)),
				termRec(6, base.Add(30*time.Minute), base.Add(38*time.Minute)),
				termRec(5, base.Add(20*time.Minute), base.Add(24*time.Minute)),
				termRec(4, base.Add(15*time.Minute), base.Add(17*time.Minute)),
				termRec(3, base.Add(10*time.Minute), base.Add(11*time.Minute)),
				termRec(2, base.Add(5*time.Minute), base.Add(5*time.Minute+30*time.Second)),
				termRec(1, base, base.Add(15*time.Second)),
			},
			wantLine: "age at kill (last 5, mixed across pods and revisions, oldest first): 1m0s, 2m0s, 4m0s, 8m0s, 16m0s",
		},
		{
			name: "one instance only (first kill)",
			occs: []remediationv1alpha1.OccurrenceRecord{
				termRec(1, base, base.Add(30*time.Second)),
			},
			wantLine: "age at kill (mixed across pods and revisions, oldest first): 30s",
		},
		{
			name: "one instance with one earlier kill without start time",
			occs: []remediationv1alpha1.OccurrenceRecord{
				termRec(2, base.Add(10*time.Minute), base.Add(10*time.Minute+30*time.Second)),
				termRec(1, time.Time{}, base),
			},
			wantLine: "age at kill (mixed across pods and revisions, oldest first): 30s; 1 other kill without a start time (of at most 32 recorded)",
		},
		{
			name: "one instance with earlier kills from older version",
			occs: []remediationv1alpha1.OccurrenceRecord{
				termRec(4, base.Add(30*time.Minute), base.Add(30*time.Minute+45*time.Second)),
				termRec(3, time.Time{}, base.Add(20*time.Minute)),
				termRec(2, time.Time{}, base.Add(10*time.Minute)),
				termRec(1, time.Time{}, base),
			},
			wantLine: "age at kill (mixed across pods and revisions, oldest first): 45s; 3 other kills without a start time (of at most 32 recorded)",
		},
		{
			name: "none with a start time (older records 10m apart claim nothing)",
			occs: []remediationv1alpha1.OccurrenceRecord{
				termRec(3, time.Time{}, base.Add(20*time.Minute)),
				termRec(2, time.Time{}, base.Add(10*time.Minute)),
				termRec(1, time.Time{}, base),
			},
			wantLine: "",
		},
		{
			name:     "none with a start time (empty occurrences)",
			occs:     nil,
			wantLine: "",
		},
		{
			name: "negative duration only claims nothing",
			occs: []remediationv1alpha1.OccurrenceRecord{
				termRec(1, base.Add(10*time.Minute+5*time.Second), base.Add(10*time.Minute)), // finish before start
			},
			wantLine: "",
		},
		{
			name: "same-second kill only renders as <1s",
			occs: []remediationv1alpha1.OccurrenceRecord{
				termRec(1, base, base),
			},
			wantLine: "age at kill (mixed across pods and revisions, oldest first): <1s",
		},
		{
			name: "same-second kill with negative duration renders <1s and counts negative as without",
			occs: []remediationv1alpha1.OccurrenceRecord{
				termRec(2, base.Add(10*time.Minute+5*time.Second), base.Add(10*time.Minute)), // finish before start
				termRec(1, base, base), // same-second kill
			},
			wantLine: "age at kill (mixed across pods and revisions, oldest first): <1s; 1 other kill without a start time (of at most 32 recorded)",
		},
		{
			name: "same-second kill kept as <1s while negative and missing times count as without",
			occs: []remediationv1alpha1.OccurrenceRecord{
				termRec(5, base.Add(40*time.Minute), base.Add(40*time.Minute+45*time.Second)),
				termRec(4, base.Add(30*time.Minute+5*time.Second), base.Add(30*time.Minute)), // negative
				termRec(3, base.Add(20*time.Minute), base.Add(20*time.Minute)),               // same-second kill
				termRec(2, base.Add(10*time.Minute), time.Time{}),                            // missing finish
				termRec(1, base, base.Add(15*time.Second)),
			},
			wantLine: "age at kill (mixed across pods and revisions, oldest first): 15s, <1s, 45s; 2 other kills without a start time (of at most 32 recorded)",
		},
		{
			name: "unsorted occurrences from interleaved replicas sort oldest first",
			occs: []remediationv1alpha1.OccurrenceRecord{
				termRec(2, base.Add(5*time.Minute), base.Add(8*time.Minute)),
				termRec(1, base, base.Add(20*time.Second)),
				termRec(3, base.Add(6*time.Minute), base.Add(6*time.Minute+25*time.Second)),
			},
			wantLine: "age at kill (mixed across pods and revisions, oldest first): 20s, 25s, 3m0s",
		},
		{
			name: "more than five with start times plus kills without start times",
			occs: []remediationv1alpha1.OccurrenceRecord{
				termRec(8, base.Add(40*time.Minute), base.Add(46*time.Minute)),
				termRec(7, base.Add(35*time.Minute), base.Add(40*time.Minute)),
				termRec(6, base.Add(30*time.Minute), base.Add(34*time.Minute)),
				termRec(5, base.Add(25*time.Minute), base.Add(28*time.Minute)),
				termRec(4, base.Add(20*time.Minute), base.Add(22*time.Minute)),
				termRec(3, base.Add(15*time.Minute), base.Add(16*time.Minute)),
				termRec(2, time.Time{}, base.Add(10*time.Minute)),
				termRec(1, time.Time{}, base),
			},
			wantLine: "age at kill (last 5, mixed across pods and revisions, oldest first): 2m0s, 3m0s, 4m0s, 5m0s, 6m0s; 2 other kills without a start time (of at most 32 recorded)",
		},
	}

	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inc := oomIncident(tc.occs...)
			sum := one(t, build(t, b, inc), SectionSummary)
			bare := oomIncident()
			bare.Status.Count = inc.Status.Count
			bareText := one(t, build(t, b, bare), SectionSummary).Text
			if tc.wantLine == "" {
				if sum.Text != bareText {
					t.Fatalf("summary with no usable start times changed:\n%s\nwant:\n%s", sum.Text, bareText)
				}
				return
			}
			want := bareText + tc.wantLine + "\n"
			if sum.Text != want {
				t.Fatalf("summary:\n%s\nwant:\n%s", sum.Text, want)
			}
			if got := red.String(tc.wantLine); got != tc.wantLine {
				t.Errorf("redactor altered fixed line:\n%q\nto:\n%q", tc.wantLine, got)
			}
		})
	}
}

// Non-OOMKilled Incidents never show the OOM lifetime line, and non-termination records in status.occurrences
// (such as events or waiting records) are neither turned into ages nor counted as kills without a start time.
// Absorbed BackOff/Unhealthy tallies and absorbedOccurrences never produce ages.
func TestOOMLifetimesScopeAndRowExclusions(t *testing.T) {
	b := newBuilder(t, reader(t, webObjects()...), sources{}, nil)
	base := testNow.Add(-time.Hour)
	rec := termRec(1, base, base.Add(30*time.Second))

	for _, reason := range []string{reasonNonZeroExit, string(remediationv1alpha1.AbsorbedBackOff), causereason.ImagePull()[0], "FailedScheduling"} {
		bare := incident(deploymentRef())
		bare.Spec.Reason = reason
		with := bare.DeepCopy()
		with.Status.Occurrences = []remediationv1alpha1.OccurrenceRecord{rec}
		if got, want := one(t, build(t, b, with), SectionSummary).Text, one(t, build(t, b, bare), SectionSummary).Text; got != want {
			t.Errorf("reason %s: summary changed:\n%s\nwant:\n%s", reason, got, want)
		}
	}

	inc := oomIncident(
		rec,
		remediationv1alpha1.OccurrenceRecord{ID: "event:0a1b2c3d-4e5f-4a6b-8c7d-000000000001", Total: 5, Began: ptrTime(base.Add(time.Minute))},
		remediationv1alpha1.OccurrenceRecord{ID: "waiting:" + strings.Repeat("a", 32), Total: 1, Began: ptrTime(base.Add(2 * time.Minute))},
		remediationv1alpha1.OccurrenceRecord{ID: "condition:" + strings.Repeat("b", 32), Total: 1, Began: ptrTime(base.Add(3 * time.Minute))},
	)
	inc.Status.Absorbed = []remediationv1alpha1.AbsorbedTally{
		absorbed(remediationv1alpha1.AbsorbedBackOff, 7, testNow.Add(-time.Minute)),
	}
	inc.Status.AbsorbedOccurrences = []remediationv1alpha1.OccurrenceRecord{
		{ID: "event:0a1b2c3d-4e5f-4a6b-8c7d-000000000002", Total: 7, Began: ptrTime(base.Add(4 * time.Minute))},
	}
	sum := one(t, build(t, b, inc), SectionSummary)
	wantTail := "phase: Open\n" +
		"age at kill (mixed across pods and revisions, oldest first): 30s\n" +
		absorbedHeader + "\n" +
		"  BackOff: 7 occurrences, last seen 2026-11-05T11:59:00Z\n"
	if !strings.HasSuffix(sum.Text, wantTail) {
		t.Fatalf("summary:\n%s\nwant it to end with:\n%s", sum.Text, wantTail)
	}
}

// The OOM lifetime line holds only fixed words, durations and counts: no pod name, container name, image or
// free text, and no part of it is quotable in Block.Data().
func TestOOMLifetimesNotQuotableAndNoFreeText(t *testing.T) {
	objs := webObjects()
	p := objs[2].(*corev1.Pod)
	p.Spec.Containers = []corev1.Container{{Name: appName, Image: "registry.example.test/secret-image:v9"}}
	b := newBuilder(t, reader(t, objs...), sources{}, nil)
	base := testNow.Add(-time.Hour)
	inc := oomIncident(
		termRec(2, base.Add(5*time.Minute), base.Add(5*time.Minute+42*time.Second)),
		termRec(1, time.Time{}, base),
	)
	sum := one(t, build(t, b, inc), SectionSummary)
	var ageLine string
	for l := range strings.SplitSeq(sum.Text, "\n") {
		if strings.HasPrefix(l, "age at kill ") {
			ageLine = l
			break
		}
	}
	if ageLine == "" {
		t.Fatalf("no age at kill line in:\n%s", sum.Text)
	}
	for _, forbidden := range []string{crashPod, appName, "secret-image", "registry.example.test", "termination:"} {
		if strings.Contains(ageLine, forbidden) {
			t.Errorf("age line %q contains %q", ageLine, forbidden)
		}
	}
	for _, part := range sum.Data() {
		if strings.Contains(part, "age at kill") || strings.Contains(part, "42s") || strings.Contains(part, "without a start time") {
			t.Errorf("age at kill line leaked into Block.Data(): %q", part)
		}
	}
}

// Even at MinSectionBytes (1 KiB) with every schema field at its limit, the longest absorbed tally, six
// usable OOM lifetimes (so "last 5, " is included, five at maximum duration and one same-second kill) and 26
// kills without a start time, every summary line, the age-at-kill line and the whole absorbed tally stay
// intact within the bound.
func TestOOMLifetimesFitsAtMinSectionBytesWithTally(t *testing.T) {
	b := newBuilder(t, reader(t), sources{}, nil)
	inc := schemaMax("é", largestTally...)
	inc.Spec.Reason = causereason.OOMKilled
	far := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	epoch := time.Unix(0, 0).UTC()
	occs := make([]remediationv1alpha1.OccurrenceRecord, 0, remediationv1alpha1.MaxOccurrenceRecords)
	for i := range maxOOMLifetimes {
		occs = append(occs, termRec(i+1, epoch, far.Add(-time.Duration(i)*time.Second)))
	}
	// A sixth usable start triggers the longer "last 5, " prefix.
	occs = append(occs, termRec(maxOOMLifetimes+1, epoch, epoch))
	for i := range remediationv1alpha1.MaxOccurrenceRecords - (maxOOMLifetimes + 1) {
		occs = append(occs, termRec(100+i, time.Time{}, far.Add(-time.Duration(i)*time.Minute)))
	}
	inc.Status.Occurrences = occs
	inc.Status.Count = math.MaxInt64

	sum := withSummary(b, MinSectionBytes).summaryBlock(inc)
	if len(sum.Text) > MinSectionBytes {
		t.Fatalf("summary is %d bytes, over MinSectionBytes (%d):\n%s", len(sum.Text), MinSectionBytes, sum.Text)
	}
	for _, want := range []string{
		"\nphase: Escalated\n",
		"\nage at kill (last 5, mixed across pods and revisions, oldest first): ",
		"; 26 other kills without a start time (of at most 32 recorded)\n",
		largestBlock,
	} {
		if !strings.Contains(sum.Text, want) {
			t.Errorf("summary missing %q:\n%s", want, sum.Text)
		}
	}
	if !strings.HasSuffix(sum.Text, largestBlock) {
		t.Errorf("summary does not end with the full absorbed tally:\n%s", sum.Text)
	}
	if !hasLabel(sum, LabelSectionCut) {
		t.Errorf("expected LabelSectionCut on shortened summary")
	}

	// Same-second kill among the last 5 stays intact as <1s at MinSectionBytes too.
	incZero := inc.DeepCopy()
	incZero.Status.Occurrences[0] = termRec(1, far, far)
	sumZero := withSummary(b, MinSectionBytes).summaryBlock(incZero)
	if len(sumZero.Text) > MinSectionBytes {
		t.Fatalf("summary with <1s is %d bytes, over MinSectionBytes (%d):\n%s", len(sumZero.Text), MinSectionBytes, sumZero.Text)
	}
	if !strings.Contains(sumZero.Text, ", <1s; 26 other kills without a start time (of at most 32 recorded)\n") {
		t.Errorf("summary with <1s missing rendered same-second kill:\n%s", sumZero.Text)
	}
}
