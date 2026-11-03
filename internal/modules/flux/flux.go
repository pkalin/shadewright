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

package flux

import "github.com/pkalin/shadewright/internal/modules"

// Name is the module's name in RemediationSettings and its status.
const Name = "flux"

// API groups, versions and kinds the module reads.
const (
	KustomizeGroup = "kustomize.toolkit.fluxcd.io"
	HelmGroup      = "helm.toolkit.fluxcd.io"
	SourceGroup    = "source.toolkit.fluxcd.io"

	KustomizeVersion = "v1"
	HelmVersion      = "v2"
	SourceVersion    = "v1"

	KindKustomization = "Kustomization"
	KindHelmRelease   = "HelmRelease"
	KindGitRepository = "GitRepository"
)

// module is the Flux integration. It has no state.
type module struct{}

// Module returns the Flux integration module, for the list of compiled-in modules.
func Module() modules.Integration { return module{} }

func (module) Name() string { return Name }

// Detection declares the GA group versions; the first one served is what status reports.
func (module) Detection() modules.IntegrationDetection {
	return modules.IntegrationDetection{GroupVersions: []string{
		KustomizeGroup + "/" + KustomizeVersion,
		HelmGroup + "/" + HelmVersion,
		SourceGroup + "/" + SourceVersion,
	}}
}

// Hooks contributes the three symptom sources and nothing else: no rules beyond them, no tools and no actions.
func (module) Hooks() modules.Hooks {
	return modules.Hooks{Symptoms: []modules.SymptomSource{
		{
			Group: KustomizeGroup, Versions: []string{KustomizeVersion}, Kind: KindKustomization,
			Resource: "kustomizations", ReportingController: "kustomize-controller",
			Symptoms: kustomizationSymptoms,
		},
		{
			Group: HelmGroup, Versions: []string{HelmVersion}, Kind: KindHelmRelease,
			Resource: "helmreleases", ReportingController: "helm-controller",
			Symptoms: helmReleaseSymptoms,
		},
		{
			Group: SourceGroup, Versions: []string{SourceVersion}, Kind: KindGitRepository,
			Resource: "gitrepositories", ReportingController: "source-controller",
			Symptoms: gitRepositorySymptoms,
		},
	}}
}
