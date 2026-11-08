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

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fluxdelivery "github.com/pkalin/shadewright/internal/providers/delivery/flux"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
	"github.com/pkalin/shadewright/internal/verification/rollout"
)

const (
	rolloutNamespace = "demo"
	rolloutName      = "cache"
	rolloutUID       = "44444444-4444-4444-8444-444444444444"
	revisionNew      = "cache-7b9c"
)

// rolloutReader is the executor's API reader in these tests: the Kustomization comes from ks, everything else
// from the fake client. fail, when set, is asked before every read and may fail it.
type rolloutReader struct {
	client.Reader
	ks   *unstructured.Unstructured
	fail func(obj client.Object) error
}

func (r *rolloutReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.fail != nil {
		if err := r.fail(obj); err != nil {
			return err
		}
	}
	if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "Kustomization" {
		if r.ks == nil || r.ks.GetNamespace() != key.Namespace || r.ks.GetName() != key.Name {
			return fmt.Errorf("unexpected Kustomization %s", key)
		}
		r.ks.DeepCopyInto(u)
		return nil
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

// kustomizationApplied is the Kustomization that applies the cache StatefulSet, Ready, having applied commit
// from the main branch.
func kustomizationApplied(t *testing.T, commit string) *unstructured.Unstructured {
	t.Helper()
	rev := mainBranch + "@sha1:" + commit
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON([]byte(`{
  "apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
  "metadata": {"name": "apps", "namespace": "flux-system", "generation": 1},
  "spec": {"path": "./apps", "sourceRef": {"kind": "GitRepository", "name": "fleet"}},
  "status": {"observedGeneration": 1, "lastAppliedRevision": "` + rev + `", "lastAttemptedRevision": "` + rev + `",
    "conditions": [{"type": "Ready", "status": "True", "reason": "ReconciliationSucceeded", "observedGeneration": 1,
      "lastTransitionTime": "2026-11-08T10:36:00Z"}]}}`)); err != nil {
		t.Fatal(err)
	}
	return u
}

// stuckStatefulSet is the lab's statefulset-oom scenario after the fix was applied: cache-1 stays on the old
// revision, nothing is updated.
func stuckStatefulSet() *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: rolloutNamespace, Name: rolloutName, UID: rolloutUID, Generation: 3,
			Labels: map[string]string{
				"kustomize.toolkit.fluxcd.io/name":      "apps",
				"kustomize.toolkit.fluxcd.io/namespace": "flux-system",
			}},
		Spec: appsv1.StatefulSetSpec{
			Replicas:       ptr.To[int32](2),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 3, Replicas: 2, ReadyReplicas: 1, CurrentReplicas: 2, UpdatedReplicas: 0,
			CurrentRevision: "cache-6d4f", UpdateRevision: revisionNew,
		},
	}
}

func completeStatefulSet() *appsv1.StatefulSet {
	ss := stuckStatefulSet()
	ss.Status = appsv1.StatefulSetStatus{ObservedGeneration: 3, Replicas: 2, ReadyReplicas: 2, CurrentReplicas: 2,
		UpdatedReplicas: 2, CurrentRevision: revisionNew, UpdateRevision: revisionNew}
	return ss
}

type rolloutWorld struct {
	c      client.Client
	reader *rolloutReader
	git    *fakegit.Git
	rec    *record.FakeRecorder
	now    time.Time
	merged time.Time
	merge  string
	base   string // the commit before the merge
	r      *RemediationRequestReconciler
	key    client.ObjectKey
}

// newRolloutWorld is a request Merged at merged, whose merge commit is the head of main in git, and the target
// ss. The Kustomization has applied nothing yet; tests set reader.ks.
func newRolloutWorld(t *testing.T, ss *appsv1.StatefulSet) *rolloutWorld {
	t.Helper()
	git := fakegit.NewGit(mainBranch, map[string][]byte{"apps/cache.yaml": []byte("old")})
	base, err := git.BranchHead(t.Context(), mainBranch)
	if err != nil {
		t.Fatal(err)
	}
	merge, err := git.Push(mainBranch, "apps/cache.yaml", []byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	settings := fx.RemediationSettings()
	settings.Spec.Repositories[0].TargetBranch = mainBranch
	merged := time.Date(2026, 11, 8, 10, 35, 14, 0, time.UTC)
	rr := fx.MemoryLimitRequest(requestNamespace, "rr")
	rr.Spec.TargetRef = remediationv1alpha1.ObjectRef{APIVersion: "apps/v1", Kind: "StatefulSet",
		Namespace: rolloutNamespace, Name: rolloutName, UID: rolloutUID}
	rr.Status.Phase = remediationv1alpha1.PhaseMerged
	rr.Status.LastPhaseTransitionTime = new(metav1.NewMicroTime(merged))
	rr.Status.PlanDigest = "sha256:" + strings.Repeat("a", 64)
	rr.Status.HoldsTarget = new(false)
	rr.Status.BreakerCounts = new(true)
	rr.Status.Source = &remediationv1alpha1.ResolvedSource{RepositoryRef: fx.RepositoryName, TargetBranch: mainBranch,
		Path: "apps/cache.yaml", BuildPath: "apps", BaseCommit: strings.Repeat("b", 40)}
	rr.Status.PlanRepository = freshness.RepositoryOf(settings.Spec.Repositories[0])
	rr.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: 56,
		URL: "https://github.com/example/fleet/pull/56", HeadSHA: strings.Repeat("c", 40), MergeCommitSHA: merge}
	c := clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).WithObjects(settings, rr, ss).
		WithStatusSubresource(&remediationv1alpha1.RemediationRequest{}).Build()
	w := &rolloutWorld{c: c, reader: &rolloutReader{Reader: c}, git: git, rec: record.NewFakeRecorder(10),
		now: merged.Add(time.Minute), merged: merged, merge: merge, base: base, key: client.ObjectKeyFromObject(rr)}
	w.r = &RemediationRequestReconciler{Client: c, Scheme: c.Scheme(), APIReader: w.reader, Recorder: w.rec,
		Now: func() time.Time { return w.now },
		OpenRepository: func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			return git, nil
		},
		Delivery: func(a providers.AncestryChecker) (DeliveryVerifier, error) {
			return fluxdelivery.New(w.reader, a)
		},
	}
	return w
}

func (w *rolloutWorld) reconcile(t *testing.T, ctx context.Context) (reconcile.Result, error) {
	t.Helper()
	return w.r.Reconcile(ctx, reconcile.Request{NamespacedName: w.key})
}

func (w *rolloutWorld) request(t *testing.T) *remediationv1alpha1.RemediationRequest {
	t.Helper()
	rr := &remediationv1alpha1.RemediationRequest{}
	if err := w.c.Get(t.Context(), w.key, rr); err != nil {
		t.Fatal(err)
	}
	return rr
}

func (w *rolloutWorld) setTarget(t *testing.T, ss *appsv1.StatefulSet) {
	t.Helper()
	live := &appsv1.StatefulSet{}
	if err := w.c.Get(t.Context(), client.ObjectKeyFromObject(ss), live); err != nil {
		t.Fatal(err)
	}
	live.Status = ss.Status
	if err := w.c.Status().Update(t.Context(), live); err != nil {
		t.Fatal(err)
	}
}

func (w *rolloutWorld) events() []string {
	var out []string
	for {
		select {
		case e := <-w.rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// The lab case of #532: the merge commit is applied, cache-1 stays on the old revision, and after the deadline
// the request says RolledOut False, RolloutStuck, naming cache-1, with one Event. Once the pod is replaced the
// condition turns True.
func TestRolloutStuckAfterMergeIsReported(t *testing.T) {
	w := newRolloutWorld(t, stuckStatefulSet())
	w.reader.ks = kustomizationApplied(t, w.merge)

	res, err := w.reconcile(t, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rr := w.request(t)
	if got := rr.Status.PullRequest.AppliedObservedAt; got == nil || !got.Time.Equal(w.now) {
		t.Fatalf("appliedObservedAt = %v, want %v", got, w.now)
	}
	c := meta.FindStatusCondition(rr.Status.Conditions, rollout.ConditionRolledOut)
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != rollout.ReasonRolloutProgressing {
		t.Fatalf("RolledOut = %+v, want Unknown/RolloutProgressing", c)
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("a rollout in progress is looked at again, got %+v", res)
	}
	if ev := w.events(); len(ev) != 0 {
		t.Fatalf("Events before the deadline: %v", ev)
	}

	applied := w.now
	w.now = applied.Add(rollout.DefaultDeadline + time.Second)
	if _, err := w.reconcile(t, t.Context()); err != nil {
		t.Fatal(err)
	}
	rr = w.request(t)
	c = meta.FindStatusCondition(rr.Status.Conditions, rollout.ConditionRolledOut)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != rollout.ReasonRolloutStuck ||
		!strings.Contains(c.Message, "cache-1") {
		t.Fatalf("RolledOut = %+v, want False/RolloutStuck naming cache-1", c)
	}
	if !rr.Status.PullRequest.AppliedObservedAt.Time.Equal(applied) {
		t.Fatalf("appliedObservedAt moved to %v", rr.Status.PullRequest.AppliedObservedAt)
	}
	ev := w.events()
	if len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning RolloutStuck ") || !strings.Contains(ev[0], "cache-1") {
		t.Fatalf("Events = %v, want one Warning RolloutStuck naming cache-1", ev)
	}

	w.now = w.now.Add(stuckPollInterval)
	if _, err := w.reconcile(t, t.Context()); err != nil {
		t.Fatal(err)
	}
	if ev := w.events(); len(ev) != 0 {
		t.Fatalf("a second Event was recorded: %v", ev)
	}

	w.setTarget(t, completeStatefulSet())
	res, err = w.reconcile(t, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	c = meta.FindStatusCondition(w.request(t).Status.Conditions, rollout.ConditionRolledOut)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != rollout.ReasonRolloutComplete {
		t.Fatalf("RolledOut = %+v, want True/RolloutComplete", c)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("a complete rollout is not looked at again, got %+v", res)
	}
}

// A rollout that completes is True, and nothing is reported.
func TestRolloutCompleteAfterMerge(t *testing.T) {
	w := newRolloutWorld(t, completeStatefulSet())
	w.reader.ks = kustomizationApplied(t, w.merge)
	if _, err := w.reconcile(t, t.Context()); err != nil {
		t.Fatal(err)
	}
	c := meta.FindStatusCondition(w.request(t).Status.Conditions, rollout.ConditionRolledOut)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("RolledOut = %+v, want True", c)
	}
	if ev := w.events(); len(ev) != 0 {
		t.Fatalf("Events = %v, want none", ev)
	}
}

// Flux applied a later commit of main that holds the merge commit: the Git host's answer, through the kill
// switch's wrapper of the provider, counts it as delivered.
func TestRolloutLaterCommitCountsAsDelivered(t *testing.T) {
	w := newRolloutWorld(t, completeStatefulSet())
	later, err := w.git.Push(mainBranch, "apps/other.yaml", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	w.reader.ks = kustomizationApplied(t, later)
	if _, err := w.reconcile(t, t.Context()); err != nil {
		t.Fatal(err)
	}
	rr := w.request(t)
	if rr.Status.PullRequest.AppliedObservedAt == nil ||
		!meta.IsStatusConditionTrue(rr.Status.Conditions, rollout.ConditionRolledOut) {
		t.Fatalf("a later commit holding the merge commit was not counted: %+v", rr.Status)
	}
}

// A delivery not seen yet (Flux still applies the commit before the merge) records nothing and is looked at
// again.
func TestRolloutWaitsForDelivery(t *testing.T) {
	w := newRolloutWorld(t, completeStatefulSet())
	before, err := w.git.BranchHead(t.Context(), mainBranch)
	if err != nil {
		t.Fatal(err)
	}
	w.merge, err = w.git.Push(mainBranch, "apps/cache.yaml", []byte("newer"))
	if err != nil {
		t.Fatal(err)
	}
	rr := w.request(t)
	rr.Status.PullRequest.MergeCommitSHA = w.merge
	if err := w.c.Status().Update(t.Context(), rr); err != nil {
		t.Fatal(err)
	}
	w.reader.ks = kustomizationApplied(t, before)
	res, err := w.reconcile(t, t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rr = w.request(t)
	if rr.Status.PullRequest.AppliedObservedAt != nil || meta.FindStatusCondition(rr.Status.Conditions,
		rollout.ConditionRolledOut) != nil {
		t.Fatalf("recorded a delivery that was not seen: %+v", rr.Status)
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("the delivery is looked at again, got %+v", res)
	}
}

// A delivery not seen applied within the window is reported once, naming the Kustomization and the merge
// commit, and the check stops.
func TestRolloutDeliveryNotObservedIsReported(t *testing.T) {
	w := newRolloutWorld(t, stuckStatefulSet())
	w.reader.ks = kustomizationApplied(t, w.base) // the commit before the merge, never the merge commit
	w.now = w.merged.Add(deliveryWindow + time.Second)
	res, err := w.reconcile(t, t.Context())
	if err != nil || res.RequeueAfter != 0 {
		t.Fatalf("Reconcile = %+v, %v; want done", res, err)
	}
	rr := w.request(t)
	c := meta.FindStatusCondition(rr.Status.Conditions, rollout.ConditionRolledOut)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != rollout.ReasonDeliveryNotObserved ||
		!strings.Contains(c.Message, "flux-system/apps") || !strings.Contains(c.Message, w.merge) {
		t.Fatalf("RolledOut = %+v, want False/DeliveryNotObserved naming the Kustomization and the merge commit", c)
	}
	if rr.Status.PullRequest.AppliedObservedAt != nil {
		t.Fatal("an applied time was recorded")
	}
	ev := w.events()
	if len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning DeliveryNotObserved ") {
		t.Fatalf("Events = %v, want one Warning DeliveryNotObserved", ev)
	}
	// Stopped: no more reads, no second Event.
	reads := 0
	w.reader.fail = func(client.Object) error { reads++; return nil }
	w.now = w.now.Add(time.Hour)
	if res, err := w.reconcile(t, t.Context()); err != nil || res.RequeueAfter != 0 {
		t.Fatalf("Reconcile = %+v, %v; want done", res, err)
	}
	if reads != 0 || len(w.events()) != 0 {
		t.Fatalf("%d reads and more Events after the report", reads)
	}
}

// A target gone at the end of the window is reported too, not retried without end.
func TestRolloutDeliveryNotObservedTargetGone(t *testing.T) {
	w := newRolloutWorld(t, stuckStatefulSet())
	if err := w.c.Delete(t.Context(), stuckStatefulSet()); err != nil {
		t.Fatal(err)
	}
	w.now = w.merged.Add(deliveryWindow + time.Second)
	if res, err := w.reconcile(t, t.Context()); err != nil || res.RequeueAfter != 0 {
		t.Fatalf("Reconcile = %+v, %v; want done", res, err)
	}
	c := meta.FindStatusCondition(w.request(t).Status.Conditions, rollout.ConditionRolledOut)
	if c == nil || c.Reason != rollout.ReasonDeliveryNotObserved || !strings.Contains(c.Message, "gone") {
		t.Fatalf("RolledOut = %+v, want DeliveryNotObserved for a gone target", c)
	}
}

// A target kind the check does not cover is said once, as Unknown, with no Event and no read.
func TestRolloutKindNotChecked(t *testing.T) {
	w := newRolloutWorld(t, stuckStatefulSet())
	rr := w.request(t)
	rr.Spec.TargetRef = remediationv1alpha1.ObjectRef{APIVersion: kedaAPIVersion, Kind: kindScaledObject,
		Namespace: rolloutNamespace, Name: rolloutName}
	if err := w.c.Update(t.Context(), rr); err != nil {
		t.Fatal(err)
	}
	reads := 0
	w.reader.fail = func(client.Object) error { reads++; return nil }
	for range 2 {
		if res, err := w.reconcile(t, t.Context()); err != nil || res.RequeueAfter != 0 {
			t.Fatalf("Reconcile = %+v, %v; want done", res, err)
		}
	}
	c := meta.FindStatusCondition(w.request(t).Status.Conditions, rollout.ConditionRolledOut)
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != rollout.ReasonKindNotChecked ||
		!strings.Contains(c.Message, kindScaledObject) {
		t.Fatalf("RolledOut = %+v, want Unknown/KindNotChecked", c)
	}
	if reads != 0 || len(w.events()) != 0 {
		t.Fatalf("%d reads, Events %v; want none", reads, w.events())
	}
}

// A read that fails is never a verdict: the request is retried and keeps what it had.
func TestRolloutReadFailureIsRetried(t *testing.T) {
	boom := errors.New("read failed")
	t.Run("Kustomization", func(t *testing.T) {
		w := newRolloutWorld(t, stuckStatefulSet())
		w.reader.ks = kustomizationApplied(t, w.merge)
		w.reader.fail = func(obj client.Object) error {
			if _, ok := obj.(*unstructured.Unstructured); ok {
				return boom
			}
			return nil
		}
		w.now = w.merged.Add(30 * time.Minute)
		if _, err := w.reconcile(t, t.Context()); err == nil {
			t.Fatal("a failed read of the Kustomization was not returned")
		}
		rr := w.request(t)
		if rr.Status.PullRequest.AppliedObservedAt != nil ||
			meta.FindStatusCondition(rr.Status.Conditions, rollout.ConditionRolledOut) != nil {
			t.Fatalf("a failed read wrote a result: %+v", rr.Status)
		}
	})
	t.Run("target after the delivery", func(t *testing.T) {
		w := newRolloutWorld(t, stuckStatefulSet())
		w.reader.ks = kustomizationApplied(t, w.merge)
		targetReads := 0
		w.reader.fail = func(obj client.Object) error {
			if _, ok := obj.(*appsv1.StatefulSet); ok {
				if targetReads++; targetReads > 1 {
					return boom
				}
			}
			return nil
		}
		w.now = w.merged.Add(30 * time.Minute) // a verdict would be Stuck, were the read not failing
		if _, err := w.reconcile(t, t.Context()); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the read error", err)
		}
		rr := w.request(t)
		if rr.Status.PullRequest.AppliedObservedAt == nil {
			t.Fatal("the delivery seen was not kept")
		}
		if c := meta.FindStatusCondition(rr.Status.Conditions, rollout.ConditionRolledOut); c != nil {
			t.Fatalf("a failed read set %+v", c)
		}
		if ev := w.events(); len(ev) != 0 {
			t.Fatalf("Events = %v", ev)
		}
	})
}

// A cancelled reconcile is not a verdict and writes nothing.
func TestRolloutCancelledWritesNothing(t *testing.T) {
	w := newRolloutWorld(t, stuckStatefulSet())
	w.reader.ks = kustomizationApplied(t, w.merge)
	w.now = w.merged.Add(30 * time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	targetReads := 0
	w.reader.fail = func(obj client.Object) error {
		if _, ok := obj.(*appsv1.StatefulSet); ok {
			// Cancelled at the read of the rollout, after the delivery was seen applied.
			if targetReads++; targetReads > 1 {
				cancel()
			}
		}
		return nil
	}
	if _, err := w.reconcile(t, ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	rr := w.request(t)
	if rr.Status.PullRequest.AppliedObservedAt != nil ||
		meta.FindStatusCondition(rr.Status.Conditions, rollout.ConditionRolledOut) != nil {
		t.Fatalf("a cancelled reconcile wrote %+v", rr.Status)
	}
}

// mutate changes the request's status as a previous reconcile would have written it.
func (w *rolloutWorld) mutate(t *testing.T, edit func(*remediationv1alpha1.RemediationRequest)) {
	t.Helper()
	rr := w.request(t)
	edit(rr)
	if err := w.c.Status().Update(t.Context(), rr); err != nil {
		t.Fatal(err)
	}
}

// appliedLongAgo is a request whose delivery was seen at applied, with RolledOut still in progress.
func (w *rolloutWorld) appliedLongAgo(t *testing.T, applied time.Time) {
	t.Helper()
	w.mutate(t, func(rr *remediationv1alpha1.RemediationRequest) {
		rr.Status.PullRequest.AppliedObservedAt = new(metav1.NewTime(applied))
		meta.SetStatusCondition(&rr.Status.Conditions, metav1.Condition{Type: rollout.ConditionRolledOut,
			Status: metav1.ConditionUnknown, Reason: rollout.ReasonRolloutProgressing, Message: "in progress"})
	})
}

func rolledOut(t *testing.T, w *rolloutWorld) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(w.request(t).Status.Conditions, rollout.ConditionRolledOut)
}

// Review of #532, point 1: a reconcile after the end of the check (a restart, a late wake-up) still ends with a
// verdict, never with Progressing left behind.
func TestRolloutLateReconcileEndsWithAVerdict(t *testing.T) {
	applied := time.Date(2026, 11, 8, 10, 36, 0, 0, time.UTC)
	end := applied.Add(rollout.DefaultDeadline + stuckWatch)
	t.Run("stuck", func(t *testing.T) {
		w := newRolloutWorld(t, stuckStatefulSet())
		w.appliedLongAgo(t, applied)
		w.now = end.Add(time.Hour)
		if res, err := w.reconcile(t, t.Context()); err != nil || res.RequeueAfter != 0 {
			t.Fatalf("Reconcile = %+v, %v; want done", res, err)
		}
		if c := rolledOut(t, w); c == nil || c.Reason != rollout.ReasonRolloutStuck {
			t.Fatalf("RolledOut = %+v, want RolloutStuck", c)
		}
		if ev := w.events(); len(ev) != 1 {
			t.Fatalf("Events = %v, want one", ev)
		}
		// Ended: nothing more is read.
		reads := 0
		w.reader.fail = func(client.Object) error { reads++; return nil }
		if _, err := w.reconcile(t, t.Context()); err != nil || reads != 0 {
			t.Fatalf("after the end: err %v, %d reads", err, reads)
		}
	})
	t.Run("target gone", func(t *testing.T) {
		w := newRolloutWorld(t, stuckStatefulSet())
		w.appliedLongAgo(t, applied)
		if err := w.c.Delete(t.Context(), stuckStatefulSet()); err != nil {
			t.Fatal(err)
		}
		w.now = end.Add(time.Minute)
		if _, err := w.reconcile(t, t.Context()); err != nil {
			t.Fatal(err)
		}
		if c := rolledOut(t, w); c == nil || c.Status != metav1.ConditionUnknown || c.Reason != rollout.ReasonRolloutNotDetermined {
			t.Fatalf("RolledOut = %+v, want Unknown/RolloutNotDetermined", c)
		}
		if ev := w.events(); len(ev) != 1 {
			t.Fatalf("Events = %v, want one", ev)
		}
	})
	t.Run("reads failing", func(t *testing.T) {
		w := newRolloutWorld(t, stuckStatefulSet())
		w.appliedLongAgo(t, applied)
		w.reader.fail = func(obj client.Object) error {
			if _, ok := obj.(*appsv1.StatefulSet); ok {
				return errors.New("read failed")
			}
			return nil
		}
		w.now = end.Add(time.Minute)
		if _, err := w.reconcile(t, t.Context()); err == nil {
			t.Fatal("a failed read within the grace period was not retried")
		}
		if c := rolledOut(t, w); c.Reason != rollout.ReasonRolloutProgressing {
			t.Fatalf("RolledOut = %+v before the grace ended", c)
		}
		w.now = end.Add(finalGrace + time.Second)
		if res, err := w.reconcile(t, t.Context()); err != nil || res.RequeueAfter != 0 {
			t.Fatalf("Reconcile = %+v, %v; want done", res, err)
		}
		if c := rolledOut(t, w); c.Reason != rollout.ReasonRolloutNotDetermined {
			t.Fatalf("RolledOut = %+v, want RolloutNotDetermined", c)
		}
	})
}

// Point 2: a delivery applied in time but first read after the window counts.
func TestRolloutDeliveryReadAfterTheWindowCounts(t *testing.T) {
	w := newRolloutWorld(t, completeStatefulSet())
	w.reader.ks = kustomizationApplied(t, w.merge)
	w.now = w.merged.Add(deliveryWindow + time.Minute)
	if _, err := w.reconcile(t, t.Context()); err != nil {
		t.Fatal(err)
	}
	if c := rolledOut(t, w); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("RolledOut = %+v, want True", c)
	}
}

// Point 3: while writes are paused the Git host is not asked, so the window does not end in DeliveryNotObserved.
func TestRolloutPausedDoesNotEndTheWindow(t *testing.T) {
	w := newRolloutWorld(t, completeStatefulSet())
	later, err := w.git.Push(mainBranch, "apps/other.yaml", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	w.reader.ks = kustomizationApplied(t, later)
	settings := &remediationv1alpha1.RemediationSettings{}
	if err := w.c.Get(t.Context(), client.ObjectKey{Name: remediationv1alpha1.SettingsName}, settings); err != nil {
		t.Fatal(err)
	}
	settings.Spec.Paused = new(true)
	if err := w.c.Update(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	w.now = w.merged.Add(deliveryWindow + time.Hour)
	res, err := w.reconcile(t, t.Context())
	if err != nil || res.RequeueAfter <= 0 {
		t.Fatalf("Reconcile = %+v, %v; want a wait", res, err)
	}
	if c := rolledOut(t, w); c != nil {
		t.Fatalf("RolledOut = %+v while paused, want none", c)
	}
	settings.Spec.Paused = new(false)
	if err := w.c.Update(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	if _, err := w.reconcile(t, t.Context()); err != nil {
		t.Fatal(err)
	}
	if c := rolledOut(t, w); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("RolledOut = %+v after the pause, want True", c)
	}
}

// Point 6: the revision applied with the merge commit is recorded and judged; a newer one ends the check as
// superseded, never Stuck.
func TestRolloutNewerRevisionSupersedes(t *testing.T) {
	w := newRolloutWorld(t, stuckStatefulSet())
	w.reader.ks = kustomizationApplied(t, w.merge)
	if _, err := w.reconcile(t, t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := w.request(t).Status.Rollout; got == nil || got.TemplateRevision != revisionNew || got.TargetGeneration != 3 {
		t.Fatalf("status.rollout = %+v, want revision %s at generation 3", got, revisionNew)
	}
	next := stuckStatefulSet()
	next.Status.UpdateRevision = "cache-8e1f"
	w.setTarget(t, next)
	w.now = w.now.Add(rollout.DefaultDeadline + time.Minute)
	if res, err := w.reconcile(t, t.Context()); err != nil || res.RequeueAfter != 0 {
		t.Fatalf("Reconcile = %+v, %v; want done", res, err)
	}
	if c := rolledOut(t, w); c == nil || c.Status != metav1.ConditionUnknown || c.Reason != rollout.ReasonRolloutSuperseded {
		t.Fatalf("RolledOut = %+v, want Unknown/RolloutSuperseded", c)
	}
}

// Point 8: without lastPhaseTransitionTime the check still runs, its window measured from a recorded start.
func TestRolloutWithoutTransitionTime(t *testing.T) {
	w := newRolloutWorld(t, completeStatefulSet())
	w.mutate(t, func(rr *remediationv1alpha1.RemediationRequest) { rr.Status.LastPhaseTransitionTime = nil })
	_, _ = w.reconcile(t, t.Context()) // no Kustomization yet: the read fails, the start is recorded first
	rr := w.request(t)
	if rr.Status.Rollout == nil || rr.Status.Rollout.WaitStartedAt == nil {
		t.Fatalf("status.rollout = %+v, want waitStartedAt", rr.Status.Rollout)
	}
	w.reader.ks = kustomizationApplied(t, w.merge)
	if _, err := w.reconcile(t, t.Context()); err != nil {
		t.Fatal(err)
	}
	if c := rolledOut(t, w); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("RolledOut = %+v, want True", c)
	}
}
