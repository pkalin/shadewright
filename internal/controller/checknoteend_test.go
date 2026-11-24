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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
)

// A write that records the merge of a stopped request ends condition ReviewCheckNotFailed (settleRecorded), with the
// lock released in it or kept for a verification. Either way no look tries the failed review check again, so the end
// is said in the log at Info, as a release by a look says it (#925).
func TestSettleRecordedLogsTheEndOfReviewCheckNotFailed(t *testing.T) {
	for _, held := range []bool{false, true} {
		rr := &api.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "rr"}}
		rr.Status.Phase = api.PhaseMerged
		rr.Status.HoldsTarget = new(held)
		rr.Status.PullRequest = &api.PullRequestStatus{Number: 7, HeadSHA: strings.Repeat("a", 40)}
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionReviewCheckNotFailed,
			Status: metav1.ConditionTrue, Reason: ReasonGitFailed, Message: "m"})
		ctx, lines := loggedCtx()
		(&RemediationRequestReconciler{}).settleRecorded(ctx, rr)
		if meta.FindStatusCondition(rr.Status.Conditions, api.ConditionReviewCheckNotFailed) != nil {
			t.Fatalf("held %v: ReviewCheckNotFailed stays after the merge is recorded", held)
		}
		var found int
		for _, l := range *lines {
			if strings.Contains(l, "may stay the latest on its head") && strings.Contains(l, ReasonGitFailed) {
				found++
			}
		}
		if found != 1 {
			t.Fatalf("held %v: %d Info lines say the condition ended, want 1: %q", held, found, *lines)
		}
	}
}

// Finding R5 of the ADR-0046 run on #925 (decision of 2026-11-21): a look of a stopped holder whose review check is no
// longer its own, a handoff intent standing or reviewer hints off, ends condition ReviewCheckNotFailed and writes
// nothing, so a check written before the stop may stay the latest on the head; the end is said in the log at Info,
// as every other end of the looks says it. Before, the condition went silently.
func TestStoppedHolderLogsTheEndOfItsNoteWhenItOwnsNoCheck(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hints bool
	}{{name: "a handoff intent", hints: true}, {name: "hints off"}} {
		t.Run(tc.name, func(t *testing.T) {
			rr := &api.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "rr"}}
			rr.Status.Phase = api.PhaseNeedsIntervention
			rr.Status.HoldsTarget = new(true)
			rr.Status.PullRequest = &api.PullRequestStatus{Number: 7, HeadSHA: strings.Repeat("a", 40)}
			if tc.hints {
				rr.Status.Handoff = &api.Handoff{To: &api.OutgoingHandoff{Phase: api.HandoffIntent}}
			}
			meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionReviewCheckNotFailed,
				Status: metav1.ConditionTrue, Reason: ReasonGitFailed, Message: "m"})
			ctx, lines := loggedCtx()
			if err := (&RemediationRequestReconciler{ReviewerHints: tc.hints}).failCheckAfterLook(ctx, rr); err != nil {
				t.Fatalf("failCheckAfterLook: %v", err)
			}
			if meta.FindStatusCondition(rr.Status.Conditions, api.ConditionReviewCheckNotFailed) != nil {
				t.Fatal("ReviewCheckNotFailed stays on a request that owns no review check")
			}
			var found int
			for _, l := range *lines {
				if strings.Contains(l, "may stay the latest on its head") && strings.Contains(l, ReasonGitFailed) {
					found++
				}
			}
			if found != 1 {
				t.Fatalf("%d Info lines say the condition ended, want 1: %q", found, *lines)
			}
		})
	}
}
