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

package analysis

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/tools"
)

// step is one scripted answer of the fake model: a completion, an error, or both (malformed output).
type step struct {
	c   providers.Completion
	err error
	// block makes the call wait until its context is done, then fail with the context's error.
	block bool
}

// scriptModel is the fake model of these tests: it answers each call with the next step and records every
// request. With no step left it answers with a retryable error.
type scriptModel struct {
	mu       sync.Mutex
	steps    []step
	requests []providers.CompletionRequest
}

func (m *scriptModel) Complete(ctx context.Context, req providers.CompletionRequest) (providers.Completion, error) {
	m.mu.Lock()
	m.requests = append(m.requests, req)
	if len(m.steps) == 0 {
		m.mu.Unlock()
		return providers.Completion{}, &providers.RetryableError{Err: context.DeadlineExceeded}
	}
	s := m.steps[0]
	m.steps = m.steps[1:]
	m.mu.Unlock()
	if s.block {
		<-ctx.Done()
		return providers.Completion{}, ctx.Err()
	}
	// As the Foundry adapter does: a reply that calls a tool the request did not offer is malformed output,
	// billed with its usage.
	for _, c := range s.c.ToolCalls {
		if !slices.ContainsFunc(req.Tools, func(d providers.ToolDefinition) bool { return d.Name == c.Name }) {
			return providers.Completion{Usage: s.c.Usage, Model: s.c.Model}, providers.ErrMalformedOutput
		}
	}
	return s.c, s.err
}

func (m *scriptModel) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func (m *scriptModel) reqs() []providers.CompletionRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]providers.CompletionRequest(nil), m.requests...)
}

// fakeModel is the model the fake replies name, and the one the fixture expects.
const fakeModel = "fake-1"

func reply(content string) step {
	return step{c: providers.Completion{Content: content, Model: fakeModel, Usage: providers.Usage{InputTokens: 100, OutputTokens: 50}}}
}

func toolReply(calls ...providers.ToolCall) step {
	return step{c: providers.Completion{ToolCalls: calls, Model: fakeModel, Usage: providers.Usage{InputTokens: 100, OutputTokens: 10}}}
}

func toolCall(id, name, args string) providers.ToolCall {
	return providers.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

type fakeBuilder struct {
	c   incidentcontext.Context
	err error
}

func (b *fakeBuilder) Build(ctx context.Context, _ *remediationv1alpha1.Incident) (*incidentcontext.Context, error) {
	if b.err != nil {
		return nil, b.err
	}
	c := b.c
	return &c, nil
}

// fakeTools answers every call with a fixed text and status (ok when empty) and records the calls it saw.
type fakeTools struct {
	mu     sync.Mutex
	text   string
	status tools.Status
	calls  []providers.ToolCall
	// extra are more tool names to offer.
	extra []string
	// err, when set, is the error of every call.
	err error
}

func (f *fakeTools) Definitions() []providers.ToolDefinition {
	names := slices.Concat([]string{tools.NameGetResource, tools.NameListEvents, tools.NameGetPodLogs,
		tools.NameGetDeliveryStatus}, f.extra)
	d := make([]providers.ToolDefinition, 0, len(names))
	for _, n := range names {
		d = append(d, providers.ToolDefinition{Name: n, Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)})
	}
	return d
}

func (f *fakeTools) Call(ctx context.Context, call providers.ToolCall) (tools.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if f.err != nil {
		return tools.Result{}, f.err
	}
	status := f.status
	if status == "" {
		status = tools.StatusOK
	}
	return tools.Result{Tool: call.Name, Status: status, Text: f.text, Values: []string{f.text}}, nil
}

func (f *fakeTools) seen() []providers.ToolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]providers.ToolCall(nil), f.calls...)
}

const (
	subjectNS  = "shop"
	subjectUID = "0b0e3c1c-7f4a-4c55-9c39-6a3e2d2f0a01"
)

func incident() *remediationv1alpha1.Incident {
	return &remediationv1alpha1.Incident{
		Spec: remediationv1alpha1.IncidentSpec{
			Fingerprint: "sha256:" + "ab" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd",
			Subject: remediationv1alpha1.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: subjectNS,
				Name: "web", UID: subjectUID},
			Reason: "OOMKilled",
		},
	}
}

// oomText is the answer of the fake tools that the tool evidence tests quote.
const oomText = "web-1 app exit code 137 reason OOMKilled\n"

// statusText is the text of the status block, which the valid result quotes.
const statusText = "pod web-1: " + statusValue + "\n"

// statusValue is the value from a source in the status block.
const statusValue = "container app terminated OOMKilled, memory limit 256Mi, 7 restarts"

func testContext() incidentcontext.Context {
	return incidentcontext.Context{
		Blocks: []incidentcontext.Block{
			{Section: incidentcontext.SectionSummary, Provenance: incidentcontext.ProvenanceIncident,
				Availability: incidentcontext.Available, Text: "Deployment shop/web, reason OOMKilled\n",
				Values: []string{"Deployment", "shop", "web", "OOMKilled"}},
			{Section: incidentcontext.SectionStatus, Provenance: incidentcontext.ProvenanceObjectStatus,
				Availability: incidentcontext.Available, Text: statusText, Values: []string{statusValue}},
		},
		Pods:     []string{"web-1"},
		Template: []incidentcontext.TemplateContainer{{Name: testContainer, MemoryLimit: new(resource.MustParse("256Mi"))}},
	}
}

// testContainer is the container validResult proposes a change for, and desiredLimit its new limit.
const (
	testContainer = "app"
	desiredLimit  = "512Mi"
)

// memoryPolicy is the policy configured for SetContainerMemoryLimit.
const memoryPolicy = "memory"

func testLimits() Limits {
	return Limits{
		MaxToolSteps: 4, MaxOutputTokens: 1024,
		Triage: &remediationv1alpha1.ModelDeployment{Endpoint: "https://a.services.ai.azure.com/", Deployment: "small",
			APIVersion: "v1", MaxTokensPerDay: new(int64(1_000_000))},
		Escalation: &remediationv1alpha1.ModelDeployment{Endpoint: "https://a.services.ai.azure.com/", Deployment: "large",
			APIVersion: "v1", MaxTokensPerDay: new(int64(1_000_000))},
	}
}

// validResult proposes raising the memory limit of container app, quoting the status block.
const validResult = `{"summary":"The container runs out of memory.","hypotheses":[{"description":"limit too low"}],` +
	`"evidence":[{"source":"c2","quote":"container app terminated OOMKilled"}],"missingData":[],"uncertainty":"none",` +
	`"proposal":{"action":{"type":"SetContainerMemoryLimit","adjustKedaMaxReplicas":null,` +
	`"setContainerMemoryLimit":{"container":"app","previous":"256Mi","desired":"512Mi"}},` +
	`"expectedEffect":"no more OOM kills","verificationChecks":["NoNewOOMKills","NoNewOOMKills"]}}`

const noProposal = `{"summary":"unclear","hypotheses":[],"evidence":[],"missingData":[],"uncertainty":"","proposal":null}`

// fixture holds an analyzer and its fakes.
type fixture struct {
	triage, escalation *scriptModel
	builder            *fakeBuilder
	tools              *fakeTools
	reserved           int
	reserveErr         error
	cfg                Config
}

func newFixture() *fixture {
	f := &fixture{triage: &scriptModel{}, escalation: &scriptModel{}, builder: &fakeBuilder{c: testContext()},
		tools: &fakeTools{text: "pod web-1 Running\n"}}
	f.cfg = Config{
		Builder: f.builder,
		Tools:   f.tools,
		Models: func(role Role, _ remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
			if role == RoleTriage {
				return f.triage, nil
			}
			return f.escalation, nil
		},
		Reserve: func(context.Context, time.Time) error {
			f.reserved++
			return f.reserveErr
		},
		Policies: map[remediationv1alpha1.ActionType]remediationv1alpha1.Name{
			remediationv1alpha1.ActionSetContainerMemoryLimit: memoryPolicy,
			remediationv1alpha1.ActionAdjustKedaMaxReplicas:   "keda",
		},
		ExpectedModels: map[Role]string{RoleTriage: fakeModel, RoleEscalation: fakeModel},
	}
	return f
}

func (f *fixture) analyze(t *testing.T, limits Limits) Outcome {
	t.Helper()
	a, err := New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Analyze(t.Context(), incident(), limits)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	return out
}
