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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The rows of the decision of 2026-11-21 on #432 about evictions. Property: one eviction gives exactly one
// Incident whatever the order of pod status writes, events and pod deletion. Each test drives one order and
// requires that every observation lands on one Incident (incidentKeys) and that the eviction is counted once.

const evictionNote432 = "The node was low on resource: memory. Threshold quantity: 100Mi, available: 91236Ki. "

// evictionSetup is the web pod running, its kubelet Evicted event, and a harness that holds both.
func evictionSetup(t *testing.T) (*harness, *corev1.Pod) {
	t.Helper()
	d, rs, _ := workload()
	pod := specPod()
	pod.Status.Phase = corev1.PodRunning
	h := newHarness(t, Config{}, d, rs, pod.DeepCopy())
	h.clock.set(t0)
	ev := evictedEvent(pod.Name, pod.UID, t0)
	ev.Note = evictionNote432
	h.create(ev)
	return h, pod
}

// setEvicted writes the pod's final status: phase Failed, reason Evicted, the app container killed at at.
func setEvicted(t *testing.T, h *harness, name string, at time.Time) {
	t.Helper()
	cur := readPod(t, h, name)
	cur.Status = evictedPod(name, cur.UID, reasonError, "containerd://a", at).Status
	cur.Status.Message = evictionNote432
	h.update(cur)
}

func deletePod(t *testing.T, h *harness, name string) {
	t.Helper()
	if err := h.cl.Delete(t.Context(), readPod(t, h, name)); err != nil {
		t.Fatal(err)
	}
}

func oneEviction(t *testing.T, got []Observation) {
	t.Helper()
	var n int64
	for _, o := range got {
		n += o.Count
	}
	if keys := incidentKeys(got); len(keys) != 1 || n != 1 {
		t.Errorf("one eviction lands on %d Incidents, counted %d times, want 1 and 1: %v", len(keys), n, keys)
	}
}

// TestEvictionEventDeliversWhenThePodIsGone is finding 3: the cache shows the pod evicted when the event is
// read, and the pod is deleted before the pod reconciler delivers the eviction. The event was committed as
// handled on what the cache showed, and the eviction was lost.
func TestEvictionEventDeliversWhenThePodIsGone(t *testing.T) {
	h, pod := evictionSetup(t)
	setEvicted(t, h, pod.Name, t0)
	h.reconcileEvent("ev-evicted")
	got := h.drain()
	deletePod(t, h, pod.Name)
	h.reconcilePod(pod.Name)
	h.clock.set(t0.Add(time.Minute))
	h.reconcileEvent("ev-evicted")
	got = append(got, h.drain()...)
	oneEviction(t, got)
}

// TestEvictionDeliveredOnceInEveryOrder: the pod reconciler and the event reconciler each deliver the eviction
// when they come first and skip it when the other did, so it is counted once in either order, and a late
// status after the event's wait (findings 12 and 13) adds nothing.
func TestEvictionDeliveredOnceInEveryOrder(t *testing.T) {
	t.Run("pod first", func(t *testing.T) {
		h, pod := evictionSetup(t)
		setEvicted(t, h, pod.Name, t0)
		h.reconcilePod(pod.Name)
		got := h.drain()
		h.reconcileEvent("ev-evicted")
		oneEviction(t, append(got, h.drain()...))
	})
	t.Run("event first", func(t *testing.T) {
		h, pod := evictionSetup(t)
		setEvicted(t, h, pod.Name, t0)
		h.reconcileEvent("ev-evicted")
		got := h.drain()
		h.reconcilePod(pod.Name)
		oneEviction(t, append(got, h.drain()...))
	})
	t.Run("status after the wait", func(t *testing.T) {
		h, pod := evictionSetup(t)
		h.reconcileEvent("ev-evicted")
		h.clock.set(t0.Add(EvictionSettleGrace))
		h.reconcileEvent("ev-evicted")
		got := h.drain()
		h.clock.set(t0.Add(EvictionSettleGrace + time.Minute))
		setEvicted(t, h, pod.Name, t0.Add(EvictionSettleGrace+30*time.Second))
		h.reconcilePod(pod.Name)
		oneEviction(t, append(got, h.drain()...))
	})
	t.Run("other reconciler delivering", func(t *testing.T) {
		h, pod := evictionSetup(t)
		setEvicted(t, h, pod.Name, t0)
		if prior, _ := h.c.evictions.claim(pod.UID, nil); prior != evictionNotDelivered {
			t.Fatalf("claim of a new eviction: %v", prior)
		}
		if res := h.reconcileEvent("ev-evicted"); res.RequeueAfter != retryAfter {
			t.Errorf("event while the pod reconciler delivers: requeue after %v, want %v", res.RequeueAfter,
				retryAfter)
		}
		if res := h.reconcilePod(pod.Name); res.RequeueAfter != retryAfter {
			t.Errorf("pod while the event reconciler delivers: requeue after %v, want %v", res.RequeueAfter,
				retryAfter)
		}
		h.none()
		h.c.evictions.finish(pod.UID, false) // that delivery failed: the next try delivers
		h.reconcileEvent("ev-evicted")
		got := h.drain()
		h.reconcilePod(pod.Name)
		oneEviction(t, append(got, h.drain()...))
	})
}

// TestEvictionCountsOnAReportedTermination is finding 2, the fallback row: the kubelet writes the killed
// container (exit code 137, phase still Running) before the pod's final phase. The DisruptionTarget condition
// is not in that write (kubelet status_manager.go, mergePodStatus, v1.37.2), so the termination is reported as
// a cause. The eviction that shows next counts on that termination's Incident and opens none, read from the
// pod's status or, when the pod is gone, from the event.
func TestEvictionCountsOnAReportedTermination(t *testing.T) {
	intermediate := func(t *testing.T, h *harness, name string) Observation {
		t.Helper()
		cur := readPod(t, h, name)
		killed := evictedPod(name, cur.UID, reasonError, "containerd://a", t0.Add(5*time.Second)).Status
		killed.Phase, killed.Reason, killed.Message = corev1.PodRunning, "", ""
		cur.Status = killed
		h.update(cur)
		h.clock.set(t0.Add(6 * time.Second))
		h.reconcilePod(name)
		o := h.one()
		if o.Reason != reasonError {
			t.Fatalf("intermediate status gave %s, want the termination", o.Reason)
		}
		return o
	}
	t.Run("from pod status", func(t *testing.T) {
		h, pod := evictionSetup(t)
		h.reconcileEvent("ev-evicted") // waits for the status
		term := intermediate(t, h, pod.Name)
		setEvicted(t, h, pod.Name, t0.Add(5*time.Second))
		h.clock.set(t0.Add(8 * time.Second))
		h.reconcilePod(pod.Name)
		got := h.drain()
		h.reconcileEvent("ev-evicted")
		got = append(got, h.drain()...)
		if len(got) != 1 || got[0].Reason != ReasonEvicted || got[0].Fingerprint != term.Fingerprint {
			t.Fatalf("eviction after a reported termination: %+v, want one eviction on %s", got, term.Fingerprint)
		}
		oneEvictionIncident(t, append(got, term))
	})
	t.Run("pod gone", func(t *testing.T) {
		h, pod := evictionSetup(t)
		h.reconcileEvent("ev-evicted")
		term := intermediate(t, h, pod.Name)
		setEvicted(t, h, pod.Name, t0.Add(5*time.Second))
		deletePod(t, h, pod.Name)
		h.reconcilePod(pod.Name)
		h.clock.set(t0.Add(20 * time.Second))
		h.reconcileEvent("ev-evicted")
		got := h.drain()
		if len(got) != 1 || got[0].Reason != ReasonEvicted || got[0].Fingerprint != term.Fingerprint {
			t.Fatalf("eviction of a gone pod after a reported termination: %+v, want one eviction on %s", got,
				term.Fingerprint)
		}
	})
	t.Run("an older crash of the pod is no cause of the eviction", func(t *testing.T) {
		h, pod := evictionSetup(t)
		term := intermediate(t, h, pod.Name)
		// The container restarted and runs; the eviction then kills it with another container ID.
		cur := readPod(t, h, pod.Name)
		cur.Status.ContainerStatuses[0].LastTerminationState = cur.Status.ContainerStatuses[0].State
		cur.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		h.update(cur)
		h.reconcilePod(pod.Name)
		h.none()
		cur = readPod(t, h, pod.Name)
		cur.Status = evictedPod(pod.Name, pod.UID, reasonError, "containerd://b", t0.Add(time.Minute)).Status
		cur.Status.Message = evictionNote432
		h.update(cur)
		h.reconcilePod(pod.Name)
		if o := h.one(); o.Fingerprint == term.Fingerprint {
			t.Errorf("eviction counted on an earlier crash %s of its pod", term.Fingerprint)
		}
	})
}

// oneEvictionIncident: every observation lands on one Incident.
func oneEvictionIncident(t *testing.T, got []Observation) {
	t.Helper()
	if keys := incidentKeys(got); len(keys) != 1 {
		t.Errorf("one eviction lands on %d Incidents, want 1: %v", len(keys), keys)
	}
}

// TestEvictionLogsDroppedTerminations is finding 4: a container termination not handled when the pod shows
// Evicted is dropped, as before, and the collector now says so in its log.
func TestEvictionLogsDroppedTerminations(t *testing.T) {
	h, pod := evictionSetup(t)
	setEvicted(t, h, pod.Name, t0)
	var mu sync.Mutex
	var lines []string
	log := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, args)
	}, funcr.Options{})
	ctx := logr.NewContext(t.Context(), log)
	if _, err := h.pods.Reconcile(ctx, reqFor(pod.Name)); err != nil {
		t.Fatal(err)
	}
	h.one()
	mu.Lock()
	defer mu.Unlock()
	for _, l := range lines {
		if strings.Contains(l, "Dropped container terminations of an evicted Pod") &&
			strings.Contains(l, `"terminations"=1`) {
			return
		}
	}
	t.Errorf("no log line for the dropped termination: %q", lines)
}

// TestEvictionWithoutFinishTime is finding 5: an evicted pod whose container shows ContainerStatusUnknown with
// no finishedAt. The eviction's time is the DisruptionTarget condition's, else the time the collector read the
// pod, and it is new to this process: not MayRepeat, counted once.
func TestEvictionWithoutFinishTime(t *testing.T) {
	for _, withCondition := range []bool{true, false} {
		h, pod := evictionSetup(t)
		h.reconcilePod("none") // the collector starts at t0
		cur := readPod(t, h, pod.Name)
		cur.Status = evictedPod(pod.Name, pod.UID, "ContainerStatusUnknown", "", t0).Status
		cur.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.Time{}
		cur.Status.ContainerStatuses[0].State.Terminated.StartedAt = metav1.Time{}
		want := t0.Add(time.Hour)
		if withCondition {
			want = t0.Add(30 * time.Minute)
			cur.Status.Conditions = append(cur.Status.Conditions, corev1.PodCondition{
				Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: "TerminationByKubelet",
				LastTransitionTime: metav1.NewTime(want),
			})
		}
		h.update(cur)
		h.clock.set(t0.Add(time.Hour))
		h.reconcilePod(pod.Name)
		o := h.one()
		if !o.Began.Equal(want) || o.MayRepeat || o.Count != 1 {
			t.Errorf("condition %t: began %v, MayRepeat %t, count %d; want %v, false, 1", withCondition, o.Began,
				o.MayRepeat, o.Count, want)
		}
	}
}
