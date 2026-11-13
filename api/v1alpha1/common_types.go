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
)

// ObjectRef identifies one Kubernetes object. The UID pins it, so an object recreated under the same name is
// never mistaken for the one an incident or a plan was about.
type ObjectRef struct {
	// apiVersion of the object, for example apps/v1 or keda.sh/v1alpha1.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?v[0-9]+((alpha|beta)[0-9]+)?$`
	// +required
	APIVersion string `json:"apiVersion"`

	// kind of the object.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Z][A-Za-z0-9]*$`
	// +required
	Kind string `json:"kind"`

	// namespace of the object; empty for cluster-scoped objects.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// name of the object.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +required
	Name string `json:"name"`

	// uid of the object.
	// +optional
	UID UID `json:"uid,omitempty"`
}

// UID is a Kubernetes object UID in its canonical lower-case form. MaxLength states the bound the pattern
// already holds, so the API server can bound the cost of CEL rules that read the field.
// +kubebuilder:validation:MaxLength=36
// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
type UID string

// ActionType is the closed catalog of remediation actions (ADR-0003). Adding a value needs typed parameters,
// policy bounds, a GitOps patcher and a test that it is refused outside its bounds.
// +kubebuilder:validation:Enum=AdjustKedaMaxReplicas;SetContainerMemoryLimit
type ActionType string

const (
	// ActionAdjustKedaMaxReplicas changes maxReplicaCount of a KEDA ScaledObject.
	ActionAdjustKedaMaxReplicas ActionType = "AdjustKedaMaxReplicas"
	// ActionSetContainerMemoryLimit changes the memory limit of one container in a workload's pod template.
	ActionSetContainerMemoryLimit ActionType = "SetContainerMemoryLimit"
)

// ActionTypes returns every value of the ActionType enum, in the enum's order. The analysis builds the model's
// output schema and the catalog section of its prompt from it, so an action added here appears in both.
func ActionTypes() []ActionType {
	return []ActionType{ActionAdjustKedaMaxReplicas, ActionSetContainerMemoryLimit}
}

// Action is one entry from the action catalog. Exactly the member named by type is set.
// +kubebuilder:validation:XValidation:rule="self.type == 'AdjustKedaMaxReplicas' ? has(self.adjustKedaMaxReplicas) : !has(self.adjustKedaMaxReplicas)",message="adjustKedaMaxReplicas must be set if and only if type is AdjustKedaMaxReplicas"
// +kubebuilder:validation:XValidation:rule="self.type == 'SetContainerMemoryLimit' ? has(self.setContainerMemoryLimit) : !has(self.setContainerMemoryLimit)",message="setContainerMemoryLimit must be set if and only if type is SetContainerMemoryLimit"
// +union
type Action struct {
	// type selects the action.
	// +unionDiscriminator
	// +required
	Type ActionType `json:"type"`

	// adjustKedaMaxReplicas holds the parameters for AdjustKedaMaxReplicas.
	// +optional
	AdjustKedaMaxReplicas *AdjustKedaMaxReplicas `json:"adjustKedaMaxReplicas,omitempty"`

	// setContainerMemoryLimit holds the parameters for SetContainerMemoryLimit.
	// +optional
	SetContainerMemoryLimit *SetContainerMemoryLimit `json:"setContainerMemoryLimit,omitempty"`
}

// AdjustKedaMaxReplicas changes spec.maxReplicaCount of a ScaledObject. previousValue must match the file in
// Git when the patch is applied, or the plan is stale.
// +kubebuilder:validation:XValidation:rule="self.desiredValue != self.previousValue",message="desiredValue must differ from previousValue"
type AdjustKedaMaxReplicas struct {
	// previousValue is the value the plan was made against.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +required
	PreviousValue int32 `json:"previousValue"`

	// desiredValue is the new maxReplicaCount.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +required
	DesiredValue int32 `json:"desiredValue"`
}

// SetContainerMemoryLimit changes resources.limits.memory of one container. previous must match the file in
// Git when the patch is applied, or the plan is stale. Both values are whole bytes between 1Mi and 1Ti. The
// rules compare a value only once its approximate float is between -2e12 and 2e12: the quantity library would
// compute a huge decimal exponent digit by digit, for either sign (#593).
// +kubebuilder:validation:XValidation:rule="!(quantity(string(self.previous)).asApproximateFloat() >= -2e12 && quantity(string(self.previous)).asApproximateFloat() <= 2e12 && quantity(string(self.desired)).asApproximateFloat() >= -2e12 && quantity(string(self.desired)).asApproximateFloat() <= 2e12) || quantity(string(self.desired)).compareTo(quantity(string(self.previous))) != 0",message="desired must differ from previous"
// +kubebuilder:validation:XValidation:rule="!(quantity(string(self.previous)).asApproximateFloat() >= -2e12 && quantity(string(self.previous)).asApproximateFloat() <= 2e12 && quantity(string(self.desired)).asApproximateFloat() >= -2e12 && quantity(string(self.desired)).asApproximateFloat() <= 2e12) || (quantity(string(self.previous)).isInteger() && quantity(string(self.desired)).isInteger())",message="previous and desired must be whole bytes"
// +kubebuilder:validation:XValidation:rule="quantity(string(self.previous)).asApproximateFloat() >= -2e12 && quantity(string(self.previous)).asApproximateFloat() <= 2e12 && quantity(string(self.previous)).compareTo(quantity('1Mi')) >= 0 && quantity(string(self.previous)).compareTo(quantity('1Ti')) <= 0",message="previous must be between 1Mi and 1Ti"
// +kubebuilder:validation:XValidation:rule="quantity(string(self.desired)).asApproximateFloat() >= -2e12 && quantity(string(self.desired)).asApproximateFloat() <= 2e12 && quantity(string(self.desired)).compareTo(quantity('1Mi')) >= 0 && quantity(string(self.desired)).compareTo(quantity('1Ti')) <= 0",message="desired must be between 1Mi and 1Ti"
type SetContainerMemoryLimit struct {
	// container is the container name in the pod template.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +required
	Container string `json:"container"`

	// previous is the limit the plan was made against.
	// +required
	Previous resource.Quantity `json:"previous"`

	// desired is the new limit.
	// +required
	Desired resource.Quantity `json:"desired"`
}

// GitFilePath is one YAML file, relative to a repository root. Every segment starts with a letter, digit or
// underscore, so absolute paths, parent references, hidden directories such as .github, option-like names
// and wildcards are all refused.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=512
// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)*\.ya?ml$`
type GitFilePath string

// GitDirPath is a directory relative to a repository root, "." for the root itself. Every segment follows
// the rule of GitFilePath, so absolute paths, parent references, hidden directories and option-like names
// are refused.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=512
// +kubebuilder:validation:Pattern=`^(\.|[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)*)$`
type GitDirPath string

// GitPathPattern is a glob, relative to a repository root, for files a policy may patch. `*` matches within one
// path segment and `**` across segments; neither matches a segment that starts with a dot. The first segment
// must be literal, so a pattern cannot cover the whole repository, and no segment may start with a dot or a
// dash.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=512
// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_*][A-Za-z0-9_.*-]*)*$`
// +kubebuilder:validation:XValidation:rule="!self.contains('..')",message="must not contain '..'"
type GitPathPattern string

// GitBranch is a branch name. Names that git or a shell could read as something else (a leading dash,
// refs/, HEAD, '..', '.lock') are refused, and so is the shadewright/ prefix Shadewright uses for its own
// branches.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=255
// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)*$`
// +kubebuilder:validation:XValidation:rule="!self.contains('..') && !self.endsWith('.lock') && !self.contains('.lock/') && self != 'HEAD' && !self.startsWith('refs/') && !self.startsWith('shadewright/')",message="must be a plain branch name, not HEAD, refs/..., shadewright/... or one containing '..' or '.lock'"
type GitBranch string

// Name is a DNS label used to reference other Shadewright objects or entries.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
type Name string
