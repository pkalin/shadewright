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

// Package testfixtures builds valid Shadewright objects for tests. Tests change one field and expect the API
// server to accept or refuse exactly that change.
package testfixtures

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// Names shared by the fixtures, so a request, its policy and the settings refer to each other.
const (
	TargetNamespace = "orders"
	RepositoryName  = "fleet"
	WorkloadName    = "orders-worker"
)

// RemediationRequest returns a valid request to raise a ScaledObject's maxReplicaCount from 10 to 15.
func RemediationRequest(namespace, name string) *v1alpha1.RemediationRequest {
	return &v1alpha1.RemediationRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: v1alpha1.RemediationRequestSpec{
			IncidentRef: v1alpha1.IncidentRef{Name: "orders-scaling", UID: "11111111-1111-4111-8111-111111111111"},
			TargetRef: v1alpha1.ObjectRef{
				APIVersion: "keda.sh/v1alpha1",
				Kind:       "ScaledObject",
				Namespace:  TargetNamespace,
				Name:       WorkloadName,
				UID:        "22222222-2222-4222-8222-222222222222",
			},
			PolicyRef: "keda-capacity",
			Action: v1alpha1.Action{
				Type:                  v1alpha1.ActionAdjustKedaMaxReplicas,
				AdjustKedaMaxReplicas: &v1alpha1.AdjustKedaMaxReplicas{PreviousValue: 10, DesiredValue: 15},
			},
		},
	}
}

// MemoryLimitRequest returns a valid request to raise a Deployment container's memory limit from 256Mi to 384Mi.
func MemoryLimitRequest(namespace, name string) *v1alpha1.RemediationRequest {
	rr := RemediationRequest(namespace, name)
	rr.Spec.TargetRef = v1alpha1.ObjectRef{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Namespace:  TargetNamespace,
		Name:       WorkloadName,
		UID:        "33333333-3333-4333-8333-333333333333",
	}
	rr.Spec.Action = MemoryLimitAction()
	return rr
}

// MemoryLimitAction returns a valid SetContainerMemoryLimit action from 256Mi to 384Mi.
func MemoryLimitAction() v1alpha1.Action {
	return v1alpha1.Action{
		Type: v1alpha1.ActionSetContainerMemoryLimit,
		SetContainerMemoryLimit: &v1alpha1.SetContainerMemoryLimit{
			Container: "worker",
			Previous:  resource.MustParse("256Mi"),
			Desired:   resource.MustParse("384Mi"),
		},
	}
}

// Incident returns a valid incident about an OOMKilled Deployment.
func Incident(namespace, name string) *v1alpha1.Incident {
	return &v1alpha1.Incident{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: v1alpha1.IncidentSpec{
			Fingerprint: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Subject: v1alpha1.ObjectRef{
				APIVersion: "apps/v1", Kind: "Deployment", Namespace: TargetNamespace, Name: WorkloadName,
				UID: "33333333-3333-4333-8333-333333333333",
			},
			Reason:  "OOMKilled",
			Symptom: "container worker terminated: OOMKilled, limit 256Mi",
		},
	}
}

// RemediationPolicy returns a valid policy allowing KEDA capacity changes in the orders namespace.
func RemediationPolicy(name string) *v1alpha1.RemediationPolicy {
	return &v1alpha1.RemediationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.RemediationPolicySpec{
			Targets: v1alpha1.PolicyTargets{Namespaces: []v1alpha1.Name{TargetNamespace}},
			Actions: []v1alpha1.PolicyAction{{
				Type:                  v1alpha1.ActionAdjustKedaMaxReplicas,
				AdjustKedaMaxReplicas: &v1alpha1.KedaMaxReplicasBounds{Max: 20},
			}},
			GitOps: v1alpha1.PolicyGitOps{
				RepositoryRef: RepositoryName,
				AllowedPaths:  []v1alpha1.GitPathPattern{"apps/orders/scaledobject.yaml"},
			},
		},
	}
}

// MemoryLimitPolicyAction returns valid bounds for SetContainerMemoryLimit: 128Mi to 2Gi, no decreases.
func MemoryLimitPolicyAction() v1alpha1.PolicyAction {
	return v1alpha1.PolicyAction{
		Type: v1alpha1.ActionSetContainerMemoryLimit,
		SetContainerMemoryLimit: &v1alpha1.MemoryLimitBounds{
			Min: resource.MustParse("128Mi"),
			Max: resource.MustParse("2Gi"),
		},
	}
}

// RemediationSettings returns valid settings with one GitHub repository.
func RemediationSettings() *v1alpha1.RemediationSettings {
	return &v1alpha1.RemediationSettings{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: v1alpha1.RemediationSettingsSpec{
			Repositories: []v1alpha1.GitRepository{{
				Name:     RepositoryName,
				URL:      "https://github.com/example/fleet",
				Provider: v1alpha1.GitProviderGitHub,
				GitHub: &v1alpha1.GitHubRepository{
					AppClientID:    "Iv23exampleclientid",
					InstallationID: 1,
					Signer: v1alpha1.Signer{AzureKeyVault: &v1alpha1.AzureKeyVaultKey{
						KeyID: "https://kv-example.vault.azure.net/keys/github-app",
					}},
				},
			}},
		},
	}
}
