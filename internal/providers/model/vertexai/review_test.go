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

package vertexai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/providers/model/openaiwire"
)

// In-cluster ranges follow the rule of the other adapters: an IPv4 address that arrives in an IPv6 form is
// matched after unmapping, so a range written in that form could never match and is refused, as is a range
// that covers everything.
func TestNewRefusesUnsafeInClusterRanges(t *testing.T) {
	for _, r := range []string{"::ffff:11.0.0.0/104", "64:ff9b::11.0.0.0/120", "2002:b00::/24", "0.0.0.0/0", "::/0"} {
		_, err := New(Config{Endpoint: testEndpoint, Model: testModel, Tokens: &fakeTokens{tok: tokenMark},
			InClusterRanges: []netip.Prefix{netip.MustParsePrefix(r)}})
		if !providers.IsInvalidConfig(err) {
			t.Errorf("%s: err = %v, want invalid config", r, err)
		}
	}
	_, err := New(Config{Endpoint: testEndpoint, Model: testModel, Tokens: &fakeTokens{tok: tokenMark},
		InClusterRanges: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}})
	if err != nil {
		t.Errorf("a plain range is refused: %v", err)
	}
}

// cancelBody cancels the call while its body is read, then serves the reply whole.
type cancelBody struct {
	r      io.Reader
	cancel context.CancelFunc
}

func (b *cancelBody) Read(p []byte) (int, error) {
	b.cancel()
	return b.r.Read(p)
}
func (b *cancelBody) Close() error { return nil }

// A cancellation that comes while the body is read ends the call with the cancellation, never with the reply.
func TestCancelWhileReadingBody(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		ctx, cancel := context.WithCancel(t.Context())
		body := replyJSON(`[{"text":"{\"cause\":\"x\"}"}]`)
		p := newProviderWith(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
			_, _ = io.Copy(io.Discard, r.Body)
			return &http.Response{StatusCode: status, Header: jsonHeader(),
				Body: &cancelBody{r: strings.NewReader(body), cancel: cancel}, Request: r}, nil
		}), nil)
		_, err := p.Complete(ctx, structuredRequest())
		if !errors.Is(err, context.Canceled) {
			t.Errorf("status %d: err = %v, want the cancellation", status, err)
		}
	}
}

// A call whose dial is refused by the address guard never reached the endpoint: it is invalid configuration,
// not sent, and not retried.
func TestRefusedDialIsNotSent(t *testing.T) {
	var calls atomic.Int32
	p := newProviderWith(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, fmt.Errorf("dial: %w", openaiwire.ErrDialRefused)
	}), func(c *Config) { c.MaxAttempts = 3; c.BaseDelay = time.Millisecond })
	_, err := p.Complete(t.Context(), structuredRequest())
	if !providers.IsInvalidConfig(err) || !providers.IsNotSent(err) || providers.IsRetryable(err) || calls.Load() != 1 {
		t.Errorf("err = %v, not sent %v, %d attempts", err, providers.IsNotSent(err), calls.Load())
	}
}

// A STOP reply that reports more output than the call's bound allowed is malformed, and its usage is still
// returned so that it is charged.
func TestUsageAboveTheBoundIsMalformed(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"cause\":\"x\"}"}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":4000,"thoughtsTokenCount":3000,"totalTokenCount":7100},` +
		`"modelVersion":"` + testModel + `"}`
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { reply(w, body) }, nil)
	req := structuredRequest()
	req.MaxOutputTokens = 1024 // the bound is 1024 + the default thinking allowance of 4096
	c, err := h.p.Complete(t.Context(), req)
	if !providers.IsMalformedOutput(err) || c.Usage.OutputTokens != 7000 {
		t.Errorf("err = %v, usage %+v", err, c.Usage)
	}
	// Within the bound, the same reply is accepted.
	req.MaxOutputTokens = 4000
	if _, err := h.p.Complete(t.Context(), req); err != nil {
		t.Errorf("7000 output tokens within a bound of 8096: %v", err)
	}
}

// Only user-side messages merge: two assistant messages in a row are refused, so text is never added to a
// replayed model turn.
func TestConsecutiveAssistantMessagesAreRefused(t *testing.T) {
	p := testProvider(t)
	c, err := parseResponse([]byte(replyJSON("["+callPart("", listEvents, `{}`)+"]")), testModel, offeredTools, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = p.encode(providers.CompletionRequest{Tools: toolDefs(), Messages: []providers.Message{
		{Role: providers.RoleUser, Content: "q"},
		{Role: providers.RoleAssistant, Content: "x"},
		{Role: providers.RoleAssistant, ToolCalls: c.ToolCalls, ProviderBlocks: c.ProviderBlocks},
		{Role: providers.RoleTool, ToolCallID: c.ToolCalls[0].ID, Content: "r"},
		{Role: providers.RoleUser, Content: "q2"},
	}})
	if !providers.IsRejected(err) {
		t.Errorf("err = %v, want rejected", err)
	}
}

// An empty ID on the wire is an ID that fails the pattern, not an absent one.
func TestEmptyWireIDIsMalformed(t *testing.T) {
	body := replyJSON(`[{"functionCall":{"id":"","name":"list_events","args":{}}}]`)
	if _, err := parseResponse([]byte(body), testModel, offeredTools, false); !providers.IsMalformedOutput(err) {
		t.Errorf("err = %v, want malformed output", err)
	}
}

// A reply with text before a function call replays: the text the caller keeps in Content is the text of the
// blocks, and anything else is refused.
func TestReplayWithTextBeforeCall(t *testing.T) {
	p := testProvider(t)
	body := replyJSON(`[{"text":"checking"},` + callPart("", listEvents, `{}`) + `]`)
	c, err := parseResponse([]byte(body), testModel, offeredTools, false)
	if err != nil || c.Content != "checking" {
		t.Fatalf("parse: %v, %q", err, c.Content)
	}
	build := func(content string) error {
		_, _, err := p.encode(providers.CompletionRequest{Tools: toolDefs(), Messages: []providers.Message{
			{Role: providers.RoleUser, Content: "q"},
			{Role: providers.RoleAssistant, Content: content, ToolCalls: c.ToolCalls, ProviderBlocks: c.ProviderBlocks},
			{Role: providers.RoleTool, ToolCallID: c.ToolCalls[0].ID, Content: "r"},
		}})
		return err
	}
	if err := build(c.Content); err != nil {
		t.Errorf("the replay of its own completion is refused: %v", err)
	}
	if err := build("other"); !providers.IsRejected(err) {
		t.Errorf("a changed content: err = %v, want rejected", err)
	}
	if err := build(""); !providers.IsRejected(err) {
		t.Errorf("a dropped content: err = %v, want rejected", err)
	}
}

// jsonHeader is the header of a reply whose body is JSON.
func jsonHeader() http.Header {
	h := http.Header{}
	h.Set("Content-Type", mimeJSON)
	return h
}
