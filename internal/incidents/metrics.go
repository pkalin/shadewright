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
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/phasegauge"
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
// occurrences held unwritten were reached (Config.MaxPendingOccurrences,
// Config.MaxPendingOccurrencesPerIncident and Config.MaxPendingOccurrencesPerNamespace). It has no labels, so
// no namespace name reaches it. Their Incident is still written; only its count and occurrence records fall
// short.
var overflowedOccurrences = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "shadewright_incident_occurrences_overflow_total",
	Help: "Observations of a new occurrence not counted because the caps on occurrences held unwritten were reached.",
})

// firstSightingsHeld is the number of fingerprints held in memory as first sightings: seen once, with no
// Incident yet, waiting to be seen again (design 3.2, #267). It is at most Config.MaxFirstSightings.
var firstSightingsHeld = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "shadewright_incident_first_sightings_held",
	Help: "Symptoms seen once and held in memory, waiting to be seen again before they open an Incident.",
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
// read it. Each reports nothing while its value is unknown: the limit until the RemediationSettings are read,
// the count until the budget ConfigMap is read and trusted. A fail-closed pass clears what it could not read,
// so neither shows a value the trigger no longer goes by.
var (
	analysesUsedToday = newOptionalGauge("shadewright_analyses_used_today",
		"Analyses counted against the current UTC day in the budget ConfigMap, at the last trigger pass; "+
			"absent while the budget is unknown.")
	analysesPerDayLimit = newOptionalGauge("shadewright_analyses_per_day_limit",
		"The daily analysis limit of the RemediationSettings (spec.analysis.maxAnalysesPerDay, or the deprecated "+
			"spec.maxAnalysesPerDay when lower), at the last trigger pass; absent while the settings are unknown.")
)

// optionalGauge is a gauge without labels that reports nothing while its value is unknown. A plain Gauge
// cannot be taken away once set, so a stale value would stay on the endpoint.
type optionalGauge struct {
	desc  *prometheus.Desc
	mu    sync.Mutex
	value float64
	known bool
}

func newOptionalGauge(name, help string) *optionalGauge {
	return &optionalGauge{desc: prometheus.NewDesc(name, help, nil, nil)}
}

// set sets the value and makes it known.
func (g *optionalGauge) set(v float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.value, g.known = v, true
}

// clear makes the value unknown, so the gauge reports nothing.
func (g *optionalGauge) clear() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.value, g.known = 0, false
}

// get returns the value and whether it is known.
func (g *optionalGauge) get() (float64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.value, g.known
}

// Describe implements prometheus.Collector.
func (g *optionalGauge) Describe(ch chan<- *prometheus.Desc) {
	ch <- g.desc
}

// Collect implements prometheus.Collector.
func (g *optionalGauge) Collect(ch chan<- prometheus.Metric) {
	if v, ok := g.get(); ok {
		ch <- prometheus.MustNewConstMetric(g.desc, prometheus.GaugeValue, v)
	}
}

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
	metrics.Registry.MustRegister(refusedObservations, droppedBatches, overflowedOccurrences, firstSightingsHeld,
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

// incidentPhaseList are the phases incidentPhases reports by name, the enum of IncidentPhase.
var incidentPhaseList = []remediationv1alpha1.IncidentPhase{
	remediationv1alpha1.IncidentOpen,
	remediationv1alpha1.IncidentAnalyzing,
	remediationv1alpha1.IncidentProposed,
	remediationv1alpha1.IncidentEscalated,
	remediationv1alpha1.IncidentResolved,
}

// phaseListTimeout bounds the cache read of one scrape. The source is set only once the informer has synced
// (package phasegauge), so the read does not wait for the API server; the bound guards the endpoint anyway.
const phaseListTimeout = 5 * time.Second

// incidentCollector counts the Incidents in the operator namespace by phase, and those due for analysis, when
// the metrics are scraped. It reads the informer the controller already uses, and only on the leader once that
// informer has synced (phasegauge.Source), so a scrape makes no API call and starts no watch. Until then, on a
// standby replica, or when the read fails, it reports nothing: a missing series is visible, a zero would claim
// there are no Incidents.
type incidentCollector struct {
	phases    *prometheus.Desc
	due       *prometheus.Desc
	source    phasegauge.Source
	namespace atomic.Pointer[string]
}

// setNamespace sets the namespace the collector counts in.
func (c *incidentCollector) setNamespace(ns string) {
	c.namespace.Store(&ns)
}

// Describe implements prometheus.Collector.
func (c *incidentCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.phases
	ch <- c.due
}

// Collect implements prometheus.Collector.
func (c *incidentCollector) Collect(ch chan<- prometheus.Metric) {
	r, ns := c.source.Reader(), c.namespace.Load()
	if r == nil || ns == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), phaseListTimeout)
	defer cancel()
	var list remediationv1alpha1.IncidentList
	// The objects are only read, so the cache's copies are used as they are.
	if err := r.List(ctx, &list, client.InNamespace(*ns), client.UnsafeDisableDeepCopy); err != nil {
		logf.Log.WithName("metrics").Error(err, "Failed to list Incidents for the phase metric")
		return
	}
	counts := make(map[string]int, len(incidentPhaseList)+2)
	for _, l := range phasegauge.Labels(incidentPhaseList) {
		counts[l] = 0
	}
	due := 0
	for i := range list.Items {
		counts[phasegauge.Label(list.Items[i].Status.Phase, incidentPhaseList)]++
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
