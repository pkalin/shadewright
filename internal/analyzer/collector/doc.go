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

// Package collector watches events, pods and workloads in the namespaces the analyzer is started with
// (--watch-namespaces) and turns them into symptoms (#41).
//
// Its markers generate the ClusterRole analyzer-watch-role (config/rbac/analyzer/watch). That role is only a
// catalogue of verbs: it is bound by one RoleBinding per watched namespace (config/watch), never by a
// ClusterRoleBinding, so the analyzer can read nothing outside the namespaces it was given. Only namespaced
// reads belong here; anything cluster-scoped or written goes in package analyzer.
//
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=replicasets;deployments;statefulsets,verbs=get;list;watch
package collector
