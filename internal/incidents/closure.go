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

package incidents

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// QuietBeforeResolved is how long an Incident in phase Open, Proposed or Escalated must go quiet, with no
// active request naming it, before the analyzer moves it to Resolved (design 3.2, #251). Quiet means nothing
// happened to it in that time: no observation (written or held), no analysis and no lifecycle move.
const QuietBeforeResolved = time.Hour

// ConditionLifecycle is the Incident condition that says why the closure rules last moved the Incident's
// phase: to Resolved (quiet, a request that Succeeded, or HolderSucceeded: a request that waited behind another
// request for the same field which Succeeded) or to Escalated (a request that ended without success). It is
// always True; its lastTransitionTime is when that move was written. A move by another rule (an analysis
// outcome, a recurrence) leaves it as it was.
const ConditionLifecycle = "Lifecycle"

// Reasons of the Lifecycle condition.
const (
	// ReasonSymptomQuiet: nothing happened to the Incident for QuietBeforeResolved and no request naming it is
	// active, so it moved to Resolved.
	ReasonSymptomQuiet = "SymptomQuiet"
	// ReasonRequestSucceeded: the newest request naming the Incident reached Succeeded (merged and verified),
	// so it moved to Resolved at once.
	ReasonRequestSucceeded = "RequestSucceeded"
	// ReasonRequestDenied, ReasonRequestExpired, ReasonRequestFailed, ReasonRequestCancelled,
	// ReasonRequestNeedsIntervention, ReasonRequestSuperseded and ReasonRequestInconclusive: the newest request
	// naming the Incident ended
	// in that phase, so it moved to Escalated for a person. A Superseded request is the newest only when no
	// newer request names the Incident.
	ReasonRequestDenied            = "RequestDenied"
	ReasonRequestExpired           = "RequestExpired"
	ReasonRequestFailed            = "RequestFailed"
	ReasonRequestCancelled         = "RequestCancelled"
	ReasonRequestNeedsIntervention = "RequestNeedsIntervention"
	ReasonRequestSuperseded        = "RequestSuperseded"
	ReasonRequestInconclusive      = "RequestInconclusive"
	// ReasonHolderSucceeded: the newest request naming the Incident ended Superseded with reason
	// AnotherRequestSucceeded (#719): it waited behind another request for the same field, of any Incident, whose
	// change took its baseline away and ended Succeeded. The Incident follows that success and moves to Resolved at
	// once, as for ReasonRequestSucceeded.
	ReasonHolderSucceeded = "HolderSucceeded"
	// ReasonReopened: the Incident was Resolved by quiet time before any analysis, and its symptom came back
	// within RecurrenceWindow of resolvedTime, so it is Open again, with no resolvedTime, and is analyzed as
	// usual ([reopens]). It is the one Lifecycle reason the observation writer sets.
	ReasonReopened = "Reopened"
	// ReasonRequestActive: the Incident was Resolved while a request naming it was active (one created after
	// the closure rules listed the requests), so it moved back to Proposed, with no resolvedTime, and the
	// request decides it as usual.
	ReasonRequestActive = "RequestActive"
)

// lifecycleFieldOwner is the field manager of the closure rules' status writes, so the managed fields tell
// them from the observation and the trigger writes.
const lifecycleFieldOwner = "shadewright-incident-lifecycle"

// closureWritesPerPass bounds the lifecycle writes of one trigger pass, so a backlog (the first pass after
// an upgrade) neither runs the pass into its deadline nor holds up the AnalysisDue marks; the rest are
// written over the next passes.
const closureWritesPerPass = 10

// endedWithoutSuccess maps each final request phase other than Succeeded to its Lifecycle reason.
var endedWithoutSuccess = map[remediationv1alpha1.RemediationPhase]string{
	remediationv1alpha1.PhaseDenied:            ReasonRequestDenied,
	remediationv1alpha1.PhaseExpired:           ReasonRequestExpired,
	remediationv1alpha1.PhaseFailed:            ReasonRequestFailed,
	remediationv1alpha1.PhaseCancelled:         ReasonRequestCancelled,
	remediationv1alpha1.PhaseNeedsIntervention: ReasonRequestNeedsIntervention,
	remediationv1alpha1.PhaseSuperseded:        ReasonRequestSuperseded,
	remediationv1alpha1.PhaseInconclusive:      ReasonRequestInconclusive,
}

// lifecycleMessages are the fixed messages of the Lifecycle condition. They hold nothing from the cluster.
var lifecycleMessages = map[string]string{
	ReasonSymptomQuiet:             "Nothing was observed for an hour and no request naming the Incident is active",
	ReasonRequestSucceeded:         "The newest request naming the Incident succeeded",
	ReasonHolderSucceeded:          "The newest request naming the Incident waited behind another request for the same field, which succeeded",
	ReasonRequestDenied:            "The newest request naming the Incident was denied; a person decides",
	ReasonRequestExpired:           "The newest request naming the Incident expired; a person decides",
	ReasonRequestFailed:            "The newest request naming the Incident failed; a person decides",
	ReasonRequestCancelled:         "The newest request naming the Incident was cancelled; a person decides",
	ReasonRequestNeedsIntervention: "The newest request naming the Incident needs intervention; a person decides",
	ReasonRequestSuperseded:        "The newest request naming the Incident was superseded and no newer one names it; a person decides",
	ReasonRequestInconclusive:      "The newest request naming the Incident was merged, and its effect could not be shown; a person decides",
	ReasonReopened:                 "The symptom came back within 24 hours of a quiet resolution before any analysis; the Incident is Open again",
	ReasonRequestActive:            "A request naming the Incident is active; it is Proposed again until the request ends",
}

// finalRequest reports whether a request phase is final for the closure rules (design 3.2): Succeeded, one
// that ended without success, or Merged. Merged is the end of a request when the executor verifies no outcomes
// (--verify-outcomes off); with verification on it is a short step before WaitingForDelivery. Either way it no
// longer keeps the Incident out of the quiet rule, but it moves nothing by itself. Every other phase, no
// phase yet and a phase this version does not know included, is active, so an unknown phase never resolves
// an Incident.
func finalRequest(p remediationv1alpha1.RemediationPhase) bool {
	_, ended := endedWithoutSuccess[p]
	return ended || p == remediationv1alpha1.PhaseSucceeded || p == remediationv1alpha1.PhaseMerged
}

// decidedByRequest reports whether the newest request naming the Incident may move it: in phase Proposed,
// and in phase Escalated when a Superseded request escalated it (ReasonRequestSuperseded), so the outcome of
// a replacement that only appeared after that escalation still applies.
func decidedByRequest(inc *remediationv1alpha1.Incident) bool {
	switch inc.Status.Phase {
	case remediationv1alpha1.IncidentProposed:
		return true
	case remediationv1alpha1.IncidentEscalated:
		return lifecycleReason(inc) == ReasonRequestSuperseded
	}
	return false
}

// lifecycleReason returns the reason of the Incident's Lifecycle condition, or "" without one.
func lifecycleReason(inc *remediationv1alpha1.Incident) string {
	if cond := meta.FindStatusCondition(inc.Status.Conditions, ConditionLifecycle); cond != nil {
		return cond.Reason
	}
	return ""
}

// Closure decides what the closure rules do to an Incident at now (design 3.2, #251): the phase it moves to
// and the Lifecycle reason, or ok false when it stays. requests are the RemediationRequests of the operator
// namespace, a complete listing (the caller fails closed when it cannot read one); only those whose
// incidentRef names this Incident, by name and UID, count. held is the newest observation of the Incident the
// controller holds unwritten, or zero. Closure reads only its arguments.
//
//   - Only an Incident whose name is derived from its spec, with an observation, in phase Open, Proposed,
//     Escalated or Resolved is decided. Analyzing and no phase stay.
//   - A Resolved Incident moves only for a request the closure rules that resolved it cannot have listed (a
//     listing is no lock), so the next pass undoes that resolution from what the API server holds, also after
//     a restart: an active request naming it moves it back to Proposed (ReasonRequestActive); otherwise, when
//     the newest request naming it was created at or after resolvedTime, it applies the request rules of
//     phase Proposed (ended without success escalates; Succeeded, also through the request it waited behind,
//     leaves it Resolved; Merged moves nothing). A final request created before resolvedTime never moves it.
//   - Any active request naming it keeps it where it is ([finalRequest]).
//   - In phase Proposed (or Escalated by a Superseded request, [decidedByRequest]), the newest request naming
//     the Incident (by creation time; at the same time a Superseded one is the older, then by name) decides:
//     Succeeded moves it to Resolved, any other final phase but Merged to Escalated. Superseded thus follows
//     the newer request when there is one, and escalates when there is none. A request Superseded because
//     another one was merged ([mergedElsewhere], reason AnotherRequestMerged) is never the newest: the merged
//     request decides. A request Superseded because the request it waited behind for the same field succeeded
//     ([succeededElsewhere], reason AnotherRequestSucceeded, #719) follows that success: Resolved with
//     ReasonHolderSucceeded, never Escalated. A lone Merged request (the end of a request with --verify-outcomes
//     off, or the step before WaitingForDelivery with it on) moves nothing: it is not active, so the quiet rule
//     applies. WaitingForDelivery and Verifying are active and keep the Incident where it is.
//   - Otherwise an Incident quiet for QuietBeforeResolved moves to Resolved. It is quiet since the latest of lastSeen, held, lastAnalysisTime and the Lifecycle
//     condition's lastTransitionTime, so an escalation stands for at least that long before the Incident can
//     go on to Resolved. An Incident whose newest occurrence is a lasting symptom state ([lastingState]) is
//     never quiet: such a state is emitted once while it lasts, so its lastSeen cannot tell that it ended.
func Closure(inc *remediationv1alpha1.Incident, requests []remediationv1alpha1.RemediationRequest, held,
	now time.Time) (remediationv1alpha1.IncidentPhase, string, bool) {
	st := &inc.Status
	if !inc.HasDerivedName() || st.LastSeen == nil {
		return "", "", false
	}
	switch st.Phase {
	case remediationv1alpha1.IncidentOpen, remediationv1alpha1.IncidentProposed, remediationv1alpha1.IncidentEscalated,
		remediationv1alpha1.IncidentResolved:
	default:
		return "", "", false
	}
	var newest *remediationv1alpha1.RemediationRequest
	active := false
	for i := range requests {
		rr := &requests[i]
		if rr.Spec.IncidentRef.Name != inc.Name || types.UID(rr.Spec.IncidentRef.UID) != inc.UID {
			continue
		}
		if !finalRequest(rr.Status.Phase) {
			active = true
		}
		if mergedElsewhere(rr) {
			// Ended because another request of the Incident was merged (#556): that request decides, never this one.
			continue
		}
		if newest == nil || newer(rr, newest) {
			newest = rr
		}
	}
	if st.Phase == remediationv1alpha1.IncidentResolved {
		if active {
			return remediationv1alpha1.IncidentProposed, ReasonRequestActive, true
		}
		if newest == nil || !createdSinceResolved(newest, st.ResolvedTime) {
			return "", "", false
		}
		if phase, reason, ok := requestOutcome(newest, requests); ok &&
			phase == remediationv1alpha1.IncidentEscalated {
			return phase, reason, true
		}
		// Succeeded, also through the request it waited behind (succeededElsewhere), leaves it Resolved; Merged
		// moves nothing.
		return "", "", false
	}
	if active {
		return "", "", false
	}
	if newest != nil && decidedByRequest(inc) {
		if phase, reason, ok := requestOutcome(newest, requests); ok {
			if phase == remediationv1alpha1.IncidentEscalated && st.Phase == remediationv1alpha1.IncidentEscalated &&
				lifecycleReason(inc) == reason {
				// Already escalated for this outcome; only the quiet rule moves it on.
				return closeQuiet(inc, held, now)
			}
			return phase, reason, true
		}
	}
	return closeQuiet(inc, held, now)
}

// mergedElsewhere reports whether rr ended Superseded because another request of the same Incident and target
// was merged (PlanFresh False with reason AnotherRequestMerged, #556). Such a request opened no pull request and
// says nothing about the Incident: the merged request does, so it is never the newest one that decides.
func mergedElsewhere(rr *remediationv1alpha1.RemediationRequest) bool {
	if rr.Status.Phase != remediationv1alpha1.PhaseSuperseded {
		return false
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
	return c != nil && c.Status == metav1.ConditionFalse && c.Reason == remediationv1alpha1.ReasonAnotherRequestMerged
}

// requestOutcome returns where the request newest moves its Incident and the Lifecycle reason: Resolved with
// ReasonRequestSucceeded for Succeeded, Resolved with ReasonHolderSucceeded when it ended because the request it
// waited behind succeeded (succeededElsewhere), Escalated with the phase's reason for any other end without
// success, and ok false for Merged or a phase that has not ended. The one helper keeps the order of these checks
// the same for every phase of the Incident.
func requestOutcome(newest *remediationv1alpha1.RemediationRequest, requests []remediationv1alpha1.RemediationRequest) (
	remediationv1alpha1.IncidentPhase, string, bool) {
	switch {
	case newest.Status.Phase == remediationv1alpha1.PhaseSucceeded:
		return remediationv1alpha1.IncidentResolved, ReasonRequestSucceeded, true
	case succeededElsewhere(newest, requests):
		// Superseded by the success of the request it waited behind: the Incident follows that outcome.
		return remediationv1alpha1.IncidentResolved, ReasonHolderSucceeded, true
	}
	if reason, ended := endedWithoutSuccess[newest.Status.Phase]; ended {
		return remediationv1alpha1.IncidentEscalated, reason, true
	}
	return "", "", false
}

// succeededElsewhere reports whether rr ended Superseded because the request it waited behind for the same field
// ended Succeeded with a change that took its baseline away (PlanFresh False with reason AnotherRequestSucceeded,
// #719). The executor writes that reason only after it read the holder Succeeded, a final phase. The record is
// checked too: rr must name the requests it waited behind (status.queuedBehind), and each of them that requests
// (the listing) still holds, by name and UID, must be Succeeded. A named request gone from the listing (deleted
// since) is accepted, since the reason was written from its Succeeded phase.
func succeededElsewhere(rr *remediationv1alpha1.RemediationRequest, requests []remediationv1alpha1.RemediationRequest) bool {
	if rr.Status.Phase != remediationv1alpha1.PhaseSuperseded || len(rr.Status.QueuedBehind) == 0 {
		return false
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != remediationv1alpha1.ReasonAnotherRequestSucceeded {
		return false
	}
	for _, ref := range rr.Status.QueuedBehind {
		for i := range requests {
			h := &requests[i]
			if h.Name == ref.Name && h.UID == types.UID(ref.UID) && h.Status.Phase != remediationv1alpha1.PhaseSucceeded {
				return false
			}
		}
	}
	return true
}

// closeQuiet is the quiet rule of [Closure], for an Incident no active request names.
func closeQuiet(inc *remediationv1alpha1.Incident, held, now time.Time) (remediationv1alpha1.IncidentPhase, string, bool) {
	if lastingState(&inc.Status) || now.Before(quietSince(inc, held).Add(QuietBeforeResolved)) {
		return "", "", false
	}
	return remediationv1alpha1.IncidentResolved, ReasonSymptomQuiet, true
}

// ResolvedClockSkew is how far before resolvedTime a request's creation may be stamped and still count as
// created after the resolve. resolvedTime comes from the analyzer's clock, creationTimestamp from the API
// server's, and the two may differ by a few seconds. A request created that close before a quiet resolve
// cannot have been seen by it: an analysis comes before each request, and the quiet rule waits an hour after
// one.
const ResolvedClockSkew = 30 * time.Second

// createdSinceResolved reports whether request rr was created at or after resolvedTime, less
// [ResolvedClockSkew], so the closure rules that resolved the Incident may have decided on a listing made
// before it existed. A missing resolvedTime tells nothing, so no request counts.
func createdSinceResolved(rr *remediationv1alpha1.RemediationRequest, resolvedTime *metav1.Time) bool {
	return resolvedTime != nil && !resolvedTime.IsZero() &&
		!rr.CreationTimestamp.Time.Before(resolvedTime.Add(-ResolvedClockSkew))
}

// newer reports whether request a is newer than b: created later; at the same time, b is Superseded and a is
// not (a superseded request is replaced by a newer one); then by name.
func newer(a, b *remediationv1alpha1.RemediationRequest) bool {
	if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
		return c > 0
	}
	as, bs := a.Status.Phase == remediationv1alpha1.PhaseSuperseded, b.Status.Phase == remediationv1alpha1.PhaseSuperseded
	if as != bs {
		return bs
	}
	return a.Name > b.Name
}

// quietSince returns when the last thing happened to the Incident that the quiet rule counts: the latest of
// lastSeen, held (an observation not written yet), lastAnalysisTime and the Lifecycle condition's
// lastTransitionTime.
func quietSince(inc *remediationv1alpha1.Incident, held time.Time) time.Time {
	st := &inc.Status
	t := latest(st.LastSeen.Time, held)
	if st.LastAnalysisTime != nil {
		t = latest(t, st.LastAnalysisTime.Time)
	}
	if cond := meta.FindStatusCondition(st.Conditions, ConditionLifecycle); cond != nil {
		t = latest(t, cond.LastTransitionTime.Time)
	}
	return t
}

// applyClosure moves the Incident to phase with the Lifecycle condition saying why, at now. Resolved sets
// resolvedTime in the same update; a move out of Resolved clears it. The condition is removed first, so its
// lastTransitionTime is always that of this move ([quietSince] reads it).
func applyClosure(inc *remediationv1alpha1.Incident, phase remediationv1alpha1.IncidentPhase, reason string, now time.Time) {
	at := metav1.NewTime(now.UTC().Truncate(time.Second))
	switch {
	case phase == remediationv1alpha1.IncidentResolved:
		inc.Status.ResolvedTime = &at
	case inc.Status.Phase == remediationv1alpha1.IncidentResolved:
		inc.Status.ResolvedTime = nil
	}
	inc.Status.Phase = phase
	meta.RemoveStatusCondition(&inc.Status.Conditions, ConditionLifecycle)
	meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{
		Type: ConditionLifecycle, Status: metav1.ConditionTrue, Reason: reason, Message: lifecycleMessages[reason],
		ObservedGeneration: inc.Generation, LastTransitionTime: at,
	})
}

// reopens reports whether a recurrence within RecurrenceWindow moves a Resolved Incident back to Open instead
// of Escalated (design 3.2, #466): the closure rules resolved it by quiet time (Lifecycle SymptomQuiet), with
// a known resolvedTime, and it was never analyzed (no lastAnalysisTime, no request recorded). An Incident that
// was analyzed, or resolved any other way, keeps the escalation.
func reopens(st *remediationv1alpha1.IncidentStatus) bool {
	cond := meta.FindStatusCondition(st.Conditions, ConditionLifecycle)
	return cond != nil && cond.Reason == ReasonSymptomQuiet && st.ResolvedTime != nil && !st.ResolvedTime.IsZero() &&
		st.LastAnalysisTime == nil && len(st.RemediationRequests) == 0
}

// reopen moves a Resolved Incident whose symptom came back before any analysis to Open: the same Incident, no
// new occurrence, resolvedTime cleared, and the Lifecycle condition Reopened. The trigger then decides it as
// any Incident never analyzed.
func reopen(inc *remediationv1alpha1.Incident, now time.Time) {
	applyClosure(inc, remediationv1alpha1.IncidentOpen, ReasonReopened, now)
}

// closeIncidents applies the closure rules ([Closure]) to every Incident in the namespace, once per trigger
// pass. It decides first on the cached Incidents and requests; for each Incident that would move, it decides
// again on the Incident read fresh from the API server and the requests listed from it on that attempt
// ([Controller.closeOne]), and writes only that decision, with optimistic concurrency. A move is one status
// update (phase, resolvedTime and the Lifecycle condition together), so a restart in the middle finds the
// Incident either moved or not, and decides it again. At most closureWritesPerPass moves are written per pass;
// a failed write does not count, and the Incidents whose last write failed are tried after the others, so a
// few that keep failing never hold up the rest.
//
// It fails closed: when the Incidents or the requests cannot be listed, nothing moves and the error is
// returned; an Incident that cannot be read or written keeps its phase. The errors returned name the
// Incidents.
func (c *Controller) closeIncidents(ctx context.Context) error {
	var list remediationv1alpha1.IncidentList
	if err := c.cache.List(ctx, &list, client.InNamespace(c.cfg.Namespace)); err != nil {
		return fmt.Errorf("listing the Incidents for the closure rules: %w", err)
	}
	var cached remediationv1alpha1.RemediationRequestList
	if err := c.cache.List(ctx, &cached, client.InNamespace(c.cfg.Namespace)); err != nil {
		return fmt.Errorf("listing the RemediationRequests for the closure rules: %w", err)
	}
	// The decision uses the time the move records (resolvedTime and lastTransitionTime are whole seconds), so
	// every observation the quiet rule counted was seen no later than resolvedTime minus QuietBeforeResolved,
	// which is what [onResolved] relies on.
	now := c.cfg.Now().UTC().Truncate(time.Second)
	listed := make(map[string]struct{}, len(list.Items))
	var first, last []string
	for i := range list.Items {
		inc := &list.Items[i]
		listed[inc.Name] = struct{}{}
		if _, _, ok := Closure(inc, cached.Items, c.unwrittenSince(inc.Spec.Fingerprint), now); !ok {
			continue
		}
		if _, failed := c.closeFailed[inc.Name]; failed {
			last = append(last, inc.Name)
		} else {
			first = append(first, inc.Name)
		}
	}
	maps.DeleteFunc(c.closeFailed, func(name string, _ struct{}) bool {
		_, ok := listed[name]
		return !ok
	})
	writes := 0
	var errs []error
	for _, name := range append(first, last...) {
		if writes == closureWritesPerPass {
			break
		}
		wrote, err := c.closeOne(ctx, name, now)
		if err != nil {
			c.closeFailed[name] = struct{}{}
			errs = append(errs, err)
			if ctx.Err() != nil {
				break
			}
			continue
		}
		delete(c.closeFailed, name)
		if wrote {
			writes++
		}
	}
	return errors.Join(errs...)
}

// closeOne reads the Incident name and the RemediationRequests from the API server, decides the closure
// rules on them and writes the move, retrying on conflict; each attempt reads both again, so a request
// created meanwhile counts. It reports whether it wrote one (an answer received, not lost). An Incident gone
// meanwhile is no error.
func (c *Controller) closeOne(ctx context.Context, name string, now time.Time) (bool, error) {
	wctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
	defer cancel()
	var moved *remediationv1alpha1.Incident
	var reason string
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		moved = nil
		inc := &remediationv1alpha1.Incident{}
		if err := c.reader.Get(wctx, types.NamespacedName{Namespace: c.cfg.Namespace, Name: name}, inc); err != nil {
			return err
		}
		var requests remediationv1alpha1.RemediationRequestList
		if err := c.reader.List(wctx, &requests, client.InNamespace(c.cfg.Namespace)); err != nil {
			return fmt.Errorf("listing the RemediationRequests: %w", err)
		}
		phase, r, ok := Closure(inc, requests.Items, c.unwrittenSince(inc.Spec.Fingerprint), now)
		if !ok {
			return nil
		}
		applyClosure(inc, phase, r, now)
		if err := c.client.Status().Update(wctx, inc, client.FieldOwner(lifecycleFieldOwner)); err != nil {
			return err
		}
		moved, reason = inc, r
		return nil
	})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("applying the closure rules to Incident %s: %w", name, err)
	}
	if moved == nil {
		return false, nil
	}
	c.mu.Lock()
	c.noteState(moved)
	c.mu.Unlock()
	logf.FromContext(ctx).Info("Moved an Incident by the closure rules", "incident", name,
		"phase", moved.Status.Phase, "reason", reason)
	return true, nil
}

// unwrittenSince returns the newest observation of the Incident of fp that is not in its status yet: held in a
// pending or failed batch, or in a write in flight, read under one lock so a batch moving between them is
// seen in one or the other. The closure rules read it, so an observation being written keeps the Incident
// from going quiet. A snapshot can still miss an observation consumed after it; [onResolved] makes such an
// observation a recurrence, never one from before the resolution.
func (c *Controller) unwrittenSince(fp string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.heldLocked(fp)
	if f := c.inflight[fp]; f != nil {
		t = latest(t, f.last)
	}
	return t
}

// takeOff records batches, just taken out of pending and failed by flush, as in flight for fp. The caller
// holds mu.
func (c *Controller) takeOff(fp string, batches []*batch) {
	f := c.inflight[fp]
	if f == nil {
		f = &flight{}
		c.inflight[fp] = f
	}
	f.n++
	for _, b := range batches {
		f.last = latest(f.last, b.last)
	}
}

// landed records that one write of fp in flight returned, whatever its outcome: its batches are back in
// pending or failed, dropped, or stored. The caller holds mu.
func (c *Controller) landed(fp string) {
	if f := c.inflight[fp]; f != nil {
		if f.n--; f.n <= 0 {
			delete(c.inflight, fp)
		}
	}
}

// capNearPercent is the share of the Incident cap at which the analyzer warns that it is near (design 3.2).
const capNearPercent = 80

// capNearThreshold returns the number of Incidents at which the cap of max is near: capNearPercent of it,
// rounded up, at least 1.
func capNearThreshold(maxIncidents int) int {
	return max((maxIncidents*capNearPercent+99)/100, 1)
}

// reportCap sets the cap gauges from the Incidents that exist or are pending (the ones the cap holds, as the
// controller's memory counts them, so no failed read leaves the gauges stale), and
// writes a Warning event IncidentCapNear about the analyzer's Pod when their number reaches capNearThreshold
// of the cap: once per crossing, and at most once per EventInterval. The message holds only numbers.
func (c *Controller) reportCap(ctx context.Context) {
	c.mu.Lock()
	n := len(c.names)
	c.mu.Unlock()
	threshold := capNearThreshold(c.cfg.MaxIncidents)
	incidentCap.Set(float64(c.cfg.MaxIncidents))
	near := n >= threshold
	if near {
		incidentCapNear.Set(1)
	} else {
		incidentCapNear.Set(0)
	}
	if !near {
		c.capNear = false
		return
	}
	if c.capNear {
		return
	}
	now := c.cfg.Now()
	if !c.lastNearEvent.IsZero() && now.Sub(c.lastNearEvent) < c.cfg.EventInterval {
		return
	}
	c.capNear = true
	logf.FromContext(ctx).Info("Incidents are near the cap", "incidents", n, "max", c.cfg.MaxIncidents)
	if c.cfg.Recorder == nil || c.cfg.Regarding == nil {
		return
	}
	c.lastNearEvent = now
	c.cfg.Recorder.Eventf(c.cfg.Regarding, corev1.EventTypeWarning, "IncidentCapNear",
		"%d Incidents of the cap of %d (%d%% or more): clear Resolved Incidents before new fingerprints are refused",
		n, c.cfg.MaxIncidents, capNearPercent)
}
