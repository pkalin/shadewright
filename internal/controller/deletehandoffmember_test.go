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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/providers"
)

// #877 is narrowed to requests outside a handoff: the deletion of a predecessor or of a replacement keeps the
// behaviour it had before. With the repository entry missing it lets go at once with a Warning Event
// PullRequestLeftOpen that says the settings hold no entry for the repository; with the RemediationSettings object
// gone it is a failure of the cleanup with reason SettingsNotFound, retried within the bound. Both results are the
// same with and without the change of #877; the follow-up issue decides these cases.
func TestDeletionOfAHandoffMemberIsUnchanged(t *testing.T) {
	hint := api.HintRef{Source: api.HintSourceGitHub, CommentID: 7,
		Digest: "sha256:" + strings.Repeat("a", 64)}
	roles := map[string]func(rr *api.RemediationRequest){
		"a predecessor": func(rr *api.RemediationRequest) {
			rr.Status.Hints = []api.Hint{{Source: hint.Source, CommentID: hint.CommentID, Digest: hint.Digest,
				State: api.HintReserved}}
			rr.Status.Handoff = &api.Handoff{To: &api.OutgoingHandoff{Name: "gone-replacement",
				UID: "5b0c3c4e-55a1-4f43-9a63-0d1f3f1b2c10", Hint: hint, ExpectedHead: strings.Repeat("b", 40),
				Phase: api.HandoffIntent}}
		},
		"a replacement": func(rr *api.RemediationRequest) {
			rr.Spec.Replaces = &api.RequestReplacement{Name: "gone-predecessor",
				UID: "3f9e6c1a-1b2d-4c5e-8f70-9a1b2c3d4e5f", Hint: hint}
			rr.Status.Handoff = &api.Handoff{From: &api.IncomingHandoff{Name: "gone-predecessor",
				UID: "3f9e6c1a-1b2d-4c5e-8f70-9a1b2c3d4e5f", Hint: hint, Phase: api.HandoffAborted}}
		},
	}
	for role, makeRole := range roles {
		for _, settingsGone := range []bool{false, true} {
			name := role + "/repository entry missing"
			if settingsGone {
				name = role + "/settings gone"
			}
			t.Run(name, func(t *testing.T) {
				r, g, rr := settlementWorld(t, api.PhaseAwaitingApproval)
				ctx := context.Background()
				recorder := record.NewFakeRecorder(20)
				r.Recorder = recorder
				n := rr.Status.PullRequest.Number
				makeRole(rr)
				rr.Finalizers = append(rr.Finalizers, FinalizerName)
				status := rr.Status
				if err := r.Update(ctx, rr); err != nil {
					t.Fatal(err)
				}
				rr.Status = status
				if err := r.Status().Update(ctx, rr); err != nil {
					t.Fatal(err)
				}
				if !inHandoff(rr) {
					t.Fatalf("the %s is not part of a handoff: %+v", role, rr.Status.Handoff)
				}
				dropRepository(t, r)
				if settingsGone {
					if err := r.Delete(ctx, &api.RemediationSettings{ObjectMeta: metav1.ObjectMeta{Name: api.SettingsName}}); err != nil {
						t.Fatal(err)
					}
				}
				if err := clientfake.AddIndex(r.Client, &api.RemediationRequest{}, clearedField, func(obj client.Object) []string {
					return []string{obj.(*api.RemediationRequest).Status.BreakerClearedBy}
				}); err != nil {
					t.Fatal(err)
				}
				if err := r.Delete(ctx, rr); err != nil {
					t.Fatal(err)
				}
				key := client.ObjectKeyFromObject(rr)
				if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
					t.Logf("Reconcile: %v", err)
				}
				if st := g.ChangeRequests()[n-1].State; st != providers.ChangeRequestOpen {
					t.Fatalf("pull request #%d is %s: want it left open", n, st)
				}
				var events []string
				for len(recorder.Events) > 0 {
					events = append(events, <-recorder.Events)
				}
				var left []string
				for _, e := range events {
					if strings.Contains(e, EventPullRequestLeftOpen) {
						left = append(left, e)
					}
				}
				got := &api.RemediationRequest{}
				err := r.Get(ctx, key, got)
				if !settingsGone {
					if !apierrors.IsNotFound(err) {
						t.Fatalf("the deleted %s is still there (%v): want it let go at once", role, err)
					}
					if len(left) != 1 || !strings.Contains(left[0], "no entry for the repository") {
						t.Fatalf("events %q: want one Warning Event %s saying the settings hold no entry for the "+
							"repository", events, EventPullRequestLeftOpen)
					}
					return
				}
				if err != nil {
					t.Fatalf("the deleted %s was let go with RemediationSettings gone: %v", role, err)
				}
				if c := meta.FindStatusCondition(got.Status.Conditions, api.ConditionCleanupFailing); c == nil ||
					c.Reason != ReasonSettingsNotFound {
					t.Fatalf("CleanupFailing = %+v: want reason %s", c, ReasonSettingsNotFound)
				}
				if len(left) != 0 {
					t.Fatalf("events %q: want no %s while the cleanup is retried", events, EventPullRequestLeftOpen)
				}
			})
		}
	}
}
