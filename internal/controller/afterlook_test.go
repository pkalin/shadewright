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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
)

// reviewCheckPinned is the CEL rule of RemediationRequest that keeps status.pullRequest.reviewCheck once a merge
// receipt is stored ("status.pullRequest.reviewCheck does not change once status.mergeReceipt exists"), for the fake
// client, which runs no CEL: it returns the API server's 422 for a status write that breaks it, after the
// resourceVersion check as the API server does.
func reviewCheckPinned(ctx context.Context, c client.Client, obj client.Object) error {
	stored := &api.RemediationRequest{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), stored); err != nil {
		return err
	}
	rr := obj.(*api.RemediationRequest)
	if rr.ResourceVersion != stored.ResourceVersion {
		// The API server checks the resourceVersion before it validates: a stale write is a conflict.
		return nil
	}
	if stored.Status.MergeReceipt == nil || stored.Status.PullRequest == nil {
		return nil
	}
	var now *api.ReviewCheck
	if rr.Status.PullRequest != nil {
		now = rr.Status.PullRequest.ReviewCheck
	}
	if equality.Semantic.DeepEqual(now, stored.Status.PullRequest.ReviewCheck) {
		return nil
	}
	return apierrors.NewInvalid(schema.GroupKind{Group: api.GroupVersion.Group, Kind: "RemediationRequest"},
		rr.Name, field.ErrorList{field.Invalid(field.NewPath("status"), "",
			"status.pullRequest.reviewCheck does not change once status.mergeReceipt exists")})
}

// recordMergeOf stores, as another writer would, the merge of rr's recorded pull request on its head: phase Merged,
// the merge commit and a receipt, with the review check left as it is.
func recordMergeOf(ctx context.Context, t *testing.T, c client.Client, rr *api.RemediationRequest) {
	t.Helper()
	live := &api.RemediationRequest{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
		t.Fatal(err)
	}
	pr := live.Status.PullRequest
	at := metav1.NewMicroTime(time.Now().Truncate(time.Second))
	pr.MergeCommitSHA = "c0ffee" + pr.HeadSHA[6:]
	pr.MergedAt = &at
	live.Status.Phase = api.PhaseMerged
	live.Status.MergeReceipt = &api.MergeReceipt{Repository: *live.Status.PlanRepository,
		TargetBranch: live.Status.Source.TargetBranch, PullRequest: pr.Number, HeadSHA: pr.HeadSHA,
		MergeCommitSHA: pr.MergeCommitSHA, MergedAt: at}
	if err := c.Status().Update(ctx, live); err != nil {
		t.Fatal(err)
	}
}

// #1008: a merge of the same pull request and head that another writer records between the look's read and the
// status write at its exit (afterLook) makes that write conflict. The retry carries the look's outcome over to the
// request as read again, but a merge receipt pins status.pullRequest.reviewCheck (CEL), so the outcome's state Stale
// must not be copied onto it: the look ends without an error and keeps its pace, and the receipt and its review check
// stay as the merge recorded them.
func TestStoppedHolderCheckOutcomeYieldsToARecordedMerge(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	r.ReviewerHints = true
	setTargetMain(t, r)
	r.Recorder = record.NewFakeRecorder(100)
	head := rr.Status.PullRequest.HeadSHA
	base := r.Client
	merged := false
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if out, ok := obj.(*api.RemediationRequest); ok && !merged && out.Status.PullRequest != nil &&
				out.Status.PullRequest.ReviewCheck != nil &&
				out.Status.PullRequest.ReviewCheck.State == api.ReviewCheckStale {
				// The merge is recorded by another writer before the look's write lands.
				merged = true
				recordMergeOf(ctx, t, base, out)
			}
			if err := reviewCheckPinned(ctx, base, obj); err != nil {
				return err
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	stale := &api.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), stale); err != nil {
		t.Fatal(err)
	}
	res, err := r.checkHeld(ctx, stale)
	if err != nil {
		t.Fatalf("checkHeld: %v; want the look to end with the merge recorded, no error", err)
	}
	if !merged {
		t.Fatal("the look wrote no state Stale: the case was not reached")
	}
	if res.RequeueAfter < heldRecheckInterval {
		t.Fatalf("result %+v: want the look's pace of %v at least", res, heldRecheckInterval)
	}
	live := &api.RemediationRequest{}
	if err := base.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
		t.Fatal(err)
	}
	if live.Status.MergeReceipt == nil || live.Status.PullRequest.ReviewCheck != nil {
		t.Fatalf("receipt %+v, review check %+v: want the merge as recorded and its review check unchanged",
			live.Status.MergeReceipt, live.Status.PullRequest.ReviewCheck)
	}
	if !failedOnly(g.CheckRuns(head)) {
		t.Fatalf("check runs %+v: want the one failed run of the look", g.CheckRuns(head))
	}
}

// #1008, ADR-0046 limit (g): a failed status write at the exit of a look on a head whose state Stale is stored
// already (only removing condition ReviewCheckNotFailed is lost) is not a blind retry: the next look reads the latest
// check of the head and, finding the failed run last, writes no second one.
func TestStoppedHolderRetryOnStoredStaleReadsTheCheck(t *testing.T) {
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	r.ReviewerHints = true
	setTargetMain(t, r)
	r.Recorder = record.NewFakeRecorder(100)
	pr := rr.Status.PullRequest
	head := pr.HeadSHA
	// Stale stored and the condition set: a later write of the failed check failed after Stale was recorded.
	live := &api.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
		t.Fatal(err)
	}
	live.Status.PullRequest.ReviewCheck = &api.ReviewCheck{Head: head, State: api.ReviewCheckStale}
	meta.SetStatusCondition(&live.Status.Conditions, metav1.Condition{Type: api.ConditionReviewCheckNotFailed,
		Status: metav1.ConditionTrue, Reason: ReasonGitFailed, Message: "the Git host failed the write"})
	if err := r.Status().Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	base := r.Client
	failWrite := true
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if failWrite {
				return errors.New("etcd is down")
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	look := func() error {
		cur := &api.RemediationRequest{}
		if err := base.Get(ctx, client.ObjectKeyFromObject(rr), cur); err != nil {
			t.Fatal(err)
		}
		_, err := r.checkHeld(ctx, cur)
		return err
	}
	if err := look(); err == nil {
		t.Fatal("checkHeld with the status write failing: want the error for the backoff")
	}
	if runs := g.CheckRuns(head); !failedOnly(runs) || len(runs) != 1 {
		t.Fatalf("check runs %+v after the first look: want the one failed run", runs)
	}
	failWrite = false
	if err := look(); err != nil {
		t.Fatalf("checkHeld on the retry: %v", err)
	}
	if runs := g.CheckRuns(head); len(runs) != 1 {
		t.Fatalf("check runs %+v after the retry: want no second run, the retry reads the check first", runs)
	}
	if err := base.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(live.Status.Conditions, api.ConditionReviewCheckNotFailed); c != nil {
		t.Fatalf("ReviewCheckNotFailed %+v after the retry: want it removed", c)
	}
}
