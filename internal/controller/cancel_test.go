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
	"sync/atomic"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
