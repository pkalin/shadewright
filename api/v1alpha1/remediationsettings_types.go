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
	// before each write. It revokes no published pull request and reverts no merge; see
	// https://shadewright.io/emergency/.
	// +kubebuilder:default=false
	// +optional
	Paused *bool `json:"paused,omitempty"`

	// maxConcurrentRemediations limits the requests that are active at once in the operator namespace: from
	// PolicyChecked up to the merge, and while a deleted one still closes its pull request. A further request
	// the policy allows stays Pending, with the condition Queued, until fewer are active; a refusal at the first
	// check is never held back, but a request already Queued is decided again only once the limits let it
	// through. Read from the API server right before each allowed decision.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	// +kubebuilder:default=1
	// +optional
	MaxConcurrentRemediations int32 `json:"maxConcurrentRemediations,omitempty"`

	// maxAnalysesPerDay is deprecated: it moved to spec.analysis.maxAnalysesPerDay. It has no default, and
	// a rule on the field refuses any write that sets or changes it. An object stored by v0.1.0 that holds
	// it can still be updated as long as the value is left as it is (CRD validation ratcheting), and removing
	// it is always allowed. While it is set, the analysis trigger goes by the lower of it and
	// spec.analysis.maxAnalysesPerDay, so an old 0 keeps analysis off. Move the value and remove this field.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10000
	// +kubebuilder:validation:XValidation:rule="false",message="spec.maxAnalysesPerDay moved to spec.analysis.maxAnalysesPerDay: set it there and remove this field"
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

	// analysis bounds every model-backed analysis and names the model deployments it may call. Left out, its
	// limits take their defaults and no model is configured. maxAnalysesPerDay is read by the analysis
	// trigger; the analysis loop, which the analyzer starts only with its --triage-model flag, reads and
	// enforces the rest, maxTokensPerDay as a ceiling for one analysis only until the spend counter of #208. The
	// endpoints decide where the analyzer sends its Entra ID token and the redacted context, so write access
	// to this object is security-relevant (threat model).
	// +kubebuilder:default={}
	// +optional
	Analysis *AnalysisSettings `json:"analysis,omitempty"`
}

// AnalysisSettings bounds each model-backed analysis (design 12.3). Every limit has a safe default and a hard
// upper bound in the schema. maxAnalysesPerDay is read by the analysis trigger; the other limits and the
// models by the analysis loop, which the analyzer starts only with its --triage-model flag.
type AnalysisSettings struct {
	// maxAnalysesPerDay limits model-backed analyses per UTC day, across all Incidents; 0 disables the
	// model. 0 to 10000. Defaulted to 100 also when spec.analysis is left out. Read today by the analysis
	// trigger: 0 marks every Incident not due (AnalysisDisabled). The analysis loop reserves one unit per
	// attempt before its first model call (ADR-0025), so the count is enforced while it runs. Replaces the
	// deprecated spec.maxAnalysesPerDay; while that is still set, the lower of the two applies.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10000
	// +kubebuilder:default=100
	// +optional
	MaxAnalysesPerDay *int32 `json:"maxAnalysesPerDay,omitempty"`

	// maxContextBytes is the most bytes of rendered context message (the preamble, the block tags and
	// notices, and the redacted, escaped text of events, pod status, log excerpts, metrics and policy bounds)
	// sent to the model in one analysis. 4096 to 49152; the default and maximum of 49152 (48 KiB) is roughly
	// 12,000 tokens. The section bounds (summary, status, logs, metrics, Git, policy) stay fixed and add up
	// to 48 KiB of block text; when the rendered message exceeds maxContextBytes (from a lower bound, escaping
	// or block tags), a cut by the context builder or by the analyzer while appending a reviewer hint or
	// readiness notice marks the context truncated, which is an escalation trigger (ADR-0023).
	// +kubebuilder:validation:Minimum=4096
	// +kubebuilder:validation:Maximum=49152
	// +kubebuilder:default=49152
	// +optional
	MaxContextBytes *int32 `json:"maxContextBytes,omitempty"`

	// maxToolSteps is the most read tool calls one escalated analysis may make. The triage call has no tools
	// (ADR-0023), so this only bounds escalation. 0 to 16; 0 means escalation gets no tools.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=16
	// +kubebuilder:default=4
	// +optional
	MaxToolSteps *int32 `json:"maxToolSteps,omitempty"`

	// maxOutputTokens is the most output tokens one model call may produce, sent as the request's
	// MaxOutputTokens, so the provider stops there. An analysis makes at most 3 + maxToolSteps model calls:
	// the triage call, one retry after invalid output, and maxToolSteps + 1 escalated turns (an invalid
	// escalated result gets no retry; it is handed off). That bounds its output. 256 to 16384.
	// +kubebuilder:validation:Minimum=256
	// +kubebuilder:validation:Maximum=16384
	// +kubebuilder:default=2048
	// +optional
	MaxOutputTokens *int32 `json:"maxOutputTokens,omitempty"`

	// maxConcurrentAnalyses is the most analyses that call a model at once. 1 to 8.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	// +kubebuilder:default=1
	// +optional
	MaxConcurrentAnalyses *int32 `json:"maxConcurrentAnalyses,omitempty"`

	// models names the model deployments the analyzer may call. Left out, no model is configured, so no
	// model is called and every incident no runbook matches is handed off to a person.
	// +optional
	Models *AnalysisModels `json:"models,omitempty"`
}

// AnalysisModels names the two model deployments of ADR-0023: a small model for the one structured triage
// call, and a stronger one used only on escalation. There is no field for a key: the analyzer authenticates
// to Foundry with its workload identity (Entra ID, design 11.1), reads an Anthropic API key from a mounted
// file (ADR-0037), and optionally reads a separate endpoint-bound OpenAI-compatible key (ADR-0042). No
// credential is ever stored in this object. The two
// must be different deployments, so the escalation cap cannot be a second budget for the triage model.
// +kubebuilder:validation:XValidation:rule="!has(self.escalation) || has(self.triage)",message="an escalation model needs a triage model"
// +kubebuilder:validation:XValidation:rule="!has(self.triage) || !has(self.escalation) || (self.triage.endpoint.endsWith('/') ? self.triage.endpoint.substring(0, self.triage.endpoint.size() - 1) : self.triage.endpoint) != (self.escalation.endpoint.endsWith('/') ? self.escalation.endpoint.substring(0, self.escalation.endpoint.size() - 1) : self.escalation.endpoint) || self.triage.deployment.lowerAscii() != self.escalation.deployment.lowerAscii()",message="triage and escalation must not name the same endpoint and deployment"
type AnalysisModels struct {
	// triage is the small model for the one structured call on the prepared context, without tools. Left out,
	// no model is called: an incident no runbook matches is handed off to a person, and no unit of
	// spec.analysis.maxAnalysesPerDay is used for it.
	// +optional
	Triage *ModelDeployment `json:"triage,omitempty"`

	// escalation is the stronger model, called with read tools only on the deterministic escalation signals
	// of ADR-0023. Left out, an escalation trigger hands the incident off to a person.
	// +optional
	Escalation *ModelDeployment `json:"escalation,omitempty"`
}

// ModelProvider names the API a model deployment's adapter speaks (ADR-0037), not the model family: a Claude
// model served by Microsoft Foundry is still Foundry.
// +kubebuilder:validation:Enum=Foundry;Anthropic;OpenAICompatible;VertexAI
type ModelProvider string

// The model providers.
const (
	// ModelProviderFoundry is Microsoft Foundry (Azure AI), reached with the analyzer's workload identity.
	ModelProviderFoundry ModelProvider = "Foundry"
	// ModelProviderAnthropic is the Anthropic API at its fixed host, reached with an API key the analyzer reads
	// from a mounted file.
	ModelProviderAnthropic ModelProvider = "Anthropic"
	// ModelProviderOpenAICompatible is an operator-allowed OpenAI-format HTTPS endpoint (ADR-0042).
	ModelProviderOpenAICompatible ModelProvider = "OpenAICompatible"
	// ModelProviderVertexAI is Google Vertex AI generateContent at an EU regional endpoint (ADR-0050). The
	// endpoint carries the API version, project and location, so apiVersion and inferenceAPI are forbidden.
	ModelProviderVertexAI ModelProvider = "VertexAI"
)

// InferenceAPI names the inference API a Foundry or OpenAICompatible deployment is called through.
// +kubebuilder:validation:Enum=ChatCompletions;Responses
type InferenceAPI string

// The OpenAI-format inference APIs.
const (
	// InferenceAPIChatCompletions is the chat completions API, used when inferenceAPI is left out.
	InferenceAPIChatCompletions InferenceAPI = "ChatCompletions"
	// InferenceAPIResponses is the Responses API (Foundry needs apiVersion v1).
	InferenceAPIResponses InferenceAPI = "Responses"
)

// AnthropicEndpoint is the only endpoint of a deployment with provider Anthropic (ADR-0037 decision 2).
const AnthropicEndpoint = "https://api.anthropic.com"

// The second rule on ModelDeployment refuses the host api.anthropic.com for every provider other than
// Anthropic, not for Foundry only: a provider added later inherits the ban on purpose, so no credential of
// another provider can be sent to that host. TestAnthropicEndpointInRules checks that the rules name
// AnthropicEndpoint and its host.
//
// apiVersion is required for Foundry, optional for Anthropic and forbidden for OpenAICompatible and VertexAI.
// The rules that name googleapis.com and VertexAI only refuse new values of a provider that did not exist
// before, or a host no Foundry or compatible deployment is expected to use (ADR-0050 decision 1). The
// googleapis.com rule is not a transition rule, so an object stored earlier with provider OpenAICompatible and
// a googleapis.com endpoint is refused on its next edit until the endpoint is changed; docs/upgrade-uninstall.md
// says so.

// ModelDeployment is one model deployment behind an inference endpoint, for example in Microsoft Foundry
// (ADR-0016), or a model of the Anthropic API (ADR-0037). It holds no key, token or other credential.
// +kubebuilder:validation:XValidation:rule="has(self.apiVersion) || (has(self.provider) && self.provider in ['Anthropic', 'OpenAICompatible', 'VertexAI'])",message="a Foundry deployment needs apiVersion",fieldPath=".apiVersion"
// +kubebuilder:validation:XValidation:rule="!has(self.provider) || self.provider != 'Anthropic' || self.endpoint == 'https://api.anthropic.com'",message="a deployment with provider Anthropic must have the endpoint https://api.anthropic.com"
// +kubebuilder:validation:XValidation:rule="(has(self.provider) && self.provider == 'Anthropic') || !isURL(self.endpoint) || url(self.endpoint).getHostname() != 'api.anthropic.com'",message="the host api.anthropic.com is only for a deployment with provider Anthropic"
// +kubebuilder:validation:XValidation:rule="!has(self.inferenceAPI) || !has(self.provider) || self.provider in ['Foundry', 'OpenAICompatible']",message="inferenceAPI is only for a deployment with provider Foundry or OpenAICompatible"
// +kubebuilder:validation:XValidation:rule="(has(self.provider) && self.provider == 'OpenAICompatible') || !has(self.inferenceAPI) || self.inferenceAPI != 'Responses' || (has(self.apiVersion) && self.apiVersion == 'v1')",message="inferenceAPI Responses needs apiVersion v1"
// +kubebuilder:validation:XValidation:rule="!has(self.provider) || self.provider != 'OpenAICompatible' || !has(self.apiVersion)",message="OpenAICompatible does not allow apiVersion"
// +kubebuilder:validation:XValidation:rule="!has(self.provider) || self.provider != 'OpenAICompatible' || !isURL(self.endpoint) || !['openai.azure.com', 'cognitiveservices.azure.com', 'services.ai.azure.com'].exists(s, url(self.endpoint).getHostname() == s || url(self.endpoint).getHostname().endsWith('.' + s))",message="OpenAICompatible does not allow Azure model hosts"
// +kubebuilder:validation:XValidation:rule="(has(self.provider) && self.provider != 'Foundry') || !isURL(self.endpoint) || url(self.endpoint).getHostname() != 'api.openai.com'",message="the host api.openai.com needs provider OpenAICompatible"
// +kubebuilder:validation:XValidation:rule="!has(self.provider) || self.provider != 'VertexAI' || (!has(self.apiVersion) && !has(self.inferenceAPI))",message="VertexAI does not allow apiVersion or inferenceAPI"
// +kubebuilder:validation:XValidation:rule="!has(self.provider) || self.provider != 'VertexAI' || (self.endpoint.matches('^https://europe-(west1|west4|west8|west9|north1|central2|southwest1)-aiplatform[.]googleapis[.]com/v1/projects/[a-z][a-z0-9-]{4,28}[a-z0-9]/locations/europe-(west1|west4|west8|west9|north1|central2|southwest1)$') && self.endpoint.endsWith('/locations/' + self.endpoint.split('/')[2].split('-aiplatform')[0]))",message="a deployment with provider VertexAI must have an EU regional endpoint https://europe-<region>-aiplatform.googleapis.com/v1/projects/<project>/locations/europe-<region>, where the region is one of europe-west1, europe-west4, europe-west8, europe-west9, europe-north1, europe-central2 and europe-southwest1 (ADR-0050), with the same location in host and path"
// +kubebuilder:validation:XValidation:rule="(has(self.provider) && self.provider == 'VertexAI') || !isURL(self.endpoint) || !(url(self.endpoint).getHostname() == 'googleapis.com' || url(self.endpoint).getHostname().endsWith('.googleapis.com'))",message="googleapis.com hosts are only for a deployment with provider VertexAI"
type ModelDeployment struct {
	// provider is the API the analyzer's adapter speaks to this deployment: Foundry (the default),
	// Anthropic, OpenAICompatible (ADR-0042) or VertexAI (ADR-0050). It names the API, not the model family: a Claude model on Microsoft Foundry is Foundry. With
	// Anthropic the endpoint must be https://api.anthropic.com, the analyzer reads its API key from the file
	// of its --anthropic-api-key-file flag (not in this object) and refuses the deployment without it,
	// deployment must equal the role's --triage-model or --escalation-model value, apiVersion may be left out
	// and is not sent (the adapter's API version is fixed), and the redacted context leaves Azure. A Foundry
	// deployment needs the analyzer's Azure workload identity and --model-endpoint-suffixes; an analyzer
	// started without them refuses it (ProviderNotConfigured). A Foundry deployment may
	// not name the host api.anthropic.com. There is no fallback between providers.
	// VertexAI (ADR-0050) needs an EU regional endpoint, https://europe-<region>-aiplatform.googleapis.com/v1/projects/<project>/locations/europe-<region>,
	// and its deployment is the Gemini model ID. It forbids apiVersion and inferenceAPI: the endpoint carries the API version, project and location.
	// OpenAICompatible matches --openai-compatible-endpoints declarations and their allowed models lists (ModelNotAllowed for an undeclared model), with no Azure identity and an optional API key
	// read from its endpoint's own mounted file. The key is bound to its full install-time base URL, never an Azure model host
	// or api.anthropic.com. Without a key it sends no Authorization header. No apiVersion is allowed.
	// +kubebuilder:default=Foundry
	// +optional
	Provider ModelProvider `json:"provider,omitempty"`

	// endpoint is the HTTPS base URL of the inference endpoint, for example
	// https://<resource>.services.ai.azure.com/ or https://<resource>.openai.azure.com/openai/v1. At most 256
	// characters: a host name of lower-case DNS labels whose last label starts with a letter (so no IP
	// address and no single-label host such as localhost), an optional port from 1 to 65535 and an optional
	// path whose segments do not start with '.'. No user info, query or fragment. The schema is
	// vendor-neutral (ADR-0013). The endpoint decides where the analyzer's Entra ID token and the redacted
	// context go: the analyzer refuses a host outside the allow-list of suffixes of its
	// --model-endpoint-suffixes flag (not in this object) or a port other than 443, refuses private, loopback
	// and link-local addresses when it dials, and requests a token for the Cognitive Services audience only.
	// For provider Anthropic it must be https://api.anthropic.com, the adapter's fixed host, and the allow-list
	// does not apply; for Foundry that host is refused whatever the allow-list says. OpenAICompatible also
	// requires HTTPS and an exact parsed base and API from --openai-compatible-endpoints. The request uses only
	// declared destination data, never a settings path. Foundry refuses declared compatible hosts. Private
	// addresses require a declared Service with egress fields, never loopback or link-local. Each optional
	// key belongs to its declared base. No redirects or environment proxy are used.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^https://([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?(:([1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5]))?(/[A-Za-z0-9_~-][A-Za-z0-9._~-]*)*/?$`
	// +required
	Endpoint string `json:"endpoint"`

	// deployment is the name of the model deployment: 1 to 64 letters, digits, '.', '_' or '-', starting
	// with a letter or digit. For provider Anthropic it is the model ID exactly as the replies name it, such as
	// claude-haiku-5-5. For OpenAICompatible it is the server's model name and must equal a name in the
	// endpoint declaration's models list byte for byte (ModelNotAllowed otherwise).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]*$`
	// +required
	Deployment string `json:"deployment"`

	// apiVersion is the inference API version the adapter sends: v1, or a date such as 2024-10-21 (month 01
	// to 12, day 01 to 31), optionally followed by -preview. Required for provider Foundry. For provider
	// Anthropic leave it out: that adapter's API version is fixed in code, so a value is accepted (an object
	// stored when the field was required stays valid) but never sent.
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^(v1|[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])(-preview)?)$`
	// OpenAICompatible forbids this field: the endpoint includes its API prefix, such as /v1.
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`

	// No schema default on inferenceAPI: the API server would set it on an Anthropic deployment too, and the rule
	// that allows the field only for provider Foundry would then refuse every Anthropic deployment.

	// inferenceAPI is the API a Foundry deployment is called through: ChatCompletions (used when it is left
	// out) or Responses, which needs apiVersion v1 and posts to <endpoint>/openai/v1/responses. Both carry the
	// same redacted context, tool definitions, response schema and output limit; the Responses request asks the
	// service to store nothing. Set Responses for a model that refuses function tools on chat completions,
	// such as the gpt-6.1 models, whose escalation calls need the read tools (#501). OpenAICompatible also
	// accepts this field and appends /responses or /chat/completions to its endpoint without an Azure prefix.
	// +optional
	InferenceAPI InferenceAPI `json:"inferenceAPI,omitempty"`

	// maxTokensPerDay is meant as the daily spend cap of this deployment, in tokens per UTC day, but it is not
	// yet counted across analyses: today it is a ceiling for one analysis, so the daily bound on spend is
	// maxAnalysesPerDay times this ceiling. An analysis counts for the UTC day on which it starts, so one still running
	// after midnight (its deadline is at most 9 minutes) spends in the new day without a unit there: a day can pass
	// the product by one ceiling for each analysis in flight at midnight. It counts input (cached or not)
	// plus output tokens of every call, a failed or timed-out call counted at its upper bound (the input
	// estimate plus maxOutputTokens), except a call the adapter marks as refused by its host before processing
	// (a 429 on every attempt from a Microsoft-operated Azure OpenAI or Foundry host), counted as 0. Required, with no default, so the admin chooses the cap of each
	// deployment. It is a count of tokens, not an amount of money, because tokens are what the provider
	// reports and bills, they are whole numbers, and a money cap would need a price list in this object that
	// goes stale. It is per deployment because the triage and escalation models differ in price. The spend
	// it allows is at most this count times the deployment's highest price per token (design 12.1) only
	// because the counter reserves a call's upper bound before calling, under one lock for concurrent
	// analyses (planned, #208). 0 to 1,000,000,000; 0, or a cap that is spent, stops calls to this deployment
	// and hands the incident off.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000000
	// +required
	MaxTokensPerDay *int64 `json:"maxTokensPerDay,omitempty"`
}

// ProviderName returns the deployment's provider, Foundry when the field is empty (an object read before the
// API server applied the default, or built in code).
func (d ModelDeployment) ProviderName() ModelProvider {
	if d.Provider == "" {
		return ModelProviderFoundry
	}
	return d.Provider
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
	// that it does not have compiled in, which then contributes nothing; DetectionFailed: True when its
	// last run could not list the nodes or read the discovery API, so the report is from the last run that
	// succeeded, or shows only generic active when none has; and NoWatchedNamespaces: True when it was
	// started with no namespace to watch (the chart's default), so it reads no events or pods and records no
	// Incident; and SymptomSourcesNotWatched: True when an active module declares a symptom source for a kind
	// the collector does not watch (not served, not granted, or the check or the watch failed), Unknown while
	// such a kind has not been checked yet, set only while it watches namespaces. A kind whose optional grant
	// the chart leaves out on purpose (the Kyverno cluster-wide kinds) is not reported.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// platform is the platform the analyzer detected and the one it uses. The analyzer writes it at start, when
	// spec changes, every 10 minutes, and when the watch state of a symptom source kind changes (ADR-0024),
	// with the only write it has outside the operator namespace.
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
