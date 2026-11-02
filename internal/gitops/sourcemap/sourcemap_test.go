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
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
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
	getErr     error
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
		"spec":   map[string]any{"url": fleetURL, "ref": map[string]any{"branch": "main"}},
		"status": map[string]any{"artifact": map[string]any{"revision": revision}},
	})

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
	r := &sourcemap.Resolver{
		Client: b.Build(),
		OpenRepository: func(_ context.Context, repo v1alpha1.GitRepository) (providers.GitProvider, error) {
			if repo.Name != "fleet" {
				return nil, fmt.Errorf("opened repository %s", repo.Name)
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
			path: soFile,
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
				f.files["apps/orders/sub/Kustomization"] = "resources:\n  - ../scaledobject.yaml\n"
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
			want := v1alpha1.ResolvedSource{RepositoryRef: "fleet", TargetBranch: "main", Path: v1alpha1.GitFilePath(tc.path), BaseCommit: head}
			if !shaRE.MatchString(res.Source.BaseCommit) {
				t.Errorf("base commit %q is not a full SHA", res.Source.BaseCommit)
			}
			if *res.Source != want {
				t.Errorf("source = %+v, want %+v", *res.Source, want)
			}
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
		}, dn, sourcemap.ReasonBranchMismatch},
		{"branch missing in Git", func(f *fixture) { f.gitBranch = "trunk" }, ni, sourcemap.ReasonBranchNotFound},

		// Step 3: paths and kustomization files.
		{"Kustomization path outside the repository", func(f *fixture) { set(f.ks, "../outside", "spec", "path") }, ni, sourcemap.ReasonInvalidPath},
		{"absolute Kustomization path", func(f *fixture) { set(f.ks, "/apps/orders", "spec", "path") }, ni, sourcemap.ReasonInvalidPath},
		{"hidden directory in the Kustomization path", func(f *fixture) { set(f.ks, "./.github", "spec", "path") }, ni, sourcemap.ReasonInvalidPath},
		{"Kustomization path is a URL", func(f *fixture) {
			set(f.ks, "https://example.com/apps", "spec", "path")
		}, ni, sourcemap.ReasonInvalidPath},
		{"no kustomization file", func(f *fixture) { delete(f.files, ksFile) }, ni, sourcemap.ReasonNoKustomizationFile},
		{"nested directory without a kustomization file", func(f *fixture) {
			f.files[ksFile] = "resources:\n  - workers\n"
		}, ni, sourcemap.ReasonNoKustomizationFile},
		{"two kustomization files", func(f *fixture) {
			f.files["apps/orders/kustomization.yml"] = "resources: []\n"
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
			f.files[ksFile] = "resources:\n  - github.com/example/fleet/apps\n"
		}, ni, sourcemap.ReasonNoKustomizationFile},
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
				f.files[fmt.Sprintf("apps/orders/d%d/kustomization.yaml", i)] = "resources: []\n"
			}
			f.files[ksFile] = b.String()
		}, ni, sourcemap.ReasonTooManyFiles},
		{"directories nested too deep", func(f *fixture) {
			dir := "apps/orders"
			for i := range sourcemap.MaxDepth + 1 {
				f.files[dir+"/kustomization.yaml"] = fmt.Sprintf("resources:\n  - d%d\n", i)
				dir = fmt.Sprintf("%s/d%d", dir, i)
			}
			f.files[dir+"/kustomization.yaml"] = "resources: []\n"
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
		{"file read fails", func(f *fixture) { f.gitFail = failOn("ReadFile") }, 30 * time.Second},
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

//nolint:goconst // literal identities keep the cases readable
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
//
//nolint:goconst // literal identities keep each case readable
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
