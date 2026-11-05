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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/phasegauge"
	"github.com/pkalin/shadewright/internal/testutil/metricguard"
)

// reservationResults are the values of the result label of analysisReservations.
var reservationResults = []string{reserveReserved, reserveBudgetSpent, reserveBudgetNotFound, reserveBudgetBad, reserveError}

// reservations returns the count of analysisReservations for each result.
func reservations() map[string]float64 {
	out := map[string]float64{}
	for _, r := range reservationResults {
		out[r] = testutil.ToFloat64(analysisReservations.WithLabelValues(r))
	}
	return out
}

// checkReservations reports every result whose count changed since before by other than want says (absent:
// unchanged).
func checkReservations(t *testing.T, before map[string]float64, want map[string]float64) {
	t.Helper()
	after := reservations()
	for _, r := range reservationResults {
		if d := after[r] - before[r]; d != want[r] {
			t.Errorf("result %s counted %v more times, want %v", r, d, want[r])
		}
	}
}

// Each result of ReserveAnalysis is counted under its own label value, and under no other.
func TestReserveAnalysisIsCounted(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 1)
	createBudget(t, ns, nil)
	before := reservations()
	if err := ReserveAnalysis(t.Context(), env.admin, env.admin, ns, triggerNow); err != nil {
		t.Fatal(err)
	}
	if err := ReserveAnalysis(t.Context(), env.admin, env.admin, ns, triggerNow); !errors.Is(err, ErrBudgetSpent) {
		t.Fatalf("a second reservation of 1 = %v, want ErrBudgetSpent", err)
	}
	if err := ReserveAnalysis(t.Context(), env.admin, env.admin, namespace(t), triggerNow); !errors.Is(err, ErrBudgetNotFound) {
		t.Fatalf("a reservation without the ConfigMap = %v, want ErrBudgetNotFound", err)
	}
	checkReservations(t, before, map[string]float64{reserveReserved: 1, reserveBudgetSpent: 1, reserveBudgetNotFound: 1})
}

func TestRecordReservationSortsErrors(t *testing.T) {
	cases := map[string]error{
		reserveReserved:       nil,
		reserveBudgetSpent:    fmt.Errorf("reserving: %w", ErrBudgetSpent),
		reserveBudgetNotFound: fmt.Errorf("reserving: %w", ErrBudgetNotFound),
		reserveBudgetBad:      fmt.Errorf("reserving: %w", ErrBudgetUnreadable),
		reserveError:          apierrors.NewServiceUnavailable("down"),
	}
	for result, err := range cases {
		before := reservations()
		recordReservation(err)
		checkReservations(t, before, map[string]float64{result: 1})
	}
}

// pollUntil polls cond until it holds or 20 seconds pass.
func pollUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// gaugeIs reports whether g is known with value v.
func gaugeIs(g *optionalGauge, v float64) bool {
	got, ok := g.get()
	return ok && got == v
}

// gaugeUnknown reports whether g reports nothing, on the endpoint too.
func gaugeUnknown(g *optionalGauge) bool {
	_, ok := g.get()
	return !ok && testutil.CollectAndCount(g) == 0
}

// A pass of the analysis trigger reports the budget it read.
func TestTriggerReportsTheBudget(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 7)
	createBudget(t, ns, usedToday(3))
	analysesUsedToday.clear()
	analysesPerDayLimit.clear()
	start(t, ns, triggerConfig)
	pollUntil(t, "the budget gauges", func() bool {
		return gaugeIs(analysesUsedToday, 3) && gaugeIs(analysesPerDayLimit, 7)
	})
	if n := testutil.CollectAndCount(analysesUsedToday); n != 1 {
		t.Fatalf("the count reports %d series, want 1", n)
	}
}

// A pass that cannot trust the budget ConfigMap takes the count it reported before away, and reports the limit
// it read from the settings.
func TestTriggerClearsAnUntrustedBudget(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 7)
	createBudget(t, ns, budgetData(tomorrow, "1")) // a day in the future: not trusted
	analysesUsedToday.set(5)
	analysesPerDayLimit.clear()
	start(t, ns, triggerConfig)
	pollUntil(t, "the count cleared and the limit set", func() bool {
		return gaugeUnknown(analysesUsedToday) && gaugeIs(analysesPerDayLimit, 7)
	})
}

// A pass without the settings takes both the limit and the count away.
func TestTriggerClearsTheBudgetWithoutSettings(t *testing.T) {
	ns := namespace(t)
	createBudget(t, ns, usedToday(3))
	analysesUsedToday.set(5)
	analysesPerDayLimit.set(9)
	start(t, ns, triggerConfig)
	pollUntil(t, "both gauges cleared", func() bool {
		return gaugeUnknown(analysesUsedToday) && gaugeUnknown(analysesPerDayLimit)
	})
}

// failingConfigMaps fails every read of a ConfigMap, so the trigger cannot read the budget.
type failingConfigMaps struct {
	client.Reader
}

func (f failingConfigMaps) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.ConfigMap); ok {
		return apierrors.NewServiceUnavailable("the API server is down")
	}
	return f.Reader.Get(ctx, key, obj, opts...)
}

// A failed pass of the analysis trigger is counted.
func TestTriggerCountsFailedPasses(t *testing.T) {
	ns := namespace(t)
	createSettings(t, 7)
	before := testutil.ToFloat64(triggerPassErrors)
	startWith(t, ns, triggerConfig, func(c *Controller, _ *countingClient) {
		c.reader = failingConfigMaps{Reader: env.admin}
	})
	pollUntil(t, "a failed pass counted", func() bool { return testutil.ToFloat64(triggerPassErrors) > before })
}

// A write of an Incident that fails and is retried is counted.
func TestFailedWriteIsCounted(t *testing.T) {
	ns := namespace(t)
	r := start(t, ns, Config{Window: 100 * time.Millisecond, RetryAfter: 50 * time.Millisecond})
	now := time.Now().Truncate(time.Second)
	r.obs <- immediate(observation(1, 1, 1, 1, now))
	eventually(t, ns, 1, countIs(1))
	before := testutil.ToFloat64(incidentWriteErrors)
	var once sync.Once
	r.writes.instead = func(context.Context) error {
		var err error
		once.Do(func() {
			err = apierrors.NewServerTimeout(remediationv1alpha1.GroupVersion.WithResource("incidents").GroupResource(), "update", 1)
		})
		return err
	}
	r.obs <- immediate(observation(1, 2, 2, 2, now))
	eventually(t, ns, 1, countIs(3))
	if got := testutil.ToFloat64(incidentWriteErrors) - before; got != 1 {
		t.Fatalf("%v failed writes counted, want 1", got)
	}
}

// incidentsIn returns a fake client holding, in namespace ns, Incidents with the given phases; those named in
// due have AnalysisDue True. One more Incident, in another namespace, must not be counted.
func incidentsIn(t *testing.T, ns string, due map[int]bool, phases ...remediationv1alpha1.IncidentPhase) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := remediationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := make([]client.Object, 0, len(phases)+1)
	objs = append(objs, &remediationv1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Namespace: "elsewhere", Name: "incident-other"},
		Status:     remediationv1alpha1.IncidentStatus{Phase: remediationv1alpha1.IncidentOpen},
	})
	for i, p := range phases {
		inc := &remediationv1alpha1.Incident{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: fmt.Sprintf("incident-%d", i)},
			Status:     remediationv1alpha1.IncidentStatus{Phase: p},
		}
		if due[i] {
			inc.Status.Conditions = []metav1.Condition{{Type: ConditionAnalysisDue, Status: metav1.ConditionTrue,
				Reason: ReasonFirstAnalysis, LastTransitionTime: metav1.Now()}}
		}
		objs = append(objs, inc)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestIncidentCollector(t *testing.T) {
	c := &incidentCollector{phases: incidentPhases.phases, due: incidentPhases.due}
	c.setNamespace("operator")
	if n := testutil.CollectAndCount(c); n != 0 {
		t.Fatalf("a collector without a source reported %d series, want none", n)
	}
	c.source.Set(incidentsIn(t, "operator", map[int]bool{0: true, 1: true},
		remediationv1alpha1.IncidentOpen, remediationv1alpha1.IncidentOpen, remediationv1alpha1.IncidentAnalyzing,
		"", "Something-From the cluster"))
	want := map[string]float64{}
	for _, l := range phasegauge.Labels(incidentPhaseList) {
		want[l] = 0
	}
	want[phasegauge.None], want[phasegauge.Other] = 1, 1
	want[string(remediationv1alpha1.IncidentOpen)] = 2
	want[string(remediationv1alpha1.IncidentAnalyzing)] = 1

	var exp strings.Builder
	exp.WriteString("# HELP shadewright_incidents " + metricguard.Help(t, c.phases) + "\n")
	exp.WriteString("# TYPE shadewright_incidents gauge\n")
	for _, phase := range slices.Sorted(maps.Keys(want)) {
		fmt.Fprintf(&exp, "shadewright_incidents{phase=%q} %v\n", phase, want[phase])
	}
	exp.WriteString("# HELP shadewright_incidents_analysis_due " + metricguard.Help(t, c.due) + "\n")
	exp.WriteString("# TYPE shadewright_incidents_analysis_due gauge\n")
	exp.WriteString("shadewright_incidents_analysis_due 2\n")
	if err := testutil.CollectAndCompare(c, strings.NewReader(exp.String())); err != nil {
		t.Fatal(err)
	}
}

// analyzerMetrics are the metrics of package incidents and the closed set of every label.
func analyzerMetrics() map[string]metricguard.Labels {
	return map[string]metricguard.Labels{
		"shadewright_incident_observations_refused_total": {},
		"shadewright_incident_batches_dropped_total": {
			"reason": {dropInvalid, dropFingerprintConflict, dropStopped},
		},
		"shadewright_incident_occurrences_overflow_total": {},
		"shadewright_incident_first_sightings_held":       {},
		"shadewright_incident_write_errors_total":         {},
		"shadewright_analysis_reservations_total":         {"result": reservationResults},
		"shadewright_analyses_used_today":                 {},
		"shadewright_analyses_per_day_limit":              {},
		"shadewright_analysis_trigger_errors_total":       {},
		"shadewright_incidents":                           {"phase": phasegauge.Labels(incidentPhaseList)},
		"shadewright_incidents_analysis_due":              {},
	}
}

// Every metric the analyzer's Incident controller registers is in the table above, takes label values only
// from closed sets, and is documented, whatever phase an Incident holds.
func TestAnalyzerMetricsAreBoundedAndDocumented(t *testing.T) {
	recordReservation(errors.New("free text"))
	incidentPhases.setNamespace("operator")
	incidentPhases.source.Set(incidentsIn(t, "operator", nil, remediationv1alpha1.IncidentOpen, "a phase from the cluster", ""))
	t.Cleanup(func() { incidentPhases.source.Set(nil) })
	analysesUsedToday.set(1)
	analysesPerDayLimit.set(2)

	metricguard.Check(t, metrics.Registry, analyzerMetrics())
	metricguard.Documented(t, filepath.Join("..", "..", "docs", "reference", "metrics.md"), analyzerMetrics())
}
