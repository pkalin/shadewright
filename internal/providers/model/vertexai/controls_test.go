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

package vertexai

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode"

	"github.com/shadewright-labs/shadewright/internal/analysis"
	"github.com/shadewright-labs/shadewright/internal/tools"
)

// heldControl reports whether a string of v, walked by reflection, holds a control code point that model
// text is stored without (Unicode Cc but newline and tab).
func heldControl(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		return strings.ContainsFunc(v.String(), func(r rune) bool { return unicode.Is(unicode.Cc, r) && r != '\n' && r != '\t' })
	case reflect.Pointer, reflect.Interface:
		return !v.IsNil() && heldControl(v.Elem())
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if heldControl(v.Index(i)) {
				return true
			}
		}
	case reflect.Struct:
		for _, f := range v.Fields() {
			if heldControl(f) {
				return true
			}
		}
	}
	return false
}

// removedControls are the code points model text is stored without: Unicode Cc but newline and tab.
func removedControls() []rune {
	var out []rune
	for r := rune(0); r <= 0x9f; r++ {
		if unicode.Is(unicode.Cc, r) && r != '\n' && r != '\t' {
			out = append(out, r)
		}
	}
	return out
}

// controlResult is a result without a proposal whose summary and missing data hold c, as a JSON escape when
// escaped, else raw (which JSON refuses for C0: such a reply is refused as invalid output, never repaired).
func controlResult(c rune, escaped bool) string {
	s := string(c)
	if escaped {
		s = fmt.Sprintf(`\u%04x`, c)
	}
	return `{"summary":"S` + s + `X","hypotheses":[],"evidence":[],"missingData":["M` + s + `Y"],` +
		`"uncertainty":"","proposal":null}`
}

// Every control code point, escaped and raw, in a tool call's arguments and in the result, through the
// adapter and the analysis loop: the tool call is refused, never run with the cleaned value, its result sent
// back is the fixed refusal (and the replay of the call, stored without its controls, is accepted), it uses
// its step, and nothing of the outcome holds a control.
func TestLoopReadsModelTextWithoutControls(t *testing.T) {
	for _, c := range removedControls() {
		for _, escaped := range []bool{true, false} {
			args := fmt.Sprintf(`{"namespace":"sh\u%04xop"}`, c)
			out, sent, lt := runLoop(t, 2, toolCallReply("c1", tools.NameListEvents, args),
				replyJSON("["+textPart(controlResult(c, escaped))+"]"))
			if len(lt.args) != 0 {
				t.Errorf("U+%04X: the tool ran with %q, want the call refused", c, lt.args)
			}
			if out.ToolCalls != 1 || out.ToolCallsWithControls != 1 {
				t.Errorf("U+%04X: tool calls %d, with controls %d, want 1 and 1", c, out.ToolCalls, out.ToolCallsWithControls)
			}
			if len(sent) != 2 || !strings.Contains(string(sent[1].body), "held control characters") {
				t.Errorf("U+%04X: %d requests, want the refusal sent back as the tool result", c, len(sent))
			}
			if heldControl(reflect.ValueOf(out)) {
				t.Errorf("U+%04X, escaped %v: the outcome holds a control: %+v", c, escaped, out)
			}
			// Escaped, and raw DEL and C1 (which JSON allows in a string), are read without the control; a raw C0
			// control is not JSON, and the reply is refused as invalid output, never repaired.
			if (escaped || c >= 0x7f) && (out.Reason != analysis.ReasonMissingData || out.Summary != "SX" ||
				len(out.MissingData) != 1 || out.MissingData[0] != "MY") {
				t.Errorf("U+%04X: reason %s, summary %q, missing data %q, want MissingData, SX and MY",
					c, out.Reason, out.Summary, out.MissingData)
			}
			if !escaped && c < 0x7f && (out.Reason != analysis.ReasonInvalidOutput || out.Summary != "") {
				t.Errorf("U+%04X: raw: reason %s, summary %q, want InvalidOutput and no summary", c, out.Reason, out.Summary)
			}
		}
	}
}

// A reply with a call that held a control and a clean one: only the clean one runs.
func TestLoopRunsOnlyTheCleanToolCall(t *testing.T) {
	bad, good := `{"namespace":"sh\u001bop"}`, shopArgs
	call := replyJSON("[" + callPart("c1", tools.NameListEvents, bad) + "," +
		callPart("c2", tools.NameListEvents, good) + "]")
	out, sent, lt := runLoop(t, 2, call, call, call)
	if len(lt.args) != 1 || lt.args[0] != good {
		t.Errorf("the tool ran with %q, want only the clean call", lt.args)
	}
	if out.Reason != analysis.ReasonStepLimit || out.ToolCalls != 2 || out.ToolCallsWithControls != 1 {
		t.Errorf("reason %s, tool calls %d, with controls %d, want StepLimit, 2 and 1", out.Reason, out.ToolCalls,
			out.ToolCallsWithControls)
	}
	if len(sent) < 2 || !strings.Contains(string(sent[1].body), "held control characters") {
		t.Error("the refusal was not sent back")
	}
}

// A control character in a call's ID or name is refused by the adapter before any provider block is made:
// the reply is malformed output with its usage, whatever the position of the call in the reply.
func TestParseRefusesControlsInIDsAndNames(t *testing.T) {
	for _, c := range removedControls() {
		for _, fc := range []string{
			fmt.Sprintf(`{"functionCall":{"id":"c\u%04x","name":"list_events","args":{}}}`, c),
			fmt.Sprintf(`{"functionCall":{"id":"c1","name":"list\u%04xevents","args":{}}}`, c),
		} {
			body := replyJSON("[" + callPart("c0", listEvents, `{}`) + "," + fc + "]")
			got, err := parseResponse([]byte(body), testModel, offeredTools, true)
			if err == nil || !strings.Contains(err.Error(), "control character") || !got.ProviderBlocks.IsZero() ||
				got.ToolCalls != nil || got.Usage != wantUsage {
				t.Errorf("U+%04X in %s: err = %v, completion %+v", c, fc, err, got)
			}
		}
	}
}
