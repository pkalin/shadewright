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

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// PolicyTargets says which objects a policy covers.
type PolicyTargets struct {
	// namespaces the policy covers. Required: there is no "all namespaces".
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +required
	Namespaces []Name `json:"namespaces"`

	// objectSelector further limits targets by label.
	// +optional
	ObjectSelector *metav1.LabelSelector `json:"objectSelector,omitempty"`
}

// KedaMaxReplicasBounds bounds AdjustKedaMaxReplicas. Decreases are refused unless maxDecreasePercent is set.
// +kubebuilder:validation:XValidation:rule="self.min <= self.max",message="min must not be greater than max"
type KedaMaxReplicasBounds struct {
	// min is the lowest maxReplicaCount the policy allows.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=1
	// +optional
	Min int32 `json:"min,omitempty"`

	// max is the highest maxReplicaCount the policy allows.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +required
	Max int32 `json:"max"`

	// maxIncreasePercent limits one increase relative to the previous value.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=50
	// +optional
	MaxIncreasePercent int32 `json:"maxIncreasePercent,omitempty"`

	// maxDecreasePercent limits one decrease relative to the previous value; 0 refuses every decrease.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=90
	// +kubebuilder:default=0
	// +optional
	MaxDecreasePercent *int32 `json:"maxDecreasePercent,omitempty"`
}

// MemoryLimitBounds bounds SetContainerMemoryLimit. Decreases are refused unless maxDecreasePercent is set.
// +kubebuilder:validation:XValidation:rule="quantity(string(self.min)).isInteger() && quantity(string(self.max)).isInteger()",message="min and max must be whole bytes"
// +kubebuilder:validation:XValidation:rule="quantity(string(self.min)).compareTo(quantity('1Mi')) >= 0",message="min must be at least 1Mi"
// +kubebuilder:validation:XValidation:rule="quantity(string(self.max)).compareTo(quantity('1Ti')) <= 0",message="max must be at most 1Ti"
// +kubebuilder:validation:XValidation:rule="quantity(string(self.min)).compareTo(quantity(string(self.max))) <= 0",message="min must not be greater than max"
type MemoryLimitBounds struct {
	// min is the lowest memory limit the policy allows.
	// +required
	Min resource.Quantity `json:"min"`

	// max is the highest memory limit the policy allows.
	// +required
	Max resource.Quantity `json:"max"`

	// maxIncreasePercent limits one increase relative to the previous value.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=50
	// +optional
	MaxIncreasePercent int32 `json:"maxIncreasePercent,omitempty"`

	// maxDecreasePercent limits one decrease relative to the previous value; 0 refuses every decrease.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=90
	// +kubebuilder:default=0
	// +optional
	MaxDecreasePercent *int32 `json:"maxDecreasePercent,omitempty"`
}

// PolicyAction allows one action type within bounds. Exactly the bounds member named by type is set.
// +kubebuilder:validation:XValidation:rule="self.type == 'AdjustKedaMaxReplicas' ? has(self.adjustKedaMaxReplicas) : !has(self.adjustKedaMaxReplicas)",message="adjustKedaMaxReplicas must be set if and only if type is AdjustKedaMaxReplicas"
// +kubebuilder:validation:XValidation:rule="self.type == 'SetContainerMemoryLimit' ? has(self.setContainerMemoryLimit) : !has(self.setContainerMemoryLimit)",message="setContainerMemoryLimit must be set if and only if type is SetContainerMemoryLimit"
type PolicyAction struct {
	// type is the allowed action.
	// +required
	Type ActionType `json:"type"`

	// adjustKedaMaxReplicas bounds AdjustKedaMaxReplicas.
	// +optional
	AdjustKedaMaxReplicas *KedaMaxReplicasBounds `json:"adjustKedaMaxReplicas,omitempty"`

	// setContainerMemoryLimit bounds SetContainerMemoryLimit.
	// +optional
	SetContainerMemoryLimit *MemoryLimitBounds `json:"setContainerMemoryLimit,omitempty"`
}

// PolicyGitOps says which repository and files a policy may change.
type PolicyGitOps struct {
	// repositoryRef names an entry in RemediationSettings spec.repositories.
	// +required
	RepositoryRef Name `json:"repositoryRef"`

	// allowedPaths are glob patterns of files the policy may patch. The resolved file must match one.
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +required
	AllowedPaths []GitPathPattern `json:"allowedPaths"`
}

// ApprovalMode says whether a human must approve.
// +kubebuilder:validation:Enum=Required;Optional
type ApprovalMode string

const (
	// ApprovalRequired always needs human review, whatever the request asks.
	ApprovalRequired ApprovalMode = "Required"
	// ApprovalOptional honours approvalRequired=false; repository rules still apply.
	ApprovalOptional ApprovalMode = "Optional"
)

// RemediationPolicySpec is owned by the cluster admin (ADR-0011).
type RemediationPolicySpec struct {
	// requestNamespaces are the namespaces whose RemediationRequests may use this policy. A request from any
	// other namespace is Denied, so being able to create requests somewhere does not grant this policy.
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:default={"shadewright-system"}
	// +optional
	RequestNamespaces []Name `json:"requestNamespaces,omitempty"`

	// targets the policy covers.
	// +required
	Targets PolicyTargets `json:"targets"`

	// actions the policy allows, at most one entry per type. MaxItems follows the size of the action catalog;
	// keeping it small keeps the CEL cost of the bounds rules within the API server's budget.
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=4
	// +required
	Actions []PolicyAction `json:"actions"`

	// gitOps limits where changes may be made.
	// +required
	GitOps PolicyGitOps `json:"gitOps"`

	// approval says whether human review is always required.
	// +kubebuilder:default=Required
	// +optional
	Approval ApprovalMode `json:"approval,omitempty"`
}

// RemediationPolicyStatus reports whether the policy is usable.
type RemediationPolicyStatus struct {
	// observedGeneration is the generation the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions report problems such as an unknown repositoryRef.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=shadewright
// +kubebuilder:printcolumn:name="Approval",type=string,JSONPath=`.spec.approval`
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=`.spec.gitOps.repositoryRef`
// +kubebuilder:printcolumn:name="Revision",type=integer,JSONPath=`.metadata.generation`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RemediationPolicy says which actions are allowed, on which targets, within which bounds, and whether a human
// must approve. Its generation is part of every plan digest, so changing it invalidates pending approvals.
// The name must be a DNS label (at most 63 characters), since requests reference it.
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="name must be at most 63 characters"
type RemediationPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec is the policy.
	// +required
	Spec RemediationPolicySpec `json:"spec"`

	// status reports whether the policy is usable.
	// +optional
	Status RemediationPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RemediationPolicyList contains a list of RemediationPolicy
type RemediationPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []RemediationPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &RemediationPolicy{}, &RemediationPolicyList{})
		return nil
	})
}
