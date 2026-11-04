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
	"errors"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// The executor's limits (design sections 6 and 10.7, #88): a lock per target, a cap on active requests and a
// circuit breaker. They hold back the step that lets a request through the policy check (Pending to
// PolicyChecked), the step after which a request writes to Git. A request that is already past it is never
// stopped by them; the kill switch (killswitch.go) is what stops writes at once.
//
// A request is active while the executor can still move it on before the merge (progresses: a phase before the
// merge with a plan digest), and while it is being deleted and still holds the finalizer, since its pull
// request may still be open until the finalizer is gone. Before the policy check writes an allowed decision it
// counts the active requests and reads the outcomes of the others, straight from the API server (never the
// cache, which may not show a request let through a moment ago), and lets the request through only when
//
//   - no other active request names the same target UID (the lock),
//   - fewer than maxConcurrentRemediations of RemediationSettings are active (the cap), and
//   - the breaker lets it through: it is closed, or it is half-open and no probe is running (the request
//     becomes the probe then).
//
// The breaker trips when BreakerThreshold failures in a row (after the latest success) are among the
// requests that went through the policy check. Once tripped it stays so until a success, and it is
//
//   - open (CircuitOpen) for BreakerCooldown after the latest failure, or after the latest probe that ended
//     without a success or a failure (Expired, Cancelled, Superseded, Denied): a probe ends the half-open
//     state only with a success, anything else opens the breaker again for a full cool-down (fail closed);
//   - half-open after that: one request at a time goes through as the probe, and the others wait
//     (CircuitHalfOpen) while it runs. Its success (Merged, Succeeded) closes the breaker, since the failures
//     in a row are counted after the latest success; its failure is the latest failure, so the breaker is
//     open again for a full cool-down.
//
// The probe is not marked anywhere: a probe is any request that went through the policy check at or after
// the trip, the time of the BreakerThresholdth failure in a row, by the lastTransitionTime of its
// PolicyAllowed condition, which only the policy check writes, when it lets the request through. While
// tripped no request goes through except as a probe, so every such request is one. That time is kept to the
// second, so a request let through in the second of the trip counts as a probe too (fail closed), and one
// without the condition counts as a probe. A probe runs (holds the others back) until it reaches a final
// phase or Merged; one being deleted runs until it is gone.
//
// The count and the status write that lets a request through run under one mutex, so two workers can never
// both take the last slot or both become the probe. All state is read from the requests themselves (phase,
// lastPhaseTransitionTime, the PolicyAllowed time), so a restart or a new leader neither forgets an open breaker
// or a running probe nor frees a slot (invariant 5). It fails closed: when the settings
// or the requests cannot be read, or there is no API server reader, no request is let through.
//
// Only a request the policy check allows is held back: refusals (Denied, NeedsIntervention) are written at
// once. A request held back keeps no phase (or Pending), gets the condition Queued with the limit as the
// reason, and is checked again after admissionRecheckDelay. While it is Queued only the limits are checked, so
// it does not read Git; once they let it through, Queued is removed and the whole policy check runs again.
//
// The limits are counted per namespace: requests live in the operator namespace. The lock is per target
// object, not per file in Git; two targets declared in one file are kept apart by the plan digest and the
// freshness check, which supersede the second plan when the first one changes the file.

// Reasons of the Queued condition. The metrics (limits_metrics.go) map them to the values of a label.
const (
	// ReasonTargetLocked: another active request names the same target UID.
	ReasonTargetLocked = "TargetLocked"
	// ReasonConcurrencyLimit: maxConcurrentRemediations requests are active.
	ReasonConcurrencyLimit = "ConcurrencyLimitReached"
	// ReasonCircuitOpen: the circuit breaker is open after consecutive failures.
	ReasonCircuitOpen = "CircuitOpen"
	// ReasonCircuitHalfOpen: the circuit breaker is half-open and another request runs as its probe.
	ReasonCircuitHalfOpen = "CircuitHalfOpen"
	// ReasonLimitsUnknown: the limits could not be read, so the request is held back (fail closed). It is
	// counted in the metrics only; the request is retried with the error backoff and gets no condition.
	ReasonLimitsUnknown = "LimitsUnknown"
)

// Fixed messages of the Queued condition.
var queuedMessages = map[string]string{
	ReasonTargetLocked:     "Another active request changes the same target; this one waits until that one ends",
	ReasonConcurrencyLimit: "RemediationSettings default allows no more active requests (maxConcurrentRemediations); this one waits for a slot",
	ReasonCircuitOpen: "The circuit breaker is open after consecutive failed requests; this one waits for the " +
		"cool-down to end",
	ReasonCircuitHalfOpen: "The circuit breaker is half-open after its cool-down and another request runs as its " +
		"probe; this one waits until the probe ends",
}

// Defaults of the circuit breaker.
const (
	// DefaultBreakerThreshold is how many consecutive failures open the breaker.
	DefaultBreakerThreshold = 3
	// DefaultBreakerCooldown is how long the breaker stays open after the latest of those failures.
	DefaultBreakerCooldown = 30 * time.Minute
)

// admissionRecheckDelay is how long a request held back by a limit waits before it is checked again.
const admissionRecheckDelay = 30 * time.Second

// errSettingsMissing means RemediationSettings default does not exist. The policy check denies such a request,
// so the check before it lets the request go on to be denied; the check after it holds the request back.
var errSettingsMissing = errors.New("RemediationSettings default does not exist")

// circuitState is the state of the circuit breaker.
type circuitState int32

const (
	circuitClosed circuitState = iota
	circuitOpen
	circuitHalfOpen
)

// limitsState is what one check of the limits found.
type limitsState struct {
	// reason is empty when the request may go through, else the reason of the Queued condition.
	reason   string
	active   int
	limit    int
	circuit  circuitState
	failures int
	// probing is whether a probe runs while the breaker is half-open.
	probing bool
}

// progresses reports whether the executor can still move a request on before the merge (recheck): a phase
// before the merge, with the plan digest the policy check fixes. Phases after the merge are not built, and a
// request in one stops there today.
func progresses(rr *remediationv1alpha1.RemediationRequest) bool {
	return beforeMerge(rr.Status.Phase) && rr.Status.PlanDigest != ""
}

// active reports whether a request counts against the lock and the cap: it progresses, or it is being deleted
// and still holds the finalizer that closes its pull request.
func active(rr *remediationv1alpha1.RemediationRequest) bool {
	if !rr.DeletionTimestamp.IsZero() {
		return controllerutil.ContainsFinalizer(rr, FinalizerName)
	}
	return progresses(rr)
}

// Outcomes of a request for the circuit breaker.
type outcomeClass int

const (
	outcomeNone outcomeClass = iota
	outcomeSuccess
	outcomeFailure
)

// breakerOutcome classifies a request for the breaker. Merged and Succeeded are successes; Failed and
// NeedsIntervention (a pull request closed without a merge included) are failures. Expired is neither: it
// comes from Git host outages and deadlines, not from a bad change. Denied, Cancelled and Superseded say
// nothing about whether the changes work, and a request without a plan digest never got through the policy
// check, so neither counts. Nor does a request without lastPhaseTransitionTime (one that ended before the
// field existed): it cannot be ordered.
func breakerOutcome(rr *remediationv1alpha1.RemediationRequest) outcomeClass {
	if rr.Status.PlanDigest == "" || rr.Status.LastPhaseTransitionTime == nil {
		return outcomeNone
	}
	switch rr.Status.Phase {
	case remediationv1alpha1.PhaseMerged, remediationv1alpha1.PhaseSucceeded:
		return outcomeSuccess
	case remediationv1alpha1.PhaseFailed, remediationv1alpha1.PhaseNeedsIntervention:
		return outcomeFailure
	}
	return outcomeNone
}

func (r *RemediationRequestReconciler) breakerThreshold() int {
	if r.BreakerThreshold > 0 {
		return r.BreakerThreshold
	}
	return DefaultBreakerThreshold
}

func (r *RemediationRequestReconciler) breakerCooldown() time.Duration {
	if r.BreakerCooldown > 0 {
		return r.BreakerCooldown
	}
	return DefaultBreakerCooldown
}

// checkLimits reads the settings and the requests in rr's namespace from the API server and decides whether rr
// may go through the policy check now. It returns errSettingsMissing when the settings do not exist, and any
// other read error as it is; the caller must then not let the request through. Without an APIReader it
// refuses too: like the kill switch, it never falls back to the cache.
func (r *RemediationRequestReconciler) checkLimits(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	limitsState, error) {
	if r.APIReader == nil {
		return limitsState{}, errors.New("cannot check the limits: no API server reader")
	}
	settings := &remediationv1alpha1.RemediationSettings{}
	switch err := r.APIReader.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, settings); {
	case apierrors.IsNotFound(err):
		return limitsState{}, errSettingsMissing
	case err != nil:
		return limitsState{}, fmt.Errorf("get RemediationSettings %s: %w", remediationv1alpha1.SettingsName, err)
	}
	list := &remediationv1alpha1.RemediationRequestList{}
	if err := r.APIReader.List(ctx, list, client.InNamespace(rr.Namespace)); err != nil {
		return limitsState{}, fmt.Errorf("list RemediationRequests in %s: %w", rr.Namespace, err)
	}
	// An object written before the field had a default, or by hand, may hold 0: the smallest limit the schema
	// allows is used then, never no limit.
	s := limitsState{limit: max(int(settings.Spec.MaxConcurrentRemediations), 1)}
	locked := false
	var lastSuccess, lastFailure time.Time
	var failures []time.Time
	// passed are the other requests that went through the policy check; the probes are among them.
	var passed []*remediationv1alpha1.RemediationRequest
	for i := range list.Items {
		other := &list.Items[i]
		if other.UID == rr.UID {
			continue
		}
		if active(other) {
			s.active++
			if other.Spec.TargetRef.UID == rr.Spec.TargetRef.UID {
				locked = true
			}
		}
		if other.Status.PlanDigest != "" {
			passed = append(passed, other)
		}
		switch breakerOutcome(other) {
		case outcomeSuccess:
			if t := other.Status.LastPhaseTransitionTime.Time; t.After(lastSuccess) {
				lastSuccess = t
			}
		case outcomeFailure:
			t := other.Status.LastPhaseTransitionTime.Time
			failures = append(failures, t)
			if t.After(lastFailure) {
				lastFailure = t
			}
		}
	}
	// The failures in a row are those after the latest success; one at the very same time counts as a failure.
	inRow := make([]time.Time, 0, len(failures))
	for _, t := range failures {
		if !t.Before(lastSuccess) {
			inRow = append(inRow, t)
		}
	}
	s.failures = len(inRow)
	s.circuit, s.probing = r.breaker(inRow, lastFailure, passed)
	switch {
	case s.circuit == circuitOpen:
		s.reason = ReasonCircuitOpen
	case s.circuit == circuitHalfOpen && s.probing:
		s.reason = ReasonCircuitHalfOpen
	case locked:
		s.reason = ReasonTargetLocked
	case s.active >= s.limit:
		s.reason = ReasonConcurrencyLimit
	}
	return s, nil
}

// breaker works out the state of the circuit breaker (see the top of this file) from inRow, the times of the
// failures in a row, lastFailure, the latest of them, and passed, the other requests that went through the
// policy check. probing is whether a probe runs; it is set only when the breaker is half-open.
func (r *RemediationRequestReconciler) breaker(inRow []time.Time, lastFailure time.Time,
	passed []*remediationv1alpha1.RemediationRequest) (state circuitState, probing bool) {
	threshold := r.breakerThreshold()
	if len(inRow) < threshold {
		return circuitClosed, false
	}
	slices.SortFunc(inRow, time.Time.Compare)
	tripped := inRow[threshold-1]
	reopened := lastFailure
	for _, p := range passed {
		if !probe(p, tripped) {
			continue
		}
		switch {
		case !p.DeletionTimestamp.IsZero():
			probing = true
		case breakerOutcome(p) != outcomeNone:
			// A success would have closed the breaker; a failure is counted in lastFailure.
		case endsWithoutOutcome(p.Status.Phase):
			// Ended without a success: the breaker opens again from then. A probe that ended without a
			// transition time (written by hand) keeps it open (fail closed).
			end := r.now()
			if p.Status.LastPhaseTransitionTime != nil {
				end = p.Status.LastPhaseTransitionTime.Time
			}
			if end.After(reopened) {
				reopened = end
			}
		default:
			probing = true
		}
	}
	if r.now().Before(reopened.Add(r.breakerCooldown())) {
		return circuitOpen, false
	}
	return circuitHalfOpen, probing
}

// probe reports whether rr went through the policy check at or after tripped, by the lastTransitionTime of its
// PolicyAllowed condition. That time is kept to the second, so a request let through in the same second as
// tripped counts; one without the condition counts too (fail closed).
func probe(rr *remediationv1alpha1.RemediationRequest, tripped time.Time) bool {
	c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed)
	if c == nil {
		return true
	}
	return c.LastTransitionTime.Add(time.Second).After(tripped)
}

// endsWithoutOutcome reports whether phase is final (or Merged) but gave the breaker no outcome. Failed,
// NeedsIntervention, Merged and Succeeded are here too: they give none without a transition time.
func endsWithoutOutcome(phase remediationv1alpha1.RemediationPhase) bool {
	switch phase {
	case remediationv1alpha1.PhaseExpired, remediationv1alpha1.PhaseCancelled, remediationv1alpha1.PhaseSuperseded,
		remediationv1alpha1.PhaseDenied, remediationv1alpha1.PhaseFailed, remediationv1alpha1.PhaseNeedsIntervention,
		remediationv1alpha1.PhaseMerged, remediationv1alpha1.PhaseSucceeded:
		return true
	}
	return false
}

// waitForLimits holds back a request that a limit does not let through: it sets Queued True with the limit as
// the reason, writes the status when that changed, and checks again after admissionRecheckDelay.
func (r *RemediationRequestReconciler) waitForLimits(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	reason string) (reconcile.Result, error) {
	if meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
		Type:               remediationv1alpha1.ConditionQueued,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            queuedMessages[reason],
		ObservedGeneration: rr.Generation,
	}) {
		if err := r.Status().Update(ctx, rr); err != nil {
			return reconcile.Result{}, fmt.Errorf("update status of RemediationRequest %s/%s: %w", rr.Namespace, rr.Name, err)
		}
		logf.FromContext(ctx).Info("Queued RemediationRequest behind a limit", "remediationRequest",
			client.ObjectKeyFromObject(rr), "reason", reason)
	}
	return reconcile.Result{RequeueAfter: admissionRecheckDelay}, nil
}

// setPhase sets the request's phase and, when it changes, lastPhaseTransitionTime to now.
func (r *RemediationRequestReconciler) setPhase(rr *remediationv1alpha1.RemediationRequest,
	phase remediationv1alpha1.RemediationPhase) {
	if rr.Status.Phase == phase && rr.Status.LastPhaseTransitionTime != nil {
		return
	}
	rr.Status.Phase = phase
	t := metav1.NewMicroTime(r.now())
	rr.Status.LastPhaseTransitionTime = &t
}

// holdBack checks the limits for a request about to go through the policy check, and holds it back (Queued)
// when one does not let it through. stop means the caller must return res and err without deciding. A failure
// to read the limits stops too, with the error, so the request is retried with backoff (fail closed).
//
// queued is the check of a request that is already Queued, made in place of the policy check so that a
// waiting request does not read Git. Once the limits let it through, Queued is removed (written at once, so it
// is not left behind when the policy check then fails for a while) and the policy check runs. Missing
// settings let it go on as well: the policy check denies the request then.
func (r *RemediationRequestReconciler) holdBack(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	queued bool) (res reconcile.Result, stop bool, err error) {
	limCtx, cancel := bound(ctx, r.CheckTimeout, DefaultCheckTimeout)
	s, err := r.checkLimits(limCtx, rr)
	err = boundErr(ctx, limCtx, "check of the limits", err)
	cancel()
	pass := queued && errors.Is(err, errSettingsMissing)
	if !pass {
		recordLimits(s, err)
		if err != nil {
			return reconcile.Result{}, true, err
		}
		if s.reason != "" {
			res, err = r.waitForLimits(ctx, rr, s.reason)
			return res, true, err
		}
	}
	if queued && meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionQueued) != nil {
		meta.RemoveStatusCondition(&rr.Status.Conditions, remediationv1alpha1.ConditionQueued)
		if err := r.Status().Update(ctx, rr); err != nil {
			return reconcile.Result{}, true, fmt.Errorf("update status of RemediationRequest %s/%s: %w",
				rr.Namespace, rr.Name, err)
		}
	}
	return reconcile.Result{}, false, nil
}
