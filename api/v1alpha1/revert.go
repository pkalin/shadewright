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
	"crypto/sha256"
	"encoding/hex"

	"k8s.io/apimachinery/pkg/api/equality"
)

// Reasons of a revert (ADR-0030 section 8).
const (
	// ReasonInvalidRevert: a request with spec.reverts was Denied before the policy check because the link does
	// not hold: the named request is missing, has another UID, did not end Failed with Verified False, is a
	// revert itself, names another Incident, target, action type or container, or this request is not exactly
	// its inverse or not named RevertName of its UID. Such a request is never treated as a revert.
	ReasonInvalidRevert = "InvalidRevert"
	// ReasonRevertApplied: a revert is never checked for clearing (Cleared Unknown), and it ends Inconclusive
	// with this reason once rolled out, configured and available held for stableFor (Verified Unknown).
	ReasonRevertApplied = "RevertApplied"
)

// MaxRevertsProposed is the most failed requests an Incident's status.revertsProposed records. The revert
// proposer creates no revert for an Incident whose list is full, so no entry is ever dropped.
const MaxRevertsProposed = 20

// ConditionRevertsStopped is the Incident condition the revert proposer sets (True) when status.revertsProposed
// is full: it proposes no more reverts for that Incident, and a person decides on its other failed requests.
const ConditionRevertsStopped = "RevertsStopped"

// ReasonRevertListFull is the reason of ConditionRevertsStopped: status.revertsProposed holds
// MaxRevertsProposed entries.
const ReasonRevertListFull = "RevertListFull"

// RevertState is the state of an entry of an Incident's status.revertsProposed.
// +kubebuilder:validation:Enum=Pending;Created;Abandoned
type RevertState string

const (
	// RevertPending: the proposer decided that the request gets no second revert, and a create is still coming.
	// Every create, the first or a retried one, runs the same checks first; the entry then moves to Created or,
	// when a check says no, to Abandoned.
	RevertPending RevertState = "Pending"
	// RevertCreated: the revert exists or existed. Nothing more is done for the request, also after the revert
	// was deleted.
	RevertCreated RevertState = "Created"
	// RevertAbandoned: no revert was made and none will be: a check before the create said no, the revert's name
	// is taken by another request, or the create was refused as invalid. The entry's reason says which, and a
	// Warning event RevertAbandoned says it too.
	RevertAbandoned RevertState = "Abandoned"
)

// RevertReason is why an entry of an Incident's status.revertsProposed is Abandoned.
// +kubebuilder:validation:Enum=TargetGone;TargetKindUnsupported;OutsideAgeBound;IncidentDeleting;RequestGone;NotRevertible;NameTaken;CreateInvalid
type RevertReason string

const (
	// RevertReasonTargetGone: the failed request's target is not found, was replaced by another object of that
	// name (another UID) or is being deleted.
	RevertReasonTargetGone RevertReason = "TargetGone"
	// RevertReasonTargetKindUnsupported: the API server no longer serves the target's kind (KEDA uninstalled, for
	// example), or the action cannot target that kind.
	RevertReasonTargetKindUnsupported RevertReason = "TargetKindUnsupported"
	// RevertReasonOutsideAgeBound: the failed request's Verified condition moved more than 24 hours ago (or is
	// too far ahead of the analyzer's clock, or has no time) before the revert could be created. A target the
	// analyzer was refused access to (Forbidden) is retried until then and ends here.
	RevertReasonOutsideAgeBound RevertReason = "OutsideAgeBound"
	// RevertReasonIncidentDeleting: the Incident is being deleted.
	RevertReasonIncidentDeleting RevertReason = "IncidentDeleting"
	// RevertReasonRequestGone: the failed request was deleted, is being deleted, or its name now belongs to
	// another request (another UID).
	RevertReasonRequestGone RevertReason = "RequestGone"
	// RevertReasonNotRevertible: the failed request is there with its UID, but no longer ended Failed with
	// Verified False, or its action can no longer be inverted.
	RevertReasonNotRevertible RevertReason = "NotRevertible"
	// RevertReasonNameTaken: a request with the revert's name exists and does not revert this request; it is
	// left as it is.
	RevertReasonNameTaken RevertReason = "NameTaken"
	// RevertReasonCreateInvalid: the API server refused the revert as invalid. The analyzer's log has the error.
	RevertReasonCreateInvalid RevertReason = "CreateInvalid"
)

// ProposedRevert is an entry of an Incident's status.revertsProposed: a failed request, by name and UID, and
// how far the proposer got with its revert.
// +kubebuilder:validation:XValidation:rule="has(self.reason) == (self.state == 'Abandoned')",message="reason is set exactly when state is Abandoned"
type ProposedRevert struct {
	RequestRef `json:",inline"`

	// state is Pending from the decision until the revert exists, then Created, or Abandoned when no revert
	// will be made. Created and Abandoned are final. A new entry is always Pending.
	// +required
	State RevertState `json:"state"`

	// reason is why the entry is Abandoned. It is set with that state, only with it, and never changes.
	// +optional
	Reason RevertReason `json:"reason,omitempty"`
}

// RevertName is the name of the revert of the request with UID failed: "revert-" and, in hex, the first 12
// bytes of the SHA-256 of "revert/" and that UID. The name derives from the failed request only, so a retried or
// repeated create finds the revert already made and a failed request has at most one revert at a time (invariant
// 5). The name does not stop a new revert with the same name after one was deleted: the analyzer's revert
// proposer records each failed request it made a revert for in the Incident's status.revertsProposed and never
// makes a second one.
func RevertName(failed UID) Name {
	h := sha256.Sum256([]byte("revert/" + string(failed)))
	return Name("revert-" + hex.EncodeToString(h[:12]))
}

// IsRevert reports whether rr is a revert: its spec.reverts is set.
func IsRevert(rr *RemediationRequest) bool {
	return rr != nil && rr.Spec.Reverts != nil
}

// InverseAction returns the action that undoes a exactly: the same action type and, where the action names one,
// the same container, with previous and desired swapped, and no other member set. ok is false when a cannot be
// inverted: an action type this version does not know, its member missing, or a value missing, not positive or
// equal on both sides. The revert proposer builds a revert's action with it and the executor's link check
// compares against it ([IsInverseAction]), so the two cannot drift.
func InverseAction(a Action) (inverse Action, ok bool) {
	inverse = Action{Type: a.Type}
	switch a.Type {
	case ActionSetContainerMemoryLimit:
		m := a.SetContainerMemoryLimit
		if m == nil || m.Container == "" || m.Previous.Sign() <= 0 || m.Desired.Sign() <= 0 ||
			m.Previous.Cmp(m.Desired) == 0 {
			return Action{}, false
		}
		inverse.SetContainerMemoryLimit = &SetContainerMemoryLimit{
			Container: m.Container, Previous: m.Desired.DeepCopy(), Desired: m.Previous.DeepCopy(),
		}
	case ActionAdjustKedaMaxReplicas:
		k := a.AdjustKedaMaxReplicas
		if k == nil || k.PreviousValue <= 0 || k.DesiredValue <= 0 || k.PreviousValue == k.DesiredValue {
			return Action{}, false
		}
		inverse.AdjustKedaMaxReplicas = &AdjustKedaMaxReplicas{PreviousValue: k.DesiredValue, DesiredValue: k.PreviousValue}
	default:
		return Action{}, false
	}
	return inverse, true
}

// IsInverseAction reports whether b undoes a exactly: b equals [InverseAction] of a, and a can be inverted.
// Quantities compare by value, so 1Gi and 1024Mi are the same limit.
func IsInverseAction(a, b Action) bool {
	want, ok := InverseAction(a)
	return ok && equality.Semantic.DeepEqual(want, b)
}
