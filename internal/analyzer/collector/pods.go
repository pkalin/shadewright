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
// container ID, so the same termination seen again is the same one. The termination message is never read:
// it is free text written by the container.
func terminations(pod *corev1.Pod) []termination {
	var out []termination
	for _, st := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
		for _, t := range []*corev1.ContainerStateTerminated{st.LastTerminationState.Terminated, st.State.Terminated} {
			if t == nil || (t.Reason != ReasonOOMKilled && t.ExitCode == 0) {
				continue
			}
			id := t.ContainerID
			if id == "" {
				id = fmt.Sprintf("restart-%d-%d", st.RestartCount, t.FinishedAt.Unix())
			}
			reason := t.Reason
			if !validReason(reason) {
				reason = reasonTerminated
			}
			out = append(out, termination{
				container: st.Name, id: id, reason: reason, exitCode: t.ExitCode, finished: t.FinishedAt.Time,
			})
		}
	}
	return out
}

func hasTermination(o client.Object) bool {
	pod, ok := o.(*corev1.Pod)
	return ok && len(terminations(pod)) > 0
}

type podState struct {
	uid     types.UID
	handled map[string][]string // container name to the IDs of its newest handled terminations
	newest  time.Time
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

// pending reports whether termination t of the pod is not yet handled, and whether it may have been
// emitted before (see Observation.MayRepeat).
func (tr *terminationTracker) pending(key types.NamespacedName, uid types.UID, t termination,
	start time.Time) (bool, bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if s, ok := tr.seen.get(key); ok && s.uid == uid {
		if slices.Contains(s.handled[t.container], t.id) {
			return false, false
		}
		return true, false
	}
	mayRepeat := t.finished.Before(start) || (!tr.evictedHigh.IsZero() && !t.finished.After(tr.evictedHigh))
	return true, mayRepeat
}

// commit records that termination t of the pod has been handled.
func (tr *terminationTracker) commit(key types.NamespacedName, uid types.UID, t termination) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	s, ok := tr.seen.get(key)
	if !ok || s.uid != uid {
		s = podState{uid: uid, handled: map[string][]string{}}
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
	var (
		resolved bool
		subject  remediationv1alpha1.ObjectRef
		owner    string
	)
	for _, t := range terminations(&pod) {
		isNew, mayRepeat := r.c.pods.pending(req.NamespacedName, pod.UID, t, start)
		if !isNew {
			continue
		}
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
		obs.Count, obs.MayRepeat = 1, mayRepeat
		skip, err := r.c.skipRepeat(ctx, &obs)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !skip && !r.c.emit(ctx, obs) {
			return ctrl.Result{RequeueAfter: retryAfter}, nil
		}
		r.c.pods.commit(req.NamespacedName, pod.UID, t)
	}
	return ctrl.Result{}, nil
}
