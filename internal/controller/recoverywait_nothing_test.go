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
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/pkalin/shadewright/api/v1alpha1"
)

// countStatusWrites wraps r's client so that every status write of the request named name is counted.
func countStatusWrites(r *RemediationRequestReconciler, name string) *int {
	n := 0
	c := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if obj.GetName() == name {
				n++
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch,
			opts ...client.SubResourcePatchOption) error {
			if obj.GetName() == name {
				n++
			}
			return c.SubResource(sub).Patch(ctx, obj, p, opts...)
		},
	})
	r.Client, r.APIReader = c, c
	return &n
}

// A recovery wait whose predecessor no longer hands over to it (gone, made again, or its status.handoff.to no longer
// names it) reads nothing, so the refusal it recorded no longer applies: the wait removes condition
// PullRequestUnreadable and says in fixed wording that it waits with nothing to read, in one status write, and writes
// nothing on the reconciles after it. The wait, its end and the lock stay.
func TestRecoveryWaitWithNothingToReadClearsTheRefusal(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(t *testing.T, r *RemediationRequestReconciler, p *api.RemediationRequest)
	}{
		{"the predecessor no longer names it", func(t *testing.T, r *RemediationRequestReconciler, p *api.RemediationRequest) {
			p.Status.Handoff.To = nil
			if err := r.Status().Update(context.Background(), p); err != nil {
				t.Fatal(err)
			}
		}},
		{"the predecessor is gone", func(t *testing.T, r *RemediationRequestReconciler, p *api.RemediationRequest) {
			if err := r.Delete(context.Background(), p); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, g, p, rr, move := boundWorld(t)
			ctx := context.Background()
			key := client.ObjectKeyFromObject(rr)
			if err := r.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
				t.Fatal(err)
			}
			entry := *gitWait(rr, waitHandoffRecovery)
			c.change(t, r, p)
			g.SetFail(nil)
			writes := countStatusWrites(r, rr.Name)
			for range 3 {
				move(time.Hour)
				res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				if err != nil || res.RequeueAfter == 0 {
					t.Fatalf("Reconcile = %+v, %v; want a wait", res, err)
				}
			}
			if err := r.Get(ctx, key, rr); err != nil {
				t.Fatal(err)
			}
			if u := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPullRequestUnreadable); u != nil {
				t.Fatalf("PullRequestUnreadable = %+v, want it gone: nothing is read", u)
			}
			w := gitWait(rr, waitHandoffRecovery)
			if w == nil || !w.Since.Equal(&entry.Since) || !w.Ends.Equal(entry.Ends) || !abortedOnRefusal(rr) {
				t.Fatalf("HandoffRecovery wait = %+v, want %+v kept", w, entry)
			}
			gr := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionGitReadRetry)
			if gr == nil || strings.Contains(gr.Message, "reads the pull request") ||
				!strings.Contains(gr.Message, "nothing to read") {
				t.Fatalf("GitReadRetry = %+v, want it to say the wait has nothing to read", gr)
			}
			if *writes != 1 {
				t.Fatalf("%d status writes over three reconciles, want one", *writes)
			}
			// The end of the wait says it had nothing to read, not that it watched a pull request.
			move(recoveryWaitBound)
			if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, key, rr); err != nil {
				t.Fatal(err)
			}
			po := meta.FindStatusCondition(rr.Status.Conditions, api.ConditionPROpened)
			if po == nil || po.Reason != ReasonRecoveryWaitExpired || !strings.Contains(po.Message, "with nothing to read") ||
				strings.Contains(po.Message, "saw no merge") {
				t.Fatalf("PullRequestOpened = %+v, want the end of a wait with nothing to read", po)
			}
		})
	}
}
