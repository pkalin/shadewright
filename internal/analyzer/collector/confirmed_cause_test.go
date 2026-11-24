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
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// The decision of 2026-11-21 on #432 (A): a BackOff or Unhealthy event carries a non-zero exit as its cause
// only when the Incident controller confirmed it saw that cause (Config.CauseSeen answered true for it). Every
// other way a hold can end emits the event without a cause, or drops it, as main does for an event with no
// cause.
//
// Property, one line: a non-zero-exit fingerprint on an emitted consequence was confirmed by CauseSeen.

// TestHeldExitCauseOfDeletedPodNotEmitted is finding 1 of the fourth pass on #432: CauseSeen answers false, the
// pod's container stopped with exit 1, a BackOff event is held with that exit as its candidate, then the pod is
// deleted and goes. Before the fix the hold ended by emitting the consequence with the exit's fingerprint, though
// nothing confirmed (and, for a pod being deleted, nothing delivers) that termination. (Since the rule was
// narrowed on 2026-11-21, an Unhealthy event never has an exit candidate at all.)
func TestHeldExitCauseOfDeletedPodNotEmitted(t *testing.T) {
	book := &causeBook{}
	d, rs, _ := workload()
	h := newHarness(t, Config{CauseSeen: book.fn}, d, rs,
		stoppedPod(reasonError, 1, corev1.RestartPolicyAlways, corev1.PodRunning, ""))
	at := t0.Add(30 * time.Second)
	h.clock.set(at)
	h.reconcilePod(webPod)
	h.drain()
	ev := consequence("ev-backoff", reasonBack, appPath, at)
	h.create(ev)
	if res := h.reconcileEvent(ev.Name); res.RequeueAfter == 0 {
		t.Fatal("the BackOff event was not held")
	}
	h.none()
	if s, ok := h.c.holds.get(reqFor(ev.Name).NamespacedName, ev.UID); !ok || !s.hasCause() {
		t.Fatal("the hold has no cause, so the test proves nothing")
	}
	deleteHeld(t, h)
	h.clock.set(at.Add(time.Minute))
	h.reconcileEvent(ev.Name)
	p := readPod(t, h, webPod)
	p.Finalizers = nil
	if err := h.cl.Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if err := h.cl.Get(t.Context(), reqFor(webPod).NamespacedName, &corev1.Pod{}); err == nil {
		t.Fatal("the pod is still there")
	}
	h.clock.set(at.Add(ConsequenceGrace + time.Second))
	h.reconcileEvent(ev.Name)
	for _, o := range h.drain() {
		if o.CauseFingerprint != "" {
			t.Errorf("%s emitted with the unconfirmed cause %s", o.Reason, o.CauseFingerprint)
		}
	}
}

// confirmLog wraps a causeBook and records each cause fingerprint CauseSeen answered true for: what the property
// checks an emitted cause against.
type confirmLog struct {
	book      *causeBook
	mu        sync.Mutex
	confirmed map[string]bool
}

func newConfirmLog() *confirmLog {
	return &confirmLog{book: &causeBook{}, confirmed: map[string]bool{}}
}

func (l *confirmLog) fn(ctx context.Context, o Observation) (bool, error) {
	ok, err := l.book.fn(ctx, o)
	if err == nil && ok {
		l.mu.Lock()
		l.confirmed[o.CauseFingerprint] = true
		l.mu.Unlock()
	}
	return ok, err
}

// unconfirmed returns the cause fingerprints on collected consequences that CauseSeen has yet to confirm.
// Only non-zero exit causes require confirmation.
func (l *confirmLog) unconfirmed(obs []Observation) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, o := range obs {
		if o.Class == ClassConsequence && o.CauseFingerprint != "" && !l.confirmed[o.CauseFingerprint] {
			out = append(out, o.CauseFingerprint)
		}
	}
	return out
}

// exitPod is the web pod in a crash loop of exit code 2, finished at t0, held by testFinalizer when deleted.
func exitPod() *corev1.Pod {
	p := crashLoopPod(t0, 1)
	p.Finalizers = []string{testFinalizer}
	return p
}

// exitHold is a harness for the ways a hold of a non-zero exit cause ends: the pod in a crash loop, its crash
// delivered (returned as fp), and the BackOff event ev-hold at at about the container, not yet reconciled.
type exitHold struct {
	*harness
	log               *confirmLog
	cfg               Config
	q                 workqueue.TypedRateLimitingInterface[reconcile.Request]
	fp                string
	at                time.Time
	got               []Observation
	unconfirmedCauses []string
}

func newExitHold(t *testing.T, withCauseSeen bool, cfg Config) *exitHold {
	t.Helper()
	log := newConfirmLog()
	if withCauseSeen {
		cfg.CauseSeen = log.fn
	}
	d, rs, _ := workload()
	at := t0.Add(30 * time.Second)
	ev := consequence("ev-hold", reasonBack, appPath, at)
	ev.Series = nil
	h := newHarness(t, cfg, d, rs, exitPod(), ev)
	e := &exitHold{harness: h, log: log, cfg: cfg, at: at}
	e.q = h.withQueue()
	log.book.release = h.c.ReleaseCause
	h.clock.set(at)
	h.reconcilePod(webPod)
	for _, o := range h.drain() {
		if o.Class == ClassCause {
			e.fp = o.Fingerprint
		}
	}
	if e.fp == "" {
		t.Fatal("the pod reconciler delivered no crash")
	}
	return e
}

func TestRetriedExitReleaseSplitsSeries(t *testing.T) {
	cases := []struct {
		name     string
		nextExit int32
		split    bool
	}{
		{"same exit code", 2, false},
		{"different exit code", 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			book := &causeBook{}
			d, rs, _ := workload()
			ev := consequence("ev-retry", reasonBack, appPath, t0)
			h := newHarness(t, Config{CauseSeen: book.fn, Buffer: 2}, d, rs, exitPod(), ev)
			h.reconcilePod(webPod)
			first := h.one()
			if res := h.reconcileEvent(ev.Name); res.RequeueAfter != ConsequenceGrace {
				t.Fatalf("first reconcile requeued after %s, want %s", res.RequeueAfter, ConsequenceGrace)
			}
			for range cap(h.c.out) {
				h.c.out <- Observation{Fingerprint: fillerFingerprint}
			}
			book.record(first.Fingerprint)
			if res := h.reconcileEvent(ev.Name); res.RequeueAfter != retryAfter {
				t.Fatalf("full channel requeued after %s, want %s", res.RequeueAfter, retryAfter)
			}
			h.drain()

			h.clock.add(retryAfter)
			pod := readPod(t, h, webPod)
			pod.Status = crashLoopPod(h.clock.Now(), 2).Status
			pod.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = tc.nextExit
			h.update(pod)
			h.reconcilePod(webPod)
			next := h.one()
			if next.Class != ClassCause || (next.Fingerprint != first.Fingerprint) != tc.split {
				t.Fatalf("next termination has class %q and fingerprint %q, first was %q",
					next.Class, next.Fingerprint, first.Fingerprint)
			}
			book.record(next.Fingerprint)
			ev.Series = &eventsv1.EventSeries{Count: 3, LastObservedTime: ev.EventTime}
			h.update(ev)

			type delivery struct {
				cause string
				total int64
				count int64
			}
			want := []delivery{{first.Fingerprint, 3, 3}}
			if tc.split {
				want = []delivery{{first.Fingerprint, 1, 1}, {next.Fingerprint, 3, 2}}
			}
			h.reconcileEvent(ev.Name)
			drained := h.drain()
			got := make([]delivery, 0, len(drained))
			for _, o := range drained {
				got = append(got, delivery{o.CauseFingerprint, o.Total, o.Count})
			}
			if !slices.Equal(got, want) {
				t.Errorf("retry delivered %+v, want %+v", got, want)
			}
			h.reconcileEvent(ev.Name)
			h.none()
		})
	}
}

// event reconciles ev-hold at the given offset from at and keeps what was emitted.
func (e *exitHold) event(after time.Duration) {
	e.t.Helper()
	e.clock.set(e.at.Add(after))
	e.reconcileEvent("ev-hold")
	e.collect()
}

// collect checks a batch against confirmations so far, before a later reconcile can confirm another cause.
func (e *exitHold) collect() {
	e.t.Helper()
	obs := e.drain()
	e.unconfirmedCauses = append(e.unconfirmedCauses, e.log.unconfirmed(obs)...)
	e.got = append(e.got, obs...)
}

// restart gives the harness a new collector over the same API server: an analyzer restart, with an empty hold
// table.
func (e *exitHold) restart() {
	e.init(e.cfg, e.cl)
	e.q = e.withQueue()
	e.log.book.release = e.c.ReleaseCause
}

// podGone deletes the web pod and removes its finalizer, so it is gone from the API server.
func (e *exitHold) podGone(t *testing.T) {
	t.Helper()
	if readPod(t, e.harness, webPod).DeletionTimestamp == nil {
		deleteHeld(t, e.harness)
	}
	p := readPod(t, e.harness, webPod)
	p.Finalizers = nil
	// Not h.update: its status write first reads the stored finalizers back into p.
	if err := e.cl.Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if err := e.cl.Get(t.Context(), reqFor(webPod).NamespacedName, &corev1.Pod{}); err == nil {
		t.Fatal("the pod is still there")
	}
}

// end is how the hold ended, from the consequences emitted.
func (e *exitHold) end() string {
	var cons []Observation
	for _, o := range e.got {
		if o.Class == ClassConsequence {
			cons = append(cons, o)
		}
	}
	switch {
	case len(cons) == 0:
		return endDropped
	case len(cons) > 1:
		return fmt.Sprintf("emitted %d times", len(cons))
	case cons[0].CauseFingerprint == "":
		return endBare
	case cons[0].CauseFingerprint == e.fp:
		return endCause
	}
	return "emitted with another cause " + cons[0].CauseFingerprint
}

const (
	endDropped = "dropped"
	endBare    = "emitted without a cause"
	endCause   = "emitted with its cause"
)

// TestExitCauseOnlyWhenConfirmed has a row per way a hold of a consequence whose cause is a non-zero exit can
// end (decided 2026-11-21 on #432 (A)). Each row checks the property, that a cause on an emitted consequence was
// confirmed by CauseSeen, and the end main has for an event with no cause (held, then emitted when its container
// kept failing, dropped otherwise or when its pod or event goes), or the cause where the controller confirmed it.
func TestExitCauseOnlyWhenConfirmed(t *testing.T) {
	grace := ConsequenceGrace + time.Second
	rows := []struct {
		name          string
		withCauseSeen bool
		cfg           Config
		run           func(t *testing.T, e *exitHold)
		want          string
	}{
		{"grace over, still failing", true, Config{}, func(_ *testing.T, e *exitHold) {
			e.event(0)
			e.event(time.Minute)
			e.event(grace)
		}, endBare},
		{"grace over, recovered", true, Config{}, func(t *testing.T, e *exitHold) {
			e.event(0)
			p := readPod(t, e.harness, webPod)
			p.Status.ContainerStatuses[0].State = running(e.at)
			p.Status.ContainerStatuses[0].Ready = true
			e.update(p)
			e.event(time.Minute)
			e.event(grace)
		}, endDropped},
		{"pod gone", true, Config{}, func(t *testing.T, e *exitHold) {
			e.event(0)
			e.podGone(t)
			e.event(time.Minute)
			e.event(grace)
		}, endDropped},
		{"pod replaced", true, Config{}, func(t *testing.T, e *exitHold) {
			e.event(0)
			e.podGone(t)
			n := exitPod()
			n.UID = "replaced-" + podUID
			e.create(n)
			e.event(time.Minute)
			e.event(grace)
		}, endDropped},
		{"pod being deleted to the end", true, Config{}, func(t *testing.T, e *exitHold) {
			e.event(0)
			deleteHeld(t, e.harness)
			e.event(time.Minute)
			e.event(grace)
		}, endBare},
		{"pod being deleted, then gone (finding 1)", true, Config{}, func(t *testing.T, e *exitHold) {
			e.event(0)
			deleteHeld(t, e.harness)
			e.event(time.Minute)
			e.podGone(t)
			e.event(grace)
		}, endDropped},
		{"marked released, CauseSeen unanswered at the end", true, Config{}, func(_ *testing.T, e *exitHold) {
			e.event(0)
			e.event(time.Minute)
			e.log.book.fail(context.DeadlineExceeded)
			e.c.ReleaseCause(e.fp)
			e.event(grace)
		}, endBare},
		{"marked released, CauseSeen says not seen", true, Config{}, func(t *testing.T, e *exitHold) {
			e.event(0)
			e.c.ReleaseCause(e.fp)
			if n := e.processQueue(e.q); n != 1 {
				t.Fatalf("the mark queued %d reconciles, want 1", n)
			}
			e.collect()
			e.event(grace)
		}, endBare},
		{"analyzer restarted, empty hold table", true, Config{}, func(_ *testing.T, e *exitHold) {
			e.event(0)
			e.restart()
			e.event(time.Minute)
			e.event(time.Minute + grace/2)
			e.event(time.Minute + grace)
		}, endBare},
		{"CauseSeen nil", false, Config{}, func(_ *testing.T, e *exitHold) {
			e.event(0)
			e.event(time.Minute)
			e.event(grace)
		}, endBare},
		{"event no longer a Warning", true, Config{}, func(t *testing.T, e *exitHold) {
			e.event(0)
			ev := &eventsv1.Event{}
			if err := e.cl.Get(t.Context(), reqFor("ev-hold").NamespacedName, ev); err != nil {
				t.Fatal(err)
			}
			ev.Type = corev1.EventTypeNormal
			e.update(ev)
			e.event(time.Minute)
			e.event(grace)
		}, endDropped},
		{"no room to hold", true, Config{MaxHeldConsequences: 1}, func(_ *testing.T, e *exitHold) {
			other := consequence("ev-other", ReasonUnhealthy, appPath, e.at)
			e.create(other)
			e.reconcileEvent(other.Name)
			if e.c.holds.len() != 1 {
				e.t.Fatal("the first consequence was not held, so the room is not full")
			}
			e.event(0)
		}, endBare},
		{"confirmed during the hold", true, Config{}, func(t *testing.T, e *exitHold) {
			e.event(0)
			e.clock.set(e.at.Add(time.Minute))
			e.log.book.see(e.fp)
			if n := e.processQueue(e.q); n != 1 {
				t.Fatalf("the release queued %d reconciles, want 1", n)
			}
			e.collect()
			e.event(grace)
		}, endCause},
		{"confirmed before the first sight", true, Config{}, func(_ *testing.T, e *exitHold) {
			e.log.book.record(e.fp)
			e.event(0)
		}, endCause},
		{"confirmed, then the pod goes before the end", true, Config{}, func(t *testing.T, e *exitHold) {
			e.event(0)
			e.log.book.record(e.fp)
			e.c.ReleaseCause(e.fp)
			e.podGone(t)
			e.event(time.Minute)
		}, endCause},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			e := newExitHold(t, r.withCauseSeen, r.cfg)
			r.run(t, e)
			if bad := e.unconfirmedCauses; len(bad) > 0 {
				t.Errorf("emitted with unconfirmed causes %v", bad)
			}
			if got := e.end(); got != r.want {
				t.Errorf("the hold ended %s, want %s", got, r.want)
			}
		})
	}
}

// exitSequences is how many random sequences TestExitCauseConfirmedRandom draws: 3000, or 400 under the race
// detector (seeds 0 to 399, a part of the same set), to keep the test under 5 seconds.
func exitSequences() uint64 {
	if raceOn {
		return 400
	}
	return 3000
}

// TestExitCauseConfirmedRandom draws seeded random sequences of pod events (a crash with another exit code, a
// recovery, a delete, the pod gone or replaced), CauseSeen answers (the controller sees a delivered cause, marks
// one without seeing it, does not answer in time), analyzer restarts, the event turning Normal and clock steps,
// each ended past ConsequenceGrace. It checks the property of TestExitCauseOnlyWhenConfirmed over every emitted
// consequence, and that every hold has ended.
func TestExitCauseConfirmedRandom(t *testing.T) {
	const parts = 8
	var mu sync.Mutex
	counts := map[string]int{}
	t.Run("parts", func(t *testing.T) {
		for part := range uint64(parts) {
			t.Run(fmt.Sprint(part), func(t *testing.T) {
				t.Parallel()
				local := map[string]int{}
				for seed := part; seed < exitSequences(); seed += parts {
					exitSequence(t, seed, local)
					if t.Failed() {
						t.Fatalf("seed %d failed", seed)
					}
				}
				mu.Lock()
				defer mu.Unlock()
				for k, v := range local {
					counts[k] += v
				}
			})
		}
	})
	// The property is not vacuous: confirmed causes were emitted, and so were unconfirmed holds without one.
	for _, k := range []string{endCause, endBare, endDropped} {
		if counts[k] == 0 {
			t.Errorf("no sequence ended %s: %v", k, counts)
		}
	}
	t.Logf("ends: %v", counts)
}

func exitSequence(t *testing.T, seed uint64, counts map[string]int) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 432)) // #nosec G404 -- a seeded corpus, not a secret
	e := newExitHold(t, rng.IntN(8) != 0, Config{})
	exists, deleting, restarts := true, false, int32(1)
	delivered := []string{e.fp}
	var steps []string
	process := func() {
		e.processQueue(e.q)
		e.collect()
	}
	for range 4 + rng.IntN(10) {
		switch rng.IntN(11) {
		case 0, 1:
			steps = append(steps, "sample")
			e.reconcileEvent("ev-hold")
			e.collect()
		case 2:
			if !exists {
				continue
			}
			steps = append(steps, "crash")
			restarts++
			p := readPod(t, e.harness, webPod)
			p.Status = crashLoopPod(e.clock.Now(), restarts).Status
			p.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = 1 + rng.Int32N(3)
			e.update(p)
			e.reconcilePod(webPod)
			for _, o := range e.drain() {
				if o.Class == ClassCause {
					delivered = append(delivered, o.Fingerprint)
				}
			}
		case 3:
			if !exists {
				continue
			}
			steps = append(steps, "recover")
			p := readPod(t, e.harness, webPod)
			p.Status.ContainerStatuses[0].State = running(e.clock.Now())
			p.Status.ContainerStatuses[0].Ready = true
			e.update(p)
		case 4:
			if !exists || deleting {
				continue
			}
			steps = append(steps, "delete")
			deleteHeld(t, e.harness)
			deleting = true
		case 5:
			switch {
			case exists:
				steps = append(steps, "gone")
				e.podGone(t)
				exists, deleting = false, false
			default:
				steps = append(steps, "replace")
				n := exitPod()
				n.UID = types.UID(fmt.Sprintf("replaced-%d", rng.IntN(1000)))
				e.create(n)
				exists = true
			}
		case 6:
			steps = append(steps, "see")
			e.log.book.see(delivered[rng.IntN(len(delivered))])
			process()
		case 7:
			steps = append(steps, "mark")
			e.c.ReleaseCause(delivered[rng.IntN(len(delivered))])
			process()
		case 8:
			if rng.IntN(2) == 0 {
				steps = append(steps, "noAnswer")
				e.log.book.fail(context.DeadlineExceeded)
			} else {
				steps = append(steps, "answer")
				e.log.book.fail(nil)
			}
		case 9:
			if rng.IntN(3) == 0 {
				steps = append(steps, "restart")
				e.restart()
			} else {
				steps = append(steps, "normal")
				ev := &eventsv1.Event{}
				if err := e.cl.Get(t.Context(), reqFor("ev-hold").NamespacedName, ev); err != nil {
					t.Fatal(err)
				}
				ev.Type = corev1.EventTypeNormal
				e.update(ev)
			}
		case 10:
			d := time.Duration(rng.IntN(70)) * time.Second
			steps = append(steps, "+"+d.String())
			e.clock.add(d)
		}
	}
	e.reconcileEvent("ev-hold")
	e.collect()
	e.clock.add(ConsequenceGrace + time.Second)
	e.reconcileEvent("ev-hold")
	e.collect()
	if bad := e.unconfirmedCauses; len(bad) > 0 {
		t.Errorf("seed %d %v: emitted with unconfirmed causes %v", seed, steps, bad)
	}
	if n := e.c.holds.len(); n != 0 {
		t.Errorf("seed %d %v: %d holds left past ConsequenceGrace", seed, steps, n)
	}
	for _, o := range e.got {
		if o.Class != ClassConsequence {
			continue
		}
		if o.CauseFingerprint != "" {
			counts[endCause]++
		} else {
			counts[endBare]++
		}
	}
	if slices.IndexFunc(e.got, func(o Observation) bool { return o.Class == ClassConsequence }) < 0 {
		counts[endDropped]++
	}
}
