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
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/pkalin/shadewright/internal/incidents/causereason"
)

// ConsequenceGrace is how long the collector holds a consequence before it decides on it: one whose pod status
// shows no cause (ADR-0039 point 19), or one whose cause the Incident controller has not seen yet (point 7). It
// is measured on the collector's clock alone, from its first sight of the occurrence (or of the increase of its
// series), never against a time the cluster reported.
const ConsequenceGrace = 2 * time.Minute

// DefaultMaxHeldConsequences bounds the consequences held at once in each of the two shares (ADR-0039 points 7
// and 19): those first seen with no cause, and those first seen with a cause the Incident controller had not
// seen.
const DefaultMaxHeldConsequences = 1024

// holdShareDivisor sets the share of one namespace in each share of the held consequences: a quarter, as for the
// occurrences the Incident controller holds unwritten (package incidents, DefaultNamespaceShareDivisor).
const holdShareDivisor = 4

// minHoldSamples is how many samples of the wait must see the held container failing for the consequence to be
// emitted.
const minHoldSamples = 2

// minHoldRestarts is how many restarts during the wait open the consequence even when the last sample sees the
// container running (a liveness loop sampled between two kills).
const minHoldRestarts = 2

// Outcomes of a hold, the label values of heldConsequences.
const (
	holdOpened      = "opened"
	holdNotOpened   = "not_opened"
	holdReleased    = "released"
	holdCauseUnseen = "cause_unseen"
	holdDropped     = "dropped"
	holdOverflow    = "overflow"
	// holdCauseUnconfirmed and holdCauseUnanswered count the removals of a non-zero-exit cause
	// (settleExitCause), not ends of a hold.
	holdCauseUnconfirmed = "cause_unconfirmed"
	holdCauseUnanswered  = "cause_unanswered"
)

// heldConsequences counts the ends of consequence holds: opened (no cause shown, emitted for the gate),
// not_opened (the container recovered: with no cause, or with a cause the controller had not seen when the wait
// ended; or, with no cause shown, its pod is gone or replaced), released (the
// Incident controller saw, during the wait, a cause the consequence would be counted on, or the pod of a hold
// with no cause showed one it had seen: emitted with it at once), cause_unseen (the hold had a cause the
// controller had not seen when the wait ended with the container still failing, or when the pod went, was replaced, or its template was retired: emitted with that cause), dropped (the event
// ended the hold itself: deleted, no longer a Warning, replaced by another event of the name, or handled without
// an observation for another reason; for a hold with a cause only an event that no longer exists; a hold whose
// non-zero exit cause was removed counts as one with the OOM kill in its place, or with no cause), so that these
// five add up to the holds started; and overflow (no room to hold, emitted at once), a consequence that was
// never held. Two more count removals of a non-zero-exit cause by settleExitCause, held or not, beside the end
// of the hold: cause_unconfirmed (CauseSeen answered no, or is not configured) and cause_unanswered (CauseSeen
// failed or ran out of its deadline when the decision was due).
var heldConsequences = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_analyzer_held_consequences_total",
	Help: "Consequence symptoms held by the collector, by outcome: opened, not_opened, released, cause_unseen, " +
		"dropped or overflow; and removals of a non-zero-exit cause: cause_unconfirmed or cause_unanswered.",
}, []string{"outcome"})

// consequencesHeld is the number of consequences held now, summed over the collectors of the process.
var consequencesHeld = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "shadewright_analyzer_consequences_held",
	Help: "Consequence symptoms the collector holds now, with no cause or with a cause the Incident controller " +
		"has not seen yet.",
})

func init() {
	metrics.Registry.MustRegister(heldConsequences, consequencesHeld)
}

// consequenceHold is one held consequence: the event it came from, the container it names in the pod it named
// at the first sight, and what the collector saw of that container since.
type consequenceHold struct {
	// cause is the last cause fingerprint a sample's pod status showed, empty while none did. A sample that
	// shows no cause keeps it: a pod that is replaced or whose template is retired during the hold emits the
	// consequence with it, as does one that recovers when the controller sees the cause (ADR-0039 point 7,
	// decided 2026-11-17). A hold with a cause waits for the Incident controller to see it; at the end of the
	// wait it is judged by its samples (point 19), as one without is (decided 2026-11-17, fifth). A non-zero exit
	// is the exception (decided 2026-11-21 on #432): kept only once Config.CauseSeen confirmed it; else
	// settleExitCause, asked at the end, puts the OOM kill of the window in its place, or no cause, and the hold
	// ends as one with that cause.
	cause string
	// obs is the observation of the last sample that showed cause, without its count: what a hold with a cause
	// emits, whatever the pod or the owner chain look like at its end.
	obs Observation
	// caused is the share the hold was admitted to: set when its first sight showed a cause. It does not change
	// for the life of the hold.
	caused bool
	// releasedFor is the cause fingerprint ReleaseCause last named for this hold while it showed that cause. It
	// is a hint, never a verdict: the reconcile it wakes asks Config.CauseSeen with obs, and only that answer
	// releases the hold (decided 2026-11-17, second, on #828). A sample that changes the cause forgets it (put).
	releasedFor string
	event       types.UID
	pod         types.UID
	container   string
	init        bool
	reason      string
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
	// total is the event's series total at the first delivery of the decision: the occurrences released under
	// cause. A retried delivery whose pod shows another cause by then delivers only these, and holds the later
	// ones again for the new cause (decided 2026-11-17, fourth, on #828). Zero until the first delivery.
	total int64
}

// shareKey names the share of one namespace: holds first seen with a cause apart from those with none.
type shareKey struct {
	namespace string
	caused    bool
}

// consequenceHolds keeps the held consequences, keyed by the event's namespace and name, at most max and share
// per namespace in each of the two shares (consequenceHold.caused). It never drops one to make room: when its
// share is full, a new consequence is not held. It is in memory only; a restart forgets it, and the hold starts
// again from the new process's first sight.
type consequenceHolds struct {
	mu    sync.Mutex
	max   int
	share int
	holds map[types.NamespacedName]consequenceHold
	// total and perNS count the holds of each share, in all and per namespace.
	total map[bool]int
	perNS map[shareKey]int
	// byPod indexes the holds by the UID of their pod, for the samples an update of the pod makes. It has an
	// entry per hold and no other, so it is bounded as holds is.
	byPod map[types.UID]map[types.NamespacedName]struct{}
	// byCause indexes the holds with a cause by that cause fingerprint, for ReleaseCause. Bounded as byPod.
	byCause map[string]map[types.NamespacedName]struct{}
	// overflowed keeps, by event key, the UID of an event emitted at once for want of room until its delivery
	// ends, so that a delivery retry emits it again instead of holding it once room has freed. At most max
	// entries: past that an overflow is not remembered, and its retry may be held.
	overflowed map[types.NamespacedName]types.UID
}

func newConsequenceHolds(capacity int) *consequenceHolds {
	capacity = max(capacity, 1)
	return &consequenceHolds{
		max: capacity, share: max(capacity/holdShareDivisor, 1),
		holds: map[types.NamespacedName]consequenceHold{}, total: map[bool]int{}, perNS: map[shareKey]int{},
		byPod:      map[types.UID]map[types.NamespacedName]struct{}{},
		byCause:    map[string]map[types.NamespacedName]struct{}{},
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

// release marks every hold of cause fingerprint fp not decided yet as released for fp
// (consequenceHold.releasedFor), and returns their keys.
func (h *consequenceHolds) release(fp string) []types.NamespacedName {
	h.mu.Lock()
	defer h.mu.Unlock()
	var keys []types.NamespacedName
	for k := range h.byCause[fp] {
		if s := h.holds[k]; !s.opened {
			s.releasedFor = fp
			h.holds[k] = s
			keys = append(keys, k)
		}
	}
	return keys
}

// addTo adds key to the set of id in index. Called with mu held.
func addTo[K comparable](index map[K]map[types.NamespacedName]struct{}, id K, key types.NamespacedName) {
	if index[id] == nil {
		index[id] = map[types.NamespacedName]struct{}{}
	}
	index[id][key] = struct{}{}
}

// removeFrom drops key from the set of id in index. Called with mu held.
func removeFrom[K comparable](index map[K]map[types.NamespacedName]struct{}, id K, key types.NamespacedName) {
	if delete(index[id], key); len(index[id]) == 0 {
		delete(index, id)
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

// decided returns the hold of the event with this UID when it has decided to emit, and records occurrences as
// its total (consequenceHold.total) at the first call, the first delivery of the decision.
func (h *consequenceHolds) decided(key types.NamespacedName, uid types.UID, occurrences int64) (consequenceHold,
	bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.holds[key]
	if !ok || s.event != uid || !s.opened {
		return consequenceHold{}, false
	}
	if s.total == 0 {
		s.total = occurrences
		h.holds[key] = s
	}
	return s, true
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

// put stores s under key and returns the hold as stored, or false when key is new and there is no room in the
// share of s (a cause shown at the first sight or not), in all or in its namespace. A hold stored already keeps
// its share, and the release mark of the hold as stored (ReleaseCause may have set it since the caller read it)
// while s keeps the stored cause; when s changes the cause, the mark is forgotten (decided 2026-11-17, second,
// on #828): it was for the old cause, and the caller asks again for the new one.
func (h *consequenceHolds) put(key types.NamespacedName, s consequenceHold) (consequenceHold, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	old, ok := h.holds[key]
	if !ok {
		s.caused = s.cause != ""
		sk := shareKey{namespace: key.Namespace, caused: s.caused}
		if h.total[s.caused] >= h.max || h.perNS[sk] >= h.share {
			return consequenceHold{}, false
		}
		h.total[s.caused]++
		h.perNS[sk]++
		consequencesHeld.Inc()
	} else {
		s.caused, s.releasedFor = old.caused, ""
		if old.cause == s.cause {
			s.releasedFor = old.releasedFor
		}
		if old.pod != s.pod {
			removeFrom(h.byPod, old.pod, key)
		}
		if old.cause != "" && old.cause != s.cause {
			removeFrom(h.byCause, old.cause, key)
		}
	}
	addTo(h.byPod, s.pod, key)
	if s.cause != "" {
		addTo(h.byCause, s.cause, key)
	}
	h.holds[key] = s
	return s, true
}

// remove drops key. Called with mu held.
func (h *consequenceHolds) remove(key types.NamespacedName) {
	s, ok := h.holds[key]
	if !ok {
		return
	}
	delete(h.holds, key)
	removeFrom(h.byPod, s.pod, key)
	if s.cause != "" {
		removeFrom(h.byCause, s.cause, key)
	}
	consequencesHeld.Dec()
	if h.total[s.caused]--; h.total[s.caused] <= 0 {
		delete(h.total, s.caused)
	}
	sk := shareKey{namespace: key.Namespace, caused: s.caused}
	if h.perNS[sk]--; h.perNS[sk] <= 0 {
		delete(h.perNS, sk)
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

// ReleaseCause is how the Incident controller tells the collector that it has seen an observation of cause
// fingerprint fp (ADR-0039 point 7, decided 2026-11-17; incidents.Controller.OnCauseSeen): the consequences
// held for that cause are marked released for fp and their events are added to the event controller's queue.
// The mark decides nothing: each of those reconciles is a sample that asks Config.CauseSeen for the cause the
// hold shows then, with its own observation, so a hold of another subject, or one whose cause changed since, is
// released only when the controller has seen a cause it would be counted on (decided 2026-11-17, second, on
// #828). It reads nothing from the API server, never blocks (a lock held for a few map lookups, then the
// queue's Add) and calls nothing back. It is safe to call from any
// goroutine, before the collector starts too: a hold it marks is emitted at its next reconcile whatever causes
// that, at the latest at the end of ConsequenceGrace.
//
// The order that makes it race-free with the hold's creation: the controller records the cause before it calls
// ReleaseCause, and the collector stores a hold before it asks Config.CauseSeen. A call that comes before the
// hold is stored was preceded by the record, so the ask after the store sees it; a call after the store finds
// the hold.
func (c *Collector) ReleaseCause(fp string) {
	keys := c.holds.release(fp)
	if len(keys) == 0 {
		return
	}
	c.releaseMu.Lock()
	q := c.releaseQueue
	c.releaseMu.Unlock()
	if q == nil {
		return
	}
	for _, k := range keys {
		q.Add(reconcile.Request{NamespacedName: k})
	}
}

// releases is the source through which the event controller hands its work queue to ReleaseCause when it
// starts. It starts no goroutine and keeps nothing but the queue, which the controller shuts down when it stops;
// an Add after that is a no-op.
func (c *Collector) releases() source.Source {
	return source.Func(func(_ context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
		c.releaseMu.Lock()
		defer c.releaseMu.Unlock()
		c.releaseQueue = q
		return nil
	})
}

// errCauseUnanswered is a CauseSeen call that ran out of its deadline: no answer, asked again later.
var errCauseUnanswered = errors.New("the Incident controller did not answer within the deadline")

// causeSeen asks Config.CauseSeen, with the deadline of a Handled call, whether the Incident controller has seen
// the cause obs names. Without CauseSeen every cause counts as seen except a non-zero exit, which counts as seen
// only on the Incident controller's word (decided 2026-11-21 on #432): with no controller to ask, it is never
// confirmed, and its consequence ends as one with no cause. An expired deadline is errCauseUnanswered; any other
// error is returned. Neither is an answer. No lock of the collector is held across the call.
func (c *Collector) causeSeen(ctx context.Context, obs Observation) (bool, error) {
	if c.cfg.CauseSeen == nil {
		return !obs.causeExit, nil
	}
	cctx, cancel := context.WithTimeout(ctx, c.cfg.LookupTimeout)
	defer cancel()
	ok, err := c.cfg.CauseSeen(cctx, obs)
	switch {
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		return false, errCauseUnanswered
	case err != nil:
		return false, fmt.Errorf("asking whether the cause of a consequence was seen: %w", err)
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
// A reconcile of the event is a sample, whether its event, an update of its pod (heldEventsOf) or ReleaseCause
// caused it; a sample asks Config.CauseSeen for the cause it shows, so a release for an older cause decides
// nothing.
//
//   - Not a consequence, and not held: holdEmit, as before.
//   - A hold whose decision to emit is not delivered yet: holdEmit, with the observation it decided on.
//   - A held event whose pod is gone or replaced: emitted with the last cause a sample showed (cause_unseen, or
//     released when the controller confirms it), or, when none did, holdDrop. A non-zero exit is asked about
//     once more (endCause): not confirmed, the OOM kill of the window takes its place, or no cause.
//   - No room to hold a new one: holdEmit at once, without a verdict, so it goes through the gate. The event is
//     remembered until its delivery ends, so a delivery retry emits it again even once room has freed. Its cause
//     goes with it as shown, except a non-zero exit, which emit asks about (settleExitCause): confirmed it stays,
//     else the OOM kill of the window or no cause takes its place, at once and without waiting.
//   - Otherwise the reconcile is a sample of the container. A hold with a cause is stored first, then the
//     controller is asked whether it has seen a cause the consequence would be counted on (Config.CauseSeen,
//     with the hold's observation; ReleaseCause gives the order): seen at the first sight, the consequence is
//     emitted at once and was never held; seen later, it is released. A non-zero exit that the controller has
//     not seen gives way at once to an OOM kill of the same container in the window, if any (askCause), which
//     is then the hold's cause, as main links it. Before the end of ConsequenceGrace:
//     holdWait until that end. At the end (endOfHold), a hold with a cause that causeOutcome confirms released
//     emits with it, even when the pod shows none now. Any other hold, with a cause or none (amended 2026-11-16
//     and 2026-11-17, fifth): holdEmit, with the cause if any (cause_unseen; a non-zero exit is asked about once
//     more and, not confirmed, gives way to the OOM kill of the window or to no cause, decided 2026-11-21 on
//     #432, endCause), when at least minHoldSamples
//     samples were failing and either this last one is failing or the container restarted at least
//     minHoldRestarts times during the hold, else holdDrop (not_opened).
//
// A hold with a cause emits the observation of the last sample that showed it (consequenceHold.obs). A CauseSeen
// call that does not answer within its deadline decides nothing: holdWait after retryAfter, the hold as it was.
// Any other error of it is returned, for the reconciler's backoff.
func (c *Collector) holdConsequence(ctx context.Context, key types.NamespacedName, ev *eventsv1.Event,
	obs *Observation, pod *corev1.Pod) (holdVerdict, time.Duration, error) {
	if c.holdOff || c.holds.wasOverflow(key, ev.UID) {
		return holdEmit, 0, nil
	}
	s, held := c.holds.get(key, ev.UID)
	switch {
	case held && s.opened:
		if s.cause != "" {
			*obs = s.obs
		}
		return holdEmit, 0, nil
	case !held && obs.Class != ClassConsequence:
		return holdEmit, 0, nil
	case held && (pod == nil || pod.UID != s.pod):
		outcome, err := c.endCause(ctx, &s)
		if err != nil {
			return holdEmit, 0, err
		}
		if s.cause != "" {
			return c.endWithCause(ctx, key, s, obs, outcome, "its Pod is gone or replaced")
		}
		heldConsequences.WithLabelValues(holdNotOpened).Inc()
		logf.FromContext(ctx).V(1).Info("Dropped held consequence, its Pod is gone or replaced",
			"reason", s.reason, "container", s.container)
		return holdDrop, 0, nil
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
	if obs.CauseFingerprint != "" {
		s.cause, s.obs = obs.CauseFingerprint, *obs
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
	left := s.since.Add(ConsequenceGrace).Sub(now)
	if left <= 0 {
		return c.endOfHold(ctx, key, s, obs, failing)
	}
	stored, ok := c.holds.put(key, s)
	if !ok {
		c.holds.markOverflow(key, ev.UID)
		heldConsequences.WithLabelValues(holdOverflow).Inc()
		logf.FromContext(ctx).V(1).Info("Emitted consequence at once, too many held",
			"limit", c.holds.max, "namespaceShare", c.holds.share, "cause", s.cause != "")
		return holdEmit, 0, nil
	}
	if stored.cause == "" {
		return holdWait, left, nil
	}
	// Asked only now that the hold is stored, so that a ReleaseCause call either came before and is seen here,
	// or finds the hold.
	seen, err := c.askCause(ctx, key, &stored)
	switch {
	case errors.Is(err, errCauseUnanswered):
		// Only while the Incident controller has not loaded its Incidents, whose own error it logs.
		logf.FromContext(ctx).Info("Asked again whether the Incident controller saw the cause of a consequence, "+
			"no answer yet", "reason", ev.Reason, "retryAfter", retryAfter)
		return holdWait, min(retryAfter, left), nil
	case err != nil:
		return holdEmit, 0, err
	case !seen:
		return holdWait, left, nil
	case !held:
		// Seen before the first sight: emitted at once, never held.
		c.holds.done(key)
		*obs = stored.obs
		obs.causeConfirmed = true
		return holdEmit, 0, nil
	}
	return c.endWithCause(ctx, key, stored, obs, holdReleased, "the Incident controller saw its cause")
}

// endOfHold decides on a hold at the end of ConsequenceGrace by its samples (point 19), with a cause or without
// (decided 2026-11-17, fifth, on #828): a consequence whose container did not keep failing is not emitted
// (not_opened), one that did is emitted, with its cause when it has one. failing is whether this last sample saw
// the container failing.
//
// A hold with a cause the controller saw (causeOutcome is released: ReleaseCause marked it and CauseSeen
// confirms) is emitted whether its container still fails or not, as a release before the end is: the controller
// links the consequence to its cause's Incident, which counts it as evidence and opens nothing of its own.
func (c *Collector) endOfHold(ctx context.Context, key types.NamespacedName, s consequenceHold, obs *Observation,
	failing bool) (holdVerdict, time.Duration, error) {
	keptFailing := s.failing >= minHoldSamples && (failing || s.restarted >= minHoldRestarts)
	// thisSample: obs is this sample's observation and shows the hold's cause, so s.obs is obs.
	hadCause, thisSample := s.cause != "", obs.CauseFingerprint != "" && obs.CauseFingerprint == s.cause
	outcome, err := c.endCause(ctx, &s)
	if err != nil {
		return holdEmit, 0, err
	}
	if thisSample {
		// The observation as endCause settled it, so that emit does not ask again.
		*obs = s.obs
	}
	if s.cause != "" && (outcome == holdReleased || keptFailing) {
		return c.endWithCause(ctx, key, s, obs, outcome, "the hold ended")
	}
	if !keptFailing {
		heldConsequences.WithLabelValues(holdNotOpened).Inc()
		logf.FromContext(ctx).V(1).Info("Dropped held consequence, its container did not keep failing",
			"reason", s.reason, "container", s.container, "cause", hadCause, "causeKept", s.cause != "",
			"failingSamples", s.failing, "restarts", s.restarted)
		return holdDrop, 0, nil
	}
	s.opened = true
	if _, ok := c.holds.put(key, s); !ok {
		// Not held before (a grace of zero): nothing to remember across a delivery retry.
		return holdEmit, 0, nil
	}
	heldConsequences.WithLabelValues(holdOpened).Inc()
	return holdEmit, 0, nil
}

// endWithCause ends a hold with a cause: obs becomes the observation of the last sample that showed the cause,
// and the hold is kept, decided, until its delivery ends, so that a delivery retry emits the same. The outcome is
// counted once, when the hold was stored.
func (c *Collector) endWithCause(ctx context.Context, key types.NamespacedName, s consequenceHold, obs *Observation,
	outcome, why string) (holdVerdict, time.Duration, error) {
	if outcome == holdReleased {
		s.obs.causeConfirmed = true
	}
	*obs = s.obs
	s.opened = true
	if _, ok := c.holds.put(key, s); ok {
		heldConsequences.WithLabelValues(outcome).Inc()
		logf.FromContext(ctx).V(1).Info("Emitted held consequence with its cause", "why", why,
			"reason", s.reason, "container", s.container, "outcome", outcome)
	}
	return holdEmit, 0, nil
}

// heldCause ends the hold with a cause of the event with this UID, when it has one, as the event leaves the
// collector by another path than the hold (its template retired, its owner chain no longer usable, its probe
// settled, no longer a Warning): the consequence is emitted with its cause all the same, so a hold with a cause
// ends in a drop only at the end of ConsequenceGrace with its container recovered (ADR-0039 point 7, decided
// 2026-11-17). It returns the observation to emit, and false when the event has no hold with a cause, or its cause
// was a non-zero exit that endCause removed with no OOM kill in its place: that hold ends as one with no cause. An
// error is only the reconcile's context ending.
func (c *Collector) heldCause(ctx context.Context, key types.NamespacedName, uid types.UID, why string) (Observation,
	bool, error) {
	s, ok := c.holds.get(key, uid)
	if !ok || s.cause == "" {
		return Observation{}, false, nil
	}
	if s.opened {
		return s.obs, true, nil
	}
	outcome, err := c.endCause(ctx, &s)
	if err != nil || s.cause == "" {
		return Observation{}, false, err
	}
	obs := s.obs
	_, _, _ = c.endWithCause(ctx, key, s, &obs, outcome, why)
	return obs, true, nil
}

// endCause settles the cause of hold s as it ends by another path than a sample's answer (the end of the hold,
// its pod gone or replaced, the event leaving the collector), and returns its outcome: "" when s has no cause
// (then or after), released when the Incident controller saw it, else cause_unseen. A non-zero exit not confirmed
// yet is asked about once more, here, through settleHold: confirmed it is released; otherwise it is removed
// (settleExitCause) and the hold goes on with the OOM kill in the window, if any, else with no cause, and a failed
// ask waits for nothing more (decided 2026-11-21 on #432). Any other cause is kept and only the metric's outcome
// depends on causeOutcome. An error is only the reconcile's context ending.
func (c *Collector) endCause(ctx context.Context, s *consequenceHold) (string, error) {
	if s.cause == "" {
		return "", nil
	}
	if s.obs.causeExit {
		seen, err := c.settleHold(ctx, s, true)
		switch {
		case err != nil:
			return "", err
		case seen:
			return holdReleased, nil
		case s.cause == "":
			return "", nil
		}
	}
	return c.causeOutcome(ctx, *s), nil
}

// askCause asks whether the Incident controller saw the cause of the stored hold s, at a sample before the end of
// ConsequenceGrace. A non-zero exit not confirmed yet is asked through settleHold: when the answer is no and the
// container shows an OOM kill in the window, the hold moves to the OOM kill, as main links it, is stored again,
// and the controller is asked for that. Errors are those of causeSeen.
func (c *Collector) askCause(ctx context.Context, key types.NamespacedName, s *consequenceHold) (bool, error) {
	if s.obs.causeExit {
		seen, err := c.settleHold(ctx, s, false)
		if err != nil || seen || s.obs.causeExit {
			return seen, err
		}
		if stored, ok := c.holds.put(key, *s); ok {
			*s = stored
		}
	}
	return c.causeSeen(ctx, s.obs)
}

// settleHold settles the cause of hold s with settleExitCause and keeps the hold's cause equal to its
// observation's.
func (c *Collector) settleHold(ctx context.Context, s *consequenceHold, final bool) (bool, error) {
	seen, err := c.settleExitCause(ctx, &s.obs, final)
	s.cause = s.obs.CauseFingerprint
	return seen, err
}

// settleExitCause is the one place that removes a non-zero-exit cause (decided 2026-11-21 on #432), and it does so
// only after it asked Config.CauseSeen for that fingerprint (causeSeen, with its deadline) in the same call. obs
// is a consequence whose cause may be a non-zero exit; any other obs, or one confirmed already, is left as it is
// and answered by its confirmation. It reports whether the exit is confirmed:
//
//   - CauseSeen answers yes: the exit stays, marked confirmed.
//   - The reconcile's context ended: nothing changes, and its error is returned.
//   - Not final (a sample during the hold): an error of CauseSeen is returned for the hold to wait on as before,
//     and an answer no changes nothing unless the container shows an OOM kill in the window (causeFallback): then
//     the exit is removed for it, as main links the consequence.
//   - Final (the end of the hold, its pod gone, the event leaving, an emit without a hold or a delivery retry):
//     the exit is removed and causeFallback, the OOM kill or "", takes its place. An error or an expired deadline
//     is logged at error level with the fingerprint and counted cause_unanswered; the consequence waits for
//     nothing more, as the hold has ended. An answer no, or no CauseSeen at all, is logged at info level and
//     counted cause_unconfirmed.
func (c *Collector) settleExitCause(ctx context.Context, obs *Observation, final bool) (bool, error) {
	if !obs.causeExit || obs.causeConfirmed {
		return obs.causeConfirmed, nil
	}
	seen, err := c.causeSeen(ctx, *obs)
	switch {
	case err == nil && seen:
		obs.causeConfirmed, obs.causeFallback = true, ""
		return true, nil
	case ctx.Err() != nil:
		return false, ctx.Err()
	case !final && (err != nil || obs.causeFallback == ""):
		return false, err
	}
	exit := obs.CauseFingerprint
	obs.CauseFingerprint, obs.causeFallback, obs.causeExit = obs.causeFallback, "", false
	log := logf.FromContext(ctx).WithValues("reason", obs.Reason, "container", obs.Container,
		"causeFingerprint", exit, "fallbackFingerprint", obs.CauseFingerprint)
	if err != nil {
		heldConsequences.WithLabelValues(holdCauseUnanswered).Inc()
		log.Error(err, "Went on without the non-zero exit cause of a consequence, the Incident controller did not answer")
		return false, nil
	}
	heldConsequences.WithLabelValues(holdCauseUnconfirmed).Inc()
	log.Info("Went on without the non-zero exit cause of a consequence, the Incident controller has not confirmed it")
	return false, nil
}

// causeOutcome is the outcome of a hold whose cause is an OOM kill or a waiting reason as it ends by another path
// than a sample's answer: released when ReleaseCause marked it for the cause it shows and Config.CauseSeen confirms
// it, else cause_unseen. Unmarked, it does not ask, so an end never waits on a controller that has not loaded its
// Incidents; a failed ask is cause_unseen. The consequence is emitted with its cause either way, and only the
// metric's outcome depends on it. A non-zero exit is endCause's.
func (c *Collector) causeOutcome(ctx context.Context, s consequenceHold) string {
	if s.cause == "" || s.releasedFor != s.cause {
		return holdCauseUnseen
	}
	if seen, err := c.causeSeen(ctx, s.obs); err == nil && seen {
		return holdReleased
	}
	return holdCauseUnseen
}

// failingNow reports whether the held container fails in st (ADR-0039 point 19, amended 2026-11-16): its restart
// count went up since the previous sample of the hold, or it is not ready (Unhealthy) or waiting in a back-off
// or in a reason of WaitingCauseReasons (BackOff; ErrImagePull is a failing pull, decided 2026-11-17, fifth, on
// #828). A container that restarted once and then runs fine is failing at that one sample only, and a
// liveness loop with no readiness probe, ready at each sample, is failing by its restarts.
func (s consequenceHold) failingNow(st corev1.ContainerStatus) bool {
	if st.RestartCount > s.restarts {
		return true
	}
	if s.reason == ReasonUnhealthy {
		return !st.Ready
	}
	w := st.State.Waiting
	return w != nil && (strings.HasSuffix(w.Reason, reasonBackOff) || causereason.IsWaiting(w.Reason))
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
