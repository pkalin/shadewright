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

// Package modules holds the registries of cluster platform and cluster integration modules (ADR-0018, design
// sections 2.4 and 2.5).
//
// A [Platform] is one cluster platform (AKS, EKS, GKE, Cluster API); an [Integration] is one add-on in the
// cluster (KEDA, Flux, Kyverno). Each module declares its name, how it is detected and what it contributes
// ([Hooks]). A module contributes hooks (symptom sources, rules, tools, actions, verification checks) only when
// the cluster admin enabled it in RemediationSettings and it is detected; detection alone never adds a tool or
// an action, and an enabled but undetected module contributes nothing (invariant 4).
//
// The core works with no module at all: the generic platform is always present and is the answer
// whenever detection is not certain, and an integration only adds knowledge or capabilities.
//
// Modules are Go packages compiled into the operator (ADR-0019). There is no global registry and no init-time
// registration: the binary builds one immutable [Registry] at start-up with [New] from an explicit list of
// modules, and passes it to whoever needs it. A name given twice, or two platforms with the same scheme and
// node labels, makes [New] fail, so a clash is found when the operator starts instead of shadowing a module.
// Lists are sorted by name, so every caller sees the same order.
//
// Detection is pure: [Registry.DetectPlatform] reads node provider IDs and label keys, and
// [Registry.DetectIntegrations] reads the group versions served by the discovery API. Neither reads the cluster
// itself; the caller collects the input. Detection data is declarative ([PlatformDetection],
// [IntegrationDetection]), so a compiled module can later be expressed as a signed declarative pack.
//
// A provider ID scheme names the cloud, not the managed platform: a Cluster API or kubeadm cluster on Azure
// machines has the "azure" scheme too. A platform is therefore recognised by the labels its managed service
// puts on nodes, every one of which a node must carry, with the scheme as an extra condition for a node whose
// provider ID is set. Nodes without a provider ID and without any label a platform requires (a node still
// joining, a virtual node) are left out; the cluster is on a platform when every other node is, and generic
// otherwise.
//
// [Registry.Select] works out the active modules from the names the cluster admin enabled and a detection,
// never from a status written earlier: generic always, the detected platform when it is enabled, and the
// detected integrations that are enabled. The [Selection] turns into RemediationSettings status and runs the
// [DiagnosisRule] values of its active modules on events and nodes. A rule writes its own reason and summary;
// every evidence value it read from the cluster is redacted and bounded here before it leaves the package.
// The generic platform's rules (node pressure and capacity hints) are active on every platform.
//
// This package imports only the standard library and this module, and nothing it depends on is a product SDK
// or an adapter (ADR-0013).
package modules
