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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// A Git read of a waiting recovery that runs into the step's own deadline keeps the wait (#786, decision of
// 2026-11-16 on a step that reaches its own deadline).

// hangingGit is a Git host whose read of a branch head, while hang is set, answers only when its context ends: the
// host accepted the connection and did not answer in time.
type hangingGit struct {
	*fakegit.Git
	hang     atomic.Bool
	onHang   func()
	hangings atomic.Int32
}

func (g *hangingGit) BranchHead(ctx context.Context, branch string) (string, error) {
	if g.hang.Load() {
		g.hangings.Add(1)
		if g.onHang != nil {
			g.onHang()
		}
		<-ctx.Done()
		return "", ctx.Err()
	}
	return g.Git.BranchHead(ctx, branch)
}

// hangingWorld is handoffAppendedWorld whose repository opens to a hangingGit, with a short step deadline.
func hangingWorld(t *testing.T) (*RemediationRequestReconciler, *hangingGit, *api.RemediationRequest,
	providers.ChangeRequest) {
	t.Helper()
	r, g, _, rr, cr, _ := handoffAppendedWorld(t, nil)
	h := &hangingGit{Git: g}
	r.OpenRepository = func(context.Context, api.GitRepository) (providers.GitProvider, error) { return h, nil }
	r.WriteTimeout = 20 * time.Millisecond
	return r, h, rr, cr
}

// While a recovery waits, a read of the branch that reaches the step's deadline keeps the wait past the deadline
// and its grace, with the poll interval as pacing and the condition naming the read that times out; once the host
// answers again, a merge of the replacement's commit is recorded.
func TestRecoveryKeepsWaitingThroughAStepTimeout(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		// cause starts the wait and returns the function that removes the cause.
		cause func(*RemediationRequestReconciler, *hangingGit) func()
		// want is the reason of the condition while the read of the branch times out.
		want string
	}{
		{name: "refused branch read", reason: ReasonGitForbidden,
			cause: func(_ *RemediationRequestReconciler, h *hangingGit) func() {
				refuseBranchHead(h.Git)
				return func() { h.SetFail(nil) }
			},
			want: ReasonGitForbidden},
		{name: "unusable settings", reason: reasonRepositoryUnusable,
			cause: func(r *RemediationRequestReconciler, _ *hangingGit) func() {
				open := r.OpenRepository
				r.OpenRepository = func(context.Context, api.GitRepository) (providers.GitProvider, error) {
					return nil, fmt.Errorf("the key is not a PEM key: %w", providers.ErrInvalidConfig)
				}
				return func() { r.OpenRepository = open }
			},
			want: ReasonRecoveryReadFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, h, rr, cr := hangingWorld(t)
			ctx := context.Background()
			fix := tc.cause(r, h)
			if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
				t.Fatal(err)
			}
			if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable); c == nil ||
				c.Reason != tc.reason {
				t.Fatalf("PullRequestUnreadable = %+v, want reason %s", c, tc.reason)
			}

			fix()
			h.hang.Store(true)
			pastDeadline(t, r, rr, handoffRecoveryGrace+2*time.Hour)
			for range 2 {
				res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
				if err != nil {
					t.Fatalf("Reconcile: %v", err)
				}
				if res.RequeueAfter != DefaultPullRequestPollInterval {
					t.Errorf("RequeueAfter = %v, want the poll interval", res.RequeueAfter)
				}
				if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
					t.Fatal(err)
				}
				if rr.Status.Phase != api.PhasePolicyChecked {
					t.Fatalf("phase = %q: a step timeout ended a replacement that waits", rr.Status.Phase)
				}
				c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable)
				if c == nil || c.Reason != tc.want || !strings.Contains(c.Message, branchHeadRead) {
					t.Fatalf("PullRequestUnreadable = %+v, want reason %s naming %q", c, tc.want, branchHeadRead)
				}
			}
			if h.hangings.Load() == 0 {
				t.Fatal("the read of the branch never hung")
			}

			h.hang.Store(false)
			if _, err := h.MergeWithCommit(cr.Number); err != nil {
				t.Fatal(err)
			}
			if got := reconcileUntilMoved(t, r, rr); got.Status.Phase != api.PhaseMerged {
				t.Fatalf("phase = %q: the merge after the wait was not recorded", got.Status.Phase)
			}
		})
	}
}

// The end of the reconcile's own context while a waiting recovery reads (a cancellation, or a deadline of the
// caller's context that is not the step's) is not a step timeout: it is returned for a retry, past the grace too,
// and the request keeps its phase and its condition. Reconcile drops a cancellation on purpose (the manager stops),
// so the cancellation is checked one level down.
func TestRecoveryReturnsTheEndOfTheReconcileWhileWaiting(t *testing.T) {
	for _, tc := range []struct {
		name string
		// parent is the reconcile's context; end, called while the read hangs, ends it.
		parent func() (ctx context.Context, end func())
		want   error
	}{
		{name: "reconcile cancelled", want: context.Canceled,
			parent: func() (context.Context, func()) { return context.WithCancel(context.Background()) }},
		{name: "caller's deadline", want: context.DeadlineExceeded,
			parent: func() (context.Context, func()) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				return ctx, func() { <-ctx.Done(); cancel() }
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, h, rr, _ := hangingWorld(t)
			r.WriteTimeout = time.Minute
			ctx := context.Background()
			refuseBranchHead(h.Git)
			if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
				t.Fatal(err)
			}
			was := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable).DeepCopy()
			h.SetFail(nil)
			pastDeadline(t, r, rr, handoffRecoveryGrace+2*time.Hour)

			parent, end := tc.parent()
			defer end()
			h.onHang = end
			h.hang.Store(true)
			_, err := r.reconcile(parent, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
			if !errors.Is(err, tc.want) || errors.Is(err, ErrTimeout) {
				t.Fatalf("reconcile: %v, want %v returned and no step timeout", err, tc.want)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
				t.Fatal(err)
			}
			if rr.Status.Phase != api.PhasePolicyChecked {
				t.Fatalf("phase = %q", rr.Status.Phase)
			}
			if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable); c == nil ||
				c.Reason != was.Reason || c.Message != was.Message {
				t.Fatalf("PullRequestUnreadable = %+v, want it kept as %+v", c, was)
			}
		})
	}
}

// Unchanged (#688): a replacement that never met a refusal ends NeedsIntervention with HandoffRecoveryFailed once
// a step timeout outlasts the grace.
func TestRecoveryWithoutARefusalKeepsTheGraceForAStepTimeout(t *testing.T) {
	r, h, rr, _ := hangingWorld(t)
	ctx := context.Background()
	h.hang.Store(true)
	pastDeadline(t, r, rr, handoffRecoveryGrace+time.Minute)
	// The read that times out records the HandoffGrace wait, past its end; the next reconcile ends it (endPassedWait).
	for range 2 {
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhaseNeedsIntervention {
		t.Fatalf("phase = %q, want NeedsIntervention once the grace is over", rr.Status.Phase)
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPROpened); c == nil ||
		c.Reason != ReasonHandoffRecoveryFailed {
		t.Fatalf("PullRequestOpened: %+v", c)
	}
	if h.hangings.Load() == 0 {
		t.Fatal("the read of the branch never hung")
	}
}
