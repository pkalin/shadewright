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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// The error paths of the handoff and the merge gate (#590).

// tookOverWorld is gateWorld after step 4: the replacement took the pull request over and waits for review,
// with the pull request not rewritten yet.
func tookOverWorld(t *testing.T) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest,
	*api.RemediationRequest, providers.ChangeRequest, string) {
	t.Helper()
	r, g, p, rr, cr, head := gateWorld(t)
	ctx := context.Background()
	if err := r.recordHandoff(ctx, rr, p, cr, head); err != nil {
		t.Fatal(err)
	}
	rr.Status.Phase = api.PhaseAwaitingApproval
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if err := r.supersedePredecessor(ctx, rr); err != nil {
		t.Fatal(err)
	}
	return r, g, p, rr, cr, head
}

// A replacement whose commit is on the pull request never expires because the repository could not be opened
// for one look: the recovery is retried, and the next look records the handoff.
func TestRecoveryRetriesWhenTheRepositoryCannotBeOpened(t *testing.T) {
	r, g, _, rr, _, head := gateWorld(t)
	ctx := context.Background()
	rr.Status.Deadline = &metav1.Time{Time: time.Now().Add(-time.Minute)}
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	r.OpenRepository = func(context.Context, api.GitRepository) (providers.GitProvider, error) {
		if opens.Add(1) == 1 {
			return nil, &providers.RetryableError{Err: errors.New("the token endpoint did not answer")}
		}
		return g, nil
	}
	if _, err := r.recheck(ctx, rr); err == nil {
		t.Fatal("a recovery that could not open the repository was not retried")
	}
	if rr.Status.Phase != api.PhasePolicyChecked {
		t.Fatalf("the replacement ended %s with its commit on the pull request", rr.Status.Phase)
	}
	if _, err := r.recheck(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if !tookOver(rr) || rr.Status.PullRequest == nil || rr.Status.PullRequest.HeadSHA != head {
		t.Fatalf("the retry did not record the handoff: phase %s, handoff %+v", rr.Status.Phase, rr.Status.Handoff)
	}
}

// A rewrite the provider refuses as invalid before it asks the host (a body over its limit) is final: it is
// recorded Refused and not asked again on every look.
func TestAfterHandoffRecordsAnInvalidRewriteAsFinal(t *testing.T) {
	r, g, _, rr, _, _ := tookOverWorld(t)
	ctx := context.Background()
	var calls atomic.Int32
	g.SetFail(func(op string) error {
		if op == opUpdateChangeRequest {
			calls.Add(1)
			return fmt.Errorf("update pull request: body is too long or not UTF-8: %w", providers.ErrRejected)
		}
		return nil
	})
	for range 2 {
		if err := r.afterHandoff(ctx, rr); err != nil {
			t.Fatal(err)
		}
	}
	if got := rr.Status.Handoff.From.PullRequestUpdate; got != api.HandoffStepRefused {
		t.Fatalf("the refused rewrite was recorded %q", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the refused rewrite was asked %d times", n)
	}
}

// A status write that fails after the rewrite is returned for a retry with backoff, not only logged.
func TestAfterHandoffReturnsAFailedStatusWrite(t *testing.T) {
	r, _, _, rr, _, _ := tookOverWorld(t)
	ctx, out := infoLog()
	base := r.Client
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if obj.GetName() == newName {
				return errors.New("etcd is down")
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	err := r.afterHandoff(ctx, rr)
	if err == nil || !strings.Contains(err.Error(), "etcd is down") {
		t.Fatalf("a failed status write after the rewrite was dropped: %v (log %q)", err, out.String())
	}
}

// A cancelled reconcile returns at once and is not logged as a failure of the steps after the handoff.
func TestAfterHandoffReturnsOnCancellationWithoutLogging(t *testing.T) {
	r, g, _, rr, _, _ := tookOverWorld(t)
	logCtx, out := infoLog()
	ctx, cancel := context.WithCancel(logCtx)
	g.SetFail(func(op string) error {
		if op == opUpdateChangeRequest {
			cancel()
			return fmt.Errorf("update pull request: %w", context.Canceled)
		}
		return nil
	})
	err := r.afterHandoff(ctx, rr)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled rewrite did not return the cancellation: %v", err)
	}
	if strings.Contains(out.String(), "Could not finish") {
		t.Fatalf("a cancellation was logged as a failure: %q", out.String())
	}
	if rr.Status.Handoff.From.PullRequestUpdate != "" {
		t.Fatal("a cancelled rewrite was recorded")
	}
}

// A transient failure of the Git host is logged and left for the next look, which the poll of the waiting pull
// request brings.
func TestAfterHandoffLogsATransientRewriteFailure(t *testing.T) {
	r, g, _, rr, _, _ := tookOverWorld(t)
	ctx, out := infoLog()
	g.SetFail(func(op string) error {
		if op == opUpdateChangeRequest {
			return &providers.RetryableError{Err: errors.New("status 502")}
		}
		return nil
	})
	if err := r.afterHandoff(ctx, rr); err != nil {
		t.Fatalf("a transient failure of the host stopped the reconcile: %v", err)
	}
	if !strings.Contains(out.String(), "Could not finish the steps after the handoff") {
		t.Fatalf("a transient failure was not logged: %q", out.String())
	}
}

// A failed read of the replacement decides nothing, and is logged with its cause.
func TestSettleHandoffLogsAFailedRead(t *testing.T) {
	r, _, p, _, _, _ := gateWorld(t)
	ctx, out := infoLog()
	base := r.Client
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == newName {
				return errors.New("etcd is down")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	upd := p.DeepCopy()
	r.settleHandoff(ctx, upd)
	if intent(upd) == nil {
		t.Fatal("a failed read aborted the intent")
	}
	if !strings.Contains(out.String(), "etcd is down") {
		t.Fatalf("a failed read of the replacement was not logged: %q", out.String())
	}
}

// pagedReviews returns its reviews as a list that never ends.
type pagedReviews struct {
	*fakegit.Git
}

func (p pagedReviews) ListReviews(ctx context.Context, n int64, cur providers.FeedbackCursor) (providers.FeedbackPage, error) {
	page, err := p.Git.ListReviews(ctx, n, cur)
	page.Complete = false
	return page, err
}

// A list of reviews too long to read in one look leaves the check in progress, and says why: an error for the
// log on every look, and a Warning Event once per head.
func TestReviewGateSaysWhyAListTooLongKeepsItWaiting(t *testing.T) {
	r, g, _, rr, cr, head := tookOverWorld(t)
	ctx := context.Background()
	events := record.NewFakeRecorder(10)
	r.Recorder = events
	rr.Status.PullRequest.ReviewCheck = &api.ReviewCheck{Head: head, State: api.ReviewCheckInProgress}
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	approve(g, cr.Number, 90, 3003, head)
	git := &gatedGit{git: pagedReviews{g}, allow: r.writeAllowed}
	for range 2 {
		if err := r.reviewGate(ctx, git, rr); !errors.Is(err, errReviewsUnread) {
			t.Fatalf("a list of reviews too long to read gave %v", err)
		}
	}
	if rr.Status.PullRequest.ReviewCheck.State != api.ReviewCheckInProgress {
		t.Fatalf("the gate decided on a list it did not read whole: %+v", rr.Status.PullRequest.ReviewCheck)
	}
	var got []string
	for len(events.Events) > 0 {
		got = append(got, <-events.Events)
	}
	if len(got) != 1 || !strings.Contains(got[0], "Warning "+ReasonReviewListTooLong) {
		t.Fatalf("Events: %q", got)
	}
}

// The comment that says why a hint author's approval does not count is asked for once per review in this
// process, not on every look: each ask reads the comments of the pull request for its marker.
func TestReviewGateAsksForTheHintAuthorCommentOnce(t *testing.T) {
	r, g, _, rr, cr, head := tookOverWorld(t)
	ctx := context.Background()
	rr.Status.PullRequest.ReviewCheck = &api.ReviewCheck{Head: head, State: api.ReviewCheckInProgress}
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	approve(g, cr.Number, 91, rr.Status.Hints[0].Author.ID, head)
	var asks atomic.Int32
	g.SetFail(func(op string) error {
		if op == "CommentChangeRequest" {
			asks.Add(1)
		}
		return nil
	})
	git := &gatedGit{git: g, allow: r.writeAllowed}
	for range 3 {
		if err := r.reviewGate(ctx, git, rr); err != nil {
			t.Fatal(err)
		}
	}
	if n := asks.Load(); n != 1 {
		t.Fatalf("the comment for one review was asked for %d times", n)
	}
	if c := g.Comments(cr.Number); len(c) != 1 {
		t.Fatalf("comments: %q", c)
	}
}

// A comment that failed is asked for again on the next look.
func TestReviewGateAsksAgainAfterAFailedComment(t *testing.T) {
	r, g, _, rr, cr, head := tookOverWorld(t)
	ctx := context.Background()
	rr.Status.PullRequest.ReviewCheck = &api.ReviewCheck{Head: head, State: api.ReviewCheckInProgress}
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	approve(g, cr.Number, 91, rr.Status.Hints[0].Author.ID, head)
	g.SetFail(func(op string) error {
		if op == "CommentChangeRequest" {
			return &providers.RetryableError{Err: errors.New("status 502")}
		}
		return nil
	})
	git := &gatedGit{git: g, allow: r.writeAllowed}
	if err := r.reviewGate(ctx, git, rr); err != nil {
		t.Fatal(err)
	}
	g.SetFail(nil)
	if err := r.reviewGate(ctx, git, rr); err != nil {
		t.Fatal(err)
	}
	if c := g.Comments(cr.Number); len(c) != 1 {
		t.Fatalf("the comment was not asked for again after a failure: %q", c)
	}
}
