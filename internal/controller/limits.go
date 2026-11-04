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
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// The executor's limits (design sections 6 and 10.7, #88): a lock per target, a cap on active requests and a
// circuit breaker. They hold back the step that lets a request through the policy check (Pending to
// PolicyChecked), the step after which a request writes to Git. A request that is already past it is never
// stopped by them; the kill switch (killswitch.go) is what stops writes at once.
//
// A request is active from PolicyChecked until it reaches a phase it stops in (Merged today, or any final
// phase); a request being deleted is not, since all that is left for it is closing its own change request.
// Before it lets a request through, the policy check counts the active requests and reads the outcomes of the
// others, straight from the API server (never the cache, which may not show a request let through a moment
// ago), and lets it through only when
//
//   - no other active request names the same target UID (the lock),
//   - fewer than maxConcurrentRemediations of RemediationSettings are active (the cap), and
//   - the breaker is closed: the last BreakerThreshold outcomes of requests that were let through are not all
//     failures, or the latest of them is older than BreakerCooldown.
//
// The count and the status write that lets a request through run under one mutex, so two workers can never
// both take the last slot. All state is read from the requests themselves (phase, lastPhaseTransitionTime), so
// a restart neither forgets an open breaker nor frees a slot (invariant 5). It fails closed: when the settings
// or the requests cannot be read, no request is let through.
//
// A request held back keeps no phase (or Pending), gets the condition Queued with the limit as the reason,
// and is checked again after admissionRecheckDelay. The same check runs once before the policy check, so a
// request that cannot go through anyway does not read Git every time it is looked at.

// Reasons of the Queued condition. The metrics (limits_metrics.go) map them to the values of a label.
const (
	// ReasonTargetLocked: another active request names the same target UID.
	ReasonTargetLocked = "TargetLocked"
	// ReasonConcurrencyLimit: maxConcurrentRemediations requests are active.
	ReasonConcurrencyLimit = "ConcurrencyLimitReached"
	// ReasonCircuitOpen: the circuit breaker is open after consecutive failures.
	ReasonCircuitOpen = "CircuitOpen"
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

// limitsState is what one check of the limits found.
type limitsState struct {
	// reason is empty when the request may go through, else the reason of the Queued condition.
	reason   string
	active   int
	limit    int
	open     bool
	failures int
}

// stoppingPhases are the phases a request stops in: the final ones, and Merged, where the request stops today
// (delivery and verification are not built). Any other phase past Pending, an unknown one included, is active.
var stoppingPhases = map[remediationv1alpha1.RemediationPhase]bool{
	remediationv1alpha1.PhaseMerged:            true,
	remediationv1alpha1.PhaseSucceeded:         true,
	remediationv1alpha1.PhaseDenied:            true,
	remediationv1alpha1.PhaseFailed:            true,
	remediationv1alpha1.PhaseExpired:           true,
	remediationv1alpha1.PhaseCancelled:         true,
	remediationv1alpha1.PhaseSuperseded:        true,
	remediationv1alpha1.PhaseNeedsIntervention: true,
}

// active reports whether a request counts against the lock and the cap.
func active(rr *remediationv1alpha1.RemediationRequest) bool {
	p := rr.Status.Phase
	return p != "" && p != remediationv1alpha1.PhasePending && !stoppingPhases[p] && rr.DeletionTimestamp.IsZero()
}

// breakerOutcome reports whether a request that was let through ended in success or failure. Merged and
// Succeeded are successes; Failed, Expired and NeedsIntervention (a pull request closed without a merge
// included) are failures. Denied, Cancelled and Superseded say nothing about whether the changes work, and a
// request without a plan digest never got through the policy check, so neither counts. Nor does a request
// without lastPhaseTransitionTime (one that ended before the field existed): it cannot be ordered.
func breakerOutcome(rr *remediationv1alpha1.RemediationRequest) (failed, ok bool) {
	if rr.Status.PlanDigest == "" || rr.Status.LastPhaseTransitionTime == nil || !rr.DeletionTimestamp.IsZero() {
		return false, false
	}
	switch rr.Status.Phase {
	case remediationv1alpha1.PhaseMerged, remediationv1alpha1.PhaseSucceeded:
		return false, true
	case remediationv1alpha1.PhaseFailed, remediationv1alpha1.PhaseExpired, remediationv1alpha1.PhaseNeedsIntervention:
		return true, true
	}
	return false, false
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
// other read error as it is; the caller must then not let the request through.
func (r *RemediationRequestReconciler) checkLimits(ctx context.Context, rr *remediationv1alpha1.RemediationRequest) (
	limitsState, error) {
	settings := &remediationv1alpha1.RemediationSettings{}
	switch err := r.getLive(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, settings); {
	case apierrors.IsNotFound(err):
		return limitsState{}, errSettingsMissing
	case err != nil:
		return limitsState{}, fmt.Errorf("get RemediationSettings %s: %w", remediationv1alpha1.SettingsName, err)
	}
	list := &remediationv1alpha1.RemediationRequestList{}
	if err := r.reader().List(ctx, list, client.InNamespace(rr.Namespace)); err != nil {
		return limitsState{}, fmt.Errorf("list RemediationRequests in %s: %w", rr.Namespace, err)
	}
	// An object written before the field had a default, or by hand, may hold 0: the smallest limit the schema
	// allows is used then, never no limit.
	s := limitsState{limit: max(int(settings.Spec.MaxConcurrentRemediations), 1)}
	locked := false
	var outcomes []*remediationv1alpha1.RemediationRequest
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
		if _, ok := breakerOutcome(other); ok {
			outcomes = append(outcomes, other)
		}
	}
	// Latest first; equal times by name, so the order does not depend on the list.
	slices.SortFunc(outcomes, func(a, b *remediationv1alpha1.RemediationRequest) int {
		if c := b.Status.LastPhaseTransitionTime.Compare(a.Status.LastPhaseTransitionTime.Time); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	for _, o := range outcomes {
		if failed, _ := breakerOutcome(o); !failed {
			break
		}
		s.failures++
	}
	s.open = s.failures >= r.breakerThreshold() &&
		r.now().Before(outcomes[0].Status.LastPhaseTransitionTime.Add(r.breakerCooldown()))
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
	rr.Status.LastPhaseTransitionTime = &metav1.Time{Time: r.now()}
}

// holdBack checks the limits for a request about to go through the policy check, and holds it back (Queued)
// when one does not let it through. stop means the caller must return res and err without deciding. A failure
// to read the limits stops too, with the error, so the request is retried with backoff (fail closed). Before
// the policy check (beforeCheck) missing settings do not stop: the policy check denies the request then.
func (r *RemediationRequestReconciler) holdBack(ctx context.Context, rr *remediationv1alpha1.RemediationRequest,
	beforeCheck bool) (res reconcile.Result, stop bool, err error) {
	limCtx, cancel := bound(ctx, r.CheckTimeout, DefaultCheckTimeout)
	s, err := r.checkLimits(limCtx, rr)
	err = boundErr(ctx, limCtx, "check of the limits", err)
	cancel()
	if beforeCheck && errors.Is(err, errSettingsMissing) {
		return reconcile.Result{}, false, nil
	}
	recordLimits(s, err)
	if err != nil {
		return reconcile.Result{}, true, err
	}
	if s.reason == "" {
		return reconcile.Result{}, false, nil
	}
	res, err = r.waitForLimits(ctx, rr, s.reason)
	return res, true, err
}
