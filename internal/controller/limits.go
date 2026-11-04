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
//   - the breaker is closed: fewer than BreakerThreshold failures came after the latest success among the
//     requests that went through the policy check, or the latest of them is older than BreakerCooldown.
//
// The count and the status write that lets a request through run under one mutex, so two workers can never
// both take the last slot. All state is read from the requests themselves (phase, lastPhaseTransitionTime), so
// a restart neither forgets an open breaker nor frees a slot (invariant 5). It fails closed: when the settings
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

// Reasons of the Queued condition. They are fixed and few, so they may be used as metric labels.
const (
	// ReasonTargetLocked: another active request names the same target UID.
	ReasonTargetLocked = "TargetLocked"
	// ReasonConcurrencyLimit: maxConcurrentRemediations requests are active.
	ReasonConcurrencyLimit = "ConcurrencyLimitReached"
	// ReasonCircuitOpen: the circuit breaker is open after consecutive failures.
	ReasonCircuitOpen = "CircuitOpen"
	// ReasonLimitsUnknown: the limits could not be read, so the request is held back (fail closed). It is
	// reported to the LimitsObserver only; the request is retried with the error backoff and gets no condition.
	ReasonLimitsUnknown = "LimitsUnknown"
)

// Fixed messages of the Queued condition.
var queuedMessages = map[string]string{
	ReasonTargetLocked:     "Another active request changes the same target; this one waits until that one ends",
	ReasonConcurrencyLimit: "RemediationSettings default allows no more active requests (maxConcurrentRemediations); this one waits for a slot",
	ReasonCircuitOpen: "The circuit breaker is open after consecutive failed requests; this one waits for the " +
		"cool-down to end",
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

// LimitsObserver receives the state of the executor's limits, for metrics. Its methods must be cheap and must
// not block. Labels taken from it are bounded: reasons are the fixed constants above, and no request or target
// is named.
type LimitsObserver interface {
	// AdmissionWaited is called each time a request the policy check would let through is held back. reason
	// is ReasonTargetLocked, ReasonConcurrencyLimit, ReasonCircuitOpen or ReasonLimitsUnknown.
	AdmissionWaited(reason string)
	// ActiveRemediations reports, after each check of the limits, the active requests (the one being decided
	// not included) and maxConcurrentRemediations in force.
	ActiveRemediations(active, limit int)
	// Breaker reports, after each check of the limits, whether the circuit breaker is open and how many
	// failures in a row the latest outcomes are.
	Breaker(open bool, consecutiveFailures int)
}

// errSettingsMissing means RemediationSettings default does not exist. The policy check denies such a request,
// so the check before it lets the request go on to be denied; the check after it holds the request back.
var errSettingsMissing = errors.New("RemediationSettings default does not exist")

// limitsState is what one check of the limits found.
type limitsState struct {
	// reason is empty when the request may go through, else the reason of the Queued condition.
	reason   string
	active   int
	limit    int
	open     bool
	failures int
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
	for _, t := range failures {
		if !t.Before(lastSuccess) {
			s.failures++
		}
	}
	s.open = s.failures >= r.breakerThreshold() && r.now().Before(lastFailure.Add(r.breakerCooldown()))
	switch {
	case s.open:
		s.reason = ReasonCircuitOpen
	case locked:
		s.reason = ReasonTargetLocked
	case s.active >= s.limit:
		s.reason = ReasonConcurrencyLimit
	}
	return s, nil
}

// observeLimits reports what a check of the limits found to the LimitsObserver, if any. err is the error of the
// check; a request held back is reported with its reason.
func (r *RemediationRequestReconciler) observeLimits(s limitsState, err error) {
	if r.Limits == nil {
		return
	}
	if err != nil {
		r.Limits.AdmissionWaited(ReasonLimitsUnknown)
		return
	}
	r.Limits.ActiveRemediations(s.active, s.limit)
	r.Limits.Breaker(s.open, s.failures)
	if s.reason != "" {
		r.Limits.AdmissionWaited(s.reason)
	}
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
		r.observeLimits(s, err)
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
