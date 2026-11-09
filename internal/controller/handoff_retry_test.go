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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// The retries of the handoff and the merge gate (#590, the review round of #604).

// pastDeadline sets rr's deadline to ago before now.
func pastDeadline(t *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest, ago time.Duration) {
	t.Helper()
	rr.Status.Deadline = &metav1.Time{Time: time.Now().Add(-ago)}
	if err := r.Status().Update(context.Background(), rr); err != nil {
		t.Fatal(err)
	}
}

// A recovery that keeps failing does not hold the target for good: once the grace after the deadline is over,
// the replacement ends NeedsIntervention with a reason that says why, and its handoff Aborted.
func TestRecoveryEndsOnceTheGraceAfterTheDeadlinePassed(t *testing.T) {
	r, _, _, rr, _, _ := gateWorld(t)
	ctx := context.Background()
	pastDeadline(t, r, rr, handoffRecoveryGrace+time.Minute)
	r.OpenRepository = func(context.Context, api.GitRepository) (providers.GitProvider, error) {
		return nil, &providers.RetryableError{Err: errors.New("the token endpoint did not answer")}
	}
	if _, err := r.recheck(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhaseNeedsIntervention {
		t.Fatalf("a recovery failing past the grace left the request %s", rr.Status.Phase)
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPROpened); c == nil || c.Reason != ReasonHandoffRecoveryFailed {
		t.Fatalf("PullRequestOpened: %+v", c)
	}
	if got := rr.Status.Handoff.From.Phase; got != api.HandoffAborted {
		t.Fatalf("the handoff of an ended replacement is %s", got)
	}
}

// After the deadline, within the grace, the recovery waits as long as the Git host asked, not one second.
func TestRecoveryHonoursRetryAfterPastTheDeadline(t *testing.T) {
	r, _, _, rr, _, _ := gateWorld(t)
	ctx := context.Background()
	pastDeadline(t, r, rr, time.Minute)
	const asked = 5 * time.Minute
	r.OpenRepository = func(context.Context, api.GitRepository) (providers.GitProvider, error) {
		return nil, &providers.RetryableError{Err: errors.New("token endpoint rate limited"), RetryAfter: asked}
	}
	for range 2 {
		res, err := r.recheck(ctx, rr)
		if err != nil {
			t.Fatal(err)
		}
		if rr.Status.Phase != api.PhasePolicyChecked {
			t.Fatalf("the recovery ended within the grace: %s", rr.Status.Phase)
		}
		if res.RequeueAfter < asked {
			t.Fatalf("the recovery retries after %s, the host asked for %s", res.RequeueAfter, asked)
		}
	}
}

// A recovery that ends through retry (here settings that cannot reach the repository) aborts the handoff.
func TestRecoveryEndedThroughRetryAbortsTheHandoff(t *testing.T) {
	r, _, _, rr, _, _ := gateWorld(t)
	ctx := context.Background()
	r.OpenRepository = func(context.Context, api.GitRepository) (providers.GitProvider, error) {
		return nil, fmt.Errorf("the key is not a PEM key: %w", providers.ErrInvalidConfig)
	}
	if _, err := r.recheck(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhaseNeedsIntervention {
		t.Fatalf("phase %s", rr.Status.Phase)
	}
	if got := rr.Status.Handoff.From.Phase; got != api.HandoffAborted {
		t.Fatalf("the handoff of an ended replacement is %s", got)
	}
}

// failRewriteRecord makes every status write that records the rewrite after the handoff fail.
func failRewriteRecord(r *RemediationRequestReconciler) client.Client {
	base := r.Client
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if rq, ok := obj.(*api.RemediationRequest); ok && rq.Name == newName && rq.Status.Handoff != nil &&
				rq.Status.Handoff.From != nil && rq.Status.Handoff.From.PullRequestUpdate != "" {
				return errors.New("etcd is down")
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	return base
}

// A rewrite whose status write fails does not hold back the look at the pull request, so a merge or a close is
// not missed behind it; the failure is still returned for a retry.
func TestStepNotRecordedDoesNotHoldBackTheLook(t *testing.T) {
	r, g, _, rr, _, _ := tookOverWorld(t)
	ctx := context.Background()
	var looks atomic.Int32
	g.SetFail(func(op string) error {
		if op == "FindChangeRequest" {
			looks.Add(1)
		}
		return nil
	})
	failRewriteRecord(r)
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
	if looks.Load() == 0 {
		t.Fatal("the look at the pull request did not run")
	}
	cur := &api.RemediationRequest{}
	if gerr := r.Get(ctx, client.ObjectKeyFromObject(rr), cur); gerr != nil {
		t.Fatal(gerr)
	}
	if progresses(cur) && !errors.Is(err, errStepNotRecorded) {
		t.Fatalf("the failed status write of a request that goes on was not returned: %v", err)
	}
}

// A rewrite whose status write keeps failing is not sent again to the Git host: the answer is kept and
// recorded once the write goes through.
func TestStepNotRecordedIsNotRewrittenAgain(t *testing.T) {
	r, g, _, rr, _, _ := tookOverWorld(t)
	ctx := context.Background()
	var rewrites atomic.Int32
	g.SetFail(func(op string) error {
		if op == opUpdateChangeRequest {
			rewrites.Add(1)
		}
		return nil
	})
	base := failRewriteRecord(r)
	for range 2 {
		if err := r.afterHandoff(ctx, rr); !errors.Is(err, errStepNotRecorded) {
			t.Fatalf("a failed status write gave %v", err)
		}
	}
	r.Client = base
	if err := r.afterHandoff(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Handoff.From.PullRequestUpdate != api.HandoffStepDone || rewrites.Load() != 1 {
		t.Fatalf("recorded %q after %d rewrites", rr.Status.Handoff.From.PullRequestUpdate, rewrites.Load())
	}
}

// settingsReader fails the reads of the settings while fail is set.
type settingsReader struct {
	client.Reader
	fail *atomic.Bool
}

func (s settingsReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*api.RemediationSettings); ok && s.fail.Load() {
		return errors.New("etcd is down")
	}
	return s.Reader.Get(ctx, key, obj, opts...)
}

// A read of the settings that fails at the kill switch after the rewrite is not a status write that failed: it
// is logged and left for the next look, which records the rewrite without sending it again.
func TestSettingsReadAfterTheRewriteIsNotAFailedStatusWrite(t *testing.T) {
	r, g, _, rr, _, _ := tookOverWorld(t)
	ctx, out := infoLog()
	var fail atomic.Bool
	var rewrites atomic.Int32
	r.APIReader = settingsReader{Reader: r.APIReader, fail: &fail}
	g.SetAfter(func(op string) error {
		if op == opUpdateChangeRequest {
			rewrites.Add(1)
			fail.Store(true)
		}
		return nil
	})
	err := r.afterHandoff(ctx, rr)
	if errors.Is(err, errStepNotRecorded) {
		t.Fatalf("a failed read of the settings was taken for a failed status write: %v", err)
	}
	if !strings.Contains(out.String(), "Could not finish the steps after the handoff") {
		t.Fatalf("the failed read was not logged: %q", out.String())
	}
	fail.Store(false)
	if err := r.afterHandoff(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Handoff.From.PullRequestUpdate != api.HandoffStepDone || rewrites.Load() != 1 {
		t.Fatalf("recorded %q after %d rewrites", rr.Status.Handoff.From.PullRequestUpdate, rewrites.Load())
	}
}

// A refused rewrite says so on the request: condition PullRequestOpened with its reason, and a Warning Event.
func TestRefusedRewriteSaysWhy(t *testing.T) {
	r, g, _, rr, _, _ := tookOverWorld(t)
	ctx := context.Background()
	events := record.NewFakeRecorder(10)
	r.Recorder = events
	g.SetFail(func(op string) error {
		if op == opUpdateChangeRequest {
			return fmt.Errorf("update pull request: %w", providers.ErrForbidden)
		}
		return nil
	})
	if err := r.afterHandoff(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Handoff.From.PullRequestUpdate != api.HandoffStepRefused {
		t.Fatalf("recorded %q", rr.Status.Handoff.From.PullRequestUpdate)
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPROpened)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonPullRequestRewriteRefused {
		t.Fatalf("PullRequestOpened: %+v", c)
	}
	stored := &api.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), stored); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(stored.Status.Conditions, api.ConditionPROpened); c == nil || c.Reason != ReasonPullRequestRewriteRefused {
		t.Fatalf("stored PullRequestOpened: %+v", c)
	}
	if len(events.Events) != 1 || !strings.Contains(<-events.Events, "Warning "+ReasonPullRequestRewriteRefused) {
		t.Fatal("no Warning Event for the refused rewrite")
	}
}

// A cancellation by the manager is not a failed reconcile: no error reaches controller-runtime.
func TestReconcileDoesNotReturnCancellationAsFailure(t *testing.T) {
	r, g, _, rr, _, _ := tookOverWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.SetFail(func(op string) error {
		if op == opUpdateChangeRequest {
			cancel()
			return fmt.Errorf("rewrite cancelled: %w", context.Canceled)
		}
		return nil
	})
	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
	if ctx.Err() == nil {
		t.Fatal("the rewrite did not cancel the reconcile")
	}
	if err != nil {
		t.Fatalf("a cancellation reached controller-runtime as an error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("a cancelled reconcile asked for another: %+v", res)
	}
}

// A list of reviews too long to read in one look completes the check run with a failure that says why, records
// it Failed with condition Approved False and one Warning Event, and is not read again on that head.
func TestReviewGateFailsTheCheckOnAListTooLong(t *testing.T) {
	r, g, _, rr, cr, head := tookOverWorld(t)
	ctx, out := infoLog()
	events := record.NewFakeRecorder(10)
	r.Recorder = events
	rr.Status.PullRequest.ReviewCheck = &api.ReviewCheck{Head: head, State: api.ReviewCheckInProgress}
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	approve(g, cr.Number, 90, 3003, head)
	var lists atomic.Int32
	g.SetFail(func(op string) error {
		if op == "ListReviews" {
			lists.Add(1)
		}
		return nil
	})
	r.OpenRepository = func(context.Context, api.GitRepository) (providers.GitProvider, error) {
		return pagedReviews{g}, nil
	}
	for range 3 {
		r.watchReviewCheck(ctx, rr, true)
	}
	if n := lists.Load(); n != maxReviewLooks {
		t.Fatalf("the reviews were read %d times, want one gate of %d looks", n, maxReviewLooks)
	}
	if got := rr.Status.PullRequest.ReviewCheck; got.State != api.ReviewCheckFailed || got.Head != head {
		t.Fatalf("review check %+v", got)
	}
	runs := g.CheckRuns(head)
	if len(runs) == 0 || runs[len(runs)-1].Status != providers.CheckRunFailure {
		t.Fatalf("check runs %+v", runs)
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionApproved); c == nil ||
		c.Status != metav1.ConditionFalse || c.Reason != ReasonReviewListTooLong {
		t.Fatalf("Approved: %+v", c)
	}
	if len(events.Events) != 1 || !strings.Contains(<-events.Events, "Warning "+ReasonReviewListTooLong) {
		t.Fatal("not one Warning Event")
	}
	if strings.Contains(out.String(), "Could not update the review check") {
		t.Fatalf("a decided head was logged as a failure: %q", out.String())
	}
}

// A hint author's comment the host refused for good is not asked again on every look.
func TestReviewGateDoesNotAskAgainForARefusedComment(t *testing.T) {
	for _, refused := range []error{providers.ErrForbidden, providers.ErrRejected} {
		t.Run(refused.Error(), func(t *testing.T) {
			r, g, _, rr, cr, head := tookOverWorld(t)
			rr.Status.PullRequest.ReviewCheck = &api.ReviewCheck{Head: head, State: api.ReviewCheckInProgress}
			approve(g, cr.Number, 91, rr.Status.Hints[0].Author.ID, head)
			asks := 0
			g.SetFail(func(op string) error {
				if op == opComment {
					asks++
					return refused
				}
				return nil
			})
			git := &gatedGit{git: g, allow: r.writeAllowed}
			for range 3 {
				if err := r.reviewGate(context.Background(), git, rr); err != nil {
					t.Fatal(err)
				}
			}
			if asks != 1 {
				t.Fatalf("a comment refused for good was asked %d times", asks)
			}
		})
	}
}

// slowComments never answers a comment before its context ends.
type slowComments struct {
	*fakegit.Git
}

func (s slowComments) CommentChangeRequest(ctx context.Context, _ int64, _, _ string) error {
	<-ctx.Done()
	return fmt.Errorf("comment: %w", ctx.Err())
}

// A comment that runs out of time does not take the time of the check run: a passing gate completes it.
func TestReviewGateCompletesTheCheckWhenACommentTimesOut(t *testing.T) {
	r, g, _, rr, cr, head := tookOverWorld(t)
	rr.Status.PullRequest.ReviewCheck = &api.ReviewCheck{Head: head, State: api.ReviewCheckInProgress}
	if err := r.Status().Update(context.Background(), rr); err != nil {
		t.Fatal(err)
	}
	approve(g, cr.Number, 91, rr.Status.Hints[0].Author.ID, head)
	approve(g, cr.Number, 92, 3003, head)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	git := &gatedGit{git: slowComments{g}, allow: r.writeAllowed}
	if err := r.reviewGate(ctx, git, rr); err != nil {
		t.Fatal(err)
	}
	if got := rr.Status.PullRequest.ReviewCheck.State; got != api.ReviewCheckPassed {
		t.Fatalf("a comment that timed out kept a passing gate from completing: %s", got)
	}
}
