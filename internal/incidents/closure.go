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
// phase: to Resolved (quiet, or a request that Succeeded) or to Escalated (a request that ended without
// success). It is always True; its lastTransitionTime is when that move was written. A move by another rule
// (an analysis outcome, a recurrence) leaves it as it was.
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
	// ReasonRequestNeedsIntervention and ReasonRequestSuperseded: the newest request naming the Incident ended
	// in that phase, so it moved to Escalated for a person. A Superseded request is the newest only when no
	// newer request names the Incident.
	ReasonRequestDenied            = "RequestDenied"
	ReasonRequestExpired           = "RequestExpired"
	ReasonRequestFailed            = "RequestFailed"
	ReasonRequestCancelled         = "RequestCancelled"
	ReasonRequestNeedsIntervention = "RequestNeedsIntervention"
	ReasonRequestSuperseded        = "RequestSuperseded"
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
}

// lifecycleMessages are the fixed messages of the Lifecycle condition. They hold nothing from the cluster.
var lifecycleMessages = map[string]string{
	ReasonSymptomQuiet:             "Nothing was observed for an hour and no request naming the Incident is active",
	ReasonRequestSucceeded:         "The newest request naming the Incident succeeded",
	ReasonRequestDenied:            "The newest request naming the Incident was denied; a person decides",
	ReasonRequestExpired:           "The newest request naming the Incident expired; a person decides",
	ReasonRequestFailed:            "The newest request naming the Incident failed; a person decides",
	ReasonRequestCancelled:         "The newest request naming the Incident was cancelled; a person decides",
	ReasonRequestNeedsIntervention: "The newest request naming the Incident needs intervention; a person decides",
	ReasonRequestSuperseded:        "The newest request naming the Incident was superseded and no newer one names it; a person decides",
}

// finalRequest reports whether a request phase is final (design 3.2): Succeeded or one that ended without
// success. Every other phase, no phase yet and a phase this version does not know included, is active, so an
// unknown phase never resolves an Incident.
func finalRequest(p remediationv1alpha1.RemediationPhase) bool {
	_, ended := endedWithoutSuccess[p]
	return ended || p == remediationv1alpha1.PhaseSucceeded
}

// Closure decides what the closure rules do to an Incident at now (design 3.2, #251): the phase it moves to
// and the Lifecycle reason, or ok false when it stays. requests are the RemediationRequests of the operator
// namespace, a complete listing (the caller fails closed when it cannot read one); only those whose
// incidentRef names this Incident, by name and UID, count. held is the newest observation of the Incident the
// controller holds unwritten, or zero. Closure reads only its arguments.
//
//   - Only an Incident whose name is derived from its spec, with an observation, in phase Open, Proposed or
//     Escalated is decided. Analyzing, Resolved and no phase stay.
//   - In phase Proposed, the newest request naming the Incident (by creation time; at the same time a
//     Superseded one is the older, then by name) decides when it is final: Succeeded moves it to Resolved,
//     any other final phase to Escalated. Superseded thus follows the newer request when there is one, and
//     escalates when there is none.
//   - Otherwise, with no active request naming it, an Incident quiet for QuietBeforeResolved moves to
//     Resolved. It is quiet since the latest of lastSeen, held, lastAnalysisTime and the Lifecycle
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
	case remediationv1alpha1.IncidentOpen, remediationv1alpha1.IncidentProposed, remediationv1alpha1.IncidentEscalated:
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
		if newest == nil || newer(rr, newest) {
			newest = rr
		}
	}
	if st.Phase == remediationv1alpha1.IncidentProposed && newest != nil {
		switch p := newest.Status.Phase; {
		case p == remediationv1alpha1.PhaseSucceeded:
			return remediationv1alpha1.IncidentResolved, ReasonRequestSucceeded, true
		case finalRequest(p):
			return remediationv1alpha1.IncidentEscalated, endedWithoutSuccess[p], true
		}
	}
	if active || lastingState(st) || now.Before(quietSince(inc, held).Add(QuietBeforeResolved)) {
		return "", "", false
	}
	return remediationv1alpha1.IncidentResolved, ReasonSymptomQuiet, true
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
// resolvedTime in the same update. The condition is removed first, so its lastTransitionTime is always that
// of this move ([quietSince] reads it).
func applyClosure(inc *remediationv1alpha1.Incident, phase remediationv1alpha1.IncidentPhase, reason string, now time.Time) {
	at := metav1.NewTime(now.UTC().Truncate(time.Second))
	inc.Status.Phase = phase
	if phase == remediationv1alpha1.IncidentResolved {
		inc.Status.ResolvedTime = &at
	}
	meta.RemoveStatusCondition(&inc.Status.Conditions, ConditionLifecycle)
	meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{
		Type: ConditionLifecycle, Status: metav1.ConditionTrue, Reason: reason, Message: lifecycleMessages[reason],
		ObservedGeneration: inc.Generation, LastTransitionTime: at,
	})
}

// closeIncidents applies the closure rules ([Closure]) to every Incident in the namespace, once per trigger
// pass, and reports how near the Incident cap is ([Controller.reportCap]). It decides first on the cached
// Incidents and requests; for each Incident that would move, it decides again on the Incident read fresh
// from the API server and the requests listed from it (once per pass), and writes only that decision, with
// optimistic concurrency, retrying on conflict. A move is one status update (phase, resolvedTime and the
// Lifecycle condition together), so a restart in the middle finds the Incident either moved or not, and
// decides it again. At most closureWritesPerPass are written per pass.
//
// It fails closed: when the Incidents or the requests cannot be listed, nothing moves and the error is
// returned; an Incident that cannot be read or written keeps its phase.
func (c *Controller) closeIncidents(ctx context.Context) error {
	var list remediationv1alpha1.IncidentList
	if err := c.cache.List(ctx, &list, client.InNamespace(c.cfg.Namespace)); err != nil {
		return fmt.Errorf("listing the Incidents for the closure rules: %w", err)
	}
	c.reportCap(ctx)
	var cached remediationv1alpha1.RemediationRequestList
	if err := c.cache.List(ctx, &cached, client.InNamespace(c.cfg.Namespace)); err != nil {
		return fmt.Errorf("listing the RemediationRequests for the closure rules: %w", err)
	}
	now := c.cfg.Now()
	var live []remediationv1alpha1.RemediationRequest
	read := false
	writes := 0
	var errs []error
	for i := range list.Items {
		inc := &list.Items[i]
		if _, _, ok := Closure(inc, cached.Items, c.heldSince(inc.Spec.Fingerprint), now); !ok {
			continue
		}
		if writes == closureWritesPerPass {
			break
		}
		if !read {
			var fresh remediationv1alpha1.RemediationRequestList
			if err := c.reader.List(ctx, &fresh, client.InNamespace(c.cfg.Namespace)); err != nil {
				return errors.Join(append(errs,
					fmt.Errorf("listing the RemediationRequests for the closure rules: %w", err))...)
			}
			live, read = fresh.Items, true
		}
		wrote, err := c.closeOne(ctx, inc.Name, live, now)
		if wrote || err != nil {
			writes++
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// closeOne reads the Incident name from the API server, decides the closure rules on it with requests (a
// fresh listing) and writes the move, retrying on conflict. It reports whether it wrote one (an answer
// received, not lost). An Incident gone meanwhile is no error.
func (c *Controller) closeOne(ctx context.Context, name string, requests []remediationv1alpha1.RemediationRequest,
	now time.Time) (bool, error) {
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
		phase, r, ok := Closure(inc, requests, c.heldSince(inc.Spec.Fingerprint), now)
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

// capNearPercent is the share of the Incident cap at which the analyzer warns that it is near (design 3.2).
const capNearPercent = 80

// capNearThreshold returns the number of Incidents at which the cap of max is near: capNearPercent of it,
// rounded up, at least 1.
func capNearThreshold(maxIncidents int) int {
	return max((maxIncidents*capNearPercent+99)/100, 1)
}

// reportCap sets the cap gauges from the Incidents that exist or are pending (the ones the cap holds), and
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
