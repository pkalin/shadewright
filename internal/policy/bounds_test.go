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
	"fmt"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy"
)

// scopeOf is the scope of the request in in: what the analyzer knows before it proposes.
func scopeOf(in policy.DecisionInput) policy.Scope {
	t := *in.Target
	return policy.Scope{RequestNamespace: in.Request.Namespace, Action: in.Request.Spec.Action.Type, Target: t,
		LabelsKnown: true}
}

// TestBoundsForAgreesWithPreflight checks that BoundsFor refuses with the reason Preflight gives a request of
// the same scope, for every check that reads no parameter, and returns the policy's own bounds otherwise.
func TestBoundsForAgreesWithPreflight(t *testing.T) {
	cases := map[string]struct {
		base   func() policy.DecisionInput
		mutate func(*policy.DecisionInput)
		want   policy.Reason
	}{
		"memory allowed": {memoryInput, func(*policy.DecisionInput) {}, ""},
		"keda allowed":   {kedaInput, func(*policy.DecisionInput) {}, ""},
		"request namespace": {memoryInput, func(in *policy.DecisionInput) { in.Request.Namespace = otherNamespace },
			policy.ReasonRequestNamespaceNotAllowed},
		"module disabled": {kedaInput, func(in *policy.DecisionInput) { in.Settings.Spec.Modules = nil },
			policy.ReasonModuleNotEnabled},
		"target namespace": {memoryInput, func(in *policy.DecisionInput) {
			in.Policy.Spec.Targets.Namespaces = []v1alpha1.Name{otherNamespace}
		}, policy.ReasonTargetNamespaceNotAllowed},
		"labels": {memoryInput, func(in *policy.DecisionInput) { in.Target.Labels = map[string]string{teamLabel: otherName} },
			policy.ReasonTargetLabelsNotMatched},
		"action not in policy": {memoryInput, func(in *policy.DecisionInput) {
			in.Policy.Spec.Actions[0].Type = v1alpha1.ActionAdjustKedaMaxReplicas
		}, policy.ReasonActionNotAllowed},
		"bounds missing": {memoryInput, func(in *policy.DecisionInput) {
			in.Policy.Spec.Actions[0].SetContainerMemoryLimit = nil
		}, policy.ReasonPolicyInvalid},
		"bounds invalid": {kedaInput, func(in *policy.DecisionInput) {
			in.Policy.Spec.Actions[0].AdjustKedaMaxReplicas.MaxIncreasePercent = 5000
		}, policy.ReasonPolicyInvalid},
		"listed twice": {memoryInput, func(in *policy.DecisionInput) {
			in.Policy.Spec.Actions = append(in.Policy.Spec.Actions, in.Policy.Spec.Actions[0])
		}, policy.ReasonPolicyInvalid},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in := c.base()
			c.mutate(&in)
			pre, verdict := policy.Preflight(in)
			if (verdict == policy.PreflightContinue) != (c.want == "") || (c.want != "" && pre.Reason != c.want) {
				t.Fatalf("the case is wrong: Preflight %v %s, want %q", verdict, pre.Reason, c.want)
			}
			b, reason, msg := policy.BoundsFor(in.Policy, in.Settings, scopeOf(in))
			if reason != c.want {
				t.Fatalf("BoundsFor reason %q (%s), want %q", reason, msg, c.want)
			}
			if c.want != "" {
				if msg != pre.Message {
					t.Errorf("message %q, Preflight's %q", msg, pre.Message)
				}
				return
			}
			pa := in.Policy.Spec.Actions[0]
			if !reflect.DeepEqual(b.Memory, pa.SetContainerMemoryLimit) || !reflect.DeepEqual(b.Replicas, pa.AdjustKedaMaxReplicas) {
				t.Errorf("bounds %+v %+v, policy %+v", b.Memory, b.Replicas, pa)
			}
			if !b.ApprovalRequired || b.SelectorUnchecked {
				t.Errorf("approval %t, selector unchecked %t", b.ApprovalRequired, b.SelectorUnchecked)
			}
		})
	}
}

// TestBoundsForUnknownLabels leaves an objectSelector unchecked, and says so, when the labels are not known;
// an invalid selector still refuses.
func TestBoundsForUnknownLabels(t *testing.T) {
	in := kedaInput()
	s := scopeOf(in)
	s.LabelsKnown, s.Target.Labels = false, nil
	b, reason, _ := policy.BoundsFor(in.Policy, in.Settings, s)
	if reason != "" || !b.SelectorUnchecked || b.Replicas == nil {
		t.Fatalf("reason %q, bounds %+v", reason, b)
	}
	if in.Policy.Spec.Targets.ObjectSelector == nil {
		t.Fatal("BoundsFor modified the policy")
	}
	in.Policy.Spec.Targets.ObjectSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
		{Key: teamLabel, Operator: bogusOperator}}}
	if _, reason, _ := policy.BoundsFor(in.Policy, in.Settings, s); reason != policy.ReasonPolicyInvalid {
		t.Errorf("invalid selector: reason %q", reason)
	}
	if _, reason, _ := policy.BoundsFor(nil, in.Settings, s); reason != policy.ReasonIncompleteInput {
		t.Errorf("no policy: reason %q", reason)
	}
}

// TestBoundsForInvalidSelectorInDecisionOrder checks that, with the labels unknown, an invalid objectSelector
// is reported at the step where Preflight reports it: a check that comes first gives its own reason.
func TestBoundsForInvalidSelectorInDecisionOrder(t *testing.T) {
	cases := map[string]func(*policy.DecisionInput){
		"request namespace": func(in *policy.DecisionInput) { in.Request.Namespace = otherNamespace },
		"module disabled":   func(in *policy.DecisionInput) { in.Settings.Spec.Modules = nil },
		"target namespace": func(in *policy.DecisionInput) {
			in.Policy.Spec.Targets.Namespaces = []v1alpha1.Name{otherNamespace}
		},
		"selector only": func(*policy.DecisionInput) {},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := kedaInput()
			in.Policy.Spec.Targets.ObjectSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: teamLabel, Operator: bogusOperator}}}
			mutate(&in)
			pre, _ := policy.Preflight(in)
			s := scopeOf(in)
			s.LabelsKnown, s.Target.Labels = false, nil
			_, reason, msg := policy.BoundsFor(in.Policy, in.Settings, s)
			if pre.Reason == "" || reason != pre.Reason || msg != pre.Message {
				t.Errorf("BoundsFor %q (%s), Preflight %q (%s)", reason, msg, pre.Reason, pre.Message)
			}
		})
	}
}

// TestAllowedMemoryAgreesWithDecide checks every end of every range AllowedMemory returns, and the value just
// outside it, against Preflight on a request with that desired value.
func TestAllowedMemoryAgreesWithDecide(t *testing.T) {
	mi := int64(1 << 20)
	dec := func(p int32) *int32 { return &p }
	cases := []struct {
		min, max, previous int64
		inc                int32
		dec                *int32
	}{
		{64 * mi, 128 * mi, 48 * mi, 50, dec(0)},   // the lab: 48Mi, 64Mi to 128Mi, +50%
		{64 * mi, 128 * mi, 100 * mi, 50, nil},     // capped by max
		{64 * mi, 128 * mi, 100 * mi, 50, dec(20)}, // decreases too
		{mi, 1 << 40, 50_000_000, 33, dec(90)},     // decimal bytes, odd percentages
		{64 * mi, 128 * mi, 200 * mi, 50, nil},     // above max, no decrease: nothing
		{64 * mi, 128 * mi, 200 * mi, 50, dec(50)}, // above max, decrease into the bounds
		{64 * mi, 128 * mi, 20 * mi, 50, nil},      // too far below min: nothing
		{64 * mi, 128 * mi, 96 * mi, 0, dec(0)},    // increases refused too: nothing
	}
	for _, c := range cases {
		name := fmt.Sprintf("%d-%d from %d +%d", c.min, c.max, c.previous, c.inc)
		t.Run(name, func(t *testing.T) {
			b := &v1alpha1.MemoryLimitBounds{Min: *resource.NewQuantity(c.min, resource.BinarySI),
				Max: *resource.NewQuantity(c.max, resource.BinarySI), MaxIncreasePercent: c.inc, MaxDecreasePercent: c.dec}
			prev := *resource.NewQuantity(c.previous, resource.BinarySI)
			allowed := func(v int64) bool {
				if v < mi || v > 1<<40 || v == c.previous {
					return false
				}
				in := memoryInput()
				in.Policy.Spec.Actions[0].SetContainerMemoryLimit = b.DeepCopy()
				in.Request.Spec.Action.SetContainerMemoryLimit.Previous = prev
				in.Request.Spec.Action.SetContainerMemoryLimit.Desired = *resource.NewQuantity(v, resource.BinarySI)
				_, verdict := policy.Preflight(in)
				return verdict == policy.PreflightContinue
			}
			ranges := policy.AllowedMemory(b, prev)
			inRange := func(v int64) bool {
				for _, r := range ranges {
					if v >= r.Min.Value() && v <= r.Max.Value() {
						return true
					}
				}
				return false
			}
			probes := []int64{c.min - 1, c.min, c.max, c.max + 1, c.previous - 1, c.previous + 1}
			for _, r := range ranges {
				probes = append(probes, r.Min.Value()-1, r.Min.Value(), r.Max.Value(), r.Max.Value()+1)
			}
			for _, v := range probes {
				if inRange(v) != allowed(v) {
					t.Errorf("value %d: in a range %t, allowed %t (ranges %v)", v, inRange(v), allowed(v), ranges)
				}
			}
		})
	}

	// The lab case of #493: from 48Mi only 64Mi to 72Mi passes, and 96Mi does not.
	b := &v1alpha1.MemoryLimitBounds{Min: resource.MustParse("64Mi"), Max: resource.MustParse("128Mi"),
		MaxIncreasePercent: 50, MaxDecreasePercent: dec(0)}
	r := policy.AllowedMemory(b, resource.MustParse("48Mi"))
	if len(r) != 1 || r[0].Min.String() != "64Mi" || r[0].Max.String() != "72Mi" {
		t.Errorf("lab ranges %v, want 64Mi to 72Mi", r)
	}
	if policy.AllowedMemory(b, resource.MustParse("100m")) != nil || policy.AllowedMemory(nil, resource.MustParse("48Mi")) != nil {
		t.Error("ranges for an invalid previous value or no bounds")
	}
}
