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

package collector

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

// The waiting causes of the tests of a consequence held for its cause (ADR-0039 point 7). Both end in BackOff
// or are a pull error, so a BackOff event about the container is a consequence of them.
const (
	causePull    = "ImagePullBackOff"
	causePullErr = "ErrImagePull"
)

// causeBook stands for the Incident controller's answer to CauseCounted: the cause fingerprints whose
// observation it has consumed. err, when set, is returned instead of an answer.
type causeBook struct {
	mu      sync.Mutex
	counted map[string]bool
	asked   int
	err     error
}

func (b *causeBook) fn(_ context.Context, o Observation) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.asked++
	if b.err != nil {
		return false, b.err
	}
	return b.counted[o.CauseFingerprint], nil
}

func (b *causeBook) count(fp string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.counted == nil {
		b.counted = map[string]bool{}
	}
	b.counted[fp] = true
}

func (b *causeBook) fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

// causePod is the web pod whose app container waits with the cause reason, failing for a BackOff consequence.
func causePod(reason string) *corev1.Pod {
	p := holdPod(reasonBackOff, sampleFailing, 0)
	p.Status.ContainerStatuses[0].State.Waiting.Reason = reason
	return p
}

// causeHoldHarness starts a harness whose CauseCounted is book, with the web workload, its pod waiting in
// ImagePullBackOff, and the BackOff event ev-hold about the app container.
func causeHoldHarness(t *testing.T) (*harness, *causeBook) {
	t.Helper()
	book := &causeBook{}
	cfg := Config{CauseCounted: book.fn}
	d, rs, _ := workload()
	return newHarness(t, cfg, d, rs, causePod(causePull), consequence("ev-hold", reasonBackOff, appPath, t0)), book
}

// setWaiting writes the waiting reason of the app container into the stored web pod; empty runs it, ready.
func (h *harness) setWaiting(reason string) {
	h.t.Helper()
	cur := readPod(h.t, h, webPod)
	if reason == "" {
		cur.Status = holdPod(reasonBackOff, sampleFine, 0).Status
	} else {
		cur.Status = causePod(reason).Status
	}
	h.update(cur)
}

// causeShown is the cause fingerprint the pod of ev-hold shows now, as the collector derives it.
func (h *harness) causeShown() string {
	h.t.Helper()
	ev := &eventsv1.Event{}
	if err := h.cl.Get(h.t.Context(), reqFor("ev-hold").NamespacedName, ev); err != nil {
		h.t.Fatal(err)
	}
	obs, _, _, err := h.c.fromEvent(h.t.Context(), ev)
	if err != nil {
		h.t.Fatal(err)
	}
	return obs.CauseFingerprint
}

// TestConsequenceHeldUntilItsCauseIsCounted (ADR-0039 point 7, #828): a consequence whose pod shows a cause that
// has no Incident yet is held, asked again every CausePoll, and emitted with its cause, once, as soon as the cause
// is counted.
func TestConsequenceHeldUntilItsCauseIsCounted(t *testing.T) {
	h, book := causeHoldHarness(t)
	fp := h.causeShown()
	if fp == "" {
		t.Fatal("the pod shows no cause, so the test proves nothing")
	}
	before := holdOutcomeCounts()
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != CausePoll {
		t.Fatalf("requeue after %s, want the cause asked again after %s", res.RequeueAfter, CausePoll)
	}
	h.none()
	h.clock.add(CausePoll)
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != CausePoll {
		t.Fatalf("requeue after %s at the second ask", res.RequeueAfter)
	}
	h.none()
	book.count(fp)
	h.clock.add(CausePoll)
	if res := h.reconcileEvent("ev-hold"); res != (ctrl.Result{}) {
		t.Fatalf("returned %+v, want the observation delivered", res)
	}
	if o := h.one(); o.CauseFingerprint != fp || o.Count != 1 || o.Class != ClassConsequence {
		t.Fatalf("emitted %+v, want it with its cause, counted once", o)
	}
	if h.c.holds.len() != 0 {
		t.Fatal("the hold is kept")
	}
	wantOutcomes(t, before, map[string]float64{holdReleased: 1})
	// Handled: a later reconcile of the same total emits nothing.
	h.reconcileEvent("ev-hold")
	h.none()
}

// TestCausePollKeepsTheEndOfTheHold: the requeue while a hold waits for its cause is CausePoll, or less when the
// end of ConsequenceGrace comes sooner, never past it.
func TestCausePollKeepsTheEndOfTheHold(t *testing.T) {
	h, _ := causeHoldHarness(t)
	h.reconcileEvent("ev-hold")
	h.clock.add(ConsequenceGrace - 3*time.Second)
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != 3*time.Second {
		t.Fatalf("requeue after %s, want the 3s left of the hold", res.RequeueAfter)
	}
}

// TestCauseCountedFirstIsNotHeld: a cause already counted when the consequence is first seen emits it at once.
func TestCauseCountedFirstIsNotHeld(t *testing.T) {
	h, book := causeHoldHarness(t)
	book.count(h.causeShown())
	before := holdOutcomeCounts()
	if res := h.reconcileEvent("ev-hold"); res != (ctrl.Result{}) {
		t.Fatalf("held: %+v", res)
	}
	if o := h.one(); o.CauseFingerprint == "" {
		t.Fatalf("emitted %+v without its cause", o)
	}
	wantOutcomes(t, before, map[string]float64{})
}

// TestCauseNeverCountedGoesToTheGateWithIt: at the end of the hold, a cause that still has no Incident sends the
// consequence on with that cause, for the gate (and for the first sightings of the Incident controller, which
// count it on the cause's Incident if that opens later). Nothing is dropped.
func TestCauseNeverCountedGoesToTheGateWithIt(t *testing.T) {
	h, _ := causeHoldHarness(t)
	fp := h.causeShown()
	before := holdOutcomeCounts()
	h.reconcileEvent("ev-hold")
	h.clock.add(ConsequenceGrace)
	h.reconcileEvent("ev-hold")
	if o := h.one(); o.CauseFingerprint != fp || o.Count != 1 {
		t.Fatalf("emitted %+v, want it with its cause", o)
	}
	wantOutcomes(t, before, map[string]float64{holdUncaused: 1})
	if h.c.holds.len() != 0 {
		t.Fatal("the hold is kept")
	}
}

// TestCauseHoldOrderCorpus runs generated orders of the two observations through the event collector, each with
// its answer known by construction: the cause counted before the consequence is first seen (cause first), right
// after its first sight (both at once), at a later sample (consequence first), or never. The samples are made by
// requeues and by updates of the pod in turn, at random steps of the clock. Whatever the order, the consequence is
// emitted exactly once, with its cause and its count; at the first sample once the cause is counted, or at the end
// of ConsequenceGrace when it never is; and the outcome counted says which.
func TestCauseHoldOrderCorpus(t *testing.T) {
	rng := rand.New(rand.NewPCG(828, 7)) // #nosec G404 -- a seeded corpus, not a secret
	const (
		causeFirst = -1
		bothAtOnce = 0
		never      = 1 << 30
	)
	seen := map[string]int{}
	for i := range 300 {
		countAt := []int{causeFirst, bothAtOnce, 1 + rng.IntN(14), never}[i%4]
		t.Run(fmt.Sprintf("%d-countAt%d", i, countAt), func(t *testing.T) {
			h, book := causeHoldHarness(t)
			fp := h.causeShown()
			before := holdOutcomeCounts()
			if countAt == causeFirst {
				book.count(fp)
			}
			start := h.clock.Now()
			var got []Observation
			emittedAt, wantAt := -1, -1
			for step := 0; step < 64 && emittedAt < 0; step++ {
				if step > 0 {
					h.clock.add(time.Duration(1+rng.IntN(int(CausePoll/time.Second))) * time.Second)
				}
				// The sample a step expects to emit: the first once the cause was counted at an earlier step,
				// or the first at or past the end of the hold.
				if wantAt < 0 && (countAt < step || !h.clock.Now().Before(start.Add(ConsequenceGrace))) {
					wantAt = step
				}
				if step > 0 && rng.IntN(2) == 0 {
					for _, req := range h.c.heldEventsOf(t.Context(), readPod(t, h, webPod)) {
						h.reconcileEvent(req.Name)
					}
				} else {
					h.reconcileEvent("ev-hold")
				}
				if step == countAt {
					book.count(fp)
				}
				if out := h.drain(); len(out) > 0 {
					got, emittedAt = append(got, out...), step
				}
			}
			if len(got) != 1 || got[0].CauseFingerprint != fp || got[0].Count != 1 {
				t.Fatalf("emitted %+v, want one observation with its cause and a count of 1", got)
			}
			if emittedAt != wantAt {
				t.Fatalf("emitted at step %d, want %d", emittedAt, wantAt)
			}
			// Released when the cause was counted at a step before the emission; a cause counted first holds
			// nothing; else the end of the hold sent it on with its cause.
			want := map[string]float64{}
			switch {
			case countAt == causeFirst:
			case countAt < emittedAt:
				want[holdReleased] = 1
			default:
				want[holdUncaused] = 1
			}
			wantOutcomes(t, before, want)
			if h.c.holds.len() != 0 {
				t.Fatalf("%d held after the emission", h.c.holds.len())
			}
			for o := range want {
				seen[o]++
			}
		})
	}
	if seen[holdReleased] == 0 || seen[holdUncaused] == 0 {
		t.Fatalf("the corpus misses an outcome: %v", seen)
	}
}

// TestCauseUnansweredDecidesNothing: an ask that runs out of its deadline is no answer. The hold stays, the
// event is read again after retryAfter, and the end of the hold does not emit while there is no answer; any
// other error goes to the reconciler's backoff. Once the answer comes, the consequence is emitted once.
func TestCauseUnansweredDecidesNothing(t *testing.T) {
	h, book := causeHoldHarness(t)
	fp := h.causeShown()
	h.reconcileEvent("ev-hold")
	book.fail(fmt.Errorf("waiting for the Incidents to load: %w", context.DeadlineExceeded))
	h.clock.add(ConsequenceGrace)
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != retryAfter {
		t.Fatalf("requeue after %s, want %s", res.RequeueAfter, retryAfter)
	}
	h.none()
	if h.c.holds.len() != 1 {
		t.Fatal("the hold is gone without an answer")
	}
	book.fail(errors.New("synthetic: the Incident controller failed"))
	if _, err := h.reconcileEventErr("ev-hold"); err == nil {
		t.Fatal("an error of CauseCounted was not returned")
	}
	h.none()
	book.fail(nil)
	book.count(fp)
	h.reconcileEvent("ev-hold")
	if o := h.one(); o.CauseFingerprint != fp {
		t.Fatalf("emitted %+v", o)
	}
}

// TestCauseHoldBoundEmitsAtOnce: a consequence waiting for its cause takes a place under the same bound as one
// with no cause; with no room it is emitted at once with its cause, counted as an overflow, never dropped.
func TestCauseHoldBoundEmitsAtOnce(t *testing.T) {
	book := &causeBook{}
	d, rs, _ := workload()
	// A total of 4 gives a namespace share of 1.
	h := newHarness(t, Config{MaxHeldConsequences: 4, CauseCounted: book.fn}, d, rs, causePod(causePull),
		consequence("ev-0", reasonBackOff, appPath, t0), consequence("ev-1", reasonBackOff, appPath, t0))
	before := holdOutcomeCounts()
	if res := h.reconcileEvent("ev-0"); res.RequeueAfter != CausePoll {
		t.Fatalf("ev-0 not held: %+v", res)
	}
	if res := h.reconcileEvent("ev-1"); res != (ctrl.Result{}) {
		t.Fatalf("ev-1 held over the bound: %+v", res)
	}
	if o := h.one(); o.Occurrence != "event:ev-ev-1" || o.CauseFingerprint == "" {
		t.Fatalf("emitted %+v, want ev-1 with its cause", o)
	}
	wantOutcomes(t, before, map[string]float64{holdOverflow: 1})
}

// TestCauseHoldPodGoneEmitsWithTheCause: the pod of a consequence held for its cause is deleted or replaced: the
// consequence is emitted with the cause the last sample showed, for the gate, not dropped (point 7).
func TestCauseHoldPodGoneEmitsWithTheCause(t *testing.T) {
	for _, s := range []sample{sampleGone, sampleReplaced} {
		t.Run(s.String(), func(t *testing.T) {
			h, _ := causeHoldHarness(t)
			fp := h.causeShown()
			before := holdOutcomeCounts()
			h.reconcileEvent("ev-hold")
			cur := readPod(t, h, webPod)
			if err := h.cl.Delete(t.Context(), cur); err != nil {
				t.Fatal(err)
			}
			if s == sampleReplaced {
				p := causePod(causePull)
				p.UID = "another-pod"
				h.create(p)
			}
			h.clock.add(time.Second)
			h.reconcileEvent("ev-hold")
			if o := h.one(); o.CauseFingerprint != fp || o.Container != containerApp || o.Class != ClassConsequence {
				t.Fatalf("emitted %+v, want it with the cause and container of the last sample", o)
			}
			wantOutcomes(t, before, map[string]float64{holdUncaused: 1})
			if h.c.holds.len() != 0 {
				t.Fatal("the hold is kept")
			}
		})
	}
}

// TestNoCauseHoldWaitsForACauseThatAppears: a consequence held with no cause whose pod then shows a cause with no
// Incident goes on waiting for that cause, within the same hold, instead of being emitted at once.
func TestNoCauseHoldWaitsForACauseThatAppears(t *testing.T) {
	book := &causeBook{}
	d, rs, _ := workload()
	h := newHarness(t, Config{CauseCounted: book.fn}, d, rs, holdPod(reasonBackOff, sampleFailing, 0),
		consequence("ev-hold", reasonBackOff, appPath, t0))
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != ConsequenceGrace {
		t.Fatalf("not held with no cause: %+v", res)
	}
	h.setWaiting(causePull)
	h.clock.add(time.Second)
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != CausePoll {
		t.Fatalf("requeue after %s, want the hold to wait for the cause", res.RequeueAfter)
	}
	h.none()
	book.count(h.causeShown())
	h.clock.add(time.Second)
	h.reconcileEvent("ev-hold")
	if o := h.one(); o.CauseFingerprint == "" {
		t.Fatalf("emitted %+v", o)
	}
}

// restartBetween holds ev-hold for its cause (the pod waits in ImagePullBackOff, no Incident), then restarts the
// collector, as a restart of the analyzer does between the consequence and its cause: a new collector, whose
// first reconcile is now, with the same CauseCounted. change runs between the two, on the cluster.
func restartBetween(t *testing.T, change func(h *harness)) (*harness, *causeBook) {
	t.Helper()
	h, book := causeHoldHarness(t)
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != CausePoll {
		t.Fatalf("not held for its cause before the restart: %+v", res)
	}
	h.clock.add(30 * time.Second)
	change(h)
	h.init(Config{CauseCounted: book.fn}, h.cl)
	h.clock.add(time.Second)
	return h, book
}

// TestRestartRebuildsTheCauseHold: the hold is memory, and the replay of the event after a restart rebuilds it.
// The consequence is held again from the new process's first sight, and emitted, marked MayRepeat, once its cause
// is counted.
func TestRestartRebuildsTheCauseHold(t *testing.T) {
	h, book := restartBetween(t, func(*harness) {})
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != CausePoll {
		t.Fatalf("not held again after the restart: %+v", res)
	}
	h.none()
	fp := h.causeShown()
	book.count(fp)
	h.clock.add(CausePoll)
	h.reconcileEvent("ev-hold")
	if o := h.one(); o.CauseFingerprint != fp || !o.MayRepeat {
		t.Fatalf("emitted %+v, want it with its cause, marked MayRepeat", o)
	}
}

// TestRestartEventGone is the first case of the known limit of 2026-11-16 (#820): the Event is gone from the API
// server when the collector replays. Nothing is left to replay, and nobody can count the consequence: this case
// remains, with or without the hold.
func TestRestartEventGone(t *testing.T) {
	h, _ := restartBetween(t, func(h *harness) {
		ev := &eventsv1.Event{}
		if err := h.cl.Get(h.t.Context(), reqFor("ev-hold").NamespacedName, ev); err != nil {
			h.t.Fatal(err)
		}
		if err := h.cl.Delete(h.t.Context(), ev); err != nil {
			h.t.Fatal(err)
		}
	})
	if res := h.reconcileEvent("ev-hold"); res != (ctrl.Result{}) {
		t.Fatalf("returned %+v", res)
	}
	h.none()
	if h.c.holds.len() != 0 {
		t.Fatal("a hold for an event that is gone")
	}
}

// TestRestartCauseChanged is the second case: the cause fingerprint changed at the replay (the pod now shows
// ErrImagePull, the kubelet's other state of the same failing pull). Before #828 the replay emitted the
// consequence at once with the new cause, and the gate held it for that cause alone. The rebuilt hold samples the
// pod for up to ConsequenceGrace and asks for the cause each sample shows: it is covered when, within the hold, the
// pod shows a cause whose Incident exists, the old one or the new one; otherwise it goes to the gate with the
// cause its last sample showed, as before.
func TestRestartCauseChanged(t *testing.T) {
	for _, tc := range []struct {
		name      string
		countLast bool // count the cause shown at the end (ImagePullBackOff again), not the one of the replay
	}{
		{"the new cause opens", false},
		{"the old cause opens", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var old string
			h, book := restartBetween(t, func(h *harness) {
				old = h.causeShown()
				h.setWaiting(causePullErr)
			})
			changed := h.causeShown()
			if changed == "" || changed == old {
				t.Fatalf("the cause did not change (%q, %q), so the test proves nothing", old, changed)
			}
			if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != CausePoll {
				t.Fatalf("emitted at once at the replay, with the changed cause: %+v", res)
			}
			h.none()
			want := changed
			if tc.countLast {
				h.setWaiting(causePull)
				want = old
			}
			book.count(want)
			h.clock.add(CausePoll)
			for _, req := range h.c.heldEventsOf(t.Context(), readPod(t, h, webPod)) {
				h.reconcileEvent(req.Name)
			}
			if o := h.one(); o.CauseFingerprint != want {
				t.Fatalf("emitted %+v, want it with the cause whose Incident exists", o)
			}
		})
	}
}

// TestRestartCauseNoLongerShown is the third case: the pod no longer shows the cause at the replay. The container
// recovered: the consequence is a consequence with no cause, judged by point 19, and opens nothing when the
// container stays fine. The pod was replaced: the event names a pod that is gone, so it has no container and is
// emitted at once, without a cause, for the gate. Both remain as the limit of 2026-11-16 says: nothing persisted
// tells the new process which cause the pod showed.
func TestRestartCauseNoLongerShown(t *testing.T) {
	t.Run("recovered", func(t *testing.T) {
		h, _ := restartBetween(t, func(h *harness) { h.setWaiting("") })
		before := holdOutcomeCounts()
		if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != ConsequenceGrace {
			t.Fatalf("not held as a consequence with no cause: %+v", res)
		}
		h.clock.add(ConsequenceGrace)
		h.reconcileEvent("ev-hold")
		h.none()
		wantOutcomes(t, before, map[string]float64{holdNotOpened: 1})
	})
	t.Run("replaced", func(t *testing.T) {
		h, _ := restartBetween(t, func(h *harness) {
			if err := h.cl.Delete(h.t.Context(), readPod(h.t, h, webPod)); err != nil {
				h.t.Fatal(err)
			}
			p := causePod(causePull)
			p.UID = types.UID("another-pod")
			h.create(p)
		})
		if res := h.reconcileEvent("ev-hold"); res != (ctrl.Result{}) {
			t.Fatalf("returned %+v", res)
		}
		if o := h.one(); o.CauseFingerprint != "" || o.Class != ClassNone {
			t.Fatalf("emitted %+v, want it without a cause", o)
		}
	})
}
