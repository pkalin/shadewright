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

	// pullCauseFingerprint identifies the pull cause for a kubelet Failed event on a verified container
	// currently in an image-pull wait. Only the exact notes "Error: ErrImagePull", "Error: ImagePullBackOff"
	// and "Error: InvalidImageName" qualify. The cause key uses the image from the pod spec byte for byte.
	// The analyzer folds this Incident only into a cause with this fingerprint. Detailed "Failed to pull image"
	// events have no link and stay separate.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// +kubebuilder:validation:MaxLength=71
	// +optional
	PullCauseFingerprint string `json:"pullCauseFingerprint,omitempty"`

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
// +kubebuilder:validation:XValidation:rule="!has(self.started) || self.id.startsWith('termination:')",message="started is allowed only on a termination record"
// +kubebuilder:validation:XValidation:rule="!has(self.started) || (has(self.began) && timestamp(self.started) <= timestamp(self.began))",message="started requires began and cannot be after it"
type OccurrenceRecord struct {
	// id names the occurrence: "event:" and the Event UID, "termination:" and a hash of the pod UID, the
	// container and the termination, "waiting:" and a hash of the pod UID, the container, the waiting reason
	// and the container's restart count, or "condition:" and a hash of the object UID, the kind, the symptom
	// reason and when the condition began, for a symptom read from an object's status by a module's symptom
	// source.
	// +kubebuilder:validation:Pattern=`^(event|termination|waiting|condition):[A-Za-z0-9-]{1,64}$`
	// +kubebuilder:validation:MaxLength=76
	// +required
	ID string `json:"id"`

	// total is the highest total of the occurrence counted: the event's series count, or 1 for a
	// termination, a wait or a condition.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=2147483647
	// +required
	Total int64 `json:"total"`

	// began is when the occurrence began: the event's creation time, the termination's finish time, the
	// condition's last transition time, or for a wait, which has no time from the cluster, when the analyzer
	// first saw it, by its own clock. Absent when none is known.
	// +optional
	Began *metav1.Time `json:"began,omitempty"`

	// started is when a terminated container instance started, from the same terminated state of the pod
	// status that began (its finish time) comes from (ADR-0023, amendments of 2026-11-21 and 2026-11-26).
	// Set only on a termination record in status.occurrences, never after began; absent for other occurrences
	// (including status.absorbedOccurrences, which never carry started), for records written by an older
	// version, or when the pod status lacked a usable start time.
	// +optional
	Started *metav1.Time `json:"started,omitempty"`
}

// MaxIncidentContainers is the most container names an Incident's status.containers keeps (ADR-0039 point 8).
const MaxIncidentContainers = 8

// MaxAbsorbedReasons is the most entries an Incident's status.absorbed keeps, one per consequence reason
// (ADR-0039 point 16). Three reasons are used now.
const MaxAbsorbedReasons = 8

// AbsorbedReason is a consequence reason whose observations count on the Incident of their cause instead of
// opening one of their own (ADR-0039 point 16). The values come from the fixed table of the rule, never from
// the cluster, so the tally carries no text an Event or a pod writes.
// +kubebuilder:validation:Enum=BackOff;Unhealthy;Failed
type AbsorbedReason string

// The consequence reasons of ADR-0039, one per value of the AbsorbedReason enum.
const (
	// AbsorbedBackOff is a kubelet BackOff of a container whose pod status shows a cause.
	AbsorbedBackOff AbsorbedReason = "BackOff"
	// AbsorbedUnhealthy is a kubelet probe failure of a container whose pod status shows a cause.
	AbsorbedUnhealthy AbsorbedReason = "Unhealthy"
	// AbsorbedFailed counts kubelet Failed events with a short pull error on a verified container in a current
	// image-pull wait. See IncidentSpec.PullCauseFingerprint for the exact notes.
	AbsorbedFailed AbsorbedReason = "Failed"
)

// AbsorbedTally counts the observations of one consequence reason that a cause's Incident took as evidence
// (ADR-0039 point 16). It holds no event text.
type AbsorbedTally struct {
	// reason is the consequence reason counted.
	// +required
	Reason AbsorbedReason `json:"reason"`

	// count is the number of occurrences of the reason counted on this Incident, counting event series
	// increments like status.count. It saturates at 1,000,000.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000000
	// +required
	Count int64 `json:"count"`

	// lastSeen is when the latest observation of the reason arrived.
	// +required
	LastSeen metav1.Time `json:"lastSeen"`
}

// IncidentStatus holds the deduplication state that must survive restarts (ADR-0007). No logs or transcripts.
// +kubebuilder:validation:XValidation:rule="!(has(oldSelf.phase) && oldSelf.phase == 'Resolved' && has(oldSelf.resolvedTime) && has(self.phase) && self.phase == 'Resolved') || (has(self.resolvedTime) && self.resolvedTime == oldSelf.resolvedTime)",message="resolvedTime cannot change while the phase stays Resolved"
// +kubebuilder:validation:XValidation:rule="!(has(oldSelf.cause) && oldSelf.cause) || (has(self.cause) && self.cause)",message="cause cannot change from true to false"
// +kubebuilder:validation:XValidation:rule="!(has(self.cause) && self.cause && has(self.consequence) && self.consequence)",message="cause and consequence cannot both be true"
// +kubebuilder:validation:XValidation:rule="!has(self.absorbed) || size(self.absorbed) == 0 || (has(self.cause) && self.cause)",message="absorbed is allowed only when cause is true"
// +kubebuilder:validation:XValidation:rule="!has(self.absorbedOccurrences) || size(self.absorbedOccurrences) == 0 || (has(self.cause) && self.cause)",message="absorbedOccurrences is allowed only when cause is true"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.occurrencesDroppedThrough) || (has(self.occurrencesDroppedThrough) && self.occurrencesDroppedThrough >= oldSelf.occurrencesDroppedThrough)",message="occurrencesDroppedThrough only moves forward"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.absorbedOccurrencesDroppedThrough) || (has(self.absorbedOccurrencesDroppedThrough) && self.absorbedOccurrencesDroppedThrough >= oldSelf.absorbedOccurrencesDroppedThrough)",message="absorbedOccurrencesDroppedThrough only moves forward"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.revertsProposed) || (has(self.revertsProposed) && oldSelf.revertsProposed.all(o, self.revertsProposed.exists(n, n.uid == o.uid && n.name == o.name && (o.state == 'Pending' || (n.state == o.state && (!has(o.reason) || (has(n.reason) && n.reason == o.reason)))))))",message="revertsProposed only grows: an entry is never removed, its state only moves from Pending to Created or Abandoned, and its reason never changes"
// +kubebuilder:validation:XValidation:rule="!has(self.foldedInto) || (has(self.phase) && self.phase == 'Resolved')",message="foldedInto is allowed only in phase Resolved"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.foldedInto) || !has(self.foldedInto) || self.foldedInto == oldSelf.foldedInto",message="foldedInto cannot change to another Incident: once set it may only be cleared"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.foldedInto) || has(self.foldedInto) || (has(self.phase) && self.phase != 'Resolved')",message="foldedInto is cleared only together with a move to a phase other than Resolved"
// +kubebuilder:validation:XValidation:rule="!(has(oldSelf.folded) && oldSelf.folded) || (has(self.folded) && self.folded)",message="folded cannot change from true to false"
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
	// the highest total the cluster reported that was counted, when it began, and for a termination, when
	// that container instance started. After a restart the
	// analyzer reads them to count only what is new. Older ones are dropped.
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Occurrences []OccurrenceRecord `json:"occurrences,omitempty"`

	// occurrencesDroppedThrough is when the newest occurrence dropped from occurrences began. An occurrence
	// with no record that began no later than this is treated as counted in full, so the bound never counts
	// one twice. It only moves forward.
	// +optional
	OccurrencesDroppedThrough *metav1.Time `json:"occurrencesDroppedThrough,omitempty"`

	// containers are the names of the containers whose observations this Incident counted, at most 8 (ADR-0039
	// point 8), written in the status write that counts an observation naming one. A name that is not a DNS
	// label, or one past the eighth, is left out and logged. The late-cause fold compares them between a
	// consequence Incident and its cause's Incident.
	// +listType=set
	// +kubebuilder:validation:MaxItems=8
	// +optional
	Containers []Name `json:"containers,omitempty"`

	// cause is true when the Incident was opened or counted by a pod-status observation of class cause, which the
	// kubelet wrote: an OOM kill, a non-zero exit (not of a pod being deleted, nor of one whose status.reason is
	// set, such as an evicted pod) or a waiting reason (ADR-0039 points 3 and 8, point 3 amended 2026-11-20 and
	// narrowed 2026-11-21). Only such an Incident takes the evidence of a consequence or folds a consequence
	// Incident: a non-zero exit's takes BackOff only, an OOM kill's or a configuration wait's BackOff and Unhealthy,
	// and an image-pull wait's (ImagePullBackOff, ErrImagePull, InvalidImageName) BackOff, Unhealthy and
	// qualifying Failed events (point 12). An Event never sets it. Once true it stays true,
	// and the status cannot be removed while it is.
	// +optional
	Cause bool `json:"cause,omitempty"`

	// consequence records that the analyzer classified all observations counted here as consequences.
	// Older status writers may omit it. The late fold matches spec.pullCauseFingerprint.
	// +optional
	Consequence bool `json:"consequence,omitempty"`

	// absorbed is the tally of the consequence observations this Incident took as evidence of its cause, one
	// entry per reason, at most 8 (ADR-0039 point 16). They change neither count nor lastSeen, and the analysis
	// trigger and the quiet rule do not read them. Allowed only when cause is true.
	// +listType=map
	// +listMapKey=reason
	// +kubebuilder:validation:MaxItems=8
	// +optional
	Absorbed []AbsorbedTally `json:"absorbed,omitempty"`

	// absorbedOccurrences are the newest occurrences counted in absorbed, newest first, at most 32, kept apart
	// from occurrences so that an absorbed observation never makes the Incident due for analysis (ADR-0039 point
	// 16). After a restart they keep an absorbed occurrence from being counted twice. Older ones are dropped.
	// Absorbed occurrences are consequence events and never carry started. Allowed only when cause is true.
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=32
	// +optional
	AbsorbedOccurrences []OccurrenceRecord `json:"absorbedOccurrences,omitempty"`

	// absorbedOccurrencesDroppedThrough is when the newest occurrence dropped from absorbedOccurrences began,
	// the drop mark of occurrencesDroppedThrough for the absorbed list. It answers only for consequences linked
	// to this Incident's fingerprint, and only moves forward.
	// +optional
	AbsorbedOccurrencesDroppedThrough *metav1.Time `json:"absorbedOccurrencesDroppedThrough,omitempty"`

	// foldedInto names the cause's Incident, by name and UID, when this consequence Incident was folded into it
	// and closed Resolved with the Lifecycle reason FoldedIntoCause (ADR-0039 point 14). Allowed only in phase
	// Resolved. It never changes to another Incident: a reopen of the folded Incident clears it, together with
	// a move out of Resolved, and that is the only change allowed once it is set. It never names the Incident
	// itself. The closure rules skip an Incident that has it. The fold sets folded with it.
	// +optional
	FoldedInto *IncidentRef `json:"foldedInto,omitempty"`

	// folded is true once this consequence Incident was folded into its cause's Incident (ADR-0039 point 14,
	// decision of 2026-11-16 on #793), set in the fold's status write. Unlike foldedInto and the Lifecycle reason,
	// which a reopen, an escalation or a request outcome replaces, it stays: an Incident with folded true is never
	// folded again, so a fold happens at most once per Incident. Once true it stays true, and the status cannot
	// be removed while it is.
	// +optional
	Folded bool `json:"folded,omitempty"`

	// resolvedTime is when the Incident moved to phase Resolved, written in the same status update (design
	// 3.2). It cannot change while the phase stays Resolved. A symptom seen again within 24 hours of it, or with it missing, moves the Incident to Escalated
	// without an analysis; seen again later, it opens the next occurrence of the fingerprint as a new Incident.
	// +optional
	ResolvedTime *metav1.Time `json:"resolvedTime,omitempty"`

	// lastAnalysisTime is when the analyzer last ran; it drives the cooldown.
	// +optional
	LastAnalysisTime *metav1.Time `json:"lastAnalysisTime,omitempty"`

	// handledHint is the reviewer hint the analysis claimed by lastAnalysisTime answers (ADR-0028 section 3):
	// written in the status update that takes the claim and kept with the outcome in the Analyzed condition, so
	// a restarted analyzer does not run the same hint twice and the executor can match a refusal to the hint.
	// Absent for an analysis without a hint. The trigger also writes it, with the Analyzed reason CoolingDown or
	// BudgetSpent, for a hint it refuses without an analysis.
	// +optional
	HandledHint *HintRef `json:"handledHint,omitempty"`

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

	// revertsProposed are the failed requests of this incident, by name and UID, that the analyzer's revert
	// proposer decided to revert (ADR-0030 section 8, #96), oldest first. The proposer writes an entry in state
	// Pending before it creates the revert and moves it to Created once the revert exists; it never takes a new
	// decision for a request listed here, so a failed request gets one revert for good, also after that revert
	// was deleted. When a check before a create says no (the target is gone, the request passed the age bound,
	// the request was deleted, the revert's name is taken by another request, the create is refused as invalid),
	// the entry moves to Abandoned instead, with a reason, which is final: no revert is made for it. A new entry
	// is always Pending, entries are never removed, a state moves only from Pending to Created or Abandoned, a
	// reason never changes, and the status cannot be removed while the list has entries; a full list stops new
	// reverts for this incident instead (condition RevertsStopped).
	// +listType=map
	// +listMapKey=uid
	// +kubebuilder:validation:MaxItems=20
	// +optional
	RevertsProposed []ProposedRevert `json:"revertsProposed,omitempty"`

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

// Incident groups repeated symptoms of one problem on one workload. The rules on the root keep a write from
// removing the whole status, which the rules on the status object cannot see, once status.revertsProposed has
// entries, status.cause or status.folded is true or status.foldedInto is set, refuse a new entry of that list in any state but
// Pending, also in the first status write, and refuse a status.foldedInto that names the Incident itself.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.revertsProposed) || size(oldSelf.status.revertsProposed) == 0 || has(self.status)",message="status cannot be removed while status.revertsProposed has entries"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.cause) || !oldSelf.status.cause || has(self.status)",message="status cannot be removed while status.cause is true"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.foldedInto) || has(self.status)",message="status cannot be removed while status.foldedInto is set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.folded) || !oldSelf.status.folded || has(self.status)",message="status cannot be removed while status.folded is true"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.foldedInto) || self.status.foldedInto.name != self.metadata.name",message="status.foldedInto cannot name the Incident itself"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.revertsProposed) || self.status.revertsProposed.all(n, n.state == 'Pending' || (has(oldSelf.status) && has(oldSelf.status.revertsProposed) && oldSelf.status.revertsProposed.exists(o, o.uid == n.uid)))",message="a new entry of status.revertsProposed must be Pending"
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
