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

package eval

import (
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/providers/model/anthropic"
)

// testTriageModel is the triage model ID of the test.
const testTriageModel = "claude-sonnet-5-5"

// clearModelEnv unsets every setting of a real-model run for the test.
func clearModelEnv(t *testing.T) {
	t.Helper()
	for _, n := range append([]string{envModel, envKeyFile, envTriageMod, envEscMod, envForceEsc}, foundryOnly...) {
		t.Setenv(n, "")
	}
}

// The Anthropic source needs a model and a key file, refuses the Foundry-only settings, and binds each role
// to its own model; the manifest names the provider and the deployment is the model ID. No network call and
// no read of the key file happen here.
func TestAnthropicSource(t *testing.T) {
	clearModelEnv(t)
	if _, err := newAnthropicSource(); err == nil || !strings.Contains(err.Error(), envKeyFile) {
		t.Fatalf("err = %v, want the required settings named", err)
	}
	t.Setenv(envTriageMod, testTriageModel)
	t.Setenv(envKeyFile, "/nonexistent/key")
	t.Setenv(envEndpoint, "https://r.openai.azure.com/")
	if _, err := newAnthropicSource(); err == nil || !strings.Contains(err.Error(), envEndpoint) {
		t.Fatalf("err = %v, want %s refused", err, envEndpoint)
	}
	t.Setenv(envEndpoint, "")
	t.Setenv(envForceEsc, "1")
	if _, err := newAnthropicSource(); err == nil || !strings.Contains(err.Error(), envForceEsc) {
		t.Fatalf("err = %v, want a forced escalation without an escalation model refused", err)
	}
	t.Setenv(envForceEsc, "")
	t.Setenv(envEscMod, "claude-opus-5-5")
	src, err := newAnthropicSource()
	if err != nil {
		t.Fatal(err)
	}
	if want := map[analysis.Role]string{analysis.RoleTriage: testTriageModel,
		analysis.RoleEscalation: "claude-opus-5-5"}; len(src.expectedModels()) != 2 ||
		src.expectedModels()[analysis.RoleTriage] != want[analysis.RoleTriage] ||
		src.expectedModels()[analysis.RoleEscalation] != want[analysis.RoleEscalation] {
		t.Errorf("expected models %v", src.expectedModels())
	}
	factory, done, err := src.factory(nil)
	if err != nil || done != nil {
		t.Fatalf("factory: %v", err)
	}
	triage, escalation := src.deployments()
	if triage.Endpoint != anthropic.Endpoint || triage.Deployment != testTriageModel ||
		triage.APIVersion != anthropic.APIVersion || escalation.Deployment != "claude-opus-5-5" {
		t.Errorf("deployments %+v %+v", triage, escalation)
	}
	if p, err := factory(analysis.RoleTriage, *triage); err != nil || p != src.providers[analysis.RoleTriage] {
		t.Errorf("triage provider %v, %v", p, err)
	}
	if _, err := factory(analysis.RoleTriage, *escalation); err == nil {
		t.Error("the triage role accepted the escalation model's deployment")
	}

	s, err := loadSource()
	if err != nil {
		t.Fatal(err)
	}
	m, err := buildManifest(s, src, nil, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := buildManifest(s, replaySource{}, nil, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider != providerAnthropic || replay.Provider != providerFake || m.Models["triage"].Model != testTriageModel {
		t.Errorf("manifest provider %q, replay %q, models %+v", m.Provider, replay.Provider, m.Models)
	}
}
