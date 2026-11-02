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
// ClusterRole); everything else is read-only. The executor never creates requests and never writes incidents.
//
// +kubebuilder:rbac:groups=remediation.shadewright.io,namespace=system,resources=remediationrequests,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=remediation.shadewright.io,namespace=system,resources=remediationrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=remediation.shadewright.io,namespace=system,resources=remediationrequests/finalizers,verbs=update
// +kubebuilder:rbac:groups=remediation.shadewright.io,resources=incidents,verbs=get;list;watch
// +kubebuilder:rbac:groups=remediation.shadewright.io,resources=remediationpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=remediation.shadewright.io,resources=remediationsettings,verbs=get;list;watch

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
