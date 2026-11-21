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
	"math"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
	"github.com/pkalin/shadewright/internal/modules"
)

// Defaults for the zero values of [Config].
const (
	DefaultBuffer        = 256
	DefaultMaxEvents     = 10000
	DefaultMaxPods       = 5000
	DefaultMaxObjects    = 5000
	DefaultMaxOwners     = 4096
	DefaultOwnerTTL      = 10 * time.Minute
	DefaultLookupTimeout = 5 * time.Second
	DefaultSendTimeout   = 10 * time.Second
	DefaultSelectionPoll = 30 * time.Second
)

// retryAfter is how long a reconcile waits before trying again when the consumer did not take an
// observation within the send timeout.
const retryAfter = 5 * time.Second

// MaxCount is the most occurrences one observation stands for. A series count is written by whoever can
// create events in the namespace, so one forged jump (to 2^31, say) adds at most this much. The cap is per
// observation: every further forged update can add as much again, and nothing in the collector bounds an
// Incident's total; the Incident controller (package incidents) saturates it at
// [remediationv1alpha1.MaxIncidentCount].
const MaxCount = 1000

// AllHandled is the answer of a [HandledFunc] for an occurrence whose record the Incident controller
// dropped to stay within its bounds: everything in it counts as counted, so a bound never counts twice.
const AllHandled int64 = math.MaxInt64

// Observation is one symptom ready for the Incident controller: the fingerprint and the normalized,
// redacted symptom are computed here, so no raw event or status text ever leaves the collector.
type Observation struct {
	// Fingerprint identifies the incident (package fingerprint).
	Fingerprint string
	// Subject is the object the symptom is correlated to: the owning workload when the owner chain could be
	// verified, otherwise the object itself.
	Subject remediationv1alpha1.ObjectRef
	// Source is the namespace the observation came from: the event's own namespace, the pod's for a
	// container termination read from pod status, or the watched object's for a module symptom source. It is
	// the namespace RBAC grants the creation of the event (or the write of the object) in, so it is not empty
	// for a cluster-scoped subject (a Node) named by an event. It is empty only for a cluster-scoped watched
	// object (a Kyverno ClusterPolicy or ClusterPolicyReport), which only a cluster-wide grant can write: its
	// observations share one empty-named share of the Incident controller's caps.
	Source string
	// Reason, Type and ReportingController are the event's; or OOMKilled (or the termination reason, or the
	// waiting reason), Warning and kubelet for a container termination or wait read from pod status; or the symptom's reason, Warning
	// and the source's reporting controller for a module symptom source.
	Reason              string
	Type                string
	ReportingController string
	// Symptom is the normalized, redacted note (at most 1024 bytes); for a module symptom source, the
	// module's fixed summary and its bounded evidence.
	Symptom string
	// Occurrence identifies what Total counts: "event:" and the Event UID, "termination:" and a hash of the
	// pod UID, the container name and the termination's ID, "waiting:" and a hash of the pod UID, the container
	// name, the waiting reason and the container's restart count, or "condition:" and a hash of the object UID, its
	// kind, the symptom's reason and when the state began, for a module symptom source. Several occurrences
	// (pods, events, objects) can share one fingerprint.
	Occurrence string
	// Total is how many times the occurrence happened as the cluster reports it: the event's series count
	// (or the count of an older recorder, or 1), or 1 for a container termination or a symptom state. It is
	// cumulative and not capped: a consumer computes an increase from it only with [Observation.Increase],
	// which applies MaxCount.
	Total int64
	// Began is when the occurrence began, as the cluster reports it: the event's creation time, the
	// termination's finish time or the symptom state's start (zero when the cluster did not report one),
	// never later than the collector's clock. For a waiting cause, whose status has no time, it is when this
	// collector first saw the state, by its own clock. It orders occurrences for a consumer that keeps a bounded number
	// of them.
	Began time.Time
	// Count is how many new occurrences this observation stands for: the increase of Total since this
	// collector last handled it, at most MaxCount, never the total of a series the collector cannot account
	// for. It is 0 exactly when MayRepeat is set.
	Count int64
	// ObservedAt is when the latest of these occurrences happened, as reported by the cluster, never later
	// than the collector's clock. For a waiting cause it is the collector's read: Began on its first report,
	// the time of the read on a repeat (a waiting cause still shown, reported again with Count 0).
	ObservedAt time.Time
	// MayRepeat is set when the collector cannot tell how much of Total is new: the occurrence is older than
	// this collector's first reconcile (a restart or a new leader), or the collector dropped state of
	// occurrences at least as old to stay within its bounds, and no HandledFunc settled it. The consumer must
	// then count Increase(what it already counted for this Occurrence).
	MayRepeat bool
	// FoldOnly is set on the observation of an event from before the collector's start about a pod that is
	// gone (#996, decided 2026-11-21): the start listed the event and it has not changed since, its last
	// occurrence is more than StaleOnStart before the start, and its pod was not found. Such an observation
	// opens no Incident. The consumer counts it only on an Incident that exists: the one that counted its
	// occurrence, or the one of its fingerprint; with neither it is dropped.
	// A restart thus opens no Incident that the running collector would not have opened.
	FoldOnly bool
	// Immediate is set when the symptom opens an Incident at its first sighting instead of waiting to be
	// seen again (design 3.2, #267). Only the collector sets it, from what it read itself: a container
	// termination in pod status whose reason is OOMKilled, or a symptom state of a module's symptom source
	// (a condition state, already settled and lasting, emitted once while it lasts, so it would never be
	// seen again). An event never sets it, whatever its reason, because whoever can create events in a
	// watched namespace writes the reason.
	Immediate bool
	// Origin is where the collector read the observation (the Source of ADR-0039 point 2; the field Source
	// above is the namespace): fingerprint.SourcePodStatus for a container termination or waiting reason read
	// from pod status, fingerprint.SourceEvent for an event, and empty for a module symptom source. Only a
	// pod-status origin is part of the fingerprint.
	Origin fingerprint.Source
	// Container is the container the observation is about: a status entry's name, or for an event the
	// container its regarding field path names in the verified pod (cause.go). Empty when none is named.
	Container string
	// Class is the observation's part in the consequence rule (ADR-0039): ClassCause only for a pod-status
	// observation, ClassConsequence only for a kubelet event on a named container, else ClassNone. The collector
	// holds a consequence with no cause, or whose cause has no Incident yet, before it emits it (hold.go).
	Class Class
	// CauseFingerprint is, on a consequence, the fingerprint of the cause its pod's status shows for its
	// container (Collector.causeFingerprint); empty when the status shows none. A non-zero exit stays here only
	// when Config.CauseSeen confirmed it (decided 2026-11-21 on #432, hold.go).
	CauseFingerprint string

	// causeExit is set, inside the collector only, when CauseFingerprint names a non-zero exit (not an OOM kill
	// and not a waiting reason). causeConfirmed is set when Config.CauseSeen answered true for CauseFingerprint.
	// causeFallback is, beside a non-zero exit, the fingerprint of the OOM kill the same container shows in the
	// window (Collector.causeFingerprint), "" when none: what takes the exit's place when it is not confirmed.
	// Only settleExitCause removes a non-zero exit, after asking Config.CauseSeen for it (hold.go); the event
	// reconciler delivers it as the cause only with both flags set (eventReconciler.emit), and clears the flags
	// before the observation leaves the collector.
	causeExit, causeConfirmed bool
	causeFallback             string
}

// immediateTerminationReasons are the container termination reasons, read from pod status, that open an
// Incident at their first sighting: OOMKilled, because a container killed for its memory is a fault on its
// own, not noise of a cluster that is starting. The set is kept small on purpose; every other symptom opens
// an Incident only when it is seen again (package incidents), except the states of module symptom sources,
// which are all immediate (sources.go).
var immediateTerminationReasons = []string{ReasonOOMKilled}

// immediateTermination reports whether a termination reason read from pod status is in
// immediateTerminationReasons.
func immediateTermination(reason string) bool {
	return slices.Contains(immediateTerminationReasons, reason)
}

// copyCause sets the cause of o to the cause of from: its fingerprint, the OOM kill behind a non-zero exit, and
// what Config.CauseSeen answered for it.
func (o *Observation) copyCause(from Observation) {
	o.CauseFingerprint, o.causeFallback = from.CauseFingerprint, from.causeFallback
	o.causeExit, o.causeConfirmed = from.causeExit, from.causeConfirmed
}

// Increase returns how many of the occurrence's Total are new when handled of them were counted before:
// the difference, never below 0 and at most MaxCount. A consumer uses it, never Total itself, for an
// observation marked MayRepeat.
func (o Observation) Increase(handled int64) int64 {
	return min(max(o.Total-handled, 0), MaxCount)
}

// HandledFunc returns how much of the Total of o's occurrence has already been counted, by any Incident: the
// lookup is by o.Occurrence and o.Began, not by o.Fingerprint, because the fingerprint depends on owner
// correlation, which can differ after a restart, while the occurrence does not. Of a consequence it may also
// read o.Class and o.CauseFingerprint, to answer from the drop mark of the evidence its cause's Incident took
// (ADR-0039 point 16) and from no other Incident's. It returns the highest Total recorded for the
// occurrence, and found false only when the occurrence was never counted. The Incident controller (package
// incidents) supplies it. To answer it keeps, per occurrence, the highest Total it counted and when the
// occurrence began, for at most [remediationv1alpha1.MaxOccurrenceRecords] occurrences per Incident, newest
// first. For an occurrence it has no record of
// that began no later than the newest one it dropped, it answers found with [AllHandled], so a bound never
// counts twice. The collector asks only for observations it would otherwise mark MayRepeat, with the
// deadline of an owner lookup, so the dedup state of an occurrence that started before a restart lives in
// the Incident, not in memory alone.
type HandledFunc func(ctx context.Context, o Observation) (handled int64, found bool, err error)

// CauseSeenFunc reports whether the Incident controller has seen the cause a consequence names (ADR-0039 point
// 7, decided 2026-11-17 three times): it has an Incident of o.CauseFingerprint that counts a cause and that it
// would count o on. A cause it holds only as a first sighting is not seen. It answers from memory, in the same
// process, and an error (the Incidents not loaded within the deadline) is no answer: the collector asks again.
// The Incident controller (package incidents) supplies it, and tells the collector of each cause it sees later
// through [Collector.ReleaseCause].
type CauseSeenFunc func(ctx context.Context, o Observation) (bool, error)

// Config configures a [Collector]. Zero values take the defaults above.
type Config struct {
	// ClusterID is hashed into every fingerprint. Required.
	ClusterID string
	// Buffer is the capacity of the observation channel.
	Buffer int
	// MaxEvents bounds the number of event series whose last handled count is remembered.
	MaxEvents int
	// MaxPods bounds the number of pods whose handled container terminations are remembered, and the number
	// of pods whose owner reference is remembered for events that arrive after the pod is gone.
	MaxPods int
	// MaxOwners bounds the owner correlation cache.
	MaxOwners int
	// OwnerTTL is how long a verified owner is reused before it is read again. An owner that could not be
	// verified is read again after 5 seconds.
	OwnerTTL time.Duration
	// LookupTimeout is the deadline of each read of an owner from the API server, and of each call of
	// Handled.
	LookupTimeout time.Duration
	// SendTimeout is how long a reconcile waits for room in the channel before it tries again, 5 seconds
	// later.
	SendTimeout time.Duration
	// Handled, when set, settles observations that would be marked MayRepeat: their Count becomes what the
	// Incident has not counted yet, and they are dropped when that is nothing.
	Handled HandledFunc
	// CauseSeen, when set, is asked whether the Incident controller has seen the cause a consequence names. A
	// consequence whose cause it has not seen is held (hold.go) until [Collector.ReleaseCause] is called for that
	// cause, at the latest for ConsequenceGrace, and then emitted with its cause (ADR-0039 point 7). Nil takes
	// every cause as seen and emits such a consequence at once, except a non-zero exit: with no controller to
	// confirm it, it is never confirmed, and its consequence goes on with the OOM kill the container shows in the
	// window, or is held and judged as one with no cause (decided 2026-11-21 on #432).
	CauseSeen CauseSeenFunc
	// Modules, when set, returns the modules active now (detection.Detector.Selection). The collector then
	// also watches the kinds in [SourceKinds] and evaluates on each object the symptom source an active module
	// declares for its kind (sources.go). Nil watches none of them.
	Modules func() modules.Selection
	// MaxObjects bounds the number of objects whose handled symptom states are remembered.
	MaxObjects int
	// MaxHeldConsequences bounds the consequences held at once (hold.go), a quarter of it per namespace, for
	// each of the two shares apart: those first seen with no cause, and those first seen with a cause the
	// Incident controller had not seen. Over it a consequence is emitted at once, without a verdict.
	MaxHeldConsequences int
	// SelectionPoll is how often the collector checks whether a module became active for a kind in
	// [SourceKinds], to evaluate then the objects of that kind it filtered out while none was, and, for a kind
	// that is not built in, whether it is served and may be listed (lazy.go).
	SelectionPoll time.Duration
	// Namespaces are the watched namespaces (--watch-namespaces), where the collector checks that it may list
	// a namespaced kind of [SourceKinds] that is not built in before it watches it. Required with Modules.
	Namespaces []string
	// Grants are the module grants the chart rendered (--module-grants, [ModuleGrants]). A kind of
	// [SourceKinds] that is not built in, whose grant is not listed, is not checked or watched: it is reported
	// as NotGranted, or, for an optional grant (kyverno-cluster), as Omitted, which is not reported as missing.
	// Nil means not known (an install without the flag): every such kind is checked.
	Grants []string
	// WatchChanged, when set, is called after the watch state of such a kind changed
	// ([Collector.SourcesNotWatched]), so detection reports it without waiting for its next run. It must not
	// block.
	WatchChanged func()
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Collector turns events and pod status in the watched namespaces into [Observation] values.
type Collector struct {
	cfg  Config
	out  chan Observation
	now  func() time.Time
	once sync.Once
	// start is the time of the first reconcile; anything older may have been counted by an earlier process.
	start time.Time

	series *seriesTracker
	// listed remembers the events of the start's list until they change (listed.go).
	listed *listedEvents
	pods   *terminationTracker
	owners *ownerResolver
	// conditions remembers the handled symptom states of objects read by module symptom sources, keyed by
	// kind and name.
	conditions *stateTracker
	// probes remembers the probe failures whose decision is put off while their pod starts (probe.go).
	probes *probeDeferrals
	// holds remembers the consequences held for at most ConsequenceGrace (hold.go).
	holds *consequenceHolds
	// holdOff, set by tests of the observation's fields only, emits a consequence without holding it.
	holdOff bool
	// releaseMu guards releaseQueue, the work queue of the event controller, which ReleaseCause adds the released
	// holds' events to. It is set when the controller starts (SetupWithManager), and nil before and in tests.
	releaseMu    sync.Mutex
	releaseQueue workqueue.TypedRateLimitingInterface[reconcile.Request]

	// watches is the watch state of each kind of SourceKinds that is not built in (lazy.go).
	watchMu sync.Mutex
	watches map[schema.GroupVersionKind]WatchState
	// lazyConfig, when set (tests only), is the config of the lazy watches' caches instead of the manager's.
	lazyConfig *rest.Config
}

// New validates cfg, applies defaults and returns a collector. Call [Collector.SetupWithManager] to start
// watching, and read [Collector.Observations].
func New(cfg Config) (*Collector, error) {
	if cfg.ClusterID == "" {
		return nil, errors.New("collector: a cluster ID is required")
	}
	setDefault(&cfg.Buffer, DefaultBuffer)
	setDefault(&cfg.MaxEvents, DefaultMaxEvents)
	setDefault(&cfg.MaxPods, DefaultMaxPods)
	setDefault(&cfg.MaxObjects, DefaultMaxObjects)
	setDefault(&cfg.MaxHeldConsequences, DefaultMaxHeldConsequences)
	setDefault(&cfg.MaxOwners, DefaultMaxOwners)
	setDefault(&cfg.OwnerTTL, DefaultOwnerTTL)
	setDefault(&cfg.LookupTimeout, DefaultLookupTimeout)
	setDefault(&cfg.SendTimeout, DefaultSendTimeout)
	setDefault(&cfg.SelectionPoll, DefaultSelectionPoll)
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	for _, g := range cfg.Grants {
		if !slices.Contains(ModuleGrants, g) {
			return nil, fmt.Errorf("collector: unknown module grant %q, want one of %v", g, ModuleGrants)
		}
	}
	return &Collector{
		cfg:    cfg,
		out:    make(chan Observation, cfg.Buffer),
		now:    cfg.Now,
		series: newSeriesTracker(cfg.MaxEvents),
		listed: newListedEvents(cfg.MaxEvents),
		pods:   newTerminationTracker(cfg.MaxPods),
		// A tracker of its own, keyed by kind and name, so a pod and a watched object of the same name, or two
		// watched kinds, keep apart.
		conditions: newStateTracker(cfg.MaxObjects),
		probes:     newProbeDeferrals(cfg.MaxEvents),
		holds:      newConsequenceHolds(cfg.MaxHeldConsequences),
		watches:    map[schema.GroupVersionKind]WatchState{},
	}, nil
}

func setDefault[T int | time.Duration](v *T, def T) {
	if *v <= 0 {
		*v = def
	}
}

// Observations returns the bounded channel the collector writes to. It is never closed.
func (c *Collector) Observations() <-chan Observation {
	return c.out
}

// SetupWithManager registers the two watches: events.k8s.io/v1 Events of type Warning and Pods with a
// terminated container or a waiting cause (and the update that clears one), and, when [Config.Modules] is set, one watch per kind in [SourceKinds]. The event
// controller also maps any update of a pod to the consequences held on it (hold.go), through the same pod informer. All read
// through the manager's cache, which the analyzer restricts to the watched namespaces (analyzer.CacheOptions). Owners are read with the API reader, one object at a time with a
// deadline, so no informer is started for ReplicaSets, Deployments or StatefulSets. There is deliberately no
// watch on core/v1 Events: they are the same objects, and watching both would count each event twice.
func (c *Collector) SetupWithManager(mgr ctrl.Manager) error {
	c.owners = newOwnerResolver(mgr.GetClient(), mgr.GetAPIReader(), c.cfg)
	er := &eventReconciler{c: c, reader: mgr.GetClient()}
	err := ctrl.NewControllerManagedBy(mgr).
		Named("collector-events").
		For(&eventsv1.Event{}, builder.WithPredicates(c.eventPredicate())).
		// An update of the pod of a held consequence is a sample of its hold (hold.go).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(c.heldEventsOf)).
		// The events of the holds that ReleaseCause releases (hold.go).
		WatchesRawSource(c.releases()).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(er)
	if err != nil {
		return fmt.Errorf("setting up the event collector: %w", err)
	}
	pr := &podReconciler{c: c, reader: mgr.GetClient()}
	err = ctrl.NewControllerManagedBy(mgr).
		Named("collector-pods").
		For(&corev1.Pod{}, builder.WithPredicates(podPredicate())).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(pr)
	if err != nil {
		return fmt.Errorf("setting up the pod collector: %w", err)
	}
	return c.setupSources(mgr)
}

// podPredicate lets through a pod that shows a symptom, and an update whose old or new pod shows one: an
// update that clears a waiting cause or a termination reaches the reconcile, which drops the wait's slot, so a
// wait shown again later starts its own time instead of keeping the first sight of the earlier one.
func podPredicate() predicate.Funcs {
	p := predicate.NewPredicateFuncs(hasPodSymptom)
	p.UpdateFunc = func(e event.UpdateEvent) bool {
		return hasPodSymptom(e.ObjectOld) || hasPodSymptom(e.ObjectNew)
	}
	return p
}

// started returns the time of the first reconcile.
func (c *Collector) started() time.Time {
	c.once.Do(func() { c.start = c.now() })
	return c.start
}

// observedAt clamps a time reported by the cluster: never zero and never in the future, so a forged
// timestamp cannot hide later occurrences from the restart check.
func (c *Collector) observedAt(t time.Time) time.Time {
	if t.IsZero() {
		return c.now()
	}
	return c.notFuture(t)
}

// notFuture clamps a time reported by the cluster to the collector's clock, keeping a zero time zero.
func (c *Collector) notFuture(t time.Time) time.Time {
	if now := c.now(); t.After(now) {
		return now
	}
	return t
}

// capCount bounds the occurrences one observation stands for to [0, MaxCount].
func capCount(n int64) int64 {
	return min(max(n, 0), MaxCount)
}

// deliver settles an observation marked MayRepeat with the Handled func, when there is one, and hands the
// observation to the consumer. It returns true when the observation was handed over or there was nothing
// new in it, so the caller may record it as handled, and false when the caller should try again later: the
// channel stayed full or Handled did not answer within its deadline.
func (c *Collector) deliver(ctx context.Context, o Observation) (bool, error) {
	if o.MayRepeat && c.cfg.Handled != nil {
		hctx, cancel := context.WithTimeout(ctx, c.cfg.LookupTimeout)
		handled, found, err := c.cfg.Handled(hctx, o)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("reading what the incidents have counted: %w", err)
		}
		if !found {
			handled = 0
		}
		o.Count, o.MayRepeat = o.Increase(handled), false
		if o.Count == 0 {
			return true, nil
		}
	}
	sent := c.emit(ctx, o)
	if sent && o.FoldOnly {
		// Counted once handed over: not at the mark, a retry, a hold that ends without it, or a Handled answer
		// that leaves nothing to send.
		staleOnStartFolded.Inc()
	}
	return sent, nil
}

// emit hands o to the consumer. It returns false when the channel stayed full for the send timeout or the
// context ended; the caller then keeps its state unchanged and tries again later, so nothing is lost and
// nothing is counted twice within one process.
//
// The send blocks the reconcile for up to SendTimeout (10 seconds by default). Each watch runs one reconcile
// at a time, so while the consumer is slow every other event (or pod) waits too, up to that long per
// observation; the blocked one is retried 5 seconds after its send timed out.
func (c *Collector) emit(ctx context.Context, o Observation) bool {
	// Room in the channel always wins: below, a timer that fired before the select runs (a loaded node) would be
	// picked at random over a free slot, and the observation would wait a retry for nothing.
	select {
	case c.out <- o:
		return true
	default:
	}
	t := time.NewTimer(c.cfg.SendTimeout)
	defer t.Stop()
	select {
	case c.out <- o:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

func isWarning(o client.Object) bool {
	ev, ok := o.(*eventsv1.Event)
	return ok && ev.Type == corev1.EventTypeWarning
}
