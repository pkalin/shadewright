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
// other platform is certain. It is the API's name for it, which settings may not list.
const GenericPlatform = string(v1alpha1.GenericPlatform)

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

// PlatformDetection says how a node is recognised as running on a platform. A platform with no node labels
// claims no node.
type PlatformDetection struct {
	// ProviderIDScheme is the scheme of the node's spec.providerID, without "://", for example "azure" for
	// "azure:///subscriptions/...". It is an extra condition, checked for a node whose provider ID is set:
	// a scheme names the cloud, not the platform, so it never claims a node on its own and needs NodeLabels.
	// Two platforms may declare the same scheme with different labels.
	ProviderIDScheme string
	// NodeLabels are the label keys a node must all carry to run on this platform, set by the managed
	// service itself, for example "kubernetes.azure.com/cluster" on AKS. Only the keys are read.
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
	// Checks are verification checks the module implements, for verification (planned, #19).
	Checks []VerificationCheck
}

// VerificationCheck implements one of the API's verification checks for one kind: after a change is
// delivered, verification (planned, #19) reads the object the request targets and asks Holds whether the
// check holds. Verification runs in the executor, which is to resolve a check by name in its own registry,
// built from the same compiled-in modules as the analyzer's, among the integrations enabled in
// RemediationSettings. It fails closed: a check the request needs that no enabled module implements fails
// verification (NeedsIntervention) and never counts as passed. [New] refuses a check with an empty Group or
// Kind, no Holds, or a Name outside the API's VerificationCheck values, and a name declared by two modules.
// It is a minimal contract, to be widened when verification is built.
type VerificationCheck struct {
	// Name is the check, one of the API's VerificationCheck values, for example KedaReady.
	Name v1alpha1.VerificationCheck
	// Group and Kind are those of the object the check reads, for example "keda.sh" and "ScaledObject".
	Group string
	Kind  string
	// Holds reports whether the check holds for obj, the object as decoded from JSON. Everything in it is
	// untrusted. Holds only reads obj, never changes it, and does no I/O.
	Holds func(obj map[string]any) bool
}

// SymptomSource turns objects of one watched kind into symptoms. It is declarative apart from Symptoms, a
// pure function, so the caller decides how the kind is watched and a test can feed it recorded objects.
// [New] refuses a source that [SymptomSource.Validate] refuses.
type SymptomSource struct {
	// Group is the API group of the watched kind, for example "kustomize.toolkit.fluxcd.io".
	Group string
	// Versions are the versions of Group the source can read, most preferred first. The caller reads the
	// kind in the first one the cluster serves, and leaves the source out when it serves none.
	Versions []string
	// Kind is the watched kind, for example "Kustomization".
	Kind string
	// Resource is the plural resource name, for RBAC rules, for example "kustomizations".
	Resource string
	// ReportingController is the component whose status the source reads, for example
	// "kustomize-controller". It is what a fingerprint hashes as the reporting controller.
	ReportingController string
	// Symptoms returns the symptoms one object of the kind shows: none when it is healthy or when its status
	// does not describe its current spec. obj is the object as decoded from JSON (the Object map of an
	// unstructured object). Everything in it is untrusted. Symptoms only reads obj, never changes it, and
	// does no I/O.
	Symptoms func(obj map[string]any) []Symptom
	// PodFilter, when set on a source of core Pods, is a cheap check on a pod's labels and container images
	// (spec.containers, in order) that the evaluator runs before it converts the pod for Symptoms: false means
	// Symptoms finds nothing on the pod, so it is not called. It must never return false for a pod Symptoms
	// finds a symptom on. Both inputs are untrusted. PodFilter only reads them and does no I/O.
	PodFilter func(labels map[string]string, images []string) bool
	// Subject, when set (any field), is the kind of controller owner the symptoms are about instead of the
	// watched object itself, for example the ScaledObject that owns an HPA. The evaluator in the analyzer
	// verifies the owner before it uses it (design 3.1); an owner it cannot verify leaves the watched object
	// as the subject.
	Subject SubjectOwner
}

// SubjectOwner is the kind of controller owner a symptom source's symptoms are about. The evaluator takes the
// watched object's owner reference marked controller with this group, version and kind, reads that object
// with a get in the watched object's namespace, and uses it only when it exists with the referenced UID and
// Claims, when set, accepts it. Owner references are untrusted: whoever can write the watched object can
// write them.
type SubjectOwner struct {
	// Group, Version and Kind are those of the owner, for example "keda.sh", "v1alpha1" and "ScaledObject".
	Group   string
	Version string
	Kind    string
	// Resource is the plural resource name, for RBAC rules, for example "scaledobjects".
	Resource string
	// Claims, when set, reports whether owner, the owner as read, names the watched object called name as
	// its own, from a field its controller writes: [ClaimOwned], [ClaimNotOwned] when it names another object
	// or none can be read, or [ClaimPending] when the field is not written yet, which the evaluator retries.
	// Everything in owner is untrusted. Claims only reads owner, never changes it, and does no I/O.
	Claims func(owner map[string]any, name string) Claim
}

// Claim is what a [SubjectOwner]'s Claims found.
type Claim int

const (
	// ClaimNotOwned: the owner names another object, or nothing that can be read as a claim. The evaluator
	// takes the watched object as the subject.
	ClaimNotOwned Claim = iota
	// ClaimOwned: the owner names the watched object.
	ClaimOwned
	// ClaimPending: the owner has not written its claim yet (a controller that has not reconciled it). The
	// evaluator waits and reads the owner again, with a bounded backoff, instead of settling on the watched
	// object as the subject.
	ClaimPending
)

// IsSet reports whether any field of the subject owner is set.
func (o SubjectOwner) IsSet() bool {
	return o.Group != "" || o.Version != "" || o.Kind != "" || o.Resource != "" || o.Claims != nil
}

// APIVersion returns the owner's group version in the form of an apiVersion field.
func (o SubjectOwner) APIVersion() string {
	if o.Group == "" {
		return o.Version
	}
	return o.Group + "/" + o.Version
}

// Tool is a read-only tool for the analyzer. It is a placeholder: its fields come with the first module that
// offers a tool.
type Tool struct{}

// generic is the generic platform. It claims no node: it is the answer when no other platform is certain. Its
// rules (generic.go) hold on every conformant cluster, so they are active on every platform.
type generic struct{}

func (generic) Name() string                 { return GenericPlatform }
func (generic) Detection() PlatformDetection { return PlatformDetection{} }
func (generic) Hooks() Hooks                 { return Hooks{Rules: genericRules()} }
