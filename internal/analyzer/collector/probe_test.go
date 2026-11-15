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
	"maps"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

// starting sets pod as the kubelet reports a pod whose container started at t0 and is not Ready yet.
func starting(pod *corev1.Pod) *corev1.Pod {
	return startedAt(pod, t0)
}

// startedAt sets pod as the kubelet reports a pod whose container started at at and is not Ready yet.
func startedAt(pod *corev1.Pod, at time.Time) *corev1.Pod {
	pod.Status.StartTime = &metav1.Time{Time: at}
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(at),
	}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerApp, Ready: false, State: running(at)}}
	return pod
}

func running(at time.Time) corev1.ContainerState {
	return corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(at)}}
}

// setReady reads pod back and sets its Ready condition to ready, changed at the test clock's now, keeping
// its container's start.
func (h *harness) setReady(name string, ready bool) {
	h.t.Helper()
	pod := readPod(h.t, h, name)
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodReady, Status: status, LastTransitionTime: metav1.NewTime(h.clock.Now()),
	}}
	for i := range pod.Status.ContainerStatuses {
		pod.Status.ContainerStatuses[i].Ready = ready
	}
	h.update(pod)
}

// becomeReady reads pod back and sets it Ready at the test clock's now, as the kubelet does after the first
// probe that passes, with restarts restarts of its container.
func (h *harness) becomeReady(name string, restarts int32) {
	h.t.Helper()
	pod := readPod(h.t, h, name)
	started := t0
	if s := pod.Status.ContainerStatuses; len(s) > 0 && s[0].State.Running != nil {
		started = s[0].State.Running.StartedAt.Time
	}
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(h.clock.Now()),
	}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: containerApp, Ready: true, RestartCount: restarts, State: running(started),
	}}
	h.update(pod)
}

// restart reads pod back and gives its container one restart, leaving it not Ready.
func (h *harness) restart(name string) {
	h.t.Helper()
	pod := readPod(h.t, h, name)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: containerApp, RestartCount: 1}}
	h.update(pod)
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

// readyAt sets pod Ready since at, with its container started at started and no restart.
func readyAt(pod *corev1.Pod, started, at time.Time) *corev1.Pod {
	startedAt(pod, started)
	pod.Status.Conditions[0].Status, pod.Status.Conditions[0].LastTransitionTime = corev1.ConditionTrue,
		metav1.NewTime(at)
	pod.Status.ContainerStatuses[0].Ready = true
	return pod
}

// TestProbeFailureOfALongRunningReadyPodCounts: a pod that has served for an hour fails a readiness probe or
// two under load and stays Ready (the kubelet flips Ready only after failureThreshold failures). The failure
// is not part of a start: it counts at once.
func TestProbeFailureOfALongRunningReadyPodCounts(t *testing.T) {
	d, rs, pod := workload()
	h := newHarness(t, Config{}, d, rs, readyAt(pod, t0.Add(-time.Hour), t0.Add(-time.Hour)))
	h.create(probeEvent("load", webPod, podUID, "Readiness", "10.244.0.17", t0))
	if res := h.reconcileEvent("load"); res.RequeueAfter != 0 {
		t.Fatalf("requeue after %s, want none", res.RequeueAfter)
	}
	h.one()
}

// TestFirstFailureAfterADependencyWentDownCounts: a pod that started a minute ago and has been Ready since
// t0+20s fails its readiness probe at t0+60s, when a dependency went down; it is still Ready, since before
// the failure. That is not a start that recovered: it counts at once.
func TestFirstFailureAfterADependencyWentDownCounts(t *testing.T) {
	d, rs, pod := workload()
	h := newHarness(t, Config{}, d, rs, readyAt(pod, t0, t0.Add(20*time.Second)))
	h.clock.add(time.Minute)
	h.create(probeEvent("dependency", webPod, podUID, "Readiness", "10.244.0.17", h.clock.Now()))
	h.reconcileEvent("dependency")
	h.one()
}

// TestFlappingPodOfADayCounts is the second reproduction of #726: a day-old pod fails its readiness probe
// every minute for 12 minutes, goes not Ready and is Ready again before each look of the collector. Before the
// narrowed rule each look found it Ready with no restarts and nothing was counted; now each failure counts.
func TestFlappingPodOfADayCounts(t *testing.T) {
	d, rs, pod := workload()
	day := t0.Add(-24 * time.Hour)
	h := newHarness(t, Config{}, d, rs, readyAt(pod, day, day))
	h.c.started() // the collector runs from t0: the event is new to it
	ev := probeEvent("flap", webPod, podUID, "Readiness", "10.244.0.17", t0)
	ev.Series = &eventsv1.EventSeries{Count: 1, LastObservedTime: metav1.NewMicroTime(t0)}
	h.create(ev)
	var counted int64
	for i := range 12 {
		if i > 0 {
			h.clock.add(time.Minute)
			ev.Series = &eventsv1.EventSeries{Count: int32(i + 1), LastObservedTime: metav1.NewMicroTime(h.clock.Now())}
			h.update(ev)
		}
		h.setReady(webPod, false)
		h.clock.add(10 * time.Second)
		h.setReady(webPod, true)
		h.reconcileEvent("flap")
		for _, o := range h.drain() {
			counted += o.Count
		}
	}
	if counted != 12 {
		t.Fatalf("%d failures counted over 12 minutes of flapping, want 12", counted)
	}
}

// TestStartupFailureOfARecentPodIsSkippedAfterTheWait: a pod in its start fails its readiness probe, is not
// Ready yet, and is Ready (after the failure) before the grace ends: skipped, as the start-up case of #726.
func TestStartupFailureOfARecentPodIsSkippedAfterTheWait(t *testing.T) {
	d, rs, pod := workload()
	h := newHarness(t, Config{}, d, rs, startedAt(pod, t0.Add(-30*time.Second)))
	before := skipped("Readiness")
	h.create(probeEvent("start", webPod, podUID, "Readiness", "10.244.0.17", t0))
	h.waits("start", ProbeSettleGrace)
	h.clock.add(20 * time.Second)
	h.becomeReady(webPod, 0)
	h.clock.add(ProbeSettleGrace)
	h.reconcileEvent("start")
	h.none()
	if got := skipped("Readiness") - before; got != 1 {
		t.Errorf("%v readiness failures counted as skipped, want 1", got)
	}
}

// TestProbeFailureOfATerminatingPod: a readiness failure at or after the pod's deletion is skipped at once (a
// process that shuts down stops answering, as during a scale-down); one before the deletion is judged by the
// other rules and, for a pod that has run for an hour, counts at once.
func TestProbeFailureOfATerminatingPod(t *testing.T) {
	d, rs, pod := workload()
	pod = startedAt(pod, t0.Add(-time.Hour))
	pod.DeletionTimestamp = &metav1.Time{Time: t0.Add(10 * time.Second)}
	pod.Finalizers = []string{"test.shadewright.io/hold"}
	h := newHarness(t, Config{}, d, rs, pod)
	before := skipped("Readiness")

	h.create(probeEvent("after", webPod, podUID, "Readiness", "10.244.0.17", t0.Add(10*time.Second)))
	if res := h.reconcileEvent("after"); res.RequeueAfter != 0 {
		t.Fatalf("requeue after %s for a failure at the deletion, want none", res.RequeueAfter)
	}
	h.none()
	if got := skipped("Readiness") - before; got != 1 {
		t.Errorf("%v readiness failures counted as skipped, want 1", got)
	}

	h.create(probeEvent("before", webPod, podUID, "Readiness", "10.244.0.17", t0.Add(5*time.Second)))
	if res := h.reconcileEvent("before"); res.RequeueAfter != 0 {
		t.Fatalf("requeue after %s for a failure before the deletion, want none", res.RequeueAfter)
	}
	h.one()
}

// TestReplayedProbeFailureOlderThanTheGraceIsDecidedAtOnce: the wait runs from the event's last occurrence,
// so an event replayed after a restart, older than the grace, about a pod still not Ready, counts at once
// instead of waiting a new grace from the replay.
func TestReplayedProbeFailureOlderThanTheGraceIsDecidedAtOnce(t *testing.T) {
	d, rs, pod := workload()
	h := newHarness(t, Config{}, d, rs, startedAt(pod, t0.Add(-30*time.Second)))
	h.clock.add(2 * time.Minute)
	h.create(probeEvent("old", webPod, podUID, "Readiness", "10.244.0.17", t0))
	if res := h.reconcileEvent("old"); res.RequeueAfter != 0 {
		t.Fatalf("requeue after %s, want none", res.RequeueAfter)
	}
	h.one()

	// A younger one waits what is left of its grace.
	h.create(probeEvent("young", webPod, podUID, "Readiness", "10.244.0.17", h.clock.Now().Add(-20*time.Second)))
	h.waits("young", 40*time.Second)
}

// TestProbeDecisionsAreForgottenOnEveryPath: a decision put off is forgotten when the event stops being a
// Warning or has nothing new, not only when it is decided or deleted, and the gauge follows.
func TestProbeDecisionsAreForgottenOnEveryPath(t *testing.T) {
	d, rs, pod := workload()
	h := newHarness(t, Config{}, d, rs, starting(pod))
	gauge := func() float64 { return testutil.ToFloat64(probeDecisionsPending) }
	base := gauge()

	ev := probeEvent("normal", webPod, podUID, "Readiness", "10.244.0.17", t0)
	h.create(ev)
	h.waits("normal", ProbeSettleGrace)
	if got := gauge() - base; got != 1 {
		t.Fatalf("pending gauge rose by %v, want 1", got)
	}
	ev.Type = corev1.EventTypeNormal
	h.update(ev)
	h.reconcileEvent("normal")
	if n := h.c.probes.len(); n != 0 {
		t.Errorf("%d decisions put off after the event became Normal, want none", n)
	}

	h.create(probeEvent("handled", webPod, podUID, "Readiness", "10.244.0.17", t0))
	h.waits("handled", ProbeSettleGrace)
	h.c.series.commit(reqFor("handled").NamespacedName, types.UID("ev-handled"), t0, 1)
	h.reconcileEvent("handled")
	if n := h.c.probes.len(); n != 0 {
		t.Errorf("%d decisions put off after an update with nothing new, want none", n)
	}
	if got := gauge() - base; got != 0 {
		t.Errorf("pending gauge %v above where it started, want 0", got)
	}
}

// TestProbeFailureReadsThePodOnce: the owner chain and the probe decision rest on one read of the pod.
func TestProbeFailureReadsThePodOnce(t *testing.T) {
	d, rs, pod := workload()
	h := newHarness(t, Config{}, d, rs, starting(pod))
	var reads int
	h.c.owners.pods = interceptor.NewClient(h.cl, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Pod); ok {
				reads++
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	h.create(probeEvent("once", webPod, podUID, "Readiness", "10.244.0.17", t0))
	h.waits("once", ProbeSettleGrace)
	if reads != 1 {
		t.Fatalf("%d reads of the pod, want 1", reads)
	}
}

// TestProbeFailureOfARetiredTemplateDuringTheWait: the workload retires the pod's template while its probe
// failure waits; the #724 check drops the event at the next look, once.
func TestProbeFailureOfARetiredTemplateDuringTheWait(t *testing.T) {
	d, rs, nrs, pod, np := rolledOut()
	d.Annotations[revisionAnnotation] = "1"
	h := newHarness(t, Config{}, d, rs, starting(pod))
	before := droppedTotal()
	h.create(probeEvent("retired", webPod, podUID, "Readiness", "10.244.0.17", t0))
	h.waits("retired", ProbeSettleGrace)

	h.create(nrs)
	h.create(np)
	d = readDeployment(t, h)
	d.Annotations[revisionAnnotation] = "2"
	h.update(d)
	rs = &appsv1.ReplicaSet{}
	if err := h.cl.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: webRS}, rs); err != nil {
		t.Fatal(err)
	}
	h.update(retire(rs))
	h.clock.add(ProbeSettleGrace)
	h.reconcileEvent("retired")
	h.none()
	h.reconcileEvent("retired")
	h.none()
	if got := droppedTotal() - before; got != 1 {
		t.Errorf("dropped counter rose by %v, want 1", got)
	}
	if n := h.c.probes.len(); n != 0 {
		t.Errorf("%d decisions put off after the drop, want none", n)
	}
}

// TestProbeCountStandsAcrossADeliveryRetry: a failure decided to count, after the grace with the pod still not
// Ready, whose delivery found the channel full, still counts at the retry although the pod became Ready
// meanwhile.
func TestProbeCountStandsAcrossADeliveryRetry(t *testing.T) {
	d, rs, pod := workload()
	h := newHarness(t, Config{Buffer: 1}, d, rs, starting(pod))
	h.create(warning("filler", 0))
	h.reconcileEvent("filler") // fills the channel

	h.create(probeEvent("slow", webPod, podUID, "Readiness", "10.244.0.17", t0))
	if res := h.reconcileEvent("slow"); res.RequeueAfter != ProbeSettleGrace {
		t.Fatalf("requeue after %s, want %s", res.RequeueAfter, ProbeSettleGrace)
	}
	h.clock.add(ProbeSettleGrace)
	if res := h.reconcileEvent("slow"); res.RequeueAfter != retryAfter {
		t.Fatalf("requeue after %s with the channel full, want %s", res.RequeueAfter, retryAfter)
	}
	h.one() // the filler
	h.becomeReady(webPod, 0)
	h.reconcileEvent("slow")
	if o := h.one(); o.Count != 1 {
		t.Errorf("count %d, want 1", o.Count)
	}
	if n := h.c.probes.len(); n != 0 {
		t.Errorf("%d decisions pending after the delivery, want none", n)
	}
}
