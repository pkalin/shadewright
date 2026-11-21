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
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
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
	// Never nil: nil means the pod was not read (deliverEviction).
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
	// evictionDeliver: deliver the pod's eviction observation now (deliverEviction).
	evictionDeliver evictionVerdict = iota
	// evictionWait: the pod is not evicted in its status yet; decide again after the returned delay.
	evictionWait
)

// settleEviction decides on a kubelet Evicted event (evictionEvent) about pod, the pod the event names as the
// owner chain verified it (nil when it is gone or replaced). In order:
//
//   - no pod, or the pod's status shows the eviction (evicted), or the pod is in a final phase without it:
//     evictionDeliver.
//   - otherwise evictionWait until EvictionSettleGrace after the collector's first look at the event, by its
//     own clock, then evictionDeliver; evictionDeliver at once when there is no room to wait.
//
// The event is never dropped on what the cache shows: it is handled without an observation only once the
// pod's eviction observation was delivered (evictionDeliveries), by whichever of the two reconcilers came
// first (decided 2026-11-21).
func (c *Collector) settleEviction(ctx context.Context, key types.NamespacedName, ev *eventsv1.Event,
	pod *corev1.Pod) (evictionVerdict, time.Duration) {
	if pod == nil || evicted(pod) || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		return evictionDeliver, 0
	}
	now := c.now()
	since, ok := c.probes.start(key, ev.UID, now)
	if !ok {
		logf.FromContext(ctx).V(1).Info("Delivered eviction of an Evicted event at once, too many decisions put off",
			"limit", c.probes.max)
		return evictionDeliver, 0
	}
	if left := since.Add(EvictionSettleGrace).Sub(now); left > 0 {
		return evictionWait, left
	}
	return evictionDeliver, 0
}

// evictionObservation is the one observation of the eviction of the pod podName with podUID in namespace, of
// subject, built the same way from the pod's status (the pod reconciler) and from the kubelet's Evicted event
// (the event reconciler), whose note is the same message: the same fingerprint (evictionFingerprint) and the
// same occurrence ID, so whichever is delivered, the Incident controller counts the eviction once. began is the
// eviction's time; mayRepeat is set when the collector cannot tell whether an earlier process counted it.
// fromStatus is set when message was read from the pod's status: only then is the observation a cause from pod
// status. Built from an event's note, it keeps the event as its origin and has no class, so an Event never
// sets status.cause (ADR-0039 point 8), even one forged with the kubelet's name.
func (c *Collector) evictionObservation(subject remediationv1alpha1.ObjectRef, namespace, podName string,
	podUID types.UID, message string, began time.Time, mayRepeat, fromStatus bool) Observation {
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
	if !fromStatus {
		obs.Origin, obs.Class = fingerprint.SourceEvent, ClassNone
	}
	return obs
}

// evictionOutcome is what deliverEviction did.
type evictionOutcome uint8

const (
	// evictionSent: this call delivered the eviction.
	evictionSent evictionOutcome = iota
	// evictionDone: the other reconciler delivered it before; nothing to send.
	evictionDone
	// evictionRetry: not delivered, try again after retryAfter: the channel stayed full, or the other reconciler
	// is delivering it now.
	evictionRetry
)

// deliverEviction delivers obs, the eviction of the pod with podUID (evictionObservation), unless it was
// delivered before in this process. shown are the IDs of the terminations the pod's status shows as its
// containers' current state, or nil when the pod is not read (gone): when the pod reconciler delivered one of
// them before the eviction showed (the kubelet writes the container's end before the pod's final phase),
// or, with nil, the newest such kill it delivered, the eviction counts on that termination's Incident and
// opens none (decided 2026-11-21).
func (c *Collector) deliverEviction(ctx context.Context, podUID types.UID, obs Observation,
	shown []string) (evictionOutcome, error) {
	prior, cause := c.evictions.claim(podUID, shown)
	switch prior {
	case evictionDelivered:
		return evictionDone, nil
	case evictionDelivering:
		return evictionRetry, nil
	}
	if cause != "" {
		obs.Fingerprint = cause
	}
	sent, err := c.deliver(ctx, obs)
	c.evictions.finish(podUID, err == nil && sent)
	if err != nil {
		return evictionRetry, err
	}
	if !sent {
		return evictionRetry, nil
	}
	return evictionSent, nil
}

// evictionState is where the delivery of one pod's eviction stands in this process.
type evictionState uint8

const (
	evictionNotDelivered evictionState = iota
	evictionDelivering
	evictionDelivered
)

// maxKillsPerPod bounds the delivered terminations remembered per pod for an eviction to count on.
const maxKillsPerPod = 4

// deliveredKill is a container termination of a pod that the pod reconciler delivered: its ID, its
// fingerprint, and whether it is a kill an eviction gives (exit code 137, not an OOM kill).
type deliveredKill struct {
	id, fingerprint string
	kill            bool
}

// evictionRecord is what the collector remembers of one pod for its eviction.
type evictionRecord struct {
	state evictionState
	kills []deliveredKill
}

// evictionDeliveries remembers, per pod UID and for at most a fixed number of pods, whether the pod's
// eviction was delivered and which of its container terminations the pod reconciler delivered. The pod and the
// event reconcilers run apart; claim makes one of them the one that delivers.
type evictionDeliveries struct {
	mu   sync.Mutex
	seen *lru[types.UID, evictionRecord]
}

func newEvictionDeliveries(maxPods int) *evictionDeliveries {
	return &evictionDeliveries{seen: newLRU[types.UID, evictionRecord](maxPods, nil)}
}

// claim returns the state of the pod's eviction, and marks it being delivered by the caller when it was
// neither delivered nor being delivered. It also returns the fingerprint of the delivered termination the
// eviction counts on (deliverEviction), or "".
func (d *evictionDeliveries) claim(podUID types.UID, shown []string) (evictionState, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, _ := d.seen.get(podUID)
	prior := r.state
	if prior == evictionNotDelivered {
		r.state = evictionDelivering
		d.seen.put(podUID, r)
	}
	cause := ""
	for _, k := range r.kills {
		if (shown == nil && k.kill) || slices.Contains(shown, k.id) {
			cause = k.fingerprint
		}
	}
	return prior, cause
}

// finish ends a claim: delivered, or open again for the next try.
func (d *evictionDeliveries) finish(podUID types.UID, delivered bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, _ := d.seen.get(podUID)
	r.state = evictionNotDelivered
	if delivered {
		r.state = evictionDelivered
	}
	d.seen.put(podUID, r)
}

// delivered reports whether the pod's eviction was delivered in this process.
func (d *evictionDeliveries) delivered(podUID types.UID) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, _ := d.seen.get(podUID)
	return r.state == evictionDelivered
}

// noteTermination records that the pod reconciler delivered termination t of the pod with fingerprint fp.
func (d *evictionDeliveries) noteTermination(podUID types.UID, t termination, fp string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, _ := d.seen.get(podUID)
	r.kills = append(r.kills, deliveredKill{id: t.id, fingerprint: fp,
		kill: t.exitCode == evictionKillExitCode && t.reason != ReasonOOMKilled})
	if len(r.kills) > maxKillsPerPod {
		r.kills = r.kills[len(r.kills)-maxKillsPerPod:]
	}
	d.seen.put(podUID, r)
}

// evictionKillExitCode is the exit code of a container the kubelet killed (SIGKILL), as an eviction does once
// the grace period ends, and as the kubelet writes for a container it reports as ContainerStatusUnknown.
const evictionKillExitCode = 137

// evictionFromEvent handles a kubelet Evicted event (evictionEvent) about the pod the event names, of subject,
// with pod as the owner chain read it (nil when gone or replaced). It skips the event once the pod's eviction
// observation was delivered; otherwise, when settleEviction says so, it delivers that observation itself, built
// from the pod's status when the status shows the eviction, from the event's note and time when not. leave
// handles the event without an observation of its own.
func (r *eventReconciler) evictionFromEvent(ctx context.Context, key types.NamespacedName, ev *eventsv1.Event,
	subject remediationv1alpha1.ObjectRef, pod *corev1.Pod, start time.Time, mayRepeat bool,
	leave func(string, bool) (ctrl.Result, error)) (ctrl.Result, error) {
	podUID := ev.Regarding.UID
	if r.c.evictions.delivered(podUID) {
		logf.FromContext(ctx).V(1).Info("Skipped Evicted event, its pod's eviction was delivered")
		return leave("its pod's eviction was delivered", true)
	}
	verdict, wait := r.c.settleEviction(ctx, key, ev, pod)
	if verdict == evictionWait {
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	var obs Observation
	var shown []string
	if e, ok := eviction(pod); ok {
		began := r.c.notFuture(e.finished)
		if began.IsZero() {
			began = r.c.now()
		}
		obs = r.c.evictionObservation(subject, pod.Namespace, pod.Name, podUID, e.message, began,
			mayRepeat || began.Before(start), true)
		shown = shownTerminations(pod)
	} else {
		obs = r.c.evictionObservation(subject, ev.Namespace, ev.Regarding.Name, podUID, ev.Note,
			r.c.notFuture(lastOccurrence(ev)), mayRepeat, false)
		if pod != nil {
			shown = shownTerminations(pod)
		}
	}
	out, err := r.c.deliverEviction(ctx, podUID, obs, shown)
	if err != nil {
		return ctrl.Result{}, err
	}
	if out == evictionRetry {
		return ctrl.Result{RequeueAfter: retryAfter}, nil
	}
	return leave("its pod's eviction was delivered", true)
}
