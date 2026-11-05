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
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer/collector"
)

// sightingOf is a Warning event observation of fingerprint fp(f), occurrence eventID(i), seen at at: not
// immediate, so it waits for a second sighting.
func sightingOf(f, i int, at time.Time) collector.Observation {
	o := observation(f, i, 1, 1, at)
	o.Immediate = false
	return o
}

// fakeClock is a clock the test moves by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func occurrencesOf(obs []collector.Observation) []string {
	ids := make([]string, 0, len(obs))
	for _, o := range obs {
		ids = append(ids, o.Occurrence)
	}
	return ids
}

func TestSightingOpensOnlyWhenSeenAgainInTheWindow(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		after time.Duration
		open  bool
	}{
		{"at once", 0, false},
		{"just before the least time", RecurAfter - time.Second, false},
		{"at the least time", RecurAfter, true},
		{"in the window", 3 * time.Minute, true},
		{"at the most time", RecurWithin, true},
		{"after the most time", RecurWithin + time.Second, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSightings(10)
			if got := s.admit(sightingOf(1, 1, t0), t0); got != nil {
				t.Fatalf("the first sighting opened %v", occurrencesOf(got))
			}
			got := s.admit(sightingOf(1, 2, t0.Add(c.after)), t0.Add(c.after))
			if !c.open {
				if got != nil {
					t.Fatalf("a second sighting %s later opened %v", c.after, occurrencesOf(got))
				}
				return
			}
			if len(got) != 2 || got[0].Occurrence != eventID(1) || got[1].Occurrence != eventID(2) {
				t.Fatalf("a second sighting %s later opened %v, want both sightings in order", c.after,
					occurrencesOf(got))
			}
			if s.len() != 0 {
				t.Errorf("%d first sightings held after the Incident opened, want none", s.len())
			}
		})
	}
}

func TestEarlySightingKeepsWaitingAndIsCounted(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	s := newSightings(10)
	s.admit(sightingOf(1, 1, t0), t0)
	if got := s.admit(sightingOf(1, 2, t0.Add(time.Minute)), t0.Add(time.Minute)); got != nil {
		t.Fatalf("a sighting 1 min after the first opened %v", occurrencesOf(got))
	}
	// The window runs from the first sighting, not from the early one.
	at := t0.Add(RecurAfter)
	if got := s.admit(sightingOf(1, 3, at), at); len(got) != 3 {
		t.Fatalf("a sighting 2 min after the first opened %v, want all three", occurrencesOf(got))
	}
}

func TestLateSightingStartsOver(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	s := newSightings(10)
	s.admit(sightingOf(1, 1, t0), t0)
	late := t0.Add(RecurWithin + time.Minute)
	if got := s.admit(sightingOf(1, 2, late), late); got != nil {
		t.Fatalf("a sighting after the window opened %v", occurrencesOf(got))
	}
	// It is the new first sighting: the old one is not counted.
	again := late.Add(RecurAfter)
	if got := s.admit(sightingOf(1, 3, again), again); len(got) != 2 || got[0].Occurrence != eventID(2) {
		t.Fatalf("a sighting 2 min after the new first one opened %v, want the new first one and itself",
			occurrencesOf(got))
	}
}

func TestSightingsAreBoundedUnderAFlood(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	const capacity = 64
	s := newSightings(capacity)
	for f := range 100 * capacity {
		s.admit(sightingOf(f, f, t0), t0)
		if s.len() > capacity {
			t.Fatalf("%d first sightings held, want at most %d", s.len(), capacity)
		}
	}
	if got := testutil.ToFloat64(firstSightingsHeld); got != capacity {
		t.Errorf("shadewright_incident_first_sightings_held = %v, want %d", got, capacity)
	}
	// The oldest first sightings went first: the newest of the flood is still held.
	if _, ok := s.items[fp(100*capacity-1)]; !ok {
		t.Error("the newest first sighting was dropped")
	}
	// The observations held for one fingerprint are bounded as well.
	for i := range 10 * maxHeldPerFingerprint {
		s.admit(sightingOf(0, i, t0), t0)
	}
	if got := s.admit(sightingOf(0, -1, t0.Add(RecurAfter)), t0.Add(RecurAfter)); len(got) != maxHeldPerFingerprint+1 {
		t.Errorf("opened with %d observations, want %d: the %d held and the one that opened it", len(got),
			maxHeldPerFingerprint+1, maxHeldPerFingerprint)
	}
	s.expire(t0.Add(RecurWithin + time.Second))
	if s.len() != 0 {
		t.Errorf("%d first sightings held after the window, want none", s.len())
	}
	if got := testutil.ToFloat64(firstSightingsHeld); got != 0 {
		t.Errorf("shadewright_incident_first_sightings_held = %v after the window, want 0", got)
	}
}

func TestRecurs(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		total       int64
		began, last time.Time
		want        bool
	}{
		{"a single fresh sighting", 1, t0, t0, false},
		{"a series counted twice within seconds", 2, t0, t0.Add(10 * time.Second), false},
		{"a series spanning the least time", 2, t0, t0.Add(RecurAfter), true},
		{"repeated just before the least time", 1, t0, t0.Add(RecurAfter - time.Second), false},
		{"repeated at the least time", 1, t0, t0.Add(RecurAfter), true},
		{"no begin time", 1, time.Time{}, t0, false},
	}
	for _, c := range cases {
		o := sightingOf(1, 1, c.last)
		o.Total, o.Began = c.total, c.began
		if got := recurs(o); got != c.want {
			t.Errorf("%s: recurs = %v, want %v", c.name, got, c.want)
		}
	}
}

// Copies of an occurrence already held are merged into it, so they neither take the slots of new
// occurrences nor count twice, and the total held for it is the highest.
func TestHeldOccurrencesAreMergedByID(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	s := newSightings(10)
	for range 10 {
		s.admit(sightingOf(1, 1, t0), t0)
	}
	grown := sightingOf(1, 1, t0.Add(time.Second))
	grown.Total = 3
	s.admit(grown, t0.Add(time.Second))
	if got, ok := s.total(eventID(1)); !ok || got != 3 {
		t.Fatalf("total held for the occurrence = %d, %v; want 3, true", got, ok)
	}
	for i := 2; i <= maxHeldPerFingerprint; i++ {
		s.admit(sightingOf(1, i, t0), t0)
	}
	got := s.admit(sightingOf(1, 99, t0.Add(RecurAfter)), t0.Add(RecurAfter))
	if len(got) != maxHeldPerFingerprint+1 {
		t.Fatalf("opened %v, want the %d occurrences held and the one that opened it", occurrencesOf(got),
			maxHeldPerFingerprint)
	}
	if got[0].Total != 3 || got[0].Count != 11 {
		t.Errorf("the merged occurrence has total %d, count %d; want 3, 11", got[0].Total, got[0].Count)
	}
	if _, ok := s.total(eventID(1)); ok {
		t.Error("a total is still held for an occurrence whose Incident opened")
	}
}

func TestOneOffWarningsOpenNothing(t *testing.T) {
	ns := namespace(t)
	t0 := time.Now().Truncate(time.Second)
	clock := &fakeClock{now: t0}
	r := start(t, ns, Config{Now: clock.Now})
	// The four one-off Warnings of the v0.1.0 lab's healthy baseline (#267), each seen once.
	for f := range 4 {
		r.obs <- sightingOf(f+1, f+1, t0)
	}
	waitHeld(t, r, "four first sightings held", func() bool { return r.sightings.len() == 4 })
	clock.set(t0.Add(RecurWithin + time.Second))
	waitHeld(t, r, "the first sightings forgotten", func() bool { return r.sightings.len() == 0 })
	var list remediationv1alpha1.IncidentList
	if err := env.admin.List(t.Context(), &list, client.InNamespace(ns)); err != nil || len(list.Items) != 0 {
		t.Fatalf("%d Incidents (%v), want none", len(list.Items), err)
	}
	// A sighting after the window starts over: still nothing.
	r.obs <- sightingOf(1, 9, t0.Add(RecurWithin+time.Second))
	waitHeld(t, r, "the late sighting held", func() bool { return r.sightings.len() == 1 })
	if err := env.admin.List(t.Context(), &list, client.InNamespace(ns)); err != nil || len(list.Items) != 0 {
		t.Fatalf("%d Incidents (%v) after a late sighting, want none", len(list.Items), err)
	}
}

func TestSecondSightingOpensOneIncidentWithBothCounted(t *testing.T) {
	ns := namespace(t)
	t0 := time.Now().Truncate(time.Second)
	clock := &fakeClock{now: t0}
	r := start(t, ns, Config{Now: clock.Now})
	r.obs <- sightingOf(1, 1, t0)
	// Seen again within 2 min: keeps waiting.
	clock.set(t0.Add(time.Minute))
	r.obs <- sightingOf(1, 2, t0.Add(time.Minute))
	waitHeld(t, r, "two sightings held", func() bool {
		e := r.sightings.items[fp(1)]
		return e != nil && len(e.Value.(*sighting).held) == 2
	})
	var list remediationv1alpha1.IncidentList
	if err := env.admin.List(t.Context(), &list, client.InNamespace(ns)); err != nil || len(list.Items) != 0 {
		t.Fatalf("%d Incidents (%v) before 2 min, want none", len(list.Items), err)
	}
	clock.set(t0.Add(3 * time.Minute))
	r.obs <- sightingOf(1, 3, t0.Add(3*time.Minute))
	inc := eventually(t, ns, 1, countIs(3))
	if len(inc.Status.Occurrences) != 3 {
		t.Errorf("%d occurrence records, want 3", len(inc.Status.Occurrences))
	}
	if !inc.Status.FirstSeen.Time.Equal(t0) {
		t.Errorf("firstSeen %v, want the first sighting %v", inc.Status.FirstSeen.Time, t0)
	}
	if err := env.admin.List(t.Context(), &list, client.InNamespace(ns)); err != nil || len(list.Items) != 1 {
		t.Fatalf("%d Incidents (%v), want one", len(list.Items), err)
	}
	// Once it exists, every sighting counts at once.
	clock.set(t0.Add(30 * time.Minute))
	r.obs <- sightingOf(1, 4, t0.Add(30*time.Minute))
	eventually(t, ns, 1, countIs(4))
}

func TestOOMKilledOpensAtOnce(t *testing.T) {
	ns := namespace(t)
	t0 := time.Now().Truncate(time.Second)
	clock := &fakeClock{now: t0}
	r := start(t, ns, Config{Now: clock.Now})
	o := sightingOf(1, 1, t0)
	o.Reason, o.Symptom, o.Immediate = collector.ReasonOOMKilled, "Container worker terminated with exit code 137", true
	r.obs <- o
	eventually(t, ns, 1, countIs(1))
	r.mu.Lock()
	n := r.sightings.len()
	r.mu.Unlock()
	if n != 0 {
		t.Errorf("%d first sightings held, want none", n)
	}
}

// A KEDA ceiling (the HPA's ScalingLimited state, from the KEDA module's symptom source) is a settled,
// lasting state the collector emits once while it lasts, so it is Immediate and opens its Incident at once,
// while a one-off Warning event still waits for a second sighting.
func TestKedaCeilingOpensAtOnceWhileAWarningWaits(t *testing.T) {
	ns := namespace(t)
	t0 := time.Now().Truncate(time.Second)
	clock := &fakeClock{now: t0}
	r := start(t, ns, Config{Now: clock.Now})
	ceiling := sightingOf(1, 1, t0)
	ceiling.Subject = remediationv1alpha1.ObjectRef{APIVersion: "keda.sh/v1alpha1", Kind: "ScaledObject",
		Namespace: ceiling.Subject.Namespace, Name: ceiling.Subject.Name, UID: "55555555-5555-4555-8555-555555555555"}
	ceiling.Reason, ceiling.ReportingController = "ScalingLimited", "horizontal-pod-autoscaler"
	ceiling.Symptom = "The HPA is at its maximum replica count"
	ceiling.Occurrence = "condition:" + strings.Repeat("a", 64)
	ceiling.Immediate = true
	r.obs <- ceiling
	r.obs <- sightingOf(2, 2, t0)
	eventually(t, ns, 1, countIs(1))
	waitHeld(t, r, "the Warning held as a first sighting", func() bool { return r.sightings.len() == 1 })
	inc := &remediationv1alpha1.Incident{}
	err := env.admin.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: Name(fp(2))}, inc)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("the one-off Warning opened an Incident (%v), want none yet", err)
	}
}

// An events.k8s.io series refreshed about every 30 minutes (kube-scheduler's FailedScheduling) is never
// seen twice within RecurWithin, and a replay after a restart arrives all at once: each opens its Incident
// at once, because the observation itself proves that the symptom recurs.
func TestObservationThatProvesRecurrenceOpensAtOnce(t *testing.T) {
	ns := namespace(t)
	t0 := time.Now().Truncate(time.Second)
	r := start(t, ns, Config{Now: (&fakeClock{now: t0}).Now})
	series := sightingOf(1, 1, t0)
	series.Total, series.Count, series.Began = 2, 1, t0.Add(-RecurAfter)
	r.obs <- series
	eventually(t, ns, 1, countIs(1))
	lasting := sightingOf(2, 2, t0)
	lasting.Began = t0.Add(-RecurAfter)
	r.obs <- lasting
	eventually(t, ns, 2, countIs(1))
	// A single, fresh sighting still waits, and so does a series repeated within seconds.
	burst := sightingOf(4, 4, t0)
	burst.Total, burst.Began = 2, t0.Add(-10*time.Second)
	r.obs <- burst
	waitHeld(t, r, "the burst held", func() bool { return r.sightings.len() == 1 })
	r.obs <- sightingOf(3, 3, t0)
	waitHeld(t, r, "the fresh sighting held", func() bool { return r.sightings.len() == 2 })
}

// An observation that opens its Incident at once counts the sightings held for its fingerprint too, and
// they are forgotten.
func TestOpeningAtOnceCountsTheHeldSightings(t *testing.T) {
	for _, c := range []struct {
		name string
		open func(collector.Observation) collector.Observation
	}{
		{"a series", func(o collector.Observation) collector.Observation {
			o.Total, o.Count, o.Began = 2, 2, o.ObservedAt.Add(-RecurAfter)
			return o
		}},
		{"an immediate one", immediate},
	} {
		t.Run(c.name, func(t *testing.T) {
			ns := namespace(t)
			t0 := time.Now().Truncate(time.Second)
			clock := &fakeClock{now: t0}
			r := start(t, ns, Config{Now: clock.Now})
			r.obs <- sightingOf(1, 1, t0)
			r.obs <- sightingOf(1, 2, t0.Add(time.Second))
			waitHeld(t, r, "two sightings held", func() bool {
				e := r.sightings.items[fp(1)]
				return e != nil && len(e.Value.(*sighting).held) == 2
			})
			o := c.open(sightingOf(1, 3, t0.Add(30*time.Second)))
			clock.set(t0.Add(30 * time.Second))
			r.obs <- o
			inc := eventually(t, ns, 1, countIs(2+o.Count))
			if !inc.Status.FirstSeen.Time.Equal(t0) {
				t.Errorf("firstSeen %v, want the first sighting %v", inc.Status.FirstSeen.Time, t0)
			}
			waitHeld(t, r, "the sightings forgotten", func() bool { return r.sightings.len() == 0 })
			if got := testutil.ToFloat64(firstSightingsHeld); got != 0 {
				t.Errorf("shadewright_incident_first_sightings_held = %v, want 0", got)
			}
		})
	}
}

// Handled answers for an occurrence held as a first sighting, so a copy the collector sends again (after it
// dropped its state, MayRepeat) is not held or counted twice.
func TestHandledAnswersForHeldOccurrences(t *testing.T) {
	ns := namespace(t)
	t0 := time.Now().Truncate(time.Second)
	clock := &fakeClock{now: t0}
	r := start(t, ns, Config{Now: clock.Now})
	held := sightingOf(1, 1, t0)
	held.Began = time.Time{} // so only what is held can answer, not a drop mark
	r.obs <- held
	waitHeld(t, r, "the sighting held", func() bool { return r.sightings.len() == 1 })
	if got, found, err := r.Handled(t.Context(), eventID(1), time.Time{}); err != nil || !found || got != 1 {
		t.Fatalf("Handled for a held occurrence = %d, %v, %v; want 1, true", got, found, err)
	}
	again := held
	again.Count, again.MayRepeat = 0, true
	for range maxHeldPerFingerprint {
		r.obs <- again
	}
	// The copies took no room: the other occurrences seen before RecurAfter are held and counted.
	for i := 2; i <= maxHeldPerFingerprint; i++ {
		r.obs <- sightingOf(1, i, t0)
	}
	clock.set(t0.Add(RecurAfter))
	r.obs <- sightingOf(1, 99, t0.Add(RecurAfter))
	// Wait for every batch to be written, however slow the writes, then check that no copy adds to it.
	eventually(t, ns, 1, countIs(maxHeldPerFingerprint+1))
	time.Sleep(300 * time.Millisecond)
	if inc := get(t, ns); inc.Status.Count != maxHeldPerFingerprint+1 {
		t.Fatalf("count %d, want %d: every occurrence once", inc.Status.Count, maxHeldPerFingerprint+1)
	}
}

// The sightings that open an Incident are counted under the caps: the cap on Incidents refuses each of them,
// and the cap on occurrences held unwritten leaves the ones over it uncounted.
func TestOpeningSightingsAreCountedUnderTheCaps(t *testing.T) {
	t.Run("the cap on Incidents", func(t *testing.T) {
		ns := namespace(t)
		t0 := time.Now().Truncate(time.Second)
		clock := &fakeClock{now: t0}
		r := start(t, ns, Config{Now: clock.Now, MaxIncidents: 1})
		r.obs <- immediate(sightingOf(1, 1, t0))
		eventually(t, ns, 1, countIs(1))
		before := testutil.ToFloat64(refusedObservations)
		r.obs <- sightingOf(2, 2, t0)
		r.obs <- sightingOf(2, 3, t0)
		clock.set(t0.Add(RecurAfter))
		r.obs <- sightingOf(2, 4, t0.Add(RecurAfter))
		waitHeld(t, r, "the sightings refused", func() bool { return testutil.ToFloat64(refusedObservations)-before == 3 })
		if n := r.sightings.len(); n != 0 {
			t.Errorf("%d first sightings held after the refusal, want none", n)
		}
	})
	t.Run("the caps on occurrences held unwritten", func(t *testing.T) {
		ns := namespace(t)
		t0 := time.Now().Truncate(time.Second)
		clock := &fakeClock{now: t0}
		r := start(t, ns, Config{Now: clock.Now, Window: time.Hour, MaxPendingOccurrencesPerIncident: 2,
			MaxPendingOccurrencesPerNamespace: 2})
		// Another Incident's first write holds one occurrence of the namespace until the window ends.
		r.obs <- immediate(sightingOf(9, 9, t0))
		eventually(t, ns, 9, countIs(1))
		r.obs <- immediate(sightingOf(9, 10, t0))
		before := testutil.ToFloat64(overflowedOccurrences)
		for i := 1; i <= maxHeldPerFingerprint; i++ {
			r.obs <- sightingOf(1, i, t0)
		}
		series := sightingOf(1, 5, t0)
		series.Total, series.Count, series.Began = 2, 2, t0.Add(-RecurAfter)
		r.obs <- series
		eventually(t, ns, 1, countIs(1))
		// The first occurrence is counted whatever the share; the rest are over the namespace's share.
		if got := testutil.ToFloat64(overflowedOccurrences) - before; got != maxHeldPerFingerprint {
			t.Errorf("overflowed metric grew by %v, want %d", got, maxHeldPerFingerprint)
		}
	})
}

// A fingerprint refused at the cap on Incidents is refused again as each observation arrives, instead of
// being held as a first sighting again.
func TestRefusedFingerprintIsNotHeldAgain(t *testing.T) {
	ns := namespace(t)
	t0 := time.Now().Truncate(time.Second)
	clock := &fakeClock{now: t0}
	r := start(t, ns, Config{Now: clock.Now, MaxIncidents: 1})
	r.obs <- immediate(sightingOf(1, 1, t0))
	eventually(t, ns, 1, countIs(1))
	before := testutil.ToFloat64(refusedObservations)
	r.obs <- immediate(sightingOf(2, 2, t0))
	waitHeld(t, r, "the first refusal", func() bool { return testutil.ToFloat64(refusedObservations)-before == 1 })
	r.obs <- sightingOf(2, 3, t0)
	waitHeld(t, r, "the second refusal", func() bool { return testutil.ToFloat64(refusedObservations)-before == 2 })
	if n := r.sightings.len(); n != 0 {
		t.Errorf("%d first sightings held for a refused fingerprint, want none", n)
	}
}

// The write that opens an Incident from the sightings held is recognised when its answer was lost, so the
// retry does not count them twice.
func TestLostAnswerOfAnOpeningWriteIsNotCountedTwice(t *testing.T) {
	ns := namespace(t)
	t0 := time.Now().Truncate(time.Second)
	clock := &fakeClock{now: t0}
	r := start(t, ns, Config{Now: clock.Now, Window: 200 * time.Millisecond, RetryAfter: 100 * time.Millisecond})
	var lost atomic.Bool
	r.writes.after = func(err error) error {
		if err == nil && lost.CompareAndSwap(false, true) {
			return context.DeadlineExceeded
		}
		return err
	}
	r.obs <- sightingOf(1, 1, t0)
	r.obs <- sightingOf(1, 2, t0)
	clock.set(t0.Add(RecurAfter))
	r.obs <- sightingOf(1, 3, t0.Add(RecurAfter))
	eventually(t, ns, 1, countIs(3))
	// Let the batch that went back be tried again: it is recognised as written.
	for range 10 {
		clock.set(clock.Now().Add(time.Second))
		time.Sleep(100 * time.Millisecond)
	}
	if inc := get(t, ns); inc.Status.Count != 3 {
		t.Fatalf("count %d after a lost answer, want 3", inc.Status.Count)
	}
	if !lost.Load() || r.writes.updates.Load() != 1 {
		t.Fatalf("lost %v, %d status writes: want the retry to find the batch applied without writing",
			lost.Load(), r.writes.updates.Load())
	}
	r.stop()
	if r.pending[fp(1)] != nil || r.failed[fp(1)] != nil {
		t.Fatal("the batch is still pending after the retry")
	}
}
