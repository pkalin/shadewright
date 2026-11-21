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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
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

// claimSetup is a way to stand a claim on cr: during the watch or after its end, the predecessor handing over.
type claimSetup func(t *testing.T) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest,
	*api.RemediationRequest, providers.ChangeRequest, func(time.Duration))

// predecessorChange is what becomes of the predecessor once the claim stands (nil: it still hands over and records
// cr).
type predecessorChange struct {
	name   string
	change func(t *testing.T, r *RemediationRequestReconciler, p *api.RemediationRequest)
}

var predecessorChanges = []predecessorChange{
	{name: "through the predecessor"},
	{name: "predecessor made again", change: func(t *testing.T, r *RemediationRequestReconciler,
		p *api.RemediationRequest) {
		t.Helper()
		remakePredecessor(t, r, p)
	}},
	{name: "predecessor gone", change: func(t *testing.T, r *RemediationRequestReconciler, p *api.RemediationRequest) {
		t.Helper()
		p = storedRR(t, r, p)
		p.Finalizers = nil
		if err := r.Update(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		if err := r.Delete(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "predecessor no longer hands over", change: func(t *testing.T, r *RemediationRequestReconciler,
		p *api.RemediationRequest) {
		t.Helper()
		p = storedRR(t, r, p)
		p.Status.Handoff.To.UID = "0ad0ff00-0000-4000-8000-000000000006"
		if err := r.Status().Update(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "predecessor records another number", change: func(t *testing.T, r *RemediationRequestReconciler,
		p *api.RemediationRequest) {
		t.Helper()
		p = storedRR(t, r, p)
		p.Status.PullRequest.Number += 100
		if err := r.Status().Update(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}},
}

// claimPaths are the ways to reach a held claim: during the watch and after its end, each with the predecessor still
// handing over, made again under its name, gone, no longer handing over to the replacement, or recording another
// pull request number (decision of 2026-11-21 on #1058).
var claimPaths = func() []claimPathCase {
	paths := make([]claimPathCase, 0, 2*len(predecessorChanges))
	for _, when := range []struct {
		name  string
		setup claimSetup
	}{{"during the watch", claimHandingOver}, {"after the watch", claimAfterTheWatch}} {
		for _, pc := range predecessorChanges {
			paths = append(paths, claimPathCase{name: when.name + ", " + pc.name, handsOver: pc.change == nil,
				setup: func(t *testing.T) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest,
					*api.RemediationRequest, providers.ChangeRequest, func(time.Duration)) {
					r, g, p, rr, cr, move := when.setup(t)
					if pc.change != nil {
						pc.change(t, r, p)
					}
					return r, g, p, rr, cr, move
				}})
		}
	}
	return paths
}()

// answeredAs is the repository with change request n answered as set does it, by its number and in every listing.
type answeredAs struct {
	providers.GitProvider
	n   int64
	set func(*providers.ChangeRequest)
}

func (a *answeredAs) GetChangeRequest(ctx context.Context, n int64) (providers.ChangeRequest, error) {
	cr, err := a.GitProvider.GetChangeRequest(ctx, n)
	if err == nil && n == a.n {
		a.set(&cr)
	}
	return cr, err
}

func (a *answeredAs) ListChangeRequests(ctx context.Context, head string, openOnly bool) (
	[]providers.ChangeRequest, error) {
	all, err := a.GitProvider.ListChangeRequests(ctx, head, openOnly)
	return a.each(all), err
}

func (a *answeredAs) ListChangeRequestsAnyBase(ctx context.Context, head string, openOnly bool) (
	[]providers.ChangeRequest, error) {
	all, err := a.GitProvider.ListChangeRequestsAnyBase(ctx, head, openOnly)
	return a.each(all), err
}

func (a *answeredAs) each(all []providers.ChangeRequest) []providers.ChangeRequest {
	for i := range all {
		if all[i].Number == a.n {
			a.set(&all[i])
		}
	}
	return all
}

// wrapRepository has r open every repository through wrap.
func wrapRepository(r *RemediationRequestReconciler, wrap func(providers.GitProvider) providers.GitProvider) {
	open := r.OpenRepository
	r.OpenRepository = func(ctx context.Context, repo api.GitRepository) (providers.GitProvider, error) {
		git, err := open(ctx, repo)
		if err != nil {
			return nil, err
		}
		return wrap(git), nil
	}
}

// claimVerdictCase is what the Git host answers about the claimed pull request from the claim's first read on, and
// the end that answer must give on every path.
type claimVerdictCase struct {
	name string
	// answer makes the Git host answer so.
	answer func(t *testing.T, r *RemediationRequestReconciler, g *fakegit.Git, cr providers.ChangeRequest)
	// recordedAtOnce: the next read records the merge (MergedWithForeignChange); otherwise the claim stands until its
	// own end, which records it MergeContentUnreadable with a message that holds says.
	recordedAtOnce bool
	// lookAgain is a read of the claim confirmWait after the first, before the move to its end: for a pull request
	// gone, the end then falls on the first look of a new confirmation instead of the one that confirms it.
	lookAgain bool
	says      string
}

const answeredNothingMatched = "answered and nothing it found matched"

// goneVerdict is the claimed pull request deleted from the host: 404 by its number, in no listing.
func goneVerdict(t *testing.T, r *RemediationRequestReconciler, _ *fakegit.Git, cr providers.ChangeRequest) {
	t.Helper()
	wrapRepository(r, func(git providers.GitProvider) providers.GitProvider {
		return &deletedChangeRequest{GitProvider: git, n: cr.Number}
	})
}

var claimVerdicts = []claimVerdictCase{
	{name: "merged, the merge commit reported", recordedAtOnce: true,
		answer: func(t *testing.T, _ *RemediationRequestReconciler, g *fakegit.Git, cr providers.ChangeRequest) {
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
		answer: func(*testing.T, *RemediationRequestReconciler, *fakegit.Git, providers.ChangeRequest) {}},
	{name: "merged into another base", says: answeredNothingMatched,
		answer: func(t *testing.T, _ *RemediationRequestReconciler, g *fakegit.Git, cr providers.ChangeRequest) {
			moveToOtherBase(t, g, cr.Number)
		}},
	{name: "the marker taken out", says: answeredNothingMatched,
		answer: func(t *testing.T, _ *RemediationRequestReconciler, g *fakegit.Git, cr providers.ChangeRequest) {
			t.Helper()
			if err := g.SetBody(cr.Number, "edited by a person"); err != nil {
				t.Fatal(err)
			}
		}},
	{name: "closed by a person", says: answeredNothingMatched,
		answer: func(t *testing.T, r *RemediationRequestReconciler, _ *fakegit.Git, cr providers.ChangeRequest) {
			wrapRepository(r, func(git providers.GitProvider) providers.GitProvider {
				return &answeredAs{GitProvider: git, n: cr.Number, set: func(c *providers.ChangeRequest) {
					c.State, c.MergeCommit = providers.ChangeRequestClosed, ""
				}}
			})
		}},
	{name: "gone, the end confirms it", says: answeredNothingMatched, answer: goneVerdict},
	{name: "gone, the end on a first look", says: answeredNothingMatched, answer: goneVerdict, lookAgain: true},
	{name: "404 by number, listed on the branch", says: "disagreed",
		answer: func(_ *testing.T, _ *RemediationRequestReconciler, g *fakegit.Git, _ providers.ChangeRequest) {
			g.SetFail(func(op string) error {
				if op == opGet {
					return providers.ErrNotFound
				}
				return nil
			})
		}},
	{name: "an answer not asked for", says: "disagreed",
		answer: func(_ *testing.T, _ *RemediationRequestReconciler, g *fakegit.Git, _ providers.ChangeRequest) {
			g.SetFail(func(op string) error {
				if op == opList {
					return providers.ErrUntrustedAnswer
				}
				return nil
			})
		}},
	{name: "every read fails", says: "kept failing",
		answer: func(_ *testing.T, _ *RemediationRequestReconciler, g *fakegit.Git, _ providers.ChangeRequest) {
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
				verdict.answer(t, r, g, cr)
				ends := gitWait(storedRR(t, r, rr), waitMergeContent).Ends.Time
				got, err := reconcileOnce(t, r, rr)
				if err != nil {
					t.Fatalf("reconcile = %v; want no error", err)
				}
				if verdict.lookAgain {
					claimStands(t, got, cr)
					move(confirmWait)
					if got, err = reconcileOnce(t, r, rr); err != nil {
						t.Fatalf("reconcile a confirmation later = %v; want no error", err)
					}
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

// #1057: the claim holder is deleted while its settings entry cannot be used (the cleanup of any request deleted
// with an unusable entry or no settings object goes the same way; the claim holder is the case the issue found). Its
// finalizer ends the claim (a request being deleted holds none) and keeps trying the cleanup at a pace, with conditions
// that say why and the Warning Event PullRequestUnreadable every other path records with that condition, instead of
// handing the settings error to the error backoff with nothing shown.
func TestClaimHolderDeletedWithAnUnusableEntry(t *testing.T) {
	r, _, _, rr, _, _ := claimHandingOver(t)
	rec := record.NewFakeRecorder(20)
	r.Recorder = rec
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
	ev := drain(rec)
	if n := countPrefix(ev, "Warning "+EventPullRequestUnreadable+" "); n != 1 {
		t.Fatalf("Events %q: want one Warning %s, for the one change of the condition", ev, EventPullRequestUnreadable)
	}
}

// countPrefix is how many of events start with prefix.
func countPrefix(events []string, prefix string) int {
	n := 0
	for _, e := range events {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

// Finding of 2026-11-21 on #1058: a failure of a read of the claim that is neither the Kubernetes API's nor a Git read
// (every provider call is named a Git read by changeOf, so this is a failure of the executor's own, such as an error
// of the record's step it does not classify) is not named a Git read. claimReadFailed returns it for the error backoff,
// as observeFailed does, records no wait for "read the Git host" and no failed read for the claim's end, so the claim
// never ends MergeContentUnreadable because of it.
func TestClaimReadOfAnUnknownFailureGoesToTheBackoff(t *testing.T) {
	for _, path := range []struct {
		name  string
		setup claimSetup
		via   func(*RemediationRequestReconciler) claimPath
	}{
		{"during the watch", claimHandingOver, (*RemediationRequestReconciler).claimDuringWatch},
		{"after the watch", claimAfterTheWatch, func(*RemediationRequestReconciler) claimPath { return claimAfterWatch() }},
	} {
		t.Run(path.name, func(t *testing.T) {
			r, _, _, rr, _, move := path.setup(t)
			held := storedRR(t, r, rr)
			// The last read past the claim's end, where a failed read would end it MergeContentUnreadable.
			move(gitWait(held, waitMergeContent).Ends.Sub(r.now()))
			key := client.ObjectKeyFromObject(held)
			r.gitRetries().markLastRead(key, []waitKind{waitMergeContent})
			unknown := errors.New("an answer the executor does not know")
			_, out, err := r.claimReadFailed(t.Context(), held, path.via(r), unknown)
			if !errors.Is(err, unknown) || out != claimUnread {
				t.Fatalf("claimReadFailed = %v, %v; want the failure returned for the error backoff, unread", out, err)
			}
			if m := r.gitRetries().take(key); m.failed != "" {
				t.Fatalf("failed read %q noted: the claim's end would say the reads kept failing", m.failed)
			}
			got := storedRR(t, r, rr)
			for _, w := range got.Status.GitWaits {
				if w.Operation == "read the Git host" {
					t.Fatalf("waits %+v: the failure was recorded as a Git read", got.Status.GitWaits)
				}
			}
		})
	}
}

// Finding of 2026-11-21 on #1058: during a cancel, a failed read of the claimed pull request is no read that
// answered. The cancel waits and the claim and the lock are kept, as for any failed read of the watch on main
// (observeFailed); once a read answers and records nothing, the cancel goes on.
func TestClaimDuringACancelWaitsForARead(t *testing.T) {
	r, g, _, rr, cr, _ := claimHandingOver(t)
	held := storedRR(t, r, rr)
	held.Spec.Cancel = true
	if err := r.Update(t.Context(), held); err != nil {
		t.Fatal(err)
	}
	g.SetFail(func(op string) error {
		if op == opGet {
			return unavailable()
		}
		return nil
	})
	got, err := reconcileOnce(t, r, rr)
	if err != nil {
		t.Fatalf("reconcile = %v; want no error", err)
	}
	if got.Status.Phase != api.PhasePolicyChecked {
		t.Fatalf("phase %q: the cancel finished on a read of the claim that failed", got.Status.Phase)
	}
	claimStands(t, got, cr)
}

// Finding of 2026-11-21 on #1058: the settings entry of the plan's repository cannot be used, so the claim's read
// cannot start, and the write of condition PullRequestUnreadable that says so is lost. The failed open is still
// recorded (condition GitReadRetry names it), and the lost write is returned for the error backoff, not joined away.
func TestClaimReadUnstartableWithTheConditionWriteLost(t *testing.T) {
	r, _, _, rr, _, _ := claimAfterTheWatch(t)
	r.OpenRepository = func(context.Context, api.GitRepository) (providers.GitProvider, error) {
		return nil, fmt.Errorf("the key is not a PEM key: %w", providers.ErrInvalidConfig)
	}
	lost := errors.New("condition write lost")
	r.Client = &unreadablePatchLost{Client: r.Client, err: lost}
	if _, err := reconcileOnce(t, r, rr); !errors.Is(err, lost) {
		t.Fatalf("reconcile = %v; want the lost write returned", err)
	}
	got := storedRR(t, r, rr)
	if c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionGitReadRetry); c == nil ||
		!strings.Contains(c.Message, "open the repository") {
		t.Fatalf("GitReadRetry %+v, waits %+v: want the failed open recorded", c, got.Status.GitWaits)
	}
}

// unreadablePatchLost fails every status patch that sets condition PullRequestUnreadable to an unusable entry, with err.
type unreadablePatchLost struct {
	client.Client
	err error
}

func (u *unreadablePatchLost) Status() client.SubResourceWriter {
	return &unreadableStatusLost{SubResourceWriter: u.Client.Status(), err: u.err}
}

type unreadableStatusLost struct {
	client.SubResourceWriter
	err error
}

func (u *unreadableStatusLost) Patch(ctx context.Context, obj client.Object, patch client.Patch,
	opts ...client.SubResourcePatchOption) error {
	if rr, ok := obj.(*api.RemediationRequest); ok {
		if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable); c != nil &&
			c.Reason == reasonRepositoryUnusable {
			return u.err
		}
	}
	return u.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

// claimedNumber is, for the tests, the number of the pull request rr's claim on a merge of another head names (otherHeadClaim), 0
// without such a claim.
func claimedNumber(rr *api.RemediationRequest) int64 {
	if !otherHeadClaim(rr) {
		return 0
	}
	return gitWait(rr, waitMergeContent).PullRequest.Number
}
