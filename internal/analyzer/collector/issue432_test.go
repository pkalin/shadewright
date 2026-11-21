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
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
)

// The reproductions of #432 seen on rc.3 outside the lab (2026-11-20): one cause of one Deployment opened
// several Incidents. Each test drives the collector with the pod states and events of the case, in their order
// and at their times (t0 stands for the first sighting of the case), and requires that every observation lands
// on one Incident, its own fingerprint or, for a linked consequence, its cause's.
//
// Properties (ADR-0039 point 3, amended 2026-11-20), one line each:
//   - P1: one crash loop of one container (non-zero exits, BackOff within KillCauseWindow) lands on one Incident.
//   - P2: two faults of one container more than KillCauseWindow apart land on two Incidents, each consequence
//     on the fault within the window, and a consequence past the window on none.
//   - P3: the evicted pods of one workload land on one Incident, the eviction's read from pod status; their
//     containers' terminations and the kubelet's Evicted events open none of their own.
//   - P4: an evicted pod's message is kept in the eviction's symptom.
//   - P5: an Evicted event alone hides nothing: about a pod whose status never shows the eviction, or about
//     a pod that is gone, it delivers the pod's eviction observation; it is skipped only once that observation
//     was delivered (decided 2026-11-21).

// incidentKeys returns the Incident each observation lands on: the cause's fingerprint for a consequence the
// collector linked, its own fingerprint otherwise.
func incidentKeys(obs []Observation) map[string][]string {
	keys := map[string][]string{}
	for _, o := range obs {
		k := o.Fingerprint
		if o.Class == ClassConsequence && o.CauseFingerprint != "" {
			k = o.CauseFingerprint
		}
		keys[k] = append(keys[k], o.Reason+"/"+string(o.Class))
	}
	return keys
}

// crashLoopPod is the web pod whose app container exits with code 2 (an unknown flag), finished at finished,
// after restarts restarts, and now waits in CrashLoopBackOff.
func crashLoopPod(finished time.Time, restarts int32) *corev1.Pod {
	p := holdPod(reasonBack, sampleFailing, restarts)
	p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{
			ContainerID: fmt.Sprintf("containerd://crash-%d", restarts), Reason: reasonError, ExitCode: 2,
			StartedAt: metav1.NewTime(finished), FinishedAt: metav1.NewTime(finished),
		}}
	return p
}

// TestIssue432CrashLoopBackOffOpensASecondIncident is cases (a) and (c) of #432 (P1): a container exits with
// code 2 (reason Error, read from pod status) and the kubelet reports BackOff for it. On main the termination
// was a symptom of no class and the BackOff found no cause in the pod status, so it was held for
// ConsequenceGrace and emitted unlinked: a second Incident about 2 minutes after the first. A non-zero exit is
// now a cause, and the BackOff links to the newest termination within KillCauseWindow of its own time, on
// either side: here the status already shows a crash 13 seconds after the event.
func TestIssue432CrashLoopBackOffOpensASecondIncident(t *testing.T) {
	// rc.3 (a): Error Incident 20:27:28Z, BackOff Incident 20:29:15Z. t0 stands for 20:25:00Z.
	d, rs, _ := workload()
	ev := consequence("ev-backoff", reasonBack, appPath, t0.Add(2*time.Minute+15*time.Second))
	ev.Series = nil
	h := newHarness(t, Config{}, d, rs, crashLoopPod(t0.Add(20*time.Second), 1), ev)

	got := make([]Observation, 0, 3)
	// 20:25:20 first crash, read from pod status.
	h.clock.set(t0.Add(20 * time.Second))
	h.reconcilePod(webPod)
	got = append(got, h.drain()...)
	// 20:27:28 a later crash of the same container: the gate opens the Error Incident on this sighting.
	h.clock.set(t0.Add(2*time.Minute + 28*time.Second))
	cur := readPod(t, h, webPod)
	cur.Status = crashLoopPod(t0.Add(2*time.Minute+28*time.Second), 5).Status
	h.update(cur)
	h.reconcilePod(webPod)
	got = append(got, h.drain()...)
	// 20:27:15 the kubelet's BackOff, then the samples of a hold, if any, to the end of ConsequenceGrace.
	for _, at := range []time.Duration{2*time.Minute + 15*time.Second, 3*time.Minute + 15*time.Second,
		4*time.Minute + 15*time.Second} {
		h.clock.set(t0.Add(at))
		h.reconcileEvent("ev-backoff")
		got = append(got, h.drain()...)
	}

	if len(got) < 3 {
		t.Fatalf("got %d observations, want the two crashes and the BackOff: %+v", len(got), got)
	}
	if keys := incidentKeys(got); len(keys) != 1 {
		t.Errorf("one crash loop lands on %d Incidents, want 1: %v", len(keys), keys)
	}
}

// TestIssue432TwoFaultsOpenTwoIncidents is P2: a container crashes with exit code 2, then, more than
// KillCauseWindow later, with exit code 1. Each BackOff counts on the crash within the window of its time,
// and a BackOff past the window of the last crash links to nothing. Two faults stay two Incidents.
func TestIssue432TwoFaultsOpenTwoIncidents(t *testing.T) {
	d, rs, _ := workload()
	pod := crashLoopPod(t0, 1)
	h := newHarness(t, Config{}, d, rs, pod)
	var got []Observation
	crash := func(exit int32, restarts int32, at time.Time) Observation {
		h.clock.set(at)
		cur := readPod(t, h, webPod)
		cur.Status = crashLoopPod(at, restarts).Status
		cur.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = exit
		h.update(cur)
		h.reconcilePod(webPod)
		o := h.one()
		got = append(got, o)
		return o
	}
	backOff := func(name string, at time.Time) Observation {
		h.clock.set(at)
		ev := consequence(name, reasonBack, appPath, at)
		ev.Series = nil
		h.create(ev)
		h.reconcileEvent(name)
		o := h.one()
		got = append(got, o)
		return o
	}
	first := crash(2, 2, t0.Add(time.Minute))
	if b := backOff("ev-first", t0.Add(90*time.Second)); b.CauseFingerprint != first.Fingerprint {
		t.Errorf("BackOff of the first fault links to %q, want %q", b.CauseFingerprint, first.Fingerprint)
	}
	second := crash(1, 3, t0.Add(time.Minute+KillCauseWindow+time.Minute))
	if b := backOff("ev-second", t0.Add(time.Minute+KillCauseWindow+90*time.Second)); b.CauseFingerprint != second.Fingerprint {
		t.Errorf("BackOff of the second fault links to %q, want %q", b.CauseFingerprint, second.Fingerprint)
	}
	if first.Fingerprint == second.Fingerprint {
		t.Fatalf("two faults share the fingerprint %s", first.Fingerprint)
	}
	if keys := incidentKeys(got); len(keys) != 2 {
		t.Errorf("two faults land on %d Incidents, want 2: %v", len(keys), keys)
	}
	late := t0.Add(time.Minute + 2*KillCauseWindow + 2*time.Minute)
	h.clock.set(late)
	ev := consequence("ev-late", reasonBack, appPath, late)
	ev.Series = nil
	h.create(ev)
	h.reconcileEvent("ev-late")
	for _, o := range h.drain() {
		if o.CauseFingerprint != "" {
			t.Errorf("BackOff past KillCauseWindow of the last crash links to %q, want none", o.CauseFingerprint)
		}
	}
}

// evictedPod is a pod of the web ReplicaSet named name with uid, evicted by the kubelet for its ephemeral
// storage: phase Failed, reason Evicted, its app container terminated with reason and exit code 137 at at.
func evictedPod(name string, uid types.UID, reason, containerID string, at time.Time) *corev1.Pod {
	p := specPod()
	p.Name, p.UID = name, uid
	p.Status.Phase, p.Status.Reason = corev1.PodFailed, "Evicted"
	p.Status.Message = "Pod ephemeral local storage usage exceeds the total limit of containers 1Gi."
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: containerApp,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ContainerID: containerID, Reason: reason, ExitCode: exitOOM,
			StartedAt: metav1.NewTime(at.Add(-time.Minute)), FinishedAt: metav1.NewTime(at),
		}},
	}}
	return p
}

// evictedRef is what the kubelet's Evicted Warning regards: pod name with uid, with no field path, so it
// names no container.
func evictedRef(pod string, uid types.UID) corev1.ObjectReference {
	return corev1.ObjectReference{APIVersion: apiCore, Kind: kindPod, Namespace: testNS, Name: pod, UID: uid}
}

// TestIssue432EvictionOpensThreeIncidents is case (b) of #432 (P3, P4): a container over its ephemeral-storage limit is
// evicted, its Deployment re-creates the pod and the new pod is evicted again. Each evicted pod shows its
// container terminated with exit code 137, with reason Error (killed) or ContainerStatusUnknown (the kubelet
// could not find it when the pod ended), and the kubelet reports Evicted about the pod. All three are
// observations of no class on main, with fingerprints that differ by reason, so each opened an Incident of
// its own once its second pod repeated it. The pod's status.reason Evicted is now the cause of the pod: one
// observation per evicted pod, with the kubelet's message, and the Evicted event about a pod whose status
// shows it is skipped.
func TestIssue432EvictionOpensThreeIncidents(t *testing.T) {
	// rc.3 (b): Error 21:14:27Z, ContainerStatusUnknown 21:16:24Z, Evicted 21:17:26Z. t0 stands for 21:12:00Z.
	d, rs, _ := workload()
	const podA, podB, podC = "web-7d4b9c8f6-aaaaa", "web-7d4b9c8f6-bbbbb", "web-7d4b9c8f6-ccccc"
	uidA, uidB, uidC := types.UID("aaaaaaaa-0000-4000-8000-000000000001"),
		types.UID("bbbbbbbb-0000-4000-8000-000000000002"), types.UID("cccccccc-0000-4000-8000-000000000003")
	h := newHarness(t, Config{}, d, rs)
	var got []Observation
	evict := func(name string, uid types.UID, reason, cid string, at time.Time) {
		h.clock.set(at)
		h.create(evictedPod(name, uid, reason, cid, at))
		h.reconcilePod(name)
		got = append(got, h.drain()...)
		ev := warning("ev-evicted-"+name, 0)
		ev.Reason, ev.Action = "Evicted", ""
		ev.Note = "Pod ephemeral local storage usage exceeds the total limit of containers 1Gi."
		ev.EventTime = metav1.NewMicroTime(at)
		ev.Regarding = evictedRef(name, uid)
		h.create(ev)
		h.reconcileEvent(ev.Name)
		got = append(got, h.drain()...)
	}
	// Pod A evicted (Error 137), re-created as B, evicted (Error 137, opens Error at 21:14:27), re-created as C,
	// evicted while its container could not be found (ContainerStatusUnknown).
	evict(podA, uidA, reasonError, "containerd://a", t0.Add(20*time.Second))
	evict(podB, uidB, reasonError, "containerd://b", t0.Add(2*time.Minute+27*time.Second))
	evict(podC, uidC, "ContainerStatusUnknown", "", t0.Add(4*time.Minute+24*time.Second))

	if len(got) != 3 {
		t.Fatalf("got %d observations, want one eviction per pod and no Evicted event: %+v", len(got), got)
	}
	for _, o := range got {
		if o.Reason != ReasonEvicted || o.Class != ClassCause || o.Origin != fingerprint.SourcePodStatus || o.Count != 1 {
			t.Errorf("observation %s, class %q, origin %q, count %d; want the eviction from pod status, a cause, "+
				"counted once", o.Reason, o.Class, o.Origin, o.Count)
		}
		// P4: the message the kubelet wrote is the symptom.
		if !strings.Contains(o.Symptom, "ephemeral local storage usage exceeds the total limit") {
			t.Errorf("eviction symptom %q lost the pod's message", o.Symptom)
		}
	}
	if keys := incidentKeys(got); len(keys) != 1 {
		t.Errorf("one eviction cause lands on %d Incidents, want 1: %v", len(keys), keys)
	}
}

// evictedEvent is the kubelet's Evicted warning ev-evicted about pod name with uid, at at.
func evictedEvent(pod string, uid types.UID, at time.Time) *eventsv1.Event {
	ev := warning("ev-evicted", 0)
	ev.Reason, ev.Action = ReasonEvicted, ""
	ev.Note = "The node was low on resource: ephemeral-storage."
	ev.EventTime = metav1.NewMicroTime(at)
	ev.Regarding = evictedRef(pod, uid)
	return ev
}

// TestIssue432EvictedEventWaitsForThePodStatus is P3 and P5 in the order the kubelet writes them: the Evicted
// event comes before the pod's status shows the eviction. The collector waits EvictionSettleGrace, by its own
// clock, for the status. The eviction is then one observation, delivered by whichever reconciler comes first,
// with one fingerprint and one occurrence ID whether it was read from the pod's status or from the event
// (decided 2026-11-21): when the status never shows it, or the pod is gone, the event delivers it.
func TestIssue432EvictedEventWaitsForThePodStatus(t *testing.T) {
	d, rs, _ := workload()
	running := specPod()
	running.Status.Phase = corev1.PodRunning
	const note = "The node was low on resource: ephemeral-storage. "
	// fromStatus: the observation was read from the pod's status, a cause; from the event's note it keeps the
	// event as its origin and has no class (an Event never sets status.cause).
	want := func(t *testing.T, h *harness, o Observation, fromStatus bool) {
		t.Helper()
		origin, class := fingerprint.SourcePodStatus, ClassCause
		if !fromStatus {
			origin, class = fingerprint.SourceEvent, ClassNone
		}
		_, _, pod := workload()
		fp := h.c.evictionFingerprint(testNS, string(d.UID), pod.Name, running.UID, note)
		occ := termination{id: evictionID, evicted: true}.occurrence(running.UID)
		if o.Reason != ReasonEvicted || o.Origin != origin || o.Class != class ||
			o.Fingerprint != fp || o.Occurrence != occ || o.Count != 1 {
			t.Errorf("got %s from %s, class %q, fingerprint %s, occurrence %s, count %d; want the pod's eviction "+
				"%s, %s, counted once", o.Reason, o.Origin, o.Class, o.Fingerprint, o.Occurrence, o.Count, fp, occ)
		}
	}
	t.Run("status shows the eviction during the wait", func(t *testing.T) {
		h := newHarness(t, Config{}, d, rs, running.DeepCopy())
		h.clock.set(t0)
		ev := evictedEvent(running.Name, running.UID, t0)
		ev.Note = note
		h.create(ev)
		if res := h.reconcileEvent("ev-evicted"); res.RequeueAfter != EvictionSettleGrace {
			t.Fatalf("first look: requeue after %v, want %v", res.RequeueAfter, EvictionSettleGrace)
		}
		h.none()
		cur := readPod(t, h, running.Name)
		cur.Status = evictedPod(running.Name, running.UID, reasonError, "containerd://a", t0).Status
		cur.Status.Message = note
		h.update(cur)
		h.clock.set(t0.Add(10 * time.Second))
		h.reconcilePod(running.Name)
		want(t, h, h.one(), true)
		h.reconcileEvent("ev-evicted")
		h.none()
	})
	t.Run("status never shows it", func(t *testing.T) {
		h := newHarness(t, Config{}, d, rs, running.DeepCopy())
		h.clock.set(t0)
		ev := evictedEvent(running.Name, running.UID, t0)
		ev.Note = note
		h.create(ev)
		h.reconcileEvent("ev-evicted")
		h.none()
		h.clock.set(t0.Add(EvictionSettleGrace - time.Second))
		if res := h.reconcileEvent("ev-evicted"); res.RequeueAfter != time.Second {
			t.Errorf("before the end of the wait: requeue after %v, want 1s", res.RequeueAfter)
		}
		h.none()
		h.clock.set(t0.Add(EvictionSettleGrace))
		h.reconcileEvent("ev-evicted")
		want(t, h, h.one(), false)
	})
	t.Run("pod gone", func(t *testing.T) {
		// The event's first look reads the running pod and its owner; the pod is deleted (a StatefulSet deletes
		// a failed pod at once) before the pod reconciler delivers anything.
		h := newHarness(t, Config{}, d, rs, running.DeepCopy())
		h.clock.set(t0)
		ev := evictedEvent(running.Name, running.UID, t0)
		ev.Note = note
		h.create(ev)
		h.reconcileEvent("ev-evicted")
		h.none()
		if err := h.cl.Delete(t.Context(), running.DeepCopy()); err != nil {
			t.Fatal(err)
		}
		h.reconcilePod(running.Name)
		h.clock.set(t0.Add(5 * time.Second))
		h.reconcileEvent("ev-evicted")
		want(t, h, h.one(), false)
	})
}

// TestIssue432LinkWindow is the window of a termination cause (P1, P2): a non-zero exit links within
// KillCauseWindow on either side of the consequence's time, an OOM kill only after its finish (ADR-0039 point 5).
func TestIssue432LinkWindow(t *testing.T) {
	cases := []struct {
		reason string
		at     time.Duration // the consequence's time relative to the finish
		want   bool
	}{
		{reasonError, 0, true},
		{reasonError, KillCauseWindow, true},
		{reasonError, KillCauseWindow + time.Second, false},
		{reasonError, -KillCauseWindow, true},
		{reasonError, -KillCauseWindow - time.Second, false},
		{ReasonOOMKilled, 0, true},
		{ReasonOOMKilled, KillCauseWindow, true},
		{ReasonOOMKilled, KillCauseWindow + time.Second, false},
		{ReasonOOMKilled, -time.Second, false},
	}
	for _, c := range cases {
		if got := linksAt(termination{reason: c.reason, finished: t0}, t0.Add(c.at)); got != c.want {
			t.Errorf("%s at %v: links %t, want %t", c.reason, c.at, got, c.want)
		}
	}
}
