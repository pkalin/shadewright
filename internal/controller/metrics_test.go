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
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/phasegauge"
	"github.com/pkalin/shadewright/internal/policy"
	"github.com/pkalin/shadewright/internal/testutil/metricguard"
)

func TestDenialReasonLabel(t *testing.T) {
	cases := map[string]string{
		ReasonPolicyNotFound:              ReasonPolicyNotFound,
		string(policy.ReasonAboveMaximum): string(policy.ReasonAboveMaximum),
		"RepositoryNotInPolicy":           "RepositoryNotInPolicy",
		"":                                labelOther,
		"web-7f9c/orders: limit raised by a stranger": labelOther,
		"Abovemaximum": labelOther,
	}
	for in, want := range cases {
		if got := denialReasonLabel(in); got != want {
			t.Errorf("denialReasonLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestErrorKind(t *testing.T) {
	gr := schema.GroupResource{Group: remediationv1alpha1.GroupVersion.Group, Resource: "remediationrequests"}
	cases := map[string]struct {
		err  error
		want string
	}{
		"phase deadline":     {fmt.Errorf("policy check: %w", ErrTimeout), errorTimeout},
		"context deadline":   {fmt.Errorf("get: %w", context.DeadlineExceeded), errorTimeout},
		"cancelled":          {fmt.Errorf("policy check cancelled: %w", context.Canceled), errorCancelled},
		"conflict":           {fmt.Errorf("update status: %w", apierrors.NewConflict(gr, "rr", errors.New("stale"))), errorConflict},
		"anything else":      {errors.New("the Git host answered 502"), errorOther},
		"server unavailable": {apierrors.NewServiceUnavailable("down"), errorOther},
	}
	for name, c := range cases {
		if got := errorKind(c.err); got != c.want {
			t.Errorf("%s: errorKind = %q, want %q", name, got, c.want)
		}
	}
}

type reconcileFunc func(context.Context, ctrl.Request) (ctrl.Result, error)

func (f reconcileFunc) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return f(ctx, req)
}

func TestCountErrorsCountsByKind(t *testing.T) {
	var next error
	r := countErrors{reconcileFunc(func(context.Context, ctrl.Request) (ctrl.Result, error) {
		return ctrl.Result{}, next
	})}
	timeouts, others := testutil.ToFloat64(reconcileErrors.WithLabelValues(errorTimeout)),
		testutil.ToFloat64(reconcileErrors.WithLabelValues(errorOther))

	next = nil
	if _, err := r.Reconcile(t.Context(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	next = fmt.Errorf("policy check: %w", ErrTimeout)
	if _, err := r.Reconcile(t.Context(), ctrl.Request{}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("the wrapper changed the error: %v", err)
	}
	if got := testutil.ToFloat64(reconcileErrors.WithLabelValues(errorTimeout)) - timeouts; got != 1 {
		t.Errorf("timeouts counted %v, want 1", got)
	}
	if got := testutil.ToFloat64(reconcileErrors.WithLabelValues(errorOther)) - others; got != 0 {
		t.Errorf("others counted %v, want 0 (a nil error is not counted)", got)
	}
}

// operatorNamespace is the namespace of the requests the phase collector counts in these tests.
const operatorNamespace = "shadewright-system"

// requestsIn returns a fake client holding requests in operatorNamespace with the given phases, and one more
// request in another namespace, which the collector must not count.
func requestsIn(t *testing.T, phases ...remediationv1alpha1.RemediationPhase) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := make([]client.Object, 0, len(phases)+1)
	objs = append(objs, &remediationv1alpha1.RemediationRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: "another-namespace", Name: "rr-other"},
		Status:     remediationv1alpha1.RemediationRequestStatus{Phase: remediationv1alpha1.PhaseDenied},
	})
	for i, p := range phases {
		objs = append(objs, &remediationv1alpha1.RemediationRequest{
			ObjectMeta: metav1.ObjectMeta{Namespace: operatorNamespace, Name: fmt.Sprintf("rr-%d", i)},
			Status:     remediationv1alpha1.RemediationRequestStatus{Phase: p},
		})
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// The collector counts the requests of the operator namespace only, by phase, with unknown and missing phases
// as Other and None, and reports nothing until its source is set.
func TestRequestPhaseCollector(t *testing.T) {
	c := &requestPhaseCollector{desc: requestPhases.desc}
	c.setNamespace(operatorNamespace)
	if n := testutil.CollectAndCount(c); n != 0 {
		t.Fatalf("a collector without a source reported %d series, want none", n)
	}
	c.source.Set(requestsIn(t, remediationv1alpha1.PhaseDenied, remediationv1alpha1.PhaseDenied,
		remediationv1alpha1.PhasePRCreated, "", "Made-Up by someone"))
	want := map[string]float64{}
	for _, l := range phasegauge.Labels(requestPhaseList) {
		want[l] = 0
	}
	want[phasegauge.None], want[phasegauge.Other] = 1, 1
	want[string(remediationv1alpha1.PhaseDenied)] = 2
	want[string(remediationv1alpha1.PhasePRCreated)] = 1

	var exp strings.Builder
	exp.WriteString("# HELP shadewright_remediation_requests " + metricguard.Help(t, c.desc) + "\n")
	exp.WriteString("# TYPE shadewright_remediation_requests gauge\n")
	for _, phase := range slices.Sorted(maps.Keys(want)) {
		fmt.Fprintf(&exp, "shadewright_remediation_requests{phase=%q} %v\n", phase, want[phase])
	}
	if err := testutil.CollectAndCompare(c, strings.NewReader(exp.String())); err != nil {
		t.Fatal(err)
	}
}

// executorMetrics are the executor's metrics and the closed set of every label.
func executorMetrics() map[string]metricguard.Labels {
	reasons := append(slices.Collect(maps.Keys(denialReasons)), labelOther)
	return map[string]metricguard.Labels{
		"shadewright_policy_decisions_total": {"outcome": {decisionAllowed, decisionDenied, decisionNeedsIntervention}},
		"shadewright_policy_denials_total":   {"reason": reasons},
		"shadewright_remediation_reconcile_errors_total": {
			labelKind: {errorTimeout, errorConflict, errorCancelled, errorOther},
		},
		"shadewright_remediation_requests":          {"phase": phasegauge.Labels(requestPhaseList)},
		"shadewright_remediations_in_flight":        {},
		"shadewright_remediation_concurrency_limit": {},
		"shadewright_remediation_waits_total":       {"reason": slices.Collect(maps.Values(waitReasons))},
		"shadewright_circuit_breaker_open":          {},
		"shadewright_circuit_breaker_trips_total":   {},
	}
}

// Every metric the executor registers is in the table above, takes label values only from closed sets, and is
// documented, whatever text a denial reason or a phase holds.
func TestExecutorMetricsAreBoundedAndDocumented(t *testing.T) {
	recordDecision(remediationv1alpha1.PhaseDenied, "a message from the cluster")
	recordDecision(remediationv1alpha1.PhaseDenied, string(policy.ReasonKindExcluded))
	reconcileErrors.WithLabelValues(errorKind(errors.New("free text")))
	requestPhases.setNamespace(operatorNamespace)
	requestPhases.source.Set(requestsIn(t, remediationv1alpha1.PhasePending, "a phase from the cluster", ""))
	t.Cleanup(func() { requestPhases.source.Set(nil) })

	metricguard.Check(t, metrics.Registry, executorMetrics())
	metricguard.Documented(t, filepath.Join("..", "..", "docs", "reference", "metrics.md"), executorMetrics())
}

// TestDenialReasonsStartAtZero checks that every denial reason and Other has a series before the first denial,
// so an alert on one reason sees 0 rather than no series.
func TestDenialReasonsStartAtZero(t *testing.T) {
	if got, want := testutil.CollectAndCount(policyDenials), len(denialReasons)+1; got != want {
		t.Fatalf("shadewright_policy_denials_total has %d series, want %d (every reason and Other)", got, want)
	}
}
