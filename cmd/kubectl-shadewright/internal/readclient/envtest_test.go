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

package readclient

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	remediationv1alpha1 "github.com/pkalin/shadewright/api/v1alpha1"
	"github.com/pkalin/shadewright/internal/fluxstatus"
)

// Names shared by the cases of TestErrorsFromAPIServer.
const (
	envNS                   = "default"
	unservedGroupNamespaced = "unserved group, namespaced"
	unservedNamespaced      = "unserved version of a served group, namespaced"
	unservedClusterScoped   = "unserved version of a served group, cluster-scoped"
)

// envtestBinaryDir returns the first envtest binary directory under bin/k8s, or "" to let envtest use
// KUBEBUILDER_ASSETS.
func envtestBinaryDir() string {
	base := filepath.Join("..", "..", "..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}

// TestErrorsFromAPIServer pins what a real kube-apiserver answers, through client-go's dynamic and metadata
// clients, for a kind it does not serve and for a missing object of a kind it serves: the first must read as
// a NoKindMatchError and the second as NotFound, as controller-runtime's client answered. A caller without a
// grant gets a 403 for both; the unserved kind must still read as a NoKindMatchError and the served one stay
// Forbidden. The fakes in TestErrors cannot show the wire shape.
func TestErrorsFromAPIServer(t *testing.T) {
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: envtestBinaryDir(),
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("starting envtest (run make setup-envtest first): %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Kinds the API server does not serve: Flux is not installed, and the remediation group has no v1beta1.
	gitRepos := schema.GroupVersionKind{Group: fluxstatus.SourceGroup, Version: "v1",
		Kind: fluxstatus.KindGitRepository}
	remediationGroup := remediationv1alpha1.GroupVersion.Group
	unservedSettings := schema.GroupVersionKind{Group: remediationGroup, Version: "v1beta1",
		Kind: "RemediationSettings"}
	unservedIncidents := schema.GroupVersionKind{Group: remediationGroup, Version: "v1beta1", Kind: "Incident"}
	// Kinds it serves: a built-in and Shadewright's CRDs, namespaced and cluster-scoped.
	configMaps := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	crds := apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition")
	incidentsV1 := remediationv1alpha1.GroupVersion.WithKind("Incident")
	settingsV1 := remediationv1alpha1.GroupVersion.WithKind("RemediationSettings")

	noKind := func(t *testing.T, err error) {
		t.Helper()
		if !meta.IsNoMatchError(err) {
			t.Fatalf("got %T %v, want a NoKindMatchError", err, err)
		}
	}
	notFound := func(t *testing.T, err error) {
		t.Helper()
		if !apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			t.Fatalf("got %T %v, want NotFound", err, err)
		}
	}
	for _, tc := range []struct {
		name string
		gvk  schema.GroupVersionKind
		ns   string
		want func(*testing.T, error)
	}{
		{unservedGroupNamespaced, gitRepos, envNS, noKind},
		{"unserved group, no namespace", gitRepos, "", noKind},
		{unservedNamespaced, unservedIncidents, envNS, noKind},
		{unservedClusterScoped, unservedSettings, "", noKind},
		{"missing built-in object, namespaced", configMaps, envNS, notFound},
		{"missing built-in object, cluster-scoped", crds, "", notFound},
		{"missing custom object, namespaced", incidentsV1, envNS, notFound},
		{"missing custom object, cluster-scoped", settingsV1, "", notFound},
	} {
		key := ObjectKey{Namespace: tc.ns, Name: "absent"}
		t.Run("get dynamic/"+tc.name, func(t *testing.T) {
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(tc.gvk)
			tc.want(t, c.Get(ctx, key, u))
		})
		t.Run("get metadata/"+tc.name, func(t *testing.T) {
			m := &metav1.PartialObjectMetadata{}
			m.SetGroupVersionKind(tc.gvk)
			tc.want(t, c.Get(ctx, key, m))
		})
	}

	// A list of a served kind succeeds, empty or not; of an unserved kind it is a NoKindMatchError.
	for _, tc := range []struct {
		name string
		gvk  schema.GroupVersionKind
		ns   string
	}{
		{unservedGroupNamespaced, gitRepos, envNS},
		{"unserved group, all namespaces", gitRepos, ""},
		{unservedNamespaced, unservedIncidents, envNS},
		{unservedClusterScoped, unservedSettings, ""},
	} {
		listGVK := tc.gvk.GroupVersion().WithKind(tc.gvk.Kind + "List")
		var opts []ListOption
		if tc.ns != "" {
			opts = append(opts, InNamespace(tc.ns))
		}
		t.Run("list dynamic/"+tc.name, func(t *testing.T) {
			u := &unstructured.UnstructuredList{}
			u.SetGroupVersionKind(listGVK)
			noKind(t, c.List(ctx, u, opts...))
		})
		t.Run("list metadata/"+tc.name, func(t *testing.T) {
			m := &metav1.PartialObjectMetadataList{}
			m.SetGroupVersionKind(listGVK)
			noKind(t, c.List(ctx, m, opts...))
		})
	}
	for _, gvk := range []schema.GroupVersionKind{configMaps, crds, incidentsV1, settingsV1} {
		listGVK := gvk.GroupVersion().WithKind(gvk.Kind + "List")
		t.Run("list served/"+gvk.Kind, func(t *testing.T) {
			u := &unstructured.UnstructuredList{}
			u.SetGroupVersionKind(listGVK)
			if err := c.List(ctx, u); err != nil {
				t.Fatalf("dynamic: %v", err)
			}
			m := &metav1.PartialObjectMetadataList{}
			m.SetGroupVersionKind(listGVK)
			if err := c.List(ctx, m); err != nil {
				t.Fatalf("metadata: %v", err)
			}
		})
	}

	// A user with no grant: authorization runs before routing, so the API server answers 403 for a kind it
	// does not serve as well as for one it does. The first must still read as a NoKindMatchError, the
	// second must stay Forbidden.
	user, err := testEnv.AddUser(envtest.User{Name: "no-grants", Groups: []string{"system:authenticated"}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	restricted, err := New(user.Config())
	if err != nil {
		t.Fatal(err)
	}
	forbidden := func(t *testing.T, err error) {
		t.Helper()
		if !apierrors.IsForbidden(err) {
			t.Fatalf("got %T %v, want Forbidden", err, err)
		}
	}
	for _, tc := range []struct {
		name string
		gvk  schema.GroupVersionKind
		ns   string
		want func(*testing.T, error)
	}{
		{unservedGroupNamespaced, gitRepos, envNS, noKind},
		{"unserved group, no namespace", gitRepos, "", noKind},
		{unservedNamespaced, unservedIncidents, envNS, noKind},
		{unservedClusterScoped, unservedSettings, "", noKind},
		{"served built-in kind, namespaced", configMaps, envNS, forbidden},
		{"served built-in kind, cluster-scoped", crds, "", forbidden},
		{"served custom kind, namespaced", incidentsV1, envNS, forbidden},
		{"served custom kind, cluster-scoped", settingsV1, "", forbidden},
	} {
		key := ObjectKey{Namespace: tc.ns, Name: "absent"}
		listGVK := tc.gvk.GroupVersion().WithKind(tc.gvk.Kind + "List")
		var opts []ListOption
		if tc.ns != "" {
			opts = append(opts, InNamespace(tc.ns))
		}
		t.Run("forbidden get dynamic/"+tc.name, func(t *testing.T) {
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(tc.gvk)
			tc.want(t, restricted.Get(ctx, key, u))
		})
		t.Run("forbidden get metadata/"+tc.name, func(t *testing.T) {
			m := &metav1.PartialObjectMetadata{}
			m.SetGroupVersionKind(tc.gvk)
			tc.want(t, restricted.Get(ctx, key, m))
		})
		t.Run("forbidden list dynamic/"+tc.name, func(t *testing.T) {
			u := &unstructured.UnstructuredList{}
			u.SetGroupVersionKind(listGVK)
			tc.want(t, restricted.List(ctx, u, opts...))
		})
		t.Run("forbidden list metadata/"+tc.name, func(t *testing.T) {
			m := &metav1.PartialObjectMetadataList{}
			m.SetGroupVersionKind(listGVK)
			tc.want(t, restricted.List(ctx, m, opts...))
		})
	}
}
