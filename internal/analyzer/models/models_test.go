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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/model/anthropic"
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

// anthropicModel and smallModel are model IDs of these tests.
const (
	anthropicModel = "claude-haiku-5-5"
	smallModel     = "small"
)

// failingTokens fails the test when a token is asked for.
type failingTokens struct{ t *testing.T }

func (f failingTokens) Token(context.Context) (string, error) {
	f.t.Error("a workload identity token was requested")
	return "", errors.New("no token in tests")
}

// The Foundry path refuses the Anthropic API's host even when the allow-list names it, before a provider is
// built, so no workload identity token can be requested for it (ADR-0037 decision 2).
func TestModelFactoryRefusesTheAnthropicHostOnFoundry(t *testing.T) {
	for _, suffixes := range [][]string{{".api.anthropic.com"}, {".anthropic.com"}, {foundrySuffix, ".anthropic.com"}} {
		f, err := NewModelFactory(ModelsConfig{Suffixes: suffixes, Tokens: failingTokens{t}, Log: logr.Discard(),
			AnthropicKeyFile: "/var/run/anthropic/key", Expected: map[analysis.Role]string{analysis.RoleTriage: smallModel}})
		if err != nil {
			t.Fatal(err)
		}
		for _, provider := range []remediationv1alpha1.ModelProvider{"", remediationv1alpha1.ModelProviderFoundry} {
			for _, endpoint := range []string{remediationv1alpha1.AnthropicEndpoint, "https://api.anthropic.com/openai/v1",
				"https://api.anthropic.com:443/"} {
				d := remediationv1alpha1.ModelDeployment{Provider: provider, Endpoint: endpoint, Deployment: smallModel,
					APIVersion: "v1"}
				p, err := f(analysis.RoleTriage, d)
				if p != nil {
					// Calling it shows what the refusal prevents: the token source fails the test.
					_, _ = p.Complete(t.Context(), providers.CompletionRequest{})
				}
				if !errors.Is(err, analysis.ErrEndpointNotAllowed) || p != nil {
					t.Errorf("%v, provider %q, %s: %v, %v", suffixes, provider, endpoint, p, err)
				}
			}
		}
	}
}

// An Anthropic deployment is built only with the analyzer's key file, at the API's fixed endpoint, for the
// model its role expects; the allow-list is not consulted, and no token is requested (ADR-0037).
func TestModelFactoryAnthropic(t *testing.T) {
	if remediationv1alpha1.AnthropicEndpoint != anthropic.Endpoint {
		t.Fatalf("the API's endpoint %q and the adapter's %q differ", remediationv1alpha1.AnthropicEndpoint,
			anthropic.Endpoint)
	}
	const keyFile = "/var/run/shadewright/anthropic/api-key"
	expected := map[analysis.Role]string{analysis.RoleTriage: anthropicModel}
	build := func(keyFile string, d remediationv1alpha1.ModelDeployment, role analysis.Role) (providers.ModelProvider, error) {
		t.Helper()
		f, err := NewModelFactory(ModelsConfig{Suffixes: []string{foundrySuffix}, Tokens: failingTokens{t},
			AnthropicKeyFile: keyFile, Expected: expected, Log: logr.Discard()})
		if err != nil {
			t.Fatal(err)
		}
		return f(role, d)
	}
	good := remediationv1alpha1.ModelDeployment{Provider: remediationv1alpha1.ModelProviderAnthropic,
		Endpoint: remediationv1alpha1.AnthropicEndpoint, Deployment: anthropicModel, APIVersion: "v1"}

	p, err := build(keyFile, good, analysis.RoleTriage)
	if _, ok := p.(*anthropic.Provider); err != nil || !ok {
		t.Fatalf("a valid deployment: %T, %v", p, err)
	}

	p, err = build("", good, analysis.RoleTriage)
	if !errors.Is(err, analysis.ErrProviderNotConfigured) || p != nil {
		t.Errorf("no key file: %v, %v", p, err)
	}

	for name, change := range map[string]func(*remediationv1alpha1.ModelDeployment){
		"a Foundry endpoint":  func(d *remediationv1alpha1.ModelDeployment) { d.Endpoint = "https://other.services.ai.azure.com/" },
		"a trailing slash":    func(d *remediationv1alpha1.ModelDeployment) { d.Endpoint += "/" },
		"another vendor host": func(d *remediationv1alpha1.ModelDeployment) { d.Endpoint = "https://console.anthropic.com" },
		"a path on that host": func(d *remediationv1alpha1.ModelDeployment) { d.Endpoint += "/v1" },
		"a port on that host": func(d *remediationv1alpha1.ModelDeployment) { d.Endpoint += ":443" },
	} {
		d := good
		change(&d)
		if p, err := build(keyFile, d, analysis.RoleTriage); !errors.Is(err, analysis.ErrEndpointNotAllowed) || p != nil {
			t.Errorf("%s: %v, %v", name, p, err)
		}
	}

	other := good
	other.Deployment = "claude-opus-5-5"
	if p, err := build(keyFile, other, analysis.RoleTriage); !errors.Is(err, analysis.ErrDeploymentNotExpected) || p != nil {
		t.Errorf("another model than the role's flag: %v, %v", p, err)
	}
	if p, err := build(keyFile, good, analysis.RoleEscalation); !errors.Is(err, analysis.ErrDeploymentNotExpected) || p != nil {
		t.Errorf("a role without a flag: %v, %v", p, err)
	}

	unknown := good
	unknown.Provider = "OpenAI"
	if p, err := build(keyFile, unknown, analysis.RoleTriage); !errors.Is(err, analysis.ErrProviderNotConfigured) || p != nil {
		t.Errorf("an unknown provider: %v, %v", p, err)
	}
}

// A refusal's error and log line name neither the key file nor the endpoint.
func TestModelFactoryRefusalsNameNoKeyFile(t *testing.T) {
	const keyFile = "/var/run/shadewright/anthropic/api-key"
	var lines []string
	sink := funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{})
	f, err := NewModelFactory(ModelsConfig{Suffixes: []string{foundrySuffix}, Tokens: failingTokens{t},
		AnthropicKeyFile: keyFile, Expected: map[analysis.Role]string{}, Log: sink})
	if err != nil {
		t.Fatal(err)
	}
	d := remediationv1alpha1.ModelDeployment{Provider: remediationv1alpha1.ModelProviderAnthropic,
		Endpoint: remediationv1alpha1.AnthropicEndpoint, Deployment: anthropicModel, APIVersion: "v1"}
	_, err = f(analysis.RoleTriage, d)
	if err == nil || len(lines) != 1 {
		t.Fatalf("%v, %d log lines", err, len(lines))
	}
	for _, s := range []string{err.Error(), lines[0]} {
		if strings.Contains(s, keyFile) || strings.Contains(s, "https://") {
			t.Errorf("%q names the key file or the endpoint", s)
		}
	}
}

// logSink returns a logger that keeps each line's key-value text.
func logSink(lines *[]string) logr.Logger {
	return funcr.New(func(_, args string) { *lines = append(*lines, args) }, funcr.Options{})
}

// A deployment the adapter refuses to build is logged like the factory's own refusals: a role flag that is a
// valid model name but not a Claude model ID, and a Foundry API version the adapter does not accept.
func TestModelFactoryLogsAdapterRefusals(t *testing.T) {
	var lines []string
	f, err := NewModelFactory(ModelsConfig{Suffixes: []string{foundrySuffix}, Tokens: failingTokens{t},
		AnthropicKeyFile: "/var/run/shadewright/anthropic/api-key",
		Expected:         map[analysis.Role]string{analysis.RoleTriage: "Claude_Haiku"}, Log: logSink(&lines)})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []remediationv1alpha1.ModelDeployment{
		{Provider: remediationv1alpha1.ModelProviderAnthropic, Endpoint: remediationv1alpha1.AnthropicEndpoint,
			Deployment: "Claude_Haiku", APIVersion: "v1"},
		{Endpoint: "https://a" + foundrySuffix + "/", Deployment: smallModel, APIVersion: "latest"},
	} {
		lines = nil
		p, err := f(analysis.RoleTriage, d)
		if !providers.IsInvalidConfig(err) || p != nil || len(lines) != 1 || !strings.Contains(lines[0], "invalid configuration") {
			t.Errorf("%s: %v, %v, log %q", d.ProviderName(), p, err, lines)
		}
	}
}

// The Anthropic endpoint refusal says what is wrong, not that the host is outside the allow-list, which is
// not consulted for that provider.
func TestModelFactoryAnthropicEndpointText(t *testing.T) {
	f, err := NewModelFactory(ModelsConfig{Suffixes: []string{foundrySuffix}, Tokens: failingTokens{t},
		AnthropicKeyFile: "/k", Expected: map[analysis.Role]string{analysis.RoleTriage: anthropicModel}, Log: logr.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f(analysis.RoleTriage, remediationv1alpha1.ModelDeployment{Provider: remediationv1alpha1.ModelProviderAnthropic,
		Endpoint: remediationv1alpha1.AnthropicEndpoint + "/", Deployment: anthropicModel, APIVersion: "v1"})
	if !errors.Is(err, analysis.ErrEndpointNotAllowed) || !strings.Contains(err.Error(), "fixed endpoint") ||
		strings.Contains(err.Error(), "allow-list") {
		t.Errorf("err = %v", err)
	}
}

// Every failed call of an Anthropic provider is logged with the adapter's fixed error text, so a key file that
// cannot be read is told apart from an outage; the key and the key file's path are never logged.
func TestModelFactoryLogsAnthropicCallFailures(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(keyFile, []byte("sk-ant-secret\nsk-ant-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var lines []string
	f, err := NewModelFactory(ModelsConfig{Suffixes: []string{foundrySuffix}, Tokens: failingTokens{t},
		AnthropicKeyFile: keyFile, Expected: map[analysis.Role]string{analysis.RoleTriage: anthropicModel},
		Log: logSink(&lines)})
	if err != nil {
		t.Fatal(err)
	}
	p, err := f(analysis.RoleTriage, remediationv1alpha1.ModelDeployment{Provider: remediationv1alpha1.ModelProviderAnthropic,
		Endpoint: remediationv1alpha1.AnthropicEndpoint, Deployment: anthropicModel, APIVersion: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Complete(t.Context(), providers.CompletionRequest{Messages: []providers.Message{
		{Role: providers.RoleUser, Content: "context"}}})
	if !providers.IsCredentialUnavailable(err) || len(lines) != 1 || !strings.Contains(lines[0], "API key file is malformed") {
		t.Fatalf("err %v, log %q", err, lines)
	}
	if strings.Contains(lines[0], "sk-ant") || strings.Contains(lines[0], keyFile) {
		t.Errorf("the log line holds the key or the key file: %q", lines[0])
	}
}
