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
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ConsequenceGrace is how long the collector holds a consequence whose pod status shows no cause before it
// decides on it (ADR-0039 point 19). It is measured on the collector's clock alone, from its first sight of the
// occurrence (or of the increase of its series), never against a time the cluster reported.
const ConsequenceGrace = 2 * time.Minute

// DefaultMaxHeldConsequences bounds the consequences held at once (ADR-0039 point 19).
const DefaultMaxHeldConsequences = 1024

// holdShareDivisor sets the share of one namespace in the held consequences: a quarter, as for the occurrences
// the Incident controller holds unwritten (package incidents, DefaultNamespaceShareDivisor).
const holdShareDivisor = 4

// minHoldSamples is how many samples of the wait must see the held container failing for the consequence to be
// emitted.
const minHoldSamples = 2

// minHoldRestarts is how many restarts during the wait open the consequence even when the last sample sees the
// container running (a liveness loop sampled between two kills).
const minHoldRestarts = 2

// Outcomes of a hold, the label values of heldConsequences.
const (
	holdOpened    = "opened"
	holdNotOpened = "not_opened"
	holdOverflow  = "overflow"
)

// heldConsequences counts the ends of consequence holds: opened (emitted for the gate), not_opened (the
// container recovered, or its pod is gone or replaced), or overflow (no room to hold, emitted at once).
var heldConsequences = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_analyzer_held_consequences_total",
	Help: "Consequence symptoms with no cause held by the collector, by outcome: opened, not_opened or overflow.",
}, []string{"outcome"})

// consequencesHeld is the number of consequences held now, summed over the collectors of the process.
var consequencesHeld = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "shadewright_analyzer_consequences_held",
	Help: "Consequence symptoms with no cause the collector holds now, waiting for the end of their grace period.",
})

func init() {
	metrics.Registry.MustRegister(heldConsequences, consequencesHeld)
}

// consequenceHold is one held consequence: the event it came from, the container it names in the pod it named
// at the first sight, and what the collector saw of that container since.
type consequenceHold struct {
	event     types.UID
	pod       types.UID
	container string
	init      bool
	reason    string
	// restarts is the container's restart count at the previous sample (at the first sight for the first one):
	// a larger one at the next sample is a restart between the two.
	restarts int32
	// restarted is how many restarts the samples saw since the first sight.
	restarted int32
	// since is the collector's clock at the first sight.
	since time.Time
	// failing is how many samples saw the container failing.
	failing int
	// opened is set once the hold decided to emit, so a delivery retried later still emits.
	opened bool
}

// consequenceHolds keeps the held consequences, keyed by the event's namespace and name, at most max in all
// and share per namespace. It never drops one to make room: when it is full, a new consequence is not held. It
// is in memory only; a restart forgets it, and the hold starts again from the new process's first sight.
type consequenceHolds struct {
	mu    sync.Mutex
	max   int
	share int
	holds map[types.NamespacedName]consequenceHold
	perNS map[string]int
	// byPod indexes the holds by the UID of their pod, for the samples an update of the pod makes. It has an
	// entry per hold and no other, so it is bounded as holds is.
	byPod map[types.UID]map[types.NamespacedName]struct{}
}

func newConsequenceHolds(capacity int) *consequenceHolds {
	capacity = max(capacity, 1)
	return &consequenceHolds{
		max: capacity, share: max(capacity/holdShareDivisor, 1),
		holds: map[types.NamespacedName]consequenceHold{}, perNS: map[string]int{},
		byPod: map[types.UID]map[types.NamespacedName]struct{}{},
	}
}

// ofPod returns the keys of the events held on the pod with this UID and not decided yet. A hold decided to
// open waits for its delivery retry (retryAfter), which an update of the pod must not hurry.
func (h *consequenceHolds) ofPod(uid types.UID) []types.NamespacedName {
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := make([]types.NamespacedName, 0, len(h.byPod[uid]))
	for k := range h.byPod[uid] {
		if !h.holds[k].opened {
			keys = append(keys, k)
		}
	}
	return keys
}

// index adds key to the holds of pod. Called with mu held.
func (h *consequenceHolds) index(pod types.UID, key types.NamespacedName) {
	if h.byPod[pod] == nil {
		h.byPod[pod] = map[types.NamespacedName]struct{}{}
	}
	h.byPod[pod][key] = struct{}{}
}

// unindex drops key from the holds of pod. Called with mu held.
func (h *consequenceHolds) unindex(pod types.UID, key types.NamespacedName) {
	if delete(h.byPod[pod], key); len(h.byPod[pod]) == 0 {
		delete(h.byPod, pod)
	}
}

// get returns the hold of the event, dropping the hold of an older event of the same name.
func (h *consequenceHolds) get(key types.NamespacedName, uid types.UID) (consequenceHold, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.holds[key]
	if ok && s.event != uid {
		h.remove(key)
		return consequenceHold{}, false
	}
	return s, ok
}

// put stores s under key, and reports false when key is new and there is no room, in all or in its namespace.
func (h *consequenceHolds) put(key types.NamespacedName, s consequenceHold) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	old, ok := h.holds[key]
	if !ok {
		if len(h.holds) >= h.max || h.perNS[key.Namespace] >= h.share {
			return false
		}
		h.perNS[key.Namespace]++
		consequencesHeld.Inc()
	} else if old.pod != s.pod {
		h.unindex(old.pod, key)
	}
	h.index(s.pod, key)
	h.holds[key] = s
	return true
}

// remove drops key. Called with mu held.
func (h *consequenceHolds) remove(key types.NamespacedName) {
	s, ok := h.holds[key]
	if !ok {
		return
	}
	delete(h.holds, key)
	h.unindex(s.pod, key)
	consequencesHeld.Dec()
	if h.perNS[key.Namespace]--; h.perNS[key.Namespace] <= 0 {
		delete(h.perNS, key.Namespace)
	}
}

// done forgets the event's hold.
func (h *consequenceHolds) done(key types.NamespacedName) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.remove(key)
}

func (h *consequenceHolds) len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.holds)
}

// holdVerdict is what the collector decided on a consequence.
type holdVerdict uint8

const (
	// holdEmit: emit the observation, as any other (not held, released or decided to open).
	holdEmit holdVerdict = iota
	// holdWait: held; read the event again after the returned delay.
	holdWait
	// holdDrop: the hold ended and opens nothing; the occurrences are handled without an observation.
	holdDrop
)

// heldEventsOf maps an update of a pod to the events held on it, so that the update is a sample of each hold
// (ADR-0039 point 19: "two reconciles of the pod or the event"). A container that recovers sends no more events,
// and only its pod's updates show the recovery. It enqueues nothing for a pod with no hold, keeps no state of its
// own, and an event reconcile it causes writes nothing, so it cannot feed itself.
func (c *Collector) heldEventsOf(_ context.Context, obj client.Object) []reconcile.Request {
	keys := c.holds.ofPod(obj.GetUID())
	reqs := make([]reconcile.Request, 0, len(keys))
	for _, k := range keys {
		reqs = append(reqs, reconcile.Request{NamespacedName: k})
	}
	return reqs
}

// holdConsequence decides on a consequence (ADR-0039 point 19). obs is the event's observation and pod the pod
// it names as fromEvent read it (nil when gone or replaced; a failed read never reaches here, it is retried).
// A reconcile of the event is a sample, whether its event or an update of its pod (heldEventsOf) caused it.
//
//   - No hold and not a consequence with no cause: holdEmit, as before.
//   - A hold whose decision to open is not delivered yet: holdEmit.
//   - A held event whose pod is gone or replaced: holdDrop.
//   - A held event whose pod now shows a cause: the hold is released, holdEmit.
//   - No room to hold a new one: holdEmit at once, without a verdict, so it goes through the gate.
//   - Otherwise the reconcile is a sample of the container. Before the end of ConsequenceGrace: holdWait. At
//     the end (amended 2026-11-16): holdEmit when at least minHoldSamples samples were failing and either this
//     last one is failing or the container restarted at least minHoldRestarts times during the hold, else
//     holdDrop.
func (c *Collector) holdConsequence(ctx context.Context, key types.NamespacedName, ev *eventsv1.Event,
	obs Observation, pod *corev1.Pod) (holdVerdict, time.Duration) {
	if c.holdOff {
		return holdEmit, 0
	}
	s, held := c.holds.get(key, ev.UID)
	if !held && (obs.Class != ClassConsequence || obs.CauseFingerprint != "") {
		return holdEmit, 0
	}
	if held && s.opened {
		return holdEmit, 0
	}
	if held && (pod == nil || pod.UID != s.pod) {
		heldConsequences.WithLabelValues(holdNotOpened).Inc()
		logf.FromContext(ctx).V(1).Info("Dropped held consequence, its Pod is gone or replaced",
			"reason", s.reason, "container", s.container)
		return holdDrop, 0
	}
	if held && obs.CauseFingerprint != "" {
		// A cause appeared during the wait: the consequence goes to the gate with it.
		c.holds.done(key)
		return holdEmit, 0
	}
	now := c.now()
	st, found := containerStatus(pod, s.container, s.init)
	if !held {
		container, init := namedContainer(ev.Regarding, pod)
		st, found = containerStatus(pod, container, init)
		s = consequenceHold{
			event: ev.UID, pod: pod.UID, container: container, init: init, reason: ev.Reason, since: now,
			restarts: st.RestartCount,
		}
	}
	failing := found && s.failingNow(st)
	if failing {
		s.failing++
	}
	if found {
		if st.RestartCount > s.restarts {
			s.restarted += st.RestartCount - s.restarts
		}
		s.restarts = st.RestartCount
	}
	if left := s.since.Add(ConsequenceGrace).Sub(now); left > 0 {
		if !c.holds.put(key, s) {
			heldConsequences.WithLabelValues(holdOverflow).Inc()
			logf.FromContext(ctx).V(1).Info("Emitted consequence at once, too many held",
				"limit", c.holds.max, "namespaceShare", c.holds.share)
			return holdEmit, 0
		}
		return holdWait, left
	}
	if s.failing < minHoldSamples || (!failing && s.restarted < minHoldRestarts) {
		heldConsequences.WithLabelValues(holdNotOpened).Inc()
		logf.FromContext(ctx).V(1).Info("Dropped held consequence, its container did not keep failing",
			"reason", s.reason, "container", s.container, "failingSamples", s.failing, "restarts", s.restarted)
		return holdDrop, 0
	}
	s.opened = true
	if !c.holds.put(key, s) {
		// Not held before (a grace of zero): nothing to remember across a delivery retry.
		return holdEmit, 0
	}
	heldConsequences.WithLabelValues(holdOpened).Inc()
	return holdEmit, 0
}

// failingNow reports whether the held container fails in st (ADR-0039 point 19, amended 2026-11-16): its restart
// count went up since the previous sample of the hold, or it is not ready (Unhealthy) or waiting in a back-off
// (BackOff). A container that restarted once and then runs fine is failing at that one sample only, and a
// liveness loop with no readiness probe, ready at each sample, is failing by its restarts.
func (s consequenceHold) failingNow(st corev1.ContainerStatus) bool {
	if st.RestartCount > s.restarts {
		return true
	}
	if s.reason == ReasonUnhealthy {
		return !st.Ready
	}
	return st.State.Waiting != nil && strings.HasSuffix(st.State.Waiting.Reason, reasonBackOff)
}

// containerStatus returns the status of the named container (init or not) of pod.
func containerStatus(pod *corev1.Pod, container string, init bool) (corev1.ContainerStatus, bool) {
	statuses := pod.Status.ContainerStatuses
	if init {
		statuses = pod.Status.InitContainerStatuses
	}
	i := slices.IndexFunc(statuses, func(s corev1.ContainerStatus) bool { return s.Name == container })
	if i < 0 {
		return corev1.ContainerStatus{}, false
	}
	return statuses[i], true
}
