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
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// The bounds of a recovery that met a refusal are derived from recorded state (decision of 2026-11-18 on #786): a
// pause does not remove a standing entry, and a refusal condition that stands without its entry gets the entry from
// the condition's lastTransitionTime before any read that can fail.

// countCalls makes every call of the provider fail with fail (nil lets it through) and counts the calls.
func countCalls(g *fakegit.Git, fail func() error) *atomic.Int32 {
	n := &atomic.Int32{}
	g.SetFail(func(string) error {
		n.Add(1)
		if fail == nil {
			return nil
		}
		return fail()
	})
	return n
}

func unavailable() error { return &providers.RetryableError{Err: errors.New("status 503")} }

func forbidden() error { return errors.Join(errors.New("status 403"), providers.ErrForbidden) }

// reconcileOnce runs one reconcile of rr and returns it as stored.
func reconcileOnce(t *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest) (*api.RemediationRequest,
	error) {
	t.Helper()
	ctx := context.Background()
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
	got := &api.RemediationRequest{}
	if gerr := r.Get(ctx, client.ObjectKeyFromObject(rr), got); gerr != nil {
		t.Fatal(gerr)
	}
	return got, err
}

// endedAtTheBound fails unless got ended NeedsIntervention with RecoveryWaitExpired, keeping the lock.
func endedAtTheBound(t *testing.T, got *api.RemediationRequest) {
	t.Helper()
	c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionPROpened)
	if got.Status.Phase != api.PhaseNeedsIntervention || c == nil || c.Reason != ReasonRecoveryWaitExpired {
		t.Fatalf("phase %q, PullRequestOpened %+v: want the end at the bound of the wait", got.Status.Phase, c)
	}
	if !ptr.Deref(got.Status.HoldsTarget, false) {
		t.Fatal("the end at the bound released the target lock")
	}
}

// A pause of Git writes keeps the HandoffRecovery entry: the reconciles under the pause remove nothing, and once the
// pause is lifted after the bound the request ends from its entry, with no read of the Git host.
func TestRecoveryEntrySurvivesAPause(t *testing.T) {
	r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
	move := movableClock(r)
	refuseAll(g)
	got, err := reconcileOnce(t, r, rr)
	if err != nil {
		t.Fatal(err)
	}
	entry := gitWait(got, waitHandoffRecovery)
	if entry == nil {
		t.Fatalf("the refusal recorded no HandoffRecovery entry: %+v", got.Status.GitWaits)
	}
	entry = entry.DeepCopy()
	lift := changeSettings(t, r, func(s *api.RemediationSettings) { s.Spec.Paused = new(true) })
	for range 3 {
		move(time.Hour)
		got, _ = reconcileOnce(t, r, rr)
		if w := gitWait(got, waitHandoffRecovery); w == nil || !w.Since.Equal(&entry.Since) || !w.Ends.Equal(entry.Ends) {
			t.Fatalf("under the pause the entry became %+v, want %+v", w, entry)
		}
	}
	move(recoveryWaitBound)
	lift()
	calls := countCalls(g, forbidden)
	got, err = reconcileOnce(t, r, rr)
	if err != nil {
		t.Fatal(err)
	}
	endedAtTheBound(t, got)
	if n := calls.Load(); n != 0 {
		t.Fatalf("the end after the pause called the Git host %d times; it ends from the recorded entry", n)
	}
}

// refusalWithoutEntry stores condition PullRequestUnreadable of a refusal on rr, at, with no entry: what a lost
// write of the entry left behind on the build that wrote the two apart.
func refusalWithoutEntry(t *testing.T, r *RemediationRequestReconciler, rr *api.RemediationRequest, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionPullRequestUnreadable,
		Status: metav1.ConditionTrue, Reason: ReasonGitForbidden, Message: "The Git host refused (GitForbidden) to " +
			"read the head of the branch; it is read again, and nothing is decided on it until a read works",
		LastTransitionTime: metav1.NewTime(at)})
	rr.Status.GitWaits = nil
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
}

// The refusal condition stands and the write of the waits was lost; every later read fails with a 503. The entry is
// recorded from the condition's lastTransitionTime before any read, and 25 hours later the request ends at its bound
// instead of staying PolicyChecked with no wait.
func TestRecoveryEntryRecordedFromAStandingRefusal(t *testing.T) {
	r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
	move := movableClock(r)
	refusedAt := reconcilerClock.Add(-time.Hour)
	refusalWithoutEntry(t, r, rr, refusedAt)
	calls := countCalls(g, unavailable)
	var reads int32
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption) error {
			if o, ok := obj.(*api.RemediationRequest); ok && gitWait(o, waitHandoffRecovery) != nil && reads == 0 {
				reads = calls.Load() + 1
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	got, _ := reconcileOnce(t, r, rr)
	w := gitWait(got, waitHandoffRecovery)
	if w == nil {
		t.Fatalf("no HandoffRecovery entry from the standing refusal: %+v", got.Status.GitWaits)
	}
	if !w.Since.Time.Equal(refusedAt) || !w.Ends.Time.Equal(refusedAt.Add(recoveryWaitBound)) {
		t.Fatalf("entry %+v, want it from the condition's time %v", w, refusedAt)
	}
	if reads != 1 {
		t.Fatalf("the entry was recorded after %d reads of the Git host; want before any", reads-1)
	}
	move(25 * time.Hour)
	got, _ = reconcileOnce(t, r, rr)
	endedAtTheBound(t, got)
}

// A 503 past the deadline records HandoffGrace; a refusal then sets the condition and the entry is lost on its way
// (the build that wrote them apart). The request does not end at the grace with a message about 24 hours: the entry
// is recorded from the condition, and it ends at the refusal's bound.
func TestRecoveryEntryLostAfterTheGraceKeepsTheBound(t *testing.T) {
	r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
	move := movableClock(r)
	ctx := context.Background()
	key := client.ObjectKeyFromObject(rr)
	pastDeadline(t, r, rr, 10*time.Minute)
	failRecoveryReadsRetryable(g, 0)
	got, _ := reconcileOnce(t, r, rr)
	if gitWait(got, waitHandoffGrace) == nil {
		t.Fatalf("the 503 past the deadline recorded no grace: %+v", got.Status.GitWaits)
	}
	refuseAll(g)
	base := r.Client.(client.WithWatch)
	stripped := false
	strip := interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption) error {
			if o, ok := obj.(*api.RemediationRequest); ok && !stripped && gitWait(o, waitHandoffRecovery) != nil {
				stripped = true
				keep := o.Status.GitWaits[:0]
				for _, w := range o.Status.GitWaits {
					if w.Kind != api.GitWaitHandoffRecovery {
						keep = append(keep, w)
					}
				}
				o.Status.GitWaits = keep
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if o, ok := obj.(*api.RemediationRequest); ok && !stripped && gitWait(o, waitHandoffRecovery) != nil {
				stripped = true
				o.Status.GitWaits = nil
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	r.Client, r.APIReader = strip, strip
	_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	if err := base.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	c := watchedRecovery(got)
	if c == nil {
		t.Fatalf("the refusal set no condition: %+v", got.Status.Conditions)
	}
	refusedAt := c.LastTransitionTime
	r.Client, r.APIReader = base, base
	move(10 * time.Minute)
	got, _ = reconcileOnce(t, r, rr)
	if got.Status.Phase != api.PhasePolicyChecked {
		c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionPROpened)
		t.Fatalf("the recovery ended at the grace (%s, %+v) although its first refusal was 10 minutes ago",
			got.Status.Phase, c)
	}
	// The entry starts at the refusal the condition records, not at the reconcile that found it missing, and the
	// write that records it removes the grace of the aborted intent.
	w := gitWait(got, waitHandoffRecovery)
	if w == nil || !w.Since.Equal(&refusedAt) || !w.Ends.Time.Equal(refusedAt.Add(recoveryWaitBound)) {
		t.Fatalf("HandoffRecovery entry %+v, want it from the refusal at %v", w, refusedAt)
	}
	if gitWait(got, waitHandoffGrace) != nil {
		t.Fatalf("the grace of the aborted intent stands with the refusal's entry: %+v", got.Status.GitWaits)
	}
	move(recoveryWaitBound - 10*time.Minute)
	got, _ = reconcileOnce(t, r, rr)
	endedAtTheBound(t, got)
}

// A status stored before the third decision of 2026-11-18 holds a HandoffGrace entry that ends 30 hours after the
// first refusal next to the HandoffRecovery entry. Each ends at its own end: the grace no longer applies once the
// refusal's entry stands and is removed, and the request ends at the end of its HandoffRecovery entry, with the span
// that entry recorded, not 30 hours.
func TestRecoveryWaitEndsAtItsOwnEnd(t *testing.T) {
	r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
	move := movableClock(r)
	ctx := context.Background()
	refuseAll(g)
	refusedAt := reconcilerClock.Truncate(time.Second)
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	rr.Status.Deadline = &metav1.Time{Time: refusedAt.Add(30*time.Hour - handoffRecoveryGrace)}
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionPullRequestUnreadable,
		Status: metav1.ConditionTrue, Reason: ReasonGitForbidden, Message: "the read of the branch was refused",
		LastTransitionTime: metav1.NewTime(refusedAt)})
	rr.Status.GitWaits = []api.GitWait{
		{Kind: api.GitWaitHandoffGrace, Since: metav1.NewTime(refusedAt), Operation: branchHeadRead,
			Ends: &metav1.Time{Time: refusedAt.Add(30 * time.Hour)}},
		{Kind: api.GitWaitHandoffRecovery, Since: metav1.NewTime(refusedAt), Operation: branchHeadRead,
			Ends: &metav1.Time{Time: refusedAt.Add(recoveryWaitBound)}},
	}
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	move(time.Hour)
	got, _ := reconcileOnce(t, r, rr)
	if gitWait(got, waitHandoffGrace) != nil || gitWait(got, waitHandoffRecovery) == nil {
		t.Fatalf("waits = %+v, want the HandoffRecovery entry alone", got.Status.GitWaits)
	}
	move(recoveryWaitBound - time.Hour)
	got, _ = reconcileOnce(t, r, rr)
	endedAtTheBound(t, got)
	msg := meta.FindStatusCondition(got.Status.Conditions, api.ConditionPROpened).Message
	if !strings.Contains(msg, recoveryWaitBound.String()) || strings.Contains(msg, (30*time.Hour).String()) {
		t.Fatalf("message %q does not state the span of the HandoffRecovery entry, %v", msg, recoveryWaitBound)
	}
}

// RequestDeadline and the kinds of the recovery of a handoff never apply to the same request (waitKind.applies), and
// an entry is recorded only for a kind that applies (putGitWaits records any other as Paced), so status.gitWaits holds
// at most five entries of its six kinds: Paced, one of RequestDeadline or the two handoff kinds, and the two merge
// waits. Every combination of what applies reads is tried.
func TestGitWaitsNeverHoldTheDeadlineWithTheHandoffKinds(t *testing.T) {
	phases := []api.RemediationPhase{api.PhasePending, api.PhasePolicyChecked, api.PhasePRCreated,
		api.PhaseAwaitingApproval, api.PhaseNeedsIntervention, api.PhaseMerged}
	fromPhases := []api.HandoffPhase{"", api.HandoffReserved, api.HandoffDone, api.HandoffAborted}
	toPhases := []api.HandoffPhase{"", api.HandoffIntent, api.HandoffDone, api.HandoffAborted}
	n := 0
	for _, phase := range phases {
		for _, replaces := range []bool{false, true} {
			for _, from := range fromPhases {
				for _, to := range toPhases {
					for _, stale := range []bool{false, true} {
						rr := &api.RemediationRequest{}
						rr.Status.Phase = phase
						if replaces {
							rr.Spec.Replaces = &api.RequestReplacement{Name: oldName}
						}
						if from != "" || to != "" {
							rr.Status.Handoff = &api.Handoff{}
						}
						if from != "" {
							rr.Status.Handoff.From = &api.IncomingHandoff{Phase: from}
						}
						if to != "" {
							rr.Status.Handoff.To = &api.OutgoingHandoff{Phase: to}
						}
						if stale {
							markStaleForBranch(rr, "main")
						}
						n++
						deadline := waitRequestDeadline.applies(rr)
						for _, k := range []waitKind{waitHandoffGrace, waitHandoffRecovery} {
							if deadline && k.applies(rr) {
								t.Fatalf("RequestDeadline and %s both apply to %+v", gitWaitKinds[k], rr.Status)
							}
						}
					}
				}
			}
		}
	}
	if n < 300 {
		t.Fatalf("only %d combinations", n)
	}
}

// The second half of the proof: an entry of a kind that no longer applies (a RequestDeadline entry left from before
// the request recovered a handoff) is removed by the write that adds a handoff kind, so the two never stand together
// in the stored status either, and five entries are enough for six kinds.
func TestGitWaitsAddRemovesAnEntryThatNoLongerApplies(t *testing.T) {
	r, _, _, rr, _, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	since := metav1.NewTime(r.now().Truncate(time.Second))
	rr.Status.GitWaits = []api.GitWait{{Kind: api.GitWaitRequestDeadline, Since: since,
		Ends: &metav1.Time{Time: since.Add(time.Hour)}, Operation: branchHeadRead}}
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if waitRequestDeadline.applies(rr) {
		t.Fatal("the fixture recovers no handoff")
	}
	for _, write := range []func() error{
		func() error {
			return r.putGitWaits(ctx, rr, []api.GitWait{refusalEntry(r.now(), branchHeadRead)})
		},
		func() error {
			_, err := r.abortOnRefusal(ctx, rr, recoveryUnreadable(rr, refusedBranchRead()), refusedBranchRead())
			return err
		},
	} {
		if err := write(); err != nil {
			t.Fatal(err)
		}
		got := &api.RemediationRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(rr), got); err != nil {
			t.Fatal(err)
		}
		if gitWait(got, waitRequestDeadline) != nil || gitWait(got, waitHandoffRecovery) == nil {
			t.Fatalf("waits = %+v, want the HandoffRecovery entry without the RequestDeadline one", got.Status.GitWaits)
		}
		got.Status.GitWaits = append(got.Status.GitWaits, api.GitWait{Kind: api.GitWaitRequestDeadline, Since: since,
			Ends: &metav1.Time{Time: since.Add(time.Hour)}, Operation: branchHeadRead})
		got.Status.GitWaits = slices.DeleteFunc(got.Status.GitWaits, func(w api.GitWait) bool {
			return w.Kind == api.GitWaitHandoffRecovery
		})
		if err := r.Status().Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		got.DeepCopyInto(rr)
	}
}

// The write that starts a wait for the merge commit adds an entry too, so it removes an entry whose kind no longer
// applies first, as every write that adds one does.
func TestStartMergeWaitRemovesAnEntryThatNoLongerApplies(t *testing.T) {
	r, _, _, rr, cr, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	since := metav1.NewTime(r.now().Truncate(time.Second))
	rr.Status.GitWaits = []api.GitWait{{Kind: api.GitWaitRequestDeadline, Since: since,
		Ends: &metav1.Time{Time: since.Add(time.Hour)}, Operation: branchHeadRead}}
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if err := r.startMergeWait(ctx, rr, cr, "read the merge commit"); err != nil {
		t.Fatal(err)
	}
	got := &api.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), got); err != nil {
		t.Fatal(err)
	}
	if gitWait(got, waitRequestDeadline) != nil || gitWait(got, waitMergeCheck) == nil {
		t.Fatalf("waits = %+v, want the MergeCheck entry without the RequestDeadline one", got.Status.GitWaits)
	}
}

func refusedBranchRead() error {
	return freshness.WrapProvider(branchHeadRead, fmt.Errorf("status 403: %w", providers.ErrForbidden))
}
