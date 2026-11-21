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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
)

// The decision of 2026-11-21 on #432 (Count and Decision, head 5caef316) narrows the crash loop link. Properties,
// one line each:
//   - N1: a confirmed non-zero exit is a cause of BackOff events of the same container only; an Unhealthy event
//     never links to one (TestNonBackOffLinksAsMain checks every event that is not a BackOff against main, with
//     the same room in the hold table).
//   - N2: the exit candidates of a hold whose candidates are exits alone are kept for its life: a later sample adds
//     one and removes none, except the oldest exit once the hold has maxHeldExits (TestHeldExitsKeepTheNewest), and
//     the hold is found by each when a cause is released. A sample that shows an OOM kill or a wait drops them
//     (decided 2026-11-22): the hold then links as main.
//   - N3: the verdict on an exit is made once: a delivery retry asks nothing, counts nothing and logs nothing more.
//   - N4: a failed CauseSeen call at a final step never drops a consequence main emits: it goes on with the OOM
//     kill if there is one, otherwise as main has it; cause_unanswered is counted once for a hold of exits alone,
//     never for one with an OOM kill (decided 2026-11-22).

// TestLivenessNeverLinksToItsExit is N1 at the collector: the kubelet killed the app container for its liveness
// probe (exit 137, reason Error) and the Incident controller has seen that termination. A liveness failure is the
// cause of that exit, not its consequence: the Unhealthy events before and after it link to nothing, and are held
// and judged as main judges an event with no cause, while a BackOff of the same container links to the exit.
func TestLivenessNeverLinksToItsExit(t *testing.T) {
	book := &causeBook{}
	d, rs, _ := workload()
	pod := holdPod(reasonBack, sampleFailing, 3)
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: terminated("liveness-kill", reasonError, exitOOM, t0)}
	h := newHarness(t, Config{CauseSeen: book.fn}, d, rs, pod)
	h.clock.set(t0.Add(time.Second))
	h.reconcilePod(webPod)
	var exitFP string
	for _, o := range h.drain() {
		if o.Class == ClassCause {
			exitFP = o.Fingerprint
			book.record(o.Fingerprint)
		}
	}
	if exitFP == "" {
		t.Fatal("the pod reconciler delivered no exit, so the test proves nothing")
	}
	got := make([]Observation, 0, 4)
	for i, off := range []time.Duration{-20 * time.Second, 30 * time.Second} {
		ev := consequence("ev-liveness-"+string(rune('a'+i)), ReasonUnhealthy, appPath, t0.Add(off))
		ev.Series = nil
		h.create(ev)
		h.reconcileEvent(ev.Name)
		got = append(got, h.drain()...)
	}
	backOff := consequence("ev-backoff", reasonBack, appPath, t0.Add(30*time.Second))
	backOff.Series = nil
	h.create(backOff)
	h.reconcileEvent(backOff.Name)
	got = append(got, h.drain()...)
	h.clock.add(time.Minute)
	for _, name := range []string{"ev-liveness-a", "ev-liveness-b"} {
		h.reconcileEvent(name)
	}
	h.clock.add(ConsequenceGrace)
	for _, name := range []string{"ev-liveness-a", "ev-liveness-b"} {
		h.reconcileEvent(name)
	}
	got = append(got, h.drain()...)
	unhealthy := 0
	for _, o := range got {
		switch o.Reason {
		case ReasonUnhealthy:
			unhealthy++
			if o.CauseFingerprint != "" {
				t.Errorf("a liveness failure links to %s, the exit it caused; want no cause, as on main", o.CauseFingerprint)
			}
		case reasonBack:
			if o.CauseFingerprint != exitFP {
				t.Errorf("the BackOff links to %q, want the confirmed exit %s", o.CauseFingerprint, exitFP)
			}
		}
	}
	if unhealthy != 2 {
		t.Errorf("%d liveness failures emitted, want both, held and judged as main judges an event with no cause", unhealthy)
	}
}

// TestOOMKeptWhenTheStatusMovesOn is N2 and finding C1 of the sixth pass on #432: an OOM kill at t0, an exit 1 at
// t0+3m, a BackOff at t0+1m, the OOM kill's Incident not open yet. A later sample shows a second exit that pushed
// the kill out of the status. Then the OOM Incident opens: the release finds the hold by the kill's fingerprint,
// and the BackOff is emitted once with it, as main emits it.
func TestOOMKeptWhenTheStatusMovesOn(t *testing.T) {
	book := &causeBook{}
	d, rs, _ := workload()
	pod := oomThenExitPod()
	at := t0.Add(time.Minute)
	ev := consequence("ev-backoff", reasonBack, appPath, at)
	ev.Series = nil
	h := newHarness(t, Config{CauseSeen: book.fn}, d, rs, pod, ev)
	q := h.withQueue()
	book.release = h.c.ReleaseCause
	oomFP := mainCauseFingerprint(h.c, pod, ownerOf(t, h), at, true)
	if oomFP == "" {
		t.Fatal("main links nothing here, so the test proves nothing")
	}
	h.clock.set(t0.Add(3*time.Minute + 10*time.Second))
	if res := h.reconcileEvent(ev.Name); res.RequeueAfter == 0 {
		t.Fatal("the BackOff was not held")
	}
	// The status moves on: a second exit at t0+3m50s, the first one in the last state, the OOM kill gone.
	cur := readPod(t, h, webPod)
	cur.Status.ContainerStatuses[0].LastTerminationState = cur.Status.ContainerStatuses[0].State
	cur.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Terminated: terminated("exit-2", reasonError, 1, t0.Add(3*time.Minute+50*time.Second))}
	cur.Status.ContainerStatuses[0].RestartCount++
	h.update(cur)
	h.clock.set(t0.Add(4 * time.Minute))
	h.reconcileEvent(ev.Name)
	got := consequenceCauses(h.drain())
	// The OOM kill's Incident opens.
	h.clock.set(t0.Add(4*time.Minute + 30*time.Second))
	book.see(oomFP)
	if q.Len() != 1 {
		t.Errorf("the release of the OOM kill queued %d events, want the held BackOff", q.Len())
	}
	h.reconcileEvent(ev.Name)
	got = append(got, consequenceCauses(h.drain())...)
	h.clock.set(t0.Add(3*time.Minute + 10*time.Second + ConsequenceGrace + time.Second))
	h.reconcileEvent(ev.Name)
	got = append(got, consequenceCauses(h.drain())...)
	if len(got) != 1 || got[0] != oomFP {
		t.Fatalf("the BackOff was emitted with causes %q, want once with the OOM kill's %s, as on main", got, oomFP)
	}
}

// TestExitVerdictOnceAcrossRetries is N3 and findings C2 and E2 of the sixth pass on #432: a hold of an unconfirmed
// exit ends, and its delivery finds the channel full four times. The verdict is counted, logged and asked for once;
// the retries send the observation as decided, without the exit even when the controller has seen it by then.
func TestExitVerdictOnceAcrossRetries(t *testing.T) {
	e := newExitHold(t, true, Config{Buffer: 1})
	sink := newKVSink()
	before := holdOutcomeCounts()
	e.eventLogged(sink, 0)
	e.eventLogged(sink, time.Minute)
	e.c.out <- Observation{Fingerprint: fillerFingerprint}
	grace := ConsequenceGrace + time.Second
	e.eventLogged(sink, grace)
	e.log.book.mu.Lock()
	asked := e.log.book.asked
	e.log.book.mu.Unlock()
	for i := 1; i <= 3; i++ {
		if i == 3 {
			// The controller sees the exit between two retries.
			e.log.book.record(e.fp)
		}
		e.c.out <- Observation{Fingerprint: fillerFingerprint}
		if res := e.eventLogged(sink, grace+time.Duration(i)*retryAfter); res.RequeueAfter != retryAfter {
			t.Fatalf("retry %d: requeue after %s, want the delivery retry", i, res.RequeueAfter)
		}
	}
	e.drain()
	e.eventLogged(sink, grace+4*retryAfter)
	e.log.book.mu.Lock()
	again := e.log.book.asked - asked
	e.log.book.mu.Unlock()
	if again != 0 {
		t.Errorf("CauseSeen asked %d more times on the delivery retries, want none", again)
	}
	if n := outcomesSince(before)[holdCauseUnconfirmed]; n != 1 {
		t.Errorf("cause_unconfirmed counted %v for one event, want 1", n)
	}
	logged := 0
	for _, l := range sink.lines() {
		if l.kv["causeFingerprint"] == e.fp {
			logged++
		}
	}
	if logged != 1 {
		t.Errorf("the verdict was logged %d times, want once", logged)
	}
	if got := e.end(); got != endBare {
		t.Errorf("the consequence ended %s, want %s: the verdict was made before the controller saw the exit", got,
			endBare)
	}
}

// TestFailedAskAtFinalStepAsMain is N4 and finding E1 of the sixth pass on #432: CauseSeen runs out of time at
// every ask, at each final step of a held BackOff (the end of the hold, its pod gone, the event leaving the
// collector). With an OOM kill in the window it is emitted once with the kill, as main emits it, and nothing is
// counted for the exit; with none it ends as main ends an event with no cause, and cause_unanswered is counted
// once.
func TestFailedAskAtFinalStepAsMain(t *testing.T) {
	grace := ConsequenceGrace + time.Second
	steps := []struct {
		name string
		end  func(t *testing.T, e *exitHold)
		// bare is how the step ends without an OOM kill, as main ends an event with no cause.
		bare string
	}{
		{"end of hold", func(t *testing.T, e *exitHold) {
			oneMoreRestart(t, e)
			e.event(grace)
		}, endBare},
		{"pod deleted and gone", func(t *testing.T, e *exitHold) {
			e.podGone(t)
			e.event(time.Minute + 30*time.Second)
		}, endDropped},
		{"event no longer a Warning", func(t *testing.T, e *exitHold) {
			ev := readEvent(t, e.harness, "ev-hold")
			ev.Type = corev1.EventTypeNormal
			e.update(ev)
			e.event(time.Minute + 30*time.Second)
		}, endDropped},
	}
	for _, step := range steps {
		for _, withOOM := range []bool{true, false} {
			name := step.name + "/no OOM kill"
			if withOOM {
				name = step.name + "/OOM kill in the window"
			}
			t.Run(name, func(t *testing.T) {
				e := newExitHold(t, true, Config{})
				var oomFP string
				if withOOM {
					// An OOM kill 20 seconds before the BackOff, older than the exit, in the last state.
					p := readPod(t, e.harness, webPod)
					p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: terminated("exit-now",
						reasonError, 2, e.at.Add(10*time.Second))}
					p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
						Terminated: terminated("oom", ReasonOOMKilled, exitOOM, e.at.Add(-20*time.Second))}
					e.update(p)
					oomFP = mainCauseFingerprint(e.c, p, ownerOf(t, e.harness), e.at, true)
					if oomFP == "" {
						t.Fatal("main links nothing here, so the test proves nothing")
					}
				}
				e.log.book.fail(context.DeadlineExceeded)
				before := holdOutcomeCounts()
				e.event(0)
				oneMoreRestart(t, e)
				e.event(time.Minute)
				step.end(t, e)
				want := step.bare
				got := e.end()
				if withOOM {
					want = "emitted with another cause " + oomFP
				}
				if got != want {
					t.Errorf("ended %s, want %s", got, want)
				}
				wantN := 1.0
				if withOOM {
					wantN = 0
				}
				if n := outcomesSince(before)[holdCauseUnanswered]; n != wantN {
					t.Errorf("cause_unanswered counted %v, want %v", n, wantN)
				}
			})
		}
	}
}

// TestIssue432CrashLoopReleasedLater is case (a) of #432 as the running operator took it (finding D5 of the sixth
// pass): the BackOff at 20:27:15 is held because the Error Incident does not exist yet, and released 13 seconds
// later, when the second crash opens it and the Incident controller calls ReleaseCause. The BackOff is emitted
// once, linked to the exit, and counted released.
func TestIssue432CrashLoopReleasedLater(t *testing.T) {
	d, rs, _ := workload()
	ev := consequence("ev-backoff", reasonBack, appPath, t0.Add(2*time.Minute+15*time.Second))
	ev.Series = nil
	book := &causeBook{}
	h := newHarness(t, Config{CauseSeen: book.fn}, d, rs, crashLoopPod(t0.Add(20*time.Second), 1), ev)
	q := h.withQueue()
	book.release = h.c.ReleaseCause
	before := holdOutcomeCounts()
	h.clock.set(t0.Add(20 * time.Second))
	h.reconcilePod(webPod)
	var exitFP string
	for _, o := range h.drain() {
		if o.Class == ClassCause {
			exitFP = o.Fingerprint
		}
	}
	if exitFP == "" {
		t.Fatal("the pod reconciler delivered no crash")
	}
	// 20:27:15 the BackOff: the controller holds the first crash in its gate, no Incident yet.
	h.clock.set(t0.Add(2*time.Minute + 15*time.Second))
	if res := h.reconcileEvent(ev.Name); res.RequeueAfter == 0 {
		t.Fatal("the BackOff was not held")
	}
	h.none()
	// 20:27:28 the second crash opens the Error Incident; the controller sees the cause and releases the hold.
	h.clock.set(t0.Add(2*time.Minute + 28*time.Second))
	cur := readPod(t, h, webPod)
	cur.Status = crashLoopPod(t0.Add(2*time.Minute+28*time.Second), 5).Status
	h.update(cur)
	h.reconcilePod(webPod)
	h.drain()
	book.see(exitFP)
	if q.Len() != 1 {
		t.Fatalf("the release queued %d events, want the held BackOff", q.Len())
	}
	h.reconcileEvent(ev.Name)
	got := consequenceCauses(h.drain())
	if len(got) != 1 || got[0] != exitFP {
		t.Fatalf("the BackOff was emitted with causes %q, want once with the exit %s", got, exitFP)
	}
	wantOutcomes(t, before, map[string]float64{holdReleased: 1})
}

// oneMoreRestart counts one more restart of the app container, so that the next sample sees it failing.
func oneMoreRestart(t *testing.T, e *exitHold) {
	t.Helper()
	p := readPod(t, e.harness, webPod)
	p.Status.ContainerStatuses[0].RestartCount++
	e.update(p)
}

// readEvent reads the stored event name back.
func readEvent(t *testing.T, h *harness, name string) *eventsv1.Event {
	t.Helper()
	ev := &eventsv1.Event{}
	if err := h.cl.Get(t.Context(), reqFor(name).NamespacedName, ev); err != nil {
		t.Fatal(err)
	}
	return ev
}
