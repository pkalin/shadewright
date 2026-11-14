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
// Requests queued behind a pull request closed without a merge (#643). A request of any Incident that a limit
// holds back (Queued) while its target is locked records the requests that hold the lock in
// status.queuedBehind, in the status write that queues it (waitForLimits). It ends Superseded with reason AnotherRequestClosed when a
// recorded holder for the same target and field ended with its pull request closed without a merge: a reviewer
// who closed that proposal said no to the change, and the waiting one was planned without knowing it. It does
// not go back to the analyzer; if the symptom continues, the Incident's next analysis proposes again, with the
// closed request in its history. Before #643 it took the lock and opened a second pull request for the same
// field (lab run of 2026-11-10).
//
// The decision reads only recorded state, never a clock (invariant 5): the record, and each recorded holder read
// from the API server (recordedHolderOf), since the cache may not show the close yet while the limits, which read
// the API server, already saw the lock it released in the same write; checkHeld records a close without a phase
// change. A holder that ended NeedsIntervention with reason ChangeRequestClosed (closedUnmerged), on the same
// target UID and field (sameField), ends the request. A request that never waited behind it (created, or first
// checked, after the lock was released) has no record of it and is decided as usual, and so is one for another
// field. When a later check finds other holders (a handoff, or another request took the lock first), the
// recorded ones are checked before the record is replaced (holdBack). The same call sites as for a merge apply.
//
// Requests queued behind a change that succeeded (#719). With verification on (--verify-outcomes) a merged holder
// keeps the lock until it ends (heldForVerification), so a request queued behind it for the same field does not
// meet the holder's change on its way to the cluster (the merged value in Git and the old one live, ValueMismatch,
// lab run of 2026-11-14). When the holder ends Succeeded, a recorded holder whose change of the same field went
// from the same previous value as the waiter's to another value in the same direction (succeededInstead) ends the
// waiter Superseded with reason AnotherRequestSucceeded: its baseline is gone because of a change that was merged
// and verified. When the holder ends Failed or Inconclusive the waiter is decided as usual, against the live
// value. The decision reads the record and the holder's phase and spec, never a clock.

// supersedable reports whether rr may end Superseded as the duplicate of a merged request: it is undecided (no
// phase or Pending), holds no lock, recorded no pull request, and is in no supersede chain.
func supersedable(rr *remediationv1alpha1.RemediationRequest) bool {
	return (rr.Status.Phase == "" || rr.Status.Phase == remediationv1alpha1.PhasePending) &&
		!holdsLock(rr) && rr.Status.PullRequest == nil && rr.Spec.Replaces == nil && rr.Status.Handoff == nil &&
		rr.DeletionTimestamp.IsZero()
}

// mergedPhases are the phases a merged request can be in, listed one by one (ADR-0030 section 10): Merged and
// every phase after it, Inconclusive and Failed included. Failed also ends steps before the merge; such a request
// has no merge time (mergeTime), so it supersedes nothing.
var mergedPhases = []remediationv1alpha1.RemediationPhase{remediationv1alpha1.PhaseMerged,
	remediationv1alpha1.PhaseWaitingForDelivery, remediationv1alpha1.PhaseVerifying, remediationv1alpha1.PhaseSucceeded,
	remediationv1alpha1.PhaseInconclusive, remediationv1alpha1.PhaseFailed}

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
func mergesBefore(ctx context.Context, w, rr *remediationv1alpha1.RemediationRequest) bool {
	if w.UID == rr.UID || rr.Spec.IncidentRef.UID == "" || w.Spec.IncidentRef.UID != rr.Spec.IncidentRef.UID ||
		w.Spec.TargetRef.UID != rr.Spec.TargetRef.UID || !slices.Contains(mergedPhases, w.Status.Phase) ||
		!sameField(ctx, w, rr) {
		return false
	}
	at, ok := mergeTime(w)
	return ok && rr.CreationTimestamp.Time.Before(at)
}

// sameField reports whether w and rr have the same action type and change the same field of the target
// (sourcemap.FieldFor): the memory limit of another container of the same Deployment is another fix, so its
// merge supersedes nothing. A field that cannot be located is never the same; that is logged, since the action
// of a request the policy check accepts always names a field.
func sameField(ctx context.Context, w, rr *remediationv1alpha1.RemediationRequest) bool {
	_, ok := commonField(ctx, w, rr)
	return ok
}

// commonField returns the field w and rr both change, located once for each (sameField), or false when they
// change different fields or one cannot be located.
func commonField(ctx context.Context, w, rr *remediationv1alpha1.RemediationRequest) (sourcemap.Field, bool) {
	if w.Spec.Action.Type != rr.Spec.Action.Type {
		return sourcemap.Field{}, false
	}
	locate := func(o *remediationv1alpha1.RemediationRequest) (sourcemap.Field, bool) {
		f, err := sourcemap.FieldFor(o.Spec.Action, o.Spec.TargetRef)
		if err != nil {
			logf.FromContext(ctx).Error(err, "Failed to locate the field a RemediationRequest changes; it counts as another field",
				"remediationRequest", client.ObjectKeyFromObject(o))
		}
		return f, err == nil
	}
	wf, ok := locate(w)
	if !ok {
		return sourcemap.Field{}, false
	}
	rf, ok := locate(rr)
	if !ok || wf.Quantity != rf.Quantity || !slices.Equal(wf.Steps, rf.Steps) {
		return sourcemap.Field{}, false
	}
	return rf, true
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

// succeededInstead reports whether w, a holder rr waited behind, ended Succeeded with a change of the same field
// (sameField) planned from the same previous value as rr's, to another value in the same direction as rr's (both
// above or both below that previous value) (#719): w's change took rr's baseline away, went the way rr would have
// gone, and was merged and verified, so rr is not needed. A holder whose change went from another previous value,
// to rr's previous value, or the other way (256Mi to 128Mi for a waiter planned 256Mi to 512Mi), does not stand
// for rr's change, so rr is decided as usual, and so is rr when any of its values or the holder's cannot be
// compared (Field.Comparable, Field.Compare): a value that cannot be read never ends a request. A Succeeded holder
// of the same field that does not end rr is logged with the check that failed. It reads the recorded spec and
// phase only.
func succeededInstead(ctx context.Context, w, rr *remediationv1alpha1.RemediationRequest) bool {
	if w.Status.Phase != remediationv1alpha1.PhaseSucceeded {
		return false
	}
	field, ok := commonField(ctx, w, rr)
	if !ok {
		return false
	}
	failed := succeededInsteadCheck(field, w, rr)
	if failed != "" {
		logf.FromContext(ctx).Info("Kept RemediationRequest: the request it waited behind ended Succeeded but does not end it",
			"remediationRequest", client.ObjectKeyFromObject(rr), "holder", w.Name, "check", failed)
		return false
	}
	return true
}

// succeededInsteadCheck returns the check of succeededInstead that w's change of field fails for rr, or "" when
// it passes them all.
func succeededInsteadCheck(field sourcemap.Field, w, rr *remediationv1alpha1.RemediationRequest) string {
	ours, ok1 := sourcemap.PreviousValue(rr.Spec.Action)
	want, ok2 := sourcemap.DesiredValue(rr.Spec.Action)
	theirs, ok3 := sourcemap.PreviousValue(w.Spec.Action)
	set, ok4 := sourcemap.DesiredValue(w.Spec.Action)
	switch {
	case !ok1 || !ok2 || !ok3 || !ok4:
		return "an action has no previous or desired value"
	case !field.Comparable(ours):
		return "the waiting request's previous value cannot be compared"
	case !field.Comparable(want):
		return "the waiting request's desired value cannot be compared"
	case !field.Comparable(theirs):
		return "the holder's previous value cannot be compared"
	case !field.Comparable(set):
		return "the holder's desired value cannot be compared"
	case !field.Equals(ours, theirs):
		return "the holder changed the field from another previous value"
	case field.Equals(ours, set):
		return "the holder set the waiting request's previous value"
	}
	ourWay, okA := field.Compare(want, ours)
	theirWay, okB := field.Compare(set, ours)
	switch {
	case !okA || !okB:
		return "the direction of a change cannot be compared"
	case ourWay != theirWay:
		return "the holder changed the field the other way"
	}
	return ""
}

// recordedHolderOf returns a request rr waited behind (status.queuedBehind) on rr's target that ends rr, with the
// PlanFresh reason to end it with, or nil: one that ended with its pull request for rr's field closed without a
// merge (closedUnmerged, sameField), reason AnotherRequestClosed (#643), or one that ended Succeeded with a change
// that took rr's baseline away (succeededInstead), reason AnotherRequestSucceeded (#719). Each recorded request is
// read from the API server, never the cache, which may not show the end yet; one that no longer exists, or exists
// under a new UID, ends nothing. Any other failed read is returned, never taken as a reason to end rr or to let it
// through.
func (r *RemediationRequestReconciler) recordedHolderOf(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	*remediationv1alpha1.RemediationRequest, string, error) {
	for _, ref := range rr.Status.QueuedBehind {
		if types.UID(ref.UID) == rr.UID {
			continue
		}
		w := &remediationv1alpha1.RemediationRequest{}
		key := client.ObjectKey{Namespace: rr.Namespace, Name: ref.Name}
		if err := r.reader().Get(ctx, key, w); err != nil {
			if apierrors.IsNotFound(err) {
				// Deleted by hand (its finalizer released the lock): it ends nothing, and rr is decided as usual.
				logf.FromContext(ctx).Info("Found no RemediationRequest this one waited behind; it ends nothing",
					"remediationRequest", client.ObjectKeyFromObject(rr), "holder", ref.Name)
				continue
			}
			return nil, "", fmt.Errorf("get RemediationRequest %s: %w", key, err)
		}
		if w.UID != types.UID(ref.UID) || w.Spec.TargetRef.UID != rr.Spec.TargetRef.UID {
			continue
		}
		switch {
		case closedUnmerged(w) && sameField(ctx, w, rr):
			return w, remediationv1alpha1.ReasonAnotherRequestClosed, nil
		case succeededInstead(ctx, w, rr):
			return w, remediationv1alpha1.ReasonAnotherRequestSucceeded, nil
		}
	}
	return nil, "", nil
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
// or else a holder rr waited behind whose pull request for the same field was closed without a merge
// (recordedHolderOf), reason AnotherRequestClosed, or one that ended Succeeded with a change that took its baseline
// away, reason AnotherRequestSucceeded (#719). The candidates for a merge are the other requests of rr's Incident
// and target in the cache. One the cache shows merged is taken as it is, since its phase never goes
// back to before the merge (the phases after Merged are merges too; a revert is read from the live target); one
// the cache shows in a phase that has not ended is read again from the API server, since the cache may not show
// its merge yet, while the limits, which read the API server, already saw the lock it released in the same
// write. A request the cache shows ended otherwise (Denied, Superseded, NeedsIntervention, ...) never reaches
// Merged and is skipped. Any read that fails is returned (the request is looked at again, and reads nothing from
// Git meanwhile).
func (r *RemediationRequestReconciler) endingRequestOf(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	*remediationv1alpha1.RemediationRequest, string, error) {
	if !supersedable(rr) {
		return nil, "", nil
	}
	list := &remediationv1alpha1.RemediationRequestList{}
	if err := r.List(ctx, list, client.InNamespace(rr.Namespace)); err != nil {
		return nil, "", fmt.Errorf("list RemediationRequests in %s: %w", rr.Namespace, err)
	}
	for i := range list.Items {
		w := &list.Items[i]
		if w.UID == rr.UID || w.Spec.IncidentRef.UID != rr.Spec.IncidentRef.UID ||
			w.Spec.TargetRef.UID != rr.Spec.TargetRef.UID {
			continue
		}
		if !slices.Contains(mergedPhases, w.Status.Phase) {
			if ended(w.Status.Phase) {
				continue
			}
			live := &remediationv1alpha1.RemediationRequest{}
			if err := r.reader().Get(ctx, client.ObjectKeyFromObject(w), live); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return nil, "", fmt.Errorf("get RemediationRequest %s/%s: %w", w.Namespace, w.Name, err)
			}
			w = live
		}
		if !mergesBefore(ctx, w, rr) {
			continue
		}
		switch inEffect, err := r.changeInEffect(ctx, w); {
		case err != nil:
			return nil, "", err
		case inEffect:
			return w.DeepCopy(), remediationv1alpha1.ReasonAnotherRequestMerged, nil
		}
	}
	return r.recordedHolderOf(ctx, rr)
}

// supersedeDuplicate ends rr Superseded because of w: PlanFresh False with reason (AnotherRequestMerged,
// AnotherRequestClosed or AnotherRequestSucceeded) and a message naming w and its pull request, Queued removed, no
// lock. Any other reason gets a message that names w only. The status write uses rr's resourceVersion, so a
// conflict (another write meanwhile) is returned and the request is looked at again.
func (r *RemediationRequestReconciler) supersedeDuplicate(ctx context.Context, rr, w *remediationv1alpha1.RemediationRequest,
	reason string) error {
	pr := "its pull request"
	if w.Status.PullRequest != nil {
		pr = fmt.Sprintf("pull request #%d", w.Status.PullRequest.Number)
	}
	var message string
	switch reason {
	case remediationv1alpha1.ReasonAnotherRequestMerged:
		message = fmt.Sprintf("RemediationRequest %s of the same Incident and target was merged (%s) "+
			"after this one was created; this one is not needed", w.Name, pr)
	case remediationv1alpha1.ReasonAnotherRequestClosed:
		message = fmt.Sprintf("This one waited behind RemediationRequest %s for the same target and field, whose %s "+
			"was closed without a merge; this one opens no pull request for that field. A new analysis of the "+
			"Incident proposes again if the symptom continues", w.Name, pr)
	case remediationv1alpha1.ReasonAnotherRequestSucceeded:
		message = fmt.Sprintf("This one waited behind RemediationRequest %s, which changed the same field from the "+
			"same previous value in the same direction; its %s was merged and the request ended Succeeded, so this "+
			"one's previous value is gone and its change is not needed. Its Incident follows that request's outcome",
			w.Name, pr)
	default:
		message = fmt.Sprintf("RemediationRequest %s for the same target ended this one", w.Name)
	}
	r.setPhase(rr, remediationv1alpha1.PhaseSuperseded)
	rr.Status.ObservedGeneration = rr.Generation
	// It waited in the queue and never took the lock: false keeps it out of the limits' lock list.
	release(rr)
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
// or a request rr waited behind had its pull request for the same target and field closed without a merge or
// ended Succeeded with rr's change (succeededInstead), and reports whether it did (or the error of the look-up or the write, with ended false).
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
