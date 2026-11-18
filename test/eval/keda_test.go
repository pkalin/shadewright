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

	"github.com/pkalin/shadewright/internal/analysis"
)

const (
	scnBrokenKeda  = "broken-keda-trigger"
	scnKedaCeiling = "keda-at-ceiling"
	// kedaCatalogLine is how the system prompt's catalog lists AdjustKedaMaxReplicas.
	kedaCatalogLine = "- AdjustKedaMaxReplicas: "
)

// runKeda runs a KEDA scenario with the recorded replies, the first one's content replaced when content is
// not empty, and returns the result and the first request's system and user messages.
func runKeda(t *testing.T, name, content string) (scenarioResult, string, string) {
	t.Helper()
	scheme, s := mustScenario(t, name)
	if content != "" {
		s.recording.Replies[0].Content = content
	}
	spy := &spySource{}
	res, err := runScenario(t.Context(), scheme, s, spy)
	if err != nil {
		t.Fatal(err)
	}
	if len(spy.messages) == 0 || len(spy.messages[0]) < 2 {
		t.Fatal("no request was sent")
	}
	return res, spy.messages[0][0].Content, spy.messages[0][1].Content
}

// broken-keda-trigger: the recorded ScaledObject is not ready (Ready False, TriggerError), so the model is shown
// its conditions and replica bounds but not AdjustKedaMaxReplicas, and the analysis ends ScalerNotReady from
// code, whether the reply proposes nothing or proposes the action anyway (ADR-0036 decision 5).
func TestBrokenKedaTriggerEndsScalerNotReady(t *testing.T) {
	res, system, user := runKeda(t, scnBrokenKeda, "")
	if res.Reason != string(analysis.ReasonScalerNotReady) || !res.Pass {
		t.Fatalf("reason %s, pass %t: %+v", res.Reason, res.Pass, res.Outcome)
	}
	if strings.Contains(system, kedaCatalogLine) {
		t.Error("the catalog offers AdjustKedaMaxReplicas for a ScaledObject that is not ready")
	}
	for _, want := range []string{"spec: minReplicaCount 1, maxReplicaCount 3",
		"condition Ready=False, reason TriggerError: Triggers defined in ScaledObject are not working correctly",
		"a change of its maxReplicaCount is not offered in this analysis"} {
		if !strings.Contains(user, want) {
			t.Errorf("the context lacks %q", want)
		}
	}
	if strings.Contains(user, "http_requests_total") {
		t.Error("the context shows the trigger's query")
	}

	anyway := `{"summary": "The HPA cannot scale on its trigger.", "hypotheses": [],
	  "evidence": [{"source": "c1", "quote": "lookup prometheus-server.monitoring.svc.cluster.local"}],
	  "missingData": [], "uncertainty": "",
	  "proposal": {"action": {"type": "AdjustKedaMaxReplicas", "setContainerMemoryLimit": null,
	               "adjustKedaMaxReplicas": {"previousValue": 3, "desiredValue": 4}},
	               "expectedEffect": "more replicas", "verificationChecks": []}}`
	if res, _, _ := runKeda(t, scnBrokenKeda, anyway); res.Reason != string(analysis.ReasonScalerNotReady) {
		t.Errorf("a proposal of the withheld action ended %s", res.Reason)
	}
}

// keda-at-ceiling: the recorded ScaledObject is ready with maxReplicaCount 2, which the context shows; the
// action is offered, and the plan carries the ScaledObject's value as its previous one, whatever the model
// wrote, so a model that misses the value cannot end MissingData or make a stale plan for that reason.
func TestKedaAtCeilingTakesThePreviousValueFromTheScaledObject(t *testing.T) {
	res, system, user := runKeda(t, scnKedaCeiling, "")
	if res.Reason != string(analysis.ReasonProposed) || !res.Pass {
		t.Fatalf("reason %s, pass %t: %+v", res.Reason, res.Pass, res.Outcome)
	}
	shown := strings.Contains(user, "spec: minReplicaCount 1, maxReplicaCount 2")
	if !strings.Contains(system, kedaCatalogLine) || !shown || strings.Contains(user, "not offered in this analysis") {
		t.Errorf("system:\n%s\nuser:\n%s", system, user)
	}

	// A stale previous value in the same direction is replaced by the ScaledObject's; scoring checks the plan
	// against the recorded object.
	_, s := mustScenario(t, scnKedaCeiling)
	stale := strings.Replace(s.recording.Replies[0].Content, `"previousValue": 2`, `"previousValue": 1`, 1)
	if stale == s.recording.Replies[0].Content {
		t.Fatal("the recorded reply does not say previousValue 2: the test proves nothing")
	}
	if res, _, _ := runKeda(t, scnKedaCeiling, stale); res.Reason != string(analysis.ReasonProposed) || !res.Outcome.Pass {
		t.Errorf("a stale previous value: reason %s, outcome %+v", res.Reason, res.Outcome)
	}
	// A desired value the ScaledObject already holds is no change.
	same := strings.Replace(s.recording.Replies[0].Content, `"desiredValue": 3`, `"desiredValue": 2`, 1)
	same = strings.Replace(same, `"previousValue": 2`, `"previousValue": 1`, 1)
	if res, _, _ := runKeda(t, scnKedaCeiling, same); res.Reason != string(analysis.ReasonNoChange) {
		t.Errorf("the current value as desired: reason %s", res.Reason)
	}
}

// mustVariant loads the variant name (<scenario>/<variant>) of the recorded scenarios.
func mustVariant(t *testing.T, name string) (*runtime.Scheme, *scenario) {
	t.Helper()
	base, v, _ := strings.Cut(name, "/")
	scheme, s := mustScenario(t, base)
	variants, err := loadVariants(scheme, filepath.Join(scenariosDir, base), s)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range variants {
		if x.name == base+"/"+v {
			return scheme, x
		}
	}
	t.Fatalf("no variant %s", name)
	return nil, nil
}

// keda-at-ceiling/wide-policy: the policy allows up to 6 replicas from 2, the evidence supports 3 (the rubric's
// size). A value at or below the supported size passes (3 of 3, 3 and 4 of 5); 4 and 5 fail against a supported
// size of 3 even though the policy allows them; 6 passes when the rubric supports it or more (#505, decided
// 2026-11-17). The recorded object stands at 2, so a proposal of 2 is no change and cannot be scored here.
func TestKedaProposalAtThePolicyMaximumFails(t *testing.T) {
	const name = scnKedaCeiling + "/wide-policy"
	scheme, s := mustVariant(t, name)
	if res := run(t, scheme, s); !res.Pass || res.Reason != string(analysis.ReasonProposed) {
		t.Fatalf("the recorded reply at the supported size: pass %t, reason %s, outcome %+v", res.Pass, res.Reason,
			res.Outcome)
	}
	at := func(desired string, supported int32) scenarioResult {
		t.Helper()
		scheme, s := mustVariant(t, name)
		s.expected.Outcome.Bounds = nil
		s.rubric.Size = &rubricSize{Supported: supported, Why: "the load needs it"}
		content := strings.Replace(s.recording.Replies[0].Content, `"desiredValue": 3`, `"desiredValue": `+desired, 1)
		if content == s.recording.Replies[0].Content && desired != "3" {
			t.Fatal("the recorded reply does not say desiredValue 3: the test proves nothing")
		}
		s.recording.Replies[0].Content = content
		return run(t, scheme, s)
	}
	res := at("6", 3)
	if res.Pass || res.Outcome.Pass || res.Reason != string(analysis.ReasonProposed) ||
		!strings.Contains(res.Outcome.Detail, "desired 6 exceeds the supported size 3") {
		t.Errorf("a proposal at the policy maximum: pass %t, reason %s, outcome %+v", res.Pass, res.Reason, res.Outcome)
	}
	if !res.Diagnosis.Pass || !res.Reads.Pass {
		t.Errorf("only the size fails: diagnosis %+v, reads %+v", res.Diagnosis, res.Reads)
	}
	for _, c := range []struct {
		desired   string
		supported int32
	}{{"3", 3}, {"3", 5}, {"4", 5}, {"4", 3}, {"5", 3}, {"6", 6}, {"6", 8}} {
		res := at(c.desired, c.supported)
		if c.supported == 3 && (c.desired == "4" || c.desired == "5") {
			if res.Outcome.Pass {
				t.Errorf("desired %s above supported 3 passed: %+v", c.desired, res.Outcome)
			}
		} else if !res.Outcome.Pass {
			t.Errorf("desired %s, supported %d: outcome %+v", c.desired, c.supported, res.Outcome)
		}
	}
}

// A rubric size applies only to a proposal of an action whose number the model chooses, and needs a value in
// the action's range and a reason.
func TestRubricSizeIsChecked(t *testing.T) {
	_, oom := mustScenario(t, scnOOM) // a SetContainerMemoryLimit proposal: code sizes it (ADR-0040)
	oom.rubric.Size = &rubricSize{Supported: 3, Why: "x"}
	if err := oom.checkRubric(); err == nil {
		t.Error("a size on a memory proposal was accepted")
	}
	scheme, k := mustScenario(t, scnKedaCeiling)
	if k.rubric.Size == nil || k.rubric.Size.Supported != 3 {
		t.Fatalf("keda-at-ceiling states the size %+v, want 3", k.rubric.Size)
	}
	for _, z := range []rubricSize{{Supported: 0, Why: "x"}, {Supported: 1001, Why: "x"}, {Supported: 3, Why: " "}} {
		k.rubric.Size = &z
		if err := k.checkRubric(); err == nil {
			t.Errorf("size %+v was accepted", z)
		}
	}
	k.rubric.Size = &rubricSize{Supported: 3, Why: "x"}
	if err := k.checkRubric(); err != nil {
		t.Error(err)
	}
	k.rubric.Size = nil
	if err := k.checkRubric(); err == nil || !strings.Contains(err.Error(), "requires size") {
		t.Errorf("a model-sized proposal without rubric size was accepted: %v", err)
	}
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join(scenariosDir, scnKedaCeiling))); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	raw, err := root.ReadFile("rubric.yaml")
	if err != nil {
		t.Fatal(err)
	}
	withoutSize, cut := strings.CutSuffix(strings.Split(string(raw), "\n# The size the evidence supports")[0]+"\n", "")
	if !cut || withoutSize == string(raw) {
		t.Fatal("the rubric has no size block to remove: the test proves nothing")
	}
	if err := root.WriteFile("rubric.yaml", []byte(withoutSize), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadScenarioDir(scheme, dir, scnKedaCeiling); err == nil ||
		!strings.Contains(err.Error(), "requires size") {
		t.Errorf("loading a model-sized proposal without rubric size: %v", err)
	}
}
