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
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	remediationv1alpha1 "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/analysis"
	"github.com/shadewright-labs/shadewright/internal/incidentcontext"
	"github.com/shadewright-labs/shadewright/internal/providers"
	"github.com/shadewright-labs/shadewright/internal/tools"
)

// These tests run the analysis loop against this adapter over an httptest server, so that what the loop
// expects of a model provider is what the real adapter does, not only what the loop's fake does.

type loopContext struct{}

func (loopContext) BuildWithin(context.Context, *remediationv1alpha1.Incident, int) (*incidentcontext.Context, error) {
	// Truncated is an escalation signal: the loop goes straight to the escalation model, with tools.
	return &incidentcontext.Context{Truncated: true, Pods: []string{"web-1"}, Blocks: []incidentcontext.Block{{
		Section: incidentcontext.SectionStatus, Provenance: incidentcontext.ProvenanceObjectStatus,
		Availability: incidentcontext.Available, Text: "container app terminated OOMKilled\n",
	}}}, nil
}

type loopTools struct {
	mu    sync.Mutex
	calls []string
	args  []string
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
	l.args = append(l.args, string(call.Arguments))
	return tools.Result{Tool: call.Name, Status: tools.StatusOK, Text: "no events\n"}, nil
}

// sentRequest is what the loop sent in one call: the names of the tools offered, the text that ends the last
// turn, and the whole body.
type sentRequest struct {
	tools       []string
	lastContent string
	// config is the generationConfig member of the body.
	config map[string]any
	body   []byte
}

// runLoop runs one analysis whose escalation model is the adapter, answering the calls with replies in order.
func runLoop(t *testing.T, maxToolSteps int, replies ...string) (analysis.Outcome, []sentRequest, *loopTools) {
	t.Helper()
	var mu sync.Mutex
	var sent []sentRequest
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []struct {
				FunctionDeclarations []struct{ Name string } `json:"functionDeclarations"`
			} `json:"tools"`
			Contents []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
			GenerationConfig map[string]any `json:"generationConfig"`
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil || json.Unmarshal(raw, &body) != nil {
			t.Errorf("decoding the request: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		s := sentRequest{body: raw, config: body.GenerationConfig}
		for _, ts := range body.Tools {
			for _, fd := range ts.FunctionDeclarations {
				s.tools = append(s.tools, fd.Name)
			}
		}
		if n := len(body.Contents); n > 0 {
			if parts := body.Contents[n-1].Parts; len(parts) > 0 {
				s.lastContent = parts[len(parts)-1].Text
			}
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
		// The model the scripted replies name.
		ExpectedModels: map[analysis.Role]string{analysis.RoleTriage: testModel, analysis.RoleEscalation: testModel},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &remediationv1alpha1.ModelDeployment{Endpoint: testEndpoint, Deployment: testModel,
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

// toolCallReply is a reply with one function call and a thought before it.
func toolCallReply(id, name, args string) string {
	return replyJSON("[" + `{"text":"checking","thought":true,"thoughtSignature":"dGhvdWdodA=="}` +
		"," + callPart(id, name, args) + "]")
}

// finalReply is a final answer with no proposal.
func finalReply() string {
	return replyJSON("[" + textPart(`{"summary":"S","hypotheses":[],"evidence":[],`+
		`"missingData":["M"],"uncertainty":"","proposal":null}`) + "]")
}

// A tool call on the last turn of an escalation reaches the loop as a tool call, which ends the analysis with
// StepLimit: the last turn offers the same tools as the others, so the adapter does not refuse the reply as
// malformed. As on every turn that offers tools, it sends no JSON mode and no response schema.
func TestLoopToolCallOnTheLastTurnIsStepLimit(t *testing.T) {
	out, sent, lt := runLoop(t, 1,
		toolCallReply("c1", tools.NameListEvents, shopArgs),
		toolCallReply("c2", tools.NameListEvents, shopArgs),
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
	for i, s := range sent {
		if _, ok := s.config["responseMimeType"]; ok {
			t.Errorf("turn %d offers tools and asks for JSON: %v", i, s.config)
		}
		if _, ok := s.config["responseJsonSchema"]; ok {
			t.Errorf("turn %d offers tools and sends a response schema: %v", i, s.config)
		}
	}
}

// A reply that names a tool the request did not offer is malformed output: the escalation hands it off as
// InvalidOutput and no tool runs. The reply was billed, so its usage is charged.
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

// The tool turn of a real loop run: the parts of the first reply (the thought, its signature and the call's)
// go back unchanged in the model turn, the result of a call without a wire ID carries no ID, the result of a
// call with one echoes it, and the final answer goes through the loop's own checks.
func TestLoopReplaysSignaturesAndKeepsLocalIDsOffTheWire(t *testing.T) {
	for _, tc := range []struct {
		name, wireID string
		wantID       bool
	}{{"a local ID", "", false}, {"a wire ID", "call-7", true}} {
		out, sent, lt := runLoop(t, 2, toolCallReply(tc.wireID, tools.NameListEvents, shopArgs), finalReply())
		if out.Reason != analysis.ReasonMissingData || out.Summary != "S" || len(sent) != 2 || len(lt.calls) != 1 {
			t.Fatalf("%s: reason %s, summary %q, %d model calls, %d tool calls", tc.name, out.Reason, out.Summary,
				len(sent), len(lt.calls))
		}
		var second struct {
			Contents []struct {
				Role  string            `json:"role"`
				Parts []json.RawMessage `json:"parts"`
			} `json:"contents"`
		}
		if err := json.Unmarshal(sent[1].body, &second); err != nil {
			t.Fatal(err)
		}
		if len(second.Contents) != 3 || second.Contents[1].Role != roleModel || second.Contents[2].Role != roleUser {
			t.Fatalf("%s: turns %+v", tc.name, second.Contents)
		}
		var first struct {
			Candidates []struct {
				Content struct {
					Parts []json.RawMessage `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal([]byte(toolCallReply(tc.wireID, tools.NameListEvents, shopArgs)), &first); err != nil {
			t.Fatal(err)
		}
		want, got := first.Candidates[0].Content.Parts, second.Contents[1].Parts
		if len(got) != len(want) || len(want) != 2 {
			t.Fatalf("%s: %d parts sent back, want %d", tc.name, len(got), len(want))
		}
		for i := range want {
			if string(got[i]) != string(want[i]) {
				t.Errorf("%s: part %d sent back as %s, want %s", tc.name, i, got[i], want[i])
			}
		}
		if !strings.Contains(string(want[0]), "dGhvdWdodA==") || !strings.Contains(string(want[1]), "thoughtSignature") {
			t.Errorf("%s: the fixture has no signatures to replay", tc.name)
		}
		var fr struct {
			FunctionResponse map[string]any `json:"functionResponse"`
		}
		if err := json.Unmarshal(second.Contents[2].Parts[0], &fr); err != nil || fr.FunctionResponse == nil {
			t.Fatalf("%s: the user turn starts with %s", tc.name, second.Contents[2].Parts[0])
		}
		id, hasID := fr.FunctionResponse["id"]
		if hasID != tc.wantID || tc.wantID && id != tc.wireID || fr.FunctionResponse["name"] != tools.NameListEvents {
			t.Errorf("%s: functionResponse %v", tc.name, fr.FunctionResponse)
		}
		if !tc.wantID && strings.Contains(string(sent[1].body), localIDPrefix) {
			t.Errorf("%s: a local ID was sent", tc.name)
		}
	}
}
