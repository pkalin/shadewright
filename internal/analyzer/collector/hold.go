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
	"errors"
	"fmt"
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

// ConsequenceGrace is how long the collector holds a consequence before it decides on it: one whose pod status
// shows no cause (ADR-0039 point 19), or one whose cause has no Incident yet (point 7). It is measured on the
// collector's clock alone, from its first sight of the occurrence (or of the increase of its series), never
// against a time the cluster reported.
const ConsequenceGrace = 2 * time.Minute

// CausePoll is how often a consequence held for its cause asks again whether the cause was consumed, when no
// update of its pod samples it sooner. The answer comes from the Incident controller's memory in the same
// process (Config.CauseCounted), so a hold costs at most ConsequenceGrace/CausePoll such reconciles besides the
// updates of its pod.
const CausePoll = 10 * time.Second

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
	holdReleased  = "released"
	holdUncaused  = "cause_unopened"
	holdDropped   = "dropped"
	holdOverflow  = "overflow"
)

// heldConsequences counts the ends of consequence holds: opened (no cause shown, emitted for the gate),
// not_opened (no cause shown, and the container recovered, or its pod is gone or replaced), released (its cause
// was consumed during the wait, emitted with it), cause_unopened (its pod showed a cause whose Incident did not
// exist at the end of the wait, or when the pod went: emitted with that cause for the gate), dropped (the event
// ended the hold itself: deleted, no longer a Warning, replaced by another event of the name, or handled without
// an observation for another reason), so that these five add up to the holds started; and overflow (no room to
// hold, emitted at once), a consequence that was never held.
var heldConsequences = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_analyzer_held_consequences_total",
	Help: "Consequence symptoms held by the collector, by outcome: opened, not_opened, released, cause_unopened, " +
		"dropped or overflow.",
}, []string{"outcome"})

// consequencesHeld is the number of consequences held now, summed over the collectors of the process.
var consequencesHeld = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "shadewright_analyzer_consequences_held",
	Help: "Consequence symptoms the collector holds now, with no cause or with a cause that has no Incident yet.",
})

func init() {
	metrics.Registry.MustRegister(heldConsequences, consequencesHeld)
}

// consequenceHold is one held consequence: the event it came from, the container it names in the pod it named
// at the first sight, and what the collector saw of that container since.
type consequenceHold struct {
	// cause is the cause fingerprint the latest sample's pod status showed, empty when it showed none. A hold
	// with a cause waits for the cause to be consumed (ADR-0039 point 7); one without is judged by its samples
	// (point 19).
	cause     string
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
	// overflowed keeps, by event key, the UID of an event emitted at once for want of room until its delivery
	// ends, so that a delivery retry emits it again instead of holding it once room has freed. At most max
	// entries: past that an overflow is not remembered, and its retry may be held.
	overflowed map[types.NamespacedName]types.UID
}

func newConsequenceHolds(capacity int) *consequenceHolds {
	capacity = max(capacity, 1)
	return &consequenceHolds{
		max: capacity, share: max(capacity/holdShareDivisor, 1),
		holds: map[types.NamespacedName]consequenceHold{}, perNS: map[string]int{},
		byPod:      map[types.UID]map[types.NamespacedName]struct{}{},
		overflowed: map[types.NamespacedName]types.UID{},
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

// get returns the hold of the event, dropping the hold (counted dropped) or the overflow mark of an older event
// of the same name.
func (h *consequenceHolds) get(key types.NamespacedName, uid types.UID) (consequenceHold, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if o, ok := h.overflowed[key]; ok && o != uid {
		delete(h.overflowed, key)
	}
	s, ok := h.holds[key]
	if ok && s.event != uid {
		h.dropLocked(key)
		return consequenceHold{}, false
	}
	return s, ok
}

// wasOverflow reports whether the event with this UID was emitted at once for want of room and its delivery has
// not ended yet.
func (h *consequenceHolds) wasOverflow(key types.NamespacedName, uid types.UID) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	o, ok := h.overflowed[key]
	return ok && o == uid
}

// markOverflow remembers that the event with this UID was emitted at once, and reports false when there is no
// room to remember it.
func (h *consequenceHolds) markOverflow(key types.NamespacedName, uid types.UID) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.overflowed[key]; !ok && len(h.overflowed) >= h.max {
		return false
	}
	h.overflowed[key] = uid
	return true
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

// done forgets the event's hold and overflow mark once its observation is delivered or its verdict counted.
func (h *consequenceHolds) done(key types.NamespacedName) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.remove(key)
	delete(h.overflowed, key)
}

// drop forgets the event's hold and overflow mark at an end the hold did not decide (the event deleted, no
// longer a Warning, or handled without an observation for another reason). A hold not decided yet is counted
// dropped; one decided to open was counted opened already.
func (h *consequenceHolds) drop(key types.NamespacedName) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropLocked(key)
	delete(h.overflowed, key)
}

// dropLocked counts and removes an undecided hold of key. Called with mu held.
func (h *consequenceHolds) dropLocked(key types.NamespacedName) {
	if s, ok := h.holds[key]; ok && !s.opened {
		heldConsequences.WithLabelValues(holdDropped).Inc()
	}
	h.remove(key)
}

func (h *consequenceHolds) overflowLen() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.overflowed)
}

func (h *consequenceHolds) len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.holds)
}

// errCauseUnanswered is a CauseCounted call that ran out of its deadline: no answer, asked again later.
var errCauseUnanswered = errors.New("the Incident controller did not answer within the deadline")

// causeCounted asks Config.CauseCounted, with the deadline of a Handled call, whether the cause obs names was
// consumed. An expired deadline is errCauseUnanswered; any other error is returned. Neither is an answer.
func (c *Collector) causeCounted(ctx context.Context, obs Observation) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, c.cfg.LookupTimeout)
	defer cancel()
	ok, err := c.cfg.CauseCounted(cctx, obs)
	switch {
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		return false, errCauseUnanswered
	case err != nil:
		return false, fmt.Errorf("asking whether the cause of a consequence was counted: %w", err)
	}
	return ok, nil
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

// holdConsequence decides on a consequence (ADR-0039 points 7 and 19). obs is the event's observation and pod the
// pod it names as fromEvent read it (nil when gone or replaced; a failed read never reaches here, it is retried).
// A reconcile of the event is a sample, whether its event or an update of its pod (heldEventsOf) caused it.
//
//   - Not a consequence, and not held: holdEmit, as before.
//   - A hold whose decision to emit is not delivered yet: holdEmit.
//   - A pod that shows a cause the Incident controller has consumed (Config.CauseCounted): holdEmit, and a hold
//     is released. Without CauseCounted any cause shown is taken as consumed, as before point 7 was built.
//   - A held event whose pod is gone or replaced: emitted with the cause the previous sample showed (holdEmit),
//     or, when it showed none, holdDrop.
//   - No room to hold a new one: holdEmit at once, without a verdict, so it goes through the gate. The event is
//     remembered until its delivery ends, so a delivery retry emits it again even once room has freed.
//   - Otherwise the reconcile is a sample of the container. Before the end of ConsequenceGrace: holdWait, after
//     CausePoll at most while the pod shows a cause. At the end, when the pod shows a cause whose Incident does
//     not exist: holdEmit with that cause, for the gate (point 7). With no cause shown (amended 2026-11-16):
//     holdEmit when at least minHoldSamples samples were failing and either this last one is failing or the
//     container restarted at least minHoldRestarts times during the hold, else holdDrop.
//
// A CauseCounted call that does not answer within its deadline decides nothing: holdWait after retryAfter, the
// hold as it was. Any other error of it is returned, for the reconciler's backoff.
func (c *Collector) holdConsequence(ctx context.Context, key types.NamespacedName, ev *eventsv1.Event,
	obs *Observation, pod *corev1.Pod) (holdVerdict, time.Duration, error) {
	if c.holdOff || c.holds.wasOverflow(key, ev.UID) {
		return holdEmit, 0, nil
	}
	s, held := c.holds.get(key, ev.UID)
	if held && s.opened {
		s.restoreCause(obs)
		return holdEmit, 0, nil
	}
	if !held && obs.Class != ClassConsequence {
		return holdEmit, 0, nil
	}
	if held && (pod == nil || pod.UID != s.pod) {
		if s.cause != "" {
			// The previous sample showed a cause whose Incident did not exist: the consequence goes to the gate with
			// it, which counts it on the cause's Incident if that opens (point 7: the hold never drops a symptom).
			s.restoreCause(obs)
			heldConsequences.WithLabelValues(holdUncaused).Inc()
			logf.FromContext(ctx).V(1).Info("Emitted held consequence with its cause, its Pod is gone or replaced",
				"reason", s.reason, "container", s.container)
			s.opened = true
			c.holds.put(key, s)
			return holdEmit, 0, nil
		}
		heldConsequences.WithLabelValues(holdNotOpened).Inc()
		logf.FromContext(ctx).V(1).Info("Dropped held consequence, its Pod is gone or replaced",
			"reason", s.reason, "container", s.container)
		return holdDrop, 0, nil
	}
	if done, v, wait, err := c.askCause(ctx, key, ev, obs, held); done {
		return v, wait, err
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
	s.cause = obs.CauseFingerprint
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
			c.holds.markOverflow(key, ev.UID)
			heldConsequences.WithLabelValues(holdOverflow).Inc()
			logf.FromContext(ctx).V(1).Info("Emitted consequence at once, too many held",
				"limit", c.holds.max, "namespaceShare", c.holds.share)
			return holdEmit, 0, nil
		}
		if s.cause != "" {
			return holdWait, min(left, CausePoll), nil
		}
		return holdWait, left, nil
	}
	if s.cause != "" {
		// The end of the wait, and the cause shown has no Incident: to the gate with it (point 7).
		s.opened = true
		if c.holds.put(key, s) {
			heldConsequences.WithLabelValues(holdUncaused).Inc()
			logf.FromContext(ctx).V(1).Info("Emitted held consequence with its cause, which has no Incident",
				"reason", s.reason, "container", s.container)
		}
		return holdEmit, 0, nil
	}
	if s.failing < minHoldSamples || (!failing && s.restarted < minHoldRestarts) {
		heldConsequences.WithLabelValues(holdNotOpened).Inc()
		logf.FromContext(ctx).V(1).Info("Dropped held consequence, its container did not keep failing",
			"reason", s.reason, "container", s.container, "failingSamples", s.failing, "restarts", s.restarted)
		return holdDrop, 0, nil
	}
	s.opened = true
	if !c.holds.put(key, s) {
		// Not held before (a grace of zero): nothing to remember across a delivery retry.
		return holdEmit, 0, nil
	}
	heldConsequences.WithLabelValues(holdOpened).Inc()
	return holdEmit, 0, nil
}

// askCause decides on a consequence whose pod shows a cause, when the cause was consumed or there is no answer
// (holdConsequence): done is false when the hold goes on, its pod showing no cause or a cause not consumed.
func (c *Collector) askCause(ctx context.Context, key types.NamespacedName, ev *eventsv1.Event, obs *Observation,
	held bool) (bool, holdVerdict, time.Duration, error) {
	if obs.CauseFingerprint == "" {
		return false, holdWait, 0, nil
	}
	counted := true
	if c.cfg.CauseCounted != nil {
		var err error
		if counted, err = c.causeCounted(ctx, *obs); errors.Is(err, errCauseUnanswered) {
			// Only while the Incident controller has not loaded its Incidents, whose own error it logs.
			logf.FromContext(ctx).Info("Asked again whether the cause of a consequence was counted, no answer yet",
				"reason", ev.Reason, "retryAfter", retryAfter)
			return true, holdWait, retryAfter, nil
		} else if err != nil {
			return true, holdEmit, 0, err
		}
	}
	if !counted {
		return false, holdWait, 0, nil
	}
	if held {
		// The cause was consumed during the wait: the consequence goes on with it.
		heldConsequences.WithLabelValues(holdReleased).Inc()
		c.holds.done(key)
	}
	return true, holdEmit, 0, nil
}

// restoreCause gives obs the cause the hold recorded when obs shows none: a hold decided to emit with its cause
// keeps that cause across a delivery retry, and one whose pod went is emitted with the cause its last sample
// showed. A hold with no cause changes nothing.
func (s consequenceHold) restoreCause(obs *Observation) {
	if s.cause == "" || obs.CauseFingerprint != "" {
		return
	}
	obs.Class, obs.Container, obs.CauseFingerprint = ClassConsequence, s.container, s.cause
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
