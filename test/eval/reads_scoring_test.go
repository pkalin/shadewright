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
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/pkalin/shadewright/internal/analysis"
)

// readsReply is a recorded escalation reply that asks for the tool calls of calls, a YAML list.
func readsReply(t *testing.T, calls string) recordedReply {
	t.Helper()
	var r recordedReply
	doc := "model: " + fakeModelName + "\nusage: {inputTokens: 900, outputTokens: 40}\ntoolCalls:\n" + calls
	if err := yaml.UnmarshalStrict([]byte(doc), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// readsRun replays oom forced to the escalation model: first a reply asking for calls, then the scenario's own
// result, scored against the expected reads.
func readsRun(t *testing.T, reads []string, calls string) scenarioResult {
	t.Helper()
	scheme, s := mustScenario(t, scnOOM)
	return readsRunOf(t, scheme, s, reads, calls)
}

func readsRunOf(t *testing.T, scheme *runtime.Scheme, s *scenario, reads []string, calls string) scenarioResult {
	t.Helper()
	s.rubric.Reads = nil
	for _, r := range reads {
		s.rubric.Reads = append(s.rubric.Reads, rubricRead{Read: r})
	}
	if err := s.checkReads(); err != nil {
		t.Fatal(err)
	}
	s.recording.Replies = append([]recordedReply{readsReply(t, calls)}, s.recording.Replies...)
	res, err := runScenario(t.Context(), scheme, s, replaySource{force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Outcome.Pass || !res.Diagnosis.Pass || res.ToolCalls == 0 {
		t.Fatalf("the replay itself does not pass: outcome %+v, diagnosis %+v, tool calls %d", res.Outcome,
			res.Diagnosis, res.ToolCalls)
	}
	return res
}

// Expected reads the tests use.
const (
	readGetWeb     = "get_resource Deployment/web"
	readEventsPods = "list_events namespace=demo kind=Pod"
)

const getWebDeployment = `  - {id: t1, name: get_resource, arguments: {kind: Deployment, namespace: demo, name: web}}
`

// An expected read the model made passes, and the run passes.
func TestExpectedReadPresentPasses(t *testing.T) {
	res := readsRun(t, []string{readGetWeb}, getWebDeployment)
	want := []readCheck{{Read: readGetWeb, Pass: true}}
	if !res.Reads.Pass || len(res.Reads.Expected) != 1 || res.Reads.Expected[0] != want[0] || res.Reads.Extra != 0 ||
		res.Reads.Refused != 0 || !res.Pass {
		t.Errorf("reads %+v, pass %v", res.Reads, res.Pass)
	}
	if esc := res.Tokens.ByRole[string(analysis.RoleEscalation)]; esc.Calls != 2 {
		t.Errorf("escalation calls %+v", esc)
	}
}

// An expected read the model did not make fails the read and the run, while the outcome and the diagnosis pass.
func TestExpectedReadMissingFails(t *testing.T) {
	res := readsRun(t, []string{readGetWeb, readEventsPods},
		getWebDeployment)
	if res.Reads.Pass || res.Pass || len(res.Reads.Expected) != 2 || !res.Reads.Expected[0].Pass ||
		res.Reads.Expected[1].Pass || res.Reads.Expected[1].Read != readEventsPods {
		t.Errorf("reads %+v, pass %v", res.Reads, res.Pass)
	}
	// The same read with other arguments is not the expected one.
	res = readsRun(t, []string{"get_resource Deployment/web namespace=other"}, getWebDeployment)
	if res.Reads.Pass || res.Reads.Extra != 1 {
		t.Errorf("another namespace: reads %+v", res.Reads)
	}
}

// A read the rubric does not expect is counted, not failed; a call the tool refused reads nothing and is
// counted apart.
func TestExtraReadIsCounted(t *testing.T) {
	res := readsRun(t, []string{readGetWeb}, getWebDeployment+
		`  - {id: t2, name: list_events, arguments: {namespace: demo}}
  - {id: t3, name: exec, arguments: {command: "ls"}}
  - {id: t4, name: get_resource, arguments: {kind: Service, namespace: demo, name: web}}
`)
	if !res.Reads.Pass || !res.Pass || res.Reads.Extra != 1 || res.Reads.Refused != 2 {
		t.Errorf("reads %+v, pass %v", res.Reads, res.Pass)
	}
	// A rubric without reads: every read is extra and the run passes.
	res = readsRun(t, nil, getWebDeployment)
	if !res.Reads.Pass || !res.Pass || res.Reads.Extra != 1 || len(res.Reads.Expected) != 0 {
		t.Errorf("no expected reads: %+v", res.Reads)
	}
}

// A rubric whose read cannot be made in its scenario is refused when it loads: a tool the scenario is not
// offered, and a read listed twice.
func TestRubricReadsAreChecked(t *testing.T) {
	_, s := mustScenario(t, scnBadImage) // no metrics.yaml, so no query_metrics
	for _, reads := range [][]string{
		{"query_metrics template=container_memory_working_set_bytes"},
		{readGetWeb, "get_resource  kind=Deployment name=web"},
		{"get_resource Service/web"},
	} {
		s.rubric.Reads = nil
		for _, r := range reads {
			s.rubric.Reads = append(s.rubric.Reads, rubricRead{Read: r})
		}
		if err := s.checkReads(); err == nil {
			t.Errorf("%q was accepted", reads)
		}
	}
	_, oom := mustScenario(t, scnOOM)
	oom.rubric.Reads = []rubricRead{{Read: "query_metrics template=container_memory_working_set_bytes"}}
	if err := oom.checkReads(); err != nil {
		t.Errorf("query_metrics in a scenario with metrics.yaml: %v", err)
	}
}

// A run that stopped with an error after its model calls keeps the tokens they used, so its cost is reported as
// incomplete instead of zero.
func TestStoppedRunKeepsItsTokens(t *testing.T) {
	scheme, s := mustScenario(t, scnBadImage)
	s.recording.Replies = append(s.recording.Replies, s.recording.Replies[len(s.recording.Replies)-1])
	res, err := runScenario(t.Context(), scheme, s, replaySource{})
	if err == nil {
		t.Fatal("a leftover reply was accepted")
	}
	if res.Tokens.Input == 0 || res.Tokens.ByRole[string(analysis.RoleTriage)].Calls != 1 {
		t.Errorf("tokens of a stopped run %+v", res.Tokens)
	}
	res.Error = err.Error()
	r := &report{Manifest: manifest{MinPassRate: 1, Repeats: 1}, Scenarios: []scenarioResult{res}}
	r.summarise(&prices{Input: 1, CachedInput: 1, Output: 1})
	if c := r.Scenarios[0].CostUSD; c == nil || *c == 0 || !r.Scenarios[0].CostIncomplete || !r.Totals.CostIncomplete ||
		r.Totals.IncompleteRuns != 1 {
		t.Errorf("run %+v, totals %+v", r.Scenarios[0], r.Totals)
	}
}

// Each run carries its own cost, and the totals the cost per passed run: every run's cost, failed runs included,
// over the passed runs. With no passed run there is none, not a zero and not a division by zero; a run that
// stopped with an error, even before any call, makes the totals incomplete and the Markdown says so.
func TestCostPerPassedRun(t *testing.T) {
	var a, b tokens
	a.addRole(string(analysis.RoleTriage), roleTokens{Calls: 1, Input: 1000, Output: 100})
	b.addRole(string(analysis.RoleEscalation), roleTokens{Calls: 2, Input: 3000, Output: 200})
	p := &prices{Input: 1, CachedInput: 1, Output: 10, Escalation: &prices{Input: 2, CachedInput: 2, Output: 20}}
	r := &report{Manifest: manifest{MinPassRate: 0, Repeats: 1}, Scenarios: []scenarioResult{
		{Name: "a", Pass: true, Tokens: a},
		{Name: "b", Pass: false, Tokens: b, ToolCalls: 3},
	}}
	r.summarise(p)
	// a: (1000 + 100*10) / 1e6 = 0.002; b: (3000*2 + 200*20) / 1e6 = 0.01.
	near := func(x *float64, y float64) bool { return x != nil && *x-y < 1e-12 && y-*x < 1e-12 }
	if !near(r.Scenarios[0].CostUSD, 0.002) || !near(r.Scenarios[1].CostUSD, 0.01) ||
		!near(r.Totals.CostUSD, 0.012) || !near(r.Totals.CostPerPassedRunUSD, 0.012) || r.Totals.CostIncomplete ||
		r.Totals.ToolCalls != 3 || r.Scenarios[1].CostUSDByRole[string(analysis.RoleEscalation)] == 0 {
		t.Errorf("runs %+v, totals %+v", r.Scenarios, r.Totals)
	}
	if md := r.markdown(); !strings.Contains(md, "- Cost per passed run: USD 0.0120 (all runs' cost over 1 passed)\n") ||
		!strings.Contains(md, "| 0.0100 |") || !strings.Contains(md, "escalation: calls 2,") {
		t.Errorf("markdown:\n%s", md)
	}

	r.Scenarios[0].Pass = false
	r.summarise(p)
	if r.Totals.CostPerPassedRunUSD != nil || !near(r.Totals.CostUSD, 0.012) {
		t.Errorf("no passed run: totals %+v", r.Totals)
	}
	if md := r.markdown(); !strings.Contains(md, "- Cost per passed run: none, no run passed") {
		t.Errorf("markdown:\n%s", md)
	}

	r.Scenarios[0].Pass = true
	r.Scenarios = append(r.Scenarios, scenarioResult{Name: "c", Error: "could not start"})
	r.summarise(p)
	if !r.Totals.CostIncomplete || r.Totals.IncompleteRuns != 1 || !r.Scenarios[2].CostIncomplete ||
		r.Scenarios[2].CostUSD == nil {
		t.Errorf("an errored run: runs %+v, totals %+v", r.Scenarios, r.Totals)
	}
	md := r.markdown()
	for _, want := range []string{"1 run(s) incomplete", "(all runs' cost over 1 passed), incomplete, a lower bound",
		"0.0000 (incomplete)"} {
		if !strings.Contains(md, want) {
			t.Errorf("the markdown lacks %q:\n%s", want, md)
		}
	}

	r.summarise(nil)
	if r.Totals.CostPerPassedRunUSD != nil || r.Scenarios[0].CostUSD != nil || strings.Contains(r.markdown(), "Cost") {
		t.Errorf("without prices: runs %+v, totals %+v", r.Scenarios, r.Totals)
	}
}

// A report reads back at its own schema version; another version, a report without one and a field the version
// does not know are refused, never read as this version.
func TestReadReportChecksTheSchemaVersion(t *testing.T) {
	res := readsRun(t, []string{readGetWeb}, getWebDeployment)
	r := &report{SchemaVersion: reportSchemaVersion, Mode: providerFake,
		Manifest:  manifest{MinPassRate: 1, Repeats: 1, Rubric: manifestRubric{FleetCommit: "0123abc"}},
		Scenarios: []scenarioResult{res}}
	r.summarise(&prices{Input: 1, CachedInput: 1, Output: 1})
	dir := t.TempDir()
	if err := r.write(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	back, err := readReport(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.Scenarios[0].Reads.Expected[0] != (readCheck{Read: readGetWeb, Pass: true}) ||
		back.Totals.CostPerPassedRunUSD == nil {
		t.Errorf("read back %+v, totals %+v", back.Scenarios[0].Reads, back.Totals)
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, want string
		edit       func(map[string]any)
	}{
		{"version 3", "schema version is 3, this harness reads version 4 only",
			func(d map[string]any) { d["schemaVersion"] = 3 }},
		{"no version", "no schemaVersion", func(d map[string]any) { delete(d, "schemaVersion") }},
		{"unknown field", "unknown field", func(d map[string]any) { d["costPerPass"] = 1 }},
	} {
		d := maps.Clone(doc)
		c.edit(d)
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := readReport(b); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	if _, err := readReport([]byte("not json")); err == nil {
		t.Error("a non-JSON report was read")
	}
}
