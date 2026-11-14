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

package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/tools"
)

// These tests run the analysis loop against this adapter over an httptest server that checks the turn rules
// of the Messages API, so that the conversations the loop builds are ones the API accepts.

type loopContext struct{}

func (loopContext) Build(context.Context, *remediationv1alpha1.Incident) (*incidentcontext.Context, error) {
	// Truncated is an escalation signal: the loop goes straight to the escalation model, with tools.
	return &incidentcontext.Context{Truncated: true, Pods: []string{"web-1"}, Blocks: []incidentcontext.Block{{
		Section: incidentcontext.SectionStatus, Provenance: incidentcontext.ProvenanceObjectStatus,
		Availability: incidentcontext.Available, Text: "container app terminated OOMKilled\n",
	}}}, nil
}

type loopTools struct {
	mu    sync.Mutex
	calls []string
}

func (*loopTools) Definitions() []providers.ToolDefinition {
	d := make([]providers.ToolDefinition, 0, 2)
	for _, n := range []string{tools.NameListEvents, tools.NameGetPodLogs} {
		d = append(d, providers.ToolDefinition{Name: n, Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)})
	}
	return d
}

func (l *loopTools) Call(_ context.Context, call providers.ToolCall) (tools.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call.Name)
	return tools.Result{Tool: call.Name, Status: tools.StatusOK, Text: "no events\n"}, nil
}

// apiTurnError checks a request body against the turn rules of the Messages API: the first turn is the
// user's, roles alternate, and every tool_use of an assistant turn is answered by a tool_result at the start of
// the next user turn. It returns "" when the body follows them.
func apiTurnError(body []byte) string {
	var req struct {
		System   []map[string]any `json:"system"`
		Messages []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "not JSON"
	}
	if len(req.System) == 0 || req.System[len(req.System)-1]["cache_control"] == nil {
		return "no cached system prompt"
	}
	pending := map[string]bool{}
	for i, m := range req.Messages {
		want := "user"
		if i%2 == 1 {
			want = "assistant"
		}
		if m.Role != want {
			return "roles do not alternate from the user"
		}
		results := true
		for _, b := range m.Content {
			switch b["type"] {
			case blockToolUse:
				pending[b["id"].(string)] = true
			case blockToolResult:
				if !results || !pending[b["tool_use_id"].(string)] {
					return "a tool_result answers no pending tool_use"
				}
				delete(pending, b["tool_use_id"].(string))
				continue
			}
			results = false
		}
		if m.Role == "user" && len(pending) > 0 {
			return "a tool_use has no tool_result"
		}
	}
	return ""
}

// runLoop runs one analysis whose escalation model is the adapter, answering the calls with replies in order.
func runLoop(t *testing.T, replies ...string) (analysis.Outcome, int, *loopTools) {
	t.Helper()
	var mu sync.Mutex
	sent := 0
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decoding the request: %v", err)
		}
		if msg := apiTurnError(raw); msg != "" {
			t.Errorf("request %d breaks the API's turn rules: %s", sent+1, msg)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		sent++
		if sent > len(replies) {
			http.Error(w, "no reply scripted", http.StatusBadRequest)
			return
		}
		reply(w, replies[sent-1])
	})
	lt := &loopTools{}
	a, err := analysis.New(analysis.Config{
		Builder: loopContext{},
		Tools:   lt,
		Models: func(analysis.Role, remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
			return h.p, nil
		},
		Reserve:        func(context.Context, time.Time) error { return nil },
		ExpectedModels: map[analysis.Role]string{analysis.RoleTriage: testModel, analysis.RoleEscalation: testModel},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &remediationv1alpha1.ModelDeployment{Endpoint: Endpoint, Deployment: testModel, APIVersion: APIVersion,
		MaxTokensPerDay: new(int64(10_000_000))}
	inc := &remediationv1alpha1.Incident{Spec: remediationv1alpha1.IncidentSpec{Subject: remediationv1alpha1.ObjectRef{
		APIVersion: "apps/v1", Kind: "Deployment", Namespace: "shop", Name: "web", UID: "uid-1"}}}
	out, err := a.Analyze(t.Context(), inc, analysis.Limits{MaxToolSteps: 2, MaxOutputTokens: 1024,
		Triage: d, Escalation: d})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return out, sent, lt
}

func toolUseReply(model, id string) string {
	return messageJSON(model, stopToolUse, `[{"type":"thinking","thinking":"","signature":"s"},`+
		`{"type":"tool_use","id":"`+id+`","name":"`+tools.NameListEvents+`","input":{"namespace":"shop"}}]`)
}

// The loop's tool rounds, up to the last turn and its fixed message, are conversations the API accepts.
func TestLoopToolRoundsFollowTheTurnRules(t *testing.T) {
	out, sent, lt := runLoop(t,
		toolUseReply(testModel, callOne), toolUseReply(testModel, "toolu_2"), toolUseReply(testModel, "toolu_3"))
	if out.Reason != analysis.ReasonStepLimit || !out.Escalated {
		t.Errorf("reason %s, escalated %v, want StepLimit", out.Reason, out.Escalated)
	}
	if sent != 3 || len(lt.calls) != 2 {
		t.Errorf("%d model calls and %d tool calls, want 3 and 2", sent, len(lt.calls))
	}
}

// A reply from another model ends the analysis as an unexpected model, and its tool call never runs.
func TestLoopAnotherModelIsRefused(t *testing.T) {
	out, sent, lt := runLoop(t, toolUseReply("claude-opus-4-8", callOne))
	if out.Reason != analysis.ReasonUnexpectedModel || out.Plan != nil {
		t.Errorf("reason %s, want UnexpectedModel", out.Reason)
	}
	if sent != 1 || len(lt.calls) != 0 {
		t.Errorf("%d model calls and %d tool calls, want 1 and 0", sent, len(lt.calls))
	}
}
