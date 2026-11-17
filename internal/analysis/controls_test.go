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

package analysis

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/redact"
	"github.com/pkalin/shadewright/internal/tools"
)

// heldControl reports whether s holds a control code point that model text is stored without.
func heldControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Cc, r) && r != '\n' && r != '\t' })
}

// controlResult is validResult with control characters in every free-text field, escaped and raw (DEL and
// C1, which JSON allows raw), a quote included, where removing them gives the text the context shows.
const controlResult = `{"summary":"The container\u001b[31m runs\r out of memory.` + "\u0085\u007f" + `",` +
	`"hypotheses":[{"description":"limit\b too low\u009b"}],` +
	`"evidence":[{"source":"c2","quote":"container app\u0007 terminated OOMKilled"}],` +
	`"missingData":["the\u0000 previous\u001b]0;title\u0007 limit"],"uncertainty":"none\f",` +
	`"proposal":{"action":{"type":"SetContainerMemoryLimit","adjustKedaMaxReplicas":null,` +
	`"setContainerMemoryLimit":{"container":"app","previous":"256Mi","desired":"512Mi"}},` +
	`"expectedEffect":"no more\u0090 OOM kills","verificationChecks":["NoNewOOMKills"]}}`

// A result holding control characters in its free-text fields is read without them: the proposal is made,
// and the text the outcome carries to the log, the record and the status holds none (#852).
func TestModelTextIsReadWithoutControls(t *testing.T) {
	f := newFixture()
	f.triage.steps = []step{reply(controlResult)}
	out := f.analyze(t, testLimits())
	if out.Reason != ReasonProposed || out.Plan == nil || out.Plan.Action.SetContainerMemoryLimit.Container != testContainer {
		t.Fatalf("outcome %+v, want the proposal for container %s", out, testContainer)
	}
	if want := "The container[31m runs out of memory."; out.Summary != want {
		t.Errorf("summary %q, want %q", out.Summary, want)
	}
	if len(out.MissingData) != 1 || out.MissingData[0] != "the previous]0;title limit" {
		t.Errorf("missing data %q, want the item without its controls", out.MissingData)
	}
}

// controlCall is a call of list_events whose namespace, once its controls are removed, is the subject's
// ("sh" ESC "op" C1 becomes "shop"): run with the cleaned value, it would read what the model did not ask for.
func controlCall(id string) providers.ToolCall {
	return toolCall(id, tools.NameListEvents, `{"namespace":"sh\u001bop`+"\u009b"+`"}`)
}

// toolMessages returns the tool results of the last request of m, by tool call ID.
func toolMessages(m *scriptModel) map[string]string {
	reqs := m.reqs()
	out := map[string]string{}
	for _, msg := range reqs[len(reqs)-1].Messages {
		if msg.Role == providers.RoleTool {
			out[msg.ToolCallID] = msg.Content
		}
	}
	return out
}

// A tool call whose arguments held a control character is not run (decided 2026-11-17 on #852): its result
// is the fixed refusal, which quotes nothing of the call; the call the loop sends back is the cleaned form
// with the provider blocks unchanged; the refusal is counted in the outcome and in the analysis record.
func TestToolCallWithControlsIsRefused(t *testing.T) {
	f := newFixture()
	f.builder.c.Truncated = true
	first := toolReply(controlCall("c1"))
	blocks := []byte(`[{"type":"tool_use","input":{"namespace":"sh\u001bop"}}]`)
	first.c.ProviderBlocks = providers.NewOpaque(blocks)
	f.escalation.steps = []step{first, reply(validResult)}
	out := f.analyze(t, testLimits())
	if out.Reason != ReasonProposed {
		t.Fatalf("reason %s, want Proposed", out.Reason)
	}
	if len(f.tools.calls) != 0 {
		t.Fatalf("the tool ran %d times with %s, want the call refused", len(f.tools.calls), f.tools.calls[0].Arguments)
	}
	if out.ToolCalls != 1 || out.ToolCallsWithControls != 1 {
		t.Errorf("tool calls %d, with controls %d, want 1 and 1", out.ToolCalls, out.ToolCallsWithControls)
	}
	results := toolMessages(f.escalation)
	if got := results["c1"]; !strings.Contains(got, heldControlsRefusal) || strings.Contains(got, "shop") || heldControl(got) {
		t.Errorf("tool result %q, want the fixed refusal", got)
	}
	reqs := f.escalation.reqs()
	var assistant providers.Message
	for _, m := range reqs[len(reqs)-1].Messages {
		if m.Role == providers.RoleAssistant {
			assistant = m
		}
	}
	if string(assistant.ProviderBlocks.Bytes()) != string(blocks) {
		t.Errorf("provider blocks sent back %q, want them unchanged", assistant.ProviderBlocks.Bytes())
	}
	if len(assistant.ToolCalls) != 1 || heldControl(string(assistant.ToolCalls[0].Arguments)) {
		t.Errorf("sent back %+v, want the cleaned call", assistant.ToolCalls)
	}
	inc := incident()
	inc.Name, inc.UID = "inc-1", "0b0e3c1c-7f4a-4c55-9c39-6a3e2d2f0a02"
	inc.Status.LastAnalysisTime = &metav1.Time{Time: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)}
	a, err := New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{Analyzer: a, Now: func() time.Time { return inc.Status.LastAnalysisTime.Time }}
	rec, err := c.buildRecord(inc, recordID(inc), out, testLimits(), "")
	if err != nil {
		t.Fatal(err)
	}
	var r Record
	if err := json.Unmarshal(rec.Data, &r); err != nil || r.ToolCallsWithControls != 1 || r.ToolCalls != 1 {
		t.Errorf("record %s (%v), want one tool call refused for its controls", rec.Data, err)
	}
}

// Of two calls in one reply, the one that held a control is refused and the clean one runs.
func TestOnlyTheCleanToolCallOfAReplyRuns(t *testing.T) {
	f := newFixture()
	f.builder.c.Truncated = true
	f.escalation.steps = []step{
		toolReply(controlCall("c1"), toolCall("c2", tools.NameListEvents, `{"namespace":"shop"}`)),
		reply(validResult),
	}
	out := f.analyze(t, testLimits())
	if out.Reason != ReasonProposed {
		t.Fatalf("reason %s, want Proposed", out.Reason)
	}
	if len(f.tools.calls) != 1 || f.tools.calls[0].ID != "c2" {
		t.Fatalf("the tool ran %+v, want only c2", f.tools.calls)
	}
	results := toolMessages(f.escalation)
	if !strings.Contains(results["c1"], heldControlsRefusal) || !strings.Contains(results["c2"], f.tools.text) {
		t.Errorf("tool results %q, want c1 refused and c2 answered", results)
	}
	if out.ToolCalls != 2 || out.ToolCallsWithControls != 1 {
		t.Errorf("tool calls %d, with controls %d, want 2 and 1", out.ToolCalls, out.ToolCallsWithControls)
	}
}

// A model that keeps asking for calls that held controls uses its steps as with any refused call, and the
// escalation ends at the step limit: there is no loop without an end.
func TestRefusedControlCallsUseTheStepBudget(t *testing.T) {
	f := newFixture()
	f.builder.c.Truncated = true
	limits := testLimits()
	limits.MaxToolSteps = 2
	for i := range 10 {
		f.escalation.steps = append(f.escalation.steps, toolReply(controlCall(fmt.Sprintf("c%d", i))))
	}
	out := f.analyze(t, limits)
	if out.Reason != ReasonStepLimit {
		t.Fatalf("reason %s, want StepLimit", out.Reason)
	}
	if n := f.escalation.calls(); n != limits.MaxToolSteps+1 {
		t.Errorf("%d escalation calls, want %d", n, limits.MaxToolSteps+1)
	}
	if len(f.tools.calls) != 0 || out.ToolCalls != 2 || out.ToolCallsWithControls != 2 {
		t.Errorf("tools ran %d, tool calls %d, with controls %d, want 0, 2 and 2",
			len(f.tools.calls), out.ToolCalls, out.ToolCallsWithControls)
	}
}

// A result that is valid with its controls, escaped or raw, still loses them: before #852 such a summary
// reached the log, the record and the Incident status with the escape sequence in it.
func TestValidResultLosesItsControls(t *testing.T) {
	f := newFixture()
	f.triage.steps = []step{reply(strings.Replace(validResult, "The container runs out of memory.",
		`The key sk\u001b[8mXX\u001b[0m runs\b\b out`+"\u009b2K", 1))}
	out := f.analyze(t, testLimits())
	if out.Reason != ReasonProposed {
		t.Fatalf("reason %s, want Proposed", out.Reason)
	}
	if want := "The key sk[8mXX[0m runs out2K"; out.Summary != want || heldControl(out.Summary) {
		t.Errorf("summary %q, want %q", out.Summary, want)
	}
}

// A control in a field of the result that is an enum, a pattern or an identifier ends the result as invalid,
// as the same value did before #852 (decided 2026-11-17 on #852): removing it would turn a value the
// validator refuses into one it accepts, and a request would name a container the model never wrote. Each
// case is valid once its control is removed, so the test fails if any of them is cleaned instead.
func TestControlInAnIdentifierFieldIsInvalidOutput(t *testing.T) {
	for name, edit := range map[string][2]string{
		"container":          {`"container":"app"`, `"container":"ap\u001bp"`},
		"container raw C1":   {`"container":"app"`, `"container":"ap` + "\u0085" + `p"`},
		"previous":           {`"previous":"256Mi"`, `"previous":"256\u001bMi"`},
		"desired":            {`"desired":"512Mi"`, `"desired":"512Mi\u0007"`},
		"evidence source":    {`"source":"c2"`, `"source":"c\u00002"`},
		"action type":        {`"type":"SetContainerMemoryLimit"`, `"type":"SetContainer\u009bMemoryLimit"`},
		"verification check": {`["NoNewOOMKills","NoNewOOMKills"]`, `["NoNewOOMKills","NoNew\u001bOOMKills"]`},
		"key":                {`"summary":`, `"summ\u001bary":`},
		"nested key":         {`"container":`, `"contain\u007fer":`},
	} {
		t.Run(name, func(t *testing.T) {
			bad := strings.Replace(validResult, edit[0], edit[1], 1)
			if bad == validResult {
				t.Fatalf("%q is not in the valid result", edit[0])
			}
			if string(providers.StripControlsJSON([]byte(bad))) != validResult {
				t.Fatalf("%s is not the valid result once cleaned, so the case proves nothing", bad)
			}
			f := newFixture()
			f.builder.c.Truncated = true
			f.escalation.steps = []step{reply(bad)}
			if out := f.analyze(t, testLimits()); out.Reason != ReasonInvalidOutput || out.Plan != nil {
				t.Errorf("reason %s, plan %v, want InvalidOutput and no plan", out.Reason, out.Plan)
			}
		})
	}
}

// In triage the refusal is the schema refusal: the model is asked once more with the fixed corrective
// message, and a clean second answer is taken.
func TestControlInAnIdentifierFieldIsRetriedInTriage(t *testing.T) {
	f := newFixture()
	f.triage.steps = []step{reply(strings.Replace(validResult, `"container":"app"`, `"container":"ap\u001bp"`, 1)),
		reply(validResult)}
	out := f.analyze(t, testLimits())
	if out.Reason != ReasonProposed || f.triage.calls() != 2 {
		t.Fatalf("reason %s after %d triage calls, want Proposed after 2", out.Reason, f.triage.calls())
	}
	reqs := f.triage.reqs()
	last := reqs[len(reqs)-1].Messages
	if got := last[len(last)-1].Content; got != retryMessage(errInvalid.code) {
		t.Errorf("corrective message %q, want the schema refusal", got)
	}
}

// Every string field of the result, and every list of strings, declares what a control in it does: free text
// is cleaned, the rest refused. A field added later without the tag fails here, and is refused meanwhile.
func TestEveryResultStringFieldDeclaresItsControls(t *testing.T) {
	var walk func(t reflect.Type, at string)
	walk = func(rt reflect.Type, at string) {
		for f := range rt.Fields() {
			ft := f.Type
			for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice {
				ft = ft.Elem()
			}
			switch ft.Kind() {
			case reflect.Struct:
				if _, ok := f.Tag.Lookup(controlsTag); ok {
					t.Errorf("%s.%s: a controls tag on a struct field is never read", at, f.Name)
				}
				walk(ft, at+"."+f.Name)
			case reflect.String:
				if tag := f.Tag.Get(controlsTag); tag != controlsClean && tag != controlsRefuse {
					t.Errorf("%s.%s: controls tag %q, want %q (free text) or %q", at, f.Name, tag, controlsClean, controlsRefuse)
				}
			}
		}
	}
	walk(reflect.TypeFor[Result](), "Result")
}

// The places a control may be: the value of a free-text field, a list element of one included; never a key,
// an identifier, a place the result does not have, or a document that did not parse (the root).
func TestControlsRefusedByPlace(t *testing.T) {
	for _, tc := range []struct {
		path    string // the members and indexes, joined by "/"
		key     bool
		refused bool
	}{
		{jsonName(reflect.TypeFor[Result]().Field(0)), false, false},
		{"hypotheses/0/description", false, false},
		{"evidence/3/quote", false, false},
		{"missingData/9", false, false},
		{"uncertainty", false, false},
		{"proposal/expectedEffect", false, false},
		{"uncertainty", true, true},
		{"", false, true},
		{"evidence/0/source", false, true},
		{"evidence/x/quote", false, true},
		{"evidence/0", false, true},
		{"missingData", false, true},
		{"Summary", false, true},
		{"confidence", false, true},
		{"summary/0", false, true},
		{"proposal/action/type", false, true},
		{"proposal/action/setContainerMemoryLimit/container", false, true},
		{"proposal/verificationChecks/0", false, true},
	} {
		var path []string
		if tc.path != "" {
			path = strings.Split(tc.path, "/")
		}
		if got := controlsRefused([]providers.ControlPath{{Path: path, Key: tc.key}}); got != tc.refused {
			t.Errorf("%q key=%v: refused %v, want %v", tc.path, tc.key, got, tc.refused)
		}
	}
	type later struct {
		Note string `json:"note"`
	}
	if freeTextAt(reflect.TypeFor[later](), []string{"note"}) {
		t.Error("a field without a controls tag is taken for free text")
	}
}

// A log line coloured with ANSI escapes reaches the model without its controls, as the context builder renders
// it (incidentcontext.RenderLog, after redaction), so a quote of it is found whether the model copies the line
// as shown or writes its escapes back: the quote is cleaned the same way (decided 2026-11-17 on #852). Before,
// the context kept the escapes and both quotes ended NoEvidence.
func TestColouredLogLineIsQuotable(t *testing.T) {
	ex := providers.LogExcerpt{Text: "starting\n\x1b[1;31mERROR\x1b[0m out of memory: killed process\r\n",
		CollectedAt: time.Unix(0, 0)}
	view := incidentcontext.RenderLog(redact.String, "pod web-1 container app", ex, nil, 4096, 4096)
	for name, quote := range map[string]string{
		"as shown":           `[1;31mERROR[0m out of memory: killed process`,
		"with its escapes":   `\u001b[1;31mERROR\u001b[0m out of memory: killed process`,
		"raw C1 in the copy": `[1;31mERROR[0m out of memory:` + "\u009b" + ` killed process`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			f.builder.c.Blocks = append(f.builder.c.Blocks, incidentcontext.Block{Section: incidentcontext.SectionLogs,
				Provenance: incidentcontext.ProvenanceLogExcerpt, Availability: incidentcontext.Available,
				Text: view.Text[view.HeadLen:]})
			f.triage.steps = []step{reply(strings.Replace(validResult,
				`{"source":"c2","quote":"container app terminated OOMKilled"}`, `{"source":"c3","quote":"`+quote+`"}`, 1))}
			out := f.analyze(t, testLimits())
			if out.Reason != ReasonProposed || len(out.QuoteFailures) != 0 {
				t.Errorf("reason %s, quote failures %+v, want the quote of c3 found", out.Reason, out.QuoteFailures)
			}
		})
	}
}

// A reviewer hint is shown without control characters, like the context it follows.
func TestReviewerHintIsShownWithoutControls(t *testing.T) {
	f := newFixture()
	f.triage.steps = []step{reply(validResult)}
	a, err := New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.AnalyzeWithHint(t.Context(), incident(), testLimits(), "use\x1b[8m 1Gi\u0085 please\r"); err != nil {
		t.Fatal(err)
	}
	msgs := f.triage.reqs()[0].Messages
	got := msgs[len(msgs)-1].Content
	if !strings.Contains(got, "use[8m 1Gi please") || heldControl(got) {
		t.Errorf("the hint block is\n%q\nwant the hint without its controls", got)
	}
}
