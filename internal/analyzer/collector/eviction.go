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
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
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

// eviction returns the eviction pod's status shows as one symptom of the pod (ADR-0039 point 3, amended
// 2026-11-20), and false when it shows none. The eviction is the cause of the pod: it replaces every
// termination and waiting state of its containers, which are what the eviction did (an exit code 137, a
// ContainerStatusUnknown), so one evicted pod is one occurrence. It names no container. Its finish time is the
// newest finishedAt of the current state of the pod's containers, written by the same kubelet, or zero when
// none has one.
func eviction(pod *corev1.Pod) (termination, bool) {
	if !evicted(pod) {
		return termination{}, false
	}
	t := termination{id: "evicted", reason: ReasonEvicted, evicted: true, message: pod.Status.Message}
	for _, st := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
		if ts := st.State.Terminated; ts != nil && ts.FinishedAt.After(t.finished) {
			t.finished = ts.FinishedAt.Time
		}
	}
	return t, true
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
	// evictionCount: count the event as any other.
	evictionCount evictionVerdict = iota
	// evictionShown: the pod's status shows the eviction, which the pod reconciler reports; no observation.
	evictionShown
	// evictionWait: the pod is not evicted in its status yet; decide again after the returned delay.
	evictionWait
)

// settleEviction decides on a kubelet Evicted event (evictionEvent) about pod, the pod the event names as the
// owner chain verified it (nil when it is gone or replaced). In order:
//
//   - no pod: evictionCount. Nothing else reports the eviction then (a StatefulSet deletes a failed pod at
//     once).
//   - the pod's status shows the eviction (evicted): evictionShown. The pod reconciler reports it as the
//     cause of the pod, with its message, so the event opens nothing of its own.
//   - the pod is in a final phase without the eviction: evictionCount.
//   - otherwise evictionWait until EvictionSettleGrace after the collector's first look at the event, by its
//     own clock, then evictionCount; evictionCount at once when there is no room to wait.
//
// An Event alone never hides a symptom: it is dropped only when the status of the pod it names, verified by
// UID, reports the same eviction.
func (c *Collector) settleEviction(ctx context.Context, key types.NamespacedName, ev *eventsv1.Event,
	pod *corev1.Pod) (evictionVerdict, time.Duration) {
	switch {
	case pod == nil:
		return evictionCount, 0
	case evicted(pod):
		return evictionShown, 0
	case pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded:
		return evictionCount, 0
	}
	now := c.now()
	since, ok := c.probes.start(key, ev.UID, now)
	if !ok {
		logf.FromContext(ctx).V(1).Info("Counted Evicted event at once, too many decisions put off",
			"limit", c.probes.max)
		return evictionCount, 0
	}
	if left := since.Add(EvictionSettleGrace).Sub(now); left > 0 {
		return evictionWait, left
	}
	return evictionCount, 0
}
