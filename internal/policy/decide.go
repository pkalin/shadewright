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

// Package policy decides whether a RemediationRequest may run under a RemediationPolicy (design sections 5.3,
// 8.1 and 8.2, ADR-0003, ADR-0011, ADR-0012).
//
// [Decide] is a pure function: it reads only its input, uses no Kubernetes client, network or clock, and
// gives the same [Decision] for the same input. The controller gathers the input (the request, the policy,
// the settings, the source resolved from Flux and the live target object) and records the result.
//
// The decision fails closed:
//
//   - Only [OutcomeAllowed] lets a plan proceed. Every check that fails gives [OutcomeDenied], and input the
//     controller could not gather gives [OutcomeNeedsIntervention]; neither is ever turned into Allowed.
//   - Decide does not rely on the API server having validated or defaulted its input. A value the schema
//     would refuse is refused here too, and an unset field is read in its strictest sense: no request
//     namespaces allows no namespace, an unset approval mode is Required, an unset maxIncreasePercent
//     refuses every increase and an unset maxDecreasePercent refuses every decrease.
//   - The path match ([pathmatch.Match]) is purely lexical, so it is not trusted to keep a change away from
//     sensitive objects. The live target's kind must also be one the action type allows, and kinds excluded
//     from remediation (design section 10, item 10: Secrets, RBAC, network policy, namespaces, volumes,
//     admission and CRDs, Flux pipelines and Shadewright's own policy objects) are refused by name, whatever
//     the path.
//
// Checks run in a fixed order and the first that fails gives the reason, so a given input always yields the
// same reason.
package policy

import (
	"cmp"
	"fmt"
	"math/big"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/pathmatch"
)

// Outcome is the result of a policy decision.
type Outcome string

const (
	// OutcomeAllowed means the plan is within the policy and may proceed.
	OutcomeAllowed Outcome = "Allowed"
	// OutcomeDenied means the policy does not allow the plan. It is final for this request.
	OutcomeDenied Outcome = "Denied"
	// OutcomeNeedsIntervention means the decision could not be made from the input, so a human must look.
	OutcomeNeedsIntervention Outcome = "NeedsIntervention"
)

// Reason is a machine-readable cause of a decision, suitable for a condition reason.
type Reason string

// Reasons. Each check has its own, so a test or an operator can tell exactly which check decided.
const (
	// ReasonWithinApprovedBounds: every check passed.
	ReasonWithinApprovedBounds Reason = "WithinApprovedBounds"

	// ReasonIncompleteInput: the request, policy, settings, resolved source or live target is missing.
	ReasonIncompleteInput Reason = "IncompleteInput"

	// ReasonPolicyMismatch: the policy is not the one the request's policyRef names.
	ReasonPolicyMismatch Reason = "PolicyMismatch"
	// ReasonRequestNamespaceNotAllowed: the request's namespace is not in the policy's requestNamespaces.
	ReasonRequestNamespaceNotAllowed Reason = "RequestNamespaceNotAllowed"
	// ReasonModeNotSupported: the request's mode is not GitOps.
	ReasonModeNotSupported Reason = "ModeNotSupported"
	// ReasonActionUnknown: the action type is not in the action catalog.
	ReasonActionUnknown Reason = "ActionUnknown"
	// ReasonInvalidParameters: the action's parameters are missing, mixed with another type's, outside the
	// catalog's range or do not change anything.
	ReasonInvalidParameters Reason = "InvalidParameters"
	// ReasonTargetMismatch: the live target is not the object the request's targetRef names.
	ReasonTargetMismatch Reason = "TargetMismatch"
	// ReasonKindExcluded: the target's kind is excluded from remediation (design section 10, item 10).
	ReasonKindExcluded Reason = "KindExcluded"
	// ReasonKindNotAllowed: the action type cannot target the target's apiVersion and kind.
	ReasonKindNotAllowed Reason = "KindNotAllowed"
	// ReasonTargetNamespaceNotAllowed: the target's namespace is not in the policy's targets.namespaces.
	ReasonTargetNamespaceNotAllowed Reason = "TargetNamespaceNotAllowed"
	// ReasonTargetLabelsNotMatched: the target's labels do not match the policy's objectSelector.
	ReasonTargetLabelsNotMatched Reason = "TargetLabelsNotMatched"
	// ReasonActionNotAllowed: the policy does not list the action type.
	ReasonActionNotAllowed Reason = "ActionNotAllowed"
	// ReasonPolicyInvalid: the policy is not usable, for example its bounds or selector are missing or
	// malformed.
	ReasonPolicyInvalid Reason = "PolicyInvalid"
	// ReasonBelowMinimum: the desired value is below the policy's min.
	ReasonBelowMinimum Reason = "BelowMinimum"
	// ReasonAboveMaximum: the desired value is above the policy's max.
	ReasonAboveMaximum Reason = "AboveMaximum"
	// ReasonIncreaseTooLarge: the increase is larger than maxIncreasePercent of the previous value.
	ReasonIncreaseTooLarge Reason = "IncreaseTooLarge"
	// ReasonDecreaseNotAllowed: the value decreases and maxDecreasePercent is 0 or unset.
	ReasonDecreaseNotAllowed Reason = "DecreaseNotAllowed"
	// ReasonDecreaseTooLarge: the decrease is larger than maxDecreasePercent of the previous value.
	ReasonDecreaseTooLarge Reason = "DecreaseTooLarge"
	// ReasonRepositoryNotAllowed: the resolved repository is not the policy's gitOps.repositoryRef.
	ReasonRepositoryNotAllowed Reason = "RepositoryNotAllowed"
	// ReasonRepositoryNotConfigured: the resolved repository is not an entry in RemediationSettings.
	ReasonRepositoryNotConfigured Reason = "RepositoryNotConfigured"
	// ReasonBranchNotAllowed: the resolved branch is not the repository's targetBranch in RemediationSettings.
	ReasonBranchNotAllowed Reason = "BranchNotAllowed"
	// ReasonPathNotAllowed: the resolved file matches none of the policy's allowedPaths.
	ReasonPathNotAllowed Reason = "PathNotAllowed"
)

// defaultTargetBranch is the schema default of GitRepository.targetBranch.
const defaultTargetBranch v1alpha1.GitBranch = "main"

// Target is the live object the request is about, as the controller read it from the cluster. The source
// resolver (ADR-0012) has matched it to the one document in the resolved file that declares it, so its
// apiVersion and kind are also what the file declares.
type Target struct {
	// APIVersion of the object, for example keda.sh/v1alpha1.
	APIVersion string
	// Kind of the object, for example ScaledObject.
	Kind string
	// Namespace of the object; empty for cluster-scoped objects.
	Namespace string
	// Name of the object.
	Name string
	// UID of the object.
	UID v1alpha1.UID
	// Labels of the object, matched against the policy's objectSelector.
	Labels map[string]string
}

// DecisionInput is everything a decision reads. Decide does not modify it.
type DecisionInput struct {
	// Request is the plan to decide on.
	Request *v1alpha1.RemediationRequest
	// Policy is the RemediationPolicy the request's policyRef names.
	Policy *v1alpha1.RemediationPolicy
	// Settings is the cluster's RemediationSettings.
	Settings *v1alpha1.RemediationSettings
	// Source is where the controller found the target's declaration in Git (ADR-0012).
	Source *v1alpha1.ResolvedSource
	// Target is the live target object.
	Target *Target
}

// Decision is the result of [Decide].
type Decision struct {
	// Outcome says whether the plan may proceed.
	Outcome Outcome
	// Reason is the machine-readable cause: the first check that failed, or ReasonWithinApprovedBounds.
	Reason Reason
	// Message explains the reason for humans. It holds only names and values from the input.
	Message string
	// EffectiveApprovalRequired says whether a human must approve. It is true unless the outcome is Allowed,
	// the policy's approval is Optional and the request asked for approvalRequired: false.
	EffectiveApprovalRequired bool
}

// Decide decides whether the request in in may run under its policy. See the package documentation for
// the rules.
func Decide(in DecisionInput) Decision {
	if r, msg := check(in); r != "" {
		outcome := OutcomeDenied
		if r == ReasonIncompleteInput {
			outcome = OutcomeNeedsIntervention
		}
		return Decision{Outcome: outcome, Reason: r, Message: msg, EffectiveApprovalRequired: true}
	}
	return Decision{
		Outcome:                   OutcomeAllowed,
		Reason:                    ReasonWithinApprovedBounds,
		Message:                   "The change is within the policy bounds",
		EffectiveApprovalRequired: approvalRequired(in.Policy.Spec.Approval, in.Request.Spec.ApprovalRequired),
	}
}

// approvalRequired is false only when the policy is Optional and the request explicitly asked for no
// approval. Any other combination, including unset or unknown values, requires approval.
func approvalRequired(mode v1alpha1.ApprovalMode, requested *bool) bool {
	return mode != v1alpha1.ApprovalOptional || requested == nil || *requested
}

// check runs every check in order and returns the first failure, or an empty reason if all pass.
func check(in DecisionInput) (Reason, string) {
	if in.Request == nil || in.Policy == nil || in.Settings == nil || in.Source == nil || in.Target == nil {
		return ReasonIncompleteInput, "The request, policy, settings, resolved source and live target are all needed"
	}
	for _, c := range []func(DecisionInput) (Reason, string){
		checkRequest, checkAction, checkTarget, checkPolicyAction, checkSource,
	} {
		if r, msg := c(in); r != "" {
			return r, msg
		}
	}
	return "", ""
}

// checkRequest checks that the request may use the policy at all.
func checkRequest(in DecisionInput) (Reason, string) {
	rr, p := in.Request, in.Policy
	if p.Name == "" || string(rr.Spec.PolicyRef) != p.Name {
		return ReasonPolicyMismatch, fmt.Sprintf("The request names policy %q, not %q", rr.Spec.PolicyRef, p.Name)
	}
	if rr.Namespace == "" || !slices.Contains(p.Spec.RequestNamespaces, v1alpha1.Name(rr.Namespace)) {
		return ReasonRequestNamespaceNotAllowed,
			fmt.Sprintf("Policy %q does not accept requests from namespace %q", p.Name, rr.Namespace)
	}
	if rr.Spec.Mode != "" && rr.Spec.Mode != v1alpha1.RemediationModeGitOps {
		return ReasonModeNotSupported, fmt.Sprintf("Mode %q is not supported; only GitOps is", rr.Spec.Mode)
	}
	return "", ""
}

// checkAction checks that the action is in the catalog and its parameters are well formed.
func checkAction(in DecisionInput) (Reason, string) {
	a := in.Request.Spec.Action
	if _, ok := catalog[a.Type]; !ok {
		return ReasonActionUnknown, fmt.Sprintf("Action type %q is not in the action catalog", a.Type)
	}
	switch a.Type {
	case v1alpha1.ActionAdjustKedaMaxReplicas:
		p := a.AdjustKedaMaxReplicas
		if p == nil || a.SetContainerMemoryLimit != nil {
			return ReasonInvalidParameters, "Exactly the adjustKedaMaxReplicas parameters must be set"
		}
		if !inReplicaRange(p.PreviousValue) || !inReplicaRange(p.DesiredValue) {
			return ReasonInvalidParameters, "previousValue and desiredValue must be between 1 and 1000"
		}
		if p.PreviousValue == p.DesiredValue {
			return ReasonInvalidParameters, "desiredValue must differ from previousValue"
		}
	case v1alpha1.ActionSetContainerMemoryLimit:
		p := a.SetContainerMemoryLimit
		if p == nil || a.AdjustKedaMaxReplicas != nil {
			return ReasonInvalidParameters, "Exactly the setContainerMemoryLimit parameters must be set"
		}
		if p.Container == "" {
			return ReasonInvalidParameters, "container must be set"
		}
		if !validMemory(p.Previous) || !validMemory(p.Desired) {
			return ReasonInvalidParameters, "previous and desired must be whole bytes between 1Mi and 1Ti"
		}
		if p.Previous.Cmp(p.Desired) == 0 {
			return ReasonInvalidParameters, "desired must differ from previous"
		}
	}
	return "", ""
}

// checkTarget checks that the live target is the requested object, of a kind the action may change, and
// inside the policy's targets.
func checkTarget(in DecisionInput) (Reason, string) {
	ref, t, p := in.Request.Spec.TargetRef, in.Target, in.Policy
	if t.UID == "" || t.UID != ref.UID || t.APIVersion != ref.APIVersion || t.Kind != ref.Kind ||
		t.Namespace != ref.Namespace || t.Name != ref.Name {
		return ReasonTargetMismatch, "The live target is not the object the request's targetRef names"
	}
	group := apiGroup(t.APIVersion)
	if excluded(group, t.Kind) {
		return ReasonKindExcluded, fmt.Sprintf("Kind %q in API group %q is excluded from remediation", t.Kind, group)
	}
	if !catalog[in.Request.Spec.Action.Type].allows(t.APIVersion, t.Kind) {
		return ReasonKindNotAllowed,
			fmt.Sprintf("Action %s cannot target %s %s", in.Request.Spec.Action.Type, t.APIVersion, t.Kind)
	}
	if t.Namespace == "" || !slices.Contains(p.Spec.Targets.Namespaces, v1alpha1.Name(t.Namespace)) {
		return ReasonTargetNamespaceNotAllowed,
			fmt.Sprintf("Policy %q does not cover target namespace %q", p.Name, t.Namespace)
	}
	if sel := p.Spec.Targets.ObjectSelector; sel != nil {
		s, err := metav1.LabelSelectorAsSelector(sel)
		if err != nil {
			return ReasonPolicyInvalid, fmt.Sprintf("Policy %q has an invalid objectSelector", p.Name)
		}
		if !s.Matches(labels.Set(t.Labels)) {
			return ReasonTargetLabelsNotMatched,
				fmt.Sprintf("The target's labels do not match the objectSelector of policy %q", p.Name)
		}
	}
	return "", ""
}

// checkPolicyAction checks that the policy allows the action type and that the change is within its bounds.
func checkPolicyAction(in DecisionInput) (Reason, string) {
	a, p := in.Request.Spec.Action, in.Policy
	var entries []v1alpha1.PolicyAction
	for _, pa := range p.Spec.Actions {
		if pa.Type == a.Type {
			entries = append(entries, pa)
		}
	}
	switch len(entries) {
	case 0:
		return ReasonActionNotAllowed, fmt.Sprintf("Policy %q does not allow action %s", p.Name, a.Type)
	case 1:
	default:
		return ReasonPolicyInvalid, fmt.Sprintf("Policy %q lists action %s more than once", p.Name, a.Type)
	}
	pa := entries[0]
	switch a.Type {
	case v1alpha1.ActionAdjustKedaMaxReplicas:
		b := pa.AdjustKedaMaxReplicas
		if b == nil {
			return ReasonPolicyInvalid, fmt.Sprintf("Policy %q has no bounds for %s", p.Name, a.Type)
		}
		prev, desired := a.AdjustKedaMaxReplicas.PreviousValue, a.AdjustKedaMaxReplicas.DesiredValue
		return checkBounds(int64(prev), int64(desired),
			cmp.Compare(desired, max(b.Min, 1)), cmp.Compare(desired, b.Max),
			b.MaxIncreasePercent, b.MaxDecreasePercent)
	case v1alpha1.ActionSetContainerMemoryLimit:
		b := pa.SetContainerMemoryLimit
		if b == nil {
			return ReasonPolicyInvalid, fmt.Sprintf("Policy %q has no bounds for %s", p.Name, a.Type)
		}
		params := a.SetContainerMemoryLimit
		// checkAction has made sure both values are whole bytes between 1Mi and 1Ti, so they fit in an int64.
		return checkBounds(params.Previous.Value(), params.Desired.Value(),
			params.Desired.Cmp(b.Min), params.Desired.Cmp(b.Max),
			b.MaxIncreasePercent, b.MaxDecreasePercent)
	}
	// Unreachable: checkAction refuses every type not handled above.
	return ReasonActionUnknown, fmt.Sprintf("Action type %q is not in the action catalog", a.Type)
}

// checkBounds checks a change from previous to desired. cmpMin and cmpMax compare desired with the policy's
// min and max (-1, 0 or +1), so quantities are compared by value, not by their text.
func checkBounds(previous, desired int64, cmpMin, cmpMax int, maxIncrease int32, maxDecrease *int32) (Reason, string) {
	if cmpMin < 0 {
		return ReasonBelowMinimum, "The desired value is below the policy's min"
	}
	if cmpMax > 0 {
		return ReasonAboveMaximum, "The desired value is above the policy's max"
	}
	if desired > previous {
		if !withinPercent(desired-previous, previous, maxIncrease) {
			return ReasonIncreaseTooLarge,
				fmt.Sprintf("The increase is more than maxIncreasePercent (%d) of the previous value", maxIncrease)
		}
		return "", ""
	}
	pct := int32(0)
	if maxDecrease != nil {
		pct = *maxDecrease
	}
	if pct <= 0 {
		return ReasonDecreaseNotAllowed, "The policy refuses decreases (maxDecreasePercent is 0)"
	}
	if !withinPercent(previous-desired, previous, pct) {
		return ReasonDecreaseTooLarge,
			fmt.Sprintf("The decrease is more than maxDecreasePercent (%d) of the previous value", pct)
	}
	return "", ""
}

// withinPercent reports whether delta is at most pct percent of base, that is delta*100 <= base*pct. It
// works in arbitrary precision, so no input can overflow it. A pct of 0 or less allows no change.
func withinPercent(delta, base int64, pct int32) bool {
	if pct <= 0 {
		return false
	}
	lhs := new(big.Int).Mul(big.NewInt(delta), big.NewInt(100))
	rhs := new(big.Int).Mul(big.NewInt(base), big.NewInt(int64(pct)))
	return lhs.Cmp(rhs) <= 0
}

// checkSource checks the resolved source against the policy and the settings.
func checkSource(in DecisionInput) (Reason, string) {
	src, p := in.Source, in.Policy
	if src.RepositoryRef == "" || src.RepositoryRef != p.Spec.GitOps.RepositoryRef {
		return ReasonRepositoryNotAllowed, fmt.Sprintf("The target is delivered from repository %q, but policy %q allows only %q",
			src.RepositoryRef, p.Name, p.Spec.GitOps.RepositoryRef)
	}
	i := slices.IndexFunc(in.Settings.Spec.Repositories, func(r v1alpha1.GitRepository) bool {
		return r.Name == src.RepositoryRef
	})
	if i < 0 {
		return ReasonRepositoryNotConfigured,
			fmt.Sprintf("Repository %q is not configured in RemediationSettings", src.RepositoryRef)
	}
	branch := in.Settings.Spec.Repositories[i].TargetBranch
	if branch == "" {
		branch = defaultTargetBranch
	}
	if src.TargetBranch != branch {
		return ReasonBranchNotAllowed, fmt.Sprintf("The source tracks branch %q, but repository %q allows only %q",
			src.TargetBranch, src.RepositoryRef, branch)
	}
	if !slices.ContainsFunc(p.Spec.GitOps.AllowedPaths, func(pat v1alpha1.GitPathPattern) bool {
		return pathmatch.Match(pat, src.Path)
	}) {
		return ReasonPathNotAllowed, fmt.Sprintf("File %q matches none of the allowedPaths of policy %q", src.Path, p.Name)
	}
	return "", ""
}

// actionKinds lists what one action type may target: an API group (any version, or exactly the given
// apiVersion when apiVersion is set) and a set of kinds.
type actionKinds struct {
	group      string
	apiVersion string
	kinds      []string
}

func (k actionKinds) allows(apiVersion, kind string) bool {
	if k.apiVersion != "" && apiVersion != k.apiVersion {
		return false
	}
	if !strings.Contains(apiVersion, "/") || apiGroup(apiVersion) != k.group {
		return false
	}
	return slices.Contains(k.kinds, kind)
}

// catalog is the closed action catalog with the kinds each action may target. It mirrors the CEL rules on
// RemediationRequestSpec. A type missing here is unknown and always Denied.
var catalog = map[v1alpha1.ActionType]actionKinds{
	v1alpha1.ActionAdjustKedaMaxReplicas:   {group: "keda.sh", kinds: []string{"ScaledObject"}},
	v1alpha1.ActionSetContainerMemoryLimit: {group: "apps", apiVersion: "apps/v1", kinds: []string{"Deployment", "StatefulSet"}},
}

// excludedGroups are API groups no remediation may touch: Shadewright's own policy objects, admission and
// CRDs, and RBAC. Groups ending in excludedGroupSuffix (Flux) are excluded too.
var excludedGroups = []string{
	"remediation.shadewright.io",
	"admissionregistration.k8s.io",
	"apiextensions.k8s.io",
	"rbac.authorization.k8s.io",
}

// excludedGroupSuffix covers every Flux API group: sources, Kustomizations, Helm releases, notifications and
// image automation, the pipeline that delivers changes.
const excludedGroupSuffix = ".toolkit.fluxcd.io"

// excludedKinds are kinds in otherwise allowed groups that no remediation may touch, keyed by API group
// ("" is the core group).
var excludedKinds = map[string][]string{
	"":                  {"Secret", "ServiceAccount", "Namespace", "PersistentVolume", "PersistentVolumeClaim"},
	"networking.k8s.io": {"NetworkPolicy"},
}

// excluded reports whether objects of kind in group are excluded from remediation (design section 10,
// item 10). The catalog already allows only a few kinds; this list is a second, independent guard that
// names the reason.
func excluded(group, kind string) bool {
	if slices.Contains(excludedGroups, group) || strings.HasSuffix(group, excludedGroupSuffix) {
		return true
	}
	return slices.Contains(excludedKinds[group], kind)
}

// apiGroup returns the group of an apiVersion: "apps" for "apps/v1", "" for the core "v1".
func apiGroup(apiVersion string) string {
	group, _, found := strings.Cut(apiVersion, "/")
	if !found {
		return ""
	}
	return group
}

func inReplicaRange(v int32) bool { return v >= 1 && v <= 1000 }

var (
	minMemory = resource.MustParse("1Mi")
	maxMemory = resource.MustParse("1Ti")
)

// validMemory reports whether q is a whole number of bytes between 1Mi and 1Ti, compared by value.
func validMemory(q resource.Quantity) bool {
	if q.Cmp(minMemory) < 0 || q.Cmp(maxMemory) > 0 {
		return false
	}
	// Value rounds up to a whole number, so it equals q only when q is whole.
	return resource.NewQuantity(q.Value(), resource.BinarySI).Cmp(q) == 0
}
