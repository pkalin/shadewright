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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// The bound of the wait of a recovery that met a refusal (#786, decision of 2026-11-17): 24 hours from the first
// refusal, then the handoff is aborted and the replacement ends NeedsIntervention with the lock kept, and the look
// that settles the lock records a merge of its commit found later.

// refuseAll makes the Git host refuse (403) every call of the provider.
func refuseAll(g *fakegit.Git) {
	g.SetFail(func(string) error { return fmt.Errorf("status 403: %w", providers.ErrForbidden) })
}

// movableClock gives r a clock the test moves, starting at reconcilerClock, and returns the setter.
func movableClock(r *RemediationRequestReconciler) func(time.Duration) {
	now := reconcilerClock
	r.Now = func() time.Time { return now }
	return func(d time.Duration) { now = now.Add(d) }
}

// boundEvent is one event of the generated orders.
type boundEvent byte

const (
	evRefusal  boundEvent = 'R' // the Git host refuses every call from now on
	evRestart  boundEvent = 'S' // the executor restarts: a new reconciler, the same cluster and Git host
	evBound    boundEvent = 'B' // the bound of the wait passes: the clock moves by the bound and a minute
	evMerged   boundEvent = 'M' // a person merges the pull request with the replacement's commit
	evClosed   boundEvent = 'C' // a person closes the pull request
	evReadable boundEvent = 'W' // the Git host answers again
)

// boundPR is the state of the pull request in the model.
type boundPR int

const (
	prIsOpen boundPR = iota
	prIsMerged
	prIsClosed
)

// boundState is where the model's replacement is.
type boundState int

const (
	stWaiting   boundState = iota // PolicyChecked, the recovery waiting or not started
	stBound                       // NeedsIntervention at the bound of the wait
	stTakenOver                   // the takeover recorded
	stMerged                      // the merge of its commit recorded
	stCancelled                   // ended on the close (HandoffAborted)
)

// boundModel is the rule, written from the decision, for one reconcile of the replacement after each event.
type boundModel struct {
	refusing  bool
	pr        boundPR
	now       time.Duration
	waitStart *time.Duration
	phase     boundState
	held      bool
}

func (m *boundModel) frozen() bool {
	return m.phase == stTakenOver || m.phase == stMerged || m.phase == stCancelled
}

func (m *boundModel) apply(e boundEvent) {
	switch e {
	case evRefusal:
		m.refusing = true
	case evReadable:
		m.refusing = false
	case evBound:
		m.now += cleanupFailingBound + time.Minute
	case evMerged:
		m.pr = prIsMerged
	case evClosed:
		m.pr = prIsClosed
	}
}

// step is one reconcile: a waiting recovery that cannot read starts its wait, or ends at the bound; one that reads
// ends as the pull request says. A request ended at the bound reads the pull request on: a merge is recorded, a
// close releases the lock, anything else keeps it.
func (m *boundModel) step() {
	switch m.phase {
	case stWaiting:
		if m.refusing {
			if m.waitStart == nil {
				start := m.now
				m.waitStart = &start
			} else if m.now-*m.waitStart >= cleanupFailingBound {
				m.phase, m.held = stBound, true
			}
			return
		}
		m.phase = map[boundPR]boundState{prIsOpen: stTakenOver, prIsMerged: stMerged, prIsClosed: stCancelled}[m.pr]
	case stBound:
		if m.refusing || !m.held {
			return
		}
		switch m.pr {
		case prIsMerged:
			m.phase = stMerged
		case prIsClosed:
			m.held = false
		}
	}
}

// boundOrders is every order of every subset of the events, a merge and a close never both.
func boundOrders() [][]boundEvent {
	all := []boundEvent{evRefusal, evRestart, evBound, evMerged, evClosed, evReadable}
	var out [][]boundEvent
	var walk func(seq []boundEvent, used map[boundEvent]bool)
	walk = func(seq []boundEvent, used map[boundEvent]bool) {
		out = append(out, append([]boundEvent(nil), seq...))
		for _, e := range all {
			if used[e] || (e == evMerged && used[evClosed]) || (e == evClosed && used[evMerged]) {
				continue
			}
			used[e] = true
			walk(append(seq, e), used)
			used[e] = false
		}
	}
	walk(nil, map[boundEvent]bool{})
	return out
}

// Every order of a refusal, a restart, the bound passing, a merge or a close, and the Git host answering again, with
// one reconcile after each event: the replacement ends as the rule says (boundModel), and one ended at the bound
// keeps the lock unless a read shows the pull request closed.
func TestRecoveryBoundGeneratedOrders(t *testing.T) {
	orders := boundOrders()
	if len(orders) < 500 {
		t.Fatalf("only %d orders", len(orders))
	}
	for _, seq := range orders {
		t.Run(string(seq)+"_", func(t *testing.T) { boundOrder(t, seq) })
	}
}

func boundOrder(t *testing.T, seq []boundEvent) {
	r, g, _, rr, cr, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	move := movableClock(r)
	m := &boundModel{pr: prIsOpen, phase: stWaiting}
	key := client.ObjectKeyFromObject(rr)
	for _, e := range seq {
		if m.frozen() {
			break
		}
		m.apply(e)
		r = boundEventOn(t, r, g, cr, move, m.refusing, e)
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		m.step()
	}
	got := &api.RemediationRequest{}
	if err := r.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	checkBoundEnd(t, m, got)
}

// boundEventOn makes event e happen to the world of r and g; refusing is whether the Git host refuses after it. It
// returns the reconciler to use from then on (a new one after a restart).
func boundEventOn(t *testing.T, r *RemediationRequestReconciler, g *fakegit.Git, cr providers.ChangeRequest,
	move func(time.Duration), refusing bool, e boundEvent) *RemediationRequestReconciler {
	t.Helper()
	switch e {
	case evRefusal:
		refuseAll(g)
	case evReadable:
		g.SetFail(nil)
	case evRestart:
		return &RemediationRequestReconciler{Client: r.Client, APIReader: r.APIReader, ReviewerHints: true,
			OpenRepository: r.OpenRepository, Now: r.Now}
	case evBound:
		move(cleanupFailingBound + time.Minute)
	case evMerged, evClosed:
		g.SetFail(nil)
		var err error
		if e == evMerged {
			_, err = g.MergeWithCommit(cr.Number)
		} else {
			err = g.CloseChangeRequest(context.Background(), cr.Number)
		}
		if err != nil {
			t.Fatal(err)
		}
		if refusing {
			refuseAll(g)
		}
	}
	return r
}

// checkBoundEnd compares the stored replacement with the model's ending.
func checkBoundEnd(t *testing.T, m *boundModel, got *api.RemediationRequest) {
	t.Helper()
	opened := meta.FindStatusCondition(got.Status.Conditions, api.ConditionPROpened)
	switch m.phase {
	case stWaiting:
		if got.Status.Phase != api.PhasePolicyChecked || got.Status.Handoff.From.Phase != api.HandoffReserved {
			t.Fatalf("phase %q, handoff %q: want the recovery still waiting", got.Status.Phase,
				got.Status.Handoff.From.Phase)
		}
		if (m.waitStart != nil) != (watchedRecovery(got) != nil) {
			t.Fatalf("PullRequestUnreadable = %+v, want a wait recorded: %v", watchedRecovery(got), m.waitStart != nil)
		}
	case stBound:
		if got.Status.Phase != api.PhaseNeedsIntervention || opened == nil || opened.Reason != ReasonRecoveryWaitExpired ||
			got.Status.Handoff.From.Phase != api.HandoffAborted {
			t.Fatalf("phase %q, PullRequestOpened %+v, handoff %q: want the end at the bound", got.Status.Phase, opened,
				got.Status.Handoff.From.Phase)
		}
		if ptr.Deref(got.Status.HoldsTarget, false) != m.held {
			t.Fatalf("holdsTarget = %v, want %v", ptr.Deref(got.Status.HoldsTarget, false), m.held)
		}
		if m.held && m.refusing && watchedRecovery(got) == nil {
			t.Fatalf("the end lost the condition that says what is refused: %+v", got.Status.Conditions)
		}
	case stTakenOver:
		if !tookOver(got) || got.Status.Phase != api.PhasePRCreated {
			t.Fatalf("phase %q, handoff %q: want the takeover", got.Status.Phase, got.Status.Handoff.From.Phase)
		}
	case stMerged:
		// The world has no policy, so the check at the merge marks the plan stale, and a reconcile after the
		// merge ends it Inconclusive (StalePlanMerged): the merge is recorded either way, with its receipt.
		if (got.Status.Phase != api.PhaseMerged && got.Status.Phase != api.PhaseInconclusive) ||
			got.Status.MergeReceipt == nil || !tookOver(got) {
			t.Fatalf("phase %q, receipt %v: want the merge of the replacement's commit recorded", got.Status.Phase,
				got.Status.MergeReceipt)
		}
	case stCancelled:
		if got.Status.Phase != api.PhaseCancelled || opened == nil || opened.Reason != api.ReasonHandoffAborted {
			t.Fatalf("phase %q, PullRequestOpened %+v: want the handoff ended on the close", got.Status.Phase, opened)
		}
	}
}

// boundWorld is a replacement whose recovery waits on a refusal of every call since reconcilerClock, with the clock
// the test moves.
func boundWorld(t *testing.T) (*RemediationRequestReconciler, *fakegit.Git, *api.RemediationRequest,
	*api.RemediationRequest, func(time.Duration)) {
	t.Helper()
	r, g, p, rr, _, _ := handoffAppendedWorld(t, nil)
	move := movableClock(r)
	refuseAll(g)
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if c := watchedRecovery(rr); c == nil || c.Reason != ReasonGitForbidden {
		t.Fatalf("the recovery does not wait: %+v", c)
	}
	return r, g, p, rr, move
}

// While the replacement's recovery waits, the predecessor's freshness check, its merge gate and its deletion are
// held for the handoff; once the bound has ended the replacement, each of them works again.
func TestRecoveryBoundReleasesThePredecessor(t *testing.T) {
	r, g, p, rr, move := boundWorld(t)
	ctx := context.Background()
	reconcilePred := func() {
		for range 3 {
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(p)})
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
			t.Fatal(err)
		}
	}

	// Before the bound, with the Git host answering the predecessor: it waits for the handoff.
	g.SetFail(nil)
	move(cleanupFailingBound - time.Hour)
	reconcilePred()
	if intent(p) == nil || p.Status.Phase != api.PhaseAwaitingApproval {
		t.Fatalf("before the bound the predecessor left the handoff: phase %q, handoff %+v", p.Status.Phase,
			p.Status.Handoff.To)
	}
	if r.gateDue(p) {
		t.Fatal("the merge gate ran while the intent stands")
	}
	if _, wait, err := r.handoffHolds(ctx, p); err != nil || !wait {
		t.Fatalf("handoffHolds = wait %v, %v: the deletion did not wait for the replacement", wait, err)
	}

	// The bound passes while the replacement still cannot read.
	refuseAll(g)
	move(2 * time.Hour)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhaseNeedsIntervention {
		t.Fatalf("phase = %q, want NeedsIntervention at the bound", rr.Status.Phase)
	}
	g.SetFail(nil)

	// The deletion no longer waits, and the merge gate is due again once the predecessor's look aborts its intent.
	if keep, wait, err := r.handoffHolds(ctx, p.DeepCopy()); err != nil || wait || keep {
		t.Fatalf("handoffHolds = keep %v, wait %v, %v: want the close of a request that was not handed over", keep,
			wait, err)
	}
	gate := p.DeepCopy()
	r.settleHandoff(ctx, gate)
	if intent(gate) != nil || !r.gateDue(gate) {
		t.Fatalf("the merge gate is not due after the bound: handoff %+v", gate.Status.Handoff.To)
	}

	// The predecessor's look checks it again: the head the replacement pushed is not its own any more.
	reconcilePred()
	if p.Status.Handoff.To.Phase != api.HandoffAborted || p.Status.Phase == api.PhaseAwaitingApproval {
		t.Fatalf("after the bound the predecessor is still held: phase %q, handoff %q", p.Status.Phase,
			p.Status.Handoff.To.Phase)
	}
}

// The clock of the bound is the condition's lastTransitionTime from the first refusal: a restart and a change of the
// reason (here the repository leaving the settings) keep it. Near the bound the request is still looked at after the
// poll interval, or after the delay the host asked for, never sooner, and nothing ends it before the bound.
func TestRecoveryBoundCountsFromTheFirstRefusal(t *testing.T) {
	r, g, _, rr, move := boundWorld(t)
	ctx := context.Background()
	key := client.ObjectKeyFromObject(rr)
	start := watchedRecovery(rr).LastTransitionTime

	restore := changeSettings(t, r, func(s *api.RemediationSettings) { s.Spec.Repositories[0].URL = otherRepositoryURL })
	move(time.Hour)
	r = &RemediationRequestReconciler{Client: r.Client, APIReader: r.APIReader, ReviewerHints: true,
		OpenRepository: r.OpenRepository, Now: r.Now}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	c := watchedRecovery(rr)
	if c == nil || c.Reason != reasonRepositoryUnavailable || !c.LastTransitionTime.Equal(&start) {
		t.Fatalf("PullRequestUnreadable = %+v, want the new reason with the time of the first refusal %v", c, start)
	}
	restore()

	// Thirty seconds before the bound: the poll interval, then the delay the host asked for, kept.
	move(cleanupFailingBound - time.Hour - 30*time.Second)
	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	if err != nil || res.RequeueAfter != DefaultPullRequestPollInterval {
		t.Fatalf("Reconcile = %+v, %v; want the poll interval near the bound", res, err)
	}
	failBranchHeadRetryable(g, 10*time.Minute)
	res, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	if err != nil || res.RequeueAfter != 10*time.Minute {
		t.Fatalf("Reconcile = %+v, %v; want the delay the host asked for", res, err)
	}
	if err := r.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhasePolicyChecked {
		t.Fatalf("phase = %q before the bound", rr.Status.Phase)
	}

	// The first look after the bound ends it.
	move(time.Minute)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPROpened); rr.Status.Phase !=
		api.PhaseNeedsIntervention || c == nil || c.Reason != ReasonRecoveryWaitExpired {
		t.Fatalf("phase %q, PullRequestOpened %+v: want the end at the bound", rr.Status.Phase, c)
	}
	if !ptr.Deref(rr.Status.HoldsTarget, false) || watchedRecovery(rr) == nil {
		t.Fatalf("the end released the lock or dropped the condition: holdsTarget %v, conditions %+v",
			rr.Status.HoldsTarget, rr.Status.Conditions)
	}
}

// The write that ends the request at the bound is retried after a conflict (the request changed between the read and
// the write): the next reconcile reads it again and ends it, from the recorded condition alone.
func TestRecoveryBoundEndRetriedAfterAConflict(t *testing.T) {
	r, _, _, rr, move := boundWorld(t)
	ctx := context.Background()
	key := client.ObjectKeyFromObject(rr)
	move(cleanupFailingBound + time.Minute)
	base := r.Client.(client.WithWatch)
	bumped := false
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if obj.GetName() == newName && !bumped {
				bumped = true
				live := &api.RemediationRequest{}
				if err := base.Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
					return err
				}
				live.Annotations = map[string]string{"test.shadewright.io/bound": "changed"}
				if err := base.Update(ctx, live); err != nil {
					return err
				}
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	r.Client, r.APIReader = c, c
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err == nil {
		t.Fatal("the end was written over a request that changed")
	}
	if !bumped {
		t.Fatal("the end made no status write")
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhaseNeedsIntervention || rr.Status.Handoff.From.Phase != api.HandoffAborted ||
		!ptr.Deref(rr.Status.HoldsTarget, false) {
		t.Fatalf("phase %q, handoff %q, holdsTarget %v: want the end at the bound after the retry", rr.Status.Phase,
			rr.Status.Handoff.From.Phase, rr.Status.HoldsTarget)
	}
}

// A request whose spec.cancel is set waits for its recovery too, and the bound ends it as one without a cancel.
func TestRecoveryBoundEndsACancelledRequest(t *testing.T) {
	r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
	move := movableClock(r)
	ctx := context.Background()
	key := client.ObjectKeyFromObject(rr)
	rr.Spec.Cancel = true
	if err := r.Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	refuseAll(g)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	move(cleanupFailingBound - time.Minute)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhasePolicyChecked {
		t.Fatalf("phase = %q before the bound", rr.Status.Phase)
	}
	move(2 * time.Minute)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPROpened); rr.Status.Phase !=
		api.PhaseNeedsIntervention || c == nil || c.Reason != ReasonRecoveryWaitExpired {
		t.Fatalf("phase %q, PullRequestOpened %+v: want the end at the bound", rr.Status.Phase, c)
	}
	if meta.FindStatusCondition(rr.Status.Conditions, api.ConditionCancelPending) != nil {
		t.Fatal("CancelPending outlived the end")
	}
}

// A request that never met a refusal is never ended by the grace of #688 on a failure of the Kubernetes API (here a
// transport error on the read of the predecessor): that failure goes to the error backoff, and the end after the
// grace is for a Git host that cannot be read.
func TestRecoveryGraceNeverEndsOnAnAPIFailure(t *testing.T) {
	r, _, p, rr, _, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	pastDeadline(t, r, rr, handoffRecoveryGrace+time.Hour)
	base := r.Client.(client.WithWatch)
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if key.Name == p.Name {
				return transportError()
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r.Client, r.APIReader = c, c
	for range 2 {
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err == nil {
			t.Fatal("a failure of the Kubernetes API was not returned for the error backoff")
		}
	}
	got := &api.RemediationRequest{}
	if err := base.Get(ctx, client.ObjectKeyFromObject(rr), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != api.PhasePolicyChecked {
		t.Fatalf("phase = %q: a failure of the Kubernetes API ended the recovery", got.Status.Phase)
	}
	if o := meta.FindStatusCondition(got.Status.Conditions, api.ConditionPROpened); o != nil &&
		strings.Contains(o.Message, "could not be read") {
		t.Fatalf("PullRequestOpened blames the Git host: %+v", o)
	}
}

// A failure of the Git host past the grace still ends a request that never met a refusal, and its message names the
// Git host, not the branch alone.
func TestRecoveryGraceEndNamesTheGitHost(t *testing.T) {
	r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
	ctx := context.Background()
	failBranchHeadRetryable(g, 0)
	pastDeadline(t, r, rr, handoffRecoveryGrace+time.Hour)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPROpened)
	if rr.Status.Phase != api.PhaseNeedsIntervention || c == nil || c.Reason != ReasonHandoffRecoveryFailed ||
		!strings.Contains(c.Message, "Git host could not be read") {
		t.Fatalf("phase %q, PullRequestOpened %+v", rr.Status.Phase, c)
	}
}

// After the end at the bound, the look that settles the lock reads the pull request the recovery reads: a failed read
// of the Kubernetes API there is returned for the error backoff and keeps the lock.
func TestHeldRecoveryReturnsAnAPIFailure(t *testing.T) {
	r, g, p, rr, move := boundWorld(t)
	ctx := context.Background()
	key := client.ObjectKeyFromObject(rr)
	move(cleanupFailingBound + time.Minute)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	g.SetFail(nil)
	base := r.Client.(client.WithWatch)
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if k == (types.NamespacedName{Namespace: p.Namespace, Name: p.Name}) {
				return errors.New("connection refused")
			}
			return c.Get(ctx, k, obj, opts...)
		},
	})
	r.Client, r.APIReader = c, c
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err == nil {
		t.Fatal("a failed read of the predecessor was not returned")
	}
	got := &api.RemediationRequest{}
	if err := base.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if !ptr.Deref(got.Status.HoldsTarget, false) || got.Status.Phase != api.PhaseNeedsIntervention {
		t.Fatalf("phase %q, holdsTarget %v", got.Status.Phase, got.Status.HoldsTarget)
	}
}

// A failure of the Kubernetes API on the look that finds the bound passed ends nothing: it goes to the error backoff,
// and the next look that fails on the Git host ends the request.
func TestRecoveryBoundNotOnAnAPIFailure(t *testing.T) {
	r, _, p, rr, move := boundWorld(t)
	ctx := context.Background()
	key := client.ObjectKeyFromObject(rr)
	move(cleanupFailingBound + time.Minute)
	base := r.Client.(client.WithWatch)
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if k.Name == p.Name {
				return transportError()
			}
			return c.Get(ctx, k, obj, opts...)
		},
	})
	r.Client, r.APIReader = c, c
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err == nil {
		t.Fatal("a failure of the Kubernetes API was not returned for the error backoff")
	}
	if err := base.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhasePolicyChecked {
		t.Fatalf("phase = %q: a failure of the Kubernetes API ended the recovery at the bound", rr.Status.Phase)
	}
	r.Client, r.APIReader = base, base
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := base.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhaseNeedsIntervention {
		t.Fatalf("phase = %q, want the end at the bound once the Git host refuses again", rr.Status.Phase)
	}
}

// The write that starts a recovery's wait sets the condition's time to the executor's clock, also over a
// PullRequestUnreadable that was True for another reason, so the bound counts from the first refusal.
func TestRecoveryWaitStartsItsOwnClock(t *testing.T) {
	r, _, _, rr, _, _ := handoffAppendedWorld(t, nil)
	move := movableClock(r)
	ctx := context.Background()
	old := metav1.NewTime(reconcilerClock.Add(-48 * time.Hour))
	meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionPullRequestUnreadable,
		Status: metav1.ConditionTrue, Reason: "SomethingElse", Message: "another read", LastTransitionTime: old})
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	move(time.Hour)
	want := refusedRead(rr, ReasonGitForbidden, fmt.Errorf("status 403: %w", providers.ErrForbidden), 0)
	if err := r.setRecoveryWait(ctx, rr, want); err != nil {
		t.Fatal(err)
	}
	got := &api.RemediationRequest{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), got); err != nil {
		t.Fatal(err)
	}
	c := watchedRecovery(got)
	if c == nil || !c.LastTransitionTime.Time.Equal(r.now().Truncate(time.Second)) {
		t.Fatalf("PullRequestUnreadable = %+v, want the time of this refusal %v", c, r.now())
	}
	if l := watchedRecovery(rr); l == nil || !l.LastTransitionTime.Equal(&c.LastTransitionTime) {
		t.Fatalf("the caller's copy = %+v, want the stored time", l)
	}
}
