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
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

const (
	echoTypeField    = "type"
	echoStatusField  = "status"
	echoMessageType  = "message"
	echoModelField   = "model"
	echoTextField    = "text"
	echoOutputField  = "output"
	echoContentField = "content"
	echoOutputText   = "output_text"
	echoCompleted    = "completed"
	echoUsageField   = "usage"
)

// Every generated tree has a known answer. JSON escaping and adjacent marker text may not hide a key.
func TestCredentialEchoCorpus(t *testing.T) {
	r := rand.New(rand.NewPCG(834, 2026)) // #nosec G404 -- reproducible adversarial corpus
	for i := range 2000 {
		before := fmt.Sprintf("prefix-%d-", r.Uint64())
		after := fmt.Sprintf("-suffix-%d", r.Uint64())
		value := before + secretKey + after
		tree := map[string]any{"nested": []any{map[string]any{echoTextField: value}}, "other": "preserved"}
		if i%2 == 0 {
			tree = map[string]any{value: []any{"preserved"}}
		}
		data, err := json.Marshal(tree)
		if err != nil {
			t.Fatal(err)
		}
		// Escape every key byte. Neither the key's position nor the neighbouring text changes.
		var escaped strings.Builder
		for _, c := range secretKey {
			fmt.Fprintf(&escaped, `\u%04x`, c)
		}
		data = []byte(strings.ReplaceAll(string(data), secretKey, escaped.String()))
		original := string(data)
		if !containsKey(data, secretKey) {
			t.Fatalf("case %d: escaped credential was missed", i)
		}
		if string(data) != original {
			t.Fatalf("case %d: matcher changed its input", i)
		}
		clean := []byte(strings.ReplaceAll(string(data), escaped.String(), "safe-marker"))
		if containsKey(clean, secretKey) {
			t.Fatalf("case %d: credential-free tree matched", i)
		}
	}
}

// FuzzCredentialEcho checks valid JSON containing the credential next to arbitrary text and escapes.
func FuzzCredentialEcho(f *testing.F) {
	f.Add("quoted \"text\" and Unicode 漢字")
	f.Add("\n\t\x00")
	f.Fuzz(func(t *testing.T, neighbour string) {
		if len(neighbour) > 4096 {
			t.Skip()
		}
		data, err := json.Marshal([]any{neighbour + secretKey + neighbour})
		if err != nil {
			t.Fatal(err)
		}
		if !containsKey(data, secretKey) {
			t.Fatal("credential in a JSON value was missed")
		}
		text, err := json.Marshal(map[string]string{"summary": neighbour + secretKey + neighbour})
		if err != nil {
			t.Fatal(err)
		}
		cut := strings.Index(string(text), secretKey) + len(secretKey)/2
		body, err := json.Marshal(map[string]any{echoOutputField: []any{map[string]any{
			echoTypeField: echoMessageType, echoContentField: []any{
				map[string]any{echoTypeField: echoOutputText, echoTextField: string(text[:cut])},
				map[string]any{echoTypeField: echoOutputText, echoTextField: string(text[cut:])},
			},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		if !replyContainsKey(body, APIResponses, secretKey) {
			t.Fatal("credential split across text parts was missed")
		}
	})
}

// The key is split at every internal byte boundary. The old outer-JSON check misses every case.
func TestSplitCredentialCorpus(t *testing.T) {
	inner := `{"summary":"` + secretKey + `"}`
	base := strings.Index(inner, secretKey)
	var data []byte
	p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel, API: APIResponses,
		KeyFile: keyFile(t, secretKey), KeyHost: compatibleHost}, testHooks{transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return response(200, "application/json", string(data)), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	for cut := 1; cut < len(secretKey); cut++ {
		t.Run(fmt.Sprint(cut), func(t *testing.T) {
			reply := map[string]any{echoModelField: testModel, echoStatusField: echoCompleted,
				echoUsageField: map[string]int64{"input_tokens": 30, "output_tokens": 5},
				echoOutputField: []any{map[string]any{echoTypeField: echoMessageType, "role": "assistant", echoContentField: []any{
					map[string]any{echoTypeField: echoOutputText, echoTextField: inner[:base+cut]},
					map[string]any{echoTypeField: echoOutputText, echoTextField: inner[base+cut:]},
				}}}}
			data, err = json.Marshal(reply)
			if err != nil {
				t.Fatal(err)
			}
			if containsKey(data, secretKey) {
				t.Fatal("corpus case was already covered by the outer check")
			}
			before := string(data)
			c, err := p.Complete(t.Context(), request())
			if !providers.IsMalformedOutput(err) || c.Content != "" || c.Model != "" {
				t.Fatal("split key was returned")
			}
			if string(data) != before {
				t.Fatal("the detector modified the reply")
			}
		})
	}
}

// baselineReplyContainsKey retains the field-by-field matcher for differential coverage.
func baselineReplyContainsKey(data []byte, api API, key string) bool {
	var r struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Output []struct {
			Type      string `json:"type"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if json.Unmarshal(data, &r) != nil {
		return false
	}
	if api == APIResponses {
		var text strings.Builder
		for _, item := range r.Output {
			switch item.Type {
			case echoMessageType:
				for _, part := range item.Content {
					if part.Type == echoOutputText {
						text.WriteString(part.Text)
					}
				}
			case echoFunctionCallType:
				if containsKey([]byte(item.Arguments), key) {
					return true
				}
			}
		}
		return containsKey([]byte(text.String()), key)
	}
	for _, choice := range r.Choices {
		if containsKey([]byte(choice.Message.Content), key) {
			return true
		}
		for _, call := range choice.Message.ToolCalls {
			if containsKey([]byte(call.Function.Arguments), key) {
				return true
			}
		}
	}
	return false
}

// Generated neighbours and escaped keys preserve every match of the original matcher.
func checkJoinedReplyBaseline(t *testing.T) {
	for i := range 100 {
		var escaped strings.Builder
		for _, c := range secretKey {
			fmt.Fprintf(&escaped, `\u%04x`, c)
		}
		inner := fmt.Sprintf(`{"summary":"before-%d-%s-after","neighbour":"%s"}`, i, escaped.String(), escaped.String())
		for _, api := range []API{APIChatCompletions, APIResponses} {
			var body any
			if api == APIResponses {
				body = map[string]any{echoOutputField: []any{
					map[string]any{echoTypeField: echoMessageType, echoContentField: []any{map[string]string{echoTypeField: echoOutputText, echoTextField: inner}}},
					map[string]string{echoTypeField: echoFunctionCallType, echoArgumentsField: inner},
				}}
			} else {
				body = map[string]any{echoChoicesField: []any{map[string]any{echoMessageType: map[string]any{
					echoContentField: inner, echoToolCallsField: []any{map[string]any{echoFunctionField: map[string]string{echoArgumentsField: inner}}},
				}}}}
			}
			data, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			before := string(data)
			if !baselineReplyContainsKey(data, api, secretKey) {
				t.Fatal("corpus lacks a baseline match")
			}
			if !replyContainsKey(data, api, secretKey) {
				t.Fatal("lost a baseline match")
			}
			if string(data) != before {
				t.Fatal("matcher changed neighbouring text")
			}
		}
	}
}
