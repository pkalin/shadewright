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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/providers"
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
	readGetWebDemo = readGetWeb + " namespace=" + nsDemo
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
	res = readsRun(t, []string{"get_resource Deployment/web-canary"}, getWebDeployment)
	if res.Reads.Pass || res.Reads.Extra != 1 {
		t.Errorf("another name: reads %+v", res.Reads)
	}
}

// The harness and the analysis loop refuse the same read: one in a namespace the tools watch that is not the
// Incident subject's. The tools would serve it, so only the loop's rule (and the harness's copy) refuses it.
func TestHarnessAndLoopRefuseReadOutsideSubjectNamespace(t *testing.T) {
	const second = "second"
	saved := watchedNamespaces
	watchedNamespaces = []string{nsDemo, second}
	t.Cleanup(func() { watchedNamespaces = saved })

	const read = "get_resource Deployment/web namespace=" + second
	_, s := mustScenario(t, scnOOM)
	offered, err := offeredToolParams()
	if err != nil {
		t.Fatal(err)
	}
	rule, err := parseRead(read, offered)
	if err != nil {
		t.Fatal(err)
	}
	set, err := readCheckTools()
	if err != nil {
		t.Fatal(err)
	}
	err = rule.callable(set, s.incident.Spec.Subject.Namespace)
	if err == nil || !strings.Contains(err.Error(), "namespace of the Incident's subject") {
		t.Fatalf("harness accepted read %q: %v", read, err)
	}

	call := `  - {id: t1, name: get_resource, arguments: {kind: Deployment, namespace: ` + second + `, name: web}}` + "\n"
	res := readsRun(t, nil, call)
	if res.Reads.Refused != 1 {
		t.Errorf("loop did not refuse the same read %q: reads %+v", read, res.Reads)
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

// A rubric read that the tool or the loop refuses on every call that matches it can never be made, so every run
// would fail for the rubric's error: it is refused when it loads, by the tool's own argument checks
// (tools.Set.Call) and the loop's namespace rule. Hand-picked lines, one per kind of check; the generated
// boundaries are in TestRubricReadsAtTheSchemaBoundaries.
func TestRubricReadsTheToolRefusesAreRefused(t *testing.T) {
	_, oom := mustScenario(t, scnOOM) // subject in demo, with metrics.yaml
	long := strings.Repeat("a", 254)
	for _, line := range []string{
		"list_events namespace=demo limit=100",                                       // above the maximum of 50
		"list_events namespace=demo limit=0",                                         // below the minimum of 1
		"list_events limit=-3",                                                       // a negative integer
		"get_resource Deployment/web namespace=Demo",                                 // not a DNS label, not watched
		"get_resource Deployment/web namespace=other",                                // not the subject's namespace
		"list_events namespace=kube-system",                                          // not the subject's namespace
		"get_resource Deployment/Web_1",                                              // not an object name
		"get_resource Deployment/" + long,                                            // longer than 253
		"check_missing_reference ConfigMap/web-config pod=Web_0",                     // the pod is not an object name
		"query_metrics template=container_memory_working_set_bytes window_minutes=5", // below 15
		"query_metrics template=container_memory_working_set_bytes window_minutes=361",
		"query_metrics template=container_memory_working_set_bytes deployment=web", // a pod template with a Deployment
		"query_metrics template=deployment_replicas_unavailable pod=web-0",         // a Deployment template with a pod
		"query_metrics template=container_memory_working_set_bytes pod=" + strings.Repeat("a", 64),
	} {
		oom.rubric.Reads = []rubricRead{{Read: line}}
		if err := oom.checkReads(); err == nil {
			t.Errorf("%q was accepted", line)
		}
	}
	for _, line := range []string{
		"list_events namespace=demo limit=50",
		"list_events limit=1",
		"list_events Deployment/web",
		readGetWebDemo,
		"get_resource kind=Pod",
		"get_resource name=web",
		"check_missing_reference ConfigMap/web-config pod=web-0",
		"check_missing_reference",
		"query_metrics template=container_memory_working_set_bytes window_minutes=15",
		"query_metrics template=deployment_replicas_unavailable window_minutes=360",
		"query_metrics template=container_memory_working_set_bytes pod=" + strings.Repeat("a", 63),
		"query_metrics deployment=web",
		"query_metrics",
	} {
		oom.rubric.Reads = []rubricRead{{Read: line}}
		if err := oom.checkReads(); err != nil {
			t.Errorf("%q: %v", line, err)
		}
	}
	// The loop's own rule: a namespace the tools watch but not the subject's is refused by the loop, not the tool.
	oom.incident.Spec.Subject.Namespace = nsOther
	oom.rubric.Reads = []rubricRead{{Read: readGetWebDemo}}
	if err := oom.checkReads(); err == nil || !strings.Contains(err.Error(), "namespace of the Incident's subject") {
		t.Errorf("a watched namespace other than the subject's: %v", err)
	}
	oom.rubric.Reads = []rubricRead{{Read: "get_resource Deployment/web"}}
	if err := oom.checkReads(); err == nil {
		t.Error("a free namespace is filled with the subject's, which the tools do not watch, and was accepted")
	}
}

// Generated from the schemas of the tools the evaluation offers: for every integer argument with bounds, the
// bounds load and one past each is refused; for every string argument with a maximum length but the namespace,
// one character past it is refused. The right answer is known from the schema alone.
func TestRubricReadsAtTheSchemaBoundaries(t *testing.T) {
	_, oom := mustScenario(t, scnOOM)
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	defs, err := toolDefinitions(scheme)
	if err != nil {
		t.Fatal(err)
	}
	cases := 0
	for _, d := range defs {
		var schema struct {
			Properties map[string]struct {
				Type      string   `json:"type"`
				Minimum   *int64   `json:"minimum"`
				Maximum   *int64   `json:"maximum"`
				MaxLength *int     `json:"maxLength"`
				Enum      []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(d.Parameters, &schema); err != nil {
			t.Fatal(err)
		}
		for name, p := range schema.Properties {
			check := func(value string, want bool) {
				t.Helper()
				cases++
				line := d.Name + " " + name + "=" + value
				oom.rubric.Reads = []rubricRead{{Read: line}}
				if err := oom.checkReads(); (err == nil) != want {
					t.Errorf("%q: loaded %t, want %t (%v)", line, err == nil, want, err)
				}
			}
			switch {
			case p.Type == typeInteger && p.Minimum != nil && p.Maximum != nil:
				check(strconv.FormatInt(*p.Minimum, 10), true)
				check(strconv.FormatInt(*p.Maximum, 10), true)
				check(strconv.FormatInt(*p.Minimum-1, 10), false)
				check(strconv.FormatInt(*p.Maximum+1, 10), false)
			case p.Type == "string" && p.MaxLength != nil && len(p.Enum) == 0 && name != argNamespace:
				check(strings.Repeat("a", *p.MaxLength+1), false)
			}
		}
	}
	if cases < 10 {
		t.Fatalf("only %d generated cases: the schemas were not read", cases)
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

// cancelingSource replays the scenario's replies and cancels the run's context once the first reply is back,
// as a caller that gives up (a timeout) does: the analysis itself then returns an error.
type cancelingSource struct {
	replaySource
	cancel context.CancelFunc
}

type cancelingModel struct {
	inner  providers.ModelProvider
	cancel context.CancelFunc
}

func (m cancelingModel) Complete(ctx context.Context, req providers.CompletionRequest) (providers.Completion, error) {
	c, err := m.inner.Complete(ctx, req)
	m.cancel()
	return c, err
}

func (s cancelingSource) factory(sc *scenario) (analysis.ModelFactory, func() error, error) {
	f, done, err := s.replaySource.factory(sc)
	if err != nil {
		return nil, nil, err
	}
	return func(role analysis.Role, d remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
		p, err := f(role, d)
		if err != nil {
			return nil, err
		}
		return cancelingModel{inner: p, cancel: s.cancel}, nil
	}, done, nil
}

// A run whose analysis itself returns an error after a model call (the context ended during the tool call the
// reply asked for) keeps the tokens of that call, as one that stops after the analysis does
// (TestStoppedRunKeepsItsTokens).
func TestFailedAnalysisKeepsItsTokens(t *testing.T) {
	scheme, s := mustScenario(t, scnOOM)
	s.recording.Replies = append([]recordedReply{readsReply(t, getWebDeployment)}, s.recording.Replies...)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	res, err := runScenario(ctx, scheme, s, cancelingSource{replaySource: replaySource{force: true}, cancel: cancel})
	if err == nil || !strings.Contains(err.Error(), "the analysis stopped") {
		t.Fatalf("the analysis did not stop with an error: %v", err)
	}
	if esc := res.Tokens.ByRole[string(analysis.RoleEscalation)]; res.Tokens.Input == 0 || esc.Calls != 1 ||
		esc.Input == 0 {
		t.Errorf("tokens of a failed analysis %+v", res.Tokens)
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
	r.summarise(&prices{Input: 1, CachedInput: 1, Output: 1, Escalation: &prices{Input: 2, CachedInput: 2, Output: 2}})
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
		{"version 4", "schema version is 4, this harness reads version 5 only",
			func(d map[string]any) { d["schemaVersion"] = 4 }},
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
	// Data after the report is refused, whatever it is: a second report would otherwise be ignored.
	for _, tail := range []string{"{}", "\n" + string(raw), "x", "1", "]", "null", " \"x\""} {
		if _, err := readReport(append(slices.Clone(raw), tail...)); err == nil {
			t.Errorf("a report followed by %q was read", tail)
		}
	}
	if _, err := readReport(append(slices.Clone(raw), " \n\t"...)); err != nil {
		t.Errorf("a report followed by white space: %v", err)
	}
}
