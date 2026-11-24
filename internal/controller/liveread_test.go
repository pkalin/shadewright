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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/providers"
	fakegit "github.com/shadewright-labs/shadewright/internal/providers/fake"
)

// A failed live read before a review check write never leaves a pass on the head that the reviews no longer give
// (decision of 2026-11-20 on #925, point 1): with the head passed, a gate that now wants the check pending (changes
// requested) or failed (a list of reviews too long to read) still writes it, and the look returns no error. A
// passing check is never written without the live read: that look writes nothing and returns the failed read.
func TestFailedLiveReadStillTakesThePassDown(t *testing.T) {
	cases := []struct {
		name string
		// world changes the passed world before the look.
		world func(t *testing.T, r *RemediationRequestReconciler, g *fakegit.Git, number int64, head string)
		want  providers.CheckRunStatus
		state api.ReviewCheckState
	}{
		{name: "changes requested", want: providers.CheckRunInProgress, state: api.ReviewCheckInProgress,
			world: func(t *testing.T, _ *RemediationRequestReconciler, g *fakegit.Git, number int64, head string) {
				requestChanges(g, number, head)
			}},
		{name: "list too long", want: providers.CheckRunFailure, state: api.ReviewCheckFailed,
			world: func(t *testing.T, r *RemediationRequestReconciler, g *fakegit.Git, _ int64, _ string) {
				r.OpenRepository = func(context.Context, api.GitRepository) (providers.GitProvider, error) {
					return pagedReviews{g}, nil
				}
			}},
		{name: "approved, host pending", want: providers.CheckRunSuccess,
			world: func(t *testing.T, _ *RemediationRequestReconciler, g *fakegit.Git, _ int64, head string) {
				if err := g.CreateCheckRun(context.Background(), head, checkRunFor(providers.CheckRunInProgress)); err != nil {
					t.Fatal(err)
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, g, p, number, head := passedWorld(t)
			ctx := context.Background()
			c.world(t, r, g, number, head)
			r.APIReader = failingRequestReader{Reader: r.Client, name: p.Name}
			runs := len(g.CheckRuns(head))
			err := r.reviewCheck(ctx, p)
			if c.want == providers.CheckRunSuccess {
				if !errors.Is(err, errReviewCheckLiveRead) {
					t.Fatalf("a pass without the live read returned %v: want errReviewCheckLiveRead", err)
				}
				if got := g.CheckRuns(head); len(got) != runs {
					t.Fatalf("a passing check was written without the live read: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("a check that does not pass, written after a failed live read, returned %v", err)
			}
			if got := lastCheckRun(g, head); got != c.want {
				t.Fatalf("the Git host shows %q after a failed live read: want %q, the pass taken down", got, c.want)
			}
			if rc := p.Status.PullRequest.ReviewCheck; rc == nil || rc.Head != head || rc.State != c.state {
				t.Fatalf("review check %+v: want %s recorded", rc, c.state)
			}
		})
	}
}

// Condition Approved with reason ReviewCheckLiveReadFailed is ended only by a look that wrote the check or found the
// Git host already showing what the reviews want (decision of 2026-11-20 on #925, point 3), not by a look that
// returned early without an error: one whose comments were behind, or one in the backoff of a refusal.
func TestLiveReadNoteKeptByALookThatWroteNothing(t *testing.T) {
	cases := []struct {
		name string
		// before changes the request after the note is set; comments is the look that follows.
		before   func(t *testing.T, r *RemediationRequestReconciler, p *api.RemediationRequest, head string)
		comments commentLook
	}{
		{name: "comments behind", comments: commentsBehind},
		{name: "refusal backoff", comments: commentsCaughtUp, before: func(t *testing.T, r *RemediationRequestReconciler,
			p *api.RemediationRequest, head string) {
			p.Status.PullRequest.ReviewCheck = &api.ReviewCheck{Head: head, State: api.ReviewCheckRefused, Refusals: 1,
				RefusedAt: &metav1.Time{Time: r.now().Add(-time.Second)}}
			if err := r.Status().Update(context.Background(), p); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, g, p, _, cr, head := gateWorld(t)
			ctx := context.Background()
			plainWaiting(t, r, p, head)
			if err := r.watchReviewCheck(ctx, p, commentsCaughtUp); err != nil {
				t.Fatal(err)
			}
			approve(g, cr.Number, 90, 3003, head)
			live := r.Client
			r.APIReader = failingRequestReader{Reader: live, name: p.Name}
			if err := r.watchReviewCheck(ctx, p, commentsCaughtUp); err != nil {
				t.Fatal(err)
			}
			if !liveReadNoted(t, live, p) {
				t.Fatal("the failed live read before a pass was not noted")
			}
			r.APIReader = live
			if c.before != nil {
				c.before(t, r, p, head)
			}
			runs := len(g.CheckRuns(head))
			if err := r.watchReviewCheck(ctx, p, c.comments); err != nil {
				t.Fatal(err)
			}
			if got := g.CheckRuns(head); len(got) != runs {
				t.Fatalf("the look wrote a check run: %+v", got)
			}
			if !liveReadNoted(t, live, p) {
				t.Fatal("a look that neither wrote the check nor found it in line ended the note")
			}
		})
	}
}

// liveReadNoted reports whether the stored request carries condition Approved with reason
// ReviewCheckLiveReadFailed.
func liveReadNoted(t *testing.T, c client.Client, p *api.RemediationRequest) bool {
	t.Helper()
	got := &api.RemediationRequest{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(p), got); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, api.ConditionApproved)
	return cond != nil && cond.Reason == ReasonReviewCheckLiveReadFailed
}

// A write of a stopped request's failed review check that the Git host refuses for good, or answers with a 404, is
// not shown as the passing failure GitFailed: condition ReviewCheckNotFailed names the refusal (GitForbidden,
// GitRejected, a token the installation refuses included), GitUnauthorized (a 401: the App's key is wrong or the
// App was deleted) or GitNotFound, so a person knows the next look will not help until it is fixed. A server error
// stays GitFailed.
func TestStoppedCheckRefusalIsNotGitFailed(t *testing.T) {
	cases := []struct {
		name string
		fail error
		want string
	}{
		{name: "forbidden", fail: fmt.Errorf("status 403: %w", providers.ErrForbidden), want: ReasonGitForbidden},
		{name: "token refused", fail: fmt.Errorf("installation token unavailable: %w", providers.ErrForbidden),
			want: ReasonGitForbidden},
		{name: "token rejected (401)", fail: fmt.Errorf("installation token unavailable: %w",
			providers.ErrUnauthorized), want: ReasonGitUnauthorized},
		{name: "rejected", fail: fmt.Errorf("status 422: %w", providers.ErrRejected), want: ReasonGitRejected},
		{name: "not found", fail: fmt.Errorf("status 404: %w", providers.ErrNotFound), want: ReasonGitNotFound},
		{name: "server error", fail: fmt.Errorf("status 502: %w", providers.ErrRetryable), want: ReasonGitFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, g, p, _, _, head := gateWorld(t)
			ctx := context.Background()
			plainWaiting(t, r, p, head)
			p.Status.Phase = api.PhaseNeedsIntervention
			p.Status.HoldsTarget = new(true)
			g.SetFail(func(op string) error {
				if op == opCreateCheckRun {
					return c.fail
				}
				return nil
			})
			r.Recorder = record.NewFakeRecorder(10)
			if err := r.failStoppedCheck(ctx, p, string(api.PhaseNeedsIntervention)); err == nil {
				t.Fatal("a failed write of the stopped check returned no error")
			}
			cond := meta.FindStatusCondition(p.Status.Conditions, api.ConditionReviewCheckNotFailed)
			if cond == nil || cond.Reason != c.want {
				t.Fatalf("ReviewCheckNotFailed %+v: want reason %s", cond, c.want)
			}
		})
	}
}

// Without the live read the only review check written is the downgrade of a pass (decision of 2026-11-20 on #925,
// after the fifth pass): pending or failed goes on the head only when its last check shadewright/review is a
// success. Over a failed or a pending check, or on a head with no check, a failed live read writes nothing and is
// returned for a retry, so a stop's failed check is never covered by a check written from a copy the API server may
// no longer hold.
func TestFailedLiveReadWritesOnlyOverAPass(t *testing.T) {
	cases := []struct {
		name string
		// last is the check written on the passed head before the look, "" for none; fresh: a head with no check.
		last  providers.CheckRunStatus
		fresh bool
		// listTooLong makes the gate want the check failed instead of pending.
		listTooLong bool
		// want is the check the look writes, "" for none.
		want providers.CheckRunStatus
	}{
		{name: "pending over a pass", want: providers.CheckRunInProgress},
		{name: "failed over a pass", listTooLong: true, want: providers.CheckRunFailure},
		{name: "pending over a failed check", last: providers.CheckRunFailure},
		{name: "failed over a pending check", last: providers.CheckRunInProgress, listTooLong: true},
		{name: "pending on a head with no check", fresh: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			var (
				r    *RemediationRequestReconciler
				g    *fakegit.Git
				p    *api.RemediationRequest
				head string
			)
			if c.fresh {
				r, g, p, _, _, head = gateWorld(t)
				plainWaiting(t, r, p, head)
			} else {
				var number int64
				r, g, p, number, head = passedWorld(t)
				requestChanges(g, number, head)
			}
			if c.last != "" {
				if err := g.CreateCheckRun(ctx, head, checkRunFor(c.last)); err != nil {
					t.Fatal(err)
				}
			}
			if c.listTooLong {
				r.OpenRepository = func(context.Context, api.GitRepository) (providers.GitProvider, error) {
					return pagedReviews{g}, nil
				}
			}
			r.APIReader = failingRequestReader{Reader: r.Client, name: p.Name}
			runs := g.CheckRuns(head)
			err := r.reviewCheck(ctx, p)
			got := g.CheckRuns(head)
			if c.want == "" {
				if !errors.Is(err, errReviewCheckLiveRead) {
					t.Fatalf("a failed live read returned %v: want errReviewCheckLiveRead for a retry", err)
				}
				if len(got) != len(runs) {
					t.Fatalf("a check was written without the live read over %q: %+v", c.last, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("the downgrade of a pass after a failed live read returned %v", err)
			}
			if len(got) != len(runs)+1 || got[len(got)-1].Status != c.want {
				t.Fatalf("check runs %+v after a failed live read over a pass: want %q written", got, c.want)
			}
		})
	}
}

// requestChanges adds a review by reviewer 3003 that requests changes on head, after passedWorld's approval.
func requestChanges(g *fakegit.Git, number int64, head string) {
	g.AddReview(number, providers.Feedback{ID: 91, Author: providers.FeedbackAuthor{ID: 3003,
		Login: "reviewer-3003", Kind: providers.AuthorPerson}, ReviewState: "CHANGES_REQUESTED",
		CommitID: head, CreatedAt: hostClock.Add(time.Minute)})
}
