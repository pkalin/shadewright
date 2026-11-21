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
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
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

// TestEvictionEventOfAGonePod: the cache shows the pod evicted when the event is read, and the pod is deleted
// before the pod reconciler delivers the eviction. The event waits for the pod reconciler while the status shows
// the eviction, and with the pod gone it is the ordinary Event symptom it was on main: counted once, never lost.
func TestEvictionEventOfAGonePod(t *testing.T) {
	h, pod := evictionSetup(t)
	setEvicted(t, h, pod.Name, t0)
	if res := h.reconcileEvent("ev-evicted"); res.RequeueAfter != retryAfter {
		t.Fatalf("event while the status shows an undelivered eviction: requeue after %v, want %v",
			res.RequeueAfter, retryAfter)
	}
	got := h.drain()
	deletePod(t, h, pod.Name)
	h.reconcilePod(pod.Name)
	h.clock.set(t0.Add(time.Minute))
	h.reconcileEvent("ev-evicted")
	got = append(got, h.drain()...)
	oneEviction(t, got)
	if got[0].Origin != fingerprint.SourceEvent || got[0].Class != ClassNone {
		t.Errorf("event of a gone pod: origin %q class %q, want an Event symptom with no class", got[0].Origin,
			got[0].Class)
	}
}

// TestEvictionDeliveredOnceInEitherOrder: the pod reconciler delivers the eviction from the pod's status, and
// the event is skipped once it did, whichever was reconciled first.
func TestEvictionDeliveredOnceInEitherOrder(t *testing.T) {
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
		got = append(got, h.drain()...)
		h.clock.set(t0.Add(retryAfter))
		h.reconcileEvent("ev-evicted")
		got = append(got, h.drain()...)
		oneEviction(t, got)
		if got[0].Origin != fingerprint.SourcePodStatus || got[0].Class != ClassCause {
			t.Errorf("eviction: origin %q class %q, want the pod-status cause", got[0].Origin, got[0].Class)
		}
	})
}

// TestForgedEvictedEventHidesNoEviction (decided 2026-11-21): a kubelet Evicted event about a running pod, with
// the pod's real UID, whose status does not show an eviction within EvictionSettleGrace (here forged: the pod
// runs on). It is then an ordinary Event symptom and marks nothing about the pod. When the pod is evicted for
// real later, its status delivers the eviction as the cause. Before, the event delivered the pod's eviction
// observation from its note, marked it delivered, and the real one was never delivered.
func TestForgedEvictedEventHidesNoEviction(t *testing.T) {
	h, pod := evictionSetup(t)
	h.reconcileEvent("ev-evicted")
	h.none()
	h.clock.set(t0.Add(EvictionSettleGrace))
	h.reconcileEvent("ev-evicted")
	forged := h.one()
	if forged.Origin != fingerprint.SourceEvent || forged.Class != ClassNone {
		t.Errorf("forged event: origin %q class %q, want an Event symptom with no class", forged.Origin,
			forged.Class)
	}
	h.clock.set(t0.Add(EvictionSettleGrace + time.Minute))
	setEvicted(t, h, pod.Name, t0.Add(EvictionSettleGrace+30*time.Second))
	h.reconcilePod(pod.Name)
	got := h.drain()
	if len(got) != 1 || got[0].Reason != ReasonEvicted || got[0].Origin != fingerprint.SourcePodStatus ||
		got[0].Class != ClassCause || got[0].Count != 1 {
		t.Fatalf("real eviction after a forged event: %+v, want the eviction from pod status as the cause", got)
	}
	if got[0].Fingerprint == forged.Fingerprint {
		t.Errorf("the real eviction landed on the forged event's fingerprint %s", forged.Fingerprint)
	}
}

// TestEvictedEventOfAGonePodListedAtStartIsFoldOnly: an old kubelet Evicted event of a pod that is gone, listed
// at start, keeps the mark of an old event of a gone pod (FoldOnly) and opens no Incident, as any other event.
// Before, the event delivered the pod's eviction observation, built anew, and the mark was lost.
func TestEvictedEventOfAGonePodListedAtStartIsFoldOnly(t *testing.T) {
	h := newHarness(t, Config{})
	ev := evictedEvent(specPod().Name, specPod().UID, t0.Add(-20*time.Minute))
	ev.CreationTimestamp = metav1.NewTime(t0.Add(-20 * time.Minute))
	h.create(ev)
	stored := &eventsv1.Event{}
	if err := h.cl.Get(t.Context(), client.ObjectKeyFromObject(ev), stored); err != nil {
		t.Fatal(err)
	}
	listAtStart(h, stored)
	h.reconcileEvent(ev.Name)
	got := h.one()
	if !got.FoldOnly || got.Origin != fingerprint.SourceEvent || got.Occurrence != "event:"+string(stored.UID) {
		t.Fatalf("old Evicted event of a gone pod: %+v, want a FoldOnly Event symptom", got)
	}
}

// TestEvictedEventKeepsItsFingerprint: an Evicted event handled as a symptom has the fingerprint, origin and
// occurrence any event had on main (ADR-0039 point 1), built here from the event's fields as fromEvent builds
// every event's, and not the pod-status fingerprint of the pod's eviction.
func TestEvictedEventKeepsItsFingerprint(t *testing.T) {
	h, pod := evictionSetup(t)
	h.reconcileEvent("ev-evicted") // waits for the pod's status
	h.none()
	stored := &eventsv1.Event{}
	if err := h.cl.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "ev-evicted"}, stored); err != nil {
		t.Fatal(err)
	}
	h.clock.set(t0.Add(EvictionSettleGrace))
	h.reconcileEvent("ev-evicted")
	got := h.one()
	d, _, _ := workload()
	want := fingerprint.Fingerprint(fingerprint.Observation{
		ClusterID: h.c.cfg.ClusterID, Namespace: testNS, OwnerUID: string(d.UID), ResourceUID: string(pod.UID),
		Reason: stored.Reason, Action: stored.Action, Type: stored.Type,
		ReportingController: stored.ReportingController, Note: stored.Note, RegardingKind: stored.Regarding.Kind,
		RegardingName: stored.Regarding.Name, Source: fingerprint.SourceEvent,
	})
	if got.Fingerprint != want || got.Origin != fingerprint.SourceEvent ||
		got.Occurrence != "event:"+string(stored.UID) || got.Class != ClassNone {
		t.Errorf("Evicted event: fingerprint %s origin %q occurrence %s class %q; want %s, event, event:%s, none",
			got.Fingerprint, got.Origin, got.Occurrence, got.Class, want, stored.UID)
	}
}

// TestConsequenceOfAnEvictedPod (decided 2026-11-21): the terminations of an evicted pod are dropped, so a
// BackOff of its container never links to one of them. It links to the eviction once the pod reconciler
// delivered it, and to nothing before. Before, it linked to the dropped termination, whose Incident never opens.
func TestConsequenceOfAnEvictedPod(t *testing.T) {
	h, pod := evictionSetup(t)
	setEvicted(t, h, pod.Name, t0)
	backOff := consequence("ev-backoff", reasonBack, "spec.containers{app}", t0.Add(time.Second))
	h.create(backOff)
	read := func() Observation {
		t.Helper()
		stored := &eventsv1.Event{}
		if err := h.cl.Get(t.Context(), client.ObjectKeyFromObject(backOff), stored); err != nil {
			t.Fatal(err)
		}
		o, _, _, err := h.c.fromEvent(t.Context(), stored)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	if o := read(); o.Class != ClassConsequence || o.CauseFingerprint != "" {
		t.Fatalf("BackOff before the eviction is delivered: class %q cause %q, want a consequence with none",
			o.Class, o.CauseFingerprint)
	}
	h.reconcilePod(pod.Name)
	eviction := h.one()
	if o := read(); o.CauseFingerprint != eviction.Fingerprint {
		t.Errorf("BackOff after the eviction is delivered: cause %q, want the eviction's %s", o.CauseFingerprint,
			eviction.Fingerprint)
	}
}

// TestEvictionCountsOnAReportedTermination is finding 2, the fallback row: the kubelet writes the killed
// container (exit code 137, phase still Running) before the pod's final phase. The DisruptionTarget condition
// is not in that write (kubelet status_manager.go, mergePodStatus, v1.37.2), so the termination is reported as
// a cause. The eviction that shows next in the pod's status counts on that termination's Incident and opens
// none. When the pod is gone before its status was read, the event is the ordinary Event symptom with its own
// fingerprint: one more Incident, never one less (decided 2026-11-21).
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
		if len(got) != 1 || got[0].Reason != ReasonEvicted || got[0].Origin != fingerprint.SourceEvent ||
			got[0].Fingerprint == term.Fingerprint {
			t.Fatalf("Evicted event of a gone pod after a reported termination: %+v, want an Event symptom of its own",
				got)
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
