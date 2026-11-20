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

	"github.com/go-logr/logr/funcr"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// ownClosed is the state of stoppedHolderWorld's own pull request closed on the target branch.
const ownClosed = "closed"

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
	for _, own := range []string{ownClosed, "closed on another base", "merged on another base", "open on another base"} {
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
