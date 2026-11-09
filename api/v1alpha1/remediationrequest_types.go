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
// is available and its rollout is complete. KedaReady: the KEDA ScaledObject reports Ready=True, is not
// paused (Paused=True, which a scale-in or scale-out pause also sets), is not on fallback (Fallback=True) and
// its HPA can read its metrics (HPAActive is not False). QueueBacklogDecreasing: the backlog that drives the
// ScaledObject is going down. NoNewOOMKills: no container of the target is OOMKilled after the change.
// +kubebuilder:validation:Enum=DeliveryRevisionApplied;WorkloadHealthy;KedaReady;QueueBacklogDecreasing;NoNewOOMKills
type VerificationCheck string

// The verification checks, one per value of the VerificationCheck enum.
const (
	CheckDeliveryRevisionApplied VerificationCheck = "DeliveryRevisionApplied"
	CheckWorkloadHealthy         VerificationCheck = "WorkloadHealthy"
	CheckKedaReady               VerificationCheck = "KedaReady"
	CheckQueueBacklogDecreasing  VerificationCheck = "QueueBacklogDecreasing"
	CheckNoNewOOMKills           VerificationCheck = "NoNewOOMKills"
)

// VerificationChecks returns every value of the VerificationCheck enum, in the enum's order.
func VerificationChecks() []VerificationCheck {
	return []VerificationCheck{
		CheckDeliveryRevisionApplied, CheckWorkloadHealthy, CheckKedaReady, CheckQueueBacklogDecreasing, CheckNoNewOOMKills,
	}
}

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
	// PhaseSuperseded means a plan input changed, or the ScaledObject is no longer ready, or the module that
	// contributes the action was disabled, so the pull request is closed and a new request is needed. Terminal.
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
	// ConditionPlanFresh is False, with the reason, when an input of the plan digest changed, the ScaledObject
	// is no longer ready or the action's module was disabled, and the request was superseded (or stopped because its change request could not be closed). It is also False, with the
	// stale reason and the phase unchanged, once the cleanup of a stale plan has started: it is written before
	// the change request is closed, so a close whose answer was lost is known on the next reconcile to be
	// Shadewright's own, and the request ends Superseded rather than NeedsIntervention. It is Unknown, with reason
	// GitForbidden or GitRejected, while the Git host refuses the freshness check of a request whose pull
	// request waits for review; the pull request is still watched and nothing is written to Git.
	ConditionPlanFresh = "PlanFresh"
	// ConditionWritesPaused is True, with reason WritesPaused, while RemediationSettings spec.paused (the kill
	// switch) holds back the request's next Git write. The request keeps its phase and the rest of its status,
	// and the condition is removed once the request may write again or ends.
	ConditionWritesPaused = "WritesPaused"
	// ConditionQueued is True, with reason TargetLocked, ConcurrencyLimitReached, CircuitOpen or
	// CircuitHalfOpen, while a limit of the executor holds back a request the policy check has not let through
	// yet: another request holds the lock on the same target (status.holdsTarget), maxConcurrentRemediations
	// requests are active, the circuit breaker is open, or it is half-open and another request runs as its
	// probe. The request keeps no phase or Pending and is checked again every 30 seconds (every 10 minutes, and
	// when the holder changes, behind a lock held by a request that stopped); the condition is removed once the
	// limits let the request through, or at the decision of the policy check.
	ConditionQueued = "Queued"
)

// PullRequestStatus records the pull request that carries the plan.
// +kubebuilder:validation:XValidation:rule="!has(self.appliedObservedAt) || has(self.mergeCommitSHA)",message="appliedObservedAt needs mergeCommitSHA"
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

	// mergeCommitSHA is the commit the Git host reports the pull request was merged as, recorded when
	// Shadewright sees the merge of the pull request it pushed. Delivery is checked against it: the GitOps
	// controller must report this commit, or a later commit of the target branch containing it, as applied.
	// Empty until then, and when the host reported none. Once set it never changes.
	// +kubebuilder:validation:MinLength=40
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	// +optional
	MergeCommitSHA string `json:"mergeCommitSHA,omitempty"`

	// appliedObservedAt is when the executor first saw the GitOps controller report mergeCommitSHA, or a later
	// commit of the target branch that holds it, as applied and Ready. It is set once and never moved, so a
	// deadline measured from it (the rollout check, condition RolledOut) cannot be pushed back. Written only for
	// a Deployment or StatefulSet target delivered by a Flux Kustomization, within an hour of the merge.
	// +optional
	AppliedObservedAt *metav1.Time `json:"appliedObservedAt,omitempty"`
}

// RolloutStatus is what the check after the merge records about the target's rollout. Only the executor writes
// it; once status.rollout exists it is never removed, each field is set once and never changed (pausedSince is
// only set or cleared, never moved, and pausedSeconds never decreases), and ended never goes back to false.
// These rules sit on the RemediationRequest, so that dropping rollout, or the whole status, and writing it again
// cannot reopen the check or move its window.
//
// The rollout judged is the one the applied commit produced: the executor reads the target right after the
// GitOps controller reports the commit applied and records, in the same status write as
// status.pullRequest.appliedObservedAt, the commit applied (appliedRevision), the target's generation
// (targetGeneration) and a hash of its pod template (templateHash). A later commit of the target branch that holds
// the merge commit counts as ours: the template it produced is judged. A change made outside Git (kubectl edit,
// another controller) is not told apart from a Git one when it lands before that read, so it is not covered.
type RolloutStatus struct {
	// waitStartedAt is when the executor started waiting for the delivery of a merged request that has no
	// lastPhaseTransitionTime; the delivery window is then measured from it instead of from the merge.
	// +optional
	WaitStartedAt *metav1.Time `json:"waitStartedAt,omitempty"`

	// pausedSince is when the executor first saw writes paused (RemediationSettings spec.paused) while it waited
	// for the delivery; cleared when it sees them resumed, the paused time then added to pausedSeconds. It is
	// never moved while set.
	// +optional
	PausedSince *metav1.Time `json:"pausedSince,omitempty"`

	// pausedSeconds is how long, in seconds, writes were paused while the executor waited for the delivery; the
	// delivery window is extended by it (and by the pause still running), so a pause never uses up the window.
	// It never decreases.
	// +kubebuilder:validation:Minimum=0
	// +optional
	PausedSeconds int64 `json:"pausedSeconds,omitempty"`

	// appliedRevision is the commit the GitOps controller reported applied when the executor first saw the merge
	// commit delivered: the merge commit, or a later commit of the target branch that holds it.
	// +kubebuilder:validation:MinLength=40
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	// +optional
	AppliedRevision string `json:"appliedRevision,omitempty"`

	// targetGeneration is the target's metadata.generation read right after appliedRevision was seen applied:
	// the generation the applied commit produced.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TargetGeneration int64 `json:"targetGeneration,omitempty"`

	// templateHash is the SHA-256 of the target's pod template at targetGeneration. When the target's controller
	// first reports a generation past targetGeneration, the revision it names is recorded only if the template
	// is still this one; otherwise the check ends with RolledOut Unknown, reason RolloutSuperseded.
	// +kubebuilder:validation:MinLength=64
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	// +optional
	TemplateHash string `json:"templateHash,omitempty"`

	// templateRevision is the template revision the check judges: a StatefulSet's status.updateRevision or a
	// Deployment's deployment.kubernetes.io/revision annotation, read once the controller observed
	// targetGeneration and the target's current generation (a revision annotation can name a later generation
	// before the status does), and, past targetGeneration, only while the template still matches templateHash. A newer revision
	// ends the check with RolledOut Unknown, reason RolloutSuperseded.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	TemplateRevision string `json:"templateRevision,omitempty"`

	// ended is true once the check gave its final verdict in RolledOut; nothing is read for it after that.
	// +optional
	Ended bool `json:"ended,omitempty"`
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

	// phase summarizes progress. It is a selectable field (field selector status.phase), so the executor's
	// limits list only the phases they need from the API server.
	// +optional
	Phase RemediationPhase `json:"phase,omitempty"`

	// lastPhaseTransitionTime is when phase last changed, to the microsecond. The circuit breaker orders the
	// outcomes of requests by it.
	// +optional
	LastPhaseTransitionTime *metav1.MicroTime `json:"lastPhaseTransitionTime,omitempty"`

	// breakerProbe is true on a request the policy check let through while the circuit breaker was tripped
	// (half-open): it is the breaker's probe. It is written with the allowed decision and never cleared. The
	// breaker finds a running probe, or one that ended without a success, by this field only; a request
	// without it is not a probe. It is a selectable field (field selector status.breakerProbe).
	// +optional
	BreakerProbe bool `json:"breakerProbe,omitempty"`

	// holdsTarget is true while the request holds the executor's lock on its target: it is set in the status
	// write of the allowed decision and cleared only once the request's pull request is known merged or closed,
	// or a look-up by its branch and marker found none of its own open. While it is true no other request on the
	// same target goes through the policy check. Unset on a request not yet decided, and on one written before the
	// field existed; the executor then derives it once from the request's phase and pull request, keeping the
	// lock when in doubt.
	// A refused request gets false with its refusal.
	// It is a selectable field (field selector status.holdsTarget), so the executor's lock reads only holders.
	// +optional
	HoldsTarget *bool `json:"holdsTarget,omitempty"`

	// breakerCounts says whether the executor's limits still read the request for the circuit breaker. It is
	// set in the status write that ends the request: true for a success (Merged or a later phase), a failure
	// (Failed, NeedsIntervention) or a probe (breakerProbe) that ended in any other way, false for any other
	// end. The executor sets it to false only when a newer request in Succeeded is recorded (breakerClearedBy
	// names it), and back to true when that request is deleted, or when a request whose deletion may still close
	// a pull request (NeedsIntervention, Expired) is being deleted. It is unset while the request has not ended
	// and on one that ended before the field existed; an unset request is read (fail closed), and the executor
	// sets the field once on an ended one from its status. It is a selectable field (field selector
	// status.breakerCounts), so the breaker reads only the outcomes that still count.
	// +optional
	BreakerCounts *bool `json:"breakerCounts,omitempty"`

	// breakerClearedBy is the name of the request in Succeeded whose success made the executor set breakerCounts
	// to false on this one, written in the same status write. When that request is deleted, its finalizer sets
	// breakerCounts back to true on every request that names it, before the finalizer is removed. It is a
	// selectable field (field selector status.breakerClearedBy).
	// +optional
	// +kubebuilder:validation:MaxLength=253
	BreakerClearedBy string `json:"breakerClearedBy,omitempty"`

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

	// rollout is what the check after the merge keeps between reconciles (the RolledOut condition).
	// +optional
	Rollout *RolloutStatus `json:"rollout,omitempty"`

	// deadline is when the current step gives up.
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`

	// conditions explain the phase. Types: PolicyAllowed, PullRequestOpened, Approved, Delivered,
	// Verified, PlanFresh, WritesPaused, Queued and RolledOut (after the merge: whether the target's pods run
	// the applied template; False with reason RolloutStuck or DeliveryNotObserved, Unknown while in progress and
	// with reason KindNotChecked, RolloutSuperseded or RolloutNotDetermined when the check ends without a verdict).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rr,categories=shadewright
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Policy",type=string,JSONPath=`.status.conditions[?(@.type=="PolicyAllowed")].reason`,description="Reason of the PolicyAllowed condition: the policy decision"
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action.type`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="PR",type=integer,JSONPath=`.status.pullRequest.number`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:selectablefield:JSONPath=`.status.phase`
// +kubebuilder:selectablefield:JSONPath=`.status.breakerProbe`
// +kubebuilder:selectablefield:JSONPath=`.status.holdsTarget`
// +kubebuilder:selectablefield:JSONPath=`.status.breakerCounts`
// +kubebuilder:selectablefield:JSONPath=`.status.breakerClearedBy`
// The set-once rules of status.pullRequest live on the object, not on pullRequest, so that removing
// pullRequest (or the whole status) and writing it again cannot change a recorded value.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.pullRequest) || !has(oldSelf.status.pullRequest.mergeCommitSHA) || (has(self.status) && has(self.status.pullRequest) && has(self.status.pullRequest.mergeCommitSHA) && self.status.pullRequest.mergeCommitSHA == oldSelf.status.pullRequest.mergeCommitSHA)",message="status.pullRequest.mergeCommitSHA is set once and never changed"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.pullRequest) || !has(oldSelf.status.pullRequest.appliedObservedAt) || (has(self.status) && has(self.status.pullRequest) && has(self.status.pullRequest.appliedObservedAt) && self.status.pullRequest.appliedObservedAt == oldSelf.status.pullRequest.appliedObservedAt)",message="status.pullRequest.appliedObservedAt is set once and never changed"
// The same holds for status.rollout: once set it stays, each of its fields is set once, and ended never goes
// back to false, so the delivery window cannot restart and a final verdict cannot reopen.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.rollout) || (has(self.status) && has(self.status.rollout))",message="status.rollout is kept once set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.rollout) || !has(oldSelf.status.rollout.waitStartedAt) || (has(self.status) && has(self.status.rollout) && has(self.status.rollout.waitStartedAt) && self.status.rollout.waitStartedAt == oldSelf.status.rollout.waitStartedAt)",message="status.rollout.waitStartedAt is set once and never changed"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.rollout) || !has(oldSelf.status.rollout.appliedRevision) || (has(self.status) && has(self.status.rollout) && has(self.status.rollout.appliedRevision) && self.status.rollout.appliedRevision == oldSelf.status.rollout.appliedRevision)",message="status.rollout.appliedRevision is set once and never changed"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.rollout) || !has(oldSelf.status.rollout.targetGeneration) || (has(self.status) && has(self.status.rollout) && has(self.status.rollout.targetGeneration) && self.status.rollout.targetGeneration == oldSelf.status.rollout.targetGeneration)",message="status.rollout.targetGeneration is set once and never changed"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.rollout) || !has(oldSelf.status.rollout.templateHash) || (has(self.status) && has(self.status.rollout) && has(self.status.rollout.templateHash) && self.status.rollout.templateHash == oldSelf.status.rollout.templateHash)",message="status.rollout.templateHash is set once and never changed"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.rollout) || !has(oldSelf.status.rollout.templateRevision) || (has(self.status) && has(self.status.rollout) && has(self.status.rollout.templateRevision) && self.status.rollout.templateRevision == oldSelf.status.rollout.templateRevision)",message="status.rollout.templateRevision is set once and never changed"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.rollout) || !has(oldSelf.status.rollout.pausedSince) || !has(self.status) || !has(self.status.rollout) || !has(self.status.rollout.pausedSince) || self.status.rollout.pausedSince == oldSelf.status.rollout.pausedSince",message="status.rollout.pausedSince is never moved while set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.rollout) || !has(oldSelf.status.rollout.pausedSeconds) || (has(self.status) && has(self.status.rollout) && has(self.status.rollout.pausedSeconds) && self.status.rollout.pausedSeconds >= oldSelf.status.rollout.pausedSeconds)",message="status.rollout.pausedSeconds never decreases"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.rollout) || !has(oldSelf.status.rollout.ended) || !oldSelf.status.rollout.ended || (has(self.status) && has(self.status.rollout) && has(self.status.rollout.ended) && self.status.rollout.ended)",message="status.rollout.ended never goes back to false"

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
