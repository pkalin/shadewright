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
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// ReasonUnhealthy is the reason of the kubelet's event for a failed or errored probe.
const ReasonUnhealthy = "Unhealthy"

// ProbeSettleGrace is how long the collector waits, on its own clock, before it decides on a readiness or
// startup probe failure about a pod that is not Ready yet and has no restarts (#726). A pod that fails its
// first probes while it starts is Ready at most a few probe periods later (10 seconds each by default, 3
// failures before the kubelet gives up on a readiness check), so one minute covers the usual start, and it
// stays well below incidents.RecurAfter (2 minutes), so a probe that keeps failing still opens its Incident
// within the same window as before, one minute later at most.
const ProbeSettleGrace = time.Minute

// labelProbe is the label of a metric that names the probe of an Unhealthy event, Readiness or Startup.
const labelProbe = "probe"

// probeSettledSkipped counts Unhealthy events not turned into an observation because the pod they are about
// was Ready, with no container restart, when the collector decided on them (#726).
var probeSettledSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_analyzer_settled_probe_failures_skipped_total",
	Help: "Readiness or startup probe failures not turned into an observation because the pod was Ready with no restarts.",
}, []string{labelProbe})

func init() {
	metrics.Registry.MustRegister(probeSettledSkipped)
}

// probeOf returns the probe a kubelet Unhealthy event about a pod names, Readiness or Startup, and false for
// any other event: another reason, a liveness probe, or an event that is not about a pod. The kubelet writes
// "<Probe> probe failed: ..." or "<Probe> probe errored: ...". The note is untrusted, as the reason is: a writer
// of events can at most hide an Unhealthy event about a pod that is Ready with no restarts, which it could
// also have left out.
func probeOf(ev *eventsv1.Event) (string, bool) {
	if ev.Reason != ReasonUnhealthy || ev.Regarding.Kind != kindPod || ev.Regarding.APIVersion != apiCore {
		return "", false
	}
	for _, p := range []string{"Readiness", "Startup"} {
		if strings.HasPrefix(ev.Note, p+" probe ") {
			return p, true
		}
	}
	return "", false
}

// probeVerdict is what the collector decided on a readiness or startup probe failure.
type probeVerdict uint8

const (
	// probeCount: count the event as any other.
	probeCount probeVerdict = iota
	// probeSettled: the pod is Ready with no restarts; the event is handled without an observation.
	probeSettled
	// probeWait: the pod is not Ready yet and has no restarts; decide again after the returned delay.
	probeWait
)

// probeDeferrals remembers, per event, when the collector first put off its decision on a probe failure, on
// the collector's clock. Events are keyed by namespace and name, and the UID tells a recreated event from the
// old one. An event whose series grows while its decision is put off keeps its first time: one deferral per
// event, not one per update. It holds at most a fixed number of events and never drops one to make room: when
// it is full, a new event is counted at once. It is in memory only, so a restart forgets it, and a replayed
// event is put off again from its replay.
type probeDeferrals struct {
	mu    sync.Mutex
	max   int
	since map[types.NamespacedName]probeDeferral
}

type probeDeferral struct {
	uid   types.UID
	since time.Time
}

func newProbeDeferrals(capacity int) *probeDeferrals {
	return &probeDeferrals{max: max(capacity, 1), since: map[types.NamespacedName]probeDeferral{}}
}

// start returns when the decision on the event was first put off, recording now when it was not, and false
// when it was not and there is no room.
func (d *probeDeferrals) start(key types.NamespacedName, uid types.UID, now time.Time) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p, ok := d.since[key]; ok {
		if p.uid == uid {
			return p.since, true
		}
		delete(d.since, key)
	}
	if len(d.since) >= d.max {
		return time.Time{}, false
	}
	d.since[key] = probeDeferral{uid: uid, since: now}
	return now, true
}

// done forgets the event.
func (d *probeDeferrals) done(key types.NamespacedName) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.since, key)
}

func (d *probeDeferrals) len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.since)
}

// settleProbe decides on a readiness or startup probe failure (probeOf) about a pod, from the pod as the
// manager's cache holds it now, read by name and checked by UID:
//
//   - Ready, and no container (init containers included) has restarted: probeSettled. The probe failed while
//     the pod started, or once, and the pod recovered without a restart.
//   - not Ready, no restarts, and less than ProbeSettleGrace since the decision was first put off: probeWait.
//   - anything else counts: a restart, a pod still not Ready after the grace, a pod that is gone or replaced
//     (its evidence counts as before, unless the #724 check dropped it), or no room to put the decision off.
//
// A failed read of the pod is returned, for a retry, as subjectOf returns it.
func (c *Collector) settleProbe(ctx context.Context, ev *eventsv1.Event) (probeVerdict, time.Duration, error) {
	var pod corev1.Pod
	key := client.ObjectKey{Namespace: ev.Namespace, Name: ev.Regarding.Name}
	if err := c.owners.pods.Get(ctx, key, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return probeCount, 0, nil
		}
		return probeCount, 0, fmt.Errorf("reading the Pod of a probe failure: %w", err)
	}
	if pod.UID != ev.Regarding.UID || restarted(&pod) {
		return probeCount, 0, nil
	}
	if podReady(&pod) {
		return probeSettled, 0, nil
	}
	now := c.now()
	since, ok := c.probes.start(types.NamespacedName{Namespace: ev.Namespace, Name: ev.Name}, ev.UID, now)
	if !ok {
		logf.FromContext(ctx).V(1).Info("Counted probe failure at once, too many decisions put off",
			"limit", c.probes.max)
		return probeCount, 0, nil
	}
	if left := since.Add(ProbeSettleGrace).Sub(now); left > 0 {
		return probeWait, left, nil
	}
	return probeCount, 0, nil
}

// podReady reports whether pod's Ready condition is True.
func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// restarted reports whether any container of pod, init containers included, has restarted.
func restarted(pod *corev1.Pod) bool {
	for _, list := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, s := range list {
			if s.RestartCount > 0 {
				return true
			}
		}
	}
	return false
}

// skipSettled counts and logs a probe failure handled without an observation.
func skipSettled(ctx context.Context, probe string) {
	probeSettledSkipped.WithLabelValues(probe).Inc()
	logf.FromContext(ctx).V(1).Info("Skipped probe failure of a Pod that is Ready with no restarts", "probe", probe)
}
