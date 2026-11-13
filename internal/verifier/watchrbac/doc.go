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

// Package watchrbac holds only the RBAC markers of the verifier's reads in the watched namespaces (ADR-0030
// section 7). make manifests turns them into the ClusterRole verifier-watch-role
// (config/rbac/verifier/watch/role.yaml), which is bound by one RoleBinding per namespace of the verifier's
// --watch-namespaces and never by a ClusterRoleBinding.
//
// The pods of a target are listed, never read one by one or watched: the verifier keeps no informer on pods
// and reads no pod by name. ReplicaSets, Deployments and StatefulSets are read one at a time by name, to prove
// the owner chain of a pod and to read the target.
//
// +kubebuilder:rbac:groups="",resources=pods,verbs=list
// +kubebuilder:rbac:groups=apps,resources=replicasets;deployments;statefulsets,verbs=get
package watchrbac
