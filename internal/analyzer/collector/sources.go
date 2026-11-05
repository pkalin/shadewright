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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
	"github.com/pkalin/shadewright/internal/modules"
)

// SourceKinds are the kinds whose module symptom sources the collector evaluates: the kinds it watches when
// [Config.Modules] is set, and that the watch role grants (the markers in doc.go). A source of an active module
// for a kind not listed here is not evaluated. Each needs get, list and watch in the watched namespaces, and its
// subject owner kind, when it declares one, get there.
var SourceKinds = []schema.GroupVersionKind{
	{Group: "autoscaling", Version: "v2", Kind: "HorizontalPodAutoscaler"},
}

// occurrenceCondition is the prefix of the occurrence ID of a symptom read from an object's status.
const occurrenceCondition = "condition:"

// Bounds of the wait for an owner's claim ([modules.ClaimPending]): the first retry after claimRetryMin, each
// next one after twice as long, never longer than claimRetryMax. After claimMaxWaits reads (about 20 minutes)
// the wait ends: the owner that never claims the object is logged and the object itself is the subject, so a
// symptom is never hidden for good.
const (
	claimRetryMin = 2 * time.Second
	claimRetryMax = 2 * time.Minute
	claimMaxWaits = 15
)

// sourceReconciler evaluates the symptom source of one watched kind, when an active module declares one, on
// each object of that kind.
type sourceReconciler struct {
	c      *Collector
	reader client.Reader // the manager's cache, restricted to the watched namespaces
	api    client.Reader // the API reader, for subject owners
	gvk    schema.GroupVersionKind
}

// setupSources registers one watch per kind in SourceKinds. The objects are read from the manager's cache
// (mgr.GetCache(); mgr.GetClient() would read unstructured objects from the API server), from the informer the
// watch starts. The watch passes on only what an active source shows a symptom on, before or after the change
// (predicate); a second source enqueues the objects with a symptom when a module becomes active for the kind,
// since the predicate let none of them through before (activations).
func (c *Collector) setupSources(mgr ctrl.Manager) error {
	if c.cfg.Modules == nil {
		return nil
	}
	for _, gvk := range SourceKinds {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		r := &sourceReconciler{c: c, reader: mgr.GetCache(), api: mgr.GetAPIReader(), gvk: gvk}
		err := ctrl.NewControllerManagedBy(mgr).
			Named("collector-"+strings.ToLower(gvk.Kind)).
			For(u, builder.WithPredicates(r.predicate())).
			WatchesRawSource(r.activations(mgr.GetCache(), c.cfg.SelectionPoll)).
			WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
			Complete(r)
		if err != nil {
			return fmt.Errorf("setting up the %s collector: %w", gvk.Kind, err)
		}
	}
	return nil
}

// active returns the source an active module declares for the kind, with the module's name, if any.
func (r *sourceReconciler) active() (string, modules.SymptomSource, bool) {
	module, src, ok := r.c.cfg.Modules().SourceFor(r.gvk.Group, r.gvk.Kind)
	if !ok || !slices.Contains(src.Versions, r.gvk.Version) {
		return "", modules.SymptomSource{}, false
	}
	return module, src, true
}

// shows reports whether the active source, if any, finds a symptom on o.
func (r *sourceReconciler) shows(o client.Object) bool {
	u, ok := o.(*unstructured.Unstructured)
	if !ok {
		return false
	}
	_, src, ok := r.active()
	return ok && len(modules.EvaluateSource(src, u.Object)) > 0
}

// predicate lets a create or an update through only when the active source shows a symptom on the object,
// before or after the update (a symptom that ends must be seen, so that its return counts again), and every
// delete, so the handled states of a deleted object are forgotten. With no active source only deletes pass: an
// HPA status written on every sync costs no reconcile.
func (r *sourceReconciler) predicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return r.shows(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return r.shows(e.ObjectOld) || r.shows(e.ObjectNew) },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(e event.GenericEvent) bool { return r.shows(e.Object) },
	}
}

// activations is a source that checks every poll whether a module became active for the kind, and then
// enqueues the cached objects its source shows a symptom on: the predicate let none of them through while no
// module was active, and an HPA at its ceiling may not change again.
func (r *sourceReconciler) activations(lister client.Reader, poll time.Duration) source.Source {
	return source.Func(func(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
		go func() {
			t := time.NewTicker(poll)
			defer t.Stop()
			// False at the start: objects created before the first poll may have been filtered out, and an
			// extra reconcile of an object already handled emits nothing.
			wasActive := false
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
				wasActive = r.enqueueActivated(ctx, lister, q, wasActive)
			}
		}()
		return nil
	})
}

// enqueueActivated enqueues the objects with a symptom when the kind has an active source now and had none at
// the previous poll (wasActive). It returns whether the kind is active, or false when the list failed, so the
// next poll tries again.
func (r *sourceReconciler) enqueueActivated(ctx context.Context, lister client.Reader,
	q workqueue.TypedRateLimitingInterface[reconcile.Request], wasActive bool) bool {
	_, src, ok := r.active()
	if !ok || wasActive {
		return ok
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(r.gvk.GroupVersion().WithKind(r.gvk.Kind + "List"))
	if err := lister.List(ctx, list); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list the objects of a kind whose module became active",
			"kind", r.gvk.Kind)
		return false
	}
	for i := range list.Items {
		o := &list.Items[i]
		if len(modules.EvaluateSource(src, o.Object)) > 0 {
			q.Add(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}})
		}
	}
	return true
}

// conditionOccurrence identifies one state of one object across collectors: the object's UID, its kind, the
// symptom's reason, when the state began as the object reports it (Unix seconds, 0 when unknown; not clamped
// to the clock, so a start in the future keeps one ID while the clock passes it) and, for a state the
// collector saw end and come back with the same start, when it saw it come back.
func conditionOccurrence(uid types.UID, gvk schema.GroupVersionKind, reason string, since, episode time.Time) string {
	var began int64
	if !since.IsZero() {
		began = since.Unix()
	}
	key := string(uid) + "\x00" + gvk.Group + "/" + gvk.Kind + "\x00" + reason + "\x00" + strconv.FormatInt(began, 10)
	if !episode.IsZero() {
		key += "\x00" + strconv.FormatInt(episode.UnixNano(), 10)
	}
	h := sha256.Sum256([]byte(key))
	return occurrenceCondition + hex.EncodeToString(h[:16])
}

// Reconcile runs the active module's symptom source on the object and emits one observation per symptom state
// not handled before. With no active module watching the kind (the module is not enabled, not detected or not
// compiled in), it reads nothing, its owner kind included, and emits nothing.
func (r *sourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	start := r.c.started()
	module, src, ok := r.active()
	if !ok {
		// The handled states are kept: a module enabled again must not count a state it already counted.
		return ctrl.Result{}, nil
	}
	key := sourceKey{gvk: r.gvk, NamespacedName: req.NamespacedName}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(r.gvk)
	if err := r.reader.Get(ctx, req.NamespacedName, obj); err != nil {
		if apierrors.IsNotFound(err) {
			r.c.conditions.forget(key)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	// The cache keeps apiVersion and kind; set them anyway, a source checks them.
	obj.SetGroupVersionKind(r.gvk)
	symptoms := map[string]modules.Symptom{}
	current := map[string]time.Time{}
	for _, s := range modules.EvaluateSource(src, obj.Object) {
		if _, seen := symptoms[s.Reason]; seen {
			continue
		}
		symptoms[s.Reason], current[s.Reason] = s, s.Since
	}
	pending := r.c.conditions.observe(key, obj.GetUID(), current, r.c.now(), start)
	if len(pending) == 0 {
		return ctrl.Result{}, nil
	}
	subject, owner, wait, err := r.subject(ctx, obj, src.Subject)
	if err != nil {
		return ctrl.Result{}, err
	}
	if wait {
		// The owner exists but has not claimed the object yet: commit nothing, so the state is not settled
		// with the object itself as the subject, and look again later, up to claimMaxWaits times.
		if n := r.c.conditions.claimWait(key); n <= claimMaxWaits {
			return ctrl.Result{RequeueAfter: claimBackoff(n)}, nil
		}
		logf.FromContext(ctx).Info("Owner never claimed the object, used the object as the subject",
			"kind", r.gvk.Kind, "namespace", obj.GetNamespace(), "name", obj.GetName())
	}
	r.c.conditions.claimDone(key)
	for _, p := range pending {
		if !validRef(subject) || !validReason(p.reason) || !validController(src.ReportingController) {
			logf.FromContext(ctx).V(1).Info("Ignored symptom outside the Incident schema", "module", module)
			r.c.conditions.commit(key, obj.GetUID(), p)
			continue
		}
		s := symptoms[p.reason]
		fo := fingerprint.Observation{
			ClusterID:           r.c.cfg.ClusterID,
			Namespace:           obj.GetNamespace(),
			OwnerUID:            owner,
			ResourceUID:         string(obj.GetUID()),
			Reason:              s.Reason,
			Type:                corev1.EventTypeWarning,
			ReportingController: src.ReportingController,
			// The fixed summary only: evidence (replica counts, metric values) changes while the state lasts and
			// must not split the Incident.
			Note:          s.Summary,
			RegardingKind: r.gvk.Kind,
			RegardingName: obj.GetName(),
		}
		obs := r.c.observation(fo, subject, obj.GetNamespace(), p.began)
		obs.Symptom = symptomText(s)
		obs.Occurrence = conditionOccurrence(obj.GetUID(), r.gvk, p.reason, p.since, p.episode)
		obs.Began, obs.Total, obs.MayRepeat = p.began, 1, p.mayRepeat
		// A symptom state is already settled and lasting, with its own start: it opens its Incident at once
		// (#267), where a one-off event waits to be seen again.
		obs.Immediate = true
		if !p.mayRepeat {
			obs.Count = 1
		}
		sent, err := r.c.deliver(ctx, obs)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !sent {
			return ctrl.Result{RequeueAfter: retryAfter}, nil
		}
		r.c.conditions.commit(key, obj.GetUID(), p)
	}
	return ctrl.Result{}, nil
}

// claimBackoff is the wait before the n-th read again (from 1) of an owner whose claim is pending.
func claimBackoff(n int) time.Duration {
	d := claimRetryMin
	for i := 1; i < n && d < claimRetryMax; i++ {
		d *= 2
	}
	return min(d, claimRetryMax)
}

// subject returns the object the symptoms are about, and the UID hashed as its owner: the declared owner kind
// when the object's controller reference names one that exists in the object's namespace with the referenced
// UID and claims the object, else the object itself with no owner. wait is true when that owner exists with
// the referenced UID but its claim is pending: the caller retries instead of settling on the object itself.
// Only the declared kind is read, once, with the lookup deadline. The ownerResolver of events and pods is not
// used: it verifies apps/v1 owners by their label selectors and caches what it found, while this check is a
// claim in the owner's status, which must be read again while it is pending.
func (r *sourceReconciler) subject(ctx context.Context, obj *unstructured.Unstructured, want modules.SubjectOwner) (
	ref remediationv1alpha1.ObjectRef, owner string, wait bool, err error) {
	self := remediationv1alpha1.ObjectRef{
		APIVersion: r.gvk.GroupVersion().String(), Kind: r.gvk.Kind,
		Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: remediationv1alpha1.UID(obj.GetUID()),
	}
	if !want.IsSet() {
		return self, "", false, nil
	}
	cref := metav1.GetControllerOfNoCopy(obj)
	if cref == nil || cref.APIVersion != want.APIVersion() || cref.Kind != want.Kind || cref.UID == "" ||
		len(validation.IsDNS1123Subdomain(cref.Name)) > 0 {
		return self, "", false, nil
	}
	gvk := schema.GroupVersionKind{Group: want.Group, Version: want.Version, Kind: want.Kind}
	ownerObj := &unstructured.Unstructured{}
	ownerObj.SetGroupVersionKind(gvk)
	lctx, cancel := context.WithTimeout(ctx, r.c.cfg.LookupTimeout)
	err = r.api.Get(lctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: cref.Name}, ownerObj)
	cancel()
	if apierrors.IsNotFound(err) {
		return self, "", false, nil
	}
	if err != nil {
		return self, "", false, fmt.Errorf("reading the %s that controls %s %s: %w", want.Kind, r.gvk.Kind,
			obj.GetName(), err)
	}
	ownerObj.SetGroupVersionKind(gvk)
	if ownerObj.GetUID() != cref.UID {
		return self, "", false, nil
	}
	if want.Claims != nil {
		switch safeClaims(want.Claims, ownerObj.Object, obj.GetName()) {
		case modules.ClaimOwned:
		case modules.ClaimPending:
			return self, "", true, nil
		default:
			return self, "", false, nil
		}
	}
	return remediationv1alpha1.ObjectRef{
		APIVersion: want.APIVersion(), Kind: want.Kind,
		Namespace: obj.GetNamespace(), Name: ownerObj.GetName(), UID: remediationv1alpha1.UID(ownerObj.GetUID()),
	}, string(ownerObj.GetUID()), false, nil
}

// safeClaims calls a module's Claims and treats a panic as no claim.
func safeClaims(f func(map[string]any, string) modules.Claim, owner map[string]any, name string) (c modules.Claim) {
	defer func() {
		if recover() != nil {
			c = modules.ClaimNotOwned
		}
	}()
	return f(owner, name)
}

// sourceKey identifies a watched object across the kinds of SourceKinds.
type sourceKey struct {
	gvk schema.GroupVersionKind
	types.NamespacedName
}

// symptomState is what the tracker knows of one symptom (one reason) of one object.
type symptomState struct {
	since   time.Time // the start the source reported, not clamped
	episode time.Time // zero for a state first seen as it is; else when the tracker saw it come back
	ended   bool      // a reconcile saw the object without this symptom since
	handled bool
}

// objectStates is what the tracker knows of one object.
type objectStates struct {
	uid    types.UID
	states map[string]symptomState // by reason
	newest time.Time               // the newest start of a handled state
	// repeatThrough is fixed when the tracker first holds the object: the newest start dropped from the
	// tracker until then. A state that began no later may have been emitted before.
	repeatThrough time.Time
	claimWaits    int // reads of an owner whose claim was pending, since the last one that was not
}

// pendingState is a symptom state not handled yet.
type pendingState struct {
	reason         string
	since, episode time.Time // as in symptomState: they make the occurrence ID
	began          time.Time // when it began, never later than the clock; zero when unknown
	mayRepeat      bool
}

// stateTracker remembers the handled symptom states of objects read by module symptom sources, for at most a
// fixed number of objects, keyed by kind, namespace and name. Only objects that showed a symptom are held.
//
// A state is identified by the object, its reason and its reported start (Since). A state the tracker saw end
// (a reconcile of the object without it) and come back with the same start, as when an HPA's ScalingLimited
// stays True with another reason in between, or reports no lastTransitionTime, is a new state, identified by
// the time the tracker saw it come back. That time is not known after a restart: a state a new process sees
// first is identified by its start alone, so one that came back before the restart and still lasts is settled
// by the Incident controller against the first state's ID and is not counted again; a return the collector did
// not see (while it was down, or between two reconciles) is not counted either.
type stateTracker struct {
	mu   sync.Mutex
	seen *lru[sourceKey, objectStates]
	// evictedHigh is the newest start in the state of any object dropped to stay within the bound.
	evictedHigh time.Time
}

func newStateTracker(maxObjects int) *stateTracker {
	t := &stateTracker{}
	t.seen = newLRU(maxObjects, func(_ sourceKey, s objectStates) {
		if s.newest.After(t.evictedHigh) {
			t.evictedHigh = s.newest
		}
	})
	return t
}

// observe records the symptoms an object shows now (reason to reported start) and returns those not handled
// yet, each with whether it may have been emitted before (see Observation.MayRepeat): it began before start,
// the collector's first reconcile, has no known start, or began no later than the eviction bound recorded when
// the tracker first held the object. An object with no symptom and no record is not recorded.
func (tr *stateTracker) observe(key sourceKey, uid types.UID, current map[string]time.Time,
	now, start time.Time) []pendingState {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	s, ok := tr.seen.get(key)
	if !ok || s.uid != uid {
		if len(current) == 0 {
			if ok {
				tr.seen.delete(key)
			}
			return nil
		}
		s = objectStates{uid: uid, states: map[string]symptomState{}, repeatThrough: tr.evictedHigh}
	}
	for reason, st := range s.states {
		if _, shown := current[reason]; !shown {
			st.ended = true
			s.states[reason] = st
		}
	}
	var out []pendingState
	for _, reason := range slices.Sorted(maps.Keys(current)) {
		since := current[reason]
		st, had := s.states[reason]
		switch {
		case !had || !st.since.Equal(since):
			st = symptomState{since: since}
		case st.ended:
			st = symptomState{since: since, episode: now}
		}
		s.states[reason] = st
		if st.handled {
			continue
		}
		began := st.episode
		if began.IsZero() {
			began = since
			if began.After(now) {
				began = now
			}
		}
		mayRepeat := began.Before(start) || (!s.repeatThrough.IsZero() && !began.After(s.repeatThrough))
		out = append(out, pendingState{
			reason: reason, since: since, episode: st.episode, began: began, mayRepeat: mayRepeat,
		})
	}
	tr.seen.put(key, s)
	return out
}

// commit records that state p of the object has been handled.
func (tr *stateTracker) commit(key sourceKey, uid types.UID, p pendingState) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	s, ok := tr.seen.get(key)
	if !ok || s.uid != uid {
		// Dropped since observe ran (only another object's state can push it out): start again from the bound.
		s = objectStates{uid: uid, states: map[string]symptomState{}, repeatThrough: tr.evictedHigh}
	}
	s.states[p.reason] = symptomState{since: p.since, episode: p.episode, handled: true}
	if p.began.After(s.newest) {
		s.newest = p.began
	}
	tr.seen.put(key, s)
}

// claimWait counts one more read of the object's owner with a pending claim and returns the count.
func (tr *stateTracker) claimWait(key sourceKey) int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	s, ok := tr.seen.get(key)
	if !ok {
		return 1
	}
	s.claimWaits++
	tr.seen.put(key, s)
	return s.claimWaits
}

// claimDone resets the count of claimWait.
func (tr *stateTracker) claimDone(key sourceKey) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if s, ok := tr.seen.get(key); ok && s.claimWaits != 0 {
		s.claimWaits = 0
		tr.seen.put(key, s)
	}
}

func (tr *stateTracker) forget(key sourceKey) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.seen.delete(key)
}

func (tr *stateTracker) len() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.seen.len()
}

// symptomText is what an observation of a symptom carries as its symptom: the module's fixed summary, then
// its evidence, already redacted and bounded by the module evaluator, as "field=value" pairs. The whole is
// normalized and redacted again, and cut to the Incident schema's 1024 bytes, by fingerprint.Normalize.
func symptomText(s modules.Symptom) string {
	var b strings.Builder
	b.WriteString(s.Summary)
	if len(s.Evidence) > 0 {
		b.WriteString(" Evidence:")
		for i, e := range s.Evidence {
			if i > 0 {
				b.WriteString(";")
			}
			b.WriteString(" ")
			b.WriteString(e.Field)
			b.WriteString("=")
			b.WriteString(e.Value)
		}
	}
	return fingerprint.Normalize(fingerprint.Observation{Note: b.String()})
}
