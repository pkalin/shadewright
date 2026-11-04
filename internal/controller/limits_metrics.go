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
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics of the executor's limits (limits.go), served on the executor's metrics endpoint. Labels hold only the
// fixed values of waitReasons; no request, target or namespace is named.

// waitReasons maps the reasons of the limits to the values of the reason label, a closed set.
var waitReasons = map[string]string{
	ReasonTargetLocked:     "target_locked",
	ReasonConcurrencyLimit: "concurrency_limit",
	ReasonCircuitOpen:      "circuit_open",
	ReasonCircuitHalfOpen:  "circuit_half_open",
	ReasonLimitsUnknown:    "limits_unknown",
}

var (
	remediationsInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "shadewright_remediations_in_flight",
		Help: "RemediationRequests that hold a slot under maxConcurrentRemediations, as last counted before a request went through the policy check.",
	})
	concurrencyLimit = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "shadewright_remediation_concurrency_limit",
		Help: "maxConcurrentRemediations in force at the last check of the limits.",
	})
	remediationWaits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "shadewright_remediation_waits_total",
		Help: "RemediationRequests held back before the policy check, by reason (target_locked, concurrency_limit, circuit_open, circuit_half_open, limits_unknown).",
	}, []string{"reason"})
	breakerOpen = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "shadewright_circuit_breaker_open",
		Help: "1 while the circuit breaker is open or half-open after consecutive failed RemediationRequests, 0 while it is closed.",
	})
	breakerTrips = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "shadewright_circuit_breaker_trips_total",
		Help: "Times this process found the circuit breaker open after it had found it closed or half-open (or at its first check).",
	})
)

// breakerSeen is the state of the breaker the last check of the limits in this process found, plus one: zero
// before the first check.
var breakerSeen atomic.Int32

func init() {
	metrics.Registry.MustRegister(remediationsInFlight, concurrencyLimit, remediationWaits, breakerOpen, breakerTrips)
	// The closed set starts at zero, so a rate is defined before the first request is held back.
	for _, label := range waitReasons {
		remediationWaits.WithLabelValues(label)
	}
}

// recordLimits updates the metrics from one check of the limits. err is the error of the check; a request held
// back is counted with its reason.
func recordLimits(s limitsState, err error) {
	if err != nil {
		remediationWaits.WithLabelValues(waitReasons[ReasonLimitsUnknown]).Inc()
		return
	}
	remediationsInFlight.Set(float64(s.active))
	concurrencyLimit.Set(float64(s.limit))
	if s.circuit == circuitClosed {
		breakerOpen.Set(0)
	} else {
		breakerOpen.Set(1)
	}
	if seen := int32(s.circuit) + 1; breakerSeen.Swap(seen) != seen && s.circuit == circuitOpen {
		breakerTrips.Inc()
	}
	if label, ok := waitReasons[s.reason]; ok {
		remediationWaits.WithLabelValues(label).Inc()
	}
}
