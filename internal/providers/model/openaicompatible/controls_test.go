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

package openaicompatible

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

// Every control code point, escaped and raw, in a tool call's arguments and in the result, through this adapter
// and the analysis loop: the tool call is refused, never run with the cleaned value, its result sent back is the
// fixed refusal, it uses its step, and nothing of the outcome holds a control (#852, threat model G4.15).
func TestLoopReadsModelTextWithoutControls(t *testing.T) {
	text := func(s string) string {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return completionJSON("stop", `{"role":"assistant","content":`+string(b)+`,"refusal":null}`)
	}
	for _, c := range removedControls() {
		for _, escaped := range []bool{true, false} {
			args := fmt.Sprintf(`{"namespace":"sh\u%04xop"}`, c)
			out, sent, lt := runLoop(t, 2, toolCallReply("c1", tools.NameListEvents, args), text(controlResult(c, escaped)))
			if len(lt.calls) != 0 {
				t.Errorf("U+%04X: the tool ran %d times, want the call refused", c, len(lt.calls))
			}
			if out.ToolCalls != 1 || out.ToolCallsWithControls != 1 {
				t.Errorf("U+%04X: tool calls %d, with controls %d, want 1 and 1", c, out.ToolCalls, out.ToolCallsWithControls)
			}
			if len(sent) != 2 || !strings.Contains(sent[1].lastContent, "held control characters") {
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
