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

// lastOccurrence is when an event last happened, by the first field that is set: the reporter's time
// (kubeletTime), else the creation time.
func lastOccurrence(ev *eventsv1.Event) time.Time {
	if t, ok := kubeletTime(ev); ok {
		return t
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
	key := req.NamespacedName
	var ev eventsv1.Event
	if err := r.reader.Get(ctx, key, &ev); err != nil {
		if apierrors.IsNotFound(err) {
			// The one end of a hold with a cause that emits nothing: the event no longer exists (dropped).
			r.c.series.forget(key)
			r.c.listed.forget(key)
			r.c.probes.done(key)
			r.c.holds.drop(key)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	n := occurrences(&ev)
	created := ev.CreationTimestamp.Time
	delta, mayRepeat := r.c.series.pending(key, ev.UID, created, start, n)
	// leave handles the event without an observation of its own; a consequence held with a cause is emitted with
	// it all the same (hold.go, heldCause).
	heldEmitted := false
	// shown is the cause fingerprint the event's pod shows at this reconcile, once fromEvent has read it.
	shown := ""
	leave := func(why string, commit bool) (ctrl.Result, error) {
		if o, ok := r.c.heldCause(ctx, key, ev.UID, why); ok && (delta > 0 || mayRepeat) {
			heldEmitted = true
			return r.emit(ctx, key, &ev, o, shown, delta, mayRepeat)
		}
		r.c.probes.done(key)
		r.c.holds.drop(key)
		if commit {
			r.c.series.commit(key, ev.UID, created, n)
		}
		return ctrl.Result{}, nil
	}
	if ev.Type != corev1.EventTypeWarning {
		return leave("no longer a Warning", false)
	}
	if delta == 0 && !mayRepeat {
		return leave("nothing new", false)
	}
	obs, pod, v, err := r.c.fromEvent(ctx, &ev)
	if err != nil {
		return ctrl.Result{}, err
	}
	shown = obs.CauseFingerprint
	switch v {
	case subjectRetired:
		// About a pod whose template its workload has retired (#724): handled, counted once.
		res, err := leave("its template was retired", true)
		if !heldEmitted {
			dropRetired(ctx, obs.Subject.Kind, "Event", 1)
		}
		return res, err
	case subjectUnusable:
		// Not a usable symptom; remembering it keeps updates of the same event cheap.
		logf.FromContext(ctx).V(1).Info("Ignored Event that cannot be correlated")
		return leave("it can no longer be correlated", true)
	}
	// An old event the start listed, unchanged since, about a pod that is gone opens no Incident (#996).
	obs.FoldOnly = r.c.goneOnStart(&ev, pod, start)
	if probe, ok := probeOf(&ev); ok {
		// A readiness or startup probe failure of a pod that starts or shuts down (#726), decided from the pod
		// as fromEvent read it.
		pv, wait := r.c.settleProbe(ctx, &ev, pod)
		switch pv {
		case probeWait:
			return ctrl.Result{RequeueAfter: wait}, nil
		case probeSettled, probeTerminating:
			skipSettled(ctx, probe, pv)
			return leave("its probe settled", true)
		}
	}
	if evictionEvent(&ev) {
		// The pod reconciler reads the eviction from the pod's status as the pod's cause. The kubelet's Evicted
		// event is skipped once that was delivered, and is otherwise the ordinary symptom it always was: its own
		// fingerprint, no cause, FoldOnly as above (decided 2026-11-21, eviction.go).
		switch verdict, wait := r.c.settleEviction(ctx, key, &ev, pod); verdict {
		case evictionWait:
			return ctrl.Result{RequeueAfter: wait}, nil
		case evictionSkip:
			logf.FromContext(ctx).V(1).Info("Skipped Evicted Event, its Pod's eviction was delivered from its status")
			return leave("its pod's eviction was delivered", true)
		}
	}
	// A consequence whose pod shows no cause, or a cause the Incident controller has not seen yet, is held before
	// it is emitted (ADR-0039 points 7 and 19, hold.go).
	hv, wait, err := r.c.holdConsequence(ctx, key, &ev, &obs, pod)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch hv {
	case holdWait:
		return ctrl.Result{RequeueAfter: wait}, nil
	case holdDrop:
		// Handled without an observation: only a larger series total later is a new hold.
		r.c.probes.done(key)
		r.c.holds.done(key)
		r.c.series.commit(key, ev.UID, created, n)
		return ctrl.Result{}, nil
	}
	return r.emit(ctx, key, &ev, obs, shown, delta, mayRepeat)
}

// emit delivers obs with the event's count, and once delivered marks the event's occurrences handled and ends
// its hold. A delivery that found the channel full is tried again after retryAfter.
//
// A hold's decision belongs to the cause it was given for (decided 2026-11-17, fourth, on #828): its first
// delivery records the series total (consequenceHolds.decided), and a retried one whose pod shows another cause
// (shown, the cause fromEvent read at this reconcile, empty when none or not read) delivers only those
// occurrences, with the cause of the decision. A hold that ended with no cause delivers them with the cause
// shown now: a delivery retry rereads the cause. Once that is delivered, the event is reconciled again at once:
// the later occurrences are a new consequence, held for the cause shown now.
func (r *eventReconciler) emit(ctx context.Context, key types.NamespacedName, ev *eventsv1.Event, obs Observation,
	shown string, delta int64, mayRepeat bool) (ctrl.Result, error) {
	n, created := occurrences(ev), ev.CreationTimestamp.Time
	split := false
	if s, ok := r.c.holds.decided(key, ev.UID, n); ok {
		if s.cause != "" {
			// A hold that ended with no cause keeps the cause read now, which the controller may have seen.
			obs.CauseFingerprint = s.cause
		}
		if shown != "" && shown != s.cause && n > s.total {
			delta = max(delta-(n-s.total), 0)
			n, split = s.total, true
		}
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
	r.c.probes.done(key)
	r.c.holds.done(key)
	r.c.series.commit(key, ev.UID, created, n)
	if split {
		// At most once: the hold is done, so the next pass has no decision to split.
		logf.FromContext(ctx).V(1).Info("Held the later occurrences of a released consequence for its new cause",
			"reason", ev.Reason, "released", n)
		return r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	}
	return ctrl.Result{}, nil
}

// fromEvent builds the observation of an event, without its count, and returns the pod the event is about as
// the owner chain read it (nil when it names no pod, or the pod is gone or replaced). It returns
// subjectUnusable for events that are not a usable symptom: fields outside the Incident schema's bounds, or a
// regarding object in another namespace than the event. For subjectRetired, the event is about a pod of a
// retired template and only the Subject of the observation, its workload, is set.
func (c *Collector) fromEvent(ctx context.Context, ev *eventsv1.Event) (Observation, *corev1.Pod, subjectVerdict,
	error) {
	if !validReason(ev.Reason) || !validAction(ev.Action) || !validController(ev.ReportingController) {
		return Observation{}, nil, subjectUnusable, nil
	}
	reg := ev.Regarding
	if reg.Namespace != "" && reg.Namespace != ev.Namespace {
		return Observation{}, nil, subjectUnusable, nil
	}
	subject, owner, v, pod, err := c.owners.subjectAndPod(ctx, ev.Namespace, reg)
	if err != nil || v == subjectUnusable {
		return Observation{}, nil, subjectUnusable, err
	}
	if v == subjectRetired {
		return Observation{Subject: subject}, pod, subjectRetired, nil
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
		Source:              fingerprint.SourceEvent,
	}
	obs := c.observation(fo, subject, ev.Namespace, lastOccurrence(ev))
	var init bool
	obs.Container, init = namedContainer(reg, pod)
	obs.Class = eventClass(ev, obs.Container)
	if obs.Class == ClassConsequence {
		at, ok := kubeletTime(ev)
		obs.CauseFingerprint = c.causeFingerprint(pod, owner, obs.Container, init, at, ok)
	}
	return obs, pod, subjectUsable, nil
}

// observation computes the fingerprint (from fingerprint.Normalize) and the shown symptom (fingerprint.Symptom,
// which keeps a milli quantity after a key) of fo, which came from namespace source. The raw note is not kept.
func (c *Collector) observation(fo fingerprint.Observation, subject remediationv1alpha1.ObjectRef, source string,
	at time.Time) Observation {
	return Observation{
		Origin:              fo.Source,
		Fingerprint:         fingerprint.Fingerprint(fo),
		Subject:             subject,
		Source:              source,
		Reason:              fo.Reason,
		Type:                fo.Type,
		ReportingController: fo.ReportingController,
		Symptom:             fingerprint.Symptom(fo),
		ObservedAt:          c.observedAt(at),
	}
}
