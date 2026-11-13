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
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
)

// statusRecorder records every status write of a RemediationRequest that goes through it, and fails those that
// fail (when set) returns true for, as a status write lost after the Git host answered.
type statusRecorder struct {
	client.Client
	fail func(*remediationv1alpha1.RemediationRequest) bool

	mu     sync.Mutex
	writes []remediationv1alpha1.RemediationRequestStatus
}

func (s *statusRecorder) Status() client.SubResourceWriter {
	return &recordingStatus{SubResourceWriter: s.Client.Status(), s: s}
}

// written returns the statuses written so far, oldest first.
func (s *statusRecorder) written() []remediationv1alpha1.RemediationRequestStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]remediationv1alpha1.RemediationRequestStatus(nil), s.writes...)
}

type recordingStatus struct {
	client.SubResourceWriter
	s *statusRecorder
}

func (w *recordingStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if rr, ok := obj.(*remediationv1alpha1.RemediationRequest); ok {
		if w.s.fail != nil && w.s.fail(rr) {
			return errors.New("status write lost")
		}
		if err := w.SubResourceWriter.Update(ctx, obj, opts...); err != nil {
			return err
		}
		w.s.mu.Lock()
		w.s.writes = append(w.s.writes, *rr.Status.DeepCopy())
		w.s.mu.Unlock()
		return nil
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

// receiptRequest is a request whose recorded pull request #7 a person merged: the state recordMerge is called on.
func receiptRequest() (*remediationv1alpha1.RemediationRequest, providers.ChangeRequest) {
	head, merge := strings.Repeat("a", 40), strings.Repeat("b", 40)
	rr := &remediationv1alpha1.RemediationRequest{}
	rr.Status.PlanRepository = &remediationv1alpha1.PlanRepository{URL: "https://github.com/example/fleet", InstallationID: 3}
	rr.Status.Source = &remediationv1alpha1.ResolvedSource{TargetBranch: mainBranch}
	rr.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: 7, HeadSHA: head}
	return rr, providers.ChangeRequest{Number: 7, HeadSHA: head, BaseBranch: mainBranch,
		State: providers.ChangeRequestMerged, MergeCommit: merge}
}

// TestRecordMergeWritesTheReceipt checks the receipt recordMerge builds (ADR-0030 section 1): every field from the
// recorded state and the merged pull request, and mergedAt from the write that sets Merged.
func TestRecordMergeWritesTheReceipt(t *testing.T) {
	rr, cr := receiptRequest()
	recordMerge(t.Context(), rr, cr)
	r := &RemediationRequestReconciler{Now: func() time.Time { return limitsNow }}
	r.setPhase(rr, remediationv1alpha1.PhaseMerged)
	want := remediationv1alpha1.MergeReceipt{
		Repository: *rr.Status.PlanRepository, TargetBranch: mainBranch, PullRequest: 7, HeadSHA: cr.HeadSHA,
		MergeCommitSHA: cr.MergeCommit, MergedAt: metav1.NewMicroTime(limitsNow),
	}
	if got := rr.Status.MergeReceipt; got == nil || *got != want {
		t.Fatalf("receipt = %+v, want %+v", got, want)
	}
	if !rr.Status.MergeReceipt.MergedAt.Equal(rr.Status.PullRequest.MergedAt) {
		t.Errorf("mergedAt %v, pullRequest.mergedAt %v", rr.Status.MergeReceipt.MergedAt, rr.Status.PullRequest.MergedAt)
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionVerified); c != nil {
		t.Errorf("Verified set with a receipt: %+v", c)
	}

	// Called again (a repeated look), it keeps the receipt it has.
	first := *rr.Status.MergeReceipt
	other := cr
	other.MergeCommit = strings.Repeat("c", 40)
	recordMerge(t.Context(), rr, other)
	if *rr.Status.MergeReceipt != first {
		t.Errorf("receipt replaced: %+v", rr.Status.MergeReceipt)
	}
}

// TestRecordMergeWithoutAReceipt checks that a merge that cannot be bound to a receipt gets none, and Verified
// Unknown with reason NoMergeReceipt in the same write, never a receipt with a missing or foreign value.
func TestRecordMergeWithoutAReceipt(t *testing.T) {
	for name, mutate := range map[string]func(*remediationv1alpha1.RemediationRequest, *providers.ChangeRequest){
		"no merge commit": func(_ *remediationv1alpha1.RemediationRequest, cr *providers.ChangeRequest) { cr.MergeCommit = "" },
		"abbreviated merge commit": func(_ *remediationv1alpha1.RemediationRequest, cr *providers.ChangeRequest) {
			cr.MergeCommit = "bbbbbbb"
		},
		"upper case merge commit": func(_ *remediationv1alpha1.RemediationRequest, cr *providers.ChangeRequest) {
			cr.MergeCommit = strings.ToUpper(cr.MergeCommit)
		},
		"another head": func(_ *remediationv1alpha1.RemediationRequest, cr *providers.ChangeRequest) {
			cr.HeadSHA = strings.Repeat("d", 40)
		},
		"another number": func(_ *remediationv1alpha1.RemediationRequest, cr *providers.ChangeRequest) { cr.Number = 8 },
		"another target branch": func(_ *remediationv1alpha1.RemediationRequest, cr *providers.ChangeRequest) {
			cr.BaseBranch = "release"
		},
		"no plan repository": func(rr *remediationv1alpha1.RemediationRequest, _ *providers.ChangeRequest) {
			rr.Status.PlanRepository = nil
		},
		"no source": func(rr *remediationv1alpha1.RemediationRequest, _ *providers.ChangeRequest) { rr.Status.Source = nil },
		"no pull request recorded": func(rr *remediationv1alpha1.RemediationRequest, _ *providers.ChangeRequest) {
			rr.Status.PullRequest = nil
		},
		"not merged": func(_ *remediationv1alpha1.RemediationRequest, cr *providers.ChangeRequest) {
			cr.State = providers.ChangeRequestOpen
		},
	} {
		t.Run(name, func(t *testing.T) {
			rr, cr := receiptRequest()
			mutate(rr, &cr)
			recordMerge(t.Context(), rr, cr)
			if rr.Status.MergeReceipt != nil {
				t.Fatalf("receipt written: %+v", rr.Status.MergeReceipt)
			}
			// status.pullRequest.mergeCommitSHA is set once: a commit without a receipt is never written.
			if pr := rr.Status.PullRequest; pr != nil && pr.MergeCommitSHA != "" {
				t.Errorf("mergeCommitSHA written without a receipt: %q", pr.MergeCommitSHA)
			}
			c := meta.FindStatusCondition(rr.Status.Conditions, remediationv1alpha1.ConditionVerified)
			if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != ReasonNoMergeReceipt {
				t.Fatalf("Verified = %+v, want Unknown %s", c, ReasonNoMergeReceipt)
			}
		})
	}
}

// TestSetPhaseMergedCarriesReceiptOrMark checks that the write that sets Merged carries the receipt or the
// NoMergeReceipt mark, never neither, also on a path that did not call recordMerge.
func TestSetPhaseMergedCarriesReceiptOrMark(t *testing.T) {
	r := &RemediationRequestReconciler{Now: func() time.Time { return limitsNow }}
	rr, _ := receiptRequest()
	rr.Status.PlanDigest = limitsDigest
	r.setPhase(rr, remediationv1alpha1.PhaseMerged)
	if !noReceiptMerge(rr) {
		t.Fatalf("Merged without a receipt and without the mark: %+v", rr.Status.Conditions)
	}
	if rr.Status.BreakerCounts == nil || *rr.Status.BreakerCounts {
		t.Errorf("breakerCounts = %v, want false: read like Inconclusive", rr.Status.BreakerCounts)
	}

	rr, cr := receiptRequest()
	recordMerge(t.Context(), rr, cr)
	r.setPhase(rr, remediationv1alpha1.PhaseMerged)
	if rr.Status.MergeReceipt == nil || meta.FindStatusCondition(rr.Status.Conditions,
		remediationv1alpha1.ConditionVerified) != nil {
		t.Fatalf("receipt %+v, conditions %+v; want the receipt only", rr.Status.MergeReceipt, rr.Status.Conditions)
	}
}

// TestInconclusiveForTheBreaker checks how the circuit breaker reads Inconclusive (ADR-0030 section 5): it ends a
// request and a probe, it is neither a success nor a failure, and only a probe that ended in it stays read.
func TestInconclusiveForTheBreaker(t *testing.T) {
	if !ended(remediationv1alpha1.PhaseInconclusive) {
		t.Fatal("Inconclusive does not end a probe")
	}
	rr := otherRequest("inconclusive", limitsTarget, remediationv1alpha1.PhaseInconclusive, time.Minute)
	if got := breakerOutcome(rr); got != outcomeNone {
		t.Errorf("breakerOutcome = %v, want none", got)
	}
	classifyBreaker(rr)
	if rr.Status.BreakerCounts == nil || *rr.Status.BreakerCounts {
		t.Errorf("breakerCounts = %v, want false outside a probe", rr.Status.BreakerCounts)
	}
	rr.Status.BreakerProbe = true
	classifyBreaker(rr)
	if rr.Status.BreakerCounts == nil || !*rr.Status.BreakerCounts {
		t.Errorf("breakerCounts = %v, want true for a probe", rr.Status.BreakerCounts)
	}
}
