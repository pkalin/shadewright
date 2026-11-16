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
	"testing"
	"time"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analyzer/collector"
)

// The tests of the answer the collector's hold of a consequence waits for (ADR-0039 point 7, #828).

// loadedAt is a controller with no API server whose Incidents count as loaded, at t0 on its own clock.
func loadedAt(t *testing.T) *Controller {
	t.Helper()
	c, _ := newAt(t)
	close(c.ready)
	return c
}

// TestCauseCountedIsWhatConsumeLinks runs generated consequences against controllers that consumed generated
// causes, and checks that CauseCounted answers true exactly when consume then counts the consequence as evidence
// on its cause's Incident, and false exactly when consume holds it or opens it on its own: the collector releases
// a held consequence only when the controller will link it.
func TestCauseCountedIsWhatConsumeLinks(t *testing.T) {
	rng := rand.New(rand.NewPCG(828, 6)) // #nosec G404 -- a seeded corpus, reproducible by design
	answers := map[bool]int{}
	for i := range 400 {
		causes := []int{1, 3, 4}         // cause fingerprints apart from the consequence's own, fp(heldF)
		consumed := causes[:rng.IntN(4)] // the causes consumed, of container web
		o := heldOnce(causes[rng.IntN(3)], 1+int64(rng.IntN(4)), t0.Add(time.Minute))
		switch rng.IntN(6) {
		case 0:
			o.Subject = subjectWith(otherSubjectUID)
		case 1:
			o.Container = "Not_A_Label"
		case 2:
			o.CauseFingerprint = ""
		case 3:
			o.Class = collector.ClassNone
		}
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			c := loadedAt(t)
			for _, f := range consumed {
				cause := causeOf(f, t0)
				cause.Occurrence = terminationID(f)
				c.consume(t.Context(), cause)
			}
			got, err := c.CauseCounted(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			answers[got]++
			c.consume(t.Context(), o)
			n, ids := int64(0), []string(nil)
			if o.CauseFingerprint != "" {
				n, ids = absorbedOn(c, o.CauseFingerprint)
			}
			linked := n == o.Count && slices.Equal(ids, []string{o.Occurrence})
			if got != linked {
				t.Fatalf("CauseCounted = %v, but consume linked it: %v (absorbed %d %v); observation %+v",
					got, linked, n, ids, o)
			}
		})
	}
	if answers[true] == 0 || answers[false] == 0 {
		t.Fatalf("the corpus misses an answer: %v", answers)
	}
}

// TestCauseCountedWaitsForTheIncidents: before the Incidents are loaded there is no answer, only an error at the
// deadline, so the collector asks again instead of releasing or keeping a hold on a guess.
func TestCauseCountedWaitsForTheIncidents(t *testing.T) {
	c, _ := newAt(t)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.CauseCounted(ctx, heldOnce(1, 1, t0)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CauseCounted before the load = %v, want a deadline error", err)
	}
}

// TestCauseCountedAfterRestart: after a restart the answer comes from the stored Incidents, so the hold the
// collector rebuilds by its replay is released for a cause whose Incident was opened before the restart.
func TestCauseCountedAfterRestart(t *testing.T) {
	ns := namespace(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	r := start(t, ns, Config{Window: 100 * time.Millisecond})
	r.obs <- oomCause(1, at)
	eventually(t, ns, 1, func(inc *remediationv1alpha1.Incident) bool { return inc.Status.Cause })
	r.stop()

	r = start(t, ns, Config{Window: 100 * time.Millisecond})
	o := heldOnce(1, 1, at.Add(time.Minute))
	if got, err := r.CauseCounted(t.Context(), o); err != nil || !got {
		t.Fatalf("CauseCounted = %v, %v after a restart; want true", got, err)
	}
	o.CauseFingerprint = fp(3)
	if got, err := r.CauseCounted(t.Context(), o); err != nil || got {
		t.Fatalf("CauseCounted = %v, %v for a cause with no Incident; want false", got, err)
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
				if got, err := c.CauseCounted(t.Context(), o); err != nil || !got {
					t.Fatalf("CauseCounted = %v, %v once the cause is consumed", got, err)
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
