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
	"encoding/json"
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/analysis"
	"github.com/shadewright-labs/shadewright/internal/providers/model/anthropic"
)

// testTriageModel is the triage model ID of the test.
const testTriageModel = "claude-sonnet-5-5"

// clearModelEnv unsets every setting of a real-model run for the test.
func clearModelEnv(t *testing.T) {
	t.Helper()
	for _, n := range append([]string{envModel, envKeyFile, envTriageMod, envEscMod, envForceEsc, envEffort,
		envAllowance}, foundryOnly...) {
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
	if m.Provider != providerAnthropic || m.Models["triage"].Model != testTriageModel || m.Anthropic == nil ||
		*m.Anthropic != (manifestAnthropic{Effort: anthropic.DefaultEffort,
			ThinkingAllowance: anthropic.DefaultThinkingAllowance}) {
		t.Errorf("manifest provider %q, settings %+v, models %+v", m.Provider, m.Anthropic, m.Models)
	}
}

// The effort and thinking allowance are checked, reach the manifest, change its digest, and show in the report.
func TestAnthropicSourceSettings(t *testing.T) {
	clearModelEnv(t)
	t.Setenv(envTriageMod, testTriageModel)
	t.Setenv(envKeyFile, "/nonexistent/key")
	for n, v := range map[string]string{envEffort: "minimal", envAllowance: "0"} {
		t.Setenv(n, v)
		if _, err := newAnthropicSource(); err == nil {
			t.Errorf("%s=%s was accepted", n, v)
		}
		t.Setenv(n, "")
	}
	s, err := loadSource()
	if err != nil {
		t.Fatal(err)
	}
	src, err := newAnthropicSource()
	if err != nil {
		t.Fatal(err)
	}
	m, err := buildManifest(s, src, nil, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envEffort, "high")
	t.Setenv(envAllowance, "8192")
	if src, err = newAnthropicSource(); err != nil {
		t.Fatal(err)
	}
	other, err := buildManifest(s, src, nil, 1, 1)
	if err != nil || *other.Anthropic != (manifestAnthropic{Effort: "high", ThinkingAllowance: 8192}) ||
		other.Digest == m.Digest {
		t.Errorf("settings %+v, digest unchanged %v (%v)", other.Anthropic, other.Digest == m.Digest, err)
	}
	if md := (&report{Mode: providerAnthropic, Manifest: other}).markdown(); !strings.Contains(md,
		"- Effort high; thinking allowance 8192 tokens above the output limit\n") {
		t.Errorf("the report does not name the settings:\n%s", md)
	}
}

// The manifests of fake and Foundry runs have no provider field and no provider settings, so their JSON, and
// with it their digests, are those they had before the Anthropic provider existed.
func TestManifestOfFakeAndFoundryLeavesTheProviderOut(t *testing.T) {
	s, err := loadSource()
	if err != nil {
		t.Fatal(err)
	}
	triage, escalation := replaySource{}.deployments()
	foundry := &foundrySource{triage: triage, escalation: escalation, triageModel: "gpt-4.1-mini",
		escModel: "gpt-4.1"}
	for name, models := range map[string]modelSource{providerFake: replaySource{}, providerFoundry: foundry} {
		m, err := buildManifest(s, models, nil, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"provider", "anthropic"} {
			if _, ok := fields[k]; ok {
				t.Errorf("%s: the manifest has the field %s, which changes its digest", name, k)
			}
		}
	}
}
