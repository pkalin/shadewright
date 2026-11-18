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
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
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

// contentWaitWithFailingReads is a request stopped on PlanFresh whose pull request a person moved to the branch the
// settings name now and merged there, and whose reads of what the merge brought fail from then on: the MergeContent
// wait is recorded.
func contentWaitWithFailingReads(t *testing.T) *modelWorld {
	t.Helper()
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
	if gitWait(m.live(), waitMergeContent) == nil {
		t.Fatalf("setup: want the content wait recorded, got waits %v, phase %s", m.live().Status.GitWaits,
			m.live().Status.Phase)
	}
	return m
}

// once runs one reconcile of the model's request without moving the clock, logs it, and returns the request after it.
func (m *modelWorld) once() (reconcile.Result, *api.RemediationRequest, error) {
	m.mu.Lock()
	m.calls = 0
	m.mu.Unlock()
	res, err := m.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: m.key})
	live := m.live()
	m.logf("reconcile: phase %s, holds %t, wait %v, err %v, waits %d%s", live.Status.Phase, holdsLock(live),
		res.RequeueAfter, err, len(live.Status.GitWaits), conditionsOf(live))
	return res, live, err
}

// verifiedReason is the reason of condition Verified of rr, empty without one.
func verifiedReason(rr *api.RemediationRequest) string {
	return reasonOf(meta.FindStatusCondition(rr.Status.Conditions, api.ConditionVerified))
}

// The ninth decision of 2026-11-18 on #771, point 1: a merge without its commit, reads that fail, and writes paused for
// longer than the whole MergeCheck wait. Under the pause nothing is read and nothing ends. At the lift the host
// reports the merge commit and reads work: the reconcile after the lift reads once more, and the merge is recorded
// with its receipt, not ended without one (NoMergeReceipt).
func TestSequenceMergeCheckLastReadAnswersAfterAPause(t *testing.T) {
	m := mergeCheckWithFailingReads(t, false)
	start := gitWait(m.live(), waitMergeCheck).Since.Time
	m.pause(true)
	m.clock.at = start.Add(mergeCommitWait + 10*time.Minute)
	if _, live, err := m.once(); err != nil || mergeRecorded(live) || m.calls > 0 {
		t.Fatalf("under the pause past the wait's end: err %v, phase %s, %d calls; want nothing read and nothing ended",
			err, live.Status.Phase, m.calls)
	}
	m.fault, m.lastingUntil, m.noCommit = faultNone, time.Time{}, false
	m.pause(false)
	_, live, err := m.once()
	switch {
	case err != nil:
		t.Fatalf("the reconcile after the lift: %v", err)
	case !mergeRecorded(live) || live.Status.MergeReceipt == nil:
		m.fatalf("receipt: the last read answered with the merge commit and the merge is not recorded with its "+
			"receipt (phase %s, receipt %t)", live.Status.Phase, live.Status.MergeReceipt != nil)
	case verifiedReason(live) == ReasonNoMergeReceipt:
		m.fatalf("receipt: the merge is marked NoMergeReceipt although the last read reported its commit")
	}
	m.report()
}

// The ninth decision of 2026-11-18 on #771, point 1: the same pause longer than the wait, and at the lift the reads
// still fail. The reconcile after the lift makes the last read, which fails, and the wait ends there, once: the merge
// is recorded without a receipt (a plan not marked stale before is marked not shown fresh, with a message that says
// the read after the wait failed). No retry follows.
func TestSequenceMergeCheckLastReadFailsAfterAPause(t *testing.T) {
	m := mergeCheckWithFailingReads(t, false)
	start := gitWait(m.live(), waitMergeCheck).Since.Time
	m.pause(true)
	m.clock.at = start.Add(mergeCommitWait + 10*time.Minute)
	if _, live, err := m.once(); err != nil || mergeRecorded(live) {
		t.Fatalf("under the pause: err %v, phase %s; want nothing ended", err, live.Status.Phase)
	}
	m.pause(false)
	res, live, err := m.once()
	switch {
	case err != nil:
		t.Fatalf("the reconcile after the lift: %v", err)
	case m.calls == 0:
		m.fatalf("last read: the wait ended after the lift with no read of the Git host")
	case !mergeRecorded(live) || live.Status.MergeReceipt != nil:
		m.fatalf("end: the last read failed and the merge is not recorded without a receipt (phase %s)",
			live.Status.Phase)
	case gitWait(live, waitMergeCheck) != nil:
		m.fatalf("end: the MergeCheck wait stands after its last read failed")
	}
	if fresh := meta.FindStatusCondition(live.Status.Conditions, api.ConditionPlanFresh); fresh != nil &&
		fresh.Reason == freshness.ReasonPlanChanged && !strings.Contains(fresh.Message, "the read after the wait failed") {
		// A plan already marked stale keeps its own reason; one marked not shown fresh here says why.
		m.fatalf("message: PlanFresh does not say the read after the wait failed: %q", fresh.Message)
	}
	if res.RequeueAfter > 0 && res.RequeueAfter < time.Second {
		m.fatalf("retry: the ended wait asks to run again at once (%v)", res.RequeueAfter)
	}
	m.report()
}

// The ninth decision of 2026-11-18 on #771, point 1: a merge without its commit, and the executor does not reconcile
// for an hour (down, or a leader change); meanwhile the host reports the merge commit. The one reconcile after it
// reads once more and records the merge with its receipt: an executor that was away never ends a wait without a read.
func TestSequenceMergeCheckLastReadAfterTheExecutorWasAway(t *testing.T) {
	m := scripted(t, modelStart{start: startRunning, fresh: true})
	m.steps(1)
	sha, err := m.g.Merge(m.number)
	if err != nil {
		t.Fatal(err)
	}
	m.merge, m.noCommit = sha, true
	m.steps(1)
	if gitWait(m.live(), waitMergeCheck) == nil {
		t.Fatalf("setup: want the MergeCheck wait recorded, got waits %v, phase %s", m.live().Status.GitWaits,
			m.live().Status.Phase)
	}
	m.clock.advance(time.Hour)
	m.restart()
	m.noCommit = false
	_, live, err := m.once()
	switch {
	case err != nil:
		t.Fatalf("the reconcile after the hour: %v", err)
	case !mergeRecorded(live) || live.Status.MergeReceipt == nil:
		m.fatalf("receipt: the merge commit was reported and the merge is not recorded with its receipt (phase %s, "+
			"Verified %s)", live.Status.Phase, verifiedReason(live))
	}
	m.report()
}

// The ninth decision of 2026-11-18 on #771, point 1: while the reads fail, writes are paused for half of every minute.
// No pause stretches the wait: it ends at the first reconcile that is not paused after its five minutes, with the
// last read failing, and not later.
func TestSequenceMergeCheckEndsUnderAPauseThatComesAndGoes(t *testing.T) {
	m := mergeCheckWithFailingReads(t, false)
	end := gitWait(m.live(), waitMergeCheck).Ends.Time
	var endedAt time.Time
	for range 60 {
		m.pause(true)
		if _, live, _ := m.once(); mergeRecorded(live) {
			m.fatalf("pause: the wait ended under a pause (phase %s)", live.Status.Phase)
		}
		m.clock.advance(30 * time.Second)
		m.pause(false)
		if _, live, _ := m.once(); mergeRecorded(live) {
			endedAt = m.clock.at
			break
		}
		m.clock.advance(30 * time.Second)
	}
	switch {
	case endedAt.IsZero():
		m.fatalf("end: an hour of pauses coming and going and the merge is not recorded")
	case endedAt.Before(end):
		m.fatalf("early: the merge was recorded %v before the wait's end", end.Sub(endedAt))
	case endedAt.After(end.Add(time.Minute)):
		m.fatalf("late: the merge was recorded %v after the wait's end; the first reconcile not paused after it is "+
			"within a minute", endedAt.Sub(end))
	}
	m.report()
}

// The ninth decision of 2026-11-18 on #771, point 1: the content wait under a pause longer than the wait, lifted while
// the reads of what the merge brought still fail. The reconcile after the lift makes the last read, which fails, and
// the request ends there with MergeContentUnreadable, the message saying so.
func TestSequenceContentWaitLastReadFailsAfterAPause(t *testing.T) {
	m := contentWaitWithFailingReads(t)
	start := gitWait(m.live(), waitMergeContent).Since.Time
	m.pause(true)
	m.clock.at = start.Add(mergeContentWait + 10*time.Minute)
	if _, live, err := m.once(); err != nil || mergeRecorded(live) || m.calls > 0 {
		t.Fatalf("under the pause past the wait's end: err %v, phase %s, %d calls; want nothing read and nothing ended",
			err, live.Status.Phase, m.calls)
	}
	m.pause(false)
	_, live, err := m.once()
	approved := meta.FindStatusCondition(live.Status.Conditions, api.ConditionApproved)
	switch {
	case err != nil:
		t.Fatalf("the reconcile after the lift: %v", err)
	case m.calls == 0:
		m.fatalf("last read: the content wait ended after the lift with no read of the Git host")
	case live.Status.Phase != api.PhaseNeedsIntervention || approved == nil ||
		approved.Reason != ReasonMergeContentUnreadable:
		m.fatalf("end: the last read failed and the request is %s (%s); want NeedsIntervention with "+
			"MergeContentUnreadable", live.Status.Phase, reasonOf(approved))
	case !strings.Contains(approved.Message, "the read after the wait failed"):
		m.fatalf("message: %q does not say the read after the wait failed", approved.Message)
	}
	m.report()
}

// The ninth decision of 2026-11-18 on #771, points 1 and 5: the content wait ends with MergeContentUnreadable, the pull
// request marked merged (ChangeRequestMerged), and writes are then paused. The merge is a recorded fact, as main's
// status rule told it: the lock is released in the write that ends the request, before the pause.
func TestSequenceContentUnreadableFreesTheTargetUnderAPause(t *testing.T) {
	m := contentWaitWithFailingReads(t)
	m.clock.at = gitWait(m.live(), waitMergeContent).Ends.Add(time.Second)
	_, live, err := m.once()
	opened := meta.FindStatusCondition(live.Status.Conditions, api.ConditionPROpened)
	if err != nil || live.Status.Phase != api.PhaseNeedsIntervention || opened == nil ||
		opened.Reason != ReasonChangeRequestMerged {
		t.Fatalf("setup: want NeedsIntervention with the pull request marked merged, got %v, phase %s, %s", err,
			live.Status.Phase, reasonOf(opened))
	}
	m.pause(true)
	m.steps(2)
	if live := m.live(); holdsLock(live) {
		m.fatalf("lock: the merge with unreadable content is recorded (%s) and the lock is held under the pause",
			conditionsOf(live))
	}
	m.report()
}

// The ninth decision of 2026-11-18 on #771, point 5: a pull request that Shadewright did not push the head of is
// merged (MergedWithForeignChange, PullRequestOpened ChangeRequestMerged), then the repository leaves the settings.
// The merge is recorded, so it frees the target without a read of the branch, as main's settleLock did.
func TestSequenceForeignMergeFreesTheTargetWithTheRepositoryGone(t *testing.T) {
	r, _, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	markClosed(rr, ReasonChangeRequestMerged, "was merged although it failed a check")
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionApproved,
		Status: metav1.ConditionFalse, Reason: ReasonMergedWithForeignChange, Message: "merged with a foreign change"})
	rr.Status.HoldsTarget = new(true)
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	settings := &api.RemediationSettings{}
	if err := r.Get(ctx, client.ObjectKey{Name: api.SettingsName}, settings); err != nil {
		t.Fatal(err)
	}
	settings.Spec.Repositories = nil
	if err := r.Update(ctx, settings); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
			t.Fatal(err)
		}
	}
	live := &api.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
		t.Fatal(err)
	}
	if holdsLock(live) {
		t.Fatalf("the merge with a foreign change is recorded (%s) and the target is held with the repository gone",
			conditionsOf(live))
	}
}

// The ninth decision of 2026-11-18 on #771, point 1, against a lost write: a merge without its commit, a pause past the
// wait's end, the lift, and a status write lost at the lift (the first write of the reconcile, or applied and reported
// failed, or a restart after it), while the reads keep failing. The MergeCheck wait is never dropped, so the next
// reconcile reads once more and ends it.
func TestSequenceMergeCheckSurvivesALostWriteAtTheLift(t *testing.T) {
	for _, c := range []struct {
		name    string
		fault   func(m *modelWorld)
		restart bool
	}{
		{"write lost", func(m *modelWorld) { m.loseNext = true }, false},
		{"write applied, reported failed", func(m *modelWorld) { m.applyNext = true }, false},
		{"restart after the lost write", func(m *modelWorld) { m.loseNext = true }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := mergeCheckWithFailingReads(t, false)
			start := gitWait(m.live(), waitMergeCheck).Since.Time
			m.pause(true)
			m.clock.at = start.Add(mergeCommitWait + time.Minute)
			m.step()
			m.pause(false)
			c.fault(m)
			m.step()
			if c.restart {
				m.restart()
			}
			for range 3 {
				if mergeRecorded(m.live()) {
					break
				}
				m.step()
			}
			if live := m.live(); !mergeRecorded(live) {
				m.fatalf("end: the merge is not recorded three reconciles after the lift (phase %s, waits %v)",
					live.Status.Phase, live.Status.GitWaits)
			}
			m.report()
		})
	}
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
