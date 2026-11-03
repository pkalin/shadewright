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

package secretsstore

import "github.com/pkalin/shadewright/internal/modules"

// Name is the module's name in RemediationSettings and its status.
const Name = "secrets-store-csi-driver"

// The API group and version the module detects (secrets-store-csi-driver v1.6.1 apis/v1/doc.go, the
// +groupName marker, and charts/secrets-store-csi-driver/crds, where v1 is served and the storage version).
const (
	Group   = "secrets-store.csi.x-k8s.io"
	Version = "v1"
)

// module is the Secrets Store CSI driver integration. It has no state.
type module struct{}

// Module returns the Secrets Store CSI driver integration module, for the list of compiled-in modules.
func Module() modules.Integration { return module{} }

func (module) Name() string { return Name }

// Detection declares secrets-store.csi.x-k8s.io/v1 only: the deprecated v1alpha1 is served next to it by the
// same CRDs, so it adds no signal.
func (module) Detection() modules.IntegrationDetection {
	return modules.IntegrationDetection{GroupVersions: []string{Group + "/" + Version}}
}

// Hooks contributes the mount failure rule and nothing else: no symptom sources (see the package
// documentation), no tools and no actions. A secrets manager guards security, so the module diagnoses only
// (design 2.5).
func (module) Hooks() modules.Hooks {
	return modules.Hooks{Rules: []modules.DiagnosisRule{{Name: RuleMountFailed, Event: mountFailed}}}
}
