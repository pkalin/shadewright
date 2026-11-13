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

package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/incidents/aggregation"
)

// ReasonResolvedBeforeRollout: Cleared True on a Succeeded request whose Incident's count did not move from the
// merge to the rollout over at least resolvedBeforeSpan (ADR-0030 section 4). The symptom had already stopped
// when the change arrived, so the change is not shown to be the cause. It is an attribution for people: nothing
// branches on it, Verified keeps NoRecurrence and the phase is Succeeded either way.
const ReasonResolvedBeforeRollout = "ResolvedBeforeRollout"

// resolvedBeforeSpan is the shortest span, on the executor's clock, between the two readings of the Incident's
// count for equal counts to say anything: the effective stableFor plus twice the analyzer's aggregation window.
// The analyzer writes the count at most once per window, and a crash loop backs off for up to one more window,
// so a shorter span may only mean that no write fell in it.
func resolvedBeforeSpan(stableFor time.Duration) time.Duration {
	return stableFor + 2*aggregation.DefaultWindow
}

// incidentCountSide says which of the two readings is taken: at the merge the read time is taken after the read,
// at the applied time before it, so the span between the two recorded times is never longer than the span over
// which the two counts were really read.
type incidentCountSide int

const (
	countAtMerge incidentCountSide = iota
	countAtApplied
)

// readIncidentCount reads the request's Incident (spec.incidentRef.name in the request's namespace) with the
// executor's get, through the API reader and within the Incident read's own deadline, and returns its count.
// It returns nil when no reading can be recorded: no name, a failed read, an Incident without a UID, or a count
// outside what the schema accepts, which would make the status write that carries it fail. Each of the last
// three is logged and never retried: the reading stays absent and nothing waits for it. A UID other than
// spec.incidentRef.uid is recorded as read; the decision refuses it.
func (r *RemediationRequestReconciler) readIncidentCount(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest, side incidentCountSide) *remediationv1alpha1.IncidentCountReading {
	ref := rr.Spec.IncidentRef
	if ref.Name == "" {
		return nil
	}
	before := r.now()
	readCtx, cancel := context.WithTimeout(ctx, incidentReadTimeout(ctx, r.IncidentReadTimeout))
	defer cancel()
	inc := &remediationv1alpha1.Incident{}
	if err := r.reader().Get(readCtx, client.ObjectKey{Namespace: rr.Namespace, Name: ref.Name}, inc); err != nil {
		logf.FromContext(ctx).Info("Could not read the Incident of RemediationRequest to record its count; the "+
			"reading stays absent", "remediationRequest", client.ObjectKeyFromObject(rr), "incident", ref.Name,
			"error", err.Error())
		return nil
	}
	if inc.UID == "" {
		logf.FromContext(ctx).Info("The Incident of RemediationRequest has no UID; the reading of its count stays "+
			"absent", "remediationRequest", client.ObjectKeyFromObject(rr), "incident", ref.Name)
		return nil
	}
	if inc.Status.Count < 0 || inc.Status.Count > remediationv1alpha1.MaxIncidentCount {
		logf.FromContext(ctx).Info("The count of the Incident of RemediationRequest is outside the accepted range; "+
			"the reading stays absent", "remediationRequest", client.ObjectKeyFromObject(rr), "incident", ref.Name,
			"count", inc.Status.Count)
		return nil
	}
	at := before.Truncate(time.Second)
	if side == countAtMerge {
		after := r.now()
		at = after.Truncate(time.Second)
		if !at.Equal(after) {
			at = at.Add(time.Second)
		}
	}
	return &remediationv1alpha1.IncidentCountReading{UID: string(inc.UID), Count: inc.Status.Count,
		ReadAt: metav1.NewTime(at)}
}

// recordCountAtMerge records the reading at the merge in rr for the status write that sets Merged with the merge
// receipt, the only write that takes it (a retry of that write after a conflict reads again). It runs only for a
// request with a merge receipt, without the reading and before the applied time is recorded; a reading already
// recorded is never replaced. A failed read leaves the reading absent for good: no later reconcile fills it in.
func (r *RemediationRequestReconciler) recordCountAtMerge(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) {
	if rr.Status.MergeReceipt == nil || (rr.Status.PullRequest != nil && rr.Status.PullRequest.AppliedObservedAt != nil) {
		return
	}
	if v := rr.Status.Verification; v != nil && v.IncidentCount != nil && v.IncidentCount.AtMerge != nil {
		return
	}
	if reading := r.readIncidentCount(ctx, rr, countAtMerge); reading != nil {
		incidentCount(rr).AtMerge = reading
	}
}

// recordCountAtApplied records the reading at the applied time in rr, for the status write that sets
// appliedObservedAt, and only with a reading at the merge. It is called only where that write is prepared, so
// it is never filled in later.
func (r *RemediationRequestReconciler) recordCountAtApplied(ctx context.Context,
	rr *remediationv1alpha1.RemediationRequest) {
	v := rr.Status.Verification
	if v == nil || v.IncidentCount == nil || v.IncidentCount.AtMerge == nil || v.IncidentCount.AtApplied != nil {
		return
	}
	if reading := r.readIncidentCount(ctx, rr, countAtApplied); reading != nil {
		v.IncidentCount.AtApplied = reading
	}
}

// incidentCount returns the readings in rr's status, created when missing.
func incidentCount(rr *remediationv1alpha1.RemediationRequest) *remediationv1alpha1.IncidentCountReadings {
	if rr.Status.Verification == nil {
		rr.Status.Verification = &remediationv1alpha1.VerificationWindow{}
	}
	if rr.Status.Verification.IncidentCount == nil {
		rr.Status.Verification.IncidentCount = &remediationv1alpha1.IncidentCountReadings{}
	}
	return rr.Status.Verification.IncidentCount
}

// resolvedBeforeRollout reports whether the recorded readings show that the analyzer counted no new occurrence
// of the Incident from the merge to the rollout, over at least resolvedBeforeSpan(stableFor): both readings
// exist, both are of the Incident spec.incidentRef.uid names, the counts are equal and below the saturation
// value, and the span between their read times, two times of the executor's clock, is long enough. With the
// span it returns it for the message. Anything else is false, and the reason stays NoRecurrence.
func resolvedBeforeRollout(rr *remediationv1alpha1.RemediationRequest, stableFor time.Duration) (time.Duration, bool) {
	v := rr.Status.Verification
	if v == nil || v.IncidentCount == nil {
		return 0, false
	}
	m, a := v.IncidentCount.AtMerge, v.IncidentCount.AtApplied
	uid := types.UID(rr.Spec.IncidentRef.UID)
	switch {
	case m == nil, a == nil, uid == "", types.UID(m.UID) != uid, types.UID(a.UID) != uid, m.Count != a.Count,
		m.Count >= remediationv1alpha1.MaxIncidentCount:
		return 0, false
	}
	span := a.ReadAt.Sub(m.ReadAt.Time)
	return span, span >= resolvedBeforeSpan(stableFor)
}

// clearedReason returns the reason and message of Cleared True for a request about to succeed with message.
func clearedReason(rr *remediationv1alpha1.RemediationRequest, stableFor time.Duration, message string) (
	string, string) {
	span, ok := resolvedBeforeRollout(rr, stableFor)
	if !ok {
		return ReasonNoRecurrence, message
	}
	return ReasonResolvedBeforeRollout, fmt.Sprintf("%s. The Incident's count did not change from the merge to "+
		"the rollout, over %s by the executor's clock (at least stableFor plus two analyzer windows, %s): the "+
		"symptom had already stopped when the change arrived, so this change is not shown to be the cause",
		message, span, resolvedBeforeSpan(stableFor))
}
