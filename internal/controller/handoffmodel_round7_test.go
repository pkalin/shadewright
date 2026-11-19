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
	"fmt"
	"testing"
)

// The second decision of 2026-11-19 on #880: a pending wait that names the merged pull request and head is a claim on
// the merge, not its record. While one request holds only such a claim the other stays MergeRecordPending; when the
// holder is deleted before its check ends, the other records the merge (P1).

// The request replaced reads the merge of its own commit put back after the takeover first, without a merge commit ID,
// and stores its MergeCheck wait; the replacement must not end MergedForPredecessor on that wait. The predecessor is
// deleted while it holds only the wait, and the replacement records the merge as a foreign change.
func TestHandoffModelClaimOfThePredecessorDeleted(t *testing.T) {
	hmDirected(t, hmStartIntent, []hmStep{stRecRepl, {Kind: hForce, Variant: forceDrop}, stMergeNoID, stRecPred,
		stRecRepl, {Kind: hDelete, Obj: hmPred}, stRecPred, stRecPred, stRecRepl})
}

// The mirror: after a refused handoff a person merges the replacement's commit, the host reports no merge commit ID,
// and the replacement stores its MergeCheck wait; the predecessor must not end MergedForReplacement on that wait. The
// replacement is deleted while it holds only the wait, and the predecessor records the merge by the rule for a deleted
// replacement.
func TestHandoffModelClaimOfTheReplacementDeleted(t *testing.T) {
	hmDirected(t, hmStartPushed, []hmStep{stSticky(403), stRecRepl, stMergeNoID, stAnswer, stRecRepl, stRecRepl,
		stRecPred, {Kind: hDelete, Obj: hmRepl}, stRecRepl, stRecRepl, stRecPred})
}

// The seeds of the strict run 60,001 to 260,000 of the generated model that broke P1 before this decision, as their
// minimal sequences under the generator of that run (the generator draws differently since, so the seed numbers no
// longer name them). Five delete the replacement while it holds only its MergeCheck wait after a refused handoff;
// the predecessor, looking while the replacement is being deleted, left the merge to it on that wait. 137005 is the
// mirror after a takeover: the predecessor holds the wait and is deleted after the replacement left the merge to it.
func TestHandoffModelClaimRegressionSeeds(t *testing.T) {
	merge := stMergeNoID
	del := func(who string) hmStep { return hmStep{Kind: hDelete, Obj: who} }
	once := func(op string) hmStep { return hmStep{Kind: hFault, Code: 403, Op: op} }
	for _, c := range []struct {
		seed  int
		start string
		steps []hmStep
	}{
		{105521, hmStartPushed, []hmStep{merge, stAnswer, once("CheckCommit"), stRecRepl, stRecRepl, stRecRepl,
			del(hmRepl)}},
		{156977, hmStartPushed, []hmStep{once("open"), merge, stRecRepl, stRecRepl, stRecRepl, del(hmRepl)}},
		{189097, hmStartPushed, []hmStep{once("open"), stRecRepl, merge, stRecRepl, stRecRepl, stRecRepl,
			del(hmRepl)}},
		{219429, hmStartPushed, []hmStep{merge, once(opGet), stRecRepl, stRecRepl, stRecRepl, del(hmRepl)}},
		{203937, hmStartPushed, []hmStep{merge, once("Parents"), stRecRepl, stRecRepl, stRecRepl, del(hmRepl)}},
		{137005, hmStartPushed, []hmStep{stRecRepl, {Kind: hForce, Variant: forceDrop},
			{Kind: hMerge, NoID: true}, stRecPred, stRecRepl, del(hmPred)}},
	} {
		t.Run(fmt.Sprintf("seed=%d", c.seed), func(t *testing.T) {
			hmDirected(t, c.start, c.steps)
		})
	}
}
