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
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// A claim on a merge of another head decides by its own pull request number everywhere it is read (decision of
// 2026-11-21 on #870, narrow): another merged pull request of the chain's branch is never recorded for it, a
// predecessor made again after the watch sends it through the claim rule, and its end says when the reads answered
// and nothing matched it.

// remakePredecessor deletes p and makes a request again under its name with another UID, as a person can.
func remakePredecessor(t *testing.T, r *RemediationRequestReconciler, p *api.RemediationRequest) {
	t.Helper()
	ctx := t.Context()
	p = storedRR(t, r, p)
	p.Finalizers = nil
	if err := r.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	again := &api.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.Name,
		UID: "0ad0ff00-0000-4000-8000-000000000005"}, Spec: *p.Spec.DeepCopy()}
	again.Spec.Cancel = true
	if err := r.Create(ctx, again); err != nil {
		t.Fatal(err)
	}
}

// mergeWithoutCommitID has a person push on top of the pull request cr of p's chain branch and merge it, with the Git
// host reporting no merge commit ID, so a replacement that holds the lock claims the merge and waits for the ID.
func mergeWithoutCommitID(t *testing.T, g *fakegit.Git, p *api.RemediationRequest, cr providers.ChangeRequest) {
	t.Helper()
	if _, err := g.Push("shadewright/"+string(chainRootOf(p)), "person.yaml", []byte("by: a person\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	if err := g.SetMergeCommit(cr.Number, ""); err != nil {
		t.Fatal(err)
	}
}

// claimedDuringWatch is watchingAfterRefusal with the replacement's watch holding a claim on a merge of another head
// (mergeWithoutCommitID), then the predecessor made again under its name.
func claimedDuringWatch(t *testing.T) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest,
	*api.RemediationRequest, providers.ChangeRequest, func(time.Duration)) {
	t.Helper()
	r, g, p, rr, cr, move := watchingAfterRefusal(t)
	mergeWithoutCommitID(t, g, p, cr)
	if got, _ := reconcileOnce(t, r, rr); !otherHeadClaim(got) || claimedNumber(got) != cr.Number {
		t.Fatalf("phase %q, waits %+v: want the watch holding a claim on #%d", got.Status.Phase, got.Status.GitWaits,
			cr.Number)
	}
	remakePredecessor(t, r, p)
	return r, g, p, rr, cr, move
}

// Row 1: while the replacement claims the merge of #N, a person opens a second pull request from the chain's branch
// (with the request's marker) and merges it with a merge commit. The claim is not settled by recording that other
// pull request: nothing is recorded for it, the claim stays on #N, and at its own end #N is recorded.
func TestClaimNotSettledByAnotherMergedPullRequest(t *testing.T) {
	r, g, p, rr, cr, move := claimedDuringWatch(t)
	branch := "shadewright/" + string(chainRootOf(p))
	if _, err := g.Push(branch, "second.yaml", []byte("by: a person, again\n")); err != nil {
		t.Fatal(err)
	}
	first, err := g.GetChangeRequest(t.Context(), cr.Number)
	if err != nil {
		t.Fatal(err)
	}
	second, err := g.AddOpenChangeRequest(branch, first.BaseBranch, first.Body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.MergeWithCommit(second); err != nil {
		t.Fatal(err)
	}
	notSecond := func(got *api.RemediationRequest) {
		t.Helper()
		if got.Status.PullRequest != nil && got.Status.PullRequest.Number == second {
			c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionApproved)
			t.Fatalf("phase %q, Approved %+v: the claim on #%d was settled by recording #%d", got.Status.Phase, c,
				cr.Number, second)
		}
	}
	got, _ := reconcileOnce(t, r, rr)
	notSecond(got)
	if !otherHeadClaim(got) || claimedNumber(got) != cr.Number || !ptr.Deref(got.Status.HoldsTarget, false) {
		t.Fatalf("phase %q, waits %+v: want the claim on #%d kept with the lock", got.Status.Phase,
			got.Status.GitWaits, cr.Number)
	}
	claimEndsAtItsEnd(t, r, rr, cr, move)
	notSecond(storedRR(t, r, rr))
}

// Row 2: the claim starts after the end of the watch (the look at stopped holders reads the predecessor's pull
// request merged with another head and no merge commit ID), then the predecessor is made again under its name. The
// claim goes through the claim rule: the lock is kept until the claimed merge is recorded at the claim's end, never
// settled by the generic look.
func TestClaimAfterTheWatchWithThePredecessorMadeAgain(t *testing.T) {
	r, g, p, rr, cr, move := endedWatch(t)
	g.SetFail(nil)
	mergeWithoutCommitID(t, g, p, cr)
	move(heldRecheckInterval)
	got, _ := reconcileOnce(t, r, rr)
	if !otherHeadClaim(got) || claimedNumber(got) != cr.Number {
		t.Fatalf("phase %q, waits %+v: want the ended replacement holding a claim on #%d", got.Status.Phase,
			got.Status.GitWaits, cr.Number)
	}
	remakePredecessor(t, r, p)
	got, _ = reconcileOnce(t, r, rr)
	if !ptr.Deref(got.Status.HoldsTarget, false) || !otherHeadClaim(got) && !recordsMerge(got) {
		t.Fatalf("phase %q, lock %v, waits %+v: the claim was settled without a record of #%d", got.Status.Phase,
			ptr.Deref(got.Status.HoldsTarget, false), got.Status.GitWaits, cr.Number)
	}
	claimEndsAtItsEnd(t, r, rr, cr, move)
}

// Row 3: the reads of a claim answer and nothing matches it (a person took the request's marker out of the claimed
// pull request): the end of the claim says the read answered and nothing matched, not that the reads kept failing.
func TestClaimEndSaysTheReadsAnswered(t *testing.T) {
	r, g, _, rr, cr, move := claimedDuringWatch(t)
	if err := g.SetBody(cr.Number, "edited by a person"); err != nil {
		t.Fatal(err)
	}
	var got *api.RemediationRequest
	for range 2 {
		move(mergeContentWait)
		got, _ = reconcileOnce(t, r, rr)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionApproved)
	if got.Status.Phase != api.PhaseNeedsIntervention || c == nil || c.Reason != ReasonMergeContentUnreadable {
		t.Fatalf("phase %q, Approved %+v: want the claim ended with MergeContentUnreadable", got.Status.Phase, c)
	}
	if strings.Contains(c.Message, "kept failing") || !strings.Contains(c.Message, "answered and nothing it found "+
		"matched pull request") {
		t.Fatalf("message %q: want it to say the read answered and nothing matched the claim", c.Message)
	}
}

// endOnOtherMerge logs that it kept the lock when no verdict releases it (the status records a pull request).
func TestEndOnOtherMergeLogsWhenTheLockIsKept(t *testing.T) {
	r, p, rr, cr, _ := predecessorHeadMerged(t)
	pred := storedRR(t, r, p)
	now := metav1.NowMicro()
	pred.Status.PullRequest.MergedAt = &now
	cr.HeadSHA = pred.Status.PullRequest.HeadSHA
	repl := storedRR(t, r, rr)
	repl.Status.PullRequest = &api.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA}
	ctx, lines := loggedCtx()
	if err := r.endOnOtherMerge(ctx, repl, pred, cr); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(*lines, func(l string) bool { return strings.Contains(l, "target lock kept") }) {
		t.Fatalf("log %q: want the kept lock logged", *lines)
	}
}

// claimAfterTheWatch is endedWatch with the ended replacement holding a claim on a merge of another head of the
// predecessor's pull request cr (mergeWithoutCommitID), the predecessor still present and handing over.
func claimAfterTheWatch(t *testing.T) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest,
	*api.RemediationRequest, providers.ChangeRequest, func(time.Duration)) {
	t.Helper()
	r, g, p, rr, cr, move := endedWatch(t)
	g.SetFail(nil)
	mergeWithoutCommitID(t, g, p, cr)
	move(heldRecheckInterval)
	if got, _ := reconcileOnce(t, r, rr); !otherHeadClaim(got) || claimedNumber(got) != cr.Number {
		t.Fatalf("phase %q, waits %+v: want the ended replacement holding a claim on #%d", got.Status.Phase,
			got.Status.GitWaits, cr.Number)
	}
	return r, g, p, rr, cr, move
}

// claimEndsDisagreed runs rr past the end of its claim on cr while the Git host's answers disagree, and fails unless
// every reconcile ends without an error (no hand-off to the error backoff) and the claim ends NeedsIntervention with
// MergeContentUnreadable on cr, saying the answers disagreed.
func claimEndsDisagreed(t *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest,
	cr providers.ChangeRequest, move func(time.Duration)) {
	t.Helper()
	got, err := reconcileOnce(t, r, rr)
	if err != nil {
		t.Fatalf("reconcile while the answers disagree = %v; want no error (the lock kept, looked at again)", err)
	}
	if !otherHeadClaim(got) || !ptr.Deref(got.Status.HoldsTarget, false) {
		t.Fatalf("phase %q, waits %+v: want the claim on #%d and the lock kept", got.Status.Phase,
			got.Status.GitWaits, cr.Number)
	}
	for range 2 {
		move(mergeContentWait)
		if got, err = reconcileOnce(t, r, rr); err != nil {
			t.Fatalf("reconcile past the claim's end = %v; want no error", err)
		}
	}
	c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionApproved)
	if got.Status.Phase != api.PhaseNeedsIntervention || c == nil || c.Reason != ReasonMergeContentUnreadable ||
		got.Status.PullRequest == nil || got.Status.PullRequest.Number != cr.Number || otherHeadClaim(got) {
		t.Fatalf("phase %q, Approved %+v, waits %+v: want the claim on #%d ended with MergeContentUnreadable",
			got.Status.Phase, c, got.Status.GitWaits, cr.Number)
	}
	if strings.Contains(c.Message, "kept failing") || !strings.Contains(c.Message, "answers about pull request") ||
		!strings.Contains(c.Message, "disagreed") {
		t.Fatalf("message %q: want it to say the Git host's answers disagreed", c.Message)
	}
}

// Count 4, case 1 (decision of 2026-11-21 on #870): the claim's pull request is read through the predecessor by its
// number, and the Git host answers 404 for it while the branch still lists it. Before, heldRecovery handed that
// verdict to the error backoff, so the claim, the lock and the missing record outlived the claim's end.
func TestClaimEndsWhenTheAnswersDisagreeByNumber(t *testing.T) {
	r, g, _, rr, cr, move := claimAfterTheWatch(t)
	g.SetFail(func(op string) error {
		if op == "GetChangeRequest" {
			return providers.ErrNotFound
		}
		return nil
	})
	claimEndsDisagreed(t, r, rr, cr, move)
}

// Count 4, case 1, through the branch (heldRecoveryAlone): the predecessor is made again, so the claim reads the
// chain's branch, and the Git host answers its listing with a pull request not asked for.
func TestClaimEndsWhenTheBranchAnswerIsUntrusted(t *testing.T) {
	r, g, p, rr, cr, move := claimAfterTheWatch(t)
	remakePredecessor(t, r, p)
	g.SetFail(func(op string) error {
		if op == "ListChangeRequests" {
			return providers.ErrUntrustedAnswer
		}
		return nil
	})
	claimEndsDisagreed(t, r, rr, cr, move)
}

// Count 4, case 2 (decision of 2026-11-21 on #870): at the claim's end the predecessor, same UID, records another
// pull request (#101). The claim decides by its own number, as the watch does: the predecessor counts as gone and the
// claimed merge is recorded with the takeover, not sent to the generic end (handoff Aborted).
func TestClaimEndWithThePredecessorRecordingAnotherNumber(t *testing.T) {
	r, _, p, rr, cr, move := claimAfterTheWatch(t)
	pred := storedRR(t, r, p)
	pred.Status.PullRequest.Number = 101
	if err := r.Status().Update(t.Context(), pred); err != nil {
		t.Fatal(err)
	}
	var got *api.RemediationRequest
	for range 2 {
		move(mergeContentWait)
		got, _ = reconcileOnce(t, r, rr)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionApproved)
	if got.Status.Phase != api.PhaseNeedsIntervention || c == nil || c.Reason != ReasonMergeContentUnreadable ||
		got.Status.PullRequest == nil || got.Status.PullRequest.Number != cr.Number {
		t.Fatalf("phase %q, Approved %+v: want the claim on #%d ended with MergeContentUnreadable", got.Status.Phase,
			c, cr.Number)
	}
	if got.Status.Handoff.From.Phase == api.HandoffAborted {
		t.Fatalf("handoff %q, message %q: the claim's end took the generic end, not the claim's",
			got.Status.Handoff.From.Phase, c.Message)
	}
}
