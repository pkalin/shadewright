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
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/pkalin/shadewright/internal/providers"
)

// thinkingRun scores a record whose judge first asks one cross-check question with blocks as its provider blocks,
// then answers. It returns the score and the fixture.
func thinkingRun(t *testing.T, seed uint64, blocks string) (Score, *fixture) {
	t.Helper()
	f := newFixture(t)
	rec := recordOf(t, rngOf(61, seed), gptID)
	first := toolReply(askCall("a", "Why?"))
	first.ProviderBlocks = providers.NewOpaque([]byte(blocks))
	f.anthropic.replies = []providers.Completion{first, answerReply(claudeID)}
	f.openai.replies = []providers.Completion{{Content: plainAnswer}}
	s, err := f.scorer.Score(context.Background(), sourceOf(t, rec))
	if err != nil {
		t.Fatal(err)
	}
	return s, f
}

const thinkingCall = `{"type":"tool_use","id":"a","name":"cross_check","input":{"question":"Why?"}}`

// A thinking block whose text the redactor would change is not sent back: the judge's second request is not
// sent and the record is quarantined with the rule's kind and KindThinkingBlock (decision of 2026-11-21).
func TestThinkingTextTheRedactorWouldChangeIsNotSent(t *testing.T) {
	const blocks = `[{"type":"thinking","thinking":"password=hunter2","signature":"s"},` + thinkingCall + `]`
	f := newFixture(t)
	if f.scorer.Redactor.String("password=hunter2") == "password=hunter2" {
		t.Fatal("the redactor does not change the thinking text; the example is moot")
	}
	s, f := thinkingRun(t, 900, blocks)
	if f.anthropic.calls() != 1 {
		for _, m := range f.anthropic.reqs[1].Messages {
			if strings.Contains(string(m.ProviderBlocks.Bytes()), "hunter2") {
				t.Fatalf("status %s: the judge's second request carries %s", s.Status, m.ProviderBlocks.Bytes())
			}
		}
		t.Fatalf("status %s, %d judge calls: want the second request not sent", s.Status, f.anthropic.calls())
	}
	if s.Status != StatusQuarantined || s.Reason != reasonSecondUnsent || !slices.Contains(s.Kinds, KindThinkingBlock) {
		t.Fatalf("score %+v; want quarantined at the second request with kind %s", s, KindThinkingBlock)
	}
}

// Thinking blocks the redactor leaves unchanged, a signature and a redacted thinking block go back in the judge's
// second request byte for byte.
func TestCleanThinkingIsSentByteForByte(t *testing.T) {
	const blocks = `[{"type":"thinking","thinking":"The limit is 128Mi and the pod used more.","signature":"c2lnbmF0dXJl"},` +
		`{"type":"redacted_thinking","data":"password=hunter2 opaque"},` + thinkingCall + `]`
	s, f := thinkingRun(t, 901, blocks)
	if s.Status != StatusScored || f.anthropic.calls() != 2 {
		t.Fatalf("score %+v after %d judge calls; want scored after two", s, f.anthropic.calls())
	}
	for _, m := range f.anthropic.reqs[1].Messages {
		if m.Role == providers.RoleAssistant {
			if !bytes.Equal(m.ProviderBlocks.Bytes(), []byte(blocks)) {
				t.Fatalf("blocks sent back %s, want %s", m.ProviderBlocks.Bytes(), blocks)
			}
			return
		}
	}
	t.Fatal("the judge's reply was not sent back")
}

// A thinking block the check cannot read is not skipped: the record is quarantined with KindUnreadableBlock.
func TestUnreadableThinkingBlockIsQuarantined(t *testing.T) {
	cases := map[string]string{
		"no text":        `{"type":"thinking","signature":"s"}`,
		"text not a str": `{"type":"thinking","thinking":7,"signature":"s"}`,
		"null text":      `{"type":"thinking","thinking":null,"signature":"s"}`,
		"other member":   `{"type":"thinking","thinking":"ok","signature":"s","extra":"password=hunter2"}`,
		"a key twice":    `{"type":"thinking","thinking":"ok","thinking":"password=hunter2","signature":"s"}`,
	}
	i := uint64(0)
	for name, block := range cases {
		i++
		t.Run(name, func(t *testing.T) {
			s, f := thinkingRun(t, 910+i, `[`+block+`,`+thinkingCall+`]`)
			if s.Status != StatusQuarantined || f.anthropic.calls() != 1 || !slices.Contains(s.Kinds, KindUnreadableBlock) {
				t.Fatalf("score %+v after %d judge calls; want quarantined with %s", s, f.anthropic.calls(),
					KindUnreadableBlock)
			}
		})
	}
}

// Known limit, not caught: each thinking text is checked alone, so a value split over two thinking blocks, each
// clean and the two joined not, goes back in the second request. Thinking texts are not joined because the
// provider does not join them either; the case is closed by #1093, where the record's values never leave. This
// test states the limit and fails when it is closed, so the reference is updated with it.
func TestThinkingSplitOverTwoBlocksIsNotCaught(t *testing.T) {
	const blocks = `[{"type":"thinking","thinking":"postgres://admin:","signature":"s"},` +
		`{"type":"thinking","thinking":"Pa55w0rdLong@db:5432/x","signature":"s"},` + thinkingCall + `]`
	f := newFixture(t)
	r := f.scorer.Redactor
	if !r.Clean("postgres://admin:") || !r.Clean("Pa55w0rdLong@db:5432/x") || r.Clean("postgres://admin:Pa55w0rdLong@db:5432/x") {
		t.Fatal("the halves must be clean and the joined value not; the example is moot")
	}
	s, f := thinkingRun(t, 920, blocks)
	if s.Status != StatusScored || f.anthropic.calls() != 2 {
		t.Fatalf("score %+v after %d judge calls: the limit is closed, update docs/reference/intake.md", s,
			f.anthropic.calls())
	}
}
