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

// Package verifier is the third identity, shadewright-verifier (ADR-0030 section 7): deterministic code with no
// model access and no Git App credentials. For every RemediationRequest in Verifying it samples the pods of the
// rollout every 30 seconds and writes one VerificationReport, which the executor reads as untrusted evidence.
// The sample itself (ownership, baselines, resets, the window) is the pure logic of
// internal/verification/clearing; this package reads the cluster, feeds it, and writes what it returns.
//
// It never writes request status and never sets a phase: its only writes are the create of a report and the
// update of a report's status, in the operator namespace.
//
// The verifier's grants in the operator namespace. Requests are read through the manager's cache, which is
// restricted to the operator namespace; reports are read one at a time through the API reader, so get is
// enough. Its leader election Lease and core Events are granted by config/rbac/verifier/leader_election.yaml.
// The reads in the watched namespaces are in package watchrbac.
//
// +kubebuilder:rbac:groups=remediation.shadewright.io,namespace=system,resources=remediationrequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=remediation.shadewright.io,namespace=system,resources=verificationreports,verbs=get;create
// +kubebuilder:rbac:groups=remediation.shadewright.io,namespace=system,resources=verificationreports/status,verbs=update
package verifier
