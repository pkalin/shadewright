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

// RequestRef names another RemediationRequest in the same namespace.
type RequestRef struct {
	// name of the request.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +required
	Name string `json:"name"`

	// uid of the request, so a request recreated under the same name is never taken for it.
	// +required
	UID UID `json:"uid"`
}

// MaxQueuedBehind is the most requests status.queuedBehind names. The lock lets one request hold a target, two
// during a handoff (ADR-0028); a request that finds more holders records the first ones by name.
const MaxQueuedBehind = 4

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
// +kubebuilder:validation:XValidation:rule="has(self.replaces) == has(self.chainRoot)",message="replaces and chainRoot are set together or not at all"
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

	// replaces names the request this one replaces and the reviewer hint of that request it answers
	// (ADR-0028 section 4). Set only by the analyzer on a replacement, together with chainRoot. It is part of
	// the plan, so it is part of the request's name: the same plan answering another hint is another request.
	// The executor checks the link once, at the policy check, and then takes the pull request over (the handoff).
	// +optional
	Replaces *RequestReplacement `json:"replaces,omitempty"`

	// chainRoot is the UID of the first request of the supersede chain: the replaced request's chainRoot, or
	// its own UID when it has none. Set together with replaces. The branch of the pull request derives from it
	// (shadewright/<chainRoot>), so every request of a chain works on the same branch and pull request.
	// +optional
	ChainRoot UID `json:"chainRoot,omitempty"`
}

// RemediationPhase summarizes where a request is. Conditions carry the reasons.
// +kubebuilder:validation:Enum=Pending;PolicyChecked;PRCreated;AwaitingApproval;AutoMergeQueued;Merged;WaitingForDelivery;Verifying;Succeeded;Denied;Failed;Expired;Cancelled;Superseded;NeedsIntervention;Inconclusive
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
	// contributes the action was disabled, so the pull request is closed and a new request is needed. With
	// reason HandedOff (ADR-0028) a replacement took over the pull request instead, which stays open. With
	// reason AnotherRequestMerged (on PlanFresh) another request of the same Incident and target was merged
	// while this one waited, before it took the target lock or opened a pull request. With reason
	// AnotherRequestClosed (on PlanFresh) this one waited behind the target lock of another request for the same
	// target and field (status.queuedBehind) whose pull request was closed without a merge, before it took the
	// lock or opened a pull request.
	// Terminal.
	PhaseSuperseded RemediationPhase = "Superseded"
	// PhaseNeedsIntervention means the controller cannot continue on its own, for example because the target
	// is not declared in a plain manifest; a human must act.
	PhaseNeedsIntervention RemediationPhase = "NeedsIntervention"
	// PhaseInconclusive means the change was merged and neither success nor failure could be shown; condition
	// Verified is Unknown and its reason says why, for example NoMergeReceipt when the merge could not be bound
	// to a merge receipt (ADR-0030 section 5). Terminal.
	PhaseInconclusive RemediationPhase = "Inconclusive"
)

// Condition types on a RemediationRequest.
const (
	ConditionPolicyAllowed = "PolicyAllowed"
	ConditionPROpened      = "PullRequestOpened"
	ConditionApproved      = "Approved"
	// ConditionDelivered is True once the GitOps controller was seen applying the merge commit, or a later
	// commit of the receipt's target branch that holds it (ADR-0030 section 2); False, with reason
	// DeliveryFailed, DeliveryBlocked, DeliverySuspended or DeliveryNotObserved, and Unknown with reason
	// DeliveryNotChecked, when the delivery window ended without that.
	ConditionDelivered = "Delivered"
	// ConditionVerified sums up verification and decides the phase after the merge: False with the reason of a
	// Failed request, Unknown with the reason of an Inconclusive one (ADR-0030 sections 3 and 5).
	ConditionVerified = "Verified"
	// ConditionConfigured is True while the live target's field holds the desired value of the action, read
	// with the field accessor of the source mapping, and False with reason ValueChanged when it does not (a
	// later commit overwrote it), which ends the request Inconclusive (ADR-0030 section 3).
	ConditionConfigured = "Configured"
	// ConditionCleared says whether the symptom cleared after the change (ADR-0030 section 3), judged by the
	// executor with the verifier's VerificationReport as its evidence: True with reason NoRecurrence when a quiet
	// window held for the effective stableFor plus the node grace, False with reason OOMKilled or Restarted when
	// the report recorded that, and Unknown while the window runs (reason Observing) and when the request ends
	// without evidence (the reason of Verified, for example ClearingNotChecked for a ScaledObject, whose symptom
	// container status does not show).
	ConditionCleared = "Cleared"
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
// +kubebuilder:validation:XValidation:rule="!has(self.reviewCheck) || self.reviewCheck.head == self.headSHA",message="reviewCheck names the recorded head"
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

	// mergedAt is when the executor first recorded the request Merged, set in the status write that sets the
	// phase and never moved, unlike lastPhaseTransitionTime, which every later phase moves. A duplicate of the same
	// Incident and target created before it ends Superseded (reason AnotherRequestMerged, #556). Empty on a
	// request merged before the field existed.
	// +optional
	MergedAt *metav1.MicroTime `json:"mergedAt,omitempty"`

	// reviewCheck is the state of the check run shadewright/review on headSHA (ADR-0028 section 5, the merge
	// gate), written only with reviewer hints on: InProgress once the executor created it on that head, Passed
	// once it completed it with success after an approval of that head by someone who wrote no hint in the
	// chain, Failed when the executor could not read every review of the pull request and completed it with
	// failure (it fails closed; condition Approved False with reason ReviewListTooLong), Refused when the Git host
	// refused to create or complete it for good (condition Approved then says so). It always names headSHA: a new
	// head starts without it, so a state never outlives its head.
	// +optional
	ReviewCheck *ReviewCheck `json:"reviewCheck,omitempty"`
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

// VerificationReset is a reset of the executor's own verification window (ADR-0030 section 4): a check of a
// Verifying request that was not good.
type VerificationReset struct {
	// reason says why the window was reset: NotAvailable (the target reports fewer available replicas than
	// desired, or a ScaledObject is not ready by the KedaReady rule), StaleReport (the verifier's report is
	// missing, was never sampled, or its sample counter stopped rising), ReportMismatch (the report names another
	// request, merge commit, target, template revision or container) or PartialRollout (the report does not cover
	// every desired replica). Every one is a reset of the no-evidence class.
	// +kubebuilder:validation:Enum=NotAvailable;StaleReport;ReportMismatch;PartialRollout
	// +required
	Reason ResetReason `json:"reason"`

	// time is when the reset was recorded, on the executor's clock. It is never earlier than windowStart at that
	// moment or the previous reset, so the window is open only while windowStart is later than it.
	// +required
	Time metav1.Time `json:"time"`
}

// VerificationWindow is the executor's own stable window of a Verifying request (ADR-0030 section 4), kept next
// to the verifier's window in the VerificationReport. Every time in it is on the executor's clock, and it is
// never compared with a time of the report. A restart of the executor loses nothing of it.
type VerificationWindow struct {
	// windowStart is when the current window opened: the first good check after the last reset. It only moves
	// forward.
	// +optional
	WindowStart *metav1.Time `json:"windowStart,omitempty"`

	// lastReset is the last reset of the window. Kept once set; its time never moves back.
	// +optional
	LastReset *VerificationReset `json:"lastReset,omitempty"`

	// resets counts the resets of the window. It never decreases.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Resets int64 `json:"resets,omitempty"`
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

// MergeReceipt binds a merge to the repository, branch, pull request and head it was made from (ADR-0030
// section 1). The executor writes it in the status write that sets Merged, only for a merge that holds exactly
// the planned change and has a full merge commit ID, and never changes it after; every later check of the
// merge (delivery, ancestry) reads it instead of the live settings.
type MergeReceipt struct {
	// repository is the repository the plan was made for, copied from status.planRepository.
	// +required
	Repository PlanRepository `json:"repository"`

	// targetBranch is the branch the pull request merged into, copied from status.source.targetBranch.
	// +required
	TargetBranch GitBranch `json:"targetBranch"`

	// pullRequest is the number of the merged pull request.
	// +kubebuilder:validation:Minimum=1
	// +required
	PullRequest int64 `json:"pullRequest"`

	// headSHA is the head that was merged: the commit Shadewright pushed, or the replacement's commit after a
	// handoff. It equals status.pullRequest.headSHA when the receipt is written.
	// +kubebuilder:validation:MinLength=40
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	// +required
	HeadSHA string `json:"headSHA"`

	// mergeCommitSHA is the commit the Git host reports for the merge. It equals
	// status.pullRequest.mergeCommitSHA (a validation rule on the object).
	// +kubebuilder:validation:MinLength=40
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	// +required
	MergeCommitSHA string `json:"mergeCommitSHA"`

	// mergedAt is when the executor recorded the merge: status.pullRequest.mergedAt of the same status write.
	// +required
	MergedAt metav1.MicroTime `json:"mergedAt"`
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

	// queuedBehind names the requests that held the lock on the target when the executor last held this one back
	// (Queued, with any reason) while the target was locked, written in that status write (#643). When a later check finds other
	// holders, the executor first reads the recorded ones from the API server and only then replaces them. Until
	// the request is decided, it ends Superseded (PlanFresh False, reason AnotherRequestClosed) when a recorded
	// request ended NeedsIntervention because its pull request for the same target and field was closed without a
	// merge. A request that never waited behind that one has no record of it and is decided as usual. Only the
	// executor writes it; once set it is never removed or emptied (a validation rule refuses that). At most four
	// holders are recorded, those for the same field first: a request behind more holders than that may have no
	// record of one, and is then decided as usual when that one's pull request is closed.
	// +listType=map
	// +listMapKey=uid
	// +kubebuilder:validation:MaxItems=4
	// +optional
	QueuedBehind []RequestRef `json:"queuedBehind,omitempty"`

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

	// mergeReceipt binds the merge to its repository, target branch, pull request, head and merge commit
	// (ADR-0030 section 1). It is written in the status write that sets Merged and is never changed or removed
	// after (validation rules on the object). A request merged without a full merge commit ID gets none and ends
	// Inconclusive with reason NoMergeReceipt; a merge with a foreign change (MergedWithForeignChange) gets none
	// either. Unset on a request merged before the field existed (condition Verified Unknown, reason
	// MergedBeforeVerification).
	// +optional
	MergeReceipt *MergeReceipt `json:"mergeReceipt,omitempty"`

	// mergeCommitAwaitedSince is when the executor first saw this request's pull request merged without a full
	// merge commit ID, on its own clock. The executor waits for the Git host to report the commit for five minutes
	// from it (GitHub computes it after the merge), keeping the phase, and then records the merge without a
	// receipt (NoMergeReceipt, ending Inconclusive) or, for a pull request it never recorded, as merged with a
	// change it cannot check. Set once and never changed or removed (a validation rule on the object), so a
	// restart does not start the wait again. Unset when the commit was reported at once.
	// +optional
	MergeCommitAwaitedSince *metav1.Time `json:"mergeCommitAwaitedSince,omitempty"`

	// rollout is what the check after the merge keeps between reconciles (the RolledOut condition).
	// +optional
	Rollout *RolloutStatus `json:"rollout,omitempty"`

	// deadline is when the current step gives up, on the executor's clock. In WaitingForDelivery it is the end
	// of the delivery window: one hour after mergeReceipt.mergedAt, extended by the time writes were paused. In
	// Verifying it is the timeout, written on entering the phase: status.pullRequest.appliedObservedAt plus the
	// larger of spec.verification.timeout and the rollout deadline plus the effective stableFor plus the
	// executor's --node-grace. Past the merge a deadline never ends a request Expired: the evidence decides
	// between Failed and Inconclusive (ADR-0030 sections 2, 4 and 6).
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`

	// verification is the executor's own stable window while the request is Verifying (ADR-0030 section 4). Only
	// the executor writes it; once set it is kept, windowStart and the time of lastReset only move forward and
	// resets never decreases (validation rules on the object).
	// +optional
	Verification *VerificationWindow `json:"verification,omitempty"`

	// hints are the reviewer hints the executor accepted from comments on the pull request, and the hints a
	// replacement carried over from its chain (ADR-0028). Two bounds: at most MaxStoredHints entries in all,
	// and at most MaxHints of them Consumed or Reserved, the hints a supersede chain may consume. Only the
	// executor writes them; a stored hint keeps its pin, moves only forward in its state and is never removed,
	// so a Declined hint stays as the record of what was asked. Planned: carrying them over to a replacement.
	// +listType=map
	// +listMapKey=source
	// +listMapKey=commentID
	// +kubebuilder:validation:MaxItems=10
	// +kubebuilder:validation:XValidation:rule="self.filter(h, h.state in ['Consumed', 'Reserved']).size() <= 3",message="status.hints: at most 3 hints are Consumed or Reserved"
	// +optional
	Hints []Hint `json:"hints,omitempty"`

	// refusedHints are the newest comments with the command that the executor refused, at most
	// MaxRefusedHints, oldest first; the oldest one whose reply is recorded is dropped for a new one, and while
	// every one still waits for its reply a new refusal is not recorded. Only the executor writes them.
	// +listType=map
	// +listMapKey=source
	// +listMapKey=commentID
	// +kubebuilder:validation:MaxItems=10
	// +optional
	RefusedHints []RefusedHint `json:"refusedHints,omitempty"`

	// commentCursor is where the executor's next look at the comments of the pull request starts (ADR-0028
	// section 3). Only the executor writes it.
	// +optional
	CommentCursor *CommentCursor `json:"commentCursor,omitempty"`

	// handoff records the supersede handoff (ADR-0028 section 4): on a replacement, the request it takes over
	// (from); on a predecessor, its replacement (to); a request in the middle of a chain has both. Each side is
	// never removed; Handoff says what may change. Only the executor writes it.
	// +optional
	Handoff *Handoff `json:"handoff,omitempty"`

	// conditions explain the phase. Types: PolicyAllowed, PullRequestOpened, Approved, Delivered,
	// Verified, Configured, Cleared, PlanFresh, WritesPaused, Queued and RolledOut (after the merge: whether the target's
	// pods run the applied template; True with reason RolloutComplete, or RolloutPartitioned when a partitioned
	// StatefulSet keeps pods on the old revision on purpose; False with reason RolloutStuck or
	// DeliveryNotObserved, Unknown while in progress and with reason KindNotChecked, RolloutSuperseded or
	// RolloutNotDetermined when the check ends without a verdict).
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
// status.mergeReceipt is set once: once written it is never changed, also by removing it or the whole status and
// writing it again. Its pull request, head, merge commit and merge time are always the ones status.pullRequest
// records, so while a receipt exists status.pullRequest.number and headSHA cannot change either; and it exists
// only in Merged or a phase after it (ADR-0030 section 1).
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.mergeReceipt) || (has(self.status) && has(self.status.mergeReceipt) && self.status.mergeReceipt == oldSelf.status.mergeReceipt)",message="status.mergeReceipt is set once and never changed"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.mergeReceipt) || (has(self.status.pullRequest) && has(self.status.pullRequest.mergeCommitSHA) && self.status.mergeReceipt.mergeCommitSHA == self.status.pullRequest.mergeCommitSHA)",message="status.mergeReceipt.mergeCommitSHA must equal status.pullRequest.mergeCommitSHA"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.mergeReceipt) || (has(self.status.pullRequest) && self.status.mergeReceipt.pullRequest == self.status.pullRequest.number && self.status.mergeReceipt.headSHA == self.status.pullRequest.headSHA && has(self.status.pullRequest.mergedAt) && self.status.mergeReceipt.mergedAt == self.status.pullRequest.mergedAt)",message="status.mergeReceipt.pullRequest, headSHA and mergedAt must equal status.pullRequest.number, headSHA and mergedAt"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.mergeCommitAwaitedSince) || (has(self.status) && has(self.status.mergeCommitAwaitedSince) && self.status.mergeCommitAwaitedSince == oldSelf.status.mergeCommitAwaitedSince)",message="status.mergeCommitAwaitedSince is set once and never changed"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.mergeReceipt) || (has(self.status.phase) && !(self.status.phase in ['Pending', 'PolicyChecked', 'PRCreated', 'AwaitingApproval', 'AutoMergeQueued']))",message="status.mergeReceipt is set only in Merged or a phase after it"
// WaitingForDelivery and Verifying are measured from the receipt (ADR-0030 section 2), and Verifying from the
// applied time, which the executor writes in the same status write that sets Verifying.
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.phase) || !(self.status.phase in ['WaitingForDelivery', 'Verifying']) || has(self.status.mergeReceipt)",message="status.phase WaitingForDelivery and Verifying require status.mergeReceipt"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.phase) || self.status.phase != 'Verifying' || (has(self.status.pullRequest) && has(self.status.pullRequest.appliedObservedAt))",message="status.phase Verifying requires status.pullRequest.appliedObservedAt"
// The hints and the handoff follow the same pattern: a hint once stored is never removed, and neither side of
// the handoff is (their pin and phase rules live on their types). A side written for the first time starts in
// its first phase.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.hints) || (has(self.status) && has(self.status.hints) && oldSelf.status.hints.all(o, self.status.hints.exists(h, h.source == o.source && h.commentID == o.commentID)))",message="status.hints: a stored hint is never removed"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.handoff) || !has(oldSelf.status.handoff.from) || (has(self.status) && has(self.status.handoff) && has(self.status.handoff.from))",message="status.handoff.from is never removed"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.handoff) || !has(oldSelf.status.handoff.to) || (has(self.status) && has(self.status.handoff) && has(self.status.handoff.to))",message="status.handoff.to is never removed"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.handoff) || !has(self.status.handoff.from) || self.status.handoff.from.phase == 'Reserved' || (has(oldSelf.status) && has(oldSelf.status.handoff) && has(oldSelf.status.handoff.from))",message="status.handoff.from starts in Reserved"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.handoff) || !has(self.status.handoff.to) || self.status.handoff.to.phase == 'Intent' || (has(oldSelf.status) && has(oldSelf.status.handoff) && has(oldSelf.status.handoff.to))",message="status.handoff.to starts in Intent"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.handoff) || !has(self.status.handoff.from) || (has(self.spec.replaces) && self.status.handoff.from.name == self.spec.replaces.name && self.status.handoff.from.uid == self.spec.replaces.uid && self.status.handoff.from.hint.source == self.spec.replaces.hint.source && self.status.handoff.from.hint.commentID == self.spec.replaces.hint.commentID && self.status.handoff.from.hint.digest == self.spec.replaces.hint.digest)",message="status.handoff.from must match spec.replaces"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.handoff) || !has(self.status.handoff.to) || (has(self.status.hints) && self.status.hints.exists(h, h.source == self.status.handoff.to.hint.source && h.commentID == self.status.handoff.to.hint.commentID && h.digest == self.status.handoff.to.hint.digest && h.state == (self.status.handoff.to.phase == 'Intent' ? 'Reserved' : self.status.handoff.to.phase == 'Done' ? 'Consumed' : 'Declined')))",message="status.handoff.to must name a hint in status.hints that is Reserved while it is Intent, Consumed once Done and Declined once Aborted"
// status.commentCursor, the watermark of the reviewer hints, is kept once set and its since never decreases, so
// a comment read already cannot be read again as new (ADR-0028 section 3).
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.commentCursor) || (has(self.status) && has(self.status.commentCursor))",message="status.commentCursor is kept once set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.commentCursor) || !has(oldSelf.status.commentCursor.since) || (has(self.status) && has(self.status.commentCursor) && has(self.status.commentCursor.since) && self.status.commentCursor.since >= oldSelf.status.commentCursor.since)",message="status.commentCursor.since never decreases"
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
// status.verification, the executor's own stable window, is kept once set, its windowStart and the time of its
// lastReset only move forward and resets never decreases, so no write can move the window back (ADR-0030
// section 4).
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.verification) || (has(self.status) && has(self.status.verification))",message="status.verification is kept once set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.verification) || !has(oldSelf.status.verification.windowStart) || (has(self.status) && has(self.status.verification) && has(self.status.verification.windowStart) && timestamp(self.status.verification.windowStart) >= timestamp(oldSelf.status.verification.windowStart))",message="status.verification.windowStart only moves forward"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.verification) || !has(oldSelf.status.verification.lastReset) || (has(self.status) && has(self.status.verification) && has(self.status.verification.lastReset) && timestamp(self.status.verification.lastReset.time) >= timestamp(oldSelf.status.verification.lastReset.time))",message="status.verification.lastReset is kept once set and its time never moves back"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.verification) || !has(oldSelf.status.verification.resets) || (has(self.status) && has(self.status.verification) && has(self.status.verification.resets) && self.status.verification.resets >= oldSelf.status.verification.resets)",message="status.verification.resets never decreases"
// status.queuedBehind is kept once set: its entries may be replaced, but the field is never removed or emptied,
// so a write cannot drop the record that ends a request queued behind a pull request closed unmerged (#643).
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.queuedBehind) || size(oldSelf.status.queuedBehind) == 0 || (has(self.status) && has(self.status.queuedBehind) && size(self.status.queuedBehind) > 0)",message="status.queuedBehind is kept once set"

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
