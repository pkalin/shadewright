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

package chart

import (
	"slices"
	"testing"
)

// TestEgressSelectorPodLabelsReplaceTheDefault: a networkPolicy.analyzer.{prometheus,traceCollector}.podLabels
// value replaces the default selector whole instead of merging with it (Helm merges maps, so a merged default
// would require labels no Pod has and match nothing; #985). Empty keeps the documented default.
func TestEgressSelectorPodLabelsReplaceTheDefault(t *testing.T) {
	const (
		promDefault = `{"app.kubernetes.io/component":"server","app.kubernetes.io/name":"prometheus"}`
		colDefault  = `{"app.kubernetes.io/name":"opentelemetry-collector"}`
	)
	selectors := map[string]struct {
		on         []string
		valuesPath string
		port       int
		def        string
	}{
		"prometheus":      {promOn, "networkPolicy.analyzer.prometheus.podLabels.", promPort, promDefault},
		"trace collector": {collectorOn, "networkPolicy.analyzer.traceCollector.podLabels.", collectorPort, colDefault},
	}
	for name, s := range selectors {
		cases := map[string]struct {
			set  []string
			want string
		}{
			"empty default": {nil, s.def},
			"one label":     {[]string{s.valuesPath + "app=mine"}, `{"app":"mine"}`},
			"two labels":    {[]string{s.valuesPath + "app=mine", s.valuesPath + "team=a"}, `{"app":"mine","team":"a"}`},
			// A null key is ignored: the workaround of an install that nulled the default key (#985).
			"null key and a label": {[]string{s.valuesPath + `app\.kubernetes\.io/name=null`, s.valuesPath + "app=otel"}, `{"app":"otel"}`},
			"all keys null":        {[]string{s.valuesPath + `app\.kubernetes\.io/name=null`}, s.def},
		}
		for cn, c := range cases {
			t.Run(name+"/"+cn, func(t *testing.T) {
				r := defaultRender(t, slices.Concat(s.on, c.set)...)
				var found []string
				for _, rule := range nonDNSRules(t, analyzerEgressKey, r) {
					if len(rule.Ports) == 1 && rule.Ports[0].Port.IntValue() == s.port {
						found = append(found, asJSON(t, rule.To[0].PodSelector.MatchLabels))
					}
				}
				if len(found) != 1 || found[0] != c.want {
					t.Errorf("selector labels %v, want exactly %s", found, c.want)
				}
			})
		}
	}
}
