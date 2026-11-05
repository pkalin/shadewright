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
//   - Only [OutcomeAllowed] lets a plan proceed. Every check that fails gives [OutcomeDenied], with three
//     exceptions: input the controller could not gather gives [OutcomeNeedsIntervention]; a target whose state
//     rules the action out until a person fixes it (a ScaledObject that is not ready) gives
//     [OutcomeNeedsIntervention] too; and a target whose state is not known yet (a ScaledObject KEDA has not
//     reported on) gives [OutcomeWait], which the controller decides again later. None is ever turned into
//     Allowed.
//   - Decide does not rely on the API server having validated or defaulted its input. A value the schema
//     would refuse is refused here too, and an unset field is read in its strictest sense: no request
//     namespaces allows no namespace, an unset approval mode is Required, an unset maxIncreasePercent
//     refuses every increase and an unset maxDecreasePercent refuses every decrease.
//     Policy bounds outside the schema's limits (a negative value, a percentage above its maximum, min
//     greater than max) make the policy [ReasonPolicyInvalid].
//   - The path match ([pathmatch.Match]) is purely lexical, so it is not trusted to keep a change away from
//     sensitive objects. The live target's kind must also be one the action type allows, and kinds excluded
//     from remediation (design section 10, item 10: Secrets, RBAC, network policy, namespaces, volumes,
//     admission and CRDs, Flux pipelines and Shadewright's own policy objects) are refused by name, whatever
//     the path.
//   - An action an integration module contributes (AdjustKedaMaxReplicas, from the keda module) is refused
//     unless the cluster admin enabled that module in RemediationSettings spec.modules.integrations (design
//     2.4 and 2.5, ADR-0018). Only the spec is read, never the status (ADR-0024).
//   - AdjustKedaMaxReplicas is refused unless the live ScaledObject is ready by the KedaReady rule
//     ([kedaready.Readiness]): a trigger error, a fallback, a pause (partial pauses and pause annotations
//     included), a deletion or an HPA that cannot read its metrics is never answered by raising replicas
//     (design section 7). The rule reads only the live object the controller read, never model output, and a
//     missing or malformed object is not ready.
//
// Checks run in a fixed order and the first that fails gives the reason, so a given input always yields the
// same reason.
//
// The checks that read only the request, the policy, the settings and the live target's identity and labels
// come first. [Preflight] runs exactly those, before the controller maps the target to Git, so a request the
// policy refuses on its own fields costs no Git read and no token. It only refuses: it never returns
// [OutcomeAllowed], and a request it lets through is decided by [Decide] in full once the source is resolved.
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
	"github.com/pkalin/shadewright/internal/kedaready"
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
	// OutcomeWait means the target's state is not known yet (a ScaledObject KEDA has not reported on), so
	// the decision is to be made again later. It is never Allowed.
	OutcomeWait Outcome = "Wait"
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
	// ReasonModuleNotEnabled: the action is contributed by an integration module that RemediationSettings
	// spec.modules.integrations does not enable.
	ReasonModuleNotEnabled Reason = "ModuleNotEnabled"
	// ReasonScaledObjectNotReady: the action is AdjustKedaMaxReplicas and the live ScaledObject is definitely
	// not ready by the KedaReady rule (Ready False, PartialTriggerError, paused, on fallback, HPAActive False,
	// being deleted), or is missing or malformed. Its outcome is NeedsIntervention: the plan may be within
	// bounds, but raising replicas does not fix a broken autoscaler, and no catalog action edits a trigger, so a
	// person must look.
	ReasonScaledObjectNotReady Reason = "ScaledObjectNotReady"
	// ReasonScaledObjectReadinessUnknown: the action is AdjustKedaMaxReplicas and KEDA has not reported on the
	// ScaledObject yet (no status, no Ready condition, Ready Unknown). Its outcome is Wait.
	ReasonScaledObjectReadinessUnknown Reason = "ScaledObjectReadinessUnknown"
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
	// Object is the whole live object as decoded from JSON (unstructured content), for the checks that read
	// its status, such as the readiness of a ScaledObject. Its apiVersion, kind, namespace, name and UID must be
	// the fields above. Decide only reads it. Nil is a missing object, which an action with a state check
	// refuses.
	Object map[string]any
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
		return refusal(r, msg)
	}
	return Decision{
		Outcome:                   OutcomeAllowed,
		Reason:                    ReasonWithinApprovedBounds,
		Message:                   "The change is within the policy bounds",
		EffectiveApprovalRequired: approvalRequired(in.Policy.Spec.Approval, in.Request.Spec.ApprovalRequired),
	}
}

// Preflight runs the checks of [Decide] that need no resolved source, in the same order and with the same
// reasons, outcomes and messages, so that the controller can refuse a request before it reads Git or requests a
// token. in.Source is ignored. It returns the refusal and true when one of those checks fails, and false when the
// request must go on to source mapping and [Decide]. It never allows anything: a request it lets through is not
// decided yet, and a refusal it returns is the decision Decide returns for the same input with any resolved
// source, because the checks it runs come first in Decide's order.
//
// The target state check is not among them, although it reads only the live target: Decide runs it last, so a
// plan the source checks refuse is Denied whatever state its target is in, and running it here would change
// that.
func Preflight(in DecisionInput) (Decision, bool) {
	if r, msg := preflight(in); r != "" {
		return refusal(r, msg), true
	}
	return Decision{}, false
}

// refusal is the decision of a failed check with reason r. It is never Allowed.
func refusal(r Reason, msg string) Decision {
	outcome := OutcomeDenied
	switch r {
	case ReasonIncompleteInput, ReasonScaledObjectNotReady:
		outcome = OutcomeNeedsIntervention
	case ReasonScaledObjectReadinessUnknown:
		outcome = OutcomeWait
	}
	return Decision{Outcome: outcome, Reason: r, Message: msg, EffectiveApprovalRequired: true}
}

// approvalRequired is false only when the policy is Optional and the request explicitly asked for no
// approval. Any other combination, including unset or unknown values, requires approval.
func approvalRequired(mode v1alpha1.ApprovalMode, requested *bool) bool {
	return mode != v1alpha1.ApprovalOptional || requested == nil || *requested
}

// check runs every check in order and returns the first failure, or an empty reason if all pass.
func check(in DecisionInput) (Reason, string) {
	if in.Source == nil {
		return ReasonIncompleteInput, incompleteInputMessage
	}
	if r, msg := preflight(in); r != "" {
		return r, msg
	}
	return runChecks(in, sourceChecks)
}

// checkFunc is one check: it returns the reason and message of a failure, or an empty reason.
type checkFunc func(DecisionInput) (Reason, string)

// fieldChecks read only the request, the policy, the settings and the live target, never the resolved source.
// They run first in Decide and are all that Preflight runs, so both share one implementation and one order.
var fieldChecks = []checkFunc{checkRequest, checkAction, checkModule, checkTarget, checkPolicyAction}

// sourceChecks need the resolved source, or must run after the checks that do (the target state check, see
// checkTargetState). Only Decide runs them.
var sourceChecks = []checkFunc{checkSource, checkTargetState}

// incompleteInputMessage is the message of ReasonIncompleteInput.
const incompleteInputMessage = "The request, policy, settings, resolved source and live target are all needed"

// preflight runs the field checks in order and returns the first failure, or an empty reason if all pass. It
// never reads in.Source.
func preflight(in DecisionInput) (Reason, string) {
	if in.Request == nil || in.Policy == nil || in.Settings == nil || in.Target == nil {
		return ReasonIncompleteInput, incompleteInputMessage
	}
	return runChecks(in, fieldChecks)
}

// runChecks runs checks in order and returns the first failure, or an empty reason if all pass.
func runChecks(in DecisionInput, checks []checkFunc) (Reason, string) {
	for _, c := range checks {
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
		if !validDNSLabel(p.Container) {
			return ReasonInvalidParameters, "container must be a DNS label"
		}
		if !validMemory(p.Previous) || !validMemory(p.Desired) {
			return ReasonInvalidParameters, "previous and desired must be whole bytes between 1Mi and 1Ti"
		}
		if cmpQuantity(p.Previous, p.Desired) == 0 {
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
	if t.Object != nil && !sameObject(t) {
		return ReasonTargetMismatch, "The live object is not the live target it was given for"
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
	return CheckObjectSelector(p, t.Labels)
}

// CheckObjectSelector checks the labels of a target against the policy's targets.objectSelector. It returns an
// empty reason when they match or the policy has no selector. The freshness check uses it too, so a target
// relabelled after the decision is judged by the same code.
func CheckObjectSelector(p *v1alpha1.RemediationPolicy, targetLabels map[string]string) (Reason, string) {
	if sel := p.Spec.Targets.ObjectSelector; sel != nil {
		s, err := metav1.LabelSelectorAsSelector(sel)
		if err != nil {
			return ReasonPolicyInvalid, fmt.Sprintf("Policy %q has an invalid objectSelector", p.Name)
		}
		if !s.Matches(labels.Set(targetLabels)) {
			return ReasonTargetLabelsNotMatched,
				fmt.Sprintf("The target's labels do not match the objectSelector of policy %q", p.Name)
		}
	}
	return "", ""
}

// checkModule checks that the module contributing the action, if any, is enabled.
func checkModule(in DecisionInput) (Reason, string) {
	return CheckModuleEnabled(in.Settings, in.Request.Spec.Action.Type)
}

// CheckModuleEnabled checks that the integration module that contributes an action of type actionType is
// enabled in settings spec.modules.integrations (design 2.4 and 2.5). A core action has no module and passes.
// It reads the spec only, never the status (ADR-0024). It returns an empty reason when the action may be used.
// The freshness check uses it too, so a module disabled after the decision makes the plan stale.
func CheckModuleEnabled(settings *v1alpha1.RemediationSettings, actionType v1alpha1.ActionType) (Reason, string) {
	entry, ok := catalog[actionType]
	if !ok || entry.module == "" {
		return "", ""
	}
	if m := settings.Spec.Modules; m == nil || !slices.Contains(m.Integrations, entry.module) {
		return ReasonModuleNotEnabled,
			fmt.Sprintf("Action %s needs the %s integration enabled in RemediationSettings spec.modules", actionType, entry.module)
	}
	return "", ""
}

// checkTargetState checks that the live target is in a state the action may be applied to. It runs last, so
// a plan the policy refuses is Denied, a final answer, whatever state its target is in.
func checkTargetState(in DecisionInput) (Reason, string) {
	return CheckTargetState(in.Request.Spec.Action.Type, in.Target.Object)
}

// CheckTargetState runs the state check of an action of type actionType on its live target object, the
// object as decoded from JSON. For AdjustKedaMaxReplicas the object must be a ScaledObject that is ready by the
// KedaReady rule ([kedaready.Readiness], the function the KEDA module's verification check uses too): a
// definite not-ready state, or a nil, missing or malformed object, gives [ReasonScaledObjectNotReady], and a
// ScaledObject KEDA has not reported on yet gives [ReasonScaledObjectReadinessUnknown]. Other action types have
// no state check. It returns an empty reason when the target passes. The freshness check uses it too, so
// readiness lost after the decision makes the plan stale by the same code.
func CheckTargetState(actionType v1alpha1.ActionType, obj map[string]any) (Reason, string) {
	if entry, ok := catalog[actionType]; ok && entry.state != nil {
		return entry.state(obj)
	}
	return "", ""
}

// scaledObjectState is the state check of AdjustKedaMaxReplicas.
func scaledObjectState(obj map[string]any) (Reason, string) {
	switch kedaready.Readiness(obj) {
	case kedaready.Ready:
		return "", ""
	case kedaready.Pending:
		return ReasonScaledObjectReadinessUnknown, "KEDA has not reported the ScaledObject ready yet; the check is made again later"
	default:
		return ReasonScaledObjectNotReady, "The ScaledObject is not ready (Ready=True, not paused, not being deleted, " +
			"not on fallback, HPAActive not False), so raising maxReplicaCount is not the fix; a person must look"
	}
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
		if !validKedaBounds(b) {
			return ReasonPolicyInvalid, fmt.Sprintf("Policy %q has bounds for %s outside the schema's limits", p.Name, a.Type)
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
		if !validMemoryBounds(b) {
			return ReasonPolicyInvalid, fmt.Sprintf("Policy %q has bounds for %s outside the schema's limits", p.Name, a.Type)
		}
		params := a.SetContainerMemoryLimit
		// checkAction has made sure both values are whole bytes between 1Mi and 1Ti, so they fit in an int64.
		previous, desired := params.Previous.DeepCopy(), params.Desired.DeepCopy()
		return checkBounds(previous.Value(), desired.Value(),
			cmpQuantity(params.Desired, b.Min), cmpQuantity(params.Desired, b.Max),
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

// sameObject reports whether t.Object has t's apiVersion, kind, namespace, name and UID.
func sameObject(t *Target) bool {
	o := t.Object
	meta, _ := o["metadata"].(map[string]any)
	str := func(m map[string]any, k string) string { v, _ := m[k].(string); return v }
	return str(o, "apiVersion") == t.APIVersion && str(o, "kind") == t.Kind && str(meta, "namespace") == t.Namespace &&
		str(meta, "name") == t.Name && str(meta, "uid") == string(t.UID)
}

// actionKinds is one action type's catalog entry: what it may target (an API group, any version or exactly
// apiVersion when it is set, and a set of kinds), the integration module that contributes it ("" for a core
// action) and the state check its live target must pass (nil for none).
type actionKinds struct {
	group      string
	apiVersion string
	kinds      []string
	module     v1alpha1.ModuleName
	state      func(obj map[string]any) (Reason, string)
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
// RemediationRequestSpec, and is stricter in one place: AdjustKedaMaxReplicas may target only a keda.sh/v1alpha1
// ScaledObject, the only version KEDA 2.x serves and the one its readiness rule reads. A type missing here is
// unknown and always Denied.
var catalog = map[v1alpha1.ActionType]actionKinds{
	v1alpha1.ActionAdjustKedaMaxReplicas: {group: kedaready.Group, apiVersion: kedaready.APIVersion,
		kinds: []string{kedaready.KindScaledObject}, module: kedaready.ModuleName, state: scaledObjectState},
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
	if cmpQuantity(q, minMemory) < 0 || cmpQuantity(q, maxMemory) > 0 {
		return false
	}
	// Value rounds up to a whole number, so it equals q only when q is whole.
	c := q.DeepCopy()
	return cmpQuantity(*resource.NewQuantity(c.Value(), resource.BinarySI), q) == 0
}

// cmpQuantity compares two quantities by value. Quantity.Cmp may cache a new representation in its receiver
// and argument, and a copied Quantity shares its decimal with the original, so it compares deep copies and
// never changes the input.
func cmpQuantity(a, b resource.Quantity) int {
	x, y := a.DeepCopy(), b.DeepCopy()
	return x.Cmp(y)
}

// Limits of the policy bounds in the CRD schema (KedaMaxReplicasBounds, MemoryLimitBounds).
const (
	maxReplicaBound   = 1000
	maxIncreaseLimit  = 1000
	maxDecreaseLimit  = 90
	dnsLabelMaxLength = 63
)

// validKedaBounds reports whether b is within the schema's limits. An unset min (0) reads as 1 and an unset
// maxIncreasePercent (0) refuses increases, so 0 is accepted for both.
func validKedaBounds(b *v1alpha1.KedaMaxReplicasBounds) bool {
	return b.Min >= 0 && b.Min <= maxReplicaBound && b.Max >= 1 && b.Max <= maxReplicaBound &&
		max(b.Min, 1) <= b.Max && validPercents(b.MaxIncreasePercent, b.MaxDecreasePercent)
}

// validMemoryBounds reports whether b is within the schema's limits: whole bytes, min at least 1Mi, max at
// most 1Ti, min not greater than max.
func validMemoryBounds(b *v1alpha1.MemoryLimitBounds) bool {
	return validMemory(b.Min) && validMemory(b.Max) && cmpQuantity(b.Min, b.Max) <= 0 &&
		validPercents(b.MaxIncreasePercent, b.MaxDecreasePercent)
}

// validPercents reports whether maxIncreasePercent is 0 (unset) or 1 to 1000, and maxDecreasePercent is
// unset or 0 to 90.
func validPercents(increase int32, decrease *int32) bool {
	if increase < 0 || increase > maxIncreaseLimit {
		return false
	}
	return decrease == nil || (*decrease >= 0 && *decrease <= maxDecreaseLimit)
}

// validDNSLabel reports whether s is a DNS label as the schema of SetContainerMemoryLimit.container defines
// it: 1 to 63 lower-case letters, digits and dashes, starting and ending with a letter or digit.
func validDNSLabel(s string) bool {
	if len(s) == 0 || len(s) > dnsLabelMaxLength || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}
