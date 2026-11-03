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
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// The fleet scenario broken-keda-trigger (shadewright-fleet scenarios/broken-keda-trigger): the ScaledObject
// demo/worker points its Prometheus trigger at an address that does not resolve, so KEDA reports Ready=False
// with TriggerError. Its expected outcome is a refusal in NeedsIntervention.
const (
	scenarioNamespace = "demo"
	scenarioName      = "worker"
	scenarioUID       = "3c1f5a7e-2b4d-4e6f-8a9b-0c1d2e3f4a5b"
	scaledObjectPath  = "apps/demo/worker-scaledobject.yaml"
	scenarioBuildPath = "apps/demo"

	kindScaledObject = "ScaledObject"
	keySpec          = "spec"
	kedaAPIVersion   = "keda.sh/v1alpha1"
	condTrue         = "True"
	condFalse        = "False"
)

// scaledObjectManifest is the ScaledObject as declared in Git, with the given maxReplicaCount.
func scaledObjectManifest(maxReplicas string) []byte {
	return []byte(`apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: worker
  namespace: demo
spec:
  scaleTargetRef:
    name: worker
  minReplicaCount: 1
  maxReplicaCount: ` + maxReplicas + `
`)
}

// triggerErrorConditions are the conditions KEDA v2.21.0 writes for the scenario: no trigger is active and the
// Prometheus trigger fails, with no fallback set.
func triggerErrorConditions() []any {
	return []any{
		kcond("Ready", condFalse, "TriggerError"),
		kcond("Active", condFalse, "ScalerNotActive"),
		kcond("Fallback", condFalse, "NoFallbackFound"),
		kcond("Paused", condFalse, ""),
	}
}

func readyConditions() []any {
	return []any{
		kcond("Ready", condTrue, "ScaledObjectReady"),
		kcond("Active", condTrue, "ScalerActive"),
		kcond("Fallback", condFalse, "NoFallbackFound"),
		kcond("Paused", condFalse, ""),
		kcond("HPAActive", condTrue, "HPAActive"),
	}
}

// kcond is a status condition as decoded from JSON; an empty reason is left out.
func kcond(typ, status, reason string) map[string]any {
	c := map[string]any{"type": typ, "status": status}
	if reason != "" {
		c["reason"] = reason
	}
	return c
}

// kedaWorld is a Pending request to raise the scenario's maxReplicaCount from 3 to 5, under a policy that
// allows it, on a ScaledObject with the given conditions. The source mapping resolves to the scenario file.
type kedaWorld struct {
	c       client.Client
	rr      *remediationv1alpha1.RemediationRequest
	so      *unstructured.Unstructured
	git     *fakegit.Git
	getErr  error
	closes  int
	request reconcile.Request
	// now is the reconciler's clock; zero means time.Now.
	now time.Time
}

func newKedaWorld(t *testing.T, conditions []any) *kedaWorld {
	t.Helper()
	w := &kedaWorld{}
	w.so = &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kedaAPIVersion, "kind": kindScaledObject,
		"metadata": map[string]any{"namespace": scenarioNamespace, "name": scenarioName, "uid": scenarioUID},
		keySpec:    map[string]any{"minReplicaCount": int64(1), "maxReplicaCount": int64(3)},
		"status":   map[string]any{"conditions": conditions},
	}}
	pol := fx.RemediationPolicy("keda-capacity")
	pol.Spec.RequestNamespaces = []remediationv1alpha1.Name{requestNamespace}
	pol.Spec.Targets.Namespaces = []remediationv1alpha1.Name{scenarioNamespace}
	pol.Spec.Actions[0].AdjustKedaMaxReplicas = &remediationv1alpha1.KedaMaxReplicasBounds{Min: 1, Max: 10, MaxIncreasePercent: 100}
	pol.Spec.GitOps.AllowedPaths = []remediationv1alpha1.GitPathPattern{appsGlob}
	settings := fx.RemediationSettings()
	settings.Spec.Repositories[0].TargetBranch = mainBranch // the API server's default; the fake client sets none
	settings.Spec.Modules = &remediationv1alpha1.Modules{Integrations: []remediationv1alpha1.ModuleName{"keda"}}
	w.rr = fx.RemediationRequest(requestNamespace, "rr")
	w.rr.UID = "55555555-5555-4555-8555-555555555555"
	w.rr.Spec.TargetRef = remediationv1alpha1.ObjectRef{APIVersion: kedaAPIVersion, Kind: kindScaledObject,
		Namespace: scenarioNamespace, Name: scenarioName, UID: scenarioUID}
	w.rr.Spec.Action.AdjustKedaMaxReplicas = &remediationv1alpha1.AdjustKedaMaxReplicas{PreviousValue: 3, DesiredValue: 5}
	w.request = reconcile.Request{NamespacedName: client.ObjectKeyFromObject(w.rr)}

	w.c = clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).WithObjects(w.so, pol, settings, w.rr).
		WithStatusSubresource(&remediationv1alpha1.RemediationRequest{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if w.getErr != nil && obj.GetObjectKind().GroupVersionKind().Group == "keda.sh" {
					return w.getErr
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	w.git = fakegit.NewGit(mainBranch, map[string][]byte{scaledObjectPath: scaledObjectManifest("3"),
		scenarioBuildPath + "/kustomization.yaml": []byte("resources:\n  - worker-scaledobject.yaml\n")})
	w.git.SetFail(func(op string) error {
		if op == opClose {
			w.closes++
		}
		return nil
	})
	return w
}

func (w *kedaWorld) reconciler() *RemediationRequestReconciler {
	return &RemediationRequestReconciler{
		Client: w.c, Scheme: w.c.Scheme(),
		Source: sourceFunc(func(ctx context.Context, _ sourcemap.Input) (sourcemap.Result, error) {
			head, _ := w.git.BranchHead(ctx, mainBranch)
			return sourcemap.Result{Outcome: sourcemap.Resolved, Reason: sourcemap.ReasonResolved,
				Source: &remediationv1alpha1.ResolvedSource{RepositoryRef: fx.RepositoryName, TargetBranch: mainBranch,
					Path: scaledObjectPath, BuildPath: scenarioBuildPath, BaseCommit: head}}, nil
		}),
		OpenRepository: func(context.Context, remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			return w.git, nil
		},
		Now: func() time.Time {
			if w.now.IsZero() {
				return time.Now()
			}
			return w.now
		},
	}
}

// reconcileResult is reconcile that also returns the result.
func (w *kedaWorld) reconcileResult(t *testing.T) (reconcile.Result, error) {
	t.Helper()
	return w.reconciler().Reconcile(context.Background(), w.request)
}

// setModules replaces the integrations RemediationSettings enables.
func (w *kedaWorld) setModules(t *testing.T, integrations ...remediationv1alpha1.ModuleName) {
	t.Helper()
	settings := &remediationv1alpha1.RemediationSettings{}
	if err := w.c.Get(context.Background(), client.ObjectKey{Name: remediationv1alpha1.SettingsName}, settings); err != nil {
		t.Fatal(err)
	}
	settings.Spec.Modules = &remediationv1alpha1.Modules{Integrations: integrations}
	if err := w.c.Update(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
}

func (w *kedaWorld) reconcile(t *testing.T) error {
	t.Helper()
	_, err := w.reconciler().Reconcile(context.Background(), w.request)
	return err
}

func (w *kedaWorld) fetch(t *testing.T) *remediationv1alpha1.RemediationRequest {
	t.Helper()
	got := &remediationv1alpha1.RemediationRequest{}
	if err := w.c.Get(context.Background(), w.request.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	return got
}

// setConditions replaces the live ScaledObject's conditions, as KEDA does when a trigger breaks.
func (w *kedaWorld) setConditions(t *testing.T, conditions []any) {
	t.Helper()
	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(w.so.GroupVersionKind())
	if err := w.c.Get(context.Background(), client.ObjectKeyFromObject(w.so), so); err != nil {
		t.Fatal(err)
	}
	so.Object["status"] = map[string]any{"conditions": conditions}
	if err := w.c.Update(context.Background(), so); err != nil {
		t.Fatal(err)
	}
}

// The broken-keda-trigger scenario's expected refusal (#228): a request to raise maxReplicaCount on a
// ScaledObject with a trigger error, within every bound of the policy, ends in NeedsIntervention with
// ScaledObjectNotReady, without a plan digest, and opens nothing. The same request on a ready ScaledObject is
// allowed, so the refusal comes from the readiness alone.
func TestBrokenKedaTriggerScenarioIsRefused(t *testing.T) {
	w := newKedaWorld(t, triggerErrorConditions())
	if err := w.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := w.fetch(t)
	if got.Status.Phase != remediationv1alpha1.PhaseNeedsIntervention {
		t.Fatalf("phase = %q, want NeedsIntervention", got.Status.Phase)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed)
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != string(policy.ReasonScaledObjectNotReady) {
		t.Fatalf("PolicyAllowed = %+v, want Unknown/%s", c, policy.ReasonScaledObjectNotReady)
	}
	if got.Status.PlanDigest != "" || got.Status.Deadline != nil {
		t.Fatalf("a refused request got a plan (%q) or a deadline (%v)", got.Status.PlanDigest, got.Status.Deadline)
	}
	// A decided request is not decided again, and nothing was written to Git.
	if err := w.reconcile(t); err != nil {
		t.Fatalf("Reconcile again: %v", err)
	}
	if again := w.fetch(t); again.ResourceVersion != got.ResourceVersion {
		t.Fatal("a refused request was written again")
	}
	if _, err := w.git.FindChangeRequest(context.Background(), freshness.BranchName(got)); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("a refused request has a change request: %v", err)
	}

	ready := newKedaWorld(t, readyConditions())
	if err := ready.reconcile(t); err != nil {
		t.Fatalf("Reconcile (ready): %v", err)
	}
	if got := ready.fetch(t); got.Status.Phase != remediationv1alpha1.PhasePolicyChecked {
		t.Fatalf("ready ScaledObject: phase = %q, want PolicyChecked", got.Status.Phase)
	}
}

// A failed read of the ScaledObject is a retry, never a pass: the request keeps no phase.
func TestKedaReadErrorIsRetried(t *testing.T) {
	w := newKedaWorld(t, readyConditions())
	w.getErr = apierrors.NewServiceUnavailable("unavailable")
	if err := w.reconcile(t); err == nil {
		t.Fatal("Reconcile: want an error to retry, got nil")
	}
	if got := w.fetch(t); got.Status.Phase != "" || len(got.Status.Conditions) != 0 {
		t.Fatalf("a read error decided the request: phase %q, conditions %v", got.Status.Phase, got.Status.Conditions)
	}
}

// Readiness lost after approval (#228): the trigger breaks while the pull request waits, so the freshness
// check before the next write finds the plan stale, closes the pull request and ends Superseded with
// PlanFresh ScaledObjectNotReady.
func TestKedaReadinessLostSupersedesThePlan(t *testing.T) {
	ctx := context.Background()
	w := newKedaWorld(t, readyConditions())
	if err := w.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := w.fetch(t)
	if got.Status.Phase != remediationv1alpha1.PhasePolicyChecked {
		t.Fatalf("phase = %q, want PolicyChecked", got.Status.Phase)
	}
	// The pull request step's part, played by hand: branch, commit, pull request.
	branch := freshness.BranchName(got)
	head, err := w.git.BranchHead(ctx, mainBranch)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.git.CreateBranch(ctx, branch, head); err != nil {
		t.Fatal(err)
	}
	if _, err := w.git.CommitFile(ctx, branch, head, providers.FileChange{Path: scaledObjectPath,
		Content: scaledObjectManifest("5")}); err != nil {
		t.Fatal(err)
	}
	cr, err := w.git.OpenChangeRequest(ctx, providers.NewChangeRequest{
		HeadBranch: branch, BaseBranch: mainBranch, Title: "t", Body: pullRequestBody(got)})
	if err != nil {
		t.Fatal(err)
	}
	got.Status.Phase = remediationv1alpha1.PhaseAwaitingApproval
	got.Status.PullRequest = &remediationv1alpha1.PullRequestStatus{Number: cr.Number, URL: cr.URL, HeadSHA: cr.HeadSHA}
	if err := w.c.Status().Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := w.reconciler().freshErr(ctx, w.fetch(t)); err != nil {
		t.Fatalf("ready ScaledObject: want a fresh plan, got %v", err)
	}

	w.setConditions(t, triggerErrorConditions())
	if err := w.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got = w.fetch(t)
	if got.Status.Phase != remediationv1alpha1.PhaseSuperseded {
		t.Fatalf("phase = %q, want Superseded", got.Status.Phase)
	}
	c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != freshness.ReasonScaledObjectNotReady {
		t.Fatalf("PlanFresh = %+v, want False/%s", c, freshness.ReasonScaledObjectNotReady)
	}
	now, err := w.git.FindChangeRequest(ctx, branch)
	if err != nil || now.State != providers.ChangeRequestClosed || w.closes != 1 {
		t.Fatalf("change request %+v (%v), %d closes; want it closed once", now, err, w.closes)
	}
}

// A cleanup cut short by a failed close finishes on the next reconcile, even if the cause is gone by then (the
// ScaledObject ready again, the module enabled again): ScaledObjectNotReady and ModuleNotEnabled are stale
// reasons the cleanup mark is recognised by.
func TestKedaStaleCleanupFinishesAfterAFailedClose(t *testing.T) {
	cases := []struct {
		name          string
		reason        string
		breakIt, undo func(w *kedaWorld)
	}{
		{"readiness lost", freshness.ReasonScaledObjectNotReady,
			func(w *kedaWorld) { w.setConditions(t, triggerErrorConditions()) },
			func(w *kedaWorld) { w.setConditions(t, readyConditions()) }},
		{"module disabled", freshness.ReasonModuleNotEnabled,
			func(w *kedaWorld) { w.setModules(t) },
			func(w *kedaWorld) { w.setModules(t, "keda") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newKedaWorld(t, readyConditions())
			if err := w.reconcile(t); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if got := w.fetch(t); got.Status.Phase != remediationv1alpha1.PhasePolicyChecked {
				t.Fatalf("phase = %q, want PolicyChecked", got.Status.Phase)
			}
			// The plan goes stale, and the Git host fails the cleanup's look-up of the change request once.
			failClose := true
			tc.breakIt(w)
			w.git.SetFail(func(op string) error {
				if failClose && (op == opClose || op == "FindChangeRequest") {
					return providers.ErrRetryable
				}
				return nil
			})
			if err := w.reconcile(t); err == nil {
				t.Fatal("Reconcile: want the failed cleanup to be retried, got nil")
			}
			got := w.fetch(t)
			c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
			if got.Status.Phase != remediationv1alpha1.PhasePolicyChecked || c == nil || c.Status != metav1.ConditionFalse ||
				c.Reason != tc.reason {
				t.Fatalf("phase %q, PlanFresh %+v; want the cleanup mark %s in PolicyChecked", got.Status.Phase, c, tc.reason)
			}

			failClose = false
			tc.undo(w)
			if err := w.reconcile(t); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if got := w.fetch(t); got.Status.Phase != remediationv1alpha1.PhaseSuperseded {
				t.Fatalf("phase = %q, want Superseded: the cleanup that started must finish", got.Status.Phase)
			}
		})
	}
}

// A ScaledObject KEDA has not reported on yet (no status) is waited for, not refused: the request keeps no
// phase, PolicyAllowed is Unknown with ScaledObjectReadinessUnknown, and the check is requeued with a growing
// delay, writing nothing while it waits. Once the ScaledObject is ready the request is decided as usual.
func TestKedaReadinessUnknownIsWaitedFor(t *testing.T) {
	w := newKedaWorld(t, readyConditions())
	start := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	w.now = start
	so := &unstructured.Unstructured{}
	so.SetGroupVersionKind(w.so.GroupVersionKind())
	if err := w.c.Get(context.Background(), client.ObjectKeyFromObject(w.so), so); err != nil {
		t.Fatal(err)
	}
	delete(so.Object, "status")
	if err := w.c.Update(context.Background(), so); err != nil {
		t.Fatal(err)
	}

	res, err := w.reconcileResult(t)
	if err != nil || res.RequeueAfter <= 0 {
		t.Fatalf("Reconcile = %+v, %v; want a requeue", res, err)
	}
	got := w.fetch(t)
	c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed)
	if got.Status.Phase != "" || got.Status.PlanDigest != "" || c == nil || c.Status != metav1.ConditionUnknown ||
		c.Reason != string(policy.ReasonScaledObjectReadinessUnknown) {
		t.Fatalf("phase %q, digest %q, PolicyAllowed %+v; want no phase and Unknown/%s", got.Status.Phase,
			got.Status.PlanDigest, c, policy.ReasonScaledObjectReadinessUnknown)
	}
	first := res.RequeueAfter

	// Waiting longer gives a longer delay and no new write.
	w.now = start.Add(time.Minute)
	res, err = w.reconcileResult(t)
	if err != nil || res.RequeueAfter <= first {
		t.Fatalf("Reconcile = %+v, %v; want a longer requeue than %v", res, err, first)
	}
	if again := w.fetch(t); again.ResourceVersion != got.ResourceVersion {
		t.Fatal("a waiting request was written again")
	}

	w.setConditions(t, readyConditions())
	if err := w.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := w.fetch(t); got.Status.Phase != remediationv1alpha1.PhasePolicyChecked {
		t.Fatalf("phase = %q, want PolicyChecked once the ScaledObject is ready", got.Status.Phase)
	}
}

// The wait is bounded: a ScaledObject whose readiness stays unknown for longer than TargetStateTimeout ends
// the request in NeedsIntervention (ScaledObjectNotReady).
func TestKedaReadinessUnknownTimesOut(t *testing.T) {
	w := newKedaWorld(t, []any{kcond("Ready", "Unknown", "")})
	start := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	w.now = start
	if res, err := w.reconcileResult(t); err != nil || res.RequeueAfter <= 0 {
		t.Fatalf("Reconcile = %+v, %v; want a requeue", res, err)
	}
	w.now = start.Add(DefaultTargetStateTimeout - time.Second)
	if res, err := w.reconcileResult(t); err != nil || res.RequeueAfter <= 0 || res.RequeueAfter > time.Second {
		t.Fatalf("Reconcile = %+v, %v; want a requeue at the end of the wait", res, err)
	}
	w.now = start.Add(DefaultTargetStateTimeout)
	if err := w.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := w.fetch(t)
	c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed)
	if got.Status.Phase != remediationv1alpha1.PhaseNeedsIntervention || c == nil ||
		c.Reason != string(policy.ReasonScaledObjectNotReady) || c.Message != messageTargetStateTimeout {
		t.Fatalf("phase %q, PolicyAllowed %+v; want NeedsIntervention/ScaledObjectNotReady after the wait", got.Status.Phase, c)
	}
}

// setPolicyAllowed writes a PolicyAllowed condition to the request's status, as an earlier reconcile left it.
func (w *kedaWorld) setPolicyAllowed(t *testing.T, c metav1.Condition) {
	t.Helper()
	rr := w.fetch(t)
	c.Type = remediationv1alpha1.ConditionPolicyAllowed
	rr.Status.Conditions = []metav1.Condition{c}
	if err := w.c.Status().Update(context.Background(), rr); err != nil {
		t.Fatal(err)
	}
}

// expectWaiting checks that the request still waits: a requeue, no phase, and PolicyAllowed Unknown with
// ScaledObjectReadinessUnknown whose lastTransitionTime is since.
func (w *kedaWorld) expectWaiting(t *testing.T, since time.Time) {
	t.Helper()
	if res, err := w.reconcileResult(t); err != nil || res.RequeueAfter <= 0 {
		t.Fatalf("Reconcile = %+v, %v; want a requeue", res, err)
	}
	got := w.fetch(t)
	c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed)
	if got.Status.Phase != "" || c == nil || c.Status != metav1.ConditionUnknown ||
		c.Reason != string(policy.ReasonScaledObjectReadinessUnknown) || !c.LastTransitionTime.Time.Equal(since) {
		t.Fatalf("phase %q, PolicyAllowed %+v; want no phase and Unknown/%s since %v", got.Status.Phase, c,
			policy.ReasonScaledObjectReadinessUnknown, since)
	}
}

// expectTimedOut checks that the reconcile ends the wait in NeedsIntervention (ScaledObjectNotReady).
func (w *kedaWorld) expectTimedOut(t *testing.T) {
	t.Helper()
	if err := w.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := w.fetch(t)
	c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed)
	if got.Status.Phase != remediationv1alpha1.PhaseNeedsIntervention || c == nil ||
		c.Reason != string(policy.ReasonScaledObjectNotReady) || c.Message != messageTargetStateTimeout {
		t.Fatalf("phase %q, PolicyAllowed %+v; want NeedsIntervention/ScaledObjectNotReady after the wait", got.Status.Phase, c)
	}
}

// An earlier Unknown PolicyAllowed with another reason does not shorten the wait (#232): the wait clock starts
// when ScaledObjectReadinessUnknown is first written, not at the old condition's lastTransitionTime, so the
// request still waits the full TargetStateTimeout and is never allowed meanwhile.
func TestKedaWaitClockStartsWithItsReason(t *testing.T) {
	w := newKedaWorld(t, []any{kcond("Ready", "Unknown", "")})
	start := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	w.setPolicyAllowed(t, metav1.Condition{Status: metav1.ConditionUnknown, Reason: "SomeEarlierWait",
		Message: "An earlier wait", ObservedGeneration: w.rr.Generation,
		LastTransitionTime: metav1.Time{Time: start.Add(-time.Hour)}})

	w.now = start
	w.expectWaiting(t, start)
	w.now = start.Add(time.Minute)
	w.expectWaiting(t, start)
	w.now = start.Add(DefaultTargetStateTimeout - time.Second)
	w.expectWaiting(t, start)
	w.now = start.Add(DefaultTargetStateTimeout)
	w.expectTimedOut(t)
}

// A wait that keeps its reason keeps its clock (#232), even when the condition is rewritten for a new message:
// otherwise the timeout would never fire. Here the wait started nine minutes ago with another message, and the
// request ends in NeedsIntervention once TargetStateTimeout has passed since then.
func TestKedaWaitClockKeepsItsStartAcrossReconciles(t *testing.T) {
	w := newKedaWorld(t, []any{kcond("Ready", "Unknown", "")})
	start := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	w.setPolicyAllowed(t, metav1.Condition{Status: metav1.ConditionUnknown,
		Reason: string(policy.ReasonScaledObjectReadinessUnknown), Message: "An older message",
		ObservedGeneration: w.rr.Generation, LastTransitionTime: metav1.Time{Time: start}})

	for _, after := range []time.Duration{9 * time.Minute, 9*time.Minute + 30*time.Second,
		DefaultTargetStateTimeout - time.Second} {
		w.now = start.Add(after)
		w.expectWaiting(t, start)
	}
	w.now = start.Add(DefaultTargetStateTimeout)
	w.expectTimedOut(t)
}

// AdjustKedaMaxReplicas needs the keda integration enabled in RemediationSettings spec.modules: without it the
// request is Denied (ModuleNotEnabled), and disabling it after approval supersedes the plan.
func TestKedaNeedsTheModule(t *testing.T) {
	w := newKedaWorld(t, readyConditions())
	w.setModules(t, "flux")
	if err := w.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := w.fetch(t)
	c := meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPolicyAllowed)
	if got.Status.Phase != remediationv1alpha1.PhaseDenied || c == nil || c.Reason != string(policy.ReasonModuleNotEnabled) {
		t.Fatalf("phase %q, PolicyAllowed %+v; want Denied/ModuleNotEnabled", got.Status.Phase, c)
	}

	w = newKedaWorld(t, readyConditions())
	if err := w.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := w.fetch(t); got.Status.Phase != remediationv1alpha1.PhasePolicyChecked {
		t.Fatalf("phase = %q, want PolicyChecked", got.Status.Phase)
	}
	w.setModules(t)
	if err := w.reconcile(t); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got = w.fetch(t)
	c = meta.FindStatusCondition(got.Status.Conditions, remediationv1alpha1.ConditionPlanFresh)
	if got.Status.Phase != remediationv1alpha1.PhaseSuperseded || c == nil || c.Reason != freshness.ReasonModuleNotEnabled {
		t.Fatalf("phase %q, PlanFresh %+v; want Superseded/ModuleNotEnabled", got.Status.Phase, c)
	}
}
