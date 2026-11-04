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

// Package eks is the Amazon Elastic Kubernetes Service platform module (design 2.4, ADR-0018). It detects EKS
// and has no diagnosis rules yet.
//
// Detection: a node is on EKS when it carries the label eks.amazonaws.com/nodegroup, which EKS sets on every
// node of a managed node group with the node group's name as its value, and, when its provider ID is set, that
// ID has the "aws" scheme (aws:///<zone>/<instance-id>). The scheme alone is not enough: a kubeadm or Cluster
// API cluster on EC2 has it too.
//
// Only managed node groups set that label. A node from a self-managed node group (unless its kubelet is given
// the label), Karpenter, EKS Auto Mode (eks.amazonaws.com/compute-type: auto) or Fargate does not carry it. Such
// a node, once it has a provider ID, is on no platform, so the cluster is generic ([modules.Registry.DetectPlatform]).
//
// The label comes from the Amazon EKS User Guide:
//
//   - eks.amazonaws.com/nodegroup, set on managed node groups only:
//     https://docs.aws.amazon.com/eks/latest/userguide/ml-workload-placement.html#_labels_that_identify_how_a_node_was_provisioned
//   - managed node group labels carry the eks.amazonaws.com prefix:
//     https://docs.aws.amazon.com/eks/latest/userguide/managed-node-groups.html
//
// The module contributes no symptom sources, rules, tools, actions or verification checks. Like every module it
// is active only when the cluster admin enabled it in RemediationSettings and the cluster is detected as EKS. It
// imports only the standard library and this module, and no AWS SDK (ADR-0013).
package eks

import "github.com/pkalin/shadewright/internal/modules"

// Name is the module's name in RemediationSettings.
const Name = "eks"

// NodeGroupLabel is set by EKS on every node of a managed node group, with the node group's name as its value.
const NodeGroupLabel = "eks.amazonaws.com/nodegroup"

const scheme = "aws"

// Platform is the EKS platform module.
type Platform struct{}

// Name returns "eks".
func (Platform) Name() string { return Name }

// Detection claims nodes with the managed node group label and, when a provider ID is set, the aws scheme.
func (Platform) Detection() modules.PlatformDetection {
	return modules.PlatformDetection{ProviderIDScheme: scheme, NodeLabels: []string{NodeGroupLabel}}
}

// Hooks returns no hooks: the module detects EKS only.
func (Platform) Hooks() modules.Hooks { return modules.Hooks{} }
