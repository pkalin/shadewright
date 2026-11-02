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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

// blockingResolver blocks in Resolve until its context ends, and reports that it started and how it ended.
type blockingResolver struct {
	started chan struct{}
	ended   chan error
}

func newBlockingResolver() *blockingResolver {
	return &blockingResolver{started: make(chan struct{}), ended: make(chan error, 1)}
}

func (b *blockingResolver) Resolve(ctx context.Context, _ sourcemap.Input) (sourcemap.Result, error) {
	close(b.started)
	<-ctx.Done()
	b.ended <- ctx.Err()
	return sourcemap.Result{}, ctx.Err()
}

func deadlineScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := remediationv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// deadlineWorld is a Pending request whose check reaches the source resolver.
func deadlineWorld(t *testing.T) (client.Client, *remediationv1alpha1.RemediationRequest) {
	t.Helper()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: fx.TargetNamespace, Name: appName, UID: "33333333-3333-4333-8333-333333333333"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: workerContainer, Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}}}}}}},
	}
	pol := fx.RemediationPolicy("mem")
	pol.Spec.Actions = []remediationv1alpha1.PolicyAction{fx.MemoryLimitPolicyAction()}
	rr := fx.MemoryLimitRequest(requestNamespace, "rr")
	rr.Spec.PolicyRef = "mem"
	rr.Spec.TargetRef.Name = dep.Name
	rr.Spec.TargetRef.UID = remediationv1alpha1.UID(dep.UID)
	c := clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).
		WithObjects(dep, pol, fx.RemediationSettings(), rr).
		WithStatusSubresource(&remediationv1alpha1.RemediationRequest{}).Build()
	return c, rr
}

func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return within 5s", what)
	}
}

// A resolver that never answers must not hold the worker: the check returns at its own deadline with a
// retryable error, the request stays undecided (no final phase), and the resolver's context was cancelled.
func TestPolicyCheckDeadlineIsRetryable(t *testing.T) {
	c, rr := deadlineWorld(t)
	src := newBlockingResolver()
	r := &RemediationRequestReconciler{Client: c, Scheme: c.Scheme(), Source: src, CheckTimeout: 50 * time.Millisecond}

	var err error
	within(t, "Reconcile", func() {
		_, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("a deadline is not a caller cancellation: %v", err)
	}
	if got := <-src.ended; !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("the resolver should have seen the deadline, got %v", got)
	}
	got := &remediationv1alpha1.RemediationRequest{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(rr), got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != "" {
		t.Fatalf("a timeout must not end in a phase, got %q", got.Status.Phase)
	}
}

// The caller's cancellation stays distinct from the phase deadline.
func TestPolicyCheckCallerCancellationIsDistinct(t *testing.T) {
	c, rr := deadlineWorld(t)
	src := newBlockingResolver()
	r := &RemediationRequestReconciler{Client: c, Scheme: c.Scheme(), Source: src, CheckTimeout: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-src.started; cancel() }()

	var err error
	within(t, "Reconcile", func() {
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(rr)})
	})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) {
		t.Fatalf("want context.Canceled and not ErrTimeout, got %v", err)
	}
}

// staleWorld is a request in PolicyChecked whose policy is gone, so the freshness check says stale at once and
// the cleanup (settings read, OpenRepository, FindChangeRequest, CloseChangeRequest) runs.
func staleWorld(t *testing.T) (client.Client, *remediationv1alpha1.RemediationRequest) {
	t.Helper()
	settings := fx.RemediationSettings()
	rr := fx.MemoryLimitRequest(requestNamespace, "rr")
	rr.Spec.PolicyRef = "gone"
	rr.Status.Phase = remediationv1alpha1.PhasePolicyChecked
	rr.Status.PlanDigest = "digest"
	rr.Status.Source = &remediationv1alpha1.ResolvedSource{RepositoryRef: fx.RepositoryName, TargetBranch: mainBranch,
		Path: sourcePath, BaseCommit: baseCommit}
	rr.Status.PlanRepository = freshness.RepositoryOf(settings.Spec.Repositories[0])
	c := clientfake.NewClientBuilder().WithScheme(deadlineScheme(t)).WithObjects(settings, rr).
		WithStatusSubresource(&remediationv1alpha1.RemediationRequest{}).Build()
	return c, rr
}

func TestCleanupDeadlineIsRetryable(t *testing.T) {
	c, rr := staleWorld(t)
	started := make(chan struct{})
	ended := make(chan error, 1)
	r := &RemediationRequestReconciler{Client: c, Scheme: c.Scheme(), Source: newBlockingResolver(),
		CleanupTimeout: 50 * time.Millisecond,
		OpenRepository: func(ctx context.Context, _ remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			close(started)
			<-ctx.Done()
			ended <- ctx.Err()
			return nil, ctx.Err()
		}}

	var err error
	within(t, "EnsureFresh", func() { err = r.EnsureFresh(context.Background(), rr) })
	if !errors.Is(err, ErrTimeout) || errors.Is(err, ErrSuperseded) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if got := <-ended; !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("the provider should have seen the deadline, got %v", got)
	}
	if rr.Status.Phase != remediationv1alpha1.PhasePolicyChecked {
		t.Fatalf("a timeout must not change the phase, got %q", rr.Status.Phase)
	}
}

func TestCleanupCallerCancellationIsDistinct(t *testing.T) {
	c, rr := staleWorld(t)
	started := make(chan struct{})
	r := &RemediationRequestReconciler{Client: c, Scheme: c.Scheme(), Source: newBlockingResolver(),
		CleanupTimeout: time.Minute,
		OpenRepository: func(ctx context.Context, _ remediationv1alpha1.GitRepository) (providers.GitProvider, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()

	var err error
	within(t, "EnsureFresh", func() { err = r.EnsureFresh(ctx, rr) })
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) {
		t.Fatalf("want context.Canceled and not ErrTimeout, got %v", err)
	}
}
