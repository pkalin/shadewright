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

// Package policyrbac holds the kubebuilder RBAC marker of the analyzer's read of the RemediationPolicies it
// names in its requests (#493): get only, no list or watch, so the context builder can show the model the
// bounds of the policy that will decide a proposal. It makes the ClusterRole analyzer-policy-reader (make
// manifests, config/rbac/analyzer/policies), which grants get on every policy; it is never installed as is.
// The chart renders the role with resourceNames set to the policy names of analyzer.analysis.actionPolicies,
// and only with a triage model and at least one such name; config/analysis-policies restricts it the same way
// with a patch. The package holds no code.
//
// +kubebuilder:rbac:groups=remediation.shadewright.io,resources=remediationpolicies,verbs=get
package policyrbac
