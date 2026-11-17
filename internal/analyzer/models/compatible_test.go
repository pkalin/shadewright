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
	"errors"
	"testing"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/providers/model/openaicompatible"
)

func TestCompatibleFactory(t *testing.T) {
	cfg := ModelsConfig{DisableFoundry: true, CompatibleSuffixes: []string{".api.openai.com", gatewaySuffix, ".api.anthropic.com", ".services.ai.azure.com"}}
	factory, err := NewModelFactory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := remediationv1alpha1.ModelDeployment{Provider: remediationv1alpha1.ModelProviderOpenAICompatible, Endpoint: "https://api.openai.com/v1", Deployment: "gpt-6.1-sol", InferenceAPI: remediationv1alpha1.InferenceAPIResponses}
	p, err := factory(analysis.RoleTriage, d)
	if _, ok := p.(*openaicompatible.Provider); err != nil || !ok {
		t.Fatalf("compatible-only deployment: %T %v", p, err)
	}
	d.Provider = remediationv1alpha1.ModelProviderFoundry
	if _, err := factory(analysis.RoleTriage, d); !errors.Is(err, analysis.ErrProviderNotConfigured) {
		t.Fatalf("Foundry without Azure: %v", err)
	}
	d.Provider = remediationv1alpha1.ModelProviderOpenAICompatible
	for _, endpoint := range []string{"http://api.openai.com/v1", "https://api.openai.com:8443/v1", "https://api.openai.com.evil.test/v1", "https://api.anthropic.com", "https://r.services.ai.azure.com"} {
		d.Endpoint = endpoint
		if _, err := factory(analysis.RoleTriage, d); err == nil {
			t.Errorf("endpoint accepted: %s", endpoint)
		}
	}
	cfg.CompatibleKeyFile, cfg.CompatibleKeyHost = "/mounted/key", "api.openai.com"
	factory, err = NewModelFactory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d.Endpoint = "https://gateway.example.com/v1"
	if _, err := factory(analysis.RoleTriage, d); err == nil {
		t.Fatal("key moved to another allowed host")
	}
	cfg.DisableFoundry, cfg.Tokens = false, failingTokens{t}
	cfg.Suffixes = []string{foundrySuffix}
	factory, err = NewModelFactory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d.Provider, d.Endpoint, d.APIVersion = remediationv1alpha1.ModelProviderFoundry, "https://api.openai.com/v1", "v1"
	if _, err := factory(analysis.RoleTriage, d); !errors.Is(err, analysis.ErrEndpointNotAllowed) {
		t.Fatalf("Entra token could reach compatible key host: %v", err)
	}
}
