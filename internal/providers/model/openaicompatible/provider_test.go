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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/model/openaiwire"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const echoNameField = "name"

const echoFunctionCallType = "function_call"
const echoToolCallsField = "tool_calls"
const echoFunctionField = "function"

const echoChoicesField = "choices"
const echoArgumentsField = "arguments"

const secretKey = "sk-FAKE-CREDENTIAL-834" // #nosec G101 -- synthetic marker used to prove credential non-disclosure
const compatibleHost = "api.openai.com"
const testModel = "gpt-6.1-sol"
const endpoint = "https://api.openai.com/v1"
const chatOK = `{"model":"` + testModel + `","choices":[{"finish_reason":"stop","message":{"content":"{\"ok\":true}"}}],"usage":{"prompt_tokens":30,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":10}}}`
const responsesOK = `{"model":"` + testModel + `","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"ok\":true}"}]}],"usage":{"input_tokens":30,"output_tokens":5,"input_tokens_details":{"cached_tokens":10}}}`

func keyFile(t *testing.T, key string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
func request() providers.CompletionRequest {
	return providers.CompletionRequest{Messages: []providers.Message{{Role: providers.RoleUser, Content: "redacted context"}}, ResponseSchema: []byte(`{"type":"object"}`), MaxOutputTokens: 100}
}
func response(code int, contentType, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}

func TestWireAndCredentialRotation(t *testing.T) {
	for _, api := range []API{APIChatCompletions, APIResponses} {
		t.Run(string(api), func(t *testing.T) {
			path := keyFile(t, secretKey)
			calls := 0
			p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel, API: api, KeyFile: path}, testHooks{transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				wantKey := secretKey
				if calls > 1 {
					wantKey = "rotated-key-834-000"
				}
				if r.Header.Get("Authorization") != "Bearer "+wantKey || r.Header.Get("x-api-key") != "" {
					t.Error("wrong credential")
				}
				path, body := "/v1/chat/completions", chatOK
				if api == APIResponses {
					path, body = "/v1/responses", responsesOK
				}
				if r.URL.Path != path || r.URL.RawQuery != "" || r.URL.Host != compatibleHost {
					t.Errorf("incorrect destination: %s", r.URL)
				}
				return response(200, "application/json", body), nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				c, err := p.Complete(t.Context(), request())
				if err != nil || c.Usage.InputTokens != 30 || c.Usage.OutputTokens != 5 || c.Usage.CachedInputTokens != 10 || c.Content != `{"ok":true}` {
					t.Fatalf("completion %+v, %v", c, err)
				}
				if err := os.WriteFile(path, []byte("rotated-key-834-000"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestKeylessSendsNoAuthorization(t *testing.T) {
	p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel}, testHooks{transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if _, ok := r.Header["Authorization"]; ok {
			t.Fatal("keyless request has Authorization")
		}
		return response(200, "application/json", chatOK), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(t.Context(), request()); err != nil {
		t.Fatal(err)
	}
}

func TestConfigurationRefusals(t *testing.T) {
	for _, cfg := range []Config{

		{Endpoint: endpoint, Deployment: "m", KeyFile: "relative"},
		{Endpoint: "http://model.demo.svc/v1", Deployment: "m"},
		{Endpoint: "https://127.0.0.1/v1", Deployment: "m"},
		{Endpoint: "https://api.openai.com/../v1", Deployment: "m"},
		{Endpoint: "https://api.anthropic.com", Deployment: "m"},
		{Endpoint: "https://r.openai.azure.com", Deployment: "m"},
		{Endpoint: "https://r.cognitiveservices.azure.com", Deployment: "m"},
		{Endpoint: "https://r.services.ai.azure.com", Deployment: "m"},
		{Endpoint: endpoint, Deployment: "m", API: "Other"},
		{Endpoint: endpoint, Deployment: "m", Service: &Service{Namespace: "demo", Port: 8443}},
	} {
		if _, err := New(cfg); !providers.IsInvalidConfig(err) {
			t.Errorf("configuration accepted: %+v, %v", cfg, err)
		}
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New(secretKey) }
func (errorReader) Close() error             { return nil }

// Every external error path is hostile: bodies, headers and transport/read errors echo the key.
func TestErrorsNeverExposeTheKey(t *testing.T) {
	cases := map[string]func(*http.Request) (*http.Response, error){
		"network": func(*http.Request) (*http.Response, error) { return nil, errors.New(secretKey) },
		"read": func(*http.Request) (*http.Response, error) {
			r := response(200, "application/json", "")
			r.Body = errorReader{}
			return r, nil
		},
		"oversize": func(*http.Request) (*http.Response, error) {
			return response(200, "application/json", strings.Repeat(secretKey, 200)), nil
		},
		"content type": func(*http.Request) (*http.Response, error) { return response(200, "text/"+secretKey, chatOK), nil },
		"malformed":    func(*http.Request) (*http.Response, error) { return response(200, "application/json", secretKey), nil },
		"echoed model": func(*http.Request) (*http.Response, error) {
			return response(200, "application/json", strings.ReplaceAll(chatOK, testModel, secretKey)), nil
		},
		"escaped echo": func(*http.Request) (*http.Response, error) {
			return response(200, "application/json", strings.ReplaceAll(chatOK, testModel, `sk-FAKE-CREDENTIAL-83\u0034`)), nil
		},
	}
	for _, code := range []int{301, 302, 303, 307, 308, 400, 401, 403, 404, 408, 413, 429, 500} {
		cases[fmt.Sprint(code)] = func(*http.Request) (*http.Response, error) {
			r := response(code, "application/json", secretKey)
			r.Header.Set("Location", "https://other.example.com/"+secretKey)
			r.Header.Set("Retry-After", secretKey)
			return r, nil
		}
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			var report CallReport
			calls := 0
			p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel, KeyFile: keyFile(t, secretKey), MaxAttempts: 1, Timeout: time.Second, AttemptTimeout: time.Second, MaxResponseBytes: 1024, OnCall: func(r CallReport) { report = r }}, testHooks{transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Context().Err() != nil {
					return nil, r.Context().Err()
				}
				if calls > 1 {
					return nil, errors.New("unexpected redirect")
				}
				return handler(r)
			})})
			if err != nil {
				t.Fatal(err)
			}
			p.client.Timeout = time.Second
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			c, err := p.Complete(ctx, request())
			if err == nil {
				t.Fatal("hostile reply accepted")
			}
			for e := err; e != nil; e = errors.Unwrap(e) {
				if strings.Contains(e.Error(), secretKey) {
					t.Fatal("error chain leaks key")
				}
			}
			if text := fmt.Sprintf("%+v %+v", c, report); strings.Contains(text, secretKey) {
				t.Fatal("completion or telemetry leaks key")
			}
			if calls != 1 {
				t.Fatal("redirect or unexpected retry")
			}
			if providers.RefusedUnbilled(c, err) {
				t.Fatal("compatible host incorrectly marked unbilled")
			}
		})
	}
}

func TestKeyFileErrorsAndCancellation(t *testing.T) {
	for _, key := range []string{"", " ", "bad\nkey", strings.Repeat("k", 513), strings.Repeat("k", 4097)} {
		p, err := New(Config{Endpoint: endpoint, Deployment: "m", KeyFile: keyFile(t, key), MaxAttempts: 1})
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.Complete(t.Context(), request())
		if !errors.Is(err, providers.ErrCredentialUnavailable) || !providers.IsNotSent(err) {
			t.Fatalf("credential error %v", err)
		}
	}
	p, err := New(Config{Endpoint: endpoint, Deployment: "m", KeyFile: "/does-not-exist", MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Complete(t.Context(), request())
	if !providers.IsRetryable(err) || !providers.IsNotSent(err) {
		t.Fatalf("unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = p.Complete(ctx, request())
	if !errors.Is(err, context.Canceled) || !providers.IsNotSent(err) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestMissingUsageFailsClosed(t *testing.T) {
	for _, api := range []API{APIChatCompletions, APIResponses} {
		body := `{"model":"` + testModel + `","choices":[{"finish_reason":"stop","message":{"content":"{}"}}]}`
		if api == APIResponses {
			body = `{"model":"` + testModel + `","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{}"}]}]}`
		}
		p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel, API: api}, testHooks{transport: roundTrip(func(*http.Request) (*http.Response, error) { return response(200, "application/json", body), nil })})
		if err != nil {
			t.Fatal(err)
		}
		c, err := p.Complete(t.Context(), request())
		if !providers.IsMalformedOutput(err) || !c.UsageUnknown || c.Content != "" || len(c.ToolCalls) > 0 {
			t.Fatalf("unknown usage: %+v %v", c, err)
		}
	}
}

func TestServiceAddressBoundary(t *testing.T) {
	for _, host := range []string{"model.demo.svc", "model.demo.svc.cluster.local"} {
		p, err := New(Config{Endpoint: "https://" + host + "/v1", Deployment: "m", Service: &Service{Namespace: "demo", PodSelector: map[string]string{"app": "model"}, Port: 8443}})
		if err != nil {
			t.Fatal(err)
		}
		tr := p.client.Transport.(*http.Transport)
		if tr.Proxy != nil || tr.TLSClientConfig.InsecureSkipVerify || tr.TLSClientConfig.MinVersion == 0 {
			t.Fatal("unsafe client")
		}
		if err := tr.DialContext; err == nil {
			t.Fatal("missing guarded dialer")
		}
		if err := p.client.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
			t.Fatal("redirects permitted")
		}
	}
	for _, addr := range []string{"127.0.0.1:443", "169.254.169.254:443", "[::1]:443", "224.0.0.1:443"} {
		if err := openaiwire.PrivateServiceControl("tcp", addr, nil); err == nil {
			t.Errorf("unsafe Service address %s accepted", addr)
		}
	}
	if err := openaiwire.PrivateServiceControl("tcp", "10.2.3.4:443", nil); err != nil {
		t.Fatal("private Service refused")
	}
}

func TestRetryRereadsKeyAndKeepsBounds(t *testing.T) {
	path := keyFile(t, secretKey)
	calls := 0
	p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel, KeyFile: path, MaxAttempts: 2}, testHooks{
		sleep: func(context.Context, time.Duration) error {
			return os.WriteFile(path, []byte("rotated-marker-834-000"), 0o600)
		},
		transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return response(429, "application/json", ""), nil
			}
			if r.Header.Get("Authorization") != "Bearer rotated-marker-834-000" {
				t.Fatal("retry cached old key")
			}
			return response(200, "application/json", chatOK), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(t.Context(), request()); err != nil || calls != 2 {
		t.Fatalf("retry: %v, calls %d", err, calls)
	}
}

func TestIncompleteUsageNeverCountsAsZero(t *testing.T) {
	for _, usage := range []string{`{}`, `{"prompt_tokens":5}`, `{"completion_tokens":5}`} {
		for _, mime := range []string{"application/json", "text/plain"} {
			body := `{"model":"` + testModel + `","choices":[{"finish_reason":"stop","message":{"content":"{}"}}],"usage":` + usage + `}`
			p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel}, testHooks{transport: roundTrip(func(*http.Request) (*http.Response, error) { return response(200, mime, body), nil })})
			if err != nil {
				t.Fatal(err)
			}
			c, err := p.Complete(t.Context(), request())
			if !providers.IsMalformedOutput(err) || !c.UsageUnknown {
				t.Fatalf("incomplete usage accepted: %+v %v", c, err)
			}
		}
	}
}

// A key may be hidden from the outer JSON by split text parts or a second JSON escape layer.
func TestSplitAndNestedCredentialEchoIsRefused(t *testing.T) {
	escaped := strings.ReplaceAll(secretKey, "-", `\u002d`)
	inner := `{"summary":"` + escaped + `"}`
	plain := `{"summary":"` + secretKey + `"}`
	split := strings.Index(plain, secretKey) + len(secretKey)/2
	usage := map[string]int64{"input_tokens": 30, "output_tokens": 5}
	responses := map[string]any{echoModelField: testModel, echoStatusField: echoCompleted, echoUsageField: usage,
		echoOutputField: []any{map[string]any{echoTypeField: echoMessageType, "role": "assistant", echoContentField: []any{
			map[string]any{echoTypeField: echoOutputText, echoTextField: plain[:split]},
			map[string]any{echoTypeField: echoOutputText, echoTextField: plain[split:]},
		}}}}
	chat := map[string]any{echoModelField: testModel,
		echoUsageField:   map[string]int64{"prompt_tokens": 30, "completion_tokens": 5},
		echoChoicesField: []any{map[string]any{"finish_reason": "stop", echoMessageType: map[string]any{echoContentField: inner}}}}
	tools := map[string]any{echoModelField: testModel, echoStatusField: echoCompleted, echoUsageField: usage,
		echoOutputField: []any{map[string]any{echoTypeField: echoFunctionCallType, "call_id": "c1", echoNameField: "t", echoArgumentsField: inner}}}
	splitChat := map[string]any{echoModelField: testModel,
		echoUsageField: map[string]int64{"prompt_tokens": 30, "completion_tokens": 5},
		echoChoicesField: []any{map[string]any{"finish_reason": echoToolCallsField, echoMessageType: map[string]any{
			echoContentField: secretKey[:len(secretKey)/2], echoToolCallsField: []any{map[string]any{"id": "c1", echoTypeField: echoFunctionField, echoFunctionField: map[string]string{
				echoNameField: "t", echoArgumentsField: `{"a":"` + secretKey[len(secretKey)/2:] + `"}`,
			}}},
		}}}}
	interleaved := map[string]any{echoModelField: testModel, echoStatusField: echoCompleted, echoUsageField: usage,
		echoOutputField: []any{
			map[string]any{echoTypeField: echoMessageType, echoContentField: []any{map[string]string{echoTypeField: echoOutputText, echoTextField: plain[:split]}}},
			map[string]string{echoTypeField: echoFunctionCallType, "call_id": "c1", echoNameField: "t", echoArgumentsField: `{"a":"neighbor"}`},
			map[string]any{echoTypeField: echoMessageType, echoContentField: []any{map[string]string{echoTypeField: echoOutputText, echoTextField: plain[split:]}}},
		}}
	for name, tc := range map[string]struct {
		api  API
		body any
	}{
		"joined Chat content and arguments": {APIChatCompletions, splitChat},
		"text split around arguments":       {APIResponses, interleaved},
		"split Responses text":              {APIResponses, responses},
		"escaped structured Chat content":   {APIChatCompletions, chat},
		"escaped tool arguments":            {APIResponses, tools},
	} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatal(err)
			}
			p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel, API: tc.api,
				KeyFile: keyFile(t, secretKey)}, testHooks{transport: roundTrip(func(*http.Request) (*http.Response, error) {
				return response(200, "application/json", string(data)), nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			req := request()
			req.Tools = []providers.ToolDefinition{{Name: "t", Parameters: []byte(`{"type":"object"}`)}}
			c, err := p.Complete(t.Context(), req)
			if !providers.IsMalformedOutput(err) || c.Content != "" || len(c.ToolCalls) != 0 || c.Model != "" {
				t.Fatalf("credential-bearing reply accepted: error class %v, content bytes %d, calls %d", err, len(c.Content), len(c.ToolCalls))
			}
		})
	}
}

func TestUsageCeiling(t *testing.T) {
	for _, api := range []API{APIChatCompletions, APIResponses} {
		body := chatOK
		if api == APIResponses {
			body = responsesOK
		}
		body = strings.Replace(body, ":30", ":1099511627776", 1)
		p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel, API: api}, testHooks{transport: roundTrip(func(*http.Request) (*http.Response, error) { return response(200, "application/json", body), nil })})
		if err != nil {
			t.Fatal(err)
		}
		c, err := p.Complete(t.Context(), request())
		if !providers.IsMalformedOutput(err) || !c.UsageUnknown || c.Usage.InputTokens != 0 {
			t.Errorf("%s: unbounded usage accepted: %+v %v", api, c, err)
		}
	}
}

func TestJoinedReplyKeyCorpus(t *testing.T) {
	checkJoinedReplyBaseline(t)
	// Raw text parts must survive even when one part is also a valid JSON number.
	for cut := 1; cut < len(secretKey); cut++ {
		data, _ := json.Marshal(map[string]any{echoOutputField: []any{map[string]any{
			echoTypeField: echoMessageType, echoContentField: []any{
				map[string]string{echoTypeField: echoOutputText, echoTextField: secretKey[:cut]},
				map[string]string{echoTypeField: echoOutputText, echoTextField: secretKey[cut:]},
			},
		}}})
		if !baselineReplyContainsKey(data, APIResponses, secretKey) {
			t.Fatal("corpus lacks a baseline match")
		}
		if !replyContainsKey(data, APIResponses, secretKey) {
			t.Errorf("lost raw text at split %d", cut)
		}
	}
	// Adding arguments or other part types must not hide a key the text-only matcher found.
	for cut := 1; cut < len(secretKey); cut++ {
		first := map[string]string{echoTypeField: echoOutputText, echoTextField: secretKey[:cut]}
		last := map[string]string{echoTypeField: echoOutputText, echoTextField: secretKey[cut:]}
		message := func(parts ...any) any { return map[string]any{echoTypeField: echoMessageType, echoContentField: parts} }
		for _, output := range [][]any{
			{message(first, map[string]string{echoTypeField: "refusal", echoTextField: "adjacent text"}, last)},
			{message(first), map[string]string{echoTypeField: echoFunctionCallType, echoArgumentsField: `{"a":"adjacent argument"}`}, message(last)},
		} {
			data, _ := json.Marshal(map[string]any{echoOutputField: output})
			before := string(data)
			if !baselineReplyContainsKey(data, APIResponses, secretKey) {
				t.Fatal("corpus lacks a baseline match")
			}
			if !replyContainsKey(data, APIResponses, secretKey) {
				t.Errorf("neighbor hid a baseline match at split %d", cut)
			}
			if string(data) != before {
				t.Fatal("matcher changed the neighbor")
			}
		}
	}
	// Known-positive split secrets, including every boundary, escape layer and API.
	for split := 1; split < len(secretKey); split++ {
		for _, api := range []API{APIChatCompletions, APIResponses} {
			for _, escaped := range []bool{false, true} {
				left, right := secretKey[:split], secretKey[split:]
				args, _ := json.Marshal(map[string]string{"a": right})
				if escaped {
					args = []byte(strings.ReplaceAll(string(args), "4", `\u0034`))
				}
				var body any
				if api == APIResponses {
					body = map[string]any{"output": []any{map[string]any{echoTypeField: echoMessageType, echoContentField: []any{map[string]string{echoTypeField: "output_text", "text": left}}}, map[string]string{echoTypeField: echoFunctionCallType, echoArgumentsField: string(args)}}}
				} else {
					body = map[string]any{echoChoicesField: []any{map[string]any{echoMessageType: map[string]any{echoContentField: left, echoToolCallsField: []any{map[string]any{echoFunctionField: map[string]string{echoArgumentsField: string(args)}}}}}}}
				}
				data, _ := json.Marshal(body)
				if !replyContainsKey(data, api, secretKey) {
					t.Errorf("%s split %d escaped %v: joined key missed", api, split, escaped)
				}
			}
		}
	}
	// Every previously matched field still matches, beside an adversarial neighbour.
	for _, value := range []string{secretKey, "before" + secretKey + "after", secretKey + secretKey} {
		data, _ := json.Marshal(map[string]any{echoChoicesField: []any{map[string]any{echoMessageType: map[string]string{"content": value}}}})
		if !replyContainsKey(data, APIChatCompletions, secretKey) {
			t.Fatal("lost a previous match")
		}
	}
}

func TestWireErrorsNameCompatibleProvider(t *testing.T) {
	for _, api := range []API{APIChatCompletions, APIResponses} {
		for _, requestError := range []bool{false, true} {
			p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel, API: api}, testHooks{transport: roundTrip(func(*http.Request) (*http.Response, error) {
				body := chatOK
				if api == APIResponses {
					body = responsesOK
				}
				return response(200, "application/json", strings.ReplaceAll(body, testModel, "invalid model")), nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			req := request()
			if requestError {
				req.Messages = nil
			}
			_, err = p.Complete(t.Context(), req)
			if err == nil {
				t.Fatal("invalid wire data accepted")
			}
			for e := err; e != nil; e = errors.Unwrap(e) {
				if strings.Contains(e.Error(), "foundry:") {
					t.Errorf("wrong provider: %v", e)
				}
			}
			if !strings.Contains(err.Error(), "openai-compatible:") {
				t.Errorf("missing provider: %v", err)
			}
		}
	}
}

func TestReportedUsageBoundary(t *testing.T) {
	req := request()
	ceiling := providers.CompatibleUsageCeiling(providers.EstimateInputTokens(req), int64(req.MaxOutputTokens))
	for _, api := range []API{APIChatCompletions, APIResponses} {
		for _, n := range []int64{0, ceiling, ceiling + 1} {
			body := strings.ReplaceAll(chatOK, `"cached_tokens":10`, `"cached_tokens":0`)
			if api == APIResponses {
				body = strings.ReplaceAll(responsesOK, `"cached_tokens":10`, `"cached_tokens":0`)
			}
			body = strings.Replace(body, ":30", fmt.Sprintf(":%d", n), 1)
			body = strings.Replace(body, ":5", ":0", 1)
			p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel, API: api}, testHooks{transport: roundTrip(func(*http.Request) (*http.Response, error) { return response(200, "application/json", body), nil })})
			if err != nil {
				t.Fatal(err)
			}
			c, err := p.Complete(t.Context(), req)
			if n <= ceiling && (err != nil || c.UsageUnknown) {
				t.Errorf("bounded usage %d refused: %v", n, err)
			}
			if n > ceiling && (!providers.IsMalformedOutput(err) || !c.UsageUnknown) {
				t.Errorf("usage above ceiling accepted: %+v %v", c, err)
			}
		}
	}
}

func TestKeyEchoAcrossKeysAndNumbers(t *testing.T) {
	const key = "sk-ABCDEF123456XYZ"
	for _, arguments := range []string{`{"123456XYZ":1}`, `{"a":123456,"b":"XYZ"}`} {
		data := `{"model":"m","choices":[{"message":{"content":"sk-ABCDEF","tool_calls":[{"function":{"arguments":` + strconv.Quote(arguments) + `}}]}}]}`
		p, err := newProvider(Config{Endpoint: endpoint, Deployment: "m", KeyFile: keyFile(t, key)}, testHooks{transport: roundTrip(func(*http.Request) (*http.Response, error) { return response(200, "application/json", data), nil })})
		if err != nil {
			t.Fatal(err)
		}
		body, _, _, err := p.once(t.Context(), []byte(`{}`))
		if !errors.Is(err, providers.ErrMalformedOutput) || len(body) != 0 {
			t.Fatalf("credential-bearing body escaped: %v", err)
		}
	}
}
func TestBilledFailedUsageCeilingBoundary(t *testing.T) {
	req := request()
	ceiling := providers.CompatibleUsageCeiling(providers.EstimateInputTokens(req), int64(req.MaxOutputTokens))
	for _, n := range []int64{ceiling, ceiling + 1} {
		p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel, MaxAttempts: 1}, testHooks{transport: roundTrip(func(*http.Request) (*http.Response, error) {
			body := fmt.Sprintf(`{"usage":{"prompt_tokens":%d,"completion_tokens":0}}`, n)
			return response(200, "text/plain", body), nil
		})})
		if err != nil {
			t.Fatal(err)
		}
		c, err := p.Complete(t.Context(), req)
		if !errors.Is(err, providers.ErrMalformedOutput) || c.UsageUnknown != (n > ceiling) {
			t.Fatalf("boundary %d: %+v %v", n, c, err)
		}
		if n > ceiling && c.Usage.InputTokens != 0 {
			t.Fatal("oversized billed failure retained usage")
		}
	}
}

func TestShortKeyIsMalformed(t *testing.T) {
	for n := 1; n < 16; n++ {
		key := strings.Repeat("k", n)
		calls := 0
		p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel, KeyFile: keyFile(t, key)}, testHooks{transport: roundTrip(func(*http.Request) (*http.Response, error) {
			calls++
			return response(200, "application/json", chatOK), nil
		})})
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.Complete(t.Context(), request())
		var retry *providers.RetryableError
		if err == nil || !providers.IsNotSent(err) || !errors.Is(err, providers.ErrCredentialUnavailable) || errors.As(err, &retry) || calls != 0 || !strings.Contains(err.Error(), "at least 16 bytes") {
			t.Fatalf("length %d: %v calls=%d", n, err, calls)
		}
	}
}
