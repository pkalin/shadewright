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
	"math/rand/v2"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/pkalin/shadewright/api/v1alpha1"
)

// The sequences the fourth decision of 2026-11-18 on #771 names, scripted through the model of gitwaitmodel_test.go
// (the generated test finds each of them too): each runs its events in order and checks the model's properties.

// scripted is a model run with the given start and no random events.
func scripted(t *testing.T, st modelStart) *modelWorld {
	t.Helper()
	// #nosec G404 -- a fixed generator: a scripted run draws nothing that matters from it.
	return newModelFrom(t, 0, rand.New(rand.NewPCG(1, 2)), requestStatusSchema(t), st)
}

func (m *modelWorld) steps(n int) {
	for range n {
		m.step(true)
	}
}

// a: the request deadline passes while the repository cannot be opened. The end of the wait reads nothing from the
// Git host and cannot be retried: the request ends Expired, and the wait with it.
func TestSequenceDeadlinePassesWhileTheRepositoryCannotBeOpened(t *testing.T) {
	m := scripted(t, modelStart{start: startPolicyChecked, open: true, lasting: 24 * time.Hour})
	m.steps(12)
	live := m.live()
	if live.Status.Phase != api.PhaseExpired || gitWait(live, waitRequestDeadline) != nil {
		m.fatalf("end: the request is %s with waits %v; want Expired without its deadline wait", live.Status.Phase,
			live.Status.GitWaits)
	}
	m.report()
}

// b: a merge reported without its commit, and the reads fail from then on. The wait for the commit is an entry of
// status.gitWaits, so it ends within its bound: the merge is recorded without a receipt.
func TestSequenceMergeWithoutItsCommitWhileReadsFail(t *testing.T) {
	m := scripted(t, modelStart{start: startRunning, fresh: true})
	m.steps(1)
	if _, err := m.g.Merge(m.number); err != nil {
		t.Fatal(err)
	}
	m.noCommit = true
	m.steps(1)
	m.fault = opGet
	m.steps(10)
	if live := m.live(); !mergeRecorded(live) {
		m.fatalf("end: the merge is not recorded: phase %s", live.Status.Phase)
	}
	m.report()
}

// c: a merge recorded with its receipt, the lock held through verification, a restart with verification off, and
// the repository gone from the settings: the receipt settles the lock without a read of the branch.
func TestSequenceReceiptReleasesTheLockWithTheRepositoryGone(t *testing.T) {
	m := scripted(t, modelStart{start: startRunning, fresh: true, verify: true})
	m.steps(1)
	sha, err := m.g.Merge(m.number)
	if err != nil {
		t.Fatal(err)
	}
	m.merge = sha
	m.steps(1)
	if live := m.live(); live.Status.MergeReceipt == nil || !holdsLock(live) {
		t.Fatalf("setup: want the merge recorded with its receipt and the lock held, got phase %s, holds %t",
			live.Status.Phase, holdsLock(live))
	}
	m.r.VerifyOutcomes = false
	m.restart()
	m.r.VerifyOutcomes = false
	m.removeRepository()
	m.steps(4)
	m.settled()
	m.report()
}

// d, directly: a write of the Git waits from a fresh read leaves the caller's copy as it was, so a later write of
// that copy is refused (a conflict) instead of putting back an older holdsTarget.
func TestWaitWriteLeavesNoPartLiveCopy(t *testing.T) {
	r, _, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	stale := rr.DeepCopy()
	live := &api.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
		t.Fatal(err)
	}
	live.Status.HoldsTarget = new(false)
	if err := r.Status().Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	w, _ := newWait(stale, waitNone, "read", time.Now())
	if err := r.putGitWaits(ctx, stale, []api.GitWait{w}); err != nil {
		t.Fatal(err)
	}
	_ = r.Status().Update(ctx, stale)
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
		t.Fatal(err)
	}
	if *live.Status.HoldsTarget {
		t.Fatal("a status write after the write of the Git waits put back the older holdsTarget")
	}
}

// e: the branch the settings name now is the longest name the schema allows, and a read of what the merge into it
// brought fails: the entry is recorded (the API server accepts it), so the content wait runs and ends.
func TestSequenceLongBranchContentWait(t *testing.T) {
	m := scripted(t, modelStart{start: stopPlanFresh, long: true})
	if err := m.g.SetBase(m.number, m.configured); err != nil {
		t.Fatal(err)
	}
	sha, err := m.g.Merge(m.number)
	if err != nil {
		t.Fatal(err)
	}
	m.merge = sha
	m.fault = faultBetween
	m.steps(10)
	if live := m.live(); !mergeRecorded(live) {
		m.fatalf("end: the merge is not recorded: phase %s", live.Status.Phase)
	}
	m.report()
}
