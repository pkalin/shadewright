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

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
)

// noteWorld is a stopped holder (NeedsIntervention) with reviewer hints on whose failed review check the Git host does
// not take (502), after one look: condition ReviewCheckNotFailed is set with reason GitFailed. writes, when it returns
// an error for an operation, fails it as well. act closes the pull request, or merges it.
func noteWorld(t *testing.T, verify bool, writes func(op string) error) (*RemediationRequestReconciler,
	func() *api.RemediationRequest, func(merge bool) error) {
	t.Helper()
	r, g, rr := settlementWorld(t, api.PhaseNeedsIntervention)
	ctx := context.Background()
	r.ReviewerHints = true
	r.VerifyOutcomes = verify
	r.Recorder = record.NewFakeRecorder(100)
	setTargetMain(t, r)
	g.SetFail(func(op string) error {
		if op == opCreateCheckRun {
			return &providers.RetryableError{Err: errors.New("status 502: bad gateway")}
		}
		if writes != nil {
			return writes(op)
		}
		return nil
	})
	get := func() *api.RemediationRequest {
		t.Helper()
		live := &api.RemediationRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(rr), live); err != nil {
			t.Fatal(err)
		}
		return live
	}
	if _, err := r.checkHeld(ctx, get()); err != nil {
		t.Fatalf("checkHeld: %v", err)
	}
	if c := meta.FindStatusCondition(get().Status.Conditions, api.ConditionReviewCheckNotFailed); c == nil ||
		c.Reason != ReasonGitFailed {
		t.Fatalf("ReviewCheckNotFailed %+v after the first look: want it with reason %s", c, ReasonGitFailed)
	}
	act := func(merge bool) error {
		if !merge {
			return g.Close(rr.Status.PullRequest.Number)
		}
		_, err := g.MergeWithCommit(rr.Status.PullRequest.Number)
		return err
	}
	return r, get, act
}

// #925, second fix round: condition ReviewCheckNotFailed says the failed check is tried again at the next look, so it
// is True only while the request's own pull request is open and its failed check is not written. Every path that ends
// the looks leaves it absent: a look that finds the pull request closed (the lock is released in the same write), or
// merged (recorded Merged, with the lock released, or kept for the verification, which runs no look). On the first
// build of #925 the condition stayed True after each of them.
func TestCheckNoteEndsWithTheLooks(t *testing.T) {
	for _, c := range []struct {
		name          string
		merge, verify bool
		phase         api.RemediationPhase
		holds         bool
	}{
		{"pull request closed", false, false, api.PhaseNeedsIntervention, false},
		{"pull request merged", true, false, api.PhaseMerged, false},
		{"pull request merged, verification on", true, true, api.PhaseMerged, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, get, act := noteWorld(t, c.verify, nil)
			if err := act(c.merge); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := r.checkHeld(context.Background(), get()); err != nil {
					t.Fatalf("checkHeld: %v", err)
				}
			}
			live := get()
			if live.Status.Phase != c.phase || holdsLock(live) != c.holds {
				t.Fatalf("phase %s, holds the lock %t: want %s and %t", live.Status.Phase, holdsLock(live), c.phase,
					c.holds)
			}
			if cond := meta.FindStatusCondition(live.Status.Conditions, api.ConditionReviewCheckNotFailed); cond != nil {
				t.Fatalf("ReviewCheckNotFailed %+v after the looks ended: want it absent", cond)
			}
		})
	}
}

// #925, second fix round: the deletion of a stopped holder ends its looks, and the finalizer's close takes over. Its
// first status write removes condition ReviewCheckNotFailed, which would otherwise say a look tries the check again
// while the finalizer retries a close that fails. On the first build of #925 the condition stayed.
func TestCheckNoteEndsWithTheDeletion(t *testing.T) {
	r, get, _ := noteWorld(t, false, func(op string) error {
		if op == opCloseAnyBase || op == opClose {
			return &providers.RetryableError{Err: errors.New("status 502: bad gateway")}
		}
		return nil
	})
	ctx := context.Background()
	live := get()
	controllerutil.AddFinalizer(live, FinalizerName)
	if err := r.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, live); err != nil {
		t.Fatal(err)
	}
	// The fake client cannot serve the list of the cap that the close of an ended request waits for, so finalize
	// returns that error; the write that removes the condition comes before it.
	_, _ = r.finalize(ctx, get())
	live = get()
	if cond := meta.FindStatusCondition(live.Status.Conditions, api.ConditionReviewCheckNotFailed); cond != nil {
		t.Fatalf("ReviewCheckNotFailed %+v while the deletion closes the pull request: want it absent", cond)
	}
}
