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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/shadewright-labs/shadewright/internal/analysis"
	"github.com/shadewright-labs/shadewright/internal/incidentcontext"
)

// The escalation scenarios (#504): recorded cases whose diagnosis rests on a fact only a read tool shows, so
// triage alone cannot reach it. Each runs on the escalation model with the read tools from its first call,
// whatever the model source. A scenario without a signal escalates through the same failed-source signal as
// EVAL_FORCE_ESCALATION (forcedEscalation): no deterministic signal of the context fires for it. A scenario
// with a signal is not forced: the product's own builder raises it (a context cut to its total bound, a
// configured source that failed), and TestEscalationScenarioSignal checks that it does.

// escalationDir holds one directory per escalation scenario.
const escalationDir = "testdata/escalation"

// escalationPrefix starts the report name of an escalation scenario: escalation/<directory>.
const escalationPrefix = "escalation/"

// The escalation scenarios that tests name.
const (
	escCutContext   = "cut-context"
	escMetricsDown  = "metrics-down"
	escOOMConfigKey = "oom-config-key"
)

// escalationFile describes an escalation scenario.
const escalationFile = "scenario.yaml"

// escalationScenario is a scenario.yaml.
type escalationScenario struct {
	// About says what the scenario tests and which read shows the deciding fact.
	About string `json:"about"`
	// FleetScenario names the fleet scenario (testdata/scenarios/<name>) whose expected.yaml, incident.yaml,
	// objects.yaml and policy.yaml it takes, unchanged; empty for a synthetic scenario with its own files.
	FleetScenario string `json:"fleetScenario,omitempty"`
	// BehindReads names the facts of rubric.yaml, verbatim, that only the expected reads show: their terms are
	// never in the first request the product builds, and always in a tool result of the recorded run.
	BehindReads []string `json:"behindReads"`
	// Signal is the product's own escalation signal the scenario relies on: "truncated" (the builder cut the
	// context to its total bound) or "failedSource" (a configured source failed). Empty: no signal fires, and
	// the harness forces the escalation.
	Signal escalationSignal `json:"signal,omitempty"`
	// ContextBytes is the builder's total bound on the context, the others staying at their defaults; it is
	// required with signal truncated and refused otherwise. The default bounds add up to the default total, so
	// only a total below their sum (one maxContextBytes allows, 4096 to 49152) can cut the context.
	ContextBytes int `json:"contextBytes,omitempty"`
	// MetricsSource "down" configures a metrics source that fails every query, a Prometheus that is down. It
	// is required with signal failedSource and refused with a metrics.yaml.
	MetricsSource string `json:"metricsSource,omitempty"`
}

// escalationSignal is the product's escalation signal an escalation scenario relies on.
type escalationSignal string

const (
	signalTruncated    escalationSignal = "truncated"
	signalFailedSource escalationSignal = "failedSource"
	// metricsDown is the one value of metricsSource.
	metricsDown = "down"
)

// dataFiles are the files an escalation scenario of a fleet scenario takes from it, and may not hold itself.
var dataFiles = []string{"expected.yaml", "incident.yaml", "objects.yaml", metricsFile, logsFile, policyFile}

// loadEscalationScenarios reads every escalation scenario directory, sorted by name.
func loadEscalationScenarios(scheme *runtime.Scheme) ([]*scenario, error) {
	entries, err := os.ReadDir(escalationDir)
	if err != nil {
		return nil, fmt.Errorf("reading the escalation scenarios: %w", err)
	}
	var out []*scenario
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := loadEscalationScenario(scheme, e.Name())
		if err != nil {
			return nil, fmt.Errorf("escalation scenario %s: %w", e.Name(), err)
		}
		out = append(out, s)
	}
	return out, nil
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	}
	return false, err
}

// loadEscalationScenario loads the escalation scenario in escalationDir/name.
func loadEscalationScenario(scheme *runtime.Scheme, name string) (*scenario, error) {
	dir := filepath.Join(escalationDir, name)
	var meta escalationScenario
	if err := readStrict(filepath.Join(dir, escalationFile), &meta); err != nil {
		return nil, err
	}
	if strings.TrimSpace(meta.About) == "" {
		return nil, fmt.Errorf("%s: about is required", escalationFile)
	}
	if len(meta.BehindReads) == 0 {
		return nil, fmt.Errorf("%s: behindReads needs at least one fact", escalationFile)
	}
	if err := meta.checkSignal(); err != nil {
		return nil, err
	}
	variants, err := exists(filepath.Join(dir, variantsDir))
	if err != nil {
		return nil, err
	}
	if variants {
		return nil, errors.New("an escalation scenario has no variants")
	}
	dataDir, expectedName := dir, name
	fleet, err := exists(filepath.Join(scenariosDir, name))
	if err != nil {
		return nil, err
	}
	if meta.FleetScenario != "" {
		dataDir, expectedName = filepath.Join(scenariosDir, meta.FleetScenario), meta.FleetScenario
		for _, f := range dataFiles {
			own, err := exists(filepath.Join(dir, f))
			if err != nil {
				return nil, err
			}
			if own {
				return nil, fmt.Errorf("%s comes from the fleet scenario %s; the directory may not hold its own",
					f, meta.FleetScenario)
			}
		}
	} else if fleet {
		// A synthetic expected.yaml under a fleet scenario's name would read as the fleet's.
		return nil, fmt.Errorf("a synthetic escalation scenario may not take the fleet scenario name %s", name)
	}
	s, err := loadScenarioFiles(scheme, dataDir, dir, escalationPrefix+name, expectedName)
	if err != nil {
		return nil, err
	}
	if meta.MetricsSource == metricsDown {
		if s.metrics != nil {
			return nil, fmt.Errorf("%s: metricsSource down and a %s exclude each other", escalationFile, metricsFile)
		}
		s.metricsDown = true
	}
	if meta.ContextBytes != 0 {
		s.limits = incidentcontext.DefaultLimits()
		s.limits.Total = meta.ContextBytes
		if err := s.limits.Validate(); err != nil {
			return nil, fmt.Errorf("%s: contextBytes: %w", escalationFile, err)
		}
	}
	if len(s.reads) == 0 {
		return nil, errors.New("rubric.yaml: an escalation scenario must list the reads it expects")
	}
	for _, name := range meta.BehindReads {
		i := slices.IndexFunc(s.rubric.Facts, func(f rubricFact) bool { return f.Fact == name })
		if i < 0 || s.rubric.Facts[i].Unscored != "" {
			return nil, fmt.Errorf("%s: behindReads names %q, which is no scored fact of rubric.yaml", escalationFile,
				name)
		}
		s.behindReads = append(s.behindReads, s.rubric.Facts[i])
	}
	s.forceEscalation, s.escalates, s.signal = meta.Signal == "", true, meta.Signal
	return s, nil
}

// checkSignal checks the signal and the settings that raise it: contextBytes only with truncated, a metrics
// source that is down with failedSource and only there.
func (m *escalationScenario) checkSignal() error {
	switch m.Signal {
	case "", signalTruncated, signalFailedSource:
	default:
		return fmt.Errorf("%s: signal %q is not truncated or failedSource", escalationFile, m.Signal)
	}
	if (m.Signal == signalTruncated) != (m.ContextBytes != 0) {
		return fmt.Errorf("%s: contextBytes is required with signal truncated and refused otherwise", escalationFile)
	}
	if m.MetricsSource != "" && m.MetricsSource != metricsDown {
		return fmt.Errorf("%s: metricsSource %q is not down", escalationFile, m.MetricsSource)
	}
	if (m.Signal == signalFailedSource) != (m.MetricsSource == metricsDown) {
		return fmt.Errorf("%s: metricsSource down is required with signal failedSource and refused otherwise",
			escalationFile)
	}
	return nil
}

// mustEscalationScenarios loads the escalation scenarios, and fails when there are none.
func mustEscalationScenarios(t *testing.T) (*runtime.Scheme, []*scenario) {
	t.Helper()
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	all, err := loadEscalationScenarios(scheme)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("no escalation scenarios")
	}
	return scheme, all
}

// Each escalation scenario passes on its recorded replies, on the escalation model from its first call, with
// every expected read made, although the replay source forces nothing.
func TestEscalationScenariosPass(t *testing.T) {
	scheme, all := mustEscalationScenarios(t)
	for _, s := range all {
		t.Run(s.name, func(t *testing.T) {
			spy := &roleSpy{}
			res, err := runScenario(t.Context(), scheme, s, spy)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Pass || !res.Escalated || res.ToolCalls == 0 {
				t.Fatalf("pass %v, escalated %v, tool calls %d: outcome %+v, diagnosis %+v, reads %+v", res.Pass,
					res.Escalated, res.ToolCalls, res.Outcome, res.Diagnosis, res.Reads)
			}
			for i, r := range spy.roles {
				if r != analysis.RoleEscalation {
					t.Errorf("call %d went to %s", i, r)
				}
			}
		})
	}
}

// The facts an escalation scenario scores from its reads cannot be reached from the context: for each fact
// behindReads names, the rubric's own terms do not score it against the first request the product builds (the
// system prompt and the context, every message of it), and do score it against the tool results of the
// recorded run. Terms are matched without case and on word boundaries (termIn), so a "256" scored for a CPU
// request is not taken from a "256Mi" of the system prompt, and a plain-text "cpu request 256" is.
func TestEscalationFactsOnlyBehindReads(t *testing.T) {
	scheme, all := mustEscalationScenarios(t)
	for _, s := range all {
		t.Run(s.name, func(t *testing.T) {
			res, err := runScenario(t.Context(), scheme, s, replaySource{})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.prompt) == 0 {
				t.Fatal("no model request")
			}
			var prompt strings.Builder
			for _, m := range res.prompt {
				prompt.WriteString(m.Content)
				prompt.WriteString("\n")
			}
			tools := strings.Join(res.toolText, "\n")
			for _, f := range s.behindReads {
				if factReached(f, prompt.String()) {
					t.Errorf("the fact %q is scored by the first request alone: triage could reach it without a read",
						f.Fact)
				}
				if !factReached(f, tools) {
					t.Errorf("the fact %q is scored by no tool result of the run: %q", f.Fact, res.toolText)
				}
			}
		})
	}
}

// factReached reports whether text holds the terms that score f, as scoreDiagnosis combines them (every term
// of All, one of Any, one group of AnyOf), each matched by termIn. Subject terms are left out: the Incident
// holds them.
func factReached(f rubricFact, text string) bool {
	in := func(term string) bool { return termIn(term, text) }
	if !slices.ContainsFunc(f.All, func(term string) bool { return !in(term) }) &&
		(len(f.Any) == 0 || slices.ContainsFunc(f.Any, in)) &&
		(len(f.AnyOf) == 0 || slices.ContainsFunc(f.AnyOf, func(g []string) bool {
			return !slices.ContainsFunc(g, func(term string) bool { return !in(term) })
		})) {
		return len(f.All) > 0 || len(f.Any) > 0 || len(f.AnyOf) > 0
	}
	return false
}

// withoutRead returns a copy of s whose replies make no call that matches r: such calls are taken out of their
// reply, and a reply left with no call and no content is dropped. It fails when no call matched.
func withoutRead(t *testing.T, s *scenario, r readRule) *scenario {
	t.Helper()
	c := *s
	c.recording.Replies = nil
	removed := 0
	for _, reply := range s.recording.Replies {
		kept := reply
		kept.ToolCalls = nil
		for _, tc := range reply.ToolCalls {
			// Accepted as the recording ran it: every recorded call of an expected read is one the tool takes.
			if r.matches(readCall{tool: tc.Name, args: tc.Arguments, accepted: true}) {
				removed++
				continue
			}
			kept.ToolCalls = append(kept.ToolCalls, tc)
		}
		if len(kept.ToolCalls) == 0 && kept.Content == "" {
			continue
		}
		c.recording.Replies = append(c.recording.Replies, kept)
	}
	if removed == 0 {
		t.Fatalf("no recorded call makes the expected read %q", r.text)
	}
	return &c
}

// Each escalation scenario fails when one of its expected reads is taken out of the replies (#504
// acceptance): the read is reported as missing and the run fails.
func TestEscalationScenarioFailsWithoutItsRead(t *testing.T) {
	scheme, all := mustEscalationScenarios(t)
	for _, s := range all {
		for _, r := range s.reads {
			t.Run(s.name+"/"+r.text, func(t *testing.T) {
				res, err := runScenario(t.Context(), scheme, withoutRead(t, s, r), replaySource{})
				if err != nil {
					t.Fatal(err)
				}
				if res.Pass || res.Reads.Pass {
					t.Fatalf("passed without the read: pass %v, reads %+v", res.Pass, res.Reads)
				}
				i := slices.IndexFunc(res.Reads.Expected, func(c readCheck) bool { return c.Read == r.text })
				if i < 0 || res.Reads.Expected[i].Pass {
					t.Errorf("the read %q is not reported missing: %+v", r.text, res.Reads.Expected)
				}
			})
		}
	}
}

// The loader refuses an escalation scenario that is not one, each with its own message: no reads, no facts
// behind reads or one the rubric does not score, no about, a variant, a synthetic one under a fleet scenario's
// name, a fleet one that holds its own copy of the fleet's files, a signal without the setting that raises it
// (or such a setting without its signal), and a logs.yaml it does not accept.
func TestEscalationScenarioRefused(t *testing.T) {
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	_, all := mustEscalationScenarios(t)
	var fleet, synthetic string
	for _, s := range all {
		n := strings.TrimPrefix(s.name, escalationPrefix)
		var meta escalationScenario
		if err := readStrict(filepath.Join(escalationDir, n, escalationFile), &meta); err != nil {
			t.Fatal(err)
		}
		if meta.FleetScenario != "" {
			fleet = n
		} else {
			synthetic = n
		}
	}
	if fleet == "" || synthetic == "" {
		t.Fatalf("need a fleet and a synthetic escalation scenario, have %q and %q", fleet, synthetic)
	}
	cases := []struct {
		name, scenario, want string
		change               func(t *testing.T, dir string)
	}{
		{"no reads", synthetic, "must list the reads it expects", func(t *testing.T, dir string) {
			rewrite(t, filepath.Join(dir, "rubric.yaml"), func(s string) string {
				i := strings.Index(s, "\nreads:")
				if i < 0 {
					t.Fatal("rubric.yaml has no reads")
				}
				return s[:i+1]
			})
		}},
		{"no facts behind reads", synthetic, "behindReads needs at least one fact", func(t *testing.T, dir string) {
			changeMeta(t, dir, func(m *escalationScenario) { m.BehindReads = nil })
		}},
		{"a fact not in the rubric", synthetic, "which is no scored fact of rubric.yaml", func(t *testing.T, dir string) {
			changeMeta(t, dir, func(m *escalationScenario) { m.BehindReads = append(m.BehindReads, "no such fact") })
		}},
		{"no about", synthetic, "about is required", func(t *testing.T, dir string) {
			changeMeta(t, dir, func(m *escalationScenario) { m.About = " " })
		}},
		{"a variant", synthetic, "has no variants", func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, variantsDir, "v"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"own copy of a fleet file", fleet, "comes from the fleet scenario", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "objects.yaml"), []byte("# copy\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"an unknown signal", synthetic, "is not truncated or failedSource", func(t *testing.T, dir string) {
			changeMeta(t, dir, func(m *escalationScenario) { m.Signal = "cut" })
		}},
		{"truncated without contextBytes", escCutContext, "contextBytes is required with signal truncated",
			func(t *testing.T, dir string) { changeMeta(t, dir, func(m *escalationScenario) { m.ContextBytes = 0 }) }},
		{"contextBytes without truncated", synthetic, "contextBytes is required with signal truncated",
			func(t *testing.T, dir string) {
				changeMeta(t, dir, func(m *escalationScenario) { m.ContextBytes = 8192 })
			}},
		{"contextBytes below the minimum", escCutContext, "contextBytes: context bound 1024 is outside",
			func(t *testing.T, dir string) {
				changeMeta(t, dir, func(m *escalationScenario) { m.ContextBytes = 1024 })
			}},
		{"an unknown metricsSource", synthetic, `metricsSource "flaky" is not down`, func(t *testing.T, dir string) {
			changeMeta(t, dir, func(m *escalationScenario) { m.MetricsSource = "flaky" })
		}},
		{"failedSource without a source down", escMetricsDown, "metricsSource down is required with signal failedSource",
			func(t *testing.T, dir string) {
				changeMeta(t, dir, func(m *escalationScenario) { m.MetricsSource = "" })
			}},
		{"a source down without failedSource", synthetic, "metricsSource down is required with signal failedSource",
			func(t *testing.T, dir string) {
				changeMeta(t, dir, func(m *escalationScenario) { m.MetricsSource = metricsDown })
			}},
		{"a source down with metrics.yaml", escMetricsDown, "exclude each other", func(t *testing.T, dir string) {
			raw, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(dir)), "scenarios", scnOOM, metricsFile))
			if err != nil {
				t.Fatal(err)
			}
			// #nosec G703 -- a fixture copied into the test's own temporary directory.
			if err := os.WriteFile(filepath.Join(dir, metricsFile), raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"own logs.yaml in a fleet one", fleet, "logs.yaml comes from the fleet scenario", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, logsFile), []byte("# copy\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"logs.yaml without a source", escCutContext, "logs.yaml: source is required", func(t *testing.T, dir string) {
			rewrite(t, filepath.Join(dir, logsFile), func(s string) string {
				return strings.Replace(s, "source: synthetic\n", "", 1)
			})
		}},
		// Only the name changes: a fleet scenario directory appears under the synthetic one's name, so the
		// scenario is otherwise the one that loads below.
		{"synthetic under a fleet name", synthetic, "may not take the fleet scenario name",
			func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(filepath.Dir(filepath.Dir(dir)), "scenarios", filepath.Base(dir)),
					0o700); err != nil {
					t.Fatal(err)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			copyTestdata(t, root)
			tc.change(t, filepath.Join(root, escalationDir, tc.scenario))
			t.Chdir(root)
			_, err := loadEscalationScenario(scheme, tc.scenario)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
	t.Run("unchanged copy loads", func(t *testing.T) {
		root := t.TempDir()
		copyTestdata(t, root)
		t.Chdir(root)
		for _, n := range []string{fleet, synthetic, escCutContext, escMetricsDown} {
			if _, err := loadEscalationScenario(scheme, n); err != nil {
				t.Errorf("%s: %v", n, err)
			}
		}
	})
}

// copyTestdata copies the scenarios and the escalation scenarios under root, at their paths.
func copyTestdata(t *testing.T, root string) {
	t.Helper()
	for _, d := range []string{scenariosDir, escalationDir} {
		if err := os.CopyFS(filepath.Join(root, d), os.DirFS(d)); err != nil {
			t.Fatal(err)
		}
	}
}

// changeMeta rewrites the scenario.yaml in dir with change applied to it.
func changeMeta(t *testing.T, dir string, change func(*escalationScenario)) {
	t.Helper()
	var m escalationScenario
	if err := readStrict(filepath.Join(dir, escalationFile), &m); err != nil {
		t.Fatal(err)
	}
	change(&m)
	raw, err := yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, escalationFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// rewrite replaces the content of path with change applied to it.
func rewrite(t *testing.T, path string, change func(string) string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(change(string(raw))), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A scenario with a signal escalates on the product's own signal, not a forced one: the context the builder
// returns is cut to its total bound (truncated) or names the failed metrics source (failedSource), every call
// goes to the escalation model, and without the cause (the default bounds, or no metrics source) the same
// scenario starts on triage.
func TestEscalationScenarioSignal(t *testing.T) {
	scheme, all := mustEscalationScenarios(t)
	signals := map[escalationSignal]bool{}
	for _, s := range all {
		if s.signal == "" {
			continue
		}
		signals[s.signal] = true
		t.Run(s.name, func(t *testing.T) {
			if s.forceEscalation {
				t.Fatal("a scenario with a signal is forced")
			}
			spy := &roleSpy{}
			res, err := runScenario(t.Context(), scheme, s, spy)
			if err != nil {
				t.Fatal(err)
			}
			switch s.signal {
			case signalTruncated:
				if !res.truncated || len(res.failedSources) > 0 {
					t.Errorf("truncated %v, failed sources %v: want only the total cut", res.truncated, res.failedSources)
				}
			case signalFailedSource:
				if res.truncated || !slices.Equal(res.failedSources, []incidentcontext.Section{incidentcontext.SectionMetrics}) {
					t.Errorf("truncated %v, failed sources %v: want only the metrics source failed", res.truncated,
						res.failedSources)
				}
			}
			if !res.Escalated || len(spy.roles) == 0 || slices.ContainsFunc(spy.roles, func(r analysis.Role) bool {
				return r != analysis.RoleEscalation
			}) {
				t.Errorf("escalated %v, roles %v", res.Escalated, spy.roles)
			}

			// Without the cause the signal does not fire, so the run starts on triage. The recorded replies are
			// escalation replies, so only the first role and the signal are checked.
			c := *s
			switch s.signal {
			case signalTruncated:
				c.limits = incidentcontext.Limits{}
			case signalFailedSource:
				c.metricsDown = false
			}
			spy = &roleSpy{}
			res, _ = runScenario(t.Context(), scheme, &c, spy)
			if res.truncated || len(res.failedSources) > 0 || len(spy.roles) == 0 ||
				spy.roles[0] != analysis.RoleTriage {
				t.Errorf("without the cause: truncated %v, failed sources %v, roles %v", res.truncated,
					res.failedSources, spy.roles)
			}
		})
	}
	for _, want := range []escalationSignal{signalTruncated, signalFailedSource} {
		if !signals[want] {
			t.Errorf("no escalation scenario relies on the signal %s", want)
		}
	}
}

// withFinalReply returns a copy of s whose last recorded reply has content instead.
func withFinalReply(s *scenario, content string) *scenario {
	c := *s
	c.recording.Replies = slices.Clone(s.recording.Replies)
	last := &c.recording.Replies[len(c.recording.Replies)-1]
	last.Content, last.ToolCalls = content, nil
	return &c
}

// memoryProposal is a final reply that keeps the diagnosis and proposes a memory limit within the scenario's
// policy, from previous to desired.
func memoryProposal(t *testing.T, s *scenario, previous, desired string) string {
	t.Helper()
	var r map[string]any
	if err := json.Unmarshal([]byte(s.recording.Replies[len(s.recording.Replies)-1].Content), &r); err != nil {
		t.Fatal(err)
	}
	r["missingData"] = []string{}
	r["proposal"] = map[string]any{
		"action": map[string]any{"type": "SetContainerMemoryLimit", "adjustKedaMaxReplicas": nil,
			"setContainerMemoryLimit": map[string]any{"container": oomWorkload, "previous": previous, "desired": desired}},
		"expectedEffect": "The container stays under its limit and stops being OOMKilled.", "verificationChecks": []any{},
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The plausible wrong first guess and the failed source fail a memory proposal: the loop proposes it (the
// policy allows it), and the scenario fails on its outcome, not on the policy or the diagnosis.
func TestEscalationMemoryProposalFails(t *testing.T) {
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, previous, desired string }{
		{escMetricsDown, "48Mi", "72Mi"},
		{escOOMConfigKey, "64Mi", "80Mi"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, err := loadEscalationScenario(scheme, c.name)
			if err != nil {
				t.Fatal(err)
			}
			res, err := runScenario(t.Context(), scheme, withFinalReply(s, memoryProposal(t, s, c.previous, c.desired)),
				replaySource{})
			if err != nil {
				t.Fatal(err)
			}
			if res.Reason != string(analysis.ReasonProposed) || res.Outcome.Pass || res.Pass {
				t.Errorf("reason %s, outcome %+v, pass %v: want a proposal that fails the outcome", res.Reason,
					res.Outcome, res.Pass)
			}
			if !res.Diagnosis.Pass || !res.Reads.Pass {
				t.Errorf("only the outcome should fail: diagnosis %+v, reads %+v", res.Diagnosis, res.Reads)
			}
		})
	}
}

// The failed source accepts a diagnosis without a proposal as well as the recorded hand-off for the missing
// metrics: both are refusals with the facts.
func TestEscalationFailedSourceAcceptsDiagnosisAndHandOff(t *testing.T) {
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	s, err := loadEscalationScenario(scheme, escMetricsDown)
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	if err := json.Unmarshal([]byte(s.recording.Replies[len(s.recording.Replies)-1].Content), &r); err != nil {
		t.Fatal(err)
	}
	r["missingData"] = []string{}
	diagnosis, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		s      *scenario
		reason analysis.Reason
	}{
		{"hand-off", s, analysis.ReasonMissingData},
		{"diagnosis", withFinalReply(s, string(diagnosis)), analysis.ReasonNoProposal},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := runScenario(t.Context(), scheme, c.s, replaySource{})
			if err != nil {
				t.Fatal(err)
			}
			if res.Reason != string(c.reason) || !res.Pass {
				t.Errorf("reason %s, pass %v (outcome %+v, diagnosis %+v)", res.Reason, res.Pass, res.Outcome,
					res.Diagnosis)
			}
		})
	}
}
