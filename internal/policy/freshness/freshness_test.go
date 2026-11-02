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
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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
	sourcePath  = "apps/orders/deployment.yaml"
	otherPath   = "docs/readme.yaml"
	otherBranch = "release"
	tierLabel   = "tier"
	targetUID   = "33333333-3333-4333-8333-333333333333"
	policyUID   = "44444444-4444-4444-8444-444444444444"
)

func manifest(limit string) []byte {
	return []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: orders-worker
  namespace: orders
spec:
  template:
    spec:
      containers:
        - name: worker
          resources:
            limits:
              memory: ` + limit + `
`)
}

// world is everything a check reads.
type world struct {
	rr       *v1alpha1.RemediationRequest
	pol      *v1alpha1.RemediationPolicy
	settings *v1alpha1.RemediationSettings
	target   *appsv1.Deployment
	git      *fakegit.Git
	getErr   func(key client.ObjectKey, obj client.Object) error
	// afterGet runs on every object read, to change what the next read sees.
	afterGet func(obj client.Object)
	// ctx is the context of the check; nil means Background.
	ctx  context.Context
	open freshness.RepositoryOpener
	// resolve is the source mapping run again before the write; newWorld maps to the recorded source.
	resolve func(ctx context.Context, in sourcemap.Input) (sourcemap.Result, error)
	// extra are more objects in the cluster, such as Flux objects.
	extra []client.Object
}

// resolverFunc adapts a function to freshness.SourceResolver.
type resolverFunc func(ctx context.Context, in sourcemap.Input) (sourcemap.Result, error)

func (f resolverFunc) Resolve(ctx context.Context, in sourcemap.Input) (sourcemap.Result, error) {
	return f(ctx, in)
}

// newWorld builds a fresh plan: the digest is computed from the objects as they are at the start.
func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{
		pol:      fx.RemediationPolicy("mem"),
		settings: fx.RemediationSettings(),
		target: &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Namespace: fx.TargetNamespace, Name: fx.WorkloadName, UID: targetUID,
			Labels: map[string]string{tierLabel: "web"}},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "worker", Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}}}}}}}},
		git: fakegit.NewGit("main", map[string][]byte{sourcePath: manifest("256Mi"), otherPath: []byte("a: 1\n")}),
	}
	w.pol.UID, w.pol.Generation = policyUID, 3
	w.pol.Spec.Targets.ObjectSelector = &metav1.LabelSelector{MatchLabels: map[string]string{tierLabel: "web"}}
	w.settings.Spec.Repositories[0].TargetBranch = "main"
	w.rr = fx.MemoryLimitRequest("default", "rr")
	w.rr.UID = "55555555-5555-4555-8555-555555555555"
	w.rr.Spec.PolicyRef = "mem"
	w.rr.Spec.TargetRef.UID = targetUID
	src := &v1alpha1.ResolvedSource{RepositoryRef: fx.RepositoryName, TargetBranch: "main", Path: sourcePath,
		BaseCommit: "0123456789abcdef0123456789abcdef01234567"}
	digest, err := freshness.ComputeDigest(w.rr, src, w.pol, w.settings)
	if err != nil {
		t.Fatal(err)
	}
	w.rr.Status = v1alpha1.RemediationRequestStatus{
		Phase: v1alpha1.PhasePolicyChecked, Source: src, PlanDigest: digest,
		PolicyUID: policyUID, PolicyRevision: freshness.RevisionOf(w.pol),
	}
	w.open = func(context.Context, v1alpha1.GitRepository) (providers.GitProvider, error) { return w.git, nil }
	w.resolve = func(context.Context, sourcemap.Input) (sourcemap.Result, error) {
		// A merge moved the base commit; the repository, branch and file are the recorded ones.
		moved := *src
		moved.BaseCommit, _ = w.git.BranchHead(context.Background(), "main")
		return sourcemap.Result{Outcome: sourcemap.Resolved, Reason: sourcemap.ReasonResolved, Source: &moved}, nil
	}
	return w
}

// fresh runs the check against the live state in w; objects set to nil are absent from the cluster.
func (w *world) fresh(t *testing.T) error {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	objs := append([]client.Object(nil), w.extra...)
	for _, o := range []client.Object{w.pol, w.settings, w.target} {
		if o != nil && !isNil(o) {
			objs = append(objs, o)
		}
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object,
			opts ...client.GetOption) error {
			if w.getErr != nil {
				if err := w.getErr(key, obj); err != nil {
					return err
				}
			}
			err := cl.Get(ctx, key, obj, opts...)
			if err == nil && w.afterGet != nil {
				w.afterGet(obj)
			}
			return err
		},
	}).Build()
	ctx := w.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	var src freshness.SourceResolver
	if w.resolve != nil {
		src = resolverFunc(w.resolve)
	}
	return freshness.Checker{Reader: c, OpenRepository: w.open, Source: src}.Fresh(ctx, w.rr)
}

func isNil(o client.Object) bool {
	switch v := o.(type) {
	case *v1alpha1.RemediationPolicy:
		return v == nil
	case *v1alpha1.RemediationSettings:
		return v == nil
	case *appsv1.Deployment:
		return v == nil
	}
	return false
}

func wantStale(t *testing.T, err error, reason string) {
	t.Helper()
	se, ok := errors.AsType[*freshness.StaleError](err)
	if !ok {
		t.Fatalf("want a StaleError %s, got %v", reason, err)
	}
	if reason != "" && se.Reason != reason {
		t.Fatalf("want reason %s, got %s (%s)", reason, se.Reason, se.Message)
	}
}

func TestFreshWhenNothingChanged(t *testing.T) {
	if err := newWorld(t).fresh(t); err != nil {
		t.Fatalf("want fresh, got %v", err)
	}
}

// Every input of the plan digest, changed after the plan was fixed, makes the plan stale.
func TestStalePerDigestInput(t *testing.T) {
	cases := []struct {
		name   string
		change func(w *world)
		reason string
	}{
		{"action desired value", func(w *world) {
			w.rr.Spec.Action.SetContainerMemoryLimit.Desired = *resourceQuantity("512Mi")
		}, freshness.ReasonPlanChanged},
		{"action previous value", func(w *world) {
			w.rr.Spec.Action.SetContainerMemoryLimit.Previous = *resourceQuantity("128Mi")
		}, freshness.ReasonPlanChanged},
		{"target UID in the request", func(w *world) {
			w.rr.Spec.TargetRef.UID = "66666666-6666-4666-8666-666666666666"
		}, freshness.ReasonPlanChanged},
		{"target recreated with another UID", func(w *world) { w.target.UID = "66666666-6666-4666-8666-666666666666" },
			freshness.ReasonTargetChanged},
		{"live limit changed, UID and Git unchanged", func(w *world) {
			w.target.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")
		}, freshness.ReasonTargetChanged},
		{"live limit removed", func(w *world) {
			w.target.Spec.Template.Spec.Containers[0].Resources.Limits = nil
		}, freshness.ReasonTargetChanged},
		{"target relabelled out of the policy selector", func(w *world) { w.target.Labels = map[string]string{tierLabel: "db"} },
			freshness.ReasonTargetChanged},
		{"target labels removed", func(w *world) { w.target.Labels = nil }, freshness.ReasonTargetChanged},
		{"target deleted", func(w *world) { w.target = nil }, freshness.ReasonTargetChanged},
		{"source repositoryRef", func(w *world) { w.rr.Status.Source.RepositoryRef = "other" }, freshness.ReasonSettingsChanged},
		{"source target branch", func(w *world) { w.rr.Status.Source.TargetBranch = otherBranch }, freshness.ReasonSettingsChanged},
		{"source path", func(w *world) { w.rr.Status.Source.Path = otherPath }, freshness.ReasonPlanChanged},
		{"source base commit", func(w *world) {
			w.rr.Status.Source.BaseCommit = "89abcdef0123456789abcdef0123456789abcdef"
		}, freshness.ReasonPlanChanged},
		{"repository URL in settings", func(w *world) {
			w.settings.Spec.Repositories[0].URL = "https://github.com/example/other"
		}, freshness.ReasonPlanChanged},
		{"installation in settings", func(w *world) { w.settings.Spec.Repositories[0].GitHub.InstallationID = 2 },
			freshness.ReasonPlanChanged},
		{"target branch in settings", func(w *world) { w.settings.Spec.Repositories[0].TargetBranch = otherBranch },
			freshness.ReasonSettingsChanged},
		{"repository removed from settings", func(w *world) { w.settings.Spec.Repositories = nil },
			freshness.ReasonSettingsChanged},
		{"settings deleted", func(w *world) { w.settings = nil }, freshness.ReasonSettingsChanged},
		{"policy UID (recreated)", func(w *world) { w.pol.UID = "77777777-7777-4777-8777-777777777777" },
			freshness.ReasonPolicyChanged},
		{"policy generation", func(w *world) { w.pol.Generation = 4 }, freshness.ReasonPolicyChanged},
		{"policy deleted", func(w *world) { w.pol = nil }, freshness.ReasonPolicyChanged},
		{"recorded digest", func(w *world) {
			w.rr.Status.PlanDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
		}, freshness.ReasonPlanChanged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			tc.change(w)
			wantStale(t, w.fresh(t), tc.reason)
		})
	}
}

func TestTargetFileOnTheTargetBranch(t *testing.T) {
	t.Run("a merge that does not touch the file does not supersede", func(t *testing.T) {
		w := newWorld(t)
		if _, err := w.git.Push("main", otherPath, []byte("a: 2\n")); err != nil {
			t.Fatal(err)
		}
		if err := w.fresh(t); err != nil {
			t.Fatalf("want fresh, got %v", err)
		}
	})
	t.Run("a change to another field of the file does not supersede", func(t *testing.T) {
		w := newWorld(t)
		data := append(manifest("256Mi"), []byte("---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n")...)
		if _, err := w.git.Push("main", sourcePath, data); err != nil {
			t.Fatal(err)
		}
		if err := w.fresh(t); err != nil {
			t.Fatalf("want fresh, got %v", err)
		}
	})
	t.Run("the same quantity written another way does not supersede", func(t *testing.T) {
		w := newWorld(t)
		if _, err := w.git.Push("main", sourcePath, manifest("268435456")); err != nil {
			t.Fatal(err)
		}
		if err := w.fresh(t); err != nil {
			t.Fatalf("want fresh, got %v", err)
		}
	})
	stale := map[string][]byte{
		"the value changed":                    manifest("512Mi"),
		"the target is no longer there":        []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"),
		"the file is not valid YAML":           []byte("a: [\n"),
		"the target is declared twice":         append(manifest("256Mi"), append([]byte("---\n"), manifest("256Mi")...)...),
		"the field is no longer set":           []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: orders-worker\n  namespace: orders\n"),
		"the target moved to a List":           []byte("apiVersion: v1\nkind: List\nitems: []\n"),
		"the target lost its namespace":        []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: orders-worker\n"),
		"the value is already the desired one": manifest("384Mi"),
	}
	for name, data := range stale {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			if _, err := w.git.Push("main", sourcePath, data); err != nil {
				t.Fatal(err)
			}
			wantStale(t, w.fresh(t), freshness.ReasonTargetFileChanged)
		})
	}
	t.Run("a change on the pull request branch alone is not looked at", func(t *testing.T) {
		w := newWorld(t)
		head, _ := w.git.BranchHead(context.Background(), "main")
		if err := w.git.CreateBranch(context.Background(), "shadewright/x", head); err != nil {
			t.Fatal(err)
		}
		if _, err := w.git.Push("shadewright/x", sourcePath, manifest("384Mi")); err != nil {
			t.Fatal(err)
		}
		if err := w.fresh(t); err != nil {
			t.Fatalf("want fresh, got %v", err)
		}
	})
}

// An input that cannot be read is never fresh, and is not a StaleError either: nothing is known about the plan.
func TestFailClosed(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name  string
		setup func(w *world)
	}{
		{"policy read fails", func(w *world) {
			w.getErr = func(k client.ObjectKey, o client.Object) error {
				if _, ok := o.(*v1alpha1.RemediationPolicy); ok {
					return boom
				}
				return nil
			}
		}},
		{"settings read fails", func(w *world) {
			w.getErr = func(k client.ObjectKey, o client.Object) error {
				if _, ok := o.(*v1alpha1.RemediationSettings); ok {
					return apierrors.NewServiceUnavailable("down")
				}
				return nil
			}
		}},
		{"target read fails", func(w *world) {
			w.getErr = func(k client.ObjectKey, o client.Object) error {
				if o.GetObjectKind().GroupVersionKind() == (schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}) {
					return boom
				}
				return nil
			}
		}},
		{"no Git provider", func(w *world) { w.open = nil }},
		{"repository cannot be opened", func(w *world) {
			w.open = func(context.Context, v1alpha1.GitRepository) (providers.GitProvider, error) { return nil, boom }
		}},
		{"branch head read fails", func(w *world) {
			w.git.SetFail(func(op string) error {
				if op == "BranchHead" {
					return &providers.RetryableError{Err: boom}
				}
				return nil
			})
		}},
		{"target branch missing", func(w *world) {
			w.rr.Status.Source.TargetBranch = "gone"
			w.settings.Spec.Repositories[0].TargetBranch = "gone"
			w.rr.Status.PlanDigest = recompute(t, w)
		}},
		{"file read fails", func(w *world) {
			w.git.SetFail(func(op string) error {
				if op == "ReadFile" {
					return &providers.RetryableError{Err: boom}
				}
				return nil
			})
		}},
		{"no digest recorded", func(w *world) { w.rr.Status.PlanDigest = "" }},
		{"no source recorded", func(w *world) { w.rr.Status.Source = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			tc.setup(w)
			err := w.fresh(t)
			if err == nil {
				t.Fatal("want an error, the plan must not count as fresh")
			}
			if _, ok := errors.AsType[*freshness.StaleError](err); ok {
				t.Fatalf("an unreadable input is not proof of a stale plan, got %v", err)
			}
		})
	}
}

func TestPausedIsNotFresh(t *testing.T) {
	w := newWorld(t)
	w.settings.Spec.Paused = new(true)
	if err := w.fresh(t); !errors.Is(err, freshness.ErrPaused) {
		t.Fatalf("want ErrPaused, got %v", err)
	}
}

func recompute(t *testing.T, w *world) string {
	t.Helper()
	d, err := freshness.ComputeDigest(w.rr, w.rr.Status.Source, w.pol, w.settings)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func resourceQuantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func TestPausedWinsOverStale(t *testing.T) {
	w := newWorld(t)
	w.settings.Spec.Paused = new(true)
	w.pol = nil // stale as well
	if err := w.fresh(t); !errors.Is(err, freshness.ErrPaused) {
		t.Fatalf("want ErrPaused before any stale verdict, got %v", err)
	}
}

// chain returns the text of err and of everything reachable through Unwrap, single or joined.
func chain(err error) []string {
	if err == nil {
		return nil
	}
	out := []string{err.Error()}
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		out = append(out, chain(u.Unwrap())...)
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			out = append(out, chain(e)...)
		}
	}
	return out
}

func TestProviderErrorsKeepNoText(t *testing.T) {
	const secret = "ghs_supersecrettoken"
	leaky := &providers.RetryableError{RetryAfter: 4 * time.Second,
		Err: fmt.Errorf("GET https://x-access-token:%s@github.com/o/r: 502", secret)}
	cases := map[string]func(w *world){
		"open": func(w *world) {
			w.open = func(context.Context, v1alpha1.GitRepository) (providers.GitProvider, error) { return nil, leaky }
		},
		"branch head": func(w *world) {
			w.git.SetFail(func(op string) error {
				if op == "BranchHead" {
					return leaky
				}
				return nil
			})
		},
		"read file": func(w *world) {
			w.git.SetFail(func(op string) error {
				if op == "ReadFile" {
					return leaky
				}
				return nil
			})
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			setup(w)
			err := w.fresh(t)
			if err == nil {
				t.Fatal("want an error")
			}
			for _, text := range chain(err) {
				if strings.Contains(text, secret) || strings.Contains(text, "github.com") {
					t.Fatalf("the chain leaks provider text: %q", text)
				}
			}
			pe, ok := errors.AsType[*freshness.ProviderError](err)
			if !ok || pe.RetryAfter != 4*time.Second || !errors.Is(err, providers.ErrRetryable) {
				t.Fatalf("want the class and delay kept, got %#v", err)
			}
		})
	}
}

func TestSourceMappedAgainstTheSettingsAndPolicy(t *testing.T) {
	w := newWorld(t)
	inner := w.resolve
	w.resolve = func(ctx context.Context, in sourcemap.Input) (sourcemap.Result, error) {
		if in.Target != w.rr.Spec.TargetRef || in.Action != w.rr.Spec.Action ||
			len(in.Repositories) != len(w.settings.Spec.Repositories) {
			t.Errorf("the mapping got another input than the policy check gives: %+v", in)
		}
		return inner(ctx, in)
	}
	if err := w.fresh(t); err != nil {
		t.Fatalf("want fresh, got %v", err)
	}
}

// The mapping is run again before the write; a different source supersedes the request.
func TestStaleWhenTheSourceMovesAway(t *testing.T) {
	cases := map[string]func(src *v1alpha1.ResolvedSource){
		"other file":       func(src *v1alpha1.ResolvedSource) { src.Path = otherPath },
		"other branch":     func(src *v1alpha1.ResolvedSource) { src.TargetBranch = otherBranch },
		"other repository": func(src *v1alpha1.ResolvedSource) { src.RepositoryRef = "elsewhere" },
	}
	for name, move := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.resolve = func(context.Context, sourcemap.Input) (sourcemap.Result, error) {
				moved := *w.rr.Status.Source
				move(&moved)
				return sourcemap.Result{Outcome: sourcemap.Resolved, Source: &moved}, nil
			}
			wantStale(t, w.fresh(t), freshness.ReasonSourceChanged)
		})
	}
}

// A mapping that stops (Denied, NeedsIntervention, anything else, or Resolved without a source) never passes
// as fresh, and its message, which may hold text from Git, is not copied into the reason.
func TestMappingThatStopsIsNeverFresh(t *testing.T) {
	for _, res := range []sourcemap.Result{
		{Outcome: sourcemap.Denied, Reason: sourcemap.ReasonPathNotAllowed, Message: "secret/path.yaml"},
		{Outcome: sourcemap.NeedsIntervention, Reason: sourcemap.ReasonHelmManaged, Message: "secret/path.yaml"},
		{Outcome: "Unknown"},
		{Outcome: sourcemap.Resolved},
	} {
		w := newWorld(t)
		w.resolve = func(context.Context, sourcemap.Input) (sourcemap.Result, error) { return res, nil }
		err := w.fresh(t)
		wantStale(t, err, freshness.ReasonSourceChanged)
		if strings.Contains(err.Error(), "secret/path.yaml") {
			t.Fatalf("the mapping message leaked into %q", err)
		}
	}
}

// A transient resolver error says nothing about the plan: not fresh, but not stale either.
func TestResolverErrorIsNotFreshAndNotStale(t *testing.T) {
	w := newWorld(t)
	w.resolve = func(context.Context, sourcemap.Input) (sourcemap.Result, error) {
		return sourcemap.Result{}, errors.New("host unavailable")
	}
	err := w.fresh(t)
	if err == nil {
		t.Fatal("want an error")
	}
	if _, ok := errors.AsType[*freshness.StaleError](err); ok {
		t.Fatalf("a resolver failure must not supersede the request: %v", err)
	}
}

func TestNoResolverFailsClosed(t *testing.T) {
	w := newWorld(t)
	w.resolve = nil
	if err := w.fresh(t); err == nil {
		t.Fatal("want an error without a resolver")
	}
}

func wantRetry(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error: the check must not pass")
	}
	if _, ok := errors.AsType[*freshness.StaleError](err); ok {
		t.Fatalf("want a retry, not a stale verdict: %v", err)
	}
}

// The target changed while the mapping ran: the mapping and the earlier checks describe different objects.
func TestTargetChangedDuringTheMappingIsRetried(t *testing.T) {
	w := newWorld(t)
	mapped := false
	inner := w.resolve
	w.resolve = func(ctx context.Context, in sourcemap.Input) (sourcemap.Result, error) {
		mapped = true
		return inner(ctx, in)
	}
	w.afterGet = func(obj client.Object) {
		if _, ok := obj.(*unstructured.Unstructured); ok && mapped {
			obj.SetResourceVersion("424242")
		}
	}
	wantRetry(t, w.fresh(t))
}

// The mapping and the file read must see one commit.
func TestBranchMovedDuringTheMappingIsRetried(t *testing.T) {
	w := newWorld(t)
	w.resolve = func(context.Context, sourcemap.Input) (sourcemap.Result, error) {
		moved := *w.rr.Status.Source
		moved.BaseCommit = "fedcba9876543210fedcba9876543210fedcba98"
		return sourcemap.Result{Outcome: sourcemap.Resolved, Source: &moved}, nil
	}
	wantRetry(t, w.fresh(t))
}

// The whole mapping runs under the caller's deadline, not a fixed shorter one.
func TestMappingUsesTheCallersDeadline(t *testing.T) {
	w := newWorld(t)
	parent, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	w.ctx = parent
	want, _ := parent.Deadline()
	inner := w.resolve
	w.resolve = func(ctx context.Context, in sourcemap.Input) (sourcemap.Result, error) {
		if got, ok := ctx.Deadline(); !ok || !got.Equal(want) {
			t.Errorf("the mapping deadline is %v, want the caller's %v", got, want)
		}
		return inner(ctx, in)
	}
	if err := w.fresh(t); err != nil {
		t.Fatal(err)
	}
}

// changeDuringMapping marks the point where the resolver ran; afterGet then changes what later reads see.
func changeDuringMapping(w *world, change func(obj client.Object)) {
	mapped := false
	inner := w.resolve
	w.resolve = func(ctx context.Context, in sourcemap.Input) (sourcemap.Result, error) {
		mapped = true
		return inner(ctx, in)
	}
	w.afterGet = func(obj client.Object) {
		if mapped {
			change(obj)
		}
	}
}

// The policy and the settings are read again before "fresh": a pause during the scan is seen.
func TestPausedDuringTheScanIsNotFresh(t *testing.T) {
	w := newWorld(t)
	changeDuringMapping(w, func(obj client.Object) {
		if st, ok := obj.(*v1alpha1.RemediationSettings); ok {
			paused := true
			st.Spec.Paused = &paused
		}
	})
	if err := w.fresh(t); !errors.Is(err, freshness.ErrPaused) {
		t.Fatalf("want ErrPaused, got %v", err)
	}
}

// A policy edited or deleted during the scan is not fresh, and not yet stale: the next check decides.
func TestPolicyChangedDuringTheScanIsRetried(t *testing.T) {
	w := newWorld(t)
	changeDuringMapping(w, func(obj client.Object) {
		if pol, ok := obj.(*v1alpha1.RemediationPolicy); ok {
			pol.Generation++
		}
	})
	wantRetry(t, w.fresh(t))

	w = newWorld(t)
	changeDuringMapping(w, func(obj client.Object) {
		if pol, ok := obj.(*v1alpha1.RemediationPolicy); ok {
			pol.UID = "77777777-7777-4777-8777-777777777777"
		}
	})
	wantRetry(t, w.fresh(t))
}

// readHook wraps a provider and runs after after every ReadFile.
type readHook struct {
	providers.GitProvider
	after func()
}

func (h readHook) ReadFile(ctx context.Context, ref, path string) (providers.File, error) {
	f, err := h.GitProvider.ReadFile(ctx, ref, path)
	h.after()
	return f, err
}

func (w *world) hookRead(after func()) {
	w.open = func(context.Context, v1alpha1.GitRepository) (providers.GitProvider, error) {
		return readHook{GitProvider: w.git, after: after}, nil
	}
}

// The target changed while the file was read (the Git reads are the slow part): not fresh, not stale.
func TestTargetChangedDuringTheFileReadIsRetried(t *testing.T) {
	read := false
	w := newWorld(t)
	w.hookRead(func() { read = true })
	w.afterGet = func(obj client.Object) {
		if _, ok := obj.(*unstructured.Unstructured); ok && read {
			obj.SetResourceVersion("424242")
		}
	}
	wantRetry(t, w.fresh(t))

	read = false
	w = newWorld(t)
	w.hookRead(func() { read = true })
	w.afterGet = func(obj client.Object) {
		if _, ok := obj.(*unstructured.Unstructured); ok && read {
			obj.SetLabels(map[string]string{tierLabel: "db"})
			obj.SetResourceVersion("424242")
		}
	}
	wantRetry(t, w.fresh(t))
}

// A Flux object the mapping saw changed while the file was read.
func TestFluxObjectChangedDuringTheFileReadIsRetried(t *testing.T) {
	gvk := sourcemap.KustomizationGVK
	ks := &unstructured.Unstructured{}
	ks.SetGroupVersionKind(gvk)
	ks.SetNamespace("flux-system")
	ks.SetName("apps")
	ks.SetUID("99999999-9999-4999-8999-999999999999")
	ks.SetResourceVersion("7")

	read := false
	w := newWorld(t)
	w.extra = []client.Object{ks}
	inner := w.resolve
	w.resolve = func(ctx context.Context, in sourcemap.Input) (sourcemap.Result, error) {
		res, err := inner(ctx, in)
		res.Observed = []sourcemap.Observed{{GVK: gvk, Namespace: "flux-system", Name: "apps", UID: string(ks.GetUID()),
			ResourceVersion: "7"}}
		return res, err
	}
	w.hookRead(func() { read = true })
	if err := w.fresh(t); err != nil {
		t.Fatalf("want fresh while the Flux object is unchanged, got %v", err)
	}
	read = false
	w.afterGet = func(obj client.Object) {
		if u, ok := obj.(*unstructured.Unstructured); ok && read && u.GetKind() == gvk.Kind {
			obj.SetResourceVersion("8")
		}
	}
	wantRetry(t, w.fresh(t))
}

// The branch moved while the file was read: the file may not be the one at the head that was read first.
func TestBranchMovedDuringTheFileReadIsRetried(t *testing.T) {
	t.Run("the file is read", func(t *testing.T) {
		w := newWorld(t)
		w.hookRead(func() {
			if _, err := w.git.Push("main", otherPath, []byte("a: 3\n")); err != nil {
				t.Fatal(err)
			}
		})
		wantRetry(t, w.fresh(t))
	})
	t.Run("a file that is not found is not reported as stale", func(t *testing.T) {
		w := newWorld(t)
		moved := false
		w.hookRead(func() {
			if !moved {
				moved = true
				if _, err := w.git.Push("main", otherPath, []byte("a: 3\n")); err != nil {
					t.Fatal(err)
				}
			}
		})
		w.rr.Status.Source.Path = "apps/missing.yaml"
		w.rr.Status.PlanDigest = recompute(t, w)
		w.resolve = func(context.Context, sourcemap.Input) (sourcemap.Result, error) {
			src := *w.rr.Status.Source
			src.BaseCommit, _ = w.git.BranchHead(context.Background(), "main")
			return sourcemap.Result{Outcome: sourcemap.Resolved, Source: &src}, nil
		}
		wantRetry(t, w.fresh(t))
	})
}
