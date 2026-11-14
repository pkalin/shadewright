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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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
	if q := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionQueued); q.Message !=
		queuedMessages[ReasonTargetLocked] {
		t.Fatalf("Queued message behind an open pull request = %q", q.Message)
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

// expectStillQueued checks that the waiter stays Queued with TargetLocked, without reading Git, and that its message
// says it waits for the verification of the holder's merged change, not for an open pull request.
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
	if c.Message != verifyingLockMessage {
		t.Fatalf("%s: Queued message = %q, want %q", when, c.Message, verifyingLockMessage)
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
// field, changed from the same previous value to another one in the same direction. A holder planned from another
// previous value, one whose change set the waiter's previous value, one that went the other way, and any value
// that cannot be compared leave the waiter to be decided as usual.
func TestSucceededInstead(t *testing.T) {
	type change = func(*remediationv1alpha1.SetContainerMemoryLimit)
	to := func(previous, desired string) change {
		return func(a *remediationv1alpha1.SetContainerMemoryLimit) {
			a.Previous, a.Desired = resource.MustParse(previous), resource.MustParse(desired)
		}
	}
	const unbounded = "1e300" // outside the bounded form of quantity.Parse: not Comparable
	for name, c := range map[string]struct {
		phase          remediationv1alpha1.RemediationPhase
		waiter, holder change
		want           bool
	}{
		"up, holder up":             {remediationv1alpha1.PhaseSucceeded, to("256Mi", "384Mi"), to("256Mi", "512Mi"), true},
		"down, holder down":         {remediationv1alpha1.PhaseSucceeded, to("256Mi", "128Mi"), to("256Mi", "192Mi"), true},
		"up, holder down":           {remediationv1alpha1.PhaseSucceeded, to("256Mi", "512Mi"), to("256Mi", "128Mi"), false},
		"down, holder up":           {remediationv1alpha1.PhaseSucceeded, to("256Mi", "128Mi"), to("256Mi", "384Mi"), false},
		"not Succeeded":             {remediationv1alpha1.PhaseFailed, to("256Mi", "384Mi"), to("256Mi", "512Mi"), false},
		"Inconclusive":              {remediationv1alpha1.PhaseInconclusive, to("256Mi", "384Mi"), to("256Mi", "512Mi"), false},
		"from another previous":     {remediationv1alpha1.PhaseSucceeded, to("256Mi", "384Mi"), to("128Mi", "512Mi"), false},
		"to the waiter's previous":  {remediationv1alpha1.PhaseSucceeded, to("256Mi", "384Mi"), to("128Mi", "256Mi"), false},
		"holder desired unbounded":  {remediationv1alpha1.PhaseSucceeded, to("256Mi", "384Mi"), to("256Mi", unbounded), false},
		"holder previous unbounded": {remediationv1alpha1.PhaseSucceeded, to("256Mi", "384Mi"), to(unbounded, "512Mi"), false},
		"waiter previous unbounded": {remediationv1alpha1.PhaseSucceeded, to(unbounded, "384Mi"), to(unbounded, "512Mi"), false},
		"waiter desired unbounded":  {remediationv1alpha1.PhaseSucceeded, to("256Mi", unbounded), to("256Mi", "512Mi"), false},
		"another container (holder)": {remediationv1alpha1.PhaseSucceeded, to("256Mi", "384Mi"), func(a *remediationv1alpha1.SetContainerMemoryLimit) {
			to("256Mi", "512Mi")(a)
			a.Container = sidecarContainer
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			waiter := otherRequest("waiter", limitsTarget, "", 0)
			c.waiter(waiter.Spec.Action.SetContainerMemoryLimit)
			holder := ofIncident(withPullRequest(otherRequest(closedHolder, limitsTarget, c.phase, time.Minute),
				ReasonPullRequestOpened))
			c.holder(holder.Spec.Action.SetContainerMemoryLimit)
			if got := succeededInstead(context.Background(), holder, waiter); got != c.want {
				t.Fatalf("succeededInstead = %v, want %v", got, c.want)
			}
		})
	}
}

// TestHolderDeletedWhileVerifyingReleasesWaiter checks #719 for a holder deleted by hand while its outcome is
// verified: its finalizer released the lock, so the request queued behind it goes on at once and is decided as
// usual (here the source mapping still finds the merged value in Git and the old one live: NeedsIntervention with
// ValueMismatch), and the look-up of the recorded holder logs that it is gone.
func TestHolderDeletedWhileVerifyingReleasesWaiter(t *testing.T) {
	r, c, rr, stub := holdWorld(t, true)
	mergeHolder(t, r, c, stub)
	moveHolder(t, r, c, remediationv1alpha1.PhaseWaitingForDelivery)
	moveHolder(t, r, c, remediationv1alpha1.PhaseVerifying)
	expectStillQueued(t, r, rr, stub, "Verifying")
	if err := c.Delete(context.Background(), getHolder(t, c)); err != nil {
		t.Fatal(err)
	}
	waiter := &remediationv1alpha1.RemediationRequest{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(rr), waiter); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var lines []string
	ctx := logf.IntoContext(context.Background(), funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, args)
	}, funcr.Options{}))
	if w, reason, err := r.recordedHolderOf(ctx, waiter); err != nil || w != nil {
		t.Fatalf("recordedHolderOf = %v, %q, %v; want nothing", w, reason, err)
	}
	mu.Lock()
	logged := slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, closedHolder) })
	mu.Unlock()
	if !logged {
		t.Fatalf("the deleted holder was not logged: %q", lines)
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

// TestTwoWaitersBehindOneSucceededHolder checks #719 with two requests, of two Incidents, queued behind one holder
// for the same field: when the holder ends Succeeded, both end Superseded with AnotherRequestSucceeded.
func TestTwoWaitersBehindOneSucceededHolder(t *testing.T) {
	r, c, rr, stub := holdWorld(t, true)
	second := rr.DeepCopy()
	second.ObjectMeta = metav1.ObjectMeta{Namespace: rr.Namespace, Name: "rr-second"}
	second.Status = remediationv1alpha1.RemediationRequestStatus{}
	second.Spec.IncidentRef = remediationv1alpha1.IncidentRef{Name: "third-incident",
		UID: "77777777-7777-4777-8777-777777777777"}
	if err := c.Create(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	res, got, err := reconcileLimits(t, r, second)
	expectQueuedAfter(t, r, second, res, err, got, stub, ReasonTargetLocked, admissionRecheckDelay)
	mergeHolder(t, r, c, stub)
	moveHolder(t, r, c, remediationv1alpha1.PhaseWaitingForDelivery)
	moveHolder(t, r, c, remediationv1alpha1.PhaseVerifying)
	for _, w := range []*remediationv1alpha1.RemediationRequest{rr, second} {
		expectStillQueued(t, r, w, stub, "Verifying, "+w.Name)
	}
	deliver(t, c)
	stub.result = resolved()
	if err := r.succeed(context.Background(), getHolder(t, c), time.Minute, "No recurrence in the window"); err != nil {
		t.Fatalf("succeed: %v", err)
	}
	for _, w := range []*remediationv1alpha1.RemediationRequest{rr, second} {
		_, got, err := reconcileLimits(t, r, w)
		if err != nil {
			t.Fatalf("%s: Reconcile: %v", w.Name, err)
		}
		pf := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
		if got.Status.Phase != remediationv1alpha1.PhaseSuperseded || pf == nil ||
			pf.Reason != remediationv1alpha1.ReasonAnotherRequestSucceeded {
			t.Fatalf("%s: phase %q, PlanFresh %+v; want Superseded with AnotherRequestSucceeded", w.Name,
				got.Status.Phase, pf)
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
