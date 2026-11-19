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
	"time"

	"k8s.io/apimachinery/pkg/api/meta"

	api "github.com/pkalin/shadewright/api/v1alpha1"
)

// #884: a pass that stops because the repository of the plan left the settings is evidence that the last read of a
// wait past its end could not start (endAfterLastRead), whichever path met it. The close of a cancel and the cleanup
// of a stale plan find the repository gone in closeOwn, not in planEntry; before #884 only planEntry noted it.

// Case 1 (seeds 12471, 64347, 76857 of TestGitWaitModel): a cancelled request in PolicyChecked whose pull request was
// merged on the branch the settings name after a change of its target branch, while the reads of what the merge
// brought fail, so its MergeContent wait stands. The repository then leaves the settings past the wait's end: the
// close of the cancel fails with errRepositoryGone and goes to the cancel watch. That reconcile ends the wait
// (MergeContentUnreadable, saying no read could start), instead of keeping it on every reconcile from then on.
func TestCancelWatchEndsAReadWaitWhenTheRepositoryIsGone(t *testing.T) {
	m := scripted(t, modelStart{start: startPolicyChecked})
	m.cancel()
	m.setConfigured("release")
	m.fault, m.lastingUntil = faultBetween, m.clock.at.Add(24*time.Hour)
	m.steps(1)
	if err := m.g.SetBase(m.number, m.configured); err != nil {
		t.Fatal(err)
	}
	sha, err := m.g.Merge(m.number)
	if err != nil {
		t.Fatal(err)
	}
	m.merge = sha
	m.steps(1)
	w := gitWait(m.live(), waitMergeContent)
	if w == nil {
		t.Fatalf("setup: want the content wait recorded, got waits %v, phase %s", m.live().Status.GitWaits,
			m.live().Status.Phase)
	}
	m.clock.at = w.Ends.Add(time.Second)
	m.removeRepository()
	_, live, err := m.once()
	approved := meta.FindStatusCondition(live.Status.Conditions, api.ConditionApproved)
	switch {
	case err != nil:
		t.Fatalf("reconcile: %v", err)
	case gitWait(live, waitMergeContent) != nil:
		m.fatalf("end: the content wait passed its end, no read could start, and it stands (phase %s, %s%s)",
			live.Status.Phase, reasonOf(approved), conditionsOf(live))
	case approved == nil || approved.Reason != ReasonMergeContentUnreadable ||
		!strings.Contains(approved.Message, "no read of the Git host could start"):
		m.fatalf("end: %s; want MergeContentUnreadable saying no read could start", reasonOf(approved))
	}
	m.report()
}

// Case 2 (seed 107262): a request in PolicyChecked whose pull request was merged without its merge commit, and whose
// plan is stale (TargetFileChanged), waits in MergeCheck. The repository leaves the settings past the wait's end: the
// cleanup of the stale plan finds it gone and ends the request NeedsIntervention. The same reconcile ends the wait,
// not the one after it.
func TestStaleCleanupEndsAReadWaitWhenTheRepositoryIsGone(t *testing.T) {
	m := scripted(t, modelStart{start: startPolicyChecked, open: true})
	m.steps(1)
	m.fault, m.lastingUntil = faultNone, time.Time{}
	m.steps(1)
	sha, err := m.g.Merge(m.number)
	if err != nil {
		t.Fatal(err)
	}
	m.merge, m.noCommit = sha, true
	m.steps(1)
	w := gitWait(m.live(), waitMergeCheck)
	if w == nil {
		t.Fatalf("setup: want the MergeCheck wait recorded, got waits %v, phase %s", m.live().Status.GitWaits,
			m.live().Status.Phase)
	}
	m.clock.at = w.Ends.Add(time.Second)
	m.removeRepository()
	_, live, err := m.once()
	switch {
	case err != nil:
		t.Fatalf("reconcile: %v", err)
	case gitWait(live, waitMergeCheck) != nil:
		m.fatalf("end: the MergeCheck wait passed its end, no read could start, and it stands after the reconcile "+
			"(phase %s%s)", live.Status.Phase, conditionsOf(live))
	}
	m.report()
}
