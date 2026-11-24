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
	"path/filepath"
	"slices"
	"strings"
	"testing"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/analysis"
	"github.com/shadewright-labs/shadewright/internal/analyzer/models"
	"github.com/shadewright-labs/shadewright/internal/providers/model/openaicompatible"
)

const providerCompatible = "openaicompatible"
const envCompatibleEndpoints = "EVAL_COMPATIBLE_ENDPOINTS"
const envCompatibleServer = "EVAL_COMPATIBLE_SERVER_ID"

// The short form of a compatible run: one endpoint, default the public OpenAI API base, and its key file.
const (
	envOpenAIEndpoint = "EVAL_OPENAI_ENDPOINT"
	envOpenAIKeyFile  = "EVAL_OPENAI_KEY_FILE"
)

// defaultOpenAIEndpoint is the public OpenAI API base, as the provider's configuration reference gives it.
const defaultOpenAIEndpoint = "https://api.openai.com/v1"

// compatibleSource uses the same factory and gates as the analyzer, with no Azure identity.
type compatibleSource struct {
	foundrySource
	serverID, endpointHash string
}

func (*compatibleSource) manifestProvider() (string, *manifestAnthropic) {
	return providerCompatible, nil
}

func newCompatibleSource() (*compatibleSource, error) {
	for _, name := range []string{envAPI, envKeyFile, envEffort, envAllowance, envSuffixes} {
		if os.Getenv(name) != "" {
			return nil, fmt.Errorf("%s does not apply to OpenAICompatible", name)
		}
	}
	if os.Getenv(envTriageDep) == "" || os.Getenv(envTriageMod) == "" || os.Getenv(envCompatibleServer) == "" {
		return nil, fmt.Errorf("%s, %s and %s are required", envTriageDep, envTriageMod, envCompatibleServer)
	}
	endpoint, endpoints, err := compatibleEndpoints()
	if err != nil {
		return nil, err
	}
	factory, err := models.NewModelFactory(models.ModelsConfig{CompatibleEndpoints: endpoints, DisableFoundry: true})
	if err != nil {
		return nil, err
	}
	src := &compatibleSource{foundrySource: foundrySource{factoryFn: factory,
		triageModel: os.Getenv(envTriageMod), escModel: os.Getenv(envEscMod)},
		serverID: os.Getenv(envCompatibleServer)}
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

// compatibleEndpoints returns the API base of the run and the endpoint declarations the analyzer would get
// from --openai-compatible-endpoints. With EVAL_COMPATIBLE_ENDPOINTS they are that JSON and EVAL_ENDPOINT, as
// on an install. Without it they are the short form: one declaration for EVAL_OPENAI_ENDPOINT (default the
// public OpenAI API base) with EVAL_OPENAI_KEY_FILE as its key file, the models of both roles and their one
// inference API. Only the path is read here; the provider opens the file at each call, so the key is never
// in the environment, a command line, the manifest or the report.
func compatibleEndpoints() (string, []openaicompatible.Endpoint, error) {
	if raw := os.Getenv(envCompatibleEndpoints); raw != "" {
		for _, name := range []string{envOpenAIEndpoint, envOpenAIKeyFile} {
			if os.Getenv(name) != "" {
				return "", nil, fmt.Errorf("%s does not apply with %s; give the key file in its keyFile",
					name, envCompatibleEndpoints)
			}
		}
		endpoint := os.Getenv(envEndpoint)
		if endpoint == "" {
			return "", nil, fmt.Errorf("%s is required with %s", envEndpoint, envCompatibleEndpoints)
		}
		endpoints, err := openaicompatible.ParseEndpoints(raw)
		return endpoint, endpoints, err
	}
	if os.Getenv(envEndpoint) != "" {
		return "", nil, fmt.Errorf("%s applies only with %s; set %s instead",
			envEndpoint, envCompatibleEndpoints, envOpenAIEndpoint)
	}
	endpoint := os.Getenv(envOpenAIEndpoint)
	if endpoint == "" {
		endpoint = defaultOpenAIEndpoint
	}
	triageAPI, err := envInferenceAPI(envTriageAPI)
	if err != nil {
		return "", nil, err
	}
	escalationAPI, err := envInferenceAPI(envEscAPI)
	if err != nil {
		return "", nil, err
	}
	api := openaicompatible.API(cmpOrAPI(triageAPI))
	if os.Getenv(envEscDep) != "" && openaicompatible.API(cmpOrAPI(escalationAPI)) != api {
		// One base is one declaration with one API, and a key file belongs to one declaration only.
		return "", nil, fmt.Errorf("%s and %s differ; use %s for two APIs", envTriageAPI, envEscAPI, envCompatibleEndpoints)
	}
	var names []string
	for _, name := range []string{os.Getenv(envTriageDep), os.Getenv(envEscDep)} {
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	keyFile := os.Getenv(envOpenAIKeyFile)
	if keyFile != "" {
		if keyFile, err = filepath.Abs(keyFile); err != nil {
			return "", nil, fmt.Errorf("%s: %w", envOpenAIKeyFile, err)
		}
	}
	endpoints := []openaicompatible.Endpoint{{URL: endpoint, API: api, Models: names, KeyFile: keyFile}}
	if err := openaicompatible.ValidateEndpoints(endpoints, nil); err != nil {
		return "", nil, fmt.Errorf("%s or %s: %w", envOpenAIEndpoint, envOpenAIKeyFile, err)
	}
	return endpoint, endpoints, nil
}

// cmpOrAPI is the inference API of a deployment that leaves it unset, as the compatible provider reads it.
func cmpOrAPI(api remediationv1alpha1.InferenceAPI) remediationv1alpha1.InferenceAPI {
	if api == "" {
		return remediationv1alpha1.InferenceAPIChatCompletions
	}
	return api
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
	for _, name := range []string{envCompatibleEndpoints, envCompatibleServer} {
		t.Setenv(name, "")
	}
	t.Setenv(envEndpoint, "https://api.openai.com/v1")
	t.Setenv(envCompatibleEndpoints, `[{"url":"https://api.openai.com/v1",`+
		`"inferenceAPI":"Responses","models":["gpt-6.1-sol"]}]`)
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
	t.Setenv(envCompatibleEndpoints, `[{"url":"https://other.example.com/v1",`+
		`"inferenceAPI":"Responses","models":["gpt-6.1-sol"],`+
		`"keyFile":"/unread/key"}]`)
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
		r.Totals.CostPerPassedRunUSD == nil || *r.Totals.CostPerPassedRunUSD != *r.Totals.CostUSD {
		t.Fatalf("per passed totals: %+v", r.Totals)
	}
	r.Scenarios[0].Pass = false
	r.summarise(p)
	if r.Totals.SecondsPerPassed != nil || r.Totals.TokensPerPassed != nil || r.Totals.CostPerPassedRunUSD != nil {
		t.Fatal("zero passes reported a per-passed number")
	}
}

// testEscalationDeployment is the escalation model of the short-form tests.
const testEscalationDeployment = "gpt-6-astra"

// clearCompatibleEnv unsets every setting of a real-model run, the compatible ones included.
func clearCompatibleEnv(t *testing.T) {
	t.Helper()
	clearModelEnv(t)
	for _, name := range []string{envCompatibleEndpoints, envCompatibleServer, envOpenAIEndpoint, envOpenAIKeyFile,
		envRepeats, envPassRate} {
		t.Setenv(name, "")
	}
}

// Without EVAL_MODEL=openaicompatible the live test skips before it reads any other setting, so make test
// and CI never call a model.
func TestEvalOpenAICompatibleSkips(t *testing.T) {
	clearCompatibleEnv(t)
	t.Setenv(envOpenAIKeyFile, "/nonexistent/key")
	for _, mode := range []string{"", "foundry", providerAnthropic} {
		t.Setenv(envModel, mode)
		var skipped bool
		t.Run("mode="+mode, func(t *testing.T) {
			defer func() { skipped = t.Skipped() }()
			TestEvalOpenAICompatible(t)
		})
		if !skipped {
			t.Errorf("EVAL_MODEL=%q did not skip", mode)
		}
	}
}

// The short form builds one declaration from EVAL_OPENAI_ENDPOINT (default the public OpenAI API base) and
// EVAL_OPENAI_KEY_FILE, as --openai-compatible-endpoints would hold it, and the analyzer's factory accepts each
// role on it. The key file is a path the provider opens at each call: nothing here reads it, and neither the
// manifest nor the report holds the key or the path.
func TestCompatibleShortForm(t *testing.T) {
	clearCompatibleEnv(t)
	dir := t.TempDir()
	key := "test-model-key-" + strings.Repeat("Q", 40)
	keyPath := filepath.Join(dir, "model-key")
	if err := os.WriteFile(keyPath, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envOpenAIKeyFile, keyPath)
	t.Setenv(envTriageDep, "gpt-6.1-sol")
	t.Setenv(envTriageMod, "gpt-6.1-sol-2026-11-01")
	t.Setenv(envTriageAPI, string(remediationv1alpha1.InferenceAPIResponses))
	t.Setenv(envEscDep, testEscalationDeployment)
	t.Setenv(envEscMod, testEscalationDeployment+"-2026-10-01")
	t.Setenv(envEscAPI, string(remediationv1alpha1.InferenceAPIResponses))
	t.Setenv(envCompatibleServer, "openai-2026-11-16")
	t.Setenv("AZURE_CLIENT_ID", "invalid")

	t.Run("relative key path", func(t *testing.T) {
		t.Chdir(dir)
		t.Setenv(envOpenAIKeyFile, "model-key")
		_, endpoints, err := compatibleEndpoints()
		if err != nil || filepath.Clean(endpoints[0].KeyFile) != filepath.Clean(keyPath) {
			t.Fatalf("declarations %+v, %v", endpoints, err)
		}
	})
	endpoint, endpoints, err := compatibleEndpoints()
	if err != nil {
		t.Fatal(err)
	}
	want := openaicompatible.Endpoint{URL: defaultOpenAIEndpoint, API: openaicompatible.APIResponses,
		Models: []string{"gpt-6.1-sol", testEscalationDeployment}, KeyFile: keyPath}
	if endpoint != defaultOpenAIEndpoint || len(endpoints) != 1 || endpoints[0].URL != want.URL ||
		endpoints[0].API != want.API || !slices.Equal(endpoints[0].Models, want.Models) ||
		filepath.Clean(endpoints[0].KeyFile) != filepath.Clean(want.KeyFile) || !filepath.IsAbs(endpoints[0].KeyFile) {
		t.Fatalf("endpoint %q, declarations %+v, want %+v", endpoint, endpoints, want)
	}
	src, err := newCompatibleSource()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []*remediationv1alpha1.ModelDeployment{src.triage, src.escalation} {
		if !shortFormDeployment(d) {
			t.Fatalf("deployment %+v", d)
		}
	}
	if src.endpointHash != fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(defaultOpenAIEndpoint))) {
		t.Errorf("endpoint hash %s", src.endpointHash)
	}
	t.Setenv(envOpenAIEndpoint, defaultOpenAIEndpoint)
	explicit, err := newCompatibleSource()
	if err != nil || explicit.endpointHash != src.endpointHash {
		t.Fatalf("the default and the same base given differ: %v", err)
	}

	checkShortFormReport(t, src, key)
}

// shortFormDeployment reports whether d is a Responses deployment of the compatible provider at the default base.
func shortFormDeployment(d *remediationv1alpha1.ModelDeployment) bool {
	return d != nil && d.Endpoint == defaultOpenAIEndpoint &&
		d.Provider == remediationv1alpha1.ModelProviderOpenAICompatible &&
		d.InferenceAPI == remediationv1alpha1.InferenceAPIResponses && d.APIVersion == ""
}

// checkShortFormReport checks that the manifest and the report name the provider and the server, and that no
// report file holds the key, the name of its file or the endpoint URL.
func checkShortFormReport(t *testing.T, src *compatibleSource, key string) {
	t.Helper()
	fleet, err := loadSource()
	if err != nil {
		t.Fatal(err)
	}
	m, err := buildManifest(fleet, src, nil, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider != providerCompatible || m.Compatible == nil || m.Compatible.ServerID != "openai-2026-11-16" {
		t.Fatalf("manifest provider %q, server %+v", m.Provider, m.Compatible)
	}
	r := &report{SchemaVersion: reportSchemaVersion, Mode: providerCompatible, Manifest: m}
	md := r.markdown()
	if !strings.Contains(md, "- Mode: openaicompatible;") ||
		!strings.Contains(md, "- Server `openai-2026-11-16`; endpoint `"+src.endpointHash+"`") {
		t.Errorf("report does not name the provider and server:\n%s", md)
	}
	out := t.TempDir()
	if err := r.write(out); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(out)
	if err != nil || len(files) == 0 {
		t.Fatalf("report files %v, %v", files, err)
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(out, f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), key) || strings.Contains(string(data), "model-key") ||
			strings.Contains(string(data), defaultOpenAIEndpoint) {
			t.Errorf("%s holds the key, its path or the endpoint URL", f.Name())
		}
	}
}

// The short form refuses what it cannot express or what belongs to the other form, each before any call.
func TestCompatibleShortFormRefusals(t *testing.T) {
	base := func(t *testing.T) {
		clearCompatibleEnv(t)
		t.Setenv(envTriageDep, "gpt-6.1-sol")
		t.Setenv(envTriageMod, "gpt-6.1-sol")
		t.Setenv(envCompatibleServer, "openai-2026-11-16")
		t.Setenv(envOpenAIKeyFile, "/run/secrets/model-key")
	}
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"EVAL_ENDPOINT without the JSON form": {map[string]string{envEndpoint: defaultOpenAIEndpoint}, envOpenAIEndpoint},
		"short form next to the JSON form": {map[string]string{envEndpoint: defaultOpenAIEndpoint,
			envCompatibleEndpoints: `[{"url":"https://api.openai.com/v1","inferenceAPI":"ChatCompletions",` +
				`"models":["gpt-6.1-sol"]}]`},
			envOpenAIKeyFile},
		"two inference APIs on one base": {map[string]string{envEscDep: testEscalationDeployment,
			envEscMod: testEscalationDeployment, envTriageAPI: string(remediationv1alpha1.InferenceAPIResponses)}, envEscAPI},
		"plain HTTP": {map[string]string{envOpenAIEndpoint: "http://api.openai.com/v1"}, envOpenAIEndpoint},
		"an Azure host": {map[string]string{envOpenAIEndpoint: "https://r.openai.azure.com/openai/v1"},
			envOpenAIEndpoint},
		"the Anthropic key":    {map[string]string{envKeyFile: "/run/secrets/model-key"}, envKeyFile},
		"no server identity":   {map[string]string{envCompatibleServer: ""}, envCompatibleServer},
		"an unknown model API": {map[string]string{envTriageAPI: "Completions"}, envTriageAPI},
	} {
		t.Run(name, func(t *testing.T) {
			base(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if _, err := newCompatibleSource(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one naming %s", err, tc.want)
			}
		})
	}
	// Without a key file the declaration has none: a server that needs no key, such as one in the lab.
	base(t)
	t.Setenv(envOpenAIKeyFile, "")
	_, endpoints, err := compatibleEndpoints()
	if err != nil || len(endpoints) != 1 || endpoints[0].KeyFile != "" ||
		endpoints[0].API != openaicompatible.APIChatCompletions {
		t.Fatalf("declarations %+v, %v", endpoints, err)
	}
}
