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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// The waiting causes of the tests of a consequence held for its cause (ADR-0039 point 7). Both end in BackOff
// or are a pull error, so a BackOff event about the container is a consequence of them.
const (
	causePull    = "ImagePullBackOff"
	causePullErr = "ErrImagePull"
)

// causeBook stands for the Incident controller: the cause fingerprints it has seen, recorded under its lock
// before it tells the collector (see), and the answer to CauseSeen read under the same lock. err, when set, is
// returned instead of an answer. before, when set, runs inside CauseSeen after the answer is read and before it
// is returned: a call of ReleaseCause that comes between the hold's store and the end of the ask.
type causeBook struct {
	mu     sync.Mutex
	seen   map[string]bool
	asked  int
	err    error
	before func()
	// release is the collector's ReleaseCause, called by see with no lock held.
	release func(string)
}

func (b *causeBook) fn(_ context.Context, o Observation) (bool, error) {
	b.mu.Lock()
	b.asked++
	err, seen, before := b.err, b.seen[o.CauseFingerprint], b.before
	b.before = nil
	b.mu.Unlock()
	if before != nil {
		before()
	}
	if err != nil {
		return false, err
	}
	return seen, nil
}

// record records fp as seen without telling the collector, as the Incidents loaded at a start are.
func (b *causeBook) record(fp string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen == nil {
		b.seen = map[string]bool{}
	}
	b.seen[fp] = true
}

// see records fp, then tells the collector, as incidents.Controller.handle does.
func (b *causeBook) see(fp string) {
	b.record(fp)
	if b.release != nil {
		b.release(fp)
	}
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

// withQueue gives the harness's collector a work queue, as the event controller hands it over when it starts
// (releases), and returns it.
func (h *harness) withQueue() workqueue.TypedRateLimitingInterface[reconcile.Request] {
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	h.t.Cleanup(q.ShutDown)
	if err := h.c.releases().Start(h.t.Context(), q); err != nil {
		h.t.Fatal(err)
	}
	return q
}

// processQueue reconciles every event the queue holds now, as the event controller's worker would, and returns
// how many.
func (h *harness) processQueue(q workqueue.TypedRateLimitingInterface[reconcile.Request]) int {
	h.t.Helper()
	n := 0
	for q.Len() > 0 {
		req, _ := q.Get()
		h.reconcileEvent(req.Name)
		q.Done(req)
		q.Forget(req)
		n++
	}
	return n
}

// causeHoldHarness starts a harness whose CauseSeen is book, with the web workload, its pod waiting in
// ImagePullBackOff, and the BackOff event ev-hold about the app container. book tells the collector through
// ReleaseCause, which adds to the returned queue.
func causeHoldHarness(t *testing.T) (*harness, *causeBook, workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	t.Helper()
	book := &causeBook{}
	d, rs, _ := workload()
	h := newHarness(t, Config{CauseSeen: book.fn}, d, rs, causePod(causePull),
		consequence("ev-hold", reasonBackOff, appPath, t0))
	book.release = h.c.ReleaseCause
	return h, book, h.withQueue()
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
	if obs.CauseFingerprint == "" {
		h.t.Fatal("the pod shows no cause, so the test proves nothing")
	}
	return obs.CauseFingerprint
}

// TestConsequenceHeldUntilItsCauseIsSeen (ADR-0039 point 7, decided 2026-11-17): a consequence whose pod shows a
// cause the Incident controller has not seen is held until the end of ConsequenceGrace, with no poll between,
// and is emitted with its cause, once, as soon as the controller tells the collector it has seen the cause: the
// call adds the event to the queue, and that reconcile emits it.
func TestConsequenceHeldUntilItsCauseIsSeen(t *testing.T) {
	h, book, q := causeHoldHarness(t)
	fp := h.causeShown()
	before := holdOutcomeCounts()
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != ConsequenceGrace {
		t.Fatalf("requeue after %s, want the end of the hold, %s", res.RequeueAfter, ConsequenceGrace)
	}
	h.none()
	h.clock.add(30 * time.Second)
	book.see(fp)
	if n := h.processQueue(q); n != 1 {
		t.Fatalf("the release queued %d reconciles, want 1", n)
	}
	if o := h.one(); o.CauseFingerprint != fp || o.Count != 1 || o.Class != ClassConsequence {
		t.Fatalf("emitted %+v, want it with its cause, counted once", o)
	}
	if h.c.holds.len() != 0 {
		t.Fatal("the hold is kept")
	}
	wantOutcomes(t, before, map[string]float64{holdReleased: 1})
	// Handled: a later reconcile of the same total, the requeue at the end of the hold, emits nothing.
	h.clock.add(ConsequenceGrace)
	h.reconcileEvent("ev-hold")
	h.none()
	// A cause seen again releases nothing more.
	book.see(fp)
	if q.Len() != 0 {
		t.Fatalf("%d queued for a cause with no hold", q.Len())
	}
}

// TestCauseSeenFirstIsNotHeld: a cause the controller has seen before the consequence is first seen emits it at
// once, never held, and counts no outcome.
func TestCauseSeenFirstIsNotHeld(t *testing.T) {
	h, book, _ := causeHoldHarness(t)
	book.see(h.causeShown())
	before := holdOutcomeCounts()
	if res := h.reconcileEvent("ev-hold"); res != (ctrl.Result{}) {
		t.Fatalf("held: %+v", res)
	}
	if o := h.one(); o.CauseFingerprint == "" {
		t.Fatalf("emitted %+v without its cause", o)
	}
	wantOutcomes(t, before, map[string]float64{})
	if h.c.holds.len() != 0 {
		t.Fatal("a hold is kept")
	}
}

// TestCauseNeverSeenEndsWithItsCause: a cause the controller never sees ends the hold at the end of
// ConsequenceGrace, and the consequence is emitted with that cause (cause_unseen), for the gate and the
// controller's first sightings. Nothing is dropped.
func TestCauseNeverSeenEndsWithItsCause(t *testing.T) {
	h, _, _ := causeHoldHarness(t)
	fp := h.causeShown()
	before := holdOutcomeCounts()
	h.reconcileEvent("ev-hold")
	h.clock.add(ConsequenceGrace)
	h.reconcileEvent("ev-hold")
	if o := h.one(); o.CauseFingerprint != fp || o.Count != 1 {
		t.Fatalf("emitted %+v, want it with its cause", o)
	}
	wantOutcomes(t, before, map[string]float64{holdCauseUnseen: 1})
	if h.c.holds.len() != 0 {
		t.Fatal("the hold is kept")
	}
}

// TestCauseHoldOrderCorpus runs generated orders of the two observations through the event collector, each with
// its answer known by construction: the controller sees the cause before the consequence is first seen, at a
// later step, or never, and the pod may stop showing the cause (the container recovers) at a random step. The
// samples are made by updates of the pod, by the release's queue and by the requeue at the end of the hold, at
// random steps of the clock. Whatever the order, the consequence is emitted exactly once, with its cause and its
// count; at the step of the release, or at the end of ConsequenceGrace when there is none; and the outcome
// counted says which.
func TestCauseHoldOrderCorpus(t *testing.T) {
	rng := rand.New(rand.NewPCG(828, 7)) // #nosec G404 -- a seeded corpus, not a secret
	const (
		seenFirst = -1
		never     = 1 << 30
	)
	seen := map[string]int{}
	for i := range 150 {
		seeAt := []int{seenFirst, 1 + rng.IntN(14), never}[i%3]
		recoverAt := []int{never, 1 + rng.IntN(14)}[rng.IntN(2)]
		t.Run(fmt.Sprintf("%d-seeAt%d-recoverAt%d", i, seeAt, recoverAt), func(t *testing.T) {
			h, book, q := causeHoldHarness(t)
			fp := h.causeShown()
			before := holdOutcomeCounts()
			if seeAt == seenFirst {
				book.see(fp)
			}
			end := h.clock.Now().Add(ConsequenceGrace)
			h.reconcileEvent("ev-hold")
			got := h.drain()
			emittedAt, wantAt, endAt := -1, -1, -1
			if len(got) > 0 {
				emittedAt = 0
			}
			if seeAt == seenFirst {
				wantAt = 0
			}
			for step := 1; step < 64 && emittedAt < 0; step++ {
				h.clock.add(time.Duration(1+rng.IntN(15)) * time.Second)
				if step == recoverAt {
					h.setWaiting("")
				}
				switch {
				case !h.clock.Now().Before(end):
					// The requeue at the end of the hold, which comes before a release at the same step.
					if endAt < 0 {
						endAt = step
					}
					h.reconcileEvent("ev-hold")
				case rng.IntN(2) == 0:
					for _, req := range h.c.heldEventsOf(t.Context(), readPod(t, h, webPod)) {
						h.reconcileEvent(req.Name)
					}
				}
				if step == seeAt {
					book.see(fp)
					h.processQueue(q)
				}
				if wantAt < 0 && (step == seeAt || !h.clock.Now().Before(end)) {
					wantAt = step
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
			want := map[string]float64{}
			switch {
			case seeAt == seenFirst:
			case seeAt == emittedAt && endAt < 0:
				want[holdReleased] = 1
			default:
				want[holdCauseUnseen] = 1
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
	if seen[holdReleased] == 0 || seen[holdCauseUnseen] == 0 {
		t.Fatalf("the corpus misses an outcome: %v", seen)
	}
}

// TestReleaseRaceBothOrders proves the order that makes the release race-free (ReleaseCause): the controller
// records the cause before it tells the collector, and the collector stores the hold before it asks. Each order
// is forced: the release before the first sight (the ask sees the cause: emitted at once, never held), and the
// release between the store and the end of the ask, whose answer was read before the cause was recorded (the
// release finds the hold). Then both run on two goroutines at once, many times, under the race detector. In
// every run the consequence is emitted exactly once, with its cause, before the end of the hold.
func TestReleaseRaceBothOrders(t *testing.T) {
	t.Run("release first", func(t *testing.T) {
		h, book, q := causeHoldHarness(t)
		book.see(h.causeShown())
		h.reconcileEvent("ev-hold")
		h.processQueue(q)
		if o := h.one(); o.CauseFingerprint == "" {
			t.Fatalf("emitted %+v", o)
		}
	})
	t.Run("release during the ask", func(t *testing.T) {
		h, book, q := causeHoldHarness(t)
		fp := h.causeShown()
		book.before = func() { book.see(fp) }
		if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != ConsequenceGrace {
			t.Fatalf("the ask saw the cause (%+v), so the case proves nothing", res)
		}
		h.none()
		if n := h.processQueue(q); n != 1 {
			t.Fatalf("the release queued %d reconciles, want 1", n)
		}
		if o := h.one(); o.CauseFingerprint != fp {
			t.Fatalf("emitted %+v", o)
		}
	})
	t.Run("concurrent", func(t *testing.T) {
		for i := range 200 {
			h, book, q := causeHoldHarness(t)
			fp := h.causeShown()
			var wg sync.WaitGroup
			wg.Go(func() { h.reconcileEvent("ev-hold") })
			wg.Go(func() {
				if i%2 == 0 {
					time.Sleep(time.Duration(i%7) * time.Microsecond)
				}
				book.see(fp)
			})
			wg.Wait()
			h.processQueue(q)
			if got := h.drain(); len(got) != 1 || got[0].CauseFingerprint != fp {
				t.Fatalf("run %d: emitted %+v before the end of the hold, want one with its cause", i, got)
			}
			if h.c.holds.len() != 0 {
				t.Fatalf("run %d: %d held", i, h.c.holds.len())
			}
		}
	})
}

// TestReleaseKeptAcrossTheSample: a release that comes between a sample's read of the hold and its store is not
// lost while the sample keeps the cause: the store keeps the mark, and the end of the hold counts it released.
func TestReleaseKeptAcrossTheSample(t *testing.T) {
	h, book, _ := causeHoldHarness(t)
	fp := h.causeShown()
	h.reconcileEvent("ev-hold")
	key := reqFor("ev-hold").NamespacedName
	s, ok := h.c.holds.get(key, "ev-ev-hold")
	if !ok {
		t.Fatal("not held")
	}
	book.see(fp)
	if stored, _ := h.c.holds.put(key, s); stored.releasedFor != fp {
		t.Fatal("the store of a hold read before the release cleared it")
	}
}

// TestReleaseForAnOldCauseIsForgotten (decided 2026-11-17, second, on #828): the hold showed cause A, the
// controller sees A and tells the collector, and a sample of the same moment shows cause B, which the controller
// has not seen. Whatever the order of the release and the sample's read and store of the hold, the release for A
// is forgotten and the hold waits for B: nothing is emitted before the end of the hold, and at its end the
// consequence is emitted with B as cause_unseen. On the first build a release between the read and the store was
// kept, and the consequence left as released with B, a cause the controller had not seen; one before the read
// emitted it with A at once.
func TestReleaseForAnOldCauseIsForgotten(t *testing.T) {
	// changed holds ev-hold for cause A, then has the pod show B, and returns both and the queue.
	changed := func(t *testing.T) (*harness, *causeBook, workqueue.TypedRateLimitingInterface[reconcile.Request],
		string, string) {
		h, book, q := causeHoldHarness(t)
		a := h.causeShown()
		if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != ConsequenceGrace {
			t.Fatalf("not held for its cause: %+v", res)
		}
		h.clock.add(time.Second)
		h.setWaiting(causePullErr)
		b := h.causeShown()
		if a == b {
			t.Fatal("the cause did not change, so the test proves nothing")
		}
		return h, book, q, a, b
	}
	// endsWithB checks that nothing was emitted, then ends the hold and wants it emitted with b, cause_unseen.
	endsWithB := func(t *testing.T, h *harness, q workqueue.TypedRateLimitingInterface[reconcile.Request], b string) {
		t.Helper()
		h.processQueue(q)
		h.none()
		key := reqFor("ev-hold").NamespacedName
		if s, ok := h.c.holds.get(key, "ev-ev-hold"); !ok || s.cause != b || s.releasedFor != "" {
			t.Fatalf("held %+v (%v), want the cause B with no release", s, ok)
		}
		before := holdOutcomeCounts()
		h.clock.add(ConsequenceGrace)
		h.reconcileEvent("ev-hold")
		if o := h.one(); o.CauseFingerprint != b {
			t.Fatalf("emitted %+v at the end of the hold, want it with B", o)
		}
		wantOutcomes(t, before, map[string]float64{holdCauseUnseen: 1})
	}
	t.Run("release between the read and the store", func(t *testing.T) {
		h, book, q, a, b := changed(t)
		key := reqFor("ev-hold").NamespacedName
		s, _ := h.c.holds.get(key, "ev-ev-hold")
		book.see(a)
		ev := &eventsv1.Event{}
		if err := h.cl.Get(t.Context(), key, ev); err != nil {
			t.Fatal(err)
		}
		obs, _, _, err := h.c.fromEvent(t.Context(), ev)
		if err != nil {
			t.Fatal(err)
		}
		s.cause, s.obs = b, obs
		if stored, _ := h.c.holds.put(key, s); stored.releasedFor != "" {
			t.Fatalf("the store of cause B kept the release for A: %+v", stored)
		}
		endsWithB(t, h, q, b)
	})
	t.Run("release before the read", func(t *testing.T) {
		h, book, q, a, b := changed(t)
		book.see(a)
		if res := h.sampleByPod(); res.RequeueAfter <= 0 {
			t.Fatalf("the sample showing B ended the hold: %+v", res)
		}
		endsWithB(t, h, q, b)
	})
	t.Run("release after the store", func(t *testing.T) {
		h, book, q, a, b := changed(t)
		h.sampleByPod()
		book.see(a)
		if n := q.Len(); n != 0 {
			t.Fatalf("the release for A woke %d holds, want none: the hold waits for B", n)
		}
		endsWithB(t, h, q, b)
	})
	t.Run("concurrent", func(t *testing.T) {
		for i := range 200 {
			h, book, q, a, b := changed(t)
			var wg sync.WaitGroup
			wg.Go(func() { h.reconcileEvent("ev-hold") })
			wg.Go(func() {
				if i%2 == 0 {
					time.Sleep(time.Duration(i%7) * time.Microsecond)
				}
				book.see(a)
			})
			wg.Wait()
			endsWithB(t, h, q, b)
		}
	})
}

// TestCauseUnansweredDecidesNothing: an ask that runs out of its deadline is no answer: the hold stays and the
// event is read again after retryAfter. Any other error goes to the reconciler's backoff. The end of the hold
// does not ask: it emits with the cause.
func TestCauseUnansweredDecidesNothing(t *testing.T) {
	h, book, _ := causeHoldHarness(t)
	fp := h.causeShown()
	book.fail(fmt.Errorf("waiting for the Incidents to load: %w", context.DeadlineExceeded))
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != retryAfter {
		t.Fatalf("requeue after %s, want %s", res.RequeueAfter, retryAfter)
	}
	h.none()
	if h.c.holds.len() != 1 {
		t.Fatal("not held without an answer")
	}
	book.fail(errors.New("synthetic: the Incident controller failed"))
	if _, err := h.reconcileEventErr("ev-hold"); err == nil {
		t.Fatal("an error of CauseSeen was not returned")
	}
	h.none()
	before := holdOutcomeCounts()
	h.clock.add(ConsequenceGrace)
	h.reconcileEvent("ev-hold")
	if o := h.one(); o.CauseFingerprint != fp {
		t.Fatalf("emitted %+v", o)
	}
	wantOutcomes(t, before, map[string]float64{holdCauseUnseen: 1})
}

// TestRecoverDuringTheHoldKeepsTheCause: the container recovers during the hold, so the pod no longer shows the
// cause: the hold keeps the last cause seen and emits the consequence with it, at the release or at the end of
// the hold, never judged as a consequence with no cause (#828: the first build dropped it as not_opened).
func TestRecoverDuringTheHoldKeepsTheCause(t *testing.T) {
	for _, release := range []bool{false, true} {
		t.Run(fmt.Sprintf("release%v", release), func(t *testing.T) {
			h, book, q := causeHoldHarness(t)
			fp := h.causeShown()
			before := holdOutcomeCounts()
			h.reconcileEvent("ev-hold")
			h.clock.add(10 * time.Second)
			h.setWaiting("")
			if res := h.sampleByPod(); res.RequeueAfter <= 0 {
				t.Fatalf("a sample of the recovered pod ended the hold: %+v", res)
			}
			h.none()
			want := holdCauseUnseen
			if release {
				book.see(fp)
				h.processQueue(q)
				want = holdReleased
			} else {
				h.clock.add(ConsequenceGrace)
				h.reconcileEvent("ev-hold")
			}
			if o := h.one(); o.CauseFingerprint != fp || o.Container != containerApp || o.Class != ClassConsequence {
				t.Fatalf("emitted %+v, want it with the cause the pod showed", o)
			}
			wantOutcomes(t, before, map[string]float64{want: 1})
		})
	}
}

// TestRetiredTemplateDuringTheHoldKeepsTheCause: a rollout retires the pod's template during the hold (#724),
// with the pod still there or gone: the event now resolves to a retired template, which drops a symptom, but the
// hold emits the consequence with its cause and does not count it as dropped (#828: the first build dropped it).
func TestRetiredTemplateDuringTheHoldKeepsTheCause(t *testing.T) {
	for _, gone := range []bool{false, true} {
		t.Run(fmt.Sprintf("podGone%v", gone), func(t *testing.T) {
			d, rs, nrs, _, _ := rolledOut()
			d.Annotations[revisionAnnotation] = "1"
			book := &causeBook{}
			h := newHarness(t, Config{CauseSeen: book.fn}, d, rs, causePod(causePull),
				consequence("ev-hold", reasonBackOff, appPath, t0))
			fp := h.causeShown()
			before, retired := holdOutcomeCounts(), droppedTotal()
			if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != ConsequenceGrace {
				t.Fatalf("not held: %+v", res)
			}
			h.create(nrs)
			cur := readDeployment(t, h)
			cur.Annotations[revisionAnnotation] = "2"
			h.update(cur)
			old := &appsv1.ReplicaSet{}
			if err := h.cl.Get(t.Context(), client.ObjectKeyFromObject(rs), old); err != nil {
				t.Fatal(err)
			}
			h.update(retire(old))
			if gone {
				if err := h.cl.Delete(t.Context(), readPod(t, h, webPod)); err != nil {
					t.Fatal(err)
				}
			}
			h.clock.add(templateCheckTTL + time.Second)
			if res := h.reconcileEvent("ev-hold"); res != (ctrl.Result{}) {
				t.Fatalf("returned %+v, want the observation delivered", res)
			}
			if o := h.one(); o.CauseFingerprint != fp || o.Count != 1 || o.Class != ClassConsequence {
				t.Fatalf("emitted %+v, want it with its cause", o)
			}
			wantOutcomes(t, before, map[string]float64{holdCauseUnseen: 1})
			if droppedTotal() != retired {
				t.Fatal("counted as evidence of a retired template dropped")
			}
			// The template is retired: a later event of the pod is dropped as before.
			h.reconcileEvent("ev-hold")
			h.none()
		})
	}
}

// TestCauseHoldOtherExitsEmitWithTheCause: an event held with a cause that leaves the collector by another path
// than the hold (here: no longer a Warning) is emitted with its cause, counted cause_unseen, not dropped.
func TestCauseHoldOtherExitsEmitWithTheCause(t *testing.T) {
	h, _, _ := causeHoldHarness(t)
	fp := h.causeShown()
	before := holdOutcomeCounts()
	h.reconcileEvent("ev-hold")
	ev := &eventsv1.Event{}
	if err := h.cl.Get(t.Context(), reqFor("ev-hold").NamespacedName, ev); err != nil {
		t.Fatal(err)
	}
	ev.Type = corev1.EventTypeNormal
	h.update(ev)
	h.reconcileEvent("ev-hold")
	if o := h.one(); o.CauseFingerprint != fp {
		t.Fatalf("emitted %+v, want it with its cause", o)
	}
	wantOutcomes(t, before, map[string]float64{holdCauseUnseen: 1})
}

// TestCauseHoldEventDeletedIsTheOneLoss: the event of a hold with a cause is deleted during the hold. Nothing is
// left to emit: the consequence is lost, counted dropped. It is the one end of a hold with a cause that emits
// nothing (decided 2026-11-17).
func TestCauseHoldEventDeletedIsTheOneLoss(t *testing.T) {
	h, _, _ := causeHoldHarness(t)
	h.causeShown()
	before := holdOutcomeCounts()
	h.reconcileEvent("ev-hold")
	ev := &eventsv1.Event{}
	if err := h.cl.Get(t.Context(), reqFor("ev-hold").NamespacedName, ev); err != nil {
		t.Fatal(err)
	}
	if err := h.cl.Delete(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	h.reconcileEvent("ev-hold")
	h.none()
	wantOutcomes(t, before, map[string]float64{holdDropped: 1})
	if h.c.holds.len() != 0 {
		t.Fatal("the hold is kept")
	}
}

// TestReleasedHoldStandsAcrossADeliveryRetry: a released hold whose delivery finds the channel full emits the
// same observation at the retry, counted released once, and is not held again.
func TestReleasedHoldStandsAcrossADeliveryRetry(t *testing.T) {
	book := &causeBook{}
	d, rs, _ := workload()
	h := newHarness(t, Config{CauseSeen: book.fn, Buffer: 1}, d, rs, causePod(causePull),
		consequence("ev-hold", reasonBackOff, appPath, t0))
	fp := h.causeShown()
	before := holdOutcomeCounts()
	h.reconcileEvent("ev-hold")
	h.c.out <- Observation{Fingerprint: fillerFingerprint}
	book.see(fp)
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != retryAfter {
		t.Fatalf("requeue after %s, want the delivery retry", res.RequeueAfter)
	}
	h.setWaiting("")
	h.drain()
	h.clock.add(retryAfter)
	h.reconcileEvent("ev-hold")
	if o := h.one(); o.CauseFingerprint != fp {
		t.Fatalf("emitted %+v at the retry", o)
	}
	wantOutcomes(t, before, map[string]float64{holdReleased: 1})
}

// TestCauseHoldsHaveTheirOwnShare: a bad-image rollout of 300 pods, each with a consequence whose cause the
// controller has not seen, fills the share of holds with a cause (256 of the default 1024 per namespace) and
// emits the other 44 at once with their cause (overflow); a consequence with no cause in the same namespace is
// still held, in its own share (#828: the first build put both in one share, and sent it to overflow).
func TestCauseHoldsHaveTheirOwnShare(t *testing.T) {
	const pods = 300
	book := &causeBook{}
	d, rs, _ := workload()
	h := newHarness(t, Config{CauseSeen: book.fn}, d, rs)
	share := DefaultMaxHeldConsequences / holdShareDivisor
	before := holdOutcomeCounts()
	for i := range pods {
		p := causePod(causePull)
		p.Name, p.UID = fmt.Sprintf("web-bad-%03d", i), types.UID(fmt.Sprintf("bad-pod-%03d", i))
		h.create(p)
		ev := consequence(fmt.Sprintf("ev-bad-%03d", i), reasonBackOff, appPath, t0)
		ev.Regarding.Name, ev.Regarding.UID = p.Name, p.UID
		h.create(ev)
		h.reconcileEvent(ev.Name)
	}
	if n := h.c.holds.len(); n != share {
		t.Fatalf("%d held, want the share of %d", n, share)
	}
	out := h.drain()
	for _, o := range out {
		if o.CauseFingerprint == "" {
			t.Fatalf("an overflow emitted without its cause: %+v", o)
		}
	}
	if len(out) != pods-share {
		t.Fatalf("%d emitted at once, want %d", len(out), pods-share)
	}
	wantOutcomes(t, before, map[string]float64{holdOverflow: float64(pods - share)})
	// A consequence with no cause: a crash loop of another pod.
	p := holdPod(reasonBackOff, sampleFailing, 0)
	p.Name, p.UID = "web-crash", "crash-pod"
	h.create(p)
	ev := consequence("ev-crash", reasonBackOff, appPath, t0)
	ev.Regarding.Name, ev.Regarding.UID = p.Name, p.UID
	h.create(ev)
	if res := h.reconcileEvent(ev.Name); res.RequeueAfter != ConsequenceGrace {
		t.Fatalf("the consequence with no cause was not held: %+v", res)
	}
	h.none()
}

// TestCauseHoldBoundEmitsAtOnce: with no room in the share of holds with a cause, a consequence waiting for its
// cause is emitted at once with it, counted as an overflow, never dropped.
func TestCauseHoldBoundEmitsAtOnce(t *testing.T) {
	book := &causeBook{}
	d, rs, _ := workload()
	// A total of 4 gives a namespace share of 1.
	h := newHarness(t, Config{MaxHeldConsequences: 4, CauseSeen: book.fn}, d, rs, causePod(causePull),
		consequence("ev-0", reasonBackOff, appPath, t0), consequence("ev-1", reasonBackOff, appPath, t0))
	before := holdOutcomeCounts()
	if res := h.reconcileEvent("ev-0"); res.RequeueAfter != ConsequenceGrace {
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
// consequence is emitted with the cause the last sample showed, not dropped (point 7).
func TestCauseHoldPodGoneEmitsWithTheCause(t *testing.T) {
	for _, s := range []sample{sampleGone, sampleReplaced} {
		t.Run(s.String(), func(t *testing.T) {
			h, _, _ := causeHoldHarness(t)
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
			wantOutcomes(t, before, map[string]float64{holdCauseUnseen: 1})
			if h.c.holds.len() != 0 {
				t.Fatal("the hold is kept")
			}
		})
	}
}

// TestNoCauseHoldWaitsForACauseThatAppears: a consequence held with no cause whose pod then shows a cause the
// controller has not seen goes on waiting for that cause, within the same hold and its share, and is released
// when the controller sees it.
func TestNoCauseHoldWaitsForACauseThatAppears(t *testing.T) {
	book := &causeBook{}
	d, rs, _ := workload()
	h := newHarness(t, Config{CauseSeen: book.fn}, d, rs, holdPod(reasonBackOff, sampleFailing, 0),
		consequence("ev-hold", reasonBackOff, appPath, t0))
	book.release = h.c.ReleaseCause
	q := h.withQueue()
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != ConsequenceGrace {
		t.Fatalf("not held with no cause: %+v", res)
	}
	h.setWaiting(causePull)
	h.clock.add(time.Second)
	if res := h.sampleByPod(); res.RequeueAfter != ConsequenceGrace-time.Second {
		t.Fatalf("requeue after %s, want the rest of the hold", res.RequeueAfter)
	}
	h.none()
	before := holdOutcomeCounts()
	book.see(h.causeShown())
	h.processQueue(q)
	if o := h.one(); o.CauseFingerprint == "" {
		t.Fatalf("emitted %+v", o)
	}
	wantOutcomes(t, before, map[string]float64{holdReleased: 1})
}

// restartBetween holds ev-hold for its cause (the pod waits in ImagePullBackOff, the controller has not seen the
// cause), then restarts the collector, as a restart of the analyzer does between the consequence and its cause: a
// new collector, whose first reconcile is now, with the same controller. change runs between the two, on the
// cluster.
func restartBetween(t *testing.T, change func(h *harness)) (*harness, *causeBook) {
	t.Helper()
	h, book, _ := causeHoldHarness(t)
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != ConsequenceGrace {
		t.Fatalf("not held for its cause before the restart: %+v", res)
	}
	h.clock.add(30 * time.Second)
	change(h)
	h.init(Config{CauseSeen: book.fn}, h.cl)
	book.release = h.c.ReleaseCause
	h.clock.add(time.Second)
	return h, book
}

// TestRestartRebuildsTheCauseHold: the hold is memory, and the replay of the event after a restart rebuilds it.
// The consequence is held again from the new process's first sight, and emitted, marked MayRepeat, once the
// controller sees its cause; or at once when the controller knows the cause from its stored Incidents.
func TestRestartRebuildsTheCauseHold(t *testing.T) {
	t.Run("seen after the replay", func(t *testing.T) {
		h, book := restartBetween(t, func(*harness) {})
		q := h.withQueue()
		if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != ConsequenceGrace {
			t.Fatalf("not held again after the restart: %+v", res)
		}
		h.none()
		fp := h.causeShown()
		book.see(fp)
		h.processQueue(q)
		if o := h.one(); o.CauseFingerprint != fp || !o.MayRepeat {
			t.Fatalf("emitted %+v, want it with its cause, marked MayRepeat", o)
		}
	})
	t.Run("loaded at start", func(t *testing.T) {
		h, book := restartBetween(t, func(h *harness) {})
		book.record(h.causeShown())
		h.reconcileEvent("ev-hold")
		if o := h.one(); o.CauseFingerprint == "" || !o.MayRepeat {
			t.Fatalf("emitted %+v, want it at once with its cause", o)
		}
	})
}

// TestRestartEventGone is the first case that remains after a restart (ADR-0039 #828 amendment): the Event is
// gone from the API server when the collector replays. Nothing is left to replay: nothing is emitted, and no
// outcome counts it, since the hold that would have counted it was in the memory of the process that stopped.
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
	before := holdOutcomeCounts()
	if res := h.reconcileEvent("ev-hold"); res != (ctrl.Result{}) {
		t.Fatalf("returned %+v", res)
	}
	h.none()
	wantOutcomes(t, before, map[string]float64{})
	if h.c.holds.len() != 0 {
		t.Fatal("a hold for an event that is gone")
	}
}

// TestRestartCauseChanged: the cause fingerprint changed at the replay (the pod now shows ErrImagePull, the
// kubelet's other state of the same failing pull). The rebuilt hold waits for the cause the pod shows; when a
// later sample shows the old cause again it waits for that one, and is released by whichever the controller sees
// while the pod shows it.
func TestRestartCauseChanged(t *testing.T) {
	for _, tc := range []struct {
		name      string
		backToOld bool // the pod shows ImagePullBackOff again before the controller sees it
	}{
		{"the new cause is seen", false},
		{"the old cause is seen", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var old string
			h, book := restartBetween(t, func(h *harness) {
				old = h.causeShown()
				h.setWaiting(causePullErr)
			})
			q := h.withQueue()
			changed := h.causeShown()
			if changed == old {
				t.Fatalf("the cause did not change (%q), so the test proves nothing", old)
			}
			if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != ConsequenceGrace {
				t.Fatalf("emitted at once at the replay, with the changed cause: %+v", res)
			}
			h.none()
			want := changed
			if tc.backToOld {
				h.setWaiting(causePull)
				h.clock.add(time.Second)
				h.sampleByPod()
				want = old
			}
			book.see(want)
			h.processQueue(q)
			if o := h.one(); o.CauseFingerprint != want {
				t.Fatalf("emitted %+v, want it with the cause the controller saw", o)
			}
		})
	}
}

// TestRestartCauseNoLongerShown is the second case that remains after a restart (ADR-0039 #828 amendment): the
// pod no longer shows the cause at the replay, and nothing persisted tells the new process which cause it showed.
// The container recovered: the consequence is one with no cause, judged by point 19, and is not opened when the
// container stays fine (not_opened). The pod was replaced: the event names a pod that is gone, so it names no
// container and is emitted at once, without a cause, for the gate, and is never held.
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
		before := holdOutcomeCounts()
		if res := h.reconcileEvent("ev-hold"); res != (ctrl.Result{}) {
			t.Fatalf("returned %+v", res)
		}
		if o := h.one(); o.CauseFingerprint != "" || o.Class != ClassNone {
			t.Fatalf("emitted %+v, want it without a cause", o)
		}
		wantOutcomes(t, before, map[string]float64{})
	})
}

// TestRetriedReleaseKeepsToItsCause (decided 2026-11-17, fourth, on #828): a hold released for cause A whose
// delivery finds the channel full is retried after the pod shows an unseen cause B and the event's series has
// grown from 1 to 3. The retry delivers for A only the occurrence released under A (the count at the release),
// and the two later ones go back to the hold, for B: they are not committed as handled under A. On the first
// build the retry emitted A's observation with the count 3 (total 3), so two occurrences skipped B's hold.
func TestRetriedReleaseKeepsToItsCause(t *testing.T) {
	book := &causeBook{}
	d, rs, _ := workload()
	h := newHarness(t, Config{CauseSeen: book.fn, Buffer: 1}, d, rs, causePod(causePull),
		consequence("ev-hold", reasonBackOff, appPath, t0))
	a := h.causeShown()
	before := holdOutcomeCounts()
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != ConsequenceGrace {
		t.Fatalf("not held for its cause: %+v", res)
	}
	h.c.out <- Observation{Fingerprint: fillerFingerprint}
	book.see(a)
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != retryAfter {
		t.Fatalf("requeue after %s, want the delivery retry", res.RequeueAfter)
	}
	h.setWaiting(causePullErr)
	b := h.causeShown()
	if a == b {
		t.Fatal("the cause did not change, so the test proves nothing")
	}
	ev := &eventsv1.Event{}
	key := reqFor("ev-hold").NamespacedName
	if err := h.cl.Get(t.Context(), key, ev); err != nil {
		t.Fatal(err)
	}
	ev.Series = &eventsv1.EventSeries{Count: 3, LastObservedTime: ev.EventTime}
	h.update(ev)
	h.drain()
	h.clock.add(retryAfter)
	res := h.reconcileEvent("ev-hold")
	if o := h.one(); o.CauseFingerprint != a || o.Total != 1 || o.Count != 1 {
		t.Fatalf("emitted cause=%s total=%d count=%d at the retry, want cause A, total 1, count 1",
			o.CauseFingerprint, o.Total, o.Count)
	}
	if res.RequeueAfter != ConsequenceGrace {
		t.Fatalf("requeue after %s, want the hold of the later occurrences for B", res.RequeueAfter)
	}
	if s, ok := h.c.holds.get(key, "ev-ev-hold"); !ok || s.cause != b || s.opened {
		t.Fatalf("held %+v (%v), want the later occurrences held for B", s, ok)
	}
	wantOutcomes(t, before, map[string]float64{holdReleased: 1})
	// B is seen: the two later occurrences are emitted with B, counted once more.
	book.see(b)
	h.reconcileEvent("ev-hold")
	if o := h.one(); o.CauseFingerprint != b || o.Total != 3 || o.Count != 2 {
		t.Fatalf("emitted cause=%s total=%d count=%d, want cause B, total 3, count 2",
			o.CauseFingerprint, o.Total, o.Count)
	}
	wantOutcomes(t, before, map[string]float64{holdReleased: 2})
	if h.c.holds.len() != 0 {
		t.Fatal("the hold is kept")
	}
}
