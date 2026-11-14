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
	"strings"
	"testing"

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
