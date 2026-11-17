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
	// BehindReads are values the diagnosis names that only the expected reads show: never in the context the
	// builder makes, always in a tool result of the recorded run.
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
	if len(meta.BehindReads) == 0 || slices.ContainsFunc(meta.BehindReads, func(v string) bool {
		return strings.TrimSpace(v) == ""
	}) {
		return nil, fmt.Errorf("%s: behindReads needs at least one value, none empty", escalationFile)
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
	s.forceEscalation, s.behindReads = true, meta.BehindReads
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

// The facts an escalation scenario scores from its reads cannot be reached from the context: each value of
// behindReads is absent from the first request (the system prompt and the context the builder made) and
// present in a tool result of the recorded run.
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
			}
			for _, v := range s.behindReads {
				if strings.Contains(prompt.String(), v) {
					t.Errorf("%q is in the context: triage could reach it without a read", v)
				}
				if !slices.ContainsFunc(res.toolText, func(text string) bool { return strings.Contains(text, v) }) {
					t.Errorf("%q is in no tool result of the run: %q", v, res.toolText)
				}
			}
		})
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

// The loader refuses an escalation scenario that is not one: no reads, no values behind reads, no about, a
// synthetic one under a fleet scenario's name, and a fleet one that holds its own copy of the fleet's files.
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
		name, scenario string
		change         func(t *testing.T, dir string)
	}{
		{"no reads", synthetic, func(t *testing.T, dir string) {
			rewrite(t, filepath.Join(dir, "rubric.yaml"), func(s string) string {
				i := strings.Index(s, "\nreads:")
				if i < 0 {
					t.Fatal("rubric.yaml has no reads")
				}
				return s[:i+1]
			})
		}},
		{"no values behind reads", synthetic, func(t *testing.T, dir string) {
			changeMeta(t, dir, func(m *escalationScenario) { m.BehindReads = nil })
		}},
		{"an empty value behind reads", synthetic, func(t *testing.T, dir string) {
			changeMeta(t, dir, func(m *escalationScenario) { m.BehindReads = append(m.BehindReads, " ") })
		}},
		{"no about", synthetic, func(t *testing.T, dir string) {
			changeMeta(t, dir, func(m *escalationScenario) { m.About = " " })
		}},
		{"a variant", synthetic, func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, variantsDir, "v"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"own copy of a fleet file", fleet, func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "objects.yaml"), []byte("# copy\n"), 0o600); err != nil {
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
			if _, err := loadEscalationScenario(scheme, tc.scenario); err == nil {
				t.Error("loaded")
			}
		})
	}
	t.Run("synthetic under a fleet name", func(t *testing.T) {
		root := t.TempDir()
		copyTestdata(t, root)
		if err := os.Rename(filepath.Join(root, escalationDir, synthetic),
			filepath.Join(root, escalationDir, scnOOM)); err != nil {
			t.Fatal(err)
		}
		t.Chdir(root)
		if _, err := loadEscalationScenario(scheme, scnOOM); err == nil {
			t.Error("loaded")
		}
	})
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
