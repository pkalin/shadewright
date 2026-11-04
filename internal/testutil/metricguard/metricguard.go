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
// Package metricguard checks, in tests, that Shadewright's Prometheus metrics keep bounded label sets and are
// documented. A label whose values come from the cluster (a name, a UID, a message) would make one series per
// object and let whoever creates objects grow the scrape without bound, so every label value must come from a
// closed set the test names.
package metricguard

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// Prefix is the name prefix of Shadewright's own metrics.
const Prefix = "shadewright_"

// Labels are the labels a metric may have, each with the values it may take. A metric without labels has an
// empty Labels.
type Labels map[string][]string

// Check gathers g and reports on t every metric family named with [Prefix] that is not in want, every family
// in want that g does not have, every label of a metric that its entry in want does not name, and every label
// value not in that label's list. Labels a metric does not carry are not required: a metric may have fewer.
func Check(t testing.TB, g prometheus.Gatherer, want map[string]Labels) {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Errorf("gathering the metrics: %v", err)
		return
	}
	seen := map[string]bool{}
	for _, f := range families {
		name := f.GetName()
		if !strings.HasPrefix(name, Prefix) {
			continue
		}
		seen[name] = true
		allowed, ok := want[name]
		if !ok {
			t.Errorf("metric %s is not in the table of bounded metrics", name)
			continue
		}
		unknown := map[string]bool{}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				values, ok := allowed[l.GetName()]
				switch {
				case !ok && !unknown[l.GetName()]:
					unknown[l.GetName()] = true
					t.Errorf("metric %s has the label %s, which is not in its table", name, l.GetName())
				case !ok:
				case !slices.Contains(values, l.GetValue()):
					t.Errorf("metric %s has %s=%q, which is not in the label's closed set", name, l.GetName(), l.GetValue())
				}
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if !seen[name] {
			t.Errorf("metric %s is in the table but not registered (or reports nothing)", name)
		}
	}
}

// Documented reports on t every name in want that the Markdown file at path does not show as code
// (`name`).
func Documented(t testing.TB, path string, want map[string]Labels) {
	t.Helper()
	doc, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("reading the metrics reference: %v", err)
		return
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if !strings.Contains(string(doc), "`"+name+"`") {
			t.Errorf("metric %s is not documented in %s", name, path)
		}
	}
}
