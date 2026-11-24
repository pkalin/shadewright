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

	"github.com/go-logr/logr/funcr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/shadewright-labs/shadewright/api/v1alpha1"
	"github.com/shadewright-labs/shadewright/internal/providers"
)

// loggedCtx is a context whose logger appends every line to the returned slice.
func loggedCtx() (context.Context, *[]string) {
	var lines []string
	return logf.IntoContext(context.Background(), funcr.New(func(_, args string) { lines = append(lines, args) },
		funcr.Options{})), &lines
}

// A stopped replacement's look (heldRecovery) that meets a final refusal and cannot write condition
// PullRequestUnreadable returns the refusal with the failed write, as observeFailed does, so the refusal reaches the
// error log of that attempt.
func TestHeldRecoveryReturnsTheRefusalWithAFailedConditionWrite(t *testing.T) {
	r, _, _, rr, move := boundWorld(t)
	ctx := context.Background()
	key := client.ObjectKeyFromObject(rr)
	move(recoveryWaitBound + time.Minute)
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, key, rr); err != nil {
		t.Fatal(err)
	}
	if rr.Status.Phase != api.PhaseNeedsIntervention || !ptr.Deref(rr.Status.HoldsTarget, false) {
		t.Fatalf("phase %q, holds %v: want the end of the wait with the lock kept", rr.Status.Phase,
			rr.Status.HoldsTarget)
	}
	// The condition is gone, so the refusal must be written again.
	meta.RemoveStatusCondition(&rr.Status.Conditions, api.ConditionPullRequestUnreadable)
	if err := r.Status().Update(ctx, rr); err != nil {
		t.Fatal(err)
	}
	failed := errors.New("status patch refused by the test")
	c := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch,
			opts ...client.SubResourcePatchOption) error {
			if obj.GetName() == rr.Name {
				return failed
			}
			return c.SubResource(sub).Patch(ctx, obj, p, opts...)
		},
	})
	r.Client, r.APIReader = c, c
	_, done, err := r.heldRecovery(ctx, rr)
	if !done || !errors.Is(err, failed) {
		t.Fatalf("heldRecovery = done %v, %v; want the failed write", done, err)
	}
	if _, _, final := refusal(err); !final {
		t.Fatalf("heldRecovery = %v; want the refusal returned with the failed write", err)
	}
}

// endMergedElsewhere logs the release of the target lock only when it released it: a request whose status records a
// pull request has no HandoffAborted verdict (lockverdict.HandoffAborted), keeps the lock, and the log says so.
func TestEndMergedElsewhereLogsTheReleaseOnlyWhenReleased(t *testing.T) {
	r, _, _, rr, _ := boundWorld(t)
	if !ptr.Deref(rr.Status.HoldsTarget, false) {
		t.Fatal("the replacement does not hold the lock")
	}
	rr.Status.PullRequest = &api.PullRequestStatus{Number: 7, URL: "https://example.com/pr/7"}
	ctx, lines := loggedCtx()
	if err := r.endMergedElsewhere(ctx, rr, providers.ChangeRequest{Number: 7, BaseBranch: freshOtherBranch}); err != nil {
		t.Fatal(err)
	}
	if !ptr.Deref(rr.Status.HoldsTarget, false) {
		t.Fatal("the lock was released without a verdict")
	}
	if log := strings.Join(*lines, "\n"); strings.Contains(log, "Released the target lock") {
		t.Fatalf("log %q says the lock was released; it is kept", log)
	}
}

// A refusal of the recovery met after the request moved on (it no longer recovers its handoff, read live) aborts
// nothing, and the refusal is logged rather than dropped.
func TestAbortOnRefusalLogsARefusalMetAfterTheRequestMovedOn(t *testing.T) {
	r, _, _, rr, _ := boundWorld(t)
	ctx, lines := loggedCtx()
	stored := rr.DeepCopy()
	stored.Status.Phase = api.PhaseNeedsIntervention
	if err := r.Status().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	refused := fmt.Errorf("status 403: %w", providers.ErrForbidden)
	want := recoveryUnreadable(rr, refused)
	res, err := r.abortOnRefusal(ctx, rr, want, refused)
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("abortOnRefusal = %+v, %v; want a requeue", res, err)
	}
	if log := strings.Join(*lines, "\n"); !strings.Contains(log, "moved on") || !strings.Contains(log, "403") {
		t.Fatalf("log %q does not record the refusal met after the request moved on", log)
	}
}
