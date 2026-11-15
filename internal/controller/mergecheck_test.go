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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
)

// failingPolicyReader fails every read of a RemediationPolicy with a server error while fail returns true, and
// reads anything else from Reader.
type failingPolicyReader struct {
	client.Reader
	fail func() bool
}

func (f failingPolicyReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object,
	opts ...client.GetOption) error {
	if _, ok := obj.(*api.RemediationPolicy); ok && f.fail() {
		return apierrors.NewServiceUnavailable("the API server is not available")
	}
	return f.Reader.Get(ctx, key, obj, opts...)
}

// blockingPolicyReader blocks a read of a RemediationPolicy until its context ends when block returns true, and
// returns the context's error; it reads anything else, and a policy when block returns false, from Reader.
type blockingPolicyReader struct {
	client.Reader
	block func() bool
}

func (b blockingPolicyReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object,
	opts ...client.GetOption) error {
	if _, ok := obj.(*api.RemediationPolicy); ok && b.block() {
		<-ctx.Done()
		return ctx.Err()
	}
	return b.Reader.Get(ctx, key, obj, opts...)
}

// policyResource is the resource name of RemediationPolicy.
const policyResource = "remediationpolicies"

// forbiddenPolicyReader refuses every read of a RemediationPolicy as forbidden, and reads anything else from Reader.
type forbiddenPolicyReader struct {
	client.Reader
}

func (f forbiddenPolicyReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object,
	opts ...client.GetOption) error {
	if _, ok := obj.(*api.RemediationPolicy); ok {
		return apierrors.NewForbidden(schema.GroupResource{Group: api.GroupVersion.Group, Resource: policyResource},
			key.Name, errors.New("the executor may not read it"))
	}
	return f.Reader.Get(ctx, key, obj, opts...)
}

// A pull request merged right after the handoff is checked at the merge as any other (#737): the world of
// handoffAppendedWorld holds no policy, so the plan is stale, and the write that sets Merged carries the mark.
func TestRecordHandoffMergedChecksThePlanAtTheMerge(t *testing.T) {
	r, g, p, rr, cr, head := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	merged, err := g.FindChangeRequest(ctx, cr.HeadBranch)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.recordHandoff(ctx, rr, p, merged, head); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhaseMerged || rr.Status.MergeReceipt == nil {
		t.Fatalf("the merge was not recorded: phase %s, receipt %v", rr.Status.Phase, rr.Status.MergeReceipt)
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPlanFresh)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != freshness.ReasonPolicyChanged {
		t.Fatalf("PlanFresh = %+v, want False with %s", c, freshness.ReasonPolicyChanged)
	}
	if !stalePlanMerged(rr) || breakerOutcome(rr) != outcomeNone {
		t.Fatalf("a stale plan merged after a handoff reads as stale=%v, breaker %v", stalePlanMerged(rr), breakerOutcome(rr))
	}
}

// A failed read of the check at the merge right after a handoff never records the merge: the takeover is recorded
// like an open pull request, and the look at the recorded pull request checks the plan again.
func TestRecordHandoffMergedRetriesAFailedCheck(t *testing.T) {
	r, g, p, rr, cr, head := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	r.APIReader = failingPolicyReader{Reader: r.APIReader, fail: func() bool { return true }}
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	merged, err := g.FindChangeRequest(ctx, cr.HeadBranch)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.recordHandoff(ctx, rr, p, merged, head); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhasePRCreated || rr.Status.MergeReceipt != nil {
		t.Fatalf("a merge was recorded without its check: phase %s, receipt %v", rr.Status.Phase, rr.Status.MergeReceipt)
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPlanFresh); c != nil {
		t.Fatalf("PlanFresh = %+v after a failed read, want none", c)
	}
}

// A check at the merge that runs out of time right after a handoff still leaves time for the writes: the start
// of the wait is recorded, so the wait bounds a check that always runs out of time, and the takeover is recorded.
func TestRecordHandoffMergedBoundsACheckThatRunsOutOfTime(t *testing.T) {
	r, g, p, rr, cr, head := handoffAppendedWorld(t, nil)
	r.APIReader = blockingPolicyReader{Reader: r.APIReader, block: func() bool { return true }}
	if _, err := g.MergeWithCommit(cr.Number); err != nil {
		t.Fatal(err)
	}
	merged, err := g.FindChangeRequest(context.Background(), cr.HeadBranch)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.recordHandoff(ctx, rr, p, merged, head); err != nil {
		t.Fatal(err)
	}
	if rr.Status.MergeCommitAwaitedSince == nil {
		t.Fatal("the start of the wait was not recorded: a check that always runs out of time is not bounded")
	}
	if rr.Status.Phase != api.PhasePRCreated || rr.Status.MergeReceipt != nil {
		t.Fatalf("phase %s, receipt %v, want the takeover recorded without the merge", rr.Status.Phase,
			rr.Status.MergeReceipt)
	}
}
