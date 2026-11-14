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

package revert

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
)

var incKey = types.NamespacedName{Namespace: ns, Name: incName}

func reconcileIncident(p *Proposer, name string) error {
	_, err := p.reconcileIncident(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	return err
}

// TestSettlementWaitsForTheCreate: the request controller wrote Pending and made its last Incident read; the
// failed request is then deleted and the Incident controller runs before the create. It waits for the Incident's
// lock, so it never writes Abandoned (RequestGone) for a revert that is being created: the create happens, the
// entry moves to Created, and the settlement then finds the revert and leaves the entry Created.
func TestSettlementWaitsForTheCreate(t *testing.T) {
	w := newWorld()
	var p *Proposer
	settled := make(chan error, 1)
	started := false
	w.funcs.Create = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if !started {
			started = true
			if err := c.Delete(ctx, &remediationv1alpha1.RemediationRequest{ObjectMeta: metav1.ObjectMeta{
				Namespace: ns, Name: failedName}}); err != nil {
				return err
			}
			go func() { settled <- reconcileIncident(p, incName) }()
			// Go on once the settlement waits for the lock, or has finished without it.
			deadline := time.Now().Add(10 * time.Second)
			for p.locks.users(incKey) < 2 && len(settled) == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
		}
		return c.Create(ctx, obj, opts...)
	}
	p, k := w.build(t)
	if err := reconcileFailed(p); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := <-settled; err != nil {
		t.Fatalf("the settlement: %v", err)
	}
	if revertOf(t, k) == nil {
		t.Fatal("no revert")
	}
	if !onlyCreated(t, k) {
		t.Errorf("status.revertsProposed = %+v, want the failed request as Created", recorded(t, k))
	}
	if ev := events(p); len(ev) != 0 {
		t.Errorf("events = %q, want none", ev)
	}
	if n := p.locks.users(incKey); n != 0 {
		t.Errorf("the lock of the Incident has %d users after both passes, want it dropped", n)
	}
}

// TestLockWaitEndsWithTheContext: a pass waiting for an Incident's lock stops when its context ends, with an
// error for a retry, and leaves no entry behind.
func TestLockWaitEndsWithTheContext(t *testing.T) {
	var l incidentLocks
	unlock, err := l.lock(context.Background(), incKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := l.lock(ctx, incKey); err == nil {
		t.Error("a wait for a held lock did not end with its context")
	}
	unlock()
	if n := l.users(incKey); n != 0 {
		t.Errorf("the lock has %d users, want it dropped", n)
	}
}

// TestMarkCreatedOnAbandoned: a move to Created that finds the entry Abandoned while the revert exists leaves
// the final state as it is, but is not silent: one Warning event on the Incident names the revert.
func TestMarkCreatedOnAbandoned(t *testing.T) {
	w := newWorld()
	abandoned := remediationv1alpha1.ProposedRevert{RequestRef: link0, State: remediationv1alpha1.RevertAbandoned,
		Reason: remediationv1alpha1.RevertReasonRequestGone}
	w.inc.Status.RevertsProposed = []remediationv1alpha1.ProposedRevert{abandoned}
	p, k := w.build(t)
	if err := p.markCreated(context.Background(), w.failed.Spec.IncidentRef, link0); err != nil {
		t.Fatalf("markCreated: %v", err)
	}
	if got := recorded(t, k); len(got) != 1 || got[0] != abandoned {
		t.Errorf("status.revertsProposed = %+v, want the entry Abandoned as before", got)
	}
	ev := events(p)
	if len(ev) != 1 || !strings.Contains(ev[0], "Warning "+EventAbandonedButMade+" ") ||
		!strings.Contains(ev[0], revertKey.Name) {
		t.Errorf("events = %q, want one Warning %s naming the revert", ev, EventAbandonedButMade)
	}
}

// TestSettlementOfALiveRequest: from the Incident's side, a Pending entry whose request is there with its UID is
// settled when no pass of the request will finish it: the request no longer ended Failed with Verified False, or
// its action can no longer be inverted (NotRevertible), or it is being deleted (RequestGone), each with one
// Warning event on the Incident. A request its own pass will finish is left to it.
func TestSettlementOfALiveRequest(t *testing.T) {
	for name, c := range map[string]struct {
		edit   func(w *world)
		reason remediationv1alpha1.RevertReason
	}{
		"Failed no longer": {func(w *world) { w.failed.Status.Phase = remediationv1alpha1.PhaseSucceeded },
			remediationv1alpha1.RevertReasonNotRevertible},
		"Verified no longer False": {func(w *world) { w.failed.Status.Conditions[0].Status = metav1.ConditionTrue },
			remediationv1alpha1.RevertReasonNotRevertible},
		"an action that cannot be inverted": {func(w *world) {
			w.failed.Spec.Action = remediationv1alpha1.Action{Type: remediationv1alpha1.ActionAdjustKedaMaxReplicas}
		}, remediationv1alpha1.RevertReasonNotRevertible},
		"being deleted": {func(w *world) {
			w.failed.DeletionTimestamp = &metav1.Time{Time: now}
			w.failed.Finalizers = []string{hold}
		}, remediationv1alpha1.RevertReasonRequestGone},
		"revertible": {func(*world) {}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			w.inc.Status.RevertsProposed = []remediationv1alpha1.ProposedRevert{pending0}
			c.edit(w)
			p, k := w.build(t)
			for range 2 {
				if err := reconcileIncident(p, incName); err != nil {
					t.Fatalf("reconcileIncident: %v", err)
				}
			}
			if revertOf(t, k) != nil {
				t.Error("a revert was created")
			}
			ev := events(p)
			if c.reason == "" {
				if got := recorded(t, k); len(got) != 1 || got[0] != pending0 || len(ev) != 0 {
					t.Errorf("status.revertsProposed = %+v, events %q, want the entry Pending and no event", got, ev)
				}
				return
			}
			if !onlyAbandoned(t, k, c.reason) {
				t.Errorf("status.revertsProposed = %+v, want the failed request as Abandoned with %s",
					recorded(t, k), c.reason)
			}
			if !abandonedOnce(ev) || !strings.Contains(ev[0], string(c.reason)) {
				t.Errorf("events = %q, want one Warning %s naming %s", ev, EventAbandoned, c.reason)
			}
		})
	}
}

// TestSettlementGoesOnAfterAnError: a read that keeps failing for one Pending entry does not stop the settlement
// of the entries after it; the pass returns that error for a retry, and the failing entry stays Pending.
func TestSettlementGoesOnAfterAnError(t *testing.T) {
	const brokenName, goneName = "remediation-broken", "remediation-gone"
	broken := pendingEntry(brokenName, "45454545-4545-4545-8545-454545454545")
	gone := pendingEntry(goneName, "46464646-4646-4646-8646-464646464646")
	w := newWorld()
	w.inc.Status.RevertsProposed = []remediationv1alpha1.ProposedRevert{broken, gone}
	w.funcs.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
		opts ...client.GetOption) error {
		if key.Name == brokenName {
			return apierrors.NewServiceUnavailable("down")
		}
		return c.Get(ctx, key, obj, opts...)
	}
	p, k := w.build(t)
	if err := reconcileIncident(p, incName); err == nil || !apierrors.IsServiceUnavailable(err) {
		t.Errorf("reconcileIncident = %v, want the failed read for a retry", err)
	}
	got := recorded(t, k)
	if len(got) != 2 || got[0] != broken || got[1].State != remediationv1alpha1.RevertAbandoned {
		t.Errorf("status.revertsProposed = %+v, want the first Pending and the second Abandoned", got)
	}
}

// TestSettleDeletedGoesOnAfterAnError: after a request was deleted, an Incident whose read fails does not stop
// the settlement of the other Incidents; the pass returns that error for a retry.
func TestSettleDeletedGoesOnAfterAnError(t *testing.T) {
	w := newWorld()
	w.failed = nil
	w.inc.Status.RevertsProposed = []remediationv1alpha1.ProposedRevert{pending0}
	other := w.inc.DeepCopy()
	other.Name, other.UID = "incident-b", "47474747-4747-4747-8747-474747474747"
	w.extra = append(w.extra, other)
	// The first Incident read, whichever the list gives first, fails.
	var reads atomic.Int32
	w.funcs.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
		opts ...client.GetOption) error {
		if _, ok := obj.(*remediationv1alpha1.Incident); ok && reads.Add(1) == 1 {
			return apierrors.NewServiceUnavailable("down")
		}
		return c.Get(ctx, key, obj, opts...)
	}
	p, k := w.build(t)
	if err := reconcileFailed(p); err == nil || !apierrors.IsServiceUnavailable(err) {
		t.Errorf("Reconcile = %v, want the failed read for a retry", err)
	}
	abandoned := 0
	for _, name := range []string{incName, other.Name} {
		inc := &remediationv1alpha1.Incident{}
		if err := k.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, inc); err != nil {
			t.Fatal(err)
		}
		if inc.Status.RevertsProposed[0].State == remediationv1alpha1.RevertAbandoned {
			abandoned++
		}
	}
	if abandoned != 1 {
		t.Errorf("%d Incidents settled, want the one whose read did not fail", abandoned)
	}
}
