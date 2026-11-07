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
	"math"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy"
	"github.com/pkalin/shadewright/internal/testfixtures"
)

const (
	requestNamespace = "shadewright-system"
	teamLabel        = "team"
	bogusOperator    = "Bogus"
	team             = "orders"
	otherName        = "other"
	otherNamespace   = "billing"
	deploymentPath   = "apps/orders/deployment.yaml"
	allApps          = "apps/**"
	condTrue         = "True"
	kedaModule       = "keda"
	condFalse        = "False"
	keyStatus        = "status"
	keyConditions    = "conditions"
)

// kedaInput returns an input that is Allowed: raise a ScaledObject's maxReplicaCount from 10 to 15 under a
// policy allowing 1 to 20, at most +50%, no decreases.
func kedaInput() policy.DecisionInput {
	rr := testfixtures.RemediationRequest(requestNamespace, "rr")
	p := testfixtures.RemediationPolicy("keda-capacity")
	p.Spec.RequestNamespaces = []v1alpha1.Name{requestNamespace}
	p.Spec.Approval = v1alpha1.ApprovalRequired
	p.Spec.Actions[0].AdjustKedaMaxReplicas = &v1alpha1.KedaMaxReplicasBounds{
		Min: 1, Max: 20, MaxIncreasePercent: 50, MaxDecreasePercent: new(int32(0)),
	}
	p.Spec.Targets.ObjectSelector = &metav1.LabelSelector{MatchLabels: map[string]string{teamLabel: team}}
	settings := testfixtures.RemediationSettings()
	settings.Spec.Modules = &v1alpha1.Modules{Integrations: []v1alpha1.ModuleName{kedaModule}}
	return policy.DecisionInput{
		Request:  rr,
		Policy:   p,
		Settings: settings,
		Source: &v1alpha1.ResolvedSource{
			RepositoryRef: testfixtures.RepositoryName,
			TargetBranch:  "main",
			Path:          "apps/orders/scaledobject.yaml",
			BaseCommit:    "0123456789abcdef0123456789abcdef01234567",
		},
		Target: targetOf(rr.Spec.TargetRef, map[string]string{teamLabel: team}),
	}
}

// memoryInput returns an input that is Allowed: raise a Deployment container's memory limit from 256Mi to
// 384Mi under a policy allowing 128Mi to 2Gi, at most +50%, no decreases.
func memoryInput() policy.DecisionInput {
	in := kedaInput()
	in.Request = testfixtures.MemoryLimitRequest(requestNamespace, "rr")
	in.Policy.Spec.Actions = []v1alpha1.PolicyAction{testfixtures.MemoryLimitPolicyAction()}
	b := in.Policy.Spec.Actions[0].SetContainerMemoryLimit
	b.MaxIncreasePercent = 50
	b.MaxDecreasePercent = new(int32(0))
	in.Policy.Spec.GitOps.AllowedPaths = []v1alpha1.GitPathPattern{deploymentPath}
	in.Source.Path = deploymentPath
	in.Target = targetOf(in.Request.Spec.TargetRef, map[string]string{teamLabel: team})
	return in
}

func targetOf(ref v1alpha1.ObjectRef, labels map[string]string) *policy.Target {
	return &policy.Target{
		APIVersion: ref.APIVersion, Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name, UID: ref.UID,
		Labels: labels, Object: liveObject(ref, readyConditions()),
	}
}

// readyConditions are the conditions of a ScaledObject that scales on its metrics, as KEDA v2.21.0 writes them.
func readyConditions() []any {
	return []any{
		kcond("Ready", condTrue, "ScaledObjectReady"),
		kcond("Active", condTrue, "ScalerActive"),
		kcond("Fallback", condFalse, "NoFallbackFound"),
		kcond("Paused", condFalse, ""),
		kcond("HPAActive", condTrue, "HPAActive"),
	}
}

// kcond is a status condition as decoded from JSON; an empty reason is left out.
func kcond(typ, status, reason string) map[string]any {
	c := map[string]any{"type": typ, keyStatus: status}
	if reason != "" {
		c["reason"] = reason
	}
	return c
}

// liveObject is the live object of ref as decoded from JSON, with the given status conditions.
func liveObject(ref v1alpha1.ObjectRef, conditions []any) map[string]any {
	return map[string]any{
		"apiVersion": ref.APIVersion, "kind": ref.Kind,
		"metadata": map[string]any{"namespace": ref.Namespace, "name": ref.Name, "uid": string(ref.UID)},
		"spec":     map[string]any{"maxReplicaCount": int64(10)},
		keyStatus:  map[string]any{keyConditions: conditions},
	}
}

// setCondition sets the status (and reason) of the first condition of the type on the live target, adding the
// condition when it is missing.
func setCondition(in *policy.DecisionInput, typ, status, reason string) {
	st := in.Target.Object[keyStatus].(map[string]any)
	conds, _ := st[keyConditions].([]any)
	for _, c := range conds {
		if c.(map[string]any)["type"] == typ {
			c.(map[string]any)[keyStatus], c.(map[string]any)["reason"] = status, reason
			return
		}
	}
	st[keyConditions] = append(conds, kcond(typ, status, reason))
}

// setTarget points the request, the live target and the live object at the given kind.
func setTarget(in *policy.DecisionInput, apiVersion, kind string) {
	in.Request.Spec.TargetRef.APIVersion = apiVersion
	in.Request.Spec.TargetRef.Kind = kind
	in.Target.APIVersion = apiVersion
	in.Target.Kind = kind
	if in.Target.Object != nil {
		in.Target.Object["apiVersion"], in.Target.Object["kind"] = apiVersion, kind
	}
}

func kedaParams(in *policy.DecisionInput, previous, desired int32) {
	in.Request.Spec.Action.AdjustKedaMaxReplicas = &v1alpha1.AdjustKedaMaxReplicas{
		PreviousValue: previous, DesiredValue: desired,
	}
}

func memoryParams(in *policy.DecisionInput, previous, desired string) {
	p := in.Request.Spec.Action.SetContainerMemoryLimit
	p.Previous = resource.MustParse(previous)
	p.Desired = resource.MustParse(desired)
}

func kedaBounds(in *policy.DecisionInput) *v1alpha1.KedaMaxReplicasBounds {
	return in.Policy.Spec.Actions[0].AdjustKedaMaxReplicas
}

func memoryBounds(in *policy.DecisionInput) *v1alpha1.MemoryLimitBounds {
	return in.Policy.Spec.Actions[0].SetContainerMemoryLimit
}

// decideCase is one case of the Decide table: an input made by base and changed by mutate, and its decision.
type decideCase struct {
	name    string
	base    func() policy.DecisionInput
	mutate  func(in *policy.DecisionInput)
	outcome policy.Outcome
	reason  policy.Reason
}

// decideCases is the Decide table. TestPreflightAgreesWithDecide runs Preflight on it too.
func decideCases() []decideCase {
	const (
		allowed = policy.OutcomeAllowed
		denied  = policy.OutcomeDenied
		needs   = policy.OutcomeNeedsIntervention
	)
	return []decideCase{
		{name: "keda baseline", base: kedaInput, mutate: func(*policy.DecisionInput) {},
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},
		{name: "memory baseline", base: memoryInput, mutate: func(*policy.DecisionInput) {},
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},

		// Incomplete input.
		{name: "no request", base: kedaInput, mutate: func(in *policy.DecisionInput) { in.Request = nil },
			outcome: needs, reason: policy.ReasonIncompleteInput},
		{name: "no policy", base: kedaInput, mutate: func(in *policy.DecisionInput) { in.Policy = nil },
			outcome: needs, reason: policy.ReasonIncompleteInput},
		{name: "no settings", base: kedaInput, mutate: func(in *policy.DecisionInput) { in.Settings = nil },
			outcome: needs, reason: policy.ReasonIncompleteInput},
		{name: "source not resolved", base: kedaInput, mutate: func(in *policy.DecisionInput) { in.Source = nil },
			outcome: needs, reason: policy.ReasonIncompleteInput},
		{name: "no live target", base: kedaInput, mutate: func(in *policy.DecisionInput) { in.Target = nil },
			outcome: needs, reason: policy.ReasonIncompleteInput},

		// Request.
		{name: "policyRef names another policy", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Request.Spec.PolicyRef = otherName },
			outcome: denied, reason: policy.ReasonPolicyMismatch},
		{name: "request from a namespace the policy does not list", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Request.Namespace = testfixtures.TargetNamespace },
			outcome: denied, reason: policy.ReasonRequestNamespaceNotAllowed},
		{name: "empty policyRef and empty policy name", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				in.Request.Spec.PolicyRef = ""
				in.Policy.Name = ""
			},
			outcome: denied, reason: policy.ReasonPolicyMismatch},
		{name: "empty request namespace listed as empty", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				in.Request.Namespace = ""
				in.Policy.Spec.RequestNamespaces = []v1alpha1.Name{""}
			},
			outcome: denied, reason: policy.ReasonRequestNamespaceNotAllowed},
		{name: "requestNamespaces unset allows no namespace", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Policy.Spec.RequestNamespaces = nil },
			outcome: denied, reason: policy.ReasonRequestNamespaceNotAllowed},
		{name: "mode other than GitOps", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Request.Spec.Mode = "Direct" },
			outcome: denied, reason: policy.ReasonModeNotSupported},
		{name: "mode unset is GitOps", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Request.Spec.Mode = "" },
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},

		// Action catalog and parameters.
		{name: "unknown action type", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Request.Spec.Action.Type = "RestartPod" },
			outcome: denied, reason: policy.ReasonActionUnknown},
		{name: "unknown action type listed by the policy", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				in.Request.Spec.Action = v1alpha1.Action{Type: "DeleteNamespace"}
				in.Policy.Spec.Actions = append(in.Policy.Spec.Actions, v1alpha1.PolicyAction{Type: "DeleteNamespace"})
			},
			outcome: denied, reason: policy.ReasonActionUnknown},
		{name: "empty action type", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Request.Spec.Action.Type = "" },
			outcome: denied, reason: policy.ReasonActionUnknown},
		{name: "keda parameters missing", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Request.Spec.Action.AdjustKedaMaxReplicas = nil },
			outcome: denied, reason: policy.ReasonInvalidParameters},
		{name: "keda with memory parameters too", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				in.Request.Spec.Action.SetContainerMemoryLimit = testfixtures.MemoryLimitAction().SetContainerMemoryLimit
			},
			outcome: denied, reason: policy.ReasonInvalidParameters},
		{name: "keda desired equals previous", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaParams(in, 10, 10) },
			outcome: denied, reason: policy.ReasonInvalidParameters},
		{name: "keda desired outside catalog range", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaParams(in, 10, 0) },
			outcome: denied, reason: policy.ReasonInvalidParameters},
		{name: "keda previous outside catalog range", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaParams(in, math.MaxInt32, 15) },
			outcome: denied, reason: policy.ReasonInvalidParameters},
		{name: "memory parameters missing", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { in.Request.Spec.Action.SetContainerMemoryLimit = nil },
			outcome: denied, reason: policy.ReasonInvalidParameters},
		{name: "memory with keda parameters too", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { kedaParams(in, 10, 15) },
			outcome: denied, reason: policy.ReasonInvalidParameters},
		{name: "memory container empty", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { in.Request.Spec.Action.SetContainerMemoryLimit.Container = "" },
			outcome: denied, reason: policy.ReasonInvalidParameters},
		{name: "memory container not a DNS label", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { in.Request.Spec.Action.SetContainerMemoryLimit.Container = "Worker" },
			outcome: denied, reason: policy.ReasonInvalidParameters},
		{name: "memory container with a slash", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { in.Request.Spec.Action.SetContainerMemoryLimit.Container = "a/b" },
			outcome: denied, reason: policy.ReasonInvalidParameters},
		{name: "memory container starting with a dash", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { in.Request.Spec.Action.SetContainerMemoryLimit.Container = "-worker" },
			outcome: denied, reason: policy.ReasonInvalidParameters},
		{name: "memory container ending with a dash", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { in.Request.Spec.Action.SetContainerMemoryLimit.Container = "worker-" },
			outcome: denied, reason: policy.ReasonInvalidParameters},
		{name: "memory container longer than 63", base: memoryInput,
			mutate: func(in *policy.DecisionInput) {
				in.Request.Spec.Action.SetContainerMemoryLimit.Container = strings.Repeat("a", 64)
			},
			outcome: denied, reason: policy.ReasonInvalidParameters},
		{name: "memory container of 63 characters", base: memoryInput,
			mutate: func(in *policy.DecisionInput) {
				in.Request.Spec.Action.SetContainerMemoryLimit.Container = "a-" + strings.Repeat("0", 61)
			},
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},
		{name: "memory not whole bytes", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { memoryParams(in, "256Mi", "300000000.5") },
			outcome: denied, reason: policy.ReasonInvalidParameters},
		{name: "memory below 1Mi", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { memoryParams(in, "256Mi", "1000Ki") },
			outcome: denied, reason: policy.ReasonInvalidParameters},
		{name: "memory above 1Ti", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { memoryParams(in, "1Ti", "1Pi") },
			outcome: denied, reason: policy.ReasonInvalidParameters},
		{name: "memory desired equals previous written differently", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { memoryParams(in, "256Mi", "268435456") },
			outcome: denied, reason: policy.ReasonInvalidParameters},

		// Target identity and kind.
		{name: "live target has another UID", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Target.UID = "99999999-9999-4999-8999-999999999999" },
			outcome: denied, reason: policy.ReasonTargetMismatch},
		{name: "live target has no UID", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				in.Target.UID = ""
				in.Request.Spec.TargetRef.UID = ""
			},
			outcome: denied, reason: policy.ReasonTargetMismatch},
		{name: "live target is another kind than targetRef", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Target.Kind = "Role" },
			outcome: denied, reason: policy.ReasonTargetMismatch},
		{name: "live target in another namespace than targetRef", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Target.Namespace = otherNamespace },
			outcome: denied, reason: policy.ReasonTargetMismatch},
		{name: "live target has another name than targetRef", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Target.Name = otherName },
			outcome: denied, reason: policy.ReasonTargetMismatch},
		{name: "keda action on a Deployment", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { setTarget(in, "apps/v1", "Deployment") },
			outcome: denied, reason: policy.ReasonKindNotAllowed},
		{name: "keda action on a ScaledObject of another group", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { setTarget(in, "keda.sh.evil.io/v1alpha1", "ScaledObject") },
			outcome: denied, reason: policy.ReasonKindNotAllowed},
		{name: "keda action on a core-group ScaledObject", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { setTarget(in, "v1", "ScaledObject") },
			outcome: denied, reason: policy.ReasonKindNotAllowed},
		// KEDA 2.x serves only v1alpha1, and the readiness rule is defined for it alone.
		{name: "keda action on another keda version", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { setTarget(in, "keda.sh/v1beta1", "ScaledObject") },
			outcome: denied, reason: policy.ReasonKindNotAllowed},
		{name: "memory action on a StatefulSet", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { setTarget(in, "apps/v1", "StatefulSet") },
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},
		{name: "memory action on a DaemonSet", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { setTarget(in, "apps/v1", "DaemonSet") },
			outcome: denied, reason: policy.ReasonKindNotAllowed},
		{name: "memory action on apps/v1beta2 Deployment", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { setTarget(in, "apps/v1beta2", "Deployment") },
			outcome: denied, reason: policy.ReasonKindNotAllowed},

		// Target namespace and labels.
		{name: "target namespace not covered", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				in.Request.Spec.TargetRef.Namespace = otherNamespace
				in.Target.Namespace = otherNamespace
				in.Target.Object["metadata"].(map[string]any)["namespace"] = otherNamespace
			},
			outcome: denied, reason: policy.ReasonTargetNamespaceNotAllowed},
		{name: "cluster-scoped target", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				in.Request.Spec.TargetRef.Namespace = ""
				in.Target.Namespace = ""
				in.Target.Object["metadata"].(map[string]any)["namespace"] = ""
			},
			outcome: denied, reason: policy.ReasonTargetNamespaceNotAllowed},
		{name: "target labels do not match", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Target.Labels = map[string]string{teamLabel: otherNamespace} },
			outcome: denied, reason: policy.ReasonTargetLabelsNotMatched},
		{name: "target has no labels", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Target.Labels = nil },
			outcome: denied, reason: policy.ReasonTargetLabelsNotMatched},
		{name: "no objectSelector covers every label", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				in.Policy.Spec.Targets.ObjectSelector = nil
				in.Target.Labels = nil
			},
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},
		{name: "invalid objectSelector", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				in.Policy.Spec.Targets.ObjectSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: teamLabel, Operator: bogusOperator},
				}}
			},
			outcome: denied, reason: policy.ReasonPolicyInvalid},

		// Policy actions.
		{name: "action type not in the policy", base: memoryInput,
			mutate: func(in *policy.DecisionInput) {
				in.Policy.Spec.Actions = testfixtures.RemediationPolicy("keda-capacity").Spec.Actions
			},
			outcome: denied, reason: policy.ReasonActionNotAllowed},
		{name: "action listed twice", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				in.Policy.Spec.Actions = append(in.Policy.Spec.Actions, in.Policy.Spec.Actions[0])
			},
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "keda bounds missing", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Policy.Spec.Actions[0].AdjustKedaMaxReplicas = nil },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "memory bounds missing", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { in.Policy.Spec.Actions[0].SetContainerMemoryLimit = nil },
			outcome: denied, reason: policy.ReasonPolicyInvalid},

		// Policy bounds outside the schema's limits.
		{name: "keda negative min", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaBounds(in).Min = -1 },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "keda min above 1000", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaBounds(in).Min = 1001 },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "keda max 0", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaBounds(in).Max = 0 },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "keda negative max", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaBounds(in).Max = -5 },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "keda max above 1000", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaBounds(in).Max = 1001 },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "keda min greater than max", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaBounds(in).Min = 21 },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "keda negative maxIncreasePercent", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaBounds(in).MaxIncreasePercent = -1 },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "keda maxIncreasePercent above 1000", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaBounds(in).MaxIncreasePercent = 1001 },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "keda maxIncreasePercent at 1000", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaBounds(in).MaxIncreasePercent = 1000 },
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},
		{name: "keda negative maxDecreasePercent", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaBounds(in).MaxDecreasePercent = new(int32(-1)) },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "keda maxDecreasePercent above 90", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaBounds(in).MaxDecreasePercent = new(int32(91)) },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "memory min below 1Mi", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { memoryBounds(in).Min = resource.MustParse("512Ki") },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "memory min unset", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { memoryBounds(in).Min = resource.Quantity{} },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "memory negative min", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { memoryBounds(in).Min = resource.MustParse("-1Gi") },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "memory max above 1Ti", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { memoryBounds(in).Max = resource.MustParse("2Ti") },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "memory max not whole bytes", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { memoryBounds(in).Max = resource.MustParse("2000000000.5") },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "memory min greater than max", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { memoryBounds(in).Min = resource.MustParse("3Gi") },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "memory negative maxIncreasePercent", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { memoryBounds(in).MaxIncreasePercent = -1 },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "memory maxIncreasePercent above 1000", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { memoryBounds(in).MaxIncreasePercent = 1001 },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "memory maxDecreasePercent above 90", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { memoryBounds(in).MaxDecreasePercent = new(int32(91)) },
			outcome: denied, reason: policy.ReasonPolicyInvalid},
		{name: "memory decimal bounds", base: memoryInput,
			mutate: func(in *policy.DecisionInput) {
				memoryBounds(in).Min = resource.MustParse("0.125Gi")
				memoryBounds(in).Max = resource.MustParse("2G")
			},
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},

		// KEDA bounds.
		{name: "keda below min", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				kedaBounds(in).Min = 12
				kedaBounds(in).MaxDecreasePercent = new(int32(90))
				kedaParams(in, 15, 11)
			},
			outcome: denied, reason: policy.ReasonBelowMinimum},
		{name: "keda at min", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				kedaBounds(in).Min = 12
				kedaBounds(in).MaxDecreasePercent = new(int32(90))
				kedaParams(in, 15, 12)
			},
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},
		{name: "keda above max", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaParams(in, 20, 21) },
			outcome: denied, reason: policy.ReasonAboveMaximum},
		{name: "keda at max", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaParams(in, 19, 20) },
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},
		{name: "keda increase above maxIncreasePercent", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaParams(in, 10, 16) },
			outcome: denied, reason: policy.ReasonIncreaseTooLarge},
		{name: "keda increase exactly maxIncreasePercent", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaParams(in, 10, 15) },
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},
		{name: "keda maxIncreasePercent unset refuses increases", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaBounds(in).MaxIncreasePercent = 0 },
			outcome: denied, reason: policy.ReasonIncreaseTooLarge},
		{name: "keda decrease when maxDecreasePercent is 0", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { kedaParams(in, 10, 9) },
			outcome: denied, reason: policy.ReasonDecreaseNotAllowed},
		{name: "keda decrease when maxDecreasePercent is unset", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				kedaBounds(in).MaxDecreasePercent = nil
				kedaParams(in, 10, 9)
			},
			outcome: denied, reason: policy.ReasonDecreaseNotAllowed},
		{name: "keda decrease above maxDecreasePercent", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				kedaBounds(in).MaxDecreasePercent = new(int32(20))
				kedaParams(in, 10, 7)
			},
			outcome: denied, reason: policy.ReasonDecreaseTooLarge},
		{name: "keda decrease exactly maxDecreasePercent", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				kedaBounds(in).MaxDecreasePercent = new(int32(20))
				kedaParams(in, 10, 8)
			},
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},
		{name: "keda min unset is 1", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				kedaBounds(in).Min = 0
				kedaBounds(in).MaxDecreasePercent = new(int32(90))
				kedaParams(in, 2, 1)
			},
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},
		{name: "keda largest percentages", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				kedaBounds(in).Max = 1000
				kedaBounds(in).MaxIncreasePercent = 1000
				kedaParams(in, 1000, 999)
				kedaBounds(in).MaxDecreasePercent = new(int32(90))
			},
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},

		// Memory bounds, compared by value.
		{name: "memory below min", base: memoryInput,
			mutate: func(in *policy.DecisionInput) {
				memoryBounds(in).MaxDecreasePercent = new(int32(90))
				memoryParams(in, "256Mi", "100Mi")
			},
			outcome: denied, reason: policy.ReasonBelowMinimum},
		{name: "memory at min written in bytes", base: memoryInput,
			mutate: func(in *policy.DecisionInput) {
				memoryBounds(in).MaxDecreasePercent = new(int32(90))
				memoryParams(in, "256Mi", "134217728")
			},
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},
		{name: "memory one byte below min", base: memoryInput,
			mutate: func(in *policy.DecisionInput) {
				memoryBounds(in).MaxDecreasePercent = new(int32(90))
				memoryParams(in, "256Mi", "134217727")
			},
			outcome: denied, reason: policy.ReasonBelowMinimum},
		{name: "memory above max", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { memoryParams(in, "2Gi", "3Gi") },
			outcome: denied, reason: policy.ReasonAboveMaximum},
		{name: "memory max in decimal units", base: memoryInput,
			mutate: func(in *policy.DecisionInput) {
				memoryBounds(in).Max = resource.MustParse("2G")
				memoryParams(in, "1800M", "2Gi")
			},
			outcome: denied, reason: policy.ReasonAboveMaximum},
		{name: "memory increase above maxIncreasePercent", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { memoryParams(in, "256Mi", "385Mi") },
			outcome: denied, reason: policy.ReasonIncreaseTooLarge},
		{name: "memory increase one byte above maxIncreasePercent", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { memoryParams(in, "256Mi", "402653185") },
			outcome: denied, reason: policy.ReasonIncreaseTooLarge},
		{name: "memory decrease when maxDecreasePercent is 0", base: memoryInput,
			mutate:  func(in *policy.DecisionInput) { memoryParams(in, "256Mi", "255Mi") },
			outcome: denied, reason: policy.ReasonDecreaseNotAllowed},
		{name: "memory decrease above maxDecreasePercent", base: memoryInput,
			mutate: func(in *policy.DecisionInput) {
				memoryBounds(in).MaxDecreasePercent = new(int32(25))
				memoryParams(in, "1Gi", "767Mi")
			},
			outcome: denied, reason: policy.ReasonDecreaseTooLarge},
		{name: "memory decrease within maxDecreasePercent", base: memoryInput,
			mutate: func(in *policy.DecisionInput) {
				memoryBounds(in).MaxDecreasePercent = new(int32(25))
				memoryParams(in, "1Gi", "768Mi")
			},
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},
		{name: "memory up to 1Ti with the largest increase percentage", base: memoryInput,
			mutate: func(in *policy.DecisionInput) {
				memoryBounds(in).Max = resource.MustParse("1Ti")
				memoryBounds(in).MaxIncreasePercent = 1000
				memoryParams(in, "512Gi", "1Ti")
			},
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},

		// Source.
		{name: "resolved repository differs from the policy's", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Source.RepositoryRef = otherName },
			outcome: denied, reason: policy.ReasonRepositoryNotAllowed},
		{name: "resolved repository empty", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				in.Source.RepositoryRef = ""
				in.Policy.Spec.GitOps.RepositoryRef = ""
			},
			outcome: denied, reason: policy.ReasonRepositoryNotAllowed},
		{name: "repository not in settings", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Settings.Spec.Repositories = nil },
			outcome: denied, reason: policy.ReasonRepositoryNotConfigured},
		{name: "branch differs from the repository's targetBranch", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Source.TargetBranch = "dev" },
			outcome: denied, reason: policy.ReasonBranchNotAllowed},
		{name: "explicit targetBranch", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				in.Settings.Spec.Repositories[0].TargetBranch = "release"
				in.Source.TargetBranch = "release"
			},
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},
		{name: "path outside allowedPaths", base: kedaInput,
			mutate:  func(in *policy.DecisionInput) { in.Source.Path = "apps/billing/scaledobject.yaml" },
			outcome: denied, reason: policy.ReasonPathNotAllowed},
		{name: "path in a hidden directory", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				in.Policy.Spec.GitOps.AllowedPaths = []v1alpha1.GitPathPattern{allApps}
				in.Source.Path = "apps/.hidden/scaledobject.yaml"
			},
			outcome: denied, reason: policy.ReasonPathNotAllowed},
		{name: "path matching a later pattern", base: kedaInput,
			mutate: func(in *policy.DecisionInput) {
				in.Policy.Spec.GitOps.AllowedPaths = []v1alpha1.GitPathPattern{"infra/*.yaml", "apps/*/scaledobject.yaml"}
			},
			outcome: allowed, reason: policy.ReasonWithinApprovedBounds},
	}
}

func TestDecide(t *testing.T) {
	for _, tt := range decideCases() {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.base()
			tt.mutate(&in)
			got := policy.Decide(in)
			if got.Outcome != tt.outcome || got.Reason != tt.reason {
				t.Fatalf("Decide() = %s/%s (%s), want %s/%s", got.Outcome, got.Reason, got.Message, tt.outcome, tt.reason)
			}
			if got.Message == "" {
				t.Error("Decide() returned an empty message")
			}
			if got.Outcome != policy.OutcomeAllowed && !got.EffectiveApprovalRequired {
				t.Error("a decision that is not Allowed must require approval")
			}
		})
	}
}

// TestAllowedPathHoldsDisallowedKind covers the carry-over from the path matcher review: the path match is
// lexical, so a broad pattern such as apps/** also matches files that declare RBAC, Secrets, Flux objects or
// policies. The kind check refuses them even though the path is allowed.
func TestAllowedPathHoldsDisallowedKind(t *testing.T) {
	tests := []struct {
		apiVersion, kind, path string
		reason                 policy.Reason
	}{
		{"rbac.authorization.k8s.io/v1", "Role", "apps/orders/rbac.yaml", policy.ReasonKindExcluded},
		{"rbac.authorization.k8s.io/v1", "RoleBinding", "apps/orders/rbac.yaml", policy.ReasonKindExcluded},
		{"v1", "Secret", "apps/orders/secret.yaml", policy.ReasonKindExcluded},
		{"v1", "ServiceAccount", "apps/orders/sa.yaml", policy.ReasonKindExcluded},
		{"v1", "PersistentVolumeClaim", "apps/orders/pvc.yaml", policy.ReasonKindExcluded},
		{"v1", "PersistentVolume", "apps/orders/pv.yaml", policy.ReasonKindExcluded},
		{"v1", "Namespace", "apps/orders/namespace.yaml", policy.ReasonKindExcluded},
		{"helm.toolkit.fluxcd.io/v2", "HelmRelease", "apps/orders/release.yaml", policy.ReasonKindExcluded},
		{"networking.k8s.io/v1", "NetworkPolicy", "apps/orders/netpol.yaml", policy.ReasonKindExcluded},
		{"kustomize.toolkit.fluxcd.io/v1", "Kustomization", "apps/orders/flux.yaml", policy.ReasonKindExcluded},
		{"source.toolkit.fluxcd.io/v1", "GitRepository", "apps/orders/flux.yaml", policy.ReasonKindExcluded},
		{"remediation.shadewright.io/v1alpha1", "RemediationPolicy", "apps/orders/policy.yaml", policy.ReasonKindExcluded},
		{"admissionregistration.k8s.io/v1", "ValidatingWebhookConfiguration", "apps/orders/hook.yaml", policy.ReasonKindExcluded},
		{"apiextensions.k8s.io/v1", "CustomResourceDefinition", "apps/orders/crd.yaml", policy.ReasonKindExcluded},
		{"v1", "Service", "apps/orders/service.yaml", policy.ReasonKindNotAllowed},
		{"v1", "ConfigMap", "apps/orders/config.yaml", policy.ReasonKindNotAllowed},
		{"apps/v1", "Deployment", deploymentPath, policy.ReasonKindNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			in := kedaInput()
			in.Policy.Spec.GitOps.AllowedPaths = []v1alpha1.GitPathPattern{allApps}
			in.Source.Path = v1alpha1.GitFilePath(tt.path)
			setTarget(&in, tt.apiVersion, tt.kind)
			got := policy.Decide(in)
			if got.Outcome != policy.OutcomeDenied || got.Reason != tt.reason {
				t.Fatalf("Decide() = %s/%s (%s), want Denied/%s", got.Outcome, got.Reason, got.Message, tt.reason)
			}
		})
	}
	// The same input with an allowed kind passes, so the refusals above come from the kind alone.
	in := kedaInput()
	in.Policy.Spec.GitOps.AllowedPaths = []v1alpha1.GitPathPattern{allApps}
	if got := policy.Decide(in); got.Outcome != policy.OutcomeAllowed {
		t.Fatalf("Decide() = %s/%s (%s), want Allowed", got.Outcome, got.Reason, got.Message)
	}
}

// TestEffectiveApprovalNeverLessStrict checks every combination of policy approval mode and requested
// approval: approval is waived only when the policy is Optional and the request asked for false, so a
// policy can never make approval less strict than the request asked for.
func TestEffectiveApprovalNeverLessStrict(t *testing.T) {
	modes := []v1alpha1.ApprovalMode{v1alpha1.ApprovalRequired, v1alpha1.ApprovalOptional, "", "Never", "optional"}
	requests := []*bool{nil, new(true), new(false)}
	for _, base := range []func() policy.DecisionInput{kedaInput, memoryInput} {
		for _, mode := range modes {
			for _, req := range requests {
				in := base()
				in.Policy.Spec.Approval = mode
				in.Request.Spec.ApprovalRequired = req
				got := policy.Decide(in)
				if got.Outcome != policy.OutcomeAllowed {
					t.Fatalf("Decide() = %s/%s, want Allowed", got.Outcome, got.Reason)
				}
				requested := req == nil || *req
				if requested && !got.EffectiveApprovalRequired {
					t.Errorf("mode %q, request %v: effective approval is false, but the request asked for approval", mode, requested)
				}
				want := mode != v1alpha1.ApprovalOptional || requested
				if got.EffectiveApprovalRequired != want {
					t.Errorf("mode %q, request %v: EffectiveApprovalRequired = %v, want %v",
						mode, requested, got.EffectiveApprovalRequired, want)
				}
			}
		}
	}
	// A denied plan always requires approval, even under an Optional policy with approvalRequired: false.
	in := kedaInput()
	in.Policy.Spec.Approval = v1alpha1.ApprovalOptional
	in.Request.Spec.ApprovalRequired = new(false)
	in.Source.Path = "apps/billing/scaledobject.yaml"
	if got := policy.Decide(in); got.Outcome != policy.OutcomeDenied || !got.EffectiveApprovalRequired {
		t.Fatalf("Decide() = %s, approval %v; want Denied with approval required", got.Outcome, got.EffectiveApprovalRequired)
	}
}

// TestUnknownActionNeverAllowed checks that no action type outside the catalog is Allowed, whatever the
// policy lists and whichever parameters are set.
func TestUnknownActionNeverAllowed(t *testing.T) {
	types := []v1alpha1.ActionType{"", "RestartPod", "adjustKedaMaxReplicas", "AdjustKedaMaxReplicas ", "Exec"}
	for _, typ := range types {
		for _, base := range []func() policy.DecisionInput{kedaInput, memoryInput} {
			in := base()
			in.Request.Spec.Action.Type = typ
			in.Policy.Spec.Actions = append(in.Policy.Spec.Actions, v1alpha1.PolicyAction{Type: typ})
			got := policy.Decide(in)
			if got.Outcome != policy.OutcomeDenied || got.Reason != policy.ReasonActionUnknown {
				t.Errorf("type %q: Decide() = %s/%s, want Denied/%s", typ, got.Outcome, got.Reason, policy.ReasonActionUnknown)
			}
		}
	}
}

// TestDecideDoesNotModifyInput checks that Decide only reads its input, including quantities written in
// mixed representations, which Quantity.Cmp would otherwise re-cache in place.
func TestDecideDoesNotModifyInput(t *testing.T) {
	mixed := func() policy.DecisionInput {
		in := memoryInput()
		memoryParams(&in, "256Mi", "0.375Gi")
		memoryBounds(&in).Min = resource.MustParse("0.125Gi")
		memoryBounds(&in).Max = resource.MustParse("2G")
		return in
	}
	mixedDecrease := func() policy.DecisionInput {
		in := mixed()
		memoryParams(&in, "0.5Gi", "400M")
		memoryBounds(&in).MaxDecreasePercent = new(int32(30))
		return in
	}
	for _, base := range []func() policy.DecisionInput{kedaInput, memoryInput, mixed, mixedDecrease} {
		in := base()
		want := base()
		got := policy.Decide(in)
		if got.Outcome != policy.OutcomeAllowed {
			t.Fatalf("Decide() = %s/%s (%s), want Allowed", got.Outcome, got.Reason, got.Message)
		}
		if !reflect.DeepEqual(in.Request, want.Request) || !reflect.DeepEqual(in.Policy, want.Policy) ||
			!reflect.DeepEqual(in.Settings, want.Settings) || !reflect.DeepEqual(in.Source, want.Source) ||
			!reflect.DeepEqual(in.Target, want.Target) {
			t.Error("Decide modified its input")
		}
	}
}

// TestKedaRefusedUnlessReady checks the gate of AdjustKedaMaxReplicas (#228, design 7): a plan within every
// bound is refused, NeedsIntervention with ScaledObjectNotReady, unless the live ScaledObject is ready by the
// KedaReady rule, and waits (OutcomeWait, ScaledObjectReadinessUnknown) while KEDA has not reported on it. Only
// the live object decides; nothing in the request can lift the refusal.
func TestKedaRefusedUnlessReady(t *testing.T) {
	const (
		allowed = policy.OutcomeAllowed
		needs   = policy.OutcomeNeedsIntervention
		wait    = policy.OutcomeWait
	)
	annotate := func(key string) func(in *policy.DecisionInput) {
		return func(in *policy.DecisionInput) {
			in.Target.Object["metadata"].(map[string]any)["annotations"] = map[string]any{key: "true"}
		}
	}
	tests := []struct {
		name   string
		mutate func(in *policy.DecisionInput)
		want   policy.Outcome
	}{
		{"ready", func(*policy.DecisionInput) {}, allowed},
		{"HPAActive Unknown", func(in *policy.DecisionInput) { setCondition(in, "HPAActive", "Unknown", "") }, allowed},
		{"no HPAActive condition yet", func(in *policy.DecisionInput) {
			in.Target.Object[keyStatus].(map[string]any)[keyConditions] = readyConditions()[:4]
		}, allowed},
		{"trigger error", func(in *policy.DecisionInput) {
			setCondition(in, "Ready", condFalse, "TriggerError")
			setCondition(in, "Active", condFalse, "ScalerNotActive")
		}, needs},
		{"partial trigger error", func(in *policy.DecisionInput) { setCondition(in, "Ready", "Unknown", "PartialTriggerError") },
			needs},
		{"on fallback, though Ready", func(in *policy.DecisionInput) { setCondition(in, "Fallback", condTrue, "FallbackExists") },
			needs},
		{"paused", func(in *policy.DecisionInput) {
			setCondition(in, "Ready", condFalse, "ScaledObjectPaused")
			setCondition(in, "Paused", condTrue, "ScaledObjectPaused")
		}, needs},
		{"paused for scale-in only, though Ready", func(in *policy.DecisionInput) {
			setCondition(in, "Paused", condTrue, "ScaledObjectScaleInPaused")
		}, needs},
		{"paused for scale-out only, though Ready", func(in *policy.DecisionInput) {
			setCondition(in, "Paused", condTrue, "ScaledObjectScaleOutPaused")
		}, needs},
		{"pause annotation, before KEDA set Paused", annotate("autoscaling.keda.sh/paused"), needs},
		{"paused-replicas annotation", annotate("autoscaling.keda.sh/paused-replicas"), needs},
		{"paused-scale-in annotation", annotate("autoscaling.keda.sh/paused-scale-in"), needs},
		{"paused-scale-out annotation", annotate("autoscaling.keda.sh/paused-scale-out"), needs},
		{"being deleted", func(in *policy.DecisionInput) {
			in.Target.Object["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-03T10:00:00Z"
		}, needs},
		{"HPA cannot read its metrics, though Ready", func(in *policy.DecisionInput) {
			setCondition(in, "HPAActive", condFalse, "HPAMetricsUnavailable")
		}, needs},
		{"missing object", func(in *policy.DecisionInput) { in.Target.Object = nil }, needs},
		{"malformed conditions", func(in *policy.DecisionInput) {
			in.Target.Object[keyStatus] = map[string]any{keyConditions: "Ready=True"}
		}, needs},
		// The request cannot lift the refusal: an Optional policy with approvalRequired: false still refuses.
		{"trigger error under an Optional policy without approval", func(in *policy.DecisionInput) {
			in.Policy.Spec.Approval = v1alpha1.ApprovalOptional
			in.Request.Spec.ApprovalRequired = new(false)
			setCondition(in, "Ready", condFalse, "TriggerError")
		}, needs},

		{"no status yet", func(in *policy.DecisionInput) { delete(in.Target.Object, keyStatus) }, wait},
		{"no Ready condition yet", func(in *policy.DecisionInput) {
			in.Target.Object[keyStatus].(map[string]any)[keyConditions] = readyConditions()[1:]
		}, wait},
		{"Ready Unknown, as KEDA initialises it", func(in *policy.DecisionInput) { setCondition(in, "Ready", "Unknown", "") },
			wait},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := kedaInput()
			tt.mutate(&in)
			got := policy.Decide(in)
			if got.Outcome != tt.want {
				t.Fatalf("Decide() = %s/%s (%s), want %s", got.Outcome, got.Reason, got.Message, tt.want)
			}
			want := map[policy.Outcome]policy.Reason{allowed: policy.ReasonWithinApprovedBounds,
				needs: policy.ReasonScaledObjectNotReady, wait: policy.ReasonScaledObjectReadinessUnknown}[tt.want]
			if got.Reason != want || (tt.want != allowed && !got.EffectiveApprovalRequired) {
				t.Fatalf("Decide() = %s/%s, approval %v; want %s with approval required",
					got.Outcome, got.Reason, got.EffectiveApprovalRequired, want)
			}
		})
	}
}

// TestKedaGateComesAfterTheDenials checks the order: a plan the policy refuses is Denied, final, whatever the
// state of its ScaledObject, and the gate applies only to AdjustKedaMaxReplicas.
func TestKedaGateComesAfterTheDenials(t *testing.T) {
	for _, change := range []func(in *policy.DecisionInput){
		func(in *policy.DecisionInput) { setCondition(in, "Ready", condFalse, "TriggerError") },
		func(in *policy.DecisionInput) { setCondition(in, "Ready", "Unknown", "") },
	} {
		in := kedaInput()
		change(&in)
		kedaParams(&in, 10, 30)
		if got := policy.Decide(in); got.Outcome != policy.OutcomeDenied || got.Reason != policy.ReasonAboveMaximum {
			t.Fatalf("Decide() = %s/%s, want Denied/%s", got.Outcome, got.Reason, policy.ReasonAboveMaximum)
		}
	}
	mem := memoryInput()
	mem.Target.Object = nil
	if got := policy.Decide(mem); got.Outcome != policy.OutcomeAllowed {
		t.Fatalf("memory action without a live object: Decide() = %s/%s, want Allowed", got.Outcome, got.Reason)
	}
	if r, _ := policy.CheckTargetState(v1alpha1.ActionSetContainerMemoryLimit, nil); r != "" {
		t.Fatalf("CheckTargetState(SetContainerMemoryLimit) = %s, want no state check", r)
	}
}

// TestKedaNeedsTheModuleEnabled checks that AdjustKedaMaxReplicas, which the keda integration module contributes,
// is Denied (ModuleNotEnabled) unless RemediationSettings spec.modules.integrations lists keda (design 2.4 and
// 2.5). Only the spec counts: a status that reports keda detected enables nothing (ADR-0024). The core memory
// action needs no module.
func TestKedaNeedsTheModuleEnabled(t *testing.T) {
	cases := []struct {
		name   string
		change func(s *v1alpha1.RemediationSettings)
	}{
		{"no modules", func(s *v1alpha1.RemediationSettings) { s.Spec.Modules = nil }},
		{"no integrations", func(s *v1alpha1.RemediationSettings) { s.Spec.Modules = &v1alpha1.Modules{} }},
		{"other integrations only", func(s *v1alpha1.RemediationSettings) {
			s.Spec.Modules = &v1alpha1.Modules{Integrations: []v1alpha1.ModuleName{"flux", "kyverno"}}
		}},
		{"keda as a platform", func(s *v1alpha1.RemediationSettings) {
			s.Spec.Modules = &v1alpha1.Modules{Platforms: []v1alpha1.ModuleName{kedaModule}}
		}},
		{"keda only in the status", func(s *v1alpha1.RemediationSettings) {
			s.Spec.Modules = nil
			s.Status.Integrations = []v1alpha1.IntegrationStatus{{Name: kedaModule, Detected: true}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := kedaInput()
			c.change(in.Settings)
			got := policy.Decide(in)
			if got.Outcome != policy.OutcomeDenied || got.Reason != policy.ReasonModuleNotEnabled {
				t.Fatalf("Decide() = %s/%s, want Denied/%s", got.Outcome, got.Reason, policy.ReasonModuleNotEnabled)
			}
		})
	}
	mem := memoryInput()
	mem.Settings.Spec.Modules = nil
	if got := policy.Decide(mem); got.Outcome != policy.OutcomeAllowed {
		t.Fatalf("memory action without modules: Decide() = %s/%s, want Allowed", got.Outcome, got.Reason)
	}
}

// TestLiveObjectMustBeTheTarget checks that a live object whose identity is not the live target's is a
// refusal (TargetMismatch), never a pass, so the state check always reads the object that was authorized.
func TestLiveObjectMustBeTheTarget(t *testing.T) {
	meta := func(in *policy.DecisionInput) map[string]any { return in.Target.Object["metadata"].(map[string]any) }
	cases := map[string]func(in *policy.DecisionInput){
		"apiVersion":  func(in *policy.DecisionInput) { in.Target.Object["apiVersion"] = "keda.sh/v1beta1" },
		"kind":        func(in *policy.DecisionInput) { in.Target.Object["kind"] = "ScaledJob" },
		"namespace":   func(in *policy.DecisionInput) { meta(in)["namespace"] = otherNamespace },
		"name":        func(in *policy.DecisionInput) { meta(in)["name"] = otherName },
		"uid":         func(in *policy.DecisionInput) { meta(in)["uid"] = "66666666-6666-4666-8666-666666666666" },
		"no uid":      func(in *policy.DecisionInput) { delete(meta(in), "uid") },
		"no metadata": func(in *policy.DecisionInput) { delete(in.Target.Object, "metadata") },
	}
	for name, change := range cases {
		for _, base := range []func() policy.DecisionInput{kedaInput, memoryInput} {
			in := base()
			change(&in)
			if got := policy.Decide(in); got.Outcome != policy.OutcomeDenied || got.Reason != policy.ReasonTargetMismatch {
				t.Errorf("%s: Decide() = %s/%s, want Denied/%s", name, got.Outcome, got.Reason, policy.ReasonTargetMismatch)
			}
		}
	}
}
