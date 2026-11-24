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
	"testing"

	"k8s.io/utils/ptr"

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
)

// Where the live copies of a handoff (decision of 2026-11-19 on #786) meet the verdicts of a pull request read by its
// number on another base (ADR-0043): the decision is taken on the live copy, the verdict is ADR-0043's.

// stoppedOnOtherBase fails unless x stopped NeedsIntervention with ChangeRequestOnOtherBase.
func stoppedOnOtherBase(t *testing.T, who string, x *api.RemediationRequest) {
	t.Helper()
	if x == nil || x.Status.Phase != api.PhaseNeedsIntervention {
		t.Fatalf("the %s: %s; want NeedsIntervention with %s", who, describe(x), ReasonChangeRequestOnOtherBase)
	}
	for _, c := range x.Status.Conditions {
		if c.Reason == ReasonChangeRequestOnOtherBase {
			return
		}
	}
	t.Fatalf("the %s stopped without %s: %v", who, ReasonChangeRequestOnOtherBase, x.Status.Conditions)
}

// handoffAborted fails unless the replacement ended its handoff with HandoffAborted before any takeover.
func handoffAborted(t *testing.T, x *api.RemediationRequest) {
	t.Helper()
	if x == nil || tookOver(x) {
		t.Fatalf("the replacement: %s; want its handoff aborted, no takeover", describe(x))
	}
	for _, c := range x.Status.Conditions {
		if c.Reason == api.ReasonHandoffAborted {
			return
		}
	}
	t.Fatalf("the replacement has no %s: %s, %v", api.ReasonHandoffAborted, describe(x), x.Status.Conditions)
}

// The intent stands and a person moves the pull request to another base. The replacement reads the predecessor's pull
// request by its number inside the handoff (findHanded) while the cache serves the predecessor from before its intent:
// the live copy still names the intent, and the verdict open on another base aborts the handoff with no commit pushed
// (P8). The predecessor then stops ChangeRequestOnOtherBase and the lock stays while the pull request is open there.
func TestHandoffModelMovedBaseReplacementReadsByNumberOnTheLiveCopy(t *testing.T) {
	m := hmDirected(t, hmStartIntent, []hmStep{{Kind: hMoveBase}, {Kind: hStale, Obj: hmPred}, stRecRepl, stRecRepl,
		stRecPred, stRecPred})
	handoffAborted(t, m.get(hmRepl))
	stoppedOnOtherBase(t, hmPred, m.get(hmPred))
	if !ptr.Deref(m.get(hmPred).Status.HoldsTarget, false) && !ptr.Deref(m.get(hmRepl).Status.HoldsTarget, false) {
		t.Fatalf("the lock is free while the pull request is open on %s (pred: %s, repl: %s)", hmMovedBase,
			describe(m.get(hmPred)), describe(m.get(hmRepl)))
	}
}

// The intent stands, nothing is pushed yet, and the predecessor looks first: its pull request, read by its number, is
// open on another base with the head it recorded, so it stops ChangeRequestOnOtherBase (the stop aborts the intent)
// and writes nothing to it. The replacement, reconciled with the predecessor changing between its first read and its
// decision, reads the live predecessor and takes nothing over, pushing nothing (P8).
func TestHandoffModelMovedBasePredecessorStopsWhileItsIntentStands(t *testing.T) {
	m := hmDirected(t, hmStartIntent, []hmStep{{Kind: hMoveBase}, stRecPred, {Kind: hInterleave, Obj: hmRepl, N: 0},
		stRecRepl, stRecRepl})
	stoppedOnOtherBase(t, hmPred, m.get(hmPred))
	if x := m.get(hmRepl); x != nil && tookOver(x) {
		t.Fatalf("the replacement took over a pull request open on %s: %s", hmMovedBase, describe(x))
	}
}

// The replacement pushed its commit and recorded the takeover; a person then moves the pull request before the
// replacement ends the predecessor (step 5). The predecessor's watch reads it by its number on another base with the
// replacement's head while its intent still stands: it reads the replacement from the API server and, finding the
// takeover recorded, leaves the pull request to it and aborts nothing (P9); the replacement stops
// ChangeRequestOnOtherBase and ends the predecessor HandedOff. Neither writes to the moved pull request, the rewrite
// after the takeover included (P8). Before the fix the predecessor stopped and stored the abort of that intent, and
// the replacement rewrote the pull request on the other base.
func TestHandoffModelMovedBaseAfterATakeoverTheIntentStillNames(t *testing.T) {
	m := hmDirected(t, hmStartIntent, []hmStep{stRecRepl, {Kind: hMoveBase}, stRecPred,
		{Kind: hInterleave, Obj: hmRepl, N: 0}, stRecRepl, stRecRepl, stRecPred})
	repl := m.get(hmRepl)
	stoppedOnOtherBase(t, hmRepl, repl)
	if !ptr.Deref(repl.Status.HoldsTarget, false) {
		t.Fatalf("the replacement released the lock while its pull request is open on %s: %s", hmMovedBase, describe(repl))
	}
	if pred := m.get(hmPred); pred != nil && !handedOff(pred) {
		t.Fatalf("the predecessor: %s; want HandedOff, its intent Done", describe(pred))
	}
}

// After the takeover a person moves the pull request: the replacement's watch reads it by its number open on another
// base and stops ChangeRequestOnOtherBase, keeping the lock; the predecessor, ended HandedOff or ending, writes
// nothing (P8). The cache serves the replacement from before its stop at the next reconcile: the live copy decides.
func TestHandoffModelMovedBaseAfterTheTakeover(t *testing.T) {
	m := hmDirected(t, hmStartTookOver, []hmStep{{Kind: hMoveBase}, stRecRepl, {Kind: hStale, Obj: hmRepl}, stRecRepl,
		stRecPred, stRecPred})
	repl := m.get(hmRepl)
	stoppedOnOtherBase(t, hmRepl, repl)
	if !ptr.Deref(repl.Status.HoldsTarget, false) {
		t.Fatalf("the replacement released the lock while its pull request is open on %s: %s", hmMovedBase, describe(repl))
	}
}

// The intent stands and a person moves the pull request and merges it there. Neither request records the merge (it did
// not reach the target branch), the predecessor stops ChangeRequestOnOtherBase with no lock, and the replacement, whose
// takeover reads the pull request merged on another base on the live predecessor, aborts its handoff; after the
// settling run nobody holds the lock (P6).
func TestHandoffModelMergedOnAnotherBaseDuringTheIntent(t *testing.T) {
	m := hmDirected(t, hmStartIntent, []hmStep{{Kind: hMoveBase}, stMerge, {Kind: hStale, Obj: hmPred}, stRecRepl,
		stRecPred, stRecRepl})
	pred := m.get(hmPred)
	stoppedOnOtherBase(t, hmPred, pred)
	for who, x := range map[string]*api.RemediationRequest{hmPred: pred, hmRepl: m.get(hmRepl)} {
		if x == nil {
			continue
		}
		if recordsMerge(x) || x.Status.MergeReceipt != nil {
			t.Fatalf("the %s recorded a merge into %s: %s", who, hmMovedBase, describe(x))
		}
		if ptr.Deref(x.Status.HoldsTarget, false) {
			t.Fatalf("the %s holds the lock after a merge into %s: %s", who, hmMovedBase, describe(x))
		}
	}
}

// The replacement's recovery meets a refusal and watches the pull request in its HandoffRecovery wait; a person moves
// the pull request to another base, and the cache serves the predecessor from before the abort. The wait stays while
// the pull request is open there and the lock is kept (P3); once it is merged there neither request records the merge,
// the wait ends with the recovery, and nobody holds the lock after the settling run (P6). No write reaches the moved
// pull request (P8).
func TestHandoffModelRecoveryWaitOnAnotherBase(t *testing.T) {
	m := hmDirected(t, hmStartPushed, []hmStep{stSticky(403), stRecRepl, stAnswer, {Kind: hMoveBase},
		{Kind: hStale, Obj: hmPred}, stRecRepl, stRecPred, stRecRepl, {Kind: hMerge}, stRecRepl, stRecPred, stRecRepl})
	if !m.reach["refusal-to-HandoffRecovery"] {
		t.Fatal("setup: the recovery never recorded its HandoffRecovery wait")
	}
	for who, x := range map[string]*api.RemediationRequest{hmPred: m.get(hmPred), hmRepl: m.get(hmRepl)} {
		if x == nil {
			continue
		}
		if recordsMerge(x) || x.Status.MergeReceipt != nil {
			t.Fatalf("the %s recorded a merge into %s: %s", who, hmMovedBase, describe(x))
		}
		if len(x.Status.GitWaits) > 0 {
			t.Fatalf("the %s keeps the waits %v after a merge into %s", who, x.Status.GitWaits, hmMovedBase)
		}
	}
}
