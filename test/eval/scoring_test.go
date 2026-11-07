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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/tools"
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

// The digest is the hash of the stored manifest without its digest field, as docs/evaluation.md says.
func TestManifestDigestIsOfTheManifestWithoutIt(t *testing.T) {
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
	m, err := buildManifest(src, replaySource{}, defs, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(&report{Manifest: m})
	if err != nil {
		t.Fatal(err)
	}
	var stored report
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	digest := stored.Manifest.Digest
	stored.Manifest.Digest = ""
	body, err := json.Marshal(stored.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "digest") {
		t.Errorf("the hashed JSON still has the digest field: %s", body)
	}
	if got, err := hashJSON(stored.Manifest); err != nil || got != digest {
		t.Errorf("recomputed %s (%v), stored %s", got, err, digest)
	}
}

// The run limits and each scenario's incident, objects and replies are inputs of the digest, not only the
// rubric files.
func TestManifestDigestCoversRunLimitsAndScenarioFiles(t *testing.T) {
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
	if base.RunLimits.MaxToolSteps != evalMaxToolSteps || base.RunLimits.MaxOutputTokens != evalMaxOutputTokens {
		t.Errorf("run limits %+v", base.RunLimits)
	}
	for name, change := range map[string]func(m *manifest){
		"max tool steps":    func(m *manifest) { m.RunLimits.MaxToolSteps++ },
		"max output tokens": func(m *manifest) { m.RunLimits.MaxOutputTokens++ },
	} {
		m := base
		change(&m)
		if d, err := manifestDigest(m); err != nil || d == base.Digest {
			t.Errorf("%s: digest unchanged (%v)", name, err)
		}
	}
	for _, file := range []string{"incident.yaml", "objects.yaml", metricsFile, "replies.yaml",
		filepath.Join(variantsDir, "rollout", "objects.yaml"), filepath.Join(variantsDir, "rollout", "variant.yaml")} {
		dir := t.TempDir()
		if err := os.CopyFS(dir, os.DirFS(scenariosDir)); err != nil {
			t.Fatal(err)
		}
		same, err := buildManifestIn(dir, src, replaySource{}, defs, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(filepath.Join(dir, scnOOM, file), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("# changed\n"); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		m, err := buildManifestIn(dir, src, replaySource{}, defs, 1, 1)
		if err != nil || m.Digest == same.Digest {
			t.Errorf("%s: digest unchanged (%v)", file, err)
		}
	}
}

// NaN and infinities are not thresholds or prices.
func TestEnvFloatRejectsNaNAndInfinity(t *testing.T) {
	for _, v := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "infinity"} {
		t.Setenv("EVAL_TEST_FLOAT", v)
		if f, err := envFloat("EVAL_TEST_FLOAT", 1, -1e9, 1e9); err == nil {
			t.Errorf("%q accepted as %v", v, f)
		}
	}
	t.Setenv("EVAL_TEST_FLOAT", "0.5")
	if f, err := envFloat("EVAL_TEST_FLOAT", 1, 0, 1); err != nil || f != 0.5 {
		t.Errorf("0.5: %v, %v", f, err)
	}
}

// A recorded reply the analysis never used fails the scenario.
func TestLeftoverReplyFails(t *testing.T) {
	scheme, s := mustScenario(t, scnBadImage)
	s.recording.Replies = append(s.recording.Replies, s.recording.Replies[len(s.recording.Replies)-1])
	_, err := runScenario(t.Context(), scheme, s, replaySource{})
	if err == nil || !strings.Contains(err.Error(), "replies.yaml records") {
		t.Fatalf("a leftover reply was accepted: %v", err)
	}
}

// spySource records the tools of every request the loop sends.
type spySource struct {
	replaySource
	mu    sync.Mutex
	tools [][]providers.ToolDefinition
}

type spyModel struct {
	inner providers.ModelProvider
	src   *spySource
}

func (m spyModel) Complete(ctx context.Context, req providers.CompletionRequest) (providers.Completion, error) {
	m.src.mu.Lock()
	m.src.tools = append(m.src.tools, req.Tools)
	m.src.mu.Unlock()
	return m.inner.Complete(ctx, req)
}

func (s *spySource) factory(sc *scenario) (analysis.ModelFactory, func() error, error) {
	f, done, err := s.replaySource.factory(sc)
	if err != nil {
		return nil, nil, err
	}
	return func(r analysis.Role, d remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
		p, err := f(r, d)
		return spyModel{inner: p, src: s}, err
	}, done, err
}

// The manifest hashes the tools the loop really offers on escalation, which leave out get_delivery_status.
func TestManifestToolsAreTheOnesTheLoopOffers(t *testing.T) {
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	defs, err := toolDefinitions(scheme)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range defs {
		if d.Name == tools.NameGetDeliveryStatus {
			t.Fatalf("%s is in the manifest's tools", d.Name)
		}
	}
	// Two invalid triage replies escalate; the escalation request carries the tools the loop offers.
	_, sc := mustScenario(t, scnOOM)
	bad := recordedReply{Model: fakeModelName, Content: "not json"}
	sc.recording.Replies = append([]recordedReply{bad, bad}, sc.recording.Replies...)
	spy := &spySource{}
	res, err := runScenario(t.Context(), scheme, sc, spy)
	if err != nil || !res.Escalated {
		t.Fatalf("the scenario did not escalate: %+v, %v", res, err)
	}
	want, _ := hashJSON(defs)
	seen := false
	for _, offered := range spy.tools {
		if len(offered) == 0 {
			continue
		}
		seen = true
		if got, _ := hashJSON(offered); got != want {
			t.Errorf("the loop offered %d tools, the manifest hashes %d", len(offered), len(defs))
		}
	}
	if !seen {
		t.Fatal("no request carried tools: the check proved nothing")
	}
}

// A memory plan must name a container of the scenario's workload and the limit it holds.
func TestMemoryPlanMustFitTheWorkload(t *testing.T) {
	cases := map[string]struct {
		change func(p *remediationv1alpha1.SetContainerMemoryLimit, e *expected)
		detail string
	}{
		"another container": {func(p *remediationv1alpha1.SetContainerMemoryLimit, _ *expected) { p.Container = "sidecar" },
			`no container "sidecar"`},
		"wrong previous limit": {func(p *remediationv1alpha1.SetContainerMemoryLimit, _ *expected) {
			p.Previous = resource.MustParse("32Mi")
		}, "not the container's current limit"},
		"expected container": {func(_ *remediationv1alpha1.SetContainerMemoryLimit, e *expected) {
			e.Outcome.Container = "db"
		}, `container "db" was expected`},
		"expected previous": {func(_ *remediationv1alpha1.SetContainerMemoryLimit, e *expected) {
			e.Outcome.Previous = "96Mi"
		}, "previous 96Mi was expected"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, s := mustScenario(t, scnOOM)
			plan := &analysis.Plan{Action: remediationv1alpha1.Action{Type: remediationv1alpha1.ActionSetContainerMemoryLimit,
				SetContainerMemoryLimit: &remediationv1alpha1.SetContainerMemoryLimit{Container: "web",
					Previous: resource.MustParse("48Mi"), Desired: resource.MustParse("64Mi")}}}
			out := analysis.Outcome{Reason: analysis.ReasonProposed, Plan: plan}
			if ok, d := scoreProposal(s, out); !ok {
				t.Fatalf("the matching plan failed: %s", d)
			}
			c.change(plan.Action.SetContainerMemoryLimit, &s.expected)
			if ok, d := scoreProposal(s, out); ok || !strings.Contains(d, c.detail) {
				t.Errorf("pass %t, detail %q: want a failure saying %q", ok, d, c.detail)
			}
		})
	}
}

// objects.yaml is split like every other manifest file of the project: any document separator line.
func TestObjectsSplitOnAnySeparator(t *testing.T) {
	scheme, s := mustScenario(t, scnOOM)
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join(scenariosDir, scnOOM))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "objects.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.ReplaceAll(string(raw), "\n---\n", "\n--- # next object\n")
	if changed == string(raw) {
		t.Fatal("objects.yaml has no separator to change")
	}
	// #nosec G703 -- a file of the test's own temp directory
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadScenarioDir(scheme, dir, scnOOM)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.objects) != len(s.objects) {
		t.Errorf("%d objects with the changed separators, %d with the original", len(got.objects), len(s.objects))
	}
}

// A variant is scored against its scenario's expected.yaml, rubric and Incident, with its own objects and
// replies, and says what it changes.
func TestVariantsShareTheScenarioRubric(t *testing.T) {
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	all, err := loadScenarios(scheme)
	if err != nil {
		t.Fatal(err)
	}
	var base, rollout *scenario
	for _, s := range all {
		switch s.name {
		case scnOOM:
			base = s
		case scnOOM + "/rollout":
			rollout = s
		}
	}
	if base == nil || rollout == nil {
		t.Fatal("the oom scenario or its rollout variant was not loaded")
	}
	if rollout.expected.Name != scnOOM || len(rollout.rubric.Facts) != len(base.rubric.Facts) ||
		rollout.incident.UID != base.incident.UID || len(rollout.objects) == len(base.objects) {
		t.Errorf("the variant does not share the scenario's rubric and Incident with its own objects")
	}
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join(scenariosDir, scnOOM))); err != nil {
		t.Fatal(err)
	}
	// #nosec G703 -- a file of the test's own temp directory
	if err := os.WriteFile(filepath.Join(dir, variantsDir, "rollout", "variant.yaml"), []byte("about: \"\"\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadVariants(scheme, dir, base); err == nil {
		t.Error("a variant that does not say what it changes was loaded")
	}
}

// A memory plan for a container whose template sets no memory limit fails: the loop takes the previous value
// from the template (#494), so no previous value fits it.
func TestMemoryPlanForAContainerWithoutALimitFails(t *testing.T) {
	_, s := mustScenario(t, scnOOM)
	for _, o := range s.objects {
		if d, ok := o.(*appsv1.Deployment); ok && d.Name == "web" {
			d.Spec.Template.Spec.Containers[0].Resources.Limits = nil
		}
	}
	plan := &analysis.Plan{Action: remediationv1alpha1.Action{Type: remediationv1alpha1.ActionSetContainerMemoryLimit,
		SetContainerMemoryLimit: &remediationv1alpha1.SetContainerMemoryLimit{Container: "web",
			Previous: resource.MustParse("48Mi"), Desired: resource.MustParse("64Mi")}}}
	ok, d := scoreProposal(s, analysis.Outcome{Reason: analysis.ReasonProposed, Plan: plan})
	if ok || !strings.Contains(d, "sets no memory limit") {
		t.Errorf("pass %t, detail %q: want a failure for a container without a limit", ok, d)
	}
}
