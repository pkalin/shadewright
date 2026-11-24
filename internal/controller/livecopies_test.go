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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/providers"
)

// A reconcile starts from the request as the API server holds it (decision of 2026-11-19 on #786): the cache is not
// read for it, so a cache that cannot serve it, or serves an older copy, never decides a reconcile.
func TestReconcileReadsTheRequestFromTheAPIServer(t *testing.T) {
	key := types.NamespacedName{Namespace: requestNamespace, Name: "absent"}
	cache := clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*api.RemediationRequest); ok {
				return errors.New("the cache was read for the request")
			}
			return c.Get(context.Background(), k, obj, opts...)
		},
	}).Build()
	live := clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).Build()
	r := &RemediationRequestReconciler{Client: cache, APIReader: live}
	if res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key}); err != nil || res.RequeueAfter != 0 {
		t.Fatalf("a request the API server does not hold: %+v, %v; want nothing to do", res, err)
	}
}

// livePair is a predecessor and its replacement after an aborted handoff, whose replacement records the merge of its
// commit on pull request 1 (with or without a receipt), as the API server holds them.
func livePair(t *testing.T) (pred, repl *api.RemediationRequest) {
	t.Helper()
	head := strings.Repeat("b", 40)
	pred = &api.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: requestNamespace, Name: oldName,
		UID: hmPredUID}}
	pred.Status.PullRequest = &api.PullRequestStatus{Number: 1, HeadSHA: strings.Repeat("a", 40)}
	pred.Status.Handoff = &api.Handoff{To: &api.OutgoingHandoff{Name: newName, UID: hmReplUID,
		Phase: api.HandoffAborted}}
	pred.Status.Phase = api.PhaseNeedsIntervention
	repl = &api.RemediationRequest{ObjectMeta: metav1.ObjectMeta{Namespace: requestNamespace, Name: newName,
		UID: hmReplUID}}
	repl.Status.Handoff = &api.Handoff{From: &api.IncomingHandoff{Name: oldName, UID: hmPredUID,
		Phase: api.HandoffDone}}
	merged := metav1.NowMicro()
	repl.Status.PullRequest = &api.PullRequestStatus{Number: 1, HeadSHA: head, MergedAt: &merged}
	repl.Status.Phase = api.PhaseMerged
	return pred, repl
}

// The admission of a wait for a read of a merge decides on live copies of both requests of the handoff: no wait is
// added to a predecessor that ended on its replacement's record of that merge (Superseded, MergedForReplacement),
// nor to one whose replacement, read at the admission, holds that record, whatever the reconcile's copy of the
// predecessor shows; a failed read of the replacement is the Kubernetes API's, for a retry; a predecessor with no
// such record on either side gets its wait.
func TestMergeWaitAdmissionDecidesOnLiveCopies(t *testing.T) {
	cr := providers.ChangeRequest{Number: 1, HeadSHA: strings.Repeat("b", 40), State: providers.ChangeRequestMerged}
	pred, repl := livePair(t)
	settled := pred.DeepCopy()
	settled.Status.Phase = api.PhaseSuperseded
	settled.Status.Conditions = []metav1.Condition{{Type: api.ConditionApproved, Status: metav1.ConditionFalse,
		Reason: ReasonMergedForReplacement, Message: "merged for the replacement", LastTransitionTime: metav1.Now()}}
	unrecorded := repl.DeepCopy()
	unrecorded.Status.PullRequest.MergedAt = nil
	unrecorded.Status.Phase = api.PhasePRCreated
	failing := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object,
		opts ...client.GetOption) error {
		if k.Name == newName {
			return apierrors.NewServiceUnavailable("down")
		}
		return c.Get(ctx, k, obj, opts...)
	}}
	for _, c := range []struct {
		name    string
		stored  []client.Object
		funcs   interceptor.Funcs
		cluster bool
		added   bool
		cached  *api.RemediationRequest
	}{
		{"settled on the replacement's record", []client.Object{settled, unrecorded}, interceptor.Funcs{}, false, false, nil},
		{"the replacement holds the record", []client.Object{pred, repl}, interceptor.Funcs{}, false, false, nil},
		{"the replacement cannot be read", []client.Object{pred, repl}, failing, true, false, nil},
		{"no record on either side", []client.Object{pred, unrecorded}, interceptor.Funcs{}, false, true, nil},
		{"the replacement is gone", []client.Object{pred}, interceptor.Funcs{}, false, true, nil},
		{"the cache has not seen the replacement's record", []client.Object{pred, repl}, interceptor.Funcs{}, false, false,
			unrecorded},
	} {
		t.Run(c.name, func(t *testing.T) {
			objs := make([]client.Object, 0, len(c.stored))
			for _, o := range c.stored {
				objs = append(objs, o.DeepCopyObject().(client.Object))
			}
			cl := interceptor.NewClient(clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).
				WithStatusSubresource(&api.RemediationRequest{}).WithObjects(objs...).Build(), c.funcs)
			r := &RemediationRequestReconciler{Client: cl, APIReader: cl, Now: func() time.Time { return reconcilerClock }}
			if c.cached != nil {
				// The cache serves the replacement as it was before its record; the API server holds the record.
				r.Client = lagging(cl, map[string]*api.RemediationRequest{newName: c.cached})
			}
			// The reconcile's copy is the predecessor before either change: not settled, and with no wait.
			copyOf := &api.RemediationRequest{}
			if err := cl.Get(context.Background(), client.ObjectKeyFromObject(pred), copyOf); err != nil {
				t.Fatal(err)
			}
			copyOf.Status = *pred.Status.DeepCopy()
			err := r.startMergeWait(context.Background(), copyOf, cr, "check the plan at the merge")
			var ce *clusterError
			switch {
			case c.cluster && !errors.As(err, &ce):
				t.Fatalf("got %v, want the Kubernetes API's error", err)
			case !c.cluster && c.added && err != nil:
				t.Fatalf("got %v, want the wait added", err)
			case !c.cluster && !c.added && !apierrors.IsConflict(err):
				t.Fatalf("got %v, want a conflict and no wait", err)
			}
			stored := &api.RemediationRequest{}
			if err := cl.Get(context.Background(), client.ObjectKeyFromObject(pred), stored); err != nil {
				t.Fatal(err)
			}
			if got := gitWait(stored, waitMergeCheck) != nil; got != c.added {
				t.Fatalf("a MergeCheck wait stored: %t, want %t (waits %v)", got, c.added, stored.Status.GitWaits)
			}
			// The same admission holds for a wait a failed read records (putGitWaits): refused, it is recorded as
			// Paced, never as a wait for the merge.
			if c.cluster {
				return
			}
			w, _ := wholeWait(api.GitWait{Kind: api.GitWaitMergeContent, PullRequest: waitPullRequest(cr)}, "read", reconcilerClock)
			if err := r.putGitWaits(context.Background(), copyOf, []api.GitWait{w}); err != nil {
				t.Fatal(err)
			}
			if err := cl.Get(context.Background(), client.ObjectKeyFromObject(pred), stored); err != nil {
				t.Fatal(err)
			}
			if got := gitWait(stored, waitMergeContent) != nil; got != c.added {
				t.Fatalf("a MergeContent wait stored: %t, want %t (waits %v)", got, c.added, stored.Status.GitWaits)
			}
		})
	}
}

// lagging is a cache over live that has not seen the latest changes of the requests serve names: it serves the copy
// given (nil: not seen at all, NotFound) for a read of each, and reads every other object from live.
func lagging(live client.Client, serve map[string]*api.RemediationRequest) client.Client {
	return interceptor.NewClient(live.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if rr, ok := obj.(*api.RemediationRequest); ok {
				if old, ok := serve[k.Name]; ok {
					if old == nil {
						return apierrors.NewNotFound(api.GroupVersion.WithResource("remediationrequests").GroupResource(), k.Name)
					}
					old.DeepCopyInto(rr)
					return nil
				}
			}
			return c.Get(ctx, k, obj, opts...)
		},
	})
}

// The gate before a Git write decides on live copies of both requests (decision of 2026-11-19 on #786): the request
// as stored shows that it waits for the other's merge record although the reconcile's copy does not, or the other
// request as stored holds a claim on the merge that the cache has not seen; either holds the write.
func TestRecordHeldDecidesOnLiveCopies(t *testing.T) {
	ctx := context.Background()
	t.Run("the request as stored waits for the record", func(t *testing.T) {
		r, _, _, rr, _, _ := gateWorld(t)
		older := rr.DeepCopy()
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: api.ConditionApproved,
			Status: metav1.ConditionFalse, Reason: ReasonMergeRecordPending, Message: "waits"})
		if err := r.Status().Update(ctx, rr); err != nil {
			t.Fatal(err)
		}
		older.ResourceVersion = rr.ResourceVersion
		if err := r.recordHeld(ctx, older); !errors.Is(err, errRecordHeld) {
			t.Fatalf("recordHeld = %v, want the write held", err)
		}
	})
	t.Run("the other request holds a claim the cache has not seen", func(t *testing.T) {
		r, _, p, rr, cr, head := gateWorld(t)
		older := p.DeepCopy()
		merged := cr
		merged.HeadSHA, merged.State = head, providers.ChangeRequestMerged
		w, _ := wholeWait(api.GitWait{Kind: api.GitWaitMergeCheck, PullRequest: waitPullRequest(merged)}, "read",
			reconcilerClock)
		p.Status.GitWaits = []api.GitWait{w}
		if err := r.Status().Update(ctx, p); err != nil {
			t.Fatal(err)
		}
		live := r.Client
		r.Client, r.APIReader = lagging(live, map[string]*api.RemediationRequest{oldName: older}), live
		if err := r.recordHeld(ctx, rr); !errors.Is(err, errRecordHeld) {
			t.Fatalf("recordHeld = %v, want the write held", err)
		}
	})
}

// The abort of a standing intent reads the replacement from the API server: one the cache has not seen yet is not
// gone, and the intent stands.
func TestSettleHandoffReadsTheReplacementLive(t *testing.T) {
	r, _, p, _, _, _ := gateWorld(t)
	live := r.Client
	r.Client, r.APIReader = lagging(live, map[string]*api.RemediationRequest{newName: nil}), live
	upd := p.DeepCopy()
	r.settleHandoff(context.Background(), upd)
	if intent(upd) == nil {
		t.Fatal("the intent was aborted for a replacement the cache has not seen")
	}
}

// Whether a moved head is the replacement's commit is decided on the replacement as the API server holds it: one
// deleted since, which the cache still serves, makes the head no handoff commit.
func TestExpectedHandoffHeadReadsTheReplacementLive(t *testing.T) {
	r, g, p, rr, _, head := gateWorld(t)
	ctx := context.Background()
	if ok, err := r.expectedHandoffHead(ctx, g, p, head); err != nil || !ok {
		t.Fatalf("fixture: expectedHandoffHead = %v, %v, want the replacement's commit", ok, err)
	}
	cached := rr.DeepCopy()
	if err := r.Delete(ctx, rr); err != nil {
		t.Fatal(err)
	}
	live := r.Client
	r.Client, r.APIReader = lagging(live, map[string]*api.RemediationRequest{newName: cached}), live
	if ok, err := r.expectedHandoffHead(ctx, g, p, head); err != nil || ok {
		t.Fatalf("expectedHandoffHead = %v, %v, want false for a replacement that is gone", ok, err)
	}
}

// The supersede chain of an inherited marker is read from the API server: a link deleted since, which the cache still
// serves, ends the walk, and the pull request is not the replacement's to close.
func TestInheritedMarkerReadsTheChainLive(t *testing.T) {
	r, _, p, rr, cr, head := handoffAppendedWorld(t, func(*api.RemediationRequest) {})
	ctx := context.Background()
	if err := r.recordHandoff(ctx, rr, p, cr, head); err != nil {
		t.Fatal(err)
	}
	root := &api.RemediationRequest{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: rootName}, root); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, root.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	live := r.Client
	r.Client, r.APIReader = lagging(live, map[string]*api.RemediationRequest{rootName: root}), live
	if ok, err := r.ownsMarker(ctx, cr.Body, rr); err != nil || ok {
		t.Fatalf("ownsMarker = %v, %v, want false", ok, err)
	}
}
