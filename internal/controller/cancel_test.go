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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/providers"
)

// countingResolver counts its calls and fails each.
type countingResolver struct{ calls atomic.Int32 }

func (c *countingResolver) Resolve(context.Context, sourcemap.Input) (sourcemap.Result, error) {
	c.calls.Add(1)
	return sourcemap.Result{}, &sourcemap.TransientError{Op: "map the target"}
}

// cancelWorld is deadlineWorld's request with spec.cancel set and status st, and a reconciler with a recorder, a
// resolver that counts its calls and a Git provider that fails any use.
func cancelWorld(t *testing.T, st remediationv1alpha1.RemediationRequestStatus) (client.Client,
	*remediationv1alpha1.RemediationRequest, *RemediationRequestReconciler, *countingResolver, *record.FakeRecorder) {
	t.Helper()
	c, rr := deadlineWorld(t)
	got := &remediationv1alpha1.RemediationRequest{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(rr), got); err != nil {
		t.Fatal(err)
	}
	got.Spec.Cancel = true
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if st.Phase != "" {
		got.Status = st
		if err := c.Status().Update(context.Background(), got); err != nil {
			t.Fatal(err)
		}
	}
	src := &countingResolver{}
	rec := record.NewFakeRecorder(10)
	r := &RemediationRequestReconciler{Client: c, Scheme: c.Scheme(), Source: src, Recorder: rec,
		OpenRepository: func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			return nil, errors.New("no Git write is expected")
		}}
	return c, got, r, src, rec
}

func reconcileCancel(t *testing.T, c client.Client, r *RemediationRequestReconciler,
	rr *remediationv1alpha1.RemediationRequest) *remediationv1alpha1.RemediationRequest {
	t.Helper()
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v, want none", res.RequeueAfter)
	}
	got := &remediationv1alpha1.RemediationRequest{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(rr), got); err != nil {
		t.Fatal(err)
	}
	return got
}

// An undecided request with spec.cancel ends Cancelled before the policy check: nothing is read from Git, no lock
// is taken, one status write and one Normal Event (#289).
func TestCancelUndecided(t *testing.T) {
	for _, phase := range []remediationv1alpha1.RemediationPhase{"", remediationv1alpha1.PhasePending} {
		t.Run(string(phase)+"-phase", func(t *testing.T) {
			c, rr, r, src, rec := cancelWorld(t, remediationv1alpha1.RemediationRequestStatus{Phase: phase})
			got := reconcileCancel(t, c, r, rr)
			if got.Status.Phase != remediationv1alpha1.PhaseCancelled {
				t.Fatalf("phase = %q, want Cancelled", got.Status.Phase)
			}
			if n := src.calls.Load(); n != 0 {
				t.Errorf("the policy check ran %d times", n)
			}
			if ptr.Deref(got.Status.HoldsTarget, true) {
				t.Errorf("holdsTarget = %v, want false", got.Status.HoldsTarget)
			}
			opened := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPROpened)
			if opened == nil || opened.Status != metav1.ConditionFalse || opened.Reason != ReasonCancelRequested ||
				opened.Message != messageCancelledUndecided {
				t.Errorf("PullRequestOpened = %+v", opened)
			}
			if e := <-rec.Events; e != "Normal "+EventCancelled+" "+messageCancelledUndecided {
				t.Errorf("Event = %q", e)
			}
			again := reconcileCancel(t, c, r, got)
			if again.ResourceVersion != got.ResourceVersion || again.Status.Phase != remediationv1alpha1.PhaseCancelled {
				t.Errorf("a second reconcile wrote: phase %q", again.Status.Phase)
			}
			select {
			case e := <-rec.Events:
				t.Errorf("a second Event: %q", e)
			default:
			}
		})
	}
}

// A request that reached a final phase, or the merge, before the executor saw spec.cancel keeps that phase (the
// API refuses the cancel there; an executor still acts on nothing if the stored object has it): no Cancelled, no
// Event, no Git (#289).
func TestCancelLeavesAFinalPhase(t *testing.T) {
	for _, phase := range []remediationv1alpha1.RemediationPhase{remediationv1alpha1.PhaseDenied,
		remediationv1alpha1.PhaseSuperseded, remediationv1alpha1.PhaseNeedsIntervention, remediationv1alpha1.PhaseExpired} {
		t.Run(string(phase), func(t *testing.T) {
			c, rr, r, _, rec := cancelWorld(t, remediationv1alpha1.RemediationRequestStatus{Phase: phase,
				HoldsTarget: new(false)})
			got := reconcileCancel(t, c, r, rr)
			if got.Status.Phase != phase {
				t.Fatalf("phase = %q, want %q unchanged", got.Status.Phase, phase)
			}
			for _, c := range got.Status.Conditions {
				if c.Reason == ReasonCancelRequested {
					t.Errorf("condition %s has reason %s", c.Type, c.Reason)
				}
			}
			select {
			case e := <-rec.Events:
				t.Errorf("an Event: %q", e)
			default:
			}
		})
	}
}

// A replacement pushed its commit and lost the status write that records the takeover, its commit was merged, and
// then a person cancelled it: the recovery of the handoff runs before the cancel, so the request ends Merged, never
// Cancelled (#289).
func TestCancelRecoversTheHandoffFirst(t *testing.T) {
	r, g, _, rr, cr, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	if _, err := g.Merge(cr.Number); err != nil {
		t.Fatal(err)
	}
	rr.Spec.Cancel = true
	if err := r.Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	got := &remediationv1alpha1.RemediationRequest{}
	for range 3 {
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
		if err := r.Get(ctx, client.ObjectKeyFromObject(rr), got); err != nil {
			t.Fatal(err)
		}
		if got.Status.Phase != remediationv1alpha1.PhasePolicyChecked {
			break
		}
	}
	if got.Status.Phase != remediationv1alpha1.PhaseMerged {
		t.Fatalf("phase = %q, want Merged: the replacement's own commit was merged", got.Status.Phase)
	}
}

// A request created with spec.cancel already set ends Cancelled at its first reconcile, without opening a
// repository (#289).
func TestCancelSetOnCreate(t *testing.T) {
	_, rr := deadlineWorld(t)
	rr = rr.DeepCopy()
	rr.ResourceVersion = ""
	rr.Spec.Cancel = true
	c := clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).WithObjects(rr).
		WithStatusSubresource(&remediationv1alpha1.RemediationRequest{}).Build()
	var opens atomic.Int32
	src := &countingResolver{}
	rec := record.NewFakeRecorder(10)
	r := &RemediationRequestReconciler{Client: c, Scheme: c.Scheme(), Source: src, Recorder: rec,
		OpenRepository: func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			opens.Add(1)
			return nil, errors.New("no Git call is expected")
		}}
	got := reconcileCancel(t, c, r, rr)
	if got.Status.Phase != remediationv1alpha1.PhaseCancelled {
		t.Fatalf("phase = %q, want Cancelled", got.Status.Phase)
	}
	if n := opens.Load(); n != 0 {
		t.Errorf("a repository was opened %d times", n)
	}
	if n := src.calls.Load(); n != 0 {
		t.Errorf("the policy check ran %d times", n)
	}
	if e := <-rec.Events; e != "Normal "+EventCancelled+" "+messageCancelledUndecided {
		t.Errorf("Event = %q", e)
	}
}

// A cancel while the request's handoff intent stands closes nothing and says so on CancelPending (HandoffInFlight),
// looks again after the poll interval, and after 24 hours says that the cancel did not finish (#289).
func TestCancelWaitsForAHandoffVisibly(t *testing.T) {
	r, g, p, _, cr, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	p.Spec.Cancel = true
	if err := r.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	reconcileP := func() (reconcile.Result, *remediationv1alpha1.RemediationRequest) {
		t.Helper()
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(p)})
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		got := &remediationv1alpha1.RemediationRequest{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(p), got); err != nil {
			t.Fatal(err)
		}
		return res, got
	}
	res, got := reconcileP()
	if res.RequeueAfter != DefaultPullRequestPollInterval {
		t.Errorf("RequeueAfter = %v, want the poll interval", res.RequeueAfter)
	}
	if got.Status.Phase != remediationv1alpha1.PhaseAwaitingApproval {
		t.Fatalf("phase = %q, want AwaitingApproval", got.Status.Phase)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionCancelPending)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonHandoffInFlight {
		t.Fatalf("CancelPending = %+v", c)
	}
	if e := <-rec.Events; !strings.HasPrefix(e, "Warning "+EventCancelPending+" ") {
		t.Errorf("Event = %q", e)
	}
	if now, _ := g.GetChangeRequest(ctx, cr.Number); now.State != providers.ChangeRequestOpen {
		t.Fatalf("the pull request was closed while the handoff stands: %s", now.State)
	}
	later := reconcilerClock.Add(cleanupFailingBound + 2*time.Second)
	r.Now = func() time.Time { return later }
	res, got = reconcileP()
	if res.RequeueAfter != DefaultPullRequestPollInterval {
		t.Errorf("RequeueAfter = %v after the bound, want the poll interval", res.RequeueAfter)
	}
	c = meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionCancelPending)
	if c == nil || c.Reason != ReasonCancelNotFinished || got.Status.Phase != remediationv1alpha1.PhaseAwaitingApproval {
		t.Fatalf("after the bound: phase %q, CancelPending = %+v", got.Status.Phase, c)
	}
	if e := <-rec.Events; !strings.HasPrefix(e, "Warning "+EventCancelPending+" ") {
		t.Errorf("Event = %q", e)
	}
}

// A replacement whose recovery of a handoff the Git host refuses (403 on the read of the branch head) while
// spec.cancel is set is a cancel that cannot finish, never an end: it keeps its phase with CancelPending and the
// refusal's reason, is watched, and once access is back a merge of its own commit ends it Merged (#289).
func TestCancelWatchesARefusedRecovery(t *testing.T) {
	r, g, _, rr, cr, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	rec := record.NewFakeRecorder(10)
	r.Recorder = rec
	rr.Spec.Cancel = true
	if err := r.Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	g.SetFail(func(op string) error {
		if op == opBranchHead {
			return fmt.Errorf("status 403: %w", providers.ErrForbidden)
		}
		return nil
	})
	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != DefaultPullRequestPollInterval {
		t.Errorf("RequeueAfter = %v, want the poll interval", res.RequeueAfter)
	}
	got := &remediationv1alpha1.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != remediationv1alpha1.PhasePolicyChecked {
		t.Fatalf("phase = %q, want PolicyChecked: the replacement's commit may still be merged", got.Status.Phase)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionCancelPending)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonGitForbidden {
		t.Fatalf("CancelPending = %+v", c)
	}
	// The refusal starts the recovery's own wait too, whose bound holds for a cancelled request (#786).
	if u := watchedRecovery(got); u == nil || u.Reason != ReasonGitForbidden {
		t.Fatalf("PullRequestUnreadable = %+v, want the wait of the recovery recorded", u)
	}
	if ev := drain(rec); !containsPrefix(ev, "Warning "+EventCancelPending+" ") ||
		!containsPrefix(ev, "Warning "+EventPullRequestUnreadable+" ") {
		t.Errorf("Events = %q", ev)
	}

	g.SetFail(nil)
	if _, err := g.Merge(cr.Number); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
		if err := r.Get(ctx, client.ObjectKeyFromObject(rr), got); err != nil {
			t.Fatal(err)
		}
		if got.Status.Phase != remediationv1alpha1.PhasePolicyChecked {
			break
		}
	}
	if got.Status.Phase != remediationv1alpha1.PhaseMerged {
		t.Fatalf("phase = %q, want Merged: the replacement's own commit was merged", got.Status.Phase)
	}
}

// The end of a cancelled request read from a stale cache (the request changed since) records no Event Cancelled:
// its status write would be refused as a conflict, and the retry may end it otherwise (#289).
func TestCancelRecordsNoEventForAStaleCopy(t *testing.T) {
	c, rr, r, _, rec := cancelWorld(t, remediationv1alpha1.RemediationRequestStatus{
		Phase: remediationv1alpha1.PhasePending})
	ctx := context.Background()
	stale := rr.DeepCopy()
	if rr.Annotations == nil {
		rr.Annotations = map[string]string{}
	}
	rr.Annotations["example.com/touched"] = "true"
	if err := c.Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	if _, err := r.cancelUndecided(ctx, stale); err == nil {
		t.Fatal("the end of a stale copy was not handed back for a retry")
	}
	select {
	case e := <-rec.Events:
		t.Errorf("an Event for a request that did not end: %q", e)
	default:
	}
	got := &remediationv1alpha1.RemediationRequest{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(rr), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != remediationv1alpha1.PhasePending {
		t.Fatalf("phase = %q, want Pending", got.Status.Phase)
	}
}
