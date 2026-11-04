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

//nolint:goconst // the tables spell their values out to read as recorded objects
package modules

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/api/v1alpha1"
)

// aksWithRule is the aks stub with one event rule, so its hooks can be seen in the selection.
var aksWithRule = rulePlatform{stubPlatform: aks, rules: []DiagnosisRule{{Name: "aks-rule",
	Event: func(Event) (Symptom, bool) { return Symptom{Reason: "AksSymptom", Summary: "s"}, true }}}}

func activeNames(s Selection) []string {
	active := s.Active()
	out := make([]string, 0, len(active))
	for _, m := range active {
		out = append(out, m.Name)
	}
	return out
}

func TestSelectNeedsEnabledAndDetected(t *testing.T) {
	r := mustNew(t, []Platform{aksWithRule, eks}, []Integration{keda, flux})
	onAKS := r.Detect([]Node{{ProviderID: azureID, Labels: map[string]string{aksLabel: ""}}},
		[]string{kedaGV, appsGV, kustomizeGV})
	cases := []struct {
		name         string
		enabled      Enabled
		d            Detection
		wantPlatform v1alpha1.PlatformStatus
		wantActive   []string
		wantInts     []v1alpha1.IntegrationStatus
	}{
		{"nothing enabled, nothing detected", Enabled{}, Detection{},
			v1alpha1.PlatformStatus{Active: v1alpha1.GenericPlatform}, []string{GenericPlatform}, nil},
		{"nothing enabled on AKS with add-ons: detection enables nothing", Enabled{}, onAKS,
			v1alpha1.PlatformStatus{Detected: aksName, Active: v1alpha1.GenericPlatform}, []string{GenericPlatform},
			[]v1alpha1.IntegrationStatus{
				{Name: fluxName, Detected: true, Version: kustomizeGV},
				{Name: kedaName, Detected: true, Version: kedaGV},
			}},
		{"enabled and detected", Enabled{Platforms: []string{eksName, aksName}, Integrations: []string{kedaName}}, onAKS,
			v1alpha1.PlatformStatus{Detected: aksName, Active: aksName}, []string{GenericPlatform, aksName, kedaName},
			[]v1alpha1.IntegrationStatus{
				{Name: fluxName, Detected: true, Version: kustomizeGV},
				{Name: kedaName, Detected: true, Version: kedaGV, Active: true},
			}},
		{"enabled but not detected", Enabled{Platforms: []string{aksName}, Integrations: []string{kedaName, fluxName}},
			Detection{}, v1alpha1.PlatformStatus{Active: v1alpha1.GenericPlatform}, []string{GenericPlatform},
			[]v1alpha1.IntegrationStatus{{Name: fluxName}, {Name: kedaName}}},
		{"generic detected", Enabled{Platforms: []string{aksName}}, Detection{Platform: r.DetectPlatform(nil)},
			v1alpha1.PlatformStatus{Active: v1alpha1.GenericPlatform}, []string{GenericPlatform}, nil},
	}
	for _, c := range cases {
		s := r.Select(c.enabled, c.d)
		if got := s.PlatformStatus(); got != c.wantPlatform {
			t.Errorf("%s: platform = %+v, want %+v", c.name, got, c.wantPlatform)
		}
		if got := activeNames(s); !slices.Equal(got, c.wantActive) {
			t.Errorf("%s: active = %v, want %v", c.name, got, c.wantActive)
		}
		if got := s.IntegrationStatus(); !reflect.DeepEqual(got, c.wantInts) {
			t.Errorf("%s: integrations = %+v, want %+v", c.name, got, c.wantInts)
		}
		aksFound := false
		for _, f := range s.EvaluateEvent(Event{}) {
			aksFound = aksFound || f.Module == aksName
		}
		if want := slices.Contains(c.wantActive, aksName); aksFound != want {
			t.Errorf("%s: the aks rule ran = %v, want %v", c.name, aksFound, want)
		}
	}
}

func TestSelectReportsUnknownNames(t *testing.T) {
	r := mustNew(t, []Platform{aks}, []Integration{keda})
	s := r.Select(Enabled{Platforms: []string{"gke", aksName, "gke"}, Integrations: []string{"kedda", kedaName}},
		r.Detect(nil, []string{kedaGV}))
	p, i := s.Unknown()
	if !slices.Equal(p, []string{"gke"}) || !slices.Equal(i, []string{"kedda"}) {
		t.Errorf("Unknown = %v, %v", p, i)
	}
	msg := s.UnknownMessage()
	if !strings.Contains(msg, "platforms gke") || !strings.Contains(msg, "integrations kedda") {
		t.Errorf("UnknownMessage = %q", msg)
	}
	want := []v1alpha1.IntegrationStatus{{Name: kedaName, Detected: true, Version: kedaGV, Active: true}, {Name: "kedda"}}
	if got := s.IntegrationStatus(); !reflect.DeepEqual(got, want) {
		t.Errorf("integrations = %+v, want %+v", got, want)
	}
	if msg := r.Select(Enabled{Platforms: []string{aksName}}, Detection{}).UnknownMessage(); msg != "" {
		t.Errorf("known names only: UnknownMessage = %q", msg)
	}
}

func TestSelectIgnoresModulesOfAnotherRegistry(t *testing.T) {
	r := mustNew(t, nil, nil)
	other := mustNew(t, []Platform{aks}, []Integration{keda})
	d := other.Detect([]Node{{ProviderID: azureID, Labels: map[string]string{aksLabel: ""}}}, []string{kedaGV})
	s := r.Select(Enabled{Platforms: []string{aksName}, Integrations: []string{kedaName}}, d)
	if s.DetectedPlatform() != "" || s.ActivePlatform() != GenericPlatform || !slices.Equal(activeNames(s), []string{GenericPlatform}) {
		t.Errorf("selection = %s/%s %v, want generic only", s.DetectedPlatform(), s.ActivePlatform(), activeNames(s))
	}
}

func TestUnknownMessageIsCapped(t *testing.T) {
	r := mustNew(t, nil, nil)
	names := make([]string, 0, MaxIntegrations)
	for i := range MaxIntegrations {
		names = append(names, fmt.Sprintf("integration-%02d-%s", i, strings.Repeat("x", 45)))
	}
	msg := r.Select(Enabled{Integrations: names}, Detection{}).UnknownMessage()
	if len(msg) > MaxUnknownMessage || !strings.HasSuffix(msg, "(and more)") || !strings.Contains(msg, names[0]) {
		t.Errorf("message of %d bytes: %q", len(msg), msg)
	}
}

// hooksSummary is what a test can compare of hooks: the fields of every element that is not a function.
func hooksSummary(h Hooks) string {
	var b strings.Builder
	for _, s := range h.Symptoms {
		fmt.Fprintf(&b, "symptom %s %v %s %s %s;", s.Group, s.Versions, s.Kind, s.Resource, s.ReportingController)
	}
	for _, r := range h.Rules {
		fmt.Fprintf(&b, "rule %s;", r.Name)
	}
	fmt.Fprintf(&b, "tools %d;actions %v;", len(h.Tools), h.Actions)
	for _, c := range h.Checks {
		fmt.Fprintf(&b, "check %s %s %s;", c.Name, c.Group, c.Kind)
	}
	return b.String()
}

// TestActiveReturnsCopies checks that a caller that changes every slice of what Active returned changes
// neither the selection nor the registry.
func TestActiveReturnsCopies(t *testing.T) {
	full := stubIntegration{
		name:      kedaName,
		detection: IntegrationDetection{GroupVersions: []string{kedaGV}},
		hooks: Hooks{
			Symptoms: []SymptomSource{validSource()},
			Rules: []DiagnosisRule{{Name: "keda-rule",
				Event: func(Event) (Symptom, bool) { return Symptom{}, false }}},
			Tools:   []Tool{{}},
			Actions: []v1alpha1.ActionType{v1alpha1.ActionAdjustKedaMaxReplicas},
			Checks:  []VerificationCheck{validCheck()},
		},
	}
	r := mustNew(t, []Platform{aksWithRule}, []Integration{full})
	d := r.Detect([]Node{{ProviderID: azureID, Labels: map[string]string{aksLabel: ""}}}, []string{kedaGV})
	s := r.Select(Enabled{Platforms: []string{aksName}, Integrations: []string{kedaName}}, d)

	first := s.Active()
	if got := activeNames(s); !slices.Equal(got, []string{GenericPlatform, aksName, kedaName}) {
		t.Fatalf("active = %v, want generic, aks and keda", got)
	}
	want := make([]string, len(first))
	for i, m := range first {
		want[i] = hooksSummary(m.Hooks)
	}
	registryWant := hooksSummary(full.hooks)

	for i := range first {
		h := &first[i].Hooks
		for j := range h.Symptoms {
			h.Symptoms[j].Group, h.Symptoms[j].Kind = "mutated", "Mutated"
			for k := range h.Symptoms[j].Versions {
				h.Symptoms[j].Versions[k] = "mutated"
			}
		}
		for j := range h.Rules {
			h.Rules[j].Name = "mutated"
		}
		// Tool has no fields yet, so changing an element shows nothing; its slice is copied all the same.
		for j := range h.Actions {
			h.Actions[j] = "Mutated"
		}
		for j := range h.Checks {
			h.Checks[j].Name, h.Checks[j].Kind = "Mutated", "Mutated"
		}
	}

	second := s.Active()
	if len(second) != len(want) {
		t.Fatalf("second Active has %d modules, want %d", len(second), len(want))
	}
	for i, m := range second {
		if got := hooksSummary(m.Hooks); got != want[i] {
			t.Errorf("module %s: a caller changed the selection's hooks:\n got %s\nwant %s", m.Name, got, want[i])
		}
	}
	if h, _ := r.IntegrationHooks(kedaName); hooksSummary(h) != registryWant {
		t.Errorf("a caller changed the registry's hooks: %s", hooksSummary(h))
	}
}
