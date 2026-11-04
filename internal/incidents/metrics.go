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
	"slices"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// The analyzer's metrics, served on its metrics endpoint (docs/reference/metrics.md). Every label takes its
// values from a closed set fixed in this file: never a name, fingerprint, namespace or message from the
// cluster, so the number of series stays bounded whatever the cluster holds.

// refusedObservations counts the observations of new fingerprints that were not stored because the
// Incident cap was reached. It is served on the analyzer's metrics endpoint.
var refusedObservations = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "shadewright_incident_observations_refused_total",
	Help: "Observations of a new fingerprint not stored because the cap on Incidents was reached.",
})

// Reasons of droppedBatches.
const (
	dropInvalid             = "invalid"
	dropFingerprintConflict = "fingerprint_conflict"
	dropStopped             = "stopped"
)

// droppedBatches counts the batches of observations that were not stored, by reason: the API server refused
// them as invalid, an Incident of the expected name holds another fingerprint, or the process stopped
// before they could be written.
var droppedBatches = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_incident_batches_dropped_total",
	Help: "Batches of observations not stored in an Incident, by reason (invalid, fingerprint_conflict, stopped).",
}, []string{"reason"})

// overflowedOccurrences counts the observations of an occurrence that were not counted because the caps on
// occurrences held unwritten were reached (Config.MaxPendingOccurrences and
// Config.MaxPendingOccurrencesPerIncident). Their Incident is still written; only its count and occurrence
// records fall short.
var overflowedOccurrences = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "shadewright_incident_occurrences_overflow_total",
	Help: "Observations of a new occurrence not counted because the caps on occurrences held unwritten were reached.",
})

// Results of analysisReservations.
const (
	reserveReserved       = "reserved"
	reserveBudgetSpent    = "budget_spent"
	reserveBudgetNotFound = "budget_not_found"
	reserveBudgetBad      = "budget_unreadable"
	reserveError          = "error"
)

// analysisReservations counts the calls of [ReserveAnalysis], by result: a unit of the daily budget was
// taken (the model may be called), the budget was spent, its ConfigMap is missing or cannot be trusted, or
// another error (a failed read or write). Only reserved allows a model call.
var analysisReservations = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_analysis_reservations_total",
	Help: "Reservations of a unit of the daily analysis budget, by result " +
		"(reserved, budget_spent, budget_not_found, budget_unreadable, error).",
}, []string{"result"})

// analysesUsedToday and analysesPerDayLimit are the analysis budget as the last pass of the analysis trigger
// read it. They are set only when both the settings and the budget ConfigMap could be read and trusted.
var (
	analysesUsedToday = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "shadewright_analyses_used_today",
		Help: "Analyses counted against the current UTC day in the budget ConfigMap, at the last trigger pass.",
	})
	analysesPerDayLimit = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "shadewright_analyses_per_day_limit",
		Help: "RemediationSettings maxAnalysesPerDay, at the last trigger pass.",
	})
)

// triggerPassErrors counts the passes of the analysis trigger that failed (each is retried at the next
// interval).
var triggerPassErrors = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "shadewright_analysis_trigger_errors_total",
	Help: "Passes of the analysis trigger that failed and are retried at the next interval.",
})

// incidentWriteErrors counts the writes of an Incident that failed and are retried. Writes the API server
// refuses for good are counted in droppedBatches instead.
var incidentWriteErrors = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "shadewright_incident_write_errors_total",
	Help: "Writes of an Incident that failed and are retried.",
})

// incidentPhases reports the Incidents in the operator namespace by phase, and those due for analysis.
var incidentPhases = &incidentCollector{
	phases: prometheus.NewDesc("shadewright_incidents",
		"Incidents in the operator namespace, by phase (None without one, Other for a phase this version "+
			"does not know).", []string{"phase"}, nil),
	due: prometheus.NewDesc("shadewright_incidents_analysis_due",
		"Incidents in the operator namespace whose AnalysisDue condition is True.", nil, nil),
}

func init() {
	metrics.Registry.MustRegister(refusedObservations, droppedBatches, overflowedOccurrences,
		analysisReservations, analysesUsedToday, analysesPerDayLimit, triggerPassErrors, incidentWriteErrors,
		incidentPhases)
	// The closed sets start at zero, so a rate is defined before the first event.
	for _, r := range []string{dropInvalid, dropFingerprintConflict, dropStopped} {
		droppedBatches.WithLabelValues(r)
	}
	for _, r := range []string{reserveReserved, reserveBudgetSpent, reserveBudgetNotFound, reserveBudgetBad,
		reserveError} {
		analysisReservations.WithLabelValues(r)
	}
}

// recordReservation counts a result of [ReserveAnalysis].
func recordReservation(err error) {
	result := reserveError
	switch {
	case err == nil:
		result = reserveReserved
	case errors.Is(err, ErrBudgetSpent):
		result = reserveBudgetSpent
	case errors.Is(err, ErrBudgetNotFound):
		result = reserveBudgetNotFound
	case errors.Is(err, ErrBudgetUnreadable):
		result = reserveBudgetBad
	}
	analysisReservations.WithLabelValues(result).Inc()
}

// Phase labels outside the enum of IncidentPhase.
const (
	labelNoPhase = "None"
	labelOther   = "Other"
)

// incidentPhaseList are the phases incidentPhases reports, the enum of IncidentPhase.
var incidentPhaseList = []remediationv1alpha1.IncidentPhase{
	remediationv1alpha1.IncidentOpen,
	remediationv1alpha1.IncidentAnalyzing,
	remediationv1alpha1.IncidentProposed,
	remediationv1alpha1.IncidentEscalated,
	remediationv1alpha1.IncidentResolved,
}

// incidentPhaseLabel returns the phase label of an Incident: the phase when it is in incidentPhaseList, None
// when it has none, and Other otherwise.
func incidentPhaseLabel(p remediationv1alpha1.IncidentPhase) string {
	if p == "" {
		return labelNoPhase
	}
	if slices.Contains(incidentPhaseList, p) {
		return string(p)
	}
	return labelOther
}

// phaseListTimeout bounds the cache read of one scrape, so a cache that is not synced yet cannot hold the
// metrics endpoint.
const phaseListTimeout = 5 * time.Second

// incidentSource is what incidentCollector lists from.
type incidentSource struct {
	reader    client.Reader
	namespace string
}

// incidentCollector counts the Incidents by phase, and those due for analysis, when the metrics are scraped.
// It reads the manager's cache, which the controller already fills with the Incidents, so a scrape costs no
// API call. Until a source is set, or when the read fails, it reports nothing: a missing series is visible, a
// zero would claim there are no Incidents.
type incidentCollector struct {
	phases *prometheus.Desc
	due    *prometheus.Desc
	source atomic.Pointer[incidentSource]
}

// setSource sets the reader and namespace the collector lists from.
func (c *incidentCollector) setSource(r client.Reader, namespace string) {
	c.source.Store(&incidentSource{reader: r, namespace: namespace})
}

// Describe implements prometheus.Collector.
func (c *incidentCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.phases
	ch <- c.due
}

// Collect implements prometheus.Collector.
func (c *incidentCollector) Collect(ch chan<- prometheus.Metric) {
	src := c.source.Load()
	if src == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), phaseListTimeout)
	defer cancel()
	var list remediationv1alpha1.IncidentList
	// The objects are only read, so the cache's copies are used as they are.
	if err := src.reader.List(ctx, &list, client.InNamespace(src.namespace), client.UnsafeDisableDeepCopy); err != nil {
		logf.Log.WithName("metrics").Error(err, "Failed to list Incidents for the phase metric")
		return
	}
	counts := make(map[string]int, len(incidentPhaseList)+2)
	for _, p := range incidentPhaseList {
		counts[string(p)] = 0
	}
	counts[labelNoPhase], counts[labelOther] = 0, 0
	due := 0
	for i := range list.Items {
		counts[incidentPhaseLabel(list.Items[i].Status.Phase)]++
		if meta.IsStatusConditionTrue(list.Items[i].Status.Conditions, ConditionAnalysisDue) {
			due++
		}
	}
	for phase, n := range counts {
		ch <- prometheus.MustNewConstMetric(c.phases, prometheus.GaugeValue, float64(n), phase)
	}
	ch <- prometheus.MustNewConstMetric(c.due, prometheus.GaugeValue, float64(due))
}

// boundedSet remembers at most n strings, forgetting the oldest first. The order is a ring, so a forgotten
// string is overwritten and not kept alive by the backing array.
type boundedSet struct {
	items map[string]struct{}
	ring  []string
	next  int
}

func newBoundedSet(n int) *boundedSet {
	n = max(n, 1)
	return &boundedSet{items: make(map[string]struct{}, n), ring: make([]string, 0, n)}
}

// add remembers s and reports whether it was new.
func (b *boundedSet) add(s string) bool {
	if _, ok := b.items[s]; ok {
		return false
	}
	if len(b.ring) < cap(b.ring) {
		b.ring = append(b.ring, s)
	} else {
		delete(b.items, b.ring[b.next])
		b.ring[b.next] = s
		b.next = (b.next + 1) % len(b.ring)
	}
	b.items[s] = struct{}{}
	return true
}
