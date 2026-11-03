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

func init() {
	metrics.Registry.MustRegister(refusedObservations)
}

// boundedSet remembers at most n strings, forgetting the oldest first.
type boundedSet struct {
	n     int
	items map[string]struct{}
	order []string
}

func newBoundedSet(n int) *boundedSet {
	return &boundedSet{n: n, items: make(map[string]struct{}, n)}
}

// add remembers s and reports whether it was new.
func (b *boundedSet) add(s string) bool {
	if _, ok := b.items[s]; ok {
		return false
	}
	if len(b.order) >= b.n {
		delete(b.items, b.order[0])
		b.order = b.order[1:]
	}
	b.items[s] = struct{}{}
	b.order = append(b.order, s)
	return true
}
