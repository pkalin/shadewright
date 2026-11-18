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
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
)

// gitReadSite is one call that gives a failed Git read its bound: retryGitRead, waitOrFail (which hands a failed
// read to retryGitRead), deliveryRetry (the same for the delivery) or readOfMergeCheck and readOfContent (a read error
// that carries its wait to the caller's retryGitRead). bound is the source text of its bound argument. check, when set, makes the read fail at that site with a delay of hostDelay, started c.left
// before the bound, with the executor's clock moving on by c.during while the call runs, and reports the wait, or
// that the path's own end ran instead (expired). A bound passed down from the caller (bound) is checked at the
// caller's row. An unbounded site names its reason in the call (unboundedRead).
type gitReadSite struct {
	bound string
	check func(t *testing.T, c boundCase) (wait time.Duration, expired bool)
}

// boundCase is one case of the generated test over the bounded sites: the time left of the bound when the failing
// call starts, how long the call takes on the executor's clock, and --verify-outcomes (for the delivery sites).
type boundCase struct {
	left, during time.Duration
	verify       bool
}

func (c boundCase) String() string {
	return fmt.Sprintf("left %v, call %v, verify outcomes %t", c.left, c.during, c.verify)
}

// testClock is the executor's clock of a check, moved on by hand.
type testClock struct{ at time.Time }

// clockOf sets r's clock to a testClock at the current whole second and returns it.
func clockOf(r *RemediationRequestReconciler) *testClock {
	c := &testClock{at: time.Now().Truncate(time.Second)}
	r.Now = func() time.Time { return c.at }
	return c
}

func (c *testClock) advance(d time.Duration) { c.at = c.at.Add(d) }

// failedRead is a failed Git read whose host asked for hostDelay.
func failedRead() error { return readFailure(opGet, delayedOutage()) }

func mustWait(t *testing.T, res reconcile.Result, err error) time.Duration {
	t.Helper()
	if err != nil {
		t.Fatalf("a failed Git read returned an error for the backoff: %v", err)
	}
	return res.RequeueAfter
}

// failOnce fails the first call op of g with a delay of hostDelay, after moving clock on by during, and reports
// whether it did.
func failOnce(g interface{ SetFail(func(string) error) }, op string, clock *testClock, during time.Duration) *bool {
	failed := new(bool)
	g.SetFail(func(name string) error {
		if name == op && !*failed {
			*failed = true
			clock.advance(during)
			return delayedOutage()
		}
		return nil
	})
	return failed
}

const (
	helperSite    = "gitread.go:retryGitRead:0"
	callerBound   = "bound"
	heldLockBound = "unboundedRead(noBoundHeldLock)"
	// handoffGraceBound is the bound of the retries of a handoff's recovery: the end of its HandoffGrace wait.
	handoffGraceBound = "boundBy(waitHandoffGrace)"
	// handoffRecoveryBound is the bound of the retries of a recovery that waits on a refusal: its HandoffRecovery
	// wait, recorded with the HandoffGrace wait of its deadline, the later of the two ending it (#786).
	handoffRecoveryBound = "boundBy(waitHandoffRecovery)"
	unboundedCalls       = "unboundedRead("
)

var gitReadSites = map[string]gitReadSite{
	"branchchange.go:checkRetargeted:1": {bound: "waitMergeContent",
		check: func(t *testing.T, c boundCase) (time.Duration, bool) {
			r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
			ctx := context.Background()
			if err := g.SetBase(rr.Status.PullRequest.Number, freshOtherBranch); err != nil {
				t.Fatal(err)
			}
			if _, err := g.Merge(rr.Status.PullRequest.Number); err != nil {
				t.Fatal(err)
			}
			cr, err := g.ForTarget(freshOtherBranch).GetChangeRequest(ctx, rr.Status.PullRequest.Number)
			if err != nil {
				t.Fatal(err)
			}
			clock := clockOf(r)
			setContentWait(rr, &metav1.Time{Time: clock.at.Add(-mergeContentWait + c.left)})
			if err := r.Status().Update(ctx, rr); err != nil {
				t.Fatal(err)
			}
			failOnce(g, opParents, clock, c.during)
			_, err = r.checkRetargeted(ctx, g, rr, cr, freshOtherBranch)
			res, err := r.waitOrFail(ctx, rr, err, unboundedRead(noBoundHeldLock))
			return mustWait(t, res, err), false
		}},
	"branchchange.go:followHeldMerge:1": {bound: heldLockBound},
	"branchchange.go:lookUnsettled:1":   {bound: callerBound},
	"cancel.go:cancelLookFailed:1":      {bound: "unboundedRead(noBoundCancelWatch)"},
	"cancel.go:cancelFailed:1": {bound: "paceWithin(left)",
		check: func(t *testing.T, c boundCase) (time.Duration, bool) {
			r, _, rr := settlementWorld(t, api.PhaseAwaitingApproval)
			clock := clockOf(r)
			rr.Status.Conditions = append(rr.Status.Conditions, metav1.Condition{Type: api.ConditionCancelPending,
				Status: metav1.ConditionTrue, Reason: ReasonCancelFailing, Message: "failing",
				LastTransitionTime: metav1.NewTime(clock.at.Add(-cleanupFailingBound + c.left - time.Second))})
			clock.advance(c.during)
			if _, past := r.cancelBound(rr); past {
				return 0, true
			}
			res, err := r.cancelFailed(context.Background(), rr, failedRead(), "read the pull request")
			return mustWait(t, res, err), false
		}},
	helperSite:                                 {},
	"handoff.go:retryRecovery:1":               {bound: handoffGraceBound, check: checkRecoveryBound},
	"recoverywait.go:waitRecovery:1":           {bound: handoffRecoveryBound, check: checkRecoveryWaitBound(false)},
	"recoverywait.go:waitingRecovery:1":        {bound: handoffRecoveryBound, check: checkRecoveryWaitBound(true)},
	"recoverywait.go:heldRecovery:1":           {bound: heldLockBound},
	"incidentcancel.go:followCancelledMerge:1": {bound: heldLockBound},
	"incidentcancel.go:closeCancelled:1":       {bound: heldLockBound},
	"merge.go:mergeCheckFailed:1": {bound: "waitMergeCheck",
		check: func(t *testing.T, c boundCase) (time.Duration, bool) {
			r, _, rr := settlementWorld(t, api.PhaseNeedsIntervention)
			clock := clockOf(r)
			pr := rr.Status.PullRequest
			cr := providers.ChangeRequest{Number: pr.Number, URL: pr.URL, HeadSHA: pr.HeadSHA,
				State: providers.ChangeRequestMerged, BaseBranch: mainBranch}
			setMergeWait(rr, clock.at.Add(-mergeCommitWait+c.left))
			if err := r.Status().Update(context.Background(), rr); err != nil {
				t.Fatal(err)
			}
			clock.advance(c.during)
			err := r.mergeCheckFailed(context.Background(), rr, cr, failedRead())
			res, err := r.waitOrFail(context.Background(), rr, err, unboundedRead(noBoundHeldLock))
			return mustWait(t, res, err), false
		}},
	"merge.go:mergeNotRecorded:1":   {bound: callerBound},
	"phases.go:deliveryRetry:1":     {bound: callerBound},
	"phases.go:deliveryStep:1":      {bound: "paceAt(windowEnd)", check: checkDeliveryWindow},
	"phases.go:deliveryStep:2":      {bound: "paceAt(windowEnd.Add(finalGrace))", check: checkDeliveryBound},
	"rollout.go:waitForDelivery:1":  {bound: "paceAt(windowEnd)", check: checkDeliveryWindow},
	"rollout.go:waitForDelivery:2":  {bound: "paceAt(windowEnd.Add(finalGrace))", check: checkDeliveryBound},
	"pullrequest.go:refuseOrWait:1": {bound: "requestBound(rr)", check: checkRequestBound},
	"pullrequest.go:refuseOrWait:2": {bound: "requestBound(rr)", check: checkRequestBound},
	"pullrequest.go:refuseOrWait:3": {bound: "unboundedRead(noBoundRefusedWatch)"},
	"pullrequest.go:waitOrFail:1":   {bound: callerBound},
	"refusedclose.go:cleanupFailedOnDelete:1": {bound: "paceWithin(failingLeft)",
		check: func(t *testing.T, c boundCase) (time.Duration, bool) {
			r, _, rr := settlementWorld(t, api.PhaseNeedsIntervention)
			clock := clockOf(r)
			err := failedRead()
			failing := cleanupFailingCondition(rr, err)
			failing.LastTransitionTime = metav1.NewTime(clock.at.Add(-cleanupFailingBound + c.left - time.Second))
			rr.Status.Conditions = append(rr.Status.Conditions, failing)
			clock.advance(c.during)
			res, release, err := r.cleanupFailedOnDelete(context.Background(), rr, err)
			if release != nil {
				return 0, true
			}
			return mustWait(t, res, err), false
		}},
	"remediationrequest_controller.go:checkFailed:1": {bound: "unboundedRead(noBoundPolicyCheck)"},
	"remediationrequest_controller.go:recheck:1":     {bound: "lookBound", check: checkLookBound},
	"remediationrequest_controller.go:recheck:2":     {bound: "lookBound", check: checkLookBound},
	"targetlock.go:heldUnsettled:1":                  {bound: heldLockBound},
}

// checkRequestBound: a request in PolicyChecked c.left before its deadline (requestBound) fails a read that takes
// c.during.
func checkRequestBound(t *testing.T, c boundCase) (time.Duration, bool) {
	var wait time.Duration
	for _, unknown := range []bool{true, false} {
		r, _, rr := settlementWorld(t, api.PhasePolicyChecked)
		clock := clockOf(r)
		rr.Status.Deadline = &metav1.Time{Time: clock.at.Add(c.left)}
		ctx := context.Background()
		clock.advance(c.during)
		err := failedRead()
		if unknown {
			err = unknownChange(err).err
		}
		res, rerr := r.refuseOrWait(ctx, rr, err)
		wait = max(wait, mustWait(t, res, rerr))
	}
	return wait, false
}

// checkRecoveryBound: the recovery of a handoff fails a read that takes c.during, c.left before the end of its
// grace.
func checkRecoveryBound(t *testing.T, c boundCase) (time.Duration, bool) {
	r, _, rr := settlementWorld(t, api.PhasePolicyChecked)
	clock := clockOf(r)
	rr.Status.Deadline = &metav1.Time{Time: clock.at.Add(-handoffRecoveryGrace + c.left)}
	// A replacement whose predecessor is reserved: its recovery is what the grace bounds (recoveringHandoff).
	rr.Spec.Replaces = &api.RequestReplacement{}
	rr.Status.Handoff = &api.Handoff{From: &api.IncomingHandoff{Phase: api.HandoffReserved}}
	ctx := context.Background()
	clock.advance(c.during)
	res, err := r.retryRecovery(ctx, rr, failedRead())
	if rr.Status.Phase == api.PhaseNeedsIntervention {
		// Past the grace: the request ended HandoffRecoveryFailed.
		return 0, true
	}
	return mustWait(t, res, err), false
}

// checkRecoveryWaitBound: a replacement whose recovery waits on a refusal (#786) c.left before the end of that wait
// (recoveryWaitBound from the condition's lastTransitionTime, the deadline long past) fails a read that takes
// c.during: the refusal again (waitRecovery) or a failure worth retrying (waitingRecovery, when retryable). The wait
// never ends here; only the check at the start of a reconcile ends it (endPassedWait).
func checkRecoveryWaitBound(retryable bool) func(t *testing.T, c boundCase) (time.Duration, bool) {
	return func(t *testing.T, c boundCase) (time.Duration, bool) {
		r, _, rr := settlementWorld(t, api.PhasePolicyChecked)
		clock := clockOf(r)
		ctx := context.Background()
		// The spec too: the wait's entry is written on the request as the API server holds it (putGitWaits).
		status := rr.Status.DeepCopy()
		rr.Spec.Replaces = &api.RequestReplacement{Name: oldName, UID: "old-uid", Hint: api.HintRef{
			Source: api.HintSourceGitHub, CommentID: 7, Digest: "sha256:" + strings.Repeat("a", 64)}}
		if err := r.Update(ctx, rr); err != nil {
			t.Fatal(err)
		}
		rr.Status = *status
		rr.Status.Handoff = &api.Handoff{From: &api.IncomingHandoff{Phase: api.HandoffReserved}}
		rr.Status.Deadline = &metav1.Time{Time: clock.at.Add(-recoveryWaitBound)}
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{
			Type: api.ConditionPullRequestUnreadable, Status: metav1.ConditionTrue, Reason: ReasonGitForbidden,
			Message: "The Git host refused the read of the branch", LastTransitionTime: metav1.NewTime(clock.at.Add(-recoveryWaitBound + c.left - time.Second))})
		if err := r.Status().Update(ctx, rr); err != nil {
			t.Fatal(err)
		}
		clock.advance(c.during)
		if retryable {
			res, err := r.waitingRecovery(ctx, rr, watchedRecovery(rr), failedRead())
			return mustWait(t, res, err), false
		}
		refused := readFailure(opBranchHead, fmt.Errorf("status 403: %w", providers.ErrForbidden))
		res, err := r.waitRecovery(ctx, rr, watchedRecovery(rr).DeepCopy(), refused)
		return mustWait(t, res, err), false
	}
}

// checkDeliveryBound: the ancestry read of the delivery fails c.left before the grace ends.
func checkDeliveryBound(t *testing.T, c boundCase) (time.Duration, bool) {
	return ancestryRetry(t, c.verify, deliveryWindow+finalGrace-c.left, c.during), false
}

// checkDeliveryWindow: the same c.left before the delivery window ends, when the verdict is due.
func checkDeliveryWindow(t *testing.T, c boundCase) (time.Duration, bool) {
	return ancestryRetry(t, c.verify, deliveryWindow-c.left, c.during), false
}

// checkLookBound: the look at the pull request of a request in PolicyChecked c.left before its deadline fails
// after c.during; the steps after it go on, and the look's retry keeps the deadline (lookBound).
func checkLookBound(t *testing.T, c boundCase) (time.Duration, bool) {
	r, g, rr := readFailureWorld(t, pathCreate)
	clock := clockOf(r)
	rr.Status.Deadline = &metav1.Time{Time: clock.at.Add(c.left)}
	ctx := context.Background()
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	failed := failOnce(g, opGet, clock, c.during)
	res, err := r.recheck(ctx, rr)
	if !*failed {
		t.Fatal("the look never read the pull request")
	}
	if rr.Status.Phase == api.PhaseExpired {
		// The deadline passed during the look: the steps after it expired the request.
		return 0, true
	}
	return mustWait(t, res, err), false
}

// gitReadCalls lists every call of retryGitRead, waitOrFail, deliveryRetry and withReadBound in the package's code (tests
// excluded), as file:function:n, with the source text of its bound argument. The declarations count as n = 0
// only for the helper itself (gitread.go:retryGitRead:0), which is listed so the table names the helper too.
func gitReadCalls(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]string{helperSite: ""}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			n := 0
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				var callee string
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					callee = fun.Sel.Name
				case *ast.Ident:
					callee = fun.Name
				}
				kinded := map[string]string{"readOfMergeCheck": "waitMergeCheck", "readOfContent": "waitMergeContent"}
				if callee != "retryGitRead" && callee != "waitOrFail" && kinded[callee] == "" && callee != "deliveryRetry" {
					return true
				}
				n++
				site := fmt.Sprintf("%s:%s:%d", name, fn.Name.Name, n)
				if kind := kinded[callee]; kind != "" {
					// A read below the path that names its wait: the kind is its bound.
					calls[site] = kind
					return true
				}
				var text bytes.Buffer
				if err := printer.Fprint(&text, fset, call.Args[len(call.Args)-1]); err != nil {
					t.Fatal(err)
				}
				calls[site] = text.String()
				return true
			})
		}
	}
	return calls
}

// Every call that bounds the retry of a failed Git read is in gitReadSites, with the bound it passes, and each
// bound of its own holds by construction, over generated cases of (time left, call duration, --verify-outcomes):
// a read that fails with the host asking for an hour is looked at again no later than its bound, and a bound that
// passed while the call ran ends the wait now (at most boundPassedWait), unless the path's own end ran at once. A
// new call site fails here until it is added with its bound and a check.
func TestEveryGitReadRetrySiteKeepsItsBound(t *testing.T) {
	calls := gitReadCalls(t)
	for site, bound := range calls {
		row, ok := gitReadSites[site]
		switch {
		case !ok:
			t.Errorf("call site %s (bound %s) is not in gitReadSites", site, bound)
		case row.bound != bound:
			t.Errorf("call site %s passes bound %s, the table says %s", site, bound, row.bound)
		case strings.HasPrefix(bound, unboundedCalls) && row.check != nil:
			t.Errorf("call site %s has no bound but a check", site)
		case !strings.HasPrefix(bound, unboundedCalls) && bound != callerBound && site != helperSite &&
			row.check == nil:
			t.Errorf("call site %s has a bound of its own (%s) and no check", site, bound)
		}
	}
	for site := range gitReadSites {
		if _, ok := calls[site]; !ok {
			t.Errorf("gitReadSites lists %s, which is no call site any more", site)
		}
	}
	cases := make([]boundCase, 0, 16)
	for _, left := range []time.Duration{time.Second, 30 * time.Second} {
		for _, during := range []time.Duration{0, 500 * time.Millisecond, 2 * time.Second, time.Minute} {
			for _, verify := range []bool{false, true} {
				cases = append(cases, boundCase{left: left, during: during, verify: verify})
			}
		}
	}
	for site, row := range gitReadSites {
		if row.check == nil {
			continue
		}
		t.Run(site, func(t *testing.T) {
			for _, c := range cases {
				t.Run(c.String(), func(t *testing.T) {
					wait, expired := row.check(t, c)
					rest := c.left - c.during
					switch {
					case expired && rest > 0:
						t.Fatalf("the path ended with %v of its bound left", rest)
					case expired:
					case wait <= 0:
						t.Fatalf("the retry has no wait (%v)", wait)
					case rest > 0 && wait > rest:
						t.Fatalf("the retry waits %v, past the %v left of its bound", wait, rest)
					case rest <= 0 && wait > boundPassedWait:
						t.Fatalf("the bound passed during the call, and the retry still waits %v", wait)
					}
				})
			}
		})
	}
}
