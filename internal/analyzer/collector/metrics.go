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

package collector

import (
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// symptomSourceWatched is 1 for a kind of [SourceKinds] that an active module declares a source for and that
// is watched, 0 for one that is not (the condition SymptomSourcesNotWatched says why), and has no series for a
// kind no active module declares or whose optional grant the chart leaves out on purpose (WatchOmitted). Its
// labels take their values from SourceKinds only, a closed set compiled in, so the number of series is bounded
// whatever the cluster holds. Built-in kinds (the HPA, the Pod) are always watched and have no series.
var symptomSourceWatched = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "shadewright_analyzer_symptom_source_watched",
	Help: "1 when a kind an active module reads symptoms from is watched, 0 when it is not (not served, not granted or failed).",
}, kindLabels)

// kindLabels are the labels of a metric about a kind: its API group and kind.
var kindLabels = []string{"group", "kind"}

func init() {
	metrics.Registry.MustRegister(symptomSourceWatched)
}

// setWatchedGauge records a kind's watch state in symptomSourceWatched.
func setWatchedGauge(gvk schema.GroupVersionKind, s WatchState) {
	switch s {
	case WatchInactive, WatchOmitted:
		symptomSourceWatched.DeleteLabelValues(gvk.Group, gvk.Kind)
	case WatchWatched:
		symptomSourceWatched.WithLabelValues(gvk.Group, gvk.Kind).Set(1)
	default:
		symptomSourceWatched.WithLabelValues(gvk.Group, gvk.Kind).Set(0)
	}
}

// subjectOwnerForbidden counts the reads of a declared subject owner that the API server refused (Forbidden), each
// leaving the watched object itself as the subject (sourceReconciler.subject). Its labels name
// the owner kind a compiled-in module declares (modules.SubjectOwner), a closed set.
var subjectOwnerForbidden = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "shadewright_analyzer_subject_owner_forbidden_total",
	Help: "Reads of a symptom's declared owner refused as Forbidden, so the watched object is the subject.",
}, kindLabels)

func init() {
	metrics.Registry.MustRegister(subjectOwnerForbidden)
}
