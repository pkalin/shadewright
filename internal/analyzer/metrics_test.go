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
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/pkalin/shadewright/internal/testutil/metricguard"
)

// TestWatchedNamespacesGauge checks that the gauge reports the count, 0 included, has no labels and is
// documented.
func TestWatchedNamespacesGauge(t *testing.T) {
	for _, n := range []int{0, 3} {
		SetWatchedNamespaces(n)
		if got := testutil.ToFloat64(watchedNamespaces); got != float64(n) {
			t.Errorf("gauge = %v after SetWatchedNamespaces(%d)", got, n)
		}
	}
	want := map[string]metricguard.Labels{"shadewright_analyzer_watched_namespaces": {}}
	metricguard.Check(t, metrics.Registry, want)
	metricguard.Documented(t, filepath.Join("..", "..", "docs", "reference", "metrics.md"), want)
}
