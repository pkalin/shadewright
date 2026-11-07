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
// +kubebuilder:validation:XValidation:rule="has(self.occurrenceNumber) == has(self.previousIncident)",message="occurrenceNumber and previousIncident are set together"
// +kubebuilder:validation:XValidation:rule="!has(self.occurrenceNumber) || (self.occurrenceNumber == 2 ? self.previousIncident == 'incident-' + self.fingerprint.substring(7) : (self.previousIncident.startsWith('incident-' + self.fingerprint.substring(7) + '-') && self.previousIncident.size() > 74 && int(self.previousIncident.substring(74)) == self.occurrenceNumber - 1))",message="previousIncident must be the occurrence before this one of the same fingerprint"
type IncidentSpec struct {
	// fingerprint is the hash of cluster, namespace, owner, reason, action, type, reporting controller and
	// normalized symptom.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// +kubebuilder:validation:MaxLength=71
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

	// occurrenceNumber is which Incident of the fingerprint this is: absent for the first, n >= 2 for the
	// Incident the analyzer opened when the symptom was seen again more than 24 hours after the previous one was
	// resolved (design 3.2). The name carries it as the suffix "-<n>".
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=999999
	// +optional
	OccurrenceNumber int32 `json:"occurrenceNumber,omitempty"`

	// previousIncident is the name of the Incident of the same fingerprint before this one, occurrence n-1,
	// set together with occurrenceNumber. It is a link for people only: the analysis never reads it, so no
	// earlier outcome or policy decision reaches the model (invariant 1).
	// +kubebuilder:validation:MaxLength=80
	// +kubebuilder:validation:Pattern=`^incident-[0-9a-f]{64}(-([2-9]|[1-9][0-9]{1,5}))?$`
	// +optional
	PreviousIncident string `json:"previousIncident,omitempty"`
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

// RecordID identifies an analysis record in the analysis store: lower-case letters, digits and hyphens, as
// a blob name of one path segment with nothing to escape.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=200
// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]{0,199}$`
type RecordID string

// MaxAnalysisRecords is the most record IDs an Incident's status.analysisRecords keeps.
const MaxAnalysisRecords = 20

// MaxIncidentCount is the value at which an Incident's status.count saturates.
const MaxIncidentCount = 1000000

// MaxOccurrenceRecords is the most occurrences an Incident's status keeps.
const MaxOccurrenceRecords = 32

// OccurrenceRecord is what an Incident counted of one occurrence (design 3.2). It holds no event text.
type OccurrenceRecord struct {
	// id names the occurrence: "event:" and the Event UID, "termination:" and a hash of the pod UID, the
	// container and the termination, or "condition:" and a hash of the object UID, the kind, the symptom
	// reason and when the condition began, for a symptom read from an object's status by a module's symptom
	// source.
	// +kubebuilder:validation:Pattern=`^(event|termination|condition):[A-Za-z0-9-]{1,64}$`
	// +kubebuilder:validation:MaxLength=76
	// +required
	ID string `json:"id"`

	// total is the highest total of the occurrence counted: the event's series count, or 1 for a
	// termination or a condition.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=2147483647
	// +required
	Total int64 `json:"total"`

	// began is when the occurrence began: the event's creation time, the termination's finish time or the
	// condition's last transition time. Absent when the cluster did not report one.
	// +optional
	Began *metav1.Time `json:"began,omitempty"`
}

// IncidentStatus holds the deduplication state that must survive restarts (ADR-0007). No logs or transcripts.
// +kubebuilder:validation:XValidation:rule="!(has(oldSelf.phase) && oldSelf.phase == 'Resolved' && has(oldSelf.resolvedTime) && has(self.phase) && self.phase == 'Resolved') || (has(self.resolvedTime) && self.resolvedTime == oldSelf.resolvedTime)",message="resolvedTime cannot change while the phase stays Resolved"
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

	// count is the number of occurrences counted, counting event series increments, not totals. It
	// saturates at 1,000,000: a series count is written by whoever can create events, so forged updates can
	// raise it without bound otherwise. It is not a count of distinct occurrences.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000
	// +optional
	Count int64 `json:"count,omitempty"`

	// occurrences are the newest occurrences counted for this incident, newest first, at most 32: for each,
	// the highest total the cluster reported that was counted, and when it began. After a restart the
	// analyzer reads them to count only what is new. Older ones are dropped.
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Occurrences []OccurrenceRecord `json:"occurrences,omitempty"`

	// occurrencesDroppedThrough is when the newest occurrence dropped from occurrences began. An occurrence
	// with no record that began no later than this is treated as counted in full, so the bound never counts
	// one twice.
	// +optional
	OccurrencesDroppedThrough *metav1.Time `json:"occurrencesDroppedThrough,omitempty"`

	// resolvedTime is when the Incident moved to phase Resolved, written in the same status update (design
	// 3.2). It cannot change while the phase stays Resolved. A symptom seen again within 24 hours of it, or with it missing, moves the Incident to Escalated
	// without an analysis; seen again later, it opens the next occurrence of the fingerprint as a new Incident.
	// +optional
	ResolvedTime *metav1.Time `json:"resolvedTime,omitempty"`

	// lastAnalysisTime is when the analyzer last ran; it drives the cooldown.
	// +optional
	LastAnalysisTime *metav1.Time `json:"lastAnalysisTime,omitempty"`

	// analysisRecords are the newest analysis record IDs, oldest first (at most MaxAnalysisRecords).
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
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.subject.kind`,priority=1
// +kubebuilder:printcolumn:name="Subject",type=string,JSONPath=`.spec.subject.name`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.spec.reason`
// +kubebuilder:printcolumn:name="Due",type=string,JSONPath=`.status.conditions[?(@.type=="AnalysisDue")].reason`,description="Reason of the AnalysisDue condition: why the incident is or is not due for analysis"
// +kubebuilder:printcolumn:name="Outcome",type=string,JSONPath=`.status.conditions[?(@.type=="Analyzed")].reason`,priority=1,description="Reason of the Analyzed condition: the outcome of the last analysis"
// +kubebuilder:printcolumn:name="Count",type=integer,JSONPath=`.status.count`,priority=1
// +kubebuilder:printcolumn:name="Last analysis",type=date,JSONPath=`.status.lastAnalysisTime`
// +kubebuilder:printcolumn:name="Last seen",type=date,JSONPath=`.status.lastSeen`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

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
