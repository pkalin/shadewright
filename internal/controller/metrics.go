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
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/phasegauge"
	"github.com/pkalin/shadewright/internal/policy"
)

// The executor's metrics, served on its metrics endpoint (docs/reference/metrics.md). Every label takes its
// values from a closed set fixed in this file: never a name, namespace, UID or message from the cluster, so the
// number of series stays bounded whatever the requests hold.

// labelOther is the value of a label whose input is not in the label's closed set.
const labelOther = phasegauge.Other

// Outcomes of policyDecisions.
const (
	decisionAllowed           = "allowed"
	decisionDenied            = "denied"
	decisionNeedsIntervention = "needs_intervention"
)

// policyDecisions counts the decisions of the policy check, by outcome, once each: it is counted after the
// status write that records the decision succeeded, and that write is made with the resourceVersion read, so
// a request is never counted twice.
var policyDecisions = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_policy_decisions_total",
	Help: "Decisions of the policy check, by outcome (allowed, denied, needs_intervention).",
}, []string{"outcome"})

// policyDenials counts the requests the policy check denied, by the reason of the PolicyAllowed condition.
// A reason not in denialReasons is counted as Other.
var policyDenials = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_policy_denials_total",
	Help: "Requests denied by the policy check, by reason (the PolicyAllowed condition's reason, or Other).",
}, []string{"reason"})

// Kinds of reconcileErrors.
const (
	errorTimeout   = "timeout"
	errorConflict  = "conflict"
	errorCancelled = "cancelled"
	errorOther     = "other"
)

// reconcileErrors counts the errors the RemediationRequest reconciler returned (each is retried with
// backoff), by kind. controller-runtime's controller_runtime_reconcile_errors_total counts the same errors
// without a kind.
var reconcileErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_remediation_reconcile_errors_total",
	Help: "Errors returned by the RemediationRequest reconciler, by kind (timeout, conflict, cancelled, other).",
}, []string{labelKind})

// labelKind is the label of reconcileErrors.
const labelKind = "kind"

// requestPhases reports the RemediationRequests in the operator namespace by phase.
var requestPhases = &requestPhaseCollector{
	desc: prometheus.NewDesc("shadewright_remediation_requests",
		"RemediationRequests in the operator namespace, by phase (None before the policy check, Other for a "+
			"phase this version does not know).", []string{"phase"}, nil),
}

func init() {
	metrics.Registry.MustRegister(policyDecisions, policyDenials, reconcileErrors, requestPhases)
	// The closed sets start at zero, so a rate is defined before the first event.
	for _, o := range []string{decisionAllowed, decisionDenied, decisionNeedsIntervention} {
		policyDecisions.WithLabelValues(o)
	}
	for _, k := range []string{errorTimeout, errorConflict, errorCancelled, errorOther} {
		reconcileErrors.WithLabelValues(k)
	}
	for r := range denialReasons {
		policyDenials.WithLabelValues(r)
	}
	policyDenials.WithLabelValues(labelOther)
}

// requestPhaseList are the phases requestPhases reports by name, the enum of RemediationPhase.
var requestPhaseList = []remediationv1alpha1.RemediationPhase{
	remediationv1alpha1.PhasePending,
	remediationv1alpha1.PhasePolicyChecked,
	remediationv1alpha1.PhasePRCreated,
	remediationv1alpha1.PhaseAwaitingApproval,
	remediationv1alpha1.PhaseAutoMergeQueued,
	remediationv1alpha1.PhaseMerged,
	remediationv1alpha1.PhaseWaitingForDelivery,
	remediationv1alpha1.PhaseVerifying,
	remediationv1alpha1.PhaseSucceeded,
	remediationv1alpha1.PhaseDenied,
	remediationv1alpha1.PhaseFailed,
	remediationv1alpha1.PhaseExpired,
	remediationv1alpha1.PhaseCancelled,
	remediationv1alpha1.PhaseSuperseded,
	remediationv1alpha1.PhaseNeedsIntervention,
}

// denialReasons are the reasons policyDenials reports by name: the reasons the controller, package policy and
// package sourcemap give. Every one is a constant in the code, never text from the cluster. The list is kept by
// hand; TestDenialReasonsCoverEveryReason fails when a reason constant of those packages is missing.
var denialReasons = func() map[string]struct{} {
	reasons := make([]string, 0, 64)
	reasons = append(reasons,
		ReasonPolicyNotFound, ReasonSettingsNotFound, ReasonTargetMismatch, ReasonPreviousValueMismatch)
	for _, r := range []policy.Reason{
		policy.ReasonIncompleteInput, policy.ReasonPolicyMismatch, policy.ReasonRequestNamespaceNotAllowed,
		policy.ReasonModeNotSupported, policy.ReasonActionUnknown, policy.ReasonInvalidParameters,
		policy.ReasonTargetMismatch, policy.ReasonKindExcluded, policy.ReasonKindNotAllowed,
		policy.ReasonTargetNamespaceNotAllowed, policy.ReasonTargetLabelsNotMatched, policy.ReasonActionNotAllowed,
		policy.ReasonPolicyInvalid, policy.ReasonBelowMinimum, policy.ReasonAboveMaximum,
		policy.ReasonIncreaseTooLarge, policy.ReasonDecreaseNotAllowed, policy.ReasonDecreaseTooLarge,
		policy.ReasonRepositoryNotAllowed, policy.ReasonRepositoryNotConfigured, policy.ReasonBranchNotAllowed,
		policy.ReasonPathNotAllowed, policy.ReasonModuleNotEnabled, policy.ReasonScaledObjectNotReady,
		policy.ReasonScaledObjectReadinessUnknown,
	} {
		reasons = append(reasons, string(r))
	}
	for _, r := range []sourcemap.Reason{
		sourcemap.ReasonInvalidInput, sourcemap.ReasonTargetNotFound, sourcemap.ReasonTargetChanged,
		sourcemap.ReasonHelmManaged, sourcemap.ReasonNoFluxLabels, sourcemap.ReasonInvalidFluxLabels,
		sourcemap.ReasonKustomizationNotFound, sourcemap.ReasonKustomizationSuspended,
		sourcemap.ReasonKustomizationNotReady, sourcemap.ReasonNotInInventory,
		sourcemap.ReasonKustomizationTransforms, sourcemap.ReasonRemoteCluster, sourcemap.ReasonDecryption,
		sourcemap.ReasonUnsupportedSource, sourcemap.ReasonSourceNotFound, sourcemap.ReasonSourceNotBranch,
		sourcemap.ReasonSourceIncludes, sourcemap.ReasonSourceOptions, sourcemap.ReasonSourceNotReady,
		sourcemap.ReasonRevisionMismatch, sourcemap.ReasonRepositoryNotConfigured,
		sourcemap.ReasonRepositoryNotInPolicy, sourcemap.ReasonRepositoryUnusable, sourcemap.ReasonBranchMismatch,
		sourcemap.ReasonBranchNotFound, sourcemap.ReasonInvalidPath, sourcemap.ReasonNoKustomizationFile,
		sourcemap.ReasonUnsupportedKustomization, sourcemap.ReasonFileNotReadable, sourcemap.ReasonTooManyFiles,
		sourcemap.ReasonDeclarationNotFound, sourcemap.ReasonAmbiguousDeclaration, sourcemap.ReasonFieldNotFound,
		sourcemap.ReasonValueMismatch, sourcemap.ReasonPathNotAllowed, sourcemap.ReasonInputTooLarge,
	} {
		reasons = append(reasons, string(r))
	}
	set := make(map[string]struct{}, len(reasons))
	for _, r := range reasons {
		set[r] = struct{}{}
	}
	return set
}()

// denialReasonLabel returns reason when it is one of denialReasons, and Other otherwise.
func denialReasonLabel(reason string) string {
	if _, ok := denialReasons[reason]; ok {
		return reason
	}
	return labelOther
}

// recordDecision counts a decision of the policy check that was written to the status.
func recordDecision(phase remediationv1alpha1.RemediationPhase, reason string) {
	switch phase {
	case remediationv1alpha1.PhasePolicyChecked:
		policyDecisions.WithLabelValues(decisionAllowed).Inc()
	case remediationv1alpha1.PhaseDenied:
		policyDecisions.WithLabelValues(decisionDenied).Inc()
		policyDenials.WithLabelValues(denialReasonLabel(reason)).Inc()
	case remediationv1alpha1.PhaseNeedsIntervention:
		policyDecisions.WithLabelValues(decisionNeedsIntervention).Inc()
	}
}

// errorKind sorts a reconcile error into the closed set of reconcileErrors.
func errorKind(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return errorCancelled
	case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return errorTimeout
	case apierrors.IsConflict(err):
		return errorConflict
	}
	return errorOther
}

// countErrors wraps a reconciler and counts the errors it returns in reconcileErrors.
type countErrors struct {
	reconcile.Reconciler
}

// Reconcile calls the wrapped reconciler and counts its error, if any.
func (c countErrors) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	res, err := c.Reconciler.Reconcile(ctx, req)
	if err != nil {
		reconcileErrors.WithLabelValues(errorKind(err)).Inc()
	}
	return res, err
}

// phaseListTimeout bounds the cache read of one scrape. The source is set only once the informer has synced
// (package phasegauge), so the read does not wait for the API server; the bound guards the endpoint anyway.
const phaseListTimeout = 5 * time.Second

// requestPhaseCollector counts the RemediationRequests in the operator namespace by phase when the metrics are
// scraped. It reads the informer the controller already uses, and only on the leader once that informer has
// synced (phasegauge.Source), so a scrape makes no API call and starts no watch. Until then, on a standby
// replica, or when the read fails, it reports nothing: a missing series is visible, a zero would claim there
// are no requests.
type requestPhaseCollector struct {
	desc      *prometheus.Desc
	source    phasegauge.Source
	namespace atomic.Pointer[string]
}

// setNamespace sets the namespace the collector counts in.
func (c *requestPhaseCollector) setNamespace(ns string) {
	c.namespace.Store(&ns)
}

// Describe implements prometheus.Collector.
func (c *requestPhaseCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

// Collect implements prometheus.Collector.
func (c *requestPhaseCollector) Collect(ch chan<- prometheus.Metric) {
	r, ns := c.source.Reader(), c.namespace.Load()
	if r == nil || ns == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), phaseListTimeout)
	defer cancel()
	var list remediationv1alpha1.RemediationRequestList
	// The objects are only read, so the cache's copies are used as they are.
	if err := r.List(ctx, &list, client.InNamespace(*ns), client.UnsafeDisableDeepCopy); err != nil {
		logf.Log.WithName("metrics").Error(err, "Failed to list RemediationRequests for the phase metric")
		return
	}
	counts := make(map[string]int, len(requestPhaseList)+2)
	for _, l := range phasegauge.Labels(requestPhaseList) {
		counts[l] = 0
	}
	for i := range list.Items {
		counts[phasegauge.Label(list.Items[i].Status.Phase, requestPhaseList)]++
	}
	for phase, n := range counts {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n), phase)
	}
}
