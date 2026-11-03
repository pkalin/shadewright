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

package kyverno

import "github.com/pkalin/shadewright/internal/modules"

// Name is the module's name in RemediationSettings and its status.
const Name = "kyverno"

// API groups, versions and kinds the module reads (kyverno v1.19.2: api/kyverno/v1/doc.go and
// api/policyreport/v1alpha2/doc.go, the +groupName markers; api/kyverno/v1/clusterpolicy_types.go,
// policy_types.go, api/policyreport/v1alpha2/policyreport_types.go and clusterpolicyreport_types.go).
const (
	PolicyGroup   = "kyverno.io"
	PolicyVersion = "v1"
	ReportGroup   = "wgpolicyk8s.io"
	ReportVersion = "v1alpha2"

	KindClusterPolicy       = "ClusterPolicy"
	KindPolicy              = "Policy"
	KindPolicyReport        = "PolicyReport"
	KindClusterPolicyReport = "ClusterPolicyReport"
)

// Components the sources name as the reporting controller: the event sources Kyverno itself writes as
// reportingController (kyverno v1.19.2 pkg/event/source.go: AdmissionController and PolicyController). The
// admission controller writes the policies' Ready condition (pkg/controllers/webhook/controller.go) and emits
// the admission events; the reports controller writes the policy reports and emits kyverno-scan events for the
// same failing results (pkg/controllers/report/utils/events.go). Using the event names rather than the
// Deployment names of the Helm chart gives a report result the reporting controller of the event about it.
const (
	eventAdmission = "kyverno-admission"
	eventScan      = "kyverno-scan"
)

// module is the Kyverno integration. It has no state.
type module struct{}

// Module returns the Kyverno integration module, for the list of compiled-in modules.
func Module() modules.Integration { return module{} }

func (module) Name() string { return Name }

// Detection declares kyverno.io/v1 only. The policy report group wgpolicyk8s.io is shared with other engines
// (Trivy, Kubewarden, kube-bench and more), so serving it says nothing about Kyverno; the report kinds are read
// only once Kyverno is detected, and then only Kyverno's results in them.
func (module) Detection() modules.IntegrationDetection {
	return modules.IntegrationDetection{GroupVersions: []string{PolicyGroup + "/" + PolicyVersion}}
}

// Hooks contributes the symptom sources and the admission refusal rule, and nothing else: no tools and no
// actions. Kyverno guards security, so the module diagnoses only (design 2.5).
func (module) Hooks() modules.Hooks {
	return modules.Hooks{
		Symptoms: []modules.SymptomSource{
			{
				Group: PolicyGroup, Versions: []string{PolicyVersion}, Kind: KindClusterPolicy,
				Resource: "clusterpolicies", ReportingController: eventAdmission,
				Symptoms: clusterPolicySymptoms,
			},
			{
				Group: PolicyGroup, Versions: []string{PolicyVersion}, Kind: KindPolicy,
				Resource: "policies", ReportingController: eventAdmission,
				Symptoms: policySymptoms,
			},
			{
				Group: ReportGroup, Versions: []string{ReportVersion}, Kind: KindPolicyReport,
				Resource: "policyreports", ReportingController: eventScan,
				Symptoms: policyReportSymptoms,
			},
			{
				Group: ReportGroup, Versions: []string{ReportVersion}, Kind: KindClusterPolicyReport,
				Resource: "clusterpolicyreports", ReportingController: eventScan,
				Symptoms: clusterPolicyReportSymptoms,
			},
		},
		Rules: []modules.DiagnosisRule{{Name: RuleAdmissionRefused, Event: admissionRefused}},
	}
}
