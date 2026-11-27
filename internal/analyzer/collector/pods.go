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

	"github.com/shadewright-labs/shadewright/internal/incidents/causereason"
	"github.com/shadewright-labs/shadewright/internal/incidents/fingerprint"
)

const (
	// ReasonOOMKilled is the termination reason of a container the kernel killed for using too much memory
	// (causereason.OOMKilled, which the context builder reads too).
	ReasonOOMKilled = causereason.OOMKilled
	// reasonTerminated replaces a termination reason that is not a plain CamelCase word.
	reasonTerminated = "Terminated"
	// reportingKubelet is the reporting controller of observations read from pod status.
	reportingKubelet = "kubelet"
	// idsPerContainer is how many terminations are remembered per container: status shows at most two, the
	// current state and the last one.
	idsPerContainer = 2
)

// termination is one container termination worth an observation, or one waiting reason of
// WaitingCauseReasons (waiting set), which has no finish time.
type termination struct {
	container string
	id        string
	reason    string
	exitCode  int32
	started   time.Time
	finished  time.Time
	waiting   bool
	image     string
	restarts  int32
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
				container: st.Name, id: id, reason: reason, exitCode: t.ExitCode,
				started: t.StartedAt.Time, finished: t.FinishedAt.Time,
			})
		}
	}
	return out
}

// waitings returns the waiting reasons of WaitingCauseReasons in a pod's status, in init and app containers
// (ADR-0039 point 4). A waiting state is identified by its reason and the container's restart count: it has no
// ID or start time. A pull also includes the spec image and treats the initial failure and back-off
// as one wait. A container that waits again with the same reason after a restart is a new one, and
// one that keeps waiting is the same one. Its finish time stays zero here; the tracker sets it to the time the
// collector first saw the state (terminationTracker.pending). The waiting message is never read: it is free
// text.
func waitings(pod *corev1.Pod) []termination {
	init := containerWaits(pod.Spec.InitContainers, pod.Status.InitContainerStatuses)
	return append(init, containerWaits(pod.Spec.Containers, pod.Status.ContainerStatuses)...)
}

func containerWaits(spec []corev1.Container, statuses []corev1.ContainerStatus) []termination {
	var out []termination
	var images map[string]string
	for _, st := range statuses {
		w := st.State.Waiting
		if w == nil || !causereason.IsWaiting(w.Reason) {
			continue
		}
		t := termination{container: st.Name, reason: w.Reason, restarts: st.RestartCount, waiting: true}
		reason := w.Reason
		if causereason.IsImagePull(reason) {
			if images == nil {
				images = make(map[string]string, len(spec))
				for _, c := range spec {
					images[c.Name] = c.Image
				}
			}
			t.image = images[st.Name]
			if t.image != "" {
				reason = pullWaitReason(reason)
			}
		}
		t.id = fmt.Sprintf("waiting-%s-restarts-%d", reason, t.restarts)
		if t.image != "" {
			t.id = fingerprint.Scoped(t.id, t.container, t.image)
		}
		out = append(out, t)
	}
	return out
}

// WaitingOccurrencePrefix starts the Occurrence of a waiting cause read from pod status. Such an occurrence
// is reported again, with nothing new to count, while the state is still shown (ADR-0039 point 4, amended
// 2026-11-16); the consumer tells it apart by this prefix.
const WaitingOccurrencePrefix = "waiting:"

// podSymptoms returns the terminations and the waiting causes of a pod's status.
func podSymptoms(pod *corev1.Pod) []termination {
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

// class is the part of t, a symptom read from pod's status, in the consequence rule (ADR-0039 point 3, amended
// 2026-11-20 and narrowed 2026-11-21): an OOM kill, a non-zero exit or a waiting reason is a cause. A termination
// that is not an OOM kill has no class when the pod's status.reason is set, whatever its value (Evicted, a node
// shutdown, a deadline, or any other): the kubelet stopped the pod, so its exits are not a crash loop. That is as
// before the crash loop link; evictions are designed apart (#1077). It is the one place that decides it, for the
// observation of t and for the cause a consequence links to.
func (t termination) class(pod *corev1.Pod) Class {
	if !t.waiting && t.reason != ReasonOOMKilled && pod.Status.Reason != "" {
		return ClassNone
	}
	return ClassCause
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
	// the eviction bound, which would mark later terminations of other pods MayRepeat.
	if !t.waiting && t.finished.After(s.newest) {
		s.newest = t.finished
	}
	tr.seen.put(key, s)
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
	for i := range ts {
		// A finish time in the future would otherwise keep later terminations of other pods marked
		// MayRepeat once this pod's state is dropped.
		orig := ts[i].finished
		ts[i].finished = r.c.notFuture(ts[i].finished)
		// Keep started only on a termination whose finish time was not clamped, with both times set
		// and start not after finish (in whole-second API precision), so any lifetime compares two
		// timestamps from the same pod status and never mixes clocks.
		if ts[i].waiting || ts[i].started.IsZero() || ts[i].finished.IsZero() ||
			!ts[i].finished.Equal(orig) ||
			ts[i].started.Truncate(time.Second).After(ts[i].finished.Truncate(time.Second)) {
			ts[i].started = time.Time{}
		}
	}
	now := r.c.now()
	pending := r.c.pods.pending(req.NamespacedName, pod.UID, ts, start, now)
	if len(pending) == 0 {
		return r.repeatLater(req.NamespacedName, pod.UID, now), nil
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
		note := terminationNote(t.container, t.exitCode)
		if t.waiting {
			note = waitingNote(t.container, t.reason)
		}
		at := t.finished
		if t.repeat {
			at = now
		}
		input := r.c.podStatusInput(&pod, owner, t.reason, note)
		obs := r.c.observation(input, subject, pod.Namespace, at)
		if t.image != "" {
			obs.Fingerprint = waitingFingerprint(input, t.container, t.image)
		}
		obs.Occurrence, obs.Began, obs.Started, obs.Total, obs.MayRepeat = t.occurrence(pod.UID), t.finished, t.started, 1, t.mayRepeat
		// A waiting reason is never in immediateTerminationReasons: a wait is often short while an image is
		// pulled (ADR-0039 point 4).
		obs.Immediate = immediateTermination(t.reason)
		obs.Container, obs.Class = t.container, t.class(&pod)
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
