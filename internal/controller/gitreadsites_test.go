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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
)

// gitReadSite is one call that gives a failed Git read its bound: retryGitRead, waitOrFail (which hands a failed
// read to retryGitRead), deliveryRetry (the same for the delivery) or withReadBound. bound is the source text of its bound argument. check, when set, makes
// the read fail at that site with a delay of hostDelay one second before the bound (or, for a bound relative to
// now, at once) and returns the wait and what was left of the bound; a bound passed down from the caller (bound)
// is checked at the caller's row. An unbounded site names its reason in the call (unboundedRead).
type gitReadSite struct {
	bound string
	check func(t *testing.T) (wait, left time.Duration)
}

// failedRead is a failed Git read whose host asked for hostDelay.
func failedRead() error { return readFailure(opGet, delayedOutage()) }

// fixedClock sets r's clock to now, whole seconds, and returns it.
func fixedClock(r *RemediationRequestReconciler) time.Time {
	now := time.Now().Truncate(time.Second)
	r.Now = func() time.Time { return now }
	return now
}

func mustWait(t *testing.T, res reconcile.Result, err error) time.Duration {
	t.Helper()
	if err != nil {
		t.Fatalf("a failed Git read returned an error for the backoff: %v", err)
	}
	return res.RequeueAfter
}

const (
	helperSite     = "gitread.go:retryGitRead:0"
	callerBound    = "bound"
	heldLockBound  = "unboundedRead(noBoundHeldLock)"
	unboundedCalls = "unboundedRead("
)

var gitReadSites = map[string]gitReadSite{
	"branchchange.go:checkRetargeted:1": {bound: "readBoundAt(since.Add(mergeContentWait))",
		check: func(t *testing.T) (time.Duration, time.Duration) {
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
			now := fixedClock(r)
			rr.Status.MergeContentUnreadSince = &metav1.Time{Time: now.Add(-mergeContentWait + time.Second)}
			g.SetFail(func(op string) error {
				if op == opParents {
					return delayedOutage()
				}
				return nil
			})
			_, err = r.checkRetargeted(ctx, g, rr, cr, freshOtherBranch)
			res, err := r.waitOrFail(ctx, rr, err, unboundedRead(noBoundHeldLock))
			return mustWait(t, res, err), time.Second
		}},
	"branchchange.go:followHeldMerge:1": {bound: heldLockBound},
	"branchchange.go:lookUnsettled:1":   {bound: callerBound},
	"cancel.go:cancelFailed:1": {bound: "readBoundIn(left)",
		check: func(t *testing.T) (time.Duration, time.Duration) {
			r, _, rr := settlementWorld(t, api.PhaseAwaitingApproval)
			now := fixedClock(r)
			rr.Status.Conditions = append(rr.Status.Conditions, metav1.Condition{Type: api.ConditionCancelPending,
				Status: metav1.ConditionTrue, Reason: ReasonCancelFailing, Message: "failing",
				LastTransitionTime: metav1.NewTime(now.Add(-cleanupFailingBound))})
			res, err := r.cancelFailed(context.Background(), rr, failedRead(), "read the pull request")
			return mustWait(t, res, err), time.Second
		}},
	helperSite:                   {},
	"handoff.go:retryRecovery:1": {bound: "readBoundAt(rr.Status.Deadline.Add(handoffRecoveryGrace))", check: checkRecoveryBound},
	"incidentcancel.go:followCancelledMerge:1": {bound: heldLockBound},
	"incidentcancel.go:closeCancelled:1":       {bound: heldLockBound},
	"merge.go:mergeCheckFailed:1": {bound: "readBoundAt(since.Add(mergeCommitWait))",
		check: func(t *testing.T) (time.Duration, time.Duration) {
			r, _, rr := settlementWorld(t, api.PhaseNeedsIntervention)
			now := fixedClock(r)
			rr.Status.MergeCommitAwaitedSince = &metav1.Time{Time: now.Add(-mergeCommitWait + time.Second)}
			ctx := context.Background()
			err := r.mergeCheckFailed(ctx, rr, failedRead())
			res, err := r.waitOrFail(ctx, rr, err, unboundedRead(noBoundHeldLock))
			return mustWait(t, res, err), time.Second
		}},
	"merge.go:mergeNotRecorded:1":   {bound: callerBound},
	"phases.go:deliveryRetry:1":     {bound: callerBound},
	"phases.go:deliveryStep:1":      {bound: "readBoundAt(windowEnd)", check: checkDeliveryWindow},
	"phases.go:deliveryStep:2":      {bound: "readBoundAt(windowEnd.Add(finalGrace))", check: checkDeliveryBound},
	"rollout.go:waitForDelivery:1":  {bound: "readBoundAt(windowEnd)", check: checkDeliveryWindow},
	"rollout.go:waitForDelivery:2":  {bound: "readBoundAt(windowEnd.Add(finalGrace))", check: checkDeliveryBound},
	"pullrequest.go:refuseOrWait:1": {bound: "requestBound(rr)", check: checkRequestBound},
	"pullrequest.go:refuseOrWait:2": {bound: "requestBound(rr)", check: checkRequestBound},
	"pullrequest.go:refuseOrWait:3": {bound: "unboundedRead(noBoundRefusedWatch)"},
	"pullrequest.go:waitOrFail:1":   {bound: callerBound},
	"refusedclose.go:cleanupFailedOnDelete:1": {bound: "readBoundIn(failingLeft)",
		check: func(t *testing.T) (time.Duration, time.Duration) {
			r, _, rr := settlementWorld(t, api.PhaseNeedsIntervention)
			now := fixedClock(r)
			err := failedRead()
			failing := cleanupFailingCondition(rr, err)
			failing.LastTransitionTime = metav1.NewTime(now.Add(-cleanupFailingBound))
			rr.Status.Conditions = append(rr.Status.Conditions, failing)
			res, release, err := r.cleanupFailedOnDelete(context.Background(), rr, err)
			if release != nil {
				t.Fatal("the cleanup let the request go before its bound")
			}
			return mustWait(t, res, err), time.Second
		}},
	"remediationrequest_controller.go:checkFailed:1": {bound: "unboundedRead(noBoundPolicyCheck)"},
	"remediationrequest_controller.go:recheck:1":     {bound: "lookBound", check: checkLookBound},
	"remediationrequest_controller.go:recheck:2":     {bound: "lookBound", check: checkLookBound},
	"targetlock.go:heldUnsettled:1":                  {bound: heldLockBound},
}

// checkRequestBound: a request in PolicyChecked one second before its deadline (requestBound) fails a read.
func checkRequestBound(t *testing.T) (time.Duration, time.Duration) {
	r, _, rr := settlementWorld(t, api.PhasePolicyChecked)
	now := fixedClock(r)
	rr.Status.Deadline = &metav1.Time{Time: now.Add(time.Second)}
	res, err := r.refuseOrWait(context.Background(), rr, unknownChange(failedRead()).err)
	wait := mustWait(t, res, err)
	if res, err = r.refuseOrWait(context.Background(), rr, failedRead()); mustWait(t, res, err) > wait {
		wait = res.RequeueAfter
	}
	return wait, time.Second
}

// checkRecoveryBound: the recovery of a handoff fails a read one second before the end of its grace.
func checkRecoveryBound(t *testing.T) (time.Duration, time.Duration) {
	r, _, rr := settlementWorld(t, api.PhasePolicyChecked)
	now := fixedClock(r)
	rr.Status.Deadline = &metav1.Time{Time: now.Add(-handoffRecoveryGrace + time.Second)}
	res, err := r.retryRecovery(context.Background(), rr, failedRead())
	return mustWait(t, res, err), time.Second
}

// checkDeliveryBound: the ancestry read of the delivery fails one second before the grace ends, both modes.
func checkDeliveryBound(t *testing.T) (time.Duration, time.Duration) {
	return max(ancestryRetry(t, false, deliveryWindow+finalGrace-time.Second),
		ancestryRetry(t, true, deliveryWindow+finalGrace-time.Second)), time.Second
}

// checkDeliveryWindow: the same one second before the delivery window ends, when the verdict is due.
func checkDeliveryWindow(t *testing.T) (time.Duration, time.Duration) {
	return max(ancestryRetry(t, false, deliveryWindow-time.Second),
		ancestryRetry(t, true, deliveryWindow-time.Second)), time.Second
}

// checkLookBound: the look at the pull request of a request in PolicyChecked one second before its deadline fails;
// the steps after it go on, and the look's retry keeps the deadline (lookBound).
func checkLookBound(t *testing.T) (time.Duration, time.Duration) {
	r, g, rr := readFailureWorld(t, pathCreate)
	now := fixedClock(r)
	rr.Status.Deadline = &metav1.Time{Time: now.Add(time.Second)}
	ctx := context.Background()
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	failed := false
	g.SetFail(func(op string) error {
		if op == opGet && !failed {
			failed = true
			return delayedOutage()
		}
		return nil
	})
	res, err := r.recheck(ctx, rr)
	if !failed {
		t.Fatal("the look never read the pull request")
	}
	return mustWait(t, res, err), time.Second
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
				if callee != "retryGitRead" && callee != "waitOrFail" && callee != "withReadBound" && callee != "deliveryRetry" {
					return true
				}
				n++
				var text bytes.Buffer
				if err := printer.Fprint(&text, fset, call.Args[len(call.Args)-1]); err != nil {
					t.Fatal(err)
				}
				calls[fmt.Sprintf("%s:%s:%d", name, fn.Name.Name, n)] = text.String()
				return true
			})
		}
	}
	return calls
}

// Every call that bounds the retry of a failed Git read is in gitReadSites, with the bound it passes, and each
// bound of its own holds: a read that fails one second before it, with the host asking for an hour, is looked at
// again within that second. A new call site fails here until it is added with its bound and a check.
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
	for site, row := range gitReadSites {
		if row.check == nil {
			continue
		}
		t.Run(site, func(t *testing.T) {
			if wait, left := row.check(t); wait > left {
				t.Fatalf("the retry waits %v, past the %v left of its bound", wait, left)
			}
		})
	}
}
