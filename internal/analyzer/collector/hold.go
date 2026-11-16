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
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
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

// minHoldSamples is how many times the held container must be seen failing during the wait, the last sample
// included, for the consequence to be emitted.
const minHoldSamples = 2

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
	// restarts is the container's restart count at the first sight: a larger one later is a restart since.
	restarts int32
	// since is the collector's clock at the first sight.
	since time.Time
	// failing is how many reconciles saw the container failing.
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
}

func newConsequenceHolds(capacity int) *consequenceHolds {
	capacity = max(capacity, 1)
	return &consequenceHolds{
		max: capacity, share: max(capacity/holdShareDivisor, 1),
		holds: map[types.NamespacedName]consequenceHold{}, perNS: map[string]int{},
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
	if _, ok := h.holds[key]; !ok {
		if len(h.holds) >= h.max || h.perNS[key.Namespace] >= h.share {
			return false
		}
		h.perNS[key.Namespace]++
		consequencesHeld.Inc()
	}
	h.holds[key] = s
	return true
}

// remove drops key. Called with mu held.
func (h *consequenceHolds) remove(key types.NamespacedName) {
	if _, ok := h.holds[key]; !ok {
		return
	}
	delete(h.holds, key)
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

// holdConsequence decides on a consequence (ADR-0039 point 19). obs is the event's observation and pod the pod
// it names as fromEvent read it (nil when gone or replaced; a failed read never reaches here, it is retried).
//
//   - No hold and not a consequence with no cause: holdEmit, as before.
//   - A hold whose decision to open is not delivered yet: holdEmit.
//   - A held event whose pod is gone or replaced: holdDrop.
//   - A held event whose pod now shows a cause: the hold is released, holdEmit.
//   - No room to hold a new one: holdEmit at once, without a verdict, so it goes through the gate.
//   - Otherwise the reconcile is a sample of the container. Before the end of ConsequenceGrace: holdWait. At
//     the end: holdEmit when this last sample is failing and at least minHoldSamples were, else holdDrop.
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
	if left := s.since.Add(ConsequenceGrace).Sub(now); left > 0 {
		if !c.holds.put(key, s) {
			heldConsequences.WithLabelValues(holdOverflow).Inc()
			logf.FromContext(ctx).V(1).Info("Emitted consequence at once, too many held",
				"limit", c.holds.max, "namespaceShare", c.holds.share)
			return holdEmit, 0
		}
		return holdWait, left
	}
	if !failing || s.failing < minHoldSamples {
		heldConsequences.WithLabelValues(holdNotOpened).Inc()
		logf.FromContext(ctx).V(1).Info("Dropped held consequence, its container did not keep failing",
			"reason", s.reason, "container", s.container, "failingSamples", s.failing)
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

// failingNow reports whether the held container fails in st: not ready for Unhealthy; for BackOff, waiting in
// a back-off or restarted since the first sight.
func (s consequenceHold) failingNow(st corev1.ContainerStatus) bool {
	if s.reason == ReasonUnhealthy {
		return !st.Ready
	}
	return (st.State.Waiting != nil && strings.HasSuffix(st.State.Waiting.Reason, reasonBackOff)) ||
		st.RestartCount > s.restarts
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
