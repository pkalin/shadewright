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
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"

	api "github.com/pkalin/shadewright/api/v1alpha1"
)

// The tenth decision of 2026-11-18 on #771: a wait for a read ends only on evidence about its last read (it answered,
// it failed, or it provably could not start); every request with an open read wait makes that read, whether or not it
// holds its lock; a merge that did not deliver the plan as shown keeps the lock until a read settles it.

// contentWaitPassed is contentWaitWithFailingReads with the clock past the content wait's end.
func contentWaitPassed(t *testing.T) *modelWorld {
	t.Helper()
	m := contentWaitWithFailingReads(t)
	m.clock.at = gitWait(m.live(), waitMergeContent).Ends.Add(time.Second)
	return m
}

// Point 1: the last read of a content wait meets answers that disagree (the pull request is missing by its number and
// the branch does not confirm it). Every read answered, none failed, none was refused a start: the wait is kept and the
// request comes back at the poll pace, never ended as "no read could be made".
func TestLastReadKeepsTheWaitOnAnswersThatDisagree(t *testing.T) {
	m := contentWaitPassed(t)
	m.fault = opGet404
	res, live, err := m.once()
	approved := meta.FindStatusCondition(live.Status.Conditions, api.ConditionApproved)
	switch {
	case err != nil:
		t.Fatalf("reconcile: %v", err)
	case approved != nil && approved.Reason == ReasonMergeContentUnreadable:
		m.fatalf("end: answers that disagree ended the content wait: %q", approved.Message)
	case gitWait(live, waitMergeContent) == nil:
		m.fatalf("end: the content wait was removed without evidence about its last read (phase %s)", live.Status.Phase)
	case res.RequeueAfter <= 0 || res.RequeueAfter > m.r.pullRequestPollInterval():
		m.fatalf("pace: the kept wait comes back after %v; want at most the poll interval", res.RequeueAfter)
	case res.RequeueAfter < time.Second:
		m.fatalf("pace: the kept wait comes back at once (%v)", res.RequeueAfter)
	}
	// The reads then fail: that last read is evidence, and the wait ends there, saying so.
	m.fault = faultBetween
	_, live, err = m.once()
	approved = meta.FindStatusCondition(live.Status.Conditions, api.ConditionApproved)
	if err != nil || approved == nil || approved.Reason != ReasonMergeContentUnreadable ||
		!strings.Contains(approved.Message, "the read after the wait failed") {
		m.fatalf("end: the failed last read did not end the wait (err %v, %s)", err, reasonOf(approved))
	}
	m.report()
}

// Point 1: a pause set after the check at the start of the reconcile (endPassedWait) and lifted before the end of the
// wait reads the settings again (endAfterLastRead). The look stopped at the pause and read nothing: the wait is kept.
func TestLastReadKeepsTheWaitWhenAPauseComesAndGoesInTheReconcile(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T) *modelWorld
		kind  waitKind
	}{
		{"content wait", contentWaitPassed, waitMergeContent},
		{"merge check of a stopped holder", func(t *testing.T) *modelWorld {
			m := stoppedMergeCheck(t)
			m.fault, m.lastingUntil = opGet, m.clock.at.Add(24*time.Hour)
			m.clock.at = gitWait(m.live(), waitMergeCheck).Ends.Add(time.Second)
			return m
		}, waitMergeCheck},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := c.setup(t)
			m.flicker = true
			_, live, err := m.once()
			switch {
			case err != nil:
				t.Fatalf("reconcile: %v", err)
			case m.calls > 0:
				m.fatalf("pause: the look read the Git host %d times under the pause", m.calls)
			case gitWait(live, c.kind) == nil:
				m.fatalf("end: the %s wait ended with no read (phase %s, %s)", gitWaitKinds[c.kind], live.Status.Phase,
					reasonOf(meta.FindStatusCondition(live.Status.Conditions, api.ConditionApproved)))
			}
			m.report()
		})
	}
}

// stoppedMergeCheck is a request stopped for a change of its target branch whose pull request a person moved to the
// branch the settings name now and merged there while the host reports no merge commit: the MergeCheck wait is
// recorded, and the pull request is marked merged (ChangeRequestMerged). The lock is kept while that wait is open: the
// merge is not shown to deliver the plan yet (settleRecordedMerge).
func stoppedMergeCheck(t *testing.T) *modelWorld {
	t.Helper()
	m := scripted(t, modelStart{start: stopPlanFresh})
	if err := m.g.SetBase(m.number, m.configured); err != nil {
		t.Fatal(err)
	}
	sha, err := m.g.Merge(m.number)
	if err != nil {
		t.Fatal(err)
	}
	m.merge, m.noCommit = sha, true
	for range 3 {
		if gitWait(m.live(), waitMergeCheck) != nil {
			break
		}
		m.step()
	}
	if live := m.live(); gitWait(live, waitMergeCheck) == nil {
		t.Fatalf("setup: want the MergeCheck wait recorded, got waits %v, phase %s", live.Status.GitWaits,
			live.Status.Phase)
	}
	return m
}

// Point 2: a request stopped for a change of its target branch, its pull request retargeted and merged without a
// merge commit at first (MergeCheck and ChangeRequestMerged recorded). Its lock is released (as a status written by a
// build that released it from ChangeRequestMerged left it), and the merge commit becomes available. A request with an
// open wait for a read makes that read whether or not it holds its lock: past the wait's end the merge is recorded with
// its receipt, and no reconcile ends the wait without a read.
func TestOpenReadWaitIsReadAfterTheLockIsReleased(t *testing.T) {
	m := stoppedMergeCheck(t)
	live := m.live()
	end := gitWait(live, waitMergeCheck).Ends.Add(time.Second)
	live.Status.HoldsTarget = new(false)
	if err := m.base.Status().Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	m.noCommit = false
	m.clock.at = end
	for range 4 {
		if mergeRecorded(m.live()) {
			break
		}
		_, live, err := m.once()
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if mergeRecorded(live) && m.calls == 0 {
			m.fatalf("read: the merge was recorded by a reconcile that read nothing (phase %s, %s)", live.Status.Phase,
				reasonOf(meta.FindStatusCondition(live.Status.Conditions, api.ConditionApproved)))
		}
		m.clock.advance(time.Minute)
	}
	live = m.live()
	if !mergeRecorded(live) || live.Status.MergeReceipt == nil {
		m.fatalf("receipt: the merge commit was reported and the merge is not recorded with its receipt (phase %s, %s)",
			live.Status.Phase, reasonOf(meta.FindStatusCondition(live.Status.Conditions, api.ConditionApproved)))
	}
	m.report()
}

// Point 2 with the lock held: the open MergeCheck wait keeps the lock (the merge is not shown to deliver the plan
// yet), and the read of the wait settles both.
func TestOpenReadWaitKeepsTheLockUntilItsRead(t *testing.T) {
	m := stoppedMergeCheck(t)
	if !holdsLock(m.live()) {
		m.fatalf("lock: the lock was released while the wait for the merge's read is open")
	}
	m.noCommit = false
	m.steps(3)
	live := m.live()
	if !mergeRecorded(live) || live.Status.MergeReceipt == nil || holdsLock(live) {
		m.fatalf("end: phase %s, receipt %t, holds %t; want the merge recorded with its receipt and the lock free",
			live.Status.Phase, live.Status.MergeReceipt != nil, holdsLock(live))
	}
	m.report()
}

// Point 5: in a reconcile that makes the last read of a wait past its end, a failed read is no retry (retryGitRead):
// it records no entry and no new start, asks for no time to run again, and is noted, so the end that follows names it
// ("failed"). A plain retry there would leave the wait to be retried again past its end.
func TestFailedLastReadIsNoRetry(t *testing.T) {
	m := contentWaitPassed(t)
	ctx := context.Background()
	rr := m.live()
	before := slices.Clone(rr.Status.GitWaits)
	m.r.gitRetries().take(m.key)
	m.r.gitRetries().markLastRead(m.key, []waitKind{waitMergeContent})
	const op = "read the parents of the merge commit"
	res, err := m.r.retryGitRead(ctx, rr, readFailure(op, errModelOutage), unboundedRead(noBoundHeldLock))
	mark := m.r.gitRetries().take(m.key)
	live := m.live()
	switch {
	case err != nil || res.RequeueAfter != 0:
		t.Fatalf("retryGitRead = %+v, %v; want no retry and no error", res, err)
	case !equality.Semantic.DeepEqual(before, live.Status.GitWaits):
		t.Fatalf("retryGitRead wrote the waits %v; want them as they were, %v", live.Status.GitWaits, before)
	case mark.failed != op:
		t.Fatalf("the failed last read is not noted: %q", mark.failed)
	}
	_, outcome, err := m.r.endAfterLastRead(ctx, m.key, mark)
	live = m.live()
	approved := meta.FindStatusCondition(live.Status.Conditions, api.ConditionApproved)
	if err != nil || outcome != lastReadEnded || approved == nil || approved.Reason != ReasonMergeContentUnreadable ||
		!strings.Contains(approved.Message, "the read after the wait failed: "+op) {
		t.Fatalf("end after the failed last read: %v, outcome %d, %s; want MergeContentUnreadable naming the read",
			err, outcome, reasonOf(approved))
	}
}
