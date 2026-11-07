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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Scenarios the scoring tests change.
const (
	scnOOM      = "oom"
	scnBadImage = "bad-image"
)

func mustScenario(t *testing.T, name string) (*runtime.Scheme, *scenario) {
	t.Helper()
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	s, err := loadScenario(scheme, name)
	if err != nil {
		t.Fatal(err)
	}
	return scheme, s
}

func run(t *testing.T, scheme *runtime.Scheme, s *scenario) scenarioResult {
	t.Helper()
	res, err := runScenario(t.Context(), scheme, s, replaySource{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A scenario whose expectation does not match what the loop concluded is a failure, for every way the
// outcome can differ.
func TestMismatchedOutcomeFails(t *testing.T) {
	cases := map[string]struct {
		scenario string
		change   func(e *expected)
		detail   string
	}{
		"pull request expected, refusal given": {scnBadImage, func(e *expected) {
			e.Outcome.Type, e.Outcome.Action = outcomePullRequest, "SetContainerMemoryLimit"
		}, "without one"},
		"refusal expected, proposal given": {scnOOM, func(e *expected) { e.Outcome.Type = outcomeRefusal },
			"the loop proposed SetContainerMemoryLimit"},
		"another action type": {scnOOM, func(e *expected) { e.Outcome.Action = "AdjustKedaMaxReplicas" },
			"the loop proposed SetContainerMemoryLimit"},
		"memory above the desired value": {scnOOM, func(e *expected) {
			e.Outcome.Bounds.Min, e.Outcome.Bounds.Max = intstr.FromString("128Mi"), intstr.FromString("256Mi")
		}, "outside"},
		"replicas below the desired value": {"keda-at-ceiling", func(e *expected) {
			e.Outcome.Bounds.Min, e.Outcome.Bounds.Max = intstr.FromInt32(4), intstr.FromInt32(5)
		}, "outside"},
		"unknown outcome type": {scnBadImage, func(e *expected) { e.Outcome.Type = "rollback" }, "unknown outcome type"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			scheme, s := mustScenario(t, c.scenario)
			if res := run(t, scheme, s); !res.Pass {
				t.Fatalf("the unchanged scenario failed: %+v", res.Outcome)
			}
			c.change(&s.expected)
			res := run(t, scheme, s)
			if res.Pass || res.Outcome.Pass || !strings.Contains(res.Outcome.Detail, c.detail) {
				t.Errorf("pass %t, outcome %+v: want a failure saying %q", res.Pass, res.Outcome, c.detail)
			}
		})
	}
}

// A refusal for a reason the model's result did not give (here: a reply from another model than the one
// configured) is not the expected refusal, and leaves no diagnosis to score.
func TestRefusalWithoutAResultFails(t *testing.T) {
	scheme, s := mustScenario(t, scnBadImage)
	s.recording.Replies[0].Model = "another-model-1"
	res := run(t, scheme, s)
	if res.Pass || res.Outcome.Pass || res.Reason != "UnexpectedModel" || res.Diagnosis.Pass {
		t.Errorf("pass %t, outcome %+v, diagnosis %+v", res.Pass, res.Outcome, res.Diagnosis)
	}
}

// A diagnosis that misses a scored fact fails, and the report names the missing term.
func TestMismatchedDiagnosisFails(t *testing.T) {
	scheme, s := mustScenario(t, scnBadImage)
	s.rubric.Facts[0].All = []string{"podinfo:9.9.9"}
	res := run(t, scheme, s)
	if res.Pass || res.Diagnosis.Pass || !res.Outcome.Pass {
		t.Fatalf("pass %t, diagnosis %+v", res.Pass, res.Diagnosis)
	}
	if f := res.Diagnosis.Facts[0]; f.Pass || len(f.Missing) != 1 || f.Missing[0] != "podinfo:9.9.9" {
		t.Errorf("fact %+v", f)
	}

	_, s = mustScenario(t, scnBadImage)
	s.rubric.Facts[1].Any = []string{"unauthorized", "denied"}
	if res := run(t, scheme, s); res.Diagnosis.Pass {
		t.Errorf("no term of any matched and the diagnosis passed: %+v", res.Diagnosis)
	}
}

// The replies of another scenario fail this one: the scoring reads what the loop concluded, not the files.
func TestWrongRecordingFails(t *testing.T) {
	scheme, s := mustScenario(t, scnOOM)
	_, other := mustScenario(t, scnBadImage)
	s.recording = other.recording
	if res := run(t, scheme, s); res.Pass || res.Outcome.Pass {
		t.Errorf("oom passed with bad-image's replies: %+v", res.Outcome)
	}
}

// The rubric must name every fact of expected.yaml once, verbatim, scored or with a reason, so a change of
// the facts in the fleet repository cannot pass unscored.
func TestRubricMustMatchExpected(t *testing.T) {
	_, s := mustScenario(t, scnOOM)
	cases := map[string]func(r *rubric){
		"fact missing":   func(r *rubric) { r.Facts = r.Facts[1:] },
		"fact reworded":  func(r *rubric) { r.Facts[0].Fact += " (changed)" },
		"fact twice":     func(r *rubric) { r.Facts = append(r.Facts, r.Facts[0]) },
		"no terms":       func(r *rubric) { r.Facts[0].All = nil },
		"terms and why":  func(r *rubric) { r.Facts[0].Unscored = "because" },
		"an empty term":  func(r *rubric) { r.Facts[0].All = []string{" "} },
		"no fact at all": func(r *rubric) { r.Facts = nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c := *s
			c.rubric = rubric{Facts: append([]rubricFact(nil), s.rubric.Facts...)}
			for i := range c.rubric.Facts {
				c.rubric.Facts[i].All = append([]string(nil), c.rubric.Facts[i].All...)
			}
			change(&c.rubric)
			if err := c.checkRubric(); err == nil {
				t.Error("accepted")
			}
		})
	}
	if err := s.checkRubric(); err != nil {
		t.Errorf("the checked-in rubric: %v", err)
	}
}

// A recorded Incident that is not the one expected.yaml describes is refused.
func TestIncidentMustMatchExpected(t *testing.T) {
	_, s := mustScenario(t, scnOOM)
	s.incident.Spec.Reason = "BackOff"
	if err := s.checkIncident(); err == nil {
		t.Error("another reason was accepted")
	}
	_, s = mustScenario(t, scnOOM)
	s.incident.Spec.Subject.Name = "worker"
	if err := s.checkIncident(); err == nil {
		t.Error("another subject was accepted")
	}
}

// One failing run fails the report against a threshold of 1, the Markdown says so, and the files are
// written.
func TestReportTotals(t *testing.T) {
	r := &report{Mode: "fake", Manifest: manifest{MinPassRate: 1, Repeats: 1, Rubric: manifestRubric{FleetCommit: "abc"}},
		Scenarios: []scenarioResult{
			{Name: "a", Pass: true, Tokens: tokens{Input: 1000, CachedInput: 400, Output: 100, Charged: 1100}},
			{Name: "b", Pass: false, Error: "x | y", Tokens: tokens{Input: 1000, Output: 100, Charged: 1100}},
		}}
	r.summarise(&prices{Input: 2, CachedInput: 1, Output: 8})
	if r.Totals.Pass || r.Totals.Passed != 1 || r.Totals.PassRate != 0.5 || r.Totals.Tokens.Charged != 2200 {
		t.Errorf("totals %+v", r.Totals)
	}
	// (1600 * 2 + 400 * 1 + 200 * 8) / 1e6
	if r.Totals.CostUSD == nil || *r.Totals.CostUSD != 0.0052 {
		t.Errorf("cost %v", r.Totals.CostUSD)
	}
	md := r.markdown()
	if !strings.HasPrefix(md, "# Evaluation: FAIL") || !strings.Contains(md, `error: x \| y`) {
		t.Errorf("markdown:\n%s", md)
	}
	r.Manifest.MinPassRate = 0.5
	r.summarise(nil)
	if !r.Totals.Pass || r.Totals.CostUSD != nil {
		t.Errorf("at threshold 0.5: %+v", r.Totals)
	}
	dir := t.TempDir()
	if err := r.write(dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"report.json", "report.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Error(err)
		}
	}
}

// A change to any input of the manifest changes its digest (ADR-0025 decision 3).
func TestManifestDigestCoversItsFields(t *testing.T) {
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	defs, err := toolDefinitions(scheme)
	if err != nil {
		t.Fatal(err)
	}
	src, err := loadSource()
	if err != nil {
		t.Fatal(err)
	}
	base, err := buildManifest(src, replaySource{}, defs, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	again, err := buildManifest(src, replaySource{}, defs, 1, 1)
	if err != nil || again.Digest != base.Digest {
		t.Fatalf("the digest is not stable: %s, %s, %v", base.Digest, again.Digest, err)
	}
	other := src
	other.FleetCommit = strings.Repeat("0", 40)
	for name, build := range map[string]func() (manifest, error){
		"threshold": func() (manifest, error) { return buildManifest(src, replaySource{}, defs, 0.9, 1) },
		"repeats":   func() (manifest, error) { return buildManifest(src, replaySource{}, defs, 1, 2) },
		"tools":     func() (manifest, error) { return buildManifest(src, replaySource{}, defs[1:], 1, 1) },
		"rubric":    func() (manifest, error) { return buildManifest(other, replaySource{}, defs, 1, 1) },
	} {
		m, err := build()
		if err != nil || m.Digest == base.Digest {
			t.Errorf("%s: digest unchanged (%v)", name, err)
		}
	}
	for name, v := range map[string]string{"prompt": base.PromptVersion, "tools": base.ToolDefinitions,
		"schema": base.OutputSchema, "context builder": base.ContextBuilder.Source, "redaction": base.Redaction.BuiltInRules,
		"rubric files": base.Rubric.Files} {
		if !strings.HasPrefix(v, "sha256:") {
			t.Errorf("%s: %q", name, v)
		}
	}
	if base.Models["triage"].Model != fakeModelName || base.Models["escalation"].Deployment != "eval-escalation" {
		t.Errorf("models %+v", base.Models)
	}
}
