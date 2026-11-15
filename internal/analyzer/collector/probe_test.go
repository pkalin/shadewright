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
	"maps"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	scaledPod    = "web-7d4b9c8f6-x2k9m"
	scaledPodUID = types.UID("9e1c2b9e-0d6a-4c1e-9b7d-2a5e8f3c6d15")
)

// probeEvent is the kubelet's Unhealthy event about pod, created at at, for a probe of kind (Readiness,
// Liveness or Startup) that failed against the pod's address ip.
func probeEvent(name, pod string, uid types.UID, kind, ip string, at time.Time) *eventsv1.Event {
	return &eventsv1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNS, UID: types.UID("ev-" + name), CreationTimestamp: metav1.NewTime(at),
		},
		EventTime:           metav1.NewMicroTime(at),
		Reason:              "Unhealthy",
		Type:                corev1.EventTypeWarning,
		ReportingController: reportingKubelet,
		Note: kind + ` probe failed: Get "http://` + ip + `:8080/healthz": dial tcp ` + ip +
			":8080: connect: connection refused",
		Regarding: corev1.ObjectReference{
			APIVersion: apiCore, Kind: kindPod, Namespace: testNS, Name: pod, UID: uid,
			FieldPath: "spec.containers{" + containerApp + "}",
		},
	}
}

// secondPod is another pod of the web ReplicaSet, as a scale-up starts it.
func secondPod(first *corev1.Pod) *corev1.Pod {
	p := first.DeepCopy()
	p.Name, p.UID, p.ResourceVersion = scaledPod, scaledPodUID, ""
	p.Labels = maps.Clone(first.Labels)
	return p
}

// starting sets pod as the kubelet reports a pod whose container started and is not Ready yet.
func starting(pod *corev1.Pod) *corev1.Pod {
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerApp, Ready: false}}
	return pod
}

// becomeReady reads pod back and sets it Ready, as the kubelet does after the first probe that passes, with
// restarts restarts of its container.
func (h *harness) becomeReady(name string, restarts int32) {
	h.t.Helper()
	var pod corev1.Pod
	if err := h.cl.Get(h.t.Context(), client.ObjectKey{Namespace: testNS, Name: name}, &pod); err != nil {
		h.t.Fatal(err)
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerApp, Ready: true, RestartCount: restarts}}
	h.update(&pod)
}

// restart reads pod back and gives its container one restart, leaving it not Ready.
func (h *harness) restart(name string) {
	h.t.Helper()
	var pod corev1.Pod
	if err := h.cl.Get(h.t.Context(), client.ObjectKey{Namespace: testNS, Name: name}, &pod); err != nil {
		h.t.Fatal(err)
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerApp, RestartCount: 1}}
	h.update(&pod)
}

// waits reconciles event name and checks that it put its decision off by want, with no observation.
func (h *harness) waits(name string, want time.Duration) {
	h.t.Helper()
	if res := h.reconcileEvent(name); res.RequeueAfter != want {
		h.t.Fatalf("event %s: requeue after %s, want %s", name, res.RequeueAfter, want)
	}
	h.none()
}

func skipped(probe string) float64 {
	return testutil.ToFloat64(probeSettledSkipped.WithLabelValues(probe))
}

// TestStartupProbeFailureOfAPodThatBecameReadyIsSkipped replays the lab sequence of #726: KEDA starts a pod
// of the workload, its readiness probe fails once while it starts and it is Ready seconds later, with no
// restart; minutes later another pod does the same. Before the fix both events were observations of one
// fingerprint (owner UID and normalized note), and the second, 2 to 15 minutes after the first, opened the
// Incident (package incidents, sightings.go). Now neither is an observation.
func TestStartupProbeFailureOfAPodThatBecameReadyIsSkipped(t *testing.T) {
	d, rs, pod := workload()
	h := newHarness(t, Config{}, d, rs, starting(pod), starting(secondPod(pod)))
	before := skipped("Readiness")

	h.create(probeEvent("first", webPod, podUID, "Readiness", "10.244.0.17", t0))
	h.waits("first", ProbeSettleGrace)
	h.clock.add(10 * time.Second)
	h.becomeReady(webPod, 0)
	h.waits("first", 0) // a look before the grace ends, with the pod Ready: skipped then

	h.clock.add(4 * time.Minute)
	h.create(probeEvent("second", scaledPod, scaledPodUID, "Readiness", "10.244.0.23", h.clock.Now()))
	h.waits("second", ProbeSettleGrace)
	h.becomeReady(scaledPod, 0)
	h.clock.add(ProbeSettleGrace)
	h.reconcileEvent("second")
	h.none()

	if got := skipped("Readiness") - before; got != 2 {
		t.Errorf("%v readiness failures counted as skipped, want 2", got)
	}
	if n := h.c.probes.len(); n != 0 {
		t.Errorf("%d decisions still put off, want none", n)
	}
	// Handled: an update of the event with nothing new is not looked at again.
	h.reconcileEvent("first")
	h.none()
}

// TestStartupProbeOfAReadyPodIsSkippedAtOnce: a startup probe failure about a pod that is Ready by the time
// the collector reads it (an event replayed after a restart, say) is skipped without waiting.
func TestStartupProbeOfAReadyPodIsSkippedAtOnce(t *testing.T) {
	d, rs, pod := workload()
	h := newHarness(t, Config{}, d, rs, starting(pod))
	h.becomeReady(webPod, 0)
	before := skipped("Startup")
	h.create(probeEvent("startup", webPod, podUID, "Startup", "10.244.0.17", t0))
	if res := h.reconcileEvent("startup"); res.RequeueAfter != 0 {
		t.Fatalf("requeue after %s, want none", res.RequeueAfter)
	}
	h.none()
	if got := skipped("Startup") - before; got != 1 {
		t.Errorf("%v startup failures counted as skipped, want 1", got)
	}
}

// TestProbeFailureOfASlowStarterCountsAfterTheGrace: pods whose readiness probe keeps failing past the grace
// count as before, one grace later, and one series that grows while it waits is one deferral. The two pods'
// failures share one fingerprint, so the second opens the Incident as before (#726, step 1).
func TestProbeFailureOfASlowStarterCountsAfterTheGrace(t *testing.T) {
	d, rs, pod := workload()
	h := newHarness(t, Config{}, d, rs, starting(pod), starting(secondPod(pod)))

	ev := probeEvent("first", webPod, podUID, "Readiness", "10.244.0.17", t0)
	ev.Series = &eventsv1.EventSeries{Count: 1, LastObservedTime: metav1.NewMicroTime(t0)}
	h.create(ev)
	h.waits("first", ProbeSettleGrace)
	h.clock.add(30 * time.Second)
	ev.Series = &eventsv1.EventSeries{Count: 3, LastObservedTime: metav1.NewMicroTime(h.clock.Now())}
	h.update(ev)
	h.waits("first", 30*time.Second)
	h.clock.add(30 * time.Second)
	h.reconcileEvent("first")
	first := h.one()
	if first.Count != 3 {
		t.Errorf("count %d, want the 3 occurrences of the series", first.Count)
	}

	h.clock.add(4 * time.Minute)
	h.create(probeEvent("second", scaledPod, scaledPodUID, "Readiness", "10.244.0.23", h.clock.Now()))
	h.waits("second", ProbeSettleGrace)
	h.clock.add(ProbeSettleGrace)
	h.reconcileEvent("second")
	second := h.one()

	if first.Fingerprint != second.Fingerprint || first.Occurrence == second.Occurrence {
		t.Fatalf("the readiness failures of two pods of one workload: fingerprints %s and %s, occurrences %s "+
			"and %s; want one fingerprint, two occurrences", first.Fingerprint, second.Fingerprint,
			first.Occurrence, second.Occurrence)
	}
	if second.Subject.Kind != kindDeployment || second.Immediate {
		t.Fatalf("observation %+v, want one about the Deployment, not immediate", second)
	}
	if n := h.c.probes.len(); n != 0 {
		t.Errorf("%d decisions still put off, want none", n)
	}
}

// TestRestartAfterAProbeFailureCounts: a container that restarts while its readiness failure waits counts at
// the next look, before the grace ends, and so does a pod that is Ready again after a restart.
func TestRestartAfterAProbeFailureCounts(t *testing.T) {
	d, rs, pod := workload()
	h := newHarness(t, Config{}, d, rs, starting(pod), starting(secondPod(pod)))
	h.create(probeEvent("first", webPod, podUID, "Readiness", "10.244.0.17", t0))
	h.waits("first", ProbeSettleGrace)
	h.clock.add(20 * time.Second)
	h.restart(webPod)
	h.reconcileEvent("first")
	h.one()

	h.becomeReady(scaledPod, 1)
	h.create(probeEvent("second", scaledPod, scaledPodUID, "Startup", "10.244.0.23", h.clock.Now()))
	h.reconcileEvent("second")
	h.one()
}

// TestProbeFailuresThatAlwaysCount: a liveness failure, an Unhealthy event about a pod that is gone or about
// no pod at all, are counted at once, whatever the pod's state.
func TestProbeFailuresThatAlwaysCount(t *testing.T) {
	d, rs, pod := workload()
	h := newHarness(t, Config{}, d, rs, starting(pod))
	h.becomeReady(webPod, 0)

	h.create(probeEvent("liveness", webPod, podUID, "Liveness", "10.244.0.17", t0))
	h.reconcileEvent("liveness")
	h.one()

	h.create(probeEvent("gone", "web-7d4b9c8f6-gone1", "aa1c2b9e-0d6a-4c1e-9b7d-2a5e8f3c6d16", "Readiness",
		"10.244.0.30", t0))
	h.reconcileEvent("gone")
	h.one()

	// The pod was replaced by another of the same name: the event is about the old one.
	h.create(probeEvent("replaced", webPod, "bb1c2b9e-0d6a-4c1e-9b7d-2a5e8f3c6d17", "Readiness", "10.244.0.31", t0))
	h.reconcileEvent("replaced")
	h.one()

	noPod := probeEvent("deployment", webPod, podUID, "Readiness", "10.244.0.17", t0)
	noPod.Regarding = corev1.ObjectReference{APIVersion: apiApps, Kind: kindDeployment, Namespace: testNS,
		Name: web, UID: deployUID}
	h.create(noPod)
	h.reconcileEvent("deployment")
	h.one()
}

// TestProbeDeferralsAreBounded: with no room to put a decision off, a probe failure counts at once.
func TestProbeDeferralsAreBounded(t *testing.T) {
	d, rs, pod := workload()
	h := newHarness(t, Config{MaxEvents: 1}, d, rs, starting(pod), starting(secondPod(pod)))
	h.create(probeEvent("first", webPod, podUID, "Readiness", "10.244.0.17", t0))
	h.waits("first", ProbeSettleGrace)
	h.create(probeEvent("second", scaledPod, scaledPodUID, "Readiness", "10.244.0.23", t0))
	h.reconcileEvent("second")
	h.one()
	if n := h.c.probes.len(); n != 1 {
		t.Errorf("%d decisions put off, want 1", n)
	}
	// A deleted event is forgotten.
	if err := h.cl.Delete(t.Context(), probeEvent("first", webPod, podUID, "Readiness", "", t0)); err != nil {
		t.Fatal(err)
	}
	h.reconcileEvent("first")
	if n := h.c.probes.len(); n != 0 {
		t.Errorf("%d decisions put off after the event was deleted, want none", n)
	}
}
