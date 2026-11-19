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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/providers"
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
	}{
		{"settled on the replacement's record", []client.Object{settled, unrecorded}, interceptor.Funcs{}, false, false},
		{"the replacement holds the record", []client.Object{pred, repl}, interceptor.Funcs{}, false, false},
		{"the replacement cannot be read", []client.Object{pred, repl}, failing, true, false},
		{"no record on either side", []client.Object{pred, unrecorded}, interceptor.Funcs{}, false, true},
		{"the replacement is gone", []client.Object{pred}, interceptor.Funcs{}, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			objs := make([]client.Object, 0, len(c.stored))
			for _, o := range c.stored {
				objs = append(objs, o.DeepCopyObject().(client.Object))
			}
			cl := interceptor.NewClient(clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).
				WithStatusSubresource(&api.RemediationRequest{}).WithObjects(objs...).Build(), c.funcs)
			r := &RemediationRequestReconciler{Client: cl, APIReader: cl, Now: func() time.Time { return reconcilerClock }}
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
