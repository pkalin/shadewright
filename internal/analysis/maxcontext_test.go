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

package analysis

import (
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// #981: maxContextBytes of the settings is the bound the analysis gives the context builder; unset it is
// 48 KiB, and a value outside the CRD's range is held to it.
func TestLimitsOfReadsMaxContextBytes(t *testing.T) {
	for _, c := range []struct {
		name string
		a    *remediationv1alpha1.AnalysisSettings
		want int
	}{
		{"no analysis settings", nil, 48 << 10},
		{"unset", &remediationv1alpha1.AnalysisSettings{}, 48 << 10},
		{"set", &remediationv1alpha1.AnalysisSettings{MaxContextBytes: ptr.To[int32](8192)}, 8192},
		{"below the range", &remediationv1alpha1.AnalysisSettings{MaxContextBytes: ptr.To[int32](0)}, 4096},
		{"above the range", &remediationv1alpha1.AnalysisSettings{MaxContextBytes: ptr.To[int32](1 << 20)}, 256 << 10},
	} {
		s := &remediationv1alpha1.RemediationSettings{Spec: remediationv1alpha1.RemediationSettingsSpec{Analysis: c.a}}
		if got := LimitsOf(s).MaxContextBytes; got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

// #981: the analysis builds the context within maxContextBytes of the RemediationSettings the API server
// stores: the set value, or the CRD's default of 49152 when the field is unset.
func TestControllerBuildsWithinMaxContextBytes(t *testing.T) {
	for _, c := range []struct {
		name string
		set  *int32
		want int
	}{
		{"unset", nil, 49152},
		{"set", ptr.To[int32](8192), 8192},
	} {
		t.Run(c.name, func(t *testing.T) {
			ns := namespace(t)
			setSettings(t, 10)
			s := &remediationv1alpha1.RemediationSettings{}
			if err := env.admin.Get(t.Context(), types.NamespacedName{Name: remediationv1alpha1.SettingsName}, s); err != nil {
				t.Fatal(err)
			}
			if c.set != nil {
				s.Spec.Analysis.MaxContextBytes = c.set
				if err := env.admin.Update(t.Context(), s); err != nil {
					t.Fatal(err)
				}
			}
			inc := dueIncident(t, ns, nil)
			f := newFixture()
			f.triage.steps = []step{reply(noProposal)}
			ctl, _ := newController(t, ns, f)
			reconcile(t, ctl, inc)
			if !slices.Equal(f.builder.totals, []int{c.want}) {
				t.Errorf("the builder was given %v, want [%d]", f.builder.totals, c.want)
			}
		})
	}
}
