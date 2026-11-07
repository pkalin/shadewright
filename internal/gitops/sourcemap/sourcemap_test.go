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
package sourcemap_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/gitops/sourcemap"
	"github.com/pkalin/shadewright/internal/providers"
	"github.com/pkalin/shadewright/internal/providers/fake"
)

const (
	fleetURL  = "https://github.com/example/fleet"
	targetUID = "22222222-2222-4222-8222-222222222222"
	webUID    = "55555555-5555-4555-8555-555555555555"
	revision  = "main@sha1:0123456789abcdef0123456789abcdef01234567"
	soFile    = "apps/orders/scaledobject.yaml"
	ksFile    = "apps/orders/kustomization.yaml"
)

const scaledObjectYAML = `apiVersion: v1
kind: Service
metadata:
  name: orders-worker
  namespace: orders
spec:
  ports:
    - port: 80
---
# The worker scales on queue depth.
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: orders-worker
  namespace: orders
spec:
  scaleTargetRef:
    name: orders-worker
  maxReplicaCount: 10
---
`

const deploymentYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: orders
spec:
  template:
    spec:
      containers:
        - name: sidecar
          resources:
            limits:
              memory: 64Mi
        - name: web
          resources:
            limits:
              memory: 268435456
`

// shaRE is the pattern of ResolvedSource.baseCommit.
var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// fixture is a working setup: a ScaledObject and a Deployment delivered by Kustomization flux-system/apps
// from GitRepository flux-system/fleet, branch main of the fleet repository. Each test case changes one thing.
type fixture struct {
	target     *unstructured.Unstructured
	deployment *unstructured.Unstructured
	ks         *unstructured.Unstructured
	src        *unstructured.Unstructured
	files      map[string]string
	gitBranch  string
	in         sourcemap.Input
	gitFail    func(op string) error
	// gitWrap, when set, wraps the fake repository, for providers that break the GitProvider contract.
	gitWrap func(providers.GitProvider) providers.GitProvider
	getErr  error
	// openErr, when set, is what the repository opener returns.
	openErr error
	// duringGit runs on the first read of the branch head, to change the cluster while Git is scanned.
	duringGit func(c client.Client)
	// afterGet runs after every successful read of an object, to change the cluster between two reads.
	afterGet func(c client.Client, obj client.Object)
	// cache, when set, is the resolver's read cache, shared by every run of the fixture.
	cache *sourcemap.ReadCache
}

// onHead calls fn once, when the branch head is read.
type onHead struct {
	providers.GitProvider
	fn func()
}

func (o onHead) BranchHead(ctx context.Context, branch string) (string, error) {
	if o.fn != nil {
		o.fn()
	}
	return o.GitProvider.BranchHead(ctx, branch)
}

func obj(apiVersion, kind, namespace, name string, fields map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: fields}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(namespace)
	u.SetName(name)
	return u
}

//nolint:goconst // literal names keep the fixture readable
func fluxLabels() map[string]string {
	return map[string]string{
		sourcemap.LabelKustomizationName:      "apps",
		sourcemap.LabelKustomizationNamespace: "flux-system",
	}
}

//nolint:goconst // literal objects keep the fixture readable
func newFixture() *fixture {
	target := obj("keda.sh/v1alpha1", "ScaledObject", "orders", "orders-worker", map[string]any{
		"spec": map[string]any{"maxReplicaCount": int64(10)},
	})
	target.SetUID(targetUID)
	target.SetLabels(fluxLabels())

	dep := obj("apps/v1", "Deployment", "orders", "web", map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{
			map[string]any{"name": "sidecar", "resources": map[string]any{"limits": map[string]any{"memory": "64Mi"}}},
			map[string]any{"name": "web", "resources": map[string]any{"limits": map[string]any{"memory": "256Mi"}}},
		}}}},
	})
	dep.SetLabels(fluxLabels())
	dep.SetUID(webUID)

	ks := obj("kustomize.toolkit.fluxcd.io/v1", "Kustomization", "flux-system", "apps", map[string]any{
		"spec": map[string]any{
			"path":      "./apps/orders",
			"prune":     true,
			"sourceRef": map[string]any{"kind": "GitRepository", "name": "fleet"},
		},
		"status": map[string]any{
			"lastAppliedRevision": revision,
			"observedGeneration":  int64(2),
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(2)},
			},
			"inventory": map[string]any{"entries": []any{
				map[string]any{"id": "orders_orders-worker_keda.sh_ScaledObject", "v": "v1alpha1"},
				map[string]any{"id": "orders_orders-worker__Service", "v": "v1"},
				map[string]any{"id": "orders_web_apps_Deployment", "v": "v1"},
			}},
		},
	})
	ks.SetGeneration(2)

	src := obj("source.toolkit.fluxcd.io/v1", "GitRepository", "flux-system", "fleet", map[string]any{
		"spec": map[string]any{"url": fleetURL, "ref": map[string]any{"branch": "main"}},
		"status": map[string]any{
			"observedGeneration": int64(4),
			"conditions":         []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(4)}},
			"artifact":           map[string]any{"revision": revision},
		},
	})
	src.SetGeneration(4)

	return &fixture{
		target:     target,
		deployment: dep,
		ks:         ks,
		src:        src,
		gitBranch:  "main",
		files: map[string]string{
			ksFile: "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" +
				"resources:\n  - scaledobject.yaml\n  - ./deployment.yaml\n",
			soFile:                        scaledObjectYAML,
			"apps/orders/deployment.yaml": deploymentYAML,
			"README.md":                   "not part of the build\n",
		},
		in: sourcemap.Input{
			Target: v1alpha1.ObjectRef{
				APIVersion: "keda.sh/v1alpha1", Kind: "ScaledObject",
				Namespace: "orders", Name: "orders-worker", UID: targetUID,
			},
			Action: v1alpha1.Action{
				Type:                  v1alpha1.ActionAdjustKedaMaxReplicas,
				AdjustKedaMaxReplicas: &v1alpha1.AdjustKedaMaxReplicas{PreviousValue: 10, DesiredValue: 15},
			},
			Repositories: []v1alpha1.GitRepository{
				{Name: "other", URL: "https://github.com/example/other", TargetBranch: "main", Provider: v1alpha1.GitProviderGitHub},
				{Name: "fleet", URL: fleetURL, TargetBranch: "main", Provider: v1alpha1.GitProviderGitHub},
			},
			GitOps: v1alpha1.PolicyGitOps{RepositoryRef: "fleet", AllowedPaths: []v1alpha1.GitPathPattern{"apps/**/*.yaml"}},
		},
	}
}

// targetDeployment makes the Deployment's web container memory limit the target.
//
//nolint:goconst // literal identities keep the fixture readable
func (f *fixture) targetDeployment() {
	f.in.Target = v1alpha1.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "orders", Name: "web", UID: webUID}
	f.in.Action = v1alpha1.Action{
		Type:                    v1alpha1.ActionSetContainerMemoryLimit,
		SetContainerMemoryLimit: &v1alpha1.SetContainerMemoryLimit{Container: "web"},
	}
}

func set(u *unstructured.Unstructured, value any, fields ...string) {
	if err := unstructured.SetNestedField(u.Object, value, fields...); err != nil {
		panic(err)
	}
}

func (f *fixture) run(t *testing.T) (sourcemap.Result, string, error) {
	t.Helper()
	files := make(map[string][]byte, len(f.files))
	for p, s := range f.files {
		files[p] = []byte(s)
	}
	git := fake.NewGit(f.gitBranch, files)
	head, err := git.BranchHead(context.Background(), f.gitBranch)
	if err != nil {
		t.Fatal(err)
	}
	if f.gitFail != nil {
		git.SetFail(f.gitFail)
	}
	b := clientfake.NewClientBuilder().WithScheme(runtime.NewScheme())
	for _, o := range []*unstructured.Unstructured{f.target, f.deployment, f.ks, f.src} {
		if o != nil {
			b = b.WithObjects(o.DeepCopy())
		}
	}
	if f.getErr != nil {
		b = b.WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return f.getErr
			},
		})
	}
	if f.afterGet != nil {
		b = b.WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				err := c.Get(ctx, key, obj, opts...)
				if err == nil {
					f.afterGet(c, obj)
				}
				return err
			},
		})
	}
	cl := b.Build()
	r := &sourcemap.Resolver{
		Client: cl,
		Cache:  f.cache,
		OpenRepository: func(_ context.Context, repo v1alpha1.GitRepository) (providers.GitProvider, error) {
			if repo.Name != "fleet" {
				return nil, fmt.Errorf("opened repository %s", repo.Name)
			}
			if f.openErr != nil {
				return nil, f.openErr
			}
			if f.duringGit != nil {
				return onHead{GitProvider: git, fn: func() { f.duringGit(cl) }}, nil
			}
			if f.gitWrap != nil {
				return f.gitWrap(git), nil
			}
			return git, nil
		},
	}
	res, err := r.Resolve(context.Background(), f.in)
	return res, head, err
}

func TestResolveFindsTheSource(t *testing.T) {
	tests := []struct {
		name   string
		change func(f *fixture)
		path   string
		build  string // the build directory; empty means apps/orders
	}{
		{
			name: "ScaledObject in a multi-document file",
			path: soFile,
		},
		{
			name:   "Deployment memory limit written as bytes in Git and Mi in the cluster",
			change: (*fixture).targetDeployment,
			path:   "apps/orders/deployment.yaml",
		},
		{
			name: "Kustomization at the repository root",
			change: func(f *fixture) {
				set(f.ks, "", "spec", "path")
				f.files["kustomization.yaml"] = "resources:\n  - apps/orders\n"
			},
			path:  soFile,
			build: ".",
		},
		{
			name: "Kustomization path without a leading dot",
			change: func(f *fixture) {
				set(f.ks, "apps/orders/", "spec", "path")
			},
			path: soFile,
		},
		{
			name: "file in a nested directory reached through a parent reference",
			change: func(f *fixture) {
				f.files[ksFile] = "resources:\n  - ../base/workers\n"
				f.files["apps/base/workers/kustomization.yml"] = "resources:\n  - so.yaml\n"
				f.files["apps/base/workers/so.yaml"] = scaledObjectYAML
				delete(f.files, soFile)
			},
			path: "apps/base/workers/so.yaml",
		},
		{
			name: "same directory listed twice is read once",
			change: func(f *fixture) {
				f.files[ksFile] = "resources:\n  - sub\n  - ./sub/\n"
				f.files["apps/orders/sub/Kustomization"] = ksParentScaledObject
			},
			path: soFile,
		},
		{
			name: "settings targetBranch left to its default",
			change: func(f *fixture) {
				f.in.Repositories[1].TargetBranch = ""
			},
			path: soFile,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			if tc.change != nil {
				tc.change(f)
			}
			res, head, err := f.run(t)
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != sourcemap.Resolved || res.Reason != sourcemap.ReasonResolved || res.Source == nil {
				t.Fatalf("result = %+v", res)
			}
			build := tc.build
			if build == "" {
				build = "apps/orders"
			}
			want := v1alpha1.ResolvedSource{RepositoryRef: "fleet", TargetBranch: "main", Path: v1alpha1.GitFilePath(tc.path),
				BuildPath: v1alpha1.GitDirPath(build), BaseCommit: head}
			if !shaRE.MatchString(res.Source.BaseCommit) {
				t.Errorf("base commit %q is not a full SHA", res.Source.BaseCommit)
			}
			if *res.Source != want {
				t.Errorf("source = %+v, want %+v", *res.Source, want)
			}
			sameWithCache(t, tc.change, res)
		})
	}
}

//nolint:goconst // literal values keep each case readable
func TestResolveStops(t *testing.T) {
	ni, dn := sourcemap.NeedsIntervention, sourcemap.Denied
	tests := []struct {
		name    string
		change  func(f *fixture)
		outcome sourcemap.Outcome
		reason  sourcemap.Reason
	}{
		// Input and target.
		{"target without a namespace", func(f *fixture) { f.in.Target.Namespace = "" }, ni, sourcemap.ReasonInvalidInput},
		{"invalid apiVersion", func(f *fixture) { f.in.Target.APIVersion = "a/b/c" }, ni, sourcemap.ReasonInvalidInput},
		{"target without a UID", func(f *fixture) { f.in.Target.UID = "" }, ni, sourcemap.ReasonInvalidInput},
		{"memory action without parameters", func(f *fixture) {
			f.targetDeployment()
			f.in.Action.SetContainerMemoryLimit = nil
		}, ni, sourcemap.ReasonInvalidInput},
		{"memory action on a ScaledObject, refused before any read", func(f *fixture) {
			f.in.Action = v1alpha1.Action{
				Type:                    v1alpha1.ActionSetContainerMemoryLimit,
				SetContainerMemoryLimit: &v1alpha1.SetContainerMemoryLimit{Container: "web"},
			}
			f.getErr = errors.New("the cluster must not be read")
		}, ni, sourcemap.ReasonInvalidInput},
		{"KEDA action on a Deployment", func(f *fixture) {
			f.targetDeployment()
			f.in.Action = newFixture().in.Action
		}, ni, sourcemap.ReasonInvalidInput},
		{"memory action on a Deployment of another group", func(f *fixture) {
			f.targetDeployment()
			f.in.Target.APIVersion = "example.com/v1"
		}, ni, sourcemap.ReasonInvalidInput},
		{"unknown action", func(f *fixture) { f.in.Action = v1alpha1.Action{Type: "DeleteNamespace"} }, ni, sourcemap.ReasonInvalidInput},
		{"target missing", func(f *fixture) { f.target = nil }, ni, sourcemap.ReasonTargetNotFound},
		{"target recreated", func(f *fixture) { f.target.SetUID("33333333-3333-4333-8333-333333333333") }, ni, sourcemap.ReasonTargetChanged},

		// Step 1: labels.
		{"installed by Helm", func(f *fixture) {
			l := f.target.GetLabels()
			l[sourcemap.LabelHelmReleaseName] = "orders"
			f.target.SetLabels(l)
		}, ni, sourcemap.ReasonHelmManaged},
		{"no Flux labels", func(f *fixture) { f.target.SetLabels(nil) }, ni, sourcemap.ReasonNoFluxLabels},
		{"namespace label missing", func(f *fixture) {
			f.target.SetLabels(map[string]string{sourcemap.LabelKustomizationName: "apps"})
		}, ni, sourcemap.ReasonInvalidFluxLabels},
		{"label is not a name", func(f *fixture) {
			f.target.SetLabels(map[string]string{
				sourcemap.LabelKustomizationName: "Apps_1", sourcemap.LabelKustomizationNamespace: "flux-system",
			})
		}, ni, sourcemap.ReasonInvalidFluxLabels},
		{"labels name a missing Kustomization", func(f *fixture) {
			f.target.SetLabels(map[string]string{
				sourcemap.LabelKustomizationName: "other", sourcemap.LabelKustomizationNamespace: "flux-system",
			})
		}, ni, sourcemap.ReasonKustomizationNotFound},

		// Step 2: Kustomization.
		{"Kustomization suspended", func(f *fixture) { set(f.ks, true, "spec", "suspend") }, ni, sourcemap.ReasonKustomizationSuspended},
		{"Kustomization not Ready", func(f *fixture) {
			set(f.ks, []any{map[string]any{"type": "Ready", "status": "False"}}, "status", "conditions")
		}, ni, sourcemap.ReasonKustomizationNotReady},
		{"Kustomization without conditions", func(f *fixture) {
			unstructured.RemoveNestedField(f.ks.Object, "status", "conditions")
		}, ni, sourcemap.ReasonKustomizationNotReady},
		{"Kustomization status for an older generation", func(f *fixture) { f.ks.SetGeneration(3) }, ni, sourcemap.ReasonKustomizationNotReady},
		{"Ready condition for an older generation", func(f *fixture) {
			set(f.ks, []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(1)}}, "status", "conditions")
		}, ni, sourcemap.ReasonKustomizationNotReady},
		{"inventory lacks the object", func(f *fixture) {
			set(f.ks, []any{map[string]any{"id": "orders_web_apps_Deployment", "v": "v1"}}, "status", "inventory", "entries")
		}, dn, sourcemap.ReasonNotInInventory},
		{"no inventory", func(f *fixture) { unstructured.RemoveNestedField(f.ks.Object, "status", "inventory") }, dn, sourcemap.ReasonNotInInventory},
		{"inventory lists another version", func(f *fixture) {
			set(f.ks, []any{map[string]any{"id": "orders_orders-worker_keda.sh_ScaledObject", "v": "v1beta1"}},
				"status", "inventory", "entries")
		}, dn, sourcemap.ReasonNotInInventory},
		{"inventory lists the object in another namespace", func(f *fixture) {
			set(f.ks, []any{map[string]any{"id": "billing_orders-worker_keda.sh_ScaledObject", "v": "v1alpha1"}},
				"status", "inventory", "entries")
		}, dn, sourcemap.ReasonNotInInventory},
		{"Kustomization patches", func(f *fixture) {
			set(f.ks, []any{map[string]any{"patch": "x"}}, "spec", "patches")
		}, ni, sourcemap.ReasonKustomizationTransforms},
		{"Kustomization sets targetNamespace", func(f *fixture) { set(f.ks, "orders", "spec", "targetNamespace") }, ni, sourcemap.ReasonKustomizationTransforms},
		{"Kustomization substitutes variables", func(f *fixture) {
			set(f.ks, map[string]any{"substitute": map[string]any{"replicas": "10"}}, "spec", "postBuild")
		}, ni, sourcemap.ReasonKustomizationTransforms},
		{"Kustomization sets images", func(f *fixture) {
			set(f.ks, []any{map[string]any{"name": "web"}}, "spec", "images")
		}, ni, sourcemap.ReasonKustomizationTransforms},
		// The fixture's inventory lists the target, so only the kubeConfig check can stop this one.
		{"Kustomization applies to a remote cluster", func(f *fixture) {
			set(f.ks, map[string]any{"secretRef": map[string]any{"name": "remote-kubeconfig"}}, "spec", "kubeConfig")
		}, ni, sourcemap.ReasonRemoteCluster},
		// The manifest keeps the target field in plaintext, as selective SOPS encryption allows.
		{"Kustomization decrypts SOPS files", func(f *fixture) {
			set(f.ks, map[string]any{"provider": "sops", "secretRef": map[string]any{"name": "sops-age"}}, "spec", "decryption")
		}, ni, sourcemap.ReasonDecryption},
		{"source is an OCIRepository", func(f *fixture) {
			set(f.ks, "OCIRepository", "spec", "sourceRef", "kind")
		}, ni, sourcemap.ReasonUnsupportedSource},
		{"GitRepository missing", func(f *fixture) { f.src = nil }, ni, sourcemap.ReasonSourceNotFound},
		{"sourceRef name is not a valid name", func(f *fixture) {
			set(f.ks, "Fleet_1", "spec", "sourceRef", "name")
		}, ni, sourcemap.ReasonUnsupportedSource},
		{"sourceRef namespace is not a valid name", func(f *fixture) {
			set(f.ks, "flux.system", "spec", "sourceRef", "namespace")
		}, ni, sourcemap.ReasonUnsupportedSource},
		{"GitRepository in another namespace is missing", func(f *fixture) {
			set(f.ks, "other", "spec", "sourceRef", "namespace")
		}, ni, sourcemap.ReasonSourceNotFound},

		// Step 2: GitRepository.
		{"GitRepository without a ref", func(f *fixture) { unstructured.RemoveNestedField(f.src.Object, "spec", "ref") }, ni, sourcemap.ReasonSourceNotBranch},
		{"GitRepository tracks a tag", func(f *fixture) {
			set(f.src, map[string]any{"tag": "v1.0.0"}, "spec", "ref")
		}, ni, sourcemap.ReasonSourceNotBranch},
		{"GitRepository tracks a semver range", func(f *fixture) {
			set(f.src, map[string]any{"semver": ">=1.0.0"}, "spec", "ref")
		}, ni, sourcemap.ReasonSourceNotBranch},
		{"GitRepository pins a commit on the branch", func(f *fixture) {
			set(f.src, map[string]any{"branch": "main", "commit": "0123456789abcdef0123456789abcdef01234567"}, "spec", "ref")
		}, ni, sourcemap.ReasonSourceNotBranch},
		{"GitRepository uses a ref name", func(f *fixture) {
			set(f.src, map[string]any{"branch": "main", "name": "refs/heads/other"}, "spec", "ref")
		}, ni, sourcemap.ReasonSourceNotBranch},
		{"GitRepository suspended", func(f *fixture) { set(f.src, true, "spec", "suspend") }, ni, sourcemap.ReasonSourceOptions},
		{"GitRepository ignores files", func(f *fixture) { set(f.src, "apps/orders/*.yaml\n", "spec", "ignore") }, ni, sourcemap.ReasonSourceOptions},
		{"GitRepository recurses into submodules", func(f *fixture) {
			set(f.src, true, "spec", "recurseSubmodules")
		}, ni, sourcemap.ReasonSourceOptions},
		{"GitRepository sparse checkout", func(f *fixture) {
			set(f.src, []any{"apps/"}, "spec", "sparseCheckout")
		}, ni, sourcemap.ReasonSourceOptions},
		{"Kustomization applied an older revision", func(f *fixture) {
			set(f.ks, "main@sha1:89abcdef0123456789abcdef0123456789abcdef", "status", "lastAppliedRevision")
		}, ni, sourcemap.ReasonRevisionMismatch},
		{"Kustomization has applied nothing", func(f *fixture) {
			unstructured.RemoveNestedField(f.ks.Object, "status", "lastAppliedRevision")
		}, ni, sourcemap.ReasonRevisionMismatch},
		{"GitRepository has no artifact", func(f *fixture) {
			unstructured.RemoveNestedField(f.src.Object, "status", "artifact")
		}, ni, sourcemap.ReasonRevisionMismatch},
		{"GitRepository not Ready", func(f *fixture) {
			set(f.src, []any{map[string]any{"type": "Ready", "status": "False"}}, "status", "conditions")
		}, ni, sourcemap.ReasonSourceNotReady},
		{"GitRepository URL changed and status is stale", func(f *fixture) {
			// The old artifact still equals lastAppliedRevision, but it was fetched for the previous spec.
			f.src.SetGeneration(5)
		}, ni, sourcemap.ReasonSourceNotReady},
		{"Ready condition of the GitRepository for an older generation", func(f *fixture) {
			set(f.src, []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(3)}}, "status", "conditions")
		}, ni, sourcemap.ReasonSourceNotReady},
		{"artifact fetched from another branch", func(f *fixture) {
			other := "release@sha1:0123456789abcdef0123456789abcdef01234567"
			set(f.src, other, "status", "artifact", "revision")
			set(f.ks, other, "status", "lastAppliedRevision")
		}, ni, sourcemap.ReasonRevisionMismatch},
		{"artifact revision without a digest", func(f *fixture) {
			set(f.src, "main", "status", "artifact", "revision")
			set(f.ks, "main", "status", "lastAppliedRevision")
		}, ni, sourcemap.ReasonRevisionMismatch},
		{".sourceignore at the repository root", func(f *fixture) {
			f.files[".sourceignore"] = "apps/orders/kustomization.yaml\n"
		}, ni, sourcemap.ReasonSourceOptions},
		{".sourceignore in the Kustomization path", func(f *fixture) {
			f.files["apps/orders/.sourceignore"] = "decoy.yaml\n"
		}, ni, sourcemap.ReasonSourceOptions},
		{".sourceignore above the Kustomization path", func(f *fixture) {
			f.files["apps/.sourceignore"] = "*\n"
		}, ni, sourcemap.ReasonSourceOptions},
		{".sourceignore in a directory a resource file lives in", func(f *fixture) {
			f.files[ksFile] = "resources:\n  - ../base/so.yaml\n"
			f.files["apps/base/so.yaml"] = scaledObjectYAML
			f.files["apps/base/.sourceignore"] = "so.yaml\n"
		}, ni, sourcemap.ReasonSourceOptions},
		{".sourceignore in a nested kustomization directory", func(f *fixture) {
			f.files[ksFile] = ksWorkers
			f.files["apps/orders/workers/kustomization.yaml"] = ksParentScaledObject
			f.files["apps/orders/workers/.sourceignore"] = "kustomization.yaml\n"
		}, ni, sourcemap.ReasonSourceOptions},
		{"GitRepository includes another source", func(f *fixture) {
			set(f.src, []any{map[string]any{"repository": map[string]any{"name": "x"}}}, "spec", "include")
		}, ni, sourcemap.ReasonSourceIncludes},

		// Step 2: settings and policy.
		{"URL with .git is not the settings URL", func(f *fixture) { set(f.src, fleetURL+".git", "spec", "url") }, dn, sourcemap.ReasonRepositoryNotConfigured},
		{"URL differs in case", func(f *fixture) {
			set(f.src, "https://github.com/Example/fleet", "spec", "url")
		}, dn, sourcemap.ReasonRepositoryNotConfigured},
		{"URL with a trailing slash", func(f *fixture) { set(f.src, fleetURL+"/", "spec", "url") }, dn, sourcemap.ReasonRepositoryNotConfigured},
		{"SSH URL", func(f *fixture) { set(f.src, "ssh://git@github.com/example/fleet", "spec", "url") }, dn, sourcemap.ReasonRepositoryNotConfigured},
		{"GitRepository without a URL", func(f *fixture) { unstructured.RemoveNestedField(f.src.Object, "spec", "url") }, dn, sourcemap.ReasonRepositoryNotConfigured},
		{"URL matches two settings entries", func(f *fixture) {
			f.in.Repositories[0].URL = fleetURL
		}, dn, sourcemap.ReasonRepositoryNotConfigured},
		{"no repositories in settings", func(f *fixture) { f.in.Repositories = nil }, dn, sourcemap.ReasonRepositoryNotConfigured},
		{"policy names another repository", func(f *fixture) { f.in.GitOps.RepositoryRef = "other" }, dn, sourcemap.ReasonRepositoryNotInPolicy},
		{"Flux tracks another branch than targetBranch", func(f *fixture) {
			set(f.src, map[string]any{"branch": "release"}, "spec", "ref")
			other := "release@sha1:0123456789abcdef0123456789abcdef01234567"
			set(f.src, other, "status", "artifact", "revision")
			set(f.ks, other, "status", "lastAppliedRevision")
		}, dn, sourcemap.ReasonBranchMismatch},
		{"branch missing in Git", func(f *fixture) { f.gitBranch = "trunk" }, ni, sourcemap.ReasonBranchNotFound},

		// Step 3: paths and kustomization files.
		{"Kustomization path outside the repository", func(f *fixture) { set(f.ks, "../outside", "spec", "path") }, ni, sourcemap.ReasonInvalidPath},
		{"absolute Kustomization path", func(f *fixture) { set(f.ks, "/apps/orders", "spec", "path") }, ni, sourcemap.ReasonInvalidPath},
		{"hidden directory in the Kustomization path", func(f *fixture) { set(f.ks, "./.github", "spec", "path") }, ni, sourcemap.ReasonInvalidPath},
		{"Kustomization path is a URL", func(f *fixture) {
			set(f.ks, "https://example.com/apps", "spec", "path")
		}, ni, sourcemap.ReasonInvalidPath},
		{"Kustomization path not in the tree", func(f *fixture) {
			set(f.ks, "./apps/missing", "spec", "path")
		}, ni, sourcemap.ReasonNoKustomizationFile},
		{"nested directory without a kustomization file", func(f *fixture) {
			f.files[ksFile] = ksWorkers
		}, ni, sourcemap.ReasonNoKustomizationFile},
		{"two kustomization files", func(f *fixture) {
			f.files["apps/orders/kustomization.yml"] = ksEmpty
		}, ni, sourcemap.ReasonUnsupportedKustomization},
		{"kustomization file with patches", func(f *fixture) {
			f.files[ksFile] += "patches:\n  - path: patch.yaml\n"
		}, ni, sourcemap.ReasonUnsupportedKustomization},
		{"kustomization file with a namespace", func(f *fixture) { f.files[ksFile] += "namespace: orders\n" }, ni, sourcemap.ReasonUnsupportedKustomization},
		{"kustomization file with a generator", func(f *fixture) {
			f.files[ksFile] += "configMapGenerator:\n  - name: cfg\n"
		}, ni, sourcemap.ReasonUnsupportedKustomization},
		{"kustomization file with a Helm chart", func(f *fixture) {
			f.files[ksFile] += "helmCharts:\n  - name: orders\n"
		}, ni, sourcemap.ReasonUnsupportedKustomization},
		{"kustomization file with an odd key", func(f *fixture) {
			f.files[ksFile] += "\"$(rm -rf /)\": 1\n"
		}, ni, sourcemap.ReasonUnsupportedKustomization},
		{"component instead of a Kustomization", func(f *fixture) {
			f.files[ksFile] = "kind: Component\nresources:\n  - scaledobject.yaml\n"
		}, ni, sourcemap.ReasonUnsupportedKustomization},
		{"remote resource", func(f *fixture) {
			f.files[ksFile] = "resources:\n  - https://github.com/example/fleet//apps?ref=main\n"
		}, ni, sourcemap.ReasonUnsupportedKustomization},
		{"remote resource without a scheme", func(f *fixture) {
			// kustomize reads github.com/<org>/<repo> as a remote base, so it is refused as a reference.
			f.files[ksFile] = "resources:\n  - github.com/example/fleet/apps\n"
		}, ni, sourcemap.ReasonUnsupportedKustomization},
		{"resource outside the repository", func(f *fixture) {
			f.files[ksFile] = "resources:\n  - ../../../etc/passwd.yaml\n"
		}, ni, sourcemap.ReasonUnsupportedKustomization},
		{"resource that is not a string", func(f *fixture) {
			f.files[ksFile] = "resources:\n  - {path: scaledobject.yaml}\n"
		}, ni, sourcemap.ReasonUnsupportedKustomization},
		{"resources that are not a list", func(f *fixture) { f.files[ksFile] = "resources: scaledobject.yaml\n" }, ni, sourcemap.ReasonFileNotReadable},
		{"kustomization file that is not YAML", func(f *fixture) { f.files[ksFile] = "resources: [\n" }, ni, sourcemap.ReasonFileNotReadable},
		{"listed file missing", func(f *fixture) { delete(f.files, soFile) }, ni, sourcemap.ReasonFileNotReadable},
		{"manifest that is not YAML", func(f *fixture) { f.files[soFile] = "kind: [\n" }, ni, sourcemap.ReasonFileNotReadable},
		{"manifest with a duplicate key", func(f *fixture) {
			f.files[soFile] = strings.Replace(scaledObjectYAML, "  maxReplicaCount: 10\n", "  maxReplicaCount: 10\n  maxReplicaCount: 20\n", 1)
		}, ni, sourcemap.ReasonFileNotReadable},
		{"file too large", func(f *fixture) {
			f.files[soFile] = scaledObjectYAML + "# " + strings.Repeat("x", sourcemap.MaxFileBytes) + "\n"
		}, ni, sourcemap.ReasonFileNotReadable},
		// An adapter that ignores the size contract and returns the whole file is still refused.
		{"file too large from a lax provider", func(f *fixture) {
			f.gitWrap = func(g providers.GitProvider) providers.GitProvider { return padReads{g} }
		}, ni, sourcemap.ReasonFileNotReadable},
		{"too many files", func(f *fixture) {
			var b strings.Builder
			b.WriteString("resources:\n")
			for i := range sourcemap.MaxFiles + 1 {
				fmt.Fprintf(&b, "  - f%d.yaml\n", i)
			}
			f.files[ksFile] = b.String()
		}, ni, sourcemap.ReasonTooManyFiles},
		{"too many directories", func(f *fixture) {
			var b strings.Builder
			b.WriteString("resources:\n")
			for i := range sourcemap.MaxReads / 2 {
				fmt.Fprintf(&b, "  - d%d\n", i)
				f.files[fmt.Sprintf("apps/orders/d%d/kustomization.yaml", i)] = ksEmpty
			}
			f.files[ksFile] = b.String()
		}, ni, sourcemap.ReasonTooManyFiles},
		{"directories nested too deep", func(f *fixture) {
			dir := "apps/orders"
			for i := range sourcemap.MaxDepth + 1 {
				f.files[dir+"/kustomization.yaml"] = fmt.Sprintf("resources:\n  - d%d\n", i)
				dir = fmt.Sprintf("%s/d%d", dir, i)
			}
			f.files[dir+"/kustomization.yaml"] = ksEmpty
		}, ni, sourcemap.ReasonTooManyFiles},

		// Step 3: the declaration.
		{"no document declares the object", func(f *fixture) {
			f.files[soFile] = strings.Replace(scaledObjectYAML, "name: orders-worker\n  namespace: orders\nspec:\n  scaleTargetRef",
				"name: orders-api\n  namespace: orders\nspec:\n  scaleTargetRef", 1)
		}, ni, sourcemap.ReasonDeclarationNotFound},
		{"object declared only in a file outside the build", func(f *fixture) {
			f.files[ksFile] = "resources:\n  - deployment.yaml\n"
		}, ni, sourcemap.ReasonDeclarationNotFound},
		{"two documents in one file declare the object", func(f *fixture) {
			f.files[soFile] = scaledObjectYAML + scaledObjectYAML[strings.Index(scaledObjectYAML, "# The worker"):]
		}, ni, sourcemap.ReasonAmbiguousDeclaration},
		{"two files declare the object", func(f *fixture) {
			f.files[ksFile] += "  - copy.yaml\n"
			f.files["apps/orders/copy.yaml"] = scaledObjectYAML
		}, ni, sourcemap.ReasonAmbiguousDeclaration},
		{"a document declares the object without a namespace", func(f *fixture) {
			f.files[ksFile] += "  - copy.yaml\n"
			f.files["apps/orders/copy.yaml"] = strings.ReplaceAll(scaledObjectYAML, "  namespace: orders\n", "")
		}, ni, sourcemap.ReasonAmbiguousDeclaration},
		{"a document declares the object in another version", func(f *fixture) {
			f.files[ksFile] += "  - copy.yaml\n"
			f.files["apps/orders/copy.yaml"] = strings.ReplaceAll(scaledObjectYAML, "keda.sh/v1alpha1", "keda.sh/v1beta1")
		}, ni, sourcemap.ReasonAmbiguousDeclaration},
		{"a List object", func(f *fixture) {
			f.files[ksFile] += "  - list.yaml\n"
			f.files["apps/orders/list.yaml"] = "apiVersion: v1\nkind: List\nitems: []\n"
		}, ni, sourcemap.ReasonDeclarationNotFound},
		{"field missing in Git", func(f *fixture) {
			f.files[soFile] = strings.Replace(scaledObjectYAML, "  maxReplicaCount: 10\n", "", 1)
		}, ni, sourcemap.ReasonFieldNotFound},
		{"field missing in the cluster", func(f *fixture) {
			unstructured.RemoveNestedField(f.target.Object, "spec", "maxReplicaCount")
		}, ni, sourcemap.ReasonFieldNotFound},
		{"value in Git differs from the live value", func(f *fixture) {
			set(f.target, int64(12), "spec", "maxReplicaCount")
		}, ni, sourcemap.ReasonValueMismatch},
		{"value in Git is a string", func(f *fixture) {
			f.files[soFile] = strings.Replace(scaledObjectYAML, "maxReplicaCount: 10", `maxReplicaCount: "10"`, 1)
		}, ni, sourcemap.ReasonValueMismatch},
		{"memory limit differs", func(f *fixture) {
			f.targetDeployment()
			f.files["apps/orders/deployment.yaml"] = strings.Replace(deploymentYAML, "268435456", "512Mi", 1)
		}, ni, sourcemap.ReasonValueMismatch},
		{"container missing in Git", func(f *fixture) {
			f.targetDeployment()
			f.files["apps/orders/deployment.yaml"] = strings.Replace(deploymentYAML, "- name: web", "- name: api", 1)
		}, ni, sourcemap.ReasonFieldNotFound},
		{"file outside allowedPaths", func(f *fixture) {
			f.in.GitOps.AllowedPaths = []v1alpha1.GitPathPattern{"apps/billing/*.yaml"}
		}, dn, sourcemap.ReasonPathNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			tc.change(f)
			res, _, err := f.run(t)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Outcome != tc.outcome || res.Reason != tc.reason {
				t.Fatalf("result = %s/%s (%s), want %s/%s", res.Outcome, res.Reason, res.Message, tc.outcome, tc.reason)
			}
			if res.Source != nil {
				t.Errorf("source set on a stop: %+v", res.Source)
			}
			if res.Message == "" {
				t.Error("no message")
			}
			if strings.Contains(res.Message, "://") {
				t.Errorf("message quotes a URL: %s", res.Message)
			}
			sameWithCache(t, tc.change, res)
		})
	}
}

// TestResolveReturnsTransientErrors checks that failed reads come back as ErrTransient without the provider's
// text, which may hold a repository URL with credentials.
func TestResolveReturnsTransientErrors(t *testing.T) {
	const secret = "ghs_s3cr3tT0ken"
	leaky := errors.New("GET https://x-access-token:" + secret + "@github.com/example/fleet.git: 502 Bad Gateway")
	failOn := func(op string) func(string) error {
		return func(got string) error {
			if got == op {
				return &providers.RetryableError{RetryAfter: 30 * time.Second, Err: leaky}
			}
			return nil
		}
	}
	tests := []struct {
		name       string
		change     func(f *fixture)
		retryAfter time.Duration
	}{
		{"cluster read fails", func(f *fixture) { f.getErr = leaky }, 0},
		{"branch head fails", func(f *fixture) { f.gitFail = failOn("BranchHead") }, 30 * time.Second},
		{"file read fails", func(f *fixture) { f.gitFail = failOn(opReadFile) }, 30 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			tc.change(f)
			res, _, err := f.run(t)
			if !errors.Is(err, sourcemap.ErrTransient) {
				t.Fatalf("error = %v, want ErrTransient", err)
			}
			if errors.Is(err, leaky) {
				t.Error("the provider error is wrapped")
			}
			for _, leak := range []string{secret, "github.com", "x-access-token", "502"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error %q contains %q", err, leak)
				}
			}
			var te *sourcemap.TransientError
			if !errors.As(err, &te) || te.RetryAfter != tc.retryAfter {
				t.Errorf("error = %#v, want RetryAfter %s", err, tc.retryAfter)
			}
			if res.Outcome != "" {
				t.Errorf("outcome %s returned with an error", res.Outcome)
			}
		})
	}
}

// TestResolveKeepsFinalRefusals checks that a Git read the host refuses for good (a 403 that is no rate limit,
// a 422) keeps its class through the mapping, so the caller stops instead of retrying until a deadline, and
// that it still carries no provider text. A rate limit stays transient whatever else it matches.
func TestResolveKeepsFinalRefusals(t *testing.T) {
	const secret = "ghs_s3cr3tT0ken"
	for _, class := range []error{providers.ErrForbidden, providers.ErrRejected} {
		for _, op := range []string{"BranchHead", opReadFile} {
			t.Run(class.Error()+" "+op, func(t *testing.T) {
				f := newFixture()
				f.gitFail = func(got string) error {
					if got == op {
						return fmt.Errorf("GET https://github.com/example/fleet with token %s: 403: %w", secret, class)
					}
					return nil
				}
				_, _, err := f.run(t)
				if !errors.Is(err, class) || errors.Is(err, sourcemap.ErrTransient) {
					t.Fatalf("error = %#v, want the class %v and not ErrTransient", err, class)
				}
				for _, leak := range []string{secret, "example/fleet", "403"} {
					if strings.Contains(err.Error(), leak) {
						t.Errorf("error %q contains %q", err, leak)
					}
				}
			})
		}
	}
	t.Run("rate limit", func(t *testing.T) {
		f := newFixture()
		f.gitFail = func(got string) error {
			if got == "BranchHead" {
				return &providers.RetryableError{RetryAfter: time.Minute, Err: providers.ErrForbidden}
			}
			return nil
		}
		_, _, err := f.run(t)
		if !errors.Is(err, sourcemap.ErrTransient) || errors.Is(err, providers.ErrForbidden) {
			t.Fatalf("error = %#v, want a transient error", err)
		}
	})
}

// TestResolveKeepsSourceURLOutOfMessages checks that a Flux URL with credentials never reaches the result.
func TestResolveKeepsSourceURLOutOfMessages(t *testing.T) {
	const secret = "ghp_s3cr3tT0ken"
	f := newFixture()
	set(f.src, "https://deploy:"+secret+"@git.example.test/example/fleet", "spec", "url")
	res, _, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != sourcemap.Denied || res.Reason != sourcemap.ReasonRepositoryNotConfigured {
		t.Fatalf("result = %+v", res)
	}
	for _, leak := range []string{secret, "deploy:", "git.example.test", "://"} {
		if strings.Contains(res.Message, leak) {
			t.Errorf("message %q contains %q", res.Message, leak)
		}
	}
}

func TestFieldFor(t *testing.T) {
	so := v1alpha1.ObjectRef{APIVersion: "keda.sh/v1alpha1", Kind: "ScaledObject"}
	dep := v1alpha1.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment"}
	sts := v1alpha1.ObjectRef{APIVersion: "apps/v1", Kind: "StatefulSet"}
	kedaAction := v1alpha1.Action{Type: v1alpha1.ActionAdjustKedaMaxReplicas}
	memAction := v1alpha1.Action{
		Type:                    v1alpha1.ActionSetContainerMemoryLimit,
		SetContainerMemoryLimit: &v1alpha1.SetContainerMemoryLimit{Container: "web"},
	}
	keda, err := sourcemap.FieldFor(kedaAction, so)
	if err != nil || keda.String() != "spec.maxReplicaCount" || keda.Quantity {
		t.Errorf("keda field = %+v, %v", keda, err)
	}
	for _, target := range []v1alpha1.ObjectRef{dep, sts} {
		mem, err := sourcemap.FieldFor(memAction, target)
		if err != nil || mem.String() != "spec.template.spec.containers[name=web].resources.limits.memory" || !mem.Quantity {
			t.Errorf("memory field for %s = %+v, %v", target.Kind, mem, err)
		}
	}
	refused := []struct {
		action v1alpha1.Action
		target v1alpha1.ObjectRef
	}{
		{kedaAction, dep},
		{memAction, so},
		{memAction, v1alpha1.ObjectRef{APIVersion: "apps/v1", Kind: "DaemonSet"}},
		{memAction, v1alpha1.ObjectRef{APIVersion: "example.com/v1", Kind: "Deployment"}},
		{v1alpha1.Action{Type: v1alpha1.ActionSetContainerMemoryLimit}, dep},
		{v1alpha1.Action{Type: "DeleteNamespace"}, dep},
		{kedaAction, v1alpha1.ObjectRef{APIVersion: "a/b/c", Kind: "ScaledObject"}},
	}
	for _, r := range refused {
		if _, err := sourcemap.FieldFor(r.action, r.target); err == nil {
			t.Errorf("%s on %s %s accepted", r.action.Type, r.target.APIVersion, r.target.Kind)
		}
	}
}

//nolint:goconst // literal values keep the test readable
func TestFieldGetRefusesDuplicateNames(t *testing.T) {
	f := sourcemap.Field{Steps: []sourcemap.Step{{Key: "containers", Name: "web"}, {Key: "image"}}}
	doc := map[string]any{"containers": []any{
		map[string]any{"name": "web", "image": "a"},
		map[string]any{"name": "web", "image": "b"},
	}}
	if v, ok := f.Get(doc); ok {
		t.Errorf("got %v from a list with two matching names", v)
	}
	doc["containers"] = []any{map[string]any{"name": "web", "image": "a"}}
	if v, ok := f.Get(doc); !ok || v != "a" {
		t.Errorf("got %v, %v", v, ok)
	}
}

// scaledObjectDoc is one ScaledObject document with the given identity and maxReplicaCount.
func scaledObjectDoc(apiVersion, kind, namespace, name string, maxReplicas int) string {
	ns := ""
	if namespace != "" {
		ns = "  namespace: " + namespace + "\n"
	}
	return fmt.Sprintf("---\napiVersion: %s\nkind: %s\nmetadata:\n  name: %s\n%sspec:\n  maxReplicaCount: %d\n",
		apiVersion, kind, name, ns, maxReplicas)
}

// TestResolveNeverPicksAnotherDocument checks that a multi-document file resolves only to the document whose
// apiVersion, kind, namespace and name all equal the live target's. The policy decision trusts this
// (ADR-0012): it cannot check itself that the resolved source and the live target are the same declaration.
func TestResolveNeverPicksAnotherDocument(t *testing.T) {
	const live = 10
	decoys := []struct {
		name   string
		doc    func(replicas int) string
		reason sourcemap.Reason
	}{
		{"other name", func(n int) string {
			return scaledObjectDoc("keda.sh/v1alpha1", "ScaledObject", "orders", "orders-api", n)
		}, sourcemap.ReasonDeclarationNotFound},
		{"other namespace", func(n int) string {
			return scaledObjectDoc("keda.sh/v1alpha1", "ScaledObject", "billing", "orders-worker", n)
		}, sourcemap.ReasonDeclarationNotFound},
		{"other kind", func(n int) string {
			return scaledObjectDoc("keda.sh/v1alpha1", "ScaledJob", "orders", "orders-worker", n)
		}, sourcemap.ReasonDeclarationNotFound},
		{"other group", func(n int) string {
			return scaledObjectDoc("example.com/v1alpha1", "ScaledObject", "orders", "orders-worker", n)
		}, sourcemap.ReasonDeclarationNotFound},
		{"other version", func(n int) string {
			return scaledObjectDoc("keda.sh/v1beta1", "ScaledObject", "orders", "orders-worker", n)
		}, sourcemap.ReasonAmbiguousDeclaration},
		{"no namespace", func(n int) string {
			return scaledObjectDoc("keda.sh/v1alpha1", "ScaledObject", "", "orders-worker", n)
		}, sourcemap.ReasonAmbiguousDeclaration},
	}
	realDoc := func(n int) string {
		return scaledObjectDoc("keda.sh/v1alpha1", "ScaledObject", "orders", "orders-worker", n)
	}

	// A decoy holding the live value never resolves, alone or between unrelated documents.
	for _, d := range decoys {
		t.Run("decoy alone: "+d.name, func(t *testing.T) {
			f := newFixture()
			f.files[soFile] = scaledObjectYAML[:strings.Index(scaledObjectYAML, "---")] + d.doc(live)
			res, _, err := f.run(t)
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != sourcemap.NeedsIntervention || res.Reason != d.reason || res.Source != nil {
				t.Fatalf("result = %s/%s (%s), want NeedsIntervention/%s", res.Outcome, res.Reason, res.Message, d.reason)
			}
		})
	}

	// With the real document among decoys that differ only in identity, the real document's value is the one
	// compared: decoys with another value do not prevent resolution, and decoys with the live value do not
	// hide a real document whose value differs.
	var differ, same strings.Builder
	for _, d := range decoys[:4] {
		differ.WriteString(d.doc(live + 1))
		same.WriteString(d.doc(live))
	}
	t.Run("real document among decoys", func(t *testing.T) {
		f := newFixture()
		f.files[soFile] = differ.String() + realDoc(live) + differ.String()
		res, _, err := f.run(t)
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != sourcemap.Resolved || res.Source == nil || res.Source.Path != soFile {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("real document with another value among decoys with the live value", func(t *testing.T) {
		f := newFixture()
		f.files[soFile] = same.String() + realDoc(live+1) + same.String()
		res, _, err := f.run(t)
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != sourcemap.NeedsIntervention || res.Reason != sourcemap.ReasonValueMismatch {
			t.Fatalf("result = %s/%s (%s), want NeedsIntervention/ValueMismatch", res.Outcome, res.Reason, res.Message)
		}
	})
}

// padReads is a GitProvider that returns every file padded past MaxFileBytes instead of ErrTooLarge.
type padReads struct{ providers.GitProvider }

func (p padReads) ReadFile(ctx context.Context, commit, path string, limit int) (providers.File, error) {
	f, err := p.GitProvider.ReadFile(ctx, commit, path, limit)
	if err == nil {
		f.Data = append(f.Data, make([]byte, providers.MaxFileBytes)...)
	}
	return f, err
}

// The Kustomization and the GitRepository choose what is scanned. If either changes while Git is scanned, the
// result is a retry, not a source.
func TestResolveRetriesWhenFluxObjectsChangeDuringTheScan(t *testing.T) {
	for _, gvk := range []schema.GroupVersionKind{sourcemap.KustomizationGVK, sourcemap.GitRepositoryGVK} {
		t.Run(gvk.Kind, func(t *testing.T) {
			f := newFixture()
			f.duringGit = func(c client.Client) {
				o := &unstructured.Unstructured{}
				o.SetGroupVersionKind(gvk)
				name := "apps"
				if gvk == sourcemap.GitRepositoryGVK {
					name = "fleet"
				}
				if err := c.Get(context.Background(), client.ObjectKey{Namespace: "flux-system", Name: name}, o); err != nil {
					t.Fatal(err)
				}
				o.SetAnnotations(map[string]string{"touched": "yes"})
				if err := c.Update(context.Background(), o); err != nil {
					t.Fatal(err)
				}
			}
			res, _, err := f.run(t)
			if !errors.Is(err, sourcemap.ErrTransient) {
				t.Fatalf("error = %v (outcome %s), want ErrTransient", err, res.Outcome)
			}
			if res.Source != nil {
				t.Error("a source was returned with the error")
			}
		})
	}
}

// TestResolveStopsOnUnusableRepositorySettings checks that settings the opener calls permanently unusable end
// in NeedsIntervention at once, not in an endless retry, and that the opener's text never reaches the result.
func TestResolveStopsOnUnusableRepositorySettings(t *testing.T) {
	const secret = "ghp_s3cr3tT0ken"
	f := newFixture()
	f.openErr = fmt.Errorf("https://x-access-token:%s@github.com/example/fleet: %w", secret, providers.ErrInvalidConfig)
	res, _, err := f.run(t)
	if err != nil {
		t.Fatalf("error = %v, want none: a permanent refusal is a result, not a retry", err)
	}
	if res.Outcome != sourcemap.NeedsIntervention || res.Reason != sourcemap.ReasonRepositoryUnusable {
		t.Fatalf("result = %+v", res)
	}
	for _, leak := range []string{secret, "github.com", "x-access-token", "://"} {
		if strings.Contains(res.Message, leak) {
			t.Errorf("message %q contains %q", res.Message, leak)
		}
	}
}

// TestResolveRetriesOtherOpenerErrors checks that an opener error that is not a settings refusal is still
// retried.
func TestResolveRetriesOtherOpenerErrors(t *testing.T) {
	f := newFixture()
	f.openErr = errors.New("workload identity is not available")
	_, _, err := f.run(t)
	if !errors.Is(err, sourcemap.ErrTransient) {
		t.Fatalf("error = %v, want ErrTransient", err)
	}
}

// touch updates a Flux object in the cluster, so its resource version changes.
func touch(t *testing.T, c client.Client, gvk schema.GroupVersionKind, name string) {
	t.Helper()
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvk)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "flux-system", Name: name}, o); err != nil {
		t.Fatal(err)
	}
	o.SetAnnotations(map[string]string{"touched": "yes"})
	if err := c.Update(context.Background(), o); err != nil {
		t.Fatal(err)
	}
}

// A stop is checked like a source (#145): a declaration not found under a Kustomization path that changed while
// Git was scanned, or a repository refused from Flux objects that changed after they were read, describes a delivery
// that no longer exists, and a caller would end a plan on it. The result is a retry.
func TestResolveRetriesAStopWhenFluxObjectsChange(t *testing.T) {
	t.Run("during the Git scan", func(t *testing.T) {
		f := newFixture()
		f.in.GitOps.AllowedPaths = []v1alpha1.GitPathPattern{"other/**"}
		f.duringGit = func(c client.Client) { touch(t, c, sourcemap.KustomizationGVK, "apps") }
		res, _, err := f.run(t)
		if !errors.Is(err, sourcemap.ErrTransient) {
			t.Fatalf("error = %v (outcome %s, reason %s), want ErrTransient", err, res.Outcome, res.Reason)
		}
	})
	t.Run("after the delivery was read", func(t *testing.T) {
		f := newFixture()
		f.in.GitOps.RepositoryRef = "other"
		touched := false
		f.afterGet = func(c client.Client, obj client.Object) {
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "GitRepository" && !touched {
				touched = true
				touch(t, c, sourcemap.KustomizationGVK, "apps")
			}
		}
		res, _, err := f.run(t)
		if !errors.Is(err, sourcemap.ErrTransient) {
			t.Fatalf("error = %v (outcome %s, reason %s), want ErrTransient", err, res.Outcome, res.Reason)
		}
	})
}

// Every outcome reports the Flux objects it was based on and, once Git was read, the tree it read, so a caller
// that acts on a stop can check that they are still current.
func TestResolveReportsWhatAStopWasBasedOn(t *testing.T) {
	t.Run("a stop in the Git scan", func(t *testing.T) {
		f := newFixture()
		f.in.GitOps.AllowedPaths = []v1alpha1.GitPathPattern{"other/**"}
		res, head, err := f.run(t)
		if err != nil || res.Outcome != sourcemap.Denied || res.Reason != sourcemap.ReasonPathNotAllowed {
			t.Fatalf("got %s %s %v, want Denied PathNotAllowed", res.Outcome, res.Reason, err)
		}
		if len(res.Observed) != 2 {
			t.Fatalf("observed %+v, want the Kustomization and the GitRepository", res.Observed)
		}
		want := sourcemap.Scanned{RepositoryRef: "fleet", Branch: "main", Commit: head}
		if res.Scanned == nil || *res.Scanned != want {
			t.Fatalf("scanned %+v, want %+v", res.Scanned, want)
		}
	})
	t.Run("a stop before Git was read", func(t *testing.T) {
		f := newFixture()
		f.in.GitOps.RepositoryRef = "other"
		res, _, err := f.run(t)
		if err != nil || res.Outcome != sourcemap.Denied || res.Reason != sourcemap.ReasonRepositoryNotInPolicy {
			t.Fatalf("got %s %s %v, want Denied RepositoryNotInPolicy", res.Outcome, res.Reason, err)
		}
		if len(res.Observed) != 2 || res.Scanned != nil {
			t.Fatalf("observed %+v, scanned %+v; want both Flux objects and no tree", res.Observed, res.Scanned)
		}
	})
	t.Run("a source", func(t *testing.T) {
		res, head, err := newFixture().run(t)
		if err != nil || res.Outcome != sourcemap.Resolved {
			t.Fatalf("got %s %v, want Resolved", res.Outcome, err)
		}
		// Source names the tree; Scanned does not repeat it.
		want := sourcemap.Scanned{RepositoryRef: "fleet", Branch: "main", Commit: head}
		if res.Scanned != nil || res.Tree() == nil || *res.Tree() != want {
			t.Fatalf("scanned %+v, tree %+v; want no Scanned and the tree %+v", res.Scanned, res.Tree(), want)
		}
	})
	t.Run("a branch not found", func(t *testing.T) {
		f := newFixture()
		f.gitBranch = "trunk"
		res, _, err := f.run(t)
		if err != nil || res.Reason != sourcemap.ReasonBranchNotFound {
			t.Fatalf("got %s %s %v, want BranchNotFound", res.Outcome, res.Reason, err)
		}
		want := sourcemap.Scanned{RepositoryRef: "fleet", Branch: "main"}
		if res.Tree() == nil || *res.Tree() != want {
			t.Fatalf("tree %+v, want %+v: the repository and branch found missing, no commit", res.Tree(), want)
		}
	})
	t.Run("a GitRepository not found", func(t *testing.T) {
		f := newFixture()
		f.src = nil
		res, _, err := f.run(t)
		if err != nil || res.Reason != sourcemap.ReasonSourceNotFound {
			t.Fatalf("got %s %s %v, want SourceNotFound", res.Outcome, res.Reason, err)
		}
		if len(res.Observed) != 2 || !res.Observed[1].Missing || res.Observed[1].Name != "fleet" ||
			res.Observed[0].Missing {
			t.Fatalf("observed %+v, want the Kustomization and the missing GitRepository", res.Observed)
		}
	})
}

// A GitRepository found missing is rechecked like one that was read: if it appears while the mapping runs,
// SourceNotFound describes a delivery that no longer exists. The result is a retry.
func TestResolveRetriesWhenAMissingSourceAppears(t *testing.T) {
	f := newFixture()
	src := f.src
	f.src = nil
	reads := 0
	f.afterGet = func(c client.Client, obj client.Object) {
		if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "Kustomization" {
			if reads++; reads == 2 { // the recheck, after the GitRepository was found missing
				if err := c.Create(context.Background(), src.DeepCopy()); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	res, _, err := f.run(t)
	if !errors.Is(err, sourcemap.ErrTransient) {
		t.Fatalf("error = %v (outcome %s, reason %s), want ErrTransient", err, res.Outcome, res.Reason)
	}
}

// readCounter is a GitProvider that counts the files it returns and their bytes.
type readCounter struct {
	providers.GitProvider
	files map[string]int
	bytes int
}

func (c *readCounter) ReadFile(ctx context.Context, commit, path string, limit int) (providers.File, error) {
	f, err := c.GitProvider.ReadFile(ctx, commit, path, limit)
	if err == nil {
		c.files[path]++
		c.bytes += len(f.Data)
	}
	return f, err
}

// bigFileBytes is just under MaxFileBytes, so no single file is refused.
const bigFileBytes = sourcemap.MaxFileBytes - 1024

// big appends a comment to doc so that it is bigFileBytes long.
func big(doc string) string {
	return doc + "# " + strings.Repeat("x", bigFileBytes-len(doc)-3) + "\n"
}

// configMaps returns n small YAML documents that do not declare the target.
func configMaps(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c%d\n  namespace: orders\n", i)
	}
	return b.String()
}

// TestResolveBoundsWhatItParses checks that the mapping stops at the second declaration and refuses a build that
// holds more bytes or documents than it parses in all, before it has read past the budget (#240).
func TestResolveBoundsWhatItParses(t *testing.T) {
	// Together the big files are far over the budget.
	copies := 2*sourcemap.MaxTotalBytes/bigFileBytes + 1
	// The fixture's files hold 3 documents: the Service and the ScaledObject, and the Deployment.
	const fixtureDocs = 3

	tests := []struct {
		name   string
		change func(f *fixture)
		want   sourcemap.Outcome
		reason sourcemap.Reason
		// maxReads bounds how often each listed file may be read; zero means it must not be read.
		maxReads map[string]int
	}{
		{"many large files that declare the object", func(f *fixture) {
			f.files[soFile] = big(scaledObjectYAML)
			for i := range copies {
				f.files[ksFile] += fmt.Sprintf("  - copy%d.yaml\n", i)
				f.files[fmt.Sprintf("apps/orders/copy%d.yaml", i)] = big(scaledObjectYAML)
			}
		}, sourcemap.NeedsIntervention, sourcemap.ReasonAmbiguousDeclaration,
			map[string]int{"apps/orders/copy0.yaml": 1, "apps/orders/copy1.yaml": 0}},
		// The document after the second declaration is not valid YAML: it is never decoded.
		{"a file that declares the object many times", func(f *fixture) {
			doc := scaledObjectYAML[strings.Index(scaledObjectYAML, "# The worker"):]
			f.files[soFile] = scaledObjectYAML + strings.Repeat(doc, 1000) + "a: [\n"
		}, sourcemap.NeedsIntervention, sourcemap.ReasonAmbiguousDeclaration, nil},
		{"a declaration without a namespace before many large files", func(f *fixture) {
			f.files[soFile] = strings.ReplaceAll(scaledObjectYAML, "  namespace: orders\n", "")
			for i := range copies {
				f.files[ksFile] += fmt.Sprintf("  - copy%d.yaml\n", i)
				f.files[fmt.Sprintf("apps/orders/copy%d.yaml", i)] = big(scaledObjectYAML)
			}
		}, sourcemap.NeedsIntervention, sourcemap.ReasonAmbiguousDeclaration,
			map[string]int{"apps/orders/copy0.yaml": 0}},
		{"many large files that do not declare the object", func(f *fixture) {
			for i := range copies {
				f.files[ksFile] += fmt.Sprintf("  - big%d.yaml\n", i)
				f.files[fmt.Sprintf("apps/orders/big%d.yaml", i)] = big(configMaps(1))
			}
		}, sourcemap.NeedsIntervention, sourcemap.ReasonInputTooLarge,
			map[string]int{fmt.Sprintf("apps/orders/big%d.yaml", sourcemap.MaxTotalBytes/bigFileBytes+1): 0}},
		// The document after the budget is not valid YAML: it is counted before it is decoded, so it never is.
		{"many documents in one file", func(f *fixture) {
			f.files[ksFile] += "  - many.yaml\n"
			f.files["apps/orders/many.yaml"] = configMaps(sourcemap.MaxDocuments-fixtureDocs) + "---\na: [\n"
		}, sourcemap.NeedsIntervention, sourcemap.ReasonInputTooLarge, nil},
		{"many documents across files", func(f *fixture) {
			for i := range 8 {
				f.files[ksFile] += fmt.Sprintf("  - many%d.yaml\n", i)
				f.files[fmt.Sprintf("apps/orders/many%d.yaml", i)] = configMaps(sourcemap.MaxDocuments/8 + 1)
			}
		}, sourcemap.NeedsIntervention, sourcemap.ReasonInputTooLarge, nil},
		{"documents that hold only comments count", func(f *fixture) {
			f.files[ksFile] += "  - comments.yaml\n"
			f.files["apps/orders/comments.yaml"] = strings.Repeat("# nothing\n---\n", sourcemap.MaxDocuments)
		}, sourcemap.NeedsIntervention, sourcemap.ReasonInputTooLarge, nil},
		{"exactly the document budget", func(f *fixture) {
			f.files[ksFile] += "  - many.yaml\n"
			f.files["apps/orders/many.yaml"] = configMaps(sourcemap.MaxDocuments - fixtureDocs)
		}, sourcemap.Resolved, sourcemap.ReasonResolved, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// counters holds one readCounter per run, with or without the cache.
			var counters []*readCounter
			change := func(f *fixture) {
				tc.change(f)
				f.gitWrap = func(g providers.GitProvider) providers.GitProvider {
					c := &readCounter{GitProvider: g, files: map[string]int{}}
					counters = append(counters, c)
					return c
				}
			}
			// The provider is asked for at most what is left of the budget, so the mapping downloads no file past
			// it (#298). A run through the cache reads from the provider only what it misses.
			checkBytes := func(c *readCounter) {
				t.Helper()
				if c.bytes > sourcemap.MaxTotalBytes {
					t.Errorf("read %d bytes, over the budget of %d", c.bytes, sourcemap.MaxTotalBytes)
				}
			}
			f := newFixture()
			change(f)
			res, _, err := f.run(t)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Outcome != tc.want || res.Reason != tc.reason {
				t.Fatalf("result = %s/%s (%s), want %s/%s", res.Outcome, res.Reason, res.Message, tc.want, tc.reason)
			}
			if len(counters) != 1 {
				t.Fatalf("opened the repository %d times, want 1", len(counters))
			}
			c := counters[0]
			checkBytes(c)
			for p, n := range tc.maxReads {
				if c.files[p] > n {
					t.Errorf("%s read %d times, want at most %d", p, c.files[p], n)
				}
			}
			counters = nil
			sameWithCache(t, change, res)
			if len(counters) == 0 {
				t.Fatal("the cached runs opened no repository")
			}
			for _, c := range counters {
				checkBytes(c)
			}
		})
	}
}

// A build of exactly MaxTotalBytes resolves, one byte more stops with InputTooLarge, and neither reads past
// the budget from the provider, with a cold or a warm cache (#298).
func TestResolveReadsExactlyTheByteBudget(t *testing.T) {
	const fillers = 17 // together able to fill the budget, each under MaxFileBytes
	// build lists the fillers, with the given sizes (nil: empty), and counts what each run reads.
	build := func(sizes []int, counters *[]*readCounter) func(*fixture) {
		return func(f *fixture) {
			for i := range fillers {
				p := fmt.Sprintf("apps/orders/fill%d.yaml", i)
				f.files[ksFile] += fmt.Sprintf("  - fill%d.yaml\n", i)
				f.files[p] = ""
				if sizes != nil {
					f.files[p] = "#" + strings.Repeat("x", sizes[i]-2) + "\n"
				}
			}
			f.gitWrap = func(g providers.GitProvider) providers.GitProvider {
				c := &readCounter{GitProvider: g, files: map[string]int{}}
				*counters = append(*counters, c)
				return c
			}
		}
	}
	run := func(t *testing.T, change func(*fixture)) sourcemap.Result {
		t.Helper()
		f := newFixture()
		change(f)
		res, _, err := f.run(t)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return res
	}

	// The rest of the build, fillers empty, gives what is left for them.
	var counters []*readCounter
	if res := run(t, build(nil, &counters)); res.Outcome != sourcemap.Resolved {
		t.Fatalf("baseline = %s/%s (%s)", res.Outcome, res.Reason, res.Message)
	}
	room := sourcemap.MaxTotalBytes - counters[0].bytes
	sizes := make([]int, fillers)
	for i := range sizes {
		sizes[i] = room / fillers
	}
	sizes[fillers-1] += room % fillers
	if sizes[fillers-1] > sourcemap.MaxFileBytes {
		t.Fatalf("filler of %d bytes is over MaxFileBytes", sizes[fillers-1])
	}

	for name, tc := range map[string]struct {
		extra int
		want  sourcemap.Reason
		total int
	}{
		"exactly the budget": {0, sourcemap.ReasonResolved, sourcemap.MaxTotalBytes},
		"one byte over":      {1, sourcemap.ReasonInputTooLarge, sourcemap.MaxTotalBytes - sizes[fillers-1]},
	} {
		t.Run(name, func(t *testing.T) {
			s := slices.Clone(sizes)
			s[fillers-1] += tc.extra
			var counters []*readCounter
			change := build(s, &counters)
			res := run(t, change)
			if res.Reason != tc.want {
				t.Fatalf("result = %s/%s (%s), want %s", res.Outcome, res.Reason, res.Message, tc.want)
			}
			if counters[0].bytes != tc.total {
				t.Errorf("read %d bytes, want %d", counters[0].bytes, tc.total)
			}
			counters = nil
			sameWithCache(t, change, res)
			for _, c := range counters {
				if c.bytes > sourcemap.MaxTotalBytes {
					t.Errorf("a cached run read %d bytes, over the budget of %d", c.bytes, sourcemap.MaxTotalBytes)
				}
			}
		})
	}
}

// ReadField decodes at most MaxDocuments documents of its file and refuses the rest unread (#240).
func TestReadFieldBoundsDocuments(t *testing.T) {
	target := v1alpha1.ObjectRef{APIVersion: "keda.sh/v1alpha1", Kind: "ScaledObject", Namespace: "orders",
		Name: "orders-worker"}
	field, err := sourcemap.FieldFor(v1alpha1.Action{Type: v1alpha1.ActionAdjustKedaMaxReplicas}, target)
	if err != nil {
		t.Fatal(err)
	}
	// scaledObjectYAML holds 2 documents: the Service and the ScaledObject.
	atBudget := scaledObjectYAML + configMaps(sourcemap.MaxDocuments-2)
	got, found, err := sourcemap.ReadField([]byte(atBudget), target, field)
	if err != nil || !found || fmt.Sprint(got) != "10" {
		t.Fatalf("at the budget: ReadField = %v, %v, %v; want 10, true, nil", got, found, err)
	}
	// The document after the budget is not valid YAML: it is counted before it is decoded, so it never is.
	_, _, err = sourcemap.ReadField([]byte(atBudget+"---\na: [\n"), target, field)
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("over the budget: ReadField error = %v, want one about too many documents", err)
	}
}
