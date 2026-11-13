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
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
)

// Duplicates of a merged request (#556). The analyzer may propose more than one request for one Incident and
// target; only one of them holds the target lock at a time (targetlock.go), and the others wait, Queued with
// TargetLocked. Once the pull request of one of them is merged, the Incident they answer has its fix, so the
// others are not needed: they end Superseded with reason AnotherRequestMerged, whose message names the merged
// request and its pull request, before they take the lock. Without this they went on to the policy check as
// soon as the lock was released, found Flux applying the merged revision and ended NeedsIntervention.
//
// The executor checks this for every undecided request before the policy check (Reconcile, so neither a wait
// for the target's state nor a refusal comes first), each time the limits let a Queued request through
// (holdBack, so a duplicate waiting behind the lock reads nothing from Git), and under the admission lock right
// before an allowed decision takes the target lock. The holder releases the lock in the same status write that
// sets Merged (stop, settleLock), and every candidate the cache does not show ended is read again from the API
// server, so a check of the limits that saw the lock released is followed by a read that sees Merged.
//
// Only a request that cannot have a pull request of its own is ended this way: no phase or Pending, no recorded
// pull request, no target lock, no supersede chain (spec.replaces, status.handoff: ADR-0028's checks decide
// those). The merged request names the same Incident UID and target UID, the same action type and the same
// field (another container of the same Deployment is another fix), and was merged (mergeTime, which no
// later phase moves) after the duplicate was created, so a request proposed after the merge (the fix did not
// help) is decided as usual. Its change must still be in effect (changeInEffect): the live target holds its
// planned value, or, within TargetStateTimeout of the merge, still the value it replaced (Flux has not applied
// the merge yet). A merge that was reverted, or a target changed since by anything else, supersedes nothing: the
// duplicate is decided as usual, and its own previous value decides whether it is stale. It writes only the
// duplicate's own status, which the executor already writes (invariant 6), and nothing to Git (invariant 3).
//
// Requests after a pull request closed without a merge (#643). A request of any Incident that waited while
// another request for the same target and field held the lock ends Superseded with reason AnotherRequestClosed
// when that other request's pull request was closed without a merge after this one was created: a reviewer
// who closed the first proposal said no to that change, and the waiting one was planned without knowing it. It
// does not go back to the analyzer; if the symptom continues, the Incident's next analysis proposes again, with
// the closed request in its history. Before #643 it took the lock and opened a second pull request for the same
// field (lab run of 2026-11-10). The same checks and call sites as for a merge apply; the decision reads only
// recorded state (invariant 5): the other request ended NeedsIntervention with reason ChangeRequestClosed
// (closedUnmerged), its target UID and field (sameField), and its lastPhaseTransitionTime, which no later phase
// moves since NeedsIntervention is final, against this request's creation. A request for another field of the
// same target, or one created after the close, is decided as usual.

// supersedable reports whether rr may end Superseded as the duplicate of a merged request: it is undecided (no
// phase or Pending), holds no lock, recorded no pull request, and is in no supersede chain.
func supersedable(rr *remediationv1alpha1.RemediationRequest) bool {
	return (rr.Status.Phase == "" || rr.Status.Phase == remediationv1alpha1.PhasePending) &&
		!holdsLock(rr) && rr.Status.PullRequest == nil && rr.Spec.Replaces == nil && rr.Status.Handoff == nil &&
		rr.DeletionTimestamp.IsZero()
}

// mergeTime returns when w was merged: status.pullRequest.mergedAt, set once with the phase Merged. A request
// merged before that field existed and still Merged has its lastPhaseTransitionTime, which is then the merge's;
// in a later phase that time has moved, so such a request has no merge time (ok false) and supersedes nothing.
func mergeTime(w *remediationv1alpha1.RemediationRequest) (time.Time, bool) {
	if w.Status.PullRequest == nil {
		return time.Time{}, false
	}
	if t := w.Status.PullRequest.MergedAt; t != nil {
		return t.Time, true
	}
	if w.Status.Phase == remediationv1alpha1.PhaseMerged && w.Status.LastPhaseTransitionTime != nil {
		return w.Status.LastPhaseTransitionTime.Time, true
	}
	return time.Time{}, false
}

// mergesBefore reports whether w is a merged request of rr's Incident and target, changing the same field with
// the same action type (sameField), that was merged after rr was created: a merge rr was proposed without
// knowing about.
func mergesBefore(w, rr *remediationv1alpha1.RemediationRequest) bool {
	if w.UID == rr.UID || rr.Spec.IncidentRef.UID == "" || w.Spec.IncidentRef.UID != rr.Spec.IncidentRef.UID ||
		w.Spec.TargetRef.UID != rr.Spec.TargetRef.UID || !slices.Contains(successPhases, w.Status.Phase) ||
		!sameField(w, rr) {
		return false
	}
	at, ok := mergeTime(w)
	return ok && rr.CreationTimestamp.Time.Before(at)
}

// sameField reports whether w and rr have the same action type and change the same field of the target
// (sourcemap.FieldFor): the memory limit of another container of the same Deployment is another fix, so its
// merge supersedes nothing. A field that cannot be located is never the same.
func sameField(w, rr *remediationv1alpha1.RemediationRequest) bool {
	if w.Spec.Action.Type != rr.Spec.Action.Type {
		return false
	}
	wf, err := sourcemap.FieldFor(w.Spec.Action, w.Spec.TargetRef)
	if err != nil {
		return false
	}
	rf, err := sourcemap.FieldFor(rr.Spec.Action, rr.Spec.TargetRef)
	if err != nil {
		return false
	}
	return wf.Quantity == rf.Quantity && slices.Equal(wf.Steps, rf.Steps)
}

// closedUnmerged reports whether w ended because its pull request was closed without a merge: NeedsIntervention
// with reason ChangeRequestClosed, on Approved (the stop of a request with a recorded pull request), or on
// PullRequestOpened (the stop before one was recorded, or markClosed). Shadewright's own close of a stale plan
// (Superseded, PullRequestClosed) and every other end are not a reviewer's no.
func closedUnmerged(w *remediationv1alpha1.RemediationRequest) bool {
	if w.Status.Phase != remediationv1alpha1.PhaseNeedsIntervention {
		return false
	}
	for _, t := range []string{remediationv1alpha1.ConditionApproved, remediationv1alpha1.ConditionPROpened} {
		if c := meta.FindStatusCondition(w.Status.Conditions, t); c != nil && c.Reason == ReasonChangeRequestClosed {
			return true
		}
	}
	return false
}

// closedBefore reports whether w is a request for rr's target, of any Incident, changing the same field with the
// same action type (sameField), whose pull request was closed without a merge (closedUnmerged) after rr was
// created: a "no" rr was proposed without knowing about. NeedsIntervention is final, so its
// lastPhaseTransitionTime is the time it ended and never moves.
func closedBefore(w, rr *remediationv1alpha1.RemediationRequest) bool {
	if w.UID == rr.UID || w.Spec.TargetRef.UID != rr.Spec.TargetRef.UID || !closedUnmerged(w) || !sameField(w, rr) {
		return false
	}
	at := w.Status.LastPhaseTransitionTime
	return at != nil && rr.CreationTimestamp.Time.Before(at.Time)
}

// changeInEffect reports whether the change of the merged request w still holds: the live target, read from the
// API server, holds w's desired value, or, until TargetStateTimeout after the merge, still w's previous value
// (the merge is not applied yet). Anything else (a revert applied, a target changed since, deleted or replaced
// by another object, a field it cannot read) is false, so the duplicate is decided as usual. Only a failed read
// is an error.
func (r *RemediationRequestReconciler) changeInEffect(ctx context.Context, w *remediationv1alpha1.RemediationRequest) (
	bool, error) {
	at, ok := mergeTime(w)
	if !ok {
		return false, nil
	}
	field, err := sourcemap.FieldFor(w.Spec.Action, w.Spec.TargetRef)
	if err != nil {
		return false, nil
	}
	live, found, err := r.target(ctx, w.Spec.TargetRef)
	if err != nil || !found || live.GetUID() != types.UID(w.Spec.TargetRef.UID) {
		return false, err
	}
	got, found := field.Get(live.Object)
	if !found || !field.Comparable(got) {
		return false, nil
	}
	if desired, ok := sourcemap.DesiredValue(w.Spec.Action); ok && field.Equals(got, desired) {
		return true, nil
	}
	previous, ok := sourcemap.PreviousValue(w.Spec.Action)
	return ok && field.Equals(got, previous) && r.now().Sub(at) < r.targetStateTimeout(), nil
}

// endingRequestOf returns the request that ends rr, with the PlanFresh reason to end it with, or nil: a request
// merged before rr (mergesBefore) whose change is still in effect (changeInEffect), reason AnotherRequestMerged,
// or else one whose pull request was closed without a merge after rr was created (closedBefore), reason
// AnotherRequestClosed. The candidates are the other requests for rr's target in the cache. One the cache shows
// merged is taken as it is, since its phase never goes back to before the merge (the phases after Merged are
// merges too; a revert is read from the live target), and so is one the cache shows ended, since a final phase
// never changes. One the cache shows in a phase that has not ended is read again from the API server, since the
// cache may not show its end yet, while the limits, which read the API server, already saw the lock it released
// in the same write. Any read that fails is returned (the request is looked at again, and reads nothing from Git
// meanwhile), never taken as a reason to end it.
func (r *RemediationRequestReconciler) endingRequestOf(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	*remediationv1alpha1.RemediationRequest, string, error) {
	if !supersedable(rr) {
		return nil, "", nil
	}
	list := &remediationv1alpha1.RemediationRequestList{}
	if err := r.List(ctx, list, client.InNamespace(rr.Namespace)); err != nil {
		return nil, "", fmt.Errorf("list RemediationRequests in %s: %w", rr.Namespace, err)
	}
	var closed *remediationv1alpha1.RemediationRequest
	for i := range list.Items {
		w := &list.Items[i]
		if w.UID == rr.UID || w.Spec.TargetRef.UID != rr.Spec.TargetRef.UID {
			continue
		}
		if !slices.Contains(successPhases, w.Status.Phase) && !ended(w.Status.Phase) {
			live := &remediationv1alpha1.RemediationRequest{}
			if err := r.reader().Get(ctx, client.ObjectKeyFromObject(w), live); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return nil, "", fmt.Errorf("get RemediationRequest %s/%s: %w", w.Namespace, w.Name, err)
			}
			w = live
		}
		if closed == nil && closedBefore(w, rr) {
			closed = w.DeepCopy()
			continue
		}
		if !mergesBefore(w, rr) {
			continue
		}
		switch inEffect, err := r.changeInEffect(ctx, w); {
		case err != nil:
			return nil, "", err
		case inEffect:
			return w.DeepCopy(), remediationv1alpha1.ReasonAnotherRequestMerged, nil
		}
	}
	if closed != nil {
		return closed, remediationv1alpha1.ReasonAnotherRequestClosed, nil
	}
	return nil, "", nil
}

// supersedeDuplicate ends rr Superseded because of w: PlanFresh False with reason (AnotherRequestMerged or
// AnotherRequestClosed) and a message naming w and its pull request, Queued removed, no lock. The status write
// uses rr's resourceVersion, so a conflict (another write meanwhile) is returned and the request is looked at
// again.
func (r *RemediationRequestReconciler) supersedeDuplicate(ctx context.Context, rr, w *remediationv1alpha1.RemediationRequest,
	reason string) error {
	pr := "its pull request"
	if w.Status.PullRequest != nil {
		pr = fmt.Sprintf("pull request #%d", w.Status.PullRequest.Number)
	}
	message := fmt.Sprintf("RemediationRequest %s of the same Incident and target was merged (%s) "+
		"after this one was created; this one is not needed", w.Name, pr)
	if reason == remediationv1alpha1.ReasonAnotherRequestClosed {
		message = fmt.Sprintf("RemediationRequest %s for the same target and field had %s closed without a merge "+
			"after this one was created; this one opens no pull request for that field. A new analysis of the "+
			"Incident proposes again if the symptom continues", w.Name, pr)
	}
	r.setPhase(rr, remediationv1alpha1.PhaseSuperseded)
	rr.Status.ObservedGeneration = rr.Generation
	rr.Status.HoldsTarget = new(false)
	meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionQueued)
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionPlanFresh,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: rr.Generation,
	})
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	logf.FromContext(ctx).Info("Superseded RemediationRequest: another request for the same target ended it",
		"remediationRequest", client.ObjectKeyFromObject(rr), "by", w.Name, "reason", reason)
	return nil
}

// endIfDuplicate ends rr Superseded when a request of its Incident and target was merged after rr was created,
// or a request for its target and field had its pull request closed without a merge after rr was created, and
// reports whether it did (or the error of the look-up or the write, with ended false).
func (r *RemediationRequestReconciler) endIfDuplicate(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	bool, error) {
	lookCtx, cancel := bound(ctx, r.CheckTimeout, DefaultCheckTimeout)
	w, reason, err := r.endingRequestOf(lookCtx, rr)
	err = boundErr(ctx, lookCtx, "look-up of a request that ends this one", err)
	cancel()
	if err != nil || w == nil {
		return false, err
	}
	if err := r.supersedeDuplicate(ctx, rr, w, reason); err != nil {
		return false, err
	}
	return true, nil
}
