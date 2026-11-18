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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/providers"
)

// reportSchemaVersion is the version of the report format (docs/evaluation.md). Bump it on any change of
// a field's meaning.
// Version 2: costUSD sums each role at its own prices (costUSDByRole), and usageUnknownCalls counts failed
// calls too.
// Version 3: a fact is scored on its value from any source the context holds (anyOf), and a subject fact from
// the Incident (docs/evaluation.md); diagnosis scores of version 2 are not comparable.
// Version 4: a run passes only when its expected reads (reads) pass too, a run carries its own cost, a run that
// stopped with an error makes the cost incomplete, and the totals give the cost per passed run (#505).
// Version 5: every model-sized proposal states the evidence-supported size, and any value above it fails.
// Version 6: rubric terms match on word boundaries (termIn, #848), so "256" is no longer taken from "256Mi";
// diagnosis scores of version 5 or earlier are not comparable.
// Version 7: a run fails when a quote of its accepted result holds the cut note, a tally line or a part the
// summary cut (summaryQuotes, #841).
// Version 8: each run gives its elapsed seconds, and the totals give seconds, secondsPerPassed and
// tokensPerPassed; the mode can be openaicompatible.
const reportSchemaVersion = 8

// report is the result of one evaluation run.
type report struct {
	SchemaVersion int `json:"schemaVersion"`
	// Mode is fake (recorded replies), foundry, anthropic or openaicompatible (a real model through that provider).
	Mode      string           `json:"mode"`
	Manifest  manifest         `json:"manifest"`
	Scenarios []scenarioResult `json:"scenarios"`
	Totals    totals           `json:"totals"`
}

// scenarioResult is one run of one scenario.
type scenarioResult struct {
	// prompt is the messages of the first model request: the system prompt and the context (not reported).
	prompt []providers.Message
	// toolText is the text of every tool result of the run, in order (not reported).
	toolText []string
	// truncated and failedSources are the escalation signals of the context the product built, before any
	// forced escalation (not reported).
	truncated     bool
	failedSources []incidentcontext.Section
	Name          string  `json:"name"`
	Seconds       float64 `json:"seconds,omitempty"`
	Repeat        int     `json:"repeat"`
	Pass          bool    `json:"pass"`
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
	// Reads scores the read tool calls the loop ran against the rubric's expected reads (#505).
	Reads readsScore `json:"reads"`
	// CostUSD is the run's estimated cost from the prices the run was given, nil without prices; CostUSDByRole
	// splits it by role, and CostIncomplete says it is a lower bound (calls without a usage, or a run that
	// stopped with an error).
	CostUSD        *float64           `json:"costUSD,omitempty"`
	CostUSDByRole  map[string]float64 `json:"costUSDByRole,omitempty"`
	CostIncomplete bool               `json:"costIncomplete,omitempty"`
	// CostUnknown says why CostUSD is left out although the run was given prices: a role used tokens and has no
	// prices of its own (prices.forRole). CostUSDByRole still holds the roles that have prices.
	CostUnknown string `json:"costUnknown,omitempty"`
	// QuoteFailures says, for a run that ended NoEvidence, how each quote differs from the block it cites
	// (#474): a class and lengths, never the quote or the block.
	QuoteFailures []quoteFailure `json:"quoteFailures,omitempty"`
	// SummaryQuotes are the quotes of an accepted result (not NoEvidence) that lie within no value their cited
	// block shows and touch the summary (#841): a cut part, the note of a cut or a tally line, which fail the run,
	// and a quote of the summary changed a little or not in it, only recorded (summaryQuote.Fails).
	SummaryQuotes []summaryQuote `json:"summaryQuotes,omitempty"`
	// Reply is the text of the last reply when it decoded as a result (#600), accepted or refused as NoEvidence,
	// so a missed rubric fact or a failed quote can be traced to what the model wrote; nil otherwise.
	Reply *replyText `json:"reply,omitempty"`
	// Error is set when the scenario could not run; it then fails.
	Error string `json:"error,omitempty"`
}

// The caps of the reply text in the report, in bytes of UTF-8 with the truncation mark included, and in items.
// They are the result's own bounds (analysis.Result: summary 1024 bytes, a hypothesis or a quote 512, at most
// 10 quotes; it allows 5 hypotheses), so a decoded reply is kept whole today; they are here so that a
// raised bound cannot grow the report unbounded: at most 1024 + 20 * 512 bytes of text per run.
const (
	replySummaryCap = 1024
	replyItemCap    = 512
	replyMaxItems   = 10
)

// truncationMark ends a field the caps cut.
const truncationMark = " [truncated]"

// replyText is the diagnosis and the evidence of the last reply that decoded as a result, redacted as the
// analyzer redacts the result's text (keeping the Incident's name) and capped (replySummaryCap and the others).
type replyText struct {
	Summary    string          `json:"summary"`
	Hypotheses []string        `json:"hypotheses,omitempty"`
	Evidence   []replyEvidence `json:"evidence,omitempty"`
	// Truncated says a field was cut, or items left out, by the caps.
	Truncated bool `json:"truncated,omitempty"`
}

// replyEvidence is one quote of the result and the ID of the block it cites.
type replyEvidence struct {
	Source string `json:"source"`
	Quote  string `json:"quote"`
}

// replyOf keeps the text of res for the report, each field passed through redact, then capped.
func replyOf(res *analysis.Result, redact func(string) string) *replyText {
	r := &replyText{}
	keep := func(s string, limit int) string {
		s = redact(s)
		if len(s) <= limit {
			return s
		}
		r.Truncated = true
		n := limit - len(truncationMark)
		for n > 0 && !utf8.RuneStart(s[n]) { // never split a UTF-8 sequence
			n--
		}
		return s[:n] + truncationMark
	}
	r.Summary = keep(res.Summary, replySummaryCap)
	for i, h := range res.Hypotheses {
		if i == replyMaxItems {
			r.Truncated = true
			break
		}
		r.Hypotheses = append(r.Hypotheses, keep(h.Description, replyItemCap))
	}
	for i, e := range res.Evidence {
		if i == replyMaxItems {
			r.Truncated = true
			break
		}
		r.Evidence = append(r.Evidence,
			replyEvidence{Source: keep(e.Source, replyItemCap), Quote: keep(e.Quote, replyItemCap)})
	}
	return r
}

// quoteFailure is one quote the evidence check refused ([analysis.QuoteFailure]).
type quoteFailure struct {
	Source       string `json:"source"`
	Block        string `json:"block,omitempty"`
	Class        string `json:"class"`
	QuoteBytes   int    `json:"quoteBytes"`
	BlockBytes   int    `json:"blockBytes"`
	MatchedBytes int    `json:"matchedBytes,omitempty"`
}

// tokens are the token counts of a run.
type tokens struct {
	Input       int64 `json:"input"`
	CachedInput int64 `json:"cachedInput"`
	Output      int64 `json:"output"`
	// Charged is what the loop charged against its token bound (Outcome.Tokens): the reported usage, or a
	// call's upper bound when its usage is unknown.
	Charged int64 `json:"charged"`
	// UsageUnknownCalls counts calls without a usage to rest the cost on: a usage the adapter could not read, or
	// a call that failed (other than with a malformed reply). The loop charges each its upper bound.
	UsageUnknownCalls int `json:"usageUnknownCalls"`
	// ByRole splits the reported counts by the role of the call (triage, escalation); roles without a call are
	// left out. Charged is the loop's count and has no split.
	ByRole map[string]roleTokens `json:"byRole,omitempty"`
}

// roleTokens are the token counts of the calls of one role.
type roleTokens struct {
	// Calls counts the model calls of the role, failed ones included.
	Calls             int   `json:"calls"`
	Input             int64 `json:"input"`
	CachedInput       int64 `json:"cachedInput"`
	Output            int64 `json:"output"`
	UsageUnknownCalls int   `json:"usageUnknownCalls"`
}

func (r roleTokens) plus(o roleTokens) roleTokens {
	return roleTokens{Calls: r.Calls + o.Calls, Input: r.Input + o.Input, CachedInput: r.CachedInput + o.CachedInput,
		Output:            r.Output + o.Output,
		UsageUnknownCalls: r.UsageUnknownCalls + o.UsageUnknownCalls}
}

// addRole counts the calls of one role, in the totals and in ByRole.
func (t *tokens) addRole(role string, o roleTokens) {
	t.Input += o.Input
	t.CachedInput += o.CachedInput
	t.Output += o.Output
	t.UsageUnknownCalls += o.UsageUnknownCalls
	if t.ByRole == nil {
		t.ByRole = map[string]roleTokens{}
	}
	t.ByRole[role] = t.ByRole[role].plus(o)
}

func (t *tokens) add(o tokens) {
	t.Input += o.Input
	t.CachedInput += o.CachedInput
	t.Output += o.Output
	t.Charged += o.Charged
	t.UsageUnknownCalls += o.UsageUnknownCalls
	for role, rt := range o.ByRole {
		if t.ByRole == nil {
			t.ByRole = map[string]roleTokens{}
		}
		t.ByRole[role] = t.ByRole[role].plus(rt)
	}
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
	Pass bool `json:"pass"`
	// Seconds includes all runs, successful or failed, measured with a monotonic clock.
	Seconds float64 `json:"seconds,omitempty"`
	// SecondsPerPassed and TokensPerPassed (charged) amortize all runs, failed ones included, over the passed
	// runs. Absent when no run passed. The cost per passed run is CostPerPassedRunUSD.
	SecondsPerPassed *float64 `json:"secondsPerPassed,omitempty"`
	TokensPerPassed  *float64 `json:"tokensPerPassed,omitempty"`
	Tokens           tokens   `json:"tokens"`
	// ToolCalls sums the runs' tool calls.
	ToolCalls int `json:"toolCalls"`
	// CostUSD is the estimated cost from the prices the run was given, nil without prices.
	CostUSD *float64 `json:"costUSD,omitempty"`
	// CostUSDByRole splits CostUSD by the role of the calls (Tokens.ByRole), each at its deployment's prices.
	CostUSDByRole map[string]float64 `json:"costUSDByRole,omitempty"`
	// CostIncomplete says the estimate leaves out what some runs used (IncompleteRuns): a lower bound, not the
	// cost.
	CostIncomplete bool `json:"costIncomplete,omitempty"`
	// IncompleteRuns counts the runs whose cost is a lower bound: a call without a usage, or a run that stopped
	// with an error.
	IncompleteRuns int `json:"incompleteRuns"`
	// CostPerPassedRunUSD is CostUSD, every run's cost failed ones included, divided by Passed: what one pass
	// cost. Nil without prices or when no run passed; a lower bound when CostIncomplete is set.
	CostPerPassedRunUSD *float64 `json:"costPerPassedRunUSD,omitempty"`
	// CostUnknown says why CostUSD and CostPerPassedRunUSD are left out although the run was given prices: a role
	// used tokens and has no prices of its own. CostUSDByRole still holds the roles that have prices.
	CostUnknown string `json:"costUnknown,omitempty"`
}

// prices are USD per million tokens of a deployment, from the run's configuration; nothing is built in.
type prices struct {
	Input, CachedInput, Output float64
	// Escalation are the escalation deployment's prices; nil when it has none, and then the cost of anything the
	// escalation used is unknown, never priced at the triage deployment's prices.
	Escalation *prices
}

// cost is the estimated cost of the counts at these prices. Cached input tokens are part of the input count and
// billed at their own price.
func (p *prices) cost(input, cachedInput, output int64) float64 {
	return (float64(input-cachedInput)*p.Input + float64(cachedInput)*p.CachedInput + float64(output)*p.Output) / 1e6
}

// estimate is the estimated cost of t, in all and by role. Every count is first priced at the triage prices,
// then the counts of each role are moved to that role's prices: tokens not attributed to a role stay at the
// triage prices. unknown is set when a role that used tokens, or made a call of unknown usage, has no prices:
// the total is then unknown and left out (ok false), and byRole holds the priced roles only.
func (p *prices) estimate(t tokens) (cost float64, byRole map[string]float64, unknown string, ok bool) {
	cost = p.cost(t.Input, t.CachedInput, t.Output)
	var unpriced []string
	for _, role := range slices.Sorted(maps.Keys(t.ByRole)) {
		rt := t.ByRole[role]
		rp := p.forRole(role)
		if rp == nil {
			if rt.Input > 0 || rt.Output > 0 || rt.UsageUnknownCalls > 0 {
				unpriced = append(unpriced, role)
			}
			continue
		}
		c := rp.cost(rt.Input, rt.CachedInput, rt.Output)
		if byRole == nil {
			byRole = map[string]float64{}
		}
		byRole[role] = c
		cost += c - p.cost(rt.Input, rt.CachedInput, rt.Output)
	}
	if len(unpriced) > 0 {
		return 0, byRole, fmt.Sprintf("no prices for the %s deployment, whose calls used tokens (set %s*)",
			strings.Join(unpriced, ", "), envEscPrices), false
	}
	return cost, byRole, "", true
}

// forRole returns the prices of a role's deployment, nil when it has none: the escalation deployment without
// its own prices, or a role this harness does not know. A role is never priced at another role's prices.
func (p *prices) forRole(role string) *prices {
	switch role {
	case string(analysis.RoleTriage):
		return p
	case string(analysis.RoleEscalation):
		return p.Escalation
	}
	return nil
}

// manifest is the evaluation manifest of ADR-0025 decision 3: the inputs that affect behaviour, and its
// digest.
type manifest struct {
	// Provider is the model provider of the run when it is anthropic; left out for fake and foundry, whose
	// manifests and digests stay as they were before the field.
	Provider string `json:"provider,omitempty"`
	// Anthropic holds the settings of the anthropic provider; left out for the others.
	Anthropic *manifestAnthropic `json:"anthropic,omitempty"`
	// Compatible identifies the server and endpoint without storing its URL or credential.
	Compatible *manifestCompatible `json:"openAICompatible,omitempty"`
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
	// Scenarios is the hash of every scenario's incident.yaml, objects.yaml, metrics.yaml, logs.yaml, replies.yaml
	// and policy.yaml, and of every file of its variants, and of the same files and the scenario.yaml of every
	// escalation scenario: what the model is shown (and, in fake mode, answers).
	Scenarios string         `json:"scenarios"`
	Rubric    manifestRubric `json:"rubric"`
	// ForcedEscalation says every run went to the escalation model (EVAL_FORCE_ESCALATION): a benchmark of
	// the escalation model, not of the deployment as it runs. Left out when false.
	ForcedEscalation bool `json:"forcedEscalation,omitempty"`
	// MinPassRate is the pass threshold of the run, 0 to 1.
	MinPassRate float64 `json:"minPassRate"`
	Repeats     int     `json:"repeats"`
	// Digest is the SHA-256 of the manifest's JSON without this field (see manifestDigest).
	Digest string `json:"digest,omitempty"`
}

// manifestCompatible identifies the qualified server without storing its endpoint URL.
type manifestCompatible struct {
	ServerID     string `json:"serverID"`
	EndpointHash string `json:"endpointHash"`
}

// manifestAnthropic are the settings of the Anthropic API adapter that change the replies.
type manifestAnthropic struct {
	// Effort is the output_config.effort sent with every call.
	Effort string `json:"effort"`
	// ThinkingAllowance is added to maxOutputTokens in max_tokens, as room for thinking.
	ThinkingAllowance int `json:"thinkingAllowance"`
}

type manifestRunLimits struct {
	MaxToolSteps    int `json:"maxToolSteps"`
	MaxOutputTokens int `json:"maxOutputTokens"`
}

type manifestModel struct {
	Deployment string `json:"deployment"`
	Model      string `json:"model"`
	APIVersion string `json:"apiVersion"`
	// InferenceAPI is the deployment's inferenceAPI, left out when it is (chat completions), so the digests of
	// earlier runs stay the same.
	InferenceAPI string `json:"inferenceAPI,omitempty"`
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
	Policy  int `json:"policy"`
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
	// Files is the hash of every scenario's and escalation scenario's expected.yaml and rubric.yaml.
	Files string `json:"files"`
}

// buildManifest fills the manifest of a run and its digest.
func buildManifest(src source, models modelSource, defs []providers.ToolDefinition, minPassRate float64,
	repeats int) (manifest, error) {
	return buildManifestIn(scenariosDir, escalationDir, src, models, defs, minPassRate, repeats)
}

// buildManifestIn is buildManifest over the scenarios in dir and the escalation scenarios in escDir.
func buildManifestIn(dir, escDir string, src source, models modelSource, defs []providers.ToolDefinition,
	minPassRate float64, repeats int) (manifest, error) {
	l := incidentcontext.DefaultLimits()
	m := manifest{
		Models:        map[string]manifestModel{},
		PromptVersion: analysis.PromptVersion(),
		MinPassRate:   minPassRate, Repeats: repeats, ForcedEscalation: models.forcesEscalation(),
		ContextBuilder: manifestContextBuilder{
			Limits: manifestLimits{Total: l.Total, Summary: l.Summary, Status: l.Status, Logs: l.Logs,
				Metrics: l.Metrics, Git: l.Git, Policy: l.Policy},
			// Recorded metrics: the metrics.yaml of the scenarios that have one, replayed (metrics_test.go).
			// Recorded logs: the logs.yaml of the escalation scenarios that have one, replayed (logs_test.go).
			// Policies: each scenario's policy.yaml, for the policy bounds section (#493).
			Sources: []string{"pods", "objects", "recorded metrics", "recorded logs", "policies"},
		},
		RunLimits: manifestRunLimits{MaxToolSteps: evalMaxToolSteps, MaxOutputTokens: evalMaxOutputTokens},
		Rubric:    manifestRubric{FleetRepository: src.FleetRepository, FleetCommit: src.FleetCommit},
	}
	m.Provider, m.Anthropic = models.manifestProvider()
	if c, ok := models.(*compatibleSource); ok {
		m.Compatible = &manifestCompatible{ServerID: c.serverID, EndpointHash: c.endpointHash}
	}
	triage, escalation := models.deployments()
	expectedModels := models.expectedModels()
	for role, d := range map[analysis.Role]*remediationv1alpha1.ModelDeployment{
		analysis.RoleTriage: triage, analysis.RoleEscalation: escalation,
	} {
		if d != nil {
			m.Models[string(role)] = manifestModel{Deployment: d.Deployment, Model: expectedModels[role],
				APIVersion: d.APIVersion, InferenceAPI: string(d.InferenceAPI)}
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
	// An escalation scenario's expected.yaml (a synthetic one's) and rubric.yaml are the rubric; its other files,
	// scenario.yaml included, what the model is shown and how the run is made.
	if m.Rubric.Files, err = hashFiles(filepath.Join(dir, "*", "expected.yaml"),
		filepath.Join(dir, "*", "rubric.yaml"), filepath.Join(escDir, "*", "expected.yaml"),
		filepath.Join(escDir, "*", "rubric.yaml")); err != nil {
		return m, err
	}
	if m.Scenarios, err = hashFiles(filepath.Join(dir, "*", "incident.yaml"), filepath.Join(dir, "*", "objects.yaml"),
		filepath.Join(dir, "*", metricsFile), filepath.Join(dir, "*", logsFile), filepath.Join(dir, "*", "replies.yaml"),
		filepath.Join(dir, "*", policyFile),
		filepath.Join(dir, "*", variantsDir, "*", "*.yaml"), filepath.Join(escDir, "*", "incident.yaml"),
		filepath.Join(escDir, "*", "objects.yaml"), filepath.Join(escDir, "*", metricsFile),
		filepath.Join(escDir, "*", logsFile),
		filepath.Join(escDir, "*", "replies.yaml"), filepath.Join(escDir, "*", policyFile),
		filepath.Join(escDir, "*", escalationFile)); err != nil {
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
	for i := range r.Scenarios {
		s := &r.Scenarios[i]
		if s.Pass {
			t.Passed++
		}
		t.Tokens.add(s.Tokens)
		t.Seconds += s.Seconds
		t.ToolCalls += s.ToolCalls
		// A failed call has no usage to price, and a run that stopped with an error may have used more than it
		// counted: its cost is a lower bound, never a zero.
		incomplete := s.Tokens.UsageUnknownCalls > 0 || s.Error != ""
		if incomplete {
			t.IncompleteRuns++
		}
		s.CostUSD, s.CostUSDByRole, s.CostIncomplete, s.CostUnknown = nil, nil, false, ""
		if p != nil {
			c, byRole, unknown, ok := p.estimate(s.Tokens)
			s.CostUSDByRole, s.CostUnknown = byRole, unknown
			if ok {
				s.CostUSD, s.CostIncomplete = &c, incomplete
			}
		}
	}
	if t.Runs > 0 {
		t.PassRate = float64(t.Passed) / float64(t.Runs)
	}
	t.Pass = t.Runs > 0 && t.PassRate >= r.Manifest.MinPassRate
	if p != nil {
		cost, byRole, unknown, ok := p.estimate(t.Tokens)
		t.CostUSDByRole, t.CostUnknown = byRole, unknown
		if ok {
			t.CostUSD = &cost
			t.CostIncomplete = t.IncompleteRuns > 0
		}
		if ok && t.Passed > 0 {
			per := cost / float64(t.Passed)
			t.CostPerPassedRunUSD = &per
		}
	}
	if t.Passed > 0 {
		seconds := t.Seconds / float64(t.Passed)
		tokens := float64(t.Tokens.Charged) / float64(t.Passed)
		t.SecondsPerPassed, t.TokensPerPassed = &seconds, &tokens
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
	if a := r.Manifest.Anthropic; a != nil {
		fmt.Fprintf(&b, "- Effort %s; thinking allowance %d tokens above the output limit\n", a.Effort,
			a.ThinkingAllowance)
	}
	if c := r.Manifest.Compatible; c != nil {
		fmt.Fprintf(&b, "- Server `%s`; endpoint `%s`\n", c.ServerID, c.EndpointHash)
	}
	fmt.Fprintf(&b, "- Prompt `%s`; rubric %s@%s\n", r.Manifest.PromptVersion, r.Manifest.Rubric.FleetRepository,
		r.Manifest.Rubric.FleetCommit[:min(12, len(r.Manifest.Rubric.FleetCommit))])
	fmt.Fprintf(&b, "- Passed %d of %d runs (%.0f%%, threshold %.0f%%, %d repeat(s))\n", r.Totals.Passed, r.Totals.Runs,
		100*r.Totals.PassRate, 100*r.Manifest.MinPassRate, r.Manifest.Repeats)
	if r.Totals.SecondsPerPassed != nil && r.Totals.Seconds > 0 {
		fmt.Fprintf(&b, "- Per passed run (all attempts included): %.2f seconds, %.0f charged tokens\n",
			*r.Totals.SecondsPerPassed, *r.Totals.TokensPerPassed)
	}
	tk := r.Totals.Tokens
	if r.Manifest.ForcedEscalation {
		b.WriteString("- Escalation forced (EVAL_FORCE_ESCALATION): every run went to the escalation model\n")
	}
	fmt.Fprintf(&b, "- Tokens: input %d (cached %d), output %d, charged %d, calls with unknown usage %d; tool calls %d\n",
		tk.Input, tk.CachedInput, tk.Output, tk.Charged, tk.UsageUnknownCalls, r.Totals.ToolCalls)
	for _, role := range []analysis.Role{analysis.RoleTriage, analysis.RoleEscalation} {
		rt, ok := tk.ByRole[string(role)]
		if !ok {
			continue
		}
		model := ""
		if m, ok := r.Manifest.Models[string(role)]; ok && m.Model != "" {
			model = " (" + m.Model + ")"
		}
		fmt.Fprintf(&b, "  - %s%s: calls %d, input %d (cached %d), output %d, calls with unknown usage %d", role, model,
			rt.Calls, rt.Input, rt.CachedInput, rt.Output, rt.UsageUnknownCalls)
		if c, ok := r.Totals.CostUSDByRole[string(role)]; ok {
			fmt.Fprintf(&b, ", estimated cost USD %.4f", c)
		} else if r.Totals.CostUnknown != "" {
			b.WriteString(", no prices, cost unknown")
		}
		b.WriteString("\n")
	}
	r.costMarkdown(&b)
	priced := r.Totals.CostUSD != nil || r.Totals.CostUnknown != ""
	b.WriteString("\n| Scenario | Run | Result | Outcome | Diagnosis | Reads | Escalated | Calls | Tokens |")
	sep := "|---|---|---|---|---|---|---|---|---|"
	if priced {
		b.WriteString(" Cost USD |")
		sep += "---|"
	}
	b.WriteString("\n" + sep + "\n")
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
		escalated := "no"
		if s.Escalated {
			escalated = "yes"
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %s | %s | %d+%d | %d |", s.Name, s.Repeat, res, cell(outcome), cell(diag),
			cell(readsCell(s.Reads)), escalated, s.Calls, s.ToolCalls, s.Tokens.Charged)
		if priced {
			switch {
			case s.CostUnknown != "":
				b.WriteString(" unknown |")
			case s.CostUSD == nil:
				b.WriteString(" - |")
			case s.CostIncomplete:
				fmt.Fprintf(&b, " %.4f (incomplete) |", *s.CostUSD)
			default:
				fmt.Fprintf(&b, " %.4f |", *s.CostUSD)
			}
		}
		b.WriteString("\n")
	}
	r.quoteFailuresMarkdown(&b)
	r.summaryQuotesMarkdown(&b)
	return b.String()
}

// costMarkdown writes the summary lines of the estimated cost, when the run was given prices.
func (r *report) costMarkdown(b *strings.Builder) {
	if r.Totals.CostUSD != nil {
		fmt.Fprintf(b, "- Estimated cost: USD %.4f", *r.Totals.CostUSD)
		if r.Totals.CostIncomplete {
			fmt.Fprintf(b, " (incomplete, a lower bound: %d call(s) failed or had unknown usage and are not priced, "+
				"%d run(s) incomplete; compare the charged tokens)", r.Totals.Tokens.UsageUnknownCalls, r.Totals.IncompleteRuns)
		}
		b.WriteString("\n")
		if c := r.Totals.CostPerPassedRunUSD; c != nil {
			fmt.Fprintf(b, "- Cost per passed run: USD %.4f (all runs' cost over %d passed)", *c, r.Totals.Passed)
			if r.Totals.CostIncomplete {
				b.WriteString(", incomplete, a lower bound")
			}
			b.WriteString("\n")
		} else {
			b.WriteString("- Cost per passed run: none, no run passed\n")
		}
	}
	if r.Totals.CostUnknown != "" {
		fmt.Fprintf(b, "- Estimated cost: unknown, %s\n- Cost per passed run: unknown\n", r.Totals.CostUnknown)
	}
}

// readsCell is the reads of a run for the Markdown table: expected reads made, extra and refused calls, and the
// expected reads missing.
func readsCell(rs readsScore) string {
	made := 0
	var missing []string
	for _, e := range rs.Expected {
		if e.Pass {
			made++
		} else {
			missing = append(missing, e.Read)
		}
	}
	out := fmt.Sprintf("%d/%d expected, %d extra, %d refused", made, len(rs.Expected), rs.Extra, rs.Refused)
	if len(missing) > 0 {
		out += "; missing " + strings.Join(missing, ", ")
	}
	return out
}

// quoteFailuresMarkdown lists the quotes of the runs that ended NoEvidence, one row each, when there are any.
func (r *report) quoteFailuresMarkdown(b *strings.Builder) {
	header := false
	for _, s := range r.Scenarios {
		for _, q := range s.QuoteFailures {
			if !header {
				b.WriteString("\n## Quotes refused as evidence\n\n")
				b.WriteString("| Scenario | Run | Source | Block | Class | Quote bytes | Block bytes | Matched bytes |\n")
				b.WriteString("|---|---|---|---|---|---|---|---|\n")
				header = true
			}
			fmt.Fprintf(b, "| %s | %d | %s | %s | %s | %d | %d | %d |\n", s.Name, s.Repeat, cell(q.Source), cell(q.Block),
				cell(q.Class), q.QuoteBytes, q.BlockBytes, q.MatchedBytes)
		}
	}
}

// summaryQuotesMarkdown lists the quotes checkSummaryQuotes reported (#841), one row each, those that failed
// the run and those only recorded, when there are any.
func (r *report) summaryQuotesMarkdown(b *strings.Builder) {
	header := false
	for _, s := range r.Scenarios {
		for _, q := range s.SummaryQuotes {
			if !header {
				b.WriteString("\n## Quotes not in the summary as shown\n\n")
				b.WriteString("| Scenario | Run | Source | Kind | Fails | Quote bytes |\n")
				b.WriteString("|---|---|---|---|---|---|\n")
				header = true
			}
			fmt.Fprintf(b, "| %s | %d | %s | %s | %t | %d |\n", s.Name, s.Repeat, cell(q.Source), cell(q.Kind), q.Fails,
				q.QuoteBytes)
		}
	}
}

// cell keeps a table cell on one line and its pipes literal.
func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ").Replace(s)
}

// readReport decodes a report.json of this harness's schema version. A report of another version is refused with
// the versions named, never read as this one: a field may mean something else there (docs/evaluation.md lists
// the changes), and a field this version does not know is refused too.
func readReport(raw []byte) (*report, error) {
	var head struct {
		SchemaVersion *int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("reading the report: %w", err)
	}
	switch {
	case head.SchemaVersion == nil:
		return nil, errors.New("reading the report: it has no schemaVersion, so it is not an evaluation report")
	case *head.SchemaVersion != reportSchemaVersion:
		return nil, fmt.Errorf("reading the report: its schema version is %d, this harness reads version %d only; "+
			"the fields changed meaning between versions (docs/evaluation.md, The report), so compare it only with "+
			"reports of its own version", *head.SchemaVersion, reportSchemaVersion)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r report
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("reading the report of schema version %d: %w", reportSchemaVersion, err)
	}
	return &r, nil
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
