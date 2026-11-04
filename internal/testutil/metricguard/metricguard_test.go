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
package metricguard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// Names of the test registry's metrics, a label and its values.
const (
	thingsName  = "shadewright_things"
	otherName   = "shadewright_other_total"
	phaseLabel  = "phase"
	openPhase   = "Open"
	clusterName = "name-from-the-cluster"
)

// recorder is a T that records errors instead of failing.
type recorder struct {
	testing.TB
	errors []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recorder) Helper() {}

func registry(t *testing.T) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	byPhase := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: thingsName, Help: "h"}, []string{phaseLabel})
	byPhase.WithLabelValues(openPhase).Set(1)
	byPhase.WithLabelValues(clusterName).Set(1)
	other := prometheus.NewCounter(prometheus.CounterOpts{Name: otherName, Help: "h"})
	foreign := prometheus.NewCounter(prometheus.CounterOpts{Name: "go_like_total", Help: "h"})
	reg.MustRegister(byPhase, other, foreign)
	return reg
}

func TestCheck(t *testing.T) {
	cases := map[string]struct {
		want map[string]Labels
		errs []string
	}{
		"bounded": {
			want: map[string]Labels{
				thingsName: {phaseLabel: {openPhase, clusterName}},
				otherName:  {},
			},
		},
		"value outside the set": {
			want: map[string]Labels{
				thingsName: {phaseLabel: {openPhase}},
				otherName:  {},
			},
			errs: []string{thingsName + ` has phase="` + clusterName + `"`},
		},
		"label not in the table": {
			want: map[string]Labels{
				thingsName: {},
				otherName:  {},
			},
			errs: []string{"shadewright_things has the label phase"},
		},
		"metric not in the table": {
			want: map[string]Labels{thingsName: {phaseLabel: {openPhase, clusterName}}},
			errs: []string{"shadewright_other_total is not in the table"},
		},
		"metric not registered": {
			want: map[string]Labels{
				thingsName:         {phaseLabel: {openPhase, clusterName}},
				otherName:          {},
				"shadewright_gone": {},
			},
			errs: []string{"shadewright_gone is in the table but not registered"},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := &recorder{TB: t}
			Check(r, registry(t), c.want)
			if len(r.errors) != len(c.errs) {
				t.Fatalf("errors %q, want %d matching %q", r.errors, len(c.errs), c.errs)
			}
			for i, e := range c.errs {
				if !strings.Contains(r.errors[i], e) {
					t.Errorf("error %q does not say %q", r.errors[i], e)
				}
			}
		})
	}
}

func TestDocumented(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.md")
	if err := os.WriteFile(path, []byte("| `shadewright_things` | gauge |\nshadewright_other_total without code\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &recorder{TB: t}
	Documented(r, path, map[string]Labels{thingsName: {}, otherName: {}})
	if len(r.errors) != 1 || !strings.Contains(r.errors[0], "shadewright_other_total is not documented") {
		t.Fatalf("errors %q, want one for shadewright_other_total", r.errors)
	}
}
