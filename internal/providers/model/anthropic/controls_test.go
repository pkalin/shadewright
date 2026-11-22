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

package anthropic

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode"

	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/tools"
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

// toolUse is a reply with the thinking block the API signs and one tool_use block per input, IDs from ids.
func toolUse(ids []string, inputs ...string) string {
	blocks := `{"type":"thinking","thinking":"","signature":"sig-` + ids[0] + `"}`
	for i, in := range inputs {
		blocks += `,{"type":"tool_use","id":"` + ids[i] + `","name":"` + tools.NameListEvents + `","input":` + in + `}`
	}
	return messageJSON(testModel, stopToolUse, `[`+blocks+`]`)
}

// replayed returns the tool_use inputs of the assistant turns and the tool_result contents of a request body.
func replayed(t *testing.T, body []byte) (inputs []json.RawMessage, results []string) {
	t.Helper()
	var req struct {
		Messages []struct {
			Content []struct {
				Type    string          `json:"type"`
				Input   json.RawMessage `json:"input"`
				Content string          `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case "tool_use":
				inputs = append(inputs, b.Input)
			case "tool_result":
				results = append(results, b.Content)
			}
		}
	}
	return inputs, results
}

// Every control code point, escaped and raw, in a tool call's input and in the result, through this adapter
// and the analysis loop: the tool call is refused, never run with the cleaned value; the blocks replayed hold
// the input as the model wrote it, answered by the fixed refusal; the call uses its step; and nothing of the
// outcome holds a control (#852).
func TestLoopReadsModelTextWithoutControls(t *testing.T) {
	for _, c := range removedControls() {
		for _, escaped := range []bool{true, false} {
			input := fmt.Sprintf(`{"namespace":"sh\u%04xop"}`, c)
			text := messageJSON(testModel, stopEndTurn, `[{"type":"text","text":`+jsonString(controlResult(c, escaped))+`}]`)
			out, bodies, lt := runLoopBodies(t, toolUse([]string{callOne}, input), text)
			if len(lt.args) != 0 {
				t.Errorf("U+%04X: the tool ran with %q, want the call refused", c, lt.args)
			}
			if out.ToolCalls != 1 || out.ToolCallsWithControls != 1 {
				t.Errorf("U+%04X: tool calls %d, with controls %d, want 1 and 1", c, out.ToolCalls, out.ToolCallsWithControls)
			}
			if len(bodies) != 2 {
				t.Fatalf("U+%04X: %d requests, want 2", c, len(bodies))
			}
			inputs, results := replayed(t, bodies[1])
			var ns map[string]string
			if len(inputs) != 1 || json.Unmarshal(inputs[0], &ns) != nil || ns["namespace"] != "sh"+string(c)+"op" {
				t.Errorf("U+%04X: replayed inputs %s, want the input as the model wrote it", c, inputs)
			}
			if len(results) != 1 || !strings.Contains(results[0], "held control characters") {
				t.Errorf("U+%04X: tool results %q, want the fixed refusal", c, results)
			}
			if heldControl(reflect.ValueOf(out)) {
				t.Errorf("U+%04X, escaped %v: the outcome holds a control: %+v", c, escaped, out)
			}
			if escaped && (out.Reason != analysis.ReasonMissingData || out.Summary != "SX" ||
				len(out.MissingData) != 1 || out.MissingData[0] != "MY") {
				t.Errorf("U+%04X: reason %s, summary %q, missing data %q, want MissingData, SX and MY",
					c, out.Reason, out.Summary, out.MissingData)
			}
		}
	}
}

// A reply with a call that held a control and a clean one: only the clean one runs, and each gets its result.
func TestLoopRunsOnlyTheCleanToolCall(t *testing.T) {
	bad, good := `{"namespace":"sh\u001bop"}`, `{"namespace":"shop"}`
	ids := []string{callOne, "toolu_2"}
	out, bodies, lt := runLoopBodies(t, toolUse(ids, bad, good), toolUse([]string{"toolu_3"}, good))
	if len(lt.args) != 1 || lt.args[0] != good {
		t.Errorf("the tool ran with %q, want only the clean call", lt.args)
	}
	if out.Reason != analysis.ReasonStepLimit || out.ToolCalls != 2 || out.ToolCallsWithControls != 1 {
		t.Errorf("reason %s, tool calls %d, with controls %d, want StepLimit, 2 and 1", out.Reason, out.ToolCalls,
			out.ToolCallsWithControls)
	}
	if len(bodies) < 2 {
		t.Fatalf("%d requests, want 2", len(bodies))
	}
	if _, results := replayed(t, bodies[1]); len(results) != 2 || !strings.Contains(results[0], "held control characters") ||
		strings.Contains(results[1], "held control characters") {
		t.Errorf("tool results %q, want the first refused and the second answered", results)
	}
}
