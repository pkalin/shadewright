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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
)

// The failed Git read of each case is answered with a delay far longer than any bound.
const hostDelay = time.Hour

func delayedOutage() error {
	return &providers.RetryableError{RetryAfter: hostDelay, Err: errors.New("outage")}
}

// A failed read of the pull request, then a freshness check that runs out of time on a Kubernetes read: the
// timeout is what fails now, and it goes back to the error backoff, never relabelled as the earlier Git read.
func TestFailedLookThenClusterTimeoutKeepsTheTimeout(t *testing.T) {
	r, g, rr := readFailureWorld(t, pathReview)
	ctx := context.Background()
	failed := false
	g.SetFail(func(op string) error {
		if op == opGet && !failed {
			failed = true
			return errors.New("outage")
		}
		return nil
	})
	base := r.Client.(client.WithWatch)
	slow := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*api.RemediationPolicy); ok {
				if _, bounded := ctx.Deadline(); bounded {
					<-ctx.Done()
					return ctx.Err()
				}
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r.Client, r.APIReader = slow, slow
	r.CleanupTimeout = 50 * time.Millisecond
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
	if !failed {
		t.Fatal("the look never read the pull request")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Reconcile error = %v, want the freshness check's timeout for the error backoff", err)
	}
}

// What a retargeted merge brought cannot be read one second before mergeContentWait ends, and the host asks for an
// hour: the request is looked at again within that second, so the wait's own end (MergeContentUnreadable) is on
// time, on the active path (watchStep) and on the stopped holder's (checkHeld).
func TestRetargetedContentReadKeepsItsBound(t *testing.T) {
	for _, path := range []string{pathWatch, pathHeld} {
		t.Run(path, func(t *testing.T) {
			r, g, rr := readFailureWorld(t, path)
			ctx := context.Background()
			now := time.Now().Truncate(time.Second)
			r.Now = func() time.Time { return now }
			rr.Status.MergeContentUnreadSince = &metav1.Time{Time: now.Add(-mergeContentWait + time.Second)}
			if err := r.Status().Update(ctx, rr); err != nil {
				t.Fatal(err)
			}
			reached := false
			g.SetFail(func(op string) error {
				if op == opParents {
					reached = true
					return delayedOutage()
				}
				return nil
			})
			res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
			if !reached {
				t.Fatal("the content of the merge was never read")
			}
			if err != nil || res.RequeueAfter <= 0 || res.RequeueAfter > time.Second {
				t.Fatalf("Reconcile = %+v, %v; want a wait of at most the second left of the content wait", res, err)
			}
		})
	}
}

// The ancestry of a later commit cannot be read one second before the delivery grace ends, and the host asks for an
// hour: the delivery is looked at again within that second, with --verify-outcomes and without, so its verdict
// (DeliveryNotChecked, RolloutNotDetermined) is not an hour late.
func TestDeliveryAncestryReadKeepsTheGrace(t *testing.T) {
	for _, verify := range []bool{false, true} {
		t.Run(fmt.Sprintf("verify outcomes %t", verify), func(t *testing.T) {
			if wait := ancestryRetry(t, verify, deliveryWindow+finalGrace-time.Second); wait > time.Second {
				t.Fatalf("the retry waits %v; want at most the second left of the grace", wait)
			}
		})
	}
}

// ancestryRetry fails the ancestry read of the delivery at since after the merge, with the host asking for an
// hour, and returns the wait of the retry, which must be shown and come with no error.
func ancestryRetry(t *testing.T, verify bool, since time.Duration) time.Duration {
	t.Helper()
	w := newRolloutWorld(t, completeStatefulSet())
	w.r.VerifyOutcomes = verify
	w.withReceipt(t, mainBranch)
	later := w.laterOn(t, mainBranch)
	w.reader.ks = kustomizationApplied(t, later)
	w.now = w.merged.Add(since)
	asked := 0
	w.git.SetFail(func(op string) error {
		if op == "IsAncestor" {
			asked++
			return delayedOutage()
		}
		return nil
	})
	var res reconcile.Result
	var err error
	for range 2 {
		// With --verify-outcomes the first reconcile may only move Merged on to WaitingForDelivery.
		if res, err = w.reconcile(t, t.Context()); asked > 0 {
			break
		}
	}
	if asked == 0 {
		t.Fatal("the ancestry of the later commit was never asked")
	}
	if err != nil || res.RequeueAfter <= 0 {
		t.Fatalf("Reconcile = %+v, %v; want a wait and no error", res, err)
	}
	if meta.FindStatusCondition(w.request(t).Status.Conditions, conditionGitReadRetry) == nil {
		t.Fatal("the retry of the ancestry read is not shown")
	}
	return res.RequeueAfter
}

// A failed read of the review check run while the look only polls the pull request (pollOnly): the retry is shown,
// and the host's hour never passes the full recheck's period that bounds the look.
func TestReviewCheckReadOnAPollKeepsTheRecheckPeriod(t *testing.T) {
	r, g, rr := readFailureWorld(t, pathReview)
	ctx := context.Background()
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	asked := false
	g.SetFail(func(op string) error {
		if op == opReviewCheckRun {
			asked = true
			return delayedOutage()
		}
		return nil
	})
	res, err := r.Reconcile(ctx, request)
	if !asked {
		t.Fatal("the review check run was never read")
	}
	if err != nil || res.RequeueAfter < r.pullRequestPollInterval() || res.RequeueAfter > freshRecheckInterval {
		t.Fatalf("Reconcile = %+v, %v; want a wait between the poll interval and the full recheck's period", res, err)
	}
	if err := r.Get(ctx, request.NamespacedName, rr); err != nil {
		t.Fatal(err)
	}
	if meta.FindStatusCondition(rr.Status.Conditions, conditionGitReadRetry) == nil {
		t.Fatal("the retry of the review check read is not shown")
	}
}
