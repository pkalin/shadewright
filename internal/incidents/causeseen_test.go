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

// TestCauseSeenCorpus runs generated consequences against controllers that consumed generated causes, opened at
// once (Immediate) or held by the gate as a first sighting, and checks that CauseSeen answers true exactly when
// an observation of the consequence's cause fingerprint for its subject was consumed, and false for a
// consequence with no cause or of another subject UID. A container name that is not a DNS label is answered
// true: consume could never link it, so the collector has nothing to wait for.
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
			// Another subject UID: the cause's Incident, or its first sighting, is of the web subject, and the
			// consequence would not be counted on it (decided 2026-11-17, second, on #828).
			o.Subject, want = subjectWith(otherSubjectUID), false
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

// causeState is one generated state of the controller's list for the cause fingerprint fp(heldCauseF), built by
// what consume and noteState record, and the consequence asked about.
type causeState struct {
	opened   bool          // a cause observation was consumed at once (Immediate): its Incident is counted
	gated    bool          // a cause observation was held by the gate as a first sighting
	causeUID string        // the subject UID of the cause observations
	resolved time.Duration // > 0: the Incident is Resolved this long before t0
	unnamed  bool          // the name is not in the controller's list (the Incident was deleted)
	o        collector.Observation
}

func genCauseState(rng *rand.Rand) causeState {
	uids := []string{podSubjectUID, otherSubjectUID}
	st := causeState{
		opened: rng.IntN(3) > 0, gated: rng.IntN(3) == 0, causeUID: uids[rng.IntN(2)],
		unnamed: rng.IntN(8) == 0,
	}
	if rng.IntN(2) == 0 {
		// Around RecurrenceWindow on both sides, to the second, and far past it (#828: 48 hours).
		ages := []time.Duration{time.Minute, RecurrenceWindow - time.Second, RecurrenceWindow,
			RecurrenceWindow + time.Second, 48 * time.Hour}
		st.resolved = ages[rng.IntN(len(ages))]
	}
	st.o = heldOnce(heldCauseF, 1+int64(rng.IntN(3)), t0.Add(time.Duration(rng.IntN(3))*time.Second))
	st.o.Subject = subjectWith(uids[rng.IntN(2)])
	if rng.IntN(6) == 0 {
		st.o.Subject.UID = ""
	}
	return st
}

// build makes the state on a loaded controller.
func (st causeState) build(t *testing.T) *Controller {
	t.Helper()
	c := loadedAt(t)
	name := c.nameOf(fp(heldCauseF))
	if st.opened {
		cause := causeOf(heldCauseF, t0)
		cause.Subject = subjectWith(st.causeUID)
		c.consume(t.Context(), cause)
		if _, ok := c.causes[name]; !ok {
			t.Fatal("the cause opened no Incident, so the case proves nothing")
		}
	}
	if st.gated {
		cause := causeOf(heldCauseF, t0)
		cause.Occurrence, cause.Immediate, cause.Subject = terminationID(9), false, subjectWith(st.causeUID)
		if st.opened {
			// A cause of a fingerprint whose Incident exists is counted there, never held; hold it directly.
			c.sightings.admit(cause, t0)
		} else {
			c.consume(t.Context(), cause)
		}
		if !c.sightings.holds(fp(heldCauseF)) {
			t.Fatal("the cause is not held as a first sighting, so the case proves nothing")
		}
	}
	if st.resolved > 0 {
		c.resolvedAt[name] = t0.Add(-st.resolved)
	}
	if st.unnamed {
		delete(c.names, name)
	}
	return c
}

// TestCauseSeenIsTheLink runs generated states of a cause (opened or not, held by the gate or not, of one subject
// UID or another, Resolved at ages around RecurrenceWindow, its name forgotten) and a consequence of one subject
// UID or another, and requires that the answer to the collector's hold and the link that counts the consequence
// agree (decided 2026-11-17, second, on #828): CauseSeen is true exactly when causeFor links the consequence, or
// a first sighting of the cause for its subject UID is held, which links it once that Incident opens. On the
// first build CauseSeen answered true for an Incident Resolved 48 hours ago or of another subject.
func TestCauseSeenIsTheLink(t *testing.T) {
	rng := rand.New(rand.NewPCG(828, 17)) // #nosec G404 -- a seeded corpus, reproducible by design
	type pair struct{ seen, linked bool }
	answers := map[pair]int{}
	for i := range 600 {
		st := genCauseState(rng)
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			c := st.build(t)
			got, err := c.CauseSeen(t.Context(), st.o)
			if err != nil {
				t.Fatal(err)
			}
			c.mu.Lock()
			_, _, _, linked := c.causeFor(st.o)
			gate := c.sightings.holdsCause(st.o.CauseFingerprint, st.o.Subject.UID)
			c.mu.Unlock()
			answers[pair{got, linked}]++
			if want := linked || gate; got != want {
				t.Fatalf("CauseSeen = %v, causeFor links %v, first sighting held for the subject %v; state %+v",
					got, linked, gate, st)
			}
			// Known by construction, apart from either function.
			uidOK := st.o.Subject.UID != "" && string(st.o.Subject.UID) == st.causeUID
			at := st.o.ObservedAt // on the controller's clock, at t0, a later time is taken as now (notFuture)
			if at.After(t0) {
				at = t0
			}
			window := st.resolved == 0 || at.Sub(t0.Add(-st.resolved)) <= RecurrenceWindow
			if want := uidOK && ((st.opened && !st.unnamed && window) || st.gated); got != want {
				t.Fatalf("CauseSeen = %v, want %v by construction; state %+v", got, want, st)
			}
		})
	}
	for _, p := range []pair{{true, true}, {false, false}, {true, false}} {
		if answers[p] == 0 {
			t.Fatalf("the corpus misses a case %+v: %v", p, answers)
		}
	}
	if answers[pair{false, true}] != 0 {
		t.Fatalf("CauseSeen answered false for a consequence causeFor links: %v", answers)
	}
}

// TestCauseResolvedLongAgoIsNotSeen is the case of #828's second decision of 2026-11-17: the cause's Incident was
// Resolved 48 hours ago and still exists. The consequence of a crash that comes back is not counted on it, so the
// hold waits: CauseSeen answers false, and a new observation of the cause, which opens the next occurrence or is
// held by the gate, makes it seen and tells the collector.
func TestCauseResolvedLongAgoIsNotSeen(t *testing.T) {
	for _, immediate := range []bool{true, false} {
		t.Run(fmt.Sprintf("immediate%v", immediate), func(t *testing.T) {
			c, clock := newAt(t)
			close(c.ready)
			var told []string
			c.OnCauseSeen(func(f string) { told = append(told, f) })
			// The cause's Incident as a listing shows it: stored, a cause, Resolved at t0.
			name := Name(fp(heldCauseF))
			c.names[name] = struct{}{}
			c.causes[name] = causeIncident{spec: specOf(causeOf(heldCauseF, t0))}
			c.resolvedAt[name] = t0
			clock.set(t0.Add(48 * time.Hour))
			o := heldOnce(heldCauseF, 1, clock.Now())
			if got, err := c.CauseSeen(t.Context(), o); err != nil || got {
				t.Fatalf("CauseSeen = %v, %v for a cause Resolved 48 hours ago; want false", got, err)
			}
			again := causeOf(heldCauseF, clock.Now())
			again.Occurrence, again.Immediate = terminationID(2), immediate
			c.handle(t.Context(), again)
			if len(told) != 1 {
				t.Fatalf("told %v, want the cause seen once it came back", told)
			}
			if got, err := c.CauseSeen(t.Context(), o); err != nil || !got {
				t.Fatalf("CauseSeen = %v, %v once the cause came back; want true", got, err)
			}
		})
	}
}
