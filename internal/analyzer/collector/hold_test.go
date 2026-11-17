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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const appPath = "spec.containers{" + containerApp + "}"

// sample is one state of the held container at a reconcile of the event, as the corpus generates it.
type sample uint8

const (
	sampleFailing   sample = iota // waiting in CrashLoopBackOff (BackOff) or not ready (Unhealthy)
	sampleFine                    // running and ready, no new restart
	sampleRestart                 // running and ready after one more restart (failing by the restart, both reasons)
	sampleGone                    // the pod is deleted
	sampleReplaced                // a pod of the same name with another UID
	sampleReadError               // the read of the pod fails once, then the same step is read again
)

func (s sample) String() string {
	return [...]string{"failing", "fine", "restart", "gone", "replaced", "readError"}[s]
}

// holdPod returns the web pod with the app container in the state s, after restarts restarts.
func holdPod(reason string, s sample, restarts int32) *corev1.Pod {
	p := specPod()
	st := corev1.ContainerStatus{Name: containerApp, RestartCount: restarts, Ready: true, State: running(t0)}
	switch {
	case s == sampleFailing && reason == reasonBackOff:
		st.Ready, st.State = false, corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "CrashLoopBackOff"}}
	case s == sampleFailing:
		st.Ready = false
	}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{st}
	return p
}

// setPodState writes the app container's state s into the stored web pod.
func (h *harness) setPodState(reason string, s sample, restarts int32) {
	h.t.Helper()
	cur := readPod(h.t, h, webPod)
	cur.Status = holdPod(reason, s, restarts).Status
	h.update(cur)
}

// failPodReads makes the reads of pods fail until the returned func is called.
func (h *harness) failPodReads() func() {
	orig := h.c.owners.pods
	h.c.owners.pods = interceptor.NewClient(h.cl, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Pod); ok {
				return errors.New("synthetic: the API server did not answer")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	return func() { h.c.owners.pods = orig }
}

// sampleByPod is an update of the web pod: it reconciles the events heldEventsOf maps the pod to, which must be
// the one held event ev-hold, and returns the result.
func (h *harness) sampleByPod() ctrl.Result {
	h.t.Helper()
	reqs := h.c.heldEventsOf(h.t.Context(), readPod(h.t, h, webPod))
	if len(reqs) != 1 || reqs[0] != reqFor("ev-hold") {
		h.t.Fatalf("an update of the pod enqueued %v, want ev-hold", reqs)
	}
	return h.reconcileEvent("ev-hold")
}

func (h *harness) reconcileEventErr(name string) (ctrl.Result, error) {
	return h.events.Reconcile(h.t.Context(), reqFor(name))
}

// holdHarness starts a harness with the web workload, its pod in state first, and a consequence event about the
// app container.
func holdHarness(t *testing.T, reason string, first sample) *harness {
	t.Helper()
	d, rs, _ := workload()
	ev := consequence("ev-hold", reason, appPath, t0)
	return newHarness(t, Config{}, d, rs, holdPod(reason, first, 0), ev)
}

// TestHoldCorpus runs generated sequences of pod states through the event collector, each with its answer known
// by construction (ADR-0039 point 19): a consequence with no cause is emitted only when, at the end of
// ConsequenceGrace, its pod exists with the same UID, the container is failing, and it was seen failing at least
// twice, and either the last sample is failing or it restarted at least twice during the wait (amended
// 2026-11-16); a pod gone or replaced during the wait opens nothing; a failed read changes nothing and is
// retried. The samples between the first and the last are made by updates of the pod and by reconciles of the
// event in turn.
func TestHoldCorpus(t *testing.T) {
	rng := rand.New(rand.NewPCG(792, 19)) // #nosec G404 -- a seeded corpus, not a secret
	middle := []sample{sampleFailing, sampleFine, sampleRestart, sampleGone, sampleReplaced, sampleReadError}
	end := []sample{sampleFailing, sampleFine, sampleRestart, sampleReadError}
	outcomes := map[bool]int{}
	for i := range 400 {
		reason := []string{reasonBackOff, ReasonUnhealthy}[i%2]
		n := rng.IntN(4)
		seq := make([]sample, 0, n+2)
		seq = append(seq, []sample{sampleFailing, sampleFine}[rng.IntN(2)])
		for range n {
			seq = append(seq, middle[rng.IntN(len(middle))])
		}
		seq = append(seq, end[rng.IntN(len(end))])
		name := fmt.Sprintf("%03d-%s-%v", i, reason, seq)
		t.Run(name, func(t *testing.T) { outcomes[runHoldSequence(t, reason, seq)]++ })
	}
	// The corpus must exercise both answers.
	if outcomes[true] < 50 || outcomes[false] < 50 {
		t.Fatalf("outcomes emitted/not: %d/%d, want at least 50 of each", outcomes[true], outcomes[false])
	}
}

// runHoldSequence runs seq and returns whether the consequence was emitted.
func runHoldSequence(t *testing.T, reason string, seq []sample) bool {
	h := holdHarness(t, reason, seq[0])
	// restarts is the count set in the pod, seen the count at the previous sample of the hold.
	var restarts, seen int32
	failing, ended := 0, false
	last := len(seq) - 1
	for i, s := range seq {
		at := t0.Add(ConsequenceGrace * time.Duration(i) / time.Duration(last))
		h.clock.set(at)
		switch s {
		case sampleGone, sampleReplaced:
			pod := readPod(t, h, webPod)
			if err := h.cl.Delete(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
			if s == sampleReplaced {
				p := holdPod(reason, sampleFailing, restarts)
				p.UID = "replacement-uid"
				h.create(p)
			}
			ended = true
		case sampleReadError:
			restore := h.failPodReads()
			if _, err := h.reconcileEventErr("ev-hold"); err == nil {
				t.Fatalf("step %d: a failed pod read gave no error", i)
			}
			restore()
			h.none()
			// The same state is read again by the retry: the last state set stays, read below.
		case sampleRestart:
			restarts++
			h.setPodState(reason, s, restarts)
		default:
			h.setPodState(reason, s, restarts)
		}
		if ended {
			// The pod is gone or replaced: this reconcile ends the hold, and every later one finds it handled.
			h.reconcileEvent("ev-hold")
			h.none()
			for range seq[i+1:] {
				h.reconcileEvent("ev-hold")
				h.none()
			}
			return false
		}
		cur := readPod(t, h, webPod)
		st := cur.Status.ContainerStatuses[0]
		// Failing: not ready (both reasons here, a back-off is never ready) or restarted since the previous sample.
		now := !st.Ready || st.RestartCount > seen
		seen = st.RestartCount
		if now {
			failing++
		}
		var res ctrl.Result
		if i > 0 && i < last && i%2 == 1 {
			res = h.sampleByPod()
		} else {
			res = h.reconcileEvent("ev-hold")
		}
		if i < last {
			if want := ConsequenceGrace - at.Sub(t0); res.RequeueAfter != want {
				t.Fatalf("step %d: requeue after %s, want %s", i, res.RequeueAfter, want)
			}
			h.none()
			continue
		}
		// seen is the restarts during the hold: the count starts at 0.
		emitted := failing >= 2 && (now || seen >= 2)
		if emitted {
			o := h.one()
			if o.Class != ClassConsequence || o.Count != 1 || o.Total != 1 {
				t.Fatalf("emitted %+v, want a consequence counting 1", o)
			}
		} else {
			h.none()
		}
		if h.c.holds.len() != 0 {
			t.Fatalf("%d holds left after the end", h.c.holds.len())
		}
		return emitted
	}
	return false
}

// TestFailingOnceAndFineAtTheEndOpensNothing is #726 and its variants: a probe failure or back-off seen once,
// with the container fine at the end of the hold, opens nothing, also when the probe rules of #726 would count it
// (a container that restarted before, a liveness probe).
func TestFailingOnceAndFineAtTheEndOpensNothing(t *testing.T) {
	for _, tc := range []struct {
		name, reason, note string
		restarts           int32
	}{
		{"readiness of a pod that restarted before", ReasonUnhealthy,
			"Readiness probe failed: connection refused", 1},
		{"liveness", ReasonUnhealthy, "Liveness probe failed: HTTP probe failed with statuscode: 500", 0},
		{"back-off", reasonBackOff, "Back-off restarting failed container app", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, rs, _ := workload()
			pod := holdPod(tc.reason, sampleFailing, tc.restarts)
			pod.Status.StartTime = &metav1.Time{Time: t0}
			ev := consequence("ev-once", tc.reason, appPath, t0)
			ev.Note = tc.note
			h := newHarness(t, Config{}, d, rs, pod, ev)
			if res := h.reconcileEvent("ev-once"); res.RequeueAfter != ConsequenceGrace {
				t.Fatalf("requeue after %s, want the grace", res.RequeueAfter)
			}
			h.none()
			h.clock.add(ConsequenceGrace)
			h.setPodState(tc.reason, sampleFine, tc.restarts)
			h.reconcileEvent("ev-once")
			h.none()
			// Handled: read again, it stays closed.
			h.reconcileEvent("ev-once")
			h.none()
		})
	}
}

// TestFailingTwiceAndAtTheEndIsEmitted: a consequence still failing at the end, seen failing at both samples,
// goes to the gate.
func TestFailingTwiceAndAtTheEndIsEmitted(t *testing.T) {
	for _, reason := range []string{reasonBackOff, ReasonUnhealthy} {
		t.Run(reason, func(t *testing.T) {
			h := holdHarness(t, reason, sampleFailing)
			h.reconcileEvent("ev-hold")
			h.none()
			h.clock.add(ConsequenceGrace)
			o := func() Observation { h.reconcileEvent("ev-hold"); return h.one() }()
			if o.Class != ClassConsequence || o.Count != 1 || o.Immediate {
				t.Fatalf("emitted %+v, want a consequence for the gate", o)
			}
		})
	}
}

// TestHeldPodGoneOrReplacedOpensNothing: the end of the wait finds no pod, or another pod of the name.
func TestHeldPodGoneOrReplacedOpensNothing(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replaced=%t", replace), func(t *testing.T) {
			h := holdHarness(t, reasonBackOff, sampleFailing)
			h.reconcileEvent("ev-hold")
			h.clock.add(ConsequenceGrace / 2)
			h.reconcileEvent("ev-hold")
			pod := readPod(t, h, webPod)
			if err := h.cl.Delete(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
			if replace {
				p := holdPod(reasonBackOff, sampleFailing, 0)
				p.UID = "replacement-uid"
				h.create(p)
			}
			h.clock.add(ConsequenceGrace / 2)
			h.reconcileEvent("ev-hold")
			h.none()
			if h.c.holds.len() != 0 {
				t.Fatal("the hold is kept for a pod that is gone")
			}
		})
	}
}

// TestHeldPodReadFailureIsRetried: a failed read of the pod at the end of the wait is an error for a retry, not
// "gone": the hold stays and the retry emits.
func TestHeldPodReadFailureIsRetried(t *testing.T) {
	h := holdHarness(t, reasonBackOff, sampleFailing)
	h.reconcileEvent("ev-hold")
	h.clock.add(ConsequenceGrace)
	restore := h.failPodReads()
	for range 3 {
		if _, err := h.reconcileEventErr("ev-hold"); err == nil {
			t.Fatal("a failed pod read gave no error")
		}
	}
	restore()
	h.none()
	if h.c.holds.len() != 1 {
		t.Fatal("a failed read released the hold")
	}
	h.reconcileEvent("ev-hold")
	if o := h.one(); o.Count != 1 {
		t.Fatalf("emitted %+v after the retry", o)
	}
}

// TestHoldBoundEmitsAtOnce: over the namespace share, and over the total, a new consequence is emitted at once,
// without a verdict, and the held ones stay held.
func TestHoldBoundEmitsAtOnce(t *testing.T) {
	d, rs, _ := workload()
	objs := make([]client.Object, 0, 6)
	objs = append(objs, d, rs, holdPod(reasonBackOff, sampleFailing, 0))
	for i := range 3 {
		objs = append(objs, consequence(fmt.Sprintf("ev-%d", i), reasonBackOff, appPath, t0))
	}
	// A total of 8 gives a namespace share of 2.
	h := newHarness(t, Config{MaxHeldConsequences: 8}, objs...)
	for i := range 2 {
		if res := h.reconcileEvent(fmt.Sprintf("ev-%d", i)); res.RequeueAfter != ConsequenceGrace {
			t.Fatalf("ev-%d: not held", i)
		}
	}
	h.none()
	h.reconcileEvent("ev-2")
	if o := h.one(); o.Count != 1 || o.Class != ClassConsequence {
		t.Fatalf("over the share: emitted %+v", o)
	}
	if h.c.holds.len() != 2 {
		t.Fatalf("%d held, want the 2 held before", h.c.holds.len())
	}
}

// TestHoldBoundDefaults: at 1,024 held consequences with no cause, 256 in each of four namespaces, the next is
// refused, in any namespace, and nothing held is dropped. The holds with a cause have a share of their own, with
// the same bounds, which the full share of those with none does not touch.
func TestHoldBoundDefaults(t *testing.T) {
	hs := newConsequenceHolds(DefaultMaxHeldConsequences)
	fits := func(key types.NamespacedName, s consequenceHold) bool {
		_, ok := hs.put(key, s)
		return ok
	}
	for _, cause := range []string{"", "a-cause"} {
		for ns := range 4 {
			for i := range 256 {
				key := types.NamespacedName{Namespace: fmt.Sprintf("ns-%d", ns), Name: fmt.Sprintf("ev-%s-%d", cause, i)}
				if !fits(key, consequenceHold{event: types.UID(key.String()), cause: cause}) {
					t.Fatalf("%s (cause %q) refused below the bound", key, cause)
				}
			}
			key := types.NamespacedName{Namespace: fmt.Sprintf("ns-%d", ns), Name: "one-more" + cause}
			if fits(key, consequenceHold{cause: cause}) {
				t.Fatalf("namespace %d took more than its share (cause %q)", ns, cause)
			}
		}
		if fits(types.NamespacedName{Namespace: "ns-new", Name: "ev" + cause}, consequenceHold{cause: cause}) {
			t.Fatalf("took one over 1,024 (cause %q)", cause)
		}
	}
	if hs.len() != 2*DefaultMaxHeldConsequences {
		t.Fatalf("%d held, want 1,024 in each share", hs.len())
	}
	const first = "ns-0"
	hs.done(types.NamespacedName{Namespace: first, Name: "ev--0"})
	if fits(types.NamespacedName{Namespace: first, Name: "again"}, consequenceHold{cause: "a-cause"}) {
		t.Fatal("a hold with no cause that ended made room in the other share")
	}
	if !fits(types.NamespacedName{Namespace: first, Name: "again"}, consequenceHold{}) {
		t.Fatal("no room after one hold ended")
	}
}

// TestLargerSeriesTotalIsANewHold: a series that grows after "not opened" is held again, and when it opens it
// counts only the increase.
func TestLargerSeriesTotalIsANewHold(t *testing.T) {
	d, rs, _ := workload()
	ev := consequence("ev-series", reasonBackOff, appPath, t0)
	ev.Series = &eventsv1.EventSeries{Count: 3, LastObservedTime: metav1.NewMicroTime(t0)}
	h := newHarness(t, Config{}, d, rs, holdPod(reasonBackOff, sampleFailing, 0), ev)
	h.reconcileEvent("ev-series")
	h.clock.add(ConsequenceGrace)
	h.setPodState(reasonBackOff, sampleFine, 0)
	h.reconcileEvent("ev-series")
	h.none()

	h.setPodState(reasonBackOff, sampleFailing, 0)
	h.clock.add(time.Minute)
	cur := &eventsv1.Event{}
	if err := h.cl.Get(t.Context(), client.ObjectKeyFromObject(ev), cur); err != nil {
		t.Fatal(err)
	}
	cur.Series.Count = 5
	cur.Series.LastObservedTime = metav1.NewMicroTime(h.clock.Now())
	h.update(cur)
	if res := h.reconcileEvent("ev-series"); res.RequeueAfter != ConsequenceGrace {
		t.Fatalf("the increase is not held: %+v", res)
	}
	h.none()
	h.clock.add(ConsequenceGrace)
	h.reconcileEvent("ev-series")
	o := h.one()
	if o.Total != 5 || o.Count != 2 {
		t.Fatalf("emitted total %d count %d, want total 5 count 2", o.Total, o.Count)
	}
}

// TestHoldAfterRestart: a new process holds an occurrence again from its own first sight and emits it.
func TestHoldAfterRestart(t *testing.T) {
	h := holdHarness(t, reasonBackOff, sampleFailing)
	h.clock.add(time.Minute)
	h.reconcileEvent("ev-hold")
	// A restart: a new collector on the same cluster, whose first reconcile is now.
	h.init(Config{}, h.cl)
	h.clock.add(time.Minute)
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != ConsequenceGrace {
		t.Fatalf("not held again after the restart: %+v", res)
	}
	h.none()
	h.clock.add(ConsequenceGrace)
	h.reconcileEvent("ev-hold")
	if o := h.one(); !o.MayRepeat || o.Occurrence != "event:ev-ev-hold" {
		t.Fatalf("emitted %+v, want the occurrence marked MayRepeat", o)
	}
}

// TestHoldDecisionStandsAcrossADeliveryRetry: once the end decided to emit, a full channel retries the delivery,
// and the retry emits even when the container is fine by then. An update of the pod meanwhile enqueues nothing,
// so the retry keeps its delay.
func TestHoldDecisionStandsAcrossADeliveryRetry(t *testing.T) {
	d, rs, _ := workload()
	h := newHarness(t, Config{Buffer: 1}, d, rs, holdPod(reasonBackOff, sampleFailing, 0),
		consequence("ev-hold", reasonBackOff, appPath, t0))
	h.reconcileEvent("ev-hold")
	select {
	case h.c.out <- Observation{Fingerprint: fillerFingerprint}:
	default:
		t.Fatal("emitted before the end of the hold")
	}
	h.clock.add(ConsequenceGrace)
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != retryAfter {
		t.Fatalf("requeue after %s, want the delivery retry", res.RequeueAfter)
	}
	h.drain()
	h.setPodState(reasonBackOff, sampleFine, 0)
	if reqs := h.c.heldEventsOf(t.Context(), readPod(t, h, webPod)); len(reqs) != 0 {
		t.Fatalf("an update of the pod enqueued %v during the delivery retry", reqs)
	}
	h.reconcileEvent("ev-hold")
	if o := h.one(); o.Count != 1 {
		t.Fatalf("emitted %+v", o)
	}
}

// TestCauseDuringTheHoldReleasesIt: a cause shown during the wait sends the consequence on with it.
func TestCauseDuringTheHoldReleasesIt(t *testing.T) {
	h := holdHarness(t, reasonBackOff, sampleFailing)
	h.reconcileEvent("ev-hold")
	cur := readPod(t, h, webPod)
	cur.Status.ContainerStatuses[0].State.Waiting.Reason = reasonConfig
	h.update(cur)
	h.clock.add(time.Second)
	h.reconcileEvent("ev-hold")
	if o := h.one(); o.CauseFingerprint == "" {
		t.Fatalf("emitted %+v, want it with its cause", o)
	}
	if h.c.holds.len() != 0 {
		t.Fatal("the hold is kept")
	}
}

// TestOnlyConsequencesWithNoCauseAreHeld: everything else is emitted at the first reconcile, as before the hold.
func TestOnlyConsequencesWithNoCauseAreHeld(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   func() *eventsv1.Event
		pod  func(*corev1.Pod)
	}{
		{"no container named", func() *eventsv1.Event { return consequence("ev", reasonBackOff, "", t0) }, nil},
		{"another reason", func() *eventsv1.Event { return consequence("ev", "Failed", appPath, t0) }, nil},
		{"a cause shown", func() *eventsv1.Event { return consequence("ev", reasonBackOff, appPath, t0) },
			func(p *corev1.Pod) {
				p.Status.ContainerStatuses[0].State.Waiting.Reason = "ImagePullBackOff"
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, rs, _ := workload()
			pod := holdPod(reasonBackOff, sampleFailing, 0)
			if tc.pod != nil {
				tc.pod(pod)
			}
			h := newHarness(t, Config{}, d, rs, pod, tc.ev())
			if res := h.reconcileEvent("ev"); res.RequeueAfter != 0 {
				t.Fatalf("held: %+v", res)
			}
			h.one()
		})
	}
}

// TestBackOffRecoveredByOneRestartOpensNothing: a container in back-off at the first sight that restarts once
// and then runs and is ready for the rest of the wait opens nothing (ADR-0039 point 19, amended 2026-11-16). It
// sends no more BackOff events once it runs, so the only reconciles of the event are the first sight and the end
// of the grace; the restart and the readiness are seen through the updates of the pod. Without those samples the
// end compares restart 1 with restart 0 at the first sight, finds a failing sample, and opens.
func TestBackOffRecoveredByOneRestartOpensNothing(t *testing.T) {
	h := holdHarness(t, reasonBackOff, sampleFailing)
	h.reconcileEvent("ev-hold")
	h.clock.add(10 * time.Second)
	// The kubelet restarts it: running, not ready yet.
	cur := readPod(t, h, webPod)
	cur.Status = holdPod(reasonBackOff, sampleRestart, 1).Status
	cur.Status.ContainerStatuses[0].Ready = false
	h.update(cur)
	if res := h.sampleByPod(); res.RequeueAfter != ConsequenceGrace-10*time.Second {
		t.Fatalf("requeue after %s, want the rest of the grace", res.RequeueAfter)
	}
	h.clock.add(5 * time.Second)
	// Ready, no new restart.
	h.setPodState(reasonBackOff, sampleFine, 1)
	h.sampleByPod()
	h.none()
	h.clock.set(t0.Add(ConsequenceGrace))
	h.reconcileEvent("ev-hold")
	h.none()
	if h.c.holds.len() != 0 {
		t.Fatal("the hold is kept after its end")
	}
}

// TestPodWithoutHoldEnqueuesNothing: an update of a pod no consequence is held on maps to no event, and an ended
// hold leaves nothing in the index.
func TestPodWithoutHoldEnqueuesNothing(t *testing.T) {
	h := holdHarness(t, reasonBackOff, sampleFailing)
	if reqs := h.c.heldEventsOf(t.Context(), readPod(t, h, webPod)); len(reqs) != 0 {
		t.Fatalf("enqueued %v before any hold", reqs)
	}
	h.reconcileEvent("ev-hold")
	h.clock.add(ConsequenceGrace)
	h.reconcileEvent("ev-hold")
	h.one()
	h.reconcileEvent("ev-hold")
	if reqs := h.c.heldEventsOf(t.Context(), readPod(t, h, webPod)); len(reqs) != 0 || len(h.c.holds.byPod) != 0 {
		t.Fatalf("enqueued %v after the end, index %v", reqs, h.c.holds.byPod)
	}
}

// TestLivenessLoopSampledWhileRunning: a liveness probe kills a container with no readiness probe. Each kill is a
// pod update with one more restart, and the end of the grace falls in a life where it runs and is ready. Two
// restarts during the hold open the consequence. One restart opens nothing even when two samples were failing (not
// ready at the first sight, then the restart): it is a recovery, and only the restart threshold could open it
// (ADR-0039 point 19, amended 2026-11-16).
func TestLivenessLoopSampledWhileRunning(t *testing.T) {
	for _, tc := range []struct {
		name     string
		first    sample
		restarts []int32
		open     bool
	}{
		{"one restart", sampleFailing, []int32{1}, false},
		{"two restarts", sampleFine, []int32{1, 2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := holdHarness(t, ReasonUnhealthy, tc.first)
			h.reconcileEvent("ev-hold")
			for _, r := range tc.restarts {
				h.clock.add(40 * time.Second)
				h.setPodState(ReasonUnhealthy, sampleRestart, r)
				h.sampleByPod()
			}
			h.none()
			if s := h.c.holds.holds[reqFor("ev-hold").NamespacedName]; !tc.open && (s.failing != 2 || s.restarted != 1) {
				t.Fatalf("before the end: %d failing samples and %d restarts, want 2 and 1", s.failing, s.restarted)
			}
			last := tc.restarts[len(tc.restarts)-1]
			// The end of the grace, in the same life as the last restart: running and ready.
			h.clock.set(t0.Add(ConsequenceGrace))
			h.setPodState(ReasonUnhealthy, sampleFine, last)
			h.reconcileEvent("ev-hold")
			if !tc.open {
				h.none()
				return
			}
			if o := h.one(); o.Class != ClassConsequence || o.Count != 1 {
				t.Fatalf("emitted %+v, want the consequence for the gate", o)
			}
		})
	}
}

// TestLivenessLoopWithoutReadinessOpens: a liveness probe that keeps killing a container with no readiness probe
// leaves it ready at each sample while its restart count goes up (0, 2, 4). Each sample after the first is
// failing by its restarts, so it opens (ADR-0039 point 19, amended 2026-11-16).
func TestLivenessLoopWithoutReadinessOpens(t *testing.T) {
	h := holdHarness(t, ReasonUnhealthy, sampleFine)
	h.reconcileEvent("ev-hold")
	h.none()
	for _, restarts := range []int32{2, 4} {
		h.clock.add(ConsequenceGrace / 2)
		h.setPodState(ReasonUnhealthy, sampleFine, restarts)
		h.reconcileEvent("ev-hold")
	}
	if o := h.one(); o.Class != ClassConsequence || o.Count != 1 {
		t.Fatalf("emitted %+v, want the consequence for the gate", o)
	}
}

// TestHoldOfAnOlderEventOfTheNameIsDropped: an event deleted and created again under the same name (another
// UID) between two reconciles starts its own hold; the samples of the older event's hold do not count for it.
func TestHoldOfAnOlderEventOfTheNameIsDropped(t *testing.T) {
	h := holdHarness(t, reasonBackOff, sampleFailing)
	h.reconcileEvent("ev-hold")
	old := &eventsv1.Event{}
	key := reqFor("ev-hold").NamespacedName
	if err := h.cl.Get(t.Context(), key, old); err != nil {
		t.Fatal(err)
	}
	if err := h.cl.Delete(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	h.clock.add(ConsequenceGrace)
	ev := consequence("ev-hold", reasonBackOff, appPath, h.clock.Now())
	ev.UID = "ev-hold-second"
	h.create(ev)
	// The older hold had one failing sample and its grace is over: kept, it would decide and emit now.
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != ConsequenceGrace {
		t.Fatalf("requeue after %s, want a new hold of the full grace", res.RequeueAfter)
	}
	h.none()
}

// holdOutcomeCounts reads shadewright_analyzer_held_consequences_total by outcome.
func holdOutcomeCounts() map[string]float64 {
	out := map[string]float64{}
	for _, o := range []string{holdOpened, holdNotOpened, holdReleased, holdCauseUnseen, holdDropped, holdOverflow} {
		out[o] = testutil.ToFloat64(heldConsequences.WithLabelValues(o))
	}
	return out
}

// outcomesSince returns the outcomes counted since before, leaving out those that did not change.
func outcomesSince(before map[string]float64) map[string]float64 {
	d := map[string]float64{}
	for o, v := range holdOutcomeCounts() {
		if v != before[o] {
			d[o] = v - before[o]
		}
	}
	return d
}

func wantOutcomes(t *testing.T, before map[string]float64, want map[string]float64) {
	t.Helper()
	if got := outcomesSince(before); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("outcomes counted %v, want %v", got, want)
	}
}

// TestOverflowStandsAcrossADeliveryRetry: overflow, full channel, room, retry. A consequence emitted at once for
// want of room whose delivery is retried is emitted at the retry, even when a hold has ended meanwhile and left
// room; it is counted once, as overflow, and its mark is gone once delivered. An update of its pod enqueues it
// not.
func TestOverflowStandsAcrossADeliveryRetry(t *testing.T) {
	d, rs, _ := workload()
	// A total of 4 gives a namespace share of 1.
	h := newHarness(t, Config{MaxHeldConsequences: 4, Buffer: 1}, d, rs, holdPod(reasonBackOff, sampleFailing, 0),
		consequence("ev-0", reasonBackOff, appPath, t0), consequence("ev-1", reasonBackOff, appPath, t0))
	before := holdOutcomeCounts()
	if res := h.reconcileEvent("ev-0"); res.RequeueAfter != ConsequenceGrace {
		t.Fatalf("ev-0 not held: %+v", res)
	}
	h.c.out <- Observation{Fingerprint: fillerFingerprint}
	if res := h.reconcileEvent("ev-1"); res.RequeueAfter != retryAfter {
		t.Fatalf("ev-1: requeue after %s, want the delivery retry", res.RequeueAfter)
	}
	if reqs := h.c.heldEventsOf(t.Context(), readPod(t, h, webPod)); len(reqs) != 1 || reqs[0] != reqFor("ev-0") {
		t.Fatalf("an update of the pod enqueued %v, want ev-0 alone", reqs)
	}
	// ev-0 ends (its event is deleted), which leaves room in the namespace, and the channel frees.
	ev0 := &eventsv1.Event{}
	if err := h.cl.Get(t.Context(), reqFor("ev-0").NamespacedName, ev0); err != nil {
		t.Fatal(err)
	}
	if err := h.cl.Delete(t.Context(), ev0); err != nil {
		t.Fatal(err)
	}
	h.reconcileEvent("ev-0")
	h.drain()
	h.clock.add(retryAfter)
	if res := h.reconcileEvent("ev-1"); res != (ctrl.Result{}) {
		t.Fatalf("the retry returned %+v, want the observation delivered", res)
	}
	if o := h.one(); o.Occurrence != "event:ev-ev-1" || o.Count != 1 {
		t.Fatalf("emitted %+v", o)
	}
	if h.c.holds.len() != 0 || h.c.holds.overflowLen() != 0 {
		t.Fatalf("%d held, %d overflow marks after the delivery, want none", h.c.holds.len(),
			h.c.holds.overflowLen())
	}
	wantOutcomes(t, before, map[string]float64{holdOverflow: 1, holdDropped: 1})
}

// TestOverflowMarksAreBounded: no more overflow marks than holds are kept; one past the bound is not remembered
// and the consequence is still emitted at once.
func TestOverflowMarksAreBounded(t *testing.T) {
	hs := newConsequenceHolds(4)
	for i := range 4 {
		key := types.NamespacedName{Namespace: testNS, Name: fmt.Sprintf("ev-%d", i)}
		if !hs.markOverflow(key, types.UID(key.Name)) {
			t.Fatalf("%s refused below the bound", key)
		}
	}
	key := types.NamespacedName{Namespace: testNS, Name: "one-more"}
	if hs.markOverflow(key, "one-more") || hs.wasOverflow(key, "one-more") {
		t.Fatal("remembered one over the bound")
	}
	// The mark is of the event's UID: an event created again under the name is not one.
	first := reqFor("ev-0").NamespacedName
	if hs.wasOverflow(first, "another") {
		t.Fatal("the mark of an older event of the name counts for a new one")
	}
	hs.get(first, "another")
	if hs.overflowLen() != 3 {
		t.Fatalf("%d marks, want the older event's dropped", hs.overflowLen())
	}
	hs.drop(types.NamespacedName{Namespace: testNS, Name: "ev-1"})
	hs.done(types.NamespacedName{Namespace: testNS, Name: "ev-2"})
	if hs.overflowLen() != 1 {
		t.Fatalf("%d marks, want 1 after a drop and a delivery", hs.overflowLen())
	}
}

// TestHoldOutcomesAddUp: every hold started ends in exactly one of opened, not_opened, released, cause_unseen
// and dropped.
func TestHoldOutcomesAddUp(t *testing.T) {
	getEvent := func(t *testing.T, h *harness) *eventsv1.Event {
		ev := &eventsv1.Event{}
		if err := h.cl.Get(t.Context(), reqFor("ev-hold").NamespacedName, ev); err != nil {
			t.Fatal(err)
		}
		return ev
	}
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, h *harness)
		want string
	}{
		{"opened", func(t *testing.T, h *harness) {
			h.clock.add(ConsequenceGrace)
			h.reconcileEvent("ev-hold")
		}, holdOpened},
		{"not opened", func(t *testing.T, h *harness) {
			h.clock.add(ConsequenceGrace)
			h.setPodState(reasonBackOff, sampleFine, 0)
			h.reconcileEvent("ev-hold")
		}, holdNotOpened},
		{"released by a cause", func(t *testing.T, h *harness) {
			cur := readPod(t, h, webPod)
			cur.Status.ContainerStatuses[0].State.Waiting.Reason = reasonConfig
			h.update(cur)
			h.reconcileEvent("ev-hold")
		}, holdReleased},
		{"event deleted", func(t *testing.T, h *harness) {
			if err := h.cl.Delete(t.Context(), getEvent(t, h)); err != nil {
				t.Fatal(err)
			}
			h.reconcileEvent("ev-hold")
		}, holdDropped},
		{"no longer a Warning", func(t *testing.T, h *harness) {
			ev := getEvent(t, h)
			ev.Type = corev1.EventTypeNormal
			h.update(ev)
			h.reconcileEvent("ev-hold")
		}, holdDropped},
		{"replaced by another event of the name", func(t *testing.T, h *harness) {
			if err := h.cl.Delete(t.Context(), getEvent(t, h)); err != nil {
				t.Fatal(err)
			}
			ev := consequence("ev-hold", reasonBackOff, appPath, h.clock.Now())
			ev.UID = "ev-hold-second"
			h.create(ev)
			h.reconcileEvent("ev-hold")
			// The new event's own hold ends too, so that the count of holds started is two.
			if err := h.cl.Delete(t.Context(), getEvent(t, h)); err != nil {
				t.Fatal(err)
			}
			h.reconcileEvent("ev-hold")
		}, holdDropped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := holdHarness(t, reasonBackOff, sampleFailing)
			before := holdOutcomeCounts()
			h.reconcileEvent("ev-hold")
			h.clock.add(time.Second)
			h.reconcileEvent("ev-hold")
			tc.end(t, h)
			h.drain()
			want := 1.0
			if tc.name == "replaced by another event of the name" {
				want = 2
			}
			wantOutcomes(t, before, map[string]float64{tc.want: want})
			if h.c.holds.len() != 0 {
				t.Fatalf("%d held after the end", h.c.holds.len())
			}
		})
	}
}

// TestOpenedHoldDeletedBeforeDeliveryIsNotDropped: a hold decided to open is counted opened once; its event
// deleted during the delivery retry adds no dropped.
func TestOpenedHoldDeletedBeforeDeliveryIsNotDropped(t *testing.T) {
	d, rs, _ := workload()
	h := newHarness(t, Config{Buffer: 1}, d, rs, holdPod(reasonBackOff, sampleFailing, 0),
		consequence("ev-hold", reasonBackOff, appPath, t0))
	before := holdOutcomeCounts()
	h.reconcileEvent("ev-hold")
	h.c.out <- Observation{Fingerprint: fillerFingerprint}
	h.clock.add(ConsequenceGrace)
	if res := h.reconcileEvent("ev-hold"); res.RequeueAfter != retryAfter {
		t.Fatalf("requeue after %s, want the delivery retry", res.RequeueAfter)
	}
	ev := &eventsv1.Event{}
	if err := h.cl.Get(t.Context(), reqFor("ev-hold").NamespacedName, ev); err != nil {
		t.Fatal(err)
	}
	if err := h.cl.Delete(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	h.reconcileEvent("ev-hold")
	wantOutcomes(t, before, map[string]float64{holdOpened: 1})
}

// fillerFingerprint marks the observation a test puts in the channel to fill it.
const fillerFingerprint = "filler"
