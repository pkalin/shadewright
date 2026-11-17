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
	"strings"
	"testing"
	"unicode"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/tools"
)

// heldControl reports whether s holds a control code point that model text is stored without.
func heldControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Cc, r) && r != '\n' && r != '\t' })
}

// controlResult is validResult with control characters in every text field, escaped and raw (DEL and C1,
// which JSON allows raw), and inside the container name and a quote, where removing them gives the valid
// value.
const controlResult = `{"summary":"The container\u001b[31m runs\r out of memory.` + "\u0085\u007f" + `",` +
	`"hypotheses":[{"description":"limit\b too low\u009b"}],` +
	`"evidence":[{"source":"c2","quote":"container app\u0007 terminated OOMKilled"}],` +
	`"missingData":["the\u0000 previous\u001b]0;title\u0007 limit"],"uncertainty":"none\f",` +
	`"proposal":{"action":{"type":"SetContainerMemoryLimit","adjustKedaMaxReplicas":null,` +
	`"setContainerMemoryLimit":{"container":"a\u001bpp","previous":"256Mi","desired":"512Mi"}},` +
	`"expectedEffect":"no more\u0090 OOM kills","verificationChecks":["NoNewOOMKills"]}}`

// A result holding control characters is read without them: the proposal is made, and the text the outcome
// carries to the log, the record and the status holds none (#852).
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

// A tool call's arguments are read without their controls: the tool gets the decoded value without them, and
// the call the loop sends back to the model is the cleaned one.
func TestToolCallArgumentsAreReadWithoutControls(t *testing.T) {
	f := newFixture()
	f.builder.c.Truncated = true
	f.escalation.steps = []step{
		toolReply(toolCall("c1", tools.NameListEvents, `{"namespace":"sh\u001bop`+"\u009b"+`"}`)),
		reply(validResult),
	}
	out := f.analyze(t, testLimits())
	if out.Reason != ReasonProposed {
		t.Fatalf("reason %s, want Proposed", out.Reason)
	}
	if len(f.tools.calls) != 1 {
		t.Fatalf("%d tool calls, want 1", len(f.tools.calls))
	}
	var args map[string]string
	if err := json.Unmarshal(f.tools.calls[0].Arguments, &args); err != nil || args["namespace"] != "shop" {
		t.Errorf("the tool got %s (%v), want the namespace without its controls", f.tools.calls[0].Arguments, err)
	}
	reqs := f.escalation.reqs()
	var sentBack []providers.ToolCall
	for _, m := range reqs[len(reqs)-1].Messages {
		sentBack = append(sentBack, m.ToolCalls...)
	}
	if len(sentBack) != 1 || string(sentBack[0].Arguments) != string(f.tools.calls[0].Arguments) {
		t.Errorf("sent back %+v, want the cleaned call", sentBack)
	}
	if heldControl(string(sentBack[0].Arguments)) {
		t.Errorf("sent back %s with a control", sentBack[0].Arguments)
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
