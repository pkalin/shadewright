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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// IncidentSpec identifies a group of similar symptoms (design section 3.2). It is immutable: a different
// fingerprint is a different incident.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable; a different symptom is a new Incident"
type IncidentSpec struct {
	// fingerprint is the hash of cluster, namespace, owner, reason, action, type, reporting controller and
	// normalized symptom.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// +required
	Fingerprint string `json:"fingerprint"`

	// subject is the object the symptoms are correlated to: the owning workload when there is one.
	// +required
	Subject ObjectRef `json:"subject"`

	// reason is the event reason or status reason, for example FailedScheduling or OOMKilled.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +required
	Reason string `json:"reason"`

	// type is the event type.
	// +kubebuilder:validation:Enum=Normal;Warning
	// +kubebuilder:default=Warning
	// +optional
	Type string `json:"type,omitempty"`

	// reportingController is the component that reported the symptom.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	ReportingController string `json:"reportingController,omitempty"`

	// symptom is the normalized, redacted description: variable parts removed, secrets never included.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Symptom string `json:"symptom,omitempty"`
}

// IncidentPhase summarizes an incident.
// +kubebuilder:validation:Enum=Open;Analyzing;Proposed;Escalated;Resolved
type IncidentPhase string

// Phases of an Incident.
const (
	// IncidentOpen means symptoms are being collected and no analysis is running.
	IncidentOpen IncidentPhase = "Open"
	// IncidentAnalyzing means the analyzer is working on the incident.
	IncidentAnalyzing IncidentPhase = "Analyzing"
	// IncidentProposed means the analysis produced a RemediationRequest.
	IncidentProposed IncidentPhase = "Proposed"
	// IncidentEscalated means the analysis made no proposal and handed the incident to a human.
	IncidentEscalated IncidentPhase = "Escalated"
	// IncidentResolved means the symptoms stopped or a remediation succeeded.
	IncidentResolved IncidentPhase = "Resolved"
)

// RecordID identifies an analysis record in the analysis store.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=256
type RecordID string

// IncidentStatus holds the deduplication state that must survive restarts (ADR-0007). No logs or transcripts.
type IncidentStatus struct {
	// phase summarizes the incident.
	// +optional
	Phase IncidentPhase `json:"phase,omitempty"`

	// firstSeen is when the first observation arrived.
	// +optional
	FirstSeen *metav1.Time `json:"firstSeen,omitempty"`

	// lastSeen is when the latest observation arrived.
	// +optional
	LastSeen *metav1.Time `json:"lastSeen,omitempty"`

	// count is the number of observations, counting event series increments, not totals.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Count int64 `json:"count,omitempty"`

	// lastAnalysisTime is when the analyzer last ran; it drives the cooldown.
	// +optional
	LastAnalysisTime *metav1.Time `json:"lastAnalysisTime,omitempty"`

	// analysisRecords are the newest analysis record IDs, oldest first.
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=20
	// +optional
	AnalysisRecords []RecordID `json:"analysisRecords,omitempty"`

	// remediationRequests are the names of requests made for this incident, oldest first.
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=20
	// +optional
	RemediationRequests []Name `json:"remediationRequests,omitempty"`

	// conditions explain the phase.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=shadewright
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.subject.kind`
// +kubebuilder:printcolumn:name="Subject",type=string,JSONPath=`.spec.subject.name`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.spec.reason`
// +kubebuilder:printcolumn:name="Count",type=integer,JSONPath=`.status.count`
// +kubebuilder:printcolumn:name="Last seen",type=date,JSONPath=`.status.lastSeen`

// Incident groups repeated symptoms of one problem on one workload.
type Incident struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec identifies the incident.
	// +required
	Spec IncidentSpec `json:"spec"`

	// status is the deduplication state.
	// +optional
	Status IncidentStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// IncidentList contains a list of Incident
type IncidentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Incident `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Incident{}, &IncidentList{})
		return nil
	})
}
