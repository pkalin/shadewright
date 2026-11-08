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
			rr := &remediationv1alpha1.RemediationRequest{}
			if !tc.noPR {
				rr.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: 1}
			}
			recordMerge(rr, tc.cr)
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
