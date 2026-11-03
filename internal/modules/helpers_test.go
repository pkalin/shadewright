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

// Names and group versions the tests use more than once.
const (
	aksName     = "aks"
	eksName     = "eks"
	gkeName     = "gke"
	kedaName    = "keda"
	fluxName    = "flux"
	sharedName  = "shared"
	kedaGV      = "keda.sh/v1alpha1"
	appsGV      = "apps/v1"
	helmGV      = "helm.toolkit.fluxcd.io/v2"
	kustomizeGV = "kustomize.toolkit.fluxcd.io/v1"
	wantName    = "name"
)

// stubPlatform and stubIntegration are detection-only modules for the tests.
type stubPlatform struct {
	name      string
	detection PlatformDetection
}

func (p stubPlatform) Name() string                 { return p.name }
func (p stubPlatform) Detection() PlatformDetection { return p.detection }
func (stubPlatform) Hooks() Hooks                   { return Hooks{} }

type stubIntegration struct {
	name      string
	detection IntegrationDetection
	hooks     Hooks
}

func (i stubIntegration) Name() string                    { return i.name }
func (i stubIntegration) Detection() IntegrationDetection { return i.detection }
func (i stubIntegration) Hooks() Hooks                    { return i.hooks }

var (
	aks = stubPlatform{name: aksName, detection: PlatformDetection{
		ProviderIDScheme: "azure",
		NodeLabels:       []string{"kubernetes.azure.com/cluster"},
	}}
	eks = stubPlatform{name: eksName, detection: PlatformDetection{
		ProviderIDScheme: "aws",
		NodeLabels:       []string{"eks.amazonaws.com/nodegroup"},
	}}
	gke = stubPlatform{name: gkeName, detection: PlatformDetection{
		ProviderIDScheme: "gce",
		NodeLabels:       []string{"cloud.google.com/gke-nodepool"},
	}}
	keda = stubIntegration{
		name:      kedaName,
		detection: IntegrationDetection{GroupVersions: []string{kedaGV}},
		hooks:     Hooks{Actions: []v1alpha1.ActionType{v1alpha1.ActionAdjustKedaMaxReplicas}},
	}
	flux = stubIntegration{name: fluxName, detection: IntegrationDetection{GroupVersions: []string{
		kustomizeGV, helmGV,
	}}}
)

// mustNew builds a registry or fails the test.
func mustNew(t interface {
	Helper()
	Fatalf(string, ...any)
}, platforms []Platform, integrations []Integration) *Registry {
	t.Helper()
	r, err := New(platforms, integrations)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}
