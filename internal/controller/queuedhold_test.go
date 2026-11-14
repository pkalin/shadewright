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
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// The lab run of 2026-11-14 (#719): one OOM opened three Incidents and three requests for the same memory
// limit. The first opened the pull request and the others waited Queued with TargetLocked. When the pull request
// merged, the first released the lock in the write that set Merged, the waiters went on to the policy check while
// Flux had not applied the merge yet, and the source mapping found the merged value in Git and the old one live:
// NeedsIntervention with ValueMismatch, and two Incidents Escalated for a fix that then Succeeded.

// holdWorld is limitsWorld with VerifyOutcomes as given and the first request of another Incident, for the same
// field, holding the lock with its pull request open. The request the limits check is queued behind it.
func holdWorld(t *testing.T, verify bool) (*RemediationRequestReconciler, client.Client,
	*remediationv1alpha1.RemediationRequest, *stubResolver) {
	t.Helper()
	first := ofIncident(holding(withPullRequest(otherRequest(closedHolder, limitsTarget,
		remediationv1alpha1.PhaseAwaitingApproval, time.Minute), ReasonPullRequestOpened), true))
	r, c, rr, stub := limitsWorld(t, 20, first)
	r.VerifyOutcomes = verify
	res, got, err := reconcileLimits(t, r, rr)
	expectQueuedAfter(t, r, rr, res, err, got, stub, ReasonTargetLocked, admissionRecheckDelay)
	if len(got.Status.QueuedBehind) != 1 || got.Status.QueuedBehind[0].Name != closedHolder {
		t.Fatalf("queuedBehind = %+v, want %s", got.Status.QueuedBehind, closedHolder)
	}
	return r, c, rr, stub
}

// getHolder reads the holder, the request called closedHolder.
func getHolder(t *testing.T, c client.Client) *remediationv1alpha1.RemediationRequest {
	t.Helper()
	o := &remediationv1alpha1.RemediationRequest{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: requestNamespace, Name: closedHolder}, o); err != nil {
		t.Fatal(err)
	}
	return o
}

// mergeHolder runs the stop that records the holder's merge, with its merge receipt, as the pull request step
// does, and points the source mapping at what Git then holds while Flux has not applied it: the merged value in
// the file and the old one live (ValueMismatch).
func mergeHolder(t *testing.T, r *RemediationRequestReconciler, c client.Client, stub *stubResolver) {
	t.Helper()
	holder := getHolder(t, c)
	mt := metav1.NewMicroTime(limitsNow)
	holder.Status.MergeReceipt = &remediationv1alpha1.MergeReceipt{TargetBranch: mainBranch, PullRequest: 7,
		HeadSHA: strings.Repeat("c", 40), MergeCommitSHA: strings.Repeat("d", 40), MergedAt: mt}
	if err := r.stop(context.Background(), holder, remediationv1alpha1.PhaseMerged, ReasonChangeRequestMerged,
		"Pull request #7 was merged"); !errors.Is(err, errStopped) {
		t.Fatalf("stop: %v", err)
	}
	stub.result = sourcemap.Result{Outcome: sourcemap.NeedsIntervention, Reason: sourcemap.ReasonValueMismatch,
		Message: "the memory limit in the file differs from the live value"}
}

// moveHolder writes the holder's next phase after the merge, as afterMerge does (no settleLock on the way), and
// runs checkHeld on it, as every reconcile of a request that stopped does.
func moveHolder(t *testing.T, r *RemediationRequestReconciler, c client.Client, phase remediationv1alpha1.RemediationPhase) {
	t.Helper()
	holder := getHolder(t, c)
	r.setPhase(holder, phase)
	if phase == remediationv1alpha1.PhaseVerifying {
		at := metav1.NewTime(holder.Status.LastPhaseTransitionTime.Time)
		holder.Status.PullRequest.AppliedObservedAt = &at
	}
	if err := c.Status().Update(context.Background(), holder); err != nil {
		t.Fatal(err)
	}
	if _, err := r.checkHeld(context.Background(), holder); err != nil {
		t.Fatalf("checkHeld: %v", err)
	}
}

// deliver sets the live memory limit of the target to the holder's desired value: Flux applied the merge.
func deliver(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: fx.TargetNamespace, Name: appName}, dep); err != nil {
		t.Fatal(err)
	}
	dep.Spec.Template.Spec.Containers[0].Resources.Limits = corev1.ResourceList{
		corev1.ResourceMemory: fx.MemoryLimitAction().SetContainerMemoryLimit.Desired}
	if err := c.Update(ctx, dep); err != nil {
		t.Fatal(err)
	}
}

// expectStillQueued checks that the waiter stays Queued with TargetLocked, without reading Git.
func expectStillQueued(t *testing.T, r *RemediationRequestReconciler, rr *remediationv1alpha1.RemediationRequest,
	stub *stubResolver, when string) {
	t.Helper()
	calls := stub.calls.Load()
	res, got, err := reconcileLimits(t, r, rr)
	if err != nil {
		t.Fatalf("%s: Reconcile: %v", when, err)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionQueued)
	if got.Status.Phase != "" || c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonTargetLocked {
		pa := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed)
		t.Fatalf("%s: phase %q, Queued %+v, PolicyAllowed %+v; want still Queued with TargetLocked",
			when, got.Status.Phase, c, pa)
	}
	if res.RequeueAfter != heldRecheckInterval {
		t.Fatalf("%s: RequeueAfter = %v, want %v", when, res.RequeueAfter, heldRecheckInterval)
	}
	if n := stub.calls.Load(); n != calls {
		t.Fatalf("%s: the queued request read Git", when)
	}
}

// TestQueuedRequestsWaitForTheHolderVerdict reproduces #719: with verification on, the holder keeps the lock
// from its merge through WaitingForDelivery and Verifying, the request queued behind it for the same field stays
// queued, and it ends Superseded with AnotherRequestSucceeded, naming the holder and its pull request, when the
// holder ends Succeeded.
func TestQueuedRequestsWaitForTheHolderVerdict(t *testing.T) {
	r, c, rr, stub := holdWorld(t, true)
	mergeHolder(t, r, c, stub)
	if h := getHolder(t, c); !ptr.Deref(h.Status.HoldsTarget, false) {
		t.Fatal("the holder released the lock in the write that set Merged")
	}
	expectStillQueued(t, r, rr, stub, "Merged")
	for _, phase := range []remediationv1alpha1.RemediationPhase{remediationv1alpha1.PhaseWaitingForDelivery,
		remediationv1alpha1.PhaseVerifying} {
		moveHolder(t, r, c, phase)
		if h := getHolder(t, c); !ptr.Deref(h.Status.HoldsTarget, false) {
			t.Fatalf("%s: the holder released the lock", phase)
		}
		expectStillQueued(t, r, rr, stub, string(phase))
	}
	deliver(t, c)
	stub.result = resolved()
	if err := r.succeed(context.Background(), getHolder(t, c), time.Minute,
		"No recurrence in the window"); err != nil {
		t.Fatalf("succeed: %v", err)
	}
	if h := getHolder(t, c); ptr.Deref(h.Status.HoldsTarget, true) {
		t.Fatal("the holder kept the lock after it ended Succeeded")
	}
	calls := stub.calls.Load()
	_, got, err := reconcileLimits(t, r, rr)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	pf := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
	if got.Status.Phase != remediationv1alpha1.PhaseSuperseded || pf == nil || pf.Status != metav1.ConditionFalse ||
		pf.Reason != remediationv1alpha1.ReasonAnotherRequestSucceeded || !strings.Contains(pf.Message, closedHolder) ||
		!strings.Contains(pf.Message, "#7") {
		t.Fatalf("phase %q, PlanFresh %+v; want Superseded with AnotherRequestSucceeded naming %s and #7",
			got.Status.Phase, pf, closedHolder)
	}
	if holdsLock(got) || meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionQueued) != nil {
		t.Fatalf("the superseded request holds the lock or stays Queued: %+v", got.Status)
	}
	if n := stub.calls.Load(); n != calls {
		t.Fatal("the superseded request read Git")
	}
	// A second look (a restart, a repeated event) changes nothing.
	before := got.ResourceVersion
	if _, again, err := reconcileLimits(t, r, rr); err != nil || again.ResourceVersion != before {
		t.Fatalf("a second reconcile wrote the request again (%v)", err)
	}
}

// TestQueuedRequestAfterHolderFailedIsCheckedAsUsual checks #719 for a holder that ends Failed or Inconclusive
// after its change was delivered: the request queued behind it is not superseded but checked against the live
// value as any request is. Its previous value is no longer the live one, so it ends Denied with
// PreviousValueMismatch.
func TestQueuedRequestAfterHolderFailedIsCheckedAsUsual(t *testing.T) {
	for _, phase := range []remediationv1alpha1.RemediationPhase{remediationv1alpha1.PhaseFailed,
		remediationv1alpha1.PhaseInconclusive} {
		t.Run(string(phase), func(t *testing.T) {
			r, c, rr, stub := holdWorld(t, true)
			mergeHolder(t, r, c, stub)
			moveHolder(t, r, c, remediationv1alpha1.PhaseWaitingForDelivery)
			moveHolder(t, r, c, remediationv1alpha1.PhaseVerifying)
			expectStillQueued(t, r, rr, stub, "Verifying")
			deliver(t, c)
			stub.result = resolved()
			if err := r.endVerification(context.Background(), getHolder(t, c), phase, ReasonOOMKilled,
				"The container was OOM killed under the new limit"); err != nil {
				t.Fatalf("endVerification: %v", err)
			}
			_, got, err := reconcileLimits(t, r, rr)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			pa := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed)
			if got.Status.Phase != remediationv1alpha1.PhaseDenied || pa == nil || pa.Reason != ReasonPreviousValueMismatch {
				t.Fatalf("phase %q, PolicyAllowed %+v; want Denied with PreviousValueMismatch", got.Status.Phase, pa)
			}
		})
	}
}

// TestQueuedRequestWithoutVerificationUnchanged checks that with verification off the holder still releases the
// lock in the write that sets Merged, and the request queued behind it goes on to the policy check at once, as
// before #719 (here the source mapping finds the merged value in Git: NeedsIntervention with ValueMismatch).
func TestQueuedRequestWithoutVerificationUnchanged(t *testing.T) {
	r, c, rr, stub := holdWorld(t, false)
	mergeHolder(t, r, c, stub)
	if h := getHolder(t, c); ptr.Deref(h.Status.HoldsTarget, true) {
		t.Fatal("with verification off the holder kept the lock after its merge")
	}
	_, got, err := reconcileLimits(t, r, rr)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	pa := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed)
	if got.Status.Phase != remediationv1alpha1.PhaseNeedsIntervention || pa == nil ||
		pa.Reason != string(sourcemap.ReasonValueMismatch) {
		t.Fatalf("phase %q, PolicyAllowed %+v; want NeedsIntervention with ValueMismatch", got.Status.Phase, pa)
	}
}

// TestHeldForVerification checks which merged holders keep the lock while their outcome is verified (#719): with
// verification on, Merged with a merge receipt, WaitingForDelivery and Verifying; never with it off, and never
// Merged without a receipt (ends Inconclusive at once) or a final phase. settleLock keeps such a lock.
func TestHeldForVerification(t *testing.T) {
	receipt := func(o *remediationv1alpha1.RemediationRequest) *remediationv1alpha1.RemediationRequest {
		o.Status.MergeReceipt = &remediationv1alpha1.MergeReceipt{TargetBranch: mainBranch, PullRequest: 7}
		return o
	}
	held := func(phase remediationv1alpha1.RemediationPhase) *remediationv1alpha1.RemediationRequest {
		return receipt(holding(withPullRequest(otherRequest("o", limitsTarget, phase, time.Minute),
			ReasonPullRequestOpened), true))
	}
	for name, c := range map[string]struct {
		rr     *remediationv1alpha1.RemediationRequest
		verify bool
		want   bool
	}{
		"Merged with a receipt":             {held(remediationv1alpha1.PhaseMerged), true, true},
		"WaitingForDelivery":                {held(remediationv1alpha1.PhaseWaitingForDelivery), true, true},
		"Verifying":                         {held(remediationv1alpha1.PhaseVerifying), true, true},
		"Merged with a receipt, flag off":   {held(remediationv1alpha1.PhaseMerged), false, false},
		"Verifying, flag off":               {held(remediationv1alpha1.PhaseVerifying), false, false},
		"Succeeded":                         {held(remediationv1alpha1.PhaseSucceeded), true, false},
		"Failed after the merge":            {held(remediationv1alpha1.PhaseFailed), true, false},
		"Inconclusive":                      {held(remediationv1alpha1.PhaseInconclusive), true, false},
		"Merged without a receipt (marked)": {holding(withPullRequest(otherRequest("o", limitsTarget, remediationv1alpha1.PhaseMerged, time.Minute), ReasonPullRequestOpened), true), true, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := heldForVerification(c.rr, c.verify); got != c.want {
				t.Fatalf("heldForVerification = %v, want %v", got, c.want)
			}
			settleLock(c.rr, c.verify)
			if got := ptr.Deref(c.rr.Status.HoldsTarget, false); got != c.want {
				t.Fatalf("after settleLock holdsTarget = %v, want %v", got, c.want)
			}
		})
	}
}

// TestSucceededInstead checks which holder that ended Succeeded ends a request queued behind it (#719): the same
// field, changed from the same previous value to another one. A holder planned from another previous value, or
// one whose change set the waiter's previous value, does not explain the waiter's lost baseline.
func TestSucceededInstead(t *testing.T) {
	waiter := otherRequest("waiter", limitsTarget, "", 0)
	succeeded := func() *remediationv1alpha1.RemediationRequest {
		return ofIncident(withPullRequest(otherRequest(closedHolder, limitsTarget,
			remediationv1alpha1.PhaseSucceeded, time.Minute), ReasonPullRequestOpened))
	}
	ctx := context.Background()
	if !succeededInstead(ctx, succeeded(), waiter) {
		t.Fatal("a holder of another Incident that changed the same field from the same value does not end the waiter")
	}
	for name, mutate := range map[string]func(*remediationv1alpha1.RemediationRequest){
		"not Succeeded": func(o *remediationv1alpha1.RemediationRequest) { o.Status.Phase = remediationv1alpha1.PhaseFailed },
		"Inconclusive": func(o *remediationv1alpha1.RemediationRequest) {
			o.Status.Phase = remediationv1alpha1.PhaseInconclusive
		},
		"another container": func(o *remediationv1alpha1.RemediationRequest) {
			o.Spec.Action.SetContainerMemoryLimit.Container = sidecarContainer
		},
		"from another previous value": func(o *remediationv1alpha1.RemediationRequest) {
			o.Spec.Action.SetContainerMemoryLimit.Previous = resource.MustParse("128Mi")
		},
		"to the waiter's previous value": func(o *remediationv1alpha1.RemediationRequest) {
			o.Spec.Action.SetContainerMemoryLimit.Previous = resource.MustParse("128Mi")
			o.Spec.Action.SetContainerMemoryLimit.Desired = waiter.Spec.Action.SetContainerMemoryLimit.Previous
		},
	} {
		o := succeeded()
		mutate(o)
		if succeededInstead(ctx, o, waiter) {
			t.Errorf("%s: ends the waiter, want not", name)
		}
	}
}

// TestDeletingVerifyingHolderClosesNothing checks that the finalizer of a merged request that holds the lock only
// while its outcome is verified (#719) reads no pull request: it was merged, so nothing is left to close, and the
// lock goes with the request.
func TestDeletingVerifyingHolderClosesNothing(t *testing.T) {
	holder := withPullRequest(otherRequest(closedHolder, limitsTarget, remediationv1alpha1.PhaseVerifying, time.Minute),
		ReasonPullRequestOpened)
	holder.Status.MergeReceipt = &remediationv1alpha1.MergeReceipt{TargetBranch: mainBranch, PullRequest: 7}
	holder.Status.HoldsTarget = new(true)
	holder.Status.PlanRepository = freshness.RepositoryOf(fx.RemediationSettings().Spec.Repositories[0])
	holder.Status.Source = resolved().Source
	holder.Finalizers = []string{FinalizerName}
	r, c, _, _ := limitsWorld(t, 20, holder)
	r.VerifyOutcomes = true
	opened := 0
	r.OpenRepository = func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
		opened++
		return nil, errors.New("no Git in this test")
	}
	ctx := context.Background()
	settings := &remediationv1alpha1.RemediationSettings{}
	if err := c.Get(ctx, types.NamespacedName{Name: remediationv1alpha1.SettingsName}, settings); err != nil {
		t.Fatal(err)
	}
	// The branch the API server defaults, so the repository is reachable and only the rule decides.
	settings.Spec.Repositories[0].TargetBranch = mainBranch
	if err := c.Update(ctx, settings); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, getHolder(t, c)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.finalize(ctx, getHolder(t, c)); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if opened != 0 {
		t.Fatalf("the finalizer opened the repository %d times, want none", opened)
	}
	o := &remediationv1alpha1.RemediationRequest{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: requestNamespace, Name: closedHolder}, o); err == nil {
		t.Fatalf("the request is still there with finalizers %v", o.Finalizers)
	}
}
