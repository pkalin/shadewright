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
)

// Reviewer hints and the supersede handoff (ADR-0028). Planned: the types are in the API, nothing writes them yet.

// MaxHints is the most hints a request's status.hints holds, which is also the most hints one supersede chain
// can consume: the consumed hints of a chain are carried into the replacement's status (ADR-0028 section 6).
const MaxHints = 3

// MaxHintTextBytes is the most bytes of a hint's text after redaction (ADR-0028 section 2).
const MaxHintTextBytes = 2048

// Reasons of the supersede handoff (ADR-0028 section 4). BranchDiverged, which a push by a person during a
// handoff can also cause, is a reason of the pull request step.
const (
	// ReasonHandedOff: the request ended Superseded because its replacement took over its pull request and
	// branch. It is the one Superseded whose pull request may still be open.
	ReasonHandedOff = "HandedOff"
	// ReasonChainLinkInvalid: a replacement was Denied at admission because the request named in spec.replaces
	// does not exist, names another Incident, target or chain root, is not AwaitingApproval with an open pull
	// request, or does not hold the named hint Pending.
	ReasonChainLinkInvalid = "ChainLinkInvalid"
	// ReasonHandoffRefused: a replacement ended Cancelled because the predecessor's write that reserves the
	// hint conflicted, or the hint was no longer Pending, or the predecessor left AwaitingApproval first.
	ReasonHandoffRefused = "HandoffRefused"
	// ReasonHandoffAborted: a replacement ended Cancelled because the handoff was abandoned in Git: a foreign
	// push to the branch, or the pull request closed or merged with the old head around the branch update.
	ReasonHandoffAborted = "HandoffAborted"
)

// HintSource is the system a hint came from. GitHub is the only one; Azure DevOps is planned.
// +kubebuilder:validation:Enum=GitHub
type HintSource string

const (
	// HintSourceGitHub is a comment on a GitHub pull request.
	HintSourceGitHub HintSource = "GitHub"
)

// HintState says what became of a hint. Pending: stored, not yet answered. Reserved: named by exactly one
// replacement being admitted. Consumed: the replacement holds the pull request. Declined: answered without a
// replacement, with the reason. A hint moves only from Pending to Reserved or Declined, and from Reserved to
// Consumed or Declined; Consumed and Declined are final.
// +kubebuilder:validation:Enum=Pending;Reserved;Consumed;Declined
type HintState string

// The hint states, one per value of the HintState enum.
const (
	HintPending  HintState = "Pending"
	HintReserved HintState = "Reserved"
	HintConsumed HintState = "Consumed"
	HintDeclined HintState = "Declined"
)

// HintRef pins one hint: where it came from, the comment that carried it and the digest of the stored text.
// A comment edited after the hint was stored does not change what the pin names.
type HintRef struct {
	// source is the system the comment is in.
	// +required
	Source HintSource `json:"source"`

	// commentID is the comment's ID in the source.
	// +kubebuilder:validation:Minimum=1
	// +required
	CommentID int64 `json:"commentID"`

	// digest is the SHA-256 of the stored, redacted hint text.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// +required
	Digest string `json:"digest"`
}

// HintAuthor is the comment's author as the source's API reported it, never a name taken from the comment's
// text. The merge gate compares authors by id, so a renamed login carries nothing over.
type HintAuthor struct {
	// id is the author's numeric user ID in the source.
	// +kubebuilder:validation:Minimum=1
	// +required
	ID int64 `json:"id"`

	// login is the author's login when the hint was accepted, for the reader only.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=100
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]*$`
	// +required
	Login string `json:"login"`
}

// Hint is one reviewer hint the executor accepted from a comment on the request's pull request (ADR-0028
// section 2): its author had write access when it was accepted, its text is redacted and bounded, and it is
// pinned by comment ID, digest and the comment's updatedAt. Only the executor writes it; the analyzer reads
// the stored text, never the comment.
// +kubebuilder:validation:XValidation:rule="size(bytes(self.text)) <= 2048",message="text must be at most 2048 bytes"
// +kubebuilder:validation:XValidation:rule="self.state != 'Declined' || has(self.reason)",message="a Declined hint needs a reason"
// +kubebuilder:validation:XValidation:rule="self.source == oldSelf.source && self.commentID == oldSelf.commentID && self.digest == oldSelf.digest && self.text == oldSelf.text && self.author == oldSelf.author && self.updatedAt == oldSelf.updatedAt && self.acceptedAt == oldSelf.acceptedAt",message="a stored hint is pinned: only its state and reason change"
// +kubebuilder:validation:XValidation:rule="oldSelf.state == self.state || (oldSelf.state == 'Pending' && self.state in ['Reserved', 'Declined']) || (oldSelf.state == 'Reserved' && self.state in ['Consumed', 'Declined'])",message="a hint moves only from Pending to Reserved or Declined, and from Reserved to Consumed or Declined"
type Hint struct {
	// source is the system the comment is in.
	// +required
	Source HintSource `json:"source"`

	// commentID is the comment's ID in the source.
	// +kubebuilder:validation:Minimum=1
	// +required
	CommentID int64 `json:"commentID"`

	// author wrote the comment.
	// +required
	Author HintAuthor `json:"author"`

	// text is the hint after redaction, at most 2048 bytes. It is untrusted input: the analysis fences it as
	// a reviewer's opinion, never as instructions or evidence.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +required
	Text string `json:"text"`

	// digest is the SHA-256 of text.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// +required
	Digest string `json:"digest"`

	// updatedAt is the comment's updated time when the hint was accepted. A comment whose updated time
	// differs later was edited after acceptance, and the edit is ignored.
	// +required
	UpdatedAt metav1.Time `json:"updatedAt"`

	// acceptedAt is when the executor accepted the hint.
	// +required
	AcceptedAt metav1.Time `json:"acceptedAt"`

	// state says what became of the hint.
	// +required
	State HintState `json:"state"`

	// reason says why a Declined hint was declined, for example BudgetSpent or NoProposal.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Z][A-Za-z0-9]*$`
	// +optional
	Reason string `json:"reason,omitempty"`
}

// RequestReplacement names the request a replacement takes over and the hint it answers (ADR-0028 section 4).
type RequestReplacement struct {
	// name of the replaced request, in the same namespace.
	// +required
	Name Name `json:"name"`

	// uid of the replaced request, so a request recreated under the same name is never taken for it.
	// +required
	UID UID `json:"uid"`

	// hint is the pinned hint of the replaced request that this plan answers.
	// +required
	Hint HintRef `json:"hint"`
}

// HandoffPhase is how far a handoff has come. On the replacement: Reserved (it took the target lock and named
// its predecessor), then Done (it holds the pull request) or Aborted. On the predecessor: Intent (it expects its
// replacement's commit on top of expectedHead), then Done (it ended Superseded with HandedOff) or Aborted.
// +kubebuilder:validation:Enum=Reserved;Intent;Done;Aborted
type HandoffPhase string

// The handoff phases, one per value of the HandoffPhase enum.
const (
	HandoffReserved HandoffPhase = "Reserved"
	HandoffIntent   HandoffPhase = "Intent"
	HandoffDone     HandoffPhase = "Done"
	HandoffAborted  HandoffPhase = "Aborted"
)

// Handoff records the supersede handoff on both requests (ADR-0028 section 4), written in this order: the
// replacement's admission sets from, the hint and phase Reserved; then one write on the predecessor sets to,
// expectedHead, phase Intent and its hint Reserved; after the branch update the replacement records the pull
// request and moves to Done, then the predecessor ends Superseded with HandedOff and moves to Done. A restart
// resumes from what both records and the branch head show. Exactly one of from and to is set, and neither they
// nor the hint change once written.
// +kubebuilder:validation:XValidation:rule="has(self.from) != has(self.to)",message="exactly one of from and to is set"
// +kubebuilder:validation:XValidation:rule="!has(self.from) || self.phase in ['Reserved', 'Done', 'Aborted']",message="the replacement's handoff is Reserved, Done or Aborted"
// +kubebuilder:validation:XValidation:rule="!has(self.to) || (self.phase in ['Intent', 'Done', 'Aborted'] && has(self.expectedHead))",message="the predecessor's handoff is Intent, Done or Aborted, with expectedHead"
// +kubebuilder:validation:XValidation:rule="!has(self.from) || !has(self.expectedHead)",message="expectedHead is recorded on the predecessor only"
// +kubebuilder:validation:XValidation:rule="oldSelf.phase == self.phase || (oldSelf.phase in ['Reserved', 'Intent'] && self.phase in ['Done', 'Aborted'])",message="a handoff moves only from Reserved or Intent to Done or Aborted"
type Handoff struct {
	// from is the predecessor, set on the replacement. It equals spec.replaces.name.
	// +optional
	From Name `json:"from,omitempty"`

	// to is the replacement, set on the predecessor.
	// +optional
	To Name `json:"to,omitempty"`

	// hint is the hint the handoff answers.
	// +required
	Hint HintRef `json:"hint"`

	// expectedHead is the head of the branch the predecessor recorded when it took the intent: the
	// replacement's commit must have it as its parent.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	// +optional
	ExpectedHead string `json:"expectedHead,omitempty"`

	// phase is how far the handoff has come.
	// +required
	Phase HandoffPhase `json:"phase"`
}
