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
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// ReasonUnhealthy is the reason of the kubelet's event for a failed or errored probe.
const ReasonUnhealthy = "Unhealthy"

// ProbeSettleGrace is how long the collector waits before it decides on a readiness or startup probe failure
// about a pod that is still starting, not Ready yet and with no restarts (#726). The wait runs from the event's
// last occurrence at the first look (clamped to the collector's clock), so an event replayed after a restart
// that is older than the grace is decided at once. A pod that fails its first probes while it starts is Ready at
// most a few probe periods later (10 seconds each by default, 3 failures before the kubelet gives up on a
// readiness check), so one minute covers the usual start, and it stays well below incidents.RecurAfter (2
// minutes), so a probe that keeps failing still opens its Incident within the same window as before, one minute
// later at most.
const ProbeSettleGrace = time.Minute

// StartupWindow is how long after its container started a probe failure can still be part of the pod's start
// (#726). A failure later than that is about a running pod and counts, whatever the pod's state. A service that
// needs longer to start should declare a startup probe; its failures past the window count.
const StartupWindow = 5 * time.Minute

// labelProbe is the label of a metric that names the probe of an Unhealthy event, Readiness or Startup.
const labelProbe = "probe"

// probeSettledSkipped counts Unhealthy events not turned into an observation because they were part of the
// pod's start or of its shutdown (#726).
var probeSettledSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_analyzer_settled_probe_failures_skipped_total",
	Help: "Readiness or startup probe failures not turned into an observation because they were part of the " +
		"pod's start or shutdown.",
}, []string{labelProbe})

// probeDecisionsPending is the number of probe failures whose decision is put off, or whose decision to count
// waits for its delivery, and of kubelet Evicted events waiting for their pod's status (eviction.go), summed
// over the collectors of the process.
var probeDecisionsPending = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "shadewright_analyzer_probe_decisions_pending",
	Help: "Readiness or startup probe failures whose decision is put off or whose count is not delivered yet, " +
		"and kubelet Evicted events waiting for their pod's status.",
})

func init() {
	metrics.Registry.MustRegister(probeSettledSkipped, probeDecisionsPending)
}

// probeOf returns the probe a kubelet Unhealthy event about a pod names, Readiness or Startup, and false for
// any other event: another reason, a liveness probe, or an event that is not about a pod. The kubelet writes
// "<Probe> probe failed: ..." or "<Probe> probe errored: ...". The note is untrusted, as the reason is: a writer
// of events can at most make the collector skip or delay its own forged event, which it could also have left
// out, since what decides is the state of the pod the event names, which the writer does not control.
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
	// probeSettled: the pod became Ready after the failure, early in its start; no observation.
	probeSettled
	// probeTerminating: the failure came at or after the pod's deletion; no observation.
	probeTerminating
	// probeWait: the pod is starting and not Ready yet; decide again after the returned delay.
	probeWait
)

// probeDeferrals remembers, per event, the decisions the collector put off or took and has not delivered yet.
// Events are keyed by namespace and name, and the UID tells a recreated event from the old one. For a decision
// put off it keeps when the wait began, the event's last occurrence at the first look (clamped to the
// collector's clock); an event whose series grows while it waits keeps that time: one wait per event, not one
// per update. For a decision to count it keeps that, so a delivery retried after the pod became Ready still
// counts. It holds at most a fixed number of events and never drops one to make room: when it is full, a new
// event is counted at once, and a decision to count is then not remembered across a delivery retry. It is in
// memory only; a restart forgets it.
type probeDeferrals struct {
	mu    sync.Mutex
	max   int
	since map[types.NamespacedName]probeDeferral
}

type probeDeferral struct {
	uid     types.UID
	since   time.Time
	counted bool
}

func newProbeDeferrals(capacity int) *probeDeferrals {
	return &probeDeferrals{max: max(capacity, 1), since: map[types.NamespacedName]probeDeferral{}}
}

// lookup returns what is remembered about the event, dropping the entry of an older event of the same name.
func (d *probeDeferrals) lookup(key types.NamespacedName, uid types.UID) (probeDeferral, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.since[key]
	if ok && p.uid != uid {
		d.remove(key)
		return probeDeferral{}, false
	}
	return p, ok
}

// start returns when the wait on the event began, recording at when there was none, and false when there was
// none and there is no room.
func (d *probeDeferrals) start(key types.NamespacedName, uid types.UID, at time.Time) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p, ok := d.since[key]; ok && p.uid == uid {
		return p.since, true
	}
	return at, d.put(key, probeDeferral{uid: uid, since: at})
}

// count records that the event is counted, until done. It reports false when there was no room.
func (d *probeDeferrals) count(key types.NamespacedName, uid types.UID) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.put(key, probeDeferral{uid: uid, counted: true})
}

// put stores p under key, replacing an entry of the key, and reports false when it is a new key and there is
// no room. Called with mu held.
func (d *probeDeferrals) put(key types.NamespacedName, p probeDeferral) bool {
	if _, ok := d.since[key]; !ok && len(d.since) >= d.max {
		return false
	}
	if _, ok := d.since[key]; !ok {
		probeDecisionsPending.Inc()
	}
	d.since[key] = p
	return true
}

// remove drops key. Called with mu held.
func (d *probeDeferrals) remove(key types.NamespacedName) {
	if _, ok := d.since[key]; ok {
		delete(d.since, key)
		probeDecisionsPending.Dec()
	}
}

// done forgets the event.
func (d *probeDeferrals) done(key types.NamespacedName) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.remove(key)
}

func (d *probeDeferrals) len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.since)
}

// settleProbe decides on a readiness or startup probe failure (probeOf) about pod, the pod the event names as
// the owner chain read it (nil when it is gone or replaced). In order:
//
//   - a decision to count that is not delivered yet stands: probeCount.
//   - no pod: probeCount.
//   - the event's last occurrence is at or after the pod's deletion timestamp: probeTerminating. A process
//     that shuts down stops answering its probes.
//   - a container (init containers included) has restarted: probeCount.
//   - the last occurrence is more than StartupWindow after the start of the probed container (the pod's start
//     when the container cannot be identified), or that start is unknown: probeCount.
//   - Ready, and its Ready condition changed at or after the last occurrence: probeSettled. Ready since before
//     the failure (the kubelet flips Ready only after failureThreshold failures): probeCount.
//   - not Ready: probeWait until ProbeSettleGrace after the wait began, then probeCount; probeCount at once
//     when there is no room to wait.
//
// The times compared with the event's last occurrence (container start, Ready transition, deletion) are
// written by the kubelet that writes the event, except the deletion timestamp, which the API server sets.
func (c *Collector) settleProbe(ctx context.Context, ev *eventsv1.Event, pod *corev1.Pod) (probeVerdict,
	time.Duration) {
	key := types.NamespacedName{Namespace: ev.Namespace, Name: ev.Name}
	if p, ok := c.probes.lookup(key, ev.UID); ok && p.counted {
		return probeCount, 0
	}
	v, wait := c.probeVerdictOf(ctx, ev, pod, key)
	if v == probeCount && !c.probes.count(key, ev.UID) {
		logf.FromContext(ctx).V(1).Info("Counted probe failure without remembering it, too many decisions pending",
			"limit", c.probes.max)
	}
	return v, wait
}

func (c *Collector) probeVerdictOf(ctx context.Context, ev *eventsv1.Event, pod *corev1.Pod,
	key types.NamespacedName) (probeVerdict, time.Duration) {
	if pod == nil {
		return probeCount, 0
	}
	last := lastOccurrence(ev)
	if pod.DeletionTimestamp != nil && !last.Before(pod.DeletionTimestamp.Time) {
		return probeTerminating, 0
	}
	if restarted(pod) {
		return probeCount, 0
	}
	started := containerStart(pod, ev.Regarding.FieldPath)
	if started.IsZero() || last.Sub(started) > StartupWindow {
		return probeCount, 0
	}
	if ready, since := readyCondition(pod); ready {
		if !since.IsZero() && !since.Before(last) {
			return probeSettled, 0
		}
		return probeCount, 0
	}
	since, ok := c.probes.start(key, ev.UID, c.notFuture(last))
	if !ok {
		logf.FromContext(ctx).V(1).Info("Counted probe failure at once, too many decisions put off",
			"limit", c.probes.max)
		return probeCount, 0
	}
	if left := since.Add(ProbeSettleGrace).Sub(c.now()); left > 0 {
		return probeWait, left
	}
	return probeCount, 0
}

// readyCondition reports whether pod's Ready condition is True, and when it last changed.
func readyCondition(pod *corev1.Pod) (bool, time.Time) {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue, c.LastTransitionTime.Time
		}
	}
	return false, time.Time{}
}

// containerStart returns when the container that fieldPath names (spec.containers{name} or
// spec.initContainers{name}, as the kubelet writes it) started running, or the pod's start time when the
// container cannot be identified or is not running. It is zero when neither is known.
func containerStart(pod *corev1.Pod, fieldPath string) time.Time {
	for _, f := range []struct {
		prefix string
		list   []corev1.ContainerStatus
	}{
		{"spec.containers{", pod.Status.ContainerStatuses},
		{"spec.initContainers{", pod.Status.InitContainerStatuses},
	} {
		name, ok := strings.CutPrefix(fieldPath, f.prefix)
		if !ok {
			continue
		}
		name, ok = strings.CutSuffix(name, "}")
		if !ok {
			break
		}
		for _, s := range f.list {
			if s.Name == name && s.State.Running != nil && !s.State.Running.StartedAt.IsZero() {
				return s.State.Running.StartedAt.Time
			}
		}
	}
	if pod.Status.StartTime != nil {
		return pod.Status.StartTime.Time
	}
	return time.Time{}
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
func skipSettled(ctx context.Context, probe string, v probeVerdict) {
	probeSettledSkipped.WithLabelValues(probe).Inc()
	msg := "Skipped probe failure of a Pod that became Ready at start with no restarts"
	if v == probeTerminating {
		msg = "Skipped probe failure of a terminating Pod"
	}
	logf.FromContext(ctx).V(1).Info(msg, "probe", probe)
}
