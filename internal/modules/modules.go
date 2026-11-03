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

package modules

import "github.com/pkalin/shadewright/api/v1alpha1"

// GenericPlatform is the name of the generic platform, which is always registered and is detected whenever no
// other platform is certain.
const GenericPlatform = "generic"

// Platform is a cluster platform module (design 2.4). Implementations must be safe for concurrent use and
// return the same values on every call.
type Platform interface {
	// Name identifies the module in settings and status: 1 to 63 lower-case letters, digits or '-',
	// starting with a letter and ending with a letter or digit.
	Name() string
	// Detection says which nodes run on this platform.
	Detection() PlatformDetection
	// Hooks returns what the module contributes.
	Hooks() Hooks
}

// PlatformDetection says how a node is recognised as running on a platform.
type PlatformDetection struct {
	// ProviderIDScheme is the scheme of the node's spec.providerID, without "://", for example "azure" for
	// "azure:///subscriptions/...". It decides for every node whose provider ID is set.
	ProviderIDScheme string
	// NodeLabels are label keys that mark a node as running on this platform. They are read only for a
	// node without a provider ID.
	NodeLabels []string
}

// Integration is a cluster integration module, one per add-on (design 2.5). Implementations must be safe for
// concurrent use and return the same values on every call.
type Integration interface {
	// Name identifies the module in settings and status, with the same rules as [Platform.Name].
	Name() string
	// Detection says when the add-on is installed.
	Detection() IntegrationDetection
	// Hooks returns what the module contributes.
	Hooks() Hooks
}

// IntegrationDetection says how an add-on is recognised from the discovery API.
type IntegrationDetection struct {
	// GroupVersions are API group versions in discovery form ("keda.sh/v1alpha1", or "v1" for the core
	// group). The add-on is detected when the cluster serves any of them. An integration without group
	// versions is never detected through discovery.
	GroupVersions []string
}

// Hooks is what a module contributes. The zero value contributes nothing, which is what a detection-only
// module returns. Fields are added as modules need them; a module that does not set a new field keeps
// contributing nothing through it.
type Hooks struct {
	// Symptoms turn watched objects and their conditions into symptoms.
	Symptoms []SymptomSource
	// Rules are deterministic diagnosis rules.
	Rules []DiagnosisRule
	// Tools are read-only tools offered to the analyzer.
	Tools []Tool
	// Actions are the catalog actions the module may propose. Each still needs its typed parameters, policy
	// rule, GitOps patcher and refusal test (invariant 2); a security integration lists none.
	Actions []v1alpha1.ActionType
}

// SymptomSource turns objects of one watched kind into symptoms. It is a placeholder: its fields come with the
// first integration that watches a kind.
type SymptomSource struct{}

// DiagnosisRule is a deterministic diagnosis rule. It is a placeholder: its fields come with the first module
// that has a rule.
type DiagnosisRule struct{}

// Tool is a read-only tool for the analyzer. It is a placeholder: its fields come with the first module that
// offers a tool.
type Tool struct{}

// generic is the generic platform. It claims no node: it is the answer when no other platform is certain.
type generic struct{}

func (generic) Name() string                 { return GenericPlatform }
func (generic) Detection() PlatformDetection { return PlatformDetection{} }
func (generic) Hooks() Hooks                 { return Hooks{} }
