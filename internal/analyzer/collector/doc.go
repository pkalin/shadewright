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

// Package collector watches events and pods in the namespaces the analyzer is started with
// (--watch-namespaces) and turns them into observations for the Incident controller (design 3.1, 3.2).
//
// What it watches: events.k8s.io/v1 Events of type Warning, and Pods with an abnormal container
// termination (OOMKilled or a non-zero exit code). OOMKilled comes from container status, never from an
// event reason, so the pod watch is how it is seen. There is no core/v1 Event watch: those are the same
// objects in an older version, and watching both would count each event twice. Both watches use the
// manager's cache, which the analyzer restricts to the watched namespaces.
//
// Observation-level deduplication: per event the collector remembers the UID and the last handled
// occurrence count (the series count, or the count of an older recorder, or one), and emits only the
// increase. Per pod it remembers the container IDs of handled terminations. Both are LRU maps with a fixed
// capacity. Anything older than the first reconcile (a restart or a new leader), or as old as state that was
// dropped to stay within the capacity, is marked MayRepeat, and a LastSeenFunc from the Incident controller
// suppresses what the Incident already counted, so restarts are absorbed by the Incident counters, not by
// memory alone.
//
// Owner correlation follows controller owner references from Pod to ReplicaSet to Deployment and from Pod
// to StatefulSet. Owner references are untrusted: each hop is a metadata read through the API reader, with a
// deadline, and must have the referenced UID, or the chain stops at the last verified object. Kinds the
// analyzer cannot read (DaemonSets, Jobs) are not followed. Results are kept in a bounded cache with a TTL.
//
// Untrusted input: notes go through package fingerprint, which redacts before it normalizes, and only the
// fingerprint and the normalized symptom leave the collector; termination messages are never read.
// Reasons, references and timestamps are checked against the Incident schema and the collector's clock.
// Observations go to a bounded channel; when it stays full, the reconcile retries later without
// committing its state, so nothing is lost or counted twice within one process.
//
// Its markers generate the ClusterRole analyzer-watch-role (config/rbac/analyzer/watch). That role is only a
// catalogue of verbs: it is bound by one RoleBinding per watched namespace (config/watch), never by a
// ClusterRoleBinding, so the analyzer can read nothing outside the namespaces it was given. Only namespaced
// reads belong here; anything cluster-scoped or written goes in package analyzer.
//
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=replicasets;deployments;statefulsets,verbs=get;list;watch
package collector
