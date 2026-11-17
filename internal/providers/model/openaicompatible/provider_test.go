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
	"strings"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/model/openaiwire"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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
			p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel, API: api, KeyFile: path, KeyHost: compatibleHost}, testHooks{transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				wantKey := secretKey
				if calls > 1 {
					wantKey = "rotated-key-834"
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
				if err := os.WriteFile(path, []byte("rotated-key-834"), 0o600); err != nil {
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
		{Endpoint: endpoint, Deployment: "m", KeyFile: "/key"},
		{Endpoint: endpoint, Deployment: "m", KeyHost: compatibleHost},
		{Endpoint: endpoint, Deployment: "m", KeyFile: "/key", KeyHost: "other.example.com"},
		{Endpoint: endpoint, Deployment: "m", KeyFile: "relative", KeyHost: compatibleHost},
		{Endpoint: "http://model.demo.svc/v1", Deployment: "m"},
		{Endpoint: "https://127.0.0.1/v1", Deployment: "m"},
		{Endpoint: "https://api.openai.com:8443/v1", Deployment: "m"},
		{Endpoint: "https://api.openai.com/../v1", Deployment: "m"},
		{Endpoint: "https://api.anthropic.com", Deployment: "m"},
		{Endpoint: "https://r.openai.azure.com", Deployment: "m"},
		{Endpoint: "https://r.cognitiveservices.azure.com", Deployment: "m"},
		{Endpoint: "https://r.services.ai.azure.com", Deployment: "m"},
		{Endpoint: endpoint, Deployment: "m", API: "Other"},
		{Endpoint: endpoint, Deployment: "m", ServiceHosts: []string{"*.svc.cluster.local"}},
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
			p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel, KeyFile: keyFile(t, secretKey), KeyHost: compatibleHost, MaxAttempts: 1, MaxResponseBytes: 1024, OnCall: func(r CallReport) { report = r }}, testHooks{transport: roundTrip(func(r *http.Request) (*http.Response, error) { calls++; return handler(r) })})
			if err != nil {
				t.Fatal(err)
			}
			c, err := p.Complete(t.Context(), request())
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
		p, err := New(Config{Endpoint: endpoint, Deployment: "m", KeyFile: keyFile(t, key), KeyHost: compatibleHost, MaxAttempts: 1})
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.Complete(t.Context(), request())
		if !errors.Is(err, providers.ErrCredentialUnavailable) || !providers.IsNotSent(err) {
			t.Fatalf("credential error %v", err)
		}
	}
	p, err := New(Config{Endpoint: endpoint, Deployment: "m", KeyFile: "/does-not-exist", KeyHost: compatibleHost, MaxAttempts: 1})
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
		p, err := New(Config{Endpoint: "https://" + host + "/v1", Deployment: "m", ServiceHosts: []string{host}})
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
	p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel, KeyFile: path, KeyHost: compatibleHost, MaxAttempts: 2}, testHooks{
		sleep: func(context.Context, time.Duration) error { return os.WriteFile(path, []byte("rotated"), 0o600) },
		transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return response(429, "application/json", ""), nil
			}
			if r.Header.Get("Authorization") != "Bearer rotated" {
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
		echoUsageField: map[string]int64{"prompt_tokens": 30, "completion_tokens": 5},
		"choices":      []any{map[string]any{"finish_reason": "stop", echoMessageType: map[string]any{echoContentField: inner}}}}
	tools := map[string]any{echoModelField: testModel, echoStatusField: echoCompleted, echoUsageField: usage,
		echoOutputField: []any{map[string]any{echoTypeField: "function_call", "call_id": "c1", "name": "t", "arguments": inner}}}
	for name, tc := range map[string]struct {
		api  API
		body any
	}{
		"split Responses text":            {APIResponses, responses},
		"escaped structured Chat content": {APIChatCompletions, chat},
		"escaped tool arguments":          {APIResponses, tools},
	} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatal(err)
			}
			p, err := newProvider(Config{Endpoint: endpoint, Deployment: testModel, API: tc.api,
				KeyFile: keyFile(t, secretKey), KeyHost: compatibleHost}, testHooks{transport: roundTrip(func(*http.Request) (*http.Response, error) {
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
