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
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// The breaker's watermark (design section 10.7, #390). The circuit breaker needs, of all the requests that ended,
// only the latest success and what came after it. Each request carries status.breakerCounts, a selectable
// field, so the breaker lists only status.breakerCounts!=false and its reads do not grow with the history
// before the latest final success:
//
//   - the status write that ends a request sets it (setPhase, classifyBreaker): true for a success, a failure
//     or a probe that ended any other way, false for any other end;
//   - a request that ended before the field existed gets it once from its status (backfill);
//   - after a check of the limits, the executor sets it to false on the outcomes that ended before the latest
//     request in Succeeded it read, and names that request in status.breakerClearedBy in the same write
//     (staleOutcomes, clearStale). Only Succeeded is final: Merged, WaitingForDelivery and Verifying may still
//     turn into Failed (#92), so they count as successes for the decision but clear nothing;
//   - when the request in Succeeded is deleted, its finalizer sets breakerCounts back to true on every request
//     that names it before the finalizer is removed (restoreCleared), so the outcomes it hid come back;
//   - a request being deleted whose finalizer may still close a pull request (NeedsIntervention, Expired) gets
//     true before that close (markDeleting), so the limits read it and count it against the cap until its
//     finalizer is gone.
//
// A request without the field is read (a missing field is not false), so a request not yet classified is
// never left out (fail closed). Clearing uses the breaker's own order (lastPhaseTransitionTime, as written by
// the leader that ended each request) and only requests this read showed: an outcome before the latest success
// is one the breaker skips (the failures in a row are counted after the latest success, an older success is not
// the latest, a probe that ended before a success opens nothing again), so clearing never changes a decision
// made from the same requests, whatever the clocks of the leaders. The writes carry the resourceVersion that was
// read (a request that changed meanwhile is left as it is and judged again at the next check) and may fail or
// be cut short at any point: what is not cleared is only read once more.
//
// clearMu orders the clearing and the restore of one executor: the clearer checks, under it, that the success
// it clears by still exists and is not being deleted, and the finalizer restores under it, so a clearing cannot
// name a success whose finalizer already restored. A clearing by a former leader that still runs while the new
// one deletes that success is not covered: leader election stops the former leader's process.

// breakerClearsPerCheck is the most requests one clearing sets to false. The first checks after the upgrade
// may find the whole older history before the latest final success; a hundred merge patches take about a
// second on a local API server, so the clearing stays short and the rest is left to the next checks, which
// read it once more meanwhile.
const breakerClearsPerCheck = 100

// backfillRetryDelay is how long a request whose backfill write conflicted waits before it is reconciled again.
const backfillRetryDelay = time.Second

// countsForBreaker reports whether the breaker may need the outcome of a request that ended: a success or a
// failure it counts (breakerOutcome), or a probe that ended in any other way, which opens the breaker again
// from its end.
func countsForBreaker(rr *remediationv1alpha1.RemediationRequest) bool {
	return breakerOutcome(rr) != outcomeNone || (rr.Status.BreakerProbe && ended(rr.Status.Phase))
}

// classifyBreaker sets status.breakerCounts in the status about to be written, from the request's phase: on a
// request that ended, whether the breaker may need its outcome, and no clearing success (it ended now, after
// any success that cleared it before). A request that has not ended is left as it is (unset on a new request,
// so the breaker's list reads it).
func classifyBreaker(rr *remediationv1alpha1.RemediationRequest) {
	if ended(rr.Status.Phase) {
		rr.Status.BreakerCounts = new(countsForBreaker(rr))
		rr.Status.BreakerClearedBy = ""
	}
}

// backfill writes, in one status write, the fields a request decided before they existed lacks:
// status.holdsTarget from derivedHold (so the requests of an older release keep the lock their status gives
// them; one refused then gets false, so the lock's list stops reading it), and on a request that ended,
// status.breakerCounts from classifyBreaker (so the breaker's list stops reading one it never needs).
func (r *RemediationRequestReconciler) backfill(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	changed := false
	if rr.Status.HoldsTarget == nil && (rr.Status.PlanDigest != "" || ended(rr.Status.Phase)) {
		rr.Status.HoldsTarget = new(derivedHold(rr))
		changed = true
	}
	if rr.Status.BreakerCounts == nil && ended(rr.Status.Phase) {
		classifyBreaker(rr)
		changed = true
	}
	if !changed {
		return nil
	}
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	return nil
}

// clearing is what one check of the limits found to clear: the requests whose outcome the breaker no longer
// needs, oldest first, and the request in Succeeded that proves it.
type clearing struct {
	by    string
	byUID types.UID
	stale []limitsEntry
}

// staleOutcomes finds, among seen (one read of the limits), the latest request in Succeeded that is not being
// deleted, and the requests with status.breakerCounts true that ended strictly before it with an outcome the
// breaker reads (a success, a failure, or a probe that ended without either), oldest first. A request without
// lastPhaseTransitionTime cannot be ordered and is never returned; nor is one being deleted (it may be read to
// count it against the cap, markDeleting). Without a request in Succeeded nothing is returned, so a long row of
// failures and probes stays read (fail closed).
func staleOutcomes(seen map[types.UID]limitsEntry) clearing {
	var c clearing
	var last limitsEntry
	for _, e := range seen {
		if !e.final || e.deleting || e.at.IsZero() {
			continue
		}
		if c.by == "" || e.at.After(last.at) || (e.at.Equal(last.at) && e.name < last.name) {
			last, c.by, c.byUID = e, e.name, e.uid
		}
	}
	if c.by == "" {
		return c
	}
	for _, e := range seen {
		if e.counts && !e.deleting && !e.at.IsZero() && e.at.Before(last.at) &&
			(e.outcome != outcomeNone || e.neutralProbe()) {
			c.stale = append(c.stale, e)
		}
	}
	slices.SortFunc(c.stale, func(a, b limitsEntry) int {
		return cmp.Or(a.at.Compare(b.at), cmp.Compare(a.name, b.name))
	})
	return c
}

// clearStale sets status.breakerCounts to false and status.breakerClearedBy to the clearing success on at most
// breakerClearsPerCheck of c.stale in namespace, oldest first, each with the resourceVersion that was read (a
// merge patch with it, so a request that changed since is left as it is). It runs after the decision of the
// check that found them, outside the admission lock, and only one clearing runs at a time (another check finds
// the rest later). Under clearMu it first checks that the success still exists, with the same UID, in
// Succeeded and not being deleted, so its finalizer, which restores under the same mutex, finds every request
// that names it. It never fails the caller: a request that is gone or changed is skipped; any other error stops
// the clearing, and the requests not cleared are read again at the next check. An error that will not go away
// by itself (Forbidden, Invalid and the like) is logged as an error and counted in
// shadewright_circuit_breaker_clear_errors_total; a transient one is logged at info level.
func (r *RemediationRequestReconciler) clearStale(ctx context.Context, namespace string, c clearing) {
	if len(c.stale) == 0 || !r.clearMu.TryLock() {
		return
	}
	defer r.clearMu.Unlock()
	ctx, cancel := bound(ctx, r.CheckTimeout, DefaultCheckTimeout)
	defer cancel()
	log := logf.FromContext(ctx)
	by := &remediationv1alpha1.RemediationRequest{}
	switch err := r.reader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: c.by}, by); {
	case apierrors.IsNotFound(err):
		return
	case err != nil:
		clearFailed(ctx, err, c.by)
		return
	}
	if by.UID != c.byUID || !by.DeletionTimestamp.IsZero() || by.Status.Phase != remediationv1alpha1.PhaseSucceeded {
		return
	}
	for i, e := range c.stale {
		if i == breakerClearsPerCheck {
			return
		}
		rr := &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: e.name,
			ResourceVersion: e.resourceVersion}}
		base := rr.DeepCopy()
		rr.Status.BreakerCounts = new(false)
		rr.Status.BreakerClearedBy = c.by
		err := r.Status().Patch(ctx, rr, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		switch {
		case err == nil, apierrors.IsNotFound(err), apierrors.IsConflict(err):
		case ctx.Err() != nil:
			log.Info("Stopped marking the outcomes the circuit breaker no longer needs", "reason", ctx.Err().Error())
			return
		default:
			clearFailed(ctx, err, e.name)
			return
		}
	}
}

// permanentClearError reports whether a write of the watermark failed in a way that will not go away by itself:
// the executor may not write (a role changed), or the API server refuses the object or the request.
func permanentClearError(err error) bool {
	return apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsInvalid(err) ||
		apierrors.IsBadRequest(err) || apierrors.IsMethodNotSupported(err) || apierrors.IsNotAcceptable(err) ||
		apierrors.IsUnsupportedMediaType(err)
}

// clearFailed logs a failed read or write of the clearing, as an error and counted when it is permanent.
func clearFailed(ctx context.Context, err error, name string) {
	log := logf.FromContext(ctx).WithValues("remediationRequest", name)
	if permanentClearError(err) {
		breakerClearErrors.Inc()
		log.Error(err, "Failed to mark the outcomes the circuit breaker no longer needs; they stay read")
		return
	}
	log.Info("Failed to mark the outcomes the circuit breaker no longer needs; they stay read", "error", err.Error())
}

// markDeleting sets status.breakerCounts true, in the status about to be written, on a request being deleted
// that ended in a phase whose deletion may close a pull request (closeOnDelete: NeedsIntervention, Expired), so
// the limits read it, and count it against the cap (active), until its finalizer is gone. It reports whether it
// changed the status.
func markDeleting(rr *remediationv1alpha1.RemediationRequest) bool {
	if !ended(rr.Status.Phase) || !closeOnDelete(rr.Status.Phase) || ptr.Deref(rr.Status.BreakerCounts, false) {
		return false
	}
	rr.Status.BreakerCounts = new(true)
	rr.Status.BreakerClearedBy = ""
	return true
}

// ReasonBreakerRestoreRefused is the reason of the Warning Event on a request in deletion whose finalizer
// cannot restore the outcomes it cleared because the API server refuses for good.
const ReasonBreakerRestoreRefused = "BreakerRestoreRefused"

// restoreRetryDelay is how long a finalizer whose restore was refused for good waits before it tries again.
const restoreRetryDelay = 5 * time.Minute

// restoreRefused handles a restore the API server refused for good (Forbidden, Invalid and the like): the
// finalizer stays (fail closed), the refusal is logged as an error, counted in
// shadewright_circuit_breaker_clear_errors_total and recorded as a Warning Event on the request, and the
// deletion is tried again after restoreRetryDelay rather than at once.
func (r *RemediationRequestReconciler) restoreRefused(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	err error) reconcile.Result {
	breakerClearErrors.Inc()
	logf.FromContext(ctx).Error(err, "Failed to restore the outcomes the circuit breaker reads again after a success is "+
		"deleted; the finalizer stays", "remediationRequest", client.ObjectKeyFromObject(rr))
	if r.Recorder != nil {
		r.Recorder.Event(rr, corev1.EventTypeWarning, ReasonBreakerRestoreRefused,
			"The outcomes this request's success hid from the circuit breaker could not be restored; the finalizer "+
				"stays until they are (see the troubleshooting guide, limits)")
	}
	return reconcile.Result{RequeueAfter: restoreRetryDelay}
}

// clearedField is the field selector of status.breakerClearedBy.
const clearedField = "status.breakerClearedBy"

// restoreCleared sets status.breakerCounts back to true, and removes status.breakerClearedBy, on every request
// in rr's namespace that names rr as the success that cleared it, read from the API server in pages. The
// finalizer calls it before it removes itself, under clearMu, so no clearing names rr after the list. Each
// write is a merge patch with the resourceVersion of the list; any error but a request that is gone is
// returned, and the finalizer stays until a retry restores them all (fail closed). Without an API server reader
// it fails.
func (r *RemediationRequestReconciler) restoreCleared(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	if r.APIReader == nil {
		return errors.New("cannot restore the outcomes cleared by this request: no API server reader")
	}
	r.clearMu.Lock()
	defer r.clearMu.Unlock()
	parent := ctx
	ctx, cancel := bound(ctx, r.CleanupTimeout, DefaultCleanupTimeout)
	defer cancel()
	err := r.restoreClearedIn(ctx, rr)
	return boundErr(parent, ctx, "restore of the circuit breaker's outcomes", err)
}

// restoreClearedIn is restoreCleared within ctx.
func (r *RemediationRequestReconciler) restoreClearedIn(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) error {
	type cleared struct{ name, resourceVersion string }
	var found []cleared
	if err := r.listPages(ctx, rr.Namespace, fields.OneTermEqualSelector(clearedField, rr.Name),
		func(o *remediationv1alpha1.RemediationRequest) {
			found = append(found, cleared{o.Name, o.ResourceVersion})
		}); err != nil {
		return err
	}
	for _, f := range found {
		o := &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: rr.Namespace, Name: f.name,
			ResourceVersion: f.resourceVersion}}
		o.Status.BreakerCounts = new(false)
		o.Status.BreakerClearedBy = rr.Name
		base := o.DeepCopy()
		o.Status.BreakerCounts = new(true)
		o.Status.BreakerClearedBy = ""
		err := r.Status().Patch(ctx, o, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("restore the circuit breaker's outcome on RemediationRequest %s/%s: %w", rr.Namespace,
				f.name, err)
		}
	}
	if len(found) > 0 {
		logf.FromContext(ctx).Info("Restored the outcomes the circuit breaker reads again after a success is deleted",
			"remediationRequest", client.ObjectKeyFromObject(rr), "restored", len(found))
	}
	return nil
}
