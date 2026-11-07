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

package policy

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/pkalin/shadewright/api/v1alpha1"
)

// Scope is what is known of a request before anything is proposed: the namespace it will be created in, the
// action type, and its target (the Incident's subject, as read from the cluster).
type Scope struct {
	// RequestNamespace is the namespace the request will be created in: the operator namespace.
	RequestNamespace string
	// Action is the action type a proposal would carry.
	Action v1alpha1.ActionType
	// Target is the subject. Object is not read.
	Target Target
	// LabelsKnown says whether Target.Labels are the subject's labels as read from the cluster. When false a
	// policy's objectSelector is not checked and [Bounds.SelectorUnchecked] is set instead.
	LabelsKnown bool
}

// Bounds are the bounds a policy sets on one action type for one target, as [Decide] reads them. Exactly one
// of Memory and Replicas is set, by the action type.
type Bounds struct {
	// Memory bounds SetContainerMemoryLimit.
	Memory *v1alpha1.MemoryLimitBounds
	// Replicas bounds AdjustKedaMaxReplicas.
	Replicas *v1alpha1.KedaMaxReplicasBounds
	// SelectorUnchecked says the policy has an objectSelector that was not checked, because the target's
	// labels were not known ([Scope.LabelsKnown]). The decision checks it.
	SelectorUnchecked bool
	// ApprovalRequired is the effective approval of a request that asks for approvalRequired: true, as the
	// analyzer's requests always do: always true. It is here so that a caller never shows less than the
	// decision requires.
	ApprovalRequired bool
}

// BoundsFor returns the bounds policy p would apply to a request of scope s, or the reason and message with
// which [Decide] would refuse any such request whatever its parameters. It runs Decide's checks that need no
// parameters, with the same functions: the request's policy and namespace, the action catalog, the module the
// action needs, the target's identity, kind, namespace and labels, and the policy's entry for the action and
// its bounds. A request inside the bounds still needs the checks of the parameters, the resolved source and
// the target's state, so these bounds never allow anything (invariant 1): they say what the decision will
// check. BoundsFor is pure and does not modify its input.
func BoundsFor(p *v1alpha1.RemediationPolicy, settings *v1alpha1.RemediationSettings, s Scope) (Bounds, Reason, string) {
	if p == nil || settings == nil {
		return Bounds{}, ReasonIncompleteInput, incompleteInputMessage
	}
	t := s.Target
	approval := true
	rr := &v1alpha1.RemediationRequest{}
	rr.Namespace = s.RequestNamespace
	rr.Spec = v1alpha1.RemediationRequestSpec{
		PolicyRef: v1alpha1.Name(p.Name), Mode: v1alpha1.RemediationModeGitOps, ApprovalRequired: &approval,
		Action: v1alpha1.Action{Type: s.Action},
		TargetRef: v1alpha1.ObjectRef{APIVersion: t.APIVersion, Kind: t.Kind, Namespace: t.Namespace,
			Name: t.Name, UID: t.UID},
	}
	t.Object = nil
	policy := p
	var b Bounds
	if !s.LabelsKnown && p.Spec.Targets.ObjectSelector != nil {
		// The selector must still parse: an invalid one refuses every request.
		if r, msg := CheckObjectSelector(p, nil); r == ReasonPolicyInvalid {
			return Bounds{}, r, msg
		}
		policy = p.DeepCopy()
		policy.Spec.Targets.ObjectSelector = nil
		b.SelectorUnchecked = true
	}
	in := DecisionInput{Request: rr, Policy: policy, Settings: settings, Target: &t}
	if r, msg := checkRequest(in); r != "" {
		return Bounds{}, r, msg
	}
	if _, ok := catalog[s.Action]; !ok {
		return Bounds{}, ReasonActionUnknown, fmt.Sprintf("Action type %q is not in the action catalog", s.Action)
	}
	if r, msg := checkModule(in); r != "" {
		return Bounds{}, r, msg
	}
	if r, msg := checkTarget(in); r != "" {
		return Bounds{}, r, msg
	}
	pa, r, msg := policyEntry(p, s.Action)
	if r != "" {
		return Bounds{}, r, msg
	}
	b.Memory = pa.SetContainerMemoryLimit.DeepCopy()
	b.Replicas = pa.AdjustKedaMaxReplicas.DeepCopy()
	b.ApprovalRequired = approvalRequired(p.Spec.Approval, &approval)
	return b, "", ""
}

// MemoryRange is a range of desired memory limits, both ends included, in bytes.
type MemoryRange struct {
	Min, Max resource.Quantity
}

// AllowedMemory returns the desired values [Decide] allows under bounds b when the current limit is previous:
// at most one range of decreases and one of increases, in that order, each within min and max and within
// maxDecreasePercent or maxIncreasePercent of previous, by the same arithmetic as the decision. It returns
// none when previous is not a whole number of bytes between 1Mi and 1Ti (the decision refuses any request
// from it), when b is not within the schema's limits, or when no value passes. The ends are whole bytes.
func AllowedMemory(b *v1alpha1.MemoryLimitBounds, previous resource.Quantity) []MemoryRange {
	if b == nil || !validMemory(previous) || !validMemoryBounds(b) {
		return nil
	}
	prev := previous.DeepCopy()
	p := prev.Value()
	lo, hi := b.Min.DeepCopy(), b.Max.DeepCopy()
	minV, maxV := lo.Value(), hi.Value()
	var out []MemoryRange
	add := func(from, to int64) {
		if from <= to {
			out = append(out, MemoryRange{Min: *resource.NewQuantity(from, resource.BinarySI),
				Max: *resource.NewQuantity(to, resource.BinarySI)})
		}
	}
	if d := b.MaxDecreasePercent; d != nil && *d > 0 {
		add(max(minV, p-maxDelta(p, *d)), min(maxV, p-1))
	}
	if b.MaxIncreasePercent > 0 {
		add(max(minV, p+1), min(maxV, p+maxDelta(p, b.MaxIncreasePercent)))
	}
	return out
}

// maxDelta is the largest delta that [withinPercent] allows for base and pct: floor(base*pct/100). base is at
// most 1Ti and pct at most 1000, so the product fits in an int64.
func maxDelta(base int64, pct int32) int64 {
	return base * int64(pct) / 100
}
