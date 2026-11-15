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

// Package fluxrules holds the rules of the source mapping (ADR-0012) that read only Flux objects already in
// hand: the Kustomization and GitRepository refusals, the revision comparison, the Flux labels, and the
// outcome and reason types of a mapping result. It reads nothing from the cluster or from Git and imports no
// Kubernetes client, so kubectl-shadewright can apply the same rules as the executor without linking
// controller-runtime. The package internal/gitops/sourcemap keeps every exported name here as an alias and
// holds the mapping itself.
package fluxrules
