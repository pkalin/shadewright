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

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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
		m.step()
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

// The seventh decision of 2026-11-18 on #771, point 1: a merge recorded with its receipt and the lock held through
// verification, then verification turned off and writes paused. As on main, the pause does not hold the release
// that the recorded status proves: the reconcile under the pause releases the lock, without a read of the Git host.
func TestSequenceReceiptReleasesTheLockUnderAPause(t *testing.T) {
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
	m.pause(true)
	m.steps(1)
	if live := m.live(); holdsLock(live) {
		m.fatalf("receipt: writes are paused and the lock of a merge recorded with its receipt is held: phase %s",
			live.Status.Phase)
	}
	m.report()
}

// The seventh decision of 2026-11-18 on #771, point 3: a request in PolicyChecked whose pull request was opened while
// the write that would record it was lost, a failed opening of the repository that starts the deadline's wait, a
// pause, the deadline passing and the pull request closed by hand during it, the lift, and (cancel) a person's cancel
// set during the pause. After the lift every reconcile that leaves the lock held by the stopped request asks to run
// again at a time, and the lock is free within a few reconciles.
func TestSequenceLockSettlesAfterAPauseOverTheDeadline(t *testing.T) {
	for _, c := range []struct {
		name   string
		cancel bool
	}{{"deadline", false}, {"cancel", true}} {
		cancel := c.cancel
		t.Run(c.name, func(t *testing.T) {
			m := scripted(t, modelStart{start: startPolicyChecked, open: true, lasting: time.Minute})
			m.steps(1)
			if live := m.live(); gitWait(live, waitRequestDeadline) == nil {
				t.Fatalf("setup: want the deadline's wait recorded, got waits %v", live.Status.GitWaits)
			}
			m.fault, m.lastingUntil = faultNone, time.Time{}
			m.pause(true)
			if cancel {
				m.cancel()
			}
			m.steps(5)
			if err := m.g.CloseChangeRequest(context.Background(), m.number); err != nil {
				t.Fatal(err)
			}
			m.logf("close the pull request by hand")
			m.pause(false)
			settledIn(m, 3)
			m.report()
		})
	}
}

// settledIn runs up to n reconciles; each that leaves the lock held by a stopped request must ask to run again at a
// time, and the lock must be free after them.
func settledIn(m *modelWorld, n int) {
	m.t.Helper()
	for i := range n {
		res, err := m.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: m.key})
		live := m.live()
		m.logf("reconcile: phase %s, holds %t, wait %v, err %v", live.Status.Phase, holdsLock(live), res.RequeueAfter, err)
		if !holdsLock(live) {
			return
		}
		if err == nil && res.RequeueAfter <= 0 && !progresses(live) {
			m.fatalf("requeue: reconcile %d after the lift leaves the lock held (phase %s) with no time to run again", i,
				live.Status.Phase)
		}
		if err != nil {
			m.clock.advance(time.Second)
		} else {
			m.clock.advance(max(res.RequeueAfter, time.Second))
		}
	}
	m.fatalf("settled: the lock is held %d reconciles after the lift (phase %s)", n, m.live().Status.Phase)
}

// The eighth decision of 2026-11-18 on #771, point 1: the reads of what a retargeted merge brought fail and start
// the content wait; writes are then paused for longer than the whole wait, and lifted while the reads still fail.
// Paused time stretches the wait: the request ends once the wait has had its five minutes of time not paused, not at
// the first reconcile after the lift, and within one poll interval after that (three minutes of reads before the
// pause count).
func TestSequenceContentWaitStretchedByAPause(t *testing.T) {
	m := scripted(t, modelStart{start: stopPlanFresh})
	if err := m.g.SetBase(m.number, m.configured); err != nil {
		t.Fatal(err)
	}
	sha, err := m.g.Merge(m.number)
	if err != nil {
		t.Fatal(err)
	}
	m.merge = sha
	m.fault = faultBetween
	m.steps(2)
	w := gitWait(m.live(), waitMergeContent)
	if w == nil {
		t.Fatalf("setup: want the content wait recorded, got waits %v, phase %s", m.live().Status.GitWaits,
			m.live().Status.Phase)
	}
	since := w.Since.Time
	// Three minutes of reads before the pause.
	for m.clock.at.Before(since.Add(3 * time.Minute)) {
		m.step()
	}
	m.pause(true)
	pausedAt := m.clock.at
	m.steps(2 * int(mergeContentWait/freshRecheckDelay))
	m.pause(false)
	lift := m.clock.at
	// What is left of the wait after the lift: its five minutes less the time not paused before the pause was seen.
	due := lift.Add(mergeContentWait - pausedAt.Sub(since))
	ended := false
	for range 20 {
		m.step()
		live := m.live()
		if ended = mergeRecorded(live); ended {
			if m.clock.at.Before(due.Add(-2 * time.Second)) {
				m.fatalf("early: the request ended %v after the lift, %v before the wait had its time not paused "+
					"(phase %s)", m.clock.at.Sub(lift), due.Sub(m.clock.at), live.Status.Phase)
			}
			break
		}
	}
	switch {
	case !ended:
		m.fatalf("end: the reads kept failing for the whole wait not paused and the request has not ended")
	case m.clock.at.After(due.Add(m.r.pullRequestPollInterval() + 2*time.Second)):
		m.fatalf("late: the request ended %v after the wait had its time not paused", m.clock.at.Sub(due))
	}
	m.report()
}

// mergeCheckWithFailingReads is a request whose pull request was merged without its merge commit and whose reads
// fail from then on: the MergeCheck wait is recorded.
func mergeCheckWithFailingReads(t *testing.T, verify bool) *modelWorld {
	t.Helper()
	m := scripted(t, modelStart{start: startRunning, fresh: true, verify: verify})
	m.steps(1)
	sha, err := m.g.Merge(m.number)
	if err != nil {
		t.Fatal(err)
	}
	m.merge, m.noCommit = sha, true
	m.steps(1)
	m.fault, m.lastingUntil = opGet, m.clock.at.Add(24*time.Hour)
	m.steps(1)
	if live := m.live(); gitWait(live, waitMergeCheck) == nil {
		t.Fatalf("setup: want the MergeCheck wait recorded, got waits %v, phase %s", live.Status.GitWaits,
			live.Status.Phase)
	}
	return m
}

// The eighth decision of 2026-11-18 on #771, point 1: a merge recorded without its commit, a pause, the lift, and a
// status write lost at the lift (the second write of the reconcile, the first applied and reported failed, or a
// restart between the writes), while the reads keep failing. The MergeCheck wait is never dropped, so it ends: the
// merge is recorded within the wait and a poll interval of time not paused.
func TestSequenceMergeCheckSurvivesALostWriteAtTheLift(t *testing.T) {
	for _, c := range []struct {
		name    string
		fault   func(m *modelWorld)
		restart bool
	}{
		{"second write lost", func(m *modelWorld) { m.passWrites, m.loseNext = 1, true }, false},
		{"first write applied, reported failed", func(m *modelWorld) { m.applyNext = true }, false},
		{"restart between the writes", func(m *modelWorld) { m.passWrites, m.loseNext = 1, true }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := mergeCheckWithFailingReads(t, false)
			start := gitWait(m.live(), waitMergeCheck).Since.Time
			m.pause(true)
			pausedAt := m.clock.at
			m.steps(3)
			m.pause(false)
			lift := m.clock.at
			c.fault(m)
			m.step()
			if c.restart {
				m.restart()
			}
			due := lift.Add(mergeCommitWait - pausedAt.Sub(start))
			at := m.clock.at
			for range 30 {
				if mergeRecorded(m.live()) {
					break
				}
				at = m.clock.at
				m.step()
			}
			live := m.live()
			switch {
			case !mergeRecorded(live):
				m.fatalf("end: the merge is not recorded %v after the lift (phase %s, waits %v)", m.clock.at.Sub(lift),
					live.Status.Phase, live.Status.GitWaits)
			case at.After(due.Add(m.r.pullRequestPollInterval())):
				m.fatalf("late: the merge was recorded %v after the wait's end", at.Sub(due))
			}
			m.report()
		})
	}
}

// The eighth decision of 2026-11-18 on #771, point 1: while the reads fail, writes are paused for half of every minute,
// for an hour. Each pause stretches the MergeCheck wait by what a reconcile saw of it, so the wait ends once it has had
// its five minutes of time not paused: never before, and not an hour later.
func TestSequenceMergeCheckEndsUnderAPauseThatComesAndGoes(t *testing.T) {
	m := mergeCheckWithFailingReads(t, false)
	start := gitWait(m.live(), waitMergeCheck).Since.Time
	unpaused := m.clock.at.Sub(start)
	run := func() {
		res, err := m.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: m.key})
		live := m.live()
		m.logf("reconcile: phase %s, wait %v, err %v, waits %d", live.Status.Phase, res.RequeueAfter, err,
			len(live.Status.GitWaits))
	}
	for range 60 {
		m.pause(true)
		run()
		m.clock.advance(30 * time.Second)
		m.pause(false)
		run()
		if mergeRecorded(m.live()) {
			break
		}
		m.clock.advance(30 * time.Second)
		unpaused += 30 * time.Second
	}
	live := m.live()
	switch {
	case !mergeRecorded(live):
		m.fatalf("end: an hour of pauses coming and going and the merge is not recorded (phase %s, waits %v)",
			live.Status.Phase, live.Status.GitWaits)
	case unpaused < mergeCommitWait-2*time.Second:
		m.fatalf("early: the merge was recorded after %v of time not paused, before the wait's %v", unpaused,
			mergeCommitWait)
	}
	m.report()
}

// The seventh decision of 2026-11-18 on #771, point 6: the cancel watch after a change of the target branch
// (cancelLook) reads the pull requests on two branches, a read new on this branch. A pause set after the close was
// tried (closeChangeRequest stops at a pause first, so only a pause set between the two gets here) holds the look:
// no call to the Git host, WritesPaused shown, the phase kept, and the look again after freshRecheckDelay.
func TestSequenceCancelLookWaitsForAPause(t *testing.T) {
	m := scripted(t, modelStart{start: startRunning, fresh: true})
	m.steps(1)
	m.cancel()
	m.setConfigured(freshOtherBranch)
	m.pause(true)
	rr := m.live()
	m.calls = 0
	res, done, err := m.r.cancelLook(context.Background(), rr, nil)
	live := m.live()
	switch {
	case err != nil || !done || res.RequeueAfter != freshRecheckDelay:
		t.Fatalf("the look under a pause: %+v, done %t, %v; want the wait for the pause", res, done, err)
	case m.calls > 0:
		t.Fatalf("the look under a pause called the Git host %d times", m.calls)
	case meta.FindStatusCondition(live.Status.Conditions, api.ConditionWritesPaused) == nil:
		t.Fatal("the look under a pause does not show WritesPaused")
	case live.Status.Phase != rr.Status.Phase:
		t.Fatalf("the look under a pause changed the phase from %s to %s", rr.Status.Phase, live.Status.Phase)
	}
}

// Point 5: a failed read of the cancel watch after a change of the target branch is recorded like every other
// failed Git read (retryGitRead, without an end): condition GitReadRetry, a Paced entry, a wait of at least the poll
// interval and no error for the rate limiter.
func TestSequenceCancelLookRecordsAFailedRead(t *testing.T) {
	m := scripted(t, modelStart{start: startRunning, fresh: true})
	m.steps(1)
	m.cancel()
	m.setConfigured(freshOtherBranch)
	m.fault = opList
	res, done, err := m.r.cancelLook(context.Background(), m.live(), nil)
	live := m.live()
	switch {
	case err != nil || !done || res.RequeueAfter < m.r.pullRequestPollInterval():
		t.Fatalf("the failed look: %+v, done %t, %v; want a visible wait of at least the poll interval", res, done, err)
	case gitWait(live, waitNone) == nil:
		t.Fatalf("the failed look records no wait: %v", live.Status.GitWaits)
	case meta.FindStatusCondition(live.Status.Conditions, api.ConditionGitReadRetry) == nil:
		t.Fatal("the failed look does not show GitReadRetry")
	case meta.FindStatusCondition(live.Status.Conditions, api.ConditionCancelPending) == nil:
		t.Fatal("the failed look does not show why the cancel waits (CancelPending)")
	}
}

// The eighth decision of 2026-11-18 on #771, point 3: the MergeCheck wait ends while the reads fail, so the merge is
// recorded without a receipt; writes are then paused. A recorded merge releases the lock from recorded status, as on
// main: in the write that records it, before the pause could hold a read that settles it.
func TestSequenceMergeWithoutReceiptReleasesUnderAPause(t *testing.T) {
	for _, verify := range []bool{false, true} {
		t.Run(map[bool]string{false: "verification off", true: "verification on"}[verify], func(t *testing.T) {
			m := mergeCheckWithFailingReads(t, verify)
			for range 12 {
				if mergeRecorded(m.live()) {
					break
				}
				m.step()
			}
			if live := m.live(); !mergeRecorded(live) || live.Status.MergeReceipt != nil {
				t.Fatalf("setup: want the merge recorded without a receipt, got phase %s, receipt %t", live.Status.Phase,
					live.Status.MergeReceipt != nil)
			}
			m.pause(true)
			m.steps(3)
			if live := m.live(); holdsLock(live) {
				m.fatalf("lock: the merge is recorded (phase %s) and writes are paused, and the lock is held",
					live.Status.Phase)
			}
			m.report()
		})
	}
}
