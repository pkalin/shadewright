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

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
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
