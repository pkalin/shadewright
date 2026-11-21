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
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// ReasonEvicted is the status reason the kubelet writes on a pod it evicted for node pressure, and the reason
// of its Warning event about that pod.
const ReasonEvicted = "Evicted"

// EvictionSettleGrace is how long the collector waits, by its own clock from its first look, for the pod a
// kubelet Evicted event names to show the eviction in its status (ADR-0039 point 3, amended 2026-11-20). The
// kubelet records the event before it writes the pod's status, so the event is usually read first. The status
// is written as the kubelet starts to stop the pod, seconds later; one minute covers that and stays below
// incidents.RecurAfter.
const EvictionSettleGrace = time.Minute

// evictionNote is the symptom of an eviction read from pod status: the message the kubelet wrote, which
// names the resource and the limit, or a fixed text when there is none. The message is untrusted text, as an
// event's note is, and goes through the same normalization and redaction.
func evictionNote(message string) string {
	if message == "" {
		return "Pod was evicted"
	}
	return message
}

// evicted reports whether pod's status shows that the kubelet evicted it: phase Failed with reason Evicted.
func evicted(pod *corev1.Pod) bool {
	return pod != nil && pod.Status.Phase == corev1.PodFailed && pod.Status.Reason == ReasonEvicted
}

// evictionID is the ID of a pod's eviction among the pod's symptoms: a pod is evicted at most once.
const evictionID = "evicted"

// eviction returns the eviction pod's status shows as one symptom of the pod (ADR-0039 point 3, amended
// 2026-11-20), and false when it shows none. The eviction is the cause of the pod: it replaces every
// termination and waiting state of its containers, which are what the eviction did (an exit code 137, a
// ContainerStatusUnknown), so one evicted pod is one occurrence. It names no container. Its finish time is the
// newest finishedAt of the current state of the pod's containers, written by the same kubelet; without one
// (decided 2026-11-21), the time the kubelet set the DisruptionTarget condition when it ended the pod; without
// that either, zero, and the caller takes the time it read the pod.
func eviction(pod *corev1.Pod) (termination, bool) {
	if !evicted(pod) {
		return termination{}, false
	}
	t := termination{id: evictionID, reason: ReasonEvicted, evicted: true, message: pod.Status.Message}
	for _, st := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
		if ts := st.State.Terminated; ts != nil && ts.FinishedAt.After(t.finished) {
			t.finished = ts.FinishedAt.Time
		}
	}
	if t.finished.IsZero() {
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.DisruptionTarget && c.Status == corev1.ConditionTrue {
				t.finished = c.LastTransitionTime.Time
			}
		}
	}
	return t, true
}

// shownTerminations returns the IDs of the terminations pod's status shows as the current state of its
// containers: what an eviction stopped.
func shownTerminations(pod *corev1.Pod) []string {
	ts := terminations(&corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: currentOnly(pod.Status.InitContainerStatuses),
		ContainerStatuses:     currentOnly(pod.Status.ContainerStatuses),
	}})
	ids := make([]string, 0, len(ts))
	for _, t := range ts {
		ids = append(ids, t.id)
	}
	return ids
}

func currentOnly(sts []corev1.ContainerStatus) []corev1.ContainerStatus {
	out := make([]corev1.ContainerStatus, 0, len(sts))
	for _, st := range sts {
		out = append(out, corev1.ContainerStatus{Name: st.Name, State: st.State})
	}
	return out
}

// containerTerminations returns the terminations of one container status that terminations reads as a
// symptom (an OOM kill or a non-zero exit), with the same reason and ID, so a cause fingerprint built from one
// is the fingerprint of that termination's own observation.
func containerTerminations(st corev1.ContainerStatus) []termination {
	return terminations(&corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{st}}})
}

// evictionEvent reports whether ev is the kubelet's Evicted Warning about a pod.
func evictionEvent(ev *eventsv1.Event) bool {
	return ev.Reason == ReasonEvicted && ev.ReportingController == reportingKubelet &&
		ev.Regarding.Kind == kindPod && ev.Regarding.APIVersion == apiCore
}

// evictionVerdict is what the collector decided on a kubelet Evicted event.
type evictionVerdict uint8

const (
	// evictionEventSymptom: handle the event as the ordinary Event symptom it is, with its own fingerprint and
	// no cause (ADR-0039 point 1).
	evictionEventSymptom evictionVerdict = iota
	// evictionSkip: the pod reconciler delivered the pod's eviction from its status; the event adds nothing.
	evictionSkip
	// evictionWait: the pod's status does not show a delivered eviction yet; decide again after the returned
	// delay.
	evictionWait
)

// settleEviction decides on a kubelet Evicted event (evictionEvent) about pod, the pod the event names as the
// owner chain verified it (nil when it is gone or replaced), from what the pod reconciler delivered and never
// the other way round (decided 2026-11-21): the event writes nothing about the pod, so an Event, forged or not,
// cannot hide or stand in for the pod-status observation. In order:
//
//   - the pod reconciler delivered the eviction of the pod the event names (evictionDeliveries): evictionSkip.
//   - no pod, or the pod is in a final phase without an eviction in its status: evictionEventSymptom.
//   - otherwise evictionWait until EvictionSettleGrace after the collector's first look at the event, by its
//     own clock, then evictionEventSymptom; evictionEventSymptom at once when there is no room to wait. While
//     the status shows the eviction, the wait is at most retryAfter, the time the pod reconciler has to deliver.
//
// An event handled as a symptom after the wait whose pod shows the eviction later gives one more Incident, never
// one less.
func (c *Collector) settleEviction(ctx context.Context, key types.NamespacedName, ev *eventsv1.Event,
	pod *corev1.Pod) (evictionVerdict, time.Duration) {
	if c.evictions.delivered(ev.Regarding.UID) {
		return evictionSkip, 0
	}
	if pod == nil || (!evicted(pod) && (pod.Status.Phase == corev1.PodFailed ||
		pod.Status.Phase == corev1.PodSucceeded)) {
		return evictionEventSymptom, 0
	}
	now := c.now()
	since, ok := c.probes.start(key, ev.UID, now)
	if !ok {
		logf.FromContext(ctx).V(1).Info("Handled Evicted Event as a symptom at once, too many decisions put off",
			"limit", c.probes.max)
		return evictionEventSymptom, 0
	}
	left := since.Add(EvictionSettleGrace).Sub(now)
	if left <= 0 {
		return evictionEventSymptom, 0
	}
	if evicted(pod) {
		left = min(left, retryAfter)
	}
	return evictionWait, left
}

// evictionObservation is the observation of the eviction of the pod podName with podUID in namespace, of
// subject, read from the pod's status: a cause from pod status with its own fingerprint (evictionFingerprint)
// and occurrence ID. began is the eviction's time; mayRepeat is set when the collector cannot tell whether an
// earlier process counted it. containers are the names of the pod's containers: the eviction is a cause of the
// pod, not of one of them, and the late-cause fold matches a consequence by them (ADR-0039 point 13, decided
// 2026-11-21).
func (c *Collector) evictionObservation(subject remediationv1alpha1.ObjectRef, namespace, podName string,
	podUID types.UID, message string, began time.Time, mayRepeat bool, containers []string) Observation {
	owner := string(subject.UID)
	obs := c.observation(c.evictionInput(namespace, owner, podName, podUID, evictionNote(message)), subject,
		namespace, began)
	obs.Fingerprint = c.evictionFingerprint(namespace, owner, podName, podUID, message)
	obs.Occurrence = termination{id: evictionID, evicted: true}.occurrence(podUID)
	obs.Began, obs.Total, obs.MayRepeat = began, 1, mayRepeat
	if !mayRepeat {
		obs.Count = 1
	}
	obs.Immediate = immediateTermination(ReasonEvicted)
	obs.Class = ClassCause
	obs.PodContainers = containers
	return obs
}

// podContainerNames returns the names of pod's containers, init containers first, as its status lists them.
func podContainerNames(pod *corev1.Pod) []string {
	var names []string
	for _, st := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
		if !slices.Contains(names, st.Name) {
			names = append(names, st.Name)
		}
	}
	return names
}

// maxKillsPerPod bounds the delivered terminations remembered per pod for an eviction to count on.
const maxKillsPerPod = 4

// deliveredKill is a container termination of a pod that the pod reconciler delivered: its ID and its
// fingerprint.
type deliveredKill struct {
	id, fingerprint string
}

// evictionRecord is what the collector remembers of one pod for its eviction: the terminations the pod
// reconciler delivered, and the fingerprint the pod's eviction was delivered with ("" while not delivered).
type evictionRecord struct {
	kills    []deliveredKill
	eviction string
}

// evictionDeliveries remembers, per pod UID and for at most a fixed number of pods, what the pod reconciler
// delivered of the pod: its container terminations, for the eviction to count on, and its eviction, for a
// consequence to link to and a kubelet Evicted event to be skipped. Only the pod reconciler writes it.
type evictionDeliveries struct {
	mu   sync.Mutex
	seen *lru[types.UID, evictionRecord]
}

func newEvictionDeliveries(maxPods int) *evictionDeliveries {
	return &evictionDeliveries{seen: newLRU[types.UID, evictionRecord](maxPods, nil)}
}

// causeFor returns the fingerprint of the newest delivered termination of the pod among shown, the IDs of the
// terminations the pod's status shows as its containers' current state (shownTerminations), or "": the
// eviction counts on that termination's Incident and opens none (decided 2026-11-21).
func (d *evictionDeliveries) causeFor(podUID types.UID, shown []string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, _ := d.seen.get(podUID)
	cause := ""
	for _, k := range r.kills {
		if slices.Contains(shown, k.id) {
			cause = k.fingerprint
		}
	}
	return cause
}

// noteEviction records that the pod reconciler delivered the pod's eviction with fingerprint fp.
func (d *evictionDeliveries) noteEviction(podUID types.UID, fp string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, _ := d.seen.get(podUID)
	r.eviction = fp
	d.seen.put(podUID, r)
}

// evictionCause returns the fingerprint the pod's eviction was delivered with, or "" when it was not
// delivered in this process.
func (d *evictionDeliveries) evictionCause(podUID types.UID) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, _ := d.seen.get(podUID)
	return r.eviction
}

// delivered reports whether the pod's eviction was delivered in this process.
func (d *evictionDeliveries) delivered(podUID types.UID) bool {
	return d.evictionCause(podUID) != ""
}

// noteTermination records that the pod reconciler delivered termination t of the pod with fingerprint fp.
func (d *evictionDeliveries) noteTermination(podUID types.UID, t termination, fp string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, _ := d.seen.get(podUID)
	r.kills = append(r.kills, deliveredKill{id: t.id, fingerprint: fp})
	if len(r.kills) > maxKillsPerPod {
		r.kills = r.kills[len(r.kills)-maxKillsPerPod:]
	}
	d.seen.put(podUID, r)
}
