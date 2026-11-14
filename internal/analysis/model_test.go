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
	"errors"
	"fmt"
	"strings"
	"testing"

	"k8s.io/client-go/tools/record"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
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
