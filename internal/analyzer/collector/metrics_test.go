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

	"github.com/pkalin/shadewright/internal/modules"
	"github.com/pkalin/shadewright/internal/modules/cloudflared"
	"github.com/pkalin/shadewright/internal/modules/flux"
	"github.com/pkalin/shadewright/internal/modules/keda"
	"github.com/pkalin/shadewright/internal/modules/kyverno"
	"github.com/pkalin/shadewright/internal/testutil/metricguard"
)

// TestSymptomSourceWatchedIsBounded checks that the gauge's labels take their values from SourceKinds only
// (kinds that are not built in), the owner counter's from the owner kinds the modules declare, and that both
// are documented.
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
	// The owner kinds the compiled-in modules declare, the only values of the owner counter's labels.
	var ownerGroups, ownerKinds []string
	for _, m := range []modules.Integration{cloudflared.Module(), flux.Module(), keda.Module(), kyverno.Module()} {
		for _, src := range m.Hooks().Symptoms {
			if src.Subject.IsSet() {
				ownerGroups, ownerKinds = append(ownerGroups, src.Subject.Group), append(ownerKinds, src.Subject.Kind)
				subjectOwnerForbidden.WithLabelValues(src.Subject.Group, src.Subject.Kind).Add(0)
			}
		}
	}
	byKind := func(groups, kinds []string) metricguard.Labels {
		return metricguard.Labels{kindLabels[0]: groups, kindLabels[1]: kinds}
	}
	want := map[string]metricguard.Labels{
		"shadewright_analyzer_symptom_source_watched":        byKind(groups, kinds),
		"shadewright_analyzer_subject_owner_forbidden_total": byKind(ownerGroups, ownerKinds),
	}
	// Registered by package analyzer, which the envtest harness imports; checked there.
	all := map[string]metricguard.Labels{"shadewright_analyzer_watched_namespaces": {}}
	maps.Copy(all, want)
	metricguard.Check(t, metrics.Registry, all)
	metricguard.Documented(t, filepath.Join("..", "..", "..", "docs", "reference", "metrics.md"), want)
}
