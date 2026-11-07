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

// Package modulerbac holds, in one sub-package per role, the kubebuilder RBAC markers of the analyzer's reads of
// an add-on's own kinds, for the symptom sources of its integration module (design 2.2.2 and 2.5.1). Each
// sub-package makes one ClusterRole (make manifests, config/rbac/analyzer/modules/<name>), which the chart
// renders only while its value under analyzer.modules is on, so an add-on that is not deployed gets no grant.
// The packages hold no code. The roles grant list and watch, for the informers of the sources; get only where
// an object is read by name (the ScaledObject a KEDA HPA names as its owner). The chart tells the analyzer which
// roles it rendered (--module-grants). The collector watches a kind only once it can list it, so a role that is
// left out stops nothing: the analyzer reports the kind as not watched (collector.Collector.SourcesNotWatched),
// except a kind of the optional kyverno-cluster role, which it leaves out without a report.
package modulerbac
