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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/policy/freshness"
	"github.com/pkalin/shadewright/internal/providers"
	fakegit "github.com/pkalin/shadewright/internal/providers/fake"
	fx "github.com/pkalin/shadewright/internal/testfixtures"
)

const (
	sourcePath = "apps/orders/deployment.yaml"
	otherPath  = "docs/readme.yaml"
	tierLabel  = "tier"
	targetUID  = "33333333-3333-4333-8333-333333333333"
	policyUID  = "44444444-4444-4444-8444-444444444444"
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
	open     freshness.RepositoryOpener
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
	var objs []client.Object
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
			return cl.Get(ctx, key, obj, opts...)
		},
	}).Build()
	return freshness.Checker{Reader: c, OpenRepository: w.open}.Fresh(context.Background(), w.rr)
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
		{"source target branch", func(w *world) { w.rr.Status.Source.TargetBranch = "release" }, freshness.ReasonSettingsChanged},
		{"source path", func(w *world) { w.rr.Status.Source.Path = otherPath }, freshness.ReasonPlanChanged},
		{"source base commit", func(w *world) {
			w.rr.Status.Source.BaseCommit = "89abcdef0123456789abcdef0123456789abcdef"
		}, freshness.ReasonPlanChanged},
		{"repository URL in settings", func(w *world) {
			w.settings.Spec.Repositories[0].URL = "https://github.com/example/other"
		}, freshness.ReasonPlanChanged},
		{"installation in settings", func(w *world) { w.settings.Spec.Repositories[0].GitHub.InstallationID = 2 },
			freshness.ReasonPlanChanged},
		{"target branch in settings", func(w *world) { w.settings.Spec.Repositories[0].TargetBranch = "release" },
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
