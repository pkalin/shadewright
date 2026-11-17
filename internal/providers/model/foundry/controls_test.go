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

package foundry

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
// escaped, else raw (which JSON refuses for C0: the reply is then cleaned into a valid one, or refused).
func controlResult(c rune, escaped bool) string {
	s := string(c)
	if escaped {
		s = fmt.Sprintf(`\u%04x`, c)
	}
	return `{"summary":"S` + s + `X","hypotheses":[],"evidence":[],"missingData":["M` + s + `Y"],` +
		`"uncertainty":"","proposal":null}`
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Every control code point, escaped and raw, in a tool call's arguments and in the result, through both
// inference APIs of this adapter and the analysis loop: the tool gets the arguments without it, and nothing
// of the outcome holds one (#852).
func TestLoopReadsModelTextWithoutControls(t *testing.T) {
	apis := map[string]struct {
		mod        func(*Config)
		call, text func(string) string
	}{
		"chat completions": {nil,
			func(args string) string { return toolCallReply("c1", tools.NameListEvents, args) },
			func(s string) string {
				return completionJSON("stop", `{"role":"assistant","content":`+jsonString(s)+`,"refusal":null}`)
			}},
		"responses": {useResponses,
			func(args string) string { return responseJSON(callItem("c1", tools.NameListEvents, args)) },
			func(s string) string { return responseJSON(textItem(s)) }},
	}
	for name, api := range apis {
		for _, c := range removedControls() {
			for _, escaped := range []bool{true, false} {
				args := fmt.Sprintf(`{"namespace":"sh\u%04xop"}`, c)
				out, _, lt := runLoopWith(t, api.mod, 2, api.call(args), api.text(controlResult(c, escaped)))
				if len(lt.args) != 1 || lt.args[0] != `{"namespace":"shop"}` {
					t.Errorf("%s, U+%04X: the tool got %q, want the namespace without the control", name, c, lt.args)
				}
				if heldControl(reflect.ValueOf(out)) {
					t.Errorf("%s, U+%04X, escaped %v: the outcome holds a control: %+v", name, c, escaped, out)
				}
				if escaped && (out.Reason != analysis.ReasonMissingData || out.Summary != "SX" ||
					len(out.MissingData) != 1 || out.MissingData[0] != "MY") {
					t.Errorf("%s, U+%04X: reason %s, summary %q, missing data %q, want MissingData, SX and MY",
						name, c, out.Reason, out.Summary, out.MissingData)
				}
			}
		}
	}
}
