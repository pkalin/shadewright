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

// GitProviderGitHub uses a GitHub App.
const GitProviderGitHub GitProviderType = "GitHub"

// AzureKeyVaultKey is a key in Azure Key Vault that may only sign (ADR-0006).
type AzureKeyVaultKey struct {
	// keyID is the key identifier, with or without a version, for example
	// https://<vault>.vault.azure.net/keys/github-app.
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://[a-z0-9-]+\.vault\.azure\.net/keys/[A-Za-z0-9-]+(/[0-9a-f]{32})?$`
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
	// appClientID is the GitHub App's client ID.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
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
// +kubebuilder:validation:XValidation:rule="self.provider != 'GitHub' || (self.url.matches('^https://github[.]com/[a-z0-9-]+/[a-z0-9_.-]+$') && !self.url.endsWith('.git') && !self.url.endsWith('/.') && !self.url.endsWith('/..'))",message="a GitHub repository URL must be https://github.com/<owner>/<repo> in lower case, without .git"
type GitRepository struct {
	// name is how policies and requests refer to the repository.
	// +required
	Name Name `json:"name"`

	// url is the HTTPS clone URL, without credentials, query or fragment. It must match the URL of the Flux
	// source that delivers a target, or no change is made.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^https://[a-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9_~-][A-Za-z0-9_.~-]*)+$`
	// +required
	URL string `json:"url"`

	// targetBranch is the only branch pull requests may target. The branch Flux tracks must equal it.
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
}

// RemediationSettingsStatus reports whether the settings are usable.
type RemediationSettingsStatus struct {
	// observedGeneration is the generation the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions report problems such as an unreachable signer.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec holds the settings.
	// +required
	Spec RemediationSettingsSpec `json:"spec"`

	// status reports whether the settings are usable.
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
