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

// Package gke is the Google Kubernetes Engine platform module (design 2.4, ADR-0018). It detects GKE and has no
// diagnosis rules yet.
//
// Detection: a node is on GKE when it carries the label cloud.google.com/gke-nodepool, which GKE sets on every
// node with the node pool's name as its value, and, when its provider ID is set, that ID has the "gce" scheme
// (gce://<project>/<zone>/<instance>). The scheme alone is not enough: a kubeadm or Cluster API cluster on
// Compute Engine has it too.
//
// The label comes from the GKE documentation:
//
//   - cloud.google.com/gke-nodepool, on each node of a node pool with the pool's name as its value:
//     https://docs.cloud.google.com/kubernetes-engine/docs/concepts/node-pools
//
// The module contributes no symptom sources, rules, tools, actions or verification checks. Like every module it
// is active only when the cluster admin enabled it in RemediationSettings and the cluster is detected as GKE. It
// imports only the standard library and this module, and no Google Cloud SDK (ADR-0013).
package gke

import "github.com/pkalin/shadewright/internal/modules"

// Name is the module's name in RemediationSettings.
const Name = "gke"

// NodePoolLabel is set by GKE on every node, with the node pool's name as its value.
const NodePoolLabel = "cloud.google.com/gke-nodepool"

const scheme = "gce"

// Platform is the GKE platform module.
type Platform struct{}

// Name returns "gke".
func (Platform) Name() string { return Name }

// Detection claims nodes with the node pool label and, when a provider ID is set, the gce scheme.
func (Platform) Detection() modules.PlatformDetection {
	return modules.PlatformDetection{ProviderIDScheme: scheme, NodeLabels: []string{NodePoolLabel}}
}

// Hooks returns no hooks: the module detects GKE only.
func (Platform) Hooks() modules.Hooks { return modules.Hooks{} }
