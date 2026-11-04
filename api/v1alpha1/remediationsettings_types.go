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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// GitProviderType names a Git hosting adapter (ADR-0004, ADR-0013).
// +kubebuilder:validation:Enum=GitHub
type GitProviderType string

const (
	// GitProviderGitHub uses a GitHub App.
	GitProviderGitHub GitProviderType = "GitHub"
)

// AzureKeyVaultKey is a key in Azure Key Vault that may only sign (ADR-0006).
type AzureKeyVaultKey struct {
	// keyID is the key identifier, with or without a version, for example
	// https://<vault>.vault.azure.net/keys/github-app. It follows the Key Vault naming rules, as the signer
	// does: a vault name of 3 to 24 lower-case letters, digits and hyphens that starts with a letter, ends with
	// a letter or digit and has no consecutive hyphens; a key name of 1 to 127 letters, digits and hyphens; an
	// optional version of 32 lower-case hexadecimal digits.
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://[a-z][a-z0-9-]{1,22}[a-z0-9]\.vault\.azure\.net/keys/[A-Za-z0-9-]{1,127}(/[0-9a-f]{32})?$`
	// +kubebuilder:validation:XValidation:rule="!self.matches('^https://[a-z0-9-]*--')",message="the vault name must not contain consecutive hyphens"
	// +required
	KeyID string `json:"keyID"`
}

// Signer says where the GitHub App private key lives. The key never enters the cluster.
// +kubebuilder:validation:XValidation:rule="has(self.azureKeyVault)",message="a signer must be configured"
type Signer struct {
	// azureKeyVault signs with a non-exportable Key Vault key through workload identity.
	// +optional
	AzureKeyVault *AzureKeyVaultKey `json:"azureKeyVault,omitempty"`
}

// GitHubRepository holds the GitHub App settings for one repository.
type GitHubRepository struct {
	// appClientID is the GitHub App's client ID: 1 to 64 letters, digits, '.', '_' or '-'.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+$`
	// +required
	AppClientID string `json:"appClientID"`

	// installationID is the App's installation that covers this repository.
	// +kubebuilder:validation:Minimum=1
	// +required
	InstallationID int64 `json:"installationID"`

	// signer says where the App's private key is.
	// +required
	Signer Signer `json:"signer"`
}

// GitRepository is a repository Shadewright may open pull requests in. Requests and policies refer to it by
// name, so the model never chooses a URL or credentials.
// +kubebuilder:validation:XValidation:rule="self.provider == 'GitHub' ? has(self.github) : !has(self.github)",message="github must be set if and only if provider is GitHub"
// +kubebuilder:validation:XValidation:rule="self.provider != 'GitHub' || (self.url.matches('^https://github[.]com/[a-z0-9][a-z0-9-]{0,38}/[a-z0-9_.-]{1,100}$') && !self.url.endsWith('.git') && !self.url.endsWith('/.') && !self.url.endsWith('/..'))",message="a GitHub repository URL must be https://github.com/<owner>/<repo> in lower case, without .git"
// +kubebuilder:validation:XValidation:rule="self.provider != 'GitHub' || (self.targetBranch.matches('^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$') && !self.targetBranch.endsWith('.'))",message="a GitHub target branch must start with a letter or digit, have at most 200 characters and not end with '.'",fieldPath=".targetBranch"
type GitRepository struct {
	// name is how policies and requests refer to the repository.
	// +required
	Name Name `json:"name"`

	// url is the HTTPS clone URL, without credentials, query or fragment, and without a path segment that is
	// '.' or '..'. It must match the URL of the Flux source that delivers a target, or no change is made. For
	// GitHub it is https://github.com/<owner>/<repo> in lower case, without .git: an owner of 1 to 39 letters,
	// digits and hyphens that starts with a letter or digit, and a repository name of 1 to 100 letters, digits,
	// '.', '_' or '-' other than '.' and '..' (it may start with '.', as in .github).
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https://[a-z0-9.-]+(:[0-9]+)?(/(\.?[A-Za-z0-9_~-][A-Za-z0-9_.~-]*|\.\.[A-Za-z0-9_.~-]+))+$`
	// +required
	URL string `json:"url"`

	// targetBranch is the only branch pull requests may target. The branch Flux tracks must equal it. For
	// GitHub it starts with a letter or digit, has at most 200 characters and does not end with '.'.
	// +kubebuilder:default=main
	// +optional
	TargetBranch GitBranch `json:"targetBranch,omitempty"`

	// provider selects the Git hosting adapter.
	// +required
	Provider GitProviderType `json:"provider"`

	// github holds GitHub App settings.
	// +optional
	GitHub *GitHubRepository `json:"github,omitempty"`
}

// ModuleName names a cluster platform or integration module (design 2.4 and 2.5): a DNS label that starts with
// a letter, at most 63 characters, the rule internal/modules checks when it builds its registry (a test keeps
// the two equal). It is a pattern, not a list of known names, so a module compiled into a later release, or
// shipped as a pack (ADR-0019), needs no schema change. A name that no compiled-in module has is accepted and
// contributes nothing.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`
type ModuleName string

// GenericPlatform is the platform of a cluster no other platform claims with certainty. It is always on and
// is never listed in spec.modules.platforms.
const GenericPlatform ModuleName = "generic"

// Modules says which platform and integration modules the cluster admin enables (ADR-0018). Platform and
// integration names are separate namespaces, so they are enabled in separate lists. A module contributes hooks
// (symptom sources, diagnosis rules, tools, actions) only when it is enabled here and the analyzer detects it:
// detection only reports and narrows, it never enables a module, and a module that is enabled but not
// detected contributes nothing. Which modules are active is always worked out again from these lists and live
// detection, never read back from status, which is only a report.
type Modules struct {
	// platforms are the platform modules enabled besides generic, which is always on and is not listed.
	// More than one may be listed, so one settings file can serve clusters on different platforms: only the
	// one the analyzer detects is active. Empty means generic only. The cap of 8 is also the most platform
	// modules besides generic one binary may compile in (internal/modules refuses more), so every one of them
	// can be enabled.
	// +listType=set
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:XValidation:rule="!('generic' in self)",message="the generic platform is always on and is not listed"
	// +optional
	Platforms []ModuleName `json:"platforms,omitempty"`

	// integrations are the integration modules enabled. Each is active only while the analyzer detects its
	// add-on. Empty means none. The cap of 32 is also the most integration modules one binary may compile in
	// (internal/modules refuses more), so every one of them can be enabled.
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Integrations []ModuleName `json:"integrations,omitempty"`
}

// RemediationSettingsSpec holds cluster-wide settings, including the kill switch.
type RemediationSettingsSpec struct {
	// paused stops every write: pull requests, auto-merge and issues. Analysis continues. Checked right
	// before each write.
	// +kubebuilder:default=false
	// +optional
	Paused *bool `json:"paused,omitempty"`

	// maxConcurrentRemediations limits requests between PolicyChecked and a terminal phase.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	// +kubebuilder:default=1
	// +optional
	MaxConcurrentRemediations int32 `json:"maxConcurrentRemediations,omitempty"`

	// maxAnalysesPerDay limits model-backed analyses per UTC day; 0 disables the model.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10000
	// +kubebuilder:default=100
	// +optional
	MaxAnalysesPerDay *int32 `json:"maxAnalysesPerDay,omitempty"`

	// repositories Shadewright may open pull requests in. URLs are unique, so a Flux source matches at most one
	// entry.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:XValidation:rule="self.all(r, self.exists_one(s, s.url == r.url))",message="repository URLs must be unique"
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Repositories []GitRepository `json:"repositories,omitempty"`

	// modules enables platform and integration modules. Left out, only the generic platform is on and no
	// integration is: nothing is enabled because it was detected.
	// +optional
	Modules *Modules `json:"modules,omitempty"`
}

// SettingsName is the name of the one RemediationSettings object.
const SettingsName = "default"

// PlatformStatus reports which platform the analyzer detected and which one it uses. They differ when the
// detected platform is not enabled in spec.modules.platforms: the analyzer then falls back to generic. So the
// three cases are: nothing detected (detected left out, active generic), detected and enabled (active equal to
// detected), and detected but not enabled (active generic). It is a report only: the analyzer never reads it
// back to decide which platform to use, it decides again from spec.modules and live detection each time.
// +kubebuilder:validation:XValidation:rule="!has(self.detected) || self.detected != 'generic'",message="detected is left out, not generic, when no platform claims the cluster"
// +kubebuilder:validation:XValidation:rule="self.active == 'generic' || (has(self.detected) && self.active == self.detected)",message="active is generic or the detected platform"
type PlatformStatus struct {
	// detected is the platform that claims every node with certainty. Left out when none does.
	// +optional
	Detected ModuleName `json:"detected,omitempty"`

	// active is the platform whose module contributes hooks: detected when it is enabled in
	// spec.modules.platforms, generic otherwise.
	// +required
	Active ModuleName `json:"active"`
}

// IntegrationStatus reports one integration module that is enabled, detected or both. It is a report only:
// the analyzer never reads it back to decide which integrations are active, it decides again from
// spec.modules and live detection each time.
// +kubebuilder:validation:XValidation:rule="self.detected == has(self.version)",message="version is set if and only if the integration is detected"
// +kubebuilder:validation:XValidation:rule="!self.active || self.detected",message="an integration that is not detected is not active"
type IntegrationStatus struct {
	// name is the integration module.
	// +required
	Name ModuleName `json:"name"`

	// detected is true when the cluster serves one of the group versions the module declares. An enabled
	// name that no compiled-in module has is never detected.
	// +required
	Detected bool `json:"detected"`

	// version is the group version that detected the add-on, in discovery form (keda.sh/v1alpha1). Set if
	// and only if the add-on is detected.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?v[0-9]+((alpha|beta)[0-9]+)?$`
	// +optional
	Version string `json:"version,omitempty"`

	// active is true when the module contributes hooks: it is enabled in spec.modules.integrations and
	// detected. It reports that decision and never feeds it.
	// +required
	Active bool `json:"active"`
}

// RemediationSettingsStatus is the analyzer's report of platform and integration detection (ADR-0024). It says
// nothing about whether a repository or signer is usable: those are built when a request needs them, and a
// problem shows in the conditions of that request.
type RemediationSettingsStatus struct {
	// observedGeneration is the generation the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions report problems. The analyzer writes UnknownModules: True when spec.modules names a module
	// that it does not have compiled in, which then contributes nothing; and DetectionFailed: True when its
	// last run could not list the nodes or read the discovery API, so the report is from the last run that
	// succeeded, or shows only generic active when none has.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// platform is the platform the analyzer detected and the one it uses. The analyzer writes it at start, when
	// spec changes and every 10 minutes (ADR-0024), with the only write it has outside the operator namespace.
	// Status is a report, never an input: no component reads any field of it to decide anything.
	// +optional
	Platform *PlatformStatus `json:"platform,omitempty"`

	// integrations lists every integration module that is enabled or detected, with whether it is active.
	// A detected add-on whose module is not enabled is listed too, so the cluster admin sees what could be
	// enabled. Written like platform. At most 64: 32 enabled (the spec cap) and 32 more
	// detected, since a detected add-on needs a compiled-in module and internal/modules refuses more than 32.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Integrations []IntegrationStatus `json:"integrations,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=shadewright
// +kubebuilder:printcolumn:name="Paused",type=boolean,JSONPath=`.spec.paused`
// +kubebuilder:printcolumn:name="Max concurrent",type=integer,JSONPath=`.spec.maxConcurrentRemediations`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RemediationSettings is the single cluster-wide configuration object, named default.
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="there is one RemediationSettings and it is named default"
type RemediationSettings struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec holds the settings.
	// +required
	Spec RemediationSettingsSpec `json:"spec"`

	// status is the analyzer's detection report. It does not report whether repositories or signers are usable.
	// +optional
	Status RemediationSettingsStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RemediationSettingsList contains a list of RemediationSettings
type RemediationSettingsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []RemediationSettings `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &RemediationSettings{}, &RemediationSettingsList{})
		return nil
	})
}
