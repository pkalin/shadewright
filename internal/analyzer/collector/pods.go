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

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/incidents/fingerprint"
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

// termination is one container termination worth an observation.
type termination struct {
	container string
	id        string
	reason    string
	exitCode  int32
	finished  time.Time
}

// terminations returns the abnormal terminations in a pod's status: OOMKilled or a non-zero exit code, in
// init and app containers, from the current and the last state. A termination is identified by its
// container ID, so the same termination seen again is the same one; without one, by its finish time, exit
// code and reason, which stay the same when it moves from the current to the last state (the restart count
// does not). The termination message is never read: it is free text written by the container.
func terminations(pod *corev1.Pod) []termination {
	var out []termination
	for _, st := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
		for _, t := range []*corev1.ContainerStateTerminated{st.LastTerminationState.Terminated, st.State.Terminated} {
			if t == nil || (t.Reason != ReasonOOMKilled && t.ExitCode == 0) {
				continue
			}
			reason := t.Reason
			if !validReason(reason) {
				reason = reasonTerminated
			}
			id := t.ContainerID
			if id == "" {
				id = fmt.Sprintf("finished-%d-exit-%d-%s", t.FinishedAt.Unix(), t.ExitCode, reason)
			}
			out = append(out, termination{
				container: st.Name, id: id, reason: reason, exitCode: t.ExitCode, finished: t.FinishedAt.Time,
			})
		}
	}
	return out
}

// occurrence identifies a termination across collectors without carrying the container ID itself.
func (t termination) occurrence(pod types.UID) string {
	h := sha256.Sum256([]byte(string(pod) + "\x00" + t.container + "\x00" + t.id))
	return "termination:" + hex.EncodeToString(h[:16])
}

func hasTermination(o client.Object) bool {
	pod, ok := o.(*corev1.Pod)
	return ok && len(terminations(pod)) > 0
}

type podState struct {
	uid     types.UID
	handled map[string][]string // container name to the IDs of its newest handled terminations
	newest  time.Time
	// repeatBefore and repeatThrough are fixed when the collector first sees the pod: its terminations that
	// finished before repeatBefore (the collector's start), or no later than repeatThrough (the newest
	// finish time dropped from the state until then), may have been emitted by an earlier process.
	repeatBefore, repeatThrough time.Time
}

// pendingTermination is a termination not handled yet.
type pendingTermination struct {
	termination
	mayRepeat bool
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
// before (see Observation.MayRepeat). That is decided from the bounds recorded when the collector first saw
// the pod, so it is the same for every termination of the pod and does not change when some of them are
// committed and the rest wait for a retry.
func (tr *terminationTracker) pending(key types.NamespacedName, uid types.UID, ts []termination,
	start time.Time) []pendingTermination {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	s, ok := tr.seen.get(key)
	if !ok || s.uid != uid {
		s = podState{uid: uid, handled: map[string][]string{}, repeatBefore: start, repeatThrough: tr.evictedHigh}
		tr.seen.put(key, s)
	}
	var out []pendingTermination
	for _, t := range ts {
		if slices.Contains(s.handled[t.container], t.id) {
			continue
		}
		mayRepeat := t.finished.Before(s.repeatBefore) ||
			(!s.repeatThrough.IsZero() && !t.finished.After(s.repeatThrough))
		out = append(out, pendingTermination{termination: t, mayRepeat: mayRepeat})
	}
	return out
}

// commit records that termination t of the pod has been handled.
func (tr *terminationTracker) commit(key types.NamespacedName, uid types.UID, t termination) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	s, ok := tr.seen.get(key)
	if !ok || s.uid != uid {
		// Dropped since pending ran (only another pod's state can push it out): start again from the bound.
		s = podState{uid: uid, handled: map[string][]string{}, repeatThrough: tr.evictedHigh}
	}
	ids := append(s.handled[t.container], t.id)
	if len(ids) > idsPerContainer {
		ids = ids[len(ids)-idsPerContainer:]
	}
	s.handled[t.container] = ids
	if t.finished.After(s.newest) {
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

// podReconciler turns abnormal container terminations in pod status into observations. This is how
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
	ts := terminations(&pod)
	for i := range ts {
		// A finish time in the future would otherwise keep later terminations of other pods marked
		// MayRepeat once this pod's state is dropped.
		ts[i].finished = r.c.notFuture(ts[i].finished)
	}
	var (
		resolved bool
		subject  remediationv1alpha1.ObjectRef
		owner    string
	)
	for _, t := range r.c.pods.pending(req.NamespacedName, pod.UID, ts, start) {
		if !resolved {
			ref, ownerUID, ok, err := r.c.owners.podSubject(ctx, &pod)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !ok {
				return ctrl.Result{}, nil
			}
			subject, owner, resolved = ref, ownerUID, true
		}
		fo := fingerprint.Observation{
			ClusterID:           r.c.cfg.ClusterID,
			Namespace:           pod.Namespace,
			OwnerUID:            owner,
			ResourceUID:         string(pod.UID),
			Reason:              t.reason,
			Type:                corev1.EventTypeWarning,
			ReportingController: reportingKubelet,
			Note:                fmt.Sprintf("Container %s terminated with exit code %d", t.container, t.exitCode),
			RegardingKind:       kindPod,
			RegardingName:       pod.Name,
		}
		obs := r.c.observation(fo, subject, t.finished)
		obs.Occurrence, obs.Total, obs.MayRepeat = t.occurrence(pod.UID), 1, t.mayRepeat
		if !t.mayRepeat {
			obs.Count = 1
		}
		sent, err := r.c.deliver(ctx, obs)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !sent {
			return ctrl.Result{RequeueAfter: retryAfter}, nil
		}
		r.c.pods.commit(req.NamespacedName, pod.UID, t.termination)
	}
	return ctrl.Result{}, nil
}
