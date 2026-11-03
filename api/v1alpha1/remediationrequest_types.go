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

// RemediationMode says how a plan is carried out. GitOps is the only mode (ADR-0003).
// +kubebuilder:validation:Enum=GitOps
type RemediationMode string

const (
	// RemediationModeGitOps changes the cluster only through a pull request to the GitOps repository.
	RemediationModeGitOps RemediationMode = "GitOps"
)

// IncidentRef points to the Incident a request answers, in the same namespace.
type IncidentRef struct {
	// name of the Incident.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +required
	Name string `json:"name"`

	// uid of the Incident.
	// +required
	UID UID `json:"uid"`
}

// ResolvedSource is where the controller found the target's declaration in Git (ADR-0012). It is derived from
// the Flux objects that deliver the target and from RemediationSettings, never taken from the request.
type ResolvedSource struct {
	// repositoryRef is the RemediationSettings entry whose URL matches the Flux source.
	// +required
	RepositoryRef Name `json:"repositoryRef"`

	// targetBranch is the branch the pull request targets: the branch Flux tracks, which must equal the
	// repository's targetBranch in RemediationSettings.
	// +required
	TargetBranch GitBranch `json:"targetBranch"`

	// path is the file that declares the target.
	// +required
	Path GitFilePath `json:"path"`

	// buildPath is the directory the Flux Kustomization builds (its spec.path, "." for the repository root).
	// Before every commit the pull request step renders it with and without the change and refuses anything
	// but the planned difference (design 8.2, steps 4 and 5). The policy check always sets it, and the plan
	// digest covers it; a request without it is never committed.
	// +optional
	BuildPath GitDirPath `json:"buildPath,omitempty"`

	// baseCommit is the full SHA of the target branch the change was made against. It is fixed when the branch
	// is created; later merges to the target branch do not change it.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	// +required
	BaseCommit string `json:"baseCommit"`
}

// VerificationCheck is a named, deterministic check run after delivery. DeliveryRevisionApplied: the GitOps
// controller has applied the revision that contains the merged change. WorkloadHealthy: the target workload
// is available and its rollout is complete. KedaReady: the KEDA ScaledObject reports Ready.
// QueueBacklogDecreasing: the backlog that drives the ScaledObject is going down. NoNewOOMKills: no container
// of the target is OOMKilled after the change.
// +kubebuilder:validation:Enum=DeliveryRevisionApplied;WorkloadHealthy;KedaReady;QueueBacklogDecreasing;NoNewOOMKills
type VerificationCheck string

// Verification says when a delivered change counts as a success. The controller always adds the checks the
// action type needs (DeliveryRevisionApplied, plus KedaReady or NoNewOOMKills), so a request can only make
// verification stricter.
// +kubebuilder:validation:XValidation:rule="!has(self.stableFor) || duration(self.stableFor) >= duration('1m')",message="stableFor must be at least 1m"
// +kubebuilder:validation:XValidation:rule="!has(self.timeout) || duration(self.timeout) <= duration('24h')",message="timeout must be at most 24h"
// +kubebuilder:validation:XValidation:rule="!has(self.timeout) || !has(self.stableFor) || duration(self.timeout) > duration(self.stableFor)",message="timeout must be longer than stableFor"
type Verification struct {
	// timeout is how long to wait for delivery and stability before giving up.
	// +kubebuilder:default="10m"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// stableFor is how long all checks must hold.
	// +kubebuilder:default="3m"
	// +optional
	StableFor *metav1.Duration `json:"stableFor,omitempty"`

	// checks to run in addition to the ones the action type always needs.
	// +listType=set
	// +kubebuilder:validation:MaxItems=8
	// +optional
	Checks []VerificationCheck `json:"checks,omitempty"`
}

// RemediationRequestSpec is one concrete plan. It is immutable: a different plan is a new request, so an
// approval can never carry over to a changed plan. It names what to change, not where: the repository, branch
// and file are resolved by the controller (ADR-0012), so the author cannot point a change at another file.
// Each action type fixes the kinds it may target.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable; a changed plan is a new RemediationRequest"
// +kubebuilder:validation:XValidation:rule="self.action.type != 'AdjustKedaMaxReplicas' || (self.targetRef.apiVersion.startsWith('keda.sh/') && self.targetRef.kind == 'ScaledObject')",message="AdjustKedaMaxReplicas can only target a keda.sh ScaledObject"
// +kubebuilder:validation:XValidation:rule="self.action.type != 'SetContainerMemoryLimit' || (self.targetRef.apiVersion == 'apps/v1' && self.targetRef.kind in ['Deployment', 'StatefulSet'])",message="SetContainerMemoryLimit can only target an apps/v1 Deployment or StatefulSet"
type RemediationRequestSpec struct {
	// incidentRef is the incident this plan answers.
	// +required
	IncidentRef IncidentRef `json:"incidentRef"`

	// targetRef is the live object the change is about.
	// +kubebuilder:validation:XValidation:rule="has(self.uid) && size(self.uid) > 0",message="targetRef.uid is required"
	// +kubebuilder:validation:XValidation:rule="has(self.namespace) && size(self.namespace) > 0",message="targetRef.namespace is required"
	// +required
	TargetRef ObjectRef `json:"targetRef"`

	// mode is how the plan is carried out.
	// +kubebuilder:default=GitOps
	// +optional
	Mode RemediationMode `json:"mode,omitempty"`

	// approvalRequired asks for human review. false is a request for automation that policy may refuse;
	// it never lowers policy or repository rules.
	// +kubebuilder:default=true
	// +optional
	ApprovalRequired *bool `json:"approvalRequired,omitempty"`

	// policyRef names the RemediationPolicy that must allow this plan.
	// +required
	PolicyRef Name `json:"policyRef"`

	// action is the typed change.
	// +required
	Action Action `json:"action"`

	// verification says when the change counts as a success.
	// +kubebuilder:default={}
	// +optional
	Verification Verification `json:"verification,omitzero"`
}

// RemediationPhase summarizes where a request is. Conditions carry the reasons.
// +kubebuilder:validation:Enum=Pending;PolicyChecked;PRCreated;AwaitingApproval;AutoMergeQueued;Merged;WaitingForDelivery;Verifying;Succeeded;Denied;Failed;Expired;Cancelled;Superseded;NeedsIntervention
type RemediationPhase string

// Phases of a RemediationRequest (design section 6).
const (
	// PhasePending means the request was accepted and is waiting for the policy check.
	PhasePending RemediationPhase = "Pending"
	// PhasePolicyChecked means the policy allows the plan and the source in Git is resolved.
	PhasePolicyChecked RemediationPhase = "PolicyChecked"
	// PhasePRCreated means the branch and pull request exist.
	PhasePRCreated RemediationPhase = "PRCreated"
	// PhaseAwaitingApproval means the pull request waits for human review.
	PhaseAwaitingApproval RemediationPhase = "AwaitingApproval"
	// PhaseAutoMergeQueued means policy allowed automation and auto-merge is enabled on the pull request.
	PhaseAutoMergeQueued RemediationPhase = "AutoMergeQueued"
	// PhaseMerged means the pull request is merged.
	PhaseMerged RemediationPhase = "Merged"
	// PhaseWaitingForDelivery means the change is merged and the GitOps controller has not applied it yet.
	PhaseWaitingForDelivery RemediationPhase = "WaitingForDelivery"
	// PhaseVerifying means the change is delivered and the verification checks are running.
	PhaseVerifying RemediationPhase = "Verifying"
	// PhaseSucceeded means every verification check held for stableFor. Terminal.
	PhaseSucceeded RemediationPhase = "Succeeded"
	// PhaseDenied means policy refused the plan; no pull request is opened. Terminal.
	PhaseDenied RemediationPhase = "Denied"
	// PhaseFailed means a step failed, for example verification did not pass before the timeout. Terminal.
	PhaseFailed RemediationPhase = "Failed"
	// PhaseExpired means a step reached its deadline. Terminal.
	PhaseExpired RemediationPhase = "Expired"
	// PhaseCancelled means the request was cancelled before the change was merged. Terminal.
	PhaseCancelled RemediationPhase = "Cancelled"
	// PhaseSuperseded means an input of the plan digest changed, so the pull request is closed and a new
	// request is needed. Terminal.
	PhaseSuperseded RemediationPhase = "Superseded"
	// PhaseNeedsIntervention means the controller cannot continue on its own, for example because the target
	// is not declared in a plain manifest; a human must act.
	PhaseNeedsIntervention RemediationPhase = "NeedsIntervention"
)

// Condition types on a RemediationRequest.
const (
	ConditionPolicyAllowed = "PolicyAllowed"
	ConditionPROpened      = "PullRequestOpened"
	ConditionApproved      = "Approved"
	ConditionDelivered     = "Delivered"
	ConditionVerified      = "Verified"
	// ConditionPlanFresh is False, with the reason, when an input of the plan digest changed and the request
	// was superseded.
	ConditionPlanFresh = "PlanFresh"
)

// PullRequestStatus records the pull request that carries the plan.
type PullRequestStatus struct {
	// number of the pull request.
	// +kubebuilder:validation:Minimum=1
	// +required
	Number int64 `json:"number"`

	// url of the pull request.
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://`
	// +required
	URL string `json:"url"`

	// headSHA is the commit Shadewright pushed. Any other head stops automation.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	// +required
	HeadSHA string `json:"headSHA"`

	// renderedTargetHead is the head of the target branch at which the change in the pull request was last
	// rendered and found to be exactly the planned one. While the pull request waits, the change is rendered
	// again only when the target branch head is another commit; empty means not rendered since it was recorded.
	// +kubebuilder:validation:MinLength=40
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	// +optional
	RenderedTargetHead string `json:"renderedTargetHead,omitempty"`
}

// PlanRepository identifies the repository a plan was made for: the URL and provider installation taken from
// RemediationSettings when planDigest was fixed. Cleanup of a superseded plan touches only this repository, so a
// settings entry that now points elsewhere can never get an unrelated pull request closed.
type PlanRepository struct {
	// url is the repository URL from RemediationSettings.
	// +kubebuilder:validation:MaxLength=512
	// +required
	URL string `json:"url"`

	// installationID is the provider installation that covers the repository; 0 when the provider has none.
	// +kubebuilder:validation:Minimum=0
	// +optional
	InstallationID int64 `json:"installationID,omitempty"`
}

// RemediationRequestStatus is written only by the remediation controller.
type RemediationRequestStatus struct {
	// observedGeneration is the generation the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// phase summarizes progress.
	// +optional
	Phase RemediationPhase `json:"phase,omitempty"`

	// effectiveApprovalRequired is what policy decided, which may be stricter than spec.approvalRequired.
	// +optional
	EffectiveApprovalRequired *bool `json:"effectiveApprovalRequired,omitempty"`

	// source is where the change is made, resolved by the controller before the policy check.
	// +optional
	Source *ResolvedSource `json:"source,omitempty"`

	// planDigest covers the semantic change, the target UID, the resolved source with the repository URL and
	// provider installation from RemediationSettings, and the policy UID and generation. It is fixed when the
	// branch is created; if any input changes later, the request becomes Superseded and its PR is closed.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// +optional
	PlanDigest string `json:"planDigest,omitempty"`

	// planRepository is the repository and installation the plan digest covers, set together with planDigest.
	// +optional
	PlanRepository *PlanRepository `json:"planRepository,omitempty"`

	// policyRevision is the policy name and generation the decision used, as name/generation. The policy UID
	// is recorded next to it, because a deleted and recreated policy can reach the same generation again.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	PolicyRevision string `json:"policyRevision,omitempty"`

	// policyUID is the UID of the policy the decision used. It is part of the plan digest.
	// +optional
	PolicyUID UID `json:"policyUID,omitempty"`

	// pullRequest is set once the pull request exists.
	// +optional
	PullRequest *PullRequestStatus `json:"pullRequest,omitempty"`

	// deadline is when the current step gives up.
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`

	// conditions explain the phase. Types: PolicyAllowed, PullRequestOpened, Approved, Delivered,
	// Verified and PlanFresh.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rr,categories=shadewright
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action.type`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="PR",type=integer,JSONPath=`.status.pullRequest.number`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RemediationRequest is one proposed fix: a typed action on one object, carried out as a GitOps pull request
// after deterministic policy allows it.
type RemediationRequest struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is the standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec is the plan.
	// +required
	Spec RemediationRequestSpec `json:"spec"`

	// status is the progress, written by the controller.
	// +optional
	Status RemediationRequestStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RemediationRequestList contains a list of RemediationRequest
type RemediationRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []RemediationRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &RemediationRequest{}, &RemediationRequestList{})
		return nil
	})
}
