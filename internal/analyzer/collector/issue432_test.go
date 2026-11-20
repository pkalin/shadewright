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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// The reproductions of #432 seen on rc.3 outside the lab (2026-11-20): one cause of one Deployment opened
// several Incidents. Each test drives the collector with the pod states and events of the case, in their order
// and at their times (t0 stands for the first sighting of the case), and requires what the issue asks: every
// observation lands on one Incident, its own fingerprint or, for a linked consequence, its cause's. They are
// skipped while main opens more than one; the skip names which rule does not hold.

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

// TestIssue432CrashLoopBackOffOpensASecondIncident is cases (a) and (c) of #432: a container exits with code 2
// (reason Error, read from pod status) and the kubelet reports BackOff for it. The termination is a symptom
// of no class (pods.go, termination.class) and the BackOff finds no cause in the pod status
// (cause.go, causeFingerprint reads only a waiting reason of WaitingCauseReasons or an OOMKilled kill;
// CrashLoopBackOff and Error are neither). So the BackOff is held for ConsequenceGrace with no cause, is still
// failing at the end, and is emitted unlinked: a second Incident about 2 minutes after the first.
func TestIssue432CrashLoopBackOffOpensASecondIncident(t *testing.T) {
	t.Skip("#432: a non-zero exit is no cause (ADR-0039 point 3, decided 2026-11-14), so BackOff of a crash loop never links to the Error Incident")
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
	// 20:27:15 the kubelet's BackOff: no cause in the pod status, so it is held.
	h.clock.set(t0.Add(2*time.Minute + 15*time.Second))
	h.reconcileEvent("ev-backoff")
	h.none()
	// Samples during the hold: the container keeps crashing.
	h.clock.set(t0.Add(3*time.Minute + 15*time.Second))
	h.reconcileEvent("ev-backoff")
	h.none()
	// 20:29:15 the end of ConsequenceGrace: still failing, emitted with no cause.
	h.clock.set(t0.Add(4*time.Minute + 15*time.Second))
	h.reconcileEvent("ev-backoff")
	backOff := h.one()
	if backOff.Class != ClassConsequence || backOff.CauseFingerprint != "" {
		t.Logf("BackOff emitted with class %q, cause %q", backOff.Class, backOff.CauseFingerprint)
	}
	got = append(got, backOff)

	if len(got) < 3 {
		t.Fatalf("got %d observations, want the two crashes and the BackOff: %+v", len(got), got)
	}
	if keys := incidentKeys(got); len(keys) != 1 {
		t.Errorf("one crash loop lands on %d Incidents, want 1: %v", len(keys), keys)
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

// TestIssue432EvictionOpensThreeIncidents is case (b) of #432: a container over its ephemeral-storage limit is
// evicted, its Deployment re-creates the pod and the new pod is evicted again. Each evicted pod shows its
// container terminated with exit code 137, with reason Error (killed) or ContainerStatusUnknown (the kubelet
// could not find it when the pod ended), and the kubelet reports Evicted about the pod. All three are
// observations of no class: a termination other than OOMKilled is no cause (pods.go, termination.class), an
// Event is never one, the Evicted event names no container, and nothing reads the pod's status.reason. Their
// fingerprints differ by reason, so each opens an Incident of its own once its second pod repeats it.
func TestIssue432EvictionOpensThreeIncidents(t *testing.T) {
	t.Skip("#432: nothing links the terminations and the Evicted event of an evicted pod; pod status.reason Evicted is not read and no rule folds them")
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

	if len(got) != 6 {
		t.Fatalf("got %d observations, want a termination and an Evicted event per pod: %+v", len(got), got)
	}
	for _, o := range got {
		if o.Class == ClassCause {
			t.Logf("%s is a cause", o.Reason)
		}
	}
	if keys := incidentKeys(got); len(keys) != 1 {
		t.Errorf("one eviction cause lands on %d Incidents, want 1: %v", len(keys), keys)
	}
}
