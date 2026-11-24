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

package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/incidents/quiet"
)

// EventIncidentStopped is the reason of the one Event recorded when a request is marked IncidentStopped.
const EventIncidentStopped = "IncidentStopped"

// messageIncidentStopped is the fixed text of condition IncidentStopped and of its Event. It holds nothing from
// the cluster.
const messageIncidentStopped = "The Incident's count has not changed for at least an hour while the pull request " +
	"waited for review, so its symptom may have stopped by itself. The pull request stays open: a person decides " +
	"whether to merge or close it"

// watchIncidentStopped is the look at the Incident of a request whose pull request waits for review (ADR-0030
// section 4, amended 2026-11-15, #715). advanceAndWatch calls it only after a full recheck that found the pull
// request open and the plan fresh and whose step did not stop at a pause (WritesPaused), so it rides that
// recheck's period and adds no requeue of its own. status.pullRequest.incidentStopped is the source of the mark;
// condition IncidentStopped is derived from it in the same status patch. It reads the Incident once
// (readIncidentOrGone) and:
//
//   - decides nothing on a failed read or on a request outside AwaitingApproval; the next full recheck reads
//     again;
//   - takes an Incident that is gone (NotFound) or made again under the same name (another UID than
//     spec.incidentRef.uid) for the end of the request's Incident: it removes a mark and never marks;
//   - with status.pullRequest.incidentStopped recorded, removes it and condition IncidentStopped when the count
//     read differs from the recorded one, and otherwise leaves both; a condition without the field (dropped by
//     an executor that predates it) is removed too;
//   - without it, remembers in memory (incidentWatch) the count and the executor's time after the first read of
//     that count, and marks the request (the field, the condition and one Event) on a later read of the same
//     count at least quiet.BeforeResolved after it. Both times are from this process's clock: after a restart or
//     a change of leader the wait starts again. A saturated count or an Incident whose newest occurrence is a
//     lasting symptom state (quiet.LastingState), whose count does not move while it lasts, is never marked.
//
// The phase, the pull request and the lock on the target are never touched. A failed status write is returned
// and records no Event; advanceAndWatch tries it once more on a conflict and otherwise leaves it to the next full
// recheck. The Event is recorded only after a status write that answered, so there is at most one per mark: a
// write that went through but whose answer was lost records none, and the condition stays the signal.
func (r *RemediationRequestReconciler) watchIncidentStopped(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) error {
	pr := rr.Status.PullRequest
	if rr.Status.Phase != remediationv1alpha1.PhaseAwaitingApproval || pr == nil || rr.Spec.IncidentRef.UID == "" {
		return nil
	}
	key := client.ObjectKeyFromObject(rr)
	log := logf.FromContext(ctx)
	inc, before, gone, ok := r.readIncidentOrGone(ctx, rr)
	switch {
	case gone:
		return r.unmarkGone(ctx, rr, "The Incident of RemediationRequest is gone")
	case !ok:
		// readIncidentOrGone logged why; a failed read is no sign that the Incident stopped or went on.
		log.V(1).Info("No count of the Incident of RemediationRequest; nothing is decided about IncidentStopped",
			"remediationRequest", key, "incident", rr.Spec.IncidentRef.Name)
		return nil
	case inc.UID != types.UID(rr.Spec.IncidentRef.UID):
		return r.unmarkGone(ctx, rr, "The Incident of RemediationRequest was made again under the same name")
	}
	after := r.now()
	count := inc.Status.Count
	orphan := pr.IncidentStopped == nil &&
		meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionIncidentStopped) != nil
	if marked := pr.IncidentStopped; marked != nil || orphan {
		// Without the field (an executor that predates it dropped it in its status write) the count of the mark is
		// unknown, so the condition goes and the wait starts again.
		if marked != nil && types.UID(marked.UID) == inc.UID && marked.Count == count {
			return nil
		}
		if err := r.unmarkIncidentStopped(ctx, rr); err != nil {
			return err
		}
		r.incidentWatch.note(rr, inc.UID, count, after)
		log.Info("The Incident of RemediationRequest counted again; removed IncidentStopped",
			"remediationRequest", key, "incident", inc.Name, "count", count)
		return nil
	}
	since, same := r.incidentWatch.since(rr, inc.UID, count)
	if !same || since.After(before) {
		// A first read of this count by this process, or a clock that went back: the wait starts now.
		r.incidentWatch.note(rr, inc.UID, count, after)
		return nil
	}
	if count >= remediationv1alpha1.MaxIncidentCount || quiet.LastingState(&inc.Status) ||
		before.Sub(since) < quiet.BeforeResolved {
		return nil
	}
	base := rr.DeepCopy()
	pr.IncidentStopped = &remediationv1alpha1.IncidentCountReading{UID: string(inc.UID), Count: count,
		ReadAt: metav1.NewTime(before.Truncate(time.Second))}
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionIncidentStopped,
		Status:             metav1.ConditionTrue,
		Reason:             remediationv1alpha1.ReasonIncidentCountUnchanged,
		Message:            messageIncidentStopped,
		ObservedGeneration: rr.Generation,
	})
	if err := r.Status().Patch(ctx, rr, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("mark RemediationRequest %s/%s IncidentStopped: %w", rr.Namespace, rr.Name, err)
	}
	r.event(rr, EventIncidentStopped, messageIncidentStopped)
	log.Info("The Incident of RemediationRequest stopped counting while its pull request waits "+
		"for review; marked IncidentStopped", "remediationRequest", key, "incident", inc.Name, "count", count,
		"unchangedFor", before.Sub(since))
	return nil
}

// unmarkGone is watchIncidentStopped on a request whose Incident is gone: it forgets the wait and removes a mark,
// logging why with msg. A request without a mark is not written.
func (r *RemediationRequestReconciler) unmarkGone(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	msg string) error {
	key := client.ObjectKeyFromObject(rr)
	r.incidentWatch.forget(key)
	if rr.Status.PullRequest.IncidentStopped == nil &&
		meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionIncidentStopped) == nil {
		logf.FromContext(ctx).V(1).Info(msg+"; it is not marked IncidentStopped", "remediationRequest", key,
			"incident", rr.Spec.IncidentRef.Name)
		return nil
	}
	if err := r.unmarkIncidentStopped(ctx, rr); err != nil {
		return err
	}
	logf.FromContext(ctx).Info(msg+"; removed IncidentStopped", "remediationRequest", key,
		"incident", rr.Spec.IncidentRef.Name)
	return nil
}

// unmarkIncidentStopped removes status.pullRequest.incidentStopped and condition IncidentStopped in one status
// patch, on the version read (optimistic lock).
func (r *RemediationRequestReconciler) unmarkIncidentStopped(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) error {
	base := rr.DeepCopy()
	rr.Status.PullRequest.IncidentStopped = nil
	meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionIncidentStopped)
	if err := r.Status().Patch(ctx, rr, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("remove IncidentStopped from RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	return nil
}

// incidentWatch keeps, per request, the Incident's count this process last read while the pull request waited
// for review and its own time after the first read of that count (watchIncidentStopped). It is memory only, so
// the wait is always measured on one process's clock; it is dropped when the request leaves AwaitingApproval
// (setPhase), is being deleted or is gone.
type incidentWatch struct {
	m sync.Map
}

// watchedCount is one entry of incidentWatch.
type watchedCount struct {
	request  types.UID
	incident types.UID
	count    int64
	since    time.Time
}

// note starts the wait for rr at since, for count of the Incident with UID incident.
func (w *incidentWatch) note(rr *remediationv1alpha1.RemediationRequest, incident types.UID, count int64,
	since time.Time) {
	w.m.Store(client.ObjectKeyFromObject(rr), watchedCount{request: rr.UID, incident: incident, count: count,
		since: since})
}

// since returns when this process first read count of the Incident incident for rr, and false when its entry
// is of another request, Incident or count, or there is none.
func (w *incidentWatch) since(rr *remediationv1alpha1.RemediationRequest, incident types.UID, count int64) (
	time.Time, bool) {
	v, ok := w.m.Load(client.ObjectKeyFromObject(rr))
	if !ok {
		return time.Time{}, false
	}
	e := v.(watchedCount)
	if e.request != rr.UID || e.incident != incident || e.count != count {
		return time.Time{}, false
	}
	return e.since, true
}

// forget drops the entry of the request with key.
func (w *incidentWatch) forget(key types.NamespacedName) {
	w.m.Delete(key)
}
