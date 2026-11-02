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

package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// The executor's permissions. Requests are reachable only in the operator namespace (a Role, not a
// ClusterRole), and only their status is writable; everything else is read-only. The executor never creates
// requests and never writes incidents. The finalizer that closes a change request on deletion (design 6)
// adds update on remediationrequests and remediationrequests/finalizers when it lands, with a test.
//
// +kubebuilder:rbac:groups=remediation.shadewright.io,namespace=system,resources=remediationrequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=remediation.shadewright.io,namespace=system,resources=remediationrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=remediation.shadewright.io,resources=incidents,verbs=get;list;watch
// +kubebuilder:rbac:groups=remediation.shadewright.io,resources=remediationpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=remediation.shadewright.io,resources=remediationsettings,verbs=get;list;watch
//
// Source mapping (ADR-0012) reads the target and the Flux objects that deliver it, one object at a time through
// the manager's API reader (never the cache), so get is enough: no list, no watch. The kinds are the action catalog's targets and the
// Flux Kustomization and GitRepository; Secrets are never granted.
//
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=get
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=gitrepositories,verbs=get

// CacheOptions restricts the executor's cache so that RemediationRequests are watched only in the operator
// namespace. A request created anywhere else is never seen, so it can never be acted on (ADR-0011).
func CacheOptions(namespace string) cache.Options {
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&remediationv1alpha1.RemediationRequest{}: {
				Namespaces: map[string]cache.Config{namespace: {}},
			},
		},
	}
}
