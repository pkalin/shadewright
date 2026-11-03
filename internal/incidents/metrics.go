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
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

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

func init() {
	metrics.Registry.MustRegister(refusedObservations, droppedBatches)
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
