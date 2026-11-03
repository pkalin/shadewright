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
// ([Hooks]). The core works with no module at all: the generic platform is always present and is the answer
// whenever detection is not certain, and an integration only adds knowledge or capabilities.
//
// Modules are Go packages compiled into the operator (ADR-0019). There is no global registry and no init-time
// registration: the binary builds one immutable [Registry] at start-up with [New] from an explicit list of
// modules, and passes it to whoever needs it. A name given twice, or two modules claiming the same detection
// key, makes [New] fail, so a clash is found when the operator starts instead of shadowing a module. Lists
// are sorted by name, so every caller sees the same order.
//
// Detection is pure: [Registry.DetectPlatform] reads node provider IDs and labels, and
// [Registry.DetectIntegrations] reads the group versions served by the discovery API. Neither reads the cluster
// itself; the caller collects the input. Detection data is declarative ([PlatformDetection],
// [IntegrationDetection]), so a compiled module can later be expressed as a signed declarative pack.
//
// This package imports only the standard library and this module; product SDKs stay in adapter packages
// (ADR-0013).
package modules
