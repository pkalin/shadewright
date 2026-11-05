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

const (
	fluxKustomizeGroup = "kustomize.toolkit.fluxcd.io"
	kindScaledObject   = "ScaledObject"
	wantSubjectGV      = "subject group version"
	fixedSummary       = "A fixed summary."
)

// ownedSource is a valid source whose symptoms are about a controller owner.
func ownedSource() SymptomSource {
	s := validSource()
	s.Subject = SubjectOwner{Group: "keda.sh", Version: "v1alpha1", Kind: kindScaledObject, Resource: "scaledobjects"}
	return s
}

func TestSubjectOwnerValidate(t *testing.T) {
	if err := ownedSource().Validate(); err != nil {
		t.Fatalf("a valid subject owner is refused: %v", err)
	}
	core := ownedSource()
	core.Subject.Group, core.Subject.Kind, core.Subject.Resource = "", "ConfigMap", "configmaps"
	if err := core.Validate(); err != nil {
		t.Errorf("a core group subject owner is refused: %v", err)
	}
	cases := []struct {
		name   string
		change func(s *SymptomSource)
		want   string
	}{
		{"only Claims set", func(s *SymptomSource) {
			s.Subject = SubjectOwner{Claims: func(map[string]any, string) Claim { return ClaimOwned }}
		}, wantSubjectGV},
		{"no version", func(s *SymptomSource) { s.Subject.Version = "" }, wantSubjectGV},
		{"version with a group", func(s *SymptomSource) { s.Subject.Version = "keda.sh/v1alpha1" }, wantSubjectGV},
		{"bad group", func(s *SymptomSource) { s.Subject.Group = "Keda" }, wantSubjectGV},
		{"lower-case kind", func(s *SymptomSource) { s.Subject.Kind = "scaledObject" }, "subject kind"},
		{"empty resource", func(s *SymptomSource) { s.Subject.Resource = "" }, "subject resource"},
		{"resource with a slash", func(s *SymptomSource) { s.Subject.Resource = "scaledobjects/status" }, "subject resource"},
		{"the watched kind itself", func(s *SymptomSource) {
			s.Subject.Group, s.Subject.Kind = s.Group, s.Kind
		}, "watched kind itself"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := ownedSource()
			c.change(&s)
			if err := s.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Validate = %v, want an error saying %q", err, c.want)
			}
		})
	}
}

// TestSourceForNeedsAnActiveModule checks that a source is found only while its module is enabled and
// detected.
func TestSourceForNeedsAnActiveModule(t *testing.T) {
	withSource := keda
	withSource.hooks = Hooks{Symptoms: []SymptomSource{ownedSource()}}
	r := mustNew(t, nil, []Integration{withSource})
	detected := r.Detect(nil, []string{kedaGV})
	cases := []struct {
		name    string
		enabled Enabled
		d       Detection
		want    bool
	}{
		{"enabled and detected", Enabled{Integrations: []string{kedaName}}, detected, true},
		{"detected only", Enabled{}, detected, false},
		{"enabled only", Enabled{Integrations: []string{kedaName}}, Detection{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := r.Select(c.enabled, c.d)
			module, src, ok := s.SourceFor(fluxKustomizeGroup, "Kustomization")
			if ok != c.want {
				t.Fatalf("SourceFor found %v, want %v", ok, c.want)
			}
			if ok && (module != kedaName || src.Subject.Kind != kindScaledObject) {
				t.Errorf("SourceFor = %q %+v", module, src.Subject)
			}
			if _, _, other := s.SourceFor(fluxKustomizeGroup, "HelmRelease"); other {
				t.Error("SourceFor found a kind no module watches")
			}
		})
	}
	var zero Selection
	if _, _, ok := zero.SourceFor(fluxKustomizeGroup, "Kustomization"); ok {
		t.Error("the zero selection has a source")
	}
}

func TestEvaluateSourceRedactsAndBounds(t *testing.T) {
	secret := "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJlc2lnbmF0dXJl"
	src := validSource()
	src.Symptoms = func(map[string]any) []Symptom {
		return []Symptom{
			{Reason: "Stalled", Summary: fixedSummary, Evidence: []Evidence{
				{Field: "status.conditions.Ready.message", Value: secret},
				{Field: "status.long", Value: strings.Repeat("x ", 2000)},
			}},
			{Reason: "not CamelCase", Summary: fixedSummary},
			{Reason: "NoSummary"},
			{Reason: "TooMuch", Summary: fixedSummary, Evidence: make([]Evidence, MaxEvidence+1)},
		}
	}
	got := EvaluateSource(src, map[string]any{})
	if len(got) != 1 || got[0].Reason != "Stalled" {
		t.Fatalf("EvaluateSource = %+v, want only the valid symptom", got)
	}
	if v := got[0].Evidence[0].Value; strings.Contains(v, "eyJ") || strings.Contains(v, "c2lnbmF0dXJl") {
		t.Errorf("a token reached the evidence: %q", v)
	}
	if v := got[0].Evidence[1].Value; len(v) > MaxEvidenceValueLength {
		t.Errorf("evidence value of %d bytes, at most %d", len(v), MaxEvidenceValueLength)
	}
}

func TestEvaluateSourceSurvivesAPanic(t *testing.T) {
	src := validSource()
	src.Symptoms = func(map[string]any) []Symptom { panic("module bug") }
	if got := EvaluateSource(src, map[string]any{}); got != nil {
		t.Errorf("EvaluateSource = %+v, want nothing", got)
	}
	if got := EvaluateSource(SymptomSource{}, nil); got != nil {
		t.Errorf("EvaluateSource without a function = %+v", got)
	}
}
