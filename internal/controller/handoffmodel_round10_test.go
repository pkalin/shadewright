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

import "testing"

// Seed 3506 of the strict model: the replacement's commit is merged before its takeover is recorded; the takeover
// reads the merge, the check of the plan at the merge fails (the read of the policy), so it stores the wait for the
// merge (its claim on it) and its own status write, which records the pull request, meets a conflict; the repository
// then leaves the settings. The wait ends with no read possible, and the end of the merge wait judged the merge
// foreign because no pull request of the replacement was recorded: MergedWithForeignChange with no receipt, and the
// predecessor a failure for the breaker (P1, P2). The wait names the pull request and the head the takeover saw as
// the replacement's commit, so its end records the merge on the replacement with its receipt.
func TestHandoffModelLostTakeoverWriteEndsMergeWaitAsOwn(t *testing.T) {
	hmDirected(t, hmStartPushed, []hmStep{stMerge, {Kind: hConflict, Obj: hmRepl},
		{Kind: hFault, Code: 404, Op: hmOpPolicyRead}, stRecRepl, {Kind: hSettings, Variant: settingsRemoved}})
}

// The same lost takeover write, with the Git host failing every read until the wait for the merge is over instead
// of the repository leaving the settings: the end of the wait records the merge on the replacement with its receipt.
func TestHandoffModelLostTakeoverWriteEndsMergeWaitAsOwnInAnOutage(t *testing.T) {
	hmDirected(t, hmStartPushed, []hmStep{stMerge, {Kind: hConflict, Obj: hmRepl},
		{Kind: hFault, Code: 404, Op: hmOpPolicyRead}, stRecRepl, stSticky(503), stRecRepl,
		{Kind: hClock, Clock: clockSmall, Mins: 6}, stRecRepl, stRecRepl, stRecPred})
}
