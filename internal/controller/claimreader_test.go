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
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// A held claim on a merge of another head is read and settled by one reader (settleClaim), by the claim's own number
// and the claim verdicts only, whatever path reaches it (decision of 2026-11-21 on #1058). The probes below are the
// three cases the issue names, each on the path where it was found.

// claimHandingOver is watchingAfterRefusal with the replacement's watch holding a claim on a merge of another head
// (mergeWithoutCommitID), the predecessor still present and handing over to it.
func claimHandingOver(t *testing.T) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest,
	*api.RemediationRequest, providers.ChangeRequest, func(time.Duration)) {
	t.Helper()
	r, g, p, rr, cr, move := watchingAfterRefusal(t)
	mergeWithoutCommitID(t, g, p, cr)
	if got, _ := reconcileOnce(t, r, rr); !otherHeadClaim(got) || claimedNumber(got) != cr.Number {
		t.Fatalf("phase %q, waits %+v: want the watch holding a claim on #%d", got.Status.Phase, got.Status.GitWaits,
			cr.Number)
	}
	return r, g, p, rr, cr, move
}

// moveToOtherBase moves pull request n to a new base branch, as a person who edits its base does: read by its
// number, the claimed merge is then merged elsewhere.
func moveToOtherBase(t *testing.T, g *fakegit.Git, n int64) {
	t.Helper()
	cr, err := g.GetChangeRequest(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	head, err := g.BranchHead(context.Background(), cr.BaseBranch)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.CreateBranch(context.Background(), "release", head); err != nil {
		t.Fatal(err)
	}
	if err := g.SetBase(n, "release"); err != nil {
		t.Fatal(err)
	}
}

// Probe 1: during the watch, the claim is read through the predecessor that still hands over, and the Git host answers
// 404 for the claimed number while the branch lists it. Before, observeFailed had no case for that verdict: the error
// went to the controller's backoff and the claim outlived its end with no condition.
func TestClaimDuringTheWatchEndsWhenTheAnswersDisagree(t *testing.T) {
	r, g, _, rr, cr, move := claimHandingOver(t)
	g.SetFail(func(op string) error {
		if op == opGet {
			return providers.ErrNotFound
		}
		return nil
	})
	claimEndsDisagreed(t, r, rr, cr, move)
}

// Probe 2: after the watch, the claim is read through the predecessor that still hands over, and the claimed pull
// request is read merged into another base. Before, heldRecoveryRead sent that verdict to the generic look, which can
// release the lock with no record; the claim must stand, with the lock, until it is recorded at its own end.
func TestClaimAfterTheWatchKeptOnAMergeElsewhere(t *testing.T) {
	r, g, _, rr, cr, move := claimAfterTheWatch(t)
	moveToOtherBase(t, g, cr.Number)
	got, err := reconcileOnce(t, r, rr)
	if err != nil {
		t.Fatalf("reconcile = %v; want no error", err)
	}
	if !otherHeadClaim(got) || !ptr.Deref(got.Status.HoldsTarget, false) {
		t.Fatalf("phase %q, lock %v, waits %+v: the claim on #%d was settled by the generic look", got.Status.Phase,
			ptr.Deref(got.Status.HoldsTarget, false), got.Status.GitWaits, cr.Number)
	}
	claimEndsAtItsEnd(t, r, rr, cr, move)
}

// Probe 3: during the watch with the predecessor made again, the listing of the branch answers about a pull request
// not asked for. The claim ends on time, and its end says the answers disagreed, not that the reads kept failing.
func TestClaimDuringTheWatchAloneEndsSayingTheAnswersDisagreed(t *testing.T) {
	r, g, _, rr, cr, move := claimedDuringWatch(t)
	g.SetFail(func(op string) error {
		if op == opList {
			return providers.ErrUntrustedAnswer
		}
		return nil
	})
	claimEndsDisagreed(t, r, rr, cr, move)
}

// claimPathCase is a path that reaches a held claim, set up with the claim standing on cr.
type claimPathCase struct {
	name string
	// handsOver is true when the predecessor still hands over to the replacement and records cr.
	handsOver bool
	setup     func(t *testing.T) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest,
		*api.RemediationRequest, providers.ChangeRequest, func(time.Duration))
}

// claimPaths are the four ways to reach a held claim: during the watch and after its end, through the predecessor
// that still hands over and with the predecessor made again under its name.
var claimPaths = []claimPathCase{
	{name: "during the watch, through the predecessor", handsOver: true, setup: claimHandingOver},
	{name: "during the watch, predecessor made again", setup: claimedDuringWatch},
	{name: "after the watch, through the predecessor", handsOver: true, setup: claimAfterTheWatch},
	{name: "after the watch, predecessor made again", setup: func(t *testing.T) (*RemediationRequestReconciler,
		*fakegit.Git, *api.RemediationRequest, *api.RemediationRequest, providers.ChangeRequest, func(time.Duration)) {
		r, g, p, rr, cr, move := claimAfterTheWatch(t)
		remakePredecessor(t, r, p)
		return r, g, p, rr, cr, move
	}},
}

// claimVerdictCase is what the Git host answers about the claimed pull request from the claim's first read on, and
// the end that answer must give on every path.
type claimVerdictCase struct {
	name string
	// answer makes the Git host answer so.
	answer func(t *testing.T, g *fakegit.Git, cr providers.ChangeRequest)
	// recordedAtOnce: the next read records the merge (MergedWithForeignChange); otherwise the claim stands until its
	// own end, which records it MergeContentUnreadable with a message that holds says.
	recordedAtOnce bool
	says           string
}

var claimVerdicts = []claimVerdictCase{
	{name: "merged, the merge commit reported", recordedAtOnce: true,
		answer: func(t *testing.T, g *fakegit.Git, cr providers.ChangeRequest) {
			t.Helper()
			head, err := g.BranchHead(context.Background(), cr.BaseBranch)
			if err != nil {
				t.Fatal(err)
			}
			if err := g.SetMergeCommit(cr.Number, head); err != nil {
				t.Fatal(err)
			}
		}},
	{name: "merged, no merge commit ever reported", says: "kept failing",
		answer: func(*testing.T, *fakegit.Git, providers.ChangeRequest) {}},
	{name: "merged into another base", says: "answered and nothing it found matched",
		answer: func(t *testing.T, g *fakegit.Git, cr providers.ChangeRequest) { moveToOtherBase(t, g, cr.Number) }},
	{name: "the marker taken out", says: "answered and nothing it found matched",
		answer: func(t *testing.T, g *fakegit.Git, cr providers.ChangeRequest) {
			t.Helper()
			if err := g.SetBody(cr.Number, "edited by a person"); err != nil {
				t.Fatal(err)
			}
		}},
	{name: "404 by number, listed on the branch", says: "disagreed",
		answer: func(_ *testing.T, g *fakegit.Git, _ providers.ChangeRequest) {
			g.SetFail(func(op string) error {
				if op == opGet {
					return providers.ErrNotFound
				}
				return nil
			})
		}},
	{name: "an answer not asked for", says: "disagreed",
		answer: func(_ *testing.T, g *fakegit.Git, _ providers.ChangeRequest) {
			g.SetFail(func(op string) error {
				if op == opList {
					return providers.ErrUntrustedAnswer
				}
				return nil
			})
		}},
	{name: "every read fails", says: "kept failing",
		answer: func(_ *testing.T, g *fakegit.Git, _ providers.ChangeRequest) {
			g.SetFail(func(string) error { return unavailable() })
		}},
}

// Every pair of a path and a verdict ends the claim the same way (decision of 2026-11-21 on #1058): no reconcile hands
// an error to the backoff; while the claim stands it keeps the lock and a condition shows it; it ends no later than
// the first reconcile at or past its own end, NeedsIntervention with the merge of the claimed pull request recorded on
// the replacement (with the reason and the message of the verdict), and the predecessor records none.
func TestClaimSettledTheSameOnEveryPath(t *testing.T) {
	for _, path := range claimPaths {
		for _, verdict := range claimVerdicts {
			t.Run(path.name+"/"+verdict.name, func(t *testing.T) {
				r, g, p, rr, cr, move := path.setup(t)
				verdict.answer(t, g, cr)
				ends := gitWait(storedRR(t, r, rr), waitMergeContent).Ends.Time
				got, err := reconcileOnce(t, r, rr)
				if err != nil {
					t.Fatalf("reconcile = %v; want no error", err)
				}
				if !verdict.recordedAtOnce {
					claimStands(t, got, cr)
					move(ends.Sub(r.now()))
					if got, err = reconcileOnce(t, r, rr); err != nil {
						t.Fatalf("reconcile at the claim's end = %v; want no error", err)
					}
				}
				want := ReasonMergeContentUnreadable
				if verdict.recordedAtOnce {
					want = ReasonMergedWithForeignChange
				}
				c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionApproved)
				if got.Status.Phase != api.PhaseNeedsIntervention || c == nil || c.Reason != want ||
					got.Status.PullRequest == nil || got.Status.PullRequest.Number != cr.Number || otherHeadClaim(got) {
					t.Fatalf("phase %q, Approved %+v, waits %+v: want the merge of #%d recorded %s", got.Status.Phase, c,
						got.Status.GitWaits, cr.Number, want)
				}
				if !strings.Contains(c.Message, verdict.says) {
					t.Fatalf("message %q: want it to say %q", c.Message, verdict.says)
				}
				if path.handsOver {
					gotP, _ := reconcileOnce(t, r, p)
					noRecordOnPredecessor(t, gotP)
				}
			})
		}
	}
}

// claimStands fails unless got still holds its claim on cr and the lock, with a condition that shows it.
func claimStands(t *testing.T, got *api.RemediationRequest, cr providers.ChangeRequest) {
	t.Helper()
	if !otherHeadClaim(got) || claimedNumber(got) != cr.Number || !ptr.Deref(got.Status.HoldsTarget, false) {
		t.Fatalf("phase %q, lock %v, waits %+v: want the claim on #%d and the lock kept", got.Status.Phase,
			ptr.Deref(got.Status.HoldsTarget, false), got.Status.GitWaits, cr.Number)
	}
	if !meta.IsStatusConditionTrue(got.Status.Conditions, api.ConditionGitReadRetry) {
		t.Fatalf("conditions %+v: want condition %s to show the claim", got.Status.Conditions,
			api.ConditionGitReadRetry)
	}
}

// #1057: the claim holder is deleted while its settings entry cannot be used. Its finalizer ends the claim (a request
// being deleted holds none) and keeps trying the cleanup at a pace, with conditions that say why, instead of handing the
// settings error to the error backoff with nothing shown.
func TestClaimHolderDeletedWithAnUnusableEntry(t *testing.T) {
	r, _, _, rr, _, _ := claimHandingOver(t)
	r.OpenRepository = func(context.Context, api.GitRepository) (providers.GitProvider, error) {
		return nil, fmt.Errorf("the key is not a PEM key: %w", providers.ErrInvalidConfig)
	}
	held := storedRR(t, r, rr)
	controllerutil.AddFinalizer(held, FinalizerName)
	if err := r.Update(t.Context(), held); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(t.Context(), held); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		res, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
		if err != nil || res.RequeueAfter <= 0 {
			t.Fatalf("reconcile of the deletion = %+v, %v; want a wait and no error", res, err)
		}
	}
	got := storedRR(t, r, rr)
	if !controllerutil.ContainsFinalizer(got, FinalizerName) || otherHeadClaim(got) {
		t.Fatalf("finalizers %v, waits %+v: want the finalizer kept and the claim ended", got.Finalizers,
			got.Status.GitWaits)
	}
	for _, want := range []string{api.ConditionCleanupFailing, api.ConditionPullRequestUnreadable} {
		if c := meta.FindStatusCondition(got.Status.Conditions, want); c == nil || c.Status != metav1.ConditionTrue ||
			c.Reason != reasonRepositoryUnusable {
			t.Fatalf("condition %s = %+v: want it True with reason %s", want, c, reasonRepositoryUnusable)
		}
	}
}

// claimedNumber is, for the tests, the number of the pull request rr's claim on a merge of another head names (otherHeadClaim), 0
// without such a claim.
func claimedNumber(rr *api.RemediationRequest) int64 {
	if !otherHeadClaim(rr) {
		return 0
	}
	return gitWait(rr, waitMergeContent).PullRequest.Number
}
