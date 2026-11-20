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
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// The end of the watch after a refused recovery is not a verdict and does not end the observation (third decision of
// 2026-11-18 on #786, point 2): the replacement stops NeedsIntervention with RecoveryWaitExpired, keeps the target lock
// and its handoff Reserved, and the look at stopped holders records a merge of its commit with its receipt whenever it
// comes; the lock goes on a verdict only.

// endedWatch is a replacement whose recovery met a refusal and whose watch then reached its end, with the clock the
// test moves.
func endedWatch(t *testing.T) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest,
	*api.RemediationRequest, providers.ChangeRequest, func(time.Duration)) {
	t.Helper()
	r, g, p, rr, cr, _ := handoffAppendedWorld(t, nil)
	move := movableClock(r)
	refuseAll(g)
	if _, err := reconcileOnce(t, r, rr); err != nil {
		t.Fatal(err)
	}
	move(recoveryWaitBound + time.Minute)
	got, err := reconcileOnce(t, r, rr)
	if err != nil {
		t.Fatal(err)
	}
	endedAtTheBound(t, got)
	if got.Status.Handoff.From.Phase != api.HandoffReserved {
		t.Fatalf("the end moved status.handoff.from to %q; it stays Reserved", got.Status.Handoff.From.Phase)
	}
	pred := &api.RemediationRequest{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(p), pred); err != nil {
		t.Fatal(err)
	}
	if pred.Status.Handoff.To.Phase != api.HandoffAborted {
		t.Fatalf("the predecessor's intent is %q at the end of the watch, want Aborted", pred.Status.Handoff.To.Phase)
	}
	return r, g, pred, got, cr, move
}

// mergeRecordedOnReplacement fails unless the merge of the replacement's commit is recorded on rr with its receipt,
// as the takeover of a merged pull request records it.
func mergeRecordedOnReplacement(t *testing.T, rr *api.RemediationRequest) {
	t.Helper()
	merged := []api.RemediationPhase{api.PhaseMerged, api.PhaseInconclusive, api.PhaseWaitingForDelivery,
		api.PhaseVerifying, api.PhaseSucceeded}
	if !slices.Contains(merged, rr.Status.Phase) || rr.Status.MergeReceipt == nil || !tookOver(rr) ||
		rr.Status.PullRequest == nil {
		t.Fatalf("phase %q, receipt %v, handoff %q: want the merge of the replacement's commit recorded with its "+
			"receipt", rr.Status.Phase, rr.Status.MergeReceipt, rr.Status.Handoff.From.Phase)
	}
}

// A merge of the replacement's commit one hour, or three days, after the end of the watch: the look at the stopped
// holder records it on the replacement with its receipt, and releases the lock with that record. The predecessor,
// whose intent is Aborted, looks at the same merged pull request first and records nothing of it as its own.
func TestRecoveryMergeAfterTheEndIsRecorded(t *testing.T) {
	for _, after := range []time.Duration{time.Hour, 72 * time.Hour} {
		t.Run(after.String(), func(t *testing.T) {
			r, g, p, rr, cr, move := endedWatch(t)
			ctx := context.Background()
			g.SetFail(nil)
			// Looks while the pull request is open keep the lock and record nothing.
			move(after / 2)
			got, err := reconcileOnce(t, r, rr)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status.Phase != api.PhaseNeedsIntervention || !ptr.Deref(got.Status.HoldsTarget, false) {
				t.Fatalf("phase %q, holdsTarget %v: an open pull request keeps the ended replacement and its lock",
					got.Status.Phase, got.Status.HoldsTarget)
			}
			if _, err := g.MergeWithCommit(cr.Number); err != nil {
				t.Fatal(err)
			}
			move(after / 2)
			for range 3 {
				_, _ = reconcileOnce(t, r, p)
			}
			for range 3 {
				_, _ = reconcileOnce(t, r, rr)
			}
			got, _ = reconcileOnce(t, r, rr)
			mergeRecordedOnReplacement(t, got)
			if ptr.Deref(got.Status.HoldsTarget, false) {
				t.Fatal("the recorded merge did not settle the lock")
			}
			if got.Status.MergeReceipt.PullRequest != cr.Number {
				t.Fatalf("receipt %+v does not name pull request #%d", got.Status.MergeReceipt, cr.Number)
			}
			pred := &api.RemediationRequest{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(p), pred); err != nil {
				t.Fatal(err)
			}
			if pred.Status.MergeReceipt != nil || pred.Status.Phase == api.PhaseMerged ||
				pred.Status.Phase == api.PhaseInconclusive || pred.Status.Phase == api.PhaseSucceeded {
				t.Fatalf("the predecessor recorded the replacement's merge as its own: phase %q, receipt %v",
					pred.Status.Phase, pred.Status.MergeReceipt)
			}
		})
	}
}

// A close after the end of the watch is a verdict: the lock is released, nothing is recorded as merged, and the
// replacement stays NeedsIntervention with RecoveryWaitExpired and its handoff Reserved.
func TestRecoveryCloseAfterTheEndReleasesTheLock(t *testing.T) {
	r, g, _, rr, cr, move := endedWatch(t)
	g.SetFail(nil)
	if err := g.CloseChangeRequest(context.Background(), cr.Number); err != nil {
		t.Fatal(err)
	}
	move(time.Hour)
	for range 2 {
		_, _ = reconcileOnce(t, r, rr)
	}
	got, _ := reconcileOnce(t, r, rr)
	opened := meta.FindStatusCondition(got.Status.Conditions, api.ConditionPROpened)
	if ptr.Deref(got.Status.HoldsTarget, false) || got.Status.MergeReceipt != nil ||
		got.Status.Phase != api.PhaseNeedsIntervention || opened == nil || opened.Reason != ReasonRecoveryWaitExpired ||
		got.Status.Handoff.From.Phase != api.HandoffAborted {
		t.Fatalf("phase %q, PullRequestOpened %+v, handoff %q, holdsTarget %v, receipt %v: want the lock released "+
			"on the close, the handoff Aborted in that write and nothing else changed", got.Status.Phase, opened, got.Status.Handoff.From.Phase,
			got.Status.HoldsTarget, got.Status.MergeReceipt)
	}
}
