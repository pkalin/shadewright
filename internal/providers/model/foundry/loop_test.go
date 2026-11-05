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

package foundry

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/analysis"
	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/tools"
)

// These tests run the analysis loop against this adapter over an httptest server, so that what the loop
// expects of a model provider is what the real adapter does, not only what the loop's fake does.

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
	d := make([]providers.ToolDefinition, 0, 3)
	for _, n := range []string{tools.NameListEvents, tools.NameGetPodLogs, tools.NameGetDeliveryStatus} {
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

// sentRequest is what the loop sent in one call: the names of the tools offered and the last message.
type sentRequest struct {
	tools       []string
	lastContent string
}

// runLoop runs one analysis whose escalation model is the adapter, answering the calls with replies in order.
func runLoop(t *testing.T, maxToolSteps int, replies ...string) (analysis.Outcome, []sentRequest, *loopTools) {
	t.Helper()
	var mu sync.Mutex
	var sent []sentRequest
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []struct {
				Function struct{ Name string } `json:"function"`
			} `json:"tools"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding the request: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		var s sentRequest
		for _, tl := range body.Tools {
			s.tools = append(s.tools, tl.Function.Name)
		}
		if n := len(body.Messages); n > 0 {
			s.lastContent = body.Messages[n-1].Content
		}
		sent = append(sent, s)
		if len(sent) > len(replies) {
			http.Error(w, "no reply scripted", http.StatusBadRequest)
			return
		}
		reply(w, replies[len(sent)-1])
	}, nil)
	lt := &loopTools{}
	a, err := analysis.New(analysis.Config{
		Builder: loopContext{},
		Tools:   lt,
		Models: func(analysis.Role, remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
			return h.p, nil
		},
		Reserve: func(context.Context, time.Time) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &remediationv1alpha1.ModelDeployment{Endpoint: testEndpoint, Deployment: testDeployment, APIVersion: "v1",
		MaxTokensPerDay: new(int64(10_000_000))}
	inc := &remediationv1alpha1.Incident{Spec: remediationv1alpha1.IncidentSpec{Subject: remediationv1alpha1.ObjectRef{
		APIVersion: "apps/v1", Kind: "Deployment", Namespace: "shop", Name: "web", UID: "uid-1"}}}
	out, err := a.Analyze(t.Context(), inc, analysis.Limits{MaxToolSteps: maxToolSteps, MaxOutputTokens: 1024,
		Triage: d, Escalation: d})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return out, slices.Clone(sent), lt
}

func toolCallReply(id, name, args string) string {
	return completionJSON("tool_calls", `{"role":"assistant","content":null,"tool_calls":[{"id":"`+id+
		`","type":"function","function":{"name":"`+name+`","arguments":`+strconv.Quote(args)+`}}]}`)
}

// A tool call on the last turn of an escalation reaches the loop as a tool call, which ends the analysis with
// StepLimit: the last turn offers the same tools as the others, so the adapter does not refuse the reply as
// malformed, and says in a fixed message that no calls are left.
func TestLoopToolCallOnTheLastTurnIsStepLimit(t *testing.T) {
	out, sent, lt := runLoop(t, 1,
		toolCallReply("c1", tools.NameListEvents, `{"namespace":"shop"}`),
		toolCallReply("c2", tools.NameListEvents, `{"namespace":"shop"}`),
	)
	if out.Reason != analysis.ReasonStepLimit || !out.Escalated {
		t.Errorf("reason %s, escalated %v, want StepLimit", out.Reason, out.Escalated)
	}
	if len(sent) != 2 || len(lt.calls) != 1 {
		t.Fatalf("%d model calls and %d tool calls, want 2 and 1", len(sent), len(lt.calls))
	}
	if !slices.Equal(sent[0].tools, sent[1].tools) || len(sent[0].tools) != 2 ||
		slices.Contains(sent[0].tools, tools.NameGetDeliveryStatus) {
		t.Errorf("tools offered %v then %v, want the same two on both turns", sent[0].tools, sent[1].tools)
	}
	if sent[0].lastContent == sent[1].lastContent || sent[1].lastContent == "" {
		t.Errorf("the last turn does not end with the fixed message: %q", sent[1].lastContent)
	}
}

// A reply that names a tool the request did not offer is malformed output: the escalation hands it off as
// InvalidOutput and no tool runs.
func TestLoopUnofferedToolIsInvalidOutput(t *testing.T) {
	out, sent, lt := runLoop(t, 4,
		toolCallReply("c1", tools.NameGetDeliveryStatus, `{"kind":"Kustomization","namespace":"flux-system","name":"apps"}`),
	)
	if out.Reason != analysis.ReasonInvalidOutput || out.Plan != nil {
		t.Errorf("reason %s, want InvalidOutput", out.Reason)
	}
	if len(sent) != 1 || len(lt.calls) != 0 {
		t.Errorf("%d model calls and %d tool calls, want 1 and 0", len(sent), len(lt.calls))
	}
	if out.Tokens != 150 {
		t.Errorf("charged %d tokens, want the 150 the malformed reply reported", out.Tokens)
	}
}
