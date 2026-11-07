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
	"maps"
	"path/filepath"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/pkalin/shadewright/internal/testutil/metricguard"
)

// TestSymptomSourceWatchedIsBounded checks that the gauge's labels take their values from SourceKinds only
// (kinds that are not built in) and that it is documented.
func TestSymptomSourceWatchedIsBounded(t *testing.T) {
	var groups, kinds []string
	for _, gvk := range SourceKinds {
		if builtinKind(gvk) {
			continue
		}
		groups, kinds = append(groups, gvk.Group), append(kinds, gvk.Kind)
		setWatchedGauge(gvk, WatchNotGranted)
	}
	t.Cleanup(func() {
		for _, gvk := range SourceKinds {
			setWatchedGauge(gvk, WatchInactive)
		}
	})
	want := map[string]metricguard.Labels{"shadewright_analyzer_symptom_source_watched": {"group": groups, "kind": kinds}}
	// Registered by package analyzer, which the envtest harness imports; checked there.
	all := map[string]metricguard.Labels{"shadewright_analyzer_watched_namespaces": {}}
	maps.Copy(all, want)
	metricguard.Check(t, metrics.Registry, all)
	metricguard.Documented(t, filepath.Join("..", "..", "..", "docs", "reference", "metrics.md"), want)
}
