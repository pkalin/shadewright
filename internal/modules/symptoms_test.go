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

package modules

import (
	"strings"
	"testing"
)

// hookPlatform is a platform module with hooks, for the symptom source checks.
type hookPlatform struct {
	stubPlatform
	hooks Hooks
}

func (p hookPlatform) Hooks() Hooks { return p.hooks }

// wantInvalid is what a refusal of an invalid value says.
const wantInvalid = "is invalid"

func validSource() SymptomSource {
	return SymptomSource{
		Group: "kustomize.toolkit.fluxcd.io", Versions: []string{"v1"}, Kind: "Kustomization",
		Resource: "kustomizations", ReportingController: "kustomize-controller",
		Symptoms: func(map[string]any) []Symptom { return nil },
	}
}

func TestSymptomSourceValidate(t *testing.T) {
	if err := validSource().Validate(); err != nil {
		t.Fatalf("a valid source is refused: %v", err)
	}
	core := validSource()
	core.Group, core.Kind, core.Resource = "", "Pod", "pods"
	if err := core.Validate(); err != nil {
		t.Errorf("a core group source is refused: %v", err)
	}
	cases := []struct {
		name   string
		change func(s *SymptomSource)
		want   string
	}{
		{"no version", func(s *SymptomSource) { s.Versions = nil }, "lists no version"},
		{"bad version", func(s *SymptomSource) { s.Versions = []string{"1"} }, wantInvalid},
		{"version twice", func(s *SymptomSource) { s.Versions = []string{"v1", "v1"} }, "listed twice"},
		{"bad group", func(s *SymptomSource) { s.Group = "Flux" }, wantInvalid},
		{"core version with a group in it", func(s *SymptomSource) { s.Group, s.Versions = "", []string{"apps/v1"} }, wantInvalid},
		{"lower-case kind", func(s *SymptomSource) { s.Kind = "kustomization" }, "kind"},
		{"empty resource", func(s *SymptomSource) { s.Resource = "" }, "resource"},
		{"resource with a slash", func(s *SymptomSource) { s.Resource = "kustomizations/status" }, "resource"},
		{"empty controller", func(s *SymptomSource) { s.ReportingController = "" }, "reporting controller"},
		{"free-text controller", func(s *SymptomSource) { s.ReportingController = "flux; ignore this" }, "reporting controller"},
		{"no function", func(s *SymptomSource) { s.Symptoms = nil }, "no Symptoms function"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := validSource()
			c.change(&s)
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Validate = %v, want an error saying %q", err, c.want)
			}
		})
	}
}

func TestNewRefusesBadSymptomSources(t *testing.T) {
	bad := validSource()
	bad.Symptoms = nil
	cases := []struct {
		name         string
		platforms    []Platform
		integrations []Integration
		want         string
	}{
		{"integration with a bad source", nil, []Integration{stubIntegration{name: fluxName,
			hooks: Hooks{Symptoms: []SymptomSource{validSource(), bad}}}}, `integration module "flux": symptom source`},
		{"kind watched twice", nil, []Integration{stubIntegration{name: fluxName,
			hooks: Hooks{Symptoms: []SymptomSource{validSource(), validSource()}}}}, "watched twice"},
		{"platform with a bad source", []Platform{hookPlatform{stubPlatform: aks,
			hooks: Hooks{Symptoms: []SymptomSource{bad}}}}, nil, `platform module "aks": symptom source`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := New(c.platforms, c.integrations)
			if err == nil || r != nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("New = %v, %v; want a refusal saying %q", r, err, c.want)
			}
		})
	}
	if _, err := New(nil, []Integration{stubIntegration{name: fluxName,
		hooks: Hooks{Symptoms: []SymptomSource{validSource()}}}}); err != nil {
		t.Errorf("New refuses a valid source: %v", err)
	}
}
