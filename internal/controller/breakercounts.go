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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// The breaker's watermark (design section 10.7, #390). The circuit breaker needs, of all the requests that ended,
// only the latest success, the failures after it and the probes that ended without a success. Each request
// carries status.breakerCounts, a selectable field, so the breaker lists only status.breakerCounts!=false and
// its reads stay bounded however long the history grows:
//
//   - the status write that ends a request sets it (setPhase, classifyBreaker): true for a success, a failure
//     or a probe that ended any other way, false for any other end;
//   - a request that ended before the field existed gets it once from its status (backfillBreaker);
//   - after a check of the limits read the requests, the executor sets it to false on the requests whose outcome
//     the breaker no longer needs, judged from that same read (staleOutcomes, clearBreakerCounts).
//
// A request without the field is read (a missing field is not false), so a request not yet classified is
// never left out (fail closed). The proof that an outcome no longer matters uses the same order as the breaker
// (lastPhaseTransitionTime, as written by the leader that ended each request) and only outcomes that this read
// showed, so clearing never changes a decision the breaker makes from the same requests, whatever the clocks of
// the leaders that wrote them: the cleared request is one the breaker would skip or whose time does not change
// its result. The writes are idempotent (only true is set to false), carry the resourceVersion that was read (a
// request that changed meanwhile is left as it is and judged again at the next check) and may fail or be cut
// short at any point: what is not cleared is only read once more.

// breakerClearsPerCheck is the most requests one check of the limits clears, so that the first checks after an
// upgrade, when every older outcome was just marked, do not spend long on writes; the rest are cleared by the
// next checks.
const breakerClearsPerCheck = limitsPageSize

// countsForBreaker reports whether the breaker may need the outcome of a request that ended: a success or a
// failure it counts (breakerOutcome), or a probe that ended in any other way, which opens the breaker again
// from its end.
func countsForBreaker(rr *remediationv1alpha1.RemediationRequest) bool {
	return breakerOutcome(rr) != outcomeNone || (rr.Status.BreakerProbe && ended(rr.Status.Phase))
}

// classifyBreaker sets status.breakerCounts in the status about to be written, from the request's phase: on a
// request that ended, whether the breaker may need its outcome. A request that has not ended is left as it is
// (unset on a new request, so the breaker's list reads it).
func classifyBreaker(rr *remediationv1alpha1.RemediationRequest) {
	if ended(rr.Status.Phase) {
		rr.Status.BreakerCounts = new(countsForBreaker(rr))
	}
}

// backfillBreaker writes status.breakerCounts once on a request that ended before the field existed, from its
// status (classifyBreaker), so the breaker's list (status.breakerCounts!=false) stops reading one it never
// needs, and the clearing after the next checks can judge the others.
func (r *RemediationRequestReconciler) backfillBreaker(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) error {
	if rr.Status.BreakerCounts != nil || !ended(rr.Status.Phase) {
		return nil
	}
	classifyBreaker(rr)
	if err := r.Status().Update(ctx, rr); err != nil {
		return fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
	}
	return nil
}

// staleOutcomes returns the requests among seen (one read of the limits) whose status.breakerCounts is true
// although the breaker no longer needs them, judged from seen alone with the breaker's own order
// (lastPhaseTransitionTime):
//
//   - a success older than the latest success (strictly): only the latest success bounds the failures in a row;
//   - a failure before the latest success (strictly): the failures in a row are counted after it;
//   - a failure with more than threshold failures after it, when it is not among the first threshold failures
//     after the latest success: the breaker stays tripped by the newer ones, the latest failure is a newer one,
//     and the trip (the threshold-th failure in a row) is among the first ones, which are kept;
//   - a probe that ended without a success or a failure, when any outcome or such a probe's end came after it
//     (strictly): the breaker opens again from the latest failure or probe end only, and a newer success leaves
//     no failure in a row before the probe.
//
// A request without lastPhaseTransitionTime cannot be ordered and is never returned, nor is anything else
// (fail closed). The latest success, the first threshold failures after it, the threshold+1 latest failures and
// the latest probe end are never returned, so once cleared the breaker reads at most 2*threshold+3 ended
// requests (more only on equal times) besides those not classified yet.
func staleOutcomes(seen map[types.UID]limitsEntry, threshold int) []limitsEntry {
	var lastSuccess, lastEvent time.Time
	var failures []time.Time
	for _, e := range seen {
		switch {
		case e.outcome == outcomeSuccess:
			lastSuccess = latest(lastSuccess, e.at)
		case e.outcome == outcomeFailure:
			failures = append(failures, e.at)
		case !e.neutralProbe():
			continue
		}
		lastEvent = latest(lastEvent, e.at)
	}
	var stale []limitsEntry
	for _, e := range seen {
		if !e.counts || e.at.IsZero() {
			continue
		}
		switch {
		case e.outcome == outcomeSuccess:
			if e.at.Before(lastSuccess) {
				stale = append(stale, e)
			}
		case e.outcome == outcomeFailure:
			if e.at.Before(lastSuccess) || failureSuperseded(e.at, lastSuccess, failures, threshold) {
				stale = append(stale, e)
			}
		case e.neutralProbe():
			if e.at.Before(lastEvent) {
				stale = append(stale, e)
			}
		}
	}
	return stale
}

// failureSuperseded reports whether a failure at at, not before lastSuccess, has more than threshold failures
// after it and at least threshold failures of the same row (not before lastSuccess) before it.
func failureSuperseded(at, lastSuccess time.Time, failures []time.Time, threshold int) bool {
	newer, olderInRow := 0, 0
	for _, t := range failures {
		switch {
		case t.After(at):
			newer++
		case t.Before(at) && !t.Before(lastSuccess):
			olderInRow++
		}
	}
	return newer > threshold && olderInRow >= threshold
}

// latest returns the later of a and b.
func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// clearBreakerCounts sets status.breakerCounts to false on at most breakerClearsPerCheck of stale, the requests
// staleOutcomes found in namespace, each with the resourceVersion that was read (a merge patch with that
// resourceVersion, so one that changed since is left as it is). It never fails the caller: a request that is
// gone or changed is skipped, and any other error stops the clearing and is logged, so those requests are
// read once more and judged again at the next check.
func (r *RemediationRequestReconciler) clearBreakerCounts(ctx context.Context, namespace string, stale []limitsEntry) {
	log := logf.FromContext(ctx)
	for i, e := range stale {
		if i == breakerClearsPerCheck {
			return
		}
		rr := &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: e.name,
			ResourceVersion: e.resourceVersion}}
		base := rr.DeepCopy()
		rr.Status.BreakerCounts = new(false)
		err := r.Status().Patch(ctx, rr, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		switch {
		case err == nil, apierrors.IsNotFound(err), apierrors.IsConflict(err):
		default:
			log.Info("Failed to mark a RemediationRequest whose outcome the circuit breaker no longer needs",
				"remediationRequest", client.ObjectKey{Namespace: namespace, Name: e.name}, "error", err.Error())
			return
		}
	}
}
