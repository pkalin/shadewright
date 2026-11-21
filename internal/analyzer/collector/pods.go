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
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// ReasonOOMKilled is the termination reason of a container the kernel killed for using too much memory.
	ReasonOOMKilled = "OOMKilled"
	// reasonTerminated replaces a termination reason that is not a plain CamelCase word.
	reasonTerminated = "Terminated"
	// reportingKubelet is the reporting controller of observations read from pod status.
	reportingKubelet = "kubelet"
	// idsPerContainer is how many terminations are remembered per container: status shows at most two, the
	// current state and the last one.
	idsPerContainer = 2
)

// termination is one container termination worth an observation, one waiting reason of
// WaitingCauseReasons (waiting set), which has no finish time, or the eviction of the pod (evicted set,
// eviction.go), which names no container and carries the kubelet's message.
type termination struct {
	container string
	id        string
	reason    string
	exitCode  int32
	finished  time.Time
	waiting   bool
	evicted   bool
	message   string
	// observed is set when finished is the time the collector read the pod, not a time the kubelet wrote (an
	// eviction with no finish time and no DisruptionTarget condition).
	observed bool
}

// terminations returns the abnormal terminations in a pod's status: OOMKilled or a non-zero exit code, in
// init and app containers, from the current and the last state. A termination is identified by its
// container ID, so the same termination seen again is the same one; without one, by its start and finish
// times, exit code and reason, which stay the same when it moves from the current to the last state (the
// restart count does not). Two terminations of one container without container IDs that started and
// finished in the same seconds with the same exit code and reason are taken for one, so they count once;
// the API reports those times in whole seconds, and nothing else tells them apart. The termination message
// is never read: it is free text written by the container.
//
// A pod being deleted (deletionTimestamp set) shows only its OOM kills: its containers are stopped by the
// deletion, with exit code 137 after the grace period or 143 for a runtime that exits on SIGTERM, and a
// rollout, a scale down or a node drain is no symptom (#966). Deciding from the pod's own state compares no
// clocks; a crash of that pod not handled before its deletion is not observed either. A deletionTimestamp is
// never removed, so a termination left out here is never taken up later for the same pod.
func terminations(pod *corev1.Pod) []termination {
	deleting := pod.DeletionTimestamp != nil
	var out []termination
	for _, st := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
		for _, t := range []*corev1.ContainerStateTerminated{st.LastTerminationState.Terminated, st.State.Terminated} {
			if t == nil || (t.Reason != ReasonOOMKilled && (t.ExitCode == 0 || deleting)) {
				continue
			}
			reason := t.Reason
			if !validReason(reason) {
				reason = reasonTerminated
			}
			id := t.ContainerID
			if id == "" {
				id = fmt.Sprintf("started-%d-finished-%d-exit-%d-%s", t.StartedAt.Unix(), t.FinishedAt.Unix(),
					t.ExitCode, reason)
			}
			out = append(out, termination{
				container: st.Name, id: id, reason: reason, exitCode: t.ExitCode, finished: t.FinishedAt.Time,
			})
		}
	}
	return out
}

// waitings returns the waiting reasons of WaitingCauseReasons in a pod's status, in init and app containers
// (ADR-0039 point 4). A waiting state is identified by its reason and the container's restart count: it has no
// ID or start time, so a container that waits again with the same reason after a restart is a new one, and
// one that keeps waiting is the same one. Its finish time stays zero here; the tracker sets it to the time the
// collector first saw the state (terminationTracker.pending). The waiting message is never read: it is free
// text.
func waitings(pod *corev1.Pod) []termination {
	var out []termination
	for _, st := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
		w := st.State.Waiting
		if w == nil || !slices.Contains(WaitingCauseReasons, w.Reason) {
			continue
		}
		out = append(out, termination{
			container: st.Name, id: fmt.Sprintf("waiting-%s-restarts-%d", w.Reason, st.RestartCount),
			reason: w.Reason, waiting: true,
		})
	}
	return out
}

// WaitingOccurrencePrefix starts the Occurrence of a waiting cause read from pod status. Such an occurrence
// is reported again, with nothing new to count, while the state is still shown (ADR-0039 point 4, amended
// 2026-11-16); the consumer tells it apart by this prefix.
const WaitingOccurrencePrefix = "waiting:"

// podSymptoms returns the terminations and the waiting causes of a pod's status, or only its eviction when the
// status shows one (eviction.go).
func podSymptoms(pod *corev1.Pod) []termination {
	if e, ok := eviction(pod); ok {
		return []termination{e}
	}
	return append(terminations(pod), waitings(pod)...)
}

// occurrence identifies a termination across collectors without carrying the container ID itself.
func (t termination) occurrence(pod types.UID) string {
	prefix := "termination:"
	if t.waiting {
		prefix = WaitingOccurrencePrefix
	}
	h := sha256.Sum256([]byte(string(pod) + "\x00" + t.container + "\x00" + t.id))
	return prefix + hex.EncodeToString(h[:16])
}

// slot is the key of the IDs handled for t's container: waiting states have their own, so they never push a
// handled termination out of the idsPerContainer kept. A container name is a DNS label and has no colon.
func (t termination) slot() string {
	if t.waiting {
		return "waiting:" + t.container
	}
	return t.container
}

// class is the termination's part in the consequence rule: every symptom read from pod status is a cause (an
// OOM kill, a non-zero exit, a waiting reason, an eviction; ADR-0039 point 3, amended 2026-11-20).
func (t termination) class() Class {
	return ClassCause
}

// note is the symptom text of the termination's observation.
func (t termination) note() string {
	switch {
	case t.evicted:
		return evictionNote(t.message)
	case t.waiting:
		return waitingNote(t.container, t.reason)
	}
	return terminationNote(t.container, t.exitCode)
}

func hasPodSymptom(o client.Object) bool {
	pod, ok := o.(*corev1.Pod)
	return ok && len(podSymptoms(pod)) > 0
}

type podState struct {
	uid     types.UID
	handled map[string][]string // termination.slot to the IDs of its newest handled terminations
	// waits holds, per waiting slot (one per container), the waiting state the pod showed at its latest
	// reconcile: at most one entry per container of the pod.
	waits  map[string]waitState
	newest time.Time
	// repeatThrough is fixed when the collector first sees the pod: the newest finish time dropped from the
	// state until then. Terminations of the pod that finished no later may have been emitted before.
	repeatThrough time.Time
}

// waitState is a waiting state the collector sees in a slot: its ID, when the collector first saw it (the pod
// status has no time for a wait), and when it last reported it (zero until its first report is handled).
type waitState struct {
	id       string
	began    time.Time
	reported time.Time
}

// pendingTermination is a termination not handled yet, or a waiting state handled before and due to be
// reported again (repeat).
type pendingTermination struct {
	termination
	mayRepeat bool
	repeat    bool
}

func newPodState(uid types.UID, repeatThrough time.Time) podState {
	return podState{uid: uid, handled: map[string][]string{}, waits: map[string]waitState{},
		repeatThrough: repeatThrough}
}

// terminationTracker remembers which container terminations were handled, for at most a fixed number of
// pods.
type terminationTracker struct {
	mu   sync.Mutex
	seen *lru[types.NamespacedName, podState]
	// evictedHigh is the newest finish time in the state of any pod dropped to stay within the bound.
	evictedHigh time.Time
}

func newTerminationTracker(maxPods int) *terminationTracker {
	t := &terminationTracker{}
	t.seen = newLRU(maxPods, func(_ types.NamespacedName, s podState) {
		if s.newest.After(t.evictedHigh) {
			t.evictedHigh = s.newest
		}
	})
	return t
}

// pending returns the terminations of the pod not yet handled, each with whether it may have been emitted
// before (see Observation.MayRepeat): it finished before start, the collector's first reconcile, which is
// fixed for the process, or no later than the eviction bound recorded when the collector first saw the pod.
// So the answer is the same for every termination of the pod and does not change when some of them are
// committed and the rest wait for a retry. A termination without a finish time may always repeat: nothing
// tells when it happened.
//
// A waiting state gets as its finish time the time the collector first saw it in its slot, read at now; the
// slot's time is dropped when the pod no longer shows the state or shows another ID there. A waiting state
// may always repeat on its first report, since an earlier process may have reported it. One handled before is
// returned again, marked repeat, once at least SettleAfter has passed since its latest report (ADR-0039 point
// 4, amended 2026-11-16); a state handled before whose slot is new (it came back) counts as reported at now.
func (tr *terminationTracker) pending(key types.NamespacedName, uid types.UID, ts []termination,
	start, now time.Time) []pendingTermination {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	s, ok := tr.seen.get(key)
	if !ok || s.uid != uid {
		s = newPodState(uid, tr.evictedHigh)
	}
	shown := map[string]string{}
	for _, t := range ts {
		if t.waiting {
			shown[t.slot()] = t.id
		}
	}
	for slot, w := range s.waits {
		if shown[slot] != w.id {
			delete(s.waits, slot)
		}
	}
	var out []pendingTermination
	for _, t := range ts {
		handled := slices.Contains(s.handled[t.slot()], t.id)
		if !t.waiting {
			if handled {
				continue
			}
			mayRepeat := t.finished.Before(start) ||
				(!s.repeatThrough.IsZero() && !t.finished.After(s.repeatThrough))
			out = append(out, pendingTermination{termination: t, mayRepeat: mayRepeat})
			continue
		}
		w, ok := s.waits[t.slot()]
		if !ok {
			w = waitState{id: t.id, began: now}
			if handled {
				w.reported = now
			}
			s.waits[t.slot()] = w
		}
		t.finished = w.began
		switch {
		case !handled:
			out = append(out, pendingTermination{termination: t, mayRepeat: true})
		case now.Sub(w.reported) >= SettleAfter:
			out = append(out, pendingTermination{termination: t, repeat: true})
		}
	}
	tr.seen.put(key, s)
	return out
}

// nextRepeat returns how long until a waiting state of the pod handled before is due to be reported again,
// and false when the pod shows none.
func (tr *terminationTracker) nextRepeat(key types.NamespacedName, uid types.UID, now time.Time) (time.Duration,
	bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	s, ok := tr.seen.get(key)
	if !ok || s.uid != uid {
		return 0, false
	}
	var next time.Duration
	found := false
	for _, w := range s.waits {
		if w.reported.IsZero() {
			continue
		}
		d := max(w.reported.Add(SettleAfter).Sub(now), time.Second)
		if !found || d < next {
			next, found = d, true
		}
	}
	return next, found
}

// commit records that termination t of the pod has been handled, at now. For a waiting state it also records
// now as the time of its latest report, so its next repeat comes SettleAfter later.
func (tr *terminationTracker) commit(key types.NamespacedName, uid types.UID, t termination, now time.Time) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	s, ok := tr.seen.get(key)
	if !ok || s.uid != uid {
		// Dropped since pending ran (only another pod's state can push it out): start again from the bound.
		s = newPodState(uid, tr.evictedHigh)
	}
	if t.waiting {
		if w, ok := s.waits[t.slot()]; ok && w.id == t.id {
			w.reported = now
			s.waits[t.slot()] = w
		}
	}
	if slices.Contains(s.handled[t.slot()], t.id) {
		tr.seen.put(key, s)
		return
	}
	ids := append(s.handled[t.slot()], t.id) //nolint:gocritic // stored back into s.handled below; no other slice shares it
	if len(ids) > idsPerContainer {
		ids = ids[len(ids)-idsPerContainer:]
	}
	s.handled[t.slot()] = ids
	// A wait's finish time is the collector's first sight of it, not a time the cluster reported: it must not move
	// the eviction bound, which would mark later terminations of other pods MayRepeat. Nor must an eviction's
	// time of reading.
	if !t.waiting && !t.observed && t.finished.After(s.newest) {
		s.newest = t.finished
	}
	tr.seen.put(key, s)
}

// unhandled returns how many of ts the tracker has not handled for the pod.
func (tr *terminationTracker) unhandled(key types.NamespacedName, uid types.UID, ts []termination) int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	s, ok := tr.seen.get(key)
	n := 0
	for _, t := range ts {
		if !ok || s.uid != uid || !slices.Contains(s.handled[t.slot()], t.id) {
			n++
		}
	}
	return n
}

func (tr *terminationTracker) forget(key types.NamespacedName) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.seen.delete(key)
}

func (tr *terminationTracker) len() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.seen.len()
}

// podReconciler turns abnormal container terminations and the waiting reasons of WaitingCauseReasons in pod
// status into observations. This is how
// OOMKilled is seen: no component reports it as an event reason.
type podReconciler struct {
	c      *Collector
	reader client.Reader
}

// Reconcile emits one observation per termination of the pod not handled before.
func (r *podReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	start := r.c.started()
	var pod corev1.Pod
	if err := r.reader.Get(ctx, req.NamespacedName, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			r.c.pods.forget(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	ts := podSymptoms(&pod)
	now := r.c.now()
	for i := range ts {
		// A finish time in the future would otherwise keep later terminations of other pods marked
		// MayRepeat once this pod's state is dropped.
		ts[i].finished = r.c.notFuture(ts[i].finished)
		if ts[i].evicted && ts[i].finished.IsZero() {
			// No time from the kubelet (decided 2026-11-21): the time of this read, so the eviction is new to
			// this process and not marked MayRepeat.
			ts[i].finished, ts[i].observed = now, true
		}
	}
	pending := r.c.pods.pending(req.NamespacedName, pod.UID, ts, start, now)
	if len(pending) == 0 {
		return r.repeatLater(req.NamespacedName, pod.UID, now), nil
	}
	if pending[0].evicted {
		// The eviction replaces the pod's container terminations (eviction.go): one not handled yet, even one
		// whose delivery is being retried, is dropped here and never reported.
		if n := r.c.pods.unhandled(req.NamespacedName, pod.UID, terminations(&pod)); n > 0 {
			logf.FromContext(ctx).Info("Dropped container terminations of an evicted Pod, its eviction is the cause",
				"terminations", n)
		}
	}
	subject, owner, v, err := r.c.owners.podSubject(ctx, &pod)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch v {
	case subjectUnusable:
		return ctrl.Result{}, nil
	case subjectRetired:
		// The pod's template is retired (#724): each termination is handled without an observation and
		// counted once, so later updates of the pod do not read and count it again, and a rollback does not
		// bring the backlog back.
		news := 0
		for _, t := range pending {
			r.c.pods.commit(req.NamespacedName, pod.UID, t.termination, now)
			if !t.repeat {
				news++
			}
		}
		if news > 0 {
			dropRetired(ctx, subject.Kind, "container termination", news)
		}
		return r.repeatLater(req.NamespacedName, pod.UID, now), nil
	}
	for _, t := range pending {
		if t.evicted {
			obs := r.c.evictionObservation(subject, pod.Namespace, pod.Name, pod.UID, t.message, t.finished,
				t.mayRepeat, true)
			out, err := r.c.deliverEviction(ctx, pod.UID, obs, shownTerminations(&pod))
			if err != nil {
				return ctrl.Result{}, err
			}
			if out == evictionRetry {
				return ctrl.Result{RequeueAfter: retryAfter}, nil
			}
			r.c.pods.commit(req.NamespacedName, pod.UID, t.termination, now)
			continue
		}
		note := t.note()
		at := t.finished
		if t.repeat {
			at = now
		}
		obs := r.c.observation(r.c.podStatusInput(&pod, owner, t.reason, note), subject, pod.Namespace, at)
		obs.Occurrence, obs.Began, obs.Total, obs.MayRepeat = t.occurrence(pod.UID), t.finished, 1, t.mayRepeat
		// A waiting reason is never in immediateTerminationReasons: a wait is often short while an image is
		// pulled (ADR-0039 point 4).
		obs.Immediate = immediateTermination(t.reason)
		obs.Container, obs.Class = t.container, t.class()
		var sent bool
		if t.repeat {
			// The same occurrence reported again: nothing new to count (MayRepeat, Count 0), so the Incident
			// controller merges it, and its ObservedAt, SettleAfter or more after Began once the wait lasts,
			// opens the Incident (incidents.recurs). It goes to the consumer as it is: settling it with Handled
			// here would drop it, since its total is already counted or held.
			obs.MayRepeat = true
			sent = r.c.emit(ctx, obs)
		} else {
			if !t.mayRepeat {
				obs.Count = 1
			}
			sent, err = r.c.deliver(ctx, obs)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		if !sent {
			return ctrl.Result{RequeueAfter: retryAfter}, nil
		}
		if !t.waiting && !t.repeat {
			r.c.evictions.noteTermination(pod.UID, t.termination, obs.Fingerprint)
		}
		r.c.pods.commit(req.NamespacedName, pod.UID, t.termination, now)
	}
	return r.repeatLater(req.NamespacedName, pod.UID, now), nil
}

// repeatLater asks for the pod to be read again when a waiting state it shows is due to be reported again,
// so a wait the pod shows with no further update still reaches the Incident controller's gate.
func (r *podReconciler) repeatLater(key types.NamespacedName, uid types.UID, now time.Time) ctrl.Result {
	if d, ok := r.c.pods.nextRepeat(key, uid, now); ok {
		return ctrl.Result{RequeueAfter: d}
	}
	return ctrl.Result{}
}
