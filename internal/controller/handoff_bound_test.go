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
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
)

// The abort of a handoff at a refused read of its recovery (#786, decision of 2026-11-18): the intent is aborted at
// the first refusal, the replacement watches for a merge of its commit until 24 hours after it and never takes the
// pull request over again, then ends NeedsIntervention with the lock kept and its handoff Reserved; the look that
// settles the lock keeps it while the pull request is open, records a merge of its commit whenever it comes, and
// releases the lock on a close (third decision of 2026-11-18).

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
	stWaiting   boundState = iota // PolicyChecked, the recovery not refused yet
	stWatching                    // PolicyChecked, the handoff aborted at the refusal, watching for a merge
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

// step is one reconcile: a recovery that meets its first refusal aborts the handoff and watches; one that reads before
// any refusal ends as the pull request says. A watching request ends at the bound from recorded state, and before it
// records a merge of its commit once a read shows one, nothing else. A request ended at the bound keeps the lock while
// a read shows the pull request open, records a merge of its commit once a read shows one, however long after the end,
// and leaves the lock once a read shows the pull request closed.
func (m *boundModel) step() {
	switch m.phase {
	case stWaiting:
		if m.refusing {
			start := m.now
			m.waitStart = &start
			m.phase = stWatching
			return
		}
		m.phase = map[boundPR]boundState{prIsOpen: stTakenOver, prIsMerged: stMerged, prIsClosed: stCancelled}[m.pr]
	case stWatching:
		switch {
		case m.now-*m.waitStart >= cleanupFailingBound:
			m.phase, m.held = stBound, true
		case !m.refusing && m.pr == prIsMerged:
			m.phase = stMerged
		}
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
	case stWaiting, stWatching:
		if got.Status.Phase != api.PhasePolicyChecked || got.Status.Handoff.From.Phase != api.HandoffReserved {
			t.Fatalf("phase %q, handoff %q: want the recovery still going", got.Status.Phase,
				got.Status.Handoff.From.Phase)
		}
		if (m.phase == stWatching) != abortedOnRefusal(got) {
			t.Fatalf("waits = %+v, want the handoff aborted at a refusal: %v", got.Status.GitWaits, m.phase == stWatching)
		}
		if m.phase == stWatching && m.refusing && watchedRecovery(got) == nil {
			t.Fatalf("the watch lost the condition that says what is refused: %+v", got.Status.Conditions)
		}
	case stBound:
		// The handoff stays Reserved while the lock is kept; the verdict that releases it records it Aborted (#786).
		want := api.HandoffReserved
		if !m.held {
			want = api.HandoffAborted
		}
		if got.Status.Phase != api.PhaseNeedsIntervention || opened == nil || opened.Reason != ReasonRecoveryWaitExpired ||
			got.Status.Handoff.From.Phase != want {
			t.Fatalf("phase %q, PullRequestOpened %+v, handoff %q: want the end at the bound, the handoff %s",
				got.Status.Phase, opened, got.Status.Handoff.From.Phase, want)
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

// At the first refusal the predecessor's intent is aborted (decision of 2026-11-18 on #786): its freshness check, its
// merge gate and its deletion work again at once, not at the bound, and its next look stops it as for a foreign push
// (the head is the replacement's commit), while the replacement keeps watching.
func TestRecoveryAbortReleasesThePredecessor(t *testing.T) {
	r, g, p, rr, move := boundWorld(t)
	ctx := context.Background()
	if err := r.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	if intent(p) != nil || p.Status.Handoff.To.Phase != api.HandoffAborted {
		t.Fatalf("the refusal left the intent standing: %+v", p.Status.Handoff.To)
	}
	if !r.gateDue(p) {
		t.Fatal("the merge gate is not due after the abort")
	}
	if keep, wait, err := r.handoffHolds(ctx, p.DeepCopy()); err != nil || wait || keep {
		t.Fatalf("handoffHolds = keep %v, wait %v, %v: want the close of a request that was not handed over", keep,
			wait, err)
	}
	g.SetFail(nil)
	move(time.Hour)
	for range 3 {
		_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(p)})
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	if p.Status.Phase == api.PhaseAwaitingApproval {
		t.Fatalf("the predecessor still waits on a head that is the replacement's commit: phase %q", p.Status.Phase)
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(rr), rr); err != nil {
		t.Fatal(err)
	}
	if !abortedOnRefusal(rr) || tookOver(rr) || !ptr.Deref(rr.Status.HoldsTarget, false) {
		t.Fatalf("phase %q, waits %+v: want the replacement watching with the lock", rr.Status.Phase, rr.Status.GitWaits)
	}
}

// The clock of the bound is the condition's lastTransitionTime from the first refusal: a restart and a change of the
// reason (here the repository leaving the settings) keep it, and the HandoffRecovery wait ends there. Before the bound the
// request is looked at again after the poll interval, or after the delay the host asked for when longer; near the
// bound never later than its end (retryGitRead), and nothing ends it before the bound.
func TestRecoveryBoundCountsFromTheFirstRefusal(t *testing.T) {
	r, g, _, rr, move := boundWorld(t)
	ctx := context.Background()
	key := client.ObjectKeyFromObject(rr)
	start := watchedRecovery(rr).LastTransitionTime
	if w := gitWait(rr, waitHandoffRecovery); w == nil || !w.Since.Equal(&start) ||
		!w.Ends.Time.Equal(start.Add(recoveryWaitBound)) {
		t.Fatalf("HandoffRecovery wait = %+v, want it from the first refusal to %v after it", w, recoveryWaitBound)
	}

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

	// Two hours before the bound: the poll interval, then the delay the host asked for, kept.
	move(cleanupFailingBound - 3*time.Hour)
	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	if err != nil || res.RequeueAfter != DefaultPullRequestPollInterval {
		t.Fatalf("Reconcile = %+v, %v; want the poll interval before the bound", res, err)
	}
	// The watch reads the pull request, not the branch: every call fails with a delay asked for.
	g.SetFail(func(string) error {
		return &providers.RetryableError{Err: errors.New("status 503"), RetryAfter: 10 * time.Minute}
	})
	res, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	if err != nil || res.RequeueAfter != 10*time.Minute {
		t.Fatalf("Reconcile = %+v, %v; want the delay the host asked for", res, err)
	}
	// Thirty seconds before the bound: looked at again at the bound, not later.
	move(2*time.Hour - 30*time.Second)
	res, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	if err != nil || res.RequeueAfter != 30*time.Second {
		t.Fatalf("Reconcile = %+v, %v; want the end of the wait", res, err)
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
	if rr.Status.Phase != api.PhaseNeedsIntervention || rr.Status.Handoff.From.Phase != api.HandoffReserved ||
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
	failRecoveryReadsRetryable(g, 0)
	pastDeadline(t, r, rr, handoffRecoveryGrace+time.Hour)
	for range 2 {
		if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)}); err != nil {
			t.Fatal(err)
		}
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

// A failure of the Kubernetes API before the bound ends nothing: it goes to the error backoff. Past the bound the end
// is made from recorded state alone at the start of the reconcile (endPassedWait), with no read of the predecessor or
// of the Git host, so neither a failing API read nor a host that keeps failing delays it.
func TestRecoveryBoundEndsFromRecordedState(t *testing.T) {
	r, g, p, rr, move := boundWorld(t)
	ctx := context.Background()
	key := client.ObjectKeyFromObject(rr)
	base := r.Client.(client.WithWatch)
	predReads := 0
	c := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if k.Name == p.Name {
				predReads++
				return transportError()
			}
			return c.Get(ctx, k, obj, opts...)
		},
	})
	r.Client, r.APIReader = c, c
	move(cleanupFailingBound - time.Minute)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err == nil {
		t.Fatal("a failure of the Kubernetes API was not returned for the error backoff")
	}
	if err := base.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhasePolicyChecked {
		t.Fatalf("phase = %q: a failure of the Kubernetes API ended the recovery", rr.Status.Phase)
	}
	move(2 * time.Minute)
	predReads = 0
	calls := 0
	g.SetFail(func(string) error {
		calls++
		return fmt.Errorf("status 403: %w", providers.ErrForbidden)
	})
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := base.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if o := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPROpened); rr.Status.Phase !=
		api.PhaseNeedsIntervention || o == nil || o.Reason != ReasonRecoveryWaitExpired {
		t.Fatalf("phase %q, PullRequestOpened %+v: want the end at the bound", rr.Status.Phase, o)
	}
	if predReads != 0 || calls != 0 {
		t.Fatalf("the end read the predecessor %d times and the Git host %d times; want none", predReads, calls)
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
	refused := freshness.WrapProvider("read the head of the branch", fmt.Errorf("status 403: %w", providers.ErrForbidden))
	if _, err := r.abortOnRefusal(ctx, rr, recoveryUnreadable(rr, refused), refused); err != nil {
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

// The first refusal's status writes may be lost or meet a change of the request (decision of 2026-11-18 on #786): the
// HandoffRecovery entry still starts at the first refusal the status records (the condition's lastTransitionTime) and
// ends recoveryWaitBound after it, whichever reconcile records it.
func TestRecoveryWaitSurvivesALostWriteAtTheFirstRefusal(t *testing.T) {
	isEntry := func(obj client.Object) bool {
		rr, ok := obj.(*api.RemediationRequest)
		return ok && rr.Name == newName && gitWait(rr, waitHandoffRecovery) != nil
	}
	for name, c := range map[string]struct {
		// hit picks the status patch the fault is put on; lose fails it unapplied, otherwise the request is changed
		// (an annotation) between the read and the patch, a conflict.
		hit  func(client.Object) bool
		lose bool
		// fromFirst: the entry counts from the first refusal; otherwise from the reconcile after the lost write.
		fromFirst bool
	}{
		// The condition and the entry are one write (abortOnRefusal): lost, neither is recorded, and the next
		// refusal records both; a conflict is retried on a fresh read in the same reconcile.
		"write lost":      {hit: isEntry, lose: true},
		"write conflicts": {hit: isEntry, fromFirst: true},
	} {
		t.Run(name, func(t *testing.T) {
			r, g, _, rr, _, _ := handoffAppendedWorld(t, nil)
			move := movableClock(r)
			refuseAll(g)
			ctx := context.Background()
			key := client.ObjectKeyFromObject(rr)
			base := r.Client.(client.WithWatch)
			hit := false
			fake := interceptor.NewClient(base, interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object,
					patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if !hit && c.hit(obj) {
						hit = true
						if c.lose {
							return errors.New("the status write was lost")
						}
						live := &api.RemediationRequest{}
						if err := base.Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
							return err
						}
						live.Annotations = map[string]string{"test.shadewright.io/refusal": "changed"}
						if err := base.Update(ctx, live); err != nil {
							return err
						}
					}
					return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			})
			r.Client, r.APIReader = fake, fake
			first := r.now().Truncate(time.Second)
			_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			if !hit {
				t.Fatal("the fault met no status write")
			}
			move(time.Hour)
			for range 2 {
				if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
					t.Fatal(err)
				}
			}
			if err := base.Get(ctx, key, rr); err != nil {
				t.Fatal(err)
			}
			want := first
			if !c.fromFirst {
				// Nothing of the first refusal was recorded: the wait starts with the refusal the status records.
				want = first.Add(time.Hour)
			}
			w := gitWait(rr, waitHandoffRecovery)
			if w == nil || !w.Since.Time.Equal(want) || !w.Ends.Time.Equal(want.Add(recoveryWaitBound)) {
				t.Fatalf("HandoffRecovery wait = %+v, want it from %v to %v after it", w, want, recoveryWaitBound)
			}
			if n := len(rr.Status.GitWaits); n != 1 {
				t.Fatalf("waits = %+v, want the HandoffRecovery entry alone", rr.Status.GitWaits)
			}
			if c := watchedRecovery(rr); c == nil || !c.LastTransitionTime.Time.Equal(want) {
				t.Fatalf("PullRequestUnreadable = %+v, want it from %v", c, want)
			}
		})
	}
}
