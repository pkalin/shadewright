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

// Package analyzer is the read-mostly side of Shadewright: the collector, the Incident controller and the
// analysis. It may write Incidents and create RemediationRequests in the operator namespace, and nothing
// else. It can never write a request's status, so nothing it produces is ever an authorization.
//
// The analyzer's permissions (design 2.2, 10.1):
//
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=replicasets;deployments;statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=remediation.shadewright.io,resources=incidents,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=remediation.shadewright.io,resources=incidents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=remediation.shadewright.io,resources=remediationsettings,verbs=get;list;watch
// +kubebuilder:rbac:groups=remediation.shadewright.io,namespace=system,resources=remediationrequests,verbs=get;list;watch;create
package analyzer
