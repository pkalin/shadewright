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
	"os"
	"path/filepath"
	"testing"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/model/openaicompatible"
)

const compatibleModel = "gpt-6.1-sol"

const compatibleBase = "https://api.openai.com/v1"

func TestCompatibleFactory(t *testing.T) {
	cfg := ModelsConfig{DisableFoundry: true, CompatibleEndpoints: []openaicompatible.Endpoint{{URL: compatibleBase, API: openaicompatible.APIResponses, Models: []string{compatibleModel}}}}
	factory, err := NewModelFactory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := remediationv1alpha1.ModelDeployment{Provider: remediationv1alpha1.ModelProviderOpenAICompatible, Endpoint: compatibleBase, Deployment: compatibleModel, InferenceAPI: remediationv1alpha1.InferenceAPIResponses}
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
	cfg.CompatibleEndpoints[0].KeyFile = "/mounted/key"
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
	d.Provider, d.Endpoint, d.APIVersion = remediationv1alpha1.ModelProviderFoundry, compatibleBase, "v1"
	if _, err := factory(analysis.RoleTriage, d); !errors.Is(err, analysis.ErrEndpointNotAllowed) {
		t.Fatalf("Entra token could reach declared compatible host: %v", err)
	}
}

// A settings edit must not select a different account path on the same gateway.
func TestCompatibleDeclaredBaseBoundary(t *testing.T) {
	factory, err := NewModelFactory(ModelsConfig{DisableFoundry: true, CompatibleEndpoints: []openaicompatible.Endpoint{{URL: "https://gateway.example.com/tenant-a/v1", API: openaicompatible.APIChatCompletions, Models: []string{compatibleModel}, KeyFile: "/key"}}})
	if err != nil {
		t.Fatal(err)
	}
	d := remediationv1alpha1.ModelDeployment{Provider: remediationv1alpha1.ModelProviderOpenAICompatible, Endpoint: "https://gateway.example.com/tenant-b/v1", Deployment: "m"}
	if _, err := factory(analysis.RoleTriage, d); !errors.Is(err, analysis.ErrEndpointNotAllowed) {
		t.Fatalf("settings moved credential to another tenant: %v", err)
	}
}
func TestCompatibleDeclarationStartupRefusals(t *testing.T) {
	for _, host := range []string{"gateway.example.com", "api.anthropic.com", "r.openai.azure.com"} {
		_, err := NewModelFactory(ModelsConfig{DisableFoundry: true, Suffixes: []string{".gateway.example.com"}, CompatibleEndpoints: []openaicompatible.Endpoint{{URL: "https://" + host + "/v1", API: openaicompatible.APIChatCompletions, Models: []string{compatibleModel}}}})
		if err == nil {
			t.Errorf("invalid declaration survived startup: %s", host)
		}
	}
}

func TestCompatibleUndeclaredModel(t *testing.T) {
	factory, err := NewModelFactory(ModelsConfig{DisableFoundry: true, CompatibleEndpoints: []openaicompatible.Endpoint{{URL: compatibleBase, API: openaicompatible.APIChatCompletions, Models: []string{"m"}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = factory(analysis.RoleTriage, remediationv1alpha1.ModelDeployment{Provider: remediationv1alpha1.ModelProviderOpenAICompatible, Endpoint: compatibleBase, Deployment: "undeclared"})
	if !errors.Is(err, analysis.ErrModelNotAllowed) {
		t.Fatalf("wrong model refusal: %v", err)
	}
}

// The factory gives each compatible provider the other declared key files: a declared key file that is
// missing at start and appears later as a link to the Anthropic key file is refused at its read, before
// any request (decision 7 of #834).
func TestCompatibleFactoryPassesOtherKeyFiles(t *testing.T) {
	dir := t.TempDir()
	anthropicKey := filepath.Join(dir, "anthropic")
	if err := os.WriteFile(anthropicKey, []byte("content-marker-834-models-0001"), 0o600); err != nil {
		t.Fatal(err)
	}
	late := filepath.Join(dir, "late")
	factory, err := NewModelFactory(ModelsConfig{DisableFoundry: true, AnthropicKeyFile: anthropicKey,
		CompatibleEndpoints: []openaicompatible.Endpoint{{URL: "https://gateway.invalid/v1",
			API: openaicompatible.APIChatCompletions, Models: []string{compatibleModel}, KeyFile: late}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(anthropicKey, late); err != nil {
		t.Fatal(err)
	}
	p, err := factory(analysis.RoleTriage, remediationv1alpha1.ModelDeployment{
		Provider: remediationv1alpha1.ModelProviderOpenAICompatible, Endpoint: "https://gateway.invalid/v1", Deployment: compatibleModel})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Complete(t.Context(), providers.CompletionRequest{Messages: []providers.Message{{Role: providers.RoleUser, Content: "x"}}})
	if !errors.Is(err, providers.ErrCredentialUnavailable) || !providers.IsNotSent(err) || providers.IsRetryable(err) {
		t.Fatalf("the Anthropic key reached a compatible provider's read: %v", err)
	}
}
