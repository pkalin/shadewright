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

// Package analyzer is the read-mostly side of Shadewright: the collector, the Incident controller and the
// analysis. It may write Incidents, create RemediationRequests and update its analysis budget ConfigMap in
// the operator namespace, and report platform and integration detection in the status of the cluster-scoped
// RemediationSettings (package detection, ADR-0024), and nothing else. It can never write a request's status,
// so nothing it produces is ever an authorization.
//
// The analyzer's permissions (design 2.2, 10.1) come in three parts:
//
//   - In the operator namespace (a Role): Incidents (get, list, watch, create) and update on their status,
//     create on RemediationRequests, and get and update on one ConfigMap, named in resourceNames: the daily
//     analysis budget (package incidents, BudgetName and ReserveAnalysis). It may not create, list or delete
//     ConfigMaps, nor read any other. Incidents live there, next to the requests that point at them
//     (IncidentRef is same-namespace). The Warning event about the analyzer's own Pod when the Incident cap is
//     reached (package incidents) is a core Event, written with the grant the leader election Role already
//     holds there (config/rbac/analyzer/leader_election.yaml), so it needs no grant of its own.
//   - Cluster-wide (a ClusterRole with a ClusterRoleBinding): read the cluster-scoped RemediationSettings and
//     update its status subresource (no resourceNames: the object is cluster-scoped and the schema pins its
//     name to default; the status is a report that nothing reads back to decide), list Nodes for platform
//     detection (no get or watch, so no informer on them), and nothing namespaced.
//   - In each watched namespace: the reads in package collector, a ClusterRole bound by one RoleBinding per
//     namespace in --watch-namespaces, never cluster-wide.
//
// +kubebuilder:rbac:groups=remediation.shadewright.io,namespace=system,resources=incidents,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=remediation.shadewright.io,namespace=system,resources=incidents/status,verbs=update
// +kubebuilder:rbac:groups=remediation.shadewright.io,namespace=system,resources=remediationrequests,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=remediation.shadewright.io,resources=remediationsettings,verbs=get;list;watch
// +kubebuilder:rbac:groups=remediation.shadewright.io,resources=remediationsettings/status,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=list
// +kubebuilder:rbac:groups="",namespace=system,resources=configmaps,resourceNames=shadewright-analysis-budget,verbs=get;update
package analyzer
