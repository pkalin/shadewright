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
	"go/ast"
	"go/token"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/providers"
)

// failingParents fails Parents of the commits fail names, as an outage of the Git host for that read alone.
type failingParents struct {
	providers.GitProvider
	fail func(sha string) bool
}

func (g failingParents) Parents(ctx context.Context, sha string) ([]string, error) {
	if g.fail(sha) {
		return nil, &providers.RetryableError{Err: errors.New("outage")}
	}
	return g.GitProvider.Parents(ctx, sha)
}

// Proven sequence 1 of the third decision of 2026-11-17 on #771: the status write that would record the pull
// request is lost and the request stops for the branch change; the pull request is moved to the configured branch
// and merged, found by its marker and adopted in memory, and the read of the merge commit's parents keeps failing.
// The content wait must end by its bound, as a failure with its reason (MergeContentUnreadable), with the adopted
// pull request recorded, and never as an error that repeats; once Git recovers the lock is released.
func TestAdoptedContentWaitEnds(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	number := lostRecord(t, r, rr, api.ConditionPROpened)
	if err := g.SetBase(number, freshOtherBranch); err != nil {
		t.Fatal(err)
	}
	merge, err := g.Merge(number)
	if err != nil {
		t.Fatal(err)
	}
	var outage atomic.Bool
	outage.Store(true)
	r.OpenRepository = func(_ context.Context, repo api.GitRepository) (providers.GitProvider, error) {
		return failingParents{GitProvider: g.ForTarget(string(repo.TargetBranch)), fail: func(sha string) bool {
			return outage.Load() && sha == merge
		}}, nil
	}
	clock := clockOf(r)
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}
	start := clock.at
	var ended bool
	for i := range 8 {
		res, err := r.Reconcile(ctx, request)
		if err != nil {
			t.Fatalf("reconcile %d at +%v returned %v; an expired wait ends, it does not repeat an error", i,
				clock.at.Sub(start), err)
		}
		if err := r.Get(ctx, request.NamespacedName, rr); err != nil {
			t.Fatal(err)
		}
		if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionApproved); c != nil &&
			c.Reason == ReasonMergeContentUnreadable {
			ended = true
			if clock.at.Sub(start) > mergeContentWait+r.pullRequestPollInterval() {
				t.Fatalf("the content wait ended at +%v, past its bound", clock.at.Sub(start))
			}
			break
		}
		clock.advance(max(res.RequeueAfter, time.Second))
	}
	if !ended {
		t.Fatalf("the content wait never ended: phase %s, waits %+v", rr.Status.Phase, rr.Status.GitWaits)
	}
	if rr.Status.PullRequest == nil || rr.Status.PullRequest.Number != number {
		t.Fatalf("the end of the content wait does not record the adopted pull request: %+v", rr.Status.PullRequest)
	}
	outage.Store(false)
	for range 3 {
		res, err := r.Reconcile(ctx, request)
		if err != nil {
			t.Fatalf("after Git recovered: %v", err)
		}
		clock.advance(max(res.RequeueAfter, time.Second))
	}
	if err := r.Get(ctx, request.NamespacedName, rr); err != nil {
		t.Fatal(err)
	}
	if holdsLock(rr) {
		t.Fatal("the lock is still held after the merge was recorded and Git recovered")
	}
}

// Proven sequence 2: a content read fails and starts its wait; the next reconciles fail reading the pull request
// itself (an unbounded retry). The content wait is an entry of its own, which the other failure neither replaces nor
// moves, so it ends by its bound while the reads of the pull request keep failing.
func TestContentWaitSurvivesAnotherFailure(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	number := rr.Status.PullRequest.Number
	if err := g.SetBase(number, freshOtherBranch); err != nil {
		t.Fatal(err)
	}
	merge, err := g.Merge(number)
	if err != nil {
		t.Fatal(err)
	}
	r.OpenRepository = func(_ context.Context, repo api.GitRepository) (providers.GitProvider, error) {
		return failingParents{GitProvider: g.ForTarget(string(repo.TargetBranch)), fail: func(sha string) bool {
			return sha == merge
		}}, nil
	}
	clock := clockOf(r)
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}
	start := clock.at
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, request.NamespacedName, rr); err != nil {
		t.Fatal(err)
	}
	if contentWaitOf(rr) == nil {
		t.Fatalf("the failed content read started no content wait: %+v", rr.Status.GitWaits)
	}
	g.SetFail(func(op string) error {
		if op == opGet || op == opList {
			return &providers.RetryableError{Err: errors.New("outage")}
		}
		return nil
	})
	for i := range 8 {
		res, err := r.Reconcile(ctx, request)
		if err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		if err := r.Get(ctx, request.NamespacedName, rr); err != nil {
			t.Fatal(err)
		}
		if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionApproved); c != nil &&
			c.Reason == ReasonMergeContentUnreadable {
			if clock.at.Sub(start) > mergeContentWait+r.pullRequestPollInterval() {
				t.Fatalf("the content wait ended at +%v, past its bound", clock.at.Sub(start))
			}
			return
		}
		clock.advance(max(res.RequeueAfter, time.Second))
	}
	t.Fatalf("the content wait never ended while the pull request reads failed: waits %+v, GitReadRetry %+v",
		rr.Status.GitWaits, meta.FindStatusCondition(rr.Status.Conditions, api.ConditionGitReadRetry))
}

// contentWaitOf is rr's content wait entry, or nil.
func contentWaitOf(rr *api.RemediationRequest) *api.GitWait { return gitWait(rr, waitMergeContent) }

// contentWaitSince is the start of rr's content wait, or nil.
func contentWaitSince(rr *api.RemediationRequest) *metav1.Time {
	if w := contentWaitOf(rr); w != nil {
		return &w.Since
	}
	return nil
}

// setContentWait gives rr a content wait about its recorded pull request started at since, or removes it (nil).
func setContentWait(rr *api.RemediationRequest, since *metav1.Time) {
	rr.Status.GitWaits = slices.DeleteFunc(rr.Status.GitWaits, func(w api.GitWait) bool {
		return w.Kind == api.GitWaitMergeContent
	})
	if since == nil {
		return
	}
	pr := &api.GitWaitPullRequest{Number: 1, URL: "https://github.com/example/pr/1", HeadSHA: strings.Repeat("a", 40)}
	if p := rr.Status.PullRequest; p != nil {
		pr = &api.GitWaitPullRequest{Number: p.Number, URL: p.URL, HeadSHA: p.HeadSHA}
	}
	rr.Status.GitWaits = append(rr.Status.GitWaits, api.GitWait{Kind: api.GitWaitMergeContent, Since: *since,
		Ends: &metav1.Time{Time: since.Add(mergeContentWait)}, Operation: "read the parents of the merge commit",
		PullRequest: pr})
}

// setMergeWait records a merge wait started at since about rr's recorded pull request (a stand-in when none is
// recorded), merged into its recorded target branch without a merge commit seen, as awaitMergeCommit does; the
// wait's start in status.mergeCommitAwaitedSince too.
func setMergeWait(rr *api.RemediationRequest, since time.Time) {
	rr.Status.GitWaits = slices.DeleteFunc(rr.Status.GitWaits, func(w api.GitWait) bool {
		return w.Kind == api.GitWaitMergeCheck
	})
	cr := providers.ChangeRequest{Number: 1, URL: "https://github.com/example/pr/1", HeadSHA: strings.Repeat("a", 40),
		State: providers.ChangeRequestMerged}
	if p := rr.Status.PullRequest; p != nil {
		cr.Number, cr.URL, cr.HeadSHA = p.Number, p.URL, p.HeadSHA
	}
	if rr.Status.Source != nil {
		cr.BaseBranch = string(rr.Status.Source.TargetBranch)
	}
	t := metav1.NewTime(since)
	rr.Status.MergeCommitAwaitedSince = &t
	w, _ := wholeWait(api.GitWait{Kind: api.GitWaitMergeCheck, PullRequest: waitPullRequest(cr)},
		"wait for the merge commit", since)
	rr.Status.GitWaits = append(rr.Status.GitWaits, w)
}

// startContentWait records a content wait about rr's recorded pull request, as a failed read of what its merge
// brought does.
func startContentWait(ctx context.Context, r *RemediationRequestReconciler, rr *api.RemediationRequest) error {
	pr := rr.Status.PullRequest
	cr := providers.ChangeRequest{Number: pr.Number, URL: pr.URL, HeadSHA: pr.HeadSHA}
	_, err := r.retryGitRead(ctx, rr, readOfContent(cr, readFailure(opParents, errors.New("outage"))),
		unboundedRead(noBoundHeldLock))
	return err
}

// Point 3 of the third decision of 2026-11-17 on #771: a failed read of what a merge brought, under a caller whose
// own wait has an end (the request's deadline), records both waits, each with its own start. Before, the caller's
// kind won over the content wait whose clock had not started, so that clock never started.
func TestContentWaitStartsBesideTheCallersWait(t *testing.T) {
	r, _, rr := settlementWorld(t, api.PhasePolicyChecked)
	ctx := context.Background()
	clock := clockOf(r)
	rr.Status.Deadline = &metav1.Time{Time: clock.at.Add(time.Hour)}
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	pr := rr.Status.PullRequest
	cr := providers.ChangeRequest{Number: pr.Number, URL: pr.URL, HeadSHA: pr.HeadSHA}
	res, err := r.retryGitRead(ctx, rr, readOfContent(cr, readFailure(opParents, errors.New("outage"))),
		boundBy(waitRequestDeadline))
	if err != nil {
		t.Fatal(err)
	}
	content, deadline := contentWaitOf(rr), gitWait(rr, waitRequestDeadline)
	if content == nil || deadline == nil || !content.Since.Equal(&metav1.Time{Time: clock.at}) ||
		content.PullRequest == nil || content.PullRequest.Number != pr.Number {
		t.Fatalf("waits %+v; want the content wait, started now with its pull request, beside the deadline's",
			rr.Status.GitWaits)
	}
	if res.RequeueAfter > mergeContentWait {
		t.Fatalf("the retry waits %v, past the content wait", res.RequeueAfter)
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionGitReadRetry); c == nil ||
		c.Reason != ReasonGitReadUntilContentWait {
		t.Fatalf("GitReadRetry = %+v; want the wait that ends first, the content wait", c)
	}
}

// A read that fails in the path that ends a wait (expiring), whose bound and recorded end have passed, is retried at
// the poll interval: never pulled to the passed end, which would bring the request back at once to read again.
func TestExpiringReadRetryWaitsThePace(t *testing.T) {
	r, _, rr := settlementWorld(t, api.PhasePolicyChecked)
	ctx := context.Background()
	clock := clockOf(r)
	rr.Status.Deadline = &metav1.Time{Time: clock.at.Add(-time.Minute)}
	w, _ := newWait(rr, waitRequestDeadline, "list the change requests of the branch", clock.at.Add(-time.Hour))
	rr.Status.GitWaits = append(rr.Status.GitWaits, w)
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	res, err := r.retryGitRead(expiringCtx(ctx), rr, readFailure(opList, errors.New("outage")), requestBound(rr))
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter < r.pullRequestPollInterval() {
		t.Fatalf("the retry waits %v; want the poll interval (%v) in the path that ends a wait", res.RequeueAfter,
			r.pullRequestPollInterval())
	}
}

// The backfill of a request decided before status.holdsTarget existed records the lock holdsLock reads for it, over
// every phase and recorded pull request state: it never clears a held lock, and records false only with the verdict
// that the status shows it free.
func TestBackfillNeverClearsAHeldLock(t *testing.T) {
	phases := []api.RemediationPhase{api.PhasePolicyChecked, api.PhasePRCreated, api.PhaseAwaitingApproval,
		api.PhaseMerged, api.PhaseWaitingForDelivery, api.PhaseSucceeded, api.PhaseFailed, api.PhaseInconclusive,
		api.PhaseNeedsIntervention, api.PhaseSuperseded, api.PhaseExpired, api.PhaseCancelled, api.PhaseDenied}
	for _, phase := range phases {
		for _, recorded := range []bool{false, true} {
			for _, closed := range []string{"", ReasonChangeRequestClosed, ReasonChangeRequestMerged} {
				t.Run(fmt.Sprintf("%s/recorded=%t/closed=%q", phase, recorded, closed), func(t *testing.T) {
					// Each case builds its own world (#961).
					t.Parallel()
					r, _, rr := settlementWorld(t, phase)
					ctx := context.Background()
					rr.Status.HoldsTarget = nil
					if !recorded {
						rr.Status.PullRequest = nil
					}
					if closed != "" {
						markClosed(rr, closed, "was closed")
					}
					if err := r.Status().Update(ctx, rr); err != nil {
						t.Fatal(err)
					}
					held := holdsLock(rr)
					if err := r.backfill(ctx, rr); err != nil {
						t.Fatal(err)
					}
					if rr.Status.HoldsTarget == nil || holdsLock(rr) != held || *rr.Status.HoldsTarget != held {
						t.Fatalf("backfill changed the lock: held %t before, field %v after", held, rr.Status.HoldsTarget)
					}
				})
			}
		}
	}
}

// unboundedReads lists every failed Git read retried without an end, by the function that retries it and its
// reason (the constant documents why). A new call site fails here until it is listed with its justification.
var unboundedReads = map[string]noBoundReason{
	"checkFailed":     noBoundPolicyCheck,
	"heldUnsettled":   noBoundHeldLock,
	"followHeldMerge": noBoundHeldLock,
	// A replacement whose recovery ended at the bound of its wait (#786) keeps the lock until a read settles it.
	"heldRecovery": noBoundHeldLock,
	// A claim held after the end of the watch is retried within its own entry, which ends it (settleClaim, #1058).
	"claimAfterWatch": noBoundHeldLock,
	"requestBound":    "", // noBoundStaleCleanup and noBoundAfterStop, by the request's state.
	"refuseOrWait":    noBoundRefusedWatch,
	// A request cancelled for its Incident keeps the lock until a read settles its pull request.
	"followCancelledMerge": noBoundHeldLock,
	"closeCancelled":       noBoundHeldLock,
	// A cancel after a change of the target branch is watched until a read settles its pull request.
	"cancelLookFailed": noBoundCancelWatch,
	// A cancel watched after a refusal or past its bound records a read of a wait of its own (#889).
	"cancelFailed": noBoundWatchedCancel,
}

func TestUnboundedReadsAreListed(t *testing.T) {
	seen := map[string]bool{}
	forEachSource(t, func(fset *token.FileSet, owner string, n ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}
		if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "unboundedRead" || owner == "" {
			return
		}
		why, listed := unboundedReads[owner]
		arg, _ := call.Args[0].(*ast.Ident)
		switch {
		case !listed:
			t.Errorf("%s: %s retries a Git read without an end and is not in unboundedReads", fset.Position(call.Pos()), owner)
		case arg == nil:
			t.Errorf("%s: %s gives no named reason", fset.Position(call.Pos()), owner)
		case why != "" && arg.Name != reasonName[why]:
			t.Errorf("%s: %s gives %s, unboundedReads says %s", fset.Position(call.Pos()), owner, arg.Name, why)
		}
		seen[owner] = true
	})
	for owner := range unboundedReads {
		if !seen[owner] {
			t.Errorf("unboundedReads lists %s, which retries no read without an end any more", owner)
		}
	}
}

var reasonName = map[noBoundReason]string{
	noBoundPolicyCheck: "noBoundPolicyCheck", noBoundHeldLock: "noBoundHeldLock",
	noBoundStaleCleanup: "noBoundStaleCleanup", noBoundAfterStop: "noBoundAfterStop",
	noBoundRefusedWatch: "noBoundRefusedWatch", noBoundCancelWatch: "noBoundCancelWatch",
	noBoundWatchedCancel: "noBoundWatchedCancel",
}

// TestHeldUnsettledKeepsTheEndOfAWait checks that a look of a stopped holder that settles nothing never asks to run
// again after the end of a wait the request records (heldUnsettled, withinWaits): both for answers that disagree (no
// failed call) and for a failure PullRequestUnreadable already describes, a MergeContent wait that ends in 30 seconds
// is not carried to heldRecheckInterval.
func TestHeldUnsettledKeepsTheEndOfAWait(t *testing.T) {
	r, _, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	clock := clockOf(r)
	left := 30 * time.Second
	rr.Status.GitWaits = []api.GitWait{{Kind: api.GitWaitMergeContent, Since: metav1.NewTime(clock.at),
		Ends: &metav1.Time{Time: clock.at.Add(left)}, Operation: "read of what the merge brought",
		PullRequest: &api.GitWaitPullRequest{Number: rr.Status.PullRequest.Number, URL: rr.Status.PullRequest.URL,
			HeadSHA: rr.Status.PullRequest.HeadSHA}}}
	// The end is read from the request the API server holds (withinWaits, #885).
	if err := r.Status().Update(context.Background(), rr); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		v    changeVerdict
		err  error
		said bool
	}{
		{"answers disagree", changeVerdict{state: changeUnknown, err: errConflictingReads}, nil, false},
		{"unreadable said", changeVerdict{state: changeUnknown, err: errors.New("no read")}, errors.New("no read"), true},
	} {
		res, err := r.heldUnsettled(context.Background(), rr, c.v, c.err, c.said)
		if err != nil || res.RequeueAfter <= 0 || res.RequeueAfter > left {
			t.Errorf("%s: %+v, %v; want a wait of at most %v, the end of the content wait", c.name, res, err, left)
		}
	}
}

// TestWithinWaitsTakesTheEndsFromTheLiveRequest checks that the cap of a pace at the end of a Git wait comes from the
// request as the API server holds it (#885): a cached copy that still shows a MergeCheck wait past its end, which a
// later write already ended, must not bring the request back after a nanosecond, while a wait the live request
// records still caps the pace at its end, and a passed one at boundPassedWait, so the next reconcile ends it.
func TestWithinWaitsTakesTheEndsFromTheLiveRequest(t *testing.T) {
	ctx := context.Background()
	r, _, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	clock := clockOf(r)
	stored := rr.DeepCopy()
	cached := rr.DeepCopy()
	cached.Status.GitWaits = []api.GitWait{{Kind: api.GitWaitMergeCheck, Since: metav1.NewTime(clock.at.Add(-10 * time.Minute)),
		Ends: &metav1.Time{Time: clock.at.Add(-5 * time.Minute)}, Operation: mergeCheckOp,
		PullRequest: &api.GitWaitPullRequest{Number: rr.Status.PullRequest.Number, URL: rr.Status.PullRequest.URL,
			HeadSHA: rr.Status.PullRequest.HeadSHA}}}
	if d, err := r.withinWaits(ctx, cached, heldRecheckInterval); err != nil || d != heldRecheckInterval {
		t.Fatalf("a wait only a stale copy shows: %v, %v; want %v, the pace, with no cap", d, err, heldRecheckInterval)
	}
	res, err := r.heldUnsettled(ctx, cached, changeVerdict{state: changeUnknown, err: errConflictingReads}, nil, false)
	if err != nil || res.RequeueAfter < time.Second {
		t.Fatalf("held look on a stale copy: %+v, %v; want no requeue sooner than a second", res, err)
	}

	for _, c := range []struct {
		name string
		ends time.Duration
		want time.Duration
	}{
		{"an end ahead", 30 * time.Second, 30 * time.Second},
		{"an end passed", -time.Minute, boundPassedWait},
	} {
		live := stored.DeepCopy()
		if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
			t.Fatal(err)
		}
		live.Status.GitWaits = []api.GitWait{{Kind: api.GitWaitMergeCheck, Since: metav1.NewTime(clock.at.Add(-time.Hour)),
			Ends: &metav1.Time{Time: clock.at.Add(c.ends)}, Operation: mergeCheckOp,
			PullRequest: cached.Status.GitWaits[0].PullRequest}}
		if err := r.Status().Update(ctx, live); err != nil {
			t.Fatal(err)
		}
		if d, err := r.withinWaits(ctx, live, heldRecheckInterval); err != nil || d != c.want {
			t.Errorf("%s: %v, %v; want %v", c.name, d, err, c.want)
		}
	}
}

// TestWithinWaitsReturnsAFailedRead checks that a failed read of the live request is returned for the error backoff,
// never taken as a request with no wait (#885).
func TestWithinWaitsReturnsAFailedRead(t *testing.T) {
	r, _, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	clock := clockOf(r)
	rr.Status.GitWaits = []api.GitWait{{Kind: api.GitWaitMergeCheck, Since: metav1.NewTime(clock.at),
		Ends: &metav1.Time{Time: clock.at.Add(time.Minute)}, Operation: mergeCheckOp,
		PullRequest: &api.GitWaitPullRequest{Number: rr.Status.PullRequest.Number, URL: rr.Status.PullRequest.URL,
			HeadSHA: rr.Status.PullRequest.HeadSHA}}}
	r.APIReader = failingGetReader{Reader: r.APIReader}
	if d, err := r.withinWaits(context.Background(), rr, heldRecheckInterval); err == nil {
		t.Fatalf("got %v with no error; want the failed read", d)
	}
}

// mergeCheckOp is the operation of the MergeCheck entries of these tests.
const mergeCheckOp = "check the plan at the merge"

// failingGetReader fails every Get of a RemediationRequest.
type failingGetReader struct{ client.Reader }

func (f failingGetReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*api.RemediationRequest); ok {
		return errors.New("the API server did not answer")
	}
	return f.Reader.Get(ctx, key, obj, opts...)
}
