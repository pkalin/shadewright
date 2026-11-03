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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/api/v1alpha1"
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
		{"kind watched by two integrations", nil, []Integration{
			stubIntegration{name: fluxName, hooks: Hooks{Symptoms: []SymptomSource{validSource()}}},
			stubIntegration{name: kedaName, hooks: Hooks{Symptoms: []SymptomSource{validSource()}}},
		}, `watched twice, by integration module "flux" and integration module "keda"`},
		{"kind watched by a platform and an integration", []Platform{hookPlatform{stubPlatform: aks,
			hooks: Hooks{Symptoms: []SymptomSource{validSource()}}}},
			[]Integration{stubIntegration{name: fluxName, hooks: Hooks{Symptoms: []SymptomSource{validSource()}}}},
			`watched twice, by platform module "aks" and integration module "flux"`},
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

// changingIntegration returns hooks backed by slices it keeps, so a test can change them after New.
type changingIntegration struct {
	stubIntegration
	sources []SymptomSource
}

func (c changingIntegration) Hooks() Hooks { return Hooks{Symptoms: c.sources} }

func TestRegistryKeepsTheValidatedHooks(t *testing.T) {
	sources := []SymptomSource{validSource()}
	in := changingIntegration{stubIntegration: stubIntegration{name: fluxName}, sources: sources}
	r := mustNew(t, nil, []Integration{in})
	sources[0].Symptoms = nil
	sources[0].Versions[0] = "not a version"

	h, ok := r.IntegrationHooks(fluxName)
	if !ok || len(h.Symptoms) != 1 {
		t.Fatalf("IntegrationHooks = %#v, %v", h, ok)
	}
	if err := h.Symptoms[0].Validate(); err != nil {
		t.Errorf("the module changed the registry's hooks after New: %v", err)
	}
	h.Symptoms[0].Versions[0] = "v9"
	if again, _ := r.IntegrationHooks(fluxName); again.Symptoms[0].Versions[0] != "v1" {
		t.Errorf("a caller changed the registry's hooks: versions %v", again.Symptoms[0].Versions)
	}

	p := hookPlatform{stubPlatform: aks, hooks: Hooks{Symptoms: []SymptomSource{validSource()}}}
	r = mustNew(t, []Platform{p}, nil)
	p.hooks.Symptoms[0].Versions[0] = "not a version"
	ph, ok := r.PlatformHooks(aksName)
	if !ok || ph.Symptoms[0].Versions[0] != "v1" {
		t.Errorf("PlatformHooks = %#v, %v; want the copy taken by New", ph, ok)
	}
	ph.Symptoms[0].Versions[0] = "v9"
	if again, _ := r.PlatformHooks(aksName); again.Symptoms[0].Versions[0] != "v1" {
		t.Errorf("a caller changed the registry's platform hooks: versions %v", again.Symptoms[0].Versions)
	}
	if _, ok := r.IntegrationHooks("missing"); ok {
		t.Error("IntegrationHooks found a module that is not registered")
	}
}

// TestRegistryKeepsTheChecks checks that the verification checks a module declares are copied like the rest
// of its hooks: neither the module nor a caller can change the registry's list after New.
func TestRegistryKeepsTheChecks(t *testing.T) {
	const kedaReady = v1alpha1.CheckKedaReady
	holds := func(map[string]any) bool { return true }
	checks := []VerificationCheck{{Name: kedaReady, Group: kedaGroup, Kind: "ScaledObject", Holds: holds}}
	r := mustNew(t, nil, []Integration{stubIntegration{name: kedaName, hooks: Hooks{Checks: checks}}})
	checks[0].Name = "Changed"

	h, ok := r.IntegrationHooks(kedaName)
	if !ok || len(h.Checks) != 1 || h.Checks[0].Name != kedaReady || h.Checks[0].Holds == nil {
		t.Fatalf("IntegrationHooks = %#v, %v; want the KedaReady check as declared", h, ok)
	}
	h.Checks[0].Name = "Changed"
	if again, _ := r.IntegrationHooks(kedaName); again.Checks[0].Name != kedaReady {
		t.Errorf("a caller changed the registry's checks: %#v", again.Checks)
	}
}

// validCheck is a verification check New accepts.
func validCheck() VerificationCheck {
	return VerificationCheck{Name: v1alpha1.CheckKedaReady, Group: kedaGroup, Kind: "ScaledObject",
		Holds: func(map[string]any) bool { return true }}
}

func TestNewRefusesInvalidChecks(t *testing.T) {
	cases := []struct {
		name   string
		change func(c *VerificationCheck)
		want   string
	}{
		{"name outside the API", func(c *VerificationCheck) { c.Name = "KedaHappy" }, "not one of the API's"},
		{"empty name", func(c *VerificationCheck) { c.Name = "" }, "not one of the API's"},
		{"no group", func(c *VerificationCheck) { c.Group = "" }, "no group or kind"},
		{"no kind", func(c *VerificationCheck) { c.Kind = "" }, "no group or kind"},
		{"no Holds", func(c *VerificationCheck) { c.Holds = nil }, "no Holds function"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			check := validCheck()
			c.change(&check)
			_, err := New(nil, []Integration{stubIntegration{name: kedaName, hooks: Hooks{Checks: []VerificationCheck{check}}}})
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), `integration module "keda"`) {
				t.Errorf("New = %v, want an error naming the module and %q", err, c.want)
			}
		})
	}
	twice := []struct {
		name         string
		platforms    []Platform
		integrations []Integration
	}{
		{"one module", nil, []Integration{stubIntegration{name: kedaName, hooks: Hooks{Checks: []VerificationCheck{validCheck(), validCheck()}}}}},
		{"two integrations", nil, []Integration{
			stubIntegration{name: kedaName, hooks: Hooks{Checks: []VerificationCheck{validCheck()}}},
			stubIntegration{name: fluxName, hooks: Hooks{Checks: []VerificationCheck{validCheck()}}},
		}},
		{"a platform and an integration", []Platform{hookPlatform{stubPlatform: aks, hooks: Hooks{Checks: []VerificationCheck{validCheck()}}}},
			[]Integration{stubIntegration{name: kedaName, hooks: Hooks{Checks: []VerificationCheck{validCheck()}}}}},
	}
	for _, c := range twice {
		t.Run("declared twice by "+c.name, func(t *testing.T) {
			if _, err := New(c.platforms, c.integrations); err == nil || !strings.Contains(err.Error(), "declared twice") {
				t.Errorf("New = %v, want a check declared twice refused", err)
			}
		})
	}
	if _, err := New(nil, []Integration{stubIntegration{name: kedaName, hooks: Hooks{Checks: []VerificationCheck{validCheck()}}}}); err != nil {
		t.Errorf("New refused a valid check: %v", err)
	}
}

// TestVerificationChecksMatchTheEnum checks that v1alpha1.VerificationChecks lists exactly the values of the
// VerificationCheck enum marker, in order, so New compares check names with what the API accepts.
func TestVerificationChecksMatchTheEnum(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "api", "v1alpha1", "remediationrequest_types.go"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "type VerificationCheck string") && i > 0 {
			enum, ok := strings.CutPrefix(strings.TrimSpace(lines[i-1]), "// +kubebuilder:validation:Enum=")
			if !ok {
				t.Fatalf("no enum marker above VerificationCheck: %q", lines[i-1])
			}
			got := make([]string, 0, len(v1alpha1.VerificationChecks()))
			for _, c := range v1alpha1.VerificationChecks() {
				got = append(got, string(c))
			}
			if strings.Join(got, ";") != enum {
				t.Errorf("VerificationChecks() = %v, the enum is %s", got, enum)
			}
			return
		}
	}
	t.Fatal("type VerificationCheck not found")
}
