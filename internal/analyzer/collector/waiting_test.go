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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

const reasonConfig = "CreateContainerConfigError"

// waitingPod is the web pod whose app container waits with reason before any restart, with a Handled that
// has counted nothing, so a first report counts 1.
func waitingPod(t *testing.T, reason string) (*harness, *corev1.Pod) {
	t.Helper()
	d, rs, _ := workload()
	pod := specPod()
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerApp,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}}
	none := func(context.Context, string, time.Time) (int64, bool, error) { return 0, false, nil }
	return newHarness(t, Config{Handled: none}, d, rs, pod), pod
}

// waits returns the waiting slots the tracker keeps for the web pod.
func waits(h *harness) map[string]waitState {
	h.c.pods.mu.Lock()
	defer h.c.pods.mu.Unlock()
	s, _ := h.c.pods.seen.get(types.NamespacedName{Namespace: testNS, Name: webPod})
	return s.waits
}

// A waiting cause still shown, with no pod update, is reported again by a requeue once per SettleAfter, as a
// repeat of the same occurrence: Began is the collector's first sight of it, ObservedAt the read, and the
// repeat counts nothing (ADR-0039 point 4, amended 2026-11-16).
func TestLastingWaitIsReportedAgain(t *testing.T) {
	h, pod := waitingPod(t, reasonConfig)
	res := h.reconcilePod(pod.Name)
	first := h.one()
	if !first.Began.Equal(t0) || !first.ObservedAt.Equal(t0) || first.Count != 1 || first.MayRepeat {
		t.Fatalf("first report %+v, want Began and ObservedAt %v and a count of 1", first, t0)
	}
	if res.RequeueAfter != SettleAfter {
		t.Fatalf("requeue after %v, want %v so that a wait with no pod update is reported again",
			res.RequeueAfter, SettleAfter)
	}
	for i := 1; i <= 2; i++ {
		h.clock.add(SettleAfter)
		res = h.reconcilePod(pod.Name)
		got := h.one()
		at := t0.Add(time.Duration(i) * SettleAfter)
		if got.Occurrence != first.Occurrence || got.Fingerprint != first.Fingerprint || !got.Began.Equal(t0) ||
			!got.ObservedAt.Equal(at) || got.Count != 0 || !got.MayRepeat || got.Total != 1 || got.Immediate {
			t.Fatalf("repeat %d: %+v, want the same occurrence, Began %v, ObservedAt %v, nothing to count", i, got,
				t0, at)
		}
		if res.RequeueAfter != SettleAfter {
			t.Errorf("repeat %d: requeue after %v, want %v", i, res.RequeueAfter, SettleAfter)
		}
		// At most once per SettleAfter: a pod update right after reports nothing.
		h.update(pod)
		h.reconcilePod(pod.Name)
		h.none()
	}
}

// A wait shorter than SettleAfter is reported once and not again: the requeue keeps its delay and no pod
// update shortens it.
func TestShortWaitIsNotReportedAgain(t *testing.T) {
	h, pod := waitingPod(t, reasonImagePull)
	h.reconcilePod(pod.Name)
	if got := h.one(); !got.Began.Equal(t0) {
		t.Fatalf("first report Began %v, want the collector's first sight %v", got.Began, t0)
	}
	h.clock.add(time.Minute)
	h.update(pod)
	res := h.reconcilePod(pod.Name)
	h.none()
	if res.RequeueAfter != SettleAfter-time.Minute {
		t.Errorf("requeue after %v, want the rest of SettleAfter, %v", res.RequeueAfter, SettleAfter-time.Minute)
	}
	// The pull succeeds: nothing more is reported, and no look again is asked for.
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	h.update(pod)
	h.clock.add(SettleAfter)
	if res := h.reconcilePod(pod.Name); res.RequeueAfter != 0 {
		t.Errorf("requeue after %v once the wait ended, want none", res.RequeueAfter)
	}
	h.none()
}

// The tracker keeps one first-seen time per waiting slot and drops it when the wait ends or its ID changes
// (another reason, a restart), so a wait that comes back starts its own time, and when the pod is gone.
func TestWaitStateIsDroppedOnChange(t *testing.T) {
	h, pod := waitingPod(t, reasonConfig)
	h.reconcilePod(pod.Name)
	h.one()
	if w := waits(h); len(w) != 1 {
		t.Fatalf("waiting slots %v, want one", w)
	}
	// The wait ends: the slot is dropped.
	h.clock.add(time.Minute)
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	h.update(pod)
	h.reconcilePod(pod.Name)
	if w := waits(h); len(w) != 0 {
		t.Fatalf("waiting slots %v after the wait ended, want none", w)
	}
	// The same wait shown again is handled, but its time starts over: its repeat comes SettleAfter after it
	// came back, with that Began.
	back := t0.Add(time.Minute)
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: reasonConfig}}
	h.update(pod)
	h.reconcilePod(pod.Name)
	h.none()
	h.clock.add(SettleAfter)
	h.reconcilePod(pod.Name)
	if got := h.one(); !got.Began.Equal(back) || got.Count != 0 {
		t.Fatalf("repeat of the wait that came back: %+v, want Began %v and nothing to count", got, back)
	}
	// A restart changes the ID: a new occurrence with its own Began, and still one slot.
	restarted := h.clock.Now().Add(time.Second)
	h.clock.set(restarted)
	pod.Status.ContainerStatuses[0].RestartCount = 1
	h.update(pod)
	h.reconcilePod(pod.Name)
	if got := h.one(); got.Count != 1 || !got.Began.Equal(restarted) {
		t.Fatalf("wait after a restart: %+v, want a new occurrence that began %v", got, restarted)
	}
	if w := waits(h); len(w) != 1 || !w["waiting:"+containerApp].began.Equal(restarted) {
		t.Fatalf("waiting slots %v, want one that began %v", w, restarted)
	}
	// The pod is gone: its state is dropped.
	if err := h.cl.Delete(h.t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	h.reconcilePod(pod.Name)
	if n := h.c.pods.len(); n != 0 {
		t.Fatalf("state of %d pods after the pod was deleted, want none", n)
	}
}

// A wait's Began is the collector's clock, not a finish time the cluster reported: dropping its pod's state
// does not mark later terminations of other pods MayRepeat.
func TestWaitDoesNotMoveTheEvictionBound(t *testing.T) {
	tr := newTerminationTracker(1)
	later := t0.Add(time.Hour)
	a := types.NamespacedName{Namespace: testNS, Name: "a"}
	wait := termination{container: containerApp, id: "waiting-" + reasonConfig + "-restarts-0", reason: reasonConfig,
		waiting: true}
	p := tr.pending(a, "a", []termination{wait}, t0, later)
	tr.commit(a, "a", p[0].termination, later)
	tr.pending(types.NamespacedName{Namespace: testNS, Name: "b"}, "b", nil, t0, later) // drops the state of a
	c := types.NamespacedName{Namespace: testNS, Name: "c"}
	got := tr.pending(c, "c", []termination{{container: containerApp, id: "1", finished: t0.Add(time.Minute)}}, t0,
		later)
	if len(got) != 1 || got[0].mayRepeat {
		t.Fatalf("termination of another pod after the wait's pod was dropped: %+v, want one, not mayRepeat", got)
	}
}

// A pod update that clears a wait passes the watch's predicate, so the reconcile drops the wait's slot and a
// wait shown again starts its own time. Were the update dropped, a wait of 30 seconds shown again after a
// pause would keep the first sight of the earlier wait as its Began and prove minutes of recurrence.
func TestWaitClearedByAnUpdateStartsOver(t *testing.T) {
	h, pod := waitingPod(t, reasonConfig)
	pred := podPredicate()
	// apply updates the pod and reconciles it when the watch's predicate lets the update through.
	apply := func(state corev1.ContainerState) {
		t.Helper()
		old := pod.DeepCopy()
		pod.Status.ContainerStatuses[0].State = state
		h.update(pod)
		if pred.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: pod.DeepCopy()}) {
			h.reconcilePod(pod.Name)
		}
	}
	if !pred.Create(event.CreateEvent{Object: pod}) {
		t.Fatal("the predicate drops a waiting pod")
	}
	h.reconcilePod(pod.Name)
	h.one()
	h.clock.add(time.Minute)
	apply(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}})
	h.none()
	h.clock.add(30 * time.Second)
	back := h.clock.Now()
	apply(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonConfig}})
	h.none()
	h.clock.add(SettleAfter)
	h.reconcilePod(pod.Name)
	if got := h.one(); !got.Began.Equal(back) {
		t.Fatalf("repeat of the wait shown again: Began %v, want %v, when it was shown again", got.Began, back)
	}
	// An update with no symptom before or after is still filtered.
	running := pod.DeepCopy()
	running.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	if pred.Update(event.UpdateEvent{ObjectOld: running, ObjectNew: running.DeepCopy()}) {
		t.Error("the predicate lets through an update of a pod with no symptom before or after")
	}
}
