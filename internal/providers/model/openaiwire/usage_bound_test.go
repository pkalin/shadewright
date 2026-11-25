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

package openaiwire

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/shadewright-labs/shadewright/internal/providers"
)

// A reply whose usage counts pass providers.MaxUsageTokens is refused as malformed in both APIs, and none of its
// counts reaches the Completion: the call is UsageUnknown, so the caller charges its ceiling. Counts at the bound
// are accepted, so input plus output never overflows.
func TestUsageCountsAboveTheBoundAreRefused(t *testing.T) {
	const big = int64(math.MaxInt64)
	chat := func(in, cached, out int64) string {
		return fmt.Sprintf(`{"model":%q,"choices":[{"finish_reason":"stop","message":{"content":"ok"}}],`+
			`"usage":{"prompt_tokens":%d,"completion_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d}}}`,
			replyModel, in, out, cached)
	}
	responses := func(in, cached, out int64) string {
		return fmt.Sprintf(`{"model":%q,"status":"completed","output":[{"type":"message","role":"assistant",`+
			`"content":[{"type":"output_text","text":"ok"}]}],`+
			`"usage":{"input_tokens":%d,"output_tokens":%d,"input_tokens_details":{"cached_tokens":%d}}}`,
			replyModel, in, out, cached)
	}
	const chatAPI, responsesAPI = "chat", "responses"
	over := providers.MaxUsageTokens + 1
	for _, tc := range []struct{ in, cached, out int64 }{
		{big, 0, 1}, {1, 0, big}, {over, 0, 0}, {0, 0, over}, {over, over, 0},
	} {
		for name, data := range map[string]string{
			chatAPI: chat(tc.in, tc.cached, tc.out), responsesAPI: responses(tc.in, tc.cached, tc.out),
		} {
			parse := parseResponse
			billed := billedUsage
			if name == responsesAPI {
				parse, billed = parseResponses, billedResponsesUsage
			}
			c, err := parse([]byte(data), nil, false)
			if !errors.Is(err, providers.ErrMalformedOutput) {
				t.Errorf("%s %+v: err = %v, want malformed output", name, tc, err)
			}
			if !c.UsageUnknown || c.Usage != (providers.Usage{}) || c.Model != "" {
				t.Errorf("%s %+v: completion = %+v, want UsageUnknown with no counts and no model", name, tc, c)
			}
			if b := billed([]byte(data)); !b.UsageUnknown || b.Usage != (providers.Usage{}) {
				t.Errorf("%s %+v: billed usage = %+v, want UsageUnknown with no counts", name, tc, b)
			}
		}
	}
	m := providers.MaxUsageTokens
	for name, data := range map[string]string{chatAPI: chat(m, m, m), responsesAPI: responses(m, m, m)} {
		parse := parseResponse
		if name == responsesAPI {
			parse = parseResponses
		}
		c, err := parse([]byte(data), nil, false)
		if err != nil || c.UsageUnknown || c.Usage.InputTokens != m || c.Usage.OutputTokens != m {
			t.Errorf("%s at the bound: completion = %+v, err = %v, want the counts accepted", name, c, err)
		}
	}
}
