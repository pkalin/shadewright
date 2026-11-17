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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// slowGit runs out the caller's time on the methods slow names: the call waits for the context and fails with it,
// as a read the step's deadline cuts short.
type slowGit struct {
	providers.GitProvider
	slow func(op string) bool
}

func (g slowGit) wait(ctx context.Context, op string) error {
	if g.slow != nil && g.slow(op) {
		<-ctx.Done()
		return freshness.WrapProvider("read "+op, ctx.Err())
	}
	return nil
}

func (g slowGit) Parents(ctx context.Context, commit string) ([]string, error) {
	if err := g.wait(ctx, opParents); err != nil {
		return nil, err
	}
	return g.GitProvider.Parents(ctx, commit)
}

func (g slowGit) GetChangeRequest(ctx context.Context, n int64) (providers.ChangeRequest, error) {
	if err := g.wait(ctx, opGet); err != nil {
		return providers.ChangeRequest{}, err
	}
	return g.GitProvider.GetChangeRequest(ctx, n)
}

func (g slowGit) ReadFile(ctx context.Context, sha, path string, limit int) (providers.File, error) {
	if err := g.wait(ctx, opReadFile); err != nil {
		return providers.File{}, err
	}
	return g.GitProvider.ReadFile(ctx, sha, path, limit)
}

// lostRecord makes rr a request whose pull request was opened while the status write that would record it was
// lost, and that then stopped for the branch change on condition, which is PullRequestOpened when no pull request
// is recorded (stop) and PlanFresh for the freshness check's stop. It returns the pull request's number.
func lostRecord(t *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest, condition string) int64 {
	t.Helper()
	number := rr.Status.PullRequest.Number
	rr.Status.PullRequest = nil
	meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionApproved)
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: condition,
		Status: metav1.ConditionFalse, Reason: reasonTargetBranchChanged, Message: whyBranchChanged})
	if err := r.Status().Update(context.Background(), rr); err != nil {
		t.Fatal(err)
	}
	return number
}

// Finding 1 of #827: the pull request was opened and its status write lost; the request then stopped for the
// branch change with the reason on PullRequestOpened (no pull request recorded). After the pull request is moved to
// the configured branch and merged, the lock stays until the merge is recorded with its receipt.
func TestBranchChangeStopOnPullRequestOpenedRecordsMerge(t *testing.T) {
	for _, branch := range []string{mainBranch, freshOtherBranch} {
		t.Run(branch, func(t *testing.T) {
			r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
			ctx := context.Background()
			number := lostRecord(t, r, rr, api.ConditionPROpened)
			if !stoppedForBranchChange(rr) {
				t.Fatal("a stop for the branch change with no pull request recorded is not seen as one")
			}
			if err := g.SetBase(number, branch); err != nil {
				t.Fatal(err)
			}
			if _, err := g.Merge(number); err != nil {
				t.Fatal(err)
			}
			for range 4 {
				if _, err := r.checkHeld(ctx, rr); err != nil {
					t.Fatal(err)
				}
				if !*rr.Status.HoldsTarget && rr.Status.MergeReceipt == nil {
					t.Fatalf("the lock went with no merge receipt: phase %s, Approved %+v", rr.Status.Phase,
						meta.FindStatusCondition(rr.Status.Conditions, api.ConditionApproved))
				}
			}
			if rr.Status.MergeReceipt == nil || rr.Status.MergeReceipt.TargetBranch != api.GitBranch(branch) {
				t.Fatalf("merge not recorded: phase %s, receipt %+v", rr.Status.Phase, rr.Status.MergeReceipt)
			}
		})
	}
}

// Finding 2 of #827: every read of what a retargeted merge brought runs out of the cleanup's time. Each reconcile
// is a paced wait with no error, the content wait's clock starts at the first, and the first reconcile past
// mergeContentWait ends the request MergeContentUnreadable, before any read.
func TestRetargetedContentTimeoutsReachUnreadable(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	if err := g.SetBase(rr.Status.PullRequest.Number, freshOtherBranch); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Merge(rr.Status.PullRequest.Number); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	r.OpenRepository = func(_ context.Context, repo api.GitRepository) (providers.GitProvider, error) {
		calls.Add(1)
		return slowGit{GitProvider: g.ForTarget(string(repo.TargetBranch)), slow: func(op string) bool { return op == opParents }}, nil
	}
	r.CleanupTimeout = 50 * time.Millisecond
	clock := clockOf(r)
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}
	for i := range 3 {
		res, err := r.Reconcile(ctx, request)
		if err != nil || res.RequeueAfter <= 0 {
			t.Fatalf("reconcile %d = %+v, %v; want a paced wait and no error", i, res, err)
		}
		if err := r.Get(ctx, request.NamespacedName, rr); err != nil {
			t.Fatal(err)
		}
		if rr.Status.MergeContentUnreadSince == nil {
			t.Fatalf("reconcile %d: the content wait has no clock", i)
		}
		c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionGitReadRetry)
		if c == nil || c.Reason != ReasonGitReadUntilContentWait {
			t.Fatalf("reconcile %d: GitReadRetry = %+v; want the content wait", i, c)
		}
		if res.RequeueAfter > mergeContentWait {
			t.Fatalf("reconcile %d waits %v, past the content wait", i, res.RequeueAfter)
		}
		clock.advance(time.Minute)
	}
	clock.advance(mergeContentWait)
	calls.Store(0)
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, request.NamespacedName, rr); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionApproved); c == nil ||
		c.Reason != ReasonMergeContentUnreadable {
		t.Fatalf("Approved = %+v; want MergeContentUnreadable after the wait", c)
	}
	if rr.Status.MergeReceipt != nil {
		t.Fatal("an unread retargeted merge got a receipt")
	}
}

// Finding 3 of #827: a stopped holder with no pull request recorded, the target branch changed, and the Git host
// refuses to open the repository: the condition names no pull request, the lock stays, and nothing panics.
func TestBranchChangeOpenRefusedWithoutPullRequest(t *testing.T) {
	r, _, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	lostRecord(t, r, rr, api.ConditionPROpened)
	r.OpenRepository = func(context.Context, api.GitRepository) (providers.GitProvider, error) {
		return nil, fmt.Errorf("open: %w", providers.ErrForbidden)
	}
	if _, err := r.checkHeld(ctx, rr); err != nil && !gitReadFailure(err) {
		t.Fatalf("checkHeld() = %v", err)
	}
	if !*rr.Status.HoldsTarget {
		t.Fatal("a refused open released the lock")
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable); c == nil ||
		c.Reason != ReasonGitForbidden {
		t.Fatalf("PullRequestUnreadable = %+v; want the refusal", c)
	}
}

// #827, design review: a pull request found by the request's marker after a lost status write, whose head is one
// commit with the planned change on top of a commit the target branch of the plan never held (so the executor never
// pushed it), merged into the configured branch: the marker proves nothing, and the merge gets no receipt.
func TestAdoptedRetargetedHeadNotPushedGetsNoReceipt(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	number := lostRecord(t, r, rr, api.ConditionPlanFresh)
	// The configured branch moves on with a commit main never held, and a head with the planned change is built on it.
	diverged, err := g.Push(freshOtherBranch, "other.yaml", []byte("other: change\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.CreateBranch(ctx, "not-pushed", diverged); err != nil {
		t.Fatal(err)
	}
	base, err := g.ReadFile(ctx, diverged, sourcePath, providers.MaxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	content, _, err := planned(base.Data, rr)
	if err != nil {
		t.Fatal(err)
	}
	head, err := g.Push("not-pushed", sourcePath, content)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.ForcePush(freshness.BranchName(rr), head); err != nil {
		t.Fatal(err)
	}
	if err := g.SetBase(number, freshOtherBranch); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Merge(number); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := r.checkHeld(ctx, rr); err != nil {
			t.Fatal(err)
		}
	}
	if rr.Status.MergeReceipt != nil {
		t.Fatal("a head the executor never pushed, adopted by its marker, received a merge receipt")
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionApproved); c == nil ||
		c.Reason != ReasonMergedWithForeignChange {
		t.Fatalf("Approved = %+v; want MergedWithForeignChange", c)
	}
}

// A Git wait past its end ends at the start of the next reconcile, before any read of the Git host: the content
// wait and the merge wait (no Git call), the handoff grace (no Git call), and the request's deadline (its expiry,
// which may read to close a pull request of its own, ends the request Expired).
func TestPassedWaitEndsBeforeAnyRead(t *testing.T) {
	type wait struct {
		reason string
		setup  func(rr *api.RemediationRequest, now time.Time)
		reads  bool
		check  func(t *testing.T, rr *api.RemediationRequest)
	}
	waits := map[string]wait{
		"content": {reason: ReasonGitReadUntilContentWait, setup: func(rr *api.RemediationRequest, now time.Time) {
			rr.Status.MergeContentUnreadSince = &metav1.Time{Time: now.Add(-mergeContentWait)}
		}, check: func(t *testing.T, rr *api.RemediationRequest) {
			if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionApproved); c == nil ||
				c.Reason != ReasonMergeContentUnreadable || rr.Status.MergeContentUnreadSince != nil {
				t.Fatalf("Approved = %+v, clock %v; want MergeContentUnreadable and the clock cleared", c,
					rr.Status.MergeContentUnreadSince)
			}
		}},
		"merge check": {reason: ReasonGitReadUntilMergeWait, setup: func(rr *api.RemediationRequest, now time.Time) {
			rr.Status.MergeCommitAwaitedSince = &metav1.Time{Time: now.Add(-mergeCommitWait)}
		}, reads: true, check: func(t *testing.T, rr *api.RemediationRequest) {
			if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPlanFresh); c == nil ||
				c.Reason != freshness.ReasonPlanChanged {
				t.Fatalf("PlanFresh = %+v; want the plan not shown fresh", c)
			}
		}},
		"handoff grace": {reason: ReasonGitReadUntilHandoffGrace, setup: func(rr *api.RemediationRequest, now time.Time) {
			rr.Status.Phase = api.PhasePolicyChecked
			rr.Status.Deadline = &metav1.Time{Time: now.Add(-handoffRecoveryGrace)}
			rr.Spec.Replaces = &api.RequestReplacement{}
			rr.Status.Handoff = &api.Handoff{From: &api.IncomingHandoff{Phase: api.HandoffReserved}}
		}, check: func(t *testing.T, rr *api.RemediationRequest) {
			if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionApproved); rr.Status.Phase !=
				api.PhaseNeedsIntervention || c == nil || c.Reason != ReasonHandoffRecoveryFailed {
				t.Fatalf("phase %s, Approved %+v; want HandoffRecoveryFailed", rr.Status.Phase, c)
			}
		}},
		"deadline": {reason: ReasonGitReadUntilDeadline, setup: func(rr *api.RemediationRequest, now time.Time) {
			rr.Status.Phase = api.PhasePolicyChecked
			rr.Status.Deadline = &metav1.Time{Time: now}
		}, reads: true, check: func(t *testing.T, rr *api.RemediationRequest) {
			if rr.Status.Phase != api.PhaseExpired {
				t.Fatalf("phase %s; want Expired", rr.Status.Phase)
			}
		}},
	}
	for name, w := range waits {
		t.Run(name, func(t *testing.T) {
			r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
			ctx := context.Background()
			clock := clockOf(r)
			w.setup(rr, clock.at)
			if rr.Spec.Replaces != nil {
				spec := rr.DeepCopy()
				if err := r.Get(ctx, client.ObjectKeyFromObject(rr), spec); err != nil {
					t.Fatal(err)
				}
				spec.Spec.Replaces = rr.Spec.Replaces
				if err := r.Update(ctx, spec); err != nil {
					t.Fatal(err)
				}
				rr.ResourceVersion = spec.ResourceVersion
			}
			meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionGitReadRetry,
				Status: metav1.ConditionTrue, Reason: w.reason, Message: "Retrying Git read: x"})
			if err := r.Status().Update(ctx, rr); err != nil {
				t.Fatal(err)
			}
			var gitCalls atomic.Int32
			g.SetFail(func(string) error { gitCalls.Add(1); return nil })
			if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
				t.Fatal(err)
			}
			if !w.reads && gitCalls.Load() > 0 {
				t.Fatalf("the end of a passed wait read the Git host %d times", gitCalls.Load())
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
				t.Fatal(err)
			}
			w.check(t, rr)
		})
	}
}

// settlementOrder is one generated order of the settlement properties: why the request stopped, what the reads of
// its pull request find, which status write is lost, and which Git read fails or runs out of time once.
type settlementOrder struct {
	stop, found, read string
	lostWrite         int
}

func (o settlementOrder) String() string {
	return fmt.Sprintf("stop=%s/found=%s/lost-write=%d/read=%s", o.stop, o.found, o.lostWrite, o.read)
}

const (
	stopPlanFresh = "plan-fresh"
	stopApproved  = "approved"
	stopUnrecord  = "no-pull-request"
	stopCancelled = "cancelled"
	stopOther     = "other"

	foundOpen       = "open"
	foundClosed     = "closed"
	foundMergedHere = "merged-recorded-branch"
	foundMergedMove = "merged-configured-branch"

	readNone    = "none"
	readTimeout = "-timeout"
	readAlways  = "-always"
)

// The two properties of the decision of 2026-11-17 on #771, over every generated order:
//   - lock: the target lock is never released while the request's pull request is open, nor while it is merged and
//     the status does not record the merge (mergeRecorded); a merge that counts ends with its receipt;
//   - bound: a Git read that fails is a paced wait with no error and a condition naming it, never later than the end
//     of its kind, and a wait past its end ends at the next reconcile (no wait stays past its end).
func TestSettlementOrders(t *testing.T) {
	orders := make([]settlementOrder, 0, 720)
	for _, stop := range []string{stopPlanFresh, stopApproved, stopUnrecord, stopCancelled, stopOther} {
		for _, found := range []string{foundOpen, foundClosed, foundMergedHere, foundMergedMove} {
			for _, lost := range []int{0, 1, 2, 3} {
				for _, read := range []string{readNone, opGet, opList, opParents, opReadFile, opGet + readTimeout,
					opParents + readTimeout, opParents + readTimeout + readAlways, opReadFile + readAlways} {
					orders = append(orders, settlementOrder{stop: stop, found: found, read: read, lostWrite: lost})
				}
			}
		}
	}
	for _, o := range orders {
		t.Run(o.String(), func(t *testing.T) {
			t.Parallel()
			runSettlementOrder(t, o)
		})
	}
}

func runSettlementOrder(t *testing.T, o settlementOrder) {
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	// The number is taken before the stop: a stop with no pull request recorded removes it from the status.
	number := rr.Status.PullRequest.Number
	orderStop(t, r, rr, o.stop)
	orderFound(t, g, number, o.found)
	always := orderReadFailure(r, g, o.read)
	base := orderLostWrite(r, o.lostWrite)
	clock := clockOf(r)
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}
	live := &api.RemediationRequest{}
	for i := range 10 {
		before := &api.RemediationRequest{}
		if err := base.Get(ctx, request.NamespacedName, before); err != nil {
			t.Fatal(err)
		}
		res, err := r.Reconcile(ctx, request)
		if err := base.Get(ctx, request.NamespacedName, live); err != nil {
			t.Fatal(err)
		}
		lockProperty(t, i, o, live)
		boundProperty(t, i, clock.at, before, live, res, err)
		clock.advance(confirmWait + time.Minute)
	}
	if !always {
		settledProperty(t, o, live)
	}
}

// orderStop stops rr for stop, in the store too.
func orderStop(t *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest, stop string) {
	t.Helper()
	switch stop {
	case stopPlanFresh:
		meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionApproved)
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionPlanFresh,
			Status: metav1.ConditionFalse, Reason: reasonTargetBranchChanged, Message: whyBranchChanged})
	case stopUnrecord:
		lostRecord(t, r, rr, api.ConditionPROpened)
	case stopCancelled:
		rr.Status.Phase = api.PhaseCancelled
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionApproved,
			Status: metav1.ConditionFalse, Reason: api.ReasonIncidentGone, Message: whyIncidentGone})
	case stopOther:
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionApproved,
			Status: metav1.ConditionFalse, Reason: ReasonGitForbidden, Message: "the host said no"})
	}
	if err := r.Status().Update(context.Background(), rr); err != nil {
		t.Fatal(err)
	}
}

// orderFound makes the pull request what the reads find: open, closed, or merged into the recorded or the
// configured branch.
func orderFound(t *testing.T, g *fakegit.Git, number int64, found string) {
	t.Helper()
	switch found {
	case foundClosed:
		if err := g.CloseChangeRequest(context.Background(), number); err != nil {
			t.Fatal(err)
		}
	case foundMergedMove:
		if err := g.SetBase(number, freshOtherBranch); err != nil {
			t.Fatal(err)
		}
		fallthrough
	case foundMergedHere:
		if _, err := g.Merge(number); err != nil {
			t.Fatal(err)
		}
	}
}

// orderReadFailure makes one Git read fail, or run out of the step's time, at its first call, or at every call;
// always reports the latter.
func orderReadFailure(r *RemediationRequestReconciler, g *fakegit.Git, read string) (always bool) {
	op, always := strings.CutSuffix(read, readAlways)
	op, timeout := strings.CutSuffix(op, readTimeout)
	var readFailed atomic.Bool
	failsNow := func() bool { return always || readFailed.CompareAndSwap(false, true) }
	g.SetFail(func(name string) error {
		if !timeout && name == op && failsNow() {
			return &providers.RetryableError{RetryAfter: hostDelay, Err: errors.New("outage")}
		}
		return nil
	})
	r.OpenRepository = func(_ context.Context, repo api.GitRepository) (providers.GitProvider, error) {
		return slowGit{GitProvider: g.ForTarget(string(repo.TargetBranch)), slow: func(name string) bool {
			return timeout && name == op && failsNow()
		}}, nil
	}
	// Long enough for every step that does not wait on a slow read, also under the race detector: only the slow
	// read runs out of the step's time.
	r.CleanupTimeout = 250 * time.Millisecond
	return always
}

// orderLostWrite loses the lost-th write of the request's status and returns the client that loses nothing.
func orderLostWrite(r *RemediationRequestReconciler, lost int) client.WithWatch {
	var writes atomic.Int32
	lose := func() error {
		if int(writes.Add(1)) == lost {
			return apierrors.NewServiceUnavailable("lost")
		}
		return nil
	}
	base := r.Client.(client.WithWatch)
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if err := lose(); err != nil {
				return err
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption) error {
			if err := lose(); err != nil {
				return err
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	r.Client, r.APIReader = c, c
	return base
}

// boundProperty: after reconcile i at now, a failed Git read is a wait with no error, never past the end of its
// kind, and a wait that was past its end before the reconcile no longer stands.
func boundProperty(t *testing.T, i int, now time.Time, before, live *api.RemediationRequest, res reconcile.Result,
	err error) {
	t.Helper()
	retry := meta.FindStatusCondition(live.Status.Conditions, api.ConditionGitReadRetry)
	switch {
	case err != nil && !isLostWrite(err):
		t.Fatalf("reconcile %d returned %v; a failed Git read is a wait", i, err)
	case err == nil && retry != nil && res.RequeueAfter <= 0:
		t.Fatalf("reconcile %d shows %s with no wait", i, retry.Reason)
	case err == nil && retry != nil:
		if end, ok := waitKindOf(retry.Reason).end(live, now); ok && now.Add(res.RequeueAfter).After(end.Add(boundPassedWait)) {
			t.Fatalf("reconcile %d waits %v, past the end of %s at %v", i, res.RequeueAfter, retry.Reason, end)
		}
	}
	if c := meta.FindStatusCondition(before.Status.Conditions, api.ConditionGitReadRetry); c != nil {
		if end, ok := waitKindOf(c.Reason).end(before, now); ok && !now.Before(end) && retry != nil &&
			retry.Reason == c.Reason && retry.LastTransitionTime.Equal(&c.LastTransitionTime) {
			t.Fatalf("reconcile %d: the wait %s was past its end and did not end", i, c.Reason)
		}
	}
}

// isLostWrite reports whether err is the status write the order loses: the one error a reconcile may return.
func isLostWrite(err error) bool {
	return apierrors.IsServiceUnavailable(err) || strings.Contains(err.Error(), "lost")
}

// lockProperty: after reconcile i, the lock is held while the pull request is open, and a released lock on a merged
// pull request comes with the merge recorded.
func lockProperty(t *testing.T, i int, o settlementOrder, live *api.RemediationRequest) {
	t.Helper()
	if holdsLock(live) {
		return
	}
	switch o.found {
	case foundOpen:
		t.Fatalf("reconcile %d released the lock while the pull request is open", i)
	case foundMergedHere, foundMergedMove:
		if !mergeRecorded(live) {
			t.Fatalf("reconcile %d released the lock on a merge the status does not record: phase %s", i,
				live.Status.Phase)
		}
	}
}

// settledProperty: once the reads work again, a closed pull request releases the lock, a merged one is recorded
// (with its receipt when it counts) and releases it, and an open one keeps it.
func settledProperty(t *testing.T, o settlementOrder, live *api.RemediationRequest) {
	t.Helper()
	switch o.found {
	case foundOpen:
		if !holdsLock(live) {
			t.Fatal("the lock went while the pull request is open")
		}
	case foundClosed:
		if holdsLock(live) {
			t.Fatalf("the lock is held on a closed pull request: %+v", meta.FindStatusCondition(live.Status.Conditions,
				api.ConditionPullRequestUnreadable))
		}
	default:
		if !mergeRecorded(live) {
			t.Fatalf("the merge is not recorded: phase %s, Approved %+v", live.Status.Phase,
				meta.FindStatusCondition(live.Status.Conditions, api.ConditionApproved))
		}
		// Every merge of these orders carries the planned change alone, so it counts, whatever the stop.
		if live.Status.MergeReceipt == nil {
			t.Fatalf("a merge that counts has no receipt: phase %s, Approved %+v", live.Status.Phase,
				meta.FindStatusCondition(live.Status.Conditions, api.ConditionApproved))
		}
	}
}
