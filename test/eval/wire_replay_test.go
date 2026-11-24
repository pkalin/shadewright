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
	"fmt"
	"reflect"
	"testing"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/analysis"
	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/providers/model/openaiwire"
)

const (
	wireType    = "type"
	wireMessage = "message"
	wireName    = "name"
	wireStatus  = "status"
)

// wireReplaySource retains the recorded scenario bytes and sends completions through each shared wire parser.
type wireReplaySource struct {
	replaySource
	api openaiwire.API
}

func (w wireReplaySource) factory(s *scenario) (analysis.ModelFactory, func() error, error) {
	p := &wireReplay{replies: s.recording.Replies, api: w.api}
	done := func() error {
		if p.next != len(p.replies) {
			return fmt.Errorf("used %d of %d recorded replies", p.next, len(p.replies))
		}
		return nil
	}
	return func(analysis.Role, remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
		return p, nil
	}, done, nil
}

type wireReplay struct {
	replies []recordedReply
	next    int
	api     openaiwire.API
}

func (w *wireReplay) Complete(_ context.Context, req providers.CompletionRequest) (providers.Completion, error) {
	codec := openaiwire.Codec{API: w.api, Deployment: "recorded-model", SendModel: true, MaxOutput: 2048}
	_, offered, err := codec.Encode(req)
	if err != nil {
		return providers.Completion{}, err
	}
	if w.next >= len(w.replies) {
		return providers.Completion{}, fmt.Errorf("recording exhausted")
	}
	c := w.replies[w.next].completion()
	w.next++
	var body any
	if w.api == openaiwire.APIResponses {
		output := []any{}
		if c.Content != "" {
			output = append(output, map[string]any{
				wireType: wireMessage, "role": "assistant",
				"content": []any{map[string]any{wireType: "output_text", "text": c.Content}},
			})
		}
		for _, tc := range c.ToolCalls {
			output = append(output, map[string]any{
				wireType: "function_call", "call_id": tc.ID, wireName: tc.Name, "arguments": string(tc.Arguments),
			})
		}
		body = map[string]any{"model": c.Model, wireStatus: "completed", "output": output,
			"usage": map[string]any{
				"input_tokens": c.Usage.InputTokens, "output_tokens": c.Usage.OutputTokens,
				"input_tokens_details": map[string]any{"cached_tokens": c.Usage.CachedInputTokens},
			},
		}
	} else {
		calls := []any{}
		finish := "stop"
		for _, tc := range c.ToolCalls {
			finish = "tool_calls"
			calls = append(calls, map[string]any{
				"id": tc.ID, wireType: "function",
				"function": map[string]any{wireName: tc.Name, "arguments": string(tc.Arguments)},
			})
		}
		body = map[string]any{
			"model": c.Model,
			"choices": []any{map[string]any{
				"finish_reason": finish, wireMessage: map[string]any{"content": c.Content, "tool_calls": calls},
			}},
			"usage": map[string]any{
				"prompt_tokens": c.Usage.InputTokens, "completion_tokens": c.Usage.OutputTokens,
				"prompt_tokens_details": map[string]any{"cached_tokens": c.Usage.CachedInputTokens},
			},
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return providers.Completion{}, err
	}
	var got providers.Completion
	if w.api == openaiwire.APIResponses {
		got, err = openaiwire.ParseResponses(raw, offered, len(req.ResponseSchema) > 0)
	} else {
		got, err = openaiwire.ParseResponse(raw, offered, len(req.ResponseSchema) > 0)
	}
	if err == nil && !reflect.DeepEqual(got, c) {
		return providers.Completion{}, fmt.Errorf("wire parser changed a recorded completion")
	}
	return got, err
}

func TestRecordedScenariosThroughSharedWire(t *testing.T) {
	for _, api := range []openaiwire.API{openaiwire.APIChatCompletions, openaiwire.APIResponses} {
		t.Run(string(api), func(t *testing.T) {
			r := evaluate(t, "fake", wireReplaySource{api: api}, 1, 1, nil)
			if !r.Totals.Pass {
				t.Fatalf("%d of %d recorded scenarios passed", r.Totals.Passed, r.Totals.Runs)
			}
		})
	}
}
