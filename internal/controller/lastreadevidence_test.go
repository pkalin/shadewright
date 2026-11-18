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
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// The eleventh decision of 2026-11-18 on #771, point 1: before the end of a wait for a read, answers that disagree
// keep it; past its end they are evidence that the read was made, and the wait ends as a failure saying so. The
// sequence: a request stopped after its target branch changed, its pull request retargeted to the branch the settings
// name now and merged there, the content wait (and in the second case the merge check) recorded; then every read of
// the pull request by its number answers 404 while the listing of its branch still reports it, for 48 hours with the
// repository configured and no pause. The wait ends at the first reconcile past its end, and the lock then settles by
// its rule.
func TestPassedReadWaitEndsOnAnswersThatDisagree(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T) *modelWorld
		kind  waitKind
	}{
		{"content wait", contentWaitWithFailingReads, waitMergeContent},
		{"merge check", stoppedMergeCheck, waitMergeCheck},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := c.setup(t)
			m.fault, m.lastingUntil = opGet404, m.clock.at.Add(72*time.Hour)
			// Before the end the answers that disagree keep the wait: the bound gives them time to agree.
			if end := gitWait(m.live(), c.kind).Ends.Add(-time.Minute); m.clock.at.After(end) {
				m.clock.at = end
			}
			res, live, err := m.once()
			switch {
			case err != nil:
				t.Fatalf("reconcile before the end: %v", err)
			case gitWait(live, c.kind) == nil:
				m.fatalf("before the end: answers that disagree ended the %s wait (phase %s)", gitWaitKinds[c.kind],
					live.Status.Phase)
				m.report()
				return
			case res.RequeueAfter <= 0:
				m.fatalf("before the end: the kept wait asks for no time to run again")
			}
			m.clock.at = gitWait(live, c.kind).Ends.Add(time.Second)
			// One reconcile a minute for 48 hours, at most.
			const reconciles = 48 * 60
			n := 0
			for ; n < reconciles && gitWait(m.live(), c.kind) != nil; n++ {
				if _, _, err := m.once(); err != nil {
					t.Fatalf("reconcile %d past the end: %v", n+1, err)
				}
				m.clock.advance(time.Minute)
			}
			live = m.live()
			switch {
			case gitWait(live, c.kind) != nil:
				m.fatalf("end: the %s wait stands %d reconciles past its end while the answers disagree (holds %t)",
					gitWaitKinds[c.kind], n, holdsLock(live))
			case n != 1:
				m.fatalf("end: the %s wait ended after %d reconciles past its end; want the first", gitWaitKinds[c.kind], n)
			case !conditionSays(live, "answers about its pull request disagreed"):
				m.fatalf("end: no condition says the answers disagreed (phase %s%s)", live.Status.Phase, conditionsOf(live))
			}
			// The lock then settles by its rule once the answers agree: a read of the branch shows no other open pull
			// request (answers that disagree keep a lock, heldUnsettled).
			m.fault, m.lastingUntil = faultNone, time.Time{}
			for range 5 {
				if !holdsLock(m.live()) {
					break
				}
				if _, _, err := m.once(); err != nil {
					t.Fatalf("reconcile after the end: %v", err)
				}
				m.clock.advance(time.Minute)
			}
			if live = m.live(); holdsLock(live) {
				m.fatalf("lock: the lock is held after the wait ended (phase %s%s)", live.Status.Phase, conditionsOf(live))
			}
			m.report()
		})
	}
}

// conditionSays reports whether a condition of rr has text in its message.
func conditionSays(rr *api.RemediationRequest, text string) bool {
	for _, c := range rr.Status.Conditions {
		if strings.Contains(c.Message, text) {
			return true
		}
	}
	return false
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

// The eleventh decision of 2026-11-18 on #771, point 3: with the RemediationSettings object missing, the last read of
// a content wait past its end cannot be made, but that is no evidence that the repository left the settings. The wait
// is kept and the request comes back at the poll pace; once the settings are back, the wait ends on its read.
func TestLastReadKeepsTheWaitWithoutSettings(t *testing.T) {
	m := contentWaitPassed(t)
	ctx := context.Background()
	settings := &api.RemediationSettings{}
	if err := m.base.Get(ctx, client.ObjectKey{Name: api.SettingsName}, settings); err != nil {
		t.Fatal(err)
	}
	if err := m.base.Delete(ctx, settings); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		res, live, err := m.once()
		approved := meta.FindStatusCondition(live.Status.Conditions, api.ConditionApproved)
		switch {
		case err != nil:
			t.Fatalf("reconcile: %v", err)
		case gitWait(live, waitMergeContent) == nil:
			m.fatalf("end: the content wait ended while RemediationSettings does not exist (phase %s, %s)",
				live.Status.Phase, reasonOf(approved))
		case !holdsLock(live):
			m.fatalf("lock: the lock was released while RemediationSettings does not exist")
		case res.RequeueAfter < time.Second || res.RequeueAfter > m.r.pullRequestPollInterval():
			m.fatalf("pace: the kept wait comes back after %v; want between a second and the poll interval",
				res.RequeueAfter)
		}
		m.clock.advance(time.Minute)
	}
	m.report()
}
