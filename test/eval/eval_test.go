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
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/analyzer/models"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
	"github.com/pkalin/shadewright/internal/providers/model/anthropic"
	"github.com/pkalin/shadewright/internal/providers/model/foundry"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/tools"
)

// Environment of a run. EVAL_REPORT_DIR applies to both modes; the others to the real model only.
const (
	envReportDir = "EVAL_REPORT_DIR"
	// EVAL_MODEL: "foundry" runs TestEvalFoundry, "anthropic" TestEvalAnthropic, "openaicompatible"
	// TestEvalOpenAICompatible.
	envModel     = "EVAL_MODEL"
	envKeyFile   = "EVAL_ANTHROPIC_KEY_FILE"
	envEffort    = "EVAL_ANTHROPIC_EFFORT"
	envAllowance = "EVAL_ANTHROPIC_THINKING_ALLOWANCE"
	envEndpoint  = "EVAL_ENDPOINT"
	envSuffixes  = "EVAL_ENDPOINT_SUFFIXES"
	envAPI       = "EVAL_API_VERSION"
	envTriageDep = "EVAL_TRIAGE_DEPLOYMENT"
	envTriageMod = "EVAL_TRIAGE_MODEL"
	envEscDep    = "EVAL_ESCALATION_DEPLOYMENT"
	envEscMod    = "EVAL_ESCALATION_MODEL"
	envTriageAPI = "EVAL_TRIAGE_INFERENCE_API"     // ChatCompletions (default) or Responses, as inferenceAPI
	envEscAPI    = "EVAL_ESCALATION_INFERENCE_API" // the same for the escalation deployment
	envRepeats   = "EVAL_REPEATS"
	envPassRate  = "EVAL_MIN_PASS_RATE" // #nosec G101 -- the name of the pass threshold variable, not a credential
	envPriceIn   = "EVAL_PRICE_INPUT_PER_MTOK"
	envPriceHit  = "EVAL_PRICE_CACHED_INPUT_PER_MTOK"
	envPriceOut  = "EVAL_PRICE_OUTPUT_PER_MTOK"
	envEscPrices = "EVAL_ESCALATION_PRICE_"
	envForceEsc  = "EVAL_FORCE_ESCALATION" // "1" sends every run to the escalation model
)

// The model providers of a run, as the report's mode and the manifest's provider name them.
const (
	providerFake      = "fake"
	providerFoundry   = "foundry"
	providerAnthropic = "anthropic"
)

// Bounds of a real-model run.
const (
	maxRepeats      = 5
	scenarioTimeout = analysis.DefaultTimeout + time.Minute
)

// fakeModelName is the model the recorded replies name.
const fakeModelName = "eval-fake-1"

// replaySource replays each scenario's recorded replies, in order, for both roles. force sends every run to
// the escalation model (tests only: the fleet scenarios' recordings are triage replies; an escalation scenario
// goes there anyway).
type replaySource struct {
	force bool
}

func (r replaySource) forcesEscalation() bool { return r.force }

func (replaySource) factory(s *scenario) (analysis.ModelFactory, func() error, error) {
	if len(s.recording.Replies) == 0 {
		return nil, nil, errors.New("replies.yaml holds no replies")
	}
	m := &fake.Model{}
	for _, r := range s.recording.Replies {
		m.Script(r.completion())
	}
	// A reply the analysis never asked for means the recording no longer matches the loop (it ends earlier, or
	// escalates differently): the scenario fails instead of passing on part of its recording.
	done := func() error {
		if used, want := len(m.Requests()), len(s.recording.Replies); used != want {
			return fmt.Errorf("the analysis made %d model calls, replies.yaml records %d replies", used, want)
		}
		return nil
	}
	return func(analysis.Role, remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
		return m, nil
	}, done, nil
}

func (replaySource) manifestProvider() (string, *manifestAnthropic) { return "", nil }

func (replaySource) expectedModels() map[analysis.Role]string {
	return map[analysis.Role]string{analysis.RoleTriage: fakeModelName, analysis.RoleEscalation: fakeModelName}
}

func (replaySource) deployments() (*remediationv1alpha1.ModelDeployment, *remediationv1alpha1.ModelDeployment) {
	const endpoint = "https://eval.invalid/"
	return deployment(endpoint, "eval-triage", "v1"), deployment(endpoint, "eval-escalation", "v1")
}

// deployment is a model deployment with the evaluation's token bound per scenario run.
func deployment(endpoint, name, api string) *remediationv1alpha1.ModelDeployment {
	return &remediationv1alpha1.ModelDeployment{Endpoint: endpoint, Deployment: name, APIVersion: api,
		MaxTokensPerDay: ptr.To[int64](analysis.DefaultMaxTokensPerAnalysis)}
}

// foundrySource calls the deployments the environment names, through the analyzer's own model factory
// (endpoint allow-list, workload identity token source, Foundry adapter).
type foundrySource struct {
	factoryFn             analysis.ModelFactory
	triage, escalation    *remediationv1alpha1.ModelDeployment
	triageModel, escModel string
	force                 bool
}

func newFoundrySource() (*foundrySource, error) {
	// The settings are checked before the identity is built, so a wrong one fails without a token exchange.
	force, err := envForceEscalation(os.Getenv(envEscDep) != "")
	if err != nil {
		return nil, err
	}
	triageAPI, err := envInferenceAPI(envTriageAPI)
	if err != nil {
		return nil, err
	}
	escAPI, err := envInferenceAPI(envEscAPI)
	if err != nil {
		return nil, err
	}
	suffixes, err := models.ParseEndpointSuffixes(os.Getenv(envSuffixes))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envSuffixes, err)
	}
	tokens, err := foundry.NewWorkloadIdentity()
	if err != nil {
		return nil, err
	}
	factory, err := models.NewModelFactory(models.ModelsConfig{Suffixes: suffixes, Tokens: tokens})
	if err != nil {
		return nil, err
	}
	api := os.Getenv(envAPI)
	if api == "" {
		api = "v1"
	}
	src := &foundrySource{factoryFn: factory, triageModel: os.Getenv(envTriageMod), escModel: os.Getenv(envEscMod),
		force: force}
	endpoint := os.Getenv(envEndpoint)
	if endpoint == "" || os.Getenv(envTriageDep) == "" || src.triageModel == "" {
		return nil, fmt.Errorf("%s, %s and %s are required", envEndpoint, envTriageDep, envTriageMod)
	}
	src.triage = deployment(endpoint, os.Getenv(envTriageDep), api)
	src.triage.InferenceAPI = triageAPI
	if dep := os.Getenv(envEscDep); dep != "" {
		if src.escModel == "" {
			return nil, fmt.Errorf("%s needs %s", envEscDep, envEscMod)
		}
		src.escalation = deployment(endpoint, dep, api)
		src.escalation.InferenceAPI = escAPI
	} else if escAPI != "" {
		return nil, fmt.Errorf("%s needs %s", envEscAPI, envEscDep)
	}
	return src, nil
}

// envInferenceAPI reads the inference API of a Foundry deployment: unset (chat completions, inferenceAPI left
// out), ChatCompletions or Responses.
func envInferenceAPI(name string) (remediationv1alpha1.InferenceAPI, error) {
	switch v := remediationv1alpha1.InferenceAPI(os.Getenv(name)); v {
	case "", remediationv1alpha1.InferenceAPIChatCompletions, remediationv1alpha1.InferenceAPIResponses:
		return v, nil
	}
	return "", fmt.Errorf("%s must be ChatCompletions, Responses or unset", name)
}

// envForceEscalation reads EVAL_FORCE_ESCALATION: 1 forces every run to the escalation model, which must
// then be configured; unset does not.
func envForceEscalation(escalationConfigured bool) (bool, error) {
	switch os.Getenv(envForceEsc) {
	case "":
		return false, nil
	case "1":
		if !escalationConfigured {
			return false, fmt.Errorf("%s needs %s and %s", envForceEsc, envEscDep, envEscMod)
		}
		return true, nil
	}
	return false, fmt.Errorf("%s must be 1 or unset", envForceEsc)
}

func (f *foundrySource) forcesEscalation() bool { return f.force }

func (f *foundrySource) factory(*scenario) (analysis.ModelFactory, func() error, error) {
	return f.factoryFn, nil, nil
}

func (*foundrySource) manifestProvider() (string, *manifestAnthropic) { return "", nil }

func (f *foundrySource) expectedModels() map[analysis.Role]string {
	m := map[analysis.Role]string{analysis.RoleTriage: f.triageModel}
	if f.escalation != nil {
		m[analysis.RoleEscalation] = f.escModel
	}
	return m
}

func (f *foundrySource) deployments() (*remediationv1alpha1.ModelDeployment, *remediationv1alpha1.ModelDeployment) {
	return f.triage, f.escalation
}

// anthropicSource calls Claude models through the Anthropic API adapter. The analyzer has no such wiring yet
// (#729): only the evaluation builds this provider. The deployments carry the API's endpoint and version and
// the model ID as the deployment name, so the manifest and report read as for Foundry.
type anthropicSource struct {
	providers             map[analysis.Role]*anthropic.Provider
	triage, escalation    *remediationv1alpha1.ModelDeployment
	triageModel, escModel string
	force                 bool
	// settings are the effort and thinking allowance both roles' providers send, for the manifest.
	settings manifestAnthropic
}

// foundryOnly are the settings that mean nothing with EVAL_MODEL=anthropic; one that is set is refused, so a
// run never looks like it used them.
var foundryOnly = []string{envEndpoint, envSuffixes, envAPI, envTriageDep, envEscDep, envTriageAPI, envEscAPI}

func newAnthropicSource() (*anthropicSource, error) {
	for _, n := range foundryOnly {
		if os.Getenv(n) != "" {
			return nil, fmt.Errorf("%s applies to EVAL_MODEL=foundry only", n)
		}
	}
	src := &anthropicSource{providers: map[analysis.Role]*anthropic.Provider{},
		triageModel: os.Getenv(envTriageMod), escModel: os.Getenv(envEscMod)}
	keyFile := os.Getenv(envKeyFile)
	if src.triageModel == "" || keyFile == "" {
		return nil, fmt.Errorf("%s and %s are required", envTriageMod, envKeyFile)
	}
	force, err := envForceEscalation(src.escModel != "")
	if err != nil {
		return nil, err
	}
	src.force = force
	src.settings.Effort = cmp.Or(os.Getenv(envEffort), anthropic.DefaultEffort)
	if src.settings.ThinkingAllowance, err = envInt(envAllowance, anthropic.DefaultThinkingAllowance,
		anthropic.MaxThinkingAllowance); err != nil {
		return nil, err
	}
	for role, model := range map[analysis.Role]string{analysis.RoleTriage: src.triageModel,
		analysis.RoleEscalation: src.escModel} {
		if model == "" {
			continue
		}
		p, err := anthropic.New(anthropic.Config{Model: model, KeyFile: keyFile, Effort: src.settings.Effort,
			ThinkingAllowance: src.settings.ThinkingAllowance})
		if err != nil {
			return nil, fmt.Errorf("the %s model: %w", role, err)
		}
		src.providers[role] = p
	}
	src.triage = deployment(anthropic.Endpoint, src.triageModel, anthropic.APIVersion)
	if src.escModel != "" {
		src.escalation = deployment(anthropic.Endpoint, src.escModel, anthropic.APIVersion)
	}
	return src, nil
}

func (a *anthropicSource) forcesEscalation() bool { return a.force }

func (a *anthropicSource) manifestProvider() (string, *manifestAnthropic) {
	s := a.settings
	return providerAnthropic, &s
}

func (a *anthropicSource) factory(*scenario) (analysis.ModelFactory, func() error, error) {
	return func(role analysis.Role, d remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
		p := a.providers[role]
		if p == nil || d.Deployment != a.expectedModels()[role] {
			return nil, fmt.Errorf("no Anthropic model is configured for the %s role", role)
		}
		return p, nil
	}, nil, nil
}

func (a *anthropicSource) expectedModels() map[analysis.Role]string {
	m := map[analysis.Role]string{analysis.RoleTriage: a.triageModel}
	if a.escalation != nil {
		m[analysis.RoleEscalation] = a.escModel
	}
	return m
}

func (a *anthropicSource) deployments() (*remediationv1alpha1.ModelDeployment, *remediationv1alpha1.ModelDeployment) {
	return a.triage, a.escalation
}

// toolDefinitions are the definitions the loop offers on escalation in an evaluation run, for the manifest:
// the same list as the loop's own (analysis offers every tool but get_delivery_status), with query_metrics,
// which a scenario with a metrics.yaml (or a metrics source that is down) is offered, and get_pod_logs, which a
// scenario with a logs.yaml is offered. A scenario without them is offered the list without those tools;
// which scenarios have them is covered by the manifest's scenarios hash.
func toolDefinitions(scheme *runtime.Scheme) ([]providers.ToolDefinition, error) {
	r, err := redact.New()
	if err != nil {
		return nil, err
	}
	c := newClient(scheme, nil)
	set, err := tools.New(tools.Config{Pods: c, Events: c, Objects: c, Redactor: r, Namespaces: watchedNamespaces,
		Metrics: newReplayMetrics(&metricsRecording{}), Logs: replayLogs{rec: &logsRecording{}}})
	if err != nil {
		return nil, err
	}
	var offered []providers.ToolDefinition
	for _, d := range set.Definitions() {
		if d.Name != tools.NameGetDeliveryStatus {
			offered = append(offered, d)
		}
	}
	return offered, nil
}

// evaluate runs every scenario repeats times and returns the report.
func evaluate(t *testing.T, mode string, src modelSource, repeats int, minPassRate float64, p *prices) *report {
	t.Helper()
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := loadSource()
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := loadScenarios(scheme)
	if err != nil {
		t.Fatal(err)
	}
	defs, err := toolDefinitions(scheme)
	if err != nil {
		t.Fatal(err)
	}
	m, err := buildManifest(fleet, src, defs, minPassRate, repeats)
	if err != nil {
		t.Fatalf("building the manifest: %v", err)
	}
	r := &report{SchemaVersion: reportSchemaVersion, Mode: mode, Manifest: m}
	dir := os.Getenv(envReportDir)
	for _, s := range scenarios {
		for i := 1; i <= repeats; i++ {
			ctx, cancel := context.WithTimeout(t.Context(), scenarioTimeout)
			start := time.Now()
			res, err := runScenario(ctx, scheme, s, src)
			res.Seconds = time.Since(start).Seconds()
			cancel()
			res.Repeat = i
			if err != nil {
				res.Name, res.Pass, res.Error = s.name, false, err.Error()
			}
			r.Scenarios = append(r.Scenarios, res)
			// The report is rewritten after every run, so a run that is cut off (a timeout, a failed login, a
			// canceled job) still leaves what it paid for.
			r.summarise(p)
			if dir != "" {
				if err := r.write(dir); err != nil {
					t.Fatalf("writing the report: %v", err)
				}
			}
		}
	}
	r.summarise(p)
	t.Log("\n" + r.markdown())
	return r
}

// replayEnvError refuses the settings of a real-model run that would change a replay's meaning: the
// recordings of the fleet scenarios are triage replies, so forcing every run to the escalation model is not
// replayed, and it is refused rather than ignored. Only the escalation scenarios, recorded as escalation
// replies, are replayed on the escalation model.
func replayEnvError() error {
	if v := os.Getenv(envForceEsc); v != "" {
		return fmt.Errorf("%s applies to make eval-model only; the replay does not force escalation for every "+
			"scenario", envForceEsc)
	}
	return nil
}

// TestEvalReplay is the CI evaluation: every scenario with its recorded replies must pass.
func TestEvalReplay(t *testing.T) {
	if err := replayEnvError(); err != nil {
		t.Fatal(err)
	}
	r := evaluate(t, providerFake, replaySource{}, 1, 1, nil)
	for _, s := range r.Scenarios {
		if !s.Pass {
			t.Errorf("scenario %s failed: outcome %+v, diagnosis %+v, error %q", s.Name, s.Outcome, s.Diagnosis, s.Error)
		}
	}
	if !r.Totals.Pass {
		t.Errorf("the run did not pass: %d of %d", r.Totals.Passed, r.Totals.Runs)
	}
}

// TestEvalFoundry runs the scenarios against the real model. It runs only with EVAL_MODEL=foundry, from the
// Evaluation workflow or by hand with a workload identity; it costs money.
func TestEvalFoundry(t *testing.T) {
	if os.Getenv(envModel) != providerFoundry {
		t.Skip("set EVAL_MODEL=foundry to evaluate the real model")
	}
	repeats, err := envInt(envRepeats, 1, maxRepeats)
	if err != nil {
		t.Fatal(err)
	}
	rate, err := envFloat(envPassRate, 1, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	p, err := envPrices(os.Getenv(envEscDep) != "")
	if err != nil {
		t.Fatal(err)
	}
	src, err := newFoundrySource()
	if err != nil {
		t.Fatal(err)
	}
	r := evaluate(t, providerFoundry, src, repeats, rate, p)
	if !r.Totals.Pass {
		t.Errorf("the run did not reach the pass threshold: %d of %d passed", r.Totals.Passed, r.Totals.Runs)
	}
}

// TestEvalAnthropic runs the scenarios against a Claude model through the Anthropic API. It runs only with
// EVAL_MODEL=anthropic, by hand with an API key in a private file (EVAL_ANTHROPIC_KEY_FILE); it costs money.
// There is no fallback to Foundry or to another model.
func TestEvalAnthropic(t *testing.T) {
	if os.Getenv(envModel) != providerAnthropic {
		t.Skip("set EVAL_MODEL=anthropic to evaluate a Claude model")
	}
	repeats, err := envInt(envRepeats, 1, maxRepeats)
	if err != nil {
		t.Fatal(err)
	}
	rate, err := envFloat(envPassRate, 1, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	src, err := newAnthropicSource()
	if err != nil {
		t.Fatal(err)
	}
	p, err := envPrices(src.escalation != nil)
	if err != nil {
		t.Fatal(err)
	}
	r := evaluate(t, providerAnthropic, src, repeats, rate, p)
	if !r.Totals.Pass {
		t.Errorf("the run did not reach the pass threshold: %d of %d passed", r.Totals.Passed, r.Totals.Runs)
	}
}

func envInt(name string, def, hi int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > hi {
		return 0, fmt.Errorf("%s must be an integer from %d to %d", name, 1, hi)
	}
	return n, nil
}

func envFloat(name string, def, lo, hi float64) (float64, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	// NaN passes every comparison with a bound, and an infinity is no threshold or price.
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < lo || f > hi {
		return 0, fmt.Errorf("%s must be a number from %g to %g", name, lo, hi)
	}
	return f, nil
}

// envPrices reads the prices; none set means no cost estimate, and a partial set is an error. The escalation
// deployment's own prices (EVAL_ESCALATION_PRICE_*) are optional and need the triage ones and an escalation
// deployment.
func envPrices(escalationConfigured bool) (*prices, error) {
	p, err := envPriceSet([]string{envPriceIn, envPriceHit, envPriceOut})
	if err != nil {
		return nil, err
	}
	esc, err := envPriceSet([]string{envEscPrices + "INPUT_PER_MTOK", envEscPrices + "CACHED_INPUT_PER_MTOK",
		envEscPrices + "OUTPUT_PER_MTOK"})
	switch {
	case err != nil:
		return nil, err
	case esc != nil && !escalationConfigured:
		return nil, fmt.Errorf("the %s* prices need %s", envEscPrices, envEscDep)
	case esc != nil && p == nil:
		return nil, fmt.Errorf("the %s* prices need %s, %s and %s", envEscPrices, envPriceIn, envPriceHit, envPriceOut)
	case p != nil:
		p.Escalation = esc
	}
	return p, nil
}

// envPriceSet reads the input, cached input and output prices of one deployment from names.
func envPriceSet(names []string) (*prices, error) {
	var vals [3]float64
	set := 0
	for i, n := range names {
		if os.Getenv(n) == "" {
			continue
		}
		f, err := envFloat(n, 0, 0, 1000)
		if err != nil {
			return nil, err
		}
		vals[i], set = f, set+1
	}
	switch set {
	case 0:
		return nil, nil
	case len(names):
		return &prices{Input: vals[0], CachedInput: vals[1], Output: vals[2]}, nil
	}
	return nil, fmt.Errorf("set all of %v or none", names)
}
