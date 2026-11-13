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

// MaxReportBaselines bounds status.baselines of a VerificationReport: one entry per pod of the rollout, so a
// target with more desired replicas cannot be verified (ADR-0030 section 4).
const MaxReportBaselines = 200

// ResetReason says why a verification window was reset (ADR-0030 section 4). The verifier sets Restarted,
// PodNotReady, PartialRollout, EvidenceMissing, ScaledToZero and ObservationGap in its report, the only reasons
// the enum of WindowReset.reason accepts. NotAvailable, StaleReport and ReportMismatch are the executor's
// reasons for its own window; with PartialRollout they are the reasons of status.verification.lastReset of a
// RemediationRequest (VerificationReset), never of a report.
type ResetReason string

// The reset reasons.
const (
	// ResetRestarted: a container of a pod of the rollout (a native sidecar included) restarted above its
	// baseline, or terminated after the window started. Trouble class; recorded in troubleResets only for a
	// restart count above its baseline after the window first opened.
	ResetRestarted ResetReason = "Restarted"
	// ResetPodNotReady: a pod of the rollout is not Ready and did not restart. No evidence.
	ResetPodNotReady ResetReason = "PodNotReady"
	// ResetPartialRollout: fewer pods on the applied revision than the desired replicas, or a pod of the target
	// on another revision; for the executor, a report that does not cover every desired replica. No evidence.
	ResetPartialRollout ResetReason = "PartialRollout"
	// ResetEvidenceMissing: a pod without a status for every container, more pods than the bound, or a target
	// that is not the one the report names. No evidence.
	ResetEvidenceMissing ResetReason = "EvidenceMissing"
	// ResetScaledToZero: the target's spec.replicas is 0. No evidence.
	ResetScaledToZero ResetReason = "ScaledToZero"
	// ResetObservationGap: no sample for more than two poll intervals, and a pod not in the baselines after it.
	// No evidence.
	ResetObservationGap ResetReason = "ObservationGap"
	// ResetNotAvailable: the target reports fewer available replicas than desired (executor). No evidence.
	ResetNotAvailable ResetReason = "NotAvailable"
	// ResetStaleReport: the report is missing or its sample counter stopped rising (executor). No evidence.
	ResetStaleReport ResetReason = "StaleReport"
	// ResetReportMismatch: the report's spec names another request, merge commit, target or template revision
	// (executor). No evidence.
	ResetReportMismatch ResetReason = "ReportMismatch"
)

// WindowReset is a reset of a verification window. Its class (Restarted is trouble, every other reason no
// evidence) follows from reason and is not stored.
type WindowReset struct {
	// reason says why the window was reset: one of the reasons the verifier sets.
	// +kubebuilder:validation:Enum=Restarted;PodNotReady;PartialRollout;EvidenceMissing;ScaledToZero;ObservationGap
	// +required
	Reason ResetReason `json:"reason"`

	// time is when the reset was recorded, on the clock of the writer of the window. It is never earlier than
	// the window's start at that moment or the previous reset, so a window is open only while windowStart is
	// later than it.
	// +required
	Time metav1.Time `json:"time"`
}

// OOMKill records an OOM kill of the action's container on a pod of the rollout (ADR-0030 section 4).
type OOMKill struct {
	// podUID is the UID of the pod.
	// +required
	PodUID UID `json:"podUID"`

	// container is the name of the container, the action's container.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +required
	Container string `json:"container"`

	// finishedAt is the finish time of the termination, written by the node's clock. It is shown, never
	// compared: no time decides the OOM rule.
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
}

// PodBaseline is the restart baseline of one pod of the rollout.
type PodBaseline struct {
	// podUID is the UID of the pod.
	// +required
	PodUID UID `json:"podUID"`

	// restarts is the sum of the restart counts of the pod's containers and native sidecars (init containers
	// with restartPolicy Always) when the baseline was last taken. A later sample that shows a higher sum is a
	// Restarted reset.
	// +kubebuilder:validation:Minimum=0
	// +required
	Restarts int64 `json:"restarts"`
}

// VerificationReportSpec binds a report to one request, its merge, its target and the template revision it
// judges. It is set when the verifier creates the report and never changes. It holds no time of the executor's
// clock.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type VerificationReportSpec struct {
	// requestUID is the UID of the RemediationRequest the report is about.
	// +required
	RequestUID UID `json:"requestUID"`

	// mergeCommitSHA is the merge commit of the request's merge receipt.
	// +kubebuilder:validation:MinLength=40
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	// +required
	MergeCommitSHA string `json:"mergeCommitSHA"`

	// targetUID is the UID of the target workload.
	// +required
	TargetUID UID `json:"targetUID"`

	// templateRevision is the template revision of the rollout, from the request's status.rollout: a
	// Deployment's revision annotation or a StatefulSet's update revision.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	TemplateRevision string `json:"templateRevision"`

	// container is the name of the action's container, for an action that names one
	// (SetContainerMemoryLimit). An OOM kill of this container ends the request Failed.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	Container string `json:"container,omitempty"`
}

// VerificationReportStatus is the verifier's pod-level window: its samples, its resets, its restart baselines
// and an OOM kill once seen. Typed fields only: nothing is copied from the cluster as text.
type VerificationReportStatus struct {
	// sample counts the samples written: the verifier raises it by one with every sample. It never decreases.
	// The executor judges the age of the report by this counter on its own clock, never by sampledAt.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Sample int64 `json:"sample,omitempty"`

	// sampledAt is when the last sample was taken, on the verifier's clock.
	// +optional
	SampledAt *metav1.Time `json:"sampledAt,omitempty"`

	// windowStart is the time of the first good sample of the current window, on the verifier's clock. It
	// only moves forward. The window is open while windowStart is later than lastReset.time; its span is
	// sampledAt - windowStart.
	// +optional
	WindowStart *metav1.Time `json:"windowStart,omitempty"`

	// desiredReplicas is the target's spec.replicas at the last sample.
	// +kubebuilder:validation:Minimum=0
	// +optional
	DesiredReplicas int32 `json:"desiredReplicas,omitempty"`

	// podsCovered is the number of pods of the rollout at the last sample: pods whose ownership by the target
	// was proven and that run the report's template revision.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=200
	// +optional
	PodsCovered int32 `json:"podsCovered,omitempty"`

	// resets counts the resets of the window. It never decreases.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Resets int64 `json:"resets,omitempty"`

	// lastReset is the last reset of the window. Once set it is kept, and its time never moves back.
	// +optional
	LastReset *WindowReset `json:"lastReset,omitempty"`

	// troubleResets counts the Restarted resets recorded as trouble over the life of the report: those with a
	// restart count above its baseline after the window first opened (the first good sample). A Restarted reset
	// before that, or one shown only by a termination time (a node's clock), is not counted. It never
	// decreases: a later no-evidence reset does not hide that a restart was observed.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TroubleResets int64 `json:"troubleResets,omitempty"`

	// lastTroubleReset is the last Restarted reset recorded as trouble. Once set it is kept, and its time never moves
	// back. A report that records trouble ends its request Failed at the timeout, whatever its last reset.
	// +kubebuilder:validation:XValidation:rule="self.reason == 'Restarted'",message="lastTroubleReset.reason must be Restarted"
	// +optional
	LastTroubleReset *WindowReset `json:"lastTroubleReset,omitempty"`

	// oomKill is the first OOM kill of the action's container on a pod of the rollout. Once set it never
	// changes.
	// +optional
	OOMKill *OOMKill `json:"oomKill,omitempty"`

	// baselines are the restart baselines of the pods, one per pod UID, at most 200. A baseline whose pod is
	// gone from the pod list is pruned in the same write.
	// +listType=map
	// +listMapKey=podUID
	// +kubebuilder:validation:MaxItems=200
	// +optional
	Baselines []PodBaseline `json:"baselines,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vr,categories=shadewright
// +kubebuilder:printcolumn:name="Sample",type=integer,JSONPath=`.status.sample`
// +kubebuilder:printcolumn:name="Covered",type=integer,JSONPath=`.status.podsCovered`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredReplicas`
// +kubebuilder:printcolumn:name="Resets",type=integer,JSONPath=`.status.resets`
// +kubebuilder:printcolumn:name="Last reset",type=string,JSONPath=`.status.lastReset.reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// The forward-only and set-once rules of status live on the object, not on status, so that removing status
// and writing it again cannot move a recorded value back.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.sample) || (has(self.status) && has(self.status.sample) && self.status.sample >= oldSelf.status.sample)",message="status.sample never decreases"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.windowStart) || (has(self.status) && has(self.status.windowStart) && timestamp(self.status.windowStart) >= timestamp(oldSelf.status.windowStart))",message="status.windowStart only moves forward"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.resets) || (has(self.status) && has(self.status.resets) && self.status.resets >= oldSelf.status.resets)",message="status.resets never decreases"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.lastReset) || (has(self.status) && has(self.status.lastReset) && timestamp(self.status.lastReset.time) >= timestamp(oldSelf.status.lastReset.time))",message="status.lastReset is kept once set and its time never moves back"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.troubleResets) || (has(self.status) && has(self.status.troubleResets) && self.status.troubleResets >= oldSelf.status.troubleResets)",message="status.troubleResets never decreases"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.lastTroubleReset) || (has(self.status) && has(self.status.lastTroubleReset) && timestamp(self.status.lastTroubleReset.time) >= timestamp(oldSelf.status.lastTroubleReset.time))",message="status.lastTroubleReset is kept once set and its time never moves back"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.oomKill) || (has(self.status) && has(self.status.oomKill) && self.status.oomKill == oldSelf.status.oomKill)",message="status.oomKill is set once and never changed"

// VerificationReport is the verifier's evidence for one RemediationRequest in Verifying: whether the pods of
// the rollout stayed up without a restart or an OOM kill over a window (ADR-0030 section 7). Only the verifier
// writes it; the executor reads it as untrusted evidence, bound by spec to the request, and writes the
// request's Cleared condition from it. Its name derives from the request UID.
type VerificationReport struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec binds the report to the request.
	// +required
	Spec VerificationReportSpec `json:"spec"`

	// status is the verifier's window.
	// +optional
	Status VerificationReportStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// VerificationReportList contains a list of VerificationReport.
type VerificationReportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []VerificationReport `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &VerificationReport{}, &VerificationReportList{})
		return nil
	})
}
