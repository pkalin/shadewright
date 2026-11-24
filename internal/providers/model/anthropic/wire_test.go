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
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/providers"
)

// The bounds the API refuses are moved into descriptions at every schema position, and only there: a
// property named like a keyword stays.
func TestOutputSchemaRemovesUnsupportedBounds(t *testing.T) {
	in := `{"type":"object","additionalProperties":false,"properties":{
		"maximum":{"type":"integer","minimum":1,"maximum":1000,"description":"Replicas."},
		"list":{"type":"array","maxItems":5,"minItems":1,"items":{"type":"string","maxLength":9,"pattern":"^[a-z]+$"}},
		"many":{"type":"array","minItems":2,"items":{"anyOf":[{"type":"number","exclusiveMinimum":0},{"type":"null"}]}}},
		"required":["maximum"]}`
	out, err := outputSchema(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatal(err)
	}
	m := s.Properties["maximum"]
	if m == nil || m["minimum"] != nil || m["maximum"] != nil || m["description"] != "Replicas. Bounds: maximum 1000, minimum 1." {
		t.Errorf("maximum property = %v", m)
	}
	l := s.Properties["list"]
	items, _ := l["items"].(map[string]any)
	if l["maxItems"] != nil || l["minItems"] != float64(1) || items["maxLength"] != nil || items["pattern"] != "^[a-z]+$" {
		t.Errorf("list property = %v", l)
	}
	if strings.Contains(string(out), `"exclusiveMinimum":`) || strings.Contains(string(out), `"minItems":2`) {
		t.Errorf("nested bounds kept: %s", out)
	}
	again, _ := outputSchema(json.RawMessage(in))
	if string(again) != string(out) {
		t.Error("the rewritten schema is not deterministic")
	}
}

func TestUsageMapping(t *testing.T) {
	u, ok := usageOf(&apiUsage{InputTokens: 5, CacheCreationInputTokens: 900, CacheReadInputTokens: 3000, OutputTokens: 40})
	if !ok || u != (providers.Usage{InputTokens: 3905, CachedInputTokens: 3000, OutputTokens: 40}) {
		t.Errorf("usage = %+v, %v", u, ok)
	}
	for _, bad := range []*apiUsage{nil, {InputTokens: -1}, {CacheReadInputTokens: maxUsageTokens + 1}} {
		if _, ok := usageOf(bad); ok {
			t.Errorf("usageOf(%+v) accepted", bad)
		}
	}
}

// FuzzParseResponse feeds arbitrary bodies to the reply parser: it never panics, a success always names the
// configured model and has text or a tool call of an offered tool with object input, and an error never
// quotes the body.
func FuzzParseResponse(f *testing.F) {
	f.Add([]byte(messageJSON(testModel, stopEndTurn, `[{"type":"text","text":"{\"a\":1}"}]`)), true)
	f.Add([]byte(messageJSON(testModel, stopToolUse, `[{"type":"tool_use","id":"t1","name":"t","input":{}}]`)), false)
	f.Add([]byte(messageJSON("other", stopEndTurn, `[{"type":"text","text":"x"}]`)), false)
	f.Add([]byte(`{"model":"m","content":[],"usage":{}}`), false)
	f.Add([]byte(`not json`), true)
	f.Fuzz(func(t *testing.T, data []byte, structured bool) {
		c, err := parseResponse(data, testModel, map[string]bool{"t": true}, structured)
		if err != nil {
			if !providers.IsMalformedOutput(err) {
				t.Fatalf("error of another class: %v", err)
			}
			if c.Content != "" || c.ToolCalls != nil {
				t.Fatal("content returned with an error")
			}
			if len(data) > 8 && strings.Contains(err.Error(), string(data)) {
				t.Fatal("error quotes the body")
			}
			return
		}
		if c.Model != testModel {
			t.Fatalf("success from model %q", c.Model)
		}
		if c.Content == "" && len(c.ToolCalls) == 0 {
			t.Fatal("success without content or tool call")
		}
		if structured && len(c.ToolCalls) == 0 && !isJSONObject([]byte(c.Content)) {
			t.Fatal("structured success that is not a JSON object")
		}
		for _, tc := range c.ToolCalls {
			if tc.Name != "t" || !isJSONObject(tc.Arguments) || !toolCallIDPattern.MatchString(tc.ID) {
				t.Fatalf("unchecked tool call %+v", tc)
			}
		}
		if c.Usage.InputTokens < 0 || c.Usage.OutputTokens < 0 || c.Usage.CachedInputTokens > c.Usage.InputTokens {
			t.Fatalf("usage out of range %+v", c.Usage)
		}
	})
}
