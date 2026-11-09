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

package incidents

import (
	"cmp"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

// Reviewer hints (ADR-0028 section 3): the executor stores a hint from the pull request in the status of the
// request it opened; the analyzer reads it there, never the comment, analyzes the Incident again with it and
// records the hint it handled in the Incident's status.handledHint. The analyzer writes nothing else new.

// ConditionAnalyzed is the Incident condition that carries the outcome of the last analysis (True, with the
// outcome's reason and a fixed message). The analysis writes it with every outcome; the trigger writes it only
// to answer a reviewer hint it refuses without an analysis ([HintAnswer]).
const ConditionAnalyzed = "Analyzed"

// Outcome reasons of the Analyzed condition that the trigger writes, with status.handledHint, for a reviewer
// hint it refuses without running an analysis. The executor reads them (ADR-0028 section 3) to decline the
// hint and tell the reviewer why.
const (
	// OutcomeHintCoolingDown: the hint came within the cooldown since the last analysis of the Incident.
	OutcomeHintCoolingDown = "CoolingDown"
	// OutcomeHintBudgetSpent: the daily analysis budget is spent or set to 0. It is the reason an analysis
	// writes when it cannot reserve its unit of the budget.
	OutcomeHintBudgetSpent = "BudgetSpent"
)

// hintOutcomeMessages are the fixed messages of the Analyzed condition the trigger writes. They hold nothing
// from the cluster or the hint.
var hintOutcomeMessages = map[string]string{
	OutcomeHintCoolingDown: "A reviewer hint came within the cooldown since the last analysis; it was not analyzed",
	OutcomeHintBudgetSpent: "A reviewer hint came while the daily analysis budget is spent; it was not analyzed",
}

// ReviewerHint is a Pending reviewer hint and the request whose status holds it.
type ReviewerHint struct {
	// Request holds the hint in status.hints. A replacement made for the hint replaces it.
	Request *remediationv1alpha1.RemediationRequest
	// Hint is the stored hint, as the executor pinned it.
	Hint *remediationv1alpha1.Hint
}

// Ref is the pin of the hint: its source, comment ID and digest.
func (h ReviewerHint) Ref() remediationv1alpha1.HintRef {
	return HintRefOf(h.Hint)
}

// HintRefOf returns the pin of a stored hint.
func HintRefOf(h *remediationv1alpha1.Hint) remediationv1alpha1.HintRef {
	return remediationv1alpha1.HintRef{Source: h.Source, CommentID: h.CommentID, Digest: h.Digest}
}

// PendingHint returns the reviewer hint an analysis of inc answers next, or nil. requests are the
// RemediationRequests to look in; only those whose incidentRef names inc by name and UID, in phase
// AwaitingApproval with a recorded pull request (the only requests the executor stores hints on and the only
// ones a replacement may take over), count. Of their Pending hints it takes the earliest accepted (then by
// source, comment ID and request name, for a stable choice), so hints are answered one at a time, in order.
//
// The earliest hint is answered once status.handledHint names it outside phase Analyzing: an analysis with it
// wrote its outcome, or the trigger refused it ([HintAnswer]). PendingHint then returns nil until the executor
// moves that hint on (Reserved, Consumed or Declined), so an answered hint never starts a second analysis,
// after a restart too, and a later hint waits behind it. In phase Analyzing a handledHint that names it is the
// running (or lost) analysis's own, and it is returned, so a lost run is retried with the same hint.
func PendingHint(inc *remediationv1alpha1.Incident, requests []remediationv1alpha1.RemediationRequest) *ReviewerHint {
	var found *ReviewerHint
	for i := range requests {
		rr := &requests[i]
		if !holdsHints(inc, rr) {
			continue
		}
		for j := range rr.Status.Hints {
			h := &rr.Status.Hints[j]
			if h.State != remediationv1alpha1.HintPending {
				continue
			}
			if found == nil || earlierHint(rr, h, found.Request, found.Hint) {
				found = &ReviewerHint{Request: rr, Hint: h}
			}
		}
	}
	if found == nil {
		return nil
	}
	if handled := inc.Status.HandledHint; handled != nil && *handled == found.Ref() &&
		inc.Status.Phase != remediationv1alpha1.IncidentAnalyzing {
		return nil
	}
	return found
}

// HintByRef returns the hint pinned by ref while it is still Pending on one of requests that counts for
// [PendingHint] (inc's, in AwaitingApproval with a pull request), or nil. A lost run of inc with a hint is
// retried with only that hint: the one its claim named in status.handledHint.
func HintByRef(inc *remediationv1alpha1.Incident, requests []remediationv1alpha1.RemediationRequest,
	ref remediationv1alpha1.HintRef) *ReviewerHint {
	for i := range requests {
		rr := &requests[i]
		if !holdsHints(inc, rr) {
			continue
		}
		for j := range rr.Status.Hints {
			if h := &rr.Status.Hints[j]; h.State == remediationv1alpha1.HintPending && HintRefOf(h) == ref {
				return &ReviewerHint{Request: rr, Hint: h}
			}
		}
	}
	return nil
}

// ReplacementFor returns the earliest of requests (then the first by name) whose incidentRef names inc by
// name and UID and whose spec.replaces.hint is ref, or nil: a replacement an analysis already created for the
// hint ref.
func ReplacementFor(inc *remediationv1alpha1.Incident, requests []remediationv1alpha1.RemediationRequest,
	ref remediationv1alpha1.HintRef) *remediationv1alpha1.RemediationRequest {
	var found *remediationv1alpha1.RemediationRequest
	for i := range requests {
		rr := &requests[i]
		if rr.Spec.Replaces == nil || rr.Spec.Replaces.Hint != ref || rr.Spec.IncidentRef.Name != inc.Name ||
			types.UID(rr.Spec.IncidentRef.UID) != inc.UID {
			continue
		}
		if found == nil || rr.CreationTimestamp.Before(&found.CreationTimestamp) ||
			(rr.CreationTimestamp.Equal(&found.CreationTimestamp) && rr.Name < found.Name) {
			found = rr
		}
	}
	return found
}

// SettlesWithoutAnalysis reports whether the analysis ends a lost run of inc without a model call: inc is in
// phase Analyzing with status.handledHint set, and either a replacement for that hint exists among requests
// ([ReplacementFor]: it is recorded) or the hint is no longer Pending there ([HintByRef]: the run is
// released). Such a run takes no unit of the budget, so the trigger marks it due (AnalysisTimedOut) with the
// budget spent or 0 too. requests must be the requests the analysis takes as its own.
func SettlesWithoutAnalysis(inc *remediationv1alpha1.Incident, requests []remediationv1alpha1.RemediationRequest) bool {
	handled := inc.Status.HandledHint
	if inc.Status.Phase != remediationv1alpha1.IncidentAnalyzing || handled == nil {
		return false
	}
	return ReplacementFor(inc, requests, *handled) != nil || HintByRef(inc, requests, *handled) == nil
}

// holdsHints reports whether rr may hold a hint an analysis of inc answers: its incidentRef names inc by
// name and UID, and it is in AwaitingApproval with a recorded pull request.
func holdsHints(inc *remediationv1alpha1.Incident, rr *remediationv1alpha1.RemediationRequest) bool {
	return rr.Spec.IncidentRef.Name == inc.Name && types.UID(rr.Spec.IncidentRef.UID) == inc.UID &&
		rr.Status.Phase == remediationv1alpha1.PhaseAwaitingApproval && rr.Status.PullRequest != nil
}

// earlierHint orders hints by acceptedAt, then source, comment ID and the name of the request holding them.
func earlierHint(ar *remediationv1alpha1.RemediationRequest, a *remediationv1alpha1.Hint,
	br *remediationv1alpha1.RemediationRequest, b *remediationv1alpha1.Hint) bool {
	if !a.AcceptedAt.Equal(&b.AcceptedAt) {
		return a.AcceptedAt.Before(&b.AcceptedAt)
	}
	if c := cmp.Compare(a.Source, b.Source); c != 0 {
		return c < 0
	}
	if a.CommentID != b.CommentID {
		return a.CommentID < b.CommentID
	}
	return ar.Name < br.Name
}

// HintAnswer returns the pin of the hint the trigger refuses without an analysis, and the outcome reason of
// the refusal, when the decision (due, reason) of [Due] on inc is one: inc is Proposed, it has a hint
// ([PendingHint]) and is not due because of the cooldown (OutcomeHintCoolingDown) or a budget that is used up
// or 0 (OutcomeHintBudgetSpent). A refusal is an answer to the reviewer, not a queue (ADR-0028 section 2): the
// hint does not wait for the cooldown or the next day. Any other not-due reason (a budget that cannot be read,
// for one) answers nothing, and the hint waits for the next pass.
func HintAnswer(inc *remediationv1alpha1.Incident, requests []remediationv1alpha1.RemediationRequest, due bool,
	reason string) (*remediationv1alpha1.HintRef, string) {
	if due || inc.Status.Phase != remediationv1alpha1.IncidentProposed {
		return nil, ""
	}
	var outcome string
	switch reason {
	case ReasonCoolingDown:
		outcome = OutcomeHintCoolingDown
	case ReasonDailyBudgetReached, ReasonAnalysisDisabled:
		outcome = OutcomeHintBudgetSpent
	default:
		return nil, ""
	}
	h := PendingHint(inc, requests)
	if h == nil {
		return nil, ""
	}
	ref := h.Ref()
	return &ref, outcome
}

// answerHint writes the refusal of a hint into inc's status: status.handledHint and the Analyzed condition
// with the outcome reason, at now. The caller writes it in the update that sets the AnalysisDue condition.
func answerHint(inc *remediationv1alpha1.Incident, ref remediationv1alpha1.HintRef, outcome string, now time.Time) {
	inc.Status.HandledHint = &ref
	meta.SetStatusCondition(&inc.Status.Conditions, metav1.Condition{
		Type: ConditionAnalyzed, Status: metav1.ConditionTrue, Reason: outcome,
		Message: hintOutcomeMessages[outcome], ObservedGeneration: inc.Generation,
		LastTransitionTime: metav1.NewTime(now.UTC().Truncate(time.Second)),
	})
}
