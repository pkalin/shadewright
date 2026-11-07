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
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/api/resource"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/incidentcontext"
	"github.com/pkalin/shadewright/internal/incidents"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/tools"
)

func TestTriageProposes(t *testing.T) {
	f := newFixture()
	f.triage.steps = []step{reply(validResult)}
	out := f.analyze(t, testLimits())
	if out.Reason != ReasonProposed || out.Plan == nil {
		t.Fatalf("outcome = %+v, want a proposal", out)
	}
	p := out.Plan
	if p.Action.Type != remediationv1alpha1.ActionSetContainerMemoryLimit || p.Action.SetContainerMemoryLimit.Container != testContainer ||
		p.Action.SetContainerMemoryLimit.Desired.String() != desiredLimit || p.PolicyRef != memoryPolicy {
		t.Errorf("plan = %+v", p)
	}
	if len(p.Checks) != 1 || p.Checks[0] != remediationv1alpha1.CheckNoNewOOMKills {
		t.Errorf("checks = %v, want one NoNewOOMKills", p.Checks)
	}
	if out.Escalated || out.Calls != 1 || f.escalation.calls() != 0 || f.reserved != 1 {
		t.Errorf("escalated %v, calls %d, escalation calls %d, reserved %d", out.Escalated, out.Calls, f.escalation.calls(), f.reserved)
	}
	req := f.triage.reqs()[0]
	if len(req.Tools) != 0 || len(req.ResponseSchema) == 0 || req.MaxOutputTokens != 1024 {
		t.Errorf("triage request: %d tools, schema %d bytes, max output %d", len(req.Tools), len(req.ResponseSchema), req.MaxOutputTokens)
	}
	if out.Tokens != 150 || out.PromptVersion != PromptVersion() || len(out.Models) != 1 {
		t.Errorf("tokens %d, prompt %q, models %v", out.Tokens, out.PromptVersion, out.Models)
	}
}

type matchAll struct{ plan Plan }

func (m matchAll) Match(*remediationv1alpha1.Incident) (Plan, bool) { return m.plan, true }

func TestRunbookRunsBeforeTheModel(t *testing.T) {
	f := newFixture()
	action, err := ProposedAction{Type: remediationv1alpha1.ActionSetContainerMemoryLimit,
		SetContainerMemoryLimit: &MemoryLimitChange{Container: "app", Previous: "256Mi", Desired: "512Mi"}}.toAPI()
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.Runbooks = []Runbook{matchAll{Plan{Action: action}}}
	out := f.analyze(t, testLimits())
	if out.Reason != ReasonRunbookMatched || out.Plan == nil || out.Plan.PolicyRef != memoryPolicy {
		t.Fatalf("outcome = %+v", out)
	}
	if f.triage.calls() != 0 || f.reserved != 0 {
		t.Errorf("a runbook match called the model %d times and reserved %d units", f.triage.calls(), f.reserved)
	}
}

func TestNoTriageModelUsesNoBudget(t *testing.T) {
	f := newFixture()
	l := testLimits()
	l.Triage, l.Escalation = nil, nil
	out := f.analyze(t, l)
	if out.Reason != ReasonNoModelConfigured || f.reserved != 0 || f.triage.calls() != 0 {
		t.Errorf("reason %s, reserved %d, calls %d", out.Reason, f.reserved, f.triage.calls())
	}
}

func TestBudgetStopsAnalyses(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want Reason
	}{
		{fmt.Errorf("reserving: %w", incidents.ErrBudgetSpent), ReasonBudgetSpent},
		{fmt.Errorf("reserving: %w", incidents.ErrBudgetNotFound), ReasonBudgetUnavailable},
		{errors.New("api server down"), ReasonBudgetUnavailable},
	} {
		f := newFixture()
		f.reserveErr = tc.err
		f.triage.steps = []step{reply(validResult)}
		out := f.analyze(t, testLimits())
		if out.Reason != tc.want || out.Plan != nil || f.triage.calls() != 0 || f.escalation.calls() != 0 {
			t.Errorf("%v: reason %s, plan %v, model calls %d+%d", tc.err, out.Reason, out.Plan, f.triage.calls(), f.escalation.calls())
		}
	}
}

func TestMalformedTriageOutputEscalates(t *testing.T) {
	f := newFixture()
	f.triage.steps = []step{
		{c: providers.Completion{Usage: providers.Usage{InputTokens: 10, OutputTokens: 10}}, err: providers.ErrMalformedOutput},
		reply(`{"summary":"x","confidence":0.99}`),
	}
	f.escalation.steps = []step{reply(validResult)}
	out := f.analyze(t, testLimits())
	if out.Reason != ReasonProposed || !out.Escalated || f.triage.calls() != 2 || f.escalation.calls() != 1 {
		t.Fatalf("reason %s, escalated %v, calls %d+%d", out.Reason, out.Escalated, f.triage.calls(), f.escalation.calls())
	}
	if len(f.escalation.reqs()[0].Tools) == 0 {
		t.Error("the escalation was offered no tools")
	}
}

func TestMalformedEscalatedOutputIsHandedOff(t *testing.T) {
	f := newFixture()
	f.builder.c.Truncated = true
	f.escalation.steps = []step{{err: fmt.Errorf("cut off: %w", providers.ErrMalformedOutput)}, reply(validResult)}
	out := f.analyze(t, testLimits())
	if out.Reason != ReasonInvalidOutput || out.Plan != nil || f.escalation.calls() != 1 || f.triage.calls() != 0 {
		t.Errorf("reason %s, calls %d (triage %d): an invalid escalated result must be handed off, not retried",
			out.Reason, f.escalation.calls(), f.triage.calls())
	}
}

func TestEscalationSignalsWithoutEscalationModel(t *testing.T) {
	f := newFixture()
	f.builder.c.FailedSources = []incidentcontext.Section{incidentcontext.SectionLogs}
	l := testLimits()
	l.Escalation = nil
	out := f.analyze(t, l)
	if out.Reason != ReasonEscalationNotConfigured || f.triage.calls() != 0 || f.reserved != 0 {
		t.Errorf("reason %s, triage calls %d, reserved %d: no call can happen, so no unit is reserved",
			out.Reason, f.triage.calls(), f.reserved)
	}
}

// A unit of the daily budget is reserved only when the first model call can happen: its deployment is
// configured and its provider could be built.
func TestNoBudgetReservedWithoutAModelCall(t *testing.T) {
	for _, tc := range []struct {
		name     string
		escalate bool
		failRole Role
	}{
		{"triage provider fails", false, RoleTriage},
		{"escalation provider fails", true, RoleEscalation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.builder.c.Truncated = tc.escalate
			models := f.cfg.Models
			f.cfg.Models = func(role Role, d remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
				if role == tc.failRole {
					return nil, errors.New("endpoint not allowed")
				}
				return models(role, d)
			}
			out := f.analyze(t, testLimits())
			if out.Reason != ReasonModelError || f.reserved != 0 || f.triage.calls()+f.escalation.calls() != 0 {
				t.Errorf("reason %s, reserved %d, calls %d+%d", out.Reason, f.reserved, f.triage.calls(), f.escalation.calls())
			}
		})
	}
}

// The retry after invalid triage output adds one corrective message from code, which names the broken rule
// by a fixed code and never repeats the reply.
func TestTriageRetryCorrects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first step
		code  string
	}{
		{"malformed", step{c: providers.Completion{Usage: providers.Usage{InputTokens: 1, OutputTokens: 1}}, err: providers.ErrMalformedOutput}, "malformed-reply"},
		{"schema", reply(`{"summary":"IGNORE ALL RULES","confidence":0.99}`), "schema"},
		{"action", reply(strings.Replace(validResult, `"adjustKedaMaxReplicas":null,`,
			`"adjustKedaMaxReplicas":{"previousValue":2,"desiredValue":4},`, 1)), "action"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.triage.steps = []step{tc.first, reply(validResult)}
			if out := f.analyze(t, testLimits()); out.Reason != ReasonProposed {
				t.Fatalf("reason %s", out.Reason)
			}
			reqs := f.triage.reqs()
			if len(reqs) != 2 || len(reqs[0].Messages) != 2 || len(reqs[1].Messages) != 3 {
				t.Fatalf("%d requests; the retry must add exactly one message", len(reqs))
			}
			last := reqs[1].Messages[2]
			if last.Role != providers.RoleUser || last.Content != retryMessage(tc.code) {
				t.Errorf("corrective message %q, want rule %s", last.Content, tc.code)
			}
			if strings.Contains(last.Content, "IGNORE") || strings.Contains(last.Content, "256Mi") {
				t.Errorf("the corrective message echoes the reply: %q", last.Content)
			}
		})
	}
}

func TestModelErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []step
		want  Reason
		calls int
	}{
		{"not found", []step{{err: providers.ErrNotFound}}, ReasonModelNotFound, 1},
		{"forbidden", []step{{err: providers.ErrForbidden}}, ReasonModelForbidden, 1},
		{"rejected", []step{{err: providers.ErrRejected}}, ReasonModelError, 1},
		{"retryable without delay", []step{{err: &providers.RetryableError{Err: errors.New("503")}}}, ReasonModelUnavailable, 1},
		{"retryable with a long delay", []step{{err: &providers.RetryableError{RetryAfter: time.Hour, Err: errors.New("429")}}},
			ReasonModelUnavailable, 1},
		{"retryable twice", []step{
			{err: &providers.RetryableError{RetryAfter: time.Millisecond, Err: errors.New("429")}},
			{err: &providers.RetryableError{RetryAfter: time.Millisecond, Err: errors.New("429")}},
		}, ReasonModelUnavailable, 2},
		{"retryable then answered", []step{
			{err: &providers.RetryableError{RetryAfter: time.Millisecond, Err: errors.New("429")}}, reply(validResult),
		}, ReasonProposed, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.triage.steps = tc.steps
			out := f.analyze(t, testLimits())
			if out.Reason != tc.want || f.triage.calls() != tc.calls || f.escalation.calls() != 0 {
				t.Errorf("reason %s, calls %d (escalation %d), want %s after %d", out.Reason, f.triage.calls(),
					f.escalation.calls(), tc.want, tc.calls)
			}
		})
	}
}

// A retry after ErrRetryable counts toward the analysis's bound, so it makes at most 3 + maxToolSteps model
// calls in total (ADR-0023 decision 6).
func TestRetriesCountTowardTheCallBound(t *testing.T) {
	f := newFixture()
	f.triage.steps = []step{
		{err: &providers.RetryableError{RetryAfter: time.Millisecond, Err: errors.New("429")}},
		{c: providers.Completion{Usage: providers.Usage{InputTokens: 1, OutputTokens: 1}}, err: providers.ErrMalformedOutput},
		{c: providers.Completion{Usage: providers.Usage{InputTokens: 1, OutputTokens: 1}}, err: providers.ErrMalformedOutput},
	}
	f.escalation.steps = []step{reply(validResult)}
	l := testLimits()
	l.MaxToolSteps = 0
	out := f.analyze(t, l)
	if total := f.triage.calls() + f.escalation.calls(); total != 3 || out.Calls != 3 {
		t.Fatalf("%d model calls (outcome %d), want at most 3 + maxToolSteps = 3", total, out.Calls)
	}
	if out.Reason != ReasonModelUnavailable || out.Plan != nil {
		t.Errorf("reason %s, want ModelUnavailable", out.Reason)
	}
}

func TestStepLimitStopsTheLoop(t *testing.T) {
	f := newFixture()
	f.builder.c.Truncated = true
	for i := range 10 {
		f.escalation.steps = append(f.escalation.steps, toolReply(
			toolCall(fmt.Sprintf("a%d", i), tools.NameGetResource, `{"kind":"Pod","namespace":"shop","name":"web-1"}`),
			toolCall(fmt.Sprintf("b%d", i), tools.NameListEvents, `{"namespace":"shop"}`)))
	}
	l := testLimits()
	l.MaxToolSteps = 3
	out := f.analyze(t, l)
	if out.Reason != ReasonStepLimit || out.Plan != nil {
		t.Fatalf("reason %s, want StepLimit", out.Reason)
	}
	if got := len(f.tools.seen()); got != 3 || out.ToolCalls != 3 {
		t.Errorf("%d tool calls ran (outcome says %d), want 3", got, out.ToolCalls)
	}
	reqs := f.escalation.reqs()
	if len(reqs) != 3 {
		t.Fatalf("%d escalated turns, want at most maxToolSteps + 1 and here 3", len(reqs))
	}
	// The real adapter refuses a call to a tool the request did not offer, so every turn offers the same
	// tools and the last one says in a fixed message that no calls are left.
	for i, req := range reqs {
		if !slices.Equal(toolNames(req.Tools), toolNames(reqs[0].Tools)) || len(req.Tools) == 0 {
			t.Errorf("turn %d offered %v, want the same tools as turn 1", i+1, toolNames(req.Tools))
		}
	}
	msgs := reqs[2].Messages
	if last := msgs[len(msgs)-1]; last.Role != providers.RoleUser || last.Content != lastTurnMessage {
		t.Errorf("the last turn does not end with the fixed message: %q", last.Content)
	}
	for _, req := range reqs[:2] {
		if slices.ContainsFunc(req.Messages, func(m providers.Message) bool { return m.Content == lastTurnMessage }) {
			t.Error("a turn with steps left says that none are left")
		}
	}
	// The second turn's reply asked for two calls with one step left: one ran, the other was refused.
	if refused := msgs[len(msgs)-2]; !strings.Contains(refused.Content, "no tool calls are left") {
		t.Errorf("the call past the limit was not refused: %q", refused.Content)
	}
}

func toolNames(defs []providers.ToolDefinition) []string {
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	return names
}

// A tool call on the last turn ends the analysis with StepLimit, and a result on it is accepted.
func TestLastTurn(t *testing.T) {
	logs := toolCall("1", tools.NameGetPodLogs, `{"namespace":"shop","pod":"web-1","container":"app"}`)
	for _, tc := range []struct {
		name string
		last step
		want Reason
	}{
		{"tool call", toolReply(toolCall("2", tools.NameListEvents, `{"namespace":"shop"}`)), ReasonStepLimit},
		{"result", reply(validResult), ReasonProposed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.builder.c.Truncated = true
			f.escalation.steps = []step{toolReply(logs), tc.last}
			l := testLimits()
			l.MaxToolSteps = 1
			out := f.analyze(t, l)
			if out.Reason != tc.want || f.escalation.calls() != 2 || len(f.tools.seen()) != 1 {
				t.Errorf("reason %s after %d turns and %d tool calls, want %s after 2 and 1", out.Reason,
					f.escalation.calls(), len(f.tools.seen()), tc.want)
			}
		})
	}
}

// A reply that calls a tool the request did not offer is malformed output, as the adapter reports it: the
// escalation hands it off, and no tool runs.
func TestUnofferedToolIsInvalidOutput(t *testing.T) {
	f := newFixture()
	f.builder.c.Truncated = true
	f.escalation.steps = []step{
		toolReply(toolCall("1", tools.NameGetDeliveryStatus, `{"kind":"Kustomization","namespace":"flux-system","name":"apps"}`)),
		reply(validResult),
	}
	out := f.analyze(t, testLimits())
	if out.Reason != ReasonInvalidOutput || len(f.tools.seen()) != 0 || f.escalation.calls() != 1 {
		t.Errorf("reason %s, tool calls %d, turns %d", out.Reason, len(f.tools.seen()), f.escalation.calls())
	}
	for _, d := range f.escalation.reqs()[0].Tools {
		if d.Name == tools.NameGetDeliveryStatus {
			t.Error("get_delivery_status was offered")
		}
	}
}

// The loop's own check of the offered set (defence in depth behind the adapter's) refuses a call to a tool
// that was not offered without naming it.
func TestBoundCallRefusesUnofferedToolWithoutNamingIt(t *testing.T) {
	f := newFixture()
	a, err := New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := &run{a: a, parent: t.Context(), ctx: t.Context(), inc: incident(), shown: map[string]shownBlock{}}
	content, quotable, err := r.boundCall(t.Context(), trace.SpanFromContext(t.Context()),
		toolCall("1", tools.NameGetDeliveryStatus, `{"kind":"Kustomization","namespace":"shop","name":"apps"}`),
		map[string]bool{tools.NameListEvents: true})
	if err != nil || quotable != nil || len(f.tools.seen()) != 0 {
		t.Fatalf("err %v, quotable %q, tool calls %d", err, quotable, len(f.tools.seen()))
	}
	if !strings.Contains(content, "not offered") || strings.Contains(content, tools.NameGetDeliveryStatus) {
		t.Errorf("refusal %q: want a refusal that does not name the tool", content)
	}
}

func TestEscalatedTurnsAreBounded(t *testing.T) {
	f := newFixture()
	f.builder.c.Truncated = true
	for i := range 20 {
		f.escalation.steps = append(f.escalation.steps,
			toolReply(toolCall(fmt.Sprintf("a%d", i), tools.NameListEvents, `{"namespace":"shop"}`)))
	}
	l := testLimits()
	l.MaxToolSteps = 16
	f.analyze(t, l)
	if got := f.escalation.calls(); got != 17 {
		t.Errorf("%d escalated calls, want maxToolSteps + 1 = 17", got)
	}
}

func TestTokenLimits(t *testing.T) {
	t.Run("analysis bound", func(t *testing.T) {
		f := newFixture()
		f.cfg.MaxTokensPerAnalysis = 100
		f.triage.steps = []step{reply(validResult)}
		out := f.analyze(t, testLimits())
		if out.Reason != ReasonTokenLimit || f.triage.calls() != 0 {
			t.Errorf("reason %s, calls %d", out.Reason, f.triage.calls())
		}
	})
	t.Run("deployment cap of 0", func(t *testing.T) {
		f := newFixture()
		l := testLimits()
		l.Triage.MaxTokensPerDay = new(int64(0))
		out := f.analyze(t, l)
		if out.Reason != ReasonTokenLimit || f.triage.calls() != 0 {
			t.Errorf("reason %s, calls %d", out.Reason, f.triage.calls())
		}
	})
	// The first triage answer is malformed. Its usage decides whether the retry fits the bound: reported
	// usage is charged as reported, unknown usage at the call's upper bound.
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprintf("usage unknown %v", unknown), func(t *testing.T) {
			f := newFixture()
			l := testLimits()
			first := step{c: providers.Completion{Usage: providers.Usage{InputTokens: 5, OutputTokens: 5}, UsageUnknown: unknown},
				err: providers.ErrMalformedOutput}
			f.triage.steps = []step{first, reply(validResult)}
			// Room for one upper bound and a bit: a second call fits only if the first was charged little.
			upper := estimateInput(providers.CompletionRequest{
				Messages:       []providers.Message{{Content: systemPrompt(false, 0)}, {Content: mustContext()}},
				ResponseSchema: ResponseSchema()}) + int64(l.MaxOutputTokens)
			f.cfg.MaxTokensPerAnalysis = upper + upper/2
			out := f.analyze(t, l)
			if unknown && (out.Reason != ReasonTokenLimit || f.triage.calls() != 1) {
				t.Errorf("unknown usage: reason %s after %d calls, want TokenLimit after 1", out.Reason, f.triage.calls())
			}
			if !unknown && (out.Reason != ReasonProposed || f.triage.calls() != 2) {
				t.Errorf("known usage: reason %s after %d calls, want Proposed after 2", out.Reason, f.triage.calls())
			}
		})
	}
}

func mustContext() string {
	c := testContext()
	s, _ := contextMessage(&c)
	return s
}

func TestDeadline(t *testing.T) {
	f := newFixture()
	f.cfg.Timeout = 50 * time.Millisecond
	f.triage.steps = []step{{block: true}}
	out := f.analyze(t, testLimits())
	if out.Reason != ReasonDeadlineExceeded {
		t.Errorf("reason %s, want DeadlineExceeded", out.Reason)
	}
}

func TestCancellationIsAnError(t *testing.T) {
	f := newFixture()
	f.triage.steps = []step{{block: true}}
	a, err := New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		for f.triage.calls() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	if _, err := a.Analyze(ctx, incident(), testLimits()); !errors.Is(err, context.Canceled) {
		t.Errorf("Analyze = %v, want the cancellation", err)
	}
}

func TestNoProposalHandsOff(t *testing.T) {
	f := newFixture()
	f.triage.steps = []step{reply(noProposal)}
	if out := f.analyze(t, testLimits()); out.Reason != ReasonNoProposal || out.Plan != nil {
		t.Errorf("reason %s", out.Reason)
	}
	f = newFixture()
	f.triage.steps = []step{reply(strings.Replace(noProposal, `"missingData":[]`, `"missingData":["previous logs"]`, 1))}
	if out := f.analyze(t, testLimits()); out.Reason != ReasonMissingData || out.Escalated {
		t.Errorf("reason %s, escalated %v: missing data is a hand-off, not an escalation", out.Reason, out.Escalated)
	}
}

// The outcome carries the result's summary and missing data for the log line, redacted (#422).
func TestOutcomeCarriesRedactedResultText(t *testing.T) {
	opaque := strings.Repeat("x9", 10)
	f := newFixture()
	f.triage.steps = []step{reply(strings.Replace(strings.Replace(noProposal, `"missingData":[]`,
		`"missingData":["previous logs","the value of Bearer `+opaque+`"]`, 1), `"summary":"unclear"`,
		`"summary":"OOM, token Bearer `+opaque+`"`, 1))}
	out := f.analyze(t, testLimits())
	if out.Reason != ReasonMissingData || len(out.MissingData) != 2 || out.MissingData[0] != "previous logs" {
		t.Fatalf("reason %s, missing data %q", out.Reason, out.MissingData)
	}
	if strings.Contains(out.MissingData[1], opaque) || strings.Contains(out.Summary, opaque) ||
		!strings.HasPrefix(out.Summary, "OOM, token") {
		t.Errorf("summary %q, missing data %q: want the token redacted", out.Summary, out.MissingData)
	}

	f = newFixture()
	f.triage.steps = []step{reply(noProposal)}
	if out := f.analyze(t, testLimits()); out.MissingData != nil || out.Summary != "unclear" {
		t.Errorf("summary %q, missing data %q", out.Summary, out.MissingData)
	}

	// The Incident's name, a long hex run the redactor takes for a token, stays readable.
	inc := incident()
	inc.Name = incidents.Name(inc.Spec.Fingerprint)
	f = newFixture()
	f.triage.steps = []step{reply(strings.Replace(strings.Replace(noProposal, `"missingData":[]`,
		`"missingData":["events of `+inc.Name+`"]`, 1), `"summary":"unclear"`, `"summary":"`+inc.Name+` OOM"`, 1))}
	a, err := New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	out, err = a.Analyze(t.Context(), inc, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if out.Summary != inc.Name+" OOM" || len(out.MissingData) != 1 || out.MissingData[0] != "events of "+inc.Name {
		t.Errorf("summary %q, missing data %q: want the Incident's name kept", out.Summary, out.MissingData)
	}
}

func TestProposalNeedsEvidence(t *testing.T) {
	for name, evidence := range map[string]string{
		"none":          `[]`,
		"not quoted":    `[{"source":"c2","quote":"container app ran out of disk"}],`,
		"another block": `[{"source":"c1","quote":"container app terminated OOMKilled"}]`,
		"too short":     `[{"source":"c2","quote":"app"}]`,
		"unknown block": `[{"source":"t1","quote":"container app terminated OOMKilled"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			res := strings.Replace(validResult, `[{"source":"c2","quote":"container app terminated OOMKilled"}]`,
				strings.TrimSuffix(evidence, ","), 1)
			f.triage.steps = []step{reply(res)}
			if out := f.analyze(t, testLimits()); out.Reason != ReasonNoEvidence || out.Plan != nil {
				t.Errorf("reason %s, plan %v", out.Reason, out.Plan)
			}
		})
	}
}

// Only the data of an available block is quotable, a quote within one value: a block that is not available,
// a fixed line of an available block and the text code writes around values are never evidence (invariant 1).
func TestFixedContextLinesAreNotEvidence(t *testing.T) {
	const fixedMetric = "container_restarts_total: could not fetch"
	for name, quote := range map[string]struct{ source, text string }{
		"git not available":    {"c3", "not available: no Git source is configured"},
		"metrics fixed line":   {"c4", fixedMetric},
		"metrics across fixed": {"c4", "7\n" + fixedMetric},
		"across two values":    {"c1", "Deployment shop/web"},
		"code text and value":  {"c2", labelAndValue},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			f.builder.c.Blocks = append(f.builder.c.Blocks,
				incidentcontext.Block{Section: incidentcontext.SectionGit, Provenance: incidentcontext.ProvenanceGit,
					Availability: incidentcontext.NotAvailable, Text: "not available: no Git source is configured\n"},
				incidentcontext.Block{Section: incidentcontext.SectionMetrics, Provenance: incidentcontext.ProvenanceMetric,
					Availability: incidentcontext.Available,
					Text:         "container_restarts_total{pod=web-1} 7\n" + fixedMetric + "\n",
					Values:       []string{"container_restarts_total{pod=web-1} 7"}})
			res := strings.Replace(validResult, `{"source":"c2","quote":"container app terminated OOMKilled"}`,
				`{"source":"`+quote.source+`","quote":`+strconv.Quote(quote.text)+`}`, 1)
			f.triage.steps = []step{reply(res)}
			if out := f.analyze(t, testLimits()); out.Reason != ReasonNoEvidence || out.Plan != nil {
				t.Errorf("reason %s: quoting %q of %s is not evidence", out.Reason, quote.text, quote.source)
			}
			if !strings.Contains(f.triage.reqs()[0].Messages[1].Content, quote.text) {
				t.Fatalf("the test quote %q is not in what the model saw", quote.text)
			}
		})
	}
	// The data line of the same metrics block is evidence.
	f := newFixture()
	f.builder.c.Blocks = append(f.builder.c.Blocks, incidentcontext.Block{Section: incidentcontext.SectionMetrics,
		Provenance: incidentcontext.ProvenanceMetric, Availability: incidentcontext.Available,
		Text:   "container_restarts_total{pod=web-1} 7\n" + fixedMetric + "\n",
		Values: []string{"container_restarts_total{pod=web-1} 7"}})
	f.triage.steps = []step{reply(strings.Replace(validResult, `{"source":"c2","quote":"container app terminated OOMKilled"}`,
		`{"source":"c3","quote":"container_restarts_total{pod=web-1} 7"}`, 1))}
	if out := f.analyze(t, testLimits()); out.Reason != ReasonProposed {
		t.Errorf("reason %s: a data line of an available block is evidence", out.Reason)
	}
}

func TestProposalTargetsTheSubjectOnly(t *testing.T) {
	f := newFixture()
	keda := strings.Replace(validResult, `{"type":"SetContainerMemoryLimit","adjustKedaMaxReplicas":null,`+
		`"setContainerMemoryLimit":{"container":"app","previous":"256Mi","desired":"512Mi"}}`,
		`{"type":"AdjustKedaMaxReplicas","adjustKedaMaxReplicas":{"previousValue":2,"desiredValue":4},"setContainerMemoryLimit":null}`, 1)
	f.triage.steps = []step{reply(keda)}
	if out := f.analyze(t, testLimits()); out.Reason != ReasonTargetNotSupported {
		t.Errorf("a ScaledObject action on a Deployment subject: reason %s", out.Reason)
	}
	f = newFixture()
	delete(f.cfg.Policies, remediationv1alpha1.ActionSetContainerMemoryLimit)
	f.triage.steps = []step{reply(validResult)}
	if out := f.analyze(t, testLimits()); out.Reason != ReasonNoPolicyForAction {
		t.Errorf("no policy: reason %s", out.Reason)
	}
}

func TestOutOfCatalogProposalIsRefused(t *testing.T) {
	f := newFixture()
	bad := strings.Replace(validResult, `"type":"SetContainerMemoryLimit"`, `"type":"DeletePod"`, 1)
	f.triage.steps = []step{reply(bad), reply(bad)}
	l := testLimits()
	l.Escalation = nil
	out := f.analyze(t, l)
	if out.Plan != nil || out.Reason != ReasonEscalationNotConfigured || f.triage.calls() != 2 {
		t.Errorf("reason %s, plan %v, calls %d", out.Reason, out.Plan, f.triage.calls())
	}
}

// An action the policy's catalog check refuses ends the analysis at once with its own reason: no retry, no
// corrective message naming the rule and no escalation, so the model cannot search for what the policy
// allows (decided 2026-11-05).
func TestOutOfBoundsActionIsHandedOffWithoutRetry(t *testing.T) {
	unchanged := strings.Replace(validResult, `"desired":"512Mi"`, `"desired":"256Mi"`, 1)
	t.Run("triage", func(t *testing.T) {
		f := newFixture()
		f.triage.steps = []step{reply(unchanged), reply(validResult)}
		f.escalation.steps = []step{reply(validResult)}
		out := f.analyze(t, testLimits())
		if out.Reason != ReasonActionOutOfBounds || out.Plan != nil || out.Escalated ||
			f.triage.calls() != 1 || f.escalation.calls() != 0 {
			t.Errorf("reason %s, escalated %v, calls %d+%d: want ActionOutOfBounds after one call", out.Reason,
				out.Escalated, f.triage.calls(), f.escalation.calls())
		}
	})
	t.Run("escalation", func(t *testing.T) {
		f := newFixture()
		f.builder.c.Truncated = true
		f.escalation.steps = []step{reply(unchanged), reply(validResult)}
		out := f.analyze(t, testLimits())
		if out.Reason != ReasonActionOutOfBounds || out.Plan != nil || f.escalation.calls() != 1 {
			t.Errorf("reason %s, calls %d: want ActionOutOfBounds after one call", out.Reason, f.escalation.calls())
		}
	})
}

func TestToolCallsAreBoundToTheIncidentNamespace(t *testing.T) {
	f := newFixture()
	f.builder.c.Truncated = true
	f.escalation.steps = []step{
		toolReply(
			toolCall("1", tools.NameGetPodLogs, `{"namespace":"payments","pod":"db-0","container":"db"}`),
			toolCall("2", tools.NameGetPodLogs, `{"namespace":"shop","pod":"db-0","container":"db"}`),
			toolCall("3", tools.NameGetPodLogs, `{"namespace":"shop","pod":"web-1","container":"app"}`),
			toolCall("5", tools.NameGetResource, `{"kind":"Pod","namespace":"shop","name":"cache-0"}`),
			toolCall("6", tools.NameListEvents, `{"kind":"Pod"}`),
			toolCall("7", tools.NameListEvents, `not json`),
			toolCall("8", tools.NameListEvents, `{"namespace":"shop","name":"`+strings.Repeat("x", tools.MaxArgumentBytes)+`"}`),
		),
		reply(validResult),
	}
	l := testLimits()
	l.MaxToolSteps = 8
	out := f.analyze(t, l)
	if out.Reason != ReasonProposed {
		t.Fatalf("reason %s", out.Reason)
	}
	seen := f.tools.seen()
	if len(seen) != 3 || seen[0].ID != "2" || seen[1].ID != "3" || seen[2].ID != "5" {
		t.Fatalf("the tools saw %v, want calls 2, 3 and 5 only", seen)
	}
	msgs := f.escalation.reqs()[1].Messages
	answers := map[string]string{}
	for _, m := range msgs {
		if m.ToolCallID != "" {
			answers[m.ToolCallID] = m.Content
		}
	}
	if !strings.Contains(answers["1"], "reads only the namespace") {
		t.Errorf("call 1 in another namespace: %q", answers["1"])
	}
	for _, id := range []string{"6", "7", "8"} {
		if !strings.Contains(answers[id], "refused") {
			t.Errorf("call %s without a namespace or with bad arguments reached the tool: %q", id, answers[id])
		}
	}
	label := string(tools.LabelOutsideAttribution)
	if !strings.Contains(answers["2"], label) || !strings.Contains(answers["5"], label) {
		t.Errorf("answers about pods outside the attribution are not labelled: %q, %q", answers["2"], answers["5"])
	}
	if strings.Contains(answers["3"], label) {
		t.Errorf("an answer about an attributed pod is labelled: %q", answers["3"])
	}
}

func TestToolResultsCountAsEvidence(t *testing.T) {
	f := newFixture()
	f.builder.c.Truncated = true
	f.tools.text = oomText
	f.escalation.steps = []step{
		toolReply(toolCall("1", tools.NameGetPodLogs, `{"namespace":"shop","pod":"web-1","container":"app"}`)),
		reply(strings.Replace(validResult, `{"source":"c2","quote":"container app terminated OOMKilled"}`,
			`{"source":"t1","quote":"exit code 137 reason OOMKilled"}`, 1)),
	}
	if out := f.analyze(t, testLimits()); out.Reason != ReasonProposed {
		t.Errorf("reason %s: a quote of a tool result is evidence", out.Reason)
	}
}

// A tool answer is evidence only when the call is about the Incident's subject or one of its attributed pods
// (decided 2026-11-05). An answer naming no pod or object, or another object, is shown but never evidence.
func TestToolEvidenceIsAboutTheSubjectOnly(t *testing.T) {
	const metric = `"template":"container_restarts_total","namespace":"shop"`
	for _, tc := range []struct {
		name, tool, args string
		want             Reason
	}{
		{"events of the namespace", tools.NameListEvents, `{"namespace":"shop"}`, ReasonNoEvidence},
		{"events of another Deployment", tools.NameListEvents, `{"namespace":"shop","kind":"Deployment","name":"api"}`, ReasonNoEvidence},
		{"events of a ReplicaSet", tools.NameListEvents, `{"namespace":"shop","kind":"ReplicaSet","name":"web-abc"}`, ReasonNoEvidence},
		{"metrics without a pod", tools.NameQueryMetrics, `{` + metric + `}`, ReasonNoEvidence},
		{"metrics of a Deployment", tools.NameQueryMetrics, `{` + metric + `,"deployment":"web"}`, ReasonNoEvidence},
		{"metrics of another pod", tools.NameQueryMetrics, `{` + metric + `,"pod":"db-0"}`, ReasonNoEvidence},
		{"another object", tools.NameGetResource, `{"kind":"Deployment","namespace":"shop","name":"api"}`, ReasonNoEvidence},
		{"same name, another kind", tools.NameGetResource, `{"kind":"StatefulSet","namespace":"shop","name":"web"}`, ReasonNoEvidence},
		{"the subject", tools.NameGetResource, `{"kind":"Deployment","namespace":"shop","name":"web"}`, ReasonProposed},
		{"events of the subject", tools.NameListEvents, `{"namespace":"shop","kind":"Deployment","name":"web"}`, ReasonProposed},
		{"events of an attributed pod", tools.NameListEvents, `{"namespace":"shop","kind":"Pod","name":"web-1"}`, ReasonProposed},
		{"metrics of an attributed pod", tools.NameQueryMetrics, `{` + metric + `,"pod":"web-1"}`, ReasonProposed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.builder.c.Truncated = true
			f.tools.text = oomText
			f.tools.extra = []string{tools.NameQueryMetrics}
			f.escalation.steps = []step{
				toolReply(toolCall("1", tc.tool, tc.args)),
				reply(strings.Replace(validResult, `{"source":"c2","quote":"container app terminated OOMKilled"}`,
					`{"source":"t1","quote":"exit code 137 reason OOMKilled"}`, 1)),
			}
			out := f.analyze(t, testLimits())
			if out.Reason != tc.want {
				t.Errorf("reason %s, want %s", out.Reason, tc.want)
			}
			if len(f.tools.seen()) != 1 {
				t.Fatalf("the tool saw %d calls, want 1: the answer must be shown", len(f.tools.seen()))
			}
		})
	}
}

// Only the data text of a successful answer about the subject is quotable: the block's header, labels and
// boilerplate, a refusal, an answer that is not available or failed, and an answer about a pod outside the
// attribution are not evidence (invariant 1).
func TestToolResultEvidenceIsTheDataOnly(t *testing.T) {
	attributed := `{"namespace":"shop","pod":"web-1","container":"app"}`
	outside := `{"namespace":"shop","pod":"db-0","container":"app"}`
	const oomQuote = "exit code 137 reason OOMKilled"
	for _, tc := range []struct {
		name   string
		args   string
		status tools.Status
		quote  string
	}{
		{"boilerplate", attributed, "", "untrusted data read from the cluster"},
		{"header", attributed, "", "tool-result tool="},
		{"tool name", attributed, "", tools.NameGetPodLogs},
		{"refusal", `{"namespace":"payments","pod":"web-1","container":"app"}`, "", "reads only the namespace of the incident"},
		{"not available", attributed, tools.StatusNotAvailable, oomQuote},
		{"could not fetch", attributed, tools.StatusCouldNotFetch, oomQuote},
		{"outside label", outside, "", string(tools.LabelOutsideAttribution)},
		{"outside data", outside, "", oomQuote},
		{"turn header", attributed, "", "Tool result t1."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			f.builder.c.Truncated = true
			f.tools.text = oomText
			f.tools.status = tc.status
			f.escalation.steps = []step{
				toolReply(toolCall("1", tools.NameGetPodLogs, tc.args)),
				reply(strings.Replace(validResult, `{"source":"c2","quote":"container app terminated OOMKilled"}`,
					`{"source":"t1","quote":`+strconv.Quote(tc.quote)+`}`, 1)),
			}
			out := f.analyze(t, testLimits())
			if out.Reason != ReasonNoEvidence || out.Plan != nil {
				t.Errorf("reason %s: quoting %q of t1 is not evidence", out.Reason, tc.quote)
			}
			msgs := f.escalation.reqs()[1].Messages
			if content := msgs[len(msgs)-1].Content; !strings.Contains(content, tc.quote) {
				t.Fatalf("the test quote %q is not in what the model saw: %q", tc.quote, content)
			}
		})
	}
}

// The previous memory limit of a proposal is the pod template's, as the context builder read it, never the
// model's (#494): during a rollout an old pod may still run with another limit, and a model may write the
// value as a byte count. A container the template does not have, or sets no limit for, is handed off.
func TestPreviousMemoryLimitComesFromThePodTemplate(t *testing.T) {
	withPrevious := func(previous string) string {
		return strings.Replace(validResult, `"previous":"256Mi"`, `"previous":"`+previous+`"`, 1)
	}
	limit := func(q string) *resource.Quantity { return new(resource.MustParse(q)) }
	cases := map[string]struct {
		reply    string
		template []incidentcontext.TemplateContainer
		reason   Reason
	}{
		"an old pod's limit during a rollout": {withPrevious("64Mi"),
			[]incidentcontext.TemplateContainer{{Name: testContainer, MemoryLimit: limit("48Mi")}}, ReasonProposed},
		"a byte count": {withPrevious("48012416"),
			[]incidentcontext.TemplateContainer{{Name: testContainer, MemoryLimit: limit("48Mi")}}, ReasonProposed},
		"the template's own value": {withPrevious("48Mi"),
			[]incidentcontext.TemplateContainer{{Name: "sidecar"}, {Name: testContainer, MemoryLimit: limit("48Mi")}},
			ReasonProposed},
		"no limit in the template": {withPrevious("64Mi"),
			[]incidentcontext.TemplateContainer{{Name: testContainer}}, ReasonPreviousValueUnknown},
		"no such container in the template": {withPrevious("64Mi"),
			[]incidentcontext.TemplateContainer{{Name: "sidecar", MemoryLimit: limit("48Mi")}},
			ReasonPreviousValueUnknown},
		"the template was not read": {withPrevious("64Mi"), nil, ReasonPreviousValueUnknown},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			f.builder.c.Template = c.template
			f.triage.steps = []step{reply(c.reply)}
			out := f.analyze(t, testLimits())
			if out.Reason != c.reason {
				t.Fatalf("reason %s, want %s", out.Reason, c.reason)
			}
			if c.reason != ReasonProposed {
				if out.Plan != nil {
					t.Errorf("a hand-off carries the plan %+v", out.Plan)
				}
				return
			}
			p := out.Plan.Action.SetContainerMemoryLimit
			if p == nil || p.Previous.String() != "48Mi" || p.Desired.String() != desiredLimit || p.Container != testContainer {
				t.Errorf("plan %+v: want app from 48Mi to 512Mi", p)
			}
		})
	}
}
