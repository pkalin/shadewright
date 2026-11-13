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
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// The phases after the merge on states the API server refuses (a receipt without a merge time, an action without
// its container, a zero applied time), so they run on a fake client without the CRD's validation. The specs on
// a real API server are in phases_test.go.

var unitMerged = time.Date(2026, 11, 8, 10, 35, 14, 0, time.UTC)

// unitWorld is deadlineWorld with the Deployment holding the desired limit (384Mi) and the request in phase,
// with a merge receipt; edit changes the status before it is written. The reconciler verifies outcomes and has
// no delivery verifier.
func unitWorld(t *testing.T, phase remediationv1alpha1.RemediationPhase,
	edit func(*remediationv1alpha1.RemediationRequest)) (*RemediationRequestReconciler, client.Client,
	*remediationv1alpha1.RemediationRequest) {
	t.Helper()
	ctx := context.Background()
	c, rr := deadlineWorld(t)
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: fx.TargetNamespace, Name: appName}, dep); err != nil {
		t.Fatal(err)
	}
	dep.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("384Mi")
	if err := c.Update(ctx, dep); err != nil {
		t.Fatal(err)
	}
	merge := strings.Repeat("d", 40)
	head := strings.Repeat("c", 40)
	mergedAt := metav1.NewMicroTime(unitMerged)
	applied := metav1.NewTime(unitMerged.Add(time.Minute))
	deadline := metav1.NewTime(applied.Add(21 * time.Minute))
	rr.Status = remediationv1alpha1.RemediationRequestStatus{
		Phase:                   phase,
		LastPhaseTransitionTime: &mergedAt,
		PlanDigest:              limitsDigest,
		PullRequest: &remediationv1alpha1.PullRequestStatus{Number: 56, URL: testPullRequest56URL,
			HeadSHA: head, MergeCommitSHA: merge, MergedAt: &mergedAt},
		MergeReceipt: &remediationv1alpha1.MergeReceipt{
			Repository:   remediationv1alpha1.PlanRepository{URL: testFleetURL, InstallationID: 7},
			TargetBranch: mainBranch, PullRequest: 56, HeadSHA: head, MergeCommitSHA: merge, MergedAt: mergedAt},
	}
	if phase == remediationv1alpha1.PhaseVerifying {
		rr.Status.PullRequest.AppliedObservedAt = &applied
		rr.Status.Deadline = &deadline
	}
	if edit != nil {
		edit(rr)
	}
	if err := c.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	r := &RemediationRequestReconciler{Client: c, Scheme: c.Scheme(), VerifyOutcomes: true,
		Now: func() time.Time { return unitMerged.Add(2 * time.Minute) }}
	return r, c, rr
}

// expectUnitEnd checks that the request ended in phase with reason in Verified.
func expectUnitEnd(t *testing.T, c client.Client, rr *remediationv1alpha1.RemediationRequest,
	phase remediationv1alpha1.RemediationPhase, reason string) {
	t.Helper()
	got := &remediationv1alpha1.RemediationRequest{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(rr), got); err != nil {
		t.Fatal(err)
	}
	v := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionVerified)
	if got.Status.Phase != phase || v == nil || v.Reason != reason {
		t.Fatalf("phase %s, Verified %+v; want %s with reason %s", got.Status.Phase, v, phase, reason)
	}
}

// TestZeroMergeTimeIsUnusable: a receipt without a merge time would put the end of the delivery window in year 1.
// The API refuses it (mergedAt is required); read anyway, it ends the request Inconclusive with NoMergeReceipt,
// saying the receipt is unusable, in Merged and in WaitingForDelivery.
func TestZeroMergeTimeIsUnusable(t *testing.T) {
	for _, phase := range []remediationv1alpha1.RemediationPhase{remediationv1alpha1.PhaseMerged,
		remediationv1alpha1.PhaseWaitingForDelivery} {
		t.Run(string(phase), func(t *testing.T) {
			r, c, rr := unitWorld(t, phase, func(rr *remediationv1alpha1.RemediationRequest) {
				rr.Status.MergeReceipt.MergedAt = metav1.MicroTime{}
			})
			if _, err := r.afterMerge(context.Background(), rr); err != nil {
				t.Fatal(err)
			}
			expectUnitEnd(t, c, rr, remediationv1alpha1.PhaseInconclusive, ReasonNoMergeReceipt)
			got := &remediationv1alpha1.RemediationRequest{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(rr), got); err != nil {
				t.Fatal(err)
			}
			if v := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionVerified); !strings.Contains(v.Message, "unusable") {
				t.Errorf("Verified message %q does not say the receipt is unusable", v.Message)
			}
		})
	}
}

// TestUnreadableFieldIsNotAChangedValue: when the action's field cannot be read on the target (here an action
// without its container), the evidence was not read: Inconclusive with TargetNotChecked, not ValueChanged.
func TestUnreadableFieldIsNotAChangedValue(t *testing.T) {
	r, c, rr := unitWorld(t, remediationv1alpha1.PhaseVerifying, func(rr *remediationv1alpha1.RemediationRequest) {
		rr.Spec.Action.SetContainerMemoryLimit.Container = ""
	})
	// The spec is not part of a status write: change it in the object the reconcile reads.
	rr.Spec.Action.SetContainerMemoryLimit.Container = ""
	if _, err := r.afterMerge(context.Background(), rr); err != nil {
		t.Fatal(err)
	}
	expectUnitEnd(t, c, rr, remediationv1alpha1.PhaseInconclusive, ReasonTargetNotChecked)
}

// TestRolloutEvaluateErrorIsLogged: a rollout that cannot be evaluated (here a zero applied time) leaves RolledOut
// Unknown and logs why with the request's key.
func TestRolloutEvaluateErrorIsLogged(t *testing.T) {
	r, _, rr := unitWorld(t, remediationv1alpha1.PhaseVerifying, nil)
	rr.Status.PullRequest.AppliedObservedAt = &metav1.Time{}
	var mu sync.Mutex
	var lines []string
	log := funcr.New(func(prefix, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, args)
	}, funcr.Options{})
	ctx := logf.IntoContext(context.Background(), log)
	if _, err := r.afterMerge(ctx, rr); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, l := range lines {
		if strings.Contains(l, "could not be evaluated") && strings.Contains(l, rr.Namespace) &&
			strings.Contains(l, "no applied time") {
			return
		}
	}
	t.Fatalf("no log line about the rollout evaluation of %s in %q", rr.Name, lines)
}
