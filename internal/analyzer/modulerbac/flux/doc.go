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

// Package flux makes the ClusterRole analyzer-watch-flux: the reads of the Flux module's symptom sources
// (Kustomizations, HelmReleases, GitRepositories) in the watched namespaces, bound there by one RoleBinding each
// (chart value analyzer.modules.flux.enabled, off by default).
// Only list and watch, for the informers of the sources, never get. The read tool get_delivery_status reads one
// Kustomization or HelmRelease, and a Kustomization's GitRepository, with list too, by a field selector on the
// name (analyzer.ListGetter, #562), by the collector's rule for this grant (collector.Granted: --module-grants
// names flux, or is not given).
//
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=list;watch
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=list;watch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=gitrepositories,verbs=list;watch
package flux
