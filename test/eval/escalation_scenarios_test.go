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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/pkalin/shadewright/internal/analysis"
)

// The escalation scenarios (#504): recorded cases whose diagnosis rests on a fact only a read tool shows, so
// triage alone cannot reach it. Each runs on the escalation model with the read tools from its first call,
// whatever the model source, through the same failed-source signal as EVAL_FORCE_ESCALATION (forcedEscalation):
// no deterministic signal of the context fires for them, and they measure what the escalation model reads.

// escalationDir holds one directory per escalation scenario.
const escalationDir = "testdata/escalation"

// escalationPrefix starts the report name of an escalation scenario: escalation/<directory>.
const escalationPrefix = "escalation/"

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
}

// dataFiles are the files an escalation scenario of a fleet scenario takes from it, and may not hold itself.
var dataFiles = []string{"expected.yaml", "incident.yaml", "objects.yaml", metricsFile, policyFile}

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
	s.forceEscalation = true
	return s, nil
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

// termIn reports whether term is in text, without case, with no letter or digit glued to an end of the term
// that is itself a letter or digit: "256" is in "requests 256 CPUs" and in `"cpu": "256"`, not in "256Mi".
func termIn(term, text string) bool {
	word := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	t, l := []rune(strings.ToLower(term)), []rune(strings.ToLower(text))
	if len(t) == 0 {
		return false
	}
	for i := 0; i+len(t) <= len(l); i++ {
		if !slices.Equal(l[i:i+len(t)], t) {
			continue
		}
		if word(t[0]) && i > 0 && word(l[i-1]) {
			continue
		}
		if end := i + len(t); word(t[len(t)-1]) && end < len(l) && word(l[end]) {
			continue
		}
		return true
	}
	return false
}

// termIn matches a term on word boundaries without case, so a value glued to a unit or another word is not taken
// for it.
func TestTermIn(t *testing.T) {
	const path = "/healthz"
	for _, c := range []struct {
		term, text string
		want       bool
	}{
		{"256", "the web container requests 256 CPUs", true},
		{"256", `"cpu": "256"`, true},
		{"256", "cpu request 256", true},
		{"256", "a limit of 256Mi", false},
		{"256", "sha256:00", false},
		{"256", "2560 bytes", false},
		{path, "GET /healthz returned 404", true},
		{path, `"path": "/healthz"`, true},
		{path, "/healthzcheck", false},
		{"Insufficient cpu", "3 insufficient CPU.", true},
		{"", "anything", false},
	} {
		if got := termIn(c.term, c.text); got != c.want {
			t.Errorf("termIn(%q, %q) = %v, want %v", c.term, c.text, got, c.want)
		}
	}
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
// name, and a fleet one that holds its own copy of the fleet's files.
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
		for _, n := range []string{fleet, synthetic} {
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
