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
	"crypto/sha256"
	"fmt"
	"os"
	"testing"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/analyzer/models"
	"github.com/pkalin/shadewright/internal/providers/model/openaicompatible"
)

const providerCompatible = "openaicompatible"
const envCompatibleKey = "EVAL_COMPATIBLE_KEY_FILE"
const envCompatibleKeyHost = "EVAL_COMPATIBLE_KEY_HOST"
const envCompatibleServices = "EVAL_COMPATIBLE_SERVICE_HOSTS"
const envCompatibleServer = "EVAL_COMPATIBLE_SERVER_ID"

// compatibleSource uses the same factory and gates as the analyzer, with no Azure identity.
type compatibleSource struct {
	foundrySource
	serverID, endpointHash string
}

func (*compatibleSource) manifestProvider() (string, *manifestAnthropic) {
	return providerCompatible, nil
}

func openaicompatibleServiceHosts() ([]string, error) {
	return openaicompatible.ParseServiceHosts(os.Getenv(envCompatibleServices))
}

func newCompatibleSource() (*compatibleSource, error) {
	for _, name := range []string{envAPI, envKeyFile, envEffort, envAllowance} {
		if os.Getenv(name) != "" {
			return nil, fmt.Errorf("%s does not apply to OpenAICompatible", name)
		}
	}
	suffixes, err := models.ParseEndpointSuffixes(os.Getenv(envSuffixes))
	if err != nil {
		return nil, err
	}
	services, err := openaicompatibleServiceHosts()
	if err != nil {
		return nil, err
	}
	factory, err := models.NewModelFactory(models.ModelsConfig{Suffixes: suffixes, DisableFoundry: true,
		CompatibleKeyFile: os.Getenv(envCompatibleKey), CompatibleKeyHost: os.Getenv(envCompatibleKeyHost),
		CompatibleServiceHosts: services})
	if err != nil {
		return nil, err
	}
	src := &compatibleSource{foundrySource: foundrySource{factoryFn: factory,
		triageModel: os.Getenv(envTriageMod), escModel: os.Getenv(envEscMod)},
		serverID: os.Getenv(envCompatibleServer)}
	endpoint := os.Getenv(envEndpoint)
	if endpoint == "" || os.Getenv(envTriageDep) == "" || src.triageModel == "" || src.serverID == "" {
		return nil, fmt.Errorf("%s, %s, %s and %s are required", envEndpoint, envTriageDep, envTriageMod, envCompatibleServer)
	}
	if _, err := models.ParseModelName(src.serverID); err != nil {
		return nil, fmt.Errorf("%s is invalid", envCompatibleServer)
	}
	src.endpointHash = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(endpoint)))
	src.force, err = envForceEscalation(os.Getenv(envEscDep) != "")
	if err != nil {
		return nil, err
	}
	triageAPI, err := envInferenceAPI(envTriageAPI)
	if err != nil {
		return nil, err
	}
	escalationAPI, err := envInferenceAPI(envEscAPI)
	if err != nil {
		return nil, err
	}
	for _, role := range []analysis.Role{analysis.RoleTriage, analysis.RoleEscalation} {
		dep, expected, api := os.Getenv(envTriageDep), src.triageModel, triageAPI
		if role == analysis.RoleEscalation {
			dep, expected, api = os.Getenv(envEscDep), src.escModel, escalationAPI
		}
		if dep == "" {
			if expected != "" || api != "" {
				return nil, fmt.Errorf("%s needs a deployment", role)
			}
			continue
		}
		if expected == "" {
			return nil, fmt.Errorf("%s needs an expected model", role)
		}
		d := deployment(endpoint, dep, "")
		d.Provider, d.InferenceAPI = remediationv1alpha1.ModelProviderOpenAICompatible, api
		if _, err := factory(role, *d); err != nil {
			return nil, err
		}
		if role == analysis.RoleTriage {
			src.triage = d
		} else {
			src.escalation = d
		}
	}
	return src, nil
}

func TestEvalOpenAICompatible(t *testing.T) {
	if os.Getenv(envModel) != providerCompatible {
		t.Skip("set EVAL_MODEL=openaicompatible to qualify a model and server")
	}
	repeats, err := envInt(envRepeats, 1, maxRepeats)
	if err != nil {
		t.Fatal(err)
	}
	rate, err := envFloat(envPassRate, 1, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	src, err := newCompatibleSource()
	if err != nil {
		t.Fatal(err)
	}
	p, err := envPrices(src.escalation != nil)
	if err != nil {
		t.Fatal(err)
	}
	r := evaluate(t, providerCompatible, src, repeats, rate, p)
	if !r.Totals.Pass {
		t.Errorf("passed %d of %d", r.Totals.Passed, r.Totals.Runs)
	}
}

func TestCompatibleSourceWithoutAzure(t *testing.T) {
	clearModelEnv(t)
	for _, name := range []string{envCompatibleKey, envCompatibleKeyHost, envCompatibleServices, envCompatibleServer} {
		t.Setenv(name, "")
	}
	t.Setenv(envEndpoint, "https://api.openai.com/v1")
	t.Setenv(envSuffixes, ".api.openai.com")
	t.Setenv(envTriageDep, "gpt-6.1-sol")
	t.Setenv(envTriageMod, "gpt-6.1-sol")
	t.Setenv(envTriageAPI, "Responses")
	t.Setenv(envCompatibleServer, "openai-2026-11-16")
	t.Setenv("AZURE_CLIENT_ID", "invalid")
	src, err := newCompatibleSource()
	if err != nil {
		t.Fatal(err)
	}
	if src.triage.Provider != remediationv1alpha1.ModelProviderOpenAICompatible || src.triage.APIVersion != "" {
		t.Fatal("wrong provider configuration")
	}
	fleet, err := loadSource()
	if err != nil {
		t.Fatal(err)
	}
	m, err := buildManifest(fleet, src, nil, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider != providerCompatible || m.Compatible == nil || m.Compatible.ServerID != "openai-2026-11-16" ||
		m.Compatible.EndpointHash != src.endpointHash {
		t.Fatal("manifest loses server identity")
	}
	t.Setenv(envCompatibleServer, "other-server")
	other, err := newCompatibleSource()
	if err != nil {
		t.Fatal(err)
	}
	changed, err := buildManifest(fleet, other, nil, 1, 1)
	if err != nil || changed.Digest == m.Digest {
		t.Fatal("server identity does not change qualification digest")
	}
	t.Setenv(envCompatibleKey, "/unread/key")
	t.Setenv(envCompatibleKeyHost, "wrong.example.com")
	if _, err := newCompatibleSource(); err == nil {
		t.Fatal("misbound key accepted")
	}
}

func TestCostTimeAndTokensPerPassedRun(t *testing.T) {
	r := &report{Scenarios: []scenarioResult{
		{Pass: true, Seconds: 2, Tokens: tokens{Input: 30, Output: 10, Charged: 40}},
		{Pass: false, Seconds: 3, Tokens: tokens{Input: 20, Output: 5, Charged: 25}},
	}}
	p := &prices{Input: 1, CachedInput: 1, Output: 2}
	r.summarise(p)
	if r.Totals.Seconds != 5 || r.Totals.SecondsPerPassed == nil || *r.Totals.SecondsPerPassed != 5 ||
		r.Totals.TokensPerPassed == nil || *r.Totals.TokensPerPassed != 65 ||
		r.Totals.CostPerPassed == nil || *r.Totals.CostPerPassed != *r.Totals.CostUSD {
		t.Fatalf("per passed totals: %+v", r.Totals)
	}
	r.Scenarios[0].Pass = false
	r.summarise(p)
	if r.Totals.SecondsPerPassed != nil || r.Totals.TokensPerPassed != nil || r.Totals.CostPerPassed != nil {
		t.Fatal("zero passes reported a per-passed number")
	}
}
