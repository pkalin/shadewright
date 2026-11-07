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

// Package kyvernocluster makes the ClusterRole analyzer-kyverno-cluster: the reads of the Kyverno module's
// cluster-scoped symptom sources (ClusterPolicies, ClusterPolicyReports), bound by a ClusterRoleBinding (chart
// value analyzer.modules.kyverno.clusterWide.enabled, off by default). It is the analyzer's one cluster-wide
// read of an add-on's kinds besides the settings and the Node list: a ClusterPolicyReport names resources in
// every namespace, watched or not.
// Only list and watch, for the informers of the sources: nothing reads these kinds by name.
//
// +kubebuilder:rbac:groups=kyverno.io,resources=clusterpolicies,verbs=list;watch
// +kubebuilder:rbac:groups=wgpolicyk8s.io,resources=clusterpolicyreports,verbs=list;watch
package kyvernocluster
