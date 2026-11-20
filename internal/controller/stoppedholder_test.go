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
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// ownClosed is the state of stoppedHolderWorld's own pull request closed on the target branch.
const ownClosed = "closed"

// ownOpenElsewhere is the state of stoppedHolderWorld's own pull request open on another base.
const ownOpenElsewhere = "open on another base"

// stoppedHolderWorld is a stopped holder (NeedsIntervention, lock held) with reviewer hints on whose recorded head has
// a passing review check written before the stop. own puts its own pull request in that state; another pull request
// from its branch, without the marker, is open on the target branch and keeps the lock.
func stoppedHolderWorld(t *testing.T, own string) (*RemediationRequestReconciler, *fakegit.Git,
	func() *api.RemediationRequest, string) {
	t.Helper()
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	r.ReviewerHints = true
	r.Recorder = record.NewFakeRecorder(100)
	setTargetMain(t, r)
	pr := rr.Status.PullRequest
	head := pr.HeadSHA
	if err := g.CreateCheckRun(ctx, head, providers.CheckRun{Name: providers.CheckRunName,
		Status: providers.CheckRunSuccess, Title: "passed before the stop"}); err != nil {
		t.Fatal(err)
	}
	if own != ownClosed {
		if err := g.CreateBranch(ctx, movedBranch, head); err != nil {
			t.Fatal(err)
		}
		if err := g.SetBase(pr.Number, movedBranch); err != nil {
			t.Fatal(err)
		}
	}
	switch own {
	case ownClosed, "closed on another base":
		if err := g.Close(pr.Number); err != nil {
			t.Fatal(err)
		}
	case "merged on another base":
		if _, err := g.MergeWithCommit(pr.Number); err != nil {
			t.Fatal(err)
		}
	}
	cr, err := g.GetChangeRequest(ctx, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.AddOpenChangeRequest(cr.HeadBranch, mainBranch, "opened by a person"); err != nil {
		t.Fatal(err)
	}
	get := func() *api.RemediationRequest {
		t.Helper()
		live := &api.RemediationRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
			t.Fatal(err)
		}
		return live
	}
	return r, g, get, head
}

// The one rule of the decision of 2026-11-20 on #925: a stopped holder whose lock another open pull request from its
// branch keeps fails the review check on its recorded head at its look, whatever its own pull request is (closed on
// the target branch, closed, merged or open on another base): a pass written before the stop must not stay last there,
// since that pull request can merge the head on it. On the branch before this rule, the look wrote nothing for its own
// pull request closed or merged, and the pass stayed (pipeline findings 1 and 8).
func TestStoppedHolderFailsItsCheckWhateverItsOwnPullRequest(t *testing.T) {
	for _, own := range []string{ownClosed, "closed on another base", "merged on another base", ownOpenElsewhere} {
		t.Run(own, func(t *testing.T) {
			r, g, get, head := stoppedHolderWorld(t, own)
			if _, err := r.checkHeld(context.Background(), get()); err != nil {
				t.Fatalf("checkHeld: %v", err)
			}
			live := get()
			if !holdsLock(live) {
				t.Fatalf("the lock was released while another pull request from the branch is open: %s",
					conditionsOf(live))
			}
			last := lastReviewCheck(g.CheckRuns(head))
			if last == nil || last.Status != providers.CheckRunFailure || last.Title != stoppedCheckTitle {
				t.Fatalf("check runs on the recorded head %+v: want the stop's failed check last", g.CheckRuns(head))
			}
			if !staleOnHead(live) || meta.FindStatusCondition(live.Status.Conditions,
				api.ConditionReviewCheckNotFailed) != nil {
				t.Fatalf("Stale on head %v, conditions %s: want Stale recorded and no ReviewCheckNotFailed",
					staleOnHead(live), conditionsOf(live))
			}
			// The next look reads the failed check last and writes nothing more.
			runs := len(g.CheckRuns(head))
			if _, err := r.checkHeld(context.Background(), get()); err != nil {
				t.Fatalf("checkHeld: %v", err)
			}
			if got := g.CheckRuns(head); len(got) != runs {
				t.Fatalf("the second look wrote %+v: want nothing while the failed check is last", got[runs:])
			}
		})
	}
}

// A refused write of the stopped holder's failed check sets ReviewCheckNotFailed, and the next look while the lock is
// held tries it again and removes the condition once it is written (decision of 2026-11-20 on #925, "one rule").
func TestStoppedHolderRetriesARefusedCheckWhileTheLockIsHeld(t *testing.T) {
	r, g, get, head := stoppedHolderWorld(t, ownClosed)
	refuse := true
	g.SetFail(func(op string) error {
		if refuse && op == opCreateCheckRun {
			return &providers.RetryableError{Err: errors.New("status 502: bad gateway")}
		}
		return nil
	})
	if _, err := r.checkHeld(context.Background(), get()); err != nil {
		t.Fatalf("checkHeld: %v", err)
	}
	if c := meta.FindStatusCondition(get().Status.Conditions, api.ConditionReviewCheckNotFailed); c == nil ||
		c.Reason != ReasonGitFailed {
		t.Fatalf("ReviewCheckNotFailed %+v after a refused write: want it with reason %s", c, ReasonGitFailed)
	}
	refuse = false
	if _, err := r.checkHeld(context.Background(), get()); err != nil {
		t.Fatalf("checkHeld: %v", err)
	}
	live := get()
	if last := lastReviewCheck(g.CheckRuns(head)); last == nil || last.Status != providers.CheckRunFailure {
		t.Fatalf("check runs %+v: want the failed check last after the retry", g.CheckRuns(head))
	}
	if c := meta.FindStatusCondition(live.Status.Conditions, api.ConditionReviewCheckNotFailed); c != nil {
		t.Fatalf("ReviewCheckNotFailed %+v after the write: want it removed", c)
	}
}

// deletedChangeRequest is the repository seen through gitView with change request n deleted from the host: the read
// by its number answers 404 and no listing shows it.
type deletedChangeRequest struct {
	providers.GitProvider
	n int64
}

func (d *deletedChangeRequest) GetChangeRequest(ctx context.Context, n int64) (providers.ChangeRequest, error) {
	if n == d.n {
		return providers.ChangeRequest{}, fmt.Errorf("pull request %d: %w", n, providers.ErrNotFound)
	}
	return d.GitProvider.GetChangeRequest(ctx, n)
}

func (d *deletedChangeRequest) ListChangeRequests(ctx context.Context, head string, openOnly bool) (
	[]providers.ChangeRequest, error) {
	all, err := d.GitProvider.ListChangeRequests(ctx, head, openOnly)
	return slices.DeleteFunc(all, func(cr providers.ChangeRequest) bool { return cr.Number == d.n }), err
}

func (d *deletedChangeRequest) ListChangeRequestsAnyBase(ctx context.Context, head string, openOnly bool) (
	[]providers.ChangeRequest, error) {
	all, err := d.GitProvider.ListChangeRequestsAnyBase(ctx, head, openOnly)
	return slices.DeleteFunc(all, func(cr providers.ChangeRequest) bool { return cr.Number == d.n }), err
}

// Seed 1456 of TestGitWaitModel (#925, "one rule"): a stopped holder whose own pull request was deleted while another
// open pull request from its branch keeps the lock. The first look that finds the number missing settles nothing
// (errGoneUnconfirmed: only a later look may conclude gone), and it leaves the lock held, so the one rule applies to it
// as to any look whose reads worked: the failed check is written on the recorded head in that look, not one look
// (heldRecheckInterval) later. Before, the unsettled look wrote nothing and the pass stayed last for that look.
func TestStoppedHolderFailsItsCheckAtAnUnconfirmedGoneLook(t *testing.T) {
	r, g, get, head := stoppedHolderWorld(t, ownClosed)
	n := get().Status.PullRequest.Number
	open := r.OpenRepository
	r.OpenRepository = func(ctx context.Context, repo api.GitRepository) (providers.GitProvider, error) {
		git, err := open(ctx, repo)
		if err != nil {
			return nil, err
		}
		return &deletedChangeRequest{GitProvider: git, n: n}, nil
	}
	if _, err := r.checkHeld(context.Background(), get()); err != nil {
		t.Fatalf("checkHeld: %v", err)
	}
	live := get()
	if !holdsLock(live) {
		t.Fatalf("the first look at a missing number released the lock: %s", conditionsOf(live))
	}
	if last := lastReviewCheck(g.CheckRuns(head)); last == nil || last.Status != providers.CheckRunFailure ||
		last.Title != stoppedCheckTitle {
		t.Fatalf("check runs on the recorded head %+v after the first look at a missing number: want the stop's "+
			"failed check last", g.CheckRuns(head))
	}
	if !staleOnHead(live) || meta.FindStatusCondition(live.Status.Conditions,
		api.ConditionReviewCheckNotFailed) != nil {
		t.Fatalf("Stale on head %v, conditions %s: want Stale recorded and no ReviewCheckNotFailed",
			staleOnHead(live), conditionsOf(live))
	}
}

// Pipeline finding 10 on #925: a release of the lock while condition ReviewCheckNotFailed is set ends the condition,
// and a check written before the stop may stay the latest on the head; the release says so in the log at Info
// (releaseLock). Before, the condition went with no log line and no Event.
func TestLockReleaseLogsAFailedCheckLeftUnwritten(t *testing.T) {
	r, g, get, _ := stoppedHolderWorld(t, ownClosed)
	g.SetFail(func(op string) error {
		if op == opCreateCheckRun {
			return &providers.RetryableError{Err: errors.New("status 502: bad gateway")}
		}
		return nil
	})
	if _, err := r.checkHeld(context.Background(), get()); err != nil {
		t.Fatalf("checkHeld: %v", err)
	}
	if meta.FindStatusCondition(get().Status.Conditions, api.ConditionReviewCheckNotFailed) == nil {
		t.Fatal("setup: ReviewCheckNotFailed is not set after a refused write")
	}
	for _, cr := range g.ChangeRequests() {
		if cr.State == providers.ChangeRequestOpen {
			if err := g.Close(cr.Number); err != nil {
				t.Fatal(err)
			}
		}
	}
	var mu sync.Mutex
	var lines []string
	ctx := logf.IntoContext(context.Background(), funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, args)
	}, funcr.Options{}))
	if _, err := r.checkHeld(ctx, get()); err != nil {
		t.Fatalf("checkHeld: %v", err)
	}
	live := get()
	if holdsLock(live) || meta.FindStatusCondition(live.Status.Conditions, api.ConditionReviewCheckNotFailed) != nil {
		t.Fatalf("holds %v, conditions %s: want the lock released and the condition gone", holdsLock(live),
			conditionsOf(live))
	}
	mu.Lock()
	defer mu.Unlock()
	for _, l := range lines {
		if strings.Contains(l, "whose failed review check was not written") {
			return
		}
	}
	t.Fatalf("log lines %q: want one saying the failed review check was not written at the release", lines)
}

// reinstalledStoppedHolder makes stoppedHolderWorld's request a stale cleanup stopped on a refusal of the Git host
// (staleCleanupStopped), whose plan's repository the settings now name under a new installation of the App (#290), and
// shows its own pull request deleted from the host, so the next look is a first look at a missing number.
func reinstalledStoppedHolder(t *testing.T, r *RemediationRequestReconciler, get func() *api.RemediationRequest) {
	t.Helper()
	reinstallStoppedHolder(t, r, get)
	n := get().Status.PullRequest.Number
	open := r.OpenRepository
	r.OpenRepository = func(ctx context.Context, repo api.GitRepository) (providers.GitProvider, error) {
		git, err := open(ctx, repo)
		if err != nil {
			return nil, err
		}
		return &deletedChangeRequest{GitProvider: git, n: n}, nil
	}
}

// reinstallStoppedHolder makes stoppedHolderWorld's request a stale cleanup stopped on a refusal of the Git host
// (staleCleanupStopped), whose plan's repository the settings now name under a new installation of the App (#290).
func reinstallStoppedHolder(t *testing.T, r *RemediationRequestReconciler, get func() *api.RemediationRequest) {
	t.Helper()
	ctx := context.Background()
	live := get()
	meta.SetStatusCondition(&live.Status.Conditions, metav1.Condition{Type: api.ConditionPlanFresh,
		Status: metav1.ConditionFalse, Reason: ReasonGitForbidden, Message: "the close was refused"})
	if err := r.Status().Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	s := &api.RemediationSettings{}
	if err := r.Get(ctx, client.ObjectKey{Name: api.SettingsName}, s); err != nil {
		t.Fatal(err)
	}
	s.Spec.Repositories[0].GitHub.InstallationID++
	if err := r.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, reinstalled, ok := cleanupRepository(s, get(), true); !ok || !reinstalled || !staleCleanupStopped(get()) {
		t.Fatal("setup: the request does not reach its repository through a new installation")
	}
}

// reversedListing is the repository seen through gitView with every listing of change requests in reverse order, so
// the open pull request opened last comes first.
type reversedListing struct{ providers.GitProvider }

func (l *reversedListing) ListChangeRequests(ctx context.Context, head string, openOnly bool) (
	[]providers.ChangeRequest, error) {
	all, err := l.GitProvider.ListChangeRequests(ctx, head, openOnly)
	slices.Reverse(all)
	return all, err
}

func (l *reversedListing) ListChangeRequestsAnyBase(ctx context.Context, head string, openOnly bool) (
	[]providers.ChangeRequest, error) {
	all, err := l.GitProvider.ListChangeRequestsAnyBase(ctx, head, openOnly)
	slices.Reverse(all)
	return all, err
}

// The decision of 2026-11-21 on #925 (one rule, no installation exception, ADR-0046): a stopped holder that reaches
// its repository through a new installation and whose look settles the lock held writes the stop's failed check on
// the recorded head, whichever pull request from its branch keeps the lock and in whatever order the Git host lists
// them. Before, a settled look wrote nothing when the pull request chosen as holding the lock was not the recorded one:
// an unmarked open pull request kept the pass on the head, also when listed before the recorded one, open as well.
func TestStoppedHolderFailsItsCheckThroughANewInstallationAtASettledLook(t *testing.T) {
	cases := []struct {
		name, own string
		// reversed lists the unmarked pull request before the recorded one; alone closes the unmarked one.
		reversed, alone bool
	}{
		{name: "own closed, an unmarked one keeps the lock", own: ownClosed},
		{name: "own open on another base, an unmarked one listed first", own: ownOpenElsewhere, reversed: true},
		{name: "own open on another base, an unmarked one listed after", own: ownOpenElsewhere},
		{name: "own open on another base, no unmarked one", own: ownOpenElsewhere, alone: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, g, get, head := stoppedHolderWorld(t, tc.own)
			if tc.alone {
				for _, cr := range g.ChangeRequests() {
					if cr.State == providers.ChangeRequestOpen && cr.Number != get().Status.PullRequest.Number {
						if err := g.Close(cr.Number); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			reinstallStoppedHolder(t, r, get)
			if tc.reversed {
				open := r.OpenRepository
				r.OpenRepository = func(ctx context.Context, repo api.GitRepository) (providers.GitProvider, error) {
					git, err := open(ctx, repo)
					if err != nil {
						return nil, err
					}
					return &reversedListing{GitProvider: git}, nil
				}
			}
			if _, err := r.checkHeld(context.Background(), get()); err != nil {
				t.Fatalf("checkHeld: %v", err)
			}
			live := get()
			if !holdsLock(live) {
				t.Fatalf("the look released the lock an open pull request keeps: %s", conditionsOf(live))
			}
			if last := lastReviewCheck(g.CheckRuns(head)); last == nil || last.Status != providers.CheckRunFailure ||
				last.Title != stoppedCheckTitle {
				t.Fatalf("check runs on the recorded head %+v: want the stop's failed check last", g.CheckRuns(head))
			}
			if !staleOnHead(live) || meta.FindStatusCondition(live.Status.Conditions,
				api.ConditionReviewCheckNotFailed) != nil {
				t.Fatalf("Stale on head %v, conditions %s: want Stale recorded and no ReviewCheckNotFailed",
					staleOnHead(live), conditionsOf(live))
			}
		})
	}
}

// Finding DR-8 of the second ADR run on #925 (ADR-0046): a stopped holder that reaches its repository through a new
// installation, whose first look finds its pull request number missing, settles nothing and leaves the lock held, and
// it does not read which pull request holds the lock, so it writes the stop's failed check on the recorded head.
func TestStoppedHolderFailsItsCheckThroughANewInstallationAtAnUnsettledLook(t *testing.T) {
	r, g, get, head := stoppedHolderWorld(t, ownClosed)
	reinstalledStoppedHolder(t, r, get)
	if _, err := r.checkHeld(context.Background(), get()); err != nil {
		t.Fatalf("checkHeld: %v", err)
	}
	live := get()
	if !holdsLock(live) {
		t.Fatalf("the first look at a missing number released the lock: %s", conditionsOf(live))
	}
	if last := lastReviewCheck(g.CheckRuns(head)); last == nil || last.Status != providers.CheckRunFailure ||
		last.Title != stoppedCheckTitle {
		t.Fatalf("check runs on the recorded head %+v: want the stop's failed check last", g.CheckRuns(head))
	}
	if !staleOnHead(live) || meta.FindStatusCondition(live.Status.Conditions,
		api.ConditionReviewCheckNotFailed) != nil {
		t.Fatalf("Stale on head %v, conditions %s: want Stale recorded and no ReviewCheckNotFailed",
			staleOnHead(live), conditionsOf(live))
	}
}

// Finding DR-2 of the second ADR run on #925, under the decision of 2026-11-21 (ADR-0046, one rule with no
// installation exception): the same stopped holder whose write at the unsettled look was refused
// (ReviewCheckNotFailed set) looks again; that look settles the lock, held by the other open pull request from the
// branch. It writes the stop's failed check on the recorded head again, records Stale and removes the condition.
// Before, it wrote nothing, since the new installation does not tie that pull request to the request, and the pass
// written before the stop stayed last.
func TestStoppedHolderRetriesItsCheckThroughANewInstallationAtASettledLook(t *testing.T) {
	r, g, get, head := stoppedHolderWorld(t, ownClosed)
	reinstalledStoppedHolder(t, r, get)
	refuse := true
	g.SetFail(func(op string) error {
		if refuse && op == opCreateCheckRun {
			return &providers.RetryableError{Err: errors.New("status 502: bad gateway")}
		}
		return nil
	})
	if _, err := r.checkHeld(context.Background(), get()); err != nil {
		t.Fatalf("checkHeld: %v", err)
	}
	if meta.FindStatusCondition(get().Status.Conditions, api.ConditionReviewCheckNotFailed) == nil {
		t.Fatalf("setup: ReviewCheckNotFailed is not set after a refused write: %s", conditionsOf(get()))
	}
	refuse = false
	// The second look comes once the wait for a missing number is over, so it concludes gone and settles the lock.
	n := get().Status.PullRequest.Number
	first, ok := r.looks().first(get(), n)
	if !ok {
		t.Fatal("setup: the first look at the missing number is not kept")
	}
	r.looks().note(get(), n, first.Add(-confirmWait-time.Second))
	if _, err := r.checkHeld(context.Background(), get()); err != nil {
		t.Fatalf("checkHeld: %v", err)
	}
	live := get()
	if !holdsLock(live) {
		t.Fatalf("the settled look released the lock another open pull request keeps: %s", conditionsOf(live))
	}
	if last := lastReviewCheck(g.CheckRuns(head)); last == nil || last.Status != providers.CheckRunFailure ||
		last.Title != stoppedCheckTitle {
		t.Fatalf("check runs on the recorded head %+v: want the stop's failed check last", g.CheckRuns(head))
	}
	if !staleOnHead(live) || meta.FindStatusCondition(live.Status.Conditions,
		api.ConditionReviewCheckNotFailed) != nil {
		t.Fatalf("Stale on head %v, conditions %s: want Stale recorded and no ReviewCheckNotFailed",
			staleOnHead(live), conditionsOf(live))
	}
}

// Finding RT-1 of the second ADR run on #925 (#984, a known limit of ADR-0046): the deletion of a stopped holder ends
// condition ReviewCheckNotFailed and writes no check, so a check written before the stop may stay on an open pull
// request from its branch; the first write of the deletion says so in the log at Info. Before, it went silently.
func TestDeletionLogsTheEndOfReviewCheckNotFailed(t *testing.T) {
	r, _, get, _ := stoppedHolderWorld(t, ownClosed)
	live := get()
	meta.SetStatusCondition(&live.Status.Conditions, metav1.Condition{Type: api.ConditionReviewCheckNotFailed,
		Status: metav1.ConditionTrue, Reason: ReasonGitFailed, Message: notWrittenYet})
	if err := r.Status().Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	ctx, lines := loggedCtx()
	if _, _, err := r.beginFinalize(ctx, get()); err != nil {
		t.Fatalf("beginFinalize: %v", err)
	}
	if c := meta.FindStatusCondition(get().Status.Conditions, api.ConditionReviewCheckNotFailed); c != nil {
		t.Fatalf("ReviewCheckNotFailed %+v after the first write of the deletion: want it ended", c)
	}
	var found int
	for _, l := range *lines {
		if strings.Contains(l, "whose failed review check was not written") && strings.Contains(l, "being deleted") {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("%d Info lines say the condition ended at the deletion, want 1: %q", found, *lines)
	}
}

// Finding D3 of the ADR-0046 run on #925 (limit (a), #984): the deletion of a stopped holder writes no check, so
// whenever its recorded head is not known to show the stop's failed check (no state Stale recorded on it) a check
// written before the stop may stay there, and the first write of the deletion says so in the log at Info, with or
// without condition ReviewCheckNotFailed. A head recorded Stale is not logged. Before, only a set condition was.
func TestDeletionLogsAHeadNotKnownToShowTheFailedCheck(t *testing.T) {
	for _, stale := range []bool{false, true} {
		r, _, get, head := stoppedHolderWorld(t, ownClosed)
		if stale {
			live := get()
			live.Status.PullRequest.ReviewCheck = &api.ReviewCheck{Head: head, State: api.ReviewCheckStale}
			if err := r.Status().Update(context.Background(), live); err != nil {
				t.Fatal(err)
			}
		}
		ctx, lines := loggedCtx()
		if _, _, err := r.beginFinalize(ctx, get()); err != nil {
			t.Fatalf("stale %v: beginFinalize: %v", stale, err)
		}
		var found int
		for _, l := range *lines {
			if strings.Contains(l, "may stay the latest on its head") && strings.Contains(l, "being deleted") {
				found++
			}
		}
		if want := map[bool]int{false: 1, true: 0}[stale]; found != want {
			t.Fatalf("stale %v: %d Info lines say a check before the stop may stay, want %d: %q", stale, found, want,
				*lines)
		}
	}
}
