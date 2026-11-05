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

package policy_test

import (
	"reflect"
	"testing"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy"
)

// laterReasons are the reasons only the checks after the preflight give: the source checks, the target state
// check and an allowed plan. Preflight must let every input Decide answers with one of them through.
var laterReasons = map[policy.Reason]bool{
	policy.ReasonWithinApprovedBounds:         true,
	policy.ReasonRepositoryNotAllowed:         true,
	policy.ReasonRepositoryNotConfigured:      true,
	policy.ReasonBranchNotAllowed:             true,
	policy.ReasonPathNotAllowed:               true,
	policy.ReasonScaledObjectNotReady:         true,
	policy.ReasonScaledObjectReadinessUnknown: true,
}

// checkPreflight checks Preflight against Decide on one input (#242): it never allows; it refuses exactly the
// inputs Decide refuses on the request's own fields, with Decide's very decision; and it reads no source, so its
// answer is the same with no source and with one the policy refuses.
func checkPreflight(t *testing.T, in policy.DecisionInput) {
	t.Helper()
	want := policy.Decide(in)
	got, refused := policy.Preflight(in)
	if refused && got.Outcome == policy.OutcomeAllowed {
		t.Fatalf("Preflight() allowed: %+v", got)
	}
	if !refused && got != (policy.Decision{}) {
		t.Fatalf("Preflight() let the request through with a decision: %+v", got)
	}
	sourceMissing := in.Source == nil && in.Request != nil && in.Policy != nil && in.Settings != nil && in.Target != nil
	wantRefused := !laterReasons[want.Reason] && !sourceMissing
	if refused != wantRefused {
		t.Fatalf("Preflight() refused = %v (%+v), want %v for Decide() = %s/%s", refused, got, wantRefused,
			want.Outcome, want.Reason)
	}
	if refused && !reflect.DeepEqual(got, want) {
		t.Fatalf("Preflight() = %+v, want Decide's decision %+v", got, want)
	}
	for name, src := range map[string]*v1alpha1.ResolvedSource{
		"no source":      nil,
		"refused source": {RepositoryRef: "elsewhere", TargetBranch: "other", Path: ".github/workflows/x.yaml"},
	} {
		other := in
		other.Source = src
		if g, r := policy.Preflight(other); r != refused || !reflect.DeepEqual(g, got) {
			t.Fatalf("Preflight() with %s = %+v/%v, want %+v/%v: it must not read the source", name, g, r, got, refused)
		}
	}
}

// TestPreflightAgreesWithDecide runs checkPreflight on every case of the Decide tables, so a new check or reason
// cannot make the two disagree unnoticed.
func TestPreflightAgreesWithDecide(t *testing.T) {
	for _, tt := range decideCases() {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.base()
			tt.mutate(&in)
			checkPreflight(t, in)
		})
	}
	// The target state check is not in the preflight: a ScaledObject that is not ready, or not reported on, goes
	// on to the mapping, so a source the policy refuses is still Denied first.
	for _, status := range []string{condFalse, "Unknown"} {
		in := kedaInput()
		setCondition(&in, "Ready", status, "")
		if d, refused := policy.Preflight(in); refused {
			t.Fatalf("Ready %s: Preflight() = %+v, want no refusal", status, d)
		}
		checkPreflight(t, in)
	}
}

// TestPreflightRefusesFieldOnlyDenials names the refusals of the acceptance criteria of #242: each is decided by
// Preflight, before any source is resolved.
func TestPreflightRefusesFieldOnlyDenials(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(in *policy.DecisionInput)
		reason policy.Reason
	}{
		{"request namespace not allowed", func(in *policy.DecisionInput) {
			in.Policy.Spec.RequestNamespaces = []v1alpha1.Name{otherNamespace}
		}, policy.ReasonRequestNamespaceNotAllowed},
		{"module not enabled", func(in *policy.DecisionInput) { in.Settings.Spec.Modules = nil },
			policy.ReasonModuleNotEnabled},
		{"above the maximum", func(in *policy.DecisionInput) { kedaParams(in, 10, 30) }, policy.ReasonAboveMaximum},
		{"action not allowed", func(in *policy.DecisionInput) { in.Policy.Spec.Actions = nil },
			policy.ReasonActionNotAllowed},
		{"target namespace not allowed", func(in *policy.DecisionInput) {
			in.Policy.Spec.Targets.Namespaces = []v1alpha1.Name{otherNamespace}
		}, policy.ReasonTargetNamespaceNotAllowed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := kedaInput()
			c.mutate(&in)
			in.Source = nil
			d, refused := policy.Preflight(in)
			if !refused || d.Outcome != policy.OutcomeDenied || d.Reason != c.reason || !d.EffectiveApprovalRequired {
				t.Fatalf("Preflight() = %+v/%v, want Denied/%s", d, refused, c.reason)
			}
		})
	}
}
