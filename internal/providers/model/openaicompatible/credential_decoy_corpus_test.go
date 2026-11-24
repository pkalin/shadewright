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
	"net/http"
	"strings"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/providers/model/openaiwire"
)

const (
	decoyPlainArgs        = `{"a":1}`
	textCredential        = "the reply contains a credential"
	textWrongType         = "the reply has a field of the wrong type"
	decoyTool             = "tool"
	decoyCallID           = "call_1"
	decoyNumKey           = "sk-ABCDEF123456XYZ" // #nosec G101 -- synthetic marker split into a number
	decoyNumClean         = "sk-QRSTUV654321JKL" // #nosec G101 -- same shape, no shared split
	decoyInputTokens      = "input_tokens"
	decoyAssistant        = "assistant"
	decoyCallIDField      = "call_id"
	decoyCompletionTokens = "completion_tokens"
	decoyFinishReason     = "finish_reason"
	decoyOutputTokens     = "output_tokens"
	decoyRoleField        = "role"
	decoyPromptTokens     = "prompt_tokens"
)

// decoyShape places a key in the slots the typed scan joins: the message text (one or two parts) and the
// arguments of one tool call. Each shape is built for a key and for a clean key of the same form.
type decoyShape struct {
	name string
	key  string
	// parts returns the message text parts and the tool call arguments for key k.
	parts func(k string) ([]string, string)
}

func decoyShapes() []decoyShape {
	shapes := []decoyShape{{name: "whole", key: secretKey, parts: func(k string) ([]string, string) {
		return []string{"before " + k + " after"}, decoyPlainArgs
	}}}
	for _, cut := range []int{1, len(secretKey) / 2, len(secretKey) - 1} {
		shapes = append(shapes,
			decoyShape{name: fmt.Sprintf("split-content-%d", cut), key: secretKey, parts: func(k string) ([]string, string) {
				return []string{`{"summary":"` + k[:cut], k[cut:] + `"}`}, `{"s":"` + k[cut:] + `"}`
			}},
			decoyShape{name: fmt.Sprintf("split-argument-key-%d", cut), key: secretKey, parts: func(k string) ([]string, string) {
				return []string{k[:cut]}, `{"` + k[cut:] + `":1}`
			}})
	}
	shapes = append(shapes, decoyShape{name: "split-number", key: decoyNumKey, parts: func(k string) ([]string, string) {
		return []string{k[:9]}, `{"a":` + k[9:15] + `,"b":"` + k[15:] + `"}`
	}})
	return append(shapes, invisibleShapes()...)
}

// invisibleRunes are characters a terminal does not show and the kubectl plugin drops when it prints
// (Unicode Cc and Cf), plus whitespace. One inside the key may not hide it from the scan.
var invisibleRunes = []string{"\u200b", "\u00ad", "\x00", "\x7f", "\u0085", "\u2060", "\ufeff", "\u202e", "\u200d", "\U000e0001", "\u2028", " "}

// withInvisible inserts sep into k at cut.
func withInvisible(k, sep string, cut int) string { return k[:cut] + sep + k[cut:] }

// jsonText marshals v and returns the JSON text, so that a control character inside it stays valid JSON.
func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// invisibleShapes put an invisible character inside the key in each slot the scan compares: the message
// text, an argument value, an argument object key, and a key split across two text parts.
func invisibleShapes() []decoyShape {
	cuts := []int{1, len(secretKey) / 2, len(secretKey) - 1}
	shapes := make([]decoyShape, 0, 4*len(cuts)*len(invisibleRunes))
	for i, sep := range invisibleRunes {
		for _, cut := range cuts {
			shapes = append(shapes,
				decoyShape{name: fmt.Sprintf("invisible-%d-content-%d", i, cut), key: secretKey, parts: func(k string) ([]string, string) {
					return []string{"before " + withInvisible(k, sep, cut) + " after"}, decoyPlainArgs
				}},
				decoyShape{name: fmt.Sprintf("invisible-%d-argument-%d", i, cut), key: secretKey, parts: func(k string) ([]string, string) {
					return []string{"x"}, jsonText(map[string]string{"s": withInvisible(k, sep, cut)})
				}},
				decoyShape{name: fmt.Sprintf("invisible-%d-argument-key-%d", i, cut), key: secretKey, parts: func(k string) ([]string, string) {
					return []string{"x"}, jsonText(map[string]int{withInvisible(k, sep, cut): 1})
				}},
				decoyShape{name: fmt.Sprintf("invisible-%d-split-%d", i, cut), key: secretKey, parts: func(k string) ([]string, string) {
					return []string{k[:cut] + sep, k[cut:]}, decoyPlainArgs
				}})
		}
	}
	return shapes
}

// decoyReply builds a reply the caller accepts (with the tool offered) that carries the shape's text.
func decoyReply(api API, texts []string, args string) map[string]any {
	if api == APIResponses {
		parts := make([]any, 0, len(texts))
		for _, t := range texts {
			parts = append(parts, map[string]any{echoTypeField: echoOutputText, echoTextField: t})
		}
		return map[string]any{echoModelField: testModel, echoStatusField: echoCompleted,
			echoUsageField: map[string]any{decoyInputTokens: 30, decoyOutputTokens: 5},
			echoOutputField: []any{
				map[string]any{echoTypeField: echoMessageType, decoyRoleField: decoyAssistant, echoContentField: parts},
				map[string]any{echoTypeField: echoFunctionCallType, decoyCallIDField: decoyCallID, echoNameField: decoyTool, echoArgumentsField: args},
			}}
	}
	// Chat Completions has one content string: a second text part goes after the arguments' key.
	content := texts[0]
	for _, second := range texts[1:] {
		args = `{"s":"` + strings.TrimSuffix(second, `"}`) + `"}`
	}
	return map[string]any{echoModelField: testModel,
		echoUsageField: map[string]any{decoyPromptTokens: 30, decoyCompletionTokens: 5},
		echoChoicesField: []any{map[string]any{decoyFinishReason: "tool_calls", echoMessageType: map[string]any{
			echoContentField: content,
			echoToolCallsField: []any{map[string]any{"id": decoyCallID, echoTypeField: "function",
				echoFunctionField: map[string]any{echoNameField: decoyTool, echoArgumentsField: args}}},
		}}}}
}

// decoy changes one field of a reply. accepted says the caller's typed decode still accepts the reply.
type decoy struct {
	name     string
	accepted bool
	apply    func(map[string]any)
}

func wrongTypes(kind string) []any {
	if kind == "string" {
		return []any{7, true, map[string]any{}, []any{"x"}}
	}
	return []any{"x", 7, true}
}

// decoys gives each field of the typed scan a wrong type in turn, on an added sibling element so that the
// key-bearing element is unchanged (the top level field itself is replaced), and adds the other API's
// field with a wrong type, which the caller ignores.
func decoys(api API) []decoy {
	var out []decoy
	add := func(name string, accepted bool, apply func(map[string]any)) {
		out = append(out, decoy{name: name, accepted: accepted, apply: apply})
	}
	items := func(r map[string]any, field string) []any { return r[field].([]any) }
	if api == APIResponses {
		for i, v := range wrongTypes("array") {
			add(fmt.Sprintf("choices-%d", i), true, func(r map[string]any) { r[echoChoicesField] = v })
			add(fmt.Sprintf("output-%d", i), false, func(r map[string]any) { r[echoOutputField] = v })
			add(fmt.Sprintf("content-%d", i), false, func(r map[string]any) {
				r[echoOutputField] = append(items(r, echoOutputField), map[string]any{echoTypeField: echoMessageType, echoContentField: v})
			})
		}
		add("choices-objects", true, func(r map[string]any) {
			r[echoChoicesField] = []any{map[string]any{echoMessageType: map[string]any{echoContentField: 7}}}
		})
		for i, v := range wrongTypes("string") {
			for _, f := range []string{echoTypeField, echoArgumentsField} {
				add(fmt.Sprintf("item-%s-%d", f, i), false, func(r map[string]any) {
					r[echoOutputField] = append(items(r, echoOutputField), map[string]any{f: v})
				})
			}
			for _, f := range []string{echoTypeField, echoTextField} {
				add(fmt.Sprintf("part-%s-%d", f, i), false, func(r map[string]any) {
					msg := items(r, echoOutputField)[0].(map[string]any)
					msg[echoContentField] = append(msg[echoContentField].([]any), map[string]any{f: v})
				})
			}
		}
		return out
	}
	choice := func(r map[string]any) map[string]any {
		return items(r, echoChoicesField)[0].(map[string]any)[echoMessageType].(map[string]any)
	}
	for i, v := range wrongTypes("array") {
		add(fmt.Sprintf("output-%d", i), true, func(r map[string]any) { r[echoOutputField] = v })
		add(fmt.Sprintf("choices-%d", i), false, func(r map[string]any) { r[echoChoicesField] = v })
		add(fmt.Sprintf("tool_calls-%d", i), false, func(r map[string]any) {
			r[echoChoicesField] = append(items(r, echoChoicesField), map[string]any{echoMessageType: map[string]any{echoToolCallsField: v}})
		})
	}
	add("output-objects", true, func(r map[string]any) {
		r[echoOutputField] = []any{map[string]any{echoContentField: 7, echoArgumentsField: []any{}}}
	})
	for i, v := range []any{"x", 7, true, []any{}} {
		add(fmt.Sprintf("message-%d", i), false, func(r map[string]any) {
			r[echoChoicesField] = append(items(r, echoChoicesField), map[string]any{echoMessageType: v})
		})
		add(fmt.Sprintf("function-%d", i), false, func(r map[string]any) {
			m := choice(r)
			m[echoToolCallsField] = append(m[echoToolCallsField].([]any), map[string]any{echoFunctionField: v})
		})
	}
	for i, v := range wrongTypes("string") {
		add(fmt.Sprintf("content-%d", i), false, func(r map[string]any) {
			r[echoChoicesField] = append(items(r, echoChoicesField), map[string]any{echoMessageType: map[string]any{echoContentField: v}})
		})
		add(fmt.Sprintf("arguments-%d", i), false, func(r map[string]any) {
			m := choice(r)
			m[echoToolCallsField] = append(m[echoToolCallsField].([]any), map[string]any{echoFunctionField: map[string]any{echoArgumentsField: v}})
		})
	}
	return out
}

func parseLikeCaller(api API, data []byte) error {
	offered := map[string]bool{decoyTool: true}
	if api == APIResponses {
		_, err := openaiwire.ParseResponses(data, offered, false)
		return err
	}
	_, err := openaiwire.ParseResponse(data, offered, false)
	return err
}

// One decoy field of the wrong type may not switch off the joined scans. A decoy the caller's decode
// ignores leaves the reply scanned field by field; a decoy the caller's decode refuses makes the reply
// count as one with a credential, and the caller is shown to refuse the same reply.
func TestDecoyFieldCredentialCorpus(t *testing.T) {
	var data []byte
	providersByKey := map[string]*Provider{}
	provider := func(api API, key string) *Provider {
		id := string(api) + key
		if p := providersByKey[id]; p != nil {
			return p
		}
		p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel, API: api, KeyFile: keyFile(t, key)},
			testHooks{transport: roundTrip(func(*http.Request) (*http.Response, error) {
				return response(200, "application/json", string(data)), nil
			})})
		if err != nil {
			t.Fatal(err)
		}
		providersByKey[id] = p
		return p
	}
	cleanKey := func(k string) string {
		if k == decoyNumKey {
			return decoyNumClean
		}
		return strings.Repeat("q", len(k))
	}
	marshal := func(r map[string]any) []byte {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, api := range []API{APIChatCompletions, APIResponses} {
		for _, shape := range decoyShapes() {
			// The undecorated replies: the caller accepts the clean one, and the scan tells them apart.
			clean := marshal(buildDecoy(api, shape, cleanKey(shape.key)))
			if err := parseLikeCaller(api, clean); err != nil {
				t.Fatalf("%s %s: the clean base reply is refused: %v", api, shape.name, err)
			}
			for _, d := range decoys(api) {
				name := fmt.Sprintf("%s/%s/%s", api, shape.name, d.name)
				withKey := buildDecoy(api, shape, shape.key)
				d.apply(withKey)
				data = marshal(withKey)
				before := string(data)
				if !replyContainsKey(data, api, shape.key) {
					t.Fatalf("%s: the key was missed", name)
				}
				c, err := provider(api, shape.key).Complete(t.Context(), request())
				refused := err != nil && (strings.Contains(err.Error(), textCredential) || !d.accepted && strings.Contains(err.Error(), textWrongType))
				if !refused || !providers.IsMalformedOutput(err) || c.Content != "" || len(c.ToolCalls) != 0 {
					t.Fatalf("%s: the key was not refused: %v", name, err)
				}
				if string(data) != before {
					t.Fatalf("%s: the detector changed the reply", name)
				}

				cleanReply := buildDecoy(api, shape, cleanKey(shape.key))
				d.apply(cleanReply)
				cleanData := marshal(cleanReply)
				callerErr := parseLikeCaller(api, cleanData)
				if d.accepted {
					if callerErr != nil {
						t.Fatalf("%s: the caller refuses a decoy it should ignore: %v", name, callerErr)
					}
					if replyContainsKey(cleanData, api, shape.key) {
						t.Fatalf("%s: a clean reply matched", name)
					}
					continue
				}
				// Failing closed is allowed only where the caller's own decode refuses the reply.
				if callerErr == nil || !strings.Contains(callerErr.Error(), "the reply is not a") {
					t.Fatalf("%s: the caller's decode accepts a reply the scan fails closed on: %v", name, callerErr)
				}
				if !replyContainsKey(cleanData, api, shape.key) {
					t.Fatalf("%s: a reply the typed decode refuses did not fail closed", name)
				}
				// No key was seen, so the refusal does not say there was one.
				data = cleanData
				c, err = provider(api, shape.key).Complete(t.Context(), request())
				if err == nil || !strings.Contains(err.Error(), textWrongType) || strings.Contains(err.Error(), textCredential) ||
					!providers.IsMalformedOutput(err) || c.Content != "" {
					t.Fatalf("%s: a wrong-typed clean reply was not refused as one: %v", name, err)
				}
			}
		}
	}
}

// A reply that is not JSON is scanned as raw bytes.
func TestInvalidJSONReplyIsScannedRaw(t *testing.T) {
	for _, api := range []API{APIChatCompletions, APIResponses} {
		if !replyContainsKey([]byte(`{"output":"`+secretKey), api, secretKey) {
			t.Fatalf("%s: raw key missed", api)
		}
		if replyContainsKey([]byte(`{"output":"sk-FAKE`), api, secretKey) {
			t.Fatalf("%s: clean invalid reply matched", api)
		}
		for i, sep := range invisibleRunes {
			for cut := 1; cut < len(secretKey); cut++ {
				if !replyContainsKey([]byte(`{"output":"`+withInvisible(secretKey, sep, cut)), api, secretKey) {
					t.Fatalf("%s: raw key with invisible %d at %d missed", api, i, cut)
				}
				// A visible character in the same place leaves the text around it alone and matches nothing.
				if replyContainsKey([]byte(`{"output":"`+withInvisible(secretKey, "-", cut)), api, secretKey) {
					t.Fatalf("%s: raw key broken by a visible character at %d matched", api, cut)
				}
			}
		}
	}
}

func buildDecoy(api API, shape decoyShape, key string) map[string]any {
	texts, args := shape.parts(key)
	return decoyReply(api, texts, args)
}
