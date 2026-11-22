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

package intake

import (
	"context"
	"slices"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

// plainAnswer is a cross-check answer the redactor leaves unchanged.
const plainAnswer = "Because."

// The judge's reply goes back in its provider blocks, and the check of the second request reads the text and
// tool calls in them: what the blocks send is what is checked (decision of 2026-11-21).
func TestJudgeReplyBlocksAreCheckedAsSent(t *testing.T) {
	const call = `{"type":"tool_use","id":"a","name":"cross_check","input":{"question":"Why?"}}`
	cases := []struct {
		name   string
		blocks string
		want   Status
		kind   string
	}{
		{"text a redactor would change", `[{"type":"text","text":"token=` + fakeToken + `"},` + call + `]`,
			StatusQuarantined, ""},
		{"a connection string split over two text blocks", `[{"type":"text","text":"postgres://admin:"},` +
			`{"type":"text","text":"Pa55w0rdLong@db:5432/x"},` + call + `]`, StatusQuarantined, ""},
		{"a tool call input other than the tool call's", `[{"type":"tool_use","id":"a","name":"cross_check",` +
			`"input":{"question":"Why?","x":"--client-secret abcdefgh12345678"}}]`, StatusQuarantined, ""},
		{"a block of another type", `[{"type":"server_tool_use","id":"s","name":"web_search","input":{}},` + call + `]`,
			StatusQuarantined, KindUnreadableBlock},
		{"a key twice", `[{"type":"text","text":"ok","text":"x"},` + call + `]`, StatusQuarantined, KindUnreadableBlock},
		{"not a list", `{"type":"text","text":"ok"}`, StatusQuarantined, KindUnreadableBlock},
		{"thinking is read", `[{"type":"thinking","thinking":"token=` + fakeToken + `","signature":"s"},` +
			`{"type":"text","text":"Checking the limit."},` + call + `]`, StatusQuarantined, KindThinkingBlock},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			rec := recordOf(t, rngOf(61, uint64(62+i)), gptID) // #nosec G115 -- a small index
			first := toolReply(askCall("a", "Why?"))
			first.Content = "Checking the limit."
			first.ProviderBlocks = providers.NewOpaque([]byte(c.blocks))
			f.anthropic.replies = []providers.Completion{first, answerReply(claudeID)}
			f.openai.replies = []providers.Completion{{Content: plainAnswer}}
			s, err := f.scorer.Score(context.Background(), sourceOf(t, rec))
			if err != nil {
				t.Fatal(err)
			}
			if s.Status != c.want {
				t.Fatalf("status %q (%s), %d judge calls; want %q", s.Status, s.Reason, f.anthropic.calls(), c.want)
			}
			if c.want == StatusQuarantined {
				if f.anthropic.calls() != 1 || s.Reason != reasonSecondUnsent ||
					c.kind != "" && !slices.Contains(s.Kinds, c.kind) {
					t.Fatalf("score %+v after %d judge calls; want the second request not sent", s, f.anthropic.calls())
				}
				return
			}
			for _, m := range f.anthropic.reqs[1].Messages {
				if m.Role == providers.RoleAssistant && (m.Content != "" || m.ProviderBlocks.IsZero()) {
					t.Fatalf("the judge's reply went back with text %q and blocks %v; want the blocks alone",
						m.Content, m.ProviderBlocks)
				}
			}
		})
	}
}
