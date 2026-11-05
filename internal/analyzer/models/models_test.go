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

package models

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
)

// foundrySuffix is the Microsoft Foundry host suffix of these tests.
const foundrySuffix = ".services.ai.azure.com"

func TestParseEndpointSuffixes(t *testing.T) {
	got, err := ParseEndpointSuffixes(".services.ai.azure.com,.openai.azure.com,.cognitiveservices.azure.com")
	if err != nil || !slices.Equal(got, []string{foundrySuffix, ".openai.azure.com", ".cognitiveservices.azure.com"}) {
		t.Fatalf("defaults: %v, %v", got, err)
	}
	if got, err := ParseEndpointSuffixes(" .a.example , .a.example,"); err != nil || len(got) != 1 {
		t.Errorf("duplicates and spaces: %v, %v", got, err)
	}
	for _, bad := range []string{"", ",", ".com", "azure.com", ".Azure.com", "..azure.com", ".azure..com", ".azure.com.",
		".-a.com", ".a_b.com", ".a.com/x", "*.azure.com"} {
		if _, err := ParseEndpointSuffixes(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestCheckEndpoint(t *testing.T) {
	suffixes := []string{foundrySuffix, ".openai.azure.com", ".lab.westeurope.example.com"}
	for _, ok := range []string{
		"https://lab.services.ai.azure.com/",
		"https://lab.services.ai.azure.com",
		"https://lab.openai.azure.com/openai/v1",
		"https://lab.openai.azure.com:443/",
		"https://lab.westeurope.example.com/",
	} {
		if err := CheckEndpoint(ok, suffixes); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://lab.services.ai.azure.com/",
		"https://labservices.ai.azure.com/",
		"https://lab.services.ai.azure.com.evil.example/",
		"https://lab.services.ai.azure.com:8443/",
		"https://user" + "@lab.services.ai.azure.com/", // user info, split so it does not read as an address
		"https://lab.services.ai.azure.com/?x=1",
		"https://lab.services.ai.azure.com/#f",
		"https://LAB.services.ai.azure.com/",
		"https://lab.services.ai.azure.com./",
		"https://lab.cognitiveservices.azure.com/",
		"https://westeurope.example.com/",
		"https://evil.lab.westeurope.example.co/",
		"https://10.0.0.1/",
		"lab.services.ai.azure.com",
		"",
	} {
		err := CheckEndpoint(bad, suffixes)
		if !errors.Is(err, analysis.ErrEndpointNotAllowed) {
			t.Errorf("%q: %v, want ErrEndpointNotAllowed", bad, err)
		} else if strings.Contains(err.Error(), "lab") || strings.Contains(err.Error(), "evil") {
			t.Errorf("%q: the error repeats the endpoint: %v", bad, err)
		}
	}
}

func TestParseActionPolicies(t *testing.T) {
	got, err := ParseActionPolicies("SetContainerMemoryLimit=memory, AdjustKedaMaxReplicas=keda-limits")
	if err != nil || got[remediationv1alpha1.ActionSetContainerMemoryLimit] != "memory" ||
		got[remediationv1alpha1.ActionAdjustKedaMaxReplicas] != "keda-limits" || len(got) != 2 {
		t.Fatalf("%v, %v", got, err)
	}
	if got, err := ParseActionPolicies(""); err != nil || len(got) != 0 {
		t.Errorf("empty: %v, %v", got, err)
	}
	for _, bad := range []string{"SetContainerMemoryLimit", "Exec=all", "setcontainermemorylimit=x",
		"SetContainerMemoryLimit=a,SetContainerMemoryLimit=b", "SetContainerMemoryLimit=Bad_Name", "SetContainerMemoryLimit="} {
		if _, err := ParseActionPolicies(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestParseModelName(t *testing.T) {
	for _, ok := range []string{"", "gpt-4.1-mini-2025-04-14", "model:1/a_b"} {
		if _, err := ParseModelName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"-gpt", "gpt 4", "gpt\n", strings.Repeat("a", 129)} {
		if _, err := ParseModelName(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// tokenSpy counts the tokens asked for.
type tokenSpy struct{ n int }

func (s *tokenSpy) Token(context.Context) (string, error) {
	s.n++
	return "", errors.New("no token in tests")
}

// A refused endpoint gets no provider, so no request and no token; an allowed one gets a provider bound to it.
func TestModelFactoryChecksTheEndpointFirst(t *testing.T) {
	if _, err := NewModelFactory(ModelsConfig{}); err == nil {
		t.Error("a factory without suffixes or tokens was built")
	}
	spy := &tokenSpy{}
	f, err := NewModelFactory(ModelsConfig{Suffixes: []string{foundrySuffix}, Tokens: spy, Log: logr.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	d := remediationv1alpha1.ModelDeployment{Endpoint: "https://attacker.example/", Deployment: "small", APIVersion: "v1"}
	if p, err := f(analysis.RoleTriage, d); !errors.Is(err, analysis.ErrEndpointNotAllowed) || p != nil {
		t.Errorf("a refused endpoint: %v, %v", p, err)
	}
	d.Endpoint = "https://lab.services.ai.azure.com/"
	if p, err := f(analysis.RoleTriage, d); err != nil || p == nil {
		t.Errorf("an allowed endpoint: %v, %v", p, err)
	}
	if spy.n != 0 {
		t.Errorf("building providers asked for %d tokens", spy.n)
	}
}
