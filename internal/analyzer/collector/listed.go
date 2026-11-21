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
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// StaleOnStart is how long before the collector's start an event listed at start must have last occurred to
// only fold (#996, ADR-0039 amendment of 2026-11-21). It is longer than a leader lease handover and a
// rollout of the analyzer, so an event that changed while no collector ran (its occurrences handled by none)
// is within it and counts as before.
const StaleOnStart = 5 * time.Minute

// staleOnStartFolded counts the event observations marked FoldOnly by goneOnStart.
var staleOnStartFolded = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "shadewright_analyzer_stale_on_start_folded_total",
	Help: "Events listed at start, unchanged since, about a gone pod and last seen more than 5 minutes before the start, sent as fold only.",
})

func init() {
	metrics.Registry.MustRegister(staleOnStartFolded)
}

// listedVersion is the version of an event as the collector's start listed it.
type listedVersion struct {
	uid             types.UID
	resourceVersion string
}

// listedEvents remembers the Warning events of the informer's initial list (the collector's start, or the
// start of its controller under a new leader) with the resource version they were listed at, for at most a
// fixed number of events (#996). An event that is still at that version has had no occurrence since the
// collector started: its last time is before the start, decided from what the API server stored, not from
// a comparison of the event's times with the collector's clock. An event dropped to stay within the bound
// is no longer known as listed, and is handled as any other (it may open an Incident).
type listedEvents struct {
	mu   sync.Mutex
	seen *lru[types.NamespacedName, listedVersion]
}

func newListedEvents(maxEvents int) *listedEvents {
	return &listedEvents{seen: newLRU[types.NamespacedName, listedVersion](maxEvents, nil)}
}

// note records ev as listed at start.
func (l *listedEvents) note(ev *eventsv1.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen.put(types.NamespacedName{Namespace: ev.Namespace, Name: ev.Name},
		listedVersion{uid: ev.UID, resourceVersion: ev.ResourceVersion})
}

// unchanged reports whether ev is the event the start listed, at the version it was listed at. An event that
// changed since (a new occurrence, or another event of the same name) is forgotten: it never becomes unchanged
// again.
func (l *listedEvents) unchanged(ev *eventsv1.Event) bool {
	key := types.NamespacedName{Namespace: ev.Namespace, Name: ev.Name}
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.seen.get(key)
	if !ok {
		return false
	}
	if v.uid != ev.UID || v.resourceVersion == "" || v.resourceVersion != ev.ResourceVersion {
		l.seen.delete(key)
		return false
	}
	return true
}

// forget drops what is remembered of a deleted event.
func (l *listedEvents) forget(key types.NamespacedName) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen.delete(key)
}

func (l *listedEvents) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen.len()
}

// eventPredicate lets through Warning events, and notes those of the informer's initial list (listedEvents).
func (c *Collector) eventPredicate() predicate.Funcs {
	p := predicate.NewPredicateFuncs(isWarning)
	p.CreateFunc = func(e event.CreateEvent) bool {
		if !isWarning(e.Object) {
			return false
		}
		if e.IsInInitialList {
			c.listed.note(e.Object.(*eventsv1.Event))
		}
		return true
	}
	return p
}

// goneOnStart reports whether ev, as fromEvent read it, is an old event from before the collector's start
// about a pod that no longer exists (#996): the start listed it, it has not changed since, its regarding
// object is a Pod that the one read of it found gone or replaced by another of the same name (pod is nil),
// and its last occurrence (lastListedOccurrence) is more than StaleOnStart before start. Such an event opens
// no Incident: its observation is marked FoldOnly, and staleOnStartFolded counts it. An event last seen
// within StaleOnStart, or with no usable time, counts as before: it may have changed while no collector ran
// (a restart or a leader handover), and folding it would drop occurrences no collector handled. An event
// about any other kind is not decided here.
func (c *Collector) goneOnStart(ev *eventsv1.Event, pod *corev1.Pod, start time.Time) bool {
	reg := ev.Regarding
	if pod != nil || reg.Kind != kindPod || reg.APIVersion != apiCore {
		return false
	}
	if !c.listed.unchanged(ev) {
		return false
	}
	if last, ok := lastListedOccurrence(ev); !ok || !last.Before(start.Add(-StaleOnStart)) {
		return false
	}
	staleOnStartFolded.Inc()
	return true
}

// lastListedOccurrence is the event's last occurrence as its reporter wrote it: the series' last observed
// time, else the deprecated last timestamp, else the event time; false when none is set. The creation
// timestamp does not count (an event with no other time counts as before).
func lastListedOccurrence(ev *eventsv1.Event) (time.Time, bool) {
	switch {
	case ev.Series != nil && !ev.Series.LastObservedTime.IsZero():
		return ev.Series.LastObservedTime.Time, true
	case !ev.DeprecatedLastTimestamp.IsZero():
		return ev.DeprecatedLastTimestamp.Time, true
	case !ev.EventTime.IsZero():
		return ev.EventTime.Time, true
	}
	return time.Time{}, false
}
