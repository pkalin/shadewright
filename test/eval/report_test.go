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
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/providers"
)

// reportSchemaVersion is the version of the report format (docs/evaluation.md). Bump it on any change of
// a field's meaning.
const reportSchemaVersion = 1

// report is the result of one evaluation run.
type report struct {
	SchemaVersion int `json:"schemaVersion"`
	// Mode is fake (recorded replies) or foundry (the real model).
	Mode      string           `json:"mode"`
	Manifest  manifest         `json:"manifest"`
	Scenarios []scenarioResult `json:"scenarios"`
	Totals    totals           `json:"totals"`
}

// scenarioResult is one run of one scenario.
type scenarioResult struct {
	Name   string `json:"name"`
	Repeat int    `json:"repeat"`
	Pass   bool   `json:"pass"`
	// Recording is the recording's source (fake mode only).
	Recording string         `json:"recording,omitempty"`
	Reason    string         `json:"reason"`
	Escalated bool           `json:"escalated"`
	Calls     int            `json:"calls"`
	ToolCalls int            `json:"toolCalls"`
	Models    []string       `json:"models,omitempty"`
	Tokens    tokens         `json:"tokens"`
	Outcome   outcomeScore   `json:"outcome"`
	Diagnosis diagnosisScore `json:"diagnosis"`
	// Error is set when the scenario could not run; it then fails.
	Error string `json:"error,omitempty"`
}

// tokens are the token counts of a run.
type tokens struct {
	Input       int64 `json:"input"`
	CachedInput int64 `json:"cachedInput"`
	Output      int64 `json:"output"`
	// Charged is what the loop charged against its token bound (Outcome.Tokens): the reported usage, or a
	// call's upper bound when its usage is unknown.
	Charged int64 `json:"charged"`
	// UsageUnknownCalls counts calls that were billed with a usage the adapter could not read.
	UsageUnknownCalls int `json:"usageUnknownCalls"`
}

func (t *tokens) add(o tokens) {
	t.Input += o.Input
	t.CachedInput += o.CachedInput
	t.Output += o.Output
	t.Charged += o.Charged
	t.UsageUnknownCalls += o.UsageUnknownCalls
}

// outcomeScore is the outcome part of a scenario's score.
type outcomeScore struct {
	Pass bool `json:"pass"`
	// Expected is expected.yaml's outcome type.
	Expected string `json:"expected"`
	// Reason is the loop's reason; Action the planned action type, empty without a plan.
	Reason string `json:"reason"`
	Action string `json:"action,omitempty"`
	// Detail says why it failed.
	Detail string `json:"detail,omitempty"`
	// NotScored lists the parts of the expected outcome that the analysis does not decide.
	NotScored []string `json:"notScored,omitempty"`
}

// diagnosisScore is the diagnosis part.
type diagnosisScore struct {
	Pass     bool           `json:"pass"`
	Facts    []factScore    `json:"facts,omitempty"`
	Unscored []unscoredFact `json:"unscored,omitempty"`
	Detail   string         `json:"detail,omitempty"`
}

type factScore struct {
	Fact    string   `json:"fact"`
	Pass    bool     `json:"pass"`
	Missing []string `json:"missing,omitempty"`
}

type unscoredFact struct {
	Fact string `json:"fact"`
	Why  string `json:"why"`
}

// totals sum the run.
type totals struct {
	Runs     int     `json:"runs"`
	Passed   int     `json:"passed"`
	PassRate float64 `json:"passRate"`
	// Pass is whether PassRate reached the manifest's threshold.
	Pass   bool   `json:"pass"`
	Tokens tokens `json:"tokens"`
	// CostUSD is the estimated cost from the prices the run was given, nil without prices.
	CostUSD *float64 `json:"costUSD,omitempty"`
}

// prices are USD per million tokens of a deployment, from the run's configuration; nothing is built in.
type prices struct {
	Input, CachedInput, Output float64
}

// manifest is the evaluation manifest of ADR-0025 decision 3: the inputs that affect behaviour, and its
// digest.
type manifest struct {
	// Models are the deployments by role: deployment name and the model ID and version the replies must name.
	Models map[string]manifestModel `json:"models"`
	// PromptVersion is analysis.PromptVersion: the prompt templates and their generated parts.
	PromptVersion string `json:"promptVersion"`
	// ToolDefinitions is the hash of the tool definitions offered on escalation.
	ToolDefinitions string `json:"toolDefinitions"`
	// OutputSchema is the hash of the generated output schema and the action catalog.
	OutputSchema   string                 `json:"outputSchema"`
	ContextBuilder manifestContextBuilder `json:"contextBuilder"`
	Redaction      manifestRedaction      `json:"redaction"`
	// RunLimits are the loop limits of the run (the sample settings' maxToolSteps and maxOutputTokens).
	RunLimits manifestRunLimits `json:"runLimits"`
	// Scenarios is the hash of every scenario's incident.yaml, objects.yaml and replies.yaml: what the model is
	// shown (and, in fake mode, answers).
	Scenarios string         `json:"scenarios"`
	Rubric    manifestRubric `json:"rubric"`
	// MinPassRate is the pass threshold of the run, 0 to 1.
	MinPassRate float64 `json:"minPassRate"`
	Repeats     int     `json:"repeats"`
	// Digest is the SHA-256 of the manifest's JSON without this field (see manifestDigest).
	Digest string `json:"digest,omitempty"`
}

type manifestRunLimits struct {
	MaxToolSteps    int `json:"maxToolSteps"`
	MaxOutputTokens int `json:"maxOutputTokens"`
}

type manifestModel struct {
	Deployment string `json:"deployment"`
	Model      string `json:"model"`
	APIVersion string `json:"apiVersion"`
}

type manifestContextBuilder struct {
	// Source is the hash of the context builder's Go sources (internal/incidentcontext), which have no version
	// of their own.
	Source  string         `json:"source"`
	Limits  manifestLimits `json:"limits"`
	Sources []string       `json:"sources"`
}

// manifestLimits are the context builder's byte bounds (incidentcontext.Limits).
type manifestLimits struct {
	Total   int `json:"total"`
	Summary int `json:"summary"`
	Status  int `json:"status"`
	Logs    int `json:"logs"`
	Metrics int `json:"metrics"`
	Git     int `json:"git"`
}

type manifestRedaction struct {
	// BuiltInRules is the hash of package redact's Go sources, which hold the built-in rules.
	BuiltInRules string `json:"builtInRules"`
	// OperatorPatterns is the number of operator patterns: none, nothing configures them yet.
	OperatorPatterns int `json:"operatorPatterns"`
}

type manifestRubric struct {
	FleetRepository string `json:"fleetRepository"`
	FleetCommit     string `json:"fleetCommit"`
	// Files is the hash of every scenario's expected.yaml and rubric.yaml.
	Files string `json:"files"`
}

// buildManifest fills the manifest of a run and its digest.
func buildManifest(src source, models modelSource, defs []providers.ToolDefinition, minPassRate float64,
	repeats int) (manifest, error) {
	return buildManifestIn(scenariosDir, src, models, defs, minPassRate, repeats)
}

// buildManifestIn is buildManifest over the scenarios in dir.
func buildManifestIn(dir string, src source, models modelSource, defs []providers.ToolDefinition,
	minPassRate float64, repeats int) (manifest, error) {
	l := incidentcontext.DefaultLimits()
	m := manifest{
		Models:        map[string]manifestModel{},
		PromptVersion: analysis.PromptVersion(),
		MinPassRate:   minPassRate, Repeats: repeats,
		ContextBuilder: manifestContextBuilder{
			Limits: manifestLimits{Total: l.Total, Summary: l.Summary, Status: l.Status, Logs: l.Logs,
				Metrics: l.Metrics, Git: l.Git},
			Sources: []string{"pods", "objects"},
		},
		RunLimits: manifestRunLimits{MaxToolSteps: evalMaxToolSteps, MaxOutputTokens: evalMaxOutputTokens},
		Rubric:    manifestRubric{FleetRepository: src.FleetRepository, FleetCommit: src.FleetCommit},
	}
	triage, escalation := models.deployments()
	expectedModels := models.expectedModels()
	for role, d := range map[analysis.Role]*remediationv1alpha1.ModelDeployment{
		analysis.RoleTriage: triage, analysis.RoleEscalation: escalation,
	} {
		if d != nil {
			m.Models[string(role)] = manifestModel{Deployment: d.Deployment, Model: expectedModels[role],
				APIVersion: d.APIVersion}
		}
	}
	var err error
	if m.ToolDefinitions, err = hashJSON(defs); err != nil {
		return m, err
	}
	if m.OutputSchema, err = hashJSON(struct {
		Schema  json.RawMessage
		Catalog []remediationv1alpha1.ActionType
	}{analysis.ResponseSchema(), remediationv1alpha1.ActionTypes()}); err != nil {
		return m, err
	}
	if m.ContextBuilder.Source, err = hashFiles("../../internal/incidentcontext/*.go"); err != nil {
		return m, err
	}
	if m.Redaction.BuiltInRules, err = hashFiles("../../internal/redact/*.go"); err != nil {
		return m, err
	}
	if m.Rubric.Files, err = hashFiles(filepath.Join(dir, "*", "expected.yaml"),
		filepath.Join(dir, "*", "rubric.yaml")); err != nil {
		return m, err
	}
	if m.Scenarios, err = hashFiles(filepath.Join(dir, "*", "incident.yaml"), filepath.Join(dir, "*", "objects.yaml"),
		filepath.Join(dir, "*", "replies.yaml")); err != nil {
		return m, err
	}
	if m.Digest, err = manifestDigest(m); err != nil {
		return m, err
	}
	return m, nil
}

// manifestDigest is the SHA-256 of the manifest's JSON without the digest field, so the stored manifest minus
// its digest hashes to the stored digest.
func manifestDigest(m manifest) (string, error) {
	m.Digest = ""
	return hashJSON(m)
}

func hashJSON(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// hashFiles hashes the files the patterns match, test files left out, by name and content in name order.
func hashFiles(patterns ...string) (string, error) {
	var files []string
	for _, p := range patterns {
		m, err := filepath.Glob(p)
		if err != nil {
			return "", err
		}
		for _, f := range m {
			if !strings.HasSuffix(f, "_test.go") {
				files = append(files, f)
			}
		}
	}
	if len(files) == 0 {
		return "", fmt.Errorf("no files match %q", patterns)
	}
	slices.Sort(files)
	h := sha256.New()
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		h.Write(fmt.Appendf(nil, "%s\x00%d\x00", filepath.ToSlash(f), len(raw)))
		h.Write(raw)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// summarise fills the totals.
func (r *report) summarise(p *prices) {
	t := totals{Runs: len(r.Scenarios)}
	for _, s := range r.Scenarios {
		if s.Pass {
			t.Passed++
		}
		t.Tokens.add(s.Tokens)
	}
	if t.Runs > 0 {
		t.PassRate = float64(t.Passed) / float64(t.Runs)
	}
	t.Pass = t.Runs > 0 && t.PassRate >= r.Manifest.MinPassRate
	if p != nil {
		// Cached input tokens are part of the input count and billed at their own price.
		cost := (float64(t.Tokens.Input-t.Tokens.CachedInput)*p.Input + float64(t.Tokens.CachedInput)*p.CachedInput +
			float64(t.Tokens.Output)*p.Output) / 1e6
		t.CostUSD = &cost
	}
	r.Totals = t
}

// markdown renders the report for a person: the totals, then one row per scenario run.
func (r *report) markdown() string {
	var b strings.Builder
	verdict := "FAIL"
	if r.Totals.Pass {
		verdict = "PASS"
	}
	fmt.Fprintf(&b, "# Evaluation: %s\n\n", verdict)
	fmt.Fprintf(&b, "- Mode: %s; manifest digest `%s`\n", r.Mode, r.Manifest.Digest)
	fmt.Fprintf(&b, "- Prompt `%s`; rubric %s@%s\n", r.Manifest.PromptVersion, r.Manifest.Rubric.FleetRepository,
		r.Manifest.Rubric.FleetCommit[:min(12, len(r.Manifest.Rubric.FleetCommit))])
	fmt.Fprintf(&b, "- Passed %d of %d runs (%.0f%%, threshold %.0f%%, %d repeat(s))\n", r.Totals.Passed, r.Totals.Runs,
		100*r.Totals.PassRate, 100*r.Manifest.MinPassRate, r.Manifest.Repeats)
	tk := r.Totals.Tokens
	fmt.Fprintf(&b, "- Tokens: input %d (cached %d), output %d, charged %d, calls with unknown usage %d\n",
		tk.Input, tk.CachedInput, tk.Output, tk.Charged, tk.UsageUnknownCalls)
	if r.Totals.CostUSD != nil {
		fmt.Fprintf(&b, "- Estimated cost: USD %.4f\n", *r.Totals.CostUSD)
	}
	b.WriteString("\n| Scenario | Run | Result | Outcome | Diagnosis | Calls | Tokens |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, s := range r.Scenarios {
		res := "fail"
		if s.Pass {
			res = "pass"
		}
		outcome := fmt.Sprintf("%s: %s", s.Outcome.Expected, s.Reason)
		if s.Outcome.Action != "" {
			outcome += " " + s.Outcome.Action
		}
		if s.Outcome.Detail != "" {
			outcome += " (" + s.Outcome.Detail + ")"
		}
		if s.Error != "" {
			outcome = "error: " + s.Error
		}
		passed := 0
		var missing []string
		for _, f := range s.Diagnosis.Facts {
			if f.Pass {
				passed++
			} else {
				missing = append(missing, f.Missing...)
			}
		}
		diag := fmt.Sprintf("%d/%d facts, %d unscored", passed, len(s.Diagnosis.Facts), len(s.Diagnosis.Unscored))
		if len(missing) > 0 {
			diag += "; missing " + strings.Join(missing, ", ")
		}
		if s.Diagnosis.Detail != "" {
			diag += "; " + s.Diagnosis.Detail
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %d+%d | %d |\n", s.Name, s.Repeat, res, cell(outcome), cell(diag),
			s.Calls, s.ToolCalls, s.Tokens.Charged)
	}
	return b.String()
}

// cell keeps a table cell on one line and its pipes literal.
func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ").Replace(s)
}

// write saves report.json and report.md in dir, the directory the person running the evaluation chose
// (EVAL_REPORT_DIR): never cluster or model data.
func (r *report) write(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil { // #nosec G703 -- the caller's own report directory
		return err
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	jsonPath, mdPath := filepath.Join(dir, "report.json"), filepath.Join(dir, "report.md")
	if err := os.WriteFile(jsonPath, append(raw, '\n'), 0o600); err != nil { // #nosec G703 -- as above
		return err
	}
	return os.WriteFile(mdPath, []byte(r.markdown()), 0o600) // #nosec G703 -- as above
}
