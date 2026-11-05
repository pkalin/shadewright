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
	"testing"

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
