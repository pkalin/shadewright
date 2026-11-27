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

package incidents

import (
	"fmt"
	"math"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/analyzer/collector"
	"github.com/shadewright-labs/shadewright/internal/incidents/causereason"
	"github.com/shadewright-labs/shadewright/internal/incidents/fingerprint"
)

var t0 = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

// invalidID is an occurrence ID the Incident schema refuses.
const invalidID = "event:not a uid"

func eventID(i int) string { return fmt.Sprintf("event:0a1b2c3d-4e5f-4a6b-8c7d-%012d", i) }

func batchOf(count int64, occ map[string]counted) *batch {
	held := make(map[string]heldOccurrence, len(occ))
	for id, o := range occ {
		held[id] = heldOccurrence{counted: o}
	}
	return &batch{count: count, first: t0, last: t0.Add(time.Minute), occurrences: held}
}

func TestMergeCountsAndSaturates(t *testing.T) {
	st := &remediationv1alpha1.IncidentStatus{}
	merge(st, batchOf(5, map[string]counted{eventID(1): {total: 5, began: t0}}))
	if st.Count != 5 || st.Phase != remediationv1alpha1.IncidentOpen {
		t.Fatalf("count %d phase %q, want 5 Open", st.Count, st.Phase)
	}
	if !st.FirstSeen.Time.Equal(t0) || !st.LastSeen.Time.Equal(t0.Add(time.Minute)) {
		t.Fatalf("first %v last %v", st.FirstSeen, st.LastSeen)
	}
	st.Count = remediationv1alpha1.MaxIncidentCount - 2
	merge(st, batchOf(1000, map[string]counted{eventID(1): {total: 1005, began: t0}}))
	if st.Count != remediationv1alpha1.MaxIncidentCount {
		t.Fatalf("count %d, want saturation at %d", st.Count, remediationv1alpha1.MaxIncidentCount)
	}
	if len(st.Occurrences) != 1 || st.Occurrences[0].Total != 1005 {
		t.Fatalf("records %+v, want one with total 1005", st.Occurrences)
	}
}

func TestMergeKeepsNewestAndMarksDropped(t *testing.T) {
	st := &remediationv1alpha1.IncidentStatus{}
	occ := map[string]counted{}
	n := remediationv1alpha1.MaxOccurrenceRecords + 8
	for i := range n {
		occ[eventID(i)] = counted{total: 1, began: t0.Add(time.Duration(i) * time.Second)}
	}
	dropped := merge(st, batchOf(int64(n), occ))
	if len(st.Occurrences) != remediationv1alpha1.MaxOccurrenceRecords || len(dropped) != 8 {
		t.Fatalf("kept %d dropped %d", len(st.Occurrences), len(dropped))
	}
	if st.Occurrences[0].ID != eventID(n-1) {
		t.Fatalf("first record %s, want the newest %s", st.Occurrences[0].ID, eventID(n-1))
	}
	if want := t0.Add(7 * time.Second); !st.OccurrencesDroppedThrough.Time.Equal(want) {
		t.Fatalf("dropped through %v, want the newest dropped %v", st.OccurrencesDroppedThrough, want)
	}
	// An older drop never moves the mark back.
	merge(st, batchOf(1, map[string]counted{eventID(1000): {total: 1, began: t0.Add(-time.Hour)}}))
	if want := t0.Add(7 * time.Second); !st.OccurrencesDroppedThrough.Time.Equal(want) {
		t.Fatalf("dropped through moved to %v", st.OccurrencesDroppedThrough)
	}
}

func TestMergeDropsUnknownBeginFirstAndMarksIt(t *testing.T) {
	st := &remediationv1alpha1.IncidentStatus{}
	occ := map[string]counted{"termination:" + fmt.Sprintf("%032x", 1): {total: 1}}
	for i := range remediationv1alpha1.MaxOccurrenceRecords {
		occ[eventID(i)] = counted{total: 1, began: t0}
	}
	dropped := merge(st, batchOf(33, occ))
	if len(dropped) != 1 || dropped[0] != "termination:"+fmt.Sprintf("%032x", 1) {
		t.Fatalf("dropped %v, want the termination without a begin time", dropped)
	}
	if st.OccurrencesDroppedThrough == nil {
		t.Fatal("dropping a record without a begin time must still set the mark")
	}
}

func TestMergeSkipsIDsTheSchemaRefuses(t *testing.T) {
	st := &remediationv1alpha1.IncidentStatus{}
	merge(st, batchOf(2, map[string]counted{invalidID: {total: 1}, eventID(1): {total: 1}}))
	if len(st.Occurrences) != 1 || st.Occurrences[0].ID != eventID(1) || st.Count != 2 {
		t.Fatalf("status %+v, want the bad ID counted but not recorded", st)
	}
}

func TestApplied(t *testing.T) {
	st := &remediationv1alpha1.IncidentStatus{}
	b := batchOf(3, map[string]counted{eventID(1): {total: 3, began: t0}})
	if applied(st, b) {
		t.Fatal("an empty status holds nothing")
	}
	merge(st, b)
	if !applied(st, b) {
		t.Fatal("a merged batch must be recognised")
	}
	if applied(st, batchOf(1, map[string]counted{eventID(1): {total: 4, began: t0}})) {
		t.Fatal("a higher total is new")
	}
	if applied(st, batchOf(1, map[string]counted{invalidID: {total: 1}})) {
		t.Fatal("a batch with only an unrecordable ID cannot be taken as applied")
	}
}

func TestAppliedPerOccurrence(t *testing.T) {
	// More occurrences than an Incident records: the cut ones began no later than the drop mark.
	many := map[string]counted{}
	for i := range remediationv1alpha1.MaxOccurrenceRecords + 1 {
		many[eventID(i)] = counted{total: 1, began: t0.Add(time.Duration(i) * time.Second)}
	}
	// An ID the schema refuses is skipped by merge, and a total over MaxInt32 is stored cut.
	skipped := map[string]counted{invalidID: {total: 1}, eventID(1): {total: 1, began: t0}}
	clamped := map[string]counted{eventID(1): {total: math.MaxInt32 + 10, began: t0}}
	// One without a begin time, dropped first by the cut, next to one that is kept.
	unknown := map[string]counted{"termination:" + fmt.Sprintf("%032x", 1): {total: 1}}
	for i := range remediationv1alpha1.MaxOccurrenceRecords {
		unknown[eventID(i)] = counted{total: 1, began: t0}
	}
	for name, occ := range map[string]map[string]counted{
		"more than the records": many, "an invalid ID": skipped, "a clamped total": clamped, "no begin time": unknown,
	} {
		st := &remediationv1alpha1.IncidentStatus{}
		b := batchOf(int64(len(occ)), occ)
		if applied(st, b) {
			t.Fatalf("%s: an empty status holds nothing", name)
		}
		merge(st, b)
		if !applied(st, b) {
			t.Fatalf("%s: a merged batch must be recognised", name)
		}
	}

	st := &remediationv1alpha1.IncidentStatus{}
	merge(st, batchOf(int64(len(many)), many))
	// An occurrence not recorded that began after the mark was never written.
	if applied(st, batchOf(2, map[string]counted{
		eventID(5): {total: 1, began: t0.Add(5 * time.Second)}, eventID(500): {total: 1, began: t0.Add(time.Hour)},
	})) {
		t.Fatal("an unrecorded occurrence newer than the mark is new")
	}
	// Nothing in the batch can prove it was written.
	if applied(st, batchOf(1, map[string]counted{eventID(600): {total: 1, began: t0.Add(-time.Hour)}})) {
		t.Fatal("a batch only of occurrences older than the mark cannot be taken as applied")
	}
}

func TestBoundedSet(t *testing.T) {
	s := newBoundedSet(2)
	if !s.add("a") || s.add("a") || !s.add("b") || !s.add("c") || !s.add("a") || s.add("c") || !s.add("b") {
		t.Fatal("unexpected membership")
	}
	for i := range 1000 {
		s.add(fmt.Sprint(i))
	}
	if len(s.items) != 2 || len(s.ring) != 2 || cap(s.ring) != 2 {
		t.Fatalf("%d items, ring %d of %d: want both bounded at 2", len(s.items), len(s.ring), cap(s.ring))
	}
}

// TestAppliedChecksAbsorbedRecords: a batch of absorbed consequences is proved written by absorbedOccurrences
// and its own drop mark, not by occurrences, so a write whose answer was lost is not counted twice.
func TestAppliedChecksAbsorbedRecords(t *testing.T) {
	st := &remediationv1alpha1.IncidentStatus{}
	merge(st, batchOf(1, map[string]counted{eventID(1): {total: 1, began: t0}}))
	absorbed := func(total int64) *batch {
		return &batch{
			absorbed: map[remediationv1alpha1.AbsorbedReason]tally{remediationv1alpha1.AbsorbedBackOff: {count: 1, last: t0}},
			occurrences: map[string]heldOccurrence{
				eventID(2): {counted: counted{total: total, began: t0}, absorbed: remediationv1alpha1.AbsorbedBackOff},
			},
		}
	}
	b := absorbed(3)
	if applied(st, b) {
		t.Fatal("an absorbed batch not written yet counts as applied")
	}
	merge(st, b)
	if !applied(st, b) {
		t.Fatal("an absorbed batch written counts as not applied: a lost answer would count it twice")
	}
	if applied(st, absorbed(4)) {
		t.Fatal("a higher total of an absorbed occurrence counts as applied")
	}
	if len(st.Occurrences) != 1 || len(st.AbsorbedOccurrences) != 1 || st.Count != 1 || st.Absorbed[0].Count != 1 {
		t.Fatalf("status %+v: absorbed records mixed with the Incident's own", st)
	}
}

// Issue #1005: merge stores started on termination records only when began is set and started is not after
// began, preserves existing started values, leaves older records without started untouched, and drops started
// from non-termination or invalid records so status writes stay within the CRD schema.
func TestMergeRecordsStartedOnTerminations(t *testing.T) {
	termID := func(i int) string { return fmt.Sprintf("termination:%032x", i) }
	waitID := fmt.Sprintf("waiting:%032x", 1)
	condID := fmt.Sprintf("condition:%032x", 1)

	olderBegan := t0.Add(-5 * time.Minute)
	st := &remediationv1alpha1.IncidentStatus{
		Occurrences: []remediationv1alpha1.OccurrenceRecord{
			// A record written by an older version has no Started.
			{ID: termID(0), Total: 1, Began: timeOf(olderBegan)},
		},
	}

	began1 := t0.Add(-2 * time.Minute)
	started1 := began1.Add(-30 * time.Second)
	began2 := t0.Add(-time.Minute)
	merge(st, batchOf(6, map[string]counted{
		termID(1):  {total: 1, began: began1.Add(500 * time.Millisecond), started: started1.Add(400 * time.Millisecond)},
		termID(2):  {total: 1, began: began2, started: began2},                  // zero duration, allowed by schema
		termID(3):  {total: 1, began: began2, started: began2.Add(time.Second)}, // invalid: started > began
		termID(4):  {total: 1, started: started1},                               // invalid: no began
		eventID(1): {total: 1, began: began1, started: started1},                // non-termination
		waitID:     {total: 1, began: began1, started: started1},                // non-termination
		condID:     {total: 1, began: began1, started: started1},                // non-termination
	}))

	byID := make(map[string]remediationv1alpha1.OccurrenceRecord, len(st.Occurrences))
	for _, r := range st.Occurrences {
		byID[r.ID] = r
	}
	if r := byID[termID(0)]; r.Started != nil {
		t.Errorf("older record gained Started = %v, want nil", r.Started)
	}
	if r := byID[termID(1)]; r.Started == nil || !r.Started.Time.Equal(started1) {
		t.Errorf("valid termination Started = %v, want %v", r.Started, started1)
	}
	if r := byID[termID(2)]; r.Started == nil || !r.Started.Time.Equal(began2) {
		t.Errorf("same-second termination Started = %v, want %v", r.Started, began2)
	}
	for _, id := range []string{termID(3), termID(4), eventID(1), waitID, condID} {
		if r := byID[id]; r.Started != nil {
			t.Errorf("%s stored Started = %v, want nil", id, r.Started)
		}
	}

	// Merging the same termination again without a start time keeps the stored start time.
	merge(st, batchOf(0, map[string]counted{
		termID(1): {total: 1, began: began1},
	}))
	for _, r := range st.Occurrences {
		if r.ID == termID(1) && (r.Started == nil || !r.Started.Time.Equal(started1)) {
			t.Fatalf("existing Started lost on re-merge: %+v", r)
		}
	}
}

// TestConsumeAndFlushPersistsTerminationStarted checks the controller path from collector.Observation to
// stored Incident.Status.Occurrences[].Started.
func TestConsumeAndFlushPersistsTerminationStarted(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&remediationv1alpha1.Incident{}).Build()
	c, err := New(Config{Namespace: fakeNamespace, Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	c.client, c.reader, c.cache = cl, cl, cl
	if err := c.load(t.Context()); err != nil {
		t.Fatal(err)
	}
	started := t0.Add(-90 * time.Second)
	finished := t0.Add(-10 * time.Second)
	obs := collector.Observation{
		Fingerprint:         fp(77),
		Subject:             firstSpec().Subject,
		Source:              firstSpec().Subject.Namespace,
		Reason:              causereason.OOMKilled,
		Type:                "Warning",
		ReportingController: kubeletController,
		Symptom:             "Container app terminated with exit code 137",
		Occurrence:          fmt.Sprintf("termination:%032x", 1),
		Total:               1,
		Began:               finished,
		Started:             started,
		Count:               1,
		ObservedAt:          finished,
		Immediate:           true,
		Origin:              fingerprint.SourcePodStatus,
		Container:           "app",
		Class:               collector.ClassCause,
	}
	c.consume(t.Context(), obs)
	c.flush(t.Context(), fp(77))

	var got remediationv1alpha1.Incident
	if err := cl.Get(t.Context(), client.ObjectKey{Namespace: fakeNamespace, Name: Name(fp(77))}, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.Occurrences) != 1 {
		t.Fatalf("occurrences = %+v, want 1", got.Status.Occurrences)
	}
	r := got.Status.Occurrences[0]
	if r.Began == nil || !r.Began.Time.Equal(finished) || r.Started == nil || !r.Started.Time.Equal(started) {
		t.Fatalf("occurrence = %+v, want Began=%v Started=%v", r, finished, started)
	}
}

// Issue #1005: higher takes started only together with the began it belongs to (same observation, or two
// observations with equal began). A clamped finish time with no started must not later adopt a real started
// from a re-read whose began differs.
func TestHigherPairsStartedWithSameBegan(t *testing.T) {
	clamped := t0
	realFinish := t0.Add(time.Minute)
	realStart := t0.Add(-30 * time.Second)

	cases := []struct {
		name        string
		a, b        counted
		wantBegan   time.Time
		wantStarted time.Time
	}{
		{
			name:        "clamped finish without started followed by real finish with started",
			a:           counted{total: 1, began: clamped},
			b:           counted{total: 1, began: realFinish, started: realStart},
			wantBegan:   clamped,
			wantStarted: time.Time{},
		},
		{
			name:        "same began fills missing started",
			a:           counted{total: 1, began: realFinish},
			b:           counted{total: 1, began: realFinish, started: realStart},
			wantBegan:   realFinish,
			wantStarted: realStart,
		},
		{
			name:        "zero began takes both began and started from b",
			a:           counted{total: 1},
			b:           counted{total: 1, began: realFinish, started: realStart},
			wantBegan:   realFinish,
			wantStarted: realStart,
		},
		{
			name:        "zero began with stray started does not pair stray started with b.began",
			a:           counted{total: 1, started: realStart},
			b:           counted{total: 1, began: clamped},
			wantBegan:   clamped,
			wantStarted: time.Time{},
		},
		{
			name:        "existing began and started kept when b has another began",
			a:           counted{total: 1, began: realFinish, started: realStart},
			b:           counted{total: 2, began: clamped},
			wantBegan:   realFinish,
			wantStarted: realStart,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := higher(tc.a, tc.b)
			if !got.began.Equal(tc.wantBegan) || !got.started.Equal(tc.wantStarted) {
				t.Fatalf("higher(%+v, %+v) = (began=%v, started=%v), want (began=%v, started=%v)",
					tc.a, tc.b, got.began, got.started, tc.wantBegan, tc.wantStarted)
			}
		})
	}

	// End-to-end through merge: a stored record with a clamped finish and no started must not pick up
	// started from a re-read of the same termination ID with a different finish time.
	termID := fmt.Sprintf("termination:%032x", 42)
	st := &remediationv1alpha1.IncidentStatus{
		Occurrences: []remediationv1alpha1.OccurrenceRecord{
			{ID: termID, Total: 1, Began: timeOf(clamped)},
		},
	}
	merge(st, batchOf(0, map[string]counted{
		termID: {total: 1, began: realFinish, started: realStart},
	}))
	if len(st.Occurrences) != 1 || st.Occurrences[0].Started != nil {
		t.Fatalf("merged clamped record gained Started from another observation: %+v", st.Occurrences)
	}
}
