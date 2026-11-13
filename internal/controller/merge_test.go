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
	"time"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
)

func TestRecordMerge(t *testing.T) {
	merge := strings.Repeat("ab", 20)
	for _, tc := range []struct {
		name   string
		cr     providers.ChangeRequest
		noPR   bool
		wantPR string
	}{
		{"merged", providers.ChangeRequest{State: providers.ChangeRequestMerged, MergeCommit: merge}, false, merge},
		{"open", providers.ChangeRequest{State: providers.ChangeRequestOpen, MergeCommit: merge}, false, ""},
		{"no commit", providers.ChangeRequest{State: providers.ChangeRequestMerged}, false, ""},
		{"upper case", providers.ChangeRequest{State: providers.ChangeRequestMerged, MergeCommit: strings.ToUpper(merge)}, false, ""},
		{"SHA-256", providers.ChangeRequest{State: providers.ChangeRequestMerged, MergeCommit: strings.Repeat("ab", 32)}, false, ""},
		{"abbreviated", providers.ChangeRequest{State: providers.ChangeRequestMerged, MergeCommit: merge[:12]}, false, ""},
		{"no pull request recorded", providers.ChangeRequest{State: providers.ChangeRequestMerged, MergeCommit: merge}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A request whose merge can be bound to a receipt, so only the commit decides (#678: the commit is
			// written only with the receipt).
			rr, bound := receiptRequest()
			if tc.noPR {
				rr.Status.PullRequest = nil
			}
			cr := tc.cr
			cr.Number, cr.HeadSHA, cr.BaseBranch = bound.Number, bound.HeadSHA, bound.BaseBranch
			recordMerge(t.Context(), rr, cr)
			if tc.noPR {
				if rr.Status.PullRequest != nil {
					t.Fatalf("a pull request was recorded: %+v", rr.Status.PullRequest)
				}
				return
			}
			if got := rr.Status.PullRequest.MergeCommitSHA; got != tc.wantPR {
				t.Errorf("MergeCommitSHA = %q, want %q", got, tc.wantPR)
			}
		})
	}
}

func TestRecordApplied(t *testing.T) {
	merge := strings.Repeat("ab", 20)
	first := time.Date(2026, 11, 8, 10, 0, 0, 500, time.UTC)
	rr := &remediationv1alpha1.RemediationRequest{}
	if recordApplied(rr, first) {
		t.Fatal("recorded without a pull request")
	}
	rr.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: 1}
	if recordApplied(rr, first) || rr.Status.PullRequest.AppliedObservedAt != nil {
		t.Fatal("recorded without a merge commit")
	}
	rr.Status.PullRequest.MergeCommitSHA = merge
	if recordApplied(rr, time.Time{}) || rr.Status.PullRequest.AppliedObservedAt != nil {
		t.Fatal("recorded a zero time")
	}
	if !recordApplied(rr, first) {
		t.Fatal("not recorded")
	}
	want := first.Truncate(time.Second)
	if got := rr.Status.PullRequest.AppliedObservedAt; got == nil || !got.Time.Equal(want) {
		t.Fatalf("AppliedObservedAt = %v, want %v", got, want)
	}
	for _, later := range []time.Time{first.Add(time.Hour), first.Add(-time.Hour)} {
		if recordApplied(rr, later) || !rr.Status.PullRequest.AppliedObservedAt.Time.Equal(want) {
			t.Errorf("recordApplied(%v) moved the time to %v", later, rr.Status.PullRequest.AppliedObservedAt)
		}
	}
}
