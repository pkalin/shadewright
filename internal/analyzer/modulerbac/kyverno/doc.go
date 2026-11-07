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

// Package kyverno makes the ClusterRole analyzer-watch-kyverno: the reads of the Kyverno module's namespaced
// symptom sources (Policies, PolicyReports) in the watched namespaces, bound there by one RoleBinding each (chart
// value analyzer.modules.kyverno.enabled, off by default).
//
// +kubebuilder:rbac:groups=kyverno.io,resources=policies,verbs=get;list;watch
// +kubebuilder:rbac:groups=wgpolicyk8s.io,resources=policyreports,verbs=get;list;watch
package kyverno
