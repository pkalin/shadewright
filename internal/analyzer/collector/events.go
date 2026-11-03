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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
)

// seriesState is what the collector remembers about one event: enough to count only the increase.
type seriesState struct {
	uid     types.UID
	handled int64
	created time.Time
}

// seriesTracker remembers the last handled occurrence count per event, for at most a fixed number of
// events. Events are keyed by namespace and name, and the UID tells a recreated event from the old one.
type seriesTracker struct {
	mu   sync.Mutex
	seen *lru[types.NamespacedName, seriesState]
	// evictedHigh is the newest creation time of any event whose state was dropped to stay within the
	// bound. An event first seen with a creation time after it cannot have been dropped, so it is new.
	evictedHigh time.Time
}

func newSeriesTracker(maxEvents int) *seriesTracker {
	t := &seriesTracker{}
	t.seen = newLRU(maxEvents, func(_ types.NamespacedName, s seriesState) {
		if s.created.After(t.evictedHigh) {
			t.evictedHigh = s.created
		}
	})
	return t
}

// pending returns how many of an event's occurrences are known not to be handled yet, uncapped, and
// whether the collector cannot tell (see Observation.MayRepeat), in which case the count is 0. A count
// that goes down is never negative.
func (t *seriesTracker) pending(key types.NamespacedName, uid types.UID, created, start time.Time,
	occurrences int64) (int64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.seen.get(key); ok && s.uid == uid {
		return max(occurrences-s.handled, 0), false
	}
	if created.Before(start) || (!t.evictedHigh.IsZero() && !created.After(t.evictedHigh)) {
		return 0, true
	}
	return occurrences, false
}

// commit records that occurrences of the event have been handled.
func (t *seriesTracker) commit(key types.NamespacedName, uid types.UID, created time.Time, occurrences int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.seen.get(key); ok && s.uid == uid && s.handled >= occurrences {
		return
	}
	t.seen.put(key, seriesState{uid: uid, handled: occurrences, created: created})
}

// forget drops the state of a deleted event.
func (t *seriesTracker) forget(key types.NamespacedName) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seen.delete(key)
}

func (t *seriesTracker) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seen.len()
}

// occurrences is how many times an event happened: its series count, or the count an older (core/v1)
// recorder kept, or one. Values below one count as one.
func occurrences(ev *eventsv1.Event) int64 {
	n := int64(1)
	switch {
	case ev.Series != nil:
		n = int64(ev.Series.Count)
	case ev.DeprecatedCount > 0:
		n = int64(ev.DeprecatedCount)
	}
	return max(n, 1)
}

// lastOccurrence is when an event last happened, by the first field that is set.
func lastOccurrence(ev *eventsv1.Event) time.Time {
	switch {
	case ev.Series != nil && !ev.Series.LastObservedTime.IsZero():
		return ev.Series.LastObservedTime.Time
	case !ev.EventTime.IsZero():
		return ev.EventTime.Time
	case !ev.DeprecatedLastTimestamp.IsZero():
		return ev.DeprecatedLastTimestamp.Time
	}
	return ev.CreationTimestamp.Time
}

// eventReconciler turns events.k8s.io/v1 Warning events into observations.
type eventReconciler struct {
	c      *Collector
	reader client.Reader
}

// Reconcile handles the increase of one event's occurrence count since it was last handled.
func (r *eventReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	start := r.c.started()
	var ev eventsv1.Event
	if err := r.reader.Get(ctx, req.NamespacedName, &ev); err != nil {
		if apierrors.IsNotFound(err) {
			r.c.series.forget(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if ev.Type != corev1.EventTypeWarning {
		return ctrl.Result{}, nil
	}
	n := occurrences(&ev)
	created := ev.CreationTimestamp.Time
	delta, mayRepeat := r.c.series.pending(req.NamespacedName, ev.UID, created, start, n)
	if delta == 0 && !mayRepeat {
		return ctrl.Result{}, nil
	}
	obs, ok, err := r.c.fromEvent(ctx, &ev)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ok {
		// Not a usable symptom; remembering it keeps updates of the same event cheap.
		logf.FromContext(ctx).V(1).Info("Ignored Event that cannot be correlated")
		r.c.series.commit(req.NamespacedName, ev.UID, created, n)
		return ctrl.Result{}, nil
	}
	obs.Occurrence, obs.Began = "event:"+string(ev.UID), r.c.notFuture(created)
	obs.Total, obs.Count, obs.MayRepeat = n, capCount(delta), mayRepeat
	sent, err := r.c.deliver(ctx, obs)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !sent {
		return ctrl.Result{RequeueAfter: retryAfter}, nil
	}
	r.c.series.commit(req.NamespacedName, ev.UID, created, n)
	return ctrl.Result{}, nil
}

// fromEvent builds the observation of an event, without its count. It returns false for events that are
// not a usable symptom: fields outside the Incident schema's bounds, or a regarding object in another
// namespace than the event.
func (c *Collector) fromEvent(ctx context.Context, ev *eventsv1.Event) (Observation, bool, error) {
	if !validReason(ev.Reason) || !validAction(ev.Action) || !validController(ev.ReportingController) {
		return Observation{}, false, nil
	}
	reg := ev.Regarding
	if reg.Namespace != "" && reg.Namespace != ev.Namespace {
		return Observation{}, false, nil
	}
	subject, owner, ok, err := c.owners.subjectOf(ctx, ev.Namespace, reg)
	if err != nil || !ok {
		return Observation{}, false, err
	}
	fo := fingerprint.Observation{
		ClusterID:           c.cfg.ClusterID,
		Namespace:           subject.Namespace,
		OwnerUID:            owner,
		ResourceUID:         string(reg.UID),
		Reason:              ev.Reason,
		Action:              ev.Action,
		Type:                ev.Type,
		ReportingController: ev.ReportingController,
		Note:                ev.Note,
		RegardingKind:       reg.Kind,
		RegardingName:       reg.Name,
	}
	return c.observation(fo, subject, lastOccurrence(ev)), true, nil
}

// observation computes the fingerprint and the symptom of fo. The raw note is not kept.
func (c *Collector) observation(fo fingerprint.Observation, subject remediationv1alpha1.ObjectRef, at time.Time) Observation {
	return Observation{
		Fingerprint:         fingerprint.Fingerprint(fo),
		Subject:             subject,
		Reason:              fo.Reason,
		Type:                fo.Type,
		ReportingController: fo.ReportingController,
		Symptom:             fingerprint.Normalize(fo),
		ObservedAt:          c.observedAt(at),
	}
}
