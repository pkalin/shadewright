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
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// The tests of the answer the collector's hold of a consequence waits for (ADR-0039 point 7, #828).

// loadedAt is a controller with no API server whose Incidents count as loaded, at t0 on its own clock.
func loadedAt(t *testing.T) *Controller {
	t.Helper()
	c, _ := newAt(t)
	close(c.ready)
	return c
}

// TestCauseSeenCorpus runs generated consequences against controllers that consumed generated causes, opened at
// once (Immediate) or held by the gate as a first sighting, and checks that CauseSeen answers true exactly when
// an observation of the consequence's cause fingerprint was consumed, whatever became of it, and false for a
// consequence with no cause. A container name that is not a DNS label is answered true: consume could never link
// it, so the collector has nothing to wait for.
func TestCauseSeenCorpus(t *testing.T) {
	rng := rand.New(rand.NewPCG(828, 6)) // #nosec G404 -- a seeded corpus, reproducible by design
	answers := map[bool]int{}
	for i := range 400 {
		causes := []int{1, 3, 4}         // cause fingerprints apart from the consequence's own, fp(heldF)
		consumed := causes[:rng.IntN(4)] // the causes consumed, of container web
		immediate := rng.IntN(2) == 0
		cf := causes[rng.IntN(3)]
		o := heldOnce(cf, 1+int64(rng.IntN(4)), t0.Add(time.Minute))
		want := slices.Contains(consumed, cf)
		switch rng.IntN(5) {
		case 0:
			o.CauseFingerprint, want = "", false
		case 1:
			o.Container, want = "Not_A_Label", true
		case 2:
			o.Subject = subjectWith(otherSubjectUID) // the fingerprint decides, not the subject
		}
		t.Run(fmt.Sprintf("%d-immediate%v", i, immediate), func(t *testing.T) {
			c := loadedAt(t)
			for _, f := range consumed {
				cause := causeOf(f, t0)
				cause.Occurrence, cause.Immediate = terminationID(f), immediate
				c.consume(t.Context(), cause)
				if !immediate && !c.sightings.holds(fp(f)) {
					t.Fatalf("the cause fp(%d) is not held as a first sighting, so the case proves nothing", f)
				}
			}
			got, err := c.CauseSeen(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			answers[got]++
			if got != want {
				t.Fatalf("CauseSeen = %v, want %v; consumed %v (immediate %v), observation %+v", got, want,
					consumed, immediate, o)
			}
		})
	}
	if answers[true] == 0 || answers[false] == 0 {
		t.Fatalf("the corpus misses an answer: %v", answers)
	}
}

// TestCauseSeenDNSLabel: the container check of CauseSeen. A consequence whose container name is not a DNS label
// is answered true although its cause was never consumed (consume leaves the name out and never links it); a
// valid name with the same cause is answered false; a valid name of the maximum length is still judged by its
// cause.
func TestCauseSeenDNSLabel(t *testing.T) {
	c := loadedAt(t)
	for _, tc := range []struct {
		container string
		want      bool
	}{
		{"Not_A_Label", true},
		{"-web", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 63), false},
		{foldContainer, false},
		{"", false},
	} {
		o := heldOnce(heldCauseF, 1, t0)
		o.Container = tc.container
		if got, err := c.CauseSeen(t.Context(), o); err != nil || got != tc.want {
			t.Errorf("CauseSeen(container %q) = %v, %v; want %v", tc.container, got, err, tc.want)
		}
	}
}

// TestGateHeldCauseNotifiesAtOnce: a cause that is not Immediate is held by the gate as a first sighting, and
// that is seen: the controller tells the collector at once, after it recorded the cause and with no lock held
// (the callback asks CauseSeen, which takes the lock and must answer true). A consequence, a cause with no
// fingerprint seen, and an observation that is no cause tell nothing.
func TestGateHeldCauseNotifiesAtOnce(t *testing.T) {
	c := loadedAt(t)
	var told []string
	c.OnCauseSeen(func(f string) {
		o := heldOnce(heldCauseF, 1, t0)
		o.CauseFingerprint = f
		seen, err := c.CauseSeen(t.Context(), o)
		if err != nil || !seen {
			t.Errorf("CauseSeen(%s) = %v, %v inside the call; want the cause recorded before it", f, seen, err)
		}
		told = append(told, f)
	})
	c.handle(t.Context(), heldOnce(heldCauseF, 1, t0))
	other := observation(5, 0, 1, 1, t0)
	c.handle(t.Context(), other)
	if len(told) != 0 {
		t.Fatalf("told %v for observations that are no cause", told)
	}
	cause := causeOf(heldCauseF, t0)
	cause.Immediate = false
	c.handle(t.Context(), cause)
	if !c.sightings.holds(fp(heldCauseF)) {
		t.Fatal("the cause is not held as a first sighting, so the case proves nothing")
	}
	if !slices.Equal(told, []string{fp(heldCauseF)}) {
		t.Fatalf("told %v, want the gate-held cause once", told)
	}
}

// TestRefusedCauseNotifiesNothing: a cause refused at the cap is not seen, so it tells nothing, and CauseSeen
// answers false for it: a hold for it ends at its end, with its cause.
func TestRefusedCauseNotifiesNothing(t *testing.T) {
	clock := &fakeClock{now: t0}
	c, err := New(Config{Namespace: "x", Now: clock.Now, MaxIncidents: 1})
	if err != nil {
		t.Fatal(err)
	}
	close(c.ready)
	told := 0
	c.OnCauseSeen(func(string) { told++ })
	c.handle(t.Context(), causeOf(1, t0))
	other := causeOf(heldCauseF, t0)
	other.Occurrence = terminationID(2)
	c.handle(t.Context(), other)
	if !c.refused.has(fp(heldCauseF)) {
		t.Fatal("the second cause was not refused at the cap, so the case proves nothing")
	}
	if told != 1 {
		t.Fatalf("told %d times, want once, for the cause counted", told)
	}
	if got, err := c.CauseSeen(t.Context(), heldOnce(heldCauseF, 1, t0)); err != nil || got {
		t.Fatalf("CauseSeen = %v, %v for a refused cause; want false", got, err)
	}
}

// TestCauseSeenWaitsForTheIncidents: before the Incidents are loaded there is no answer, only an error at the
// deadline, so the collector asks again instead of releasing a hold on a guess.
func TestCauseSeenWaitsForTheIncidents(t *testing.T) {
	c, _ := newAt(t)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.CauseSeen(ctx, heldOnce(1, 1, t0)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CauseSeen before the load = %v, want a deadline error", err)
	}
}

// TestCauseSeenAfterRestart: after a restart the answer comes from the stored Incidents, so the hold the
// collector rebuilds by its replay is released for a cause whose Incident was opened before the restart.
func TestCauseSeenAfterRestart(t *testing.T) {
	ns := namespace(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	r := start(t, ns, Config{Window: 100 * time.Millisecond})
	r.obs <- oomCause(1, at)
	eventually(t, ns, 1, func(inc *remediationv1alpha1.Incident) bool { return inc.Status.Cause })
	r.stop()

	r = start(t, ns, Config{Window: 100 * time.Millisecond})
	o := heldOnce(1, 1, at.Add(time.Minute))
	if got, err := r.CauseSeen(t.Context(), o); err != nil || !got {
		t.Fatalf("CauseSeen = %v, %v after a restart; want true", got, err)
	}
	o.CauseFingerprint = fp(3)
	if got, err := r.CauseSeen(t.Context(), o); err != nil || got {
		t.Fatalf("CauseSeen = %v, %v for a cause with no Incident; want false", got, err)
	}
}

// TestReleasedConsequenceCountedOnce runs generated orders of what the collector sends for one consequence
// occurrence, with the count known by construction: a copy sent before its cause was consumed (emitted at once
// over the hold's bound, or at the end of the hold) and held as a first sighting, the cause, and the consequence
// released by the hold afterwards, with the increase of its series, or marked MayRepeat after a restart. The
// cause's Incident ends with the occurrence's total as its evidence, never more, and the consequence opens
// nothing of its own.
func TestReleasedConsequenceCountedOnce(t *testing.T) {
	rng := rand.New(rand.NewPCG(828, 16)) // #nosec G404 -- a seeded corpus, reproducible by design
	for i := range 300 {
		early := int64(rng.IntN(4))        // the total sent before the cause; 0: none was
		late := early + int64(rng.IntN(4)) // the total at the release; equal to early: the collector sends nothing
		restart := rng.IntN(2) == 0
		t.Run(fmt.Sprintf("%d-early%d-late%d-restart%v", i, early, late, restart), func(t *testing.T) {
			c := loadedAt(t)
			if early > 0 {
				c.consume(t.Context(), heldOnce(1, early, t0))
				if !c.sightings.holds(fp(heldF)) {
					t.Fatal("the early copy is not held, so the case proves nothing")
				}
			}
			c.consume(t.Context(), causeOf(1, t0.Add(time.Second)))
			if late > early || (restart && late > 0) {
				o := heldOnce(1, late, t0)
				o.ObservedAt = t0.Add(2 * time.Second)
				if got, err := c.CauseSeen(t.Context(), o); err != nil || !got {
					t.Fatalf("CauseSeen = %v, %v once the cause is consumed", got, err)
				}
				if o.Count = late - early; restart {
					o.Count, o.MayRepeat = 0, true
				}
				c.consume(t.Context(), o)
			}
			want := max(early, late)
			if n, _ := absorbedOn(c, fp(1)); n != want {
				t.Fatalf("absorbed on the cause = %d, want %d", n, want)
			}
			if r := c.records[eventID(2)]; want > 0 && (r.total != want || r.incident != Name(fp(1))) {
				t.Fatalf("record %+v, want total %d on %s", r, want, Name(fp(1)))
			}
			if _, ok := c.pending[fp(heldF)]; ok || c.sightings.len() != 0 {
				t.Fatalf("the consequence is held or opened on its own: %d first sightings", c.sightings.len())
			}
		})
	}
}
