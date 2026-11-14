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
	"strings"
	"testing"

	"k8s.io/client-go/tools/record"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/incidents"
	"github.com/pkalin/shadewright/internal/providers"
)

// ADR-0023 decision 8: a reply from another model than the one configured for the role, or one that names
// no model, is refused and handed off; its tokens are still charged.
func TestUnexpectedModelIsRefused(t *testing.T) {
	other := reply(validResult)
	other.c.Model = "gpt-other-2025-01-01"
	unnamed := reply(validResult)
	unnamed.c.Model = ""
	billed := step{c: providers.Completion{Model: "gpt-other-2025-01-01", Usage: providers.Usage{InputTokens: 10}},
		err: providers.ErrMalformedOutput}
	for name, s := range map[string]step{"another model": other, "no model": unnamed, "malformed from another model": billed} {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			f.triage.steps = []step{s, reply(validResult)}
			out := f.analyze(t, testLimits())
			if out.Reason != ReasonUnexpectedModel || out.Plan != nil || f.triage.calls() != 1 || f.escalation.calls() != 0 {
				t.Errorf("reason %s, plan %v, calls %d+%d", out.Reason, out.Plan, f.triage.calls(), f.escalation.calls())
			}
			if out.Tokens == 0 {
				t.Error("the refused reply was not charged")
			}
		})
	}
	t.Run("escalation", func(t *testing.T) {
		f := newFixture()
		f.builder.c.Truncated = true
		f.cfg.ExpectedModels[RoleEscalation] = "large-1"
		f.escalation.steps = []step{reply(validResult)}
		out := f.analyze(t, testLimits())
		if out.Reason != ReasonUnexpectedModel || out.Plan != nil {
			t.Errorf("reason %s, plan %v: the escalated reply named the triage's model", out.Reason, out.Plan)
		}
	})
}

// An endpoint the factory refuses as outside the allow-list is handed off with its own reason, before any
// call and any unit of the budget.
func TestEndpointNotAllowedHandsOff(t *testing.T) {
	f := newFixture()
	f.cfg.Models = func(Role, remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
		return nil, fmt.Errorf("model endpoint host: %w", ErrEndpointNotAllowed)
	}
	if out := f.analyze(t, testLimits()); out.Reason != ReasonEndpointNotAllowed || f.reserved != 0 {
		t.Errorf("reason %s, reserved %d", out.Reason, f.reserved)
	}
	f.cfg.Models = func(Role, remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
		return nil, errors.New("bad deployment")
	}
	if out := f.analyze(t, testLimits()); out.Reason != ReasonModelError {
		t.Errorf("another factory error: reason %s", out.Reason)
	}
}

// A provider the analyzer was not started for (an Anthropic deployment without the key file flag) and a
// deployment that names another model than its role's are handed off with their own reasons, before any call
// and any unit of the budget (ADR-0037).
func TestProviderRefusalsHandOff(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want Reason
	}{
		{ErrProviderNotConfigured, ReasonProviderNotConfigured},
		{ErrDeploymentNotExpected, ReasonUnexpectedModel},
	} {
		f := newFixture()
		f.cfg.Models = func(Role, remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
			return nil, fmt.Errorf("anthropic deployment: %w", tc.err)
		}
		if out := f.analyze(t, testLimits()); out.Reason != tc.want || f.reserved != 0 || out.Calls != 0 {
			t.Errorf("%v: reason %s, reserved %d, calls %d", tc.err, out.Reason, f.reserved, out.Calls)
		}
	}
}

// The first analysis that uses a role's provider, and the first after the settings moved the role to another
// provider, records one Normal event on its Incident naming the role and the providers only; an analysis on
// the same provider records none, and a refused deployment changes nothing (ADR-0037).
func TestProviderChangeIsRecorded(t *testing.T) {
	f := newFixture()
	f.triage.steps = append(f.triage.steps, reply(noProposal), reply(noProposal), reply(noProposal))
	events := record.NewFakeRecorder(8)
	f.cfg.Recorder = events
	a, err := New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	run := func(p remediationv1alpha1.ModelProvider) {
		t.Helper()
		l := testLimits()
		l.Triage.Provider = p
		if p == remediationv1alpha1.ModelProviderAnthropic {
			l.Triage.Endpoint, l.Triage.Deployment = remediationv1alpha1.AnthropicEndpoint, "claude-haiku-5-5"
		}
		if _, err := a.Analyze(t.Context(), incident(), l); err != nil {
			t.Fatal(err)
		}
	}
	next := func() string {
		select {
		case e := <-events.Events:
			return e
		default:
			return ""
		}
	}
	run("")
	if e := next(); e != "Normal "+EventModelProviderChanged+" The triage model now uses provider Foundry "+
		"(before: none since the analyzer started); its redacted context goes to that provider" {
		t.Errorf("first use: event %q", e)
	}
	run(remediationv1alpha1.ModelProviderFoundry)
	if e := next(); e != "" {
		t.Errorf("same provider: event %q, want none", e)
	}
	run(remediationv1alpha1.ModelProviderAnthropic)
	e := next()
	if e != "Normal "+EventModelProviderChanged+" The triage model now uses provider Anthropic (before: Foundry); "+
		"its redacted context goes to that provider" {
		t.Errorf("switch: event %q", e)
	}
	for _, leak := range []string{"https://", "claude-haiku", "small", "services.ai.azure.com"} {
		if strings.Contains(e, leak) {
			t.Errorf("the event names %q", leak)
		}
	}
	f.cfg.Models = func(Role, remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
		return nil, ErrProviderNotConfigured
	}
	a.cfg.Models = f.cfg.Models
	run(remediationv1alpha1.ModelProviderFoundry)
	if e := next(); e != "" {
		t.Errorf("refused deployment: event %q, want none", e)
	}
}

// A role the operator configured no model for is not configured, whatever RemediationSettings names.
func TestRoleWithoutExpectedModelIsNotConfigured(t *testing.T) {
	f := newFixture()
	delete(f.cfg.ExpectedModels, RoleTriage)
	f.triage.steps = []step{reply(validResult)}
	if out := f.analyze(t, testLimits()); out.Reason != ReasonNoModelConfigured || f.triage.calls() != 0 || f.reserved != 0 {
		t.Errorf("no triage model: reason %s, calls %d, reserved %d", out.Reason, f.triage.calls(), f.reserved)
	}

	f = newFixture()
	delete(f.cfg.ExpectedModels, RoleEscalation)
	f.builder.c.Truncated = true
	f.escalation.steps = []step{reply(validResult)}
	if out := f.analyze(t, testLimits()); out.Reason != ReasonEscalationNotConfigured || f.escalation.calls() != 0 {
		t.Errorf("no escalation model: reason %s, calls %d", out.Reason, f.escalation.calls())
	}
}

// credentialErr is what the Anthropic adapter returns when its key file could not be used and no attempt sent
// a request: retryable for a file that could not be read, not for a malformed one.
func credentialErr(retryable bool) error {
	err := fmt.Errorf("anthropic: the API key file could not be used: %w", providers.ErrCredentialUnavailable)
	if retryable {
		err = &providers.RetryableError{Err: err}
	}
	return providers.NotSent(err)
}

// A key file that could not be read, or is malformed, ends the analysis with ProviderNotConfigured, whose
// message names the key file, not with ModelUnavailable or ModelForbidden. A call that sent nothing is charged
// nothing; one that may have sent something is charged its upper bound.
func TestKeyFileFailureHandsOffAsNotConfigured(t *testing.T) {
	for name, err := range map[string]error{"unreadable": credentialErr(true), "malformed": credentialErr(false)} {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			f.triage.steps = []step{{err: err}}
			out := f.analyze(t, testLimits())
			if out.Reason != ReasonProviderNotConfigured || out.Tokens != 0 || out.Calls != 1 {
				t.Errorf("reason %s, tokens %d, calls %d", out.Reason, out.Tokens, out.Calls)
			}
		})
	}
	if m := outcomeMessages[ReasonProviderNotConfigured]; !strings.Contains(m, "key file") {
		t.Errorf("the message %q does not name the key file", m)
	}
	t.Run("sent before", func(t *testing.T) {
		f := newFixture()
		f.triage.steps = []step{{err: &providers.RetryableError{Err: providers.ErrCredentialUnavailable}}}
		out := f.analyze(t, testLimits())
		if out.Reason != ReasonModelUnavailable || out.Tokens == 0 {
			t.Errorf("reason %s, tokens %d: an earlier attempt may have been billed", out.Reason, out.Tokens)
		}
	})
}

// A call whose request reached the transport and then failed (a network error, or a reply that came after the
// attempt's deadline) is not marked as not sent by the adapter: it may have been billed and is charged its
// upper bound. The same error marked as not sent is charged nothing.
func TestSentThenFailedCallIsChargedItsUpperBound(t *testing.T) {
	for name, err := range map[string]error{
		"network": &providers.RetryableError{Err: errors.New("anthropic: the model request failed (network)")},
		"timeout": &providers.RetryableError{Err: errors.New("anthropic: the model request timed out")},
	} {
		t.Run(name, func(t *testing.T) {
			l := testLimits()
			f := newFixture()
			f.triage.steps = []step{{err: err}}
			if out := f.analyze(t, l); out.Calls != 1 || out.Tokens <= int64(l.MaxOutputTokens) {
				t.Errorf("sent: calls %d, tokens %d, want the upper bound (more than %d)", out.Calls, out.Tokens,
					l.MaxOutputTokens)
			}
			f = newFixture()
			f.triage.steps = []step{{err: providers.NotSent(err)}}
			if out := f.analyze(t, l); out.Calls != 1 || out.Tokens != 0 {
				t.Errorf("not sent: calls %d, tokens %d, want 0", out.Calls, out.Tokens)
			}
		})
	}
}

// The messages of the reasons that refuse a provider or an endpoint say only that nothing went to it: an
// escalation refused after triage calls has sent context to the triage provider.
func TestRefusalMessagesAfterTriage(t *testing.T) {
	f := newFixture()
	f.triage.steps = []step{reply("not json"), reply("not json")}
	f.cfg.Models = func(role Role, _ remediationv1alpha1.ModelDeployment) (providers.ModelProvider, error) {
		if role == RoleTriage {
			return f.triage, nil
		}
		return nil, ErrProviderNotConfigured
	}
	if out := f.analyze(t, testLimits()); out.Reason != ReasonProviderNotConfigured || out.Calls == 0 {
		t.Fatalf("reason %s, calls %d", out.Reason, out.Calls)
	}
	for _, r := range []Reason{ReasonProviderNotConfigured, ReasonEndpointNotAllowed} {
		if m := outcomeMessages[r]; strings.Contains(m, "; nothing was sent;") {
			t.Errorf("%s: %q says nothing was sent at all", r, m)
		}
	}
	// The endpoint refusal also covers the Anthropic API's fixed endpoint, where no allow-list applies.
	if m := outcomeMessages[ReasonEndpointNotAllowed]; !strings.Contains(m, "Anthropic") {
		t.Errorf("%q blames the allow-list only", m)
	}
}

// The provider change is recorded on the analysis whose call is the first to return having sent a request to
// the new provider: not on one whose budget was spent or unavailable, whose call sent nothing, or that was
// cancelled before its call.
func TestProviderChangeIsRecordedOnFirstSend(t *testing.T) {
	f := newFixture()
	events := record.NewFakeRecorder(8)
	f.cfg.Recorder = events
	a, err := New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	next := func() string {
		select {
		case e := <-events.Events:
			return e
		default:
			return ""
		}
	}
	run := func(ctx context.Context) {
		t.Helper()
		if _, err := a.Analyze(ctx, incident(), testLimits()); err != nil && ctx.Err() == nil {
			t.Fatal(err)
		}
	}
	f.reserveErr = incidents.ErrBudgetSpent
	run(t.Context())
	f.reserveErr = errors.New("conflict")
	run(t.Context())
	f.reserveErr = nil
	f.triage.steps = []step{{err: credentialErr(true)}}
	run(t.Context())
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	run(cancelled)
	if e := next(); e != "" {
		t.Fatalf("event %q before any request was sent", e)
	}
	f.triage.steps = []step{reply(noProposal)}
	run(t.Context())
	if e := next(); !strings.Contains(e, EventModelProviderChanged+" The triage model now uses provider Foundry") {
		t.Errorf("first send: event %q", e)
	}
}
