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

package analyzer

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// watchedNamespaces is how many namespaces the analyzer was started with (--watch-namespaces). Zero is the
// chart's default and a valid setting (it watches nothing), so an alert on 0 is how a forgotten list is caught.
var watchedNamespaces = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "shadewright_analyzer_watched_namespaces",
	Help: "Namespaces the analyzer watches (--watch-namespaces). 0: it reads no events or pods and records no Incident.",
})

func init() {
	metrics.Registry.MustRegister(watchedNamespaces)
}

// SetWatchedNamespaces records the number of watched namespaces in the gauge
// shadewright_analyzer_watched_namespaces. The binary calls it once at start.
func SetWatchedNamespaces(n int) {
	watchedNamespaces.Set(float64(n))
}
