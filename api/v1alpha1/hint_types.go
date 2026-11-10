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

// Reviewer hints and the supersede handoff (ADR-0028). The executor writes status.hints, status.refusedHints
// and status.commentCursor; the analyzer reads stored hints and writes spec.replaces, spec.chainRoot and the
// Incident's status.handledHint. The executor writes status.handoff on both requests of a handoff.

// MaxHints is the most hints one supersede chain can consume, and so the most Consumed or Reserved entries of
// a request's status.hints: the consumed hints of a chain are carried into the replacement's status (ADR-0028
// section 6).
const MaxHints = 3

// MaxStoredHints is the most entries a request's status.hints holds in all, of any state. A stored hint is
// never removed, so Declined ones stay as the record; once the list is full no further hint is stored.
const MaxStoredHints = 10

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
	// ReasonHintsTurnedOff: the executor runs with reviewer hints off (its flag --reviewer-hints unset), so it
	// takes nothing a hint asked for (ADR-0028, amendment of 2026-11-10). It is the reason of a Pending or
	// Reserved hint the executor declines, of a replacement Denied at admission and of a replacement admitted
	// before that ends Cancelled without the handoff.
	ReasonHintsTurnedOff = "HintsTurnedOff"
)

// ReasonAnotherRequestMerged is the PlanFresh reason of a request that ended Superseded because another request
// of the same Incident and target was merged after it was created, while that merge's change is still in effect,
// before it took the target lock or opened a pull request (#556). The closure rules of the Incident follow the
// merged request instead of this one.
const ReasonAnotherRequestMerged = "AnotherRequestMerged"

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
// +kubebuilder:validation:XValidation:rule="!(oldSelf.state in ['Consumed', 'Declined']) || (has(self.reason) == has(oldSelf.reason) && (!has(self.reason) || self.reason == oldSelf.reason))",message="the reason of a Consumed or Declined hint never changes"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.editedAfterAccept) || !oldSelf.editedAfterAccept || (has(self.editedAfterAccept) && self.editedAfterAccept)",message="editedAfterAccept is never unset"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.outcome) || (has(self.outcome) && self.outcome == oldSelf.outcome)",message="the outcome of a hint never changes once set"
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

	// reason says why a Declined hint was declined: the analysis's outcome (for example BudgetSpent or
	// NoProposal), NoAnswer when none came within the executor's bound, or the end of the request
	// (PullRequestMerged, PullRequestClosed, RequestEnded), or the end of its replacement without the handoff:
	// ReplacementDenied when the replacement was Denied, HandoffAborted when it ended otherwise, or
	// HintsTurnedOff when the executor runs with reviewer hints off, which declines every Pending or Reserved
	// hint. HintDeclineReasons lists them.
	// +kubebuilder:validation:Enum=NoModelConfigured;BudgetSpent;BudgetUnavailable;NoProposal;MissingData;NoEvidence;TargetNotSupported;PreviousValueUnknown;NoChange;DirectionReversed;NoPolicyForAction;ActionOutOfBounds;InvalidOutput;EscalationNotConfigured;StepLimit;TokenLimit;DeadlineExceeded;ModelUnavailable;ModelNotFound;ModelForbidden;ModelError;UnexpectedModel;EndpointNotAllowed;PlanUnchanged;HintNotPending;RequestNotCreated;CoolingDown;NoAnswer;PullRequestMerged;PullRequestClosed;RequestEnded;ReplacementDenied;HandoffAborted;HintsTurnedOff
	// +optional
	Reason string `json:"reason,omitempty"`

	// editedAfterAccept is true once the executor saw the comment edited after it accepted the hint. The
	// stored text stays as it was; the edit is recorded once, here, whatever the number of edits.
	// +optional
	EditedAfterAccept bool `json:"editedAfterAccept,omitempty"`

	// outcome is the analysis's outcome that answered the hint with a replacement (Proposed or RunbookMatched),
	// recorded when the executor first read it in the Incident. The hint is answered from then on, whatever
	// the Incident handles later: it waits for the handoff, and the answer timeout no longer applies. Once set
	// it never changes.
	// +kubebuilder:validation:Enum=Proposed;RunbookMatched
	// +optional
	Outcome string `json:"outcome,omitempty"`
}

// HintDeclineReasons are the values of Hint.reason, the same as its enum: the analysis's outcomes that create
// no replacement, and the executor's own.
var HintDeclineReasons = []string{"NoModelConfigured", "BudgetSpent", "BudgetUnavailable", "NoProposal",
	"MissingData", "NoEvidence", "TargetNotSupported", "PreviousValueUnknown", "NoChange", "DirectionReversed",
	"NoPolicyForAction", "ActionOutOfBounds", "InvalidOutput", "EscalationNotConfigured", "StepLimit",
	"TokenLimit", "DeadlineExceeded", "ModelUnavailable", "ModelNotFound", "ModelForbidden", "ModelError",
	"UnexpectedModel", "EndpointNotAllowed", "PlanUnchanged", "HintNotPending", "RequestNotCreated",
	"CoolingDown", "NoAnswer", "PullRequestMerged", "PullRequestClosed", "RequestEnded", "ReplacementDenied",
	"HandoffAborted", "HintsTurnedOff"}

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

// HandoffPhase is how far one side of a handoff has come. Incoming, on the replacement: Reserved (it took the
// target lock and named its predecessor), then Done (it holds the pull request) or Aborted. Outgoing, on the
// predecessor: Intent (it expects its replacement's commit on top of expectedHead), then Done (it ended
// Superseded with HandedOff) or Aborted. Each side starts in its first phase and moves only once; an outgoing
// side that was Aborted may be replaced by the Intent of a later replacement for another hint.
// +kubebuilder:validation:Enum=Reserved;Intent;Done;Aborted
type HandoffPhase string

// The handoff phases, one per value of the HandoffPhase enum.
const (
	HandoffReserved HandoffPhase = "Reserved"
	HandoffIntent   HandoffPhase = "Intent"
	HandoffDone     HandoffPhase = "Done"
	HandoffAborted  HandoffPhase = "Aborted"
)

// HandoffStep is the record of a step after a handoff that the Git host either did or refused for good.
// +kubebuilder:validation:Enum=Done;Refused
type HandoffStep string

// The records of a step after a handoff.
const (
	// HandoffStepDone: the Git host did the step.
	HandoffStepDone HandoffStep = "Done"
	// HandoffStepRefused: the Git host refused the step for good (a permission the App lacks, a rule, an invalid
	// request); it is not asked again.
	HandoffStepRefused HandoffStep = "Refused"
)

// ReviewCheckState is how far the check run shadewright/review of a head has come (ADR-0028 section 5).
// InProgress: created, in progress, waiting for an approval of the head by someone who wrote no hint in the
// chain. Passed: completed with conclusion success after such an approval. Failed: completed with conclusion
// failure because the merge gate could not read every review of the pull request, so it fails closed on this
// head and a person decides the merge. Refused: the Git host refused to create or complete it for good, so the
// required check does not pass on this head and the pull request cannot merge until a person acts.
// +kubebuilder:validation:Enum=InProgress;Passed;Failed;Refused
type ReviewCheckState string

// The states of the check run shadewright/review.
const (
	ReviewCheckInProgress ReviewCheckState = "InProgress"
	ReviewCheckPassed     ReviewCheckState = "Passed"
	ReviewCheckFailed     ReviewCheckState = "Failed"
	ReviewCheckRefused    ReviewCheckState = "Refused"
)

// ReviewCheck is the check run shadewright/review on one head of the pull request (ADR-0028 section 5),
// pinned to that head: a state recorded for one head never counts for another. Every head the executor pushes
// or adopts starts without one, and the executor creates the check in progress on it. On one head the state
// moves only from InProgress to Passed, Failed or Refused.
// +kubebuilder:validation:XValidation:rule="self.head != oldSelf.head || self.state == oldSelf.state || (oldSelf.state == 'InProgress' && self.state in ['Passed', 'Failed', 'Refused'])",message="on one head the check run moves only from InProgress to Passed, Failed or Refused"
type ReviewCheck struct {
	// head is the commit the check run is on, always the pull request's recorded head (headSHA).
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	// +required
	Head string `json:"head"`

	// state is how far the check run on head has come.
	// +required
	State ReviewCheckState `json:"state"`
}

// IncomingHandoff is the replacement's side of a handoff: the predecessor it takes over and the hint it
// answers, equal to spec.replaces. Once written, only its phase and the record of the rewrite after it change.
// The check run shadewright/review is not recorded here: it belongs to each head of the pull request, also
// one no handoff pushed (PullRequestStatus.reviewCheck).
// +kubebuilder:validation:XValidation:rule="self.phase in ['Reserved', 'Done', 'Aborted']",message="an incoming handoff is Reserved, Done or Aborted"
// +kubebuilder:validation:XValidation:rule="self.name == oldSelf.name && self.uid == oldSelf.uid && self.hint == oldSelf.hint",message="an incoming handoff is pinned: only its phase changes"
// +kubebuilder:validation:XValidation:rule="!has(self.pullRequestUpdate) || self.phase == 'Done'",message="the rewrite after a handoff is recorded only once it is Done"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.pullRequestUpdate) || (has(self.pullRequestUpdate) && self.pullRequestUpdate == oldSelf.pullRequestUpdate)",message="the record of the pull request's rewrite never changes once set"
// +kubebuilder:validation:XValidation:rule="oldSelf.phase == self.phase || (oldSelf.phase == 'Reserved' && self.phase in ['Done', 'Aborted'])",message="an incoming handoff moves only from Reserved to Done or Aborted"
type IncomingHandoff struct {
	// name of the predecessor.
	// +required
	Name Name `json:"name"`

	// uid of the predecessor.
	// +required
	UID UID `json:"uid"`

	// hint is the predecessor's hint the replacement answers.
	// +required
	Hint HintRef `json:"hint"`

	// phase is how far this side has come.
	// +required
	Phase HandoffPhase `json:"phase"`

	// pullRequestUpdate records the rewrite of the pull request's title and body for this request's plan, with
	// its marker (ADR-0028 section 4, step 6): Done once the Git host did it, Refused when it refused it for
	// good. Until it is Done the body carries the predecessor's marker, or after refused rewrites further up the
	// chain an earlier request's, which this request accepts as its own while its chain reaches that request.
	// +optional
	PullRequestUpdate HandoffStep `json:"pullRequestUpdate,omitempty"`
}

// OutgoingHandoff is the predecessor's side of a handoff: the replacement that takes it over, the hint of this
// request it answers (which must be in status.hints, in the state its phase implies: Reserved while Intent,
// Consumed once Done, Declined once Aborted) and the branch head the replacement's commit must have as its
// parent. Only its phase changes once written, except that an Aborted one is replaced by the Intent of a later
// replacement, for another hint of this request.
// +kubebuilder:validation:XValidation:rule="self.phase in ['Intent', 'Done', 'Aborted']",message="an outgoing handoff is Intent, Done or Aborted"
// +kubebuilder:validation:XValidation:rule="(oldSelf.phase == 'Aborted' && self.phase == 'Intent') || (self.name == oldSelf.name && self.uid == oldSelf.uid && self.hint == oldSelf.hint && self.expectedHead == oldSelf.expectedHead)",message="an outgoing handoff is pinned: only its phase changes, or an Aborted one is replaced"
// +kubebuilder:validation:XValidation:rule="oldSelf.phase == self.phase || (oldSelf.phase == 'Intent' && self.phase in ['Done', 'Aborted']) || (oldSelf.phase == 'Aborted' && self.phase == 'Intent' && self.uid != oldSelf.uid && (self.hint.source != oldSelf.hint.source || self.hint.commentID != oldSelf.hint.commentID))",message="an outgoing handoff moves only from Intent to Done or Aborted, or from Aborted to the Intent of another replacement for another hint"
type OutgoingHandoff struct {
	// name of the replacement.
	// +required
	Name Name `json:"name"`

	// uid of the replacement, so a request recreated under the same name is never taken for it.
	// +required
	UID UID `json:"uid"`

	// hint is this request's hint the replacement answers.
	// +required
	Hint HintRef `json:"hint"`

	// expectedHead is the head of the branch when the intent was recorded: the replacement's commit must have
	// it as its parent.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	// +required
	ExpectedHead string `json:"expectedHead"`

	// phase is how far this side has come.
	// +required
	Phase HandoffPhase `json:"phase"`
}

// Handoff records the supersede handoff (ADR-0028 section 4), written in this order: the replacement's
// admission sets from (phase Reserved); one write on the predecessor sets to (phase Intent) and its hint
// Reserved; after the branch update the replacement's from moves to Done, then the predecessor ends Superseded
// with HandedOff, its to Done and its hint Consumed. A request in the middle of a chain has both: from for the
// handoff that gave it the pull request, to for the one that passes it on. Each side starts in its first phase
// and is never removed: from changes only its phase and, once Done, the record of the rewrite
// (pullRequestUpdate); to changes only its phase, except that an Aborted one is replaced by the Intent of a
// later replacement.
// +kubebuilder:validation:XValidation:rule="has(self.from) || has(self.to)",message="a handoff has from, to or both"
type Handoff struct {
	// from is the incoming side, set on a replacement.
	// +optional
	From *IncomingHandoff `json:"from,omitempty"`

	// to is the outgoing side, set on a predecessor.
	// +optional
	To *OutgoingHandoff `json:"to,omitempty"`
}

// MaxRefusedHints is the most refusals a request's status.refusedHints keeps; the oldest goes first.
const MaxRefusedHints = 10

// HintRefusal says why the executor stored no hint for a writer's comment that carried the command (ADR-0028
// section 2); a command of an author without write access is never recorded, so it cannot push a record out.
// CapReached: MaxHints hints of the request are Consumed or Reserved already, which count for its whole
// supersede chain. TooLong: the hint is over MaxHintTextBytes after redaction, or the body was cut before the
// end of its first line. NotRedactable: the redacted text still holds something the redactor would change.
// Empty: the command has no text. EditedAfterSeen: a comment the executor had read already was edited to carry
// the command. An edit of a stored hint's comment is recorded on the hint (editedAfterAccept), not here.
// +kubebuilder:validation:Enum=CapReached;TooLong;NotRedactable;Empty;EditedAfterSeen
type HintRefusal string

// The refusals, one per value of the HintRefusal enum.
const (
	HintRefusalCapReached      HintRefusal = "CapReached"
	HintRefusalTooLong         HintRefusal = "TooLong"
	HintRefusalNotRedactable   HintRefusal = "NotRedactable"
	HintRefusalEmpty           HintRefusal = "Empty"
	HintRefusalEditedAfterSeen HintRefusal = "EditedAfterSeen"
)

// RefusedHint records a comment that carried the command and was refused, so the refusal is made once and a
// reader sees why no hint was stored. It holds no text of the comment.
type RefusedHint struct {
	// source is the system the comment is in.
	// +required
	Source HintSource `json:"source"`

	// commentID is the comment's ID in the source.
	// +kubebuilder:validation:Minimum=1
	// +required
	CommentID int64 `json:"commentID"`

	// author wrote the comment, as the source's API reported it.
	// +required
	Author HintAuthor `json:"author"`

	// reason says why no hint was stored.
	// +required
	Reason HintRefusal `json:"reason"`

	// refusedAt is when the executor refused it.
	// +required
	RefusedAt metav1.Time `json:"refusedAt"`
}

// CommentCursor is where the executor's next look at the conversation comments of the request's pull request
// starts (ADR-0028 section 3): the durable watermark, so a restart neither reads a comment again as new nor
// skips one. It is written in the status update that stores what the look found, just after the last comment
// the look judged. Once set it is kept, and since never decreases (CEL on RemediationRequest). Its fields
// other than seen mean something only to the code host's adapter.
// +kubebuilder:validation:XValidation:rule="!has(self.etag) || ((!has(self.page) || self.page == 0) && (!has(self.afterID) || self.afterID == 0))",message="etag is kept only on the first page of a new sweep"
// +kubebuilder:validation:XValidation:rule="has(self.evictedAt) == (has(self.evictedID) && self.evictedID > 0)",message="evictedAt and evictedID are set together"
type CommentCursor struct {
	// since is the oldest update time the current sweep of the comments asks for. Every comment created at or
	// before it was returned by an earlier look, so a command in one counts as an edit (EditedAfterSeen). That
	// is an assumption about the code host: the GitHub adapter sets since a minute before the host's time when
	// the previous sweep began, assuming a new comment shows up in the list within a minute. One that shows up
	// later is refused as EditedAfterSeen and has to be posted again: it fails closed.
	// +optional
	Since *metav1.Time `json:"since,omitempty"`

	// page is the page the next look starts on; 0 starts a new sweep.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100000
	// +optional
	Page int32 `json:"page,omitempty"`

	// afterID: the comments of the current sweep with an ID up to this one were returned already.
	// +kubebuilder:validation:Minimum=0
	// +optional
	AfterID int64 `json:"afterID,omitempty"`

	// sweepStart is the code host's time when the current sweep began.
	// +optional
	SweepStart *metav1.Time `json:"sweepStart,omitempty"`

	// etag is the entity tag of the last answer to the first page of a sweep that held no comment, sent as
	// If-None-Match, so an unchanged answer is "not modified" and costs no read.
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^(W/)?"[!#-~]*"$`
	// +optional
	ETag string `json:"etag,omitempty"`

	// seen are the commands the executor judged whose update time is at or after since, so a look that returns
	// them again unchanged (the overlap of two sweeps) does not judge them again: at most MaxSeenComments, the
	// newest. A comment returned with another update time was edited and is judged again.
	// +listType=map
	// +listMapKey=commentID
	// +kubebuilder:validation:MaxItems=50
	// +optional
	Seen []SeenComment `json:"seen,omitempty"`

	// evictedAt is the newest update time of the commands pushed out of seen by its bound; with evictedID it is
	// the watermark of those commands. A command with an ID up to evictedID and an update time up to evictedAt
	// counts as judged, so an evicted one returned again unchanged is not judged again, while one edited after
	// its judgement has a later update time and is. The watermark only grows, and is dropped once since passes
	// evictedAt. A command edited during a sweep and passed by it may count as judged too when more than
	// MaxSeenComments newer ones were judged first: it is then not judged, never accepted twice. Those newer
	// ones include commands from authors without write access, so anyone who can comment can cause this on
	// purpose; it fails closed (the edited command is missed, its author posts a new comment).
	// +optional
	EvictedAt *metav1.Time `json:"evictedAt,omitempty"`

	// evictedID is the highest ID of the commands pushed out of seen; set together with evictedAt.
	// +kubebuilder:validation:Minimum=0
	// +optional
	EvictedID int64 `json:"evictedID,omitempty"`
}

// MaxSeenComments is the most entries status.commentCursor.seen keeps.
const MaxSeenComments = 50

// SeenComment is a comment the executor judged, with the update time it had then.
type SeenComment struct {
	// commentID is the comment's ID in the code host.
	// +kubebuilder:validation:Minimum=1
	// +required
	CommentID int64 `json:"commentID"`

	// updatedAt is the comment's update time when it was judged.
	// +required
	UpdatedAt metav1.Time `json:"updatedAt"`
}
