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
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shadewright-labs/shadewright/internal/providers"
)

// Model IDs other than testModel.
const (
	modelPro  = "gemini-2.5-pro"
	modelLite = "gemini-2.5-flash-lite"
)

// A reply that echoes the bearer token as its model version never puts the token in the call report or the
// completion.
func TestEchoedTokenInModelVersionIsNotReported(t *testing.T) {
	body := strings.Replace(replyJSON(`[{"text":"{}"}]`), `"modelVersion":"`+testModel+`"`, `"modelVersion":"`+tokenMark+`"`, 1)
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { reply(w, body) }, nil)
	c, _ := h.p.Complete(t.Context(), structuredRequest())
	r := h.lastReport(t)
	if strings.Contains(r.Model, tokenMark) || strings.Contains(c.Model, tokenMark) {
		t.Errorf("the token is in the report model %q or the completion model %q", r.Model, c.Model)
	}
	if c.Usage != wantUsage {
		t.Errorf("usage %+v, want %+v", c.Usage, wantUsage)
	}
}

// A reply above the output bound keeps its usage and model, and exposes no content, call or replay block.
func TestOverBoundReplyExposesNoContent(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"checking"},` + callPart("", listEvents, `{}`) +
		`]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":4000,"thoughtsTokenCount":3000,"totalTokenCount":7100},` +
		`"modelVersion":"` + testModel + `"}`
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { reply(w, body) }, nil)
	req := providers.CompletionRequest{Tools: toolDefs(), MaxOutputTokens: 1024,
		Messages: []providers.Message{{Role: providers.RoleUser, Content: "q"}}}
	c, err := h.p.Complete(t.Context(), req)
	if !providers.IsMalformedOutput(err) || c.Usage.OutputTokens != 7000 || c.Model != testModel {
		t.Fatalf("err = %v, usage %+v, model %q", err, c.Usage, c.Model)
	}
	if c.Content != "" || len(c.ToolCalls) != 0 || !c.ProviderBlocks.IsZero() {
		t.Errorf("the rejected reply exposes content %q, %d calls, blocks zero %v", c.Content, len(c.ToolCalls),
			c.ProviderBlocks.IsZero())
	}
}

// A thought signature is a bytes field: one that is not base64 makes the reply malformed before any tool runs,
// and its usage stays.
func TestInvalidThoughtSignatureIsMalformed(t *testing.T) {
	for _, sig := range []string{"not base64!!", "c2ln*", "a"} {
		part := `{"functionCall":{"name":"` + listEvents + `","args":{}},"thoughtSignature":"` + sig + `"}`
		h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { reply(w, replyJSON("["+part+"]")) }, nil)
		req := providers.CompletionRequest{Tools: toolDefs(), Messages: []providers.Message{{Role: providers.RoleUser, Content: "q"}}}
		c, err := h.p.Complete(t.Context(), req)
		if !providers.IsMalformedOutput(err) || c.Usage != wantUsage || c.UsageUnknown || len(c.ToolCalls) != 0 {
			t.Errorf("signature %q: err = %v, completion %+v", sig, err, c)
		}
	}
}

// The thinking budget must be one the model accepts: Pro cannot turn thinking off, Flash and Flash-Lite stop
// at 24576.
func TestThinkingAllowanceFitsTheModel(t *testing.T) {
	n := func(v int) *int { return &v }
	for _, tc := range []struct {
		model string
		allow *int
		ok    bool
	}{
		{modelPro, n(0), false},
		{modelPro, n(32768), true},
		{modelPro, nil, true},
		{testModel, n(32768), false},
		{testModel, n(24576), true},
		{testModel, n(0), true},
		{modelLite, n(32768), false},
		{modelLite, n(0), true},
	} {
		_, err := New(Config{Endpoint: testEndpoint, Model: tc.model, Tokens: &fakeTokens{tok: tokenMark},
			ThinkingAllowance: tc.allow})
		if tc.ok != (err == nil) || err != nil && !providers.IsInvalidConfig(err) {
			t.Errorf("%s with %v: err = %v, want accepted %v", tc.model, tc.allow, err, tc.ok)
		}
	}
}

// A 403 whose headers arrived is the authorization failure even when its body stalls past the attempt
// deadline: it is not retried as a timeout.
func TestStalledErrorBodyKeepsTheStatus(t *testing.T) {
	var calls atomic.Int32
	p := newProviderWith(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		return &http.Response{StatusCode: http.StatusForbidden, Header: jsonHeader(), Request: r,
			Body: stalledBody{ctx: r.Context()}}, nil
	}), func(c *Config) {
		c.MaxAttempts = 3
		c.AttemptTimeout = 30 * time.Millisecond
		c.BaseDelay = time.Millisecond
	})
	_, err := p.Complete(t.Context(), structuredRequest())
	if !providers.IsForbidden(err) || providers.IsRetryable(err) || calls.Load() != 1 {
		t.Errorf("err = %v, %d requests, want forbidden after one", err, calls.Load())
	}
}

type stalledBody struct {
	ctx interface{ Done() <-chan struct{} }
}

func (b stalledBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, io.ErrUnexpectedEOF
}
func (stalledBody) Close() error { return nil }

// A model version of the wrong type is malformed, but the usage that was read independently is kept.
func TestNumericModelVersionKeepsUsage(t *testing.T) {
	body := strings.Replace(replyJSON(`[{"text":"{}"}]`), `"modelVersion":"`+testModel+`"`, `"modelVersion":2.5`, 1)
	c, err := parseResponse([]byte(body), testModel, nil, true)
	if !providers.IsMalformedOutput(err) || c.Usage != wantUsage || c.UsageUnknown {
		t.Errorf("parse: err = %v, completion %+v", err, c)
	}
	if b := billedUsage([]byte(body)); b.Usage != wantUsage || b.UsageUnknown {
		t.Errorf("billedUsage: %+v", b)
	}
}

// With tools offered no response schema is sent, so a plain text answer is left to the caller to validate.
func TestToolsWithSchemaAcceptPlainText(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { reply(w, replyJSON(`[{"text":"plain answer"}]`)) }, nil)
	req := structuredRequest()
	req.Tools = toolDefs()
	c, err := h.p.Complete(t.Context(), req)
	if err != nil || c.Content != "plain answer" {
		t.Errorf("err = %v, content %q", err, c.Content)
	}
}
