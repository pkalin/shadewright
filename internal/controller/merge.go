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
	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
)

// recordMerge writes the merge commit of the recorded pull request into the status, in the same write that
// ends the request in Merged, so delivery can later be checked against exactly that commit (design 8.2 step 8,
// #92). cr must be the recorded pull request, found merged with the head Shadewright pushed. A commit that does
// not pass the schema's pattern is not written: the status then holds none, and no delivery can be shown
// against it, instead of the whole write failing and the merge going unrecorded.
func recordMerge(rr *remediationv1alpha1.RemediationRequest, cr providers.ChangeRequest) {
	if rr.Status.PullRequest == nil || cr.State != providers.ChangeRequestMerged || !shaPattern.MatchString(cr.MergeCommit) {
		return
	}
	rr.Status.PullRequest.MergeCommitSHA = cr.MergeCommit
}
