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

package freshness_test

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

const (
	scaledObjectPath = "apps/orders/scaledobject.yaml"
	scaledObjectUID  = "22222222-2222-4222-8222-222222222222"
	mainBranch       = "main"
	kindScaledObject = "ScaledObject"
)

var scaledObjectFile = []byte(`apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: orders-worker
  namespace: orders
spec:
  scaleTargetRef:
    name: orders-worker
  maxReplicaCount: 10
`)

// kedaWorld is a fixed AdjustKedaMaxReplicas plan (10 to 15) on a ScaledObject with the given conditions.
type kedaWorld struct {
	rr       *v1alpha1.RemediationRequest
	pol      *v1alpha1.RemediationPolicy
	settings *v1alpha1.RemediationSettings
	target   *unstructured.Unstructured
	git      *fakegit.Git
	getErr   error
	// soReads counts the reads of the ScaledObject; afterGet runs on every object read, to change what it
	// returns.
	soReads  int
	afterGet func(obj client.Object)
}

func newKedaWorld(t *testing.T, conditions ...map[string]any) *kedaWorld {
	t.Helper()
	w := &kedaWorld{
		pol:      fx.RemediationPolicy("keda-capacity"),
		settings: fx.RemediationSettings(),
		git:      fakegit.NewGit(mainBranch, map[string][]byte{scaledObjectPath: scaledObjectFile}),
	}
	w.pol.UID, w.pol.Generation = policyUID, 3
	w.settings.Spec.Repositories[0].TargetBranch = mainBranch
	w.settings.Spec.Modules = &v1alpha1.Modules{Integrations: []v1alpha1.ModuleName{"keda"}}
	w.rr = fx.RemediationRequest("default", "rr")
	w.rr.UID = "55555555-5555-4555-8555-555555555555"
	w.rr.Spec.PolicyRef = "keda-capacity"
	conds := make([]any, 0, len(conditions))
	for _, c := range conditions {
		conds = append(conds, c)
	}
	w.target = &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "keda.sh/v1alpha1", "kind": kindScaledObject,
		"metadata": map[string]any{"namespace": fx.TargetNamespace, "name": fx.WorkloadName, "uid": scaledObjectUID},
		"spec":     map[string]any{"maxReplicaCount": int64(10)},
		"status":   map[string]any{"conditions": conds},
	}}
	src := &v1alpha1.ResolvedSource{RepositoryRef: fx.RepositoryName, TargetBranch: mainBranch, Path: scaledObjectPath,
		BuildPath: "apps/orders", BaseCommit: "0123456789abcdef0123456789abcdef01234567"}
	digest, err := freshness.ComputeDigest(w.rr, src, w.pol, w.settings)
	if err != nil {
		t.Fatal(err)
	}
	w.rr.Status = v1alpha1.RemediationRequestStatus{
		Phase: v1alpha1.PhasePRCreated, Source: src, PlanDigest: digest,
		PolicyUID: policyUID, PolicyRevision: freshness.RevisionOf(w.pol),
	}
	return w
}

func (w *kedaWorld) check(t *testing.T) error {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	soGVK := schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: kindScaledObject}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(w.pol, w.settings, w.target).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				isSO := obj.GetObjectKind().GroupVersionKind() == soGVK
				if w.getErr != nil && isSO {
					return w.getErr
				}
				if isSO {
					w.soReads++
				}
				err := cl.Get(ctx, key, obj, opts...)
				if err == nil && w.afterGet != nil {
					w.afterGet(obj)
				}
				return err
			},
		}).Build()
	resolve := resolverFunc(func(context.Context, sourcemap.Input) (sourcemap.Result, error) {
		moved := *w.rr.Status.Source
		moved.BaseCommit, _ = w.git.BranchHead(context.Background(), mainBranch)
		return sourcemap.Result{Outcome: sourcemap.Resolved, Reason: sourcemap.ReasonResolved, Source: &moved}, nil
	})
	open := func(context.Context, v1alpha1.GitRepository) (providers.GitProvider, error) { return w.git, nil }
	_, err := freshness.Checker{Reader: c, OpenRepository: open, Source: resolve}.Check(context.Background(), w.rr)
	return err
}

func condition(typ, status string) map[string]any {
	return map[string]any{"type": typ, "status": status}
}

func readyScaledObject() []map[string]any {
	return []map[string]any{condition("Ready", "True"), condition("Active", "True"), condition("Fallback", "False"),
		condition("Paused", "False"), condition("HPAActive", "True")}
}

// A ScaledObject that loses its readiness after the decision makes an AdjustKedaMaxReplicas plan stale
// (#228): the freshness check before every write repeats the policy's readiness rule, so a trigger that
// breaks after approval supersedes the plan and closes its pull request instead of raising the ceiling.
func TestKedaReadinessLostMakesThePlanStale(t *testing.T) {
	if err := newKedaWorld(t, readyScaledObject()...).check(t); err != nil {
		t.Fatalf("ready ScaledObject: want fresh, got %v", err)
	}
	cases := []struct {
		name  string
		conds []map[string]any
	}{
		{"trigger error", []map[string]any{condition("Ready", "False"), condition("Active", "False"),
			condition("Fallback", "False"), condition("Paused", "False")}},
		{"on fallback", []map[string]any{condition("Ready", "True"), condition("Fallback", "True")}},
		{"paused for scale-in", []map[string]any{condition("Ready", "True"), condition("Paused", "True")}},
		{"HPA cannot read its metrics", []map[string]any{condition("Ready", "True"), condition("HPAActive", "False")}},
		{"no conditions", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantStale(t, newKedaWorld(t, tc.conds...).check(t), freshness.ReasonScaledObjectNotReady)
		})
	}
}

// A failed read of the ScaledObject is not fresh and not stale: the write waits and the check is retried.
// A ScaledObject that is gone is stale (TargetChanged), never fresh.
func TestKedaReadErrorIsRetried(t *testing.T) {
	w := newKedaWorld(t, readyScaledObject()...)
	w.getErr = apierrors.NewServiceUnavailable("etcd is down")
	err := w.check(t)
	if err == nil {
		t.Fatal("want an error, got fresh")
	}
	if _, ok := errors.AsType[*freshness.StaleError](err); ok {
		t.Fatalf("a read error is not stale: %v", err)
	}
	w = newKedaWorld(t, readyScaledObject()...)
	w.getErr = apierrors.NewNotFound(schema.GroupResource{Group: "keda.sh", Resource: "scaledobjects"}, fx.WorkloadName)
	wantStale(t, w.check(t), freshness.ReasonTargetChanged)
}

func setConditions(obj client.Object, conds ...map[string]any) {
	list := make([]any, 0, len(conds))
	for _, c := range conds {
		list = append(list, c)
	}
	obj.(*unstructured.Unstructured).Object["status"] = map[string]any{"conditions": list}
}

func triggerError() []map[string]any {
	return []map[string]any{condition("Ready", "False"), condition("Active", "False")}
}

// One blip never closes an approved plan (#228): a ScaledObject read not ready once and ready on the second
// read is a retry, not stale and not fresh.
func TestKedaNotReadyOnceIsRetried(t *testing.T) {
	w := newKedaWorld(t, readyScaledObject()...)
	w.afterGet = func(obj client.Object) {
		if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == kindScaledObject && w.soReads == 1 {
			setConditions(obj, triggerError()...)
		}
	}
	err := w.check(t)
	if err == nil {
		t.Fatal("want a retry, got fresh")
	}
	if _, ok := errors.AsType[*freshness.StaleError](err); ok {
		t.Fatalf("one not-ready read made the plan stale: %v", err)
	}
	if w.soReads < 2 {
		t.Fatalf("the ScaledObject was read %d times, want it read again before a verdict", w.soReads)
	}
}

// A pause of writes that starts while a not-ready ScaledObject is confirmed wins: nothing is called stale, so
// nothing is closed while writes are paused.
func TestKedaNotReadyConfirmationHonoursAPause(t *testing.T) {
	w := newKedaWorld(t, triggerError()...)
	w.afterGet = func(obj client.Object) {
		if s, ok := obj.(*v1alpha1.RemediationSettings); ok && w.soReads >= 2 {
			s.Spec.Paused = new(true)
		}
	}
	if err := w.check(t); !errors.Is(err, freshness.ErrPaused) {
		t.Fatalf("want ErrPaused, got %v", err)
	}
}

// The previous value is compared before readiness, so a changed maxReplicaCount reports TargetChanged even
// when the ScaledObject is not ready either.
func TestKedaChangedValueBeforeReadiness(t *testing.T) {
	w := newKedaWorld(t, triggerError()...)
	w.target.Object["spec"] = map[string]any{"maxReplicaCount": int64(12)}
	wantStale(t, w.check(t), freshness.ReasonTargetChanged)
}

// A pause annotation or a deletion after approval makes the plan stale too.
func TestKedaPausedOrDeletedAfterApproval(t *testing.T) {
	w := newKedaWorld(t, readyScaledObject()...)
	w.target.SetAnnotations(map[string]string{"autoscaling.keda.sh/paused-scale-out": "true"})
	wantStale(t, w.check(t), freshness.ReasonScaledObjectNotReady)

	w = newKedaWorld(t, readyScaledObject()...)
	w.afterGet = func(obj client.Object) {
		if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == kindScaledObject {
			u.Object["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-03T10:00:00Z"
		}
	}
	wantStale(t, w.check(t), freshness.ReasonScaledObjectNotReady)
}

// Disabling the keda integration after approval withdraws the action: the plan is stale (ModuleNotEnabled).
func TestKedaModuleDisabledAfterApproval(t *testing.T) {
	for name, modules := range map[string]*v1alpha1.Modules{
		"no modules":   nil,
		"keda removed": {Integrations: []v1alpha1.ModuleName{"flux"}},
	} {
		t.Run(name, func(t *testing.T) {
			w := newKedaWorld(t, readyScaledObject()...)
			w.settings.Spec.Modules = modules
			wantStale(t, w.check(t), freshness.ReasonModuleNotEnabled)
		})
	}
}
